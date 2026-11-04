package transcripts

import (
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
)

func TestAllowedSessionGrantExactMatch(t *testing.T) {
	now := time.Date(2026, 10, 5, 14, 0, 0, 0, time.UTC)
	caller := Caller{RosterName: "requesting-worker", Repo: "example/worker"}
	tests := []struct {
		name    string
		session string
		meta    Meta
		want    bool
	}{
		{"key", "guide-1", Meta{Key: "guide-1"}, true},
		{"longer key", "guide-1", Meta{Key: "guide-10"}, false},
		{"roster name", "guide-1", Meta{RosterName: "guide-1"}, true},
		{"longer roster name", "guide-1", Meta{RosterName: "guide-10"}, false},
		{"embedded name", "guide-1", Meta{Name: "private-guide-1-backup"}, false},
		{"name", "guide-1", Meta{Name: "guide-1"}, true},
		{"id", "guide-1", Meta{ID: "guide-1"}, true},
		{"host session", "guide-1", Meta{HostSession: "guide-1"}, true},
		{"execution", "guide-1", Meta{Execution: "guide-1"}, true},
		{"case and whitespace", " GUIDE-1 ", Meta{Key: "guide-1"}, true},
		{"issue", "example/other#1", Meta{Ref: "example/other#1"}, true},
		{"issue URL", "https://github.com/example/other/issues/1", Meta{Ref: "example/other#1"}, true},
		{"additional issue", "example/other#1", Meta{Refs: []string{"example/other#2", "example/other#1"}}, true},
		{"issue case", "EXAMPLE/OTHER#1", Meta{Ref: "example/other#1"}, true},
		{"longer issue", "example/other#1", Meta{Ref: "example/other#10"}, false},
		{"issue embedded in name", "example/other#1", Meta{Name: "private-example/other#1-backup"}, false},
		{"empty", "", Meta{Key: "guide-1"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.meta.Repo = "example/other"
			grants := []state.TranscriptGrant{{To: state.Party{Name: caller.RosterName}, Session: tt.session, Expires: now.Add(time.Hour)}}
			if got := Allowed(caller, tt.meta, grants, now); got != tt.want {
				t.Fatalf("Allowed = %v, want %v", got, tt.want)
			}
		})
	}
}
