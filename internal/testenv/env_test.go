package testenv

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSetupIsolatesIdentityAndGit(t *testing.T) {
	const child = "TESTENV_CHILD"
	keys := []string{"BEEKEEPER_CODEX_AGENT", "BEEKEEPER_AGENT_NAME", "BEEKEEPER_OMP_AGENT", "BEEKEEPER_AGY_AGENT", "CLAUDE_CODE_SESSION_ID", "CLAUDE_CODE_HOST_SESSION_ID", "CLAUDE_SESSION_ID", "CLAUDE_CODE_SESSION_NAME", "CLAUDE_CONFIG_DIR", "CODEX_HOME", "GIT_DIR", "GIT_AUTHOR_NAME"}
	if os.Getenv(child) == "1" {
		Setup()
		for _, key := range keys {
			if _, ok := os.LookupEnv(key); ok {
				t.Errorf("inherited %s", key)
			}
		}
		if os.Getenv("BEEKEEPER_TEST_HELPER") != "fixture" {
			t.Error("lost child helper dispatch flag")
		}
		if os.Getenv("BEEKEEPER_TEST_DATABASE_URL") != "test-database" {
			t.Error("lost integration database input")
		}
		dir := t.TempDir()
		for _, args := range [][]string{{"init", "-q", dir}, {"-C", dir, "commit", "-q", "--allow-empty", "-m", "fixture"}} {
			if out, err := exec.Command("git", args...).CombinedOutput(); err != nil { //nolint:gosec // fixed Git fixture commands in a test-owned directory
				t.Fatalf("fixture git: %v: %s", err, out)
			}
		}
		out, err := exec.Command("git", "-C", dir, "log", "-1", "--format=%an <%ae>").Output() //nolint:gosec // read this test's fixture commit
		if err != nil || strings.TrimSpace(string(out)) != "Test <test@example.com>" {
			t.Fatalf("fixture identity: %v: %s", err, out)
		}
		if err := exec.Command("git", "config", "--get", "test.hostile").Run(); err == nil {
			t.Fatal("read caller git config")
		}
		return
	}
	global := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(global, []byte("[commit]\n gpgsign = true\n[gpg]\n program = missing-test-signer\n[test]\n hostile = true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	c := exec.Command(os.Args[0], "-test.run=^TestSetupIsolatesIdentityAndGit$") //nolint:gosec // reexecute this test binary with a fixed test selector
	c.Env = append(os.Environ(), child+"=1", "BEEKEEPER_TEST_HELPER=fixture", "GIT_CONFIG_GLOBAL="+global, "BEEKEEPER_TEST_DATABASE_URL=test-database", "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=user.name", "GIT_CONFIG_VALUE_0=Hostile")
	for _, key := range keys {
		c.Env = append(c.Env, key+"=caller")
	}
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("isolated suite: %v: %s", err, out)
	}
}
