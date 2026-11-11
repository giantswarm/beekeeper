package cmd

import (
	"path/filepath"
	"testing"

	"github.com/giantswarm/beekeeper/internal/state"
)

func TestGuideCallerRejectsInheritedDesktopIdentity(t *testing.T) {
	for _, record := range []bool{false, true} {
		t.Run(map[bool]string{false: "record missing", true: "different desktop CLI"}[record], func(t *testing.T) {
			a, _ := stubApp(t)
			a.as = ""
			plat.Machine = tableMachine{plat.Machine}
			guide := state.Party{Session: bgID, Name: guideSeven}
			parent := state.Party{Session: "parent-cli", HostSession: "local_parent", Name: "parent title"}
			seed(t, a, func(st *state.State) {
				st.Starts = []state.Start{
					{Party: parent, Mode: state.ModeBypass},
					{Party: guide, Mode: state.ModeBypass, Role: guideName, Background: true},
				}
				st.Guide = &state.Role{Holder: &state.Supervisor{Party: guide, Since: a.now}, Run: 7}
			})
			if record {
				writeFile(t, filepath.Join(a.cfg.Claude.DesktopDir, "a", "b", parent.HostSession+".json"),
					`{"sessionId":"local_parent","cliSessionId":"parent-cli","title":"parent title"}`)
			}
			t.Setenv("CLAUDE_CODE_SESSION_ID", guide.Session)
			t.Setenv("CLAUDE_CODE_HOST_SESSION_ID", parent.HostSession)
			t.Setenv("CLAUDE_CODE_SESSION_NAME", parent.Name)
			got, err := a.callerSession()
			if err != nil || got.Session != guide.Session || got.HostSession != "" || got.Name != guide.Name {
				t.Fatalf("caller = %+v, %v", got, err)
			}
			if err := a.runStart(t.Context(), guideRole, false); err != nil {
				t.Fatal(err)
			}
			st, err := a.store.Read()
			if err != nil || st.Starts[0].Party != parent || st.Starts[1].Party != guide ||
				st.Guide == nil || st.Guide.Holder == nil || st.Guide.Holder.Session != guide.Session || st.Guide.Holder.HostSession != "" || st.Guide.Holder.Name != guide.Name {
				t.Fatalf("caller identity contaminated state: %+v, %v", st, err)
			}
		})
	}
}

func TestCallerKeepsDesktopIdentityForItsOwnCLI(t *testing.T) {
	a, _ := stubApp(t)
	a.as = ""
	p := state.Party{Session: restartedCLI, HostSession: restartHost, Name: restartName}
	desktopRecord(t, a.cfg.Claude.DesktopDir, restartHost, restartedCLI, false)
	a.discardInheritedHost(&p)
	if p.HostSession != restartHost || p.Name != restartName {
		t.Fatalf("own desktop identity discarded: %+v", p)
	}
}
