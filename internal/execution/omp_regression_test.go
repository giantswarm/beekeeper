package execution

import "testing"

func TestExecutionOmpAdmissionRegression(t *testing.T) {
	if !SupportedHarness("omp") {
		t.Fatal("omp has no SSH execution adapter")
	}
	f := testFixture(t)
	f.s.Config.Harnesses = map[string]HarnessConfig{"omp": {Executable: "/usr/local/bin/omp", Version: "18.14.2", ConfigDir: "/srv/omp-login"}}
	if err := f.s.Config.Validate(); err != nil {
		t.Fatalf("omp host configuration refused: %v", err)
	}
	f.host.Harnesses = []string{"omp"}
	if err := f.host.Validate(); err != nil {
		t.Fatalf("omp catalogue refused: %v", err)
	}
}
