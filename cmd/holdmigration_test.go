package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// An upgrade from state an older beekeeper recorded, whose memos and pinned
// notes held the items they name: board next offers nothing until note
// migrate --apply has closed them, and its refusal names the migration.
func TestBoardNextRefusesUntilNoteMigrateRan(t *testing.T) {
	a, _ := noteApp(t)
	a.cfg.Board.Owner, a.cfg.StateDir = defaultOrg, t.TempDir()
	writeState(t, a, olderNotes)

	c := a.boardCmd()
	c.SetArgs([]string{"next"})
	c.SetOut(a.out)
	c.SetErr(a.out)
	c.SilenceUsage, c.SilenceErrors = true, true
	err := c.Execute()
	if Code(err) != ExitRefused {
		t.Fatalf("board next over unmigrated memos = exit %d, %v", Code(err), err)
	}
	for _, want := range []string{"board next offers no item", "4 memos and pinned notes", "note migrate --dry-run", "note migrate --apply"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal lacks %q: %v", want, err)
		}
	}

	if _, err := noteCommand(a, "migrate", "--apply"); err != nil {
		t.Fatal(err)
	}
	if err := a.unmigrated("board next"); err != nil {
		t.Errorf("board next still refused once note migrate ran: %v", err)
	}
}

// The new binary's step of a self-update says the migration the state
// still needs, and nothing once there is none.
func TestSelfUpdateSaysThePendingMigration(t *testing.T) {
	dir := asBinary(t, newBinary)
	runsInstalled = func() bool { return false }
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(olderNotes), 0o600); err != nil {
		t.Fatal(err)
	}
	out := beekeeper(t, selfUpdateCmdName, "--"+syncPluginsFlag)
	if !strings.Contains(out, "Migration needed, board next refuses until it ran: 4 memos and pinned notes") || !strings.Contains(out, "note migrate --apply") {
		t.Errorf("the update did not name the migration:\n%s", out)
	}

	beekeeper(t, "--as", "Supervisor", "note", "migrate", "--apply")
	if out := beekeeper(t, selfUpdateCmdName, "--"+syncPluginsFlag); strings.Contains(out, "Migration needed") {
		t.Errorf("the update names a migration that ran:\n%s", out)
	}
}
