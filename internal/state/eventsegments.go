package state

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gofrs/flock"
)

const eventSegmentBytes = 8 << 20

// Rotation only changes the log layout, never its contents or retention. The
// state lock serializes appends/outbox recovery; this separate lock lets readers
// open a consistent set of inodes without waiting for a state transaction.
func (s *FileStore) rotateEvents() error {
	var policy EventSegmentation
	found, err := s.ReadFile("event-segmentation.json", &policy)
	if err != nil || !found || !policy.Enabled {
		return err
	}
	return s.rotateEventsAbove(eventSegmentBytes)
}

// EventSegmentation is enabled only after the signed recovery target has
// demonstrated support for segmented history. Legacy rollback loops cannot
// read archives, so the first protected hop must keep its original log layout.
type EventSegmentation struct {
	Enabled bool            `json:"enabled"`
	Readers map[string]bool `json:"readers"`
}

func (s *FileStore) rotateEventsAbove(limit int64) error {
	info, err := os.Stat(s.path("events.jsonl"))
	if errors.Is(err, os.ErrNotExist) || err == nil && info.Size() < limit {
		return nil
	}
	if err != nil {
		return err
	}
	lock := flock.New(s.path("events-layout.lock"))
	if err := lock.Lock(); err != nil {
		return err
	}
	defer func() { _ = lock.Unlock() }()
	dir := s.path("event-segments")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// Complete a torn last line before sealing the segment, so a later
	// append cannot accidentally complete it in a different file.
	f, err := s.openEventLog()
	if err != nil {
		return err
	}
	if err := endLine(f); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	serial := time.Now().UTC().UnixNano()
	paths, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if err != nil {
		return err
	}
	slices.Sort(paths)
	if len(paths) > 0 {
		last, err := strconv.ParseInt(strings.TrimSuffix(filepath.Base(paths[len(paths)-1]), ".jsonl"), 10, 64)
		if err != nil {
			return err
		}
		serial = max(serial, last+1)
	}
	name := filepath.Join(dir, fmt.Sprintf("%020d.jsonl", serial))
	if err := os.Rename(s.path("events.jsonl"), name); err != nil {
		return err
	}
	if err := syncDirectory(dir); err != nil {
		return err
	}
	return syncDirectory(s.dir)
}

// eventFiles pins the inodes while holding the layout lock. Decoding happens
// after releasing it, so slow history readers never block append/rotation.
func (s *FileStore) eventFiles() ([]*os.File, error) {
	return s.eventFilesContext(context.Background())
}

func (s *FileStore) eventFilesContext(ctx context.Context) ([]*os.File, error) {
	lock := flock.New(s.path("events-layout.lock"))
	ctx, cancel := context.WithTimeout(ctx, lockWait)
	defer cancel()
	held, err := lock.TryRLockContext(ctx, 10*time.Millisecond)
	if err != nil {
		return nil, err
	}
	if !held {
		return nil, fmt.Errorf("event layout lock unavailable")
	}
	defer func() { _ = lock.Unlock() }()
	paths, err := filepath.Glob(filepath.Join(s.path("event-segments"), "*.jsonl"))
	if err != nil {
		return nil, err
	}
	slices.Sort(paths)
	paths = append(paths, s.path("events.jsonl"))
	var files []*os.File
	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			closeEventFiles(files)
			return nil, err
		}
		f, err := os.Open(path) //nolint:gosec // paths come only from this store's fixed archive glob and active log
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			closeEventFiles(files)
			return nil, err
		}
		files = append(files, f)
	}
	return files, nil
}

func closeEventFiles(files []*os.File) {
	for _, f := range files {
		_ = f.Close()
	}
}
