package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/codex"
	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/state"
)

// codexInbox makes the inbox of the codex start id in dir with a reader, as
// a running codex agent holds it.
func codexInbox(t *testing.T, dir, id string) string {
	t.Helper()
	inbox := codex.InboxPath(dir, id)
	if err := codex.MakeInbox(inbox); err != nil {
		t.Fatal(err)
	}
	r, err := os.OpenFile(inbox, os.O_RDWR, 0) //nolint:gosec // test inbox
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return inbox
}

// The guide answers a codex worker's message by SendMessage to the worker's
// roster name: the hook writes it to the worker's inbox, headed with the
// guide's own stable address.
func TestGuideRepliesToACodexWorkerByRosterName(t *testing.T) {
	const (
		workerID     = "1234abcd-0000-4000-8000-000000000001"
		guideSession = "7c0ffee0-0000-4000-8000-000000000002"
		workerName   = "codex: giantswarm/beekeeper#2143"
	)
	dir := t.TempDir()
	store, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Update(func(st *state.State) ([]state.Event, error) {
		st.SetGuideRole(state.Role{Holder: &state.Supervisor{Party: state.Party{Session: guideSession, Name: guideSeven}, Since: time.Now()}})
		st.Agents = append(st.Agents, state.Agent{Party: state.Party{HostSession: codex.HostPrefix + workerID, Name: workerName}})
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	inbox := codexInbox(t, dir, workerID)
	cfgFile := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgFile, []byte("stateDir: "+dir+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	hookCmd := New()
	var out bytes.Buffer
	hookCmd.SetOut(&out)
	hookCmd.SetIn(strings.NewReader(`{"session_id":"` + guideSession + `","hook_event_name":"PreToolUse","tool_name":"SendMessage","tool_input":{"to":"` + workerName + `","message":"merge on the go","summary":"go"}}`))
	hookCmd.SetArgs([]string{"--" + configFlag + "=" + cfgFile, configHookVerb, configPreToolUse})
	if err := hookCmd.Execute(); err != nil {
		t.Fatalf("hook pretooluse: %v", err)
	}
	if !strings.Contains(out.String(), "Handled by beekeeper") || !strings.Contains(out.String(), "codex inbox of "+workerName) {
		t.Fatalf("hook output %s, want the delivery to the codex inbox", out.String())
	}
	msgs, err := codex.Drain(inbox)
	if err != nil || len(msgs) != 1 {
		t.Fatalf("Drain = (%q, %v), want one message", msgs, err)
	}
	if want := `[from "guide": reply by SendMessage to "guide", or beekeeper agents wake "guide"]` + "\nmerge on the go"; msgs[0] != want {
		t.Fatalf("inbox got %q, want %q", msgs[0], want)
	}
}

// A worker's wake carries its roster name, which outlives the relay turn
// that delivers it.
func TestWakeCarriesTheSendersRosterName(t *testing.T) {
	const (
		senderID = "1234abcd-0000-4000-8000-000000000003"
		peerID   = "1234abcd-0000-4000-8000-000000000004"
	)
	dir := t.TempDir()
	store, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	sender := state.Party{HostSession: codex.HostPrefix + senderID, Name: "worker a"}
	if err := store.Update(func(st *state.State) ([]state.Event, error) {
		st.Agents = append(st.Agents, state.Agent{Party: sender}, state.Agent{Party: state.Party{HostSession: codex.HostPrefix + peerID, Name: "worker b"}})
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	inbox := codexInbox(t, dir, peerID)
	a := &app{cfg: &config.Config{StateDir: dir}, store: store, out: &bytes.Buffer{}, now: time.Now()}
	for _, c := range []struct {
		by        state.Party
		msg, want string
	}{
		{sender, "rebased, please look", `[from "worker a": reply by SendMessage to "worker a", or beekeeper agents wake "worker a"]` + "\nrebased, please look"},
		// A sender off the roster has no address to reply by.
		{state.Party{Name: "a script"}, "as sent", "as sent"},
	} {
		if err := a.wakeAgent(t.Context(), c.by, "worker b", c.msg, ""); err != nil {
			t.Fatal(err)
		}
		msgs, err := codex.Drain(inbox)
		if err != nil || len(msgs) != 1 || msgs[0] != c.want {
			t.Fatalf("Drain = (%q, %v), want [%q]", msgs, err, c.want)
		}
		if err := codex.Confirm(inbox); err != nil {
			t.Fatal(err)
		}
	}
}
