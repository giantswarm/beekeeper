//go:build scenario && linux

package harness

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/giantswarm/beekeeper/internal/clock"
)

// A fake whose starter cleaned its environment names the world's clock
// again, so that what it starts reads the world's time. It leaves a clock
// its environment names alone, and names none where no clock listens.
func TestNameClock(t *testing.T) {
	dir := t.TempDir()
	socket := filepath.Join(dir, "clock.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	file := filepath.Join(dir, "clock.txt")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, env, path, want string
	}{
		{"cleaned environment", "", socket, socket},
		{"named already", "/elsewhere/clock.sock", socket, "/elsewhere/clock.sock"},
		{"no clock", "", filepath.Join(dir, "missing.sock"), ""},
		{"no socket", "", file, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(clock.Env, tc.env)
			nameClock(tc.path)
			if got := os.Getenv(clock.Env); got != tc.want {
				t.Fatalf("%s = %q, want %q", clock.Env, got, tc.want)
			}
		})
	}
}
