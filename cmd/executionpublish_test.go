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
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/superloop"
)

const publicationClaudeVersion = "2.1.291"
const publicationExpiredLease = "expired publication lease"
const publicationFixturePush = "push"
const publicationFixtureCreate = "pr create"
const publicationFixtureEdit = "api -X"
const publicationFixtureTitle = "fix: fixture change"
const publicationFixtureBody = "Resolve the fixture defect."
const publicationFixturePRURL = "https://github.com/giantswarm/beekeeper/pull/42"

type publicationRig struct {
	a                       *app
	request                 execution.StartRequest
	workspace, remote, base string
	created                 bool
	creates                 int
	dropCreate              bool
	refuseCreate            error
	title, body             string
	edits                   [][]string
	refuseEdit              error
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
		case "api graphql":
			return inventoryPage(t, nil, false, ""), nil
		case "repo view":
			return []byte(`{"defaultBranchRef":{"name":"main"}}`), nil
		case "pr list":
			if rig.created {
				return json.Marshal([]publicationPull{{URL: publicationFixturePRURL, State: emergencyOpenState, Title: rig.title, Body: rig.body}})
			}
			return []byte(`[]`), nil
		case publicationFixtureEdit:
			rig.edits = append(rig.edits, args)
			return nil, rig.refuseEdit
		case publicationFixtureCreate:
			if rig.refuseCreate != nil {
				return nil, rig.refuseCreate
			}
			rig.created = true
			rig.creates++
			rig.title, rig.body = publicationFixtureTitle, publicationFixtureBody
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
	return executionPublication{Head: head, BundleDigest: digest, Title: publicationFixtureTitle, Body: publicationFixtureBody}
}

func TestExecutionPublicationPushCreateRecoverAndReviewFix(t *testing.T) {
	rig := newPublicationRig(t)
	head := rig.commit(t, "change.txt", "one\n")
	p := rig.publication(t, head)
	var result executionPublicationResult
	rig.dropCreate = true
	if err := coordinatorTestCall(t, rig.a, rig.request, executionPublish, p, &result, false); err != nil {
		t.Fatal("lost create response not settled by the owned branch:", err)
	}
	if result.URL != publicationFixturePRURL {
		t.Fatal("settled creation lost its URL", result)
	}
	if err := coordinatorTestCall(t, rig.a, rig.request, executionPublish, p, &result, true); err == nil {
		t.Fatal("lost callback response reported success")
	}
	if err := coordinatorTestCall(t, rig.a, rig.request, executionPublish, p, &result, false); err != nil {
		t.Fatal(err)
	}
	if rig.creates != 1 || result.URL != publicationFixturePRURL || result.Branch != executionBranch(rig.request.Ref) {
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
	err = coordinatorTestCall(t, rig.a, rig.request, executionPublish, p, &result, false)
	if err == nil || !strings.Contains(err.Error(), "publication Git push failed: the remote branch moved") {
		t.Fatal("non-fast-forward publication accepted or undiagnosed", err)
	}
}

// A create GitHub refuses leaves no PR: the failure says so and why, and the
// identical retry creates it once the cause is gone, never an open "uncertain".
func TestExecutionPublicationNamesARefusedCreateAndRetryCreates(t *testing.T) {
	rig := newPublicationRig(t)
	head := rig.commit(t, "change.txt", "one\n")
	p := rig.publication(t, head)
	var result executionPublicationResult
	const raw = "HTTP 403: fixture-credential at example.invalid/remote-detail"
	rig.refuseCreate = errors.New(raw)
	err := coordinatorTestCall(t, rig.a, rig.request, executionPublish, p, &result, false)
	if err == nil || !strings.Contains(err.Error(), "GitHub created no pull request") || !strings.Contains(err.Error(), "denied the coordinator's credential") {
		t.Fatal("refused create not diagnosed:", err)
	}
	if strings.Contains(err.Error(), "uncertain") || strings.Contains(err.Error(), "fixture-credential") {
		t.Fatal("refused create stayed uncertain or leaked raw output:", err)
	}
	rig.refuseCreate = nil
	if err := coordinatorTestCall(t, rig.a, rig.request, executionPublish, p, &result, false); err != nil {
		t.Fatal(err)
	}
	if rig.creates != 1 || result.URL != publicationFixturePRURL {
		t.Fatal("identical retry did not create the pull request", rig.creates, result)
	}
}

// The attempt's time can run out on the transfer, right before the create: the
// create and the read that settles it keep their own, or every retry ends the
// same way.
func TestExecutionPublicationCreateOutlivesTheAttemptsDeadline(t *testing.T) {
	rig := newPublicationRig(t)
	head := rig.commit(t, "change.txt", "one\n")
	p := rig.publication(t, head)
	original := executionPublishGH
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	executionPublishGH = func(gh context.Context, args ...string) ([]byte, error) {
		if strings.Join(args[:2], " ") == publicationFixtureCreate {
			cancel()
			if gh.Err() != nil {
				return nil, gh.Err()
			}
		}
		return original(gh, args...)
	}
	var result executionPublicationResult
	if err := rig.a.store.Update(func(st *state.State) ([]state.Event, error) {
		var err error
		result, err = rig.a.publishExecution(ctx, st, rig.request.Ref, p)
		return nil, err
	}); err != nil {
		t.Fatal(err)
	}
	if rig.creates != 1 || result.URL != publicationFixturePRURL {
		t.Fatal("create did not run past the attempt's deadline", rig.creates, result)
	}
}

// A lost create reply and a persistently unavailable GraphQL list must
// reconcile through REST, including on an identical republication.
func TestExecutionPublicationReconcilesThroughREST(t *testing.T) {
	rig := newPublicationRig(t)
	p := rig.publication(t, rig.commit(t, "change.txt", "one\n"))
	rig.dropCreate = true
	original := executionPublishGH
	reads := 0
	executionPublishGH = func(ctx context.Context, args ...string) ([]byte, error) {
		if args[0] == "pr" && args[1] == transcriptListCommand {
			return nil, errors.New("GraphQL unavailable")
		}
		if args[0] == "api" && strings.HasPrefix(args[1], "repos/"+rig.request.Repository+"/pulls?") {
			reads++
			if !strings.Contains(args[1], "state=all&per_page=2&head=giantswarm%3A") {
				t.Fatalf("unscoped reconciliation: %v", args)
			}
			if rig.created {
				return json.Marshal([]struct {
					URL   string `json:"html_url"`
					State string `json:"state"`
					Title string `json:"title"`
					Body  string `json:"body"`
				}{{URL: publicationFixturePRURL, State: "open", Title: rig.title, Body: rig.body}})
			}
			return []byte(`[]`), nil
		}
		return original(ctx, args...)
	}
	var result executionPublicationResult
	for range 2 {
		if err := coordinatorTestCall(t, rig.a, rig.request, executionPublish, p, &result, false); err != nil {
			t.Fatal(err)
		}
		if result.URL != publicationFixturePRURL || rig.creates != 1 {
			t.Fatal("publication was not reconciled", result, rig.creates)
		}
	}
	if reads != 3 {
		t.Fatal("expected pre-create, lost reply and retry REST reads", reads)
	}
}

func TestOwnedPullsRESTRejectsUnreadableOrAmbiguousResults(t *testing.T) {
	original := executionPublishGH
	t.Cleanup(func() { executionPublishGH = original })
	for _, body := range []string{`not-json`, `[{},{}]`, `[{}]`, `[{"html_url":"url","state":"unknown"}]`} {
		t.Run(body, func(t *testing.T) {
			executionPublishGH = func(context.Context, ...string) ([]byte, error) { return []byte(body), nil }
			if _, err := ownedPullsREST(t.Context(), "giantswarm/beekeeper", "owned"); err == nil {
				t.Fatal("invalid response treated as a settled outcome")
			}
		})
	}
}

// editNames says the edit's arguments carry a field with this prefix.
func editNames(args []string, prefix string) bool {
	return slices.ContainsFunc(args, func(a string) bool { return strings.HasPrefix(a, prefix) })
}

func TestExecutionPublicationRepublishUpdatesTitleAndBody(t *testing.T) {
	rig := newPublicationRig(t)
	p := rig.publication(t, rig.commit(t, "change.txt", "one\n"))
	var result executionPublicationResult
	if err := coordinatorTestCall(t, rig.a, rig.request, executionPublish, p, &result, false); err != nil {
		t.Fatal(err)
	}
	if len(rig.edits) != 0 || len(result.Updated) != 0 {
		t.Fatal("a first publication edited its pull request", rig.edits, result)
	}

	// An identical retry writes nothing.
	if err := coordinatorTestCall(t, rig.a, rig.request, executionPublish, p, &result, false); err != nil {
		t.Fatal(err)
	}
	if len(rig.edits) != 0 {
		t.Fatal("an identical retry edited its pull request", rig.edits)
	}

	// No flags keep what a reviewer sees.
	p = rig.publication(t, rig.commit(t, "change.txt", "two\n"))
	p.Title, p.Body = "", ""
	if err := coordinatorTestCall(t, rig.a, rig.request, executionPublish, p, &result, false); err != nil {
		t.Fatal(err)
	}
	if len(rig.edits) != 0 {
		t.Fatal("a publication without flags edited its pull request", rig.edits)
	}

	// --body-file alone leaves the title.
	p = rig.publication(t, rig.commit(t, "change.txt", "three\n"))
	p.Title, p.Body = "", "Corrected body."
	if err := coordinatorTestCall(t, rig.a, rig.request, executionPublish, p, &result, false); err != nil {
		t.Fatal(err)
	}
	if len(rig.edits) != 1 || editNames(rig.edits[0], publicationTitle+"=") || !editNames(rig.edits[0], publicationBody+"=@") || !slices.Equal(result.Updated, []string{publicationBody}) {
		t.Fatal("body update not applied alone", rig.edits, result)
	}

	// --title alone fixes a title that fails its check.
	rig.edits, rig.body = nil, p.Body
	p = rig.publication(t, rig.commit(t, "change.txt", "four\n"))
	p.Title, p.Body = "fix: conventional title", ""
	if err := coordinatorTestCall(t, rig.a, rig.request, executionPublish, p, &result, false); err != nil {
		t.Fatal(err)
	}
	if len(rig.edits) != 1 || !slices.Contains(rig.edits[0], publicationTitle+"="+p.Title) || editNames(rig.edits[0], publicationBody+"=@") || !slices.Contains(rig.edits[0], "repos/"+rig.request.Repository+"/pulls/42") || !slices.Equal(result.Updated, []string{publicationTitle}) {
		t.Fatal("title update not applied alone", rig.edits, result)
	}
}

// A lease that runs out during the push stops the pull request's write, and a
// write that starts ends with the lease: only the settling read outlives it.
func TestExecutionPublicationWritesStayWithinTheLease(t *testing.T) {
	for name, tc := range map[string]struct {
		existing bool
		verb     string
	}{"create": {false, publicationFixtureCreate}, "edit": {true, publicationFixtureEdit}} {
		t.Run(name, func(t *testing.T) {
			lease := func(rig *publicationRig, until time.Time) {
				t.Helper()
				if err := rig.a.store.Update(func(st *state.State) ([]state.Event, error) {
					st.Starts[0].Execution.Lease = &execution.LeaseGrant{ExpiresAt: until}
					return nil, nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			publish := func(rig *publicationRig, p executionPublication) error {
				return rig.a.store.Update(func(st *state.State) ([]state.Event, error) {
					_, err := rig.a.publishExecution(t.Context(), st, rig.request.Ref, p)
					return nil, err
				})
			}
			// The lease ends while the push runs: no write starts.
			rig := newPublicationRig(t)
			p := rig.publication(t, rig.commit(t, "change.txt", "one\n"))
			if tc.existing {
				rig.created, rig.title, rig.body = true, publicationFixtureTitle, publicationFixtureBody
				p.Title = "fix: changed title"
			}
			until := time.Now().Add(2 * time.Second)
			lease(rig, until)
			pushing := executionPublishGit
			executionPublishGit = func(ctx context.Context, dir string, args ...string) ([]byte, error) {
				out, err := pushing(ctx, dir, args...)
				if args[0] == publicationFixturePush {
					time.Sleep(time.Until(until) + 10*time.Millisecond)
				}
				return out, err
			}
			if err := publish(rig, p); err == nil || !strings.Contains(err.Error(), "live, leased") {
				t.Fatalf("write past the lease = %v", err)
			}
			if rig.creates != 0 || len(rig.edits) != 0 {
				t.Fatal("a write started after the lease ended", rig.creates, rig.edits)
			}
			executionPublishGit = pushing

			// A write that starts carries the lease as its deadline.
			rig = newPublicationRig(t)
			p = rig.publication(t, rig.commit(t, "change.txt", "two\n"))
			if tc.existing {
				rig.created, rig.title, rig.body = true, publicationFixtureTitle, publicationFixtureBody
				p.Title = "fix: changed title"
			}
			until = time.Now().Add(publishCreateTimeout / 2)
			lease(rig, until)
			fake := executionPublishGH
			var deadline time.Time
			executionPublishGH = func(ctx context.Context, args ...string) ([]byte, error) {
				if strings.Join(args[:2], " ") == tc.verb {
					deadline, _ = ctx.Deadline()
				}
				return fake(ctx, args...)
			}
			if err := publish(rig, p); err != nil {
				t.Fatal(err)
			}
			if deadline.IsZero() || deadline.After(until) {
				t.Fatalf("write deadline %v is past the lease %v", deadline, until)
			}
		})
	}
}

// A body that did not change must say so, not stay silent.
func TestExecutionPublicationNamesAFailedUpdate(t *testing.T) {
	rig := newPublicationRig(t)
	p := rig.publication(t, rig.commit(t, "change.txt", "one\n"))
	var result executionPublicationResult
	if err := coordinatorTestCall(t, rig.a, rig.request, executionPublish, p, &result, false); err != nil {
		t.Fatal(err)
	}
	rig.refuseEdit = errors.New("HTTP 403: fixture-credential at example.invalid/remote-detail")
	p.Title, p.Body = "fix: conventional title", "Corrected body."
	err := coordinatorTestCall(t, rig.a, rig.request, executionPublish, p, &result, false)
	if err == nil || !strings.Contains(err.Error(), "title and body was not updated") || strings.Contains(err.Error(), "fixture-credential") {
		t.Fatal("failed update not named:", err)
	}
	rig.refuseEdit = nil
	if err := coordinatorTestCall(t, rig.a, rig.request, executionPublish, p, &result, false); err != nil {
		t.Fatal("identical retry did not update:", err)
	}
	if !slices.Equal(result.Updated, []string{publicationTitle, publicationBody}) {
		t.Fatal("retry did not update both fields", result)
	}
}

// Detached maintenance outlives the command: it failed fixture cleanups with
// "directory not empty", and would race a transfer's removal the same way.
func TestPublicationGitRunsNoDetachedMaintenance(t *testing.T) {
	dir := t.TempDir()
	if _, err := runPublicationGit(t.Context(), dir, nil, "init", "-q"); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{"maintenance.auto": "false", "gc.auto": "0"} {
		got, err := runPublicationGit(t.Context(), dir, nil, "config", "--get", key)
		if err != nil || strings.TrimSpace(string(got)) != want {
			t.Fatalf("%s = %q, %v, want %s", key, got, err, want)
		}
	}
}

func TestExecutionPublicationDiagnosesWorkflowScopeRejection(t *testing.T) {
	rig := newPublicationRig(t)
	// The remote rejects the push with GitHub's message for a credential
	// without the workflow scope, after the transfer itself succeeded.
	const remoteDetail = "fixture-credential at example.invalid/remote-detail"
	hook := "#!/bin/sh\necho 'remote: " + remoteDetail + "' >&2\necho '! [remote rejected] HEAD -> branch (refusing to allow an OAuth App to create or update workflow `.github/workflows/ci.yaml` without `workflow` scope)' >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(rig.remote, "hooks", "pre-receive"), []byte(hook), 0o700); err != nil { //nolint:gosec // executable fixture hook
		t.Fatal(err)
	}
	head := rig.commit(t, "workflow.txt", "workflow change\n")
	p := rig.publication(t, head)
	var result executionPublicationResult
	err := coordinatorTestCall(t, rig.a, rig.request, executionPublish, p, &result, false)
	if err == nil || rig.created {
		t.Fatal("rejected push reported success or created a PR")
	}
	msg := err.Error()
	if !strings.Contains(msg, "publication Git push failed") || !strings.Contains(msg, "lacks the workflow scope") || !strings.Contains(msg, "authorization barrier") {
		t.Fatal("workflow rejection lost its stage or diagnosis:", msg)
	}
	if strings.Contains(msg, "fixture-credential") || strings.Contains(msg, "example.invalid") || strings.Contains(msg, "remote rejected") {
		t.Fatal("raw Git output leaked into the publication error:", msg)
	}
	evs, readErr := rig.a.store.Events(0, func(e state.Event) bool {
		return e.Verb == "execution.publish-denied" && strings.Contains(e.Detail, "lacks the workflow scope")
	})
	if readErr != nil || len(evs) != 1 {
		t.Fatal("publication denial event lost its diagnosis", readErr, len(evs))
	}
}

func TestPublicationGitFailureNamesOnlyKnownStages(t *testing.T) {
	err := publicationGitFailure(context.Background(), []string{"--upload-pack=/fixture", "x"}, "remote: fatal: Authentication failed for 'https://example.invalid/'")
	if err.Error() != "publication Git operation failed: the coordinator's GitHub credential was not accepted" {
		t.Fatal(err)
	}
	err = publicationGitFailure(context.Background(), []string{gitBundle, gitVerify, "/fixture/path"}, "unexpected text")
	if err.Error() != "publication Git bundle verify failed: unclassified Git failure" {
		t.Fatal(err)
	}
	for _, stderr := range []string{
		"error: Repository lacks these prerequisite commits:\nerror: 39f79705b73f1f29590eb81d1b15f3aa0a1335eb",
		"Fehler: Dem Repository fehlen folgende vorausgesetzte Commits:\nFehler: 39f79705b73f1f29590eb81d1b15f3aa0a1335eb",
	} {
		err = publicationGitFailure(context.Background(), []string{gitBundle, gitVerify, "/fixture/path"}, stderr)
		if !strings.Contains(err.Error(), "lacks prerequisite commits") {
			t.Fatalf("prerequisite failure not diagnosed (%q): %v", stderr, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err = publicationGitFailure(ctx, []string{publicationFixturePush}, ""); !strings.Contains(err.Error(), "ran out of time") {
		t.Fatal(err)
	}
}

// syncBase runs the --sync-base round trip: it downloads the bundle of the
// default branch through the coordinator and merges it into the workspace.
func (rig *publicationRig) syncBase(t *testing.T) string {
	t.Helper()
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
	return head
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
	head := rig.syncBase(t)
	p := rig.publication(t, head)
	var result executionPublicationResult
	if err := coordinatorTestCall(t, rig.a, rig.request, executionPublish, p, &result, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(rig.workspace, "upstream.txt")); err != nil {
		t.Fatal("upstream change missing", err)
	}
}

// A head after --sync-base holds default-branch commits the worker did not
// write. Their content and messages are not the worker's to answer for; the
// worker's own commits on the same head stay under the outbound rules.
func TestExecutionPublicationScanAfterSyncBaseSkipsBaseCommits(t *testing.T) {
	const finding = "publication-fixture-upstream-finding"
	for _, tt := range []struct {
		name, worker string
		refused      bool
	}{
		{name: "upstream finding only", worker: "worker change\n"},
		{name: "worker finding too", worker: finding + "\n", refused: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rig := newPublicationRig(t)
			rig.a.cfg.Outbound.Phrases = []string{finding}
			ctx := context.Background()
			rig.commit(t, "upstream.txt", finding+"\n")
			if _, err := publishGit(ctx, rig.workspace, "commit", "--amend", "-m", "upstream change\n\n"+finding); err != nil {
				t.Fatal(err)
			}
			if _, err := publishGit(ctx, rig.workspace, "push", rig.remote, "HEAD:main"); err != nil {
				t.Fatal(err)
			}
			if _, err := publishGit(ctx, rig.workspace, "reset", "--hard", rig.base); err != nil {
				t.Fatal(err)
			}
			rig.commit(t, "worker.txt", tt.worker)
			p := rig.publication(t, rig.syncBase(t))
			var result executionPublicationResult
			err := coordinatorTestCall(t, rig.a, rig.request, executionPublish, p, &result, false)
			if tt.refused {
				if err == nil || rig.created {
					t.Fatalf("worker finding escaped behind the base: err=%v created=%t", err, rig.created)
				}
				return
			}
			if err != nil || !rig.created {
				t.Fatalf("upstream finding stopped the publication: err=%v created=%t", err, rig.created)
			}
		})
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

// ownedPublicationHost installs a prepared host whose journal owns the rig's
// execution, for host-side publication commands run as that execution.
func ownedPublicationHost(t *testing.T, rig *publicationRig) (s *execution.Server, j execution.Journal, root, bodyPath string) {
	t.Helper()
	root, rootErr := filepath.EvalSymlinks(t.TempDir())
	if rootErr != nil {
		t.Fatal(rootErr)
	}
	key, err := execution.SigningKey(filepath.Join(root, "key"))
	if err != nil {
		t.Fatal(err)
	}
	endpoint := execution.Endpoint{SSH: "publication-coordinator.example", Executable: sshTestExecutable, Config: sshTestConfig}
	s = &execution.Server{Root: filepath.Join(root, "execution"), Rules: []byte("fixture rules"), Version: "publication-test", Executable: sshTestExecutable, ConfigPath: sshTestConfig, Config: execution.HostConfig{Identity: rig.request.Ref.Host, RulesFile: filepath.Join(root, "rules.md"), WorkspaceRoot: filepath.Join(root, "workspaces"), Repositories: map[string]string{rig.request.Repository: filepath.Join(root, "source")}, Harnesses: map[string]execution.HarnessConfig{execution.HarnessClaude: {Executable: "/usr/local/bin/claude", Version: publicationClaudeVersion}}, Coordinators: map[string]execution.Coordinator{rig.request.Coordinator: {PublicKey: execution.PublicKey(key), Callback: endpoint}}, MaxBusy: 1}}
	digest, err := s.Prepare(s.Policy())
	if err != nil {
		t.Fatal(err)
	}
	rig.request.PolicyDigest = digest
	j = execution.Journal{Request: rig.request, Digest: rig.request.Digest(), Handle: execution.Handle{Ref: rig.request.Ref, Unit: execution.UnitName(rig.request.Ref), ConversationID: rig.request.ConversationID, Workspace: rig.workspace, Commit: rig.base}}
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
	bodyPath = filepath.Join(scratch, "pr.md")
	if err := os.WriteFile(bodyPath, []byte("Resolve the fixture defect."), 0o600); err != nil {
		t.Fatal(err)
	}
	return s, j, root, bodyPath
}

func TestExecutionPublicationOwnedHostCommand(t *testing.T) {
	rig := newPublicationRig(t)
	rig.commit(t, "change.txt", "host command\n")
	s, _, root, bodyPath := ownedPublicationHost(t, rig)
	command := rig.a.executionPublishCmd()
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetArgs([]string{titleFlag, "fix: fixture change", "--body-file", bodyPath})
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
	for _, key := range []string{"GIT_AUTHOR_NAME", litGitAuthorEmail, "GIT_COMMITTER_NAME", litGitCommitterEmail} {
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
	message, messageErr := publishGit(context.Background(), rig.workspace, "log", "-1", "--format=%B")
	if messageErr != nil || message != "Merge upstream default branch" {
		t.Fatalf("base synchronization must use a neutral merge message: %q (%v)", message, messageErr)
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

// A host upgrade re-prepares its policy under a running execution that already
// published. Its follow-up keeps failing closed until the execution is rebound
// to the prepared policy, then publishes to the same owned PR.
func TestExecutionPublicationFollowUpAfterHostPolicyRebind(t *testing.T) {
	rig := newPublicationRig(t)
	rig.commit(t, "change.txt", "before the host upgrade\n")
	s, j, _, bodyPath := ownedPublicationHost(t, rig)
	publish := func(args ...string) (executionPublicationResult, error) {
		command := rig.a.executionPublishCmd()
		var output bytes.Buffer
		command.SetOut(&output)
		command.SetArgs(args)
		var result executionPublicationResult
		if err := command.Execute(); err != nil {
			return result, err
		}
		return result, json.Unmarshal(output.Bytes(), &result)
	}
	first, err := publish(titleFlag, "fix: fixture before the host upgrade", "--body-file", bodyPath)
	if err != nil || first.URL == "" {
		t.Fatal("first publication failed", err)
	}
	// The rebind needs a launched execution whose owned unit is active.
	j.Phase = execution.Running
	raw, err := json.Marshal(j)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.Root, string(j.Handle.Ref.Execution)+".json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	s.Launcher = &agyTestLauncher{states: map[string]string{j.Handle.Unit: unitActive}}
	original := j.Request.PolicyDigest
	s.Version = "publication-test-upgraded"
	prepared, err := s.Prepare(s.Policy())
	if err != nil || prepared == original {
		t.Fatal("fixture upgrade did not change the prepared policy", err)
	}
	head := rig.commit(t, "follow-up.txt", "after the host upgrade\n")
	if _, err := publish(); err == nil || !strings.Contains(err.Error(), "agents recover") {
		t.Fatal("follow-up published under a changed policy, or its refusal names no recovery", err)
	}
	if _, err := s.RebindPolicy(context.Background(), j.Handle.Ref, execution.PolicyRebind{From: original, To: prepared}); err != nil {
		t.Fatal(err)
	}
	followUp, err := publish()
	if err != nil || followUp.URL != first.URL || followUp.Head != head || followUp.Branch != first.Branch || rig.creates != 1 {
		t.Fatalf("rebound follow-up did not publish to the owned PR: %+v, %v", followUp, err)
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

func TestExecutionPublicationFromShallowPRWorkspaceAndSyncBase(t *testing.T) {
	rig := newPublicationRig(t)
	ctx := context.Background()
	branch := executionBranch(rig.request.Ref)
	prHead := rig.commit(t, "pr.txt", "initial PR commit\n")
	if _, err := publishGit(ctx, rig.workspace, "push", rig.remote, prHead+":refs/heads/"+branch); err != nil {
		t.Fatal(err)
	}
	if _, err := publishGit(ctx, rig.workspace, "reset", "--hard", rig.base); err != nil {
		t.Fatal(err)
	}
	mainHead := rig.commit(t, "upstream.txt", "upstream change on main\n")
	if _, err := publishGit(ctx, rig.workspace, "push", rig.remote, mainHead+":refs/heads/main"); err != nil {
		t.Fatal(err)
	}
	rig.created = true

	root, rootErr := filepath.EvalSymlinks(t.TempDir())
	if rootErr != nil {
		t.Fatal(rootErr)
	}
	shallowWorkspace := filepath.Join(root, "shallow-worker")
	if _, err := publishGit(ctx, root, "clone", "--depth", "1", "--branch", branch, "file://"+rig.remote, shallowWorkspace); err != nil {
		t.Fatal(err)
	}
	if _, err := publishGit(ctx, shallowWorkspace, "config", "user.email", "fixture@example.invalid"); err != nil {
		t.Fatal(err)
	}
	if _, err := publishGit(ctx, shallowWorkspace, "config", "user.name", "Fixture"); err != nil {
		t.Fatal(err)
	}
	if !isShallowWorkspace(ctx, shallowWorkspace) {
		t.Fatal("expected shallow workspace fixture")
	}
	rig.workspace = shallowWorkspace
	if err := rig.a.store.Update(func(st *state.State) ([]state.Event, error) {
		record := st.Starts[0].Execution
		record.Handle.Commit = prHead
		record.Handle.Workspace = shallowWorkspace
		record.Request.GitRef = prHead
		record.Published = &state.ExecutionPublication{At: time.Now().UTC(), Branch: branch, Head: prHead, URL: publicationFixturePRURL}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}

	key, err := execution.SigningKey(filepath.Join(root, "key"))
	if err != nil {
		t.Fatal(err)
	}
	endpoint := execution.Endpoint{SSH: "publication-coordinator.example", Executable: sshTestExecutable, Config: sshTestConfig}
	s := &execution.Server{Root: filepath.Join(root, "execution"), Rules: []byte("fixture rules"), Version: "publication-test", Executable: sshTestExecutable, ConfigPath: sshTestConfig, Config: execution.HostConfig{Identity: rig.request.Ref.Host, RulesFile: filepath.Join(root, "rules.md"), WorkspaceRoot: filepath.Join(root, "workspaces"), Repositories: map[string]string{rig.request.Repository: filepath.Join(root, "source")}, Harnesses: map[string]execution.HarnessConfig{execution.HarnessClaude: {Executable: hostFixtureClaude, Version: publicationClaudeVersion}}, Coordinators: map[string]execution.Coordinator{rig.request.Coordinator: {PublicKey: execution.PublicKey(key), Callback: endpoint}}, MaxBusy: 1}}
	digest, err := s.Prepare(s.Policy())
	if err != nil {
		t.Fatal(err)
	}
	rig.request.PolicyDigest = digest
	j := execution.Journal{Request: rig.request, Digest: rig.request.Digest(), Handle: execution.Handle{Ref: rig.request.Ref, Unit: execution.UnitName(rig.request.Ref), ConversationID: rig.request.ConversationID, Workspace: shallowWorkspace, Commit: prHead}}
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
	if err := os.MkdirAll(filepath.Join(shallowWorkspace, ".beekeeper-scratch"), 0o700); err != nil {
		t.Fatal(err)
	}

	fixHead := rig.commit(t, "pr.txt", "review fix from shallow workspace\n")
	command := rig.a.executionPublishCmd()
	var output bytes.Buffer
	command.SetOut(&output)
	if err := command.Execute(); err != nil {
		t.Fatalf("publish review fix from shallow PR workspace failed: %v", err)
	}
	actual, err := publishGit(ctx, rig.remote, "rev-parse", "refs/heads/"+branch)
	if err != nil || actual != fixHead {
		t.Fatalf("remote branch head = %s (%v), want %s", actual, err, fixHead)
	}

	command = rig.a.executionPublishCmd()
	command.SetArgs([]string{"--sync-base"})
	command.SetOut(&bytes.Buffer{})
	if err := command.Execute(); err != nil {
		t.Fatalf("--sync-base in shallow PR workspace failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(shallowWorkspace, "upstream.txt")); err != nil {
		t.Fatalf("expected upstream.txt after --sync-base: %v", err)
	}
	mergedHead, err := publishGit(ctx, shallowWorkspace, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	command = rig.a.executionPublishCmd()
	command.SetOut(&bytes.Buffer{})
	if err := command.Execute(); err != nil {
		t.Fatalf("publish after --sync-base in shallow PR workspace failed: %v", err)
	}
	actual, err = publishGit(ctx, rig.remote, "rev-parse", "refs/heads/"+branch)
	if err != nil || actual != mergedHead {
		t.Fatalf("remote branch head after --sync-base publish = %s (%v), want %s", actual, err, mergedHead)
	}
}

func TestCoordinatorRecoversStrandedPublicationTransfer(t *testing.T) {
	rig := newPublicationRig(t)
	ctx := context.Background()
	branch := executionBranch(rig.request.Ref)
	prHead := rig.commit(t, "pr.txt", "initial PR commit\n")
	if _, err := publishGit(ctx, rig.workspace, "push", rig.remote, prHead+":refs/heads/"+branch); err != nil {
		t.Fatal(err)
	}
	rig.created = true
	fixHead := rig.commit(t, "pr.txt", "stranded review fix\n")
	p := rig.publication(t, fixHead)
	digest := p.BundleDigest

	if err := rig.a.store.Update(func(st *state.State) ([]state.Event, error) {
		record := st.Starts[0].Execution
		record.Handle.Commit = prHead
		record.Request.GitRef = prHead
		record.Published = &state.ExecutionPublication{At: time.Now().UTC(), Branch: branch, Head: prHead, URL: publicationFixturePRURL}
		record.Observation.ProcessState = execution.Exited
		record.Settled = true
		record.Lease.ExpiresAt = time.Now().Add(-time.Minute)
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}

	st, err := rig.a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if !rig.a.hasUnrecoveredPublication(st.Starts[0].Execution) {
		t.Fatal("expected unrecovered publication to be detected")
	}

	command := rig.a.executionPublishCmd()
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetArgs([]string{string(rig.request.Ref.Execution)})
	if err := command.Execute(); err != nil {
		t.Fatalf("coordinator recovery failed: %v", err)
	}
	var recovered executionPublicationResult
	if json.Unmarshal(output.Bytes(), &recovered) != nil || recovered.URL != publicationFixturePRURL || recovered.Head != fixHead {
		t.Fatalf("recovery output = %q, want PR URL and head %s", output.String(), fixHead)
	}
	actual, err := publishGit(ctx, rig.remote, "rev-parse", "refs/heads/"+branch)
	if err != nil || actual != fixHead {
		t.Fatalf("remote branch head after recovery = %s (%v), want %s", actual, err, fixHead)
	}
	st, err = rig.a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	rec := st.Starts[0].Execution
	if rec.Published == nil || rec.Published.Head != fixHead || rec.RecoveredTransfer != digest {
		t.Fatalf("unexpected execution record after recovery: published=%+v recovered=%q", rec.Published, rec.RecoveredTransfer)
	}
	if rig.a.hasUnrecoveredPublication(rec) {
		t.Fatal("expected publication to be marked recovered")
	}
}

const (
	review3325SourceOnly    = "source only"
	review3325DefaultTrunk  = "default trunk"
	review3325WrongRepo     = "wrong repo"
	review3325WrongBranch   = "wrong branch"
	review3325WrongSession  = "wrong session"
	review3325UnrelatedRepo = "unrelated repo"
	review3325WrongRef      = "wrong ref"
	review3325NewPR         = "new PR"
	review3325Metadata      = "metadata"
	review3325Authorized    = "authorized"
	review3325Unassigned    = "unassigned"
	review3325Superseded    = "superseded"
	review3325Finished      = "finished"
)

func TestReview3325AssignedReviewerPublication(t *testing.T) {
	for _, scenario := range []string{review3325Authorized, review3325Unassigned, claudeLoginExpired, review3325UnrelatedRepo, review3325WrongRef, review3325WrongSession, review3325Superseded, review3325Finished, review3325NewPR, review3325Metadata} {
		t.Run(scenario, func(t *testing.T) {
			rig := newPublicationRig(t)
			const authorBranch = "fixture/author-repair"
			if _, err := publishGit(context.Background(), rig.workspace, "push", rig.remote, rig.base+":refs/heads/"+authorBranch); err != nil {
				t.Fatal(err)
			}
			head := rig.commit(t, "repair.txt", "repair\n")
			p := rig.publication(t, head)
			p.Title, p.Body = "", ""
			rig.created = true
			originalRead := readLoopPull
			t.Cleanup(func() { readLoopPull = originalRead })
			readLoopPull = func(context.Context, string) (superloop.Pull, error) {
				return superloop.Pull{Ref: "giantswarm/beekeeper#42", State: superloop.Open, Branch: authorBranch, Head: rig.base}, nil
			}
			if err := rig.a.store.Update(func(st *state.State) ([]state.Event, error) {
				start := &st.Starts[0]
				start.Execution.Request.GitRef = authorBranch
				round := state.DeskReview{ID: "repair-assignment", Ref: "giantswarm/beekeeper#42", Head: rig.base, Round: 1, Reviewer: start.Party, AssignedAt: time.Now()}
				switch scenario {
				case claudeLoginExpired:
					start.Execution.Lease.ExpiresAt = time.Now().Add(-time.Second)
				case review3325UnrelatedRepo:
					round.Ref = "other/repo#42"
				case review3325WrongRef:
					start.Execution.Request.GitRef = "fixture/unrelated"
				case review3325WrongSession:
					round.Reviewer = state.Party{Session: "different-session"}
				case review3325Finished:
					round.Result = &state.DeskReviewResult{}
				case review3325NewPR:
					rig.created = false
				case review3325Metadata:
					p.Title = "fix: metadata change"
				}
				if scenario != review3325Unassigned {
					st.DeskReviews = append(st.DeskReviews, round)
				}
				if scenario == review3325Superseded {
					next := round
					next.ID = "next-assignment"
					next.Round++
					next.Reviewer = state.Party{Session: "next-session"}
					st.DeskReviews = append(st.DeskReviews, next)
				}
				return nil, nil
			}); err != nil {
				t.Fatal(err)
			}
			st, err := rig.a.store.Read()
			if err != nil {
				t.Fatal(err)
			}
			result, err := rig.a.publishExecution(context.Background(), st, rig.request.Ref, p)
			if scenario == review3325Authorized {
				if err != nil || result.Branch != authorBranch || result.URL != publicationFixturePRURL {
					t.Fatalf("authorized publication failed: %+v %v", result, err)
				}
			} else if scenario == review3325Unassigned || scenario == review3325WrongSession {
				if err == nil && result.Branch == authorBranch {
					t.Fatal("unassigned execution wrote author branch")
				}
			} else if err == nil {
				t.Fatalf("invalid assignment %s admitted: %+v", scenario, result)
			}
			got, readErr := publishGit(context.Background(), rig.workspace, "ls-remote", rig.remote, "refs/heads/"+authorBranch)
			if readErr != nil {
				t.Fatal(readErr)
			}
			want := rig.base
			if scenario == review3325Authorized {
				want = head
			}
			if !strings.HasPrefix(got, want+"\t") {
				t.Fatalf("author branch changed without authorization: %q", got)
			}
			if rig.creates != 0 || len(rig.edits) != 0 {
				t.Fatal("reviewer created or edited metadata")
			}
		})
	}
}

func TestReview3325GrantExpiresDuringBranchRead(t *testing.T) {
	rig := newPublicationRig(t)
	st, err := rig.a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	start := &st.Starts[0]
	record := start.Execution
	const branch = "fixture/lease-bound-review"
	const ref = "giantswarm/beekeeper#42"
	record.Request.GitRef = branch
	st.DeskReviews = append(st.DeskReviews, state.DeskReview{ID: "lease-bound-round", Ref: ref, Head: rig.base, Round: 1, Reviewer: start.Party, AssignedAt: time.Now()})
	originalRead := readLoopPull
	t.Cleanup(func() { readLoopPull = originalRead })
	readLoopPull = func(context.Context, string) (superloop.Pull, error) {
		record.Lease.ExpiresAt = time.Now().Add(-time.Second)
		return superloop.Pull{Ref: ref, State: superloop.Open, Branch: branch}, nil
	}
	if _, err := executionReviewWriteBranch(context.Background(), st, rig.request.Ref); err == nil {
		t.Fatal("branch read outlived the lease but still returned a write grant")
	}
}
