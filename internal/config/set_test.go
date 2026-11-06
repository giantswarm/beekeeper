package config

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	keyAgeIdentities = "secret.ageIdentities"
	keyStore         = "secret.store"
	keyGrantTTL      = "grantTTL"
)

const storeConfig = `# the desk
grantTTL: 20m # short
secret:
  # the vault
  vault: V
  ageIdentities:
    - recipient: age1x
      ref: op://V/i/f
`

func writeConfig(t *testing.T, raw string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(raw), 0o640); err != nil { //nolint:gosec // a mode Set keeps
		t.Fatal(err)
	}
	return p
}

// A reference and its source set together land in one write, the file's
// comments, other keys and mode kept.
func TestSetCrossReferencedKeys(t *testing.T) {
	p := writeConfig(t, storeConfig)
	c, err := Set(p, []Setting{
		{keyAgeIdentities, `[{recipient: age1y, ref: "store://keys/age"}]`},
		{keyStore, "{read: [secret-tool, lookup, entry]}"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Incomplete()) != 0 || c.Secret.AgeIdentities[0].Ref != "store://keys/age" || len(c.Secret.Store.Read) != 3 {
		t.Errorf("set = %+v, incomplete %v", c.Secret, c.Incomplete())
	}
	raw, err := os.ReadFile(filepath.Clean(p))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"# the desk", "grantTTL: 20m # short", "# the vault", "vault: V", "secret-tool"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("written file lost %q:\n%s", want, raw)
		}
	}
	if fi, err := os.Stat(p); err != nil || fi.Mode().Perm() != 0o640 {
		t.Errorf("mode = %v, %v", fi.Mode(), err)
	}
	if _, err := Load(p); err != nil {
		t.Errorf("written file does not load: %v", err)
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(p), ".config.yaml.*")); len(left) != 0 {
		t.Errorf("temporary files left: %v", left)
	}
}

// A result that would not load is refused, the file untouched.
func TestSetRefusesInvalid(t *testing.T) {
	p := writeConfig(t, storeConfig)
	for name, s := range map[string][]Setting{
		"bad ref":       {{keyStore, "{read: [r]}"}, {keyAgeIdentities, "[{recipient: age1y, ref: ~/key}]"}},
		"bad duration":  {{keyGrantTTL, "soon"}},
		"bad value":     {{keyStore, "{read: ["}},
		"no map":        {{"grantTTL.x", "1"}},
		"empty key":     {{"secret..store", "1"}},
		"unknown shape": {{"secret.store.read", "{a: b}"}},
	} {
		_, err := Set(p, s)
		if err == nil {
			t.Errorf("%s: accepted", name)
		}
		if raw, _ := os.ReadFile(filepath.Clean(p)); string(raw) != storeConfig {
			t.Errorf("%s: file changed to\n%s", name, raw)
		}
	}
	if _, err := Set(p, []Setting{{keyGrantTTL, "soon"}}); !errors.Is(err, ErrInvalid) {
		t.Errorf("invalid result = %v, want ErrInvalid", err)
	}
}

// A reference whose source is missing is written: it loads, Incomplete
// says it.
func TestSetIncomplete(t *testing.T) {
	p := writeConfig(t, storeConfig)
	c, err := Set(p, []Setting{{keyAgeIdentities, `[{recipient: age1y, ref: "store://keys/age"}]`}})
	if err != nil || len(c.Incomplete()) != 1 {
		t.Fatalf("set = %v, incomplete %v", err, c)
	}
}

// A missing or empty file is created; a linked one is replaced where it
// lives, the link kept.
func TestSetCreatesAndFollowsLinks(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "new", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Set(p, []Setting{{"secret.store.read", "[r]"}}); err != nil {
		t.Fatal(err)
	}
	if c, err := Load(p); err != nil || len(c.Secret.Store.Read) != 1 {
		t.Errorf("created = %v", err)
	}
	if err := os.WriteFile(p, []byte("# nothing yet\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Set(p, []Setting{{keyGrantTTL, "1m"}}); err != nil {
		t.Errorf("comment-only file: %v", err)
	}

	real := writeConfig(t, storeConfig)
	link := filepath.Join(dir, "link.yaml")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Set(link, []Setting{{keyGrantTTL, "5m"}}); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("link replaced: %v, %v", fi.Mode(), err)
	}
	if raw, _ := os.ReadFile(filepath.Clean(real)); !strings.Contains(string(raw), "grantTTL: 5m") {
		t.Errorf("linked file not written:\n%s", raw)
	}
}

// Get reads a key's value as YAML without its comments, and whether the file
// has it; Change sets and removes keys in one write, a mapping left empty
// removed with its last key.
func TestGetAndChange(t *testing.T) {
	p := writeConfig(t, storeConfig)
	if v, ok, err := Get(p, keyGrantTTL); err != nil || !ok || v != "20m" {
		t.Errorf("Get grantTTL = %q, %v, %v", v, ok, err)
	}
	if v, ok, err := Get(p, "secret.ageIdentities"); err != nil || !ok || !SameValue(v, "[{recipient: age1x, ref: op://V/i/f}]") {
		t.Errorf("Get ageIdentities = %q, %v, %v", v, ok, err)
	}
	if _, ok, err := Get(p, "secret.vault.x"); err != nil || ok {
		t.Errorf("Get under a scalar = %v, %v", ok, err)
	}
	if _, ok, err := Get(filepath.Join(t.TempDir(), "none.yaml"), keyGrantTTL); err != nil || ok {
		t.Errorf("Get of a missing file = %v, %v", ok, err)
	}
	if _, err := Change(p, []Setting{{keyGrantTTL, "5m"}}, []string{"secret.vault", "secret.ageIdentities", "github.floor"}); err != nil {
		t.Fatal(err)
	}
	if v, _, _ := Get(p, keyGrantTTL); v != "5m" {
		t.Errorf("grantTTL = %q", v)
	}
	if _, ok, _ := Get(p, "secret"); ok {
		raw, _ := os.ReadFile(filepath.Clean(p))
		t.Errorf("secret kept empty:\n%s", raw)
	}
	if _, err := Change(p, nil, []string{"a..b"}); err == nil {
		t.Error("an empty key unset")
	}
}

// Separate processes must share a transaction lock even through a symlink.
func TestSetConcurrentProcesses(t *testing.T) {
	const helper = "BEEKEEPER_TEST_CONFIG_WRITER"
	if key := os.Getenv(helper); key != "" {
		fmt.Println("ready")
		if _, err := io.Copy(io.Discard, os.Stdin); err != nil {
			t.Fatal(err)
		}
		if _, err := Set(os.Getenv("BEEKEEPER_TEST_CONFIG_PATH"), []Setting{{key, "1"}}); err != nil {
			t.Fatal(err)
		}
		return
	}
	for _, missing := range []bool{false, true} {
		t.Run(fmt.Sprintf("missing=%v", missing), func(t *testing.T) {
			dir := t.TempDir()
			target := filepath.Join(dir, "config.yaml")
			// Parsing the padding widens the stale-read window without external calls.
			if !missing {
				if err := os.WriteFile(target, []byte("# preserved\nmetrics: {models: {}}\n# "+strings.Repeat("padding ", 128*1024)+"\n"), 0o640); err != nil { //nolint:gosec // verify mode preservation
					t.Fatal(err)
				}
			}
			alias := filepath.Join(dir, "alias")
			if err := os.Symlink(dir, alias); err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(dir, "link.yaml")
			if !missing {
				if err := os.Symlink("config.yaml", link); err != nil {
					t.Fatal(err)
				}
			} else {
				link = filepath.Join(alias, "config.yaml")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			const writers = 8
			var commands []*exec.Cmd
			var barriers []io.WriteCloser
			var outputs []*bytes.Buffer
			var readers []*bufio.Reader
			for i := range writers {
				path := target
				if i%2 != 0 {
					path = link
				}
				cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSetConcurrentProcesses$") //nolint:gosec // run this test binary
				cmd.Env = append(os.Environ(), helper+"="+fmt.Sprintf("metrics.models.writer-%d.input", i), "BEEKEEPER_TEST_CONFIG_PATH="+path)
				in, err := cmd.StdinPipe()
				if err != nil {
					t.Fatal(err)
				}
				out, err := cmd.StdoutPipe()
				if err != nil {
					t.Fatal(err)
				}
				stderr := new(bytes.Buffer)
				cmd.Stderr = stderr
				if err := cmd.Start(); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if cmd.ProcessState == nil {
						_ = cmd.Process.Kill()
						_ = cmd.Wait()
					}
				})
				reader := bufio.NewReader(out)
				if line, err := reader.ReadString('\n'); err != nil || line != "ready\n" {
					t.Fatalf("helper readiness: %q, %v", line, err)
				}
				commands = append(commands, cmd)
				barriers = append(barriers, in)
				outputs = append(outputs, stderr)
				readers = append(readers, reader)
			}
			for _, in := range barriers {
				if err := in.Close(); err != nil {
					t.Fatal(err)
				}
			}
			for i, cmd := range commands {
				stdout, _ := io.ReadAll(readers[i])
				if err := cmd.Wait(); err != nil {
					t.Fatalf("writer %d: %v: %s", i, err, string(stdout)+outputs[i].String())
				}
			}
			for i := range writers {
				key := fmt.Sprintf("metrics.models.writer-%d.input", i)
				if value, ok, err := Get(target, key); err != nil || !ok || value != "1" {
					t.Errorf("successful edit lost: %s = %q, %v, %v", key, value, ok, err)
				}
			}
			if !missing {
				raw, err := os.ReadFile(filepath.Clean(target))
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Contains(raw, []byte("# preserved")) {
					t.Error("comment lost")
				}
				fi, err := os.Stat(target)
				if err != nil {
					t.Fatal(err)
				}
				if fi.Mode().Perm() != 0o640 {
					t.Errorf("mode = %v", fi.Mode())
				}
				fi, err = os.Lstat(link)
				if err != nil {
					t.Fatal(err)
				}
				if fi.Mode()&os.ModeSymlink == 0 {
					t.Error("symlink replaced")
				}
			}
		})
	}
}
