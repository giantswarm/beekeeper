package claude

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDesktopFocus(t *testing.T) {
	const log = `2026-09-25 13:14:03 [info] Resume deep link: importing CLI session 2326e971
2026-09-25 13:14:04 [info] [CCD] LocalSessions.setFocusedSession: sessionId=null
2026-09-25 13:14:04 [info] [CCD] LocalSessions.setFocusedSession: sessionId=local_2326e971
2026-09-25 13:14:05 [info] [Stop hook] Query completed for session local_other
`
	for name, tc := range map[string]struct{ log, want string }{
		"the last change wins":  {log, "local_2326e971"},
		"a view that is none":   {strings.SplitAfter(log, "\n")[0] + strings.SplitAfter(log, "\n")[1], ""},
		"no change logged":      {"2026-09-25 [info] started\n", ""},
		"a line cut off at EOF": {"x LocalSessions.setFocusedSession: sessionId=local_ab", "local_ab"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := lastFocus([]byte(tc.log)); got != tc.want {
				t.Errorf("lastFocus = %q, want %q", got, tc.want)
			}
		})
	}

	// Only the log's tail is read.
	p := filepath.Join(t.TempDir(), "main.log")
	head := "x LocalSessions.setFocusedSession: sessionId=local_old\n"
	if err := os.WriteFile(p, []byte(head+strings.Repeat("y\n", focusTail)), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := DesktopFocus(p); err != nil || got != "" {
		t.Errorf("DesktopFocus past the tail = %q, %v", got, err)
	}
	if _, err := DesktopFocus(filepath.Join(t.TempDir(), "none.log")); err == nil {
		t.Error("DesktopFocus of a missing log: no error")
	}
}

// A warm spawn the desktop's CLI governor declined since a show reads as
// its cap; an older one, or none, as 0.
func TestAtCap(t *testing.T) {
	since := time.Date(2026, 10, 2, 13, 41, 30, 0, time.Local)
	log := []byte("2026-10-02 13:41:24 [info] [CliGovernor] at cap=27; would evict local_y (idle 1s) for warm spawn\n" +
		"2026-10-02 13:41:24 [info] [CliGovernor] at cap; yielding warm spawn\n" +
		"2026-10-02 13:41:30 [info] [CliGovernor] at cap=28; would evict local_x (idle 83238s) for warm spawn\n" +
		"2026-10-02 13:41:30 [info] [CliGovernor] at cap; yielding warm spawn\n")
	if n, ok := atCap(log, since); !ok || n != 28 {
		t.Errorf("atCap = %d, %v, want 28", n, ok)
	}
	if n, ok := atCap(log, since.Add(time.Second)); ok {
		t.Errorf("atCap after the decline = %d, want none", n)
	}
}
