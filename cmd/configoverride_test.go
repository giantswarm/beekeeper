package cmd

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/state"
)

const (
	overrideIssue   = "o/r#7"
	overrideIssueGH = "gh api repos/o/r/issues/7 --jq .state"
	overrideClosed  = "closed"
	overrideReason  = "--reason"
	overrideAs      = "--as"
	overrideTTL     = "20m"
)

// overrideRig is a configuration file with its state directory, the
// configuration set and restored through the commands, and the issue
// state the fake GitHub answers.
type overrideRig struct {
	t     *testing.T
	cfg   string
	issue string
}

func newOverrideRig(t *testing.T, raw string) *overrideRig {
	t.Helper()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	raw = "stateDir: " + filepath.Join(dir, "state") + "\nguide: {person: pat}\n" + raw
	if err := os.WriteFile(cfg, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BEEKEEPER_CONFIG", cfg)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	r := &overrideRig{t: t, cfg: cfg, issue: noteStatusOpen}
	fakeRead(t, func(cmd string) ([]byte, error) {
		if cmd != overrideIssueGH {
			t.Errorf("unexpected check %q", cmd)
		}
		return []byte(r.issue + "\n"), nil
	})
	return r
}

// app is a watch tick's app at now.
func (r *overrideRig) app(now time.Time) *app {
	r.t.Helper()
	a := &app{cfgPath: r.cfg, out: &bytes.Buffer{}}
	if err := a.load(); err != nil {
		r.t.Fatal(err)
	}
	a.now = now
	return a
}

// tick settles the overrides at now as the watch does and returns its lines.
func (r *overrideRig) tick(now time.Time) []string {
	r.t.Helper()
	a := r.app(now)
	lines, err := a.settleOverrides(context.Background(), watchParty, func(ts []state.Timer) map[int]checkResult {
		return checkTimers(context.Background(), ts, now, false)
	})
	if err != nil {
		r.t.Fatal(err)
	}
	return lines
}

func (r *overrideRig) get(key string) (string, bool) {
	r.t.Helper()
	v, ok, err := config.Get(r.cfg, key)
	if err != nil {
		r.t.Fatal(err)
	}
	return v, ok
}

func (r *overrideRig) state() *state.State {
	r.t.Helper()
	st, err := r.app(time.Now()).store.Read()
	if err != nil {
		r.t.Fatal(err)
	}
	return st
}

// verbs are the override events logged, oldest first.
func (r *overrideRig) verbs() []string {
	r.t.Helper()
	evs, err := r.app(time.Now()).store.Events(0, func(e state.Event) bool { return strings.HasPrefix(e.Verb, "config.") })
	if err != nil {
		r.t.Fatal(err)
	}
	var out []string
	for _, e := range evs {
		out = append(out, e.Verb)
	}
	return out
}

// An override with --until <issue> stays while the issue is open and is
// restored once it closes, a key the file lacked removed again; the event
// log records the set, the end and the restore, and handover lists it while
// it is in effect.
func TestOverrideUntilIssueRestoredWhenItCloses(t *testing.T) {
	r := newOverrideRig(t, "github:\n  floor: 500 # the floor\n")
	if _, err := execute(t, "config", "set", untilFlag, "https://github.com/o/r/issues/7", overrideReason, "merge batch", overrideAs, "tester",
		"--", "github.floor", "-1", "capacity.local.ceiling", "8"); err != nil {
		t.Fatalf("set = %v", err)
	}
	if v, _ := r.get("github.floor"); v != "-1" {
		t.Fatalf("floor = %q after the set", v)
	}
	st := r.state()
	if len(st.Overrides) != 1 {
		t.Fatalf("overrides = %+v", st.Overrides)
	}
	o := st.Overrides[0]
	if o.Ref != overrideIssue || o.Reason != "merge batch" || o.Keys[0].Previous != "500" || !o.Keys[1].Absent {
		t.Errorf("override = %+v", o)
	}
	a := r.app(time.Now())
	out := a.capture(func() { a.printOverrides(st.Overrides) })
	if !strings.Contains(out, "github.floor -1 (was 500)") || !strings.Contains(out, "until o/r#7 closes") || !strings.Contains(out, "merge batch") {
		t.Errorf("listed:\n%s", out)
	}

	now := time.Now().Add(time.Minute)
	if lines := r.tick(now); len(lines) != 0 {
		t.Errorf("open issue: lines %q", lines)
	}
	if v, _ := r.get("github.floor"); v != "-1" {
		t.Errorf("floor = %q while the issue is open", v)
	}
	r.issue = overrideClosed
	if lines := r.tick(now.Add(time.Minute)); len(lines) != 0 {
		t.Errorf("checked again within timerEvery: lines %q", lines)
	}
	lines := r.tick(now.Add(timerEvery + time.Minute))
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "CONFIG OVERRIDE ENDED: #1, o/r#7 closed") || !strings.HasPrefix(lines[1], "CONFIG RESTORED: #1") {
		t.Errorf("closed issue: lines %q", lines)
	}
	if v, _ := r.get("github.floor"); v != "500" {
		t.Errorf("floor = %q after the restore, want 500", v)
	}
	if v, ok := r.get("capacity"); ok {
		t.Errorf("capacity = %q after the restore, want it gone", v)
	}
	raw, _ := os.ReadFile(r.cfg)
	if !strings.Contains(string(raw), "# the floor") {
		t.Errorf("the restore lost the key's comment:\n%s", raw)
	}
	if st := r.state(); len(st.Overrides) != 0 {
		t.Errorf("overrides after the restore = %+v", st.Overrides)
	}
	if got, want := r.verbs(), []string{overrideSetVerb, overrideEndVerb, overrideRestoreVerb}; !slices.Equal(got, want) {
		t.Errorf("events = %q, want %q", got, want)
	}
}

// An override with a time ends when it comes.
func TestOverrideUntilTimeRestoredWhenItComes(t *testing.T) {
	r := newOverrideRig(t, "grantTTL: 20m\n")
	if _, err := execute(t, "config", "set", "grantTTL", "1h", untilFlag, "2h", overrideReason, "long review", overrideAs, "tester"); err != nil {
		t.Fatalf("set = %v", err)
	}
	if lines := r.tick(time.Now().Add(time.Hour)); len(lines) != 0 {
		t.Errorf("before its time: lines %q", lines)
	}
	if lines := r.tick(time.Now().Add(3 * time.Hour)); len(lines) != 2 || !strings.Contains(lines[1], "grantTTL 20m") {
		t.Errorf("past its time: lines %q", lines)
	}
	if v, _ := r.get("grantTTL"); v != overrideTTL {
		t.Errorf("grantTTL = %q, want 20m", v)
	}
}

// A key changed since the override is not restored: the person is asked in
// a note, which config keep answers and closes.
func TestOverrideChangedSinceAsksThePerson(t *testing.T) {
	r := newOverrideRig(t, "grantTTL: 20m\n")
	if _, err := execute(t, "config", "set", "grantTTL", "1h", untilFlag, overrideIssue, overrideReason, "long review", overrideAs, "tester"); err != nil {
		t.Fatalf("set = %v", err)
	}
	if _, err := execute(t, "config", "set", "grantTTL", "2h"); err != nil {
		t.Fatalf("plain set = %v", err)
	}
	r.issue = overrideClosed
	lines := r.tick(time.Now().Add(time.Minute))
	if len(lines) != 2 || !strings.Contains(lines[1], "NEEDS A DECISION") || !strings.Contains(lines[1], "grantTTL is 2h, the override set 1h") || !strings.Contains(lines[1], "asks pat") {
		t.Errorf("lines %q", lines)
	}
	if v, _ := r.get("grantTTL"); v != "2h" {
		t.Errorf("grantTTL = %q, want the later change kept", v)
	}
	st := r.state()
	if len(st.Overrides) != 1 || st.Overrides[0].Note == 0 || len(st.Notes) != 1 || st.Notes[0].For != "pat" {
		t.Fatalf("overrides %+v, notes %+v", st.Overrides, st.Notes)
	}
	if lines := r.tick(time.Now().Add(time.Hour)); len(lines) != 0 {
		t.Errorf("a decision asked once: lines %q", lines)
	}
	if _, err := execute(t, "config", "restore", "1", overrideAs, "tester"); Code(err) != ExitRefused {
		t.Errorf("restore without --force = exit %d (%v), want refused", Code(err), err)
	}
	if _, err := execute(t, "config", "keep", "1", overrideAs, "tester"); err != nil {
		t.Fatalf("keep = %v", err)
	}
	if st := r.state(); len(st.Overrides) != 0 || len(st.Notes) != 0 {
		t.Errorf("after keep: overrides %+v, notes %+v", st.Overrides, st.Notes)
	}
	if v, _ := r.get("grantTTL"); v != "2h" {
		t.Errorf("grantTTL = %q after keep", v)
	}
}

// A second override of a key takes it over: its end restores the value
// before the first.
func TestOverrideTakesOverAKey(t *testing.T) {
	r := newOverrideRig(t, "grantTTL: 20m\n")
	for _, v := range []string{"1h", "2h"} {
		if _, err := execute(t, "config", "set", "grantTTL", v, untilFlag, "1h", overrideReason, "review "+v, overrideAs, "tester"); err != nil {
			t.Fatalf("set %s = %v", v, err)
		}
	}
	st := r.state()
	if len(st.Overrides) != 1 || st.Overrides[0].ID != 2 || st.Overrides[0].Keys[0].Previous != overrideTTL {
		t.Fatalf("overrides = %+v", st.Overrides)
	}
	if _, err := execute(t, "config", "restore", "2", overrideAs, "tester"); err != nil {
		t.Fatalf("restore = %v", err)
	}
	if v, _ := r.get("grantTTL"); v != overrideTTL {
		t.Errorf("grantTTL = %q, want 20m", v)
	}
}

// --until takes a time or an issue, and needs --reason; --reason needs
// --until. A refused set leaves no override.
func TestOverrideUsage(t *testing.T) {
	r := newOverrideRig(t, "")
	set := func(value string, flags ...string) error {
		_, err := execute(t, append([]string{configName, "set", overrideAs, "tester", "grantTTL", value}, flags...)...)
		return err
	}
	why := []string{overrideReason, "why"}
	for _, flags := range [][]string{
		{untilFlag, "1h"},
		why,
		append([]string{untilFlag, "someday"}, why...),
	} {
		if err := set("1h", flags...); Code(err) != ExitUsage {
			t.Errorf("%q = exit %d (%v), want usage", flags, Code(err), err)
		}
	}
	if err := set("soon", append([]string{untilFlag, "1h"}, why...)...); Code(err) != ExitRefused {
		t.Errorf("an invalid value = exit %d (%v), want refused", Code(err), err)
	}
	if v, ok := r.get("grantTTL"); ok {
		t.Errorf("grantTTL = %q after the refusals", v)
	}
}

// A valid config write can still fail while opening state. The settings
// must be restored even when load fails before an override is recorded.
func TestOverrideLoadFailureRestoresConfig(t *testing.T) {
	r := newOverrideRig(t, "grantTTL: 20m\n")
	blocked := filepath.Join(t.TempDir(), "state-file")
	if err := os.WriteFile(blocked, []byte("regular file"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := config.Set(r.cfg, []config.Setting{{Key: "stateDir", Value: blocked}}); err != nil {
		t.Fatal(err)
	}
	_, err := execute(t, configName, "set", "grantTTL", "1h", "capacity.local.ceiling", "8", untilFlag, "1h", overrideReason, "review", overrideAs, "tester")
	if err == nil || !strings.Contains(err.Error(), "values before are restored") {
		t.Fatalf("set = %v, want load failure with rollback", err)
	}
	if v, _ := r.get("grantTTL"); v != overrideTTL {
		t.Errorf("grantTTL = %q after failed load", v)
	}
	if v, ok := r.get("capacity"); ok {
		t.Errorf("capacity = %q after failed load, want absent", v)
	}
}

// Changing the state directory would record the override in a different
// history and strand it when restored, whether the new path loads or not.
func TestOverrideRejectsStateDir(t *testing.T) {
	for _, regularFile := range []bool{false, true} {
		t.Run(fmt.Sprint(regularFile), func(t *testing.T) {
			r := newOverrideRig(t, "grantTTL: 20m\n")
			target := filepath.Join(t.TempDir(), "new-state")
			if regularFile {
				if err := os.WriteFile(target, []byte("file"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.ReadFile(r.cfg)
			if err != nil {
				t.Fatal(err)
			}
			_, err = execute(t, configName, "set", "grantTTL", "1h", "stateDir", target, untilFlag, "1h", overrideReason, "review", overrideAs, "tester")
			if Code(err) != ExitRefused || !strings.Contains(err.Error(), "stateDir cannot be overridden") {
				t.Fatalf("set = %v, want stateDir refusal", err)
			}
			after, err := os.ReadFile(r.cfg)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatal("refused override changed config")
			}
			if st := r.state(); len(st.Overrides) != 0 {
				t.Fatalf("refused override recorded: %+v", st.Overrides)
			}
		})
	}
}

// Opening state can succeed while recording the override fails. Neither
// existing nor newly introduced settings may remain as untracked changes.
func TestOverrideRecordFailureRestoresConfig(t *testing.T) {
	r := newOverrideRig(t, "grantTTL: 20m\n")
	a := r.app(time.Now())
	if err := os.WriteFile(filepath.Join(a.store.Dir(), "state.json"), []byte("invalid json"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := execute(t, configName, "set", "grantTTL", "1h", "capacity.local.ceiling", "8", untilFlag, "1h", overrideReason, "review", overrideAs, "tester")
	if err == nil || !strings.Contains(err.Error(), "values before are restored") {
		t.Fatalf("set = %v, want record failure with rollback", err)
	}
	if v, _ := r.get("grantTTL"); v != overrideTTL {
		t.Errorf("grantTTL = %q after failed record", v)
	}
	if v, ok := r.get("capacity"); ok {
		t.Errorf("capacity = %q after failed record, want absent", v)
	}
}
