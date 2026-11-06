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
		{"valid", 2 * time.Hour, false, "logged in", false},
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
	w.now = time.Now().UTC().Truncate(time.Millisecond)
	w.cfg.Guide.Person = "guide-person"
	writeLogin(t, path, w.now.Add(2*time.Hour), true)
	w.claudeLogin()
	if !strings.Contains(out.String(), "logged in; access token expires") {
		t.Fatalf("healthy login absent: %s", out)
	}
	writeLogin(t, path, w.now.Add(30*time.Minute), true)
	w.claudeLogin()
	w.claudeLogin()
	st, err := w.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Notes) != 1 || st.Notes[0].Kind != noteLogin || st.Notes[0].For != w.cfg.Guide.Person || !strings.Contains(st.Notes[0].Text, "browser lease") {
		t.Fatalf("credential step: %+v", st.Notes)
	}
	if strings.Count(out.String(), "CLAUDE CLI LOGIN: expiring") != 1 {
		t.Fatalf("duplicate warning: %s", out)
	}
	// A second watch reuses the same outstanding login note.
	w2, _, _ := notifyingWatch(t, w.cfg.StateDir, false)
	w2.now, w2.cfg.Guide.Person = w.now, w.cfg.Guide.Person
	w2.claudeLogin()
	st, _ = w.store.Read()
	if len(st.Notes) != 1 {
		t.Fatalf("restart duplicated note: %+v", st.Notes)
	}
	if err := w.claudeLoginCmd().RunE(nil, nil); err == nil {
		t.Fatal("expiring probe succeeded")
	}
	if !strings.Contains(st.Notes[0].Until, "CLAUDE_CONFIG_DIR=") {
		t.Fatal("probe lost account directory")
	}
	writeLogin(t, path, w.now.Add(3*time.Hour), true)
	w.claudeLogin()
	if !strings.Contains(out.String(), "ENDED") {
		t.Fatalf("warning never ended: %s", out)
	}
	// The same status command used by the note's probe succeeds after renewal.
	if err := w.claudeLoginCmd().RunE(nil, nil); err != nil {
		t.Fatal(err)
	}
}

func TestExpiredClaudeLoginStopsLaunches(t *testing.T) {
	path := loginFixture(t)
	w, _, _ := notifyingWatch(t, t.TempDir(), false)
	w.now = time.Now()
	writeLogin(t, path, w.now.Add(-time.Hour), false)
	assert := func(err error) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), "headless Claude turn refused") {
			t.Fatalf("expected login refusal, got %v", err)
		}
	}
	_, err := w.startAgent(context.Background(), agentStart{})
	assert(err)
	assert(w.resumeTurn(context.Background(), state.Party{}, wakeTarget{}, "task"))
	assert(w.resumeBackground(context.Background(), wakeTarget{}, "task"))
	assert(w.browse(context.Background(), "task", "", "", time.Minute, "", nil, nil))
	assert(w.launchReport("unused", "unused", "unused", "task"))
}
