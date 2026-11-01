package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/state"
)

const publicationClaudeVersion = "2.1.291"
const publicationExpiredLease = "expired publication lease"

type publicationRig struct {
	a                       *app
	request                 execution.StartRequest
	workspace, remote, base string
	created                 bool
	creates                 int
	dropCreate              bool
}

func newPublicationRig(t *testing.T) *publicationRig {
	t.Helper()
	a, r := executionTestApp(t)
	root, rootErr := filepath.EvalSymlinks(t.TempDir())
	if rootErr != nil {
		t.Fatal(rootErr)
	}
	rig := &publicationRig{a: a, request: r, workspace: filepath.Join(root, "worker"), remote: filepath.Join(root, "upstream.git")}
	git := func(dir string, args ...string) string {
		out, err := executionPublishGit(context.Background(), dir, args...)
		if err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
		return strings.TrimSpace(string(out))
	}
	git(root, "init", "--bare", rig.remote)
	git(root, "init", "-b", "main", rig.workspace)
	git(rig.workspace, "config", "user.email", "fixture@example.invalid")
	git(rig.workspace, "config", "user.name", "Fixture")
	if err := os.WriteFile(filepath.Join(rig.workspace, "base.txt"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(rig.workspace, "add", "base.txt")
	git(rig.workspace, "commit", "-m", "base")
	rig.base = git(rig.workspace, "rev-parse", "HEAD")
	git(rig.workspace, "push", rig.remote, "main")
	r.LeaseRequired = true
	rig.request = r
	a.cfg.GitHub.AllowedOrgs = []string{defaultOrg}
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		record := st.Starts[0].Execution
		record.Request = r
		record.Handle.Commit = rig.base
		record.Lease = &execution.LeaseGrant{ExpiresAt: time.Now().Add(time.Hour)}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	originalGit, originalGH := executionPublishGit, executionPublishGH
	t.Cleanup(func() { executionPublishGit, executionPublishGH = originalGit, originalGH })
	executionPublishGit = func(ctx context.Context, dir string, args ...string) ([]byte, error) {
		copied := append([]string(nil), args...)
		for i, arg := range copied {
			if arg == "https://github.com/"+r.Repository+".git" {
				copied[i] = rig.remote
			}
		}
		return originalGit(ctx, dir, copied...)
	}
	executionPublishGH = func(_ context.Context, args ...string) ([]byte, error) {
		switch strings.Join(args[:2], " ") {
		case "repo view":
			return []byte(`{"defaultBranchRef":{"name":"main"}}`), nil
		case "pr list":
			if rig.created {
				return []byte(`[{"url":"https://github.com/giantswarm/beekeeper/pull/42","state":"OPEN"}]`), nil
			}
			return []byte(`[]`), nil
		case "pr create":
			rig.created = true
			rig.creates++
			if rig.dropCreate {
				return nil, errors.New("fixture lost creation reply")
			}
			return []byte("https://github.com/giantswarm/beekeeper/pull/42\n"), nil
		default:
			t.Fatalf("unexpected GitHub write: %v", args)
			return nil, nil
		}
	}
	return rig
}

func (rig *publicationRig) commit(t *testing.T, file, text string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(rig.workspace, file), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{loopAdd, file}, {"commit", "-m", "fixture change"}} {
		if _, err := executionPublishGit(context.Background(), rig.workspace, args...); err != nil {
			t.Fatal(err)
		}
	}
	head, err := publishGit(context.Background(), rig.workspace, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	return head
}
func (rig *publicationRig) publication(t *testing.T, head string) executionPublication {
	t.Helper()
	path := filepath.Join(t.TempDir(), "changes.bundle")
	if _, err := publishGit(context.Background(), rig.workspace, "bundle", "create", path, "HEAD", "^"+rig.base); err != nil {
		t.Fatal(err)
	}
	data, err := readPublicationBundle(path)
	if err != nil {
		t.Fatal(err)
	}
	digest := execution.Digest(data)
	for offset := 0; offset < len(data); offset += publicationChunkSize {
		chunk := publicationChunk{Digest: digest, Offset: int64(offset), Total: int64(len(data)), Data: data[offset:min(offset+publicationChunkSize, len(data))]}
		var result publicationChunk
		if err := coordinatorTestCall(t, rig.a, rig.request, publicationTransfer, chunk, &result, false); err != nil {
			t.Fatal(err)
		}
	}
	return executionPublication{Head: head, BundleDigest: digest, Title: "fix: fixture change", Body: "Resolve the fixture defect."}
}

func TestExecutionPublicationPushCreateRecoverAndReviewFix(t *testing.T) {
	rig := newPublicationRig(t)
	head := rig.commit(t, "change.txt", "one\n")
	p := rig.publication(t, head)
	var result executionPublicationResult
	rig.dropCreate = true
	if err := coordinatorTestCall(t, rig.a, rig.request, executionPublish, p, &result, false); err == nil {
		t.Fatal("lost create response reported success")
	}
	if err := coordinatorTestCall(t, rig.a, rig.request, executionPublish, p, &result, true); err == nil {
		t.Fatal("lost callback response reported success")
	}
	if err := coordinatorTestCall(t, rig.a, rig.request, executionPublish, p, &result, false); err != nil {
		t.Fatal(err)
	}
	if rig.creates != 1 || result.URL != "https://github.com/giantswarm/beekeeper/pull/42" || result.Branch != executionBranch(rig.request.Ref) {
		t.Fatal("creation retry duplicated PR or lost ownership", result)
	}
	head = rig.commit(t, "change.txt", "review fix\n")
	p = rig.publication(t, head)
	p.Title, p.Body = "", ""
	if err := coordinatorTestCall(t, rig.a, rig.request, executionPublish, p, &result, false); err != nil {
		t.Fatal(err)
	}
	actual, err := publishGit(context.Background(), rig.remote, "rev-parse", "refs/heads/"+result.Branch)
	if err != nil || actual != head || rig.creates != 1 {
		t.Fatal("review fix did not update owned branch", actual, err)
	}
	// Rewind only the local fixture; the coordinator must refuse its push.
	if _, err := publishGit(context.Background(), rig.workspace, "reset", "--hard", rig.base); err != nil {
		t.Fatal(err)
	}
	head = rig.commit(t, "different.txt", "divergent\n")
	p = rig.publication(t, head)
	if err := coordinatorTestCall(t, rig.a, rig.request, executionPublish, p, &result, false); err == nil {
		t.Fatal("non-fast-forward publication accepted")
	}
}

func TestExecutionPublicationSyncBaseBundleAndMerge(t *testing.T) {
	rig := newPublicationRig(t)
	rig.commit(t, "upstream.txt", "upstream change\n")
	if _, err := publishGit(context.Background(), rig.workspace, "push", rig.remote, "HEAD:main"); err != nil {
		t.Fatal(err)
	}
	if _, err := publishGit(context.Background(), rig.workspace, "reset", "--hard", rig.base); err != nil {
		t.Fatal(err)
	}
	rig.commit(t, "worker.txt", "worker change\n")
	var result executionPublicationResult
	if err := coordinatorTestCall(t, rig.a, rig.request, executionPublish, executionPublication{Head: rig.base, SyncBase: true}, &result, false); err != nil {
		t.Fatal(err)
	}
	var data bytes.Buffer
	for offset := int64(0); offset < result.BundleSize; {
		var chunk publicationChunk
		if err := coordinatorTestCall(t, rig.a, rig.request, publicationTransfer, publicationChunk{Digest: result.BundleDigest, Total: result.BundleSize, Offset: offset, Download: true}, &chunk, false); err != nil {
			t.Fatal(err)
		}
		if len(chunk.Data) == 0 {
			t.Fatal("empty download")
		}
		data.Write(chunk.Data)
		offset += int64(len(chunk.Data))
	}
	path := filepath.Join(t.TempDir(), "base.bundle")
	if err := os.WriteFile(path, data.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"fetch", path, "refs/heads/base"}, {"merge", "--no-edit", "FETCH_HEAD"}} {
		if _, err := publishGit(context.Background(), rig.workspace, args...); err != nil {
			t.Fatal(err)
		}
	}
	head, err := publishGit(context.Background(), rig.workspace, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	p := rig.publication(t, head)
	if err := coordinatorTestCall(t, rig.a, rig.request, executionPublish, p, &result, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(rig.workspace, "upstream.txt")); err != nil {
		t.Fatal("upstream change missing", err)
	}
}

func TestExecutionPublicationRefusesRevokedAndForgedAuthority(t *testing.T) {
	for _, kind := range []string{bgStopCmd, "completed", publicationExpiredLease, "missing-lease", "allowlist", "generation", "forged-proof", "github-hold"} {
		t.Run(kind, func(t *testing.T) {
			rig := newPublicationRig(t)
			head := rig.commit(t, "change.txt", "one\n")
			p := rig.publication(t, head)
			if err := rig.a.store.Update(func(st *state.State) ([]state.Event, error) {
				r := st.Starts[0].Execution
				switch kind {
				case bgStopCmd:
					st.Stop = &state.Stop{Reason: "publication authority fixture"}
				case "completed":
					r.Completed = true
				case publicationExpiredLease:
					r.Lease.ExpiresAt = time.Now().Add(-time.Minute)
				case "missing-lease":
					r.Lease = nil
				case "github-hold":
					st.Holds = append(st.Holds, state.Hold{Target: holdGitHub})
				}
				return nil, nil
			}); err != nil {
				t.Fatal(err)
			}
			request := rig.request
			switch kind {
			case "allowlist":
				rig.a.cfg.GitHub.AllowedOrgs = nil
			case "generation":
				request.Ref.Generation++
			case "forged-proof":
				request.ControlKey, _ = execution.NewControlKey()
			}
			var result executionPublicationResult
			if err := coordinatorTestCall(t, rig.a, request, executionPublish, p, &result, false); err == nil {
				t.Fatal("unauthorized publication accepted")
			}
			if rig.creates != 0 {
				t.Fatal("unauthorized GitHub mutation")
			}
		})
	}
}

func TestExecutionPublicationTransfersAreBoundedAndIdempotent(t *testing.T) {
	rig := newPublicationRig(t)
	data := bytes.Repeat([]byte("fixture"), publicationChunkSize)
	digest := execution.Digest(data)
	p := publicationChunk{Digest: digest, Total: int64(len(data)), Data: data[:publicationChunkSize]}
	var result publicationChunk
	if err := coordinatorTestCall(t, rig.a, rig.request, publicationTransfer, p, &result, true); err == nil {
		t.Fatal("lost upload reply reported success")
	}
	if err := coordinatorTestCall(t, rig.a, rig.request, publicationTransfer, p, &result, false); err != nil {
		t.Fatal(err)
	}
	p.Offset = int64(publicationChunkSize)
	p.Data = data[publicationChunkSize : 2*publicationChunkSize]
	if err := coordinatorTestCall(t, rig.a, rig.request, publicationTransfer, p, &result, false); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*publicationChunk){
		func(p *publicationChunk) { p.Total = publishBundleLimit + 1 },
		func(p *publicationChunk) { p.Offset = -1 },
		func(p *publicationChunk) { p.Data = bytes.Repeat([]byte("x"), publicationChunkSize+1) },
		func(p *publicationChunk) { p.Digest = "../../outside" },
		func(p *publicationChunk) { p.Data = bytes.Repeat([]byte("x"), len(p.Data)) },
	} {
		bad := p
		change(&bad)
		if err := coordinatorTestCall(t, rig.a, rig.request, publicationTransfer, bad, &result, false); err == nil {
			t.Fatal("invalid chunk accepted")
		}
	}
	// The digest binds the entire file, not just the acknowledged prefix.
	if _, err := rig.a.loadPublicationTransfer(rig.request.Ref, digest); err == nil {
		t.Fatal("partial upload accepted")
	}
}

func TestExecutionPublicationRejectsUnrelatedOrMislabeledBundle(t *testing.T) {
	rig := newPublicationRig(t)
	head := rig.commit(t, "change.txt", "one\n")
	p := rig.publication(t, head)
	p.Head = strings.Repeat("a", 40)
	var result executionPublicationResult
	if err := coordinatorTestCall(t, rig.a, rig.request, executionPublish, p, &result, false); err == nil {
		t.Fatal("mislabeled bundle accepted")
	}
	p = rig.publication(t, head)
	p.Body = "Please ask @fixture to check this."
	if err := coordinatorTestCall(t, rig.a, rig.request, executionPublish, p, &result, false); err == nil {
		t.Fatal("mention guard bypassed")
	}
	if rig.created {
		t.Fatal("refused content published")
	}
}

// Ensure chunk envelopes remain inside the existing authenticated wire bound.
func TestExecutionPublicationChunkFitsWire(t *testing.T) {
	raw, err := json.Marshal(publicationChunk{Digest: execution.Digest(nil), Total: publishBundleLimit, Data: make([]byte, publicationChunkSize)})
	if err != nil || len(raw) > execution.MaxWire/2 {
		t.Fatal("chunk exceeds callback bound", len(raw), err)
	}
}

func TestExecutionPublicationAppliesOutboundCommitRules(t *testing.T) {
	rig := newPublicationRig(t)
	rig.a.cfg.Outbound.Phrases = []string{"publication-fixture-private"}
	head := rig.commit(t, "change.txt", "publication-fixture-private\n")
	p := rig.publication(t, head)
	var result executionPublicationResult
	if err := coordinatorTestCall(t, rig.a, rig.request, executionPublish, p, &result, false); err == nil {
		t.Fatal("commit content bypassed outbound rules")
	}
	if rig.created {
		t.Fatal("private commit content was published")
	}
}

func TestExecutionPublicationCheckRejectsOlderHost(t *testing.T) {
	a, r := executionTestApp(t)
	host := a.cfg.Agents.Hosts[sshTestSelector]
	a.executionProbe = func(context.Context, execution.Host) (execution.Capabilities, error) {
		return execution.Capabilities{Protocol: execution.Protocol, Identity: r.Ref.Host, PolicyDigest: host.PolicyDigest, Harnesses: map[string]string{execution.HarnessClaude: publicationClaudeVersion}, Resources: execution.Resources{ObservedAt: time.Now(), AvailableMiB: 1 << 16}}, nil
	}
	if _, err := a.probeExecution(context.Background(), host); err == nil || !strings.Contains(err.Error(), "publication") {
		t.Fatal("older host accepted", err)
	}
}

func TestExecutionPublicationOwnedHostCommand(t *testing.T) {
	rig := newPublicationRig(t)
	rig.commit(t, "change.txt", "host command\n")
	root, rootErr := filepath.EvalSymlinks(t.TempDir())
	if rootErr != nil {
		t.Fatal(rootErr)
	}
	key, err := execution.SigningKey(filepath.Join(root, "key"))
	if err != nil {
		t.Fatal(err)
	}
	endpoint := execution.Endpoint{SSH: "publication-coordinator.example", Executable: sshTestExecutable, Config: sshTestConfig}
	s := &execution.Server{Root: filepath.Join(root, "execution"), Rules: []byte("fixture rules"), Version: "publication-test", Executable: sshTestExecutable, ConfigPath: sshTestConfig, Config: execution.HostConfig{Identity: rig.request.Ref.Host, RulesFile: filepath.Join(root, "rules.md"), WorkspaceRoot: filepath.Join(root, "workspaces"), Repositories: map[string]string{rig.request.Repository: filepath.Join(root, "source")}, Harnesses: map[string]execution.HarnessConfig{execution.HarnessClaude: {Executable: "/usr/local/bin/claude", Version: publicationClaudeVersion}}, Coordinators: map[string]execution.Coordinator{rig.request.Coordinator: {PublicKey: execution.PublicKey(key), Callback: endpoint}}, MaxBusy: 1}}
	digest, err := s.Prepare(s.Policy())
	if err != nil {
		t.Fatal(err)
	}
	rig.request.PolicyDigest = digest
	j := execution.Journal{Request: rig.request, Digest: rig.request.Digest(), Handle: execution.Handle{Ref: rig.request.Ref, Unit: execution.UnitName(rig.request.Ref), ConversationID: rig.request.ConversationID, Workspace: rig.workspace, Commit: rig.base}}
	raw, err := json.Marshal(j)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.Root, string(j.Handle.Ref.Execution)+".json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	s.OwnUnit = func() string { return j.Handle.Unit }
	s.Callback = func(ctx context.Context, _ []string, raw []byte) ([]byte, error) {
		var out bytes.Buffer
		err := rig.a.serveExecutionCoordinator(ctx, string(j.Handle.Ref.Host), bytes.NewReader(raw), &out)
		return out.Bytes(), err
	}
	rig.a.executionRuntime = func() (*execution.Server, error) { return s, nil }
	t.Setenv(execution.EnvID, string(j.Handle.Ref.Execution))
	scratch := filepath.Join(rig.workspace, ".beekeeper-scratch")
	if err := os.MkdirAll(scratch, 0o700); err != nil {
		t.Fatal(err)
	}
	bodyPath := filepath.Join(scratch, "pr.md")
	if err := os.WriteFile(bodyPath, []byte("Resolve the fixture defect."), 0o600); err != nil {
		t.Fatal(err)
	}
	command := rig.a.executionPublishCmd()
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetArgs([]string{"--title", "fix: fixture change", "--body-file", bodyPath})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	var result executionPublicationResult
	if json.Unmarshal(output.Bytes(), &result) != nil || result.URL == "" || rig.creates != 1 {
		t.Fatal("host did not publish through coordinator", output.String())
	}
	// Model a normal host whose worker commits use only global identity.
	if _, err := publishGit(context.Background(), rig.workspace, "checkout", "-b", "publication-upstream", rig.base); err != nil {
		t.Fatal(err)
	}
	rig.commit(t, "upstream-identity.txt", "upstream identity fixture\n")
	if _, err := publishGit(context.Background(), rig.workspace, "push", rig.remote, "HEAD:main"); err != nil {
		t.Fatal(err)
	}
	if _, err := publishGit(context.Background(), rig.workspace, "checkout", "main"); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"user.name", "user.email"} {
		if _, err := publishGit(context.Background(), rig.workspace, "config", "--unset", key); err != nil {
			t.Fatal(err)
		}
	}
	const fixtureName = "Global Publication Fixture"
	const fixtureEmail = "publication-global@example.invalid"
	globalPath := filepath.Join(root, "global.gitconfig")
	if err := os.WriteFile(globalPath, []byte(fmt.Sprintf("[user]\n name = %s\n email = %s\n", fixtureName, fixtureEmail)), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", globalPath)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	// The shared test harness supplies user.* through environment config.
	// Remove those overrides so this fixture exercises global configuration.
	t.Setenv("GIT_CONFIG_COUNT", "0")
	t.Setenv("GIT_CONFIG_PARAMETERS", "")
	for _, key := range []string{"GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL", "GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL"} {
		t.Setenv(key, "")
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
	}
	normalCommit := exec.CommandContext(context.Background(), "git", "-c", "core.hooksPath="+os.DevNull, "commit", "--allow-empty", "-m", "normal worker global identity") //nolint:gosec // fixed fixture-only commit arguments and platform null hooks path; no shell
	normalCommit.Dir = rig.workspace
	if err := normalCommit.Run(); err != nil {
		t.Fatal("normal global-identity commit failed", err)
	}
	normalName, normalNameErr := publishGit(context.Background(), rig.workspace, "log", "-1", "--format=%cn")
	normalEmail, normalEmailErr := publishGit(context.Background(), rig.workspace, "log", "-1", "--format=%ce")
	if normalNameErr != nil || normalEmailErr != nil || normalName != fixtureName || normalEmail != fixtureEmail {
		t.Fatal("normal fixture commit must use the configured global identity")
	}
	command = rig.a.executionPublishCmd()
	command.SetArgs([]string{"--sync-base"})
	command.SetOut(&bytes.Buffer{})
	if err := command.Execute(); err != nil {
		t.Fatal("global-identity base synchronization failed", err)
	}
	parents, err := publishGit(context.Background(), rig.workspace, "rev-list", "--parents", "-n", "1", "HEAD")
	if err != nil || len(strings.Fields(parents)) != 3 {
		t.Fatal("base synchronization did not create a divergent merge", err)
	}
	name, nameErr := publishGit(context.Background(), rig.workspace, "log", "-1", "--format=%cn")
	email, emailErr := publishGit(context.Background(), rig.workspace, "log", "-1", "--format=%ce")
	if nameErr != nil || emailErr != nil || name != fixtureName || email != fixtureEmail {
		t.Fatalf("merge lost fixture identity: nameMatches=%t emailMatches=%t nameLength=%d emailLength=%d errors=%v/%v", name == fixtureName, email == fixtureEmail, len(name), len(email), nameErr, emailErr)
	}
	// A command in an unrelated service cannot impersonate this execution.
	s.OwnUnit = func() string { return "unrelated.service" }
	command = rig.a.executionPublishCmd()
	command.SetArgs(nil)
	if err := command.Execute(); err == nil {
		t.Fatal("unowned process published")
	}
}

func TestExecutionPublicationRechecksLeaseAfterFetch(t *testing.T) {
	rig := newPublicationRig(t)
	head := rig.commit(t, "change.txt", "lease deadline fixture\n")
	p := rig.publication(t, head)
	st, err := rig.a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	gh := executionPublishGH
	executionPublishGH = func(ctx context.Context, args ...string) ([]byte, error) {
		out, err := gh(ctx, args...)
		// Simulate the lease expiring during repository/PR reads, without a sleep
		// or another process racing the coordinating state transaction.
		if len(args) > 1 && strings.Join(args[:2], " ") == "pr list" {
			st.Starts[0].Execution.Lease.ExpiresAt = time.Now().Add(-time.Second)
		}
		return out, err
	}
	if _, err := rig.a.publishExecution(context.Background(), st, rig.request.Ref, p); err == nil {
		t.Fatal("publication wrote after lease expiry")
	}
	if rig.created {
		t.Fatal("expired execution created a PR")
	}
}

func TestExecutionPublicationRejectsBulletMessage(t *testing.T) {
	rig := newPublicationRig(t)
	const sentinel = "publication-protected-bullet-sentinel"
	rig.a.cfg.Outbound.Phrases = []string{sentinel}
	rig.commit(t, "bullet-message.txt", "ordinary fixture content\n")
	if _, err := publishGit(context.Background(), rig.workspace, "commit", "--amend", "-m", "fixture message\n\n- "+sentinel); err != nil {
		t.Fatal(err)
	}
	head, err := publishGit(context.Background(), rig.workspace, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	p := rig.publication(t, head)
	git := executionPublishGit
	pushes := 0
	executionPublishGit = func(ctx context.Context, dir string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "push" {
			pushes++
		}
		return git(ctx, dir, args...)
	}
	var result executionPublicationResult
	err = coordinatorTestCall(t, rig.a, rig.request, executionPublish, p, &result, false)
	if err == nil || pushes != 0 || rig.created {
		t.Fatalf("protected commit-message bullet escaped: rejected=%t pushes=%d created=%t", err != nil, pushes, rig.created)
	}
}
