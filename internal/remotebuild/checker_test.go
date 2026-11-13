//nolint:goconst // Keep the fixtures' scripts and outputs readable.
package remotebuild

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestCheckerEnvProvidesTheToolsAndNamesWhatItLacks runs a command in the
// checker environment: the image's tools run as they are, a tool the image
// lacks is installed once into the workspace (jq as gojq), a failed install is
// exit 125, and the packages the repository's CI installs that the image lacks
// are named on one line before the command runs.
//
// control: none: the checker environment is new, so the base cannot build the test
func TestCheckerEnvProvidesTheToolsAndNamesWhatItLacks(t *testing.T) {
	bash, err := exec.LookPath(shellBash)
	if err != nil {
		t.Skip("bash unavailable")
	}
	// The system programs the environment and its fakes use, and no other:
	// the image's PATH must find neither this machine's jq nor dpkg-query.
	system := t.TempDir()
	for _, tool := range []string{"ln", "mkdir", "chmod"} {
		p, err := exec.LookPath(tool)
		if err != nil {
			t.Skipf("%s unavailable", tool)
		}
		if err := os.Symlink(p, filepath.Join(system, tool)); err != nil {
			t.Fatal(err)
		}
	}
	tools := t.TempDir()
	script := strings.ReplaceAll(checkerEnv, "/work/tools", tools)
	lint := "#!/bin/sh\necho \"linted by $0 $* cache=$GOLANGCI_LINT_CACHE\"\n"
	jq := "#!/bin/sh\necho \"jq of the image $*\"\n"
	// The install must not touch the image's GOPATH, which the builder's
	// non-root user cannot write; the tool it builds is named after the
	// module's last path element.
	install := "#!/bin/sh\n" +
		`[ "$1" = install ] && [ "$GOPATH" = '` + filepath.Join(tools, "gopath") + `' ] && [ -z "$GOFLAGS" ] || exit 1` + "\n" +
		`built=${2%@*}; built=${built##*/}` + "\n" +
		`mkdir -p "$GOBIN" && printf '#!/bin/sh\necho "%s by $0 $* cache=$GOLANGCI_LINT_CACHE"\n' "$built" > "$GOBIN/$built" && chmod 700 "$GOBIN/$built"` + "\n"
	fails := "#!/bin/sh\nexit 1\n"
	dpkgQuery := "#!/bin/sh\n[ \"$3\" = libfoo ] && printf 'install ok installed' && exit 0\necho \"dpkg-query: no packages found matching $3\" >&2\nexit 1\n"
	version := func(module string) string { return module[strings.LastIndex(module, "/")+1:] }
	for _, tc := range []struct {
		name     string
		image    map[string]string
		packages []string
		argv     []string
		fresh    bool
		rc       int
		want     []string
		not      []string
	}{
		{
			name:  "image tools",
			image: map[string]string{"golangci-lint": lint, "jq": jq, "go": fails},
			argv:  []string{"golangci-lint", "run", "./..."},
			want:  []string{"linted by IMAGE/golangci-lint run ./... cache=TOOLS/lint-cache"},
			not:   []string{"installing", EnvironmentLacks},
		},
		{
			name:  "install",
			image: map[string]string{"go": install},
			argv:  []string{"jq", "."},
			want: []string{
				"golangci-lint not in builder image; installing " + GolangCILintModule,
				"jq not in builder image; installing " + GoJQModule,
				"gojq by TOOLS/" + version(GoJQModule) + "/jq .",
			},
		},
		{
			name:  "already installed",
			image: map[string]string{"go": fails},
			argv:  []string{"golangci-lint", "run", "./..."},
			want:  []string{"golangci-lint by TOOLS/" + version(GolangCILintModule) + "/golangci-lint run ./... cache=TOOLS/lint-cache"},
			not:   []string{"installing"},
		},
		{
			name:  "install fails",
			image: map[string]string{"go": fails},
			argv:  []string{"golangci-lint", "run", "./..."},
			fresh: true,
			rc:    125,
			want:  []string{"installing " + GolangCILintModule + " failed"},
			not:   []string{"linted by"},
		},
		{
			name:     "a package the image lacks",
			image:    map[string]string{"golangci-lint": lint, "jq": jq, "dpkg-query": dpkgQuery},
			packages: []string{"libasound2-dev", "libfoo"},
			argv:     []string{"jq", "."},
			want:     []string{"beekeeper run: " + EnvironmentLacks + " libasound2-dev, which the repository CI installs", "jq of the image ."},
			not:      []string{"libfoo,"},
		},
		{
			name:     "an image without dpkg",
			image:    map[string]string{"golangci-lint": lint, "jq": jq},
			packages: []string{"libfoo"},
			argv:     []string{"jq", "."},
			want:     []string{EnvironmentLacks + " libfoo, which the repository CI installs", "jq of the image ."},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.fresh {
				if err := os.RemoveAll(tools); err != nil {
					t.Fatal(err)
				}
			}
			image := t.TempDir()
			for name, content := range tc.image {
				if err := os.WriteFile(filepath.Join(image, name), []byte(content), 0o700); err != nil { //nolint:gosec // executable fixture in the test-owned directory
					t.Fatal(err)
				}
			}
			argv := checkerCommand(tc.argv, tc.packages)
			argv[2] = script
			var out bytes.Buffer
			c := exec.CommandContext(t.Context(), bash, argv[1:]...) //nolint:gosec // test-owned environment and fixtures
			c.Env = []string{"PATH=" + image + ":" + system, "GOPATH=/read-only-image-gopath", "GOFLAGS=-mod=mod"}
			c.Stdout, c.Stderr = &out, &out
			rc := 0
			var exit *exec.ExitError
			if err := c.Run(); errors.As(err, &exit) {
				rc = exit.ExitCode()
			} else if err != nil {
				t.Fatal(err)
			}
			text := strings.NewReplacer(image, "IMAGE", tools, "TOOLS").Replace(out.String())
			if rc != tc.rc {
				t.Fatalf("rc=%d, want %d: %s", rc, tc.rc, text)
			}
			for _, want := range tc.want {
				if !strings.Contains(text, want) {
					t.Errorf("output lacks %q: %s", want, text)
				}
			}
			for _, not := range tc.not {
				if strings.Contains(text, not) {
					t.Errorf("output has %q: %s", not, text)
				}
			}
		})
	}
}

// TestCIPackagesReadsWhatTheRepositoryCIInstalls reads the packages a
// repository's CI installs: apt-packages inputs and apt-get and apt install
// commands of GitHub workflows and the CircleCI config, for the builder's
// architecture, literal names only, and never a file out of the worktree.
//
// control: none: CIPackages is new, so the base cannot build the test
func TestCIPackagesReadsWhatTheRepositoryCIInstalls(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.yml")
	files := map[string]string{
		".github/workflows/ci.yml": `on:
  pull_request:
jobs:
  ci:
    uses: example/workflows/.github/workflows/ci-go.yml@main
    with:
      apt-packages: libasound2-dev, libudev-dev ${{ inputs.more }}
  test:
    runs-on: ubuntu-latest
    steps:
      - name: Install the apt packages
        run: |
          sudo apt-get update
          sudo DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
            jq libssl-dev=3.0.13-0ubuntu3 libasound2-dev:arm64 "${{ inputs.extra }}" $EXTRA
      - run: apt -o Acquire::Retries=3 install -y shellcheck && echo ${{ github.sha }}
      - run: apt-get remove -y nano
`,
		".github/workflows/lint.yaml":  "jobs:\n  lint:\n    steps:\n      - run: sudo apt-get -y install yamllint\n",
		".github/workflows/broken.yml": "jobs: [\n",
		".circleci/config.yml":         "jobs:\n  build:\n    steps:\n      - run: apt-get install -y protobuf-compiler\n",
		outside:                        "jobs:\n  x:\n    steps:\n      - run: apt-get install -y leaked\n",
	}
	for name, content := range files {
		p := name
		if !filepath.IsAbs(p) {
			p = filepath.Join(root, name)
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(outside, filepath.Join(root, ".github/workflows/link.yml")); err != nil {
		t.Fatal(err)
	}
	want := []string{"jq", "libasound2-dev", "libssl-dev", "libudev-dev", "protobuf-compiler", "shellcheck", "yamllint"}
	if got := CIPackages(root); !slices.Equal(got, want) {
		t.Fatalf("CIPackages = %q, want %q", got, want)
	}
	if got := CIPackages(t.TempDir()); len(got) != 0 {
		t.Fatalf("a repository without CI installs %q", got)
	}
}
