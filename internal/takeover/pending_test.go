package takeover

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPendingUsesLiteralDirectoryNames(t *testing.T) {
	for _, tc := range []struct {
		name, state, session, otherState, otherSession string
	}{
		{"state directory", "state[work]", tSession, "statew", tSession},
		{"unclosed bracket", "state[", tSession, "other", tSession},
		{"session directory", "state", "session[ab]", "state", "sessiona"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			dir := Dir(filepath.Join(root, tc.state))
			at := time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)
			writePendingFixture(t, dir, Request{ID: "a-later", Session: tc.session, At: at.Add(time.Minute)})
			writePendingFixture(t, dir, Request{ID: "z-earlier", Session: tc.session, At: at})
			writePendingFixture(t, Dir(filepath.Join(root, tc.otherState)), Request{
				ID: "other-session", Session: tc.otherSession, At: at.Add(-time.Minute),
			})

			rs, err := Pending(dir, tc.session)
			if err != nil {
				t.Fatal(err)
			}
			if len(rs) != 2 || rs[0].ID != "z-earlier" || rs[1].ID != "a-later" {
				t.Fatalf("Pending = %+v, want this session's two requests, oldest first", rs)
			}
		})
	}
}

func TestPendingMissingSessionHasNoRequests(t *testing.T) {
	rs, err := Pending(t.TempDir(), tSession)
	if err != nil || len(rs) != 0 {
		t.Fatalf("Pending for a missing session = %+v, %v, want no requests and no error", rs, err)
	}
}

func writePendingFixture(t *testing.T, dir string, r Request) {
	t.Helper()
	folder := filepath.Join(dir, r.Session)
	if err := os.MkdirAll(folder, 0o700); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(folder, r.ID+requestExt), b, 0o600); err != nil {
		t.Fatal(err)
	}
}
