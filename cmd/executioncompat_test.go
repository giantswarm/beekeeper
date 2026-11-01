package cmd

import (
	"context"
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
