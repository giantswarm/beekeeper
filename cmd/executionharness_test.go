package cmd

import (
	"errors"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/state"
)

func TestExecutionHarnessKeepsLocalDispatchAndRefusesUnsupportedRemoteCalls(t *testing.T) {
	base := Harness(claudeHarness{})
	for _, selector := range []string{"", executionLocal} {
		h, err := executionHarness(base, executionStartOptions{Host: selector}, "")
		if err != nil || h != base {
			t.Fatal("local harness dispatch changed")
		}
	}
	if _, err := executionHarness(agyHarness{}, executionStartOptions{Host: sshTestSelector}, ""); err == nil {
		t.Fatal("unsupported remote harness admitted")
	}
	h, err := executionHarness(base, executionStartOptions{Host: sshTestSelector}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Wake(nil, state.Party{}, state.Agent{}, "", ""); !errors.Is(err, execution.ErrUnsupported) {
		t.Fatal("remote wake fell through to a local harness")
	}
	if err := h.Send(nil, "", "", ""); !errors.Is(err, execution.ErrUnsupported) {
		t.Fatal("remote message fell through to a local inbox")
	}
	if sessions := h.Discover(nil, nil, time.Now()); len(sessions) != 0 {
		t.Fatal("SSH adapter discovered local sessions")
	}
	if _, err := h.Follow("", 1); err == nil {
		t.Fatal("remote transcript attempted a coordinator filesystem read")
	}
}
