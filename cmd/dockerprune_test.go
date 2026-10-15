package cmd

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/lease"
	"github.com/giantswarm/beekeeper/internal/state"
)

// fakeDocker is a docker CLI with 7 GiB of images over a 4 GiB cap and
// 6 GiB of build cache over a 4 GiB cap: three unused images and one a
// kind lab's node holds, each of 2 GiB. A removal or prune shrinks what
// df says.
type fakeDocker struct {
	images, cache int64
	removed       [][]string
	pruned        []string
}

func (f *fakeDocker) run(_ context.Context, args ...string) ([]byte, error) {
	switch {
	case slices.Equal(args, []string{"system", "df", "--format", "{{json .}}"}):
		return []byte(`{"Type":"Images","Size":"` + bytesStr(f.images) + `"}` + "\n" +
			`{"Type":"Build Cache","Size":"` + bytesStr(f.cache) + `"}` + "\n"), nil
	case slices.Equal(args, []string{"system", "df", "-v", "--format", "{{json .}}"}):
		return []byte(`{"Images":[
{"Containers":"0","CreatedAt":"2026-10-14 01:00:00 +0000 UTC","ID":"sha256:new","Repository":"x","Tag":"dev-3","UniqueSize":"2147483648B"},
{"Containers":"0","CreatedAt":"2026-10-12 01:00:00 +0000 UTC","ID":"sha256:mid","Repository":"x","Tag":"dev-2","UniqueSize":"2147483648B"},
{"Containers":"0","CreatedAt":"2026-10-10 01:00:00 +0000 UTC","ID":"sha256:old","Repository":"x","Tag":"dev-1","UniqueSize":"2147483648B"},
{"Containers":"0","CreatedAt":"2026-10-01 01:00:00 +0000 UTC","ID":"sha256:lab","Repository":"gsoci.azurecr.io/giantswarm/y","Tag":"dev","UniqueSize":"2147483648B"},
{"Containers":"2","CreatedAt":"2026-09-01 01:00:00 +0000 UTC","ID":"sha256:node","Repository":"kindest/node","Tag":"v1","UniqueSize":"2147483648B"}
]}`), nil
	case args[0] == "ps":
		return []byte("agentlab-control-plane\trunning\n"), nil
	case args[0] == dockerExec:
		return []byte(`{"images":[{"id":"sha256:inside","repoTags":["gsoci.azurecr.io/giantswarm/y:dev"]}]}`), nil
	case args[0] == "image" && args[1] == "rm":
		f.removed = append(f.removed, args[2:])
		f.images -= 2 << 30
		return nil, nil
	case args[0] == "builder" && args[1] == "prune":
		f.pruned = args[2:]
		f.cache = 3 << 30
		return nil, nil
	}
	return nil, errors.New("unexpected docker " + strings.Join(args, " "))
}

// dockerExec is the docker verb the fake answers with a kind node's images.
const dockerExec = "exec"

func bytesStr(n int64) string { return strconv.FormatInt(n, 10) + "B" }

func dockerApp(t *testing.T) (*app, *fakeDocker) {
	t.Helper()
	f := &fakeDocker{images: 7 << 30, cache: 6 << 30}
	dockerCLI = f.run
	t.Cleanup(func() { dockerCLI = nil })
	dir := t.TempDir()
	store, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{StateDir: dir, Doctor: config.Doctor{DockerImagesMaxGiB: 4, DockerBuildCacheMaxGiB: 4}}
	return &app{cfg: cfg, store: store, now: time.Now(), out: &bytes.Buffer{}}, f
}

func noHolds() string { return "" }

func TestPruneDockerKeepsUnderTheCapsAndLogs(t *testing.T) {
	a, f := dockerApp(t)
	r := a.pruneDocker(false, noHolds)
	if r.err != nil || r.removed != 2 || !r.pruned {
		t.Fatalf("prune = %+v; want 2 images removed and the build cache pruned", r)
	}
	// 7 GiB down to 3 GiB (three quarters of the 4 GiB cap): the two
	// oldest unused images go; the lab's, older still, and the node's stay.
	if want := [][]string{{"x:dev-1"}, {"x:dev-2"}}; !slices.EqualFunc(f.removed, want, slices.Equal) {
		t.Errorf("removed %v; want %v", f.removed, want)
	}
	if want := []string{"--force", "--max-used-space", "3221225472"}; !slices.Equal(f.pruned, want) {
		t.Errorf("builder prune %v; want %v", f.pruned, want)
	}
	want := "Docker: images 7.0 GiB, cap 4.0 GiB; build cache 6.0 GiB, cap 4.0 GiB (doctor.dockerImagesMaxGiB, doctor.dockerBuildCacheMaxGiB); " +
		"2 unused images removed (about 4.0 GiB), images now 3.0 GiB; build cache pruned to 3.0 GiB"
	if got := r.String(); got != want {
		t.Errorf("line =\n %q\nwant\n %q", got, want)
	}
	evs, err := a.store.Events(10, func(e state.Event) bool { return e.Verb == dockerVerb })
	if err != nil || len(evs) != 1 || evs[0].Detail != want {
		t.Errorf("logged %+v, %v; want one %s event with the line", evs, err, dockerVerb)
	}
}

func TestPruneDockerNeverRemovesAnImageAKindLabHolds(t *testing.T) {
	a, f := dockerApp(t)
	a.cfg.Doctor.DockerImagesMaxGiB = 1
	a.cfg.Doctor.DockerBuildCacheMaxGiB = -1
	r := a.pruneDocker(false, noHolds)
	if r.err != nil || r.removed != 3 {
		t.Fatalf("prune = %+v; want every unused image removed", r)
	}
	for _, refs := range f.removed {
		if slices.Contains(refs, "gsoci.azurecr.io/giantswarm/y:dev") || slices.Contains(refs, "kindest/node:v1") {
			t.Errorf("removed %v, which a kind lab uses", refs)
		}
	}
	if f.pruned != nil {
		t.Error("the build cache was pruned with its cap off")
	}
}

func TestPruneDockerWaitsWhileAnImportRuns(t *testing.T) {
	a, f := dockerApp(t)
	r := a.pruneDocker(false, func() string { return "image imports or builds run (PIDs 4242)" })
	if r.acted() || f.removed != nil || f.pruned != nil {
		t.Fatalf("pruned while an import ran: %+v", r)
	}
	if !strings.HasSuffix(r.String(), "its prune waits: image imports or builds run (PIDs 4242)") || !r.notable() {
		t.Errorf("line = %q", r)
	}
	// An import that begins mid-trim stops it at the next image.
	a, f = dockerApp(t)
	a.cfg.Doctor.DockerBuildCacheMaxGiB = -1
	calls := 0
	r = a.pruneDocker(false, func() string {
		if calls++; calls > 2 {
			return "image imports or builds run (PIDs 7)"
		}
		return ""
	})
	if r.removed != 1 || len(f.removed) != 1 || r.err != nil || r.held == "" {
		t.Errorf("prune = %+v; want one image removed, then the wait", r)
	}
}

func TestFreshLabLeaseHoldsThePrune(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	labs := []lease.Holder{{Env: labOne, Since: now.Add(-3 * time.Hour).Format(time.RFC3339)}}
	if got := freshLab(labs, now); got != "" {
		t.Errorf("a lease of three hours holds: %q", got)
	}
	labs = append(labs, lease.Holder{Env: "agentlab-2", Since: now.Add(-20 * time.Minute).Format(time.RFC3339)})
	if got := freshLab(labs, now); got != "lab lease agentlab-2 was claimed 20m0s ago" {
		t.Errorf("fresh = %q", got)
	}
	if got := freshLab([]lease.Holder{{Env: labOne, Purpose: "(being claimed)"}}, now); got != "lab lease "+labOne+" is being claimed" {
		t.Errorf("being claimed = %q", got)
	}
}

func TestPruneDockerDryRunUnderTheCapOrOff(t *testing.T) {
	a, f := dockerApp(t)
	r := a.pruneDocker(true, noHolds)
	if r.acted() || f.removed != nil || !strings.HasSuffix(r.String(), "would prune images to 3.0 GiB and build cache to 3.0 GiB") {
		t.Errorf("dry run = %+v, %q", r, r)
	}
	a.cfg.Doctor.DockerImagesMaxGiB, a.cfg.Doctor.DockerBuildCacheMaxGiB = 40, 20
	if r := a.pruneDocker(false, noHolds); r.acted() || r.notable() {
		t.Errorf("under the caps: %+v", r)
	}
	dockerCLI = nil
	if r := a.pruneDocker(false, noHolds); !r.off() || r.notable() {
		t.Errorf("no docker: %+v", r)
	}
}
