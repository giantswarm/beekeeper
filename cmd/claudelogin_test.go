package cmd

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
)

func loginFixture(t *testing.T) string {
	t.Helper()
	for _, key := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY"} {
		t.Setenv(key, "")
	}
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	return filepath.Join(dir, ".credentials.json")
}

func writeLogin(t *testing.T, path string, expiry time.Time, refresh bool) {
	t.Helper()
	token := ""
	if refresh {
		token = "SECRET_REFRESH"
	}
	raw, err := json.Marshal(map[string]any{"claudeAiOauth": map[string]any{"accessToken": "SECRET_ACCESS", "refreshToken": token, "expiresAt": expiry.UnixMilli()}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestClaudeLoginStatesAndRefusal(t *testing.T) {
	path := loginFixture(t)
	now := time.Now().UTC().Truncate(time.Millisecond)
	for _, tt := range []struct {
		name    string
		offset  time.Duration
		refresh bool
		state   string
		refuse  bool
	}{
		{"valid login", 2 * time.Hour, false, "logged in", false},
		{"warning boundary", time.Hour, false, claudeLoginExpiring, false},
		{claudeLoginExpiring, time.Minute, false, claudeLoginExpiring, false},
		{"expired boundary", 0, false, claudeLoginExpired, true},
		{claudeLoginExpired, -time.Minute, false, claudeLoginExpired, true},
		{"refreshable expiry", -time.Minute, true, claudeLoginExpired, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			writeLogin(t, path, now.Add(tt.offset), tt.refresh)
			v := readClaudeLogin(now)
			if v.State != tt.state || !v.Expires.Equal(now.Add(tt.offset)) {
				t.Fatalf("metadata: %+v", v)
			}
			a := &app{now: now}
			err := a.requireClaudeLogin()
			if (err != nil) != tt.refuse {
				t.Fatalf("refusal: %v", err)
			}
			if err != nil && (!strings.Contains(err.Error(), "login") || !strings.Contains(err.Error(), "claude auth login")) {
				t.Fatalf("generic refusal: %v", err)
			}
			raw, _ := json.Marshal(v)
			if strings.Contains(v.line()+string(raw), "SECRET") {
				t.Fatal("credential leaked")
			}
		})
	}
}

func TestClaudeLoginUnknownAndExternal(t *testing.T) {
	path := loginFixture(t)
	now := time.Now()
	if v := readClaudeLogin(now); !strings.HasPrefix(v.State, "unknown") {
		t.Fatalf("missing: %+v", v)
	}
	if err := os.WriteFile(path, []byte(`{"claudeAiOauth":{"accessToken":SECRET_PRIVATE`), 0600); err != nil {
		t.Fatal(err)
	}
	if v := readClaudeLogin(now); !strings.HasPrefix(v.State, "unknown") || strings.Contains(v.line(), "SECRET") {
		t.Fatalf("malformed: %+v", v)
	}
	writeLogin(t, path, now.Add(-time.Hour), false)
	t.Setenv("ANTHROPIC_API_KEY", "SECRET_API_KEY")
	if err := (&app{now: now}).requireClaudeLogin(); err != nil {
		t.Fatalf("external auth blocked: %v", err)
	}
	if v := readClaudeLogin(now); !v.Expires.IsZero() || !strings.HasPrefix(v.State, "external") {
		t.Fatalf("external: %+v", v)
	}
}

func TestWatchClaudeLoginCredentialStep(t *testing.T) {
	path := loginFixture(t)
	w, _, out := notifyingWatch(t, t.TempDir(), false)
	watchApp(w.Host).now = time.Now().UTC().Truncate(time.Millisecond)
	watchApp(w.Host).cfg.Guide.Person = "guide-person"
	writeLogin(t, path, watchApp(w.Host).now.Add(2*time.Hour), true)
	w.ClaudeLogin()
	if !strings.Contains(out.String(), "logged in; access token expires") {
		t.Fatalf("healthy login absent: %s", out)
	}
	writeLogin(t, path, watchApp(w.Host).now.Add(30*time.Minute), false)
	w.ClaudeLogin()
	w.ClaudeLogin()
	st, err := watchApp(w.Host).store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Notes) != 1 || st.Notes[0].Kind != noteLogin || st.Notes[0].For != watchApp(w.Host).cfg.Guide.Person || !strings.Contains(st.Notes[0].Text, "browser lease") {
		t.Fatalf("credential step: %+v", st.Notes)
	}
	if strings.Count(out.String(), "CLAUDE CLI LOGIN: expiring") != 1 {
		t.Fatalf("duplicate warning: %s", out)
	}
	// A second watch reuses the same outstanding login note.
	w2, _, _ := notifyingWatch(t, watchApp(w.Host).cfg.StateDir, false)
	watchApp(w2.Host).now, watchApp(w2.Host).cfg.Guide.Person = watchApp(w.Host).now, watchApp(w.Host).cfg.Guide.Person
	w2.ClaudeLogin()
	st, _ = watchApp(w.Host).store.Read()
	if len(st.Notes) != 1 {
		t.Fatalf("restart duplicated note: %+v", st.Notes)
	}
	if err := watchApp(w.Host).claudeLoginCmd().RunE(nil, nil); err == nil {
		t.Fatal("expiring probe succeeded")
	}
	if !strings.Contains(st.Notes[0].Until, "CLAUDE_CONFIG_DIR=") {
		t.Fatal("probe lost account directory")
	}
	writeLogin(t, path, watchApp(w.Host).now.Add(3*time.Hour), true)
	w.ClaudeLogin()
	if !strings.Contains(out.String(), "ENDED") {
		t.Fatalf("warning never ended: %s", out)
	}
	// The same status command used by the note's probe succeeds after renewal.
	if err := watchApp(w.Host).claudeLoginCmd().RunE(nil, nil); err != nil {
		t.Fatal(err)
	}
}

func TestExpiredClaudeLoginStopsLaunches(t *testing.T) {
	path := loginFixture(t)
	w, _, _ := notifyingWatch(t, t.TempDir(), false)
	watchApp(w.Host).now = time.Now()
	writeLogin(t, path, watchApp(w.Host).now.Add(-time.Hour), false)
	assert := func(err error) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), "headless Claude turn refused") {
			t.Fatalf("expected login refusal, got %v", err)
		}
	}
	_, err := watchApp(w.Host).startAgent(context.Background(), agentStart{})
	assert(err)
	assert(watchApp(w.Host).resumeTurn(context.Background(), state.Party{}, wakeTarget{}, "task"))
	assert(watchApp(w.Host).resumeBackground(context.Background(), wakeTarget{}, "task"))
	assert(watchApp(w.Host).browse(context.Background(), "task", "", "", time.Minute, "", nil, nil))
	assert(w.LaunchReport("unused", "unused", "unused", "task"))
}

func TestClaudeLoginUTCExpiryInCET(t *testing.T) {
	path := loginFixture(t)
	cet := time.FixedZone("CET", 60*60)
	now := time.Date(2026, 11, 7, 13, 17, 0, 0, cet)
	expiry := time.Date(2026, 11, 7, 13, 14, 57, 0, time.UTC)
	writeLogin(t, path, expiry, true)
	v := readClaudeLogin(now)
	if v.State != claudeLoginExpiring || v.needsLogin() {
		t.Fatalf("refreshable token with 57 minutes remaining needs no manual login: %+v", v)
	}
	if utc := readClaudeLogin(now.UTC()); utc != v {
		t.Fatalf("timezone changed login state: local=%+v UTC=%+v", v, utc)
	}
}

func TestRefreshableClaudeLoginDoesNotFilePersonNote(t *testing.T) {
	path := loginFixture(t)
	w, _, out := notifyingWatch(t, t.TempDir(), false)
	watchApp(w.Host).now = time.Now().UTC().Truncate(time.Millisecond)
	watchApp(w.Host).cfg.Guide.Person = "guide-person"
	for _, offset := range []time.Duration{30 * time.Minute, -time.Minute} {
		writeLogin(t, path, watchApp(w.Host).now.Add(offset), true)
		w.ClaudeLogin()
		w.ClaudeLogin()
		st, err := watchApp(w.Host).store.Read()
		if err != nil {
			t.Fatal(err)
		}
		if len(st.Notes) != 0 {
			t.Fatalf("routine CLI refresh filed person note: %+v", st.Notes)
		}
		// Existing login notes use this command as their completion probe.
		if err := watchApp(w.Host).claudeLoginCmd().RunE(nil, nil); err != nil {
			t.Fatalf("refreshable login must clear manual step: %v", err)
		}
	}
	if strings.Contains(out.String(), "browser lease") || !strings.Contains(out.String(), "CLI refresh available") {
		t.Fatalf("refresh status must stay visible without requesting sign-in: %s", out)
	}
}
