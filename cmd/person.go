package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/guard"
)

// rosterFresh is how long a read roster answers before it is read again.
const rosterFresh = 24 * time.Hour

// personName is the person command's name, which the broker runs too.
const personName = "person"

// defaultOrg is the org person answers for unless --org names another.
const defaultOrg = "giantswarm"

// ghTokenEnv are the variables gh takes a token from ahead of its own
// login: an agent shell may carry the App's there.
var ghTokenEnv = []string{"GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN"}

// personAnswer is what person prints with --json.
type personAnswer struct {
	Login  string    `json:"login"`
	Org    string    `json:"org"`
	Answer string    `json:"answer"`
	Reason string    `json:"reason,omitempty"`
	At     time.Time `json:"rosterAt,omitzero"`
}

func (a *app) personCmd() *cobra.Command {
	var org string
	var refresh bool
	c := &cobra.Command{
		Use:   personName + " <login>",
		Short: "Whether a GitHub login is a member of the org: member, not a member or permission missing",
		Long: `Answer whether login is a member of the org (--org, default giantswarm),
private memberships included, so that no agent asks GitHub's members
endpoints itself: the App's token an agent's gh carries sees public
memberships only, and its 404 for a private member reads as "not a member".

The answer comes from the org's roster, read with the person's own gh login
(the gh on PATH outside agents.shell.path, ignoring GH_TOKEN and
GITHUB_TOKEN) and kept for a day in the state directory; --refresh reads it
again. The token is held in this process only. A login whose token may not
read the whole membership (no read:org, not an active member, an App
without the members permission) answers "permission missing", never "not a
member". In the agent sandbox the host's broker answers.

Prints member or not a member and exits 0; prints permission missing and
exits 3 when the roster cannot be read whole.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			login := args[0]
			ans := personAnswer{Login: login, Org: org}
			r, err := a.roster(cmd.Context(), org, refresh)
			switch {
			case errors.Is(err, github.ErrPermission):
				ans.Answer, ans.Reason = github.ErrPermission.Error(), err.Error()
			case err != nil:
				return err
			default:
				ans.Answer, ans.At = r.Answer(login), r.At
			}
			if a.json {
				if err := json.NewEncoder(cmd.OutOrStdout()).Encode(ans); err != nil {
					return err
				}
			} else {
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), ans.Answer)
			}
			if ans.Reason != "" {
				return refused("%s", ans.Reason)
			}
			return nil
		},
	}
	c.Flags().StringVar(&org, "org", defaultOrg, "the GitHub organization")
	c.Flags().BoolVar(&refresh, "refresh", false, "read the roster again even when the kept one is under a day old")
	return c
}

// rosterPath is where org's roster is kept.
func (a *app) rosterPath(org string) string {
	return filepath.Join(a.cfg.StateDir, "roster", org+".json")
}

// roster is org's roster: the kept one while under a day old, else read
// with the person's own gh login and kept. A roster that cannot be read
// whole is never kept.
func (a *app) roster(ctx context.Context, org string, refresh bool) (github.Roster, error) {
	if strings.ContainsAny(org, `/\`) || org == "" || org[0] == '.' {
		return github.Roster{}, usageErr("--org %q is not an organization", org)
	}
	path := a.rosterPath(org)
	now := time.Now().UTC()
	if !refresh {
		var kept github.Roster
		if b, err := os.ReadFile(path); err == nil && json.Unmarshal(b, &kept) == nil && //nolint:gosec // the state directory's roster file
			kept.Org == org && !kept.At.After(now) && now.Sub(kept.At) < rosterFresh {
			return kept, nil
		}
	}
	token, err := personToken(ctx, a.cfg.Agents.Shell.Path)
	if err != nil {
		return github.Roster{}, err
	}
	r, err := github.ReadRoster(ctx, &http.Client{Timeout: 30 * time.Second}, token, org, now)
	if err != nil {
		return github.Roster{}, err
	}
	b, err := json.Marshal(r)
	if err != nil {
		return r, err
	}
	return r, writeKept(path, b)
}

// writeKept replaces path with b in one rename, so that a reader never
// finds half a roster.
func writeKept(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	_, werr := f.Write(b)
	if err := errors.Join(werr, f.Close()); err != nil {
		_ = os.Remove(f.Name())
		return err
	}
	return os.Rename(f.Name(), path)
}

// personToken is the token of the person's own gh login: gh found on PATH
// outside the agent's own directories (agents.shell.path), asked with the
// token variables an agent shell sets removed.
func personToken(ctx context.Context, agentPath []string) (string, error) {
	gh, err := personGH(os.Getenv("PATH"), agentPath)
	if err != nil {
		return "", err
	}
	c := exec.CommandContext(ctx, gh, "auth", "token") //nolint:gosec // gh from PATH
	c.Env = slices.DeleteFunc(os.Environ(), func(kv string) bool {
		name, _, _ := strings.Cut(kv, "=")
		return slices.Contains(ghTokenEnv, name)
	})
	out, err := c.Output()
	if err != nil {
		return "", fmt.Errorf("%s auth token: %w (the person logs in with `gh auth login`)", gh, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// personGH is the first gh on path that is not the agent's own: none of
// agentPath's directories.
func personGH(path string, agentPath []string) (string, error) {
	for _, dir := range filepath.SplitList(path) {
		if dir == "" {
			continue
		}
		gh := filepath.Join(dir, "gh")
		if len(agentPath) > 0 && guard.Brokered(gh, agentPath) {
			continue
		}
		if fi, err := os.Stat(gh); err == nil && fi.Mode().IsRegular() && fi.Mode()&0o111 != 0 { //nolint:gosec // a directory of the caller's PATH
			return gh, nil
		}
	}
	return "", fmt.Errorf("no gh on PATH outside the agent's own directories (%s)", strings.Join(agentPath, ", "))
}
