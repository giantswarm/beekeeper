package dockerprune

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestParseSize(t *testing.T) {
	for in, want := range map[string]int64{"0B": 0, "512kB": 512e3, "81.58GB": 81_580_000_000, "18.39GB (42%)": 18_390_000_000, "1.2TB": 1.2e12} {
		if got, err := ParseSize(in); err != nil || got != want {
			t.Errorf("ParseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "GB", "12XB"} {
		if _, err := ParseSize(in); err == nil {
			t.Errorf("ParseSize(%q) took it", in)
		}
	}
}

const (
	nginxRef = "docker.io/library/nginx:1"
	kind     = "kind"
	build    = "build"
	docker   = "docker"
	image    = "image"
	oldID    = "sha256:old"
	// inlabID is the fixture's image a kind node holds, danglingID its
	// image without a tag.
	inlabID, danglingID = "sha256:inlab", "sha256:dangling"
)

func TestNormalize(t *testing.T) {
	for in, want := range map[string]string{
		"nginx:1":                           nginxRef,
		"kindest/node:v1":                   "docker.io/kindest/node:v1",
		"gsoci.azurecr.io/giantswarm/x:dev": "gsoci.azurecr.io/giantswarm/x:dev",
		"localhost/x:dev":                   "localhost/x:dev",
		"localhost:5000/x:dev":              "localhost:5000/x:dev",
		nginxRef:                            nginxRef,
	} {
		if got := Normalize(in); got != want {
			t.Errorf("Normalize(%q) = %q; want %q", in, got, want)
		}
	}
}

func TestImporter(t *testing.T) {
	yes := [][]string{
		{kind, "load", "docker-image", "x:dev", "--name", "agentlab"},
		{"/usr/bin/kind", "create", "cluster"},
		{docker, build, "-t", "x", "."},
		{docker, "--context", "default", build, "."},
		{docker, image, "load", "-i", "x.tar"},
		{docker, "save", "x"},
		{"/usr/lib/docker/cli-plugins/docker-buildx", "buildx", build, "."},
		{"ctr", "-n", "k8s.io", "images", "import", "x.tar"},
	}
	no := [][]string{
		{kind, "get", "kubeconfig"},
		{docker, "ps"},
		{docker, "run", "img", "make", build},
		{docker, "exec", "node", "crictl", "images"},
		{"go", build, "./..."},
		{"kind"},
	}
	for _, a := range yes {
		if !Importer(a) {
			t.Errorf("%v is no importer", a)
		}
	}
	for _, a := range no {
		if Importer(a) {
			t.Errorf("%v is an importer", a)
		}
	}
}

// fake is a docker CLI over images; it records the removals.
type fake struct {
	df, dfv string
	nodes   string
	crictl  map[string]string
	removed [][]string
	refuse  string
	// tagged are the images' tag times by ID; an image without one was
	// never tagged.
	tagged map[string]string
}

func (f *fake) run(_ context.Context, args ...string) ([]byte, error) {
	switch {
	case slices.Equal(args, []string{"system", "df", "--format", "{{json .}}"}):
		return []byte(f.df), nil
	case slices.Equal(args, []string{"system", "df", "-v", "--format", "{{json .}}"}):
		return []byte(f.dfv), nil
	case args[0] == image && args[1] == "inspect":
		var out string
		for _, id := range args[4:] {
			at, ok := f.tagged[id]
			if !ok {
				at = "0001-01-01T00:00:00Z"
			}
			out += id + "\t\"" + at + "\"\n"
		}
		return []byte(out), nil
	case args[0] == "ps":
		return []byte(f.nodes), nil
	case args[0] == "exec":
		return []byte(f.crictl[args[1]]), nil
	case args[0] == image && args[1] == "rm":
		if slices.Contains(args, f.refuse) {
			return nil, errors.New("conflict: image is being used by a container")
		}
		f.removed = append(f.removed, args[2:])
		return nil, nil
	}
	return nil, errors.New("unexpected docker " + strings.Join(args, " "))
}

const dfv = `{"Images":[
{"Containers":"0","CreatedAt":"2026-10-15 01:00:00 +0200 CEST","ID":"sha256:new","Repository":"gsoci.azurecr.io/giantswarm/x","Tag":"dev-2","Digest":"","UniqueSize":"1GB"},
{"Containers":"0","CreatedAt":"2026-10-10 01:00:00 +0200 CEST","ID":"sha256:old","Repository":"gsoci.azurecr.io/giantswarm/x","Tag":"dev-1","Digest":"","UniqueSize":"2GB"},
{"Containers":"0","CreatedAt":"2026-10-10 01:00:00 +0200 CEST","ID":"sha256:old","Repository":"x","Tag":"dev-1","Digest":"","UniqueSize":"2GB"},
{"Containers":"0","CreatedAt":"2026-10-12 01:00:00 +0200 CEST","ID":"sha256:dangling","Repository":"<none>","Tag":"<none>","Digest":"<none>","UniqueSize":"500MB"},
{"Containers":"1","CreatedAt":"2026-01-01 01:00:00 +0200 CEST","ID":"sha256:node","Repository":"kindest/node","Tag":"v1","Digest":"","UniqueSize":"1GB"},
{"Containers":"0","CreatedAt":"2026-01-02 01:00:00 +0200 CEST","ID":"sha256:inlab","Repository":"nginx","Tag":"1","Digest":"","UniqueSize":"3GB"}
]}`

func TestListImagesGroupsTagsAndCandidatesSkipTheUsed(t *testing.T) {
	f := &fake{dfv: dfv,
		tagged: map[string]string{oldID: "2026-10-16T09:00:00.5+02:00"},
		nodes:  "lab-control-plane\trunning\n",
		crictl: map[string]string{"lab-control-plane": `{"images":[{"id":"sha256:other","repoTags":["docker.io/library/nginx:1"],"repoDigests":[]}]}`}}
	images, err := ListImages(context.Background(), f.run)
	if err != nil || len(images) != 5 {
		t.Fatalf("images = %+v, %v", images, err)
	}
	tagged := time.Date(2026, 10, 16, 7, 0, 0, 5e8, time.UTC)
	if old := images[1]; old.ID != oldID || len(old.Refs) != 2 || old.Unique != 2e9 || !old.Tagged.Equal(tagged) {
		t.Errorf("old = %+v; want both tags, 2 GB, tagged %v", old, tagged)
	}
	held, err := KindImages(context.Background(), f.run)
	if err != nil {
		t.Fatal(err)
	}
	ids := func(keep time.Time) []string {
		var out []string
		for _, c := range Candidates(images, held, keep) {
			out = append(out, c.ID)
		}
		return out
	}
	// The node image runs a container; nginx:1, created before all
	// others, is in the lab by its tag and goes last; old, created first
	// but retagged since new was built, goes after new.
	if got, want := ids(time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)), []string{danglingID, "sha256:new", oldID, inlabID}; !slices.Equal(got, want) {
		t.Errorf("candidates = %v; want %v", got, want)
	}
	// Kept since the 13th: new, created on the 15th, and old, tagged on the
	// 16th, stay whatever their Created.
	keep := time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)
	if got, want := ids(keep), []string{danglingID, inlabID}; !slices.Equal(got, want) {
		t.Errorf("candidates kept since the 13th = %v; want %v", got, want)
	}
	var why []string
	for _, b := range Kept(images, keep) {
		why = append(why, b.Image.ID+": "+b.Why)
	}
	if want := []string{
		"sha256:new: " + inFlight(images[0].Active()),
		"sha256:old: " + inFlight(images[1].Active()),
		"sha256:node: a container uses it",
	}; !slices.Equal(why, want) {
		t.Errorf("kept = %q; want %q", why, want)
	}
}

func TestListImagesFailsWithoutTagTimes(t *testing.T) {
	f := &fake{dfv: dfv}
	run := func(ctx context.Context, args ...string) ([]byte, error) {
		if args[0] == image && args[1] == "inspect" {
			return nil, errors.New("no such image")
		}
		return f.run(ctx, args...)
	}
	if _, err := ListImages(context.Background(), run); err == nil {
		t.Error("images listed without their tag times")
	}
}

func TestKindImagesSkipsAStoppedNode(t *testing.T) {
	f := &fake{nodes: "lab-control-plane\texited\nlab-worker\trunning\n",
		crictl: map[string]string{"lab-worker": `{"images":[{"id":"sha256:inlab","repoTags":[],"repoDigests":[]}]}`}}
	held, err := KindImages(context.Background(), f.run)
	if err != nil || !held.Has(Image{ID: inlabID}) || len(held) != 1 {
		t.Errorf("held = %v, %v; want the running node's image alone", held, err)
	}
}

func TestTrimImagesStopsAtTheTargetAndWhenBusy(t *testing.T) {
	cands := []Image{
		{ID: "a", Unique: 2e9},
		{ID: "b", Refs: []string{"x:1", "y:1"}, Unique: 2e9},
		{ID: "c", Refs: []string{"x:2"}, Unique: 2e9},
	}
	f := &fake{}
	tr, err := TrimImages(context.Background(), f.run, cands, 10e9, 6e9, nil, func() bool { return false })
	if err != nil || !slices.Equal(tr.Removed, []string{"a", "x:1 y:1"}) || tr.Freed != 4e9 {
		t.Fatalf("trim = %+v, %v; want 2 removed, 4 GB", tr, err)
	}
	if want := [][]string{{"a"}, {"x:1", "y:1"}}; !slices.EqualFunc(f.removed, want, slices.Equal) {
		t.Errorf("removed %v; want %v", f.removed, want)
	}
	f = &fake{}
	calls := 0
	tr, err = TrimImages(context.Background(), f.run, cands, 10e9, 0, nil, func() bool { calls++; return calls > 1 })
	if !errors.Is(err, ErrBusy) || len(tr.Removed) != 1 {
		t.Errorf("busy trim = %v, %v; want 1 removed, then ErrBusy", tr.Removed, err)
	}
}

// Removing a and b frees 4 GB by their unique sizes but 3 GB on disk:
// the rest they share with c. The trim reads the size again and goes on
// until Docker says the target is reached.
func TestTrimImagesGoesOnWhileDockerSaysMore(t *testing.T) {
	cands := []Image{{ID: "a", Unique: 2e9}, {ID: "b", Unique: 2e9}, {ID: "c", Unique: 2e9}, {ID: "d", Unique: 2e9}}
	f := &fake{}
	sizes := []int64{7e9, 6e9}
	reads := 0
	size := func() (int64, error) { reads++; return sizes[reads-1], nil }
	tr, err := TrimImages(context.Background(), f.run, cands, 10e9, 6e9, size, func() bool { return false })
	if err != nil || !slices.Equal(tr.Removed, []string{"a", "b", "c"}) || reads != 2 {
		t.Errorf("trim = %+v, %v after %d reads; want a, b and c removed after 2", tr, err, reads)
	}
}

// A refused image is tried again after the others: a parent goes once its
// child did. One refused twice is the trim's to report.
func TestTrimImagesRetriesARefusedImage(t *testing.T) {
	const stuck, parent, child = "stuck", "parent", "child"
	f := &fake{refuse: stuck}
	parentRefused := true
	run := func(ctx context.Context, args ...string) ([]byte, error) {
		switch {
		case slices.Contains(args, parent) && parentRefused:
			return nil, errors.New("conflict: image has dependent child images")
		case slices.Contains(args, child):
			parentRefused = false
		}
		return f.run(ctx, args...)
	}
	cands := []Image{{ID: stuck, Unique: 1}, {ID: parent, Unique: 1}, {ID: child, Unique: 1}}
	tr, err := TrimImages(context.Background(), run, cands, 3, 0, nil, func() bool { return false })
	if err != nil || !slices.Equal(tr.Removed, []string{child, parent}) {
		t.Fatalf("trim = %+v, %v; want child, then parent removed", tr, err)
	}
	if len(tr.Refused) != 1 || tr.Refused[0].Image.ID != stuck || !strings.Contains(tr.Refused[0].Why, "being used by a container") {
		t.Errorf("refused = %+v; want stuck with Docker's reason", tr.Refused)
	}
}

func TestReadUsage(t *testing.T) {
	f := &fake{df: `{"Size":"81.58GB","Type":"Images"}
{"Size":"8.1GB","Type":"Containers"}
{"Reclaimable":"18.39GB","Size":"42.88GB","Type":"Build Cache"}
`}
	u, err := ReadUsage(context.Background(), f.run)
	if err != nil || u.Images != 81_580_000_000 || u.BuildCache != 42_880_000_000 {
		t.Errorf("usage = %+v, %v", u, err)
	}
	f.df = `{"Size":"1GB","Type":"Images"}`
	if _, err := ReadUsage(context.Background(), f.run); err == nil {
		t.Error("a df without the build cache row read")
	}
}
