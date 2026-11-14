package cmd

import (
	"strings"
	"testing"
)

// A chore line the doctor said is not said again while it is unchanged; a
// changed one is.
//
// control: none: the watch extraction only changes names and access paths; assertions are unchanged.
// The merge base has neither internal/watch nor the names these tests now use, so they cannot build there.
func TestSayChoresOnce(t *testing.T) {
	w, _, out := notifyingWatch(t, t.TempDir(), false)
	rep := doctorReport{chores: []string{`"a": its desktop session local_a stays: its CLI 7 is in a turn`}}
	watchSayChores(w, rep)
	watchSayChores(w, rep)
	if n := strings.Count(out.String(), "stays: its CLI 7"); n != 1 {
		t.Errorf("said %d times, want once:\n%s", n, out)
	}
	rep.chores = []string{`"a": archived its desktop session local_a`}
	watchSayChores(w, rep)
	if !strings.Contains(out.String(), "archived its desktop session") {
		t.Errorf("a changed line is said:\n%s", out)
	}
}

// An empty report has no archive configuration reminder.
//
// control: none: the watch extraction only changes names and access paths; assertions are unchanged.
// The merge base has neither internal/watch nor the names these tests now use, so they cannot build there.
func TestSayChoresEmpty(t *testing.T) {
	w, _, out := notifyingWatch(t, t.TempDir(), false)
	watchSayChores(w, doctorReport{})
	if out.Len() != 0 {
		t.Errorf("empty report printed: %s", out)
	}
}
