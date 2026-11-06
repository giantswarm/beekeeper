package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// RecoveryPath is the private last-known-good admission configuration.
func RecoveryPath(dir string) string { return filepath.Join(dir, "fatal-admission.yaml") }

// SaveRecovery persists only a successfully loaded configuration. Installation
// saves it before starting recovery units; healthy sentinel scans refresh it.
func SaveRecovery(dir, source string) error {
	b, err := os.ReadFile(filepath.Clean(source))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	cfg, err := parse(source, b)
	if err != nil {
		return err
	}
	if cfg.StateDir != dir {
		return fmt.Errorf("recovery state directory differs from configuration")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	return writeAtomic(RecoveryPath(dir), b, 0600)
}

// LoadRecovery never accepts defaults for a missing snapshot or switches the
// pinned store. Stop, holds, capacity and repair ownership remain live state.
func LoadRecovery(dir string) (*Config, error) {
	info, err := os.Lstat(RecoveryPath(dir))
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return nil, fmt.Errorf("recovery admission must be a private regular file")
	}
	cfg, err := Load(RecoveryPath(dir))
	if err != nil {
		return nil, err
	}
	if cfg.StateDir != dir {
		return nil, fmt.Errorf("recovery admission changed the pinned state directory")
	}
	return cfg, nil
}
