package tui

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

// lifecycleSource keeps work outstanding until Run cancels it.
type lifecycleSource struct {
	fakeSource
	started chan context.Context
}

func (s *lifecycleSource) Data(ctx context.Context) (*Data, error) {
	s.started <- ctx
	<-ctx.Done()
	return nil, ctx.Err()
}

func (s *lifecycleSource) Send(ctx context.Context, _, _ string) (string, error) {
	s.started <- ctx
	<-ctx.Done()
	return "", ctx.Err()
}

func (s *lifecycleSource) ReleaseOwned(ctx context.Context, id string) error {
	return s.Release(ctx, id)
}

func (s *lifecycleSource) Release(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.released = append(s.released, id)
	return nil
}

func TestRunProgramCleanup(t *testing.T) {
	startupErr := errors.New("terminal startup failed")
	for _, exit := range []struct {
		name string
		err  error
	}{{"normal", nil}, {"interrupt", tea.ErrInterrupted}, {"startup", startupErr}} {
		for _, nilFinal := range []bool{false, true} {
			for _, operation := range []string{"data", "send"} {
				name := exit.name + "/" + operation
				if nilFinal {
					name += "/nil-final"
				}
				t.Run(name, func(t *testing.T) {
					src := &lifecycleSource{started: make(chan context.Context, 1)}
					done := make(chan struct{})
					var sourceCtx context.Context
					err := runProgram(src, Options{}, func(initial tea.Model) (tea.Model, error) {
						m := initial.(*model)
						// These are known owned sessions, as recorded by successful take-overs.
						m.taken = map[string]bool{"session-one": true, "session-two": true}
						cmd := m.refreshCmd()
						if operation == "send" {
							cmd = m.sendCmd("session-one", "hello")
						}
						go func() { cmd(); close(done) }()
						select {
						case sourceCtx = <-src.started:
						case <-time.After(time.Second):
							t.Fatal("source work did not start")
						}
						if sourceCtx.Err() != nil {
							t.Fatal("source context cancelled before runner exited")
						}
						if nilFinal {
							return nil, exit.err
						}
						return initial, exit.err
					})
					if !errors.Is(err, exit.err) {
						t.Fatalf("Run error = %v, want %v", err, exit.err)
					}
					if !errors.Is(sourceCtx.Err(), context.Canceled) {
						t.Errorf("source context error = %v, want canceled", sourceCtx.Err())
					}
					select {
					case <-done:
					case <-time.After(time.Second):
						t.Error("source work still running after Run returned")
					}
					slices.Sort(src.released)
					if !slices.Equal(src.released, []string{"session-one", "session-two"}) {
						t.Errorf("released = %v, want both owned sessions exactly once", src.released)
					}
				})
			}
		}
	}
}
