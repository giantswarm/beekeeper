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
	"sync/atomic"
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
	base := strings.TrimSuffix(path, reqSuffix)
	out := base + replySuffix
	if _, err := os.Lstat(out); err == nil {
		return
	}
	r, gone, err := handle(ctx, path, procDir, h)
	if gone {
		// a streamed call whose requester ended: nobody reads its answer
		_ = os.Remove(path)
		_ = os.Remove(base + outSuffix)
		return
	}
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

// holderPoll is how often a streamed call's requester is looked for.
const holderPoll = time.Second

// handle acts on the request at path; gone says that a streamed call's
// requester ended before its call's answer.
func handle(ctx context.Context, path, procDir string, h Handler) (Reply, bool, error) {
	raw, st, err := read(path)
	if err != nil {
		return Reply{}, false, err
	}
	if len(raw) > maxRequest {
		return Reply{}, false, fmt.Errorf("request over %d bytes", maxRequest)
	}
	var req Request
	if err := json.Unmarshal(raw, &req); err != nil {
		return Reply{}, false, fmt.Errorf("malformed request: %w", err)
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return Reply{}, false, errors.New("request: no inode")
	}
	pids := Holders(procDir, sys.Dev, sys.Ino)
	if len(pids) != 1 {
		return Reply{}, false, fmt.Errorf("request held by %d processes of this user, want one", len(pids))
	}
	pid := pids[0]
	if !req.Stream {
		r, err := h(ctx, pid, req)
		return r, false, err
	}
	f, err := os.OpenFile(strings.TrimSuffix(path, reqSuffix)+outSuffix, os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, 0o600) //nolint:gosec // the request's output in our own spool; O_EXCL refuses a planted file
	if err != nil {
		return Reply{}, false, err
	}
	defer func() { _ = f.Close() }()
	req.out = f
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var gone atomic.Bool
	go func() {
		t := time.NewTicker(holderPoll)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			if !holds(filepath.Join(procDir, strconv.Itoa(pid)), sys.Dev, sys.Ino) {
				gone.Store(true)
				cancel()
				return
			}
		}
	}()
	r, err := h(ctx, pid, req)
	// a requester that ended while the call ran reads no answer either
	return r, gone.Load() || !holds(filepath.Join(procDir, strconv.Itoa(pid)), sys.Dev, sys.Ino), err
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
		if holds(dir, dev, ino) {
			out = append(out, pid)
		}
	}
	return out
}

// holds reports whether the process at dir (/proc/<pid>) holds the file
// dev:ino open.
func holds(dir string, dev, ino uint64) bool {
	fds, err := os.ReadDir(filepath.Join(dir, "fd"))
	if err != nil {
		return false
	}
	for _, fd := range fds {
		st, err := os.Stat(filepath.Join(dir, "fd", fd.Name()))
		if err != nil {
			continue
		}
		if s, ok := st.Sys().(*syscall.Stat_t); ok && s.Dev == dev && s.Ino == ino {
			return true
		}
	}
	return false
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

// Sandboxed reports whether process pid runs in another mount namespace
// than procDir's own process: the agent sandbox (bubblewrap) always mounts
// its own, which no process inside can leave or fake.
func Sandboxed(procDir string, pid int) (bool, error) {
	self, err := os.Readlink(filepath.Join(procDir, "self", "ns", "mnt"))
	if err != nil {
		return false, err
	}
	theirs, err := os.Readlink(filepath.Join(procDir, strconv.Itoa(pid), "ns", "mnt"))
	if err != nil {
		return false, err
	}
	return theirs != self, nil
}
