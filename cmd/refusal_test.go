package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/state"
)

// An upstream refusal is a hook.refused event, and the watch says each one
// logged after its first reading once.
func TestWatchSaysUpstreamRefusals(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("stateDir: "+dir+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(cfg.GitHub.AllowedOrgs, ","); got != "giantswarm" {
		t.Errorf("github.allowedOrgs defaults to %q, want giantswarm", got)
	}
	store, err := state.Open(cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	a := &app{cfg: cfg, store: store, out: &out}
	a.recordRefusal("s0", "gh pr create -R o/before --fill", "o/before")
	a.now = time.Now()
	w := &watcher{app: a, last: map[string]time.Time{}}
	w.refusals() // the baseline
	time.Sleep(time.Millisecond)
	a.recordRefusal("s1", "gh pr create -R o/r --fill", "o/r")
	w.refusals()
	w.refusals()
	s := out.String()
	if strings.Count(s, "REFUSED ") != 1 || !strings.Contains(s, "upstream write to o/r: gh pr create -R o/r --fill") || strings.Contains(s, "o/before") {
		t.Errorf("watch:\n%s", s)
	}
	evs, err := store.Events(0, func(e state.Event) bool { return e.Verb == verbHookRefused })
	if err != nil || len(evs) != 2 || evs[1].By.Session != "s1" {
		t.Errorf("hook.refused events %+v, %v", evs, err)
	}
}
