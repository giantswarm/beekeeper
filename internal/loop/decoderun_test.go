package loop

import "testing"

// Given capacity --json answers and then writes the diagnostic of a state
// snapshot older than a pass to its stderr, when the loop judges a start's
// room, then it reads the answer: a full machine stays full and a free one
// free. An answer without a document leaves the room open, as before: the
// start judges it again itself.
func TestRoomOpenReadsTheAnswerBeforeItsDiagnostic(t *testing.T) {
	const stale = "state snapshot is 2m0s old (supervisor pass 30s)\n"
	for _, tc := range []struct {
		out  string
		want bool
	}{
		{`{"room":0}` + "\n" + stale, false},
		{`{"room":1}` + "\n" + stale, true},
		{`{"room":0}`, false},
		{stale, true},
	} {
		if got := roomOpen(tc.out); got != tc.want {
			t.Errorf("roomOpen(%q) = %v, want %v", tc.out, got, tc.want)
		}
	}
}
