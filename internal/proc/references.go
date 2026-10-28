package proc

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
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

// UserReferences scans every numeric process entry, including entries the
// general process table cannot read. Unknown ownership fails closed. Kernel
// EACCES on a non-dumpable or credential-changing helper is expected; other
// errors on live user processes remain failures.
func UserReferences(uid int) ([]string, []string, error) {
	return userReferencesAt(procRoot, uid, referenceFiles())
}

func userReferencesAt(root string, uid int, files referenceIO) ([]string, []string, error) {
	entries, err := files.readDir(root)
	if err != nil {
		return nil, nil, err
	}
	var refs, commands []string
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		dir := filepath.Join(root, e.Name())
		raw, err := files.readFile(filepath.Join(dir, "status"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		owner, protected, err := referenceOwner(raw)
		if err != nil {
			return nil, nil, err
		}
		if owner != uid {
			continue
		}
		info, err := files.stat(dir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		// Current kernels omit Dumpable from status, but reset proc directory
		// ownership when a process makes itself non-dumpable.
		protected = protected || ownerUID(info) != uid
		paths, err := referencesWith(root, pid, protected, files)
		if err != nil {
			return nil, nil, err
		}
		comm, err := files.readFile(filepath.Join(dir, "comm"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		refs = append(refs, paths...)
		commands = append(commands, strings.TrimSpace(string(comm)))
	}
	return refs, commands, nil
}

func referenceOwner(status []byte) (int, bool, error) {
	uid := -1
	protected := false
	for line := range strings.Lines(string(status)) {
		if v, ok := strings.CutPrefix(line, "Uid:"); ok {
			fields := strings.Fields(v)
			if len(fields) != 4 {
				return -1, false, errors.New("malformed process Uid")
			}
			for i, f := range fields {
				n, err := strconv.Atoi(f)
				if err != nil {
					return -1, false, err
				}
				if i == 0 {
					uid = n
				} else {
					protected = protected || n != uid
				}
			}
		}
		if v, ok := strings.CutPrefix(line, "Dumpable:"); ok && strings.TrimSpace(v) == "0" {
			protected = true
		}
	}
	if uid < 0 {
		return -1, false, errors.New("process Uid unavailable")
	}
	return uid, protected, nil
}
