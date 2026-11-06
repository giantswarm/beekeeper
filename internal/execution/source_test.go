package execution

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	data, err := readRegular(bundle, MaxSourceBundle)
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(f.s.Root, "sources", string(f.r.Ref.Execution), "source.bundle")
	if err := os.MkdirAll(filepath.Dir(source), 0700); err != nil {
		t.Fatal(err)
	}
	if err := atomicFile(source, data); err != nil {
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

func TestWorkspaceUnshallowsHostCheckoutWhenPinnedSourceIsComplete(t *testing.T) {
	f := testFixture(t)
	ctx := context.Background()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	upstream := filepath.Join(root, "upstream")
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
	for _, name := range []string{"second.txt", "third.txt"} {
		if err := os.WriteFile(filepath.Join(upstream, name), []byte(name+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		git(upstream, "add", name)
		git(upstream, "commit", "-m", "add "+name)
	}
	// Replace the host's mapped checkout with a shallow clone of an older commit.
	shallowSource := filepath.Join(root, "shallow-source")
	git("", "clone", "--depth", "1", "file://"+f.source, shallowSource)
	git(shallowSource, "remote", "set-url", "origin", "https://github.com/"+f.r.Repository+".git")
	f.s.Config.Repositories[f.r.Repository] = shallowSource
	digest, err := f.s.Prepare(f.s.Policy())
	if err != nil {
		t.Fatal(err)
	}
	f.r.PolicyDigest = digest

	head := git(upstream, "rev-parse", "HEAD")
	git(upstream, "update-ref", "refs/heads/execution-source", head)
	bundle := filepath.Join(root, "source.bundle")
	git(upstream, "bundle", "create", bundle, "refs/heads/execution-source")
	data, err := readRegular(bundle, MaxSourceBundle)
	if err != nil {
		t.Fatal(err)
	}
	source := f.s.sourcePath(f.r.Ref)
	if err := os.MkdirAll(filepath.Dir(source), 0700); err != nil {
		t.Fatal(err)
	}
	if err := atomicFile(source, data); err != nil {
		t.Fatal(err)
	}
	f.r.GitRef, f.r.SourceDigest = head, Digest(data)

	handle, err := f.s.Start(ctx, f.r)
	if err != nil {
		t.Fatalf("prepare workspace from shallow host checkout: %v", err)
	}
	if got := git(handle.Workspace, "rev-parse", "--is-shallow-repository"); got != "false" {
		t.Fatalf("provisioned workspace remained shallow: %s", got)
	}
	if got := git(handle.Workspace, "rev-list", "--count", "HEAD"); got != "3" {
		t.Fatalf("provisioned workspace history count = %s, want 3", got)
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
	data, err := readRegular(target, MaxWire)
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

func TestSourceInspectionDistinguishesMissingAndUnreadableJournal(t *testing.T) {
	f := testFixture(t)
	ctx := context.Background()
	transport := f.transport(t, nil)
	if _, err := transport.Inspect(ctx, f.r.Ref); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing journal: %v", err)
	}
	p, err := f.s.journalPath(f.r.Ref)
	if err != nil {
		t.Fatal(err)
	}
	if err := atomicFile(p, []byte("corrupt journal")); err != nil {
		t.Fatal(err)
	}
	if _, err := transport.Inspect(ctx, f.r.Ref); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("unreadable journal treated as absent: %v", err)
	}
}

// An organization admitted by host policy needs no preseeded source checkout.
func TestWorkspaceProvisionsThirdRepository(t *testing.T) {
	for _, denied := range []bool{false, true} {
		t.Run(fmt.Sprint(denied), func(t *testing.T) {
			f := testFixture(t)
			ctx := context.Background()
			out, err := f.s.git(ctx, f.source, "rev-parse", "HEAD")
			if err != nil {
				t.Fatal(err)
			}
			head := strings.TrimSpace(string(out))
			if _, err := f.s.git(ctx, f.source, "update-ref", "refs/heads/execution-source", head); err != nil {
				t.Fatal(err)
			}
			bundle := filepath.Join(t.TempDir(), "source.bundle")
			if _, err := f.s.git(ctx, f.source, "bundle", "create", bundle, "refs/heads/execution-source"); err != nil {
				t.Fatal(err)
			}
			data, err := readRegular(bundle, MaxSourceBundle)
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(f.s.Config)
			var config map[string]any
			if err := json.Unmarshal(raw, &config); err != nil {
				t.Fatal(err)
			}
			config["repositoryOrgs"] = []string{fixtureRepositoryOwner}
			raw, _ = json.Marshal(config)
			if err := json.Unmarshal(raw, &f.s.Config); err != nil {
				t.Fatal(err)
			}
			digest, err := f.s.Prepare(f.s.Policy())
			if err != nil {
				t.Fatal(err)
			}
			f.r.Repository, f.r.GitRef, f.r.SourceDigest, f.r.PolicyDigest = "example/third", head, Digest(data), digest
			if denied {
				f.r.Repository = "foreign/third"
			}
			if err := f.s.ReceiveSource(ctx, f.r.Ref, SourceChunk{Total: int64(len(data)), Data: data}); err != nil {
				t.Fatal(err)
			}
			h, err := f.s.Start(ctx, f.r)
			if denied {
				if err == nil {
					t.Fatal("untrusted repository admitted")
				}
				return
			}
			if err != nil {
				t.Fatalf("provision third repository: %v", err)
			}
			origin, err := f.s.git(ctx, h.Workspace, "remote", "get-url", "origin")
			if err != nil || strings.TrimSpace(string(origin)) != "https://github.com/example/third.git" {
				t.Fatalf("origin=%s err=%v", origin, err)
			}
			if h.Commit != head {
				t.Fatalf("commit=%s want=%s", h.Commit, head)
			}
			again, err := f.s.Start(ctx, f.r)
			if err != nil || again.Commit != head {
				t.Fatalf("retry: %v %v", again, err)
			}
		})
	}
}

// A host nobody configured still gives its worker a commit identity; one the
// execution user configured stays theirs.
func TestPreparedWorkspaceCarriesCommitIdentity(t *testing.T) {
	for name, tc := range map[string]struct{ config, want string }{
		"unconfigured host": {"", WorkerGitName + " <" + WorkerGitEmail + ">"},
		"configured host":   {"[user]\n\tname = Execution User\n\temail = execution@example.invalid\n", "Execution User <execution@example.invalid>"},
	} {
		t.Run(name, func(t *testing.T) {
			f := testFixture(t)
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
			if tc.config != "" {
				if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte(tc.config), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			handle, err := f.s.Start(t.Context(), f.r)
			if err != nil {
				t.Fatal(err)
			}
			out, err := hostCommand(t.Context(), handle.Workspace, append(cleanEnvironment("", ""), "GIT_CONFIG_NOSYSTEM=1"), "git", "var", "GIT_AUTHOR_IDENT")
			if err != nil {
				t.Fatalf("a worker's commit identity does not resolve: %v", err)
			}
			if !strings.HasPrefix(string(out), tc.want+" ") {
				t.Fatalf("identity = %q, want %q", out, tc.want)
			}
		})
	}
}
