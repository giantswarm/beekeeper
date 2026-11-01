package execution

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"time"

	"github.com/google/uuid"
)

const LeaseDuration = 90 * time.Second
const LeaseRenewal = 20 * time.Second

type LeaseRequest struct {
	Previous uint64 `json:"previous"`
	Nonce    string `json:"nonce"`
}

type LeaseGrant struct {
	Ref         Ref       `json:"ref"`
	Coordinator string    `json:"coordinator"`
	Sequence    uint64    `json:"sequence"`
	Nonce       string    `json:"nonce"`
	IssuedAt    time.Time `json:"issuedAt"`
	ExpiresAt   time.Time `json:"expiresAt"`
	Signature   string    `json:"signature"`
}

// IssueLease is called inside the coordinator's stop/renewal transaction. A
// repeated request returns its original deadline, never another lease interval.
func IssueLease(ref Ref, coordinator string, previous *LeaseGrant, request LeaseRequest, key ed25519.PrivateKey, now time.Time) (LeaseGrant, error) {
	if ref.Validate() != nil || !ValidIdentity(coordinator) || len(key) != ed25519.PrivateKeySize {
		return LeaseGrant{}, errors.New("invalid lease execution")
	}
	if _, err := uuid.Parse(request.Nonce); err != nil {
		return LeaseGrant{}, errors.New("invalid lease nonce")
	}
	sequence := uint64(0)
	if previous != nil {
		if previous.Ref != ref || previous.Coordinator != coordinator || previous.Sequence == 0 {
			return LeaseGrant{}, errors.New("previous lease belongs to another execution")
		}
		sequence = previous.Sequence
		if request.Nonce == previous.Nonce && request.Previous == sequence-1 {
			return *previous, nil
		}
	}
	if request.Previous != sequence || sequence == ^uint64(0) {
		return LeaseGrant{}, errors.New("lease sequence replay or gap")
	}
	grant := LeaseGrant{Ref: ref, Coordinator: coordinator, Sequence: sequence + 1, Nonce: request.Nonce, IssuedAt: now.UTC(), ExpiresAt: now.UTC().Add(LeaseDuration)}
	grant.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(key, mustJSON(grant)))
	return grant, nil
}

func VerifyLease(grant LeaseGrant, request LeaseRequest, ref Ref, coordinator, key string) error {
	pub, err := publicKey(key)
	if err != nil {
		return err
	}
	if grant.Ref != ref || grant.Coordinator != coordinator || grant.Sequence != request.Previous+1 || grant.Sequence == 0 || grant.Nonce != request.Nonce || grant.ExpiresAt.Sub(grant.IssuedAt) != LeaseDuration {
		return errors.New("mismatched coordinator lease")
	}
	signature, err := base64.StdEncoding.DecodeString(grant.Signature)
	if err != nil {
		return errors.New("invalid lease signature")
	}
	grant.Signature = ""
	if !ed25519.Verify(pub, mustJSON(grant), signature) {
		return errors.New("untrusted coordinator lease")
	}
	return nil
}

// renewLease persists the challenge before calling the coordinator, and stores
// its signed response before granting runtime. The caller measures deadlines
// from BEFORE the request with a monotonic clock; host wall-clock skew never
// extends authority. An uncertain retry keeps the original challenge and time.
func (s *Server) renewLease(ctx context.Context, ref Ref) (*LeaseGrant, error) {
	if err := s.update(ctx, ref, func(j *Journal) error {
		if j.StopRequest != nil {
			return errors.New("lease renewal revoked by stop")
		}
		if j.LeasePending == nil {
			previous := uint64(0)
			if j.Lease != nil {
				previous = j.Lease.Sequence
			}
			j.LeasePending = &LeaseRequest{Previous: previous, Nonce: uuid.NewString()}
		}
		return nil
	}); err != nil {
		return nil, err
	}
	j, err := s.Read(ref)
	if err != nil {
		return nil, err
	}
	co := s.Config.Coordinators[j.Request.Coordinator]
	var grant LeaseGrant
	if err := CoordinatorCall(ctx, co.Callback, string(ref.Host), "renew-lease", ref, j.LeasePending, j.Request.ControlKey, &grant, s.Callback); err != nil {
		return nil, err
	}
	if err := VerifyLease(grant, *j.LeasePending, ref, j.Request.Coordinator, co.PublicKey); err != nil {
		return nil, err
	}
	err = s.update(ctx, ref, func(current *Journal) error {
		if current.StopRequest != nil || current.LeasePending == nil || *current.LeasePending != *j.LeasePending {
			return errors.New("lease renewal superseded")
		}
		current.Lease, current.LeasePending = &grant, nil
		return nil
	})
	return &grant, err
}

// maintainLease runs only inside the owned runner. Expiry durably records stop
// intent before stopping the exact owned service (and all its child scopes).
// Termination is not acknowledged until the service manager observes it ended.
func (s *Server) maintainLease(ctx context.Context, ref Ref, acquired time.Time, grant *LeaseGrant) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	s.leaseLoop(ctx, ref, acquired, grant, ticker.C, time.Now)
}

func (s *Server) leaseLoop(ctx context.Context, ref Ref, acquired time.Time, grant *LeaseGrant, ticks <-chan time.Time, now func() time.Time) {
	deadline := acquired.Add(LeaseDuration)
	next := acquired.Add(LeaseRenewal)
	var pendingSince time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
			if !now().Before(deadline) {
				stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
				err := s.expireLease(stopCtx, ref, grant)
				cancel()
				if err == nil {
					return
				}
				continue
			}
			if now().Before(next) {
				continue
			}
			if pendingSince.IsZero() {
				pendingSince = now()
			}
			renewalCtx, cancel := context.WithDeadline(ctx, deadline)
			renewed, err := s.renewLease(renewalCtx, ref)
			cancel()
			if err == nil {
				// A retried response must not move the original deadline. Successful
				// retries are deliberately conservative: only a new sequence extends it.
				if renewed.Sequence > grant.Sequence {
					deadline, grant = pendingSince.Add(LeaseDuration), renewed
					pendingSince = time.Time{}
				}
			}
			next = now().Add(LeaseRenewal)
		}
	}
}

func (s *Server) expireLease(ctx context.Context, ref Ref, grant *LeaseGrant) error {
	if err := s.update(ctx, ref, func(j *Journal) error {
		if j.StopRequest == nil {
			j.StopRequest = &StopRequest{ID: "lease-expired-" + grant.Nonce, Reason: "coordinator lease expired; reconcile owned execution"}
		}
		return nil
	}); err != nil {
		// Storage failure cannot grant runtime to a disconnected process.
		if s.Launcher != nil {
			return errors.Join(err, s.Launcher.Stop(ctx, UnitName(ref)))
		}
		return err
	}
	if s.Launcher == nil {
		return errors.New("expired lease; owned supervisor unavailable")
	}
	return s.Launcher.Stop(ctx, UnitName(ref))
}
