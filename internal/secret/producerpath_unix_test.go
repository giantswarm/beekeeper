//go:build unix

package secret

import (
	"context"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// opensslName is the producer the tests stand a fake in for.
const opensslName = "openssl"

// randHex16 is the allowed producer the tests run.
var randHex16 = []string{opensslName, "rand", "-hex", "16"}

// writeExec writes an executable script name into dir.
func writeExec(t *testing.T, dir, name, script string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o700); err != nil { //nolint:gosec // an executable the test runs
		t.Fatal(err)
	}
}

// An openssl wrapper first on the caller's PATH never runs: the real
// openssl generates the value, from the system's directory.
func TestProduceIgnoresAWrapperOnTheCallersPATH(t *testing.T) {
	if _, err := producerPath(opensslName); err != nil {
		t.Skipf("no trusted openssl: %v", err)
	}
	dir := t.TempDir()
	ran := filepath.Join(dir, "ran")
	writeExec(t, dir, opensslName, "#!/bin/sh\ntouch "+ran+"\necho wrapped\n")
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	out, err := produce(context.Background(), CaptureOptions{Producer: randHex16, Env: ProducerEnv(os.Environ(), nil)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(ran); err == nil {
		t.Error("the wrapper on the caller's PATH ran")
	}
	if v := strings.TrimSpace(string(out)); len(v) != 32 {
		t.Errorf("openssl rand -hex 16 printed %d characters, want 32", len(v))
	} else if _, err := hex.DecodeString(v); err != nil {
		t.Error("openssl rand -hex 16 printed no hex")
	}
}

// The caller's LD_PRELOAD, PYTHONPATH, BASH_ENV, PATH, HOME and working
// directory never reach the producer: it runs in / with the clean
// environment alone.
func TestProduceRunsInACleanEnvironment(t *testing.T) {
	dir := t.TempDir()
	t.Cleanup(UseProducersIn(dir))
	writeExec(t, dir, opensslName, "#!/bin/sh\necho \"${LD_PRELOAD-none} ${PYTHONPATH-none} ${BASH_ENV-none} $PATH $(pwd)\"\n")
	t.Setenv("LD_PRELOAD", filepath.Join(dir, "absent.so"))
	t.Setenv("PYTHONPATH", dir)
	t.Setenv("BASH_ENV", filepath.Join(dir, "env.sh"))
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	t.Chdir(dir)
	out, err := produce(context.Background(), CaptureOptions{Producer: randHex16, Env: ProducerEnv(os.Environ(), nil)})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.TrimSpace(string(out)), "none none none /usr/bin:/bin /"; got != want {
		t.Errorf("the producer saw %q, want %q", got, want)
	}
}

// A producer only outside the system's directories, or in one a user
// other than root owns or a group can write, is refused before it runs.
func TestProducerPathRefusesAnUntrustedProducer(t *testing.T) {
	dir := t.TempDir()
	ran := filepath.Join(dir, "ran")
	writeExec(t, dir, opensslName, "#!/bin/sh\ntouch "+ran+"\n")
	dirs := producerDirs
	t.Cleanup(func() { producerDirs = dirs })

	producerDirs = []string{filepath.Join(dir, "none")}
	if _, err := producerPath(opensslName); err == nil || !strings.Contains(err.Error(), "is in none of") {
		t.Errorf("an absent producer: %v", err)
	}
	producerDirs = []string{dir}
	_, err := producerPath(opensslName)
	if err == nil || !strings.Contains(err.Error(), "nothing run") {
		t.Errorf("a producer in %s: %v", dir, err)
	}
	if _, err := produce(context.Background(), CaptureOptions{Producer: []string{opensslName, "rand", "16"}}); err == nil {
		t.Error("an untrusted producer ran")
	}
	if _, err := os.Stat(ran); err == nil {
		t.Error("an untrusted producer ran")
	}
}
