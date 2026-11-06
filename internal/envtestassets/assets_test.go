package envtestassets

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

func TestEnsurePreservesOverride(t *testing.T) {
	t.Setenv(Env, "/explicit/assets")
	got, err := Ensure(context.Background(), "")
	if err != nil || got != "/explicit/assets" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestEnsureCachesAndRepairs(t *testing.T) {
	t.Setenv(Env, "")
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	// A fake setup-envtest checks the CLI contract and creates runnable assets.
	script := `#!/bin/sh
set -eu
[ "$1" = run ]
[ "$2" = '` + tool + `' ]
[ "$3" = use ]
[ "$4" = --bin-dir ]
[ "$6" = -p ]
[ "$7" = path ]
[ "$8" = '` + version + `' ]
mkdir -p "$5/binaries"
for name in etcd kube-apiserver; do
 printf '#!/bin/sh\nexit 0\n' > "$5/binaries/$name"
 chmod +x "$5/binaries/$name"
done
printf x >> "$5/downloads"
printf '%s/binaries\n' "$PWD"
`
	if err := os.WriteFile(filepath.Join(bin, "go"), []byte(script), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(bin, "go"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	state := filepath.Join(root, "state")
	first, err := Ensure(context.Background(), state)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Ensure(context.Background(), state)
	if err != nil || first != second {
		t.Fatalf("cache: %q, %v", second, err)
	}
	count := func(want string) {
		t.Helper()
		root, err := os.OpenRoot(filepath.Join(state, "envtest"))
		if err != nil {
			t.Fatal(err)
		}
		defer root.Close()
		f, err := root.Open("downloads")
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		data, err := io.ReadAll(f)
		if err != nil || string(data) != want {
			t.Fatalf("downloads %q, %v", data, err)
		}
	}
	count("x")
	if err := os.Remove(filepath.Join(first, "etcd")); err != nil {
		t.Fatal(err)
	}
	if _, err := Ensure(context.Background(), state); err != nil {
		t.Fatal(err)
	}
	count("xx")
}

func TestEnsureFailure(t *testing.T) {
	t.Setenv(Env, "")
	t.Setenv("PATH", t.TempDir())
	_, err := Ensure(context.Background(), t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "setup-envtest") {
		t.Fatalf("got %v", err)
	}
}

func TestProvisionedEnvtest(t *testing.T) {
	if os.Getenv("BEEKEEPER_TEST_PROVISION_ENVTEST") != "1" {
		t.Skip("set BEEKEEPER_TEST_PROVISION_ENVTEST=1 to download and start envtest")
	}
	t.Setenv(Env, "")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	assets, err := Ensure(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(Env, assets)
	controlPlane := &envtest.Environment{}
	if _, err := controlPlane.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := controlPlane.Stop(); err != nil {
			t.Error(err)
		}
	})
}
