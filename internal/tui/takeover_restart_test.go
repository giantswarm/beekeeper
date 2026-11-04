package tui

import (
	"reflect"
	"testing"
)

func TestRestartedPaneAnswersQueueAndReleasesBothKeys(t *testing.T) {
	src := &fakeSource{data: &Data{Sessions: []Session{{ID: "current", Name: tBee, TakeoverAlias: "original", TakenOver: true,
		Approvals: []Approval{{ID: "held", Session: "original", Gist: "Bash"}}}}}}
	m := newTestModel(t, src)
	m.detail = tBee
	run(m, m.answer(true))
	if !reflect.DeepEqual(src.answers, []string{"original/held allow"}) {
		t.Fatalf("answers = %v", src.answers)
	}
	m.acting = false
	m.taken = map[string]bool{"original": true, "current": true}
	run(m, m.toggleTakeOver())
	if !reflect.DeepEqual(src.released, []string{"current", "original"}) {
		t.Fatalf("released = %v", src.released)
	}
	if len(m.taken) != 0 {
		t.Fatalf("still remembers released keys: %v", m.taken)
	}
}
