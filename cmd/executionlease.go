package cmd

import (
	"context"
	"errors"
	"path/filepath"
	"time"

	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/state"
)

func (a *app) renewExecutionLease(ctx context.Context, host string, req execution.CallbackRequest) (*execution.LeaseGrant, error) {
	issue := func(st *state.State) (*execution.LeaseGrant, error) {
		r, _ := executionRecord(st, req.Ref)
		if r == nil || req.Protocol != execution.Protocol || req.RequestID == "" || req.Ref.Validate() != nil || string(req.Ref.Host) != host || !execution.VerifyCallback(req, r.Request.ControlKey) {
			return nil, errors.New("unauthenticated execution callback")
		}
		h, ok := a.cfg.Agents.Hosts[r.Selector]
		if !ok || h.Identity != req.Ref.Host {
			return nil, errors.New("execution host is no longer configured")
		}
		if !r.Request.LeaseRequired || r.StopRequest != nil || r.Terminal() || r.Completed || st.Stop != nil || r.Request.Coordinator != a.cfg.Identity.Host {
			return nil, errors.New("coordinator lease renewal revoked")
		}
		var request execution.LeaseRequest
		if execution.Decode(req.Payload, &request) != nil {
			return nil, errors.New("invalid lease request")
		}
		key, err := execution.SigningKey(filepath.Join(a.cfg.StateDir, "execution-coordinator"))
		if err != nil {
			return nil, err
		}
		grant, err := execution.IssueLease(req.Ref, r.Request.Coordinator, r.Lease, request, key, time.Now())
		return &grant, err
	}
	if store, ok := a.store.(interface {
		RenewExecutionLease(context.Context, execution.Ref, func(*state.State) (*execution.LeaseGrant, error)) (*execution.LeaseGrant, error)
	}); ok {
		return store.RenewExecutionLease(ctx, req.Ref, issue)
	}
	var grant *execution.LeaseGrant
	err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		var err error
		grant, err = issue(st)
		if err == nil {
			r, _ := executionRecord(st, req.Ref)
			r.Lease = grant
		}
		return nil, err
	})
	return grant, err
}
