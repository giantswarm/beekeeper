package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/state"
)

// fakeRelay puts a claude on PATH whose relay turn records each SendMessage
// input it is given and answers delivered, or fails to authenticate when
// fail is set. It returns the file of recorded inputs, one per line.
func fakeRelay(t *testing.T, fail bool) string {
	t.Helper()
	dir := t.TempDir()
	sent := filepath.Join(dir, "sent")
	turn := `echo "$req" >> "` + sent + `"
echo '{"type":"user","message":{"content":[{"type":"tool_result","content":"{\"success\":true}"}]}}'`
	if fail {
		turn = `echo '{"type":"assistant","error":"authentication_failed","message":{"content":[]}}'
echo '{"type":"result","is_error":true,"result":"Failed to authenticate: OAuth session expired and could not be refreshed"}'
exit 1`
	}
	script := "#!/bin/sh\nfor a; do last=\"$a\"; done\nreq=\"{${last#*\\{}\"\n" + turn + "\n"
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(script), 0o700); err != nil { //nolint:gosec // a test script
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return sent
}

func relayed(t *testing.T, sent string) []string {
	t.Helper()
	b, _ := os.ReadFile(sent) //nolint:gosec // the test's own file
	var tos []string
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var in struct{ To string }
		if json.Unmarshal([]byte(l), &in) == nil && in.To != "" {
			tos = append(tos, in.To)
		}
	}
	return tos
}

// A broadcast and a steward's archive ask reach a running CLI through the
// relay turn; when the relay turn cannot run, both name its error.
func TestPeerSendDeliversBroadcastsAndStewardAsks(t *testing.T) {
	a, _ := noteApp(t)
	a.cfg.StateDir = t.TempDir()
	agents := []state.Agent{{Party: four}, {Party: state.Party{Session: "s5", Name: agentFive}}}
	sessions := []*claude.Session{{ID: "s4", HostID: hostFour, Name: agentFour}, {ID: "s5", Name: agentFive}}
	s := steward{host: "local_steward", sock: "/run/user/1000/cc-socks/1.sock"}
	ask := archiveRequest(s, []string{"local_finished"}, "beekeeper doctor", "agreed")
	done := func() bool { return true }

	sent := fakeRelay(t, false)
	told, missed := broadcast(context.Background(), agents, sessions, nil, func(ctx context.Context, name string) error {
		return a.peerSend(ctx, name, "hello")
	})
	if fmt.Sprint(told) != "[Agent four Agent five]" || len(missed) != 0 {
		t.Fatalf("broadcast told %v, missed %v", told, missed)
	}
	if err := askSteward(context.Background(), s, ask, done, a.peerSend, time.Second); err != nil {
		t.Fatalf("archive ask: %v", err)
	}
	if got := fmt.Sprint(relayed(t, sent)); got != "[Agent four Agent five uds:"+s.sock+"]" {
		t.Errorf("relayed to %s", got)
	}

	fakeRelay(t, true)
	_, missed = broadcast(context.Background(), agents, sessions, nil, func(ctx context.Context, name string) error {
		return a.peerSend(ctx, name, "hello")
	})
	err := askSteward(context.Background(), s, ask, done, a.peerSend, time.Second)
	if len(missed) != 2 {
		t.Fatalf("broadcast missed %v", missed)
	}
	for _, e := range append(missed, fmt.Sprint(err)) {
		if !strings.Contains(e, "the relay turn failed: authentication_failed; Failed to authenticate") || strings.Contains(e, "no SendMessage") {
			t.Errorf("a failed relay turn reads %q", e)
		}
	}
}
