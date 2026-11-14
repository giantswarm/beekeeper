package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/guard"
	"github.com/giantswarm/beekeeper/internal/state"
)

const claudeLoginExpired = "expired"
const claudeLoginExpiring = "expiring"

const claudeLoginWarnBefore = time.Hour
const claudeLoginKey = "claude-login"
const claudeLoginHint = "beekeeper owns the credential step: the guide should arrange `claude auth login` in a terminal, with the person completing sign-in in their browser session while holding the browser lease"

// claudeLogin contains metadata only. Never return credential contents or JSON
// decoding errors: those can contain parts of the credential file.
type claudeLogin struct {
	State       string    `json:"state"`
	Expires     time.Time `json:"expires,omitempty"`
	Refreshable bool      `json:"refreshable"`
}

func readClaudeLogin(now time.Time) claudeLogin {
	// These credentials select another authentication route, whose lifetime
	// cannot be inferred from the saved claude.ai login.
	for _, key := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY"} {
		if os.Getenv(key) != "" {
			return claudeLogin{State: "external authentication; expiry unknown"}
		}
	}
	dir := os.Getenv("CLAUDE_CONFIG_DIR")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return claudeLogin{State: "unknown; cannot locate credentials"}
		}
		dir = filepath.Join(home, ".claude")
	}
	raw, err := os.ReadFile(filepath.Join(dir, ".credentials.json")) //nolint:gosec // read only the operator-selected CLI credential metadata; never print file contents
	if err != nil {
		return claudeLogin{State: "unknown; no readable file credentials (a keychain or helper may supply login)"}
	}
	var doc struct {
		OAuth *struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			ExpiresAt    int64  `json:"expiresAt"`
		} `json:"claudeAiOauth"`
	}
	if json.Unmarshal(raw, &doc) != nil || doc.OAuth == nil || doc.OAuth.AccessToken == "" || doc.OAuth.ExpiresAt <= 0 {
		return claudeLogin{State: "unknown; no usable claude.ai expiry metadata"}
	}
	v := claudeLogin{State: "logged in", Expires: time.UnixMilli(doc.OAuth.ExpiresAt).UTC(), Refreshable: doc.OAuth.RefreshToken != ""}
	if !v.Expires.After(now) {
		v.State = claudeLoginExpired
	} else if v.Expires.Sub(now) <= claudeLoginWarnBefore {
		v.State = claudeLoginExpiring
	}
	return v
}

func (v claudeLogin) needsLogin() bool {
	// Access-token expiry is routine when the CLI can refresh it. Keep the
	// expiry visible, but do not turn it into a repeated browser-login request.
	return !v.Refreshable && (v.State == claudeLoginExpired || v.State == claudeLoginExpiring)
}
func (v claudeLogin) line() string {
	line := "CLAUDE CLI LOGIN: " + v.State
	if !v.Expires.IsZero() {
		line += "; access token expires " + v.Expires.Format(time.RFC3339)
	}
	if v.Refreshable {
		line += "; CLI refresh available (success not verified)"
	}
	if v.needsLogin() {
		line += "; " + claudeLoginHint
	}
	return line
}

func (a *app) requireClaudeLogin() error {
	v := readClaudeLogin(a.now)
	if v.State == claudeLoginExpired && !v.Refreshable {
		return refused("headless Claude turn refused: %s", v.line())
	}
	return nil
}

// claude-login is also the login note's probe. A healthy saved login or a
// CLI refresh path clears the manual step; unknown authentication does not.
func (a *app) claudeLoginCmd() *cobra.Command {
	return &cobra.Command{Use: "claude-login", Short: "Show the local Claude CLI login and access-token expiry", Args: cobra.NoArgs, RunE: func(_ *cobra.Command, _ []string) error {
		v := readClaudeLogin(a.now)
		if a.json {
			if err := a.printJSON(v); err != nil {
				return err
			}
		} else if _, err := fmt.Fprintln(a.out, v.line()); err != nil {
			return err
		}
		if v.State != "logged in" && !v.Refreshable {
			return &exitError{code: ExitRefused}
		}
		return nil
	}}
}

func watchClaudeLogin(w *Watcher) {
	v := readClaudeLogin(w.Host.now)
	// A changed expiry gets a new status line even if the state stays healthy.
	key := claudeLoginKey + " " + v.State + " " + v.Expires.Format(time.RFC3339)
	if v.needsLogin() || v.Expires.IsZero() {
		w.ClearMissing(claudeLoginKey+" ", map[string]bool{key: true})
		w.Emit(key, "%s", v.line())
	} else {
		w.ClearMissing(claudeLoginKey+" ", nil)
		if w.LastClaudeLogin != key {
			w.EmitNow(claudeLoginKey, "%s", v.line())
		}
	}
	w.LastClaudeLogin = key
	if !v.needsLogin() || w.Host.cfg.Guide.Person == "" {
		return
	}
	self, err := os.Executable()
	if err != nil {
		return
	}
	probe := guard.ShellQuote(self) + " claude-login"
	if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		probe = "CLAUDE_CONFIG_DIR=" + guard.ShellQuote(dir) + " " + probe
	}
	if err := w.Host.store.Update(func(st *state.State) ([]state.Event, error) {
		for _, n := range st.Notes {
			if n.Kind == noteLogin && n.Until == probe {
				return nil, nil
			}
		}
		st.NextNote++
		n := state.Note{ID: st.NextNote, For: w.Host.cfg.Guide.Person, By: WatchParty, At: w.Host.now.UTC(), Due: w.Host.now.UTC(), Kind: noteLogin, Until: probe, Text: v.line(), Default: "headless Claude turns may fail until the CLI login is renewed"}
		st.Notes = append(st.Notes, n)
		return []state.Event{event(WatchParty, "note.add", "#%d %s", n.ID, n.Text)}, nil
	}); err != nil {
		w.Fail(claudeLoginKey+"-note", "CLAUDE CLI LOGIN: cannot record credential step")
	} else {
		w.Clear(claudeLoginKey + "-note")
	}
}
