package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Serve answers the requests in dir until ctx ends, polling every tick.
// Each is answered as the one process of this user that holds it open; a
// request no process or more than one holds is refused. Requests are
// answered side by side, so a long secret call holds up no capped run.
func Serve(ctx context.Context, dir, procDir string, tick time.Duration, h Handler) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	var wg sync.WaitGroup
	defer wg.Wait()
	var mu sync.Mutex
	busy := map[string]bool{}
	for {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		for _, e := range entries {
			path := filepath.Join(dir, e.Name())
			mu.Lock()
			skip := busy[path]
			mu.Unlock()
			if skip || !strings.HasSuffix(e.Name(), reqSuffix) || !e.Type().IsRegular() {
				continue
			}
			mu.Lock()
			busy[path] = true
			mu.Unlock()
			wg.Go(func() {
				answer(ctx, path, procDir, h)
				mu.Lock()
				delete(busy, path)
				mu.Unlock()
			})
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// answer answers the request at path once: a request with an answer is
// left to its requester, and an answer whose requester gave up is removed.
func answer(ctx context.Context, path, procDir string, h Handler) {
	out := strings.TrimSuffix(path, reqSuffix) + replySuffix
	if _, err := os.Lstat(out); err == nil {
		return
	}
	r, err := handle(ctx, path, procDir, h)
	if err != nil {
		r = Reply{Error: err.Error()}
	}
	b, _ := json.Marshal(r)
	tmp := out + tmpSuffix
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, out)
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		_ = os.Remove(out)
	}
}

func handle(ctx context.Context, path, procDir string, h Handler) (Reply, error) {
	raw, st, err := read(path)
	if err != nil {
		return Reply{}, err
	}
	if len(raw) > maxRequest {
		return Reply{}, fmt.Errorf("request over %d bytes", maxRequest)
	}
	var req Request
	if err := json.Unmarshal(raw, &req); err != nil {
		return Reply{}, fmt.Errorf("malformed request: %w", err)
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return Reply{}, errors.New("request: no inode")
	}
	pids := Holders(procDir, sys.Dev, sys.Ino)
	if len(pids) != 1 {
		return Reply{}, fmt.Errorf("request held by %d processes of this user, want one", len(pids))
	}
	return h(ctx, pids[0], req)
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

// Origin is where process pid runs: its working directory and, of its
// environment, the variables named in keys only.
func Origin(procDir string, pid int, keys []string) (string, []string, error) {
	dir := filepath.Join(procDir, strconv.Itoa(pid))
	cwd, err := os.Readlink(filepath.Join(dir, "cwd"))
	if err != nil {
		return "", nil, err
	}
	raw, err := os.ReadFile(filepath.Join(dir, "environ")) //nolint:gosec // the holder's environment, of which only keys leave this function
	if err != nil {
		return "", nil, err
	}
	var env []string
	for kv := range strings.SplitSeq(string(raw), "\x00") {
		if k, _, ok := strings.Cut(kv, "="); ok && slices.Contains(keys, k) {
			env = append(env, kv)
		}
	}
	return cwd, env, nil
}
