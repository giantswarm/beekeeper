// Package dockerprune keeps the Docker images and build cache of a lab
// machine under a size: the unused images created or tagged longest ago go
// first, the build cache is pruned by Docker's own least-recently-used order.
// An image a container uses, one a kind lab's node holds, or one created or
// tagged since a keep time is never removed.
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

// InUse are the images that must stay: by ID, normalized reference and
// digest.
type InUse map[string]bool

// Has says img is in use: a container runs it, or a kind node holds it.
func (u InUse) Has(img Image) bool {
	if img.Containers > 0 || u[img.ID] || (img.Digest != "" && u[img.Digest]) {
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

// KindImages are the images every kind node holds, and the node images
// themselves through the containers using them. A node that is not running
// cannot say what it holds: then nothing is known to be unused, and the
// call fails.
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
			return nil, fmt.Errorf("kind node %s is %s: what it holds is unknown", name, state)
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

// Candidates are the images a trim may remove, in the order it removes
// them: dangling ones first, then the ones created or tagged longest ago.
// An image created or tagged after keep is no candidate: a session may have
// produced it for a lab it has not loaded yet.
func Candidates(images []Image, used InUse, keep time.Time) []Image {
	out := slices.DeleteFunc(slices.Clone(images), func(i Image) bool { return used.Has(i) || i.Active().After(keep) })
	slices.SortStableFunc(out, func(a, b Image) int {
		if da, db := len(a.Refs) == 0, len(b.Refs) == 0; da != db {
			if da {
				return -1
			}
			return 1
		}
		return a.Active().Compare(b.Active())
	})
	return out
}

// TrimImages removes candidates in order until the images' size, from
// total, is at most target, asking busy before each removal. An image
// Docker refuses to remove (a container started on it since) is skipped.
// It returns what it freed by the images' unique sizes and the names of
// the images it removed.
func TrimImages(ctx context.Context, d Docker, candidates []Image, total, target int64, busy func() bool) (freed int64, removed []string, err error) {
	var refused []error
	for _, img := range candidates {
		if total-freed <= target {
			break
		}
		if busy() {
			return freed, removed, ErrBusy
		}
		args := []string{"image", "rm"}
		if len(img.Refs) == 0 {
			args = append(args, img.ID)
		} else {
			args = append(args, img.Refs...)
		}
		if _, err := d(ctx, args...); err != nil {
			refused = append(refused, fmt.Errorf("%s: %w", img.ID, err))
			continue
		}
		freed += img.Unique
		removed = append(removed, img.Name())
	}
	if len(refused) > 0 && len(removed) == 0 {
		return freed, removed, errors.Join(refused...)
	}
	return freed, removed, nil
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
