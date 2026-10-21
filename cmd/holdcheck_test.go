package cmd

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// holdRunner runs hold subcommands under a configuration with extra lines
// and returns their output and exit code.
func holdRunner(t *testing.T, extra string) func(args ...string) (string, int) {
	t.Helper()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	body := fmt.Sprintf("stateDir: %s\nleaseDir: %s\nresources: [kind-1]\n%s", filepath.Join(dir, "state"), filepath.Join(dir, "leases"), extra)
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return func(args ...string) (string, int) {
		t.Helper()
		var out bytes.Buffer
		a := &app{cfgPath: cfg, as: "checker", out: &out}
		if err := a.load(); err != nil {
			t.Fatal(err)
		}
		cmd := a.holdCmd()
		cmd.SetArgs(args)
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SilenceErrors, cmd.SilenceUsage = true, true
		if err := cmd.Execute(); err != nil {
			out.WriteString(err.Error())
			return out.String(), Code(err)
		}
		return out.String(), 0
	}
}

// hold check always prints a verdict, and a lookup that fails is named as
// the failure with an exit code other than the verdict's.
func TestHoldCheckPrintsAVerdictOrNamesTheFailedLookup(t *testing.T) {
	local := holdRunner(t, "")
	if out, code := local("check", "o/r"); code != 0 || out != "o/r is not held\n" {
		t.Errorf("not held: exit %d: %q", code, out)
	}
	if out, code := local("set", "o/r", "-r", "fixing main"); code != 0 {
		t.Fatalf("set: exit %d: %s", code, out)
	}
	if out, code := local("check", "o/r"); code != ExitRefused || !strings.Contains(out, "o/r is held by") || !strings.Contains(out, "fixing main") {
		t.Errorf("held: exit %d: %q", code, out)
	}
	// A hold waiting on the repository's release still holds its merges and
	// says that it lets the promote through.
	if out, code := local("set", "o/r", "-r", "promote first", "--lift-when", "gh release view v1.2.4 --repo o/r"); code != 0 {
		t.Fatalf("set: exit %d: %s", code, out)
	}
	if out, code := local("check", "o/r"); code != ExitRefused || !strings.Contains(out, "o/r except its promote is held by") {
		t.Errorf("held but for its promote: exit %d: %q", code, out)
	}
	central := holdRunner(t, "central: {context: lab, muster: "+filepath.Join(t.TempDir(), "no-muster")+"}\nlanes: [{name: portal, installation: gazelle, repositories: [o/r]}]\n")
	out, code := central("check", "o/r")
	if code == 0 || code == ExitRefused || !strings.HasPrefix(out, "the central hold lookup for o/r failed, no verdict: ") || strings.Contains(out, "not held") {
		t.Errorf("failed central lookup: exit %d: %q", code, out)
	}
}

// hold set --except-session records the session the hold admits and names
// it with the target; a github hold and a central one make no exception.
func TestHoldSetExceptSessionNamesTheAdmittedSession(t *testing.T) {
	local := holdRunner(t, "")
	if out, code := local("set", "o/r", "-r", "promoting", "--except-session", "BK 0.156 promoter"); code != 0 || !strings.Contains(out, `held o/r except session "BK 0.156 promoter"`) {
		t.Errorf("set: exit %d: %q", code, out)
	}
	if out, code := local("check", "o/r"); code != ExitRefused || !strings.Contains(out, `o/r except session "BK 0.156 promoter" is held by`) {
		t.Errorf("check: exit %d: %q", code, out)
	}
	if out, code := local("set", "github", "-r", "budget", "--except-session", "x"); code != ExitUsage {
		t.Errorf("github: exit %d: %q", code, out)
	}
	central := holdRunner(t, "central: {context: lab, muster: "+filepath.Join(t.TempDir(), "no-muster")+"}\nlanes: [{name: portal, installation: gazelle, repositories: [o/r]}]\n")
	if out, code := central("set", "o/r", "-r", "promoting", "--except-session", "x"); code != ExitUsage || !strings.Contains(out, "--except-session") {
		t.Errorf("central: exit %d: %q", code, out)
	}
}
