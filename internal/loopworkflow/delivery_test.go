//go:build integration

package loopworkflow

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/state"
)

func TestSupervisorWorkflowDeliveryLimit(t *testing.T) {
	binary := os.Getenv("BEEKEEPER_TEST_LOOP_BINARY")
	if binary == "" {
		binary = buildCandidate(t, "")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	p := probe{ctx: ctx, dir: t.TempDir()}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	self, err := os.Executable()
	must(err)
	must(p.prepareCanary(self, "delivery"))
	api := httptest.NewServer(http.HandlerFunc(p.serveAPI))
	defer api.Close()
	p.env = append(p.env, "BEEKEEPER_GITHUB_API="+api.URL, "FIXTURE_CANDIDATE="+binary)
	config, err := os.ReadFile(p.path("config.yaml"))
	must(err)
	config = []byte(strings.Replace(string(config), "ceiling: 1", "ceiling: 4", 1) + "delivery:\n  wipLimit: 2\n")
	must(os.WriteFile(p.path("config.yaml"), config, 0600))
	// A real merge may finish before its release is available. The fixture's
	// release waiter returns an explicit unconfirmed verdict, never success.
	must(os.WriteFile(p.path("bin/devctl"), []byte("#!/bin/sh\nprintf '%s\\n' '{\"verdict\":\"timeout\"}'\n"), 0700))
	store, err := state.Open(p.path("state"))
	must(err)
	must(store.Update(func(st *state.State) ([]state.Event, error) { st.DeliveriesAt = time.Now().UTC(); return nil, nil }))
	var logs strings.Builder
	run := func(dir string, args ...string) error {
		c := exec.CommandContext(ctx, binary, args...)
		c.Dir, c.Env = dir, p.env
		out, err := c.CombinedOutput()
		logs.Write(out)
		return err
	}
	t.Cleanup(func() {
		if t.Failed() {
			trace, _ := os.ReadFile(p.path("trace.jsonl"))
			events, _ := os.ReadFile(p.path("state/events.jsonl"))
			sanitize := func(s string) string { return strings.ReplaceAll(s, p.dir, "<fixture>") }
			if dest := os.Getenv("BEEKEEPER_TEST_LOOP_ARTIFACTS"); dest != "" {
				dest = filepath.Join(dest, t.Name())
				must(os.MkdirAll(dest, 0700))
				for name, data := range map[string]string{"output.log": logs.String(), "trace.jsonl": string(trace), "events.jsonl": string(events)} {
					must(os.WriteFile(filepath.Join(dest, name), []byte(sanitize(data)), 0600))
				}
			}
			t.Logf("%s\ntrace:\n%s\nevents:\n%s", sanitize(logs.String()), sanitize(string(trace)), sanitize(string(events)))
		}
	})
	active := func() int {
		t.Helper()
		st, err := store.Read()
		must(err)
		count := 0
		for _, d := range st.Deliveries {
			if d.Active() {
				count++
			}
		}
		if count > 2 {
			t.Fatalf("delivery limit exceeded: %d", count)
		}
		return count
	}
	pass := func() { t.Helper(); must(run(p.dir, "supervisor", "loop", "--once")); active() }
	started := func(n string) bool {
		t.Helper()
		trace, err := os.ReadFile(p.path("trace.jsonl"))
		must(err)
		return strings.Contains(string(trace), `"project `+n+`"`)
	}
	pass()
	if active() != 2 || !started("2") || !started("3") || started("4") || started("5") {
		t.Fatal("four free items and four slots did not admit exactly two issues")
	}
	// Free both worker slots without completing either delivery.
	checkout := p.path(filepath.Join("checkouts", "project"))
	must(run(checkout, "--as", "project 3", "agents", "park", "fixture implementation wait"))
	must(p.workerPublishAndPark(checkout, run))
	editPull := func(edit func(map[string]any)) {
		t.Helper()
		raw, err := os.ReadFile(p.path(pullJSONFile))
		must(err)
		var pull map[string]any
		must(json.Unmarshal(raw, &pull))
		pull["closingIssuesReferences"] = []any{map[string]any{"number": 2, "url": "https://github.com/fixture/project/issues/2", "repository": map[string]any{"nameWithOwner": "fixture/project"}}}
		edit(pull)
		raw, err = json.Marshal(pull)
		must(err)
		must(os.WriteFile(p.path(pullJSONFile), raw, 0600))
	}
	editPull(func(pull map[string]any) {
		pull["statusCheckRollup"] = []any{map[string]any{"name": "test", "status": "IN_PROGRESS", "conclusion": ""}}
	})
	pass()
	if active() != 2 || started("4") {
		t.Fatal("CI wait freed a delivery place")
	}
	editPull(func(pull map[string]any) {
		pull["statusCheckRollup"] = []any{map[string]any{"name": "test", "status": "COMPLETED", "conclusion": "SUCCESS"}}
	})
	pass() // independent reviewer starts at full WIP
	pass() // observe reviewing before the reviewer's external response
	rawReview, err := os.ReadFile(p.path(pullJSONFile))
	must(err)
	var reviewing map[string]any
	must(json.Unmarshal(rawReview, &reviewing))
	review := exec.CommandContext(ctx, p.path("bin/gh"), "pr", "review", "1", "--repo", "fixture/project", "--comment", "--body", "Desk review (opus): changes requested\nHead reviewed: `"+reviewing["headRefOid"].(string)+"`\nFix the fixture wording.")
	review.Env = p.env
	out, reviewErr := review.CombinedOutput()
	logs.Write(out)
	must(reviewErr)
	pass() // findings go back to the original worker
	events, err := os.ReadFile(p.path("state/events.jsonl"))
	must(err)
	if !strings.Contains(string(events), `"verb":"supervisor.review"`) || !strings.Contains(string(events), `"verb":"supervisor.forward"`) || started("4") {
		t.Fatal("review and fix did not precede the third issue at full WIP")
	}
	// The deterministic worker performs the requested fix and publishes a new
	// head. No review or merge outcome is injected into the candidate's state.
	rel := filepath.Join("checkouts", "project")
	must(p.runAll(rel, []string{gitCheckout, changeBranch}))
	must(os.WriteFile(filepath.Join(checkout, readmeFile), []byte("fixture corrected change\n"), 0600))
	must(p.runAll(rel, []string{gitAdd, readmeFile}, []string{gitCommit, "-m", "fix fixture wording"}, []string{gitPush, originRemote, changeBranch}))
	head, err := p.run(rel, gitRevParse, gitHead)
	must(err)
	must(p.runAll(rel, []string{gitPush, originRemote, head + ":refs/pull/1/head"}))
	editPull(func(pull map[string]any) { pull["headRefOid"] = head })
	must(run(checkout, "--as", "project 2", "agents", "park", "--on", "fixture/project#1", "--post-merge", "fixture post-release proof", "desk review"))
	must(p.runAll(rel, []string{gitCheckout, mainBranch}))
	pass()
	pass()
	raw, err := os.ReadFile(p.path(pullJSONFile))
	must(err)
	var merged struct {
		State string `json:"state"`
	}
	must(json.Unmarshal(raw, &merged))
	if merged.State != "MERGED" || active() != 2 || started("4") {
		t.Fatal("merge must keep its place until release")
	}
	// Publish the external release observation. The next pass must consume the
	// confirmed release and admit issue 4 immediately, with issue 5 still held.
	must(os.WriteFile(p.path("released"), []byte("available\n"), 0600))
	// Release probes have a persisted five-minute budget. Move only that probe
	// reservation back; this does not change a delivery step or verdict.
	must(store.Update(func(st *state.State) ([]state.Event, error) {
		for i := range st.Deliveries {
			st.Deliveries[i].ReleaseChecked = time.Time{}
		}
		return nil, nil
	}))
	pass()
	if !started("4") || started("5") || active() != 2 {
		t.Fatal("third issue did not start on the pass after release")
	}
	events, err = os.ReadFile(p.path("state/events.jsonl"))
	must(err)
	for _, step := range []string{state.DeliveryImplementing, state.DeliveryCI, state.DeliveryReviewing, state.DeliveryFixing, state.DeliveryReleasing, state.DeliveryDone} {
		if !strings.Contains(string(events), `\"step\":\"`+step+`\"`) {
			t.Fatalf("missing delivery.step evidence for %s", step)
		}
	}
	base, err := p.run(serverDir, "--git-dir="+p.path("origin.git"), gitRevParse, "refs/heads/main")
	must(err)
	if base == p.base {
		t.Fatal("workflow did not merge the corrected head into the Git origin")
	}
	if unknown, err := os.ReadFile(p.path("unknown-call")); err == nil {
		t.Fatalf("unknown fixture boundary: %s", unknown)
	}
}
