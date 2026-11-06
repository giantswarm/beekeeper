package loopworkflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
)

// ProbeTimeout bounds one probe, under a controller's minute between evaluations.
const ProbeTimeout = 45 * time.Second

// ErrInconclusive marks a probe that says nothing about the candidate: the
// fixture could not be built here (no git, a full disk). A failed candidate is
// every other error.
var ErrInconclusive = errors.New("probe inconclusive")

// Probe drives the candidate at binary through a reviewed pull request of a
// scratch origin: real loop passes, parser, merge validation and gate, with a
// fixture GitHub only. server is the executable that plays that GitHub, a
// binary that calls Serve first; the candidate's own bytes never judge
// themselves. It needs no backlog, account or network, and leaves nothing
// behind in scratch. Nil means the merge is proven in the origin.
func Probe(ctx context.Context, binary, server, scratch string) error {
	ctx, cancel := context.WithTimeout(ctx, ProbeTimeout)
	defer cancel()
	dir, err := os.MkdirTemp(scratch, "probe-*")
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInconclusive, err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	p := probe{ctx: ctx, dir: dir}
	if err := p.prepare(server); err != nil {
		return fmt.Errorf("%w: %w", ErrInconclusive, err)
	}
	api := httptest.NewServer(http.HandlerFunc(p.serveAPI))
	defer api.Close()
	p.env = append(p.env, "BEEKEEPER_GITHUB_API="+api.URL)
	for range 2 {
		c := exec.CommandContext(ctx, binary, "supervisor", "loop", "--once") //nolint:gosec // the installed candidate under probation
		c.Env, c.Dir = p.env, dir
		if out, err := c.CombinedOutput(); err != nil {
			// A loaded machine can run out of time; a heartbeat names a hung loop.
			if ctx.Err() != nil {
				return fmt.Errorf("%w: %w", ErrInconclusive, ctx.Err())
			}
			return fmt.Errorf("supervisor loop --once: %w: %s", err, tail(out))
		}
	}
	return p.proven()
}

type probe struct {
	ctx      context.Context //nolint:containedctx // bounds this one probe
	dir, git string
	base     string
	env      []string
	now      time.Time
}

func (p *probe) path(name string) string { return filepath.Join(p.dir, name) }

func (p *probe) run(cwd string, args ...string) (string, error) {
	c := exec.CommandContext(p.ctx, p.git, args...) //nolint:gosec // this machine's git on a scratch repository
	c.Dir, c.Env = p.path(cwd), p.env
	out, err := c.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, tail(out))
	}
	return strings.TrimSpace(string(out)), nil
}

func (p *probe) runAll(cwd string, steps ...[]string) error {
	for _, s := range steps {
		if _, err := p.run(cwd, s...); err != nil {
			return err
		}
	}
	return nil
}

// prepare builds the origin, one reviewed change, the configuration and the
// state, as the integration test's no-checkout scenario does.
func (p *probe) prepare(server string) (err error) {
	if p.git, err = exec.LookPath("git"); err != nil {
		return err
	}
	for _, d := range []string{"bin", "server"} {
		if err := os.MkdirAll(p.path(d), 0o700); err != nil {
			return err
		}
	}
	for _, name := range []string{"gh", "git"} {
		if err := os.Symlink(server, filepath.Join(p.path("bin"), name)); err != nil {
			return err
		}
	}
	home := p.path("home")
	p.env = []string{
		"HOME=" + home, "XDG_CONFIG_HOME=" + home, "XDG_STATE_HOME=" + home, "PATH=" + p.path("bin"), "LANG=C.UTF-8", "TZ=UTC",
		"BEEKEEPER_CONFIG=" + p.path("config.yaml"), "FIXTURE_DIR=" + p.dir, "FIXTURE_GIT=" + p.git, "FIXTURE_CASE=no-checkout",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_ALLOW_PROTOCOL=file", "GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=Fixture", "GIT_AUTHOR_EMAIL=fixture@example.invalid", "GIT_COMMITTER_NAME=Fixture", "GIT_COMMITTER_EMAIL=fixture@example.invalid",
		"HTTP_PROXY=http://127.0.0.1:1", "HTTPS_PROXY=http://127.0.0.1:1", "NO_PROXY=127.0.0.1",
	}
	readme := filepath.Join("server", "README.md")
	write := func(name, text string) error { return os.WriteFile(p.path(name), []byte(text), 0o600) }
	if err := p.runAll("server", []string{"init", "--bare", p.path("origin.git")}, []string{"init", "-b", "main"}, []string{"remote", "add", "origin", p.path("origin.git")}); err != nil {
		return err
	}
	if err := write(readme, "fixture base\n"); err != nil {
		return err
	}
	if err := p.runAll("server", []string{"add", "."}, []string{"commit", "-m", "fixture base"}, []string{"push", "origin", "main"}, []string{"checkout", "-b", "change"}); err != nil {
		return err
	}
	if err := write(readme, "fixture change\n"); err != nil {
		return err
	}
	if err := p.runAll("server", []string{"add", "."}, []string{"commit", "-m", "fixture change"}); err != nil {
		return err
	}
	head, err := p.run("server", "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if err := p.runAll("server", []string{"push", "origin", "change"}, []string{"push", "origin", head + ":refs/pull/1/head"}, []string{"checkout", "main"}); err != nil {
		return err
	}
	if p.base, err = p.run("server", "rev-parse", "HEAD"); err != nil {
		return err
	}
	p.now = time.Now().UTC()
	if err := write("config.yaml", fmt.Sprintf("board:\n  owner: fixture\n  project: 1\n  tidyEvery: -1s\n  order:\n  - name: ready\n    status: [Ready]\ncapacity:\n  ceiling: 0\nidentity:\n  host: fixture\nstateDir: %s\ndisk:\n  checkouts: [\"\"]\ngithub:\n  floor: -1\n  allowedOrgs: [fixture]\nsupervisor:\n  ownership:\n    authors: [fixture-bot]\n  reviewRounds: 1\n", p.path("state"))); err != nil {
		return err
	}
	raw, err := json.Marshal(map[string]any{
		"state": "OPEN", "url": "https://github.com/fixture/project/pull/1", "baseRefName": "main", "headRefName": "change", "headRefOid": head,
		"author": map[string]any{"login": "fixture-bot"}, "title": "fixture change", "isDraft": false, "mergeable": "MERGEABLE", "mergeStateStatus": "CLEAN",
		"changedFiles": 1, "files": []any{map[string]any{"path": "README.md"}},
		"statusCheckRollup": []any{map[string]any{"name": "test", "status": "COMPLETED", "conclusion": "SUCCESS"}},
		"reviews":           []any{map[string]any{"id": "R1", "author": map[string]any{"login": "fixture-bot"}, "authorAssociation": "MEMBER", "state": "COMMENTED", "body": "Review: approved\nHead reviewed: `" + head + "`", "commit": map[string]any{"oid": head}, "submittedAt": p.now.Format(time.RFC3339)}},
	})
	if err != nil {
		return err
	}
	if err := os.WriteFile(p.path("pull.json"), raw, 0o600); err != nil {
		return err
	}
	return p.seed(head)
}

// seed writes the review park and events a reviewed pull request has.
func (p *probe) seed(head string) error {
	store, err := state.Open(p.path("state"))
	if err != nil {
		return err
	}
	if err := store.Update(func(st *state.State) ([]state.Event, error) {
		st.AutoInstallPaused = true
		st.ReviewParks = []state.ReviewPark{{On: "fixture/project#1", At: p.now, ReviewHead: head, Agent: "fixture-worker"}}
		st.Budget = &state.Budget{Remaining: 5000, Limit: 5000, Reset: p.now.Add(time.Hour), At: p.now, GraphQL: &state.GraphQL{Remaining: 5000, Limit: 5000, Reset: p.now.Add(time.Hour), At: p.now}}
		return nil, nil
	}); err != nil {
		return err
	}
	by := state.Party{Name: "supervisor loop"}
	for _, e := range []state.Event{
		{At: p.now.Add(-time.Minute), By: by, Verb: "supervisor.loop", Detail: "started"},
		{At: p.now, By: by, Verb: "watch.budget-state", Detail: "false"},
		{At: p.now, By: by, Verb: "supervisor.review", Subject: "fixture/project#1", Detail: "[review fixture/project#1@" + head + "] fixture-reviewer"},
	} {
		if err := store.Record(e); err != nil {
			return err
		}
	}
	return nil
}

func (p *probe) serveAPI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-RateLimit-Limit", "5000")
	w.Header().Set("X-RateLimit-Remaining", "4900")
	w.Header().Set("X-RateLimit-Reset", fmt.Sprint(p.now.Add(time.Hour).Unix()))
	switch r.URL.Path {
	case "/repos/giantswarm/beekeeper":
		_, _ = fmt.Fprint(w, `{}`)
	case "/graphql":
		_, _ = fmt.Fprintf(w, `{"data":{"viewer":{"login":"fixture-bot"},"rateLimit":{"limit":5000,"remaining":4900,"used":100,"resetAt":%q}}}`, p.now.Add(time.Hour).Format(time.RFC3339))
	case "/rate_limit":
		_, _ = fmt.Fprint(w, `{"resources":{"search":{"limit":30,"remaining":30},"code_search":{"limit":10,"remaining":10}}}`)
	default:
		http.Error(w, "unknown fixture call", http.StatusInternalServerError)
	}
}

// proven is merge proof read from the origin, never from a command's exit: the
// base moved to the reported merge commit, by exactly one mutation.
func (p *probe) proven() error {
	if unknown, err := os.ReadFile(p.path("unknown-call")); err == nil {
		return fmt.Errorf("unrecognized external command: %s", unknown)
	}
	trace, err := os.ReadFile(p.path("trace.jsonl"))
	if err != nil {
		return fmt.Errorf("no external call was made: %w", err)
	}
	if n := strings.Count(string(trace), `"gh","pr","merge"`); n != 1 {
		return fmt.Errorf("the reviewed pull request saw %d merges", n)
	}
	after, err := p.run("server", "--git-dir="+p.path("origin.git"), "rev-parse", "refs/heads/main")
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInconclusive, err)
	}
	raw, err := os.ReadFile(p.path("pull.json"))
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInconclusive, err)
	}
	var result struct {
		MergeCommit struct {
			OID string `json:"oid"`
		} `json:"mergeCommit"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return fmt.Errorf("%w: %w", ErrInconclusive, err)
	}
	if after == p.base || after != result.MergeCommit.OID {
		return errors.New("the origin does not show the reviewed merge")
	}
	return nil
}

// tail is the end of a command's output, enough to name a failure.
func tail(out []byte) string {
	const limit = 400
	s := strings.TrimSpace(string(out))
	if len(s) > limit {
		s = s[len(s)-limit:]
	}
	return s
}
