package transcripts

import (
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
)

func TestTranscriptGrantStaysBoundToRecipientIdentity(t *testing.T) {
	recipient := state.Party{Name: "worker", Session: "session-original", HostSession: "desktop-original", Person: "person-a", Host: "installation-a"}
	tests := []struct {
		name   string
		to     state.Party
		caller Caller
		want   bool
	}{
		{"original session", recipient, Caller{Party: recipient}, true},
		{"desktop restart", recipient, Caller{Party: state.Party{Session: "session-restarted", HostSession: recipient.HostSession, Person: recipient.Person, Host: recipient.Host}}, true},
		{"reused roster name", recipient, Caller{Party: state.Party{Session: "session-replacement"}, RosterName: recipient.Name}, false},
		{"reused party name", recipient, Caller{Party: state.Party{Session: "session-replacement", Name: recipient.Name}}, false},
		{"different person", recipient, Caller{Party: state.Party{Session: recipient.Session, Person: "person-b"}, RosterName: recipient.Name}, false},
		{"different host", recipient, Caller{Party: state.Party{HostSession: recipient.HostSession, Host: "installation-b"}, RosterName: recipient.Name}, false},
		{"session alias is roster name", recipient, Caller{RosterName: recipient.Session}, false},
		{"session alias is execution", recipient, Caller{Execution: recipient.Session}, false},
		{"desktop only reused name", state.Party{Name: recipient.Name, HostSession: recipient.HostSession}, Caller{Party: state.Party{Name: recipient.Name}}, false},
		{"name only roster compatibility", state.Party{Name: "WORKER"}, Caller{RosterName: recipient.Name}, true},
		{"name only session compatibility", state.Party{Name: recipient.Session}, Caller{Party: recipient}, true},
	}
	now := time.Now()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			grant := state.TranscriptGrant{To: tt.to, All: true}
			if got := GrantAppliesTo(grant, tt.caller); got != tt.want {
				t.Errorf("GrantAppliesTo = %v, want %v", got, tt.want)
			}
			if got := Allowed(tt.caller, Meta{ID: "unrelated-transcript"}, []state.TranscriptGrant{grant}, now); got != tt.want {
				t.Errorf("Allowed = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSessionGrantDoesNotAuthorizeSubstringMatches(t *testing.T) {
	const grantedSession = "guide-1"
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	caller := Caller{RosterName: "requesting-worker", Repo: "example/source"}
	grant := state.TranscriptGrant{
		To: state.Party{Name: caller.RosterName}, Session: grantedSession,
		Expires: now.Add(time.Hour),
	}
	for _, tc := range []struct {
		name string
		meta Meta
		want bool
	}{
		{"exact key", Meta{Key: grantedSession}, true},
		{"exact name", Meta{RosterName: "Guide-1"}, true},
		{"longer key", Meta{Key: "guide-10"}, false},
		{"longer name", Meta{RosterName: "guide-10"}, false},
		{"embedded name", Meta{Name: "private-guide-1-backup"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Allowed(caller, tc.meta, []state.TranscriptGrant{grant}, now); got != tc.want {
				t.Fatalf("Allowed = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSessionGrantPreservesReferenceScope(t *testing.T) {
	grant := state.TranscriptGrant{Session: "https://github.com/example/target/issues/12"}
	for _, meta := range []Meta{
		{Ref: "example/target#12"},
		{Refs: []string{"example/target#12"}},
	} {
		if !grantCovers(grant, meta, "") {
			t.Fatal("session grant should cover its normalized issue reference")
		}
	}
	if grantCovers(grant, Meta{Ref: "example/target#123"}, "") {
		t.Fatal("session grant covered another issue")
	}
}
