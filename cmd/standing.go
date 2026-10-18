package cmd

import (
	"cmp"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// standingFile, in the state directory, holds the desk's standing
// instructions. Both hand-overs print it in full; the person edits it, and an
// instruction ends when its block is deleted.
const standingFile = "standing.md"

func (a *app) standingPath() string { return filepath.Join(a.cfg.StateDir, standingFile) }

// readStanding reads the desk's standing instructions, "" for none.
func (a *app) readStanding() (string, error) {
	raw, err := os.ReadFile(a.standingPath())
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("the standing instructions: %w", err)
	}
	return strings.TrimSpace(string(raw)), nil
}

// promptStanding prints the standing instructions in a successor's prompt
// in full; nothing without any.
func promptStanding(p printer, s string) {
	if s == "" {
		return
	}
	section(p, "Standing instructions", false, "")
	p("%s\n", s)
}

// printStanding prints the standing instructions in a hand-over.
func (a *app) printStanding(s string) {
	_, _ = fmt.Fprintln(a.out, cmp.Or(s, "none"))
}
