package upgrade

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReaderRetryAndCauses(t *testing.T) {
	for _, tc := range []struct {
		name, script, cause string
		timeout             time.Duration
	}{
		{"retry succeeds", "if [ ! -f \"$0.once\" ]; then touch \"$0.once\"; kill -KILL $$; fi\necho '{\"items\":[]}'", "", time.Second},
		{"timeout retry succeeds", "if [ ! -f \"$0.once\" ]; then touch \"$0.once\"; exec sleep 10; fi\necho '{\"items\":[] }'", "", 50 * time.Millisecond},
		{"killed", "echo misleading-error >&2; kill -KILL $$", "signal: killed", time.Second},
		{"timeout", "exec sleep 10", "timed out after 20ms", 20 * time.Millisecond},
		{"missing context", "echo 'context was not found for specified context' >&2; exit 1", noContextErr, time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exe := filepath.Join(t.TempDir(), "kubectl")
			if err := os.WriteFile(exe, []byte("#!/bin/sh\necho attempt >> \"$0.calls\"\n"+tc.script+"\n"), 0700); err != nil { //nolint:gosec // a path of this test
				t.Fatal(err)
			}
			r := Reader{Kubectl: exe, Timeout: tc.timeout}
			s := r.Read(context.Background(), []Target{{Name: "test", Context: "test"}}, time.Now(), func(string, string) bool { return false })[0]
			calls, err := os.ReadFile(exe + ".calls") //nolint:gosec // a path of this test
			if err != nil {
				t.Fatal(err)
			}
			want := 2
			if s.NoContext {
				want = 1
			}
			if got := strings.Count(string(calls), "attempt"); got != want {
				t.Fatalf("attempts = %d, want %d", got, want)
			}
			if tc.cause == "" {
				if s.Err != "" {
					t.Fatalf("retry failed: %+v", s)
				}
			} else if !strings.Contains(s.Err, tc.cause) {
				t.Fatalf("missing cause: %+v", s)
			}
			if s.Err != "" && !s.NoContext && !strings.Contains(s.Err, "deadline "+tc.timeout.String()) {
				t.Fatalf("missing deadline: %+v", s)
			}
		})
	}
}
