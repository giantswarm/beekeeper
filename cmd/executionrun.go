package cmd

import (
	"context"

	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/guard"
)

func (a *app) executionRunOptions(ctx context.Context, options *guard.Options) error {
	if a.managedExecutionID() == "" {
		return nil
	}
	s, j, err := a.executionCaller()
	if err != nil {
		return err
	}
	p, err := s.InstalledPolicy()
	if err != nil {
		return err
	}
	if p.Digest() != j.Request.PolicyDigest {
		return refused("execution policy changed before the capped run")
	}
	var authority executionAuthority
	co := s.Config.Coordinators[j.Request.Coordinator]
	if err := execution.CoordinatorCall(ctx, co.Callback, string(j.Handle.Ref.Host), "guard", j.Handle.Ref, nil, j.Request.ControlKey, &authority, s.Callback); err != nil {
		return err
	}
	if authority.Stopped {
		return refused("coordinator stopped this execution before the capped run")
	}
	options.StopWith = j.Handle.Unit
	return nil
}
