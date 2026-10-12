package cmd

import (
	"bytes"
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/state"
)

const (
	untitledPID   = 1<<23 + 34
	unrosteredPID = 1<<23 + 35
	untitledName  = "klaus-lab-34"
	rosterName    = "BK worker"
)

// serveApp runs two sessions the desktop has not titled, named by their
// processes; the first is on the roster as rosterName.
func serveApp(t *testing.T) (*app, state.Store) {
	t.Helper()
	dir := t.TempDir()
	store, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents = []state.Agent{{Party: state.Party{Session: "s" + strconv.Itoa(untitledPID), Name: rosterName}}}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{StateDir: dir}
	cfg.Claude.SessionsDir, cfg.Claude.DesktopDir, cfg.Claude.ProjectsDir = t.TempDir(), t.TempDir(), t.TempDir()
	cfg.Omp.SessionsDir = t.TempDir()
	a := &app{cfg: cfg, store: store, now: keepNow, out: &bytes.Buffer{}, as: "test: supervisor"}
	was := plat.Machine
	t.Cleanup(func() { plat.Machine = was })
	runLive(t, a, map[int]string{untitledPID: untitledName, unrosteredPID: "klaus-lab-35"})
	return a, store
}

func serve(a *app, args ...string) (string, error) {
	c := a.serveCmd()
	usageArgs(c)
	c.SetArgs(args)
	c.SetOut(a.out)
	c.SetErr(a.out)
	c.SilenceUsage, c.SilenceErrors = true, true
	err := c.ExecuteContext(context.Background())
	return a.out.(*bytes.Buffer).String(), err
}

// An untitled session on the roster serves under its roster name, in the
// record, the command's line and the records `sessions` prints; a session
// not on the roster keeps its process name.
func TestServeRecordsTheRosterName(t *testing.T) {
	a, store := serveApp(t)
	if out, err := serve(a, untitledName, "giantswarm/beekeeper#1027"); err != nil || !strings.Contains(out, `"BK worker" serves giantswarm/beekeeper#1027`) {
		t.Fatalf("serve the roster's session: %q, %v", out, err)
	}
	if out, err := serve(a, "klaus-lab-35", "giantswarm/beekeeper#1026"); err != nil || !strings.Contains(out, `"klaus-lab-35" serves giantswarm/beekeeper#1026`) {
		t.Fatalf("serve the session off the roster: %q, %v", out, err)
	}
	st, _ := store.Read()
	if len(st.Records) != 2 || st.Records[0].Session.Name != rosterName || st.Records[1].Session.Name != "klaus-lab-35" {
		t.Fatalf("records %+v", st.Records)
	}
	sessions, _, err := a.sessions()
	if err != nil {
		t.Fatal(err)
	}
	a.out = &bytes.Buffer{}
	a.printRecords(st, st.Records, sessions)
	if s := a.out.(*bytes.Buffer).String(); !strings.Contains(s, `- "BK worker" serves giantswarm/beekeeper#1027`) || strings.Contains(s, untitledName) {
		t.Errorf("records:\n%s", s)
	}
}
