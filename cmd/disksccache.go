package cmd

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/diskprune"
	"github.com/giantswarm/beekeeper/internal/gocache"
	"github.com/giantswarm/beekeeper/internal/proc"
)

// sccacheStopWait bounds sccache --stop-server and the server's exit after it.
const sccacheStopWait = 30 * time.Second

// pruneSccache trims sccache's local disk cache as the Go cache is trimmed:
// over its cap least recently used entries first down to three quarters of
// it, under disk pressure all of it. The caller holds the build-cache lock
// and saw no build running. An idle sccache server is stopped first, so its
// size accounting starts again from the trimmed cache; one that does not
// stop keeps the cache, and a server that starts during the trim ends it.
func pruneSccache(r *diskRun, m diskMachine, capBytes int64, pressure bool) {
	if m.sccache == "" {
		return
	}
	if diskprune.InUse(m.sccache, m.refs) {
		r.Keep("sccache in use")
		return
	}
	u, err := scanSccache(m.sccache)
	if err != nil {
		r.Errors = append(r.Errors, fmt.Errorf("sccache: %w", err))
		return
	}
	target, trim := diskCacheTarget(u.Bytes, capBytes, pressure)
	if !trim || len(u.Entries) == 0 {
		r.Keep("sccache under cap")
		return
	}
	if r.dry {
		dryTrim(r, u, target)
		return
	}
	servers := func() []int { return nil }
	if m.sccacheServers != nil {
		servers = m.sccacheServers
	}
	if pids := servers(); len(pids) > 0 {
		if m.stopSccache == nil {
			r.Keep("sccache: server running")
			return
		}
		if err := m.stopSccache(pids); err != nil {
			r.Keep("sccache: server did not stop")
			return
		}
	}
	freed, removed, err := gocache.Trim(u, target, func() bool { return len(servers()) > 0 })
	r.Freed += freed
	r.Removed += removed
	switch {
	case errors.Is(err, gocache.ErrBusy):
		r.Keep("sccache: a build started")
	case err != nil:
		r.Errors = append(r.Errors, fmt.Errorf("sccache: %w", err))
	}
	removeEmptyDirs(m.sccache)
}

// dryTrim counts what trimming u down to target would remove.
func dryTrim(r *diskRun, u gocache.Usage, target int64) {
	left := u.Bytes
	for _, e := range u.Entries {
		if left <= target {
			break
		}
		r.Freed += e.Size
		r.Removed++
		left -= e.Size
	}
}

// scanSccache reads sccache's local cache entries, least recently used
// first: sccache moves an entry's modification time on each hit. Symbolic
// links are neither followed nor counted, and a missing cache is empty.
func scanSccache(dir string) (gocache.Usage, error) {
	u := gocache.Usage{Dir: dir}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == dir && errors.Is(err, fs.ErrNotExist) {
				return fs.SkipAll
			}
			if errors.Is(err, fs.ErrNotExist) {
				return nil // removed since its directory was read
			}
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil //nolint:nilerr // removed since its directory was read
		}
		u.Entries = append(u.Entries, gocache.Entry{Path: p, Size: info.Size(), Used: info.ModTime()})
		u.Bytes += info.Size()
		return nil
	})
	slices.SortFunc(u.Entries, func(a, b gocache.Entry) int { return a.Used.Compare(b.Used) })
	return u, err
}

// removeEmptyDirs removes the directories under root a trim left empty,
// deepest first; root itself stays. sccache creates them again on a write.
func removeEmptyDirs(root string) {
	var dirs []string
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() && p != root {
			dirs = append(dirs, p)
		}
		return nil
	})
	slices.SortFunc(dirs, func(a, b string) int {
		return strings.Count(b, string(filepath.Separator)) - strings.Count(a, string(filepath.Separator))
	})
	for _, d := range dirs {
		_ = os.Remove(d) // fails on a directory still holding entries
	}
}

// sccacheServers are the user's processes named sccache: the server, and
// clients a running build started.
func sccacheServers() []int { return proc.Named("sccache", os.Getuid()) }

// sccacheServerAddress are the variables that select the server a client
// talks to.
var sccacheServerAddress = []string{"SCCACHE_SERVER_PORT", "SCCACHE_SERVER_UDS"}

// sccacheStopEnv is the caller's environment base with the server address
// of server's environment alone: a caller's own address never reaches
// another server, and a server without one is reached at sccache's default.
func sccacheStopEnv(base []string, server map[string]string) []string {
	env := slices.DeleteFunc(slices.Clone(base), func(kv string) bool {
		k, _, _ := strings.Cut(kv, "=")
		return slices.Contains(sccacheServerAddress, k)
	})
	for _, k := range sccacheServerAddress {
		if v, ok := server[k]; ok {
			env = append(env, k+"="+v)
		}
	}
	return env
}

// stopSccacheServers asks each sccache server to stop as sccache
// --stop-server does, through its own executable and the server address its
// environment names (none, sccache's default), and waits for every process to
// exit. A server whose environment cannot be read is not asked: the caller's
// address could reach another server.
func stopSccacheServers(pids []int) error {
	ctx, cancel := context.WithTimeout(context.Background(), sccacheStopWait)
	defer cancel()
	var errs []error
	for _, pid := range pids {
		exe, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "exe"))
		if err != nil || strings.HasSuffix(exe, " (deleted)") {
			if exe, err = proc.LookPath("sccache"); err != nil {
				errs = append(errs, err)
				continue
			}
		}
		env, err := proc.Environ(pid)
		if err != nil {
			errs = append(errs, fmt.Errorf("sccache server address unknown: %w", err))
			continue
		}
		c := proc.Command(ctx, exe, "--stop-server")
		c.Env = sccacheStopEnv(os.Environ(), env)
		if err := c.Run(); err != nil {
			errs = append(errs, fmt.Errorf("sccache --stop-server: %w", err))
		}
	}
	for slices.ContainsFunc(pids, proc.Alive) {
		select {
		case <-ctx.Done():
			return errors.Join(append(errs, errors.New("sccache server still running"))...)
		case <-time.After(100 * time.Millisecond):
		}
	}
	return nil
}
