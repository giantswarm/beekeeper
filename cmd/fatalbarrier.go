package cmd

import (
	"cmp"
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/gofrs/flock"

	"github.com/giantswarm/beekeeper/internal/fatal"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/state"
)

func (a *app) fatalStateDir() string {
	if a.store != nil {
		return cmp.Or(a.store.Dir(), a.cfg.StateDir)
	}
	return a.cfg.StateDir
}

var fatalDefaultBranch = func(ctx context.Context, repo string) (string, error) {
	out, err := github.RunGH(ctx, "api", "repos/"+repo, "--jq", ".default_branch // empty")
	branch := strings.TrimSpace(string(out))
	if err != nil || branch == "" {
		return "", fmt.Errorf("default branch of %s unreadable: %v", repo, err)
	}
	return branch, nil
}

// fatalPublicationBarrier protects only the red repository's default branch
// and its releases (pr == 0). The repair's durable PR identity, never the
// caller's display name, grants the exception through review and takeover.
// All existing CI, review, stop and hold checks still apply to that repair.
func (a *app) fatalPublicationBarrier(ctx context.Context, st *state.State, repo string, pr int) error {
	q, err := fatal.Read(a.fatalStateDir())
	if err != nil {
		return fmt.Errorf("fatal publication barrier unreadable: %w", err)
	}
	x, held := q.MainOutage(repo)
	if !held {
		return nil
	}
	if pr == 0 {
		return x.Refusal()
	}
	ref := fmt.Sprintf("%s#%d", repo, pr)
	if strings.EqualFold(x.Repair, ref) {
		return nil
	}
	if st == nil && x.Owner != "" {
		st, err = a.store.Read()
		if err != nil {
			return err
		}
	}
	if fatalRepairPark(st, x.Owner, ref) {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, fatalProbeTimeout)
	defer cancel()
	base, err := PullBase(ctx, repo, pr)
	if err != nil {
		return fmt.Errorf("%v; merge base unreadable: %w", x.Refusal(), err)
	}
	branch, err := fatalDefaultBranch(ctx, repo)
	if err != nil {
		return fmt.Errorf("%v; %w", x.Refusal(), err)
	}
	if base.Base != "" && base.Base != branch {
		return nil
	}
	return x.Refusal()
}

func fatalRepairPark(st *state.State, owner, ref string) bool {
	if owner == "" || st == nil {
		return false
	}
	if rp, ok := st.ReviewParkFor(ref); ok && rp.Agent == owner {
		return true
	}
	for _, ag := range st.Agents {
		if ag.Name == owner && ag.Park != nil && ag.Park.ReviewHead != "" && strings.EqualFold(ag.Park.On, ref) {
			return true
		}
	}
	return false
}

// Keep the repair identity when the loop retires its worker or a reviewer
// hands the same PR to another fixer. Do not hold the main state lock while
// taking fatal.lock: the sentinel takes those locks in the opposite order.
func (a *app) recordFatalRepair(owner, ref string) error {
	r, ok := parseRef(ref)
	if !ok || owner == "" {
		return nil
	}
	dir := a.fatalStateDir()
	q, err := fatal.Read(dir)
	if err != nil {
		return err
	}
	if x, held := q.MainOutage(r.Repo); !held || x.Owner != owner || x.Repair == ref {
		return nil
	}
	ctx, cancel := context.WithTimeout(a.context(), 10*time.Second)
	defer cancel()
	lock := flock.New(filepath.Join(dir, "fatal.lock"))
	locked, err := lock.TryLockContext(ctx, 50*time.Millisecond)
	if err != nil {
		return err
	}
	if !locked {
		return fmt.Errorf("fatal repair identity could not be saved: sentinel busy")
	}
	defer func() { _ = lock.Unlock() }()
	q, err = fatal.Read(dir)
	if err != nil {
		return err
	}
	for i := range q.Incidents {
		x := &q.Incidents[i]
		if x.Resolved.IsZero() && x.Owner == owner && strings.EqualFold(x.Repository(), r.Repo) {
			x.Repair = ref
			return q.Save(dir)
		}
	}
	return nil
}
