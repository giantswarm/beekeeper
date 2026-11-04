package execution

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
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
