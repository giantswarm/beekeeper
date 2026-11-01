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

// The mapped checkout deliberately never receives the newer upstream commit.
// Preparation must use the coordinator's pinned objects without GitHub access.
func TestWorkspaceStartsAtCoordinatorHeadWithStaleHost(t *testing.T) {
	f := testFixture(t)
	ctx := context.Background()
	upstream := filepath.Join(t.TempDir(), "upstream")
	git := func(dir string, args ...string) string {
		t.Helper()
		out, err := hostCommand(ctx, dir, os.Environ(), "git", args...)
		if err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
		return strings.TrimSpace(string(out))
	}
	git("", "clone", f.source, upstream)
	git(upstream, "config", "user.name", "Fixture")
	git(upstream, "config", "user.email", "fixture@example.invalid")
	if err := os.WriteFile(filepath.Join(upstream, "new.txt"), []byte("current upstream\n"), 0600); err != nil {
		t.Fatal(err)
	}
	git(upstream, "add", "new.txt")
	git(upstream, "commit", "-m", "new upstream commit")
	head := git(upstream, "rev-parse", "HEAD")
	git(upstream, "update-ref", "refs/heads/execution-source", head)
	bundle := filepath.Join(t.TempDir(), "source.bundle")
	git(upstream, "bundle", "create", bundle, "refs/heads/execution-source")
	data, err := os.ReadFile(bundle)
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(f.s.Root, "sources", string(f.r.Ref.Execution), "source.bundle")
	if err := os.MkdirAll(filepath.Dir(source), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, data, 0600); err != nil {
		t.Fatal(err)
	}
	// JSON keeps this regression executable against the pre-fix request type.
	raw, _ := json.Marshal(f.r)
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	fields["gitRef"], fields["sourceDigest"] = head, Digest(data)
	raw, _ = json.Marshal(fields)
	if err := json.Unmarshal(raw, &f.r); err != nil {
		t.Fatal(err)
	}
	handle, err := f.s.Start(ctx, f.r)
	if err != nil {
		t.Fatalf("prepare current upstream head from stale host: %v", err)
	}
	if handle.Commit != head || git(handle.Workspace, "rev-parse", "HEAD") != head {
		t.Fatal("workspace did not pin current upstream commit")
	}
	if git(f.source, "rev-parse", "HEAD") == head {
		t.Fatal("host source was mutated")
	}
	if _, err := os.Stat(filepath.Join(handle.Workspace, "new.txt")); err != nil {
		t.Fatal(err)
	}
	retry, err := f.s.Start(ctx, f.r)
	if err != nil || retry.Commit != head {
		t.Fatalf("retry changed pinned source: %v", err)
	}
}

func TestSourceChunksRejectConflictsAndPreserveRetries(t *testing.T) {
	f := testFixture(t)
	ctx := context.Background()
	first := SourceChunk{Total: 6, Data: []byte("abc")}
	if err := f.s.ReceiveSource(ctx, f.r.Ref, first); err != nil {
		t.Fatal(err)
	}
	if err := f.s.ReceiveSource(ctx, f.r.Ref, first); err != nil {
		t.Fatal(err)
	}
	for _, chunk := range []SourceChunk{{Total: 6, Data: []byte("bad")}, {Offset: 4, Total: 6, Data: []byte("ef")}, {Offset: 1<<63 - 1, Total: 6, Data: []byte("ef")}, {Total: MaxSourceBundle + 1, Data: []byte("x")}} {
		if err := f.s.ReceiveSource(ctx, f.r.Ref, chunk); err == nil {
			t.Fatal("invalid transfer accepted")
		}
	}
	if err := f.s.ReceiveSource(ctx, f.r.Ref, SourceChunk{Offset: 3, Total: 6, Data: []byte("def")}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(f.s.sourcePath(f.r.Ref))
	if err != nil || string(data) != "abcdef" {
		t.Fatalf("source corrupted: %v", err)
	}
}

func TestSourceUploadRoundTripAndPreparedRetry(t *testing.T) {
	f := testFixture(t)
	ctx := context.Background()
	transport := SSHTransport{Host: f.host, Run: func(ctx context.Context, _ []string, raw []byte) ([]byte, error) {
		var out bytes.Buffer
		err := f.s.Serve(ctx, bytes.NewReader(raw), &out)
		return out.Bytes(), err
	}}
	data := bytes.Repeat([]byte("x"), SourceChunkSize+7)
	if err := transport.UploadSource(ctx, f.r.Ref, data); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(f.s.sourcePath(f.r.Ref))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("wire transfer: %v", err)
	}
	// A lost preparation reply permits only identical source retries.
	if _, err := f.s.Start(ctx, f.r); err != nil {
		t.Fatal(err)
	}
	if err := transport.UploadSource(ctx, f.r.Ref, data); err != nil {
		t.Fatal(err)
	}
	data[0] = 'y'
	if err := transport.UploadSource(ctx, f.r.Ref, data); err == nil {
		t.Fatal("prepared source was overwritten")
	}
}

func TestSourceTransferRejectsSymlink(t *testing.T) {
	f := testFixture(t)
	p := f.s.sourcePath(f.r.Ref)
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "untouched")
	if err := os.WriteFile(target, []byte("untouched"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, p); err != nil {
		t.Fatal(err)
	}
	if err := f.s.ReceiveSource(context.Background(), f.r.Ref, SourceChunk{Total: 1, Data: []byte("x")}); err == nil {
		t.Fatal("symlink source accepted")
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "untouched" {
		t.Fatal("symlink target changed")
	}
}

func TestWorkspaceRejectsCorruptPinnedSource(t *testing.T) {
	f := testFixture(t)
	ctx := context.Background()
	head, err := f.s.git(ctx, f.source, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	f.r.GitRef = strings.TrimSpace(string(head))
	f.r.SourceDigest = Digest([]byte("expected bundle"))
	p := f.s.sourcePath(f.r.Ref)
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("corrupted"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.Start(ctx, f.r); err == nil || !strings.Contains(err.Error(), "missing or corrupt") {
		t.Fatalf("corrupt source accepted: %v", err)
	}
	if len(f.launcher.starts) != 0 {
		t.Fatal("corrupt source launched a worker")
	}
}
