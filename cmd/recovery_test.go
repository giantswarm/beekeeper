package cmd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/loopworkflow"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/update"
)

const (
	recoveryGoodVersion = "v1.1.0-rc.1"
	recoveryBadVersion  = "v1.1.0-rc.2"
	recoveryGoodBytes   = "good release"
	recoveryBadBytes    = "broken release"
	recoveryMergeFailed = "merge-exit-1"
)

func recoverySum(s string) string {
	d := sha256.Sum256([]byte(s))
	return hex.EncodeToString(d[:])
}

// recoveryRig is a candidate in probation with its controller. Nothing in it
// reaches this machine's processes, services or network.
func recoveryRig(t *testing.T) (*recoveryController, *time.Time) {
	t.Helper()
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	store, err := state.Open(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "beekeeper")
	if err := os.WriteFile(target, []byte(recoveryBadBytes), 0o700); err != nil { //nolint:gosec // an executable fixture
		t.Fatal(err)
	}
	recovery := target + ".update"
	if err := os.Mkdir(recovery, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, b := range []string{recoveryGoodBytes, recoveryBadBytes} {
		if err := os.WriteFile(filepath.Join(recovery, recoverySum(b)), []byte(b), 0o700); err != nil { //nolint:gosec // an executable fixture
			t.Fatal(err)
		}
	}
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	j := update.Journal{
		Schema: 1, Displaced: update.Binary{Version: recoveryGoodVersion, Digest: recoverySum(recoveryGoodBytes)}, Previous: &update.Binary{Version: recoveryGoodVersion, Digest: recoverySum(recoveryGoodBytes)},
		Candidate: update.Binary{Version: recoveryBadVersion, Digest: recoverySum(recoveryBadBytes)}, InstalledAt: now, Probation: "pending", Watched: true,
	}
	raw, err := json.Marshal(j)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(recovery, "journal.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	a := &app{cfg: &config.Config{StateDir: stateDir}, store: store, out: io.Discard}
	c := &recoveryController{a: a, store: store, w: io.Discard, target: target, server: target, scratch: dir, step: time.Minute}
	c.now = func() time.Time { return now }
	c.moveProcesses = func(context.Context, int) bool { return true }
	probe, up, record := recoveryProbe, recoveryGitHubUp, recoveryRecord
	t.Cleanup(func() { recoveryProbe, recoveryGitHubUp, recoveryRecord = probe, up, record })
	recoveryProbe = func(context.Context, string, string, string) error { return nil }
	recoveryGitHubUp = func(context.Context) bool { return true }
	return c, &now
}

func (c *recoveryController) mustTick(t *testing.T) bool {
	t.Helper()
	settled, err := c.tick(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return settled
}

func installed(t *testing.T, c *recoveryController) string {
	t.Helper()
	raw, err := os.ReadFile(c.target)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func journal(t *testing.T, c *recoveryController) *update.Journal {
	t.Helper()
	j, err := update.Inspect(c.target)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func (c *recoveryController) failMerges(t *testing.T, n int, at time.Time) {
	t.Helper()
	for range n {
		raw, _ := json.Marshal(throughputOutcome{At: at, Operation: runMerge, Reason: recoveryMergeFailed, Candidate: normalizeLoopVersion(recoveryBadVersion)})
		if err := c.store.Record(state.Event{At: at, Verb: throughputOutcomeVerb, Detail: string(raw)}); err != nil {
			t.Fatal(err)
		}
	}
}

func (c *recoveryController) filedReports(t *testing.T) []string {
	t.Helper()
	evs, err := c.store.Events(0, func(e state.Event) bool { return e.Verb == loopFileVerb })
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, e := range evs {
		lines = append(lines, e.Detail)
	}
	return lines
}

func TestRecoveryRollsBackAFailureStreak(t *testing.T) {
	c, now := recoveryRig(t)
	c.failMerges(t, 3, *now)
	if !c.mustTick(t) {
		t.Fatal("a finished rollback left the controller running")
	}
	if got := installed(t, c); got != recoveryGoodBytes {
		t.Fatalf("installed %q", got)
	}
	j := journal(t, c)
	if !j.RolledBack() || j.Quarantine[recoverySum(recoveryBadBytes)] == "" {
		t.Fatalf("%+v", j)
	}
	if !c.a.isSkippedRelease(recoveryBadVersion) {
		t.Fatal("ordinary self-update could offer the failed release again")
	}
	lines := c.filedReports(t)
	if len(lines) != 1 || !strings.Contains(lines[0], recoveryBadVersion) || !strings.Contains(lines[0], recoveryGoodVersion) {
		t.Fatalf("the person's line: %q", lines)
	}
	if c.mustTick(t); len(c.filedReports(t)) != 1 {
		t.Fatal("one rollback told twice")
	}
}

func TestRecoveryRollsBackAFailedProbeOnAnIdleDesk(t *testing.T) {
	c, _ := recoveryRig(t)
	recoveryProbe = func(context.Context, string, string, string) error {
		return errors.New("the origin does not show the reviewed merge")
	}
	for range update.ProbeFailures - 1 {
		c.mustTick(t)
		if got := installed(t, c); got != recoveryBadBytes {
			t.Fatal("rolled back before the third failed probe")
		}
	}
	c.mustTick(t)
	if got := installed(t, c); got != recoveryGoodBytes {
		t.Fatalf("installed %q", got)
	}
}

func TestRecoveryRollsBackAMissingHeartbeat(t *testing.T) {
	c, now := recoveryRig(t)
	// The old release's loop ran here, the candidate's never showed up.
	if err := c.store.WriteFile(throughputObservationFile, throughputObservation{At: now.Add(-time.Minute), Candidate: normalizeLoopVersion(recoveryGoodVersion)}); err != nil {
		t.Fatal(err)
	}
	ticks := 0
	for installed(t, c) == recoveryBadBytes {
		c.mustTick(t)
		ticks++
	}
	if ticks != int(update.HeartbeatGrace/time.Minute) {
		t.Fatalf("rolled back after %d ticks", ticks)
	}
}

func TestRecoveryKeepsACandidateUnprovenDuringAnOutage(t *testing.T) {
	c, now := recoveryRig(t)
	c.failMerges(t, 3, *now)
	recoveryGitHubUp = func(context.Context) bool { return false }
	for range 40 {
		c.mustTick(t)
	}
	if got := installed(t, c); got != recoveryBadBytes {
		t.Fatal("GitHub's outage rolled the candidate back")
	}
	if !journal(t, c).Pending() {
		t.Fatal("an unproven candidate is neither promoted nor settled")
	}
}

func TestRecoveryIgnoresAnInconclusiveProbe(t *testing.T) {
	c, _ := recoveryRig(t)
	recoveryProbe = func(context.Context, string, string, string) error { return loopworkflow.ErrInconclusive }
	for range 40 {
		c.mustTick(t)
	}
	if j := journal(t, c); !j.Pending() {
		t.Fatalf("%+v", j)
	}
}

func TestRecoveryPromotesAHealthyCandidateAtTheWindow(t *testing.T) {
	c, _ := recoveryRig(t)
	probes := 0
	recoveryProbe = func(context.Context, string, string, string) error { probes++; return nil }
	for range int(update.ProbationWindow/time.Minute) - 1 {
		if c.mustTick(t) {
			t.Fatal("promoted before the window ended")
		}
	}
	if !c.mustTick(t) {
		t.Fatal("not promoted at the window's end")
	}
	if got := installed(t, c); got != recoveryBadBytes {
		t.Fatal("a healthy candidate was replaced")
	}
	if probes != 6 {
		t.Fatalf("%d probes in a window, one every five minutes", probes)
	}
}

func TestRecoveryRetriesAFailedNotification(t *testing.T) {
	c, now := recoveryRig(t)
	c.failMerges(t, 3, *now)
	recoveryRecord = func(state.Store, state.Event) error { return errors.New("the state is unwritable") }
	if settled, err := c.tick(t.Context()); settled || err == nil {
		t.Fatalf("settled %v, %v", settled, err)
	}
	if got := installed(t, c); got != recoveryGoodBytes {
		t.Fatal("a notification stopped the rollback")
	}
	recoveryRecord = func(s state.Store, e state.Event) error { return s.Record(e) }
	if !c.mustTick(t) || len(c.filedReports(t)) != 1 {
		t.Fatalf("settled without its line: %q", c.filedReports(t))
	}
}

func TestRecoveryHoldsOnACorruptBackup(t *testing.T) {
	c, now := recoveryRig(t)
	if err := os.WriteFile(filepath.Join(c.target+".update", recoverySum(recoveryGoodBytes)), []byte("tampered"), 0o700); err != nil { //nolint:gosec // an executable fixture
		t.Fatal(err)
	}
	c.failMerges(t, 3, *now)
	for range recoveryFailures - 1 {
		if settled, err := c.tick(t.Context()); settled || err == nil {
			t.Fatalf("settled %v, %v", settled, err)
		}
	}
	if !c.mustTick(t) {
		t.Fatal("a held recovery keeps running")
	}
	if got := installed(t, c); got != recoveryBadBytes {
		t.Fatal("a failed recovery replaced the installed binary")
	}
	if c.a.recoveryHeld() == "" || journal(t, c).Held() == "" {
		t.Fatal("no hold on starts and updates")
	}
	if lines := c.filedReports(t); len(lines) != 1 || !strings.Contains(lines[0], "RELEASE RECOVERY FAILED") {
		t.Fatalf("%q", lines)
	}
	if err := update.ClearHold(t.Context(), c.target); err != nil {
		t.Fatal(err)
	}
	if j := journal(t, c); j.Held() != "" || j.Previous != nil {
		t.Fatalf("a target that fails verification would refuse every later update: %+v", j)
	}
}

func TestRecoveryHoldsOnIncompatibleState(t *testing.T) {
	c, now := recoveryRig(t)
	stable, err := state.OpenVersion(c.a.cfg.StateDir, "v1.1.0")
	if err != nil {
		t.Fatal(err)
	}
	if err := stable.Update(func(st *state.State) ([]state.Event, error) {
		st.Notes = append(st.Notes, state.Note{ID: 1, Text: "kept"})
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	c.failMerges(t, 3, *now)
	for range recoveryFailures {
		_, _ = c.tick(t.Context())
	}
	if got := installed(t, c); got != recoveryBadBytes {
		t.Fatal("an older release took over state a newer one wrote")
	}
	if c.a.recoveryHeld() == "" {
		t.Fatal("no hold")
	}
	st, err := c.store.Read()
	if err != nil || len(st.Notes) != 1 {
		t.Fatalf("%v, %v", st, err)
	}
}

func TestRecoveryHoldClears(t *testing.T) {
	c, _ := recoveryRig(t)
	if err := c.hold(t.Context(), recoveryBadVersion, errors.New("cause")); err != nil {
		t.Fatal(err)
	}
	if err := update.ClearHold(t.Context(), c.target); err != nil {
		t.Fatal(err)
	}
	if err := c.store.WriteFile(recoveryHoldFile, recoveryHold{}); err != nil {
		t.Fatal(err)
	}
	if why := c.a.recoveryHeld(); why != "" {
		t.Fatal(why)
	}
}

// A controller killed between two durable writes resumes the rollback from
// the journal and tells the person once.
func TestRecoveryResumesAnInterruptedRollback(t *testing.T) {
	c, now := recoveryRig(t)
	c.failMerges(t, 3, *now)
	restored := false
	if err := update.Rollback(t.Context(), c.target, "failed-merge", func(_, _ string, _ func() error) error { restored = true; return errors.New("killed") }); err == nil || !restored {
		t.Fatal(err)
	}
	if err := update.Observe(t.Context(), c.target, func(j *update.Journal) error { j.Probation = "rolling-back"; return nil }); err != nil {
		t.Fatal(err)
	}
	if !c.mustTick(t) {
		t.Fatal("not resumed")
	}
	if got := installed(t, c); got != recoveryGoodBytes {
		t.Fatal(got)
	}
	if lines := c.filedReports(t); len(lines) != 1 || !strings.Contains(lines[0], "failed-merge") {
		t.Fatalf("%q", lines)
	}
}

func TestLaunchRecoveryNeedsTheUpdatersOwnBytes(t *testing.T) {
	a := &app{}
	if err := a.launchRecovery(t.Context(), update.Binary{Version: recoveryGoodVersion, Digest: recoverySum("another binary")}, "unused", "unused"); err == nil {
		t.Fatal("an older recovery target would die on the controller's flags")
	}
}
