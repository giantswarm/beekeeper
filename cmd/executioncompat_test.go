package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/execution"
)

func TestExecutionHostReleaseCompatibility(t *testing.T) {
	withCoordinatorVersion(t, hostNewRelease)
	for _, version := range []string{"v0.171.0-rc.60", "v0.171.0-rc.61", "v0.172.0"} {
		t.Run(version, func(t *testing.T) {
			a, _ := sshStartTestApp(t)
			h := a.cfg.Agents.Hosts[sshTestSelector]
			if err := a.admitExecution(sshTestSelector, h); err != nil {
				t.Fatalf("release mismatch blocked compatible probe: %v", err)
			}
			caps, err := a.executionProbe(context.Background(), h)
			if err != nil {
				t.Fatal(err)
			}
			caps.Version = version
			a.executionProbe = func(context.Context, execution.Host) (execution.Capabilities, error) { return caps, nil }
			if got, err := a.probeExecution(context.Background(), h); err != nil || got.Version != version {
				t.Fatalf("compatible host release: version=%q err=%v", got.Version, err)
			}
			for _, mismatch := range []string{"identity", "protocol", "policy", "harness", "resources"} {
				t.Run(mismatch, func(t *testing.T) {
					bad := caps
					switch mismatch {
					case "identity":
						bad.Identity = "another-host"
					case "protocol":
						bad.Protocol++
					case "policy":
						bad.PolicyDigest = execution.Digest([]byte("different policy"))
					case "harness":
						bad.Harnesses = nil
					case "resources":
						bad.Resources.ObservedAt = time.Time{}
					}
					a.executionProbe = func(context.Context, execution.Host) (execution.Capabilities, error) { return bad, nil }
					if _, err := a.probeExecution(context.Background(), h); err == nil {
						t.Fatal("accepted incompatible host")
					}
				})
			}
		})
	}
}

func TestHostsCheckPrintsFailedHarnessReadiness(t *testing.T) {
	const (
		readinessCheckPassed = "passed"
		readinessCheckFailed = "failed"
	)
	a, _ := sshStartTestApp(t)
	failure := errors.New("codex: independently provisioned host login required")
	a.executionProbe = func(context.Context, execution.Host) (execution.Capabilities, error) {
		return execution.Capabilities{
			Harnesses:   map[string]string{},
			Unavailable: map[string]string{execution.HarnessCodex: failure.Error()},
			Readiness: map[string]execution.HarnessReadiness{
				execution.HarnessCodex: {Version: codexSSHFixtureVersion, VersionCheck: readinessCheckPassed, Login: readinessCheckFailed, Guard: readinessCheckPassed},
			},
		}, failure
	}
	out, err := runAgents(a, "hosts", "check", sshTestSelector)
	if !errors.Is(err, failure) {
		t.Fatalf("hosts check must fail: %v", err)
	}
	var caps execution.Capabilities
	if err := json.Unmarshal([]byte(out), &caps); err != nil {
		t.Fatalf("missing JSON diagnostics: %q: %v", out, err)
	}
	if caps.Readiness[execution.HarnessCodex].Login != readinessCheckFailed || len(caps.Harnesses) != 0 {
		t.Fatalf("wrong readiness diagnostics: %+v", caps)
	}
	if len(caps.ResourceAudit) != 1 || caps.ResourceAudit[0].State != execution.Unknown {
		t.Fatalf("missing unavailable audit diagnostics: %+v", caps.ResourceAudit)
	}
}

func TestLegacyHostSourcePreparationRejectedBeforeReservation(t *testing.T) {
	a, brief := sshStartTestApp(t)
	h := a.cfg.Agents.Hosts[sshTestSelector]
	caps, err := a.executionProbe(context.Background(), h)
	if err != nil {
		t.Fatal(err)
	}
	// Legacy hosts pass the ordinary protocol, policy and publication checks,
	// but omit the source preparation capability from their check response.
	caps.Version = "v0.171.0-rc.111"
	caps.SourcePreparation = false
	a.executionProbe = func(context.Context, execution.Host) (execution.Capabilities, error) { return caps, nil }
	a.executionTransport = func(execution.Host) execution.Transport {
		t.Fatal("legacy host contacted after admission")
		return nil
	}
	if _, err := a.probeExecution(context.Background(), h); err == nil || !strings.Contains(err.Error(), "source preparation") {
		t.Fatalf("host check accepted missing source preparation: %v", err)
	}
	err = a.startSSHExecution(context.Background(), "fixture worker", brief, executionStartOptions{Host: sshTestSelector, Repository: sshTestRepository, GitRef: mainBranch, Harness: claudeComm, Model: bgModel})
	if err == nil || !strings.Contains(err.Error(), "source preparation") {
		t.Fatalf("start accepted missing source preparation: %v", err)
	}
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Starts) != 0 || len(st.Agents) != 0 {
		t.Fatal("incompatible host reserved an execution")
	}
}

func TestHostsCheckReportsPinOlderThanCoordinatorRepositoriesNeedBeforeStart(t *testing.T) {
	a, brief := sshStartTestApp(t)
	withCoordinatorVersion(t, "dev")
	repoDir := filepath.Join(a.cfg.Projects.Dir, "beekeeper")
	if err := os.MkdirAll(repoDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "go.mod"), []byte("module github.com/giantswarm/beekeeper\n\ngo 1.26.0\n\ntoolchain go1.27.4\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := a.cfg.Agents.Hosts[sshTestSelector]
	caps, err := a.executionProbe(context.Background(), h)
	if err != nil {
		t.Fatal(err)
	}
	caps.Tools = map[string]string{execution.ToolGitHub: "2.83.0", execution.ToolGo: "1.27.3"}
	a.executionProbe = func(context.Context, execution.Host) (execution.Capabilities, error) { return caps, nil }
	out, err := runAgents(a, "hosts", "check", sshTestSelector)
	if Code(err) != ExitRefused || !strings.Contains(err.Error(), "pinned 1.27.3 is older than 1.27.4") {
		t.Fatalf("hosts check with stale Go pin: out=%s err=%v", out, err)
	}
	if err := a.startSSHExecution(context.Background(), "fixture worker", brief, executionStartOptions{Host: sshTestSelector, Repository: sshTestRepository, GitRef: mainBranch, Harness: claudeComm, Model: bgModel}); Code(err) != ExitRefused || !strings.Contains(err.Error(), "pinned 1.27.3 is older than 1.27.4") {
		t.Fatalf("start with stale Go pin: %v", err)
	}
	st, err := a.store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Starts) != 0 || len(st.Agents) != 0 {
		t.Fatal("host with stale Go pin reserved an execution")
	}
}
