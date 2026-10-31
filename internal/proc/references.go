package proc

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
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
		return referenceGone(err) || protected && errors.Is(err, os.ErrPermission)
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
// EACCES on helpers protected by dumpability, credentials or capabilities
// is expected; other errors on live user processes remain failures.
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
		if referenceGone(err) {
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
		// A cached directory inode may retain its original owner after a
		// process becomes non-dumpable. Ptrace-gated children reflect the
		// current protection, including when status omits Dumpable.
		gone := false
		for _, path := range []string{dir, filepath.Join(dir, "environ")} {
			info, err := files.stat(path)
			if referenceGone(err) {
				gone = true
				break
			}
			if err != nil {
				return nil, nil, err
			}
			protected = protected || ownerUID(info) != uid
		}
		if gone {
			continue
		}
		paths, err := referencesWith(root, pid, protected, files)
		if err != nil {
			return nil, nil, err
		}
		comm, err := files.readFile(filepath.Join(dir, "comm"))
		if referenceGone(err) {
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

// Linux may return ESRCH while a process exits, including when reading a
// zombie's environ. Like ENOENT, it cannot hide a live process's references.
func referenceGone(err error) bool {
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH)
}
