package execution

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gofrs/flock"
	"github.com/google/uuid"

	"github.com/giantswarm/beekeeper/internal/proc"
)

func NewControlKey() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b), nil
}
func validControlKey(s string) bool {
	b, err := base64.StdEncoding.DecodeString(s)
	return err == nil && len(b) == 32
}

type CallbackRequest struct {
	WireRequest
	Proof string `json:"proof"`
}

func callbackProof(r WireRequest, key string) string {
	b, _ := base64.StdEncoding.DecodeString(key)
	m := hmac.New(sha256.New, b)
	_, _ = m.Write(mustJSON(r))
	return base64.StdEncoding.EncodeToString(m.Sum(nil))
}

func VerifyCallback(r CallbackRequest, key string) bool {
	if !validControlKey(key) {
		return false
	}
	return hmac.Equal([]byte(r.Proof), []byte(callbackProof(r.WireRequest, key)))
}

func CoordinatorCall(ctx context.Context, endpoint Endpoint, host, op string, ref Ref, payload any, key string, result any, run CommandRunner) error {
	if err := endpoint.Validate(); err != nil {
		return err
	}
	if !ValidIdentity(host) || !validControlKey(key) {
		return errors.New("invalid execution callback identity")
	}
	p, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	r := CallbackRequest{WireRequest: WireRequest{Protocol: Protocol, RequestID: newID(), Operation: op, Ref: ref, Payload: p}}
	r.Proof = callbackProof(r.WireRequest, key)
	b, err := json.Marshal(r)
	if err != nil || len(b) > MaxWire {
		return errors.New("invalid callback size")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if run == nil {
		run = SSHCommand
	}
	raw, err := run(ctx, SSHArgv(endpoint, 10*time.Second, endpointCommand(endpoint, "coordinator-rpc", "--host", host)), b)
	if err != nil {
		return &UnreachableError{Operation: op, Cause: err}
	}
	var reply WireResponse
	if Decode(raw, &reply) != nil || reply.Protocol != Protocol || reply.RequestID != r.RequestID || reply.Ref != ref {
		return errors.New("invalid coordinator callback response")
	}
	if reply.Error != "" {
		return errors.New(reply.Error)
	}
	return Decode(reply.Payload, result)
}

// Caller verifies process ownership through the service cgroup. Environment
// variables alone cannot impersonate a different worker on the same host.
func (s *Server) Caller(id string) (Journal, error) {
	var j Journal
	if _, err := uuid.Parse(id); err != nil {
		return j, errors.New("invalid calling execution")
	}
	if err := readJSON(filepath.Join(s.Root, id+".json"), &j); err != nil {
		return j, err
	}
	j, err := s.Read(j.Handle.Ref)
	if err != nil {
		return j, err
	}
	unit := filepath.Base(proc.Cgroup(os.Getpid()))
	if s.OwnUnit != nil {
		unit = s.OwnUnit()
	}
	if unit != j.Handle.Unit {
		// A capped build scope retains ancestry even though its cgroup changes.
		// The recorded start time prevents a reused PID impersonating a runner.
		table, err := proc.Read()
		owned := false
		if err == nil && j.RunnerPID > 0 {
			for p := table.ByPID[os.Getpid()]; p != nil; p = table.ByPID[p.PPID] {
				if p.PID == j.RunnerPID && p.Start.Equal(j.RunnerStarted) {
					owned = true
					break
				}
				if p.PID == p.PPID {
					break
				}
			}
		}
		if !owned {
			return j, errors.New("calling process does not belong to this execution's service")
		}
	}
	return j, nil
}

type outbox struct {
	Next    uint64          `json:"next"`
	Pending *ControlRequest `json:"pending,omitempty"`
}

// SendControl stores a mutation before contacting the coordinator. A caller
// that retries identical data receives the original receipt, including a note
// number. Different work cannot overtake an unacknowledged mutation.
func (s *Server) SendControl(ctx context.Context, j Journal, op string, data any, id string, run CommandRunner) (ControlResult, error) {
	var result ControlResult
	if op != ControlNote && op != ControlPark && op != ControlReport && op != "handover" && op != "event" {
		return result, errors.New("unsupported remote worker control operation")
	}
	if id != "" && !ValidIdentity(id) {
		return result, errors.New("invalid control request idempotency key")
	}
	if _, err := s.InstalledPolicy(); err != nil {
		return result, err
	}
	b, err := json.Marshal(data)
	if err != nil || len(b) > MaxWire/2 {
		return result, errors.New("invalid worker control request")
	}
	dir := filepath.Join(s.Root, "control", string(j.Handle.Ref.Execution))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return result, err
	}
	p := filepath.Join(dir, "queue.json")
	l := flock.New(p + ".lock")
	ok, err := l.TryLockContext(ctx, 20*time.Millisecond)
	if err != nil || !ok {
		return result, errors.New("execution control outbox is busy")
	}
	defer func() { _ = l.Unlock() }()
	var q outbox
	if err := readJSON(p, &q); err != nil && !errors.Is(err, os.ErrNotExist) {
		return result, err
	}
	want := ControlRequest{Ref: j.Handle.Ref, Operation: op, Data: b}
	receiptPath := func(id string) string {
		return filepath.Join(dir, strings.TrimPrefix(Digest([]byte(id)), "sha256:")+".receipt")
	}
	// A receipt is synced before removing the pending request. Recover that
	// boundary after a host crash without resending an acknowledged mutation.
	if q.Pending != nil {
		var old ControlResult
		if err := readJSON(receiptPath(q.Pending.ID), &old); err == nil && old.ID == q.Pending.ID && old.Sequence == q.Pending.Sequence && old.Digest == q.Pending.ContentDigest() {
			q.Pending = nil
			if err := atomicFile(p, mustJSON(q)); err != nil {
				return result, err
			}
		}
	}
	if id != "" {
		var old ControlResult
		if err := readJSON(receiptPath(id), &old); err == nil {
			if old.ID != id || old.Digest != want.ContentDigest() {
				return result, errors.New("control idempotency key reused with different content")
			}
			return old, controlError(old)
		} else if !errors.Is(err, os.ErrNotExist) {
			return result, err
		}
	}
	if q.Pending != nil {
		if q.Pending.Operation != op || string(q.Pending.Data) != string(b) || id != "" && id != q.Pending.ID {
			return result, errors.New("an earlier control request is pending; run agents hosts sync before changing the request")
		}
		want = *q.Pending
	} else {
		q.Next++
		if id == "" {
			id = newID()
		}
		want.Sequence, want.ID = q.Next, id
		q.Pending = &want
		if err := atomicFile(p, mustJSON(q)); err != nil {
			return result, err
		}
	}
	co, ok := s.Config.Coordinators[j.Request.Coordinator]
	if !ok {
		return result, errors.New("execution coordinator is no longer trusted")
	}
	if err := CoordinatorCall(ctx, co.Callback, string(j.Handle.Ref.Host), "control", j.Handle.Ref, want, j.Request.ControlKey, &result, run); err != nil {
		return result, err
	}
	if result.ID != want.ID || result.Sequence != want.Sequence || result.Digest != want.ContentDigest() {
		return ControlResult{}, errors.New("control receipt mismatch; request remains pending")
	}
	if err := atomicFile(receiptPath(want.ID), mustJSON(result)); err != nil {
		return ControlResult{}, err
	}
	q.Pending = nil
	if err := atomicFile(p, mustJSON(q)); err != nil {
		return ControlResult{}, err
	}
	return result, controlError(result)
}

func controlError(r ControlResult) error {
	if r.Error != "" {
		return errors.New(r.Error)
	}
	return nil
}

func (s *Server) SyncControl(ctx context.Context, r Ref, run CommandRunner) (ControlResult, error) {
	j, err := s.Read(r)
	if err != nil {
		return ControlResult{}, err
	}
	var q outbox
	p := filepath.Join(s.Root, "control", string(r.Execution), "queue.json")
	if err := readJSON(p, &q); errors.Is(err, os.ErrNotExist) {
		return ControlResult{}, nil
	} else if err != nil {
		return ControlResult{}, err
	}
	if q.Pending == nil {
		return ControlResult{}, nil
	}
	return s.SendControl(ctx, j, q.Pending.Operation, q.Pending.Data, q.Pending.ID, run)
}
