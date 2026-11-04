package transcripts

import (
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
)

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
