package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/state"
)

// The doctor can archive a finished worker with no running sessions, even
// when that worker previously declined a peer request. The executable below
// stands in for the installed desktop API adapter, including its record update.
func TestDoctorArchivesWithoutSteward(t *testing.T) {
	worker := state.Party{Session: finishedID, HostSession: finishedHost, Name: finishedName}
	a, _ := archiveApp(t, func(st *state.State) {
		st.Starts = []state.Start{{Party: worker}}
		st.Agents = []state.Agent{{Party: worker}}
		st.Declines = []state.Decline{{Host: finishedHost, At: time.Now(), Why: "declined"}}
	})
	plat.Machine = noCLIMachine{plat.Machine}
	desktopRecord(t, a.cfg.Claude.DesktopDir, finishedHost, finishedID, false)
	recordPath := filepath.Join(a.cfg.Claude.DesktopDir, "a", "b", finishedHost+".json")
	script := filepath.Join(t.TempDir(), "archive")
	// Arguments include whitespace; no shell reparsing may occur in production.
	writeT(t, script, `#!/bin/sh
[ "$1" = "fixed argument" ] && [ "$3" = "local_fin" ] || exit 9
printf '{"sessionId":"local_fin","cliSessionId":"fin","isArchived":true}' > "$2"
`)
	if err := os.Chmod(script, 0o700); err != nil {
		t.Fatal(err)
	}
	a.cfg.Agents.ArchiveAgreement = "standing agreement"
	a.cfg.Agents.ArchiveCommand = []string{script, "fixed argument", recordPath}
	record := func(host string) (*claude.Record, bool) { return claude.ReadRecord(a.cfg, host) }
	lines, _, err := a.finishRoster(t.Context(), []chore{{kind: choreRemove, agent: state.Agent{Party: worker}, archive: true, why: whyReportedDone}}, nil, record, func(state.Party) bool { return false }, state.Party{Name: "doctor"}, "beekeeper doctor")
	if err != nil {
		t.Fatal(err)
	}
	r, ok := record(finishedHost)
	if !ok || !r.IsArchived {
		t.Fatal("desktop row was not archived")
	}
	st, err := a.store.Read()
	if err != nil || len(st.Agents) != 0 || len(st.Archives) != 0 {
		t.Fatalf("state %+v, error %v", st, err)
	}
	if !strings.Contains(strings.Join(lines, "\n"), "desktop archive command") {
		t.Fatalf("lines %v", lines)
	}
}

func TestArchiveCommandFailureAndConfirmation(t *testing.T) {
	for _, tc := range []struct {
		name, script, want string
		confirmed          bool
	}{
		{"failed", "exit 7", "command failed", true},
		{"not recorded", "exit 0", "did not record", false},
		{"command timeout", "exec sleep 60", "command failed", false},
		{"recorded", "exit 0", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "archive")
			writeT(t, path, "#!/bin/sh\n"+tc.script+"\n")
			if err := os.Chmod(path, 0o700); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			err := archiveDesktopCommand(ctx, []string{path}, finishedHost, func() bool { return tc.confirmed })
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %v, want %s", err, tc.want)
			}
		})
	}
}
