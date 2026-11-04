package config

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const editPathEnv = "BEEKEEPER_TEST_CONFIG_EDIT_PATH"

func TestChangeConcurrentWriter(t *testing.T) {
	p := os.Getenv(editPathEnv)
	if p == "" {
		return
	}
	if _, err := fmt.Fprintln(os.Stdout, "ready"); err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, os.Stdin); err != nil {
		t.Fatal(err)
	}
	key := "metrics.models.writer-" + os.Getenv("BEEKEEPER_TEST_CONFIG_EDIT_INDEX") + ".input"
	if _, err := Set(p, []Setting{{Key: key, Value: "1"}}); err != nil {
		t.Fatal(err)
	}
}

// Independently started commands must preserve each other's unrelated keys,
// including when a command reaches the same configuration through a symlink.
func TestChangeConcurrentProcesses(t *testing.T) {
	p := writeConfig(t, "# "+strings.Repeat("padding ", 32*1024)+"\ngrantTTL: 20m\n")
	link := filepath.Join(t.TempDir(), "linked.yaml")
	if err := os.Symlink(p, link); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	type writer struct {
		cmd   *exec.Cmd
		start io.WriteCloser
		log   bytes.Buffer
	}
	const count = 8
	writers := make([]writer, count)
	for i := range writers {
		w := &writers[i]
		target := p
		if i%2 != 0 {
			target = link
		}
		w.cmd = exec.Command(executable, "-test.run=^TestChangeConcurrentWriter$") //nolint:gosec // this test binary, with fixed helper arguments
		w.cmd.Env = append(os.Environ(), editPathEnv+"="+target, fmt.Sprintf("BEEKEEPER_TEST_CONFIG_EDIT_INDEX=%d", i))
		w.cmd.Stderr = &w.log
		w.start, err = w.cmd.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		ready, err := w.cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := w.cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_ = w.start.Close()
			_ = w.cmd.Process.Kill()
			_ = w.cmd.Wait()
		})
		if line, err := bufio.NewReader(ready).ReadString('\n'); err != nil || line != "ready\n" {
			t.Fatalf("writer not ready: %q, %v", line, err)
		}
	}
	for i := range writers {
		if err := writers[i].start.Close(); err != nil {
			t.Fatal(err)
		}
	}
	for i := range writers {
		if err := writers[i].cmd.Wait(); err != nil {
			t.Fatalf("writer %d: %v: %s", i, err, writers[i].log.String())
		}
	}
	for i := range count {
		key := fmt.Sprintf("metrics.models.writer-%d.input", i)
		if value, found, err := Get(p, key); err != nil || !found || value != "1" {
			t.Errorf("successful edit lost: %s = %q, found %v, error %v", key, value, found, err)
		}
	}
}

func TestChangeLockFailureLeavesConfig(t *testing.T) {
	const raw = "grantTTL: 20m\n"
	p := writeConfig(t, raw)
	if err := os.Mkdir(p+".lock", 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Set(p, []Setting{{Key: keyGrantTTL, Value: "1m"}}); err == nil {
		t.Fatal("edit succeeded without a lock")
	}
	if got, err := os.ReadFile(filepath.Clean(p)); err != nil || string(got) != raw {
		t.Fatalf("lock failure changed configuration: %v", err)
	}
}
