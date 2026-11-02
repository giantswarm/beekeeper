package proc

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

const (
	fixtureCommand     = "comm"
	fixtureCWD         = "cwd"
	fixtureStatus      = "status"
	fixtureEnvironment = "environ"
	fixtureMappings    = "maps"
	fixtureStat        = "stat"
	fixtureZombieState = "Z (zombie)"
	fixtureLiveStatus  = "State: S\nThreads: 1\n"
	fixtureName        = "fixture"
)

func TestReferencesReadsOpenFilesMappingsAndBuildDirectories(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "42")
	if err := os.MkdirAll(filepath.Join(dir, "fd"), 0700); err != nil {
		t.Fatal(err)
	}
	paths := []string{filepath.Join(root, "work"), filepath.Join(root, "output"), filepath.Join(root, "mapped"), filepath.Join(root, "tmp")}
	if err := os.Symlink(paths[0], filepath.Join(dir, fixtureCWD)); err != nil {
		t.Skip(err)
	}
	if err := os.Symlink(paths[1]+" (deleted)", filepath.Join(dir, "fd", "3")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, fixtureMappings), []byte("00-10 r--p 0 0:0 1 "+paths[2]+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, fixtureEnvironment), []byte("GOTMPDIR="+paths[3]+"\x00UNRELATED=value\x00"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := referencesAt(root, 42)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range paths {
		if !slices.Contains(got, p) {
			t.Errorf("missing reference %s", filepath.Base(p))
		}
	}
	// A file masquerading as /proc/<pid>/fd is not an empty set of files.
	if err := os.Remove(filepath.Join(dir, "fd", "3")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "fd")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "fd"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := referencesAt(root, 42); err == nil || strings.TrimSpace(err.Error()) == "" {
		t.Fatal("unreadable references looked unused")
	}
}

func TestUserReferencesDistinguishesProtectedHelpersFromUnreadableProcesses(t *testing.T) {
	for _, tc := range []struct {
		name, status, denied string
		wantErr              bool
	}{
		{"non-dumpable helper", "Dumpable: 0\n", fixtureEnvironment, false},
		{"credential-changing helper", "setuid", fixtureMappings, false},
		{"kernel-owned helper", "kernel-owned", fixtureEnvironment, false},
		{"permitted capabilities", "CapPrm: 0000000000000001\n", fixtureEnvironment, false},
		{"effective capabilities", "CapEff: 0000000000000001\n", fixtureCWD, false},
		{"mixed groups", "Gid: 1 2 1 1\n", fixtureMappings, false},
		{"ordinary credentials", "Gid: 1 1 1 1\nCapPrm: 0000000000000000\nCapEff: 0000000000000000\n", fixtureEnvironment, true},
		{"dumpable environment", "", fixtureEnvironment, true},
		{"dumpable mappings", "", fixtureMappings, true},
		{"dumpable descriptors", "", "fd", true},
		{"dumpable cwd", "", fixtureCWD, true},
		{"dumpable descriptor target", "", "3", true},
		{"unknown ownership", "", fixtureStatus, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "42")
			if err := os.MkdirAll(filepath.Join(dir, "fd"), 0700); err != nil {
				t.Fatal(err)
			}
			uid := os.Getuid()
			if tc.status == "kernel-owned" {
				uid++
			}
			status := fmt.Sprintf("Uid: %d %d %d %d\n", uid, uid, uid, uid) + tc.status
			if tc.status == "setuid" {
				status = fmt.Sprintf("Uid: %d %d %d %d\n", uid, uid+1, uid, uid)
			}
			for name, data := range map[string]string{fixtureStatus: status, fixtureCommand: "go", fixtureEnvironment: "", fixtureMappings: "", "fd/3": ""} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0600); err != nil {
					t.Fatal(err)
				}
			}
			files := referenceFiles()
			files.readFile = func(p string) ([]byte, error) {
				if filepath.Base(p) == tc.denied {
					return nil, os.ErrPermission
				}
				return os.ReadFile(filepath.Clean(p))
			}
			files.readDir = func(p string) ([]os.DirEntry, error) {
				if filepath.Base(p) == tc.denied {
					return nil, os.ErrPermission
				}
				return os.ReadDir(p)
			}
			files.readlink = func(p string) (string, error) {
				if filepath.Base(p) == tc.denied {
					return "", os.ErrPermission
				}
				return "/scratch/output", nil
			}
			refs, commands, err := userReferencesAt(root, uid, files)
			if (err != nil) != tc.wantErr {
				t.Fatalf("error=%v, want error=%v", err, tc.wantErr)
			}
			if !tc.wantErr && (!slices.Contains(commands, "go") || len(refs) == 0) {
				t.Fatal("lost readable helper references or build command")
			}
			if err != nil && strings.Contains(err.Error(), "process references unavailable:") {
				t.Fatal("duplicate error wrapper")
			}
		})
	}
}

func TestUserReferencesFailsClosedWithoutProcessRootOrOwnership(t *testing.T) {
	root := t.TempDir()
	if _, _, err := userReferencesAt(filepath.Join(root, "missing"), os.Getuid(), referenceFiles()); err == nil {
		t.Fatal("missing process root looked idle")
	}
	if _, _, err := referenceOwner([]byte("Name: helper\n")); err == nil {
		t.Fatal("missing Uid accepted")
	}
	files := referenceFiles()
	files.readDir = func(string) ([]os.DirEntry, error) { return nil, os.ErrPermission }
	if _, _, err := userReferencesAt(root, os.Getuid(), files); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("root error=%v", err)
	}
	files = referenceFiles()
	files.readlink = func(string) (string, error) { return "", errors.New("link read failed") }
	if _, err := referencesWith(root, 42, true, files); err == nil {
		t.Fatal("helper hid an unexpected readlink failure")
	}
}

func TestUserReferencesSkipsOnlyConfirmedZombies(t *testing.T) {
	for _, tc := range []struct {
		name, state, threads string
		exited, inspect      bool
	}{
		{"single-thread zombie", fixtureZombieState, "1", true, false},
		{"zero-thread zombie", fixtureZombieState, "0", true, false},
		{"zombie leader with live threads", fixtureZombieState, "2", false, true},
		{"unknown zombie threads", fixtureZombieState, "", false, true},
		{"invalid zombie threads", fixtureZombieState, "invalid", false, true},
		{"negative zombie threads", fixtureZombieState, "-1", false, true},
		{"sleeping", "S (sleeping)", "1", false, true},
		{"malformed state", "Zebra", "1", false, true},
		{"missing state", "", "1", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "42")
			uid := os.Getuid()
			status := fmt.Sprintf("State:\t%s\nUid:\t%d %d %d %d\n", tc.state, uid, uid, uid, uid)
			if tc.threads != "" {
				status += "Threads:\t" + tc.threads + "\n"
			}
			// The leader's own task is a zombie; thread 43 is alive.
			for _, d := range []string{dir, filepath.Join(dir, "task", "42"), filepath.Join(dir, "task", "43")} {
				if err := os.MkdirAll(filepath.Join(d, "fd"), 0700); err != nil {
					t.Fatal(err)
				}
				data := status
				if filepath.Base(d) == "43" {
					data = fixtureLiveStatus
				}
				for name, content := range map[string]string{fixtureStatus: data, fixtureEnvironment: "", fixtureCommand: fixtureName} {
					if err := os.WriteFile(filepath.Join(d, name), []byte(content), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			files := referenceFiles()
			inspected := false
			files.readFile = func(path string) ([]byte, error) {
				if filepath.Base(path) == fixtureEnvironment {
					inspected = true
					return nil, &os.PathError{Op: "read", Path: path, Err: os.ErrPermission}
				}
				return os.ReadFile(filepath.Clean(path))
			}
			refs, commands, err := userReferencesAt(root, uid, files)
			if tc.exited {
				if err != nil || inspected || len(refs) != 0 || len(commands) != 0 {
					t.Fatalf("zombie did not count as exited: refs=%v commands=%v inspected=%v err=%v", refs, commands, inspected, err)
				}
			} else if err == nil || inspected != tc.inspect {
				t.Fatalf("unconfirmed zombie did not fail closed: inspected=%v err=%v", inspected, err)
			} else if !errors.Is(err, os.ErrPermission) {
				t.Fatalf("live process lost permission error: %v", err)
			}
		})
	}
}

// A zombie leader is read through its live threads and counts as exited
// once every other thread has exited too.
func TestUserReferencesReadsZombieLeaderThroughLiveThreads(t *testing.T) {
	for _, tc := range []struct {
		name, thread string
		want         bool
	}{
		{"live thread", fixtureLiveStatus, true},
		{"exited thread", "State: Z\n", false},
		{"vanished thread", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "42")
			uid := os.Getuid()
			ids := fmt.Sprintf("Uid: %d %d %d %d\n", uid, uid, uid, uid)
			for _, d := range []string{dir, filepath.Join(dir, "task", "42"), filepath.Join(dir, "task", "43")} {
				if err := os.MkdirAll(filepath.Join(d, "fd"), 0700); err != nil {
					t.Fatal(err)
				}
				data := ids + "State: Z (zombie)\nThreads: 2\n"
				if filepath.Base(d) == "43" {
					data = tc.thread
				}
				for name, content := range map[string]string{fixtureStatus: data, fixtureEnvironment: "", fixtureMappings: "", fixtureCommand: fixtureName} {
					if err := os.WriteFile(filepath.Join(d, name), []byte(content), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			if tc.thread == "" {
				if err := os.Remove(filepath.Join(dir, "task", "43", fixtureStatus)); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink("/scratch/held", filepath.Join(dir, "task", "43", "fd", "3")); err != nil {
				t.Skip(err)
			}
			// The leader itself has released its files.
			files := referenceFiles()
			files.readDir = func(path string) ([]os.DirEntry, error) {
				if path == filepath.Join(dir, "fd") {
					return nil, syscall.ESRCH
				}
				return os.ReadDir(path)
			}
			refs, commands, err := userReferencesAt(root, uid, files)
			if err != nil {
				t.Fatal(err)
			}
			if got := slices.Contains(refs, "/scratch/held"); got != tc.want || (len(commands) == 1) != tc.want {
				t.Fatalf("refs=%v commands=%v, want thread references=%v", refs, commands, tc.want)
			}
		})
	}
}

// One unreadable process leaves the others' references known.
func TestUserReferencesKeepsReadableProcessesNextToUnreadable(t *testing.T) {
	root := t.TempDir()
	uid := os.Getuid()
	for _, pid := range []string{"42", "43"} {
		dir := filepath.Join(root, pid)
		if err := os.MkdirAll(filepath.Join(dir, "fd"), 0700); err != nil {
			t.Fatal(err)
		}
		for name, content := range map[string]string{fixtureStatus: fmt.Sprintf("Uid: %d %d %d %d\n", uid, uid, uid, uid), fixtureEnvironment: "", fixtureMappings: "", fixtureCommand: "p" + pid} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := os.Symlink("/scratch/held", filepath.Join(root, "43", "fd", "3")); err != nil {
		t.Skip(err)
	}
	files := referenceFiles()
	files.readFile = func(path string) ([]byte, error) {
		if path == filepath.Join(root, "42", fixtureMappings) {
			return nil, syscall.EIO
		}
		return os.ReadFile(filepath.Clean(path))
	}
	refs, commands, err := userReferencesAt(root, uid, files)
	var unknown *UnknownError
	if !errors.As(err, &unknown) || len(unknown.Processes) != 1 || unknown.Processes[0].PID != 42 || unknown.Processes[0].Command != "p42" || !errors.Is(err, syscall.EIO) {
		t.Fatalf("unreadable process not named: %v", err)
	}
	if !slices.Contains(refs, "/scratch/held") || !slices.Equal(commands, []string{"p43"}) {
		t.Fatalf("readable process lost: refs=%v commands=%v", refs, commands)
	}
}

// Exercise real proc entries, including zombies when present, without
// depending on the readability of unrelated live processes.
func TestUserReferencesAgainstLiveProc(t *testing.T) {
	if _, err := os.Stat("/proc/self/status"); errors.Is(err, os.ErrNotExist) {
		t.Skip("Linux proc is unavailable")
	} else if err != nil {
		t.Fatal(err)
	}
	held, err := os.CreateTemp(t.TempDir(), "held-")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Close() }()
	// Only this process and confirmed exited zombies belong to this live probe.
	// Other live processes may be ptrace-restricted by their namespace;
	// fixture tests above prove those errors still fail closed in production.
	files := referenceFiles()
	files.readDir = func(path string) ([]os.DirEntry, error) {
		entries, err := os.ReadDir(path)
		if path != procRoot || err != nil {
			return entries, err
		}
		var selected []os.DirEntry
		for _, entry := range entries {
			if entry.Name() == strconv.Itoa(os.Getpid()) {
				selected = append(selected, entry)
				continue
			}
			if _, err := strconv.Atoi(entry.Name()); err != nil {
				continue
			}
			status, err := os.ReadFile(filepath.Join(procRoot, entry.Name(), "status"))
			if err != nil {
				continue
			} // A process can exit after ReadDir.
			if zombie, threads := referenceState(status); zombie && threads >= 0 && threads <= 1 {
				selected = append(selected, entry)
			}
		}
		return selected, nil
	}
	refs, commands, err := userReferencesAt(procRoot, os.Getuid(), files)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(refs, held.Name()) || len(commands) == 0 {
		t.Fatal("live scan missed this process's open scratch file")
	}
}

func TestReferenceOwnerRejectsMalformedProtectionMetadata(t *testing.T) {
	for _, status := range []string{"Uid: 1 1 1\n", "Uid: 1 1 1 -1\n", "Uid: 1 1 1 1\nGid: 1 1\n", "Uid: 1 1 1 1\nCapPrm: invalid\n", "Uid: 1 1 1 1\nCapEff: invalid\n"} {
		if _, _, err := referenceOwner([]byte(status)); err == nil {
			t.Fatal("malformed credentials looked readable")
		}
	}
}

// Missing entries are exit races. ESRCH alone cannot confirm exit: a
// zombie leader may have live threads, and unknown live errors fail closed.
func TestUserReferencesToleratesExitingProcesses(t *testing.T) {
	for _, target := range []string{"status", fixtureStat, "environ", "maps", "fd", fixtureCWD, "exe", fixtureCommand} {
		for _, tc := range []struct {
			name, status string
			failure      error
			gone, leader bool
		}{
			{"vanished", fixtureLiveStatus, os.ErrNotExist, true, false},
			{"live ESRCH", fixtureLiveStatus, syscall.ESRCH, false, false},
			{"live I/O error", fixtureLiveStatus, syscall.EIO, false, false},
			{"zombie leader ESRCH", "State: Z\nThreads: 2\n", syscall.ESRCH, false, true},
			{"exited zombie ESRCH", "State: Z\nThreads: 1\n", syscall.ESRCH, true, false},
		} {
			t.Run(target+"/"+tc.name, func(t *testing.T) {
				root := t.TempDir()
				dir := filepath.Join(root, "42")
				if err := os.MkdirAll(filepath.Join(dir, "fd"), 0700); err != nil {
					t.Fatal(err)
				}
				uid := os.Getuid()
				for name, data := range map[string]string{"status": fmt.Sprintf("Uid: %d %d %d %d\n", uid, uid, uid, uid) + tc.status, "environ": "", "maps": "", fixtureCommand: fixtureName} {
					if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0600); err != nil {
						t.Fatal(err)
					}
				}
				if tc.leader {
					// A zombie leader is read through its live thread.
					thread := filepath.Join(dir, "task", "43")
					if err := os.MkdirAll(filepath.Join(thread, "fd"), 0700); err != nil {
						t.Fatal(err)
					}
					for name, data := range map[string]string{fixtureStatus: fixtureLiveStatus, fixtureEnvironment: "", fixtureMappings: ""} {
						if err := os.WriteFile(filepath.Join(thread, name), []byte(data), 0600); err != nil {
							t.Fatal(err)
						}
					}
				}
				failure := tc.failure
				files := referenceFiles()
				files.readFile = func(path string) ([]byte, error) {
					if filepath.Base(path) == target {
						return nil, &os.PathError{Op: "read", Path: path, Err: failure}
					}
					return os.ReadFile(filepath.Clean(path))
				}
				files.readDir = func(path string) ([]os.DirEntry, error) {
					if filepath.Base(path) == target {
						return nil, failure
					}
					return os.ReadDir(path)
				}
				files.readlink = func(path string) (string, error) {
					if filepath.Base(path) == target {
						return "", failure
					}
					return "/scratch/held", nil
				}
				files.stat = func(path string) (os.FileInfo, error) {
					if target == fixtureStat {
						return nil, failure
					}
					return os.Stat(path)
				}
				_, _, err := userReferencesAt(root, uid, files)
				wantGone := tc.gone && (target != fixtureStatus || errors.Is(failure, os.ErrNotExist))
				if wantGone {
					if err != nil {
						t.Fatalf("exited process blocked scan: %v", err)
					}
				} else if err == nil {
					t.Fatal("unconfirmed exit did not fail closed")
				} else if !errors.Is(err, failure) {
					t.Fatalf("unexpected process error lost: %v", err)
				}
			})
		}
	}
}
