package tui

import (
	"reflect"
	"testing"
)

const currentWord = "current"

const originalWord = "original"

func TestRestartedPaneAnswersQueueAndReleasesBothKeys(t *testing.T) {
	src := &fakeSource{data: &Data{Sessions: []Session{{ID: currentWord, Name: tBee, TakeoverAlias: originalWord, TakenOver: true,
		Approvals: []Approval{{ID: "held", Session: originalWord, Gist: "Bash"}}}}}}
	m := newTestModel(t, src)
	m.detail = tBee
	run(m, m.answer(true))
	if !reflect.DeepEqual(src.answers, []string{"original/held allow"}) {
		t.Fatalf("answers = %v", src.answers)
	}
	m.acting = false
	m.taken = map[string]bool{originalWord: true, currentWord: true}
	run(m, m.toggleTakeOver())
	if !reflect.DeepEqual(src.released, []string{currentWord, originalWord}) {
		t.Fatalf("released = %v", src.released)
	}
	if len(m.taken) != 0 {
		t.Fatalf("still remembers released keys: %v", m.taken)
	}
}
