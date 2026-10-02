//go:build unix

package cmd

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/omp"
	"github.com/giantswarm/beekeeper/internal/state"
)

// The screen's message reaches an omp agent beekeeper started in its
// inbox, stamped as the person's; an omp session the person runs in its
// own terminal, and two CLIs under one name, are refused.
func TestMessageSession(t *testing.T) {
	const id = "5678abcd-0000-4000-8000-000000000000"
	dir := t.TempDir()
	store, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{StateDir: dir}
	cfg.Guide.Person = tPerson
	a := &app{cfg: cfg, store: store, out: &bytes.Buffer{}}
	inbox := omp.InboxPath(dir, id)
	if err := omp.MakeInbox(inbox); err != nil {
		t.Fatal(err)
	}
	r, err := os.OpenFile(inbox, os.O_RDWR, 0) //nolint:gosec // the test's inbox
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()

	agent := &claude.Session{Name: "test: omp agent", Harness: omp.Harness, HostID: omp.HostPrefix + id}
	where, err := a.messageSession(context.Background(), []*claude.Session{agent}, agent, " look at #12 ")
	if err != nil || !strings.Contains(where, "omp inbox") {
		t.Fatalf("message to an omp agent = %q, %v", where, err)
	}
	if line, _ := bufio.NewReader(r).ReadString('\n'); !strings.Contains(line, `"message":"From Grace through beekeeper ui: look at #12"`) {
		t.Fatalf("inbox got %q, want the person's stamped message", line)
	}
	evs, err := store.Events(10, nil)
	if err != nil || len(evs) != 1 || evs[0].Verb != "ui.message" || evs[0].By.Name != tPerson {
		t.Errorf("events = %+v, %v, want one ui.message by Grace", evs, err)
	}

	own := &claude.Session{Name: "omp in lab", Harness: omp.Harness}
	if _, err := a.messageSession(context.Background(), []*claude.Session{own}, own, "hi"); err == nil || !strings.Contains(err.Error(), "did not start") {
		t.Errorf("message to the person's own omp = %v, want the refusal", err)
	}

	twin := []*claude.Session{{Name: tBeeName, PID: 1}, {Name: tBeeName, PID: 2}}
	if _, err := a.messageSession(context.Background(), twin, twin[0], "hi"); err == nil || !strings.Contains(err.Error(), "2 running CLIs") {
		t.Errorf("message to one of two namesakes = %v, want the refusal", err)
	}
}
