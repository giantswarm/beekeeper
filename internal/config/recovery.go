package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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

// RecoverySource identifies and validates the original source when a repair
// worker inherits the snapshot as BEEKEEPER_CONFIG. A scan must observe that
// source, never count loading the fallback itself as verified recovery.
func RecoverySource(snapshot string) (string, error) {
	raw, err := os.ReadFile(filepath.Clean(snapshot))
	if err != nil {
		return "", err
	}
	header, _, _ := strings.Cut(string(raw), "\n")
	quoted, ok := strings.CutPrefix(header, "# fatal recovery source: ")
	if !ok {
		return "", fmt.Errorf("missing recovery source")
	}
	source, err := strconv.Unquote(quoted)
	if err != nil || !filepath.IsAbs(source) || filepath.Clean(source) == filepath.Clean(snapshot) {
		return "", fmt.Errorf("invalid recovery source")
	}
	if _, err := LoadRecovery(source, snapshot); err != nil {
		return "", err
	}
	return source, nil
}
