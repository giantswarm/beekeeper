package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/dockerprune"
	"github.com/giantswarm/beekeeper/internal/lease"
	"github.com/giantswarm/beekeeper/internal/state"
)

// fakeDocker is a docker CLI with 7 GiB of images over a 4 GiB cap and
// 6 GiB of build cache over a 4 GiB cap: three unused images and one a
// kind lab's node holds, each of 2 GiB. A removal shrinks what df says by
// shrink (2 GiB, less when the image shares layers), a prune likewise. An
// image was last tagged at its tagged time, never without one.
type fakeDocker struct {
	images, cache, shrink int64
	tagged                map[string]time.Time
	removed               [][]string
	pruned                []string
	// cacheRecords are the build cache's records the fake answers
	// `builder du` with; a per-record prune removes one and shrinks the
	// cache size, and a record the map does not list is in use or unreadable
	// and stays.
	cacheRecords map[string]fakeCacheRecord
	// builderRefused are the records the fake refuses to remove.
	builderRefused map[string]bool
	// builderRemoved are the records the per-record prunes took.
	builderRemoved []string
}

type fakeCacheRecord struct {
	size int64
	// usedAgo is the record's "LastUsedAt" as docker prints it.
	usedAgo string
	// free says docker calls the record reclaimable.
	free bool
}

func (f *fakeDocker) run(_ context.Context, args ...string) ([]byte, error) {
	switch {
	case slices.Equal(args, []string{"system", "df", "--format", "{{json .}}"}):
		return []byte(`{"Type":"Images","Size":"` + bytesStr(f.images) + `"}` + "\n" +
			`{"Type":"Build Cache","Size":"` + bytesStr(f.cache) + `"}` + "\n"), nil
	case slices.Equal(args, []string{"system", "df", "-v", "--format", "{{json .}}"}):
		return []byte(`{"Images":[
{"Containers":"0","CreatedAt":"2026-10-14 01:00:00 +0000 UTC","ID":"sha256:new","Repository":"x","Tag":"dev-3","Size":"2147483648B","UniqueSize":"2147483648B"},
{"Containers":"0","CreatedAt":"2026-10-12 01:00:00 +0000 UTC","ID":"sha256:mid","Repository":"x","Tag":"dev-2","Size":"2147483648B","UniqueSize":"2147483648B"},
{"Containers":"0","CreatedAt":"2026-10-10 01:00:00 +0000 UTC","ID":"sha256:old","Repository":"x","Tag":"dev-1","Size":"2147483648B","UniqueSize":"2147483648B"},
{"Containers":"0","CreatedAt":"2026-10-01 01:00:00 +0000 UTC","ID":"sha256:lab","Repository":"gsoci.azurecr.io/giantswarm/y","Tag":"dev","Size":"2147483648B","UniqueSize":"2147483648B"},
{"Containers":"2","CreatedAt":"2026-09-01 01:00:00 +0000 UTC","ID":"sha256:node","Repository":"kindest/node","Tag":"v1","Size":"2147483648B","UniqueSize":"2147483648B"}
]}`), nil
	case args[0] == dockerImage && args[1] == "inspect":
		var out []byte
		for _, id := range args[4:] {
			js, _ := json.Marshal(f.tagged[id])
			refs, _ := json.Marshal(map[string][]string{
				"sha256:old": {devOne}, "sha256:mid": {devTwo}, "sha256:new": {devThree},
				"sha256:lab": {labRef}, "sha256:node": {"kindest/node:v1"},
			}[id])
			out = fmt.Appendf(out, "%s\t%s\t%s\n", id, js, refs)
		}
		return out, nil
	case args[0] == "ps":
		return []byte("agentlab-control-plane\trunning\n"), nil
	case args[0] == dockerExec:
		return []byte(`{"images":[{"id":"sha256:inside","repoTags":["gsoci.azurecr.io/giantswarm/y:dev"]}]}`), nil
	case args[0] == dockerImage && args[1] == "rm":
		if args[2] != "--no-prune" {
			return nil, errors.New("image removal must disable parent pruning")
		}
		f.removed = append(f.removed, args[3:])
		f.images -= f.shrink
		return nil, nil
	case args[0] == "builder" && args[1] == "du":
		var out []byte
		for id, rec := range f.cacheRecords {
			out = fmt.Appendf(out, `{"ID":%q,"Size":%q,"Reclaimable":%t,"LastUsedAt":%q}`+"\n", id, bytesStr(rec.size), rec.free, rec.usedAgo)
		}
		return out, nil
	case args[0] == "builder" && args[1] == litPrune:
		f.pruned = args[2:]
		if len(args) == 5 && args[2] == pruneForceFlag && args[3] == "--filter" && strings.HasPrefix(args[4], "id=") {
			id := strings.TrimPrefix(args[4], "id=")
			if f.builderRefused[id] {
				return nil, errors.New("docker builder prune: failed to remove " + id)
			}
			rec, ok := f.cacheRecords[id]
			if !ok || !rec.free {
				return nil, errors.New("docker builder prune: " + id + " is not reclaimable")
			}
			f.builderRemoved = append(f.builderRemoved, id)
			delete(f.cacheRecords, id)
			f.cache -= rec.size
			return []byte("Total:\t0B\n"), nil
		}
		return nil, errors.New("unexpected builder prune " + strings.Join(args[2:], " "))
	}
	return nil, errors.New("unexpected docker " + strings.Join(args, " "))
}

// The fake's images by the reference a removal names them with.
const (
	devOne, devTwo, devThree = "x:dev-1", "x:dev-2", "x:dev-3"
	labRef                   = "gsoci.azurecr.io/giantswarm/y:dev"
)

// dockerExec is the docker verb the fake answers with a kind node's images.
const dockerExec = "exec"

// pruneForceFlag is the flag that makes `builder prune` act without asking.
const pruneForceFlag = "--force"

// dockerImage is the docker command the fake inspects and removes images with.
const dockerImage = "image"

func bytesStr(n int64) string { return strconv.FormatInt(n, 10) + "B" }

func dockerApp(t *testing.T) (*app, *fakeDocker) {
	t.Helper()
	f := &fakeDocker{images: 7 << 30, cache: 6 << 30, shrink: 2 << 30, tagged: map[string]time.Time{},
		cacheRecords: map[string]fakeCacheRecord{
			"cache-old": {size: 2 << 30, usedAgo: "5 days ago", free: true},
			"cache-mid": {size: 1 << 30, usedAgo: "2 days ago", free: true},
			"cache-new": {size: 3 << 30, usedAgo: "1 hour ago", free: true},
		}}
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
	if r.err != nil || len(r.removed) != 2 || !r.pruned {
		t.Fatalf("prune = %+v; want 2 images removed and the build cache pruned", r)
	}
	// 7 GiB down to 3 GiB (three quarters of the 4 GiB cap): the two
	// oldest unused images go; the lab's, older still, and the node's stay.
	if want := [][]string{{devOne}, {devTwo}}; !slices.EqualFunc(f.removed, want, slices.Equal) {
		t.Errorf("removed %v; want %v", f.removed, want)
	}
	// 6 GiB down to 3 GiB (three quarters of the 4 GiB cap): the two
	// reclaimable records used longest ago go; the freshest stays.
	if want := []string{"cache-old", "cache-mid"}; !slices.Equal(f.builderRemoved, want) {
		t.Errorf("builder removed %v; want %v", f.builderRemoved, want)
	}
	if want := []string{pruneForceFlag, "--filter", "id=cache-mid"}; !slices.Equal(f.pruned, want) {
		t.Errorf("last builder prune %v; want %v", f.pruned, want)
	}
	want := "Docker: images 7.0 GiB, cap 4.0 GiB; build cache 6.0 GiB, cap 4.0 GiB (doctor.dockerImagesMaxGiB, doctor.dockerBuildCacheMaxGiB); " +
		"2 unused images removed (4.0 GiB freed), images now 3.0 GiB; build cache pruned to 3.0 GiB"
	if got := r.String(); got != want {
		t.Errorf("line =\n %q\nwant\n %q", got, want)
	}
	evs, err := a.store.Events(10, func(e state.Event) bool { return e.Verb == dockerVerb })
	if logged := want + "; removed: x:dev-1, x:dev-2"; err != nil || len(evs) != 1 || evs[0].Detail != logged {
		t.Errorf("logged %+v, %v; want one %s event %q", evs, err, dockerVerb, logged)
	}
}

func TestPruneDockerKeepsAnOldImageTaggedRecently(t *testing.T) {
	a, f := dockerApp(t)
	a.cfg.Doctor.DockerBuildCacheMaxGiB = -1
	// x:dev-1 was created longest ago but rebuilt from cache an hour ago:
	// the session that tagged it has not loaded it yet.
	f.tagged["sha256:old"] = time.Now().Add(-time.Hour)
	r := a.pruneDocker(false, noHolds)
	if want := [][]string{{devTwo}, {devThree}}; r.err != nil || !slices.EqualFunc(f.removed, want, slices.Equal) {
		t.Errorf("removed %v, %v; want %v, the recently tagged x:dev-1 kept", f.removed, r.err, want)
	}
}

func TestDockerKeep(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	if got := dockerKeep(nil, now); !got.Equal(now.Add(-dockerTagSettle)) {
		t.Errorf("no lab: keep %v; want the settle window", got)
	}
	labs := []lease.Holder{
		{Env: labOne, Since: now.Add(-30 * time.Hour).Format(time.RFC3339)},
		{Env: labTwo, Purpose: "(being claimed)"},
	}
	if got := dockerKeep(labs, now); !got.Equal(now.Add(-30 * time.Hour)) {
		t.Errorf("a lab held 30h: keep %v; want its claim", got)
	}
	labs[0].Since = now.Add(-10 * 24 * time.Hour).Format(time.RFC3339)
	if got := dockerKeep(labs, now); !got.Equal(now.Add(-dockerLabKeep)) {
		t.Errorf("a lab held ten days: keep %v; want %v back at most", got, dockerLabKeep)
	}
}

func TestPruneDockerRemovesAKindLabsCopyLast(t *testing.T) {
	a, f := dockerApp(t)
	f.images = 10 << 30
	a.cfg.Doctor.DockerImagesMaxGiB = 3
	a.cfg.Doctor.DockerBuildCacheMaxGiB = -1
	r := a.pruneDocker(false, noHolds)
	// The lab's image, created first, goes after every other unused one;
	// the node image a container runs stays.
	if want := [][]string{{devOne}, {devTwo}, {devThree}, {labRef}}; r.err != nil || !slices.EqualFunc(f.removed, want, slices.Equal) {
		t.Errorf("removed %v, %v; want %v", f.removed, r.err, want)
	}
	if r.after.Images > 3<<30 || r.blocked() {
		t.Errorf("prune = %+v; want the images under their cap", r)
	}
	if f.pruned != nil {
		t.Error("the build cache was pruned with its cap off")
	}
}

// The unused images share layers: each removal frees 1 GiB on disk though
// its size says 2. One run still ends under the cap.
func TestPruneDockerReachesTheCapThroughSharedLayers(t *testing.T) {
	a, f := dockerApp(t)
	f.shrink = 1 << 30
	a.cfg.Doctor.DockerBuildCacheMaxGiB = -1
	r := a.pruneDocker(false, noHolds)
	if r.err != nil || r.after.Images > 4<<30 || len(f.removed) != 4 || r.blocked() {
		t.Errorf("prune = %+v, removed %v; want the images at 3 GiB after four removals", r, f.removed)
	}
}

// Over a 1 GiB cap with two images in flight: the old and the lab's images
// go, the in-flight ones and the node's stay, and the line names them.
func TestPruneDockerNamesWhatKeepsTheImagesOverTheCap(t *testing.T) {
	a, f := dockerApp(t)
	a.cfg.Doctor.DockerImagesMaxGiB = 1
	a.cfg.Doctor.DockerBuildCacheMaxGiB = -1
	f.tagged["sha256:new"] = time.Now().Add(-time.Hour)
	f.tagged["sha256:mid"] = time.Now().Add(-2 * time.Hour)
	r := a.pruneDocker(false, noHolds)
	if want := [][]string{{devOne}, {labRef}}; r.err != nil || !slices.EqualFunc(f.removed, want, slices.Equal) {
		t.Fatalf("removed %v, %v; want %v", f.removed, r.err, want)
	}
	line := r.String()
	for _, want := range []string{
		"2 unused images removed (4.0 GiB freed), images now 3.0 GiB; images stay over their cap: ",
		"x:dev-3 (2.0 GiB, created or tagged ", "x:dev-2 (2.0 GiB, created or tagged ", ", in flight)",
		"kindest/node:v1 (2.0 GiB, a container uses it)",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("line %q; want %q in it", line, want)
		}
	}
	if !r.blocked() || !r.notable() {
		t.Errorf("prune = %+v; want it blocked and notable", r)
	}
}

func TestBlockerListNamesTheLargest(t *testing.T) {
	var bs []dockerprune.Blocker
	for i := range 7 {
		bs = append(bs, dockerprune.Blocker{Image: dockerprune.Image{ID: "sha256:" + strconv.Itoa(i), Size: int64(i+1) << 30}, Why: "a container uses it"})
	}
	want := "sha256:6 (7.0 GiB, a container uses it), sha256:5 (6.0 GiB, a container uses it), sha256:4 (5.0 GiB, a container uses it), " +
		"sha256:3 (4.0 GiB, a container uses it), sha256:2 (3.0 GiB, a container uses it), and 2 more"
	if got := blockerList(bs); got != want {
		t.Errorf("blockers =\n %q\nwant\n %q", got, want)
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
	if len(r.removed) != 1 || len(f.removed) != 1 || r.err != nil || r.held == "" {
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

func TestDockerPressurePrunesBelowNormalCaps(t *testing.T) {
	a, f := dockerApp(t)
	f.images = 10 << 30 // Five distinct 2 GiB images, including the in-use node.
	a.cfg.Doctor.DockerImagesMaxGiB = 100
	a.cfg.Doctor.DockerBuildCacheMaxGiB = 100
	if r := a.pruneDocker(false, func() string { return "" }); r.acted() {
		t.Fatalf("normal prune below caps: %s", r)
	}
	r := a.pruneDockerForPressure(false, true, func() string { return "" })
	if r.err != nil || len(f.removed) == 0 || len(f.builderRemoved) == 0 {
		t.Fatalf("pressure failed to reclaim below caps: %s", r)
	}
}

func TestDockerPressurePreservesHoldsAndDisabledCaches(t *testing.T) {
	a, f := dockerApp(t)
	r := a.pruneDockerForPressure(false, true, func() string { return "build running" })
	if r.held != "build running" || len(f.removed) != 0 || len(f.builderRemoved) != 0 {
		t.Fatalf("pressure ignored build hold: %s", r)
	}
	a.cfg.Doctor.DockerImagesMaxGiB = -1
	a.cfg.Doctor.DockerBuildCacheMaxGiB = -1
	if r := a.pruneDockerForPressure(false, true, func() string { return "" }); !r.off() {
		t.Fatalf("pressure enabled disabled Docker caches: %s", r)
	}
}

func TestDockerPressureKeepsRecentlyTaggedImagesAndDryRun(t *testing.T) {
	a, f := dockerApp(t)
	f.tagged["sha256:old"] = time.Now()
	beforeImages, beforeCache := f.images, f.cache
	if r := a.pruneDockerForPressure(true, true, func() string { return "" }); r.err != nil || r.acted() || f.images != beforeImages || f.cache != beforeCache {
		t.Fatalf("pressure preview modified Docker: %s", r)
	}
	r := a.pruneDockerForPressure(false, true, func() string { return "" })
	if r.err != nil {
		t.Fatal(r.err)
	}
	for _, args := range f.removed {
		if slices.Contains(args, devOne) {
			t.Fatal("pressure removed recently tagged image")
		}
	}
	if len(f.removed) == 0 {
		t.Fatal("pressure did not remove eligible older images")
	}
}
