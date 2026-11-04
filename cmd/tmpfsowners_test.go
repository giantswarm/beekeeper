package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/proc"
)

func TestTmpfsLargestNamesEntriesAndWorkerOwners(t *testing.T) {
	root := t.TempDir()
	for i, name := range []string{litSmall, "medium", "large", "largest"} {
		writeT(t, filepath.Join(root, name, "data"), strings.Repeat("x", 8192*(i+1)))
	}
	table := proc.NewTable(&proc.Process{PID: 10, Comm: workerRole}, &proc.Process{PID: 11, PPID: 10, Comm: "go"}, &proc.Process{PID: 12, Comm: "gone"})
	refs := func(pid int) ([]string, error) {
		if pid == 11 {
			return []string{filepath.Join(root, "largest", "data"), filepath.Join(root, "largest", "other"), filepath.Join(root, "large-other", "data")}, nil
		}
		return nil, errors.New("process unavailable")
	}
	got := tmpfsLargest(root, table, map[int]string{10: "Build issue 797"}, refs)
	for _, want := range []string{`"largest"`, `go pid 11 ("Build issue 797")`, `"large"`, `"medium"`, "owner unknown"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q missing %q", got, want)
		}
	}
	if strings.Contains(got, `"small"`) {
		t.Errorf("included fourth entry: %s", got)
	}
	if strings.Index(got, `"largest"`) > strings.Index(got, `"large"`) {
		t.Errorf("not sorted: %s", got)
	}
	if strings.Count(got, "go pid 11") != 1 {
		t.Errorf("duplicate or prefix attribution: %s", got)
	}
}

func TestTmpfsLargestDoesNotFollowSymlinks(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	writeT(t, filepath.Join(outside, "data"), strings.Repeat("x", 2<<20))
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	got := tmpfsLargest(root, nil, nil, nil)
	if !strings.Contains(got, `"link"`) || !strings.Contains(got, "owner unknown") || !strings.Contains(got, "0 MiB") {
		t.Fatal(got)
	}
	if got := tmpfsLargest(filepath.Join(root, "absent"), nil, nil, nil); got != "largest entries unavailable" {
		t.Fatal(got)
	}
	if got := tmpfsLargest(t.TempDir(), nil, nil, nil); !strings.Contains(got, "deleted open files") {
		t.Fatal(got)
	}
}
