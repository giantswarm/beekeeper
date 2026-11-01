package cmd

import (
	"strings"
	"testing"
)

// Uses existing watch helpers so this regression also runs on the base.
func TestWatchLinesBecomeDurableEvents(t *testing.T) {
	w, _ := onceWatch(t, nil)
	w.check("disk", true, "LOW DISK: test writers")
	w.check("disk", true, "LOW DISK: test writers")
	w.clear("disk")
	evs, err := w.store.Events(0, nil)
	if err != nil {
		t.Fatal(err)
	}
	starts, clears := 0, 0
	for _, e := range evs {
		if e.Verb == "watch.disk" && strings.Contains(e.Detail, "test writers") {
			starts++
		}
		if e.Verb == "watch.clear" && e.Detail == "disk" {
			clears++
		}
	}
	if starts != 1 || clears != 1 {
		t.Fatalf("durable watch transitions: starts=%d clears=%d", starts, clears)
	}
}
