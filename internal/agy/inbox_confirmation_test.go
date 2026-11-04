//go:build unix

package agy

import (
	"os"
	"slices"
	"testing"
)

func TestDelayedStreamConfirmationKeepsUnacceptedWake(t *testing.T) {
	for _, initialStdin := range []bool{true, false} {
		name := "hook"
		if initialStdin {
			name = "stdin"
		}
		t.Run(name, func(t *testing.T) {
			inbox := InboxPath(t.TempDir(), "delayed-confirm")
			if err := MakeInbox(inbox); err != nil {
				t.Fatal(err)
			}
			reader, err := os.OpenFile(inbox, os.O_RDWR, 0) //nolint:gosec // synthetic FIFO
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = reader.Close() }()
			if err := Send(inbox, "accepted first"); err != nil {
				t.Fatal(err)
			}
			if initialStdin {
				if _, ok, err := popInflight(inbox); err != nil || !ok {
					t.Fatalf("popInflight = %v, %v", ok, err)
				}
			} else if _, err := Drain(inbox); err != nil {
				t.Fatal(err)
			}
			transcript := inbox + ".transcript"
			writeFile(t, transcript, `{"type":"USER_INPUT","content":"accepted first"}`+"\n")
			if got, err := drainHook(inbox, transcript); err != nil || len(got) != 0 {
				t.Fatalf("transcript acknowledgement = %q, %v", got, err)
			}
			if err := Send(inbox, "unaccepted wake"); err != nil {
				t.Fatal(err)
			}
			want := []string{"unaccepted wake"}
			if got, err := Drain(inbox); err != nil || !slices.Equal(got, want) {
				t.Fatalf("first wake injection = %q, %v", got, err)
			}
			// The hook output was ignored. Only the older input's delayed
			// stdout acknowledgement arrives; the wake is still unaccepted.
			if err := Confirm(inbox); err != nil {
				t.Fatal(err)
			}
			if got, err := Drain(inbox); err != nil || !slices.Equal(got, want) {
				t.Fatalf("retry after older stream acknowledgement = %q, %v; want %q", got, err, want)
			}
			if err := restoreInflight(inbox); err != nil {
				t.Fatal(err)
			}
			if got, err := Drain(inbox); err != nil || !slices.Equal(got, want) {
				t.Fatalf("retry after restart = %q, %v; want %q", got, err, want)
			}
			// Stream ordinals start afresh with the restarted process.
			if err := Confirm(inbox); err != nil {
				t.Fatal(err)
			}
			if got, err := Drain(inbox); err != nil || len(got) != 0 {
				t.Fatalf("retry after wake acceptance = %q, %v; want empty", got, err)
			}
		})
	}
}
