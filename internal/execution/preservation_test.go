package execution

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestStoppedExecutionRetainsUnpublishedWork(t *testing.T) {
	for _, mode := range []string{"explicit stop", "repeated stop", "inspect expired unit"} {
		t.Run(mode, func(t *testing.T) {
			f := testFixture(t)
			ctx := context.Background()
			if _, err := f.s.Start(ctx, f.r); err != nil {
				t.Fatal(err)
			}
			f.r.RegistrationReceipt = Sign(f.r, f.key, f.s.now())
			h, err := f.s.Start(ctx, f.r)
			if err != nil {
				t.Fatal(err)
			}
			git := func(args ...string) string {
				t.Helper()
				out, err := f.s.git(ctx, h.Workspace, args...)
				if err != nil {
					t.Fatalf("git %v: %v", args, err)
				}
				return strings.TrimSpace(string(out))
			}
			write := func(path, contents string) {
				t.Helper()
				if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			tracked := filepath.Join(h.Workspace, "tracked.txt")
			write(tracked, "unpublished commit\n")
			git("add", "tracked.txt")
			git("-c", "user.name=Example", "-c", "user.email=example@example.invalid", "commit", "-m", "unpublished follow-up")
			commit := git("rev-parse", "HEAD")
			if commit == h.Commit {
				t.Fatal("fixture has no unpublished follow-up")
			}
			write(tracked, "uncommitted edit\n")
			untracked := filepath.Join(h.Workspace, "untracked.txt")
			write(untracked, "untracked work\n")
			source := f.s.sourcePath(f.r.Ref)
			if err := os.MkdirAll(filepath.Dir(source), 0o700); err != nil {
				t.Fatal(err)
			}
			write(source, "retained source artifact\n")
			stop := StopRequest{ID: uuid.NewString(), Reason: "lease expired"}
			if mode == "inspect expired unit" {
				// The lease runner already stopped the unit. Inspection is
				// the first coordinator reconciliation of that terminal state.
				if err := f.s.update(ctx, f.r.Ref, func(j *Journal) error {
					j.StopRequest = &stop
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				f.launcher.states[h.Unit] = unitInactive
			} else {
				if _, err := f.s.Stop(ctx, f.r.Ref, stop); err != nil {
					t.Fatal(err)
				}
			}
			for range 2 {
				o, err := f.s.Inspect(ctx, f.r.Ref)
				if err != nil || o.ProcessState != Exited || !o.StopAcknowledged {
					t.Fatalf("termination not acknowledged: %+v, %v", o, err)
				}
			}
			if mode != "explicit stop" {
				if _, err := f.s.Stop(ctx, f.r.Ref, stop); err != nil {
					t.Fatal(err)
				}
			}
			if got := git("rev-parse", "HEAD"); got != commit {
				t.Fatalf("unpublished commit changed: %s != %s", got, commit)
			}
			if got := git("show", "HEAD:tracked.txt"); got != "unpublished commit" {
				t.Fatalf("unpublished content lost: %q", got)
			}
			for path, want := range map[string]string{
				tracked:   "uncommitted edit\n",
				untracked: "untracked work\n",
				source:    "retained source artifact\n",
			} {
				got, err := os.ReadFile(path) //nolint:gosec // paths are created under this test's temporary execution directory
				if err != nil || string(got) != want {
					t.Fatalf("retained work lost at %s: %q, %v", path, got, err)
				}
			}
		})
	}
}
