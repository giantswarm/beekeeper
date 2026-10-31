//nolint:goconst // Literal tool names and protocol keys keep fixtures readable.
package remotebuild

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestCacheSecretProjection(t *testing.T) {
	c := validConfig()
	c.Cache = Cache{Enabled: true, Secret: "build-cache", ReadOnly: true}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	pod := Job(c, "test", "builder:test").Spec.Template.Spec
	env := pod.Containers[0].Env
	values := map[string]string{}
	refs := 0
	for _, v := range env {
		values[v.Name] = v.Value
		if v.ValueFrom != nil {
			refs++
			r := v.ValueFrom.SecretKeyRef
			if v.Value != "" || r == nil || r.Name != "build-cache" || !*r.Optional || r.Key != v.Name {
				t.Fatalf("credential must be an optional named Secret key: %s", v.Name)
			}
		}
	}
	if refs != 13 || values["CACHEPROG_DISABLE_PUT"] != "true" || values["SCCACHE_S3_RW_MODE"] != "READ_ONLY" || values["CACHEPROG_S3_PREFIX"] == values["SCCACHE_S3_KEY_PREFIX"] {
		t.Fatalf("cache env policy: refs=%d", refs)
	}
	if len(pod.Containers[0].EnvFrom) != 0 {
		t.Fatal("unrestricted Secret env projection")
	}
	base := Job(validConfig(), "test", "builder:test").Annotations["beekeeper.dev/builder-spec"]
	if base == Job(c, "test", "builder:test").Annotations["beekeeper.dev/builder-spec"] {
		t.Fatal("cache config must invalidate warm builder spec")
	}
	c.Cache.Enabled = false
	if base != Job(c, "test", "builder:test").Annotations["beekeeper.dev/builder-spec"] {
		t.Fatal("disabled cache must retain noncache builder compatibility")
	}
	for _, bad := range []Cache{{Enabled: true}, {Enabled: true, Secret: "bad/name"}, {Enabled: true, Secret: "build-cache", GoHelper: "cacheprog;echo injected"}, {Enabled: true, Secret: "build-cache", GoPrefix: "../escape"}, {Enabled: true, Secret: "build-cache", GoPrefix: "same", RustPrefix: "same/"}} {
		if bad.validate() == nil {
			t.Fatalf("invalid cache config accepted: %+v", bad)
		}
	}
}

func TestCacheStartupFailureDoesNotFailCommand(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux builder shell contract")
	}
	for _, tool := range []string{"go", "cargo"} {
		t.Run(tool, func(t *testing.T) {
			dir := t.TempDir()
			bin := t.TempDir()
			script := func(s string) string { return strings.ReplaceAll(s, "/work", dir) }
			for _, d := range []string{"busy", "src", "incoming", "runs/test"} {
				if err := os.MkdirAll(filepath.Join(dir, d), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(dir, "busy/id"), []byte("test"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "runs/test/log"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			// Startup errors must not reach the build journal, even if the provider
			// echoes sensitive request details. These are synthetic, noncredential data.
			for _, name := range []string{"cacheprog", "sccache"} {
				p := filepath.Join(bin, name)
				if err := os.WriteFile(p, []byte("#!/bin/sh\necho PROVIDER_DIAGNOSTIC_MUST_NOT_APPEAR >&2\nexit 19\n"), 0o700); err != nil {
					t.Fatal(err)
				} //nolint:gosec // executable test fixture
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, shellBash, "-c", script(wrapper), "--", "test", ".", "printf BUILD_COMPLETED; exit 37", tool) //nolint:gosec // fixed production script in an owned fixture
			cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "BEEKEEPER_CACHE_ENABLED=1", "BEEKEEPER_GO_CACHE_HELPER=cacheprog", "CACHEPROG_S3_BUCKET=fixture", "CACHEPROG_S3_ACCESS_KEY_ID=fixture", "CACHEPROG_S3_ACCESS_KEY_SECRET=fixture", "SCCACHE_BUCKET=fixture", "AWS_ACCESS_KEY_ID=fixture", "AWS_SECRET_ACCESS_KEY=fixture")
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("wrapper failed: %v %s", err, out)
			}
			rc, err := os.ReadFile(filepath.Join(dir, "runs/test/rc"))
			if err != nil || string(rc) != "37\n" {
				t.Fatalf("exit=%q err=%v", rc, err)
			} //nolint:gosec // owned fixture
			log, err := os.ReadFile(filepath.Join(dir, "runs/test/log"))
			if err != nil {
				t.Fatal(err)
			} //nolint:gosec // owned fixture
			if !bytes.Contains(log, []byte("BUILD_COMPLETED")) || bytes.Contains(log, []byte("PROVIDER_DIAGNOSTIC_MUST_NOT_APPEAR")) || !bytes.Contains(log, []byte("unavailable")) {
				t.Fatalf("bad safe fallback journal: %s", log)
			}
		})
	}
}

func TestCacheFiltersDiagnosticsAndCleansDisk(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux builder shell contract")
	}
	for _, tool := range []string{"go", "cargo"} {
		t.Run(tool, func(t *testing.T) {
			dir := t.TempDir()
			bin := t.TempDir()
			script := func(s string) string { return strings.ReplaceAll(s, "/work", dir) }
			for _, d := range []string{"busy", "src", "incoming", "runs/test"} {
				if err := os.MkdirAll(filepath.Join(dir, d), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(dir, "busy/id"), []byte("test"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "runs/test/log"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			helper := `#!/bin/bash
mkdir -p "$CACHEPROG_DISK_STORAGE_ROOT"
echo PROVIDER_DIAGNOSTIC_MUST_NOT_APPEAR >&2
echo 'cacheprog statistics get_calls=9 get_hits=7 put_calls=2' >&2
echo CACHE_PROTOCOL_OK
`
			sccache := `#!/bin/bash
case "$1" in
--show-stats) echo 'Cache hits 5'; echo 'Cache location PROVIDER_DIAGNOSTIC_MUST_NOT_APPEAR';;
--stop-server) touch "$STOP_MARKER";;
esac
`
			for name, body := range map[string]string{"cacheprog": helper, "sccache": sccache} {
				if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o700); err != nil {
					t.Fatal(err)
				} //nolint:gosec // executable test fixture
			}
			command := `"$GOCACHEPROG" </dev/null; printf BUILD_COMPLETED; exit 37`
			hits := "Go cache hits 7"
			if tool == "cargo" {
				command = `[ "$RUSTC_WRAPPER" = sccache ] || exit 3; printf BUILD_COMPLETED; exit 37`
				hits = "Rust cache hits 5"
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, shellBash, "-c", script(wrapper), "--", "test", ".", command, tool) //nolint:gosec // production script in an owned fixture
			cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "BEEKEEPER_CACHE_ENABLED=1", "BEEKEEPER_GO_CACHE_HELPER=cacheprog", "CACHEPROG_S3_BUCKET=fixture", "CACHEPROG_S3_ACCESS_KEY_ID=fixture", "CACHEPROG_S3_ACCESS_KEY_SECRET=fixture", "SCCACHE_BUCKET=fixture", "AWS_ACCESS_KEY_ID=fixture", "AWS_SECRET_ACCESS_KEY=fixture", "STOP_MARKER="+filepath.Join(dir, "stopped"))
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("wrapper: %v %s", err, out)
			}
			log, err := os.ReadFile(filepath.Join(dir, "runs/test/log"))
			if err != nil {
				t.Fatal(err)
			} //nolint:gosec // owned fixture
			if strings.Count(string(log), "BUILD_COMPLETED") != 1 || !bytes.Contains(log, []byte(hits)) || bytes.Contains(log, []byte("PROVIDER_DIAGNOSTIC_MUST_NOT_APPEAR")) {
				t.Fatalf("unsafe or missing statistics: %s", log)
			}
			if _, err := os.Stat(filepath.Join(dir, "runs/test/cache")); !os.IsNotExist(err) {
				t.Fatalf("per-run Go disk cache retained: %v", err)
			}
			if tool == "cargo" {
				if _, err := os.Stat(filepath.Join(dir, "stopped")); err != nil {
					t.Fatal("sccache server was not stopped")
				}
			}
		})
	}
}
