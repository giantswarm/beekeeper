package cmd

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/gofrs/flock"

	"github.com/giantswarm/beekeeper/internal/dockerprune"
	"github.com/giantswarm/beekeeper/internal/lease"
	"github.com/giantswarm/beekeeper/internal/proc"
)

// A lab machine that loads dev images into kind clusters piles up Docker
// images and build cache nothing removes. Over doctor.dockerImagesMaxGiB the
// watch removes the unused images created or tagged longest ago down to
// three quarters of the cap, the ones a kind lab's node holds a copy of
// last; over doctor.dockerBuildCacheMaxGiB it prunes the build cache
// likewise. An image a container uses is never removed, nor one created or
// tagged within dockerTagSettle or since a held lab lease was claimed (a
// session's dev image not yet loaded), and nothing is pruned while an image
// import or build runs or a lab lease is fresh (its holder may be mid-load).
// A prune that leaves the images over their cap names the images that kept
// them there. Each prune is logged (docker.prune) with the images it
// removed. One process prunes at a time (docker.lock), and the watches
// share when Docker was read last (docker.json).

const (
	dockerFile = "docker.json"
	dockerLock = "docker.lock"
	dockerVerb = "docker.prune"
	// dockerLabSettle is how long after a lab lease's claim its holder
	// may still be loading images: no prune runs before.
	dockerLabSettle = time.Hour
	// dockerTagSettle is how long an image created or tagged stays: the
	// session that built it may load it into a lab hours later.
	dockerTagSettle = 24 * time.Hour
	// dockerLabKeep bounds how far back a held lab lease keeps images, so
	// a forgotten lease does not stop every prune.
	dockerLabKeep = 72 * time.Hour
	// dockerTimeout bounds one look at Docker, prune included.
	dockerTimeout = 15 * time.Minute
)

// dockerRun is one look at Docker's images and build cache.
type dockerRun struct {
	imagesMax, cacheMax int64
	before, after       dockerprune.Usage
	// removed are the images removed, which freed about freed bytes;
	// pruned says the build cache was pruned.
	removed []string
	freed   int64
	pruned  bool
	// blockers are the images that kept the images over their cap after
	// a trim: in use, in flight or refused by Docker.
	blockers []dockerprune.Blocker
	// held says what held the prune back; locked says another process
	// prunes now.
	held   string
	locked bool
	err    error
}

func (r dockerRun) off() bool        { return r.imagesMax == 0 && r.cacheMax == 0 }
func (r dockerRun) imagesOver() bool { return r.imagesMax > 0 && r.before.Images > r.imagesMax }
func (r dockerRun) cacheOver() bool  { return r.cacheMax > 0 && r.before.BuildCache > r.cacheMax }
func (r dockerRun) over() bool       { return r.imagesOver() || r.cacheOver() }
func (r dockerRun) acted() bool      { return len(r.removed) > 0 || r.pruned }
func (r dockerRun) blocked() bool    { return len(r.blockers) > 0 }

// dockerBlockersShown bounds the blockers a line names, the largest first.
const dockerBlockersShown = 5

// String says the run in one line.
func (r dockerRun) String() string {
	if r.off() {
		return "Docker: no cap (doctor.dockerImagesMaxGiB and doctor.dockerBuildCacheMaxGiB negative, or no docker)"
	}
	if r.locked {
		return "Docker: another beekeeper prunes it now"
	}
	head := fmt.Sprintf("Docker: images %s, cap %s; build cache %s, cap %s (doctor.dockerImagesMaxGiB, doctor.dockerBuildCacheMaxGiB)",
		gibStr(r.before.Images), capStr(r.imagesMax), gibStr(r.before.BuildCache), capStr(r.cacheMax))
	var did []string
	if len(r.removed) > 0 {
		did = append(did, fmt.Sprintf("%d unused images removed (about %s), images now %s", len(r.removed), gibStr(r.freed), gibStr(r.after.Images)))
	}
	if r.pruned {
		did = append(did, "build cache pruned to "+gibStr(r.after.BuildCache))
	}
	if r.blocked() {
		did = append(did, "images stay over their cap: "+blockerList(r.blockers))
	}
	switch {
	case r.err != nil && len(did) > 0:
		return fmt.Sprintf("%s; %s, then: %v", head, strings.Join(did, "; "), r.err)
	case r.err != nil:
		return fmt.Sprintf("%s; %v", head, r.err)
	case r.held != "" && len(did) > 0:
		return fmt.Sprintf("%s; %s; the rest waits: %s", head, strings.Join(did, "; "), r.held)
	case r.held != "":
		return fmt.Sprintf("%s; its prune waits: %s", head, r.held)
	case len(did) > 0:
		return head + "; " + strings.Join(did, "; ")
	case r.over():
		var would []string
		if r.imagesOver() {
			would = append(would, "images to "+gibStr(r.imagesMax*3/4))
		}
		if r.cacheOver() {
			would = append(would, "build cache to "+gibStr(r.cacheMax*3/4))
		}
		return head + "; would prune " + strings.Join(would, " and ")
	}
	return head
}

// logLine is the run's line with the images it removed, so a later "No
// such image" is traceable to its prune.
func (r dockerRun) logLine() string {
	if len(r.removed) == 0 {
		return r.String()
	}
	return r.String() + "; removed: " + strings.Join(r.removed, ", ")
}

// blockerList names the largest blockers with what each holds and why,
// and how many more hold how much.
func blockerList(bs []dockerprune.Blocker) string {
	bs = slices.Clone(bs)
	slices.SortStableFunc(bs, func(a, b dockerprune.Blocker) int { return cmp.Compare(b.Image.Unique, a.Image.Unique) })
	var parts []string
	for _, b := range bs[:min(len(bs), dockerBlockersShown)] {
		parts = append(parts, fmt.Sprintf("%s (%s, %s)", b.Image.Name(), gibStr(b.Image.Unique), b.Why))
	}
	if rest := bs[min(len(bs), dockerBlockersShown):]; len(rest) > 0 {
		var n int64
		for _, b := range rest {
			n += b.Image.Unique
		}
		parts = append(parts, fmt.Sprintf("and %d more (%s)", len(rest), gibStr(n)))
	}
	return strings.Join(parts, ", ")
}

func capStr(b int64) string {
	if b == 0 {
		return "none"
	}
	return gibStr(b)
}

// notable says the run is worth a doctor's line: Docker over a cap,
// pruned, held by another prune or unreadable.
func (r dockerRun) notable() bool {
	return !r.off() && (r.over() || r.acted() || r.locked || r.err != nil)
}

// dockerCLI runs the docker CLI; nil without one. Tests replace it
// (TestMain), so no test reads or prunes the machine's Docker.
var dockerCLI = func() dockerprune.Docker {
	if _, err := exec.LookPath("docker"); err != nil {
		return nil
	}
	return runDocker
}()

func runDocker(ctx context.Context, args ...string) ([]byte, error) {
	c := proc.Command(proc.Background(ctx), "docker", args...)
	var stderr bytes.Buffer
	c.Stderr = &stderr
	out, err := c.Output()
	if err != nil {
		return out, fmt.Errorf("docker %s: %w: %s", strings.Join(args[:min(len(args), 2)], " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// dockerHolds says what holds a prune back, "" for nothing: a running
// image import or build, or a lab lease claimed within dockerLabSettle.
// An unreadable process table holds it back too.
func (a *app) dockerHolds() string {
	t, err := proc.Read()
	if err != nil {
		return "the process table is unreadable: " + err.Error()
	}
	var pids []int
	for pid, p := range t.ByPID {
		if dockerprune.Importer(p.Args) {
			pids = append(pids, pid)
		}
	}
	if len(pids) > 0 {
		slices.Sort(pids)
		return "image imports or builds run (PIDs " + pidList(pids) + ")"
	}
	return freshLab(a.heldLabs(), time.Now())
}

// freshLab names the first lab lease claimed within dockerLabSettle of
// now, "" for none. A lease whose claim time is unknown (being claimed)
// is fresh.
func freshLab(labs []lease.Holder, now time.Time) string {
	for _, h := range labs {
		since := h.SinceTime()
		if since.IsZero() {
			return "lab lease " + h.Env + " is being claimed"
		}
		if now.Sub(since) < dockerLabSettle {
			return fmt.Sprintf("lab lease %s was claimed %s ago", h.Env, now.Sub(since).Round(time.Minute))
		}
	}
	return ""
}

// pruneDocker reads the size of Docker's images and build cache and,
// unless dryRun, prunes each over its cap under the lock while nothing
// holds it back (holds), logging what it freed.
func (a *app) pruneDocker(dryRun bool, holds func() string) dockerRun {
	r := dockerRun{imagesMax: a.cfg.Doctor.DockerImagesMax(), cacheMax: a.cfg.Doctor.DockerBuildCacheMax()}
	d := dockerCLI
	if d == nil {
		r.imagesMax, r.cacheMax = 0, 0
	}
	if r.off() {
		return r
	}
	lock := flock.New(filepath.Join(a.store.Dir(), dockerLock))
	ok, err := lock.TryLock()
	if err != nil || !ok {
		r.locked, r.err = err == nil, err
		return r
	}
	defer func() { _ = lock.Unlock() }()
	ctx, cancel := context.WithTimeout(context.Background(), dockerTimeout)
	defer cancel()
	r.before, r.err = dockerprune.ReadUsage(ctx, d)
	r.after = r.before
	if r.err != nil || !r.over() || dryRun {
		return r
	}
	if r.held = holds(); r.held != "" {
		return r
	}
	var errs []error
	if r.cacheOver() {
		if err := dockerprune.PruneBuildCache(ctx, d, r.cacheMax*3/4); err != nil {
			errs = append(errs, err)
		} else {
			r.pruned = true
		}
	}
	if r.imagesOver() {
		errs = append(errs, a.trimDockerImages(ctx, d, &r, holds))
	}
	if r.acted() {
		if u, err := dockerprune.ReadUsage(ctx, d); err == nil {
			r.after = u
		} else {
			errs = append(errs, err)
		}
	}
	if r.after.Images <= r.imagesMax {
		r.blockers = nil
	}
	r.err = errors.Join(errs...)
	if r.acted() {
		_ = a.store.Log(event(watchParty, dockerVerb, "%s", r.logLine()))
	}
	return r
}

// trimDockerImages removes the unused images, the recently produced left
// alone (dockerKeep) and the kind labs' last, until Docker says the images
// are under three quarters of their cap or something holds the prune back.
// The images it could not remove are the run's blockers.
func (a *app) trimDockerImages(ctx context.Context, d dockerprune.Docker, r *dockerRun, holds func() string) error {
	images, err := dockerprune.ListImages(ctx, d)
	if err != nil {
		return err
	}
	held, err := dockerprune.KindImages(ctx, d)
	if err != nil {
		return fmt.Errorf("no image removed: %w", err)
	}
	busy := func() bool { r.held = holds(); return r.held != "" }
	size := func() (int64, error) {
		u, err := dockerprune.ReadUsage(ctx, d)
		return u.Images, err
	}
	keep := dockerKeep(a.heldLabs(), time.Now())
	t, err := dockerprune.TrimImages(ctx, d, dockerprune.Candidates(images, held, keep), r.before.Images, r.imagesMax*3/4, size, busy)
	r.freed, r.removed = t.Freed, t.Removed
	switch {
	case errors.Is(err, dockerprune.ErrBusy):
		return nil
	case err != nil:
		return err
	}
	r.blockers = append(dockerprune.Kept(images, keep), t.Refused...)
	return nil
}

// dockerKeep is the time since which a created or tagged image stays:
// dockerTagSettle before now, or the claim of the oldest held lab lease if
// earlier, as its holder may still load what it built for it, though never
// before dockerLabKeep. A lease whose claim time is unknown keeps the
// settle window.
func dockerKeep(labs []lease.Holder, now time.Time) time.Time {
	keep, floor := now.Add(-dockerTagSettle), now.Add(-dockerLabKeep)
	for _, h := range labs {
		if since := h.SinceTime(); !since.IsZero() && since.Before(keep) {
			keep = since
		}
	}
	if keep.Before(floor) {
		return floor
	}
	return keep
}

// dockerMark is the watches' shared memory of Docker: when its size was
// read last, and since when a prune waits.
type dockerMark = goCacheMark

// docker reads Docker's sizes every doctor.dockerEvery and prunes what is
// over its cap, in the background so the sample goes on. A prune an import
// or a fresh lab lease holds back is tried again every goCacheRetry; once
// it has waited doctor.dockerEvery the watch says DOCKER until it ran. A
// failed read or prune, or one that leaves the images over their cap, is
// DOCKER too.
func (w *watcher) docker(now time.Time) {
	if !w.dockerOn || dockerCLI == nil || (w.cfg.Doctor.DockerImagesMax() == 0 && w.cfg.Doctor.DockerBuildCacheMax() == 0) ||
		!w.dockerPruning.CompareAndSwap(false, true) {
		return
	}
	if now.Before(w.dockerNext) {
		w.dockerPruning.Store(false)
		return
	}
	w.bg.Add(1)
	go func() {
		defer w.bg.Done()
		defer w.dockerPruning.Store(false)
		every := w.cfg.Doctor.DockerEvery.Duration
		var m dockerMark
		_, _ = w.store.ReadFile(dockerFile, &m)
		if m.Waiting.IsZero() && now.Sub(m.Checked) < every {
			w.dockerNext = m.Checked.Add(every)
			return
		}
		r := w.pruneDocker(false, w.dockerHolds)
		switch {
		case r.locked:
			w.dockerNext = now.Add(goCacheRetry)
			return
		case r.held != "":
			if m.Waiting.IsZero() {
				m.Waiting = now
			}
			w.dockerNext = now.Add(goCacheRetry)
		default:
			m.Waiting = time.Time{}
			w.dockerNext = now.Add(every)
		}
		m.Checked = now
		_ = w.store.WriteFile(dockerFile, m)
		waited := !m.Waiting.IsZero() && now.Sub(m.Waiting) >= every
		w.check("docker", r.err != nil || r.blocked() || waited, "DOCKER: %s", r)
	}()
}
