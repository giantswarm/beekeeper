package execution

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	readinessPassed     = "passed"
	readinessFailed     = "failed"
	readinessNotChecked = "not checked"
)

// Decode the wire report so this regression can also run against the base
// without depending on the new Go field.
func readinessReport(t *testing.T, caps Capabilities, harness string) map[string]string {
	t.Helper()
	raw, err := json.Marshal(caps)
	if err != nil {
		t.Fatal(err)
	}
	var report struct {
		Readiness map[string]map[string]string `json:"readiness"`
	}
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatal(err)
	}
	result := report.Readiness[harness]
	if result == nil {
		t.Fatalf("host check omitted %s version/login/guard report", harness)
	}
	return result
}

func TestHostReadinessDistinguishesLoginAndVersionFailures(t *testing.T) {
	for _, harness := range []string{HarnessCodex, HarnessAgy} {
		t.Run(harness, func(t *testing.T) {
			var f fixture
			var dir, version string
			if harness == HarnessCodex {
				f, dir = codexFixture(t)
				version = fixtureCodexVersion
			} else {
				f, dir = agyFixture(t)
				version = fixtureAgyVersion
			}
			ctx := context.Background()
			check := func(wantVersion, wantLogin string, advertised bool) {
				t.Helper()
				caps, err := f.s.Check(ctx)
				if err != nil {
					t.Fatal(err)
				}
				result := readinessReport(t, caps, harness)
				if result["version"] != version || result["versionCheck"] != wantVersion || result["login"] != wantLogin || result["guard"] != readinessPassed {
					t.Fatalf("wrong readiness report: %v", result)
				}
				if (caps.Harnesses[harness] != "") != advertised {
					t.Fatalf("advertised failed harness: %+v", caps)
				}
				if caps.Harnesses[HarnessClaude] == "" {
					t.Fatal("healthy Claude harness disappeared")
				}
			}
			check(readinessPassed, readinessPassed, true)
			if err := os.WriteFile(filepath.Join(dir, "logged-out"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			check(readinessPassed, readinessFailed, false)
			// The managed environment intentionally drops arbitrary variables.
			// Change the fake executable instead of injecting its version.
			if err := os.WriteFile(filepath.Join(dir, harness), []byte("#!/bin/sh\necho '"+harness+" version 0.0.0'\n"), 0o700); err != nil { //nolint:gosec // test-owned executable fixture
				t.Fatal(err)
			}
			check(readinessFailed, readinessNotChecked, false)
		})
	}
}

// A host with only one harness must retain the same diagnostics as a mixed
// host. Exercise the wire boundary as well as the host's admission decision.
func TestSingleHarnessReadinessFailureSurvivesSSH(t *testing.T) {
	for _, harness := range []string{HarnessCodex, HarnessOmp, HarnessAgy} {
		t.Run(harness, func(t *testing.T) {
			var f fixture
			var dir string
			switch harness {
			case HarnessCodex:
				f, dir = codexFixture(t)
			case HarnessOmp:
				f, dir = ompFixture(t)
			case HarnessAgy:
				f, dir = agyFixture(t)
			}
			f.s.Config.Harnesses = map[string]HarnessConfig{harness: f.s.Config.Harnesses[harness]}
			digest, err := f.s.Prepare(f.s.Policy())
			if err != nil {
				t.Fatal(err)
			}
			f.host.Harnesses, f.host.PolicyDigest = []string{harness}, digest
			f.r.PolicyDigest = digest
			if err := os.WriteFile(filepath.Join(dir, "logged-out"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			tr := f.transport(t, nil)
			caps, err := tr.Check(context.Background())
			if err == nil || len(caps.Harnesses) != 0 || caps.Unavailable[harness] == "" {
				t.Fatalf("failed harness must refuse admission and retain diagnostics: %+v %v", caps, err)
			}
			report := readinessReport(t, caps, harness)
			if report["versionCheck"] != readinessPassed || report["login"] != readinessFailed {
				t.Fatalf("wrong failed readiness report: %v", report)
			}
			if _, err := tr.Start(context.Background(), f.r); err == nil || !strings.Contains(err.Error(), "login") {
				t.Fatalf("logged-out harness admission did not preserve its login refusal: %v", err)
			}
			if len(f.launcher.starts) != 0 {
				t.Fatal("failed readiness launched a worker")
			}
		})
	}
}
