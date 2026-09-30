package proc

import (
	"context"
	"os/exec"
	"time"
)

// Niceness is the nice level of a command started under a Background
// context: behind every interactive process on a starved machine.
const Niceness = "10"

// waitDelay bounds the wait for a killed command's pipes: a kubectl killed on
// its deadline whose credential plugin (tsh) still holds its stdout must not
// hold up the caller.
const waitDelay = 5 * time.Second

type backgroundKey struct{}

// Background marks ctx: the commands Command starts under it run at nice
// Niceness.
func Background(ctx context.Context) context.Context {
	return context.WithValue(ctx, backgroundKey{}, true)
}

// IsBackground reports whether ctx is marked by Background.
func IsBackground(ctx context.Context) bool {
	b, _ := ctx.Value(backgroundKey{}).(bool)
	return b
}

// Command is exec.CommandContext for a read the caller can do without: under
// a Background context it runs through nice (when the machine has it), and
// a command killed by ctx gives up its pipes after waitDelay.
func Command(ctx context.Context, name string, arg ...string) *exec.Cmd {
	if IsBackground(ctx) {
		if nice, err := exec.LookPath("nice"); err == nil {
			arg = append([]string{"-n", Niceness, name}, arg...)
			name = nice
		}
	}
	c := exec.CommandContext(ctx, name, arg...)
	c.WaitDelay = waitDelay
	return c
}
