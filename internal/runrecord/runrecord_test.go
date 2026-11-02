package runrecord

import (
	"strings"
	"testing"
	"time"
)

const (
	testScope = "memcap-1-2.scope"
	testRepo  = "o/r"
	testPod   = "remote:pod-abc.3f2a9c1b"
)

func TestDetails(t *testing.T) {
	start := StartDetail(RemoteID("builder-pod", "3f2a9c1b-7e6d-4c2a-9f0e-1d2c3b4a5f6e"), Facts("giantswarm/beekeeper", "16a89d963f2a9c1b")+Cache(true, "build-workers"), "go test ./...")
	if !strings.HasPrefix(start, "remote:builder-pod.3f2a9c1b giantswarm/beekeeper@16a89d96 ns build-workers cache warm: go test ./...") {
		t.Errorf("start %q", start)
	}
	end := EndDetail("remote:builder-pod.3f2a9c1b", 37, 95*time.Second, "failed: --- FAIL: TestFoo", "go test ./...")
	if end != "remote:builder-pod.3f2a9c1b exit 37 after 1m35s, failed: --- FAIL: TestFoo: go test ./..." {
		t.Errorf("end %q", end)
	}
	local := StartDetail("memcap-2516344-492870.scope", "", "make lint")
	if local != "memcap-2516344-492870.scope: make lint" {
		t.Errorf("local %q", local)
	}
	fb := FallbackDetail(Facts("o/r", "0123456789ab"), "cluster unreachable: dial 1.2.3.4", "go build ./...")
	if fb != "local o/r@01234567 fell back (cluster unreachable; dial 1.2.3.4): go build ./..." {
		t.Errorf("fallback %q", fb)
	}
}

// The cache fact reads from the start's facts only: a command named warm
// on a cold record, or a failed run's tail, is output, not cache state.
func TestWarm(t *testing.T) {
	for _, tc := range []struct {
		detail string
		warm   bool
		ok     bool
	}{
		{StartDetail(testPod, Facts(testRepo, "0123456789ab")+Cache(true, "build-workers"), "go test ./..."), true, true},
		{StartDetail(testPod, Facts(testRepo, "0123456789ab")+Cache(false, ""), "go test ./... -run warm"), false, true},
		{testPod + " cache warm: go build ./...", true, true},
		// the facts hold it; the tail of a failed run and the command never do
		{testPod + " exit 1 after 2s, failed: cache warm output: go build ./...", false, false},
		{testScope + " slot 1 max 12G: go test ./...", false, false},
	} {
		warm, ok := Warm(tc.detail)
		if warm != tc.warm || ok != tc.ok {
			t.Errorf("Warm(%q) = %v,%v, want %v,%v", tc.detail, warm, ok, tc.warm, tc.ok)
		}
	}
}

func TestReaders(t *testing.T) {
	for _, tc := range []struct {
		detail string
		id     string
		remote bool
		repo   string
		rc     int
		took   time.Duration
		end    bool
	}{
		{testScope + " slot 1 max 12G: go test ./...", testScope, false, "", 0, 0, false},
		{testScope + " " + testRepo + "@01234567 slot 1 max 12G: go test ./...", testScope, false, testRepo, 0, 0, false},
		{testPod + " " + testRepo + "@01234567 ns build-workers cache cold: go test ./...", testPod, true, testRepo, 0, 0, false},
		// the tail of a failed remote run is output: its text is no repository
		{testPod + " exit 1 after 2s, failed: x/y@deadbeef leaked tail: go build ./...", testPod, true, "", 1, 2 * time.Second, true},
		{testScope + " exit 0 after 1m35s: make lint", testScope, false, "", 0, 95 * time.Second, true},
		{"local " + testRepo + "@01234567 fell back (cluster unreachable): go build ./...", "local", false, testRepo, 0, 0, false},
		// a record of an older release: no facts
		{testScope + " exit 1 after 3s: make", testScope, false, "", 1, 3 * time.Second, true},
	} {
		if got := ID(tc.detail); got != tc.id {
			t.Errorf("ID(%q) = %q, want %q", tc.detail, got, tc.id)
		}
		if got := Remote(tc.detail); got != tc.remote {
			t.Errorf("Remote(%q) = %v, want %v", tc.detail, got, tc.remote)
		}
		if got := Repository(tc.detail); got != tc.repo {
			t.Errorf("Repository(%q) = %q, want %q", tc.detail, got, tc.repo)
		}
		rc, took, ok := ParseEnd(tc.detail)
		if ok != tc.end || rc != tc.rc || took != tc.took {
			t.Errorf("ParseEnd(%q) = %d,%v,%v, want %d,%v,%v", tc.detail, rc, took, ok, tc.rc, tc.took, tc.end)
		}
	}
}
