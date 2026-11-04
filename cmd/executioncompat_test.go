package cmd

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/execution"
)

func TestExecutionHostReleaseCompatibility(t *testing.T) {
	for _, version := range []string{"v0.171.0-rc.60", "v0.171.0-rc.61", "v0.172.0"} {
		t.Run(version, func(t *testing.T) {
			a, _ := sshStartTestApp(t)
			h := a.cfg.Agents.Hosts[sshTestSelector]
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

func TestHostsCheckFlagsCoordinatorReleaseMismatch(t *testing.T) {
	withCoordinatorVersion(t, hostNewRelease)
	if err := checkHostRelease(hostOldRelease); err == nil {
		t.Fatal("stale host release accepted")
	}
	if err := checkHostRelease(hostNewRelease); err != nil {
		t.Fatal(err)
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
