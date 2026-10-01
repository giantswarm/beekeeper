//go:build unix

package cmd

import (
	"bufio"
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/omp"
	"github.com/giantswarm/beekeeper/internal/state"
)

// A wake of an omp agent goes to its inbox while its process reads it, and
// is refused once none does: nothing resumes an omp agent.
func TestWakeOmp(t *testing.T) {
	const id = "1234abcd-0000-4000-8000-000000000000"
	dir := t.TempDir()
	store, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	a := &app{cfg: &config.Config{StateDir: dir}, store: store, out: &out}
	ag := state.Agent{Party: state.Party{HostSession: omp.HostPrefix + id, Name: "test: omp"}}
	inbox := omp.InboxPath(dir, id)
	if err := omp.MakeInbox(inbox); err != nil {
		t.Fatal(err)
	}
	if err := a.wakeOmp(state.Party{Name: "test"}, ag, id, "hello"); err == nil || !strings.Contains(err.Error(), "no longer runs") {
		t.Fatalf("wake with no reader = %v, want the refusal", err)
	}
	r, err := os.OpenFile(inbox, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	if err := a.wakeOmp(state.Party{Name: "test"}, ag, id, "hello"); err != nil {
		t.Fatal(err)
	}
	if line, _ := bufio.NewReader(r).ReadString('\n'); !strings.Contains(line, `"message":"hello"`) {
		t.Fatalf("inbox got %q", line)
	}
	if !strings.Contains(out.String(), "written to its inbox") {
		t.Fatalf("output %q", out.String())
	}
}

func TestRoleTextNamesTheHarness(t *testing.T) {
	sv := &sessionView{Session: newOmpSession()}
	if got := roleText(sv, "agent: fix it"); got != "omp busy agent: fix it" {
		t.Fatalf("roleText = %q", got)
	}
}

func newOmpSession() *claude.Session {
	return &claude.Session{Harness: omp.Harness, State: omp.StateBusy}
}
