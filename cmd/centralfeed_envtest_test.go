package cmd

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/central/centraltest"
)

// feedWatch is ana's watch of the central feed, keeping its mark in her
// state as a session's watch does, running until stop returns.
func (p *person) feedWatch(out *syncBuffer) (w *watcher, stop func()) {
	p.t.Helper()
	a := &app{cfgPath: p.cfg, as: p.agent, out: out}
	if err := a.load(); err != nil {
		p.t.Fatal(err)
	}
	w = a.newWatcher(false, true)
	if w.markFile == "" {
		p.t.Fatal("the watch keeps no mark")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.watchFeed(ctx)
	}()
	return w, func() { cancel(); <-done }
}

// soon fails the test unless cond holds within 10s.
func soon(t *testing.T, what string, cond func() bool) {
	t.Helper()
	if !eventually(10*time.Second, cond) {
		t.Fatalf("%s: not within 10s", what)
	}
}

// The watch prints the central feed's lines through muster as the person,
// each with its actor's person and host, once each across a restart: a
// watch with no mark starts at the newest event, a restarted one prints
// what happened while it was down and nothing it printed before.
func TestCentralFeedEnvtest(t *testing.T) {
	e := newServeEnv(t)
	m := centraltest.New(t, e.url, "beekeeper")
	ana := newPerson(t, m, e.token(t, anaEmail, teamGroup), anaEmail, ourTeam, "ana-laptop", anaAgent)
	pia := newPerson(t, m, e.token(t, piaEmail, "giantswarm:team-planeteers"), piaEmail, piaTeam, "pia-laptop", "pia-agent")

	if out, code := ana.run("lease", "claim", glean, "-p", "before the watch"); code != 0 {
		t.Fatalf("ana's claim: exit %d: %s", code, out)
	}
	var lines syncBuffer
	w, stop := ana.feedWatch(&lines)
	feedID := func() string { w.mu.Lock(); defer w.mu.Unlock(); return w.feedID }
	soon(t, "the first read", func() bool { return feedID() != "" })
	if strings.Contains(lines.String(), "CENTRAL ") {
		t.Errorf("a watch without a mark printed the past:\n%s", lines.String())
	}

	claim := "CENTRAL lease.claim (pia-agent): " + graveler
	if out, code := pia.run("lease", "claim", graveler, "-p", "e2e"); code != 0 {
		t.Fatalf("pia's claim: exit %d: %s", code, out)
	}
	soon(t, "the claim's update", func() bool { return m.Updated(resourceFeed) == 1 })
	soon(t, "the claim's line", func() bool { return strings.Contains(lines.String(), claim) })
	if line := lines.String(); !strings.Contains(line, "["+piaEmail+" on pia-laptop]") {
		t.Errorf("the claim's line names no person and host:\n%s", line)
	}
	m.Updated(resourceFeed)
	stop()

	// Down: pia releases; the restarted watch prints the release once and
	// the claim not again.
	if out, code := pia.run("lease", "release", graveler); code != 0 {
		t.Fatalf("pia's release: exit %d: %s", code, out)
	}
	var again syncBuffer
	w2, stop2 := ana.feedWatch(&again)
	defer stop2()
	if w2.feedID == "" {
		t.Fatal("the restarted watch read no last id from its mark")
	}
	release := "CENTRAL lease.release (pia-agent): " + graveler
	soon(t, "the release's line", func() bool { return strings.Contains(again.String(), release) })
	m.Updated(resourceFeed)
	time.Sleep(200 * time.Millisecond)
	if all := lines.String() + again.String(); strings.Count(all, claim) != 1 || strings.Count(all, release) != 1 {
		t.Errorf("each line once across the restart:\n--- first watch\n%s--- restarted\n%s", lines.String(), again.String())
	}

	// Unreachable: a muster that does not answer is the CENTRAL
	// UNREACHABLE line.
	dead := centraltest.New(t, e.url, "beekeeper")
	dead.Close()
	var down syncBuffer
	_, stop3 := newPerson(t, dead, "token", anaEmail, ourTeam, "ana-laptop", anaAgent).feedWatch(&down)
	defer stop3()
	soon(t, "the unreachable line", func() bool { return strings.Contains(down.String(), "CENTRAL UNREACHABLE lab: ") })
}
