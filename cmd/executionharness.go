package cmd

import (
	"context"
	"errors"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
)

// sshHarness decorates the selected coding harness with an execution host.
// It is never a second provider, subscription pool or local conversation.
type sshHarness struct {
	Harness
	options   executionStartOptions
	briefPath string
	ref       *execution.Ref
}

var _ Harness = sshHarness{}

func executionHarness(base Harness, options executionStartOptions, briefPath string) (Harness, error) {
	if options.Host == "" || options.Host == executionLocal {
		return base, nil
	}
	if base == nil || base.Name() != "" {
		return nil, refused("SSH execution supports the Claude harness in this slice")
	}
	return sshHarness{Harness: base, options: options, briefPath: briefPath}, nil
}

func (h sshHarness) Start(ctx context.Context, a *app, sp agentStart) error {
	o := h.options
	if sp.model != "" {
		o.Model = sp.model
	}
	if sp.task != "" {
		o.Task = sp.task
	}
	return a.startSSHExecution(ctx, sp.name, h.briefPath, o)
}
func (h sshHarness) DryRun(ctx context.Context, a *app, sp agentStart, _ []string) error {
	h.options.DryRun = true
	return h.Start(ctx, a, sp)
}
func (sshHarness) Wake(*app, state.Party, state.Agent, string, string) error {
	return execution.ErrUnsupported
}
func (sshHarness) Send(*app, string, string, string) error { return execution.ErrUnsupported }
func (h sshHarness) Stop(ctx context.Context, a *app, _ string) string {
	if h.ref == nil {
		return "SSH execution reference is missing"
	}
	if err := a.stopExecution(ctx, *h.ref, "owned harness stop"); err != nil {
		return err.Error()
	}
	return ""
}
func (sshHarness) Discover(*config.Config, *proc.Table, time.Time) []*claude.Session    { return nil }
func (sshHarness) Ended(*config.Config, []*claude.Session, time.Time) []*claude.Session { return nil }
func (sshHarness) Tail(string, int) ([]claude.Turn, error) {
	return nil, errors.New("remote transcript access is unavailable; use agents logs")
}
func (h sshHarness) Follow(path string, n int) ([]claude.Turn, error) { return h.Tail(path, n) }
func (sshHarness) ReadTranscript(*app, *claude.Session, time.Time) (claude.Work, claude.Activity) {
	return claude.Work{}, claude.Activity{}
}
func (sshHarness) SpendTranscripts(*config.Config, time.Time, time.Time) ([]claude.SpendTranscript, error) {
	return nil, nil
}
