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
	"cmp"
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

const (
	imageCommand   = "image"
	builderCommand = "builder"
)

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
	// Size is all its layers, the ones it shares with other images
	// included: at most what removing it frees, once every image sharing
	// them is gone. Unique is the layers no other image shares, what
	// removing it alone frees for certain.
	Size, Unique int64
	Containers   int
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

// ListImages reads every image with its size and what removing it frees,
// from docker system df -v, and its complete references and last tag time
// from docker image inspect. Verbose df lists only the first tag.
func ListImages(ctx context.Context, d Docker) ([]Image, error) {
	raw, err := d(ctx, "system", "df", "-v", "--format", "{{json .}}")
	if err != nil {
		return nil, err
	}
	var doc struct {
		Images []struct {
			ID, Repository, Tag, Digest, CreatedAt, Size, UniqueSize, Containers string
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
			size, err := ParseSize(r.Size)
			if err != nil {
				return nil, fmt.Errorf("docker system df -v: image %s: %w", r.ID, err)
			}
			unique, err := ParseSize(r.UniqueSize)
			if err != nil {
				return nil, fmt.Errorf("docker system df -v: image %s: %w", r.ID, err)
			}
			containers, _ := strconv.Atoi(r.Containers)
			i, at[r.ID] = len(out), len(out)
			out = append(out, Image{ID: r.ID, Digest: none(r.Digest), Created: created, Size: size, Unique: unique, Containers: containers})
		}
		if repo, tag := none(r.Repository), none(r.Tag); repo != "" && tag != "" {
			out[i].Refs = append(out[i].Refs, repo+":"+tag)
		}
	}
	return out, inspectImages(ctx, d, out)
}

// inspectImages sets each image's Tagged and Refs. An unreadable inspection
// fails the list: what may be removed is then unknown.
func inspectImages(ctx context.Context, d Docker, images []Image) error {
	if len(images) == 0 {
		return nil
	}
	args := []string{imageCommand, "inspect", "--format", "{{.Id}}	{{json .Metadata.LastTagTime}}	{{json .RepoTags}}"}
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
		id, data, _ := strings.Cut(strings.TrimSpace(string(line)), "\t")
		i, ok := at[id]
		if !ok {
			continue
		}
		js, tags, ok := strings.Cut(data, "\t")
		if !ok {
			return fmt.Errorf("docker image inspect: image %s: missing references", id)
		}
		var t time.Time
		if err := json.Unmarshal([]byte(js), &t); err != nil {
			return fmt.Errorf("docker image inspect: image %s: tag time %s: %w", id, js, err)
		}
		var refs []string
		if err := json.Unmarshal([]byte(tags), &refs); err != nil {
			return fmt.Errorf("docker image inspect: image %s: references: %w", id, err)
		}
		images[i].Refs = refs
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

// Trimmed is what a trim did: the names of the images it removed, and the
// candidates Docker refused to remove.
type Trimmed struct {
	Removed []string
	Refused []Blocker
}

// TrimImages removes candidates in order until the images' size, from
// total, is at most target, asking busy before each removal. It counts
// each removal by the image's whole size, more than it may free (a layer
// another image shares stays), so the count never says more than Docker
// does; once the count says the target may be reached it reads the size
// again (size, nil for none) and goes on while Docker still says more. An
// image Docker refuses to remove (a container started on it since, a child
// image built on it) is skipped; while the images stay over target the
// refused ones are tried again, the newest first so a child goes before
// its parent, as long as a pass removes one.
func TrimImages(ctx context.Context, d Docker, candidates []Image, total, target int64, size func() (int64, error), busy func() bool) (Trimmed, error) {
	var t Trimmed
	if err := ctx.Err(); err != nil {
		return t, err
	}
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
			if err := ctx.Err(); err != nil {
				return refused, err
			}
			if more, err := over(); err != nil || !more {
				return refused, errors.Join(err, ctx.Err())
			}
			if busy() {
				return refused, ErrBusy
			}
			// Parents must pass the candidate policy and be removed explicitly.
			args := []string{imageCommand, "rm", "--no-prune"}
			if len(img.Refs) == 0 {
				args = append(args, img.ID)
			} else {
				args = append(args, img.Refs...)
			}
			if _, err := d(ctx, args...); err != nil {
				if canceled := ctx.Err(); canceled != nil {
					return refused, canceled
				}
				refused = append(refused, Blocker{Image: img, Why: "Docker refused its removal: " + err.Error()})
				continue
			}
			left -= max(img.Size, img.Unique)
			t.Removed = append(t.Removed, img.Name())
		}
		return refused, nil
	}
	refused, err := pass(candidates)
	slices.Reverse(refused)
	for removed := len(t.Removed); err == nil && len(refused) > 0 && removed > 0; {
		retry := make([]Image, len(refused))
		for i, b := range refused {
			retry[i] = b.Image
		}
		before := len(t.Removed)
		refused, err = pass(retry)
		removed = len(t.Removed) - before
	}
	t.Refused = refused
	return t, err
}

// CacheRecord is one build cache record of `docker builder du`.
type CacheRecord struct {
	ID string
	// Size is what the record lists; shared layers may free less.
	Size int64
	// Reclaimable is Docker saying no build or container uses the record.
	// A record that is not reclaimable stays, whoever asks.
	Reclaimable bool
	// UsedAgo is how long before the reading the record was last used;
	// -1 when Docker's age is unreadable, which keeps the record.
	UsedAgo time.Duration
}

// CacheRecords reads the build cache's records with their sizes and ages
// from `docker builder du`.
func CacheRecords(ctx context.Context, d Docker) ([]CacheRecord, error) {
	raw, err := d(ctx, builderCommand, "du", "--format", "{{json .}}")
	if err != nil {
		return nil, err
	}
	var out []CacheRecord
	for line := range bytes.Lines(raw) {
		var r struct {
			ID, Size, LastUsedAt string
			Reclaimable          bool
		}
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		if json.Unmarshal(line, &r) != nil {
			continue
		}
		size, err := ParseSize(r.Size)
		if err != nil {
			return nil, fmt.Errorf("docker builder du: record %s: %w", r.ID, err)
		}
		out = append(out, CacheRecord{ID: r.ID, Size: size, Reclaimable: r.Reclaimable, UsedAgo: parseUsedAgo(r.LastUsedAt)})
	}
	return out, nil
}

// CacheTrim is one build cache trim's result.
type CacheTrim struct {
	// Freed is the listed size of the records removed; shared layers may
	// free less, so the freed bytes of a run come from the df re-read.
	Freed int64
	// Removed names how many records went.
	Removed int
	// Kept counts the records that stay above the target because no
	// removal may take them (a build or container uses them), and KeptBytes
	// is what they hold.
	Kept      int
	KeptBytes int64
}

// PruneBuildCache brings the build cache down to keep bytes, removing the
// reclaimable record used longest ago first. Docker's own
// `builder prune --max-used-space` cannot be asked for this: it is a
// best-effort pass of the daemon's GC policy and frees nothing while the
// policy's reserved or min-free-space floor sits above the target, which
// on a nearly full disk is always. A record Docker does not call
// reclaimable is never touched and is counted in Kept. A successful prune
// can remove nothing, so fresh records confirm progress and remaining size.
// Records blocked by children are retried while a pass removes a record.
func PruneBuildCache(ctx context.Context, d Docker, keep int64) (t CacheTrim, err error) {
	if err := ctx.Err(); err != nil {
		return t, err
	}
	records, err := CacheRecords(ctx, d)
	if err != nil {
		return t, errors.Join(err, ctx.Err())
	}
	var total int64
	for _, r := range records {
		total += r.Size
	}
	if total <= keep {
		return t, nil
	}
	candidates := slices.DeleteFunc(slices.Clone(records), func(r CacheRecord) bool { return !r.Reclaimable || r.UsedAgo < 0 })
	// Oldest last use first, the daemon's own least-recently-used order;
	// the ID breaks ties so a run is deterministic.
	slices.SortStableFunc(candidates, func(a, b CacheRecord) int {
		return cmp.Or(cmp.Compare(b.UsedAgo, a.UsedAgo), cmp.Compare(a.ID, b.ID))
	})
	var refused []error
	var canceled error
	defer func() {
		for _, r := range records {
			t.Kept++
			t.KeptBytes += r.Size
		}
	}()
	for len(candidates) > 0 && total > keep && canceled == nil {
		var retry []CacheRecord
		before := t.Removed
		for _, r := range candidates {
			if canceled = ctx.Err(); canceled != nil {
				break
			}
			if total <= keep {
				break
			}
			if _, err := d(ctx, builderCommand, "prune", "--force", "--filter", "id="+r.ID); err != nil {
				if canceled = ctx.Err(); canceled != nil {
					break
				}
				refused = append(refused, fmt.Errorf("%s: %w", r.ID, err))
				continue
			}
			current, err := CacheRecords(ctx, d)
			if err != nil {
				return t, errors.Join(err, ctx.Err())
			}
			records, total = current, 0
			for _, remaining := range records {
				total += remaining.Size
			}
			if slices.ContainsFunc(records, func(remaining CacheRecord) bool { return remaining.ID == r.ID }) {
				retry = append(retry, r)
				continue
			}
			t.Freed += r.Size
			t.Removed++
		}
		if t.Removed == before {
			break
		}
		candidates = retry
	}
	if canceled != nil {
		return t, canceled
	}
	if canceled != nil {
		return t, canceled
	}
	if len(refused) > 0 && t.Removed == 0 {
		return t, errors.Join(refused...)
	}
	return t, nil
}

// parseUsedAgo reads `docker builder du`'s human age ("26 minutes ago",
// "5 days ago") into a duration; -1 for an age it cannot read, which keeps
// the record a trim would otherwise order.
func parseUsedAgo(s string) time.Duration {
	n, unit, ok := strings.Cut(strings.TrimSuffix(strings.TrimSpace(s), " ago"), " ")
	if !ok {
		return -1
	}
	v, err := strconv.ParseFloat(n, 64)
	if err != nil {
		return -1
	}
	mult, ok := map[string]time.Duration{
		"second": time.Second, "minute": time.Minute, "hour": time.Hour,
		"day": 24 * time.Hour, "week": 7 * 24 * time.Hour,
		"month": 30 * 24 * time.Hour, "year": 365 * 24 * time.Hour,
	}[strings.TrimSuffix(unit, "s")]
	if !ok {
		return -1
	}
	return time.Duration(v * float64(mult))
}

// Importer says an argument vector loads or builds images: kind load and
// kind create (which loads its node image), docker build, load, save and
// import, buildx, and containerd's ctr import. Global options and their
// values precede the command path; command arguments are never verbs.
func Importer(args []string) bool {
	if len(args) < 2 {
		return false
	}
	name := args[0][strings.LastIndex(args[0], "/")+1:]
	const build, buildx = "build", "buildx"
	dockerFlags := []string{"--config", "--context", "-c", "--host", "-H", "--log-level", "-l", "--tlscacert", "--tlscert", "--tlskey"}
	spec := map[string]struct{ flags, groups, verbs []string }{
		"kind": {
			flags: []string{"--verbosity", "-v"},
			verbs: []string{"load", "create"},
		},
		"docker": {
			flags: dockerFlags, groups: []string{imageCommand, builderCommand, "container"},
			verbs: []string{build, buildx, "load", "save", "import"},
		},
		"docker-buildx": {
			flags: append(slices.Clone(dockerFlags), "--builder"), groups: []string{buildx},
			verbs: []string{build, "bake"},
		},
		"buildctl": {
			flags: []string{"--addr", "--timeout", "--log-format", "--tlscacert", "--tlscert", "--tlskey", "--tlsservername", "--tlsdir"},
			verbs: []string{build},
		},
		"ctr": {
			flags:  []string{"--address", "-a", "--namespace", "-n", "--timeout", "--connect-timeout"},
			groups: []string{"images", imageCommand, "i"}, verbs: []string{"import"},
		},
	}[name]
	words := commandArgs(args[1:], spec.flags)
	if len(words) == 0 {
		return false
	}
	// Compose takes its own options before its verb, and builds images.
	if name == "docker" && words[0] == "compose" {
		return slices.Contains(words[1:], build)
	}
	// Docker's management groups and the buildx plugin's forwarded
	// prefix introduce a subcommand, not an arbitrary positional argument.
	if slices.Contains(spec.groups, words[0]) {
		words = commandArgs(words[1:], spec.flags)
	}
	if len(words) == 0 {
		return false
	}
	return slices.Contains(spec.verbs, words[0])
}

// commandArgs skips leading options, consuming a separate value only for
// options that take one. Short options with attached values need no skip.
func commandArgs(args, valueFlags []string) []string {
	for len(args) > 0 {
		a := args[0]
		if a == "--" {
			return args[1:]
		}
		if !strings.HasPrefix(a, "-") {
			return args
		}
		args = args[1:]
		flag, _, attached := strings.Cut(a, "=")
		if !attached && slices.Contains(valueFlags, flag) {
			if len(args) == 0 {
				return nil
			}
			args = args[1:]
		}
	}
	return nil
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
