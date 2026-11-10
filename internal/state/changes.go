package state

import (
	"context"
	"os"
	"path/filepath"

	"github.com/fsnotify/fsnotify"
)

// Changes subscribes before a caller reads its snapshot, avoiding a lost wake
// between the read and sleep. Paths name atomic files or lease directories.
// Events coalesce; callers must still keep a deadline for time-based work and
// external sources. A missing directory or exhausted OS watch limit falls back
// to that deadline. No state or event contents are read here.
func Changes(ctx context.Context, paths ...string) (<-chan struct{}, func()) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, func() {}
	}
	changes := make(chan struct{}, 1)
	dirs := map[string]bool{}
	targets := map[string]bool{}
	for _, path := range paths {
		if path == "" {
			continue
		}
		path = filepath.Clean(path)
		targets[path] = true
		dirs[filepath.Dir(path)] = true
		if info, err := os.Stat(path); err == nil && info.IsDir() {
			dirs[path] = true
		}
	}
	added := 0
	for dir := range dirs {
		if err := watcher.Add(dir); err == nil {
			added++
		}
	}
	if added == 0 {
		_ = watcher.Close()
		return nil, func() {}
	}
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() { _ = watcher.Close() }()
		notify := func() {
			select {
			case changes <- struct{}{}:
			default:
			}
		}
		for {
			select {
			case <-ctx.Done():
				return
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				if event.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Remove|fsnotify.Rename) != 0 &&
					(targets[event.Name] || targets[filepath.Dir(event.Name)]) {
					notify()
				}
			case _, ok := <-watcher.Errors:
				if !ok {
					return
				}
				// Overflow means a change may have been missed. Reread once;
				// the deadline remains the fallback if watching stops working.
				notify()
			}
		}
	}()
	return changes, func() { cancel(); <-done }
}
