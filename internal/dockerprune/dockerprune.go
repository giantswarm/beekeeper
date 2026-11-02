// Package dockerprune keeps the Docker images and build cache of a lab
// machine under a size: the unused images created or tagged longest ago go
// first, the ones a kind lab's node holds a copy of last, and the build cache
// is pruned by Docker's own least-recently-used order. An image a container
// uses, or one created or tagged since a keep time, is never removed; a trim
// that stays over its size says which images kept it there.
package dockerprune

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// ErrBusy stops a trim: an image import or build began while it ran.
var ErrBusy = errors.New("an image import runs")

// KindLabel is the label kind puts on its node containers.
const KindLabel = "io.x-k8s.kind.cluster"

// Docker runs one docker CLI call and returns its stdout.
type Docker func(ctx context.Context, args ...string) ([]byte, error)

// Usage is the size of the images and the build cache, as docker system df
// says them.
type Usage struct {
	Images, BuildCache int64
}

// ReadUsage reads the totals of docker system df.
func ReadUsage(ctx context.Context, d Docker) (Usage, error) {
	raw, err := d(ctx, "system", "df", "--format", "{{json .}}")
	if err != nil {
		return Usage{}, err
	}
	var u Usage
	var seen int
	for line := range bytes.Lines(raw) {
		var row struct{ Type, Size string }
		if json.Unmarshal(line, &row) != nil {
			continue
		}
		n, err := ParseSize(row.Size)
		if err != nil {
			return Usage{}, fmt.Errorf("docker system df: %s: %w", row.Type, err)
		}
		switch row.Type {
		case "Images":
			u.Images, seen = n, seen|1
		case "Build Cache":
			u.BuildCache, seen = n, seen|2
		}
	}
	if seen != 3 {
		return Usage{}, errors.New("docker system df: no Images or Build Cache row")
	}
	return u, nil
}

// Image is one image of the machine's Docker.
type Image struct {
	ID string
	// Refs are its repository:tag names, none for a dangling image.
	Refs    []string
	Digest  string
	Created time.Time
	// Tagged is when a tag was last put on it (docker build, tag or load):
	// an image rebuilt from cache or retagged keeps its old Created.
	Tagged time.Time
	// Unique is what removing it frees: the layers no other image shares.
	Unique     int64
	Containers int
}

// Active is when the image was last produced: created or tagged, the later.
func (i Image) Active() time.Time {
	if i.Tagged.After(i.Created) {
		return i.Tagged
	}
	return i.Created
}

// Name is how a removal names the image: its references, or its ID for a
// dangling one.
func (i Image) Name() string {
	if len(i.Refs) == 0 {
		return i.ID
	}
	return strings.Join(i.Refs, " ")
}

// ListImages reads every image with what removing it frees, from docker
// system df -v, which lists an image once per tag, and when it was tagged
// last, from docker image inspect.
func ListImages(ctx context.Context, d Docker) ([]Image, error) {
	raw, err := d(ctx, "system", "df", "-v", "--format", "{{json .}}")
	if err != nil {
		return nil, err
	}
	var doc struct {
		Images []struct {
			ID, Repository, Tag, Digest, CreatedAt, UniqueSize, Containers string
		}
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("docker system df -v: %w", err)
	}
	var out []Image
	at := map[string]int{}
	for _, r := range doc.Images {
		i, ok := at[r.ID]
		if !ok {
			created, _ := time.Parse("2006-01-02 15:04:05 -0700 MST", r.CreatedAt)
			unique, err := ParseSize(r.UniqueSize)
			if err != nil {
				return nil, fmt.Errorf("docker system df -v: image %s: %w", r.ID, err)
			}
			containers, _ := strconv.Atoi(r.Containers)
			i, at[r.ID] = len(out), len(out)
			out = append(out, Image{ID: r.ID, Digest: none(r.Digest), Created: created, Unique: unique, Containers: containers})
		}
		if repo, tag := none(r.Repository), none(r.Tag); repo != "" && tag != "" {
			out[i].Refs = append(out[i].Refs, repo+":"+tag)
		}
	}
	return out, tagTimes(ctx, d, out)
}

// tagTimes sets each image's Tagged. An image whose tag time is unreadable
// fails the list: what may be removed is then unknown.
func tagTimes(ctx context.Context, d Docker, images []Image) error {
	if len(images) == 0 {
		return nil
	}
	args := []string{"image", "inspect", "--format", "{{.Id}}\t{{json .Metadata.LastTagTime}}"}
	at := map[string]int{}
	for i, img := range images {
		args, at[img.ID] = append(args, img.ID), i
	}
	raw, err := d(ctx, args...)
	if err != nil {
		return err
	}
	seen := 0
	for line := range bytes.Lines(raw) {
		id, js, _ := strings.Cut(strings.TrimSpace(string(line)), "\t")
		i, ok := at[id]
		if !ok {
			continue
		}
		var t time.Time
		if err := json.Unmarshal([]byte(js), &t); err != nil {
			return fmt.Errorf("docker image inspect: image %s: tag time %s: %w", id, js, err)
		}
		images[i].Tagged, seen = t, seen+1
	}
	if seen != len(images) {
		return fmt.Errorf("docker image inspect: tag times of %d of %d images", seen, len(images))
	}
	return nil
}

// none is s, or "" for docker's "<none>".
func none(s string) string {
	if s == "<none>" {
		return ""
	}
	return s
}

// InUse are the images kind labs' nodes hold: by ID, normalized reference
// and digest.
type InUse map[string]bool

// Has says a kind node holds img. The node's containerd has its own copy,
// so the host's may go, though a lab created again would load it anew.
func (u InUse) Has(img Image) bool {
	if u[img.ID] || (img.Digest != "" && u[img.Digest]) {
		return true
	}
	return slices.ContainsFunc(img.Refs, func(r string) bool { return u[Normalize(r)] })
}

// Normalize names a reference as containerd does: docker.io/library/nginx:1
// for nginx:1.
func Normalize(ref string) string {
	name, _, _ := strings.Cut(ref, "@")
	first, _, ok := strings.Cut(name, "/")
	if !ok {
		return "docker.io/library/" + ref
	}
	if !strings.ContainsAny(first, ".:") && first != "localhost" {
		return "docker.io/" + ref
	}
	return ref
}

// KindImages are the images every running kind node holds. A node that is
// not running cannot say what it holds and is skipped: what it holds only
// ranks a trim's candidates, and its own copy stays whatever the host
// removes.
func KindImages(ctx context.Context, d Docker) (InUse, error) {
	raw, err := d(ctx, "ps", "-a", "--filter", "label="+KindLabel, "--format", "{{.Names}}\t{{.State}}")
	if err != nil {
		return nil, err
	}
	used := InUse{}
	sc := bufio.NewScanner(bytes.NewReader(raw))
	for sc.Scan() {
		name, state, _ := strings.Cut(strings.TrimSpace(sc.Text()), "\t")
		if name == "" {
			continue
		}
		if state != "running" {
			continue
		}
		out, err := d(ctx, "exec", name, "crictl", "images", "-o", "json")
		if err != nil {
			return nil, fmt.Errorf("kind node %s: crictl images: %w", name, err)
		}
		var doc struct {
			Images []struct {
				ID          string   `json:"id"`
				RepoTags    []string `json:"repoTags"`
				RepoDigests []string `json:"repoDigests"`
			} `json:"images"`
		}
		if err := json.Unmarshal(out, &doc); err != nil {
			return nil, fmt.Errorf("kind node %s: crictl images: %w", name, err)
		}
		for _, img := range doc.Images {
			used[img.ID] = true
			for _, t := range img.RepoTags {
				used[Normalize(t)] = true
			}
			for _, rd := range img.RepoDigests {
				if _, dg, ok := strings.Cut(rd, "@"); ok {
					used[dg] = true
				}
			}
		}
	}
	return used, nil
}

// kept says why an image is no candidate, "" for a candidate: a container
// uses it, or it was created or tagged after keep, so a session may have
// produced it for a lab it has not loaded yet.
func kept(i Image, keep time.Time) string {
	switch {
	case i.Containers > 0:
		return "a container uses it"
	case i.Active().After(keep):
		return inFlight(i.Active())
	}
	return ""
}

// inFlight says why an image created or tagged at t stays.
func inFlight(t time.Time) string {
	return "created or tagged " + t.Format("2006-01-02 15:04 MST") + ", in flight"
}

// Candidates are the images a trim may remove, in the order it removes
// them: dangling ones first, then the ones created or tagged longest ago,
// and the ones a kind node holds (held) after all others, by the same
// order. An image a container uses or one created or tagged after keep is
// no candidate (Kept).
func Candidates(images []Image, held InUse, keep time.Time) []Image {
	out := slices.DeleteFunc(slices.Clone(images), func(i Image) bool { return kept(i, keep) != "" })
	rank := func(i Image) int {
		r := 0
		if held.Has(i) {
			r += 2
		}
		if len(i.Refs) > 0 {
			r++
		}
		return r
	}
	slices.SortStableFunc(out, func(a, b Image) int {
		if ra, rb := rank(a), rank(b); ra != rb {
			return ra - rb
		}
		return a.Active().Compare(b.Active())
	})
	return out
}

// Blocker is an image a trim left in place, and why.
type Blocker struct {
	Image Image
	Why   string
}

// Kept are the images no trim removes, with why: the ones a container
// uses and the ones created or tagged after keep.
func Kept(images []Image, keep time.Time) []Blocker {
	var out []Blocker
	for _, i := range images {
		if why := kept(i, keep); why != "" {
			out = append(out, Blocker{Image: i, Why: why})
		}
	}
	return out
}

// Trimmed is what a trim did: what it freed by the images' unique sizes,
// the names of the images it removed, and the candidates Docker refused to
// remove.
type Trimmed struct {
	Freed   int64
	Removed []string
	Refused []Blocker
}

// TrimImages removes candidates in order until the images' size, from
// total, is at most target, asking busy before each removal. Once the
// unique sizes it freed say the target is reached, it reads the size again
// (size, nil for none) and goes on while Docker still says more: layers
// the removed images shared with others free nothing until the last of
// them goes. An image Docker refuses to remove (a container started on it
// since, a child image built on it) is skipped and, while the images stay
// over target, tried once more after the others.
func TrimImages(ctx context.Context, d Docker, candidates []Image, total, target int64, size func() (int64, error), busy func() bool) (Trimmed, error) {
	var t Trimmed
	left := total
	over := func() (bool, error) {
		if left > target || size == nil {
			return left > target, nil
		}
		n, err := size()
		if err != nil {
			return false, err
		}
		left = n
		return left > target, nil
	}
	pass := func(images []Image) ([]Blocker, error) {
		var refused []Blocker
		for _, img := range images {
			if more, err := over(); err != nil || !more {
				return refused, err
			}
			if busy() {
				return refused, ErrBusy
			}
			args := []string{"image", "rm"}
			if len(img.Refs) == 0 {
				args = append(args, img.ID)
			} else {
				args = append(args, img.Refs...)
			}
			if _, err := d(ctx, args...); err != nil {
				refused = append(refused, Blocker{Image: img, Why: "Docker refused its removal: " + err.Error()})
				continue
			}
			t.Freed += img.Unique
			left -= img.Unique
			t.Removed = append(t.Removed, img.Name())
		}
		return refused, nil
	}
	refused, err := pass(candidates)
	if err == nil && len(refused) > 0 {
		retry := make([]Image, len(refused))
		for i, b := range refused {
			retry[i] = b.Image
		}
		refused, err = pass(retry)
	}
	t.Refused = refused
	return t, err
}

// PruneBuildCache prunes the build cache down to keep bytes; the cache a
// build uses stays.
func PruneBuildCache(ctx context.Context, d Docker, keep int64) error {
	_, err := d(ctx, "builder", "prune", "--force", "--max-used-space", strconv.FormatInt(keep, 10))
	return err
}

// Importer says an argument vector loads or builds images: kind load and
// kind create (which loads its node image), docker build, load, save and
// import, buildx, and containerd's ctr import. The verb is one of the first
// three words that are no flag: docker image load, docker --context x
// build, ctr -n k8s.io images import.
func Importer(args []string) bool {
	if len(args) < 2 {
		return false
	}
	name := args[0][strings.LastIndex(args[0], "/")+1:]
	var words []string
	for _, a := range args[1:] {
		if !strings.HasPrefix(a, "-") {
			if words = append(words, a); len(words) == 3 {
				break
			}
		}
	}
	const build = "build"
	verbs := map[string][]string{
		"kind":          {"load", "create"},
		"docker":        {build, "buildx", "load", "save", "import"},
		"docker-buildx": {build, "bake"},
		"buildctl":      {build},
		"ctr":           {"import"},
	}[name]
	return slices.ContainsFunc(words, func(a string) bool { return slices.Contains(verbs, a) })
}

// ParseSize reads a size as docker prints it: decimal units, "1.5GB",
// "512kB", "0B", with an optional percentage after a space.
func ParseSize(s string) (int64, error) {
	s, _, _ = strings.Cut(strings.TrimSpace(s), " ")
	i := strings.IndexFunc(s, func(r rune) bool { return (r < '0' || r > '9') && r != '.' })
	if i <= 0 {
		return 0, fmt.Errorf("size %q", s)
	}
	n, err := strconv.ParseFloat(s[:i], 64)
	if err != nil {
		return 0, fmt.Errorf("size %q: %w", s, err)
	}
	mult, ok := map[string]float64{"B": 1, "kB": 1e3, "KB": 1e3, "MB": 1e6, "GB": 1e9, "TB": 1e12, "PB": 1e15}[s[i:]]
	if !ok {
		return 0, fmt.Errorf("size %q: unit %q", s, s[i:])
	}
	return int64(n * mult), nil
}
