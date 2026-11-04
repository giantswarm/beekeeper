package dockerprune

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
	"text/template"
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
	systemCommand  = "system"
	inspectCommand = "inspect"
	nginxRef       = "docker.io/library/nginx:1"
	kind           = "kind"
	build          = "build"
	docker         = "docker"
	image          = "image"
	oldID          = "sha256:old"
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
	// du is the `builder du` rows; pruneArgs are the per-record prunes
	// run, and pruneRefused names the records they refuse.
	du           string
	pruneArgs    [][]string
	pruneRefused map[string]bool
	// tagged are the images' tag times by ID; an image without one was
	// never tagged.
	tagged map[string]string
	refs   map[string][]string
}

func (f *fake) run(_ context.Context, args ...string) ([]byte, error) {
	switch {
	case slices.Equal(args, []string{systemCommand, "df", "--format", "{{json .}}"}):
		return []byte(f.df), nil
	case slices.Equal(args, []string{systemCommand, "df", "-v", "--format", "{{json .}}"}):
		return []byte(f.dfv), nil
	case args[0] == "builder" && args[1] == "du":
		return []byte(f.du), nil
	case args[0] == "builder" && args[1] == "prune":
		f.pruneArgs = append(f.pruneArgs, slices.Clone(args[2:]))
		if len(args) == 5 && args[4] != "" && f.pruneRefused[strings.TrimPrefix(args[4], "id=")] {
			return nil, errors.New("failed to remove " + strings.TrimPrefix(args[4], "id="))
		}
		return []byte("Total:\t0B\n"), nil
	case args[0] == image && args[1] == inspectCommand:
		var out string
		for _, id := range args[4:] {
			at, ok := f.tagged[id]
			if !ok {
				at = "0001-01-01T00:00:00Z"
			}
			refs, _ := json.Marshal(f.refs[id])
			out += id + "\t\"" + at + "\"\t" + string(refs) + "\n"
		}
		return []byte(out), nil
	case args[0] == "ps":
		return []byte(f.nodes), nil
	case args[0] == "exec":
		return []byte(f.crictl[args[1]]), nil
	case args[0] == image && args[1] == "rm":
		f.removed = append(f.removed, args[2:])
		return nil, nil
	}
	return nil, errors.New("unexpected docker " + strings.Join(args, " "))
}

const dfv = `{"Images":[
{"Containers":"0","CreatedAt":"2026-10-15 01:00:00 +0200 CEST","ID":"sha256:new","Repository":"gsoci.azurecr.io/giantswarm/x","Tag":"dev-2","Digest":"","Size":"1GB","UniqueSize":"1GB"},
{"Containers":"0","CreatedAt":"2026-10-10 01:00:00 +0200 CEST","ID":"sha256:old","Repository":"gsoci.azurecr.io/giantswarm/x","Tag":"dev-1","Digest":"","Size":"2GB","UniqueSize":"2GB"},
{"Containers":"0","CreatedAt":"2026-10-12 01:00:00 +0200 CEST","ID":"sha256:dangling","Repository":"<none>","Tag":"<none>","Digest":"<none>","Size":"500MB","UniqueSize":"500MB"},
{"Containers":"1","CreatedAt":"2026-01-01 01:00:00 +0200 CEST","ID":"sha256:node","Repository":"kindest/node","Tag":"v1","Digest":"","Size":"1GB","UniqueSize":"1GB"},
{"Containers":"0","CreatedAt":"2026-01-02 01:00:00 +0200 CEST","ID":"sha256:inlab","Repository":"nginx","Tag":"1","Digest":"","Size":"3GB","UniqueSize":"3GB"}
]}`

func TestListImagesGroupsTagsAndCandidatesSkipTheUsed(t *testing.T) {
	f := &fake{dfv: dfv,
		tagged: map[string]string{oldID: "2026-10-16T09:00:00.5+02:00"},
		refs: map[string][]string{
			"sha256:new":  {"gsoci.azurecr.io/giantswarm/x:dev-2"},
			oldID:         {"gsoci.azurecr.io/giantswarm/x:dev-1", "x:dev-1"},
			"sha256:node": {"kindest/node:v1"},
			inlabID:       {"nginx:1"},
		},
		nodes:  "lab-control-plane\trunning\n",
		crictl: map[string]string{"lab-control-plane": `{"images":[{"id":"sha256:other","repoTags":["docker.io/library/nginx:1"],"repoDigests":[]}]}`}}
	images, err := ListImages(context.Background(), f.run)
	if err != nil || len(images) != 5 {
		t.Fatalf("images = %+v, %v", images, err)
	}
	tagged := time.Date(2026, 10, 16, 7, 0, 0, 5e8, time.UTC)
	if old := images[1]; old.ID != oldID || len(old.Refs) != 2 || old.Unique != 2e9 || old.Size != 2e9 || !old.Tagged.Equal(tagged) {
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
		if args[0] == image && args[1] == inspectCommand {
			return nil, errors.New("no such image")
		}
		return f.run(ctx, args...)
	}
	if _, err := ListImages(context.Background(), run); err == nil {
		t.Error("images listed without their tag times")
	}
}

func TestListImagesRemovesAllTagsFromOneDFRow(t *testing.T) {
	refs := []string{"example:one", "example:two"}
	remaining := map[string]bool{refs[0]: true, refs[1]: true}
	run := func(_ context.Context, args ...string) ([]byte, error) {
		switch {
		case args[0] == systemCommand:
			// Docker's verbose df prints just the first tag per image.
			return []byte(`{"Images":[{"ID":"sha256:old","Repository":"example","Tag":"one","CreatedAt":"2026-01-01 00:00:00 +0000 UTC","Size":"4GB","UniqueSize":"4GB","Containers":"0"}]}`), nil
		case args[0] == image && args[1] == inspectCommand:
			// Render the requested Docker template over a complete image
			// inspection, independently of which fields the package asks for.
			js := func(v any) string { raw, _ := json.Marshal(v); return string(raw) }
			format, err := template.New(inspectCommand).Funcs(template.FuncMap{"json": js}).Parse(args[3])
			if err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			err = format.Execute(&out, map[string]any{
				"Id": oldID, "Metadata": map[string]any{"LastTagTime": time.Time{}}, "RepoTags": refs,
			})
			if err != nil {
				t.Fatal(err)
			}
			out.WriteByte('\n')
			return out.Bytes(), nil
		case args[0] == image && args[1] == "rm":
			for _, ref := range args[2:] {
				delete(remaining, ref)
			}
			return nil, nil
		default:
			t.Fatalf("unexpected Docker call: %v", args)
			return nil, nil
		}
	}
	images, err := ListImages(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	size := func() (int64, error) {
		if len(remaining) > 0 {
			return 4e9, nil
		}
		return 0, nil
	}
	trimmed, err := TrimImages(context.Background(), run, Candidates(images, nil, time.Now()), 4e9, 0, size, func() bool { return false })
	if err != nil || len(trimmed.Removed) != 1 || len(remaining) != 0 {
		t.Fatalf("trim = %+v, %v; tags %v still retain the 4 GB image", trimmed, err, remaining)
	}
}

func TestListImagesFailsWithUnreadableReferences(t *testing.T) {
	for _, refs := range []string{"", "\tnot-json", "\t{}"} {
		f := &fake{dfv: dfv}
		run := func(ctx context.Context, args ...string) ([]byte, error) {
			if args[0] == image && args[1] == inspectCommand {
				return []byte("sha256:new\t\"0001-01-01T00:00:00Z\"" + refs + "\n"), nil
			}
			return f.run(ctx, args...)
		}
		if _, err := ListImages(context.Background(), run); err == nil {
			t.Errorf("listed images with unreadable references %q", refs)
		}
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
		{ID: "a", Size: 2e9},
		{ID: "b", Refs: []string{"x:1", "y:1"}, Size: 2e9},
		{ID: "c", Refs: []string{"x:2"}, Size: 2e9},
	}
	f := &fake{}
	tr, err := TrimImages(context.Background(), f.run, cands, 10e9, 6e9, nil, func() bool { return false })
	if err != nil || !slices.Equal(tr.Removed, []string{"a", "x:1 y:1"}) {
		t.Fatalf("trim = %+v, %v; want 2 removed", tr, err)
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

// The images share their layers: none frees anything alone (Unique 0),
// each 2 GB once all sharing them are gone. The trim counts removals by
// their whole size, so it reads the size again before it could pass the
// target: it stops where Docker says the target is reached, the kind copy
// ranked last (d) left, and goes on while Docker says more.
func TestTrimImagesReadsTheSizeBeforeItCouldPassTheTarget(t *testing.T) {
	cands := []Image{{ID: "a", Size: 2e9}, {ID: "b", Size: 2e9}, {ID: "c", Size: 2e9}, {ID: "d", Size: 2e9}}
	for _, c := range []struct {
		sizes []int64
		want  []string
	}{
		{sizes: []int64{6e9}, want: []string{"a", "b"}},
		{sizes: []int64{7e9, 6e9}, want: []string{"a", "b", "c"}},
	} {
		f := &fake{}
		reads := 0
		size := func() (int64, error) { reads++; return c.sizes[reads-1], nil }
		tr, err := TrimImages(context.Background(), f.run, cands, 10e9, 6e9, size, func() bool { return false })
		if err != nil || !slices.Equal(tr.Removed, c.want) || reads != len(c.sizes) {
			t.Errorf("sizes %v: trim = %+v, %v after %d reads; want %v removed after %d", c.sizes, tr, err, reads, c.want, len(c.sizes))
		}
	}
}

// refusing is a docker CLI that refuses to remove an image another image
// still builds on (children) or a container uses (stuck), like Docker.
func refusing(f *fake, children map[string]string, stuck string) Docker {
	return func(ctx context.Context, args ...string) ([]byte, error) {
		if args[0] == image && args[1] == "rm" {
			id := args[2]
			if id == stuck {
				return nil, errors.New("conflict: image is being used by a container")
			}
			if child, ok := children[id]; ok && !slices.ContainsFunc(f.removed, func(r []string) bool { return slices.Contains(r, child) }) {
				return nil, errors.New("conflict: image has dependent child images")
			}
		}
		return f.run(ctx, args...)
	}
}

// A refused image is tried again, the newest first, while a pass removes
// one: a chain of parents goes from its child up. One refused for good is
// the trim's to report.
func TestTrimImagesRetriesARefusedImage(t *testing.T) {
	const stuck, grand, parent, child = "stuck", "grand", "parent", "child"
	f := &fake{}
	run := refusing(f, map[string]string{grand: parent, parent: child}, stuck)
	cands := []Image{{ID: stuck, Size: 1}, {ID: grand, Size: 1}, {ID: parent, Size: 1}, {ID: child, Size: 1}}
	tr, err := TrimImages(context.Background(), run, cands, 4, 0, nil, func() bool { return false })
	if err != nil || !slices.Equal(tr.Removed, []string{child, parent, grand}) {
		t.Fatalf("trim = %+v, %v; want child, parent, then grand removed", tr, err)
	}
	if len(tr.Refused) != 1 || tr.Refused[0].Image.ID != stuck || !strings.Contains(tr.Refused[0].Why, "being used by a container") {
		t.Errorf("refused = %+v; want stuck with Docker's reason", tr.Refused)
	}
}

// A pass that removed nothing changed nothing: no image is tried again.
func TestTrimImagesRetriesNothingAfterAPassRemovedNothing(t *testing.T) {
	const stuck = "stuck"
	f := &fake{}
	calls := 0
	run := refusing(f, nil, stuck)
	counted := func(ctx context.Context, args ...string) ([]byte, error) { calls++; return run(ctx, args...) }
	tr, err := TrimImages(context.Background(), counted, []Image{{ID: stuck, Size: 1}}, 1, 0, nil, func() bool { return false })
	if err != nil || len(tr.Removed) != 0 || len(tr.Refused) != 1 || calls != 1 {
		t.Errorf("trim = %+v, %v after %d removals; want stuck refused once", tr, err, calls)
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

// The builder prune flags and one fixture age, named once so a fixture
// row differs only by the record it describes.
const (
	pruneForce  = "--force"
	pruneFilter = "--filter"
	age2d       = "2 days ago"
)

// duRow is one `docker builder du` record; duRows renders them as the
// daemon's json lines do. gib sizes the record in whole gigabytes.
type duRow struct {
	id      string
	gib     int
	free    bool
	usedAgo string
}

func duRows(rows ...duRow) string {
	var out string
	for _, r := range rows {
		out += fmt.Sprintf(`{"ID":%q,"Size":%q,"Reclaimable":%t,"LastUsedAt":%q}`+"\n", r.id, strconv.Itoa(r.gib)+"GB", r.free, r.usedAgo)
	}
	return out
}

// The trim takes the reclaimable record used longest ago first, stops at
// the target, and never asks for a record Docker does not call
// reclaimable; the daemon's own --max-used-space pass, which frees nothing
// while its reserved-space floor sits above the target, is gone.
func TestPruneBuildCacheRemovesTheOldestReclaimableToTheTarget(t *testing.T) {
	f := &fake{du: duRows(
		duRow{"old", 2, true, "5 days ago"},
		duRow{"mid", 1, true, age2d},
		duRow{"new", 3, true, "1 hour ago"},
		duRow{"busy", 2, false, "3 minutes ago"},
	)}
	// 8 GB total, target 3 GB: old (2) and mid (1) still leave 5 GB, so
	// new goes too (2 GB left); the non-reclaimable busy record is never
	// asked for and is what stays.
	trimmed, err := PruneBuildCache(context.Background(), f.run, 3_000_000_000)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.pruneArgs) != 3 {
		t.Fatalf("prunes = %v; want the target reached after three", f.pruneArgs)
	}
	for i, want := range [][]string{
		{pruneForce, pruneFilter, "id=old"},
		{pruneForce, pruneFilter, "id=mid"},
		{pruneForce, pruneFilter, "id=new"},
	} {
		if !slices.Equal(f.pruneArgs[i], want) {
			t.Errorf("prune %d = %v; want %v", i, f.pruneArgs[i], want)
		}
	}
	if trimmed.Removed != 3 || trimmed.Freed != 6e9 {
		t.Errorf("trim = %+v; want 3 records, 6 GB", trimmed)
	}
	if trimmed.Kept != 1 || trimmed.KeptBytes != 2e9 {
		t.Errorf("trim = %+v; want the busy record kept with 2 GB", trimmed)
	}
}

// A cache under its target needs no prune; a non-reclaimable or
// unreadable-age record is never asked for, and a trim that only meets
// refusals says so by the record it could not remove.
func TestPruneBuildCacheUnderTheTargetAndWithRefusals(t *testing.T) {
	f := &fake{du: duRows(duRow{"a", 1, true, age2d})}
	if trimmed, err := PruneBuildCache(context.Background(), f.run, 3<<30); err != nil || trimmed.Removed != 0 || len(f.pruneArgs) != 0 {
		t.Errorf("under target: %+v, %v, %v", trimmed, err, f.pruneArgs)
	}
	f = &fake{du: duRows(duRow{"a", 2, true, age2d}, duRow{"b", 2, true, "9 days ago"}),
		pruneRefused: map[string]bool{"a": true}}
	trimmed, err := PruneBuildCache(context.Background(), f.run, 0)
	if err != nil || trimmed.Removed != 1 || trimmed.Freed != 2e9 {
		t.Errorf("partly refused trim = %+v, %v; want b removed", trimmed, err)
	}
	f = &fake{du: duRows(duRow{"a", 2, true, age2d}),
		pruneRefused: map[string]bool{"a": true}}
	if trimmed, err := PruneBuildCache(context.Background(), f.run, 0); err == nil || !strings.Contains(err.Error(), "a") || trimmed.Removed != 0 {
		t.Errorf("all-refused trim = %+v, %v; want an error naming a", trimmed, err)
	}
}

func TestParseUsedAgo(t *testing.T) {
	for _, c := range []struct {
		in   string
		want time.Duration
	}{
		{"10 minutes ago", 10 * time.Minute},
		{"26 hours ago", 26 * time.Hour},
		{"5 days ago", 5 * 24 * time.Hour},
		{"3 weeks ago", 3 * 7 * 24 * time.Hour},
		{"Less than a second ago", -1},
		{"", -1},
	} {
		if got := parseUsedAgo(c.in); got != c.want {
			t.Errorf("parseUsedAgo(%q) = %v; want %v", c.in, got, c.want)
		}
	}
}
