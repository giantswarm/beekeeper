package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Serve answers the requests in dir until ctx ends, polling every tick.
// Each is answered as the one process of this user that holds it open; a
// request no process or more than one holds is refused.
func Serve(ctx context.Context, dir, procDir string, tick time.Duration, h Handler) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), reqSuffix) && e.Type().IsRegular() {
				answer(filepath.Join(dir, e.Name()), procDir, h)
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// answer answers the request at path once: a request with an answer is
// left to its requester.
func answer(path, procDir string, h Handler) {
	out := strings.TrimSuffix(path, reqSuffix) + replySuffix
	if _, err := os.Lstat(out); err == nil {
		return
	}
	var r reply
	if err := handle(path, procDir, h); err != nil {
		r.Error = err.Error()
	}
	b, _ := json.Marshal(r)
	tmp := out + tmpSuffix
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, out)
}

func handle(path, procDir string, h Handler) error {
	raw, st, err := read(path)
	if err != nil {
		return err
	}
	if len(raw) > maxRequest {
		return fmt.Errorf("request over %d bytes", maxRequest)
	}
	var req Request
	if err := json.Unmarshal(raw, &req); err != nil {
		return fmt.Errorf("malformed request: %w", err)
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("request: no inode")
	}
	pids := Holders(procDir, sys.Dev, sys.Ino)
	if len(pids) != 1 {
		return fmt.Errorf("request held by %d processes of this user, want one", len(pids))
	}
	return h(pids[0], req)
}

// read reads the request at path, closed again before its holders are
// looked for.
func read(path string) ([]byte, os.FileInfo, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0) //nolint:gosec // a request in our own spool
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxRequest+1))
	return raw, st, err
}

// Holders are the processes of this user under procDir that hold the file
// dev:ino open.
func Holders(procDir string, dev, ino uint64) []int {
	entries, err := os.ReadDir(procDir)
	if err != nil {
		return nil
	}
	uid := uint32(os.Getuid()) //nolint:gosec // a uid fits
	var out []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		dir := filepath.Join(procDir, e.Name())
		if st, err := os.Stat(dir); err != nil {
			continue
		} else if s, ok := st.Sys().(*syscall.Stat_t); !ok || s.Uid != uid {
			continue
		}
		fds, err := os.ReadDir(filepath.Join(dir, "fd"))
		if err != nil {
			continue
		}
		for _, fd := range fds {
			st, err := os.Stat(filepath.Join(dir, "fd", fd.Name()))
			if err != nil {
				continue
			}
			if s, ok := st.Sys().(*syscall.Stat_t); ok && s.Dev == dev && s.Ino == ino {
				out = append(out, pid)
				break
			}
		}
	}
	return out
}
