package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// RecoveryPath is the private last-known-good admission configuration.
func RecoveryPath(dir string) string { return filepath.Join(dir, "fatal-admission.yaml") }

// SaveRecovery persists only a successfully loaded configuration. Installation
// saves it before starting recovery units; healthy sentinel scans refresh it.
func SaveRecovery(dir, source string) error {
	source, err := filepath.Abs(source)
	if err != nil {
		return err
	}
	b, err := os.ReadFile(filepath.Clean(source))
	if err != nil {
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
	return writeAtomic(RecoveryPath(dir), append([]byte(recoveryHeader(source)), b...), 0600)
}

func recoveryHeader(source string) string {
	return "# fatal recovery source: " + strconv.Quote(filepath.Clean(source)) + "\n"
}

// RecoverySource binds admission to the original file, including when a repair
// worker inherits the snapshot as its configuration. Never verify the fallback
// itself as proof that the broken original recovered.
func RecoverySource(dir string) (string, error) {
	if _, err := LoadRecovery(dir); err != nil {
		return "", err
	}
	b, err := os.ReadFile(filepath.Clean(RecoveryPath(dir)))
	if err != nil {
		return "", err
	}
	header, _, _ := strings.Cut(string(b), "\n")
	quoted, ok := strings.CutPrefix(header, "# fatal recovery source: ")
	if !ok {
		return "", fmt.Errorf("missing recovery source")
	}
	source, err := strconv.Unquote(quoted)
	if err != nil || !filepath.IsAbs(source) || filepath.Clean(source) == RecoveryPath(dir) || !bytes.HasPrefix(b, []byte(recoveryHeader(source))) {
		return "", fmt.Errorf("invalid recovery source")
	}
	return filepath.Clean(source), nil
}

func LoadRecoveryForSource(dir, source string) (*Config, error) {
	want, err := filepath.Abs(source)
	if err != nil {
		return nil, err
	}
	got, err := RecoverySource(dir)
	if err != nil {
		return nil, err
	}
	if got != want {
		return nil, fmt.Errorf("recovery configuration belongs to another source")
	}
	return LoadRecovery(dir)
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
