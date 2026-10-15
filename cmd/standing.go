package cmd

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/giantswarm/beekeeper/internal/state"
)

// standingFile, in the state directory, holds the desk's standing
// instructions: the pinned notes of an older beekeeper, moved there by
// `note migrate --apply`. Both hand-overs print it in full; the person
// edits it, and an instruction ends when its block is deleted.
const standingFile = "standing.md"

func (a *app) standingPath() string { return filepath.Join(a.cfg.StateDir, standingFile) }

// standing is the desk's standing instructions: the file note migrate moved
// pins into, and the pins it has not moved yet.
type standing struct {
	File string
	Pins []state.Note
}

func (s standing) empty() bool { return s.File == "" && len(s.Pins) == 0 }

// readStanding reads the standing instructions of st's desk.
func (a *app) readStanding(st *state.State) (standing, error) {
	var s standing
	for _, n := range st.Memos {
		if n.Pinned() {
			s.Pins = append(s.Pins, n)
		}
	}
	raw, err := os.ReadFile(a.standingPath())
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return s, fmt.Errorf("the standing instructions: %w", err)
	}
	s.File = strings.TrimSpace(string(raw))
	return s, nil
}

// unmovedPins introduces the pins note migrate has not moved yet.
const unmovedPins = "Pinned notes of an older beekeeper, until `beekeeper note migrate --apply` moves them into " + standingFile + ":"

// promptStanding prints the standing instructions in a successor's prompt,
// every one in full; nothing without any.
func (a *app) promptStanding(p printer, s standing) {
	if s.empty() {
		return
	}
	section(p, "Standing instructions", false, "")
	if s.File != "" {
		p("%s\n", s.File)
	}
	if len(s.Pins) > 0 {
		p("%s\n", unmovedPins)
		a.promptNoteLines(p, s.Pins)
		p("")
	}
}

// printStanding prints the standing instructions in a hand-over.
func (a *app) printStanding(s standing) {
	if s.empty() {
		_, _ = fmt.Fprintln(a.out, "none")
		return
	}
	if s.File != "" {
		_, _ = fmt.Fprintln(a.out, s.File)
	}
	if len(s.Pins) > 0 {
		if s.File != "" {
			_, _ = fmt.Fprintln(a.out)
		}
		_, _ = fmt.Fprintln(a.out, unmovedPins)
		a.printNotes(s.Pins)
	}
}

// standingBlock is pin n as standing.md carries it: a heading naming it,
// then its whole text.
func standingBlock(n *state.Note) string {
	var tags []string
	if n.For != "" {
		tags = append(tags, "for "+n.For)
	}
	if n.By.Name != "" {
		tags = append(tags, "by "+quotedOwner(n.By))
	}
	if !n.At.IsZero() {
		tags = append(tags, "pinned "+n.At.UTC().Format("2006-01-02"))
	}
	head := fmt.Sprintf("#### #%d", n.ID)
	if len(tags) > 0 {
		head += " (" + strings.Join(tags, ", ") + ")"
	}
	return head + "\n\n" + strings.TrimSpace(n.Text) + "\n"
}

// movePins appends every pin's block standing.md lacks to it and reads the
// file back: it returns the pins whose block the file does not carry, with
// why, so their notes stay open.
func (a *app) movePins(pins []state.Note) ([]int, error) {
	if len(pins) == 0 {
		return nil, nil
	}
	path := a.standingPath()
	raw, err := os.ReadFile(path) //nolint:gosec // standing.md in beekeeper's state folder
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return pinIDs(pins), err
	}
	text := string(raw)
	for i := range pins {
		if b := standingBlock(&pins[i]); !strings.Contains(text, b) {
			if text != "" && !strings.HasSuffix(text, "\n\n") {
				text = strings.TrimRight(text, "\n") + "\n\n"
			}
			text += b
		}
	}
	if text != string(raw) {
		if err := writeStanding(path, []byte(text)); err != nil {
			return pinIDs(pins), err
		}
	}
	back, err := os.ReadFile(path) //nolint:gosec // standing.md in beekeeper's state folder
	if err != nil {
		return pinIDs(pins), err
	}
	var lost []int
	for i := range pins {
		if !strings.Contains(string(back), standingBlock(&pins[i])) {
			lost = append(lost, pins[i].ID)
		}
	}
	if len(lost) > 0 {
		return lost, fmt.Errorf("%s does not carry them after the write", path)
	}
	return nil, nil
}

func pinIDs(pins []state.Note) []int {
	ids := make([]int, len(pins))
	for i, n := range pins {
		ids[i] = n.ID
	}
	return ids
}

// writeStanding replaces path with b through a temporary file beside it.
func writeStanding(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil { //nolint:gosec // beekeeper's state folder
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".standing-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }() //nolint:gosec // the temporary file beside standing.md
	if _, err := f.Write(b); err != nil {
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
	return os.Rename(f.Name(), path) //nolint:gosec // standing.md in beekeeper's state folder
}
