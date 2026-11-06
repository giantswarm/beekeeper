package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

// RecoverySnapshot validates the exact bytes persisted for independent fatal
// repair admission. Only the last loadable configuration replaces the snapshot.
// The caller must have established that a deliberately named source exists.
func RecoverySnapshot(source, destination string) (*Config, error) {
	raw, err := os.ReadFile(filepath.Clean(source))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	c, err := parse(source, raw)
	if err != nil {
		return nil, err
	}
	if c.StateDir != filepath.Dir(destination) {
		return nil, fmt.Errorf("recovery state directory differs from configuration")
	}
	body := append([]byte(recoveryHeader(source)), raw...)
	if err := writeAtomic(destination, body, 0600); err != nil {
		return nil, err
	}
	return c, nil
}

func recoveryHeader(source string) string {
	return "# fatal recovery source: " + strconv.Quote(filepath.Clean(source)) + "\n"
}

// LoadRecovery validates both the source binding and the persisted settings.
// It never falls back to defaults when the recovery file is absent.
func LoadRecovery(source, snapshot string) (*Config, error) {
	raw, err := os.ReadFile(filepath.Clean(snapshot))
	if err != nil {
		return nil, err
	}
	if !bytes.HasPrefix(raw, []byte(recoveryHeader(source))) {
		return nil, fmt.Errorf("recovery configuration belongs to another source")
	}
	c, err := parse(snapshot, raw)
	if err != nil {
		return nil, err
	}
	if c.StateDir != filepath.Dir(snapshot) {
		return nil, fmt.Errorf("recovery state directory differs from configuration")
	}
	return c, nil
}
