package state

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// FollowSnapshot opts a reader into a durable projection. revision must change
// whenever its predicate changes (including a time window). Other stores retain
// the ordinary Follow contract. Checkpoints are disposable; audit segments are
// the source of truth and remain available to Events/log --since.
func FollowSnapshot(s Store, name, revision string, keep func(Event) bool) ([]Event, error) {
	return FollowSnapshotContext(context.Background(), s, name, revision, keep)
}

// FollowSnapshotContext is FollowSnapshot with caller cancellation during cold
// or incremental history scans.
func FollowSnapshotContext(ctx context.Context, s Store, name, revision string, keep func(Event) bool) ([]Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if indexed, ok := s.(interface {
		FollowSnapshotContext(context.Context, string, string, func(Event) bool) ([]Event, error)
	}); ok {
		return indexed.FollowSnapshotContext(ctx, name, revision, keep)
	}
	if indexed, ok := s.(interface {
		FollowSnapshot(string, string, func(Event) bool) ([]Event, error)
	}); ok {
		return indexed.FollowSnapshot(name, revision, keep)
	}
	if cancellable, ok := s.(interface {
		EventsContext(context.Context, int, func(Event) bool) ([]Event, error)
	}); ok {
		return cancellable.EventsContext(ctx, 0, keep)
	}
	return s.Follow(name, keep)
}

// FollowLast returns the last n events keep accepts, oldest first, as
// Events(n, keep) does. A file store remembers under name how far it read and
// keeps only those n, so a reader that asks again every pass, for an event
// that may never have been logged, reads only what was appended since instead
// of the whole history again.
func FollowLast(s Store, name string, n int, keep func(Event) bool) ([]Event, error) {
	fs, ok := s.(*FileStore)
	if !ok || n <= 0 {
		return s.Events(n, keep)
	}
	at := fs.eventReader("last:" + name)
	at.mu.Lock()
	defer at.mu.Unlock()
	if _, err := fs.followContext(context.Background(), at, keep); err != nil {
		return nil, err
	}
	if len(at.kept) > n {
		at.kept = slices.Clone(at.kept[len(at.kept)-n:])
	}
	return slices.Clone(at.kept), nil
}

// A reader republishes its checkpoint after checkpointEvery, or sooner once
// checkpointBytes of its log were read since the last one.
const (
	checkpointEvery = time.Minute
	checkpointBytes = 1 << 20
)

type eventCheckpoint struct {
	Revision string  `json:"revision"`
	Source   string  `json:"source"`
	Offset   int64   `json:"offset"`
	Kept     []Event `json:"kept"`
}

// A hard-link anchor supplies a portable, restart-safe os.SameFile identity.
// It prevents a replaced/truncated log from being mistaken for the old cursor.
// No checkpoint can authorize a state write or alter recovery's writer fence.
func (s *FileStore) FollowSnapshot(name, revision string, keep func(Event) bool) ([]Event, error) {
	return s.FollowSnapshotContext(context.Background(), name, revision, keep)
}

func (s *FileStore) FollowSnapshotContext(ctx context.Context, name, revision string, keep func(Event) bool) ([]Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if filepath.Base(name) != name || name == "." || strings.ContainsAny(name, `/\`) {
		return s.FollowContext(ctx, name, keep)
	}
	key := name + ":" + revision
	at := s.eventReader(key)
	at.mu.Lock()
	defer at.mu.Unlock()
	dir := s.path("event-checkpoints")
	path := filepath.Join(dir, name+".json")
	previousFile, previousOffset := at.file, at.offset
	savedKept, savedAnchor := slices.Clone(at.kept), at.anchor
	var saved eventCheckpoint
	if at.file == nil {
		if raw, err := os.ReadFile(path); err == nil { //nolint:gosec // validated reader basename under the store checkpoint directory
			if json.Unmarshal(raw, &saved) != nil {
				saved = eventCheckpoint{}
			}
		}
	}
	if at.file == nil && saved.Revision == revision && saved.Offset >= 0 && filepath.Base(saved.Source) == saved.Source {
		if fi, err := os.Stat(filepath.Join(dir, saved.Source)); err == nil && fi.Size() >= saved.Offset {
			at.file, at.offset, at.kept, at.anchor = fi, saved.Offset, saved.Kept, saved.Source
		}
	}
	events, err := s.followContext(ctx, at, keep)
	if err != nil {
		if ctx.Err() != nil {
			at.file, at.offset, at.kept, at.anchor = previousFile, previousOffset, savedKept, savedAnchor
		}
		return events, err
	}
	if at.file == nil {
		return events, nil
	}
	if previousFile != nil && at.offset == previousOffset && os.SameFile(previousFile, at.file) {
		return events, nil
	}
	if saved.Revision == revision && saved.Offset == at.offset {
		if fi, err := os.Stat(filepath.Join(dir, saved.Source)); err == nil && os.SameFile(fi, at.file) {
			return events, nil
		}
	}
	// A checkpoint only shortens the next process's first read. Publishing
	// one on every appended event rewrites everything kept each time; this
	// process republishes once enough time or history has passed.
	if at.anchor != "" && time.Since(at.publishedAt) < checkpointEvery &&
		(!os.SameFile(at.publishedFile, at.file) || at.offset-at.publishedOffset < checkpointBytes) {
		return events, nil
	}
	// Snapshot publication is best effort. A full/unwritable cache must not
	// turn a successfully read log into a failed supervisor pass.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return events, nil
	}
	files, err := s.eventFilesContext(ctx)
	if err != nil {
		return events, nil
	}
	defer closeEventFiles(files)
	for _, f := range files {
		fi, err := f.Stat()
		if err != nil || !os.SameFile(fi, at.file) {
			continue
		}
		// A unique anchor and atomic JSON publication keep concurrent writers'
		// source/offset pairs consistent. A rotation racing Link only loses
		// this cache refresh, never audit data or reader progress.
		source := name + "-" + time.Now().UTC().Format("20060102T150405.000000000")
		anchor := filepath.Join(dir, source)
		if err := os.Link(f.Name(), anchor); err != nil {
			return events, nil
		}
		linked, err := os.Stat(anchor)
		if err != nil || !os.SameFile(linked, at.file) {
			_ = os.Remove(anchor)
			return events, nil
		}
		next := eventCheckpoint{Revision: revision, Source: source, Offset: at.offset, Kept: slices.Clone(at.kept)}
		if err := writeJSON(path, next); err != nil {
			_ = os.Remove(anchor)
			return events, nil
		}
		oldSource := at.anchor
		at.anchor = source
		at.publishedAt, at.publishedFile, at.publishedOffset = time.Now(), at.file, at.offset
		if oldSource == "" {
			oldSource = saved.Source
		}
		if oldSource != "" && oldSource != source && filepath.Base(oldSource) == oldSource {
			_ = os.Remove(filepath.Join(dir, oldSource))
		}
		break
	}
	return events, nil
}
