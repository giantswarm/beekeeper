package cmd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/state"
)

// writeState writes doc as the state document of a's store.
func writeState(t *testing.T, a *app, doc string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(a.store.(*state.FileStore).Dir(), "state.json"), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
}

// olderMemos is a state document an older beekeeper left with a board-skip
// memo and a pinned priority memo it never migrated, beside an open decision.
const olderMemos = `{"nextNote": 3, "notes": [
	{"id": 3, "for": "Pat", "kind": "decision", "text": "merge o/r#9?", "refs": ["o/r#9"], "default": "it waits", "by": {"name": "Agent one"}, "at": "2026-10-01T00:00:00Z"}],
	"memos": [
	{"id": 1, "for": "Supervisor", "kind": "memo", "text": "Board skip: o/r#10 until its plan merges", "by": {"name": "Supervisor run 3"}, "at": "2026-10-01T00:00:00Z"},
	{"id": 2, "kind": "memo", "pinned": true, "text": "priorities: o/r#12 first", "by": {"name": "Guide"}, "at": "2026-10-01T00:00:00Z"}]}`

// errBoardRead is what the fake GitHub answers board next's read with.
var errBoardRead = errors.New("the board read reached GitHub")

// No note or memo holds or ranks an item: board next over a state that still
// carries an older beekeeper's memos reads the board from GitHub as over any
// other, and there is no note migrate.
func TestBoardNextReadsGitHubWhateverMemosTheStateKeeps(t *testing.T) {
	a, _ := noteApp(t)
	a.cfg.Board.Owner, a.cfg.Board.Project, a.cfg.StateDir = defaultOrg, 1, t.TempDir()
	a.cfg.Board.Order = []config.BoardStep{{Name: stepNextUp, Status: []string{"Up Next"}}}
	writeState(t, a, olderMemos)
	was := boardGH
	boardGH = func(context.Context, ...string) ([]byte, error) { return nil, errBoardRead }
	t.Cleanup(func() { boardGH = was })

	c := a.boardCmd()
	c.SetArgs([]string{"next"})
	c.SetOut(a.out)
	c.SetErr(a.out)
	c.SilenceUsage, c.SilenceErrors = true, true
	err := c.Execute()
	if !errors.Is(err, errBoardRead) || strings.Contains(err.Error(), "memo") {
		t.Fatalf("board next over an older beekeeper's memos = exit %d, %v", Code(err), err)
	}

	if _, err := noteCommand(a, "migrate"); err == nil {
		t.Errorf("note migrate: %v", err)
	}
	if st, err := a.store.Read(); err != nil || len(st.Notes) != 1 {
		t.Errorf("the open decision is the state's one note: %+v, %v", st, err)
	}
}
