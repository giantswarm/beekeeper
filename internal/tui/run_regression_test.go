package tui

import (
	tea "charm.land/bubbletea/v2"
	"context"
	"errors"
	"testing"
)

type exitSource struct {
	fakeSource
	contexts []context.Context
}

func (s *exitSource) Data(ctx context.Context) (*Data, error) {
	s.contexts = append(s.contexts, ctx)
	return s.data, nil
}

func (s *exitSource) Send(ctx context.Context, session, text string) (string, error) {
	s.contexts = append(s.contexts, ctx)
	return "queued", nil
}

func TestRunCancelsSourceOnEveryProgramExit(t *testing.T) {
	failure := errors.New("program interrupted")
	for _, tc := range []struct {
		name               string
		nilFinal, keyboard bool
		err                error
	}{
		{name: "normal"},
		{name: "interrupt", err: failure},
		{name: "startup error", nilFinal: true, err: failure},
		{name: "nil final model", nilFinal: true},
		{name: "keyboard", keyboard: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := &exitSource{fakeSource: fakeSource{data: fixtureData()}}
			err := runProgram(src, Options{}, func(input tea.Model) (tea.Model, error) {
				m := input.(*model)
				m.refreshCmd()()
				src.Send(m.ctx, "session", "hello")
				m.taken = map[string]bool{"owned-session": true}
				if tc.keyboard {
					m.key(keyText("q"))
				}
				if tc.nilFinal {
					return nil, tc.err
				}
				return m, tc.err
			})
			if !errors.Is(err, tc.err) {
				t.Fatalf("Run error = %v, want %v", err, tc.err)
			}
			for _, ctx := range src.contexts {
				if ctx.Err() != context.Canceled {
					t.Fatalf("Source context remains live after %s", tc.name)
				}
			}
			if len(src.released) != 1 || src.released[0] != "owned-session" {
				t.Fatalf("cleanup lost the known model: releases %v", src.released)
			}
		})
	}
}
