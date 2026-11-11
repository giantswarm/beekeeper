package cmd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/state"
)

func TestPrepareExecutionSourcePinsAndRecoversExactCommit(t *testing.T) {
	rig := newPublicationRig(t)
	ctx := context.Background()
	if err := rig.a.store.Update(func(st *state.State) ([]state.Event, error) {
		rec, _ := executionRecord(st, rig.request.Ref)
		rec.Phase = executionPending
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	request, data, err := rig.a.prepareExecutionSource(ctx, rig.request, nil)
	if err != nil {
		t.Fatal(err)
	}
	if request.GitRef != rig.base || request.SourceDigest != execution.Digest(data) {
		t.Fatal("source was not pinned to fetched head")
	}
	st, err := rig.a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	rec, _ := executionRecord(st, request.Ref)
	if rec.Request != request || rec.RequestDigest != request.Digest() {
		t.Fatal("pinned request was not durably registered")
	}
	// A later upstream change must not move a recovered execution.
	head := rig.commit(t, "later.txt", "later upstream")
	if _, err := executionPublishGit(ctx, rig.workspace, "push", rig.remote, head+":refs/heads/main"); err != nil {
		t.Fatal(err)
	}
	again, retry, err := rig.a.prepareExecutionSource(ctx, request, nil)
	if err != nil || again != request || execution.Digest(retry) != request.SourceDigest {
		t.Fatalf("recovery changed source: %v", err)
	}
}

func TestExecutionConfirmedMissingPreparationPinsFreshSource(t *testing.T) {
	rig := newPublicationRig(t)
	a := rig.a
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		rec, _ := executionRecord(st, rig.request.Ref)
		rec.Phase = executionPending
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	host := a.cfg.Agents.Hosts[sshTestSelector]
	handle := execution.Handle{Ref: rig.request.Ref, Repository: rig.request.Repository, Commit: rig.base, Workspace: filepath.Join(host.WorkspaceRoot, string(rig.request.Ref.Execution), "workspace"), Unit: execution.UnitName(rig.request.Ref), ConversationID: rig.request.ConversationID}
	prepared, running := false, false
	uploads, starts := 0, 0
	var digest string
	fake := &testExecutionTransport{}
	fake.inspect = func(context.Context, execution.Ref) (execution.Observation, error) {
		if !prepared {
			return execution.Observation{}, execution.ErrNotFound
		}
		phase := execution.Prepared
		if running {
			phase = execution.Running
		}
		return execution.Observation{Handle: handle, ProcessState: phase, Ready: running}, nil
	}
	fake.start = func(_ context.Context, r execution.StartRequest) (execution.Handle, error) {
		starts++
		if uploads != 1 || r.GitRef != rig.base || r.SourceDigest != digest {
			t.Fatal("fresh preparation preceded pinned source upload")
		}
		prepared = true
		if r.RegistrationReceipt != "" {
			running = true
		}
		return handle, nil
	}
	transport := &sourceRecoveryTransport{testExecutionTransport: fake, upload: func(_ context.Context, ref execution.Ref, data []byte) error {
		uploads++
		digest = execution.Digest(data)
		st, err := a.store.Read()
		if err != nil {
			t.Fatal(err)
		}
		rec, _ := executionRecord(st, ref)
		if rec.Request.GitRef != rig.base || rec.Request.SourceDigest != digest || rec.RequestDigest != rec.Request.Digest() {
			t.Fatal("upload preceded durable source pin")
		}
		return nil
	}}
	a.executionTransport = func(execution.Host) execution.Transport { return transport }
	if err := a.activateExecution(context.Background(), rig.request.Ref); err != nil {
		t.Fatal(err)
	}
	if uploads != 1 || starts != 2 || !running {
		t.Fatal("fresh preparation was not activated exactly once")
	}
}

func TestExecutionAmbiguousPreparationInspectionDoesNotUploadOrStart(t *testing.T) {
	rig := newPublicationRig(t)
	if err := rig.a.store.Update(func(st *state.State) ([]state.Event, error) {
		rec, _ := executionRecord(st, rig.request.Ref)
		rec.Phase = executionPending
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	inspectionErr := errors.New("ambiguous inspection failure")
	fake := &testExecutionTransport{}
	fake.inspect = func(context.Context, execution.Ref) (execution.Observation, error) {
		return execution.Observation{}, inspectionErr
	}
	fake.start = func(context.Context, execution.StartRequest) (execution.Handle, error) {
		t.Fatal("ambiguous inspection started preparation")
		return execution.Handle{}, nil
	}
	transport := &sourceRecoveryTransport{testExecutionTransport: fake, upload: func(context.Context, execution.Ref, []byte) error {
		t.Fatal("ambiguous inspection uploaded source")
		return nil
	}}
	rig.a.executionTransport = func(execution.Host) execution.Transport { return transport }
	if err := rig.a.activateExecution(context.Background(), rig.request.Ref); !errors.Is(err, inspectionErr) {
		t.Fatalf("inspection error lost: %v", err)
	}
}

// The coordinator bundles a start's source against the host's basis: only
// history beyond it travels, so a repository whose whole history exceeds the
// transfer limit still starts on a host that holds most of it.
func TestPrepareExecutionSourceShipsOnlyHistoryBeyondHostBasis(t *testing.T) {
	rig := newPublicationRig(t)
	ctx := context.Background()
	if err := rig.a.store.Update(func(st *state.State) ([]state.Event, error) {
		rec, _ := executionRecord(st, rig.request.Ref)
		rec.Phase = executionPending
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	git := func(dir string, args ...string) (string, error) {
		out, err := executionPublishGit(ctx, dir, args...)
		return strings.TrimSpace(string(out)), err
	}
	// The host's source: the history up to base.
	hostRepo := filepath.Join(t.TempDir(), "host.git")
	if _, err := git(filepath.Dir(hostRepo), "init", "--bare", hostRepo); err != nil {
		t.Fatal(err)
	}
	if _, err := git(hostRepo, "fetch", "--no-tags", rig.remote, "main:refs/heads/main"); err != nil {
		t.Fatal(err)
	}
	head := rig.commit(t, "later.txt", "later upstream")
	if _, err := git(rig.workspace, "push", rig.remote, head+":refs/heads/main"); err != nil {
		t.Fatal(err)
	}
	unknown := strings.Repeat("ab", 20)
	request, data, err := rig.a.prepareExecutionSource(ctx, rig.request, []string{unknown, rig.base})
	if err != nil {
		t.Fatal(err)
	}
	if request.GitRef != head || request.SourceDigest != execution.Digest(data) {
		t.Fatal("source was not pinned to the fetched head")
	}
	bundle := filepath.Join(t.TempDir(), "source.bundle")
	if err := os.WriteFile(bundle, data, 0o600); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(t.TempDir(), "empty.git")
	if _, err := git(filepath.Dir(empty), "init", "--bare", empty); err != nil {
		t.Fatal(err)
	}
	if _, err := git(empty, "fetch", "--no-tags", bundle, "refs/heads/execution-source"); err == nil {
		t.Fatal("bundle carries the history the host already holds")
	}
	if _, err := git(hostRepo, "fetch", "--no-tags", bundle, "refs/heads/execution-source"); err != nil {
		t.Fatalf("host source cannot take the incremental bundle: %v", err)
	}
	if got, _ := git(hostRepo, "rev-parse", "FETCH_HEAD"); got != head {
		t.Fatalf("incremental bundle delivered %s, want %s", got, head)
	}

	// A host already at the head receives the head alone, against its parents.
	if got := sourcePrerequisites(ctx, rig.workspace, head, []string{head}); len(got) != 1 || got[0] != "^"+rig.base {
		t.Fatalf("head basis excluded %v, want ^%s", got, rig.base)
	}
	if got := sourcePrerequisites(ctx, rig.workspace, head, []string{unknown}); got != nil {
		t.Fatalf("unknown basis excluded %v", got)
	}
	if got := sourcePrerequisites(ctx, rig.workspace, head, nil); got != nil {
		t.Fatalf("empty basis excluded %v", got)
	}
}

// Activation asks the host for its basis before it pins the source.
func TestExecutionStartUploadsSourceAgainstHostBasis(t *testing.T) {
	rig := newPublicationRig(t)
	a := rig.a
	if err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		rec, _ := executionRecord(st, rig.request.Ref)
		rec.Phase = executionPending
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	head := rig.commit(t, "later.txt", "later upstream")
	if _, err := executionPublishGit(context.Background(), rig.workspace, "push", rig.remote, head+":refs/heads/main"); err != nil {
		t.Fatal(err)
	}
	fake := &testExecutionTransport{}
	fake.inspect = func(context.Context, execution.Ref) (execution.Observation, error) {
		return execution.Observation{}, execution.ErrNotFound
	}
	uploaded := errors.New("uploaded")
	var asked string
	transport := &basisTransport{testExecutionTransport: fake, basis: []string{rig.base}, asked: &asked, upload: func(_ context.Context, _ execution.Ref, data []byte) error {
		bundle := filepath.Join(t.TempDir(), "source.bundle")
		if err := os.WriteFile(bundle, data, 0o600); err != nil {
			t.Fatal(err)
		}
		empty := filepath.Join(t.TempDir(), "empty.git")
		if _, err := executionPublishGit(context.Background(), filepath.Dir(empty), "init", "--bare", empty); err != nil {
			t.Fatal(err)
		}
		if _, err := executionPublishGit(context.Background(), empty, "fetch", "--no-tags", bundle, "refs/heads/execution-source"); err == nil {
			t.Fatal("uploaded source ignores the host's basis")
		}
		return uploaded
	}}
	a.executionTransport = func(execution.Host) execution.Transport { return transport }
	if err := a.activateExecution(context.Background(), rig.request.Ref); !errors.Is(err, uploaded) {
		t.Fatalf("source was not uploaded: %v", err)
	}
	if asked != rig.request.Repository {
		t.Fatalf("host basis asked for %q, want %q", asked, rig.request.Repository)
	}
}

type basisTransport struct {
	*testExecutionTransport
	basis  []string
	asked  *string
	upload func(context.Context, execution.Ref, []byte) error
}

func (t *basisTransport) SourceBasis(_ context.Context, _ execution.Ref, repository string) (execution.SourceBasis, error) {
	if t.asked != nil {
		*t.asked = repository
	}
	return execution.SourceBasis{Commits: t.basis}, nil
}

func (t *basisTransport) UploadSource(ctx context.Context, ref execution.Ref, data []byte) error {
	return t.upload(ctx, ref, data)
}

// hosts check reports how many default-branch commits each mapped host
// source lacks, and names one too far behind as a problem.
func TestHostsCheckReportsHostSourceBehind(t *testing.T) {
	a, _ := sshStartTestApp(t)
	h := a.cfg.Agents.Hosts[sshTestSelector]
	if _, ok := h.Repositories[sshTestRepository]; !ok || len(h.Repositories) != 1 {
		t.Fatalf("fixture host maps %v", h.Repositories)
	}
	tip := strings.Repeat("cd", 20)
	transport := &basisTransport{testExecutionTransport: &testExecutionTransport{}, basis: []string{tip}}
	a.executionTransport = func(execution.Host) execution.Transport { return transport }
	behind := 0
	original := executionPublishGH
	t.Cleanup(func() { executionPublishGH = original })
	var compared string
	executionPublishGH = func(_ context.Context, args ...string) ([]byte, error) {
		switch {
		case len(args) > 1 && args[1] == "repos/"+sshTestRepository:
			return []byte("main\n"), nil
		case len(args) > 1 && strings.HasPrefix(args[1], "repos/"+sshTestRepository+"/compare/"):
			compared = args[1]
			return []byte(strconv.Itoa(behind) + "\n"), nil
		}
		t.Fatalf("unexpected gh call %v", args)
		return nil, nil
	}
	reports := a.hostSourceReports(context.Background(), h)
	if len(reports) != 1 || reports[0].Head != tip || reports[0].Branch != "main" || reports[0].Missing != 0 || reports[0].Problem != "" {
		t.Fatalf("current source reported %+v", reports)
	}
	if compared != "repos/"+sshTestRepository+"/compare/"+tip+"...main" {
		t.Fatalf("compared %q", compared)
	}
	behind = sourceBehindCommits + 1
	reports = a.hostSourceReports(context.Background(), h)
	if len(reports) != 1 || reports[0].Missing != behind || !strings.Contains(reports[0].Problem, "lacks") {
		t.Fatalf("source behind reported %+v", reports)
	}
	a.executionTransport = func(execution.Host) execution.Transport { return &testExecutionTransport{} }
	if reports = a.hostSourceReports(context.Background(), h); len(reports) != 1 || !strings.Contains(reports[0].Problem, "no source basis route") {
		t.Fatalf("host without a basis route reported %+v", reports)
	}
}
