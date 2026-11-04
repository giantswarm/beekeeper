package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/lease"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/teleport"
)

func TestTeleportView(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.Local)
	p := func(left time.Duration) teleport.Profile {
		return teleport.Profile{Cluster: "login.example.com", Username: "ada", ValidUntil: now.Add(left)}
	}
	failed := teleport.Record{FailedAt: now.Add(-time.Minute), FailedFor: now.Add(50 * time.Minute), Reason: "tsh login did not complete within 3m0s"}
	cases := []struct {
		name string
		p    teleport.Profile
		perr error
		rec  teleport.Record
		key  string
		line string
	}{
		{"valid", p(7 * time.Hour), nil, teleport.Record{RenewedAt: now.Add(-5 * time.Hour)}, "", "teleport: ada@login.example.com valid until 19:00 (7h00m left), renewed 07:00"},
		{"under the warning", p(45 * time.Minute), nil, teleport.Record{}, teleportWarn, "TELEPORT LOGIN expires 12:45 (45m left): " + teleportRenewHint},
		{"the keeper waits", p(45 * time.Minute), nil, teleport.Record{Waiting: `the browser lease is held by "Agent one" (proof)`, WaitingAt: now.Add(-time.Minute)}, teleportWarn,
			`TELEPORT LOGIN expires 12:45 (45m left): the keeper's renewal waits (the browser lease is held by "Agent one" (proof))`},
		{"expired", p(-time.Minute), nil, teleport.Record{}, teleportExpired, "TELEPORT LOGIN EXPIRED at 11:59: installation reads fail: " + teleportRenewHint},
		{"not logged in", teleport.Profile{}, teleport.ErrNotLoggedIn, teleport.Record{}, teleportExpired, "TELEPORT LOGIN none: not logged in; installation reads fail"},
		{"failed", p(50 * time.Minute), nil, failed, teleportFailed, "TELEPORT RENEWAL FAILED at 11:59: tsh login did not complete within 3m0s; the login expires 12:50: " + teleportRenewHint},
		{"failed for an older login", p(11 * time.Hour), nil, failed, "", "teleport: ada@login.example.com valid until 23:00"},
		{"unreadable", teleport.Profile{}, errors.New("tsh status: exit status 2"), teleport.Record{}, teleportUnreadable, "TELEPORT LOGIN unreadable: tsh status: exit status 2"},
	}
	for _, c := range cases {
		v := newTeleportView(c.p, c.perr, c.rec, now, time.Hour)
		if v.Key != c.key || !strings.HasPrefix(v.line(now), c.line) {
			t.Errorf("%s: %q %q, want %q %q", c.name, v.Key, v.line(now), c.key, c.line)
		}
	}
}

func TestSnapshotDiffSaysTheTeleportLogin(t *testing.T) {
	at := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	prev := &snapshot{At: at, Teleport: &teleportView{Cluster: "c", Username: "u", ValidUntil: at.Add(2 * time.Hour)}}
	cur := &snapshot{At: at.Add(time.Minute), Teleport: &teleportView{Cluster: "c", Username: "u", ValidUntil: at.Add(2 * time.Hour)}}
	if d := diffSnapshots(prev, cur); len(d) != 0 {
		t.Errorf("unchanged login: %q", d)
	}
	cur.Teleport.Key = teleportWarn
	if d := diffSnapshots(prev, cur); len(d) != 1 || !strings.HasPrefix(d[0], "TELEPORT LOGIN expires") {
		t.Errorf("warning: %q", d)
	}
}

// teleportApp is an app with guide.person Pat, a scratch store and lease
// directory, as the keeper (no session) unless as says otherwise.
func teleportApp(t *testing.T) *app {
	t.Helper()
	a, _ := noteApp(t)
	a.as = ""
	a.cfg.LeaseDir = t.TempDir()
	a.cfg.GrantTTL.Duration = 30 * time.Minute
	return a
}

func TestHoldBrowser(t *testing.T) {
	keeper := state.Party{Name: teleportKeeper}
	a := teleportApp(t)
	dir := lease.Dir(a.cfg.LeaseDir)

	release, err := a.holdBrowser(keeper)
	if err != nil {
		t.Fatal(err)
	}
	if h, _ := dir.Get(config.Browser); h == nil || h.Name != keeper.Name || h.Purpose != teleportPurpose {
		t.Fatalf("holder %+v", h)
	}
	release()
	if h, _ := dir.Get(config.Browser); h != nil {
		t.Fatalf("still held by %+v", h)
	}

	session := state.Party{Session: "s1", Name: agentOne}
	if _, err := a.holdBrowser(session); err == nil || !strings.Contains(err.Error(), "claim the browser lease first") {
		t.Errorf("a session without the lease: %v", err)
	}
	if _, err := dir.Claim(config.Browser, lease.Holder{Env: config.Browser, Session: "s1", Name: agentOne, Purpose: "proof"}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.holdBrowser(keeper); err == nil || !strings.Contains(err.Error(), `held by "Agent one" (proof)`) {
		t.Errorf("held by a session: %v", err)
	}
	release, err = a.holdBrowser(session)
	if err != nil {
		t.Fatalf("the session's own lease: %v", err)
	}
	release()
	if h, _ := dir.Get(config.Browser); h == nil || h.Name != agentOne {
		t.Errorf("a renewal gave back the session's own lease: %+v", h)
	}
	if err := dir.Release(config.Browser); err != nil {
		t.Fatal(err)
	}

	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.Grants = append(st.Grants, state.Grant{Resource: config.Browser, To: session, At: a.now})
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.holdBrowser(keeper); err == nil || !strings.Contains(err.Error(), `granted to "Agent one"`) {
		t.Errorf("granted to a session: %v", err)
	}
}

func TestTeleportFailedLeavesASignInNote(t *testing.T) {
	a := teleportApp(t)
	keeper := state.Party{Name: teleportKeeper}
	before := teleport.Profile{ValidUntil: a.now.Add(80 * time.Minute)}
	failure := errors.New("tsh login did not complete within 3m0s")

	if err := a.teleportFailed(keeper, false, false, before, nil, failure); !errors.Is(err, failure) {
		t.Fatalf("by hand: %v", err)
	}
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Notes) != 0 {
		t.Fatalf("a renewal by hand left a note: %+v", st.Notes)
	}

	if err := a.teleportFailed(keeper, true, false, before, nil, failure); !errors.Is(err, failure) {
		t.Fatalf("by the keeper: %v", err)
	}
	rec, err := a.teleportRecord()
	if err != nil {
		t.Fatal(err)
	}
	if !rec.Failed(before, nil) || rec.Reason != failure.Error() {
		t.Errorf("record %+v", rec)
	}
	if due, _ := teleport.Due(before, nil, rec, a.now, 90*time.Minute, false); due {
		t.Error("the keeper would try the same login again")
	}
	st, err = a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Notes) != 1 {
		t.Fatalf("notes %+v", st.Notes)
	}
	n := st.Notes[0]
	if n.For != notePerson || n.Kind != noteLogin || !strings.HasSuffix(n.Until, " teleport") || n.Default == "" ||
		!strings.Contains(n.Text, failure.Error()) || !strings.Contains(n.Text, "beekeeper teleport renew") {
		t.Errorf("note %+v", n)
	}
}

func TestTeleportFailedAfterAMissedTick(t *testing.T) {
	a := teleportApp(t)
	keeper := state.Party{Name: teleportKeeper}
	before := teleport.Profile{ValidUntil: a.now.Add(-30 * time.Minute)}
	failure := errors.New("tsh login: dial tcp: lookup login.example.com: no such host")

	if err := a.teleportFailed(keeper, true, true, before, nil, failure); !errors.Is(err, failure) {
		t.Fatalf("after a missed tick: %v", err)
	}
	rec, err := a.teleportRecord()
	if err != nil {
		t.Fatal(err)
	}
	if !rec.AfterMiss || !rec.Failed(before, nil) || !rec.CheckedAt.Equal(a.now.UTC()) {
		t.Errorf("record %+v", rec)
	}
	if due, why := teleport.Due(before, nil, rec, a.now.Add(10*time.Minute), 90*time.Minute, false); !due {
		t.Errorf("the next run does not try once more: %s", why)
	}
	v := newTeleportView(before, nil, rec, a.now, time.Hour)
	if v.Key != teleportFailed || !strings.Contains(v.line(a.now), "the keeper tries once more at its next run") {
		t.Errorf("the watch does not say the failure: %q %q", v.Key, v.line(a.now))
	}
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Notes) != 0 {
		t.Errorf("a failure the keeper retries left a note: %+v", st.Notes)
	}
	if evs, _ := a.store.Events(0, func(e state.Event) bool { return e.Verb == "teleport.fail" }); len(evs) != 1 {
		t.Errorf("the failure is not logged: %+v", evs)
	}

	if err := a.teleportFailed(keeper, true, false, before, nil, failure); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	if rec, _ := a.teleportRecord(); rec.AfterMiss {
		t.Error("the retry's failure is retried again")
	}
	if due, _ := teleport.Due(before, nil, mustRecord(t, a), a.now.Add(20*time.Minute), 90*time.Minute, false); due {
		t.Error("the keeper tries the same login a third time")
	}
	if st, _ := a.store.Read(); len(st.Notes) != 1 {
		t.Errorf("the retry's failure left no sign-in note: %+v", st.Notes)
	}
}

func mustRecord(t *testing.T, a *app) teleport.Record {
	t.Helper()
	rec, err := a.teleportRecord()
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

// teleportKeeper is the name the keeper's unit runs as.
const teleportKeeper = "teleport keeper"

func TestTeleportMissedRetryWaitsForBrowser(t *testing.T) {
	for _, missing := range []bool{false, true} {
		for _, grant := range []bool{false, true} {
			t.Run(fmt.Sprintf("missing=%t/grant=%t", missing, grant), func(t *testing.T) {
				a := teleportApp(t)
				a.as = teleportKeeper
				start := a.now
				every := 10 * time.Minute
				until := start.Add(-time.Hour)
				if missing {
					until = time.Time{}
				}
				raw := `{"active":null}`
				if !missing {
					raw = fmt.Sprintf(`{"active":{"valid_until":%q}}`, until.Format(time.RFC3339))
				}
				dir := t.TempDir()
				a.cfg.StateDir = t.TempDir()
				attempts := filepath.Join(dir, "attempts")
				tsh := filepath.Join(dir, "tsh")
				script := "#!/bin/sh\ncase \"$1\" in\nstatus) echo '" + raw + "';;\nlogin) echo attempt >> '" + attempts + "'; exit 1;;\n*) exit 2;;\nesac\n"
				if err := os.WriteFile(tsh, []byte(script), 0o700); err != nil {
					t.Fatal(err)
				}
				a.cfg.Teleport = config.Teleport{Proxy: "test", Tsh: tsh, Home: filepath.Join(dir, "home"), Every: config.Duration{Duration: every}, RenewBefore: config.Duration{Duration: 90 * time.Minute}, LoginTimeout: config.Duration{Duration: time.Second}}
				initial := teleport.Record{CheckedAt: start, FailedAt: start, FailedFor: until, Reason: "previous failure"}
				if err := a.store.WriteFile(teleportFile, initial); err != nil {
					t.Fatal(err)
				}
				browser := lease.Dir(a.cfg.LeaseDir)
				if grant {
					if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
						st.Grants = append(st.Grants, state.Grant{Resource: config.Browser, To: state.Party{Session: "test", Name: "test"}, At: start.Add(time.Hour)})
						return nil, nil
					}); err != nil {
						t.Fatal(err)
					}
				} else {
					if _, err := browser.Claim(config.Browser, lease.Holder{Env: config.Browser, Name: "test", Purpose: "test"}); err != nil {
						t.Fatal(err)
					}
				}
				run := func(at time.Time) error {
					a.now = at
					c := a.teleportRenewCmd()
					c.SetContext(context.Background())
					if err := c.Flags().Set("keeper", "true"); err != nil {
						t.Fatal(err)
					}
					return c.RunE(c, nil)
				}
				count := func() int {
					b, err := os.ReadFile(attempts)
					if err != nil && !os.IsNotExist(err) {
						t.Fatal(err)
					}
					return strings.Count(string(b), "attempt\n")
				}
				for i := 0; i < 2; i++ {
					at := start.Add(time.Hour + time.Duration(i)*every)
					if err := run(at); err != nil {
						t.Fatal(err)
					}
					rec := mustRecord(t, a)
					if !rec.PendingMiss || rec.AfterMiss || rec.Waiting == "" || !rec.CheckedAt.Equal(at) || !rec.FailedAt.Equal(initial.FailedAt) || count() != 0 {
						t.Fatalf("wait %d: record %+v, attempts %d", i, rec, count())
					}
				}
				if grant {
					if err := a.store.Update(func(st *state.State) ([]state.Event, error) { st.Grants = nil; return nil, nil }); err != nil {
						t.Fatal(err)
					}
				} else if err := browser.Release(config.Browser); err != nil {
					t.Fatal(err)
				}
				for i := 0; i < 2; i++ {
					at := start.Add(time.Hour + time.Duration(i+2)*every)
					if err := run(at); err == nil || !strings.Contains(err.Error(), "tsh login") {
						t.Fatalf("attempt %d: %v", i, err)
					}
					rec := mustRecord(t, a)
					if rec.PendingMiss || rec.AfterMiss != (i == 0) || rec.Waiting != "" || !rec.FailedAt.Equal(at) || count() != i+1 {
						t.Fatalf("attempt %d: record %+v, attempts %d", i, rec, count())
					}
					st, err := a.store.Read()
					if err != nil {
						t.Fatal(err)
					}
					if len(st.Notes) != i {
						t.Fatalf("attempt %d: notes %d", i, len(st.Notes))
					}
				}
				for i := 0; i < 2; i++ {
					if err := run(start.Add(time.Hour + time.Duration(i+4)*every)); err != nil {
						t.Fatal(err)
					}
					st, err := a.store.Read()
					if err != nil {
						t.Fatal(err)
					}
					if count() != 2 || len(st.Notes) != 1 {
						t.Fatalf("suppressed run: attempts %d, notes %d", count(), len(st.Notes))
					}
				}
			})
		}
	}
}
