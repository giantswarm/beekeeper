package cmd

import (
	"strings"
	"testing"
)

// A chore line the doctor said is not said again while it is unchanged; a
// changed one is.
func TestSayChoresOnce(t *testing.T) {
	w, _, out := notifyingWatch(t, t.TempDir(), false)
	rep := doctorReport{chores: []string{`"a": its desktop session local_a stays: its CLI 7 is in a turn`}}
	w.sayChores(rep)
	w.sayChores(rep)
	if n := strings.Count(out.String(), "stays: its CLI 7"); n != 1 {
		t.Errorf("said %d times, want once:\n%s", n, out)
	}
	rep.chores = []string{`"a": archived its desktop session local_a`}
	w.sayChores(rep)
	if !strings.Contains(out.String(), "archived its desktop session") {
		t.Errorf("a changed line is said:\n%s", out)
	}
}

// Finished workers waiting on agents.archiveAgreement are one summary line
// for the whole watch, ended once none waits.
func TestSayChoresUnagreedSummary(t *testing.T) {
	w, _, out := notifyingWatch(t, t.TempDir(), false)
	for range 3 {
		w.sayChores(doctorReport{unagreed: 244})
	}
	if n := strings.Count(out.String(), "agents.archiveAgreement"); n != 1 {
		t.Errorf("summary said %d times, want once:\n%s", n, out)
	}
	if !strings.Contains(out.String(), "244 finished workers") {
		t.Errorf("summary has no count:\n%s", out)
	}
	w.sayChores(doctorReport{})
	if !strings.Contains(out.String(), "ENDED") {
		t.Errorf("summary does not end:\n%s", out)
	}
}
