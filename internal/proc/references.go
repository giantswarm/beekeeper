package proc

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// References reads a process's files, mappings and temporary/build directory
// environment. An unreadable live process is an error: a pruner must keep
// candidates when it cannot establish whether that process uses them.
func References(pid int) ([]string, error) { return referencesAt(procRoot, pid) }

type referenceIO struct {
	readFile func(string) ([]byte, error)
	readDir  func(string) ([]os.DirEntry, error)
	readlink func(string) (string, error)
	stat     func(string) (os.FileInfo, error)
}

func referenceFiles() referenceIO {
	return referenceIO{os.ReadFile, os.ReadDir, os.Readlink, os.Stat}
}

func referencesAt(root string, pid int) ([]string, error) {
	return referencesWith(root, pid, false, referenceFiles())
}

func referencesWith(root string, pid int, protected bool, files referenceIO) ([]string, error) {
	dir := filepath.Join(root, strconv.Itoa(pid))
	ignored := func(err error) bool {
		return errors.Is(err, os.ErrNotExist) || protected && errors.Is(err, os.ErrPermission)
	}
	var paths []string
	links := []string{filepath.Join(dir, "cwd"), filepath.Join(dir, "exe")}
	fds, err := files.readDir(filepath.Join(dir, "fd"))
	if err != nil && !ignored(err) {
		return nil, err
	}
	for _, fd := range fds {
		links = append(links, filepath.Join(dir, "fd", fd.Name()))
	}
	for _, link := range links {
		p, err := files.readlink(link)
		if err != nil && !ignored(err) {
			return nil, err
		}
		if err == nil && filepath.IsAbs(p) {
			paths = append(paths, strings.TrimSuffix(p, " (deleted)"))
		}
	}
	for _, name := range []string{"environ", "maps"} {
		raw, err := files.readFile(filepath.Clean(filepath.Join(dir, name)))
		if err != nil && ignored(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if name == "environ" {
			for _, e := range bytes.Split(raw, []byte{0}) {
				k, v, _ := strings.Cut(string(e), "=")
				switch k {
				case "TMPDIR", "GOTMPDIR", "CARGO_TARGET_DIR":
					if filepath.IsAbs(v) {
						paths = append(paths, filepath.Clean(v))
					}
				}
			}
		} else {
			for line := range strings.Lines(string(raw)) {
				if i := strings.Index(line, "/"); i >= 0 {
					paths = append(paths, strings.TrimSuffix(strings.TrimSpace(line[i:]), " (deleted)"))
				}
			}
		}
	}
	return paths, nil
}

// UserScan scans every numeric process entry, including entries the
// general process table cannot read. Unknown ownership fails closed. Kernel
// EACCES on helpers protected by dumpability, credentials or capabilities
// is expected; other errors on live user processes remain failures.
//
// A process whose references cannot be read does not end the scan: the
// readable processes' references are returned with an *UnknownError naming
// the unreadable ones, so a caller can keep what they could reference while
// still deciding with the rest. An error on the process table itself is
// returned alone. A process the kernel refuses in another namespace is
// skipped into Scan.Foreign.
func UserScan(uid int) (Scan, error) { return userScanAt(procRoot, uid, referenceFiles()) }

// Scan is one read of the user's processes' references.
type Scan struct {
	Refs     []string
	Commands []string
	// Foreign lists the user's-uid processes the kernel refuses to read
	// that run in another pid or user namespace (a container's, a kind
	// node's pause container for one). Their paths name the container's
	// filesystem, not the person's, so they are skipped, not unknown.
	Foreign []UnknownProcess
}

// errForeign marks a process skipped by Scan.Foreign.
var errForeign = errors.New("unreadable process in another namespace")

// UnknownProcess is a process whose references could not be read.
type UnknownProcess struct {
	PID     int
	Command string
	Err     error
}

// UnknownError lists the processes whose references are unknown.
type UnknownError struct{ Processes []UnknownProcess }

func (e *UnknownError) Error() string {
	parts := make([]string, 0, len(e.Processes))
	for _, p := range e.Processes {
		name := strconv.Itoa(p.PID)
		if p.Command != "" {
			name += " (" + p.Command + ")"
		}
		parts = append(parts, "process "+name+": "+p.Err.Error())
	}
	return strings.Join(parts, "; ")
}

func (e *UnknownError) Unwrap() []error {
	errs := make([]error, 0, len(e.Processes))
	for _, p := range e.Processes {
		errs = append(errs, p.Err)
	}
	return errs
}

func userReferencesAt(root string, uid int, files referenceIO) ([]string, []string, error) {
	s, err := userScanAt(root, uid, files)
	return s.Refs, s.Commands, err
}

func userScanAt(root string, uid int, files referenceIO) (Scan, error) {
	entries, err := files.readDir(root)
	if err != nil {
		return Scan{}, err
	}
	var s Scan
	var unknown []UnknownProcess
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		paths, comm, ok, err := processReferences(root, pid, uid, files)
		if errors.Is(err, errForeign) {
			s.Foreign = append(s.Foreign, UnknownProcess{PID: pid, Command: comm, Err: err})
			continue
		}
		if err != nil {
			unknown = append(unknown, UnknownProcess{PID: pid, Command: comm, Err: err})
			continue
		}
		if ok {
			s.Refs = append(s.Refs, paths...)
			s.Commands = append(s.Commands, comm)
		}
	}
	if len(unknown) > 0 {
		return s, &UnknownError{Processes: unknown}
	}
	return s, nil
}

// processReferences reads one process. ok is false for a process that is
// gone, exited or another user's.
func processReferences(root string, pid, uid int, files referenceIO) ([]string, string, bool, error) {
	dir := filepath.Join(root, strconv.Itoa(pid))
	raw, err := files.readFile(filepath.Join(dir, "comm"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, "", false, nil
	}
	commErr := err
	comm := strings.TrimSpace(string(raw))
	raw, err = files.readFile(filepath.Join(dir, "status"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, "", false, nil
	}
	if err != nil {
		return nil, comm, false, err
	}
	owner, protected, err := referenceOwner(raw)
	if err != nil {
		return nil, comm, false, err
	}
	if owner != uid {
		return nil, "", false, nil
	}
	zombie, threads := referenceState(raw)
	if zombie && threads >= 0 && threads <= 1 {
		return nil, "", false, nil
	}
	// A cached directory inode may retain its original owner after a
	// process becomes non-dumpable. Ptrace-gated children reflect the
	// current protection, including when status omits Dumpable.
	for _, path := range []string{dir, filepath.Join(dir, "environ")} {
		info, err := files.stat(path)
		if errors.Is(err, os.ErrNotExist) {
			return nil, "", false, nil
		}
		if err != nil {
			return nil, comm, false, err
		}
		protected = protected || ownerUID(info) != uid
	}
	var paths []string
	live := true
	if zombie {
		paths, live, err = zombieThreadReferences(dir, pid, protected, files)
	} else {
		paths, err = referencesWith(root, pid, protected, files)
	}
	if errors.Is(err, os.ErrPermission) && foreignNamespace(root, pid, raw, files) {
		return nil, comm, false, fmt.Errorf("%w: %w", errForeign, err)
	}
	if err != nil {
		return nil, comm, false, err
	}
	if !live {
		return nil, "", false, nil
	}
	if commErr != nil {
		return nil, comm, false, commErr
	}
	return paths, comm, true, nil
}

// foreignNamespace reports a process in another pid namespace (NSpid lists
// more than one id) or another user namespace (its uid_map is not ours).
// The kernel refuses the person a container process that shares their uid
// but not their gid, a kind pod's fsGroup for one; such a process sees the
// container's filesystem, so its paths cannot name the person's files.
// Anything unreadable here counts as the person's own namespace.
func foreignNamespace(root string, pid int, status []byte, files referenceIO) bool {
	for line := range strings.Lines(string(status)) {
		if key, value, _ := strings.Cut(line, ":"); key == "NSpid" && len(strings.Fields(value)) > 1 {
			return true
		}
	}
	theirs, err := files.readFile(filepath.Join(root, strconv.Itoa(pid), "uid_map"))
	if err != nil {
		return false
	}
	ours, err := files.readFile(filepath.Join(root, "self", "uid_map"))
	if err != nil {
		return false
	}
	return !slices.Equal(strings.Fields(string(theirs)), strings.Fields(string(ours)))
}

// zombieThreadReferences reads a zombie thread-group leader through its
// other threads: the leader released its files and memory, while a live
// thread still holds the group's descriptors, mappings and directory.
// live is false when no thread but exited ones remain.
func zombieThreadReferences(dir string, pid int, protected bool, files referenceIO) ([]string, bool, error) {
	tasks := filepath.Join(dir, "task")
	entries, err := files.readDir(tasks)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var paths []string
	live := false
	for _, e := range entries {
		tid, err := strconv.Atoi(e.Name())
		if err != nil || tid == pid {
			continue
		}
		raw, err := files.readFile(filepath.Join(tasks, e.Name(), "status"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, false, err
		}
		if zombie, _ := referenceState(raw); zombie {
			continue
		}
		got, err := referencesWith(tasks, tid, protected, files)
		if err != nil {
			return nil, false, err
		}
		paths = append(paths, got...)
		live = true
	}
	return paths, live, nil
}

// referenceState reads whether a process is a zombie and its thread count,
// -1 when unknown. A zombie leader with more threads may have live ones.
func referenceState(status []byte) (bool, int) {
	zombie := false
	threads := -1
	for line := range strings.Lines(string(status)) {
		key, value, _ := strings.Cut(line, ":")
		switch key {
		case "State":
			fields := strings.Fields(value)
			zombie = len(fields) > 0 && fields[0] == "Z"
		case "Threads":
			if n, err := strconv.Atoi(strings.TrimSpace(value)); err == nil && n >= 0 {
				threads = n
			}
		}
	}
	return zombie, threads
}

func referenceOwner(status []byte) (int, bool, error) {
	uid := -1
	protected := false
	for line := range strings.Lines(string(status)) {
		key, value, _ := strings.Cut(line, ":")
		switch key {
		case "Uid", "Gid":
			realID, mixed, err := referenceIDs(value)
			if err != nil {
				return -1, false, err
			}
			if key == "Uid" {
				uid = realID
			}
			protected = protected || mixed
		case "CapPrm", "CapEff":
			capabilities, err := strconv.ParseUint(strings.TrimSpace(value), 16, 64)
			if err != nil {
				return -1, false, err
			}
			protected = protected || capabilities != 0
		case "Dumpable":
			protected = protected || strings.TrimSpace(value) == "0"
		}
	}
	if uid < 0 {
		return -1, false, errors.New("process Uid unavailable")
	}
	return uid, protected, nil
}

func referenceIDs(value string) (int, bool, error) {
	fields := strings.Fields(value)
	if len(fields) != 4 {
		return -1, false, errors.New("malformed process credentials")
	}
	realID := -1
	mixed := false
	for i, field := range fields {
		id, err := strconv.Atoi(field)
		if err != nil || id < 0 {
			return -1, false, errors.New("invalid process credentials")
		}
		if i == 0 {
			realID = id
		} else {
			mixed = mixed || id != realID
		}
	}
	return realID, mixed, nil
}
