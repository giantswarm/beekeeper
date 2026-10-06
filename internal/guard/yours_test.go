package guard

import (
	"slices"
	"strings"
	"testing"
)

// The resources of the yours tests, and the sender's session.
const (
	labA       = "lab-a"
	labB       = "lab-b"
	sessionKey = "session_id"
	yoursSup   = "sup-1"
)

func TestYours(t *testing.T) {
	resources := []string{labA, labB, "staging", "model-server", browserLease}
	for _, c := range []struct {
		message string
		want    []string
	}{
		{"yours lab-a", []string{labA}},
		{"browser yours", []string{browserLease}},
		{"lab-b is yours for the e2e proof, 40 minutes", []string{labB}},
		{"`browser yours`; then `yours staging`.", []string{browserLease, "staging"}},
		{"Yours: Browser", []string{browserLease}}, // spelt as the configuration spells it
		{"browser yours, browser yours", []string{browserLease}},
		{"the browser is not yours: wait for the proof", nil},
		{"once the merge lands staging is free", nil},
		{"yours", nil},
		{"", nil},
		{"model-server yours (--gib 12)", []string{"model-server"}},
	} {
		if got := Yours(c.message, resources); !slices.Equal(got, c.want) {
			t.Errorf("Yours(%q) = %v, want %v", c.message, got, c.want)
		}
	}
}

// A supervisor's `yours <resource>` goes through Yours with the target the
// hook resolved, and what Yours says reaches the sender as additional
// context, with and without a redirect.
func TestSendMessageRecordsTheSupervisorsYours(t *testing.T) {
	const worker = "Board pull 224"
	var got []string
	yours := func(session, to, message string) string {
		got = append(got, session+"|"+to+"|"+message)
		if !strings.Contains(message, "yours") {
			return ""
		}
		return "beekeeper: your `browser yours` is recorded: granted browser to " + to
	}
	send := func(h Hook, to, message string) *decision {
		t.Helper()
		ev := toolEvent(SendMessageTool, map[string]any{"to": to, messageKey: message})
		ev[sessionKey] = yoursSup
		return decideEvent(t, h, ev)
	}
	h := Hook{Yours: yours, Peer: func(string) (string, error) { return worker, nil }}
	d := send(h, worker, "browser yours for the proof")
	if d == nil || d.PermissionDecision != "" || !strings.Contains(d.AdditionalContext, "granted browser to "+worker) {
		t.Fatalf("a send by name with a yours: %+v", d)
	}
	d = send(h, "local_abc", "yours browser")
	if d == nil || d.PermissionDecision != decisionAllow || d.UpdatedInput["to"] != worker || !strings.Contains(d.AdditionalContext, "granted browser to "+worker) {
		t.Fatalf("a redirected send with a yours: %+v", d)
	}
	if d := send(h, worker, "merged 12 v1.2.0"); d != nil {
		t.Fatalf("a send without a yours passes unchanged: %+v", d)
	}
	if want := []string{yoursSup + "|" + worker + "|browser yours for the proof", yoursSup + "|" + worker + "|yours browser", yoursSup + "|" + worker + "|merged 12 v1.2.0"}; !slices.Equal(got, want) {
		t.Fatalf("Yours saw %q, want %q", got, want)
	}
	if d := send(Hook{}, worker, "browser yours"); d != nil {
		t.Fatalf("no Yours lookup: pass, got %+v", d)
	}
}
