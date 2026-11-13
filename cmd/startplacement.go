package cmd

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/giantswarm/beekeeper/internal/capacity"
	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/loop"
	"github.com/giantswarm/beekeeper/internal/state"
)

// hostStartView is capacity --host's verdict for one start on the execution
// host selector names: the host's admission and probe, the coordinator's
// provider pool and only the harnesses the host offers. It takes no probe
// lock, as the start it judges takes none.
func (a *app) hostStartView(ctx context.Context, selector string, st *state.State, sessions []*claude.Session, builds bool, exclude []string, kind capacity.StartFor, harness, model string) (*capacity.View, error) {
	h, err := a.executionHost(selector)
	if err != nil {
		return nil, err
	}
	if err := a.admitExecution(selector, h); err != nil {
		return nil, err
	}
	caps, err := a.probeExecution(ctx, h)
	if err != nil {
		return nil, err
	}
	k := a.liveCapacity()
	var legacy *capacity.UsageView
	if len(k.Providers) == 0 {
		k, legacy = a.resolveUsageForStart(ctx, k, kind)
	}
	return a.executionCapacity(st, sessions, k, h, caps.Resources, a.executionReadings(ctx, k), legacy, builds, append(slices.Clone(exclude), hostUnoffered(caps)...), kind, harness, model), nil
}

// handStart is what placeHandStart weighs of a start made without --host.
type handStart struct {
	brief, repository, ref, dir, harness, model, mode string
	review, desktop, builds, byTimer                  bool
	kind                                              capacity.StartFor
	exclude                                           []string
}

// handPlacement is where a start without --host runs: Host, an execution
// host's selector, with the Repository and Ref it pins; else this machine,
// and Local says why when hosts were configured to take it.
type handPlacement struct {
	Host, Repository, Ref, Local string
}

// placeHandStart places a start the guide or a person makes without --host
// as the supervisor loop places its own (supervisor.hostsFirst): on the first
// of supervisor.hosts that maps the start's repository, offers its harness
// and has room for it (capacity --host), pinned to --ref, else to the
// repository's default branch. The loop's own starts come placed. A start
// stays on this machine, saying why, when it is a review (the loop records a
// host review's assignment), a timer's start, a desktop or background
// session, names no repository, or no host takes it; with hostsFirst false or
// no host configured it stays there as before.
func (a *app) placeHandStart(ctx context.Context, s handStart) handPlacement {
	k := a.cfg.Supervisor
	if a.as == loop.LoopParty.Name || !k.HostsFirst || len(k.Hosts) == 0 {
		return handPlacement{}
	}
	local := func(format string, args ...any) handPlacement {
		return handPlacement{Local: fmt.Sprintf(format, args...)}
	}
	switch {
	case s.review:
		return local("a review start stays here: the loop places reviewers and records a host review's assignment")
	case s.byTimer:
		return local("a timer's start stays here, where its timer records it")
	case s.desktop:
		return local("--desktop asks for a session on this machine")
	case s.mode != "" && s.mode != executionHeadless:
		return local("--mode %s runs on this machine; hosts run headless sessions", s.mode)
	case s.harness == capacity.HarnessAuto && len(a.liveCapacity().Choices) == 0:
		return local("no capacity.choices to give a host start its model")
	case s.harness != capacity.HarnessAuto && !execution.SupportedHarness(s.harness):
		return local("harness %s has no execution host adapter", s.harness)
	case s.harness != capacity.HarnessAuto && s.model == "":
		return local("harness %s names no model for a host start (--model)", s.harness)
	}
	repo, err := a.handStartRepository(ctx, s.repository, s.brief, s.dir)
	if err != nil {
		return local("%v", err)
	}
	st, err := a.readSnapshot()
	if err != nil {
		return local("state unreadable for host placement: %v", err)
	}
	sessions, _, err := a.sessions()
	if err != nil {
		return local("sessions unreadable for host placement: %v", err)
	}
	var reasons []string
	for _, selector := range k.Hosts {
		h, err := a.executionHost(selector)
		if err != nil {
			reasons = append(reasons, fmt.Sprintf("%s: %s", selector, oneLine(err.Error())))
			continue
		}
		if !h.AdmitsRepository(repo) {
			reasons = append(reasons, fmt.Sprintf("%s: %s is not mapped", selector, repo))
			continue
		}
		if s.harness != capacity.HarnessAuto && !h.Supports(s.harness) {
			reasons = append(reasons, fmt.Sprintf("%s: does not list harness %s", selector, s.harness))
			continue
		}
		v, err := a.hostStartView(ctx, selector, st, sessions, s.builds, s.exclude, s.kind, s.harness, s.model)
		if err != nil {
			reasons = append(reasons, fmt.Sprintf("%s: %s", selector, oneLine(err.Error())))
			continue
		}
		if v.Room <= 0 {
			reasons = append(reasons, fmt.Sprintf("%s: %s", selector, oneLine(v.Verdict())))
			continue
		}
		ref := s.ref
		if ref == "" {
			if ref, err = loop.GhDefaultBranch(ctx, repo); err != nil || ref == "" {
				reasons = append(reasons, fmt.Sprintf("%s: no default branch of %s to pin (%v)", selector, repo, err))
				continue
			}
		}
		return handPlacement{Host: selector, Repository: repo, Ref: ref}
	}
	return local("no host takes it: %s", strings.Join(reasons, "; "))
}

// placementBrief is the brief a start is placed by, "" when this session
// may not read it here: the start itself then says why.
func (a *app) placementBrief(path string) string {
	if a.sandboxPaths([]string{path}, nil) != nil {
		return ""
	}
	brief, _ := readBrief(path)
	return brief
}

// handStartRepository is the repository a start names: --repo, else the one
// its brief names, else the origin of its checkout (--dir, else agents.dir,
// else the current directory).
func (a *app) handStartRepository(ctx context.Context, repository, brief, dir string) (string, error) {
	if r := strings.TrimSpace(repository); r != "" {
		return r, nil
	}
	switch repos := briefRepositories(brief); len(repos) {
	case 0:
	case 1:
		return repos[0], nil
	default:
		return "", fmt.Errorf("the brief names several repositories (%s)", strings.Join(repos, ", "))
	}
	checkout, err := a.agentDir(dir, "")
	if err == nil {
		var out []byte
		if out, err = runGit(ctx, checkout, gitRemoteCommand, "get-url", workerGitHubOrigin); err == nil {
			if r := execution.RepositoryIdentity(string(out)); r != "" {
				return r, nil
			}
		}
	}
	return "", fmt.Errorf("the start names no repository: no --repo, none in its brief and no GitHub origin of its checkout")
}
