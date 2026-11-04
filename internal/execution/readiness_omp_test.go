package execution

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestOmpReadinessDistinguishesLoginAndGuardFailures(t *testing.T) {
	for _, failure := range []string{"", "logged-out", "extension-error"} {
		t.Run("failure="+failure, func(t *testing.T) {
			f, dir := ompFixture(t)
			if failure != "" {
				if err := os.WriteFile(filepath.Join(dir, failure), nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			caps, err := f.s.Check(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			report := readinessReport(t, caps, HarnessOmp)
			wantLogin, wantGuard := readinessPassed, readinessPassed
			if failure == "logged-out" {
				wantLogin, wantGuard = readinessFailed, readinessNotChecked
			}
			if failure == "extension-error" {
				wantGuard = readinessFailed
			}
			if report["version"] != fixtureOmpVersion || report["versionCheck"] != readinessPassed || report["login"] != wantLogin || report["guard"] != wantGuard {
				t.Fatalf("wrong readiness report: %v", report)
			}
			if (caps.Harnesses[HarnessOmp] != "") != (failure == "") {
				t.Fatalf("advertisement ignores readiness: %+v", caps)
			}
			if failure != "" && caps.Unavailable[HarnessOmp] == "" {
				t.Fatal("missing refusal explanation")
			}
		})
	}
}
