package state

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/gofrs/flock"

	"github.com/giantswarm/beekeeper/internal/clock"
	"github.com/giantswarm/beekeeper/internal/execution"
)

// commitLock serializes the durable state boundary, not desk callbacks. The
// order is state.lock then state.commit.lock; renewal takes only the latter.
// Never run network calls or command callbacks while holding this lock. A
// transaction's commit neither decodes nor encodes the whole state under it:
// a renewal waits commitWait at most. A renewal itself still reads, decodes
// and writes the whole state under it (RenewExecutionLease).
func (s *FileStore) commitLock(ctx context.Context) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	timing := startLockClock(commitLockName, false)
	ctx, cancel := context.WithTimeout(ctx, commitWait)
	defer cancel()
	l := flock.New(s.path("state.commit.lock"))
	ok, err := l.TryLockContext(ctx, 10*time.Millisecond)
	if err != nil {
		timing.busy()
		return nil, errors.Join(execution.ErrCoordinatorBusy, err)
	}
	if !ok {
		timing.busy()
		return nil, execution.ErrCoordinatorBusy
	}
	timing.acquire()
	return func() {
		_ = l.Unlock()
		timing.release()
	}, nil
}

// commitWait bounds every wait for the commit lock.
const commitWait = 5 * time.Second

// RenewExecutionLease changes only the lease of an authenticated execution.
// The issuer sees the authoritative, locked state, including global stop and
// generation revocation. Ordinary writers merge concurrent renewals at commit.
// An interrupted event outbox is preserved verbatim, never flushed here.
func (s *FileStore) RenewExecutionLease(ctx context.Context, ref execution.Ref, issue func(*State) (*execution.LeaseGrant, error)) (*execution.LeaseGrant, error) {
	unlock, err := s.commitLock(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	st, err := s.load()
	if err != nil {
		return nil, err
	}
	if err := s.checkRecoveryFence(); err != nil {
		return nil, err
	}
	// Renewal does not own the event log's desk lock. Refuse stale writers
	// without appending an audit event alongside another writer's outbox.
	if own, ok := release(s.version); ok && st.Writer != nil {
		if newer, ok := release(st.Writer.Version); ok && own.LessThan(newer) {
			return nil, &StaleWriterError{Version: s.version, Newer: st.Writer.Version}
		}
	}
	// Issue against a separate copy: the callback cannot change stop, the
	// execution registration, or any other part of the saved document.
	view, err := s.load()
	if err != nil {
		return nil, err
	}
	grant, err := issue(view)
	if err != nil {
		return nil, err
	}
	if grant == nil || grant.Ref != ref {
		return nil, errors.New("invalid execution lease grant")
	}
	for i := range st.Starts {
		r := st.Starts[i].Execution
		if r != nil && r.Request.Ref == ref {
			r.Lease = grant
			if err := s.stamp(st, clock.Now()); err != nil {
				return nil, err
			}
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			return grant, writeJSON(s.path("state.json"), st)
		}
	}
	return nil, errors.New("execution lease registration missing")
}

// preserveRenewals runs under both locks. A slow callback may have read an
// earlier sequence. Carry forward only grants for its exact registration;
// explicit lease edits, deletion, stop and a replacement generation still commit
// atomically. Only a lease left unchanged by the callback can be carried forward.
// While state.json is still base, the file the transaction read, no renewal
// replaced it and its writer was judged at begin: nothing is read again.
// Otherwise only the writer and the leases are decoded, not the whole document.
// renewed says a grant was carried into st.
func (s *FileStore) preserveRenewals(st *State, leasesBefore map[execution.Ref]execution.LeaseGrant, base *os.File) (renewed bool, err error) {
	if err := s.checkRecoveryFence(); err != nil {
		return false, err
	}
	if unchangedSince(base, s.path("state.json")) {
		return false, nil
	}
	current, err := s.readLeases()
	if err != nil {
		return false, err
	}
	if err := s.checkWriterVersion(current.writer(), clock.Now()); err != nil {
		return false, err
	}
	leases := make(map[execution.Ref]leaseRecord)
	for _, start := range current.Starts {
		if r := start.Execution; r != nil && r.Lease != nil {
			leases[r.Request.Ref] = *r
		}
	}
	for _, start := range st.Starts {
		r := start.Execution
		if r == nil {
			continue
		}
		before, hadLease := leasesBefore[r.Request.Ref]
		if (r.Lease == nil && hadLease) || (r.Lease != nil && (!hadLease || *r.Lease != before)) {
			continue
		}
		latest, ok := leases[r.Request.Ref]
		if ok && latest.RequestDigest == r.RequestDigest && (r.Lease == nil || latest.Lease.Sequence > r.Lease.Sequence) {
			r.Lease = latest.Lease
			renewed = true
		}
	}
	return renewed, nil
}

// unchangedSince says path is still the file base is open on, unchanged: an
// open descriptor keeps its inode from being reused by a later file.
func unchangedSince(base *os.File, path string) bool {
	if base == nil {
		return false
	}
	was, err := base.Stat()
	if err != nil {
		return false
	}
	now, err := os.Stat(path)
	return err == nil && sameSnapshot(was, now)
}

// leaseView is what preserveRenewals reads of a state a renewal replaced: the
// writer and the execution leases, without allocating every other record.
type leaseView struct {
	Writer *struct {
		Version string `json:"version"`
	} `json:"writer"`
	Starts []struct {
		Execution *leaseRecord `json:"execution"`
	} `json:"starts"`
}

type leaseRecord struct {
	Request struct {
		Ref execution.Ref `json:"ref"`
	} `json:"request"`
	RequestDigest string                `json:"requestDigest"`
	Lease         *execution.LeaseGrant `json:"lease"`
}

func (v *leaseView) writer() string {
	if v.Writer == nil {
		return ""
	}
	return v.Writer.Version
}

func (s *FileStore) readLeases() (*leaseView, error) {
	raw, err := os.ReadFile(s.path("state.json"))
	if errors.Is(err, os.ErrNotExist) {
		return &leaseView{}, nil
	}
	if err != nil {
		return nil, err
	}
	v := &leaseView{}
	if err := json.Unmarshal(raw, v); err != nil {
		return nil, fmt.Errorf("%s: %w", s.path("state.json"), err)
	}
	return v, nil
}
