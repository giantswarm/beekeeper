package dockerprune

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
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
		{docker, "image", "load", "-i", "x.tar"},
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
}

func (f *fake) run(_ context.Context, args ...string) ([]byte, error) {
	switch {
	case slices.Equal(args, []string{"system", "df", "--format", "{{json .}}"}):
		return []byte(f.df), nil
	case slices.Equal(args, []string{"system", "df", "-v", "--format", "{{json .}}"}):
		return []byte(f.dfv), nil
	case args[0] == "ps":
		return []byte(f.nodes), nil
	case args[0] == "exec":
		return []byte(f.crictl[args[1]]), nil
	case args[0] == "image" && args[1] == "rm":
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
		nodes:  "lab-control-plane\trunning\n",
		crictl: map[string]string{"lab-control-plane": `{"images":[{"id":"sha256:other","repoTags":["docker.io/library/nginx:1"],"repoDigests":[]}]}`}}
	images, err := ListImages(context.Background(), f.run)
	if err != nil || len(images) != 5 {
		t.Fatalf("images = %+v, %v", images, err)
	}
	if old := images[1]; old.ID != "sha256:old" || len(old.Refs) != 2 || old.Unique != 2e9 {
		t.Errorf("old = %+v; want both tags, 2 GB", old)
	}
	used, err := KindImages(context.Background(), f.run)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, c := range Candidates(images, used) {
		ids = append(ids, c.ID)
	}
	// The node image runs a container, nginx:1 is in the lab by its tag.
	if want := []string{"sha256:dangling", "sha256:old", "sha256:new"}; !slices.Equal(ids, want) {
		t.Errorf("candidates = %v; want %v", ids, want)
	}
}

func TestKindImagesRefusesAStoppedNode(t *testing.T) {
	f := &fake{nodes: "lab-control-plane\texited\n"}
	if _, err := KindImages(context.Background(), f.run); err == nil || !strings.Contains(err.Error(), "exited") {
		t.Errorf("err = %v; want the stopped node named", err)
	}
}

func TestTrimImagesStopsAtTheTargetAndWhenBusy(t *testing.T) {
	cands := []Image{
		{ID: "a", Unique: 2e9},
		{ID: "b", Refs: []string{"x:1", "y:1"}, Unique: 2e9},
		{ID: "c", Refs: []string{"x:2"}, Unique: 2e9},
	}
	f := &fake{}
	freed, removed, err := TrimImages(context.Background(), f.run, cands, 10e9, 6e9, func() bool { return false })
	if err != nil || removed != 2 || freed != 4e9 {
		t.Fatalf("trim = %d, %d, %v; want 2 removed, 4 GB", freed, removed, err)
	}
	if want := [][]string{{"a"}, {"x:1", "y:1"}}; !slices.EqualFunc(f.removed, want, slices.Equal) {
		t.Errorf("removed %v; want %v", f.removed, want)
	}
	f = &fake{}
	calls := 0
	_, removed, err = TrimImages(context.Background(), f.run, cands, 10e9, 0, func() bool { calls++; return calls > 1 })
	if !errors.Is(err, ErrBusy) || removed != 1 {
		t.Errorf("busy trim = %d, %v; want 1 removed, then ErrBusy", removed, err)
	}
}

func TestTrimImagesSkipsARefusedImage(t *testing.T) {
	f := &fake{refuse: "a"}
	cands := []Image{{ID: "a", Unique: 1}, {ID: "b", Unique: 1}}
	if _, removed, err := TrimImages(context.Background(), f.run, cands, 2, 0, func() bool { return false }); err != nil || removed != 1 {
		t.Errorf("trim = %d, %v; want b removed, a skipped", removed, err)
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
