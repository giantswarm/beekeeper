package state

import (
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
	if indexed, ok := s.(interface {
		FollowSnapshot(string, string, func(Event) bool) ([]Event, error)
	}); ok {
		return indexed.FollowSnapshot(name, revision, keep)
	}
	return s.Follow(name, keep)
}

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
	if filepath.Base(name) != name || name == "." || strings.ContainsAny(name, `/\`) {
		return s.Follow(name, keep)
	}
	key := name + ":" + revision
	at := s.eventReader(key)
	at.mu.Lock()
	defer at.mu.Unlock()
	dir := s.path("event-checkpoints")
	path := filepath.Join(dir, name+".json")
	previousFile, previousOffset := at.file, at.offset
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
	events, err := s.follow(at, keep)
	if err != nil {
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
	// Snapshot publication is best effort. A full/unwritable cache must not
	// turn a successfully read log into a failed supervisor pass.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return events, nil
	}
	files, err := s.eventFiles()
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
