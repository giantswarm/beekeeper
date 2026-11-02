//go:build linux

package cmd

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/platform"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/pkg/project"
)

// A real executable replaced just after startup remains visible even when
// its install falls inside the process-start timestamp's uncertain second.
func TestStaleBinariesReplacementAtStartup(t *testing.T) {
	raw, err := os.ReadFile("/bin/sh")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "beekeeper")
	for _, name := range []string{path, path + ".new"} {
		if err := os.WriteFile(name, raw, 0o755); err != nil { //nolint:gosec // executable fixtures in the test directory
			t.Fatal(err)
		}
	}
	child := exec.Command(path, "-c", "echo ready; read line") //nolint:gosec // executable fixture in the test directory
	out, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	in, err := child.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = in.Close(); _ = child.Process.Kill(); _ = child.Wait() })
	if line, err := bufio.NewReader(out).ReadString('\n'); err != nil || line != "ready\n" {
		t.Fatalf("child readiness: %q, %v", line, err)
	}
	if err := os.Rename(path+".new", path); err != nil {
		t.Fatal(err)
	}
	install, ok := platform.ProcessBinary(child.Process.Pid).Replacement()
	if !ok || install.Path != path {
		t.Fatalf("replacement = %+v, %v", install, ok)
	}
	if elapsed := install.At.Sub(started); elapsed < 0 || elapsed >= time.Second {
		t.Skipf("startup took %s; cannot exercise the subsecond interval under this load", elapsed)
	}
	p := &proc.Process{PID: child.Process.Pid, Comm: project.Name, Start: started}
	tab := &proc.Table{ByPID: map[int]*proc.Process{p.PID: p}}
	got := staleBinaries(tab, os.Getpid(), replacedBinary)
	if len(got) != 1 || got[0].by.Path != path || !got[0].by.At.Equal(install.At) {
		t.Fatalf("startup replacement not reported: %+v", got)
	}
}
