package execution

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A start of a repository whose whole history exceeds the source transfer
// limit ships only the commits beyond the host's source: the host names its
// basis, the coordinator excludes it, and the workspace clone from the mapped
// checkout supplies the bundle's prerequisites.
func TestWorkspaceStartsFromIncrementalSourceBundle(t *testing.T) {
	f := testFixture(t)
	ctx := context.Background()
	git := func(dir string, args ...string) string {
		t.Helper()
		out, err := hostCommand(ctx, dir, os.Environ(), "git", args...)
		if err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
		return strings.TrimSpace(string(out))
	}
	sourceHead := git(f.source, "rev-parse", "HEAD")
	request, _ := json.Marshal(WireRequest{Protocol: Protocol, RequestID: "basis-one", Operation: SourceBasisOperation, Payload: mustJSON(f.r.Repository)})
	var output bytes.Buffer
	if err := f.s.Serve(ctx, bytes.NewReader(request), &output); err != nil {
		t.Fatal(err)
	}
	var reply WireResponse
	if err := json.Unmarshal(output.Bytes(), &reply); err != nil || reply.Error != "" {
		t.Fatalf("source basis refused: %v %s", err, reply.Error)
	}
	var basis SourceBasis
	if err := json.Unmarshal(reply.Payload, &basis); err != nil {
		t.Fatal(err)
	}
	if len(basis.Commits) == 0 || basis.Commits[0] != sourceHead {
		t.Fatalf("basis does not lead with the checkout's HEAD %s: %v", sourceHead, basis.Commits)
	}
	if _, err := f.s.SourceBasis(ctx, "elsewhere/unadmitted"); err == nil {
		t.Fatal("source basis answered for a repository the host does not admit")
	}

	upstream := filepath.Join(t.TempDir(), "upstream")
	git("", "clone", f.source, upstream)
	git(upstream, "config", "user.name", "Fixture")
	git(upstream, "config", "user.email", "fixture@example.invalid")
	if err := os.WriteFile(filepath.Join(upstream, "new.txt"), []byte("current upstream\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(upstream, "add", "new.txt")
	git(upstream, "commit", "-m", "new upstream commit")
	head := git(upstream, "rev-parse", "HEAD")
	git(upstream, "update-ref", "refs/heads/execution-source", head)
	bundle := filepath.Join(t.TempDir(), "source.bundle")
	args := []string{"bundle", "create", bundle, "refs/heads/execution-source"}
	for _, c := range basis.Commits {
		args = append(args, "^"+c)
	}
	git(upstream, args...)
	// The bundle is incremental: a repository without the basis cannot use it.
	empty := filepath.Join(t.TempDir(), "empty.git")
	git("", "init", "--bare", empty)
	if _, err := hostCommand(ctx, empty, os.Environ(), "git", "fetch", bundle, "refs/heads/execution-source"); err == nil {
		t.Fatal("bundle carries the host's history instead of only what it lacks")
	}
	data, err := readRegular(bundle, MaxSourceBundle)
	if err != nil {
		t.Fatal(err)
	}
	source := f.s.sourcePath(f.r.Ref)
	if err := os.MkdirAll(filepath.Dir(source), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := atomicFile(source, data); err != nil {
		t.Fatal(err)
	}
	f.r.GitRef, f.r.SourceDigest = head, Digest(data)
	handle, err := f.s.Start(ctx, f.r)
	if err != nil {
		t.Fatalf("prepare from incremental source bundle: %v", err)
	}
	if handle.Commit != head || git(handle.Workspace, "rev-parse", "HEAD") != head {
		t.Fatal("workspace did not pin the bundled head")
	}
}
