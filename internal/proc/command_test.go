package proc

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

func TestCommandNicesBackgroundReads(t *testing.T) {
	if _, err := exec.LookPath("nice"); err != nil {
		t.Skip("no nice on this machine")
	}
	// Field 19 of /proc/self/stat is the nice value of the process reading it.
	nice := func(ctx context.Context) (int, *exec.Cmd) {
		c := Command(ctx, "sh", "-c", "cut -d' ' -f19 /proc/self/stat")
		out, err := c.Output()
		if err != nil {
			t.Fatal(err)
		}
		n, err := strconv.Atoi(strings.TrimSpace(string(out)))
		if err != nil {
			t.Fatal(err)
		}
		return n, c
	}
	ctx := context.Background()
	fg, c := nice(ctx)
	if c.WaitDelay == 0 {
		t.Error("a command has no wait delay for its pipes")
	}
	bg, _ := nice(Background(ctx))
	want, _ := strconv.Atoi(Niceness)
	if want = min(fg+want, 19); bg != want {
		t.Fatalf("a background command runs at nice %d, a foreground one at %d: want %d", bg, fg, want)
	}
}
