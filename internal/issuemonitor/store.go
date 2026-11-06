package issuemonitor

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/gofrs/flock"
)

// WithState serializes poll, queue consumption and contributor/report reads.
func WithState(ctx context.Context, dir string, fn func(*State, func(*State) error) error) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	lock := flock.New(filepath.Join(dir, "lock"))
	if err := lock.Lock(); err != nil {
		return err
	}
	defer func() { _ = lock.Unlock() }()
	if err := ctx.Err(); err != nil {
		return err
	}
	path := filepath.Join(dir, "state.json")
	var s State
	raw, err := os.ReadFile(filepath.Clean(path))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err == nil {
		if err := json.Unmarshal(raw, &s); err != nil {
			return err
		}
	}
	s.Init()
	save := func(s *State) error {
		raw, err := json.MarshalIndent(s, "", "  ")
		if err != nil {
			return err
		}
		f, err := os.CreateTemp(dir, ".state-*")
		if err != nil {
			return err
		}
		defer func() { _ = os.Remove(f.Name()) }()
		if _, err := f.Write(raw); err != nil {
			_ = f.Close()
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
		return os.Rename(f.Name(), path)
	}
	if err := fn(&s, save); err != nil {
		return err
	}
	return save(&s)
}
