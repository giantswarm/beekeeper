package state

import (
	"context"
	"errors"
	"time"

	"github.com/gofrs/flock"

	"github.com/giantswarm/beekeeper/internal/clock"
	"github.com/giantswarm/beekeeper/internal/execution"
)

// commitLock serializes the durable state boundary, not desk callbacks. The
// order is state.lock then state.commit.lock; renewal takes only the latter.
// Never run network calls or command callbacks while holding this lock.
func (s *FileStore) commitLock(ctx context.Context) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	l := flock.New(s.path("state.commit.lock"))
	ok, err := l.TryLockContext(ctx, 10*time.Millisecond)
	if err != nil {
		return nil, errors.Join(execution.ErrCoordinatorBusy, err)
	}
	if !ok {
		return nil, execution.ErrCoordinatorBusy
	}
	return func() { _ = l.Unlock() }, nil
}

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
func (s *FileStore) preserveRenewals(st *State, leasesBefore map[execution.Ref]execution.LeaseGrant) error {
	current, err := s.load()
	if err != nil {
		return err
	}
	if err := s.checkWriter(current, clock.Now()); err != nil {
		return err
	}
	leases := make(map[execution.Ref]*ExecutionRecord)
	for _, start := range current.Starts {
		if r := start.Execution; r != nil && r.Lease != nil {
			leases[r.Request.Ref] = r
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
		latest := leases[r.Request.Ref]
		if latest != nil && latest.RequestDigest == r.RequestDigest && (r.Lease == nil || latest.Lease.Sequence > r.Lease.Sequence) {
			r.Lease = latest.Lease
		}
	}
	return nil
}
