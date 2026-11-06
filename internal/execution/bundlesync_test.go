package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

const bundleRule = "Follow the reviewed process."

// fixtureBundle installs nothing: it builds a bundle whose rule says rule.
func fixtureBundle(t *testing.T, rule string) CoordinatorBundle {
	t.Helper()
	root, src := bundleFixture(t)
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte(rule), 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := BuildCoordinatorBundle(root, src, map[string]string{"model_reasoning_effort": "high"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func withCodex(t *testing.T, f fixture) {
	t.Helper()
	f.s.Config.Harnesses[HarnessCodex] = HarnessConfig{Executable: "/usr/bin/codex", Version: "0.165.0", ConfigDir: t.TempDir()}
}

func TestInstallBundleMaterializesForEachHarnessAndLeavesThePin(t *testing.T) {
	f := testFixture(t)
	withCodex(t, f)
	ctx := context.Background()
	pinned := f.s.Policy()
	b := fixtureBundle(t, bundleRule)
	digest, err := f.s.InstallBundle(ctx, b)
	if err != nil {
		t.Fatal(err)
	}
	if digest != b.Digest() || f.s.Policy() != pinned {
		t.Fatal("a bundle moved the host's pin or answered another digest")
	}
	bound, err := f.s.boundTo(digest)
	if err != nil {
		t.Fatal(err)
	}
	if bound == f.s || f.s.coordinatorBundle != nil {
		t.Fatal("binding changed the shared server")
	}
	rules, err := os.ReadFile(filepath.Clean(bound.rulesFile()))
	if err != nil || !strings.HasPrefix(string(rules), string(f.s.Rules)) || !strings.Contains(string(rules), bundleRule) || !strings.Contains(string(rules), "skills/review/SKILL.md") {
		t.Fatal("claude rules miss the bundle", err)
	}
	home := bound.codexHomeDir()
	config, err := os.ReadFile(filepath.Join(home, "config.toml")) //nolint:gosec // managed home of the test's own server
	if err != nil || !strings.Contains(string(config), `model_reasoning_effort = "high"`) || !strings.Contains(string(config), "hooks = true") || !strings.Contains(string(config), "plugins = false") {
		t.Fatal("supported setting or security feature missing", err)
	}
	if _, err := os.Stat(filepath.Join(home, "skills/review/SKILL.md")); err != nil {
		t.Fatal(err)
	}
	hooks, err := os.ReadFile(filepath.Join(home, hooksJSONFile)) //nolint:gosec // managed home of the test's own server
	if err != nil || !strings.Contains(string(hooks), "execution-guard") {
		t.Fatal("host guard missing", err)
	}
	if target, err := os.Readlink(filepath.Join(home, "auth.json")); err != nil || !strings.HasSuffix(target, "auth.json") {
		t.Fatal("login is not linked", err)
	}
	if again, err := f.s.InstallBundle(ctx, b); err != nil || again != digest {
		t.Fatal("repeating an install is not idempotent", err)
	}
}

func TestStartBindsItsBundleAndLaterBundlesLeaveItAlone(t *testing.T) {
	f := testFixture(t)
	ctx := context.Background()
	first := fixtureBundle(t, "First rule.")
	second := fixtureBundle(t, "Second rule.")
	f.r.BundleDigest = first.Digest()
	if _, err := f.s.Start(ctx, f.r); err == nil || !strings.Contains(err.Error(), "not installed") {
		t.Fatalf("a start ran on a bundle the host lacks: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(f.s.Root, string(f.r.Ref.Execution)+".json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a refused start left a journal", err)
	}
	if _, err := f.s.InstallBundle(ctx, first); err != nil {
		t.Fatal(err)
	}
	launched(t, f)
	// The coordinator moves on while this execution is admitted and parked.
	if _, err := f.s.InstallBundle(ctx, second); err != nil {
		t.Fatal(err)
	}
	j, err := f.s.Read(f.r.Ref)
	if err != nil {
		t.Fatal(err)
	}
	bound, err := f.s.boundTo(j.Request.BundleDigest)
	if err != nil {
		t.Fatal(err)
	}
	rules, err := os.ReadFile(filepath.Clean(bound.rulesFile()))
	if err != nil || !strings.Contains(string(rules), "First rule.") || strings.Contains(string(rules), "Second rule.") {
		t.Fatal("a parked execution lost the policy it was admitted under", err)
	}
	next, err := f.s.boundTo(second.Digest())
	if err != nil {
		t.Fatal(err)
	}
	rules, err = os.ReadFile(filepath.Clean(next.rulesFile()))
	if err != nil || !strings.Contains(string(rules), "Second rule.") {
		t.Fatal("the next start misses the change", err)
	}
}

func TestInterruptedInstallIsInvisibleAndRepeats(t *testing.T) {
	f := testFixture(t)
	ctx := context.Background()
	b := fixtureBundle(t, bundleRule)
	stage := f.s.bundleDir(b.Digest()) + bundleStaging
	if err := os.MkdirAll(stage, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage, claudeRulesFile), []byte("half"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.boundTo(b.Digest()); err == nil {
		t.Fatal("a partial bundle admitted a start")
	}
	st, err := f.s.BundleStatus(b.Digest())
	if err != nil || st.Installed || st.Problem != "" || st.Latest != "" {
		t.Fatalf("partial bundle reported: %+v %v", st, err)
	}
	if _, err := f.s.InstallBundle(ctx, b); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(stage); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("stale staging survived", err)
	}
	if _, err := f.s.boundTo(b.Digest()); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentInstallsNeverShowAPartialBundle(t *testing.T) {
	f := testFixture(t)
	ctx := context.Background()
	b := fixtureBundle(t, bundleRule)
	var wg sync.WaitGroup
	errs := make([]error, 6)
	for i := range errs {
		wg.Go(func() {
			_, errs[i] = f.s.InstallBundle(ctx, b)
			if errs[i] == nil {
				_, errs[i] = f.s.boundTo(b.Digest())
			}
		})
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		t.Fatal(err)
	}
}

func TestTamperedBundleRefusesStartAndIsNotRepaired(t *testing.T) {
	for _, name := range []string{claudeRulesFile, bundleManifest} {
		t.Run(name, func(t *testing.T) {
			f := testFixture(t)
			ctx := context.Background()
			b := fixtureBundle(t, bundleRule)
			if _, err := f.s.InstallBundle(ctx, b); err != nil {
				t.Fatal(err)
			}
			file := filepath.Join(f.s.bundleDir(b.Digest()), name)
			if err := os.WriteFile(file, []byte(`{"schema":1}`), 0o600); err != nil {
				t.Fatal(err)
			}
			f.r.BundleDigest = b.Digest()
			if _, err := f.s.Start(ctx, f.r); err == nil {
				t.Fatal("started on a tampered bundle")
			}
			st, err := f.s.BundleStatus(b.Digest())
			if err != nil || st.Installed || st.Problem == "" {
				t.Fatalf("tampering not reported: %+v %v", st, err)
			}
			if _, err := SyncCurrentBundle(ctx, fakeSyncer{s: f.s}, b); err == nil {
				t.Fatal("sync admitted a tampered bundle")
			}
			got, err := os.ReadFile(filepath.Clean(file))
			if err != nil || string(got) != `{"schema":1}` {
				t.Fatal("tampered evidence rewritten", err)
			}
		})
	}
}

func TestInstallRefusesSymlinkedBundleDirectory(t *testing.T) {
	f := testFixture(t)
	ctx := context.Background()
	b := fixtureBundle(t, bundleRule)
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(f.s.Root, bundlesDir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, f.s.bundleDir(b.Digest())); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.InstallBundle(ctx, b); err == nil {
		t.Fatal("installed through a symlink")
	}
	if entries, err := os.ReadDir(outside); err != nil || len(entries) != 0 {
		t.Fatal("wrote outside the managed directory", err)
	}
}

// A prepared release changes the host's rules under a bundle a parked worker is
// bound to: its bundle follows, in the same directory, and keeps the worker's files.
func TestPrepareRefreshesBundlesForNewHostRules(t *testing.T) {
	f := testFixture(t)
	withCodex(t, f)
	ctx := context.Background()
	b := fixtureBundle(t, bundleRule)
	if _, err := f.s.InstallBundle(ctx, b); err != nil {
		t.Fatal(err)
	}
	bound, err := f.s.boundTo(b.Digest())
	if err != nil {
		t.Fatal(err)
	}
	thread := filepath.Join(bound.codexHomeDir(), "sessions", "thread.jsonl")
	if err := os.MkdirAll(filepath.Dir(thread), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(thread, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.s.Rules = append(f.s.Rules, []byte("\nRules of the next release.")...)
	if _, err := f.s.boundTo(b.Digest()); err == nil {
		t.Fatal("stale bundle passed before its prepare")
	}
	if _, err := f.s.Prepare(f.s.Policy()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.boundTo(b.Digest()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(thread); err != nil {
		t.Fatal("prepare lost a parked thread", err)
	}
}

func TestSyncPrunesOnlyExpiredUnboundBundles(t *testing.T) {
	f := testFixture(t)
	ctx := context.Background()
	bound, expired, fresh := fixtureBundle(t, "Bound."), fixtureBundle(t, "Expired."), fixtureBundle(t, "Fresh.")
	for _, b := range []CoordinatorBundle{bound, expired, fresh} {
		if _, err := f.s.InstallBundle(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	f.r.BundleDigest = bound.Digest()
	launched(t, f)
	old := f.s.now().Add(-2 * DefaultRetention)
	for _, b := range []CoordinatorBundle{bound, expired} {
		if err := os.Chtimes(f.s.bundleDir(b.Digest()), old, old); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.s.InstallBundle(ctx, fixtureBundle(t, "Current.")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(f.s.bundleDir(expired.Digest())); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("an expired unbound bundle stayed", err)
	}
	for _, b := range []CoordinatorBundle{bound, fresh} {
		if _, err := f.s.boundTo(b.Digest()); err != nil {
			t.Fatal("a bundle in use or in retention went", err)
		}
	}
}

// seedBundles fills the host to its bound with bundles only as old as age.
func seedBundles(t *testing.T, f fixture, age time.Duration) {
	t.Helper()
	base := fixtureBundle(t, bundleRule)
	at := f.s.now().Add(-age)
	for i := range maxInstalledBundles {
		digest := Digest(fmt.Appendf(nil, "seed %d", i))
		dir := f.s.bundleDir(digest)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, bundleManifest), mustJSON(base), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(dir, at, at); err != nil {
			t.Fatal(err)
		}
	}
}

// A full host still answers, prepares and takes a new bundle: no retained
// inventory may leave it needing an operator.
func TestBundleBoundMakesRoomBeforePublishing(t *testing.T) {
	f := testFixture(t)
	ctx := context.Background()
	seedBundles(t, f, time.Hour)
	stale := f.s.bundleDir(Digest([]byte("interrupted"))) + bundleStaging
	if err := os.MkdirAll(stale, 0o700); err != nil {
		t.Fatal(err)
	}
	b := fixtureBundle(t, "Next rule.")
	if _, err := f.s.BundleStatus(b.Digest()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.Prepare(f.s.Policy()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.InstallBundle(ctx, b); err != nil {
		t.Fatal(err)
	}
	digests, err := f.s.installedBundles()
	if err != nil || len(digests) > maxInstalledBundles {
		t.Fatalf("host passed its bound: %d %v", len(digests), err)
	}
	if _, err := os.Lstat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("an interrupted install still counts", err)
	}
	if _, err := f.s.boundTo(b.Digest()); err != nil {
		t.Fatal(err)
	}
}

func TestBundleBoundRefusesWhenEveryBundleIsBound(t *testing.T) {
	f := testFixture(t)
	ctx := context.Background()
	seedBundles(t, f, time.Hour)
	digests, err := f.s.installedBundles()
	if err != nil {
		t.Fatal(err)
	}
	for _, digest := range digests {
		r := f.r
		r.Ref.Execution = ExecutionID(uuid.NewString())
		r.BundleDigest = digest
		if err := os.WriteFile(filepath.Join(f.s.Root, string(r.Ref.Execution)+".json"), mustJSON(Journal{Request: r}), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	b := fixtureBundle(t, "Next rule.")
	if _, err := f.s.InstallBundle(ctx, b); err == nil || !strings.Contains(err.Error(), "none can go") {
		t.Fatalf("published past the bound: %v", err)
	}
	if _, err := os.Lstat(f.s.bundleDir(b.Digest())); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a refused bundle was published", err)
	}
	if st, err := f.s.BundleStatus(b.Digest()); err != nil || st.Installed {
		t.Fatalf("a full host stopped answering: %+v %v", st, err)
	}
}

func TestPluginTravelsAsDocumentsOnly(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "doc.md"), []byte("Plugin document."), 0o600); err != nil {
		t.Fatal(err)
	}
	build := func(capability []BundleCapability, artifacts ...string) (CoordinatorBundle, error) {
		var src []BundleSource
		for _, a := range artifacts {
			src = append(src, BundleSource{Source: "doc.md", Artifact: a})
		}
		return BuildCoordinatorBundle(root, src, nil, capability, nil)
	}
	required := []BundleCapability{{Name: "plugin:review", Required: true}}
	if _, err := build(required, "rules/desk/AGENTS.md"); err == nil {
		t.Fatal("a required plugin passed without its files")
	}
	for _, refused := range []string{"plugins/review/hooks/hooks.json", "plugins/review/.mcp.json", "plugins/review/commands/run.sh", "plugins/review/skills/check/run.sh"} {
		if _, err := build(nil, refused); err == nil {
			t.Fatalf("accepted %s", refused)
		}
	}
	b, err := build(required, "plugins/review/commands/check.md", "plugins/review/skills/check/SKILL.md", "plugins/review/skills/check/notes.md")
	if err != nil {
		t.Fatal(err)
	}
	f := testFixture(t)
	withCodex(t, f)
	if _, err := f.s.InstallBundle(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	bound, err := f.s.boundTo(b.Digest())
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"skills/review-check/SKILL.md", "skills/review-check/notes.md"} {
		if _, err := os.Stat(filepath.Join(bound.codexHomeDir(), name)); err != nil {
			t.Fatal(err)
		}
	}
	rules, err := os.ReadFile(filepath.Join(bound.codexHomeDir(), "AGENTS.md")) //nolint:gosec // managed home of the test's own server
	if err != nil || !strings.Contains(string(rules), "Plugin document.") {
		t.Fatal("a plugin command is missing from codex's rules", err)
	}
	claude, err := os.ReadFile(filepath.Clean(bound.rulesFile()))
	if err != nil || strings.Count(string(claude), "Plugin document.") != 3 {
		t.Fatal("claude rules miss the plugin", err)
	}
}

func TestOwnedHomeHoldsWhatAWorkerWritesToTilde(t *testing.T) {
	f := testFixture(t)
	account := t.TempDir()
	if err := os.WriteFile(filepath.Join(account, ".gitconfig"), []byte("[user]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := launched(t, f)
	j := Journal{Handle: h, Request: f.r}
	j.Request.Harness = HarnessCodex
	value := func(env []string, key string) string {
		for _, kv := range env {
			if v, ok := strings.CutPrefix(kv, key+"="); ok {
				return v
			}
		}
		return ""
	}
	env, err := f.s.ownedEnvironment(j, []string{"HOME=" + account})
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(filepath.Dir(h.Workspace), homeDir)
	if value(env, "HOME") != home || value(env, EnvHome) != account {
		t.Fatalf("HOME not routed: %v", env)
	}
	if value(env, "GIT_CONFIG_GLOBAL") != filepath.Join(account, ".gitconfig") || value(env, "XDG_CONFIG_HOME") != filepath.Join(account, ".config") {
		t.Fatalf("account configuration out of reach: %v", env)
	}
	if info, err := os.Stat(home); err != nil || !info.IsDir() {
		t.Fatal("owned home missing", err)
	}
	for name, setup := range map[string]func(*Journal, *Server) []string{
		"agy keeps its managed home": func(*Journal, *Server) []string { return []string{"HOME=/managed", EnvHome + "=" + account} },
		"claude without a configDir needs its login": func(j *Journal, s *Server) []string {
			j.Request.Harness = HarnessClaude
			c := s.Config.Harnesses[HarnessClaude]
			c.ConfigDir = ""
			s.Config.Harnesses = map[string]HarnessConfig{HarnessClaude: c}
			return []string{"HOME=" + account}
		},
		"a shared host": func(_ *Journal, s *Server) []string { s.Config.SharedHome = true; return []string{"HOME=" + account} },
	} {
		t.Run(name, func(t *testing.T) {
			s, j := *f.s, j
			given := setup(&j, &s)
			env, err := s.ownedEnvironment(j, given)
			if err != nil {
				t.Fatal(err)
			}
			if value(env, "HOME") != value(given, "HOME") {
				t.Fatalf("HOME moved: %v", env)
			}
		})
	}
}

func TestHomeAuditReportsWhatItCannotList(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	f := testFixture(t)
	state := filepath.Join(home, ".local", "state")
	for i := range homeLeftoverBound + 1 {
		if err := os.MkdirAll(filepath.Join(state, fmt.Sprintf("entry-%d", i)), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if row := auditRows(t, f)[state]; row.State != Unknown || !strings.Contains(row.Reason, "not listed") {
		t.Fatalf("overflow: %+v", row)
	}
	if err := os.Chmod(state, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(state, 0o700) }) //nolint:gosec // restores the fixture directory for its removal
	if os.Getuid() != 0 {
		if row := auditRows(t, f)[state]; row.State != Unknown || !strings.Contains(row.Reason, "unreadable") {
			t.Fatalf("inaccessible: %+v", row)
		}
	}
}

func TestBundleStatusNamesWhatDiffers(t *testing.T) {
	f := testFixture(t)
	ctx := context.Background()
	installed := fixtureBundle(t, "Installed rule.")
	if _, err := f.s.InstallBundle(ctx, installed); err != nil {
		t.Fatal(err)
	}
	expected := fixtureBundle(t, "Changed rule.")
	st, err := f.s.BundleStatus(expected.Digest())
	if err != nil {
		t.Fatal(err)
	}
	r := expected.Compare(st)
	if r.Current || r.Installed != installed.Digest() || r.Expected != expected.Digest() || len(r.Differs) != 1 || r.Differs[0] != "rules/project/AGENTS.md" {
		t.Fatalf("report: %+v", r)
	}
	raw, err := json.Marshal(st)
	if err != nil || strings.Contains(string(raw), "Installed rule.") {
		t.Fatal("status carries content", err)
	}
	st, err = f.s.BundleStatus(installed.Digest())
	if err != nil {
		t.Fatal(err)
	}
	if r := installed.Compare(st); !r.Current || len(r.Differs) != 0 {
		t.Fatalf("report: %+v", r)
	}
}

type fakeSyncer struct {
	s     *Server
	sends *int
	fail  error
}

func (c fakeSyncer) BundleStatus(_ context.Context, digest string) (BundleStatus, error) {
	return c.s.BundleStatus(digest)
}

func (c fakeSyncer) SyncBundle(ctx context.Context, b CoordinatorBundle) (string, error) {
	if c.sends != nil {
		*c.sends++
	}
	if c.fail != nil {
		return "", c.fail
	}
	return c.s.InstallBundle(ctx, b)
}

func TestSyncTransfersOnlyWhatTheHostLacks(t *testing.T) {
	f := testFixture(t)
	ctx := context.Background()
	b := fixtureBundle(t, bundleRule)
	sends := 0
	c := fakeSyncer{s: f.s, sends: &sends}
	for range 2 {
		if digest, err := SyncCurrentBundle(ctx, c, b); err != nil || digest != b.Digest() {
			t.Fatal(digest, err)
		}
	}
	if sends != 1 {
		t.Fatalf("sent %d times", sends)
	}
	lost := errors.New("connection lost")
	if _, err := SyncCurrentBundle(ctx, fakeSyncer{s: f.s, fail: lost}, fixtureBundle(t, "Other.")); !errors.Is(err, lost) {
		t.Fatal("a failed transfer passed", err)
	}
}

func TestBundleOversizeRefusesBeforeTheWire(t *testing.T) {
	b := fixtureBundle(t, bundleRule)
	big := make([]byte, maxArtifactBytes)
	for i := range big {
		big[i] = 'a'
	}
	for i := range 4 {
		name := "skills/review/part" + string(rune('a'+i)) + ".md"
		b.Artifacts = append(b.Artifacts, BundleArtifact{BundleSource: BundleSource{Source: name, Artifact: name}, Digest: Digest(big), Content: big})
	}
	if err := b.Validate(); err == nil {
		t.Fatal("accepted a bundle no wire request carries")
	}
}

func TestAuditAccountsForRetirementAndHomeLeftovers(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	f := testFixture(t)
	ctx := context.Background()
	toolchain := filepath.Join(home, ".local", "state", "go-toolchain")
	if err := os.MkdirAll(toolchain, 0o700); err != nil {
		t.Fatal(err)
	}
	h := launched(t, f)
	f.launcher.states[h.Unit] = unitInactive
	stray := filepath.Join(f.s.Config.WorkspaceRoot, retiredWorkspaces, "not-an-execution-1")
	if err := os.MkdirAll(stray, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(f.s.Config.WorkspaceRoot, retiredWorkspaces, retiredLabel(f.r.Ref)+"-link")
	if err := os.Symlink(home, link); err != nil {
		t.Fatal(err)
	}
	if err := f.s.ReconcileResource(ctx, f.r.Ref, false); err != nil {
		t.Fatal(err)
	}
	rows := auditRows(t, f)
	aside := filepath.Join(f.s.Config.WorkspaceRoot, retiredWorkspaces, retiredLabel(f.r.Ref))
	row := rows[aside]
	if row.State != "retired" || row.Reason != "managed retention" || row.Ref != f.r.Ref || row.EligibleAt.IsZero() {
		t.Fatalf("retired root: %+v", row)
	}
	if _, err := os.Lstat(h.Workspace); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("reconciliation left the workspace in place", err)
	}
	if rows[stray].State != Unknown || rows[link].State != Unknown {
		t.Fatalf("unowned retired entries classified: %+v %+v", rows[stray], rows[link])
	}
	if row := rows[filepath.Join(f.s.Config.WorkspaceRoot, retiredWorkspaces)]; row.State != "shared" {
		t.Fatalf("retired container: %+v", row)
	}
	if rows[toolchain].State != Unknown || !strings.Contains(rows[toolchain].Reason, "home state") {
		t.Fatalf("home leftover: %+v", rows[toolchain])
	}
	if _, err := os.Stat(toolchain); err != nil {
		t.Fatal("audit or reconciliation touched home", err)
	}

	// Past retention the row says so, and reconciliation itself removes it.
	now := f.s.now()
	f.s.Now = func() time.Time { return now.Add(DefaultRetention + time.Hour) }
	if row := auditRows(t, f)[aside]; row.Reason != "past retention; removal pending" {
		t.Fatalf("past retention: %+v", row)
	}
	if err := f.s.ReconcileResource(ctx, f.r.Ref, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(aside); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("disposable retirement stayed past retention", err)
	}
	for _, p := range []string{stray, link, toolchain} {
		if _, err := os.Lstat(p); err != nil {
			t.Fatal("removal reached what it does not own", p, err)
		}
	}
}

func TestAuditKeepsRecoveryWorkAndReportsAStoppedRetirement(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	f := testFixture(t)
	ctx := context.Background()
	h := launched(t, f)
	leaveWork(t, f, h)
	f.launcher.states[h.Unit] = unitInactive
	if err := f.s.ReconcileResource(ctx, f.r.Ref, false); err != nil {
		t.Fatal(err)
	}
	rows := auditRows(t, f)
	aside := filepath.Join(f.s.Config.WorkspaceRoot, retiredWorkspaces, retiredLabel(f.r.Ref))
	if row := rows[aside]; row.Reason != "retained recovery work" || !row.EligibleAt.IsZero() {
		t.Fatalf("recovery: %+v", row)
	}
	bundle := f.s.recoveryBundle(f.r.Ref)
	if row := rows[bundle]; row.Ref != f.r.Ref || row.Reason != "retained recovery work" {
		t.Fatalf("recovery bundle: %+v", row)
	}
	now := f.s.now()
	f.s.Now = func() time.Time { return now.Add(365 * 24 * time.Hour) }
	if err := f.s.ReconcileResource(ctx, f.r.Ref, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(aside, "repository.git")); err != nil {
		t.Fatal("retention erased unpublished work", err)
	}
	if err := f.s.update(ctx, f.r.Ref, func(j *Journal) error {
		j.Retirement.Stage, j.Retirement.LastError = RetirePreserved, "moved halfway"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if row := auditRows(t, f)[aside]; row.State != "terminal" || !strings.Contains(row.Reason, "moved halfway") || !strings.Contains(row.Retry, "reconcile") {
		t.Fatalf("stopped retirement: %+v", row)
	}
}

func auditRows(t *testing.T, f fixture) map[string]ResourceFinding {
	t.Helper()
	rows, err := f.s.AuditResources(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]ResourceFinding{}
	for _, r := range rows {
		got[r.Path] = r
	}
	return got
}
