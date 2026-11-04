package remotebuild

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Exercise both spellings explicitly on every platform, rather than relying
// on a platform's temporary directory happening to contain a symlink.
func TestSweepFinishedSymlinkAliases(t *testing.T) {
	for _, recordedAlias := range []bool{false, true} {
		name := "physical record"
		if recordedAlias {
			name = "alias record"
		}
		t.Run(name, func(t *testing.T) {
			physical, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			alias := filepath.Join(t.TempDir(), "alias")
			if err := os.Symlink(physical, alias); err != nil {
				t.Fatal(err)
			}
			recorded, finished := physical, alias
			if recordedAlias {
				recorded, finished = alias, physical
			}
			host, err := os.Hostname()
			if err != nil {
				t.Fatal(err)
			}
			state := t.TempDir()
			dir := filepath.Join(state, "remote-build")
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			builder := builderName(host, recorded)
			var calls [][]string
			runner := Runner{StateDir: state, Command: func(_ context.Context, _ io.Reader, _ io.Writer, args ...string) error {
				calls = append(calls, slices.Clone(args))
				return nil
			}}
			runner.Config.Kubeconfig = filepath.Join(state, "fixture-kubeconfig")
			runner.Config.Context = "fixture-context"
			runner.Config.Namespace = "builders"
			if err := runner.remember(recorded, host, builder); err != nil {
				t.Fatal(err)
			}
			record := filepath.Join(dir, builderRecord{Kubeconfig: runner.Config.Kubeconfig, Context: runner.Config.Context, Namespace: runner.Config.Namespace}.file(builder))
			for _, step := range []struct {
				finished []string
				dry      bool
			}{
				{nil, false}, // An existing, unfinished alias must survive.
				{[]string{finished}, true},
			} {
				if err := runner.Sweep(context.Background(), step.finished, step.dry); err != nil {
					t.Fatal(err)
				}
				if len(calls) != 0 {
					t.Fatalf("premature cluster calls: %v", calls)
				}
				if _, err := os.Stat(record); err != nil {
					t.Fatalf("ownership record lost before retirement: %v", err)
				}
			}
			if err := runner.Sweep(context.Background(), []string{finished}, false); err != nil {
				t.Fatal(err)
			}
			want := []string{"--kubeconfig", runner.Config.Kubeconfig, "--context", runner.Config.Context, "--namespace", runner.Config.Namespace, "--request-timeout=15s"}
			want = append(want, strings.Fields("delete job "+builder+" --ignore-not-found=true --wait=false")...)
			if len(calls) != 1 || !slices.Equal(calls[0], want) {
				t.Fatalf("cluster calls = %v, want [%v]", calls, want)
			}
			if _, err := os.Stat(record); !os.IsNotExist(err) {
				t.Fatalf("retired builder record remains: %v", err)
			}
			if _, err := os.Stat(physical); err != nil {
				t.Fatalf("retirement changed the retained worktree: %v", err)
			}
		})
	}
}
