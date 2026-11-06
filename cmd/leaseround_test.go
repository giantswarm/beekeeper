package cmd

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/lease"
)

func TestLeaseClaimWaitNamesTheOverrun(t *testing.T) {
	a := claimApp(t, claimGrant(graveler, claimMe, time.Minute))
	h := lease.Holder{Env: graveler, Name: claimOther.Name, Purpose: "60-minute round", Since: a.now.Add(-78 * time.Minute).Format(time.RFC3339)}
	if _, err := lease.Dir(a.cfg.LeaseDir).Claim(graveler, h); err != nil {
		t.Fatal(err)
	}
	was := claimPause
	t.Cleanup(func() { claimPause = was })
	claimPause = func(context.Context, time.Duration) error { return nil }
	out, err := runLease(a, "claim", graveler, "-p", "next proof", "--wait", "1m")
	for _, want := range []string{"queued: number 1 behind", claimOther.Name, "since", "overrun 19m past its 1h00m round", "waited 1m"} {
		if !strings.Contains(out, want) {
			t.Errorf("claim output %q lacks %q", out, want)
		}
	}
	if Code(err) != ExitRefused {
		t.Errorf("claim exit = %d, want refused", Code(err))
	}
	if cur, err := lease.Dir(a.cfg.LeaseDir).Get(graveler); err != nil || cur == nil || cur.Name != h.Name || cur.Since != h.Since {
		t.Errorf("queued claim changed the holder: %+v, %v", cur, err)
	}
}

func TestWatchLeaseRound(t *testing.T) {
	a := claimApp(t)
	h := lease.Holder{Env: graveler, Name: claimOther.Name, Purpose: "60-minute round", Since: a.now.Add(-time.Hour).Format(time.RFC3339)}
	if _, err := lease.Dir(a.cfg.LeaseDir).Claim(graveler, h); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	a.out = &out
	w := &watcher{app: a, reported: map[string]bool{}, last: map[string]time.Time{}}
	w.staleLeases(context.Background(), nil)
	if out.Len() != 0 {
		t.Fatalf("not past round: %q", out.String())
	}
	a.now = a.now.Add(18 * time.Minute)
	w.staleLeases(context.Background(), nil)
	for _, want := range []string{"LEASE HELD PAST ROUND:", graveler, claimOther.Name, "overrun 18m past its 1h00m round"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("watch output %q lacks %q", out.String(), want)
		}
	}
	out.Reset()
	a.now = a.now.Add(time.Minute)
	w.staleLeases(context.Background(), nil)
	if out.Len() != 0 {
		t.Errorf("watch repeats the same overdue lease: %q", out.String())
	}
	h.Since = a.now.Add(-2 * time.Hour).Format(time.RFC3339)
	if err := lease.Dir(a.cfg.LeaseDir).Hand(h); err != nil {
		t.Fatal(err)
	}
	w.staleLeases(context.Background(), nil)
	if !strings.Contains(out.String(), "LEASE HELD PAST ROUND:") {
		t.Errorf("new claim not reported: %q", out.String())
	}
	if err := lease.Dir(a.cfg.LeaseDir).Release(graveler); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	w.staleLeases(context.Background(), nil)
	if !strings.Contains(out.String(), "ENDED") {
		t.Errorf("released lease still active: %q", out.String())
	}
	if len(w.active) != 0 {
		t.Errorf("released lease left active conditions: %+v", w.active)
	}
}

func TestLeaseClaimUsesConfiguredRound(t *testing.T) {
	a := claimApp(t, claimGrant(graveler, claimMe, time.Minute))
	a.cfg.LeaseRound.Duration = 5 * time.Minute
	holdStartingLease(t, a, graveler, claimOther)
	out, err := runLease(a, "claim", graveler, "-p", "next proof")
	if Code(err) != ExitRefused || !strings.Contains(out, "overrun 5m past its 5m round") {
		t.Errorf("configured round: %q, %v", out, err)
	}
}
