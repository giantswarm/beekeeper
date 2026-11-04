package execution

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExecutionOmpPinnedVersionOutputFormats(t *testing.T) {
	const packagedPrefix = "omp/"
	for _, tc := range []struct {
		name, output string
		offered      bool
	}{
		{"legacy", "omp v" + fixtureOmpVersion, true},
		{"packaged", packagedPrefix + fixtureOmpVersion, true},
		{"bare", fixtureOmpVersion, true},
		{"mismatched", "omp/18.14.3", false},
		{"other program", "other/" + fixtureOmpVersion, false},
		{"trailing", packagedPrefix + fixtureOmpVersion + " extra", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, dir := ompFixture(t)
			script := strings.Replace(ompScript, "omp v18.14.2", tc.output, 1)
			if err := os.WriteFile(filepath.Join(dir, HarnessOmp), []byte(script), 0o700); err != nil { //nolint:gosec // test-owned executable fixture
				t.Fatal(err)
			}
			caps, err := f.s.Check(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if (caps.Harnesses[HarnessOmp] != "") != tc.offered {
				t.Fatalf("version %q: wrong readiness: %+v", tc.output, caps)
			}
			if !tc.offered && !strings.Contains(caps.Unavailable[HarnessOmp], "version") {
				t.Fatalf("mismatched pin lacks version refusal: %+v", caps)
			}
			if _, err := os.Stat(filepath.Join(dir, "prompts")); !os.IsNotExist(err) {
				t.Fatalf("readiness must not submit a model prompt: %v", err)
			}
		})
	}
}
