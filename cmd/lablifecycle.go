package cmd

import (
	"context"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/claude"
	"github.com/giantswarm/beekeeper/internal/lease"
	"github.com/giantswarm/beekeeper/internal/sandbox"
	"github.com/giantswarm/beekeeper/internal/state"
)

const labIdleGrace = 15 * time.Minute

const (
	labStart = "start"
	labStop  = "stop"
)

func (a *app) labDockerCall(ctx context.Context, args ...string) ([]byte, error) {
	if a.labDocker != nil {
		return a.labDocker(ctx, args...)
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput() //nolint:gosec // subcommands and arguments are assembled by the lifecycle functions
	if err != nil {
		return nil, fmt.Errorf("docker %s: %w: %s", args[0], err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

// labNodes selects exact kind cluster labels, including stopped nodes.
func (a *app) labNodes(ctx context.Context, cluster string, running bool) ([]string, error) {
	args := []string{"ps", "--all", "--quiet", "--no-trunc", "--filter", "label=io.x-k8s.kind.cluster=" + cluster}
	if running {
		args = append(args, "--filter", "status=running")
	} else {
		args = append(args, "--filter", "status=exited", "--filter", "status=created")
	}
	out, err := a.labDockerCall(ctx, args...)
	return strings.Fields(string(out)), err
}

// doctorLabs serializes lease release and container stops with claims. A
// failed Docker read never counts as an idle observation. Dry runs save nothing.
func (a *app) doctorLabs(ctx context.Context, sessions []*claude.Session, r doctorRun) []string {
	var lines []string
	update := func(st *state.State) ([]state.Event, error) {
		dir := lease.Dir(a.cfg.LeaseDir)
		holders, err := dir.List()
		if err != nil {
			return nil, err
		}
		var evs []state.Event
		for _, h := range holders {
			if !a.leaseHolderEnded(st, sessions, h) {
				continue
			}
			line := fmt.Sprintf("released %s: holder %q's session ended", h.Env, h.Name)
			if r.dryRun {
				lines = append(lines, "would "+strings.Replace(line, "released ", "release ", 1))
				continue
			}
			configLine, err := a.labRelease(dir, h, r.by, false, "", true)
			if err != nil {
				lines = append(lines, fmt.Sprintf("lease %s: %v", h.Env, err))
				continue
			}
			if err := dir.Release(h.Env); err != nil {
				lines = append(lines, fmt.Sprintf("lease %s: %v", h.Env, err))
				continue
			}
			if st.Released == nil {
				st.Released = map[string]time.Time{}
			}
			st.Released[h.Env] = a.now.UTC()
			lines = append(lines, line+configLine)
			evs = append(evs, event(r.by, leaseReleaseGoneVerb, "%s%s", line, configLine))
		}
		for _, res := range a.cfg.Resources {
			cluster := a.cfg.LabCluster(res)
			if cluster == "" {
				continue
			}
			h, err := dir.Get(res)
			if err != nil {
				lines = append(lines, fmt.Sprintf("lab %s: %v", res, err))
				continue
			}
			if h != nil {
				if !r.dryRun {
					delete(st.LabIdle, res)
				}
				continue
			}
			nodes, err := a.labNodes(ctx, cluster, true)
			if err != nil {
				lines = append(lines, fmt.Sprintf("lab %s: %v", res, err))
				continue
			}
			if len(nodes) == 0 {
				if !r.dryRun {
					delete(st.LabIdle, res)
				}
				continue
			}
			since := st.LabIdle[res]
			if released := st.Released[res]; released.After(since) {
				since = released
			}
			if since.IsZero() {
				since = a.now
			}
			if !r.dryRun {
				if st.LabIdle == nil {
					st.LabIdle = map[string]time.Time{}
				}
				st.LabIdle[res] = since
			}
			if a.now.Sub(since) < labIdleGrace {
				continue
			}
			line := fmt.Sprintf("stopped kind lab %s (%s): unleased for %s", res, cluster, dur(a.now.Sub(since)))
			if r.dryRun {
				lines = append(lines, "would "+strings.Replace(line, "stopped ", "stop ", 1))
				continue
			}
			if _, err := a.labDockerCall(ctx, append([]string{labStop, "--time", "10"}, nodes...)...); err != nil {
				lines = append(lines, fmt.Sprintf("lab %s: %v", res, err))
				continue
			}
			delete(st.LabIdle, res)
			lines = append(lines, line)
			evs = append(evs, event(r.by, "lab.stop", "%s", line))
		}
		return evs, nil
	}
	var err error
	if r.dryRun {
		var st *state.State
		st, err = a.store.Read()
		if err == nil {
			_, err = update(st)
		}
	} else {
		err = a.store.Update(update)
	}
	if err != nil {
		lines = append(lines, fmt.Sprintf("lab lifecycle: %v", err))
	}
	return lines
}

func (a *app) leaseResumeCmd() *cobra.Command {
	return &cobra.Command{
		Use: "resume <lab>", Hidden: true, Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error { return a.resumeLab(cmd.Context(), args[0]) },
	}
}

// resumeLab starts existing stopped nodes only, never creates a cluster.
// The host broker checks the same holder before accessing Docker.
func (a *app) resumeLab(ctx context.Context, res string) error {
	if err := a.checkResource(res); err != nil {
		return err
	}
	cluster := a.cfg.LabCluster(res)
	if cluster == "" {
		return usageErr("%s is not a kind lab", res)
	}
	if inSandbox() {
		return a.brokeredAnswer(sandbox.Request{Op: sandbox.OpLab, Resource: res, Args: []string{labResume}}, 2*time.Minute, true)
	}
	me, err := a.caller()
	if err != nil {
		return err
	}
	return a.store.Update(func(st *state.State) ([]state.Event, error) {
		h, err := lease.Dir(a.cfg.LeaseDir).Get(res)
		if err != nil {
			return nil, err
		}
		if h == nil || !h.Party().Is(me) {
			return nil, refused("resume lab %s: claim its lease first", res)
		}
		nodes, err := a.labNodes(ctx, cluster, false)
		if err != nil || len(nodes) == 0 {
			return nil, err
		}
		running, err := a.runningClusters()
		if err != nil {
			return nil, err
		}
		if !slices.Contains(running, cluster) && len(running) >= a.cfg.KindClusters(ramMiB()) {
			return nil, refused("cannot restart %s: the machine already runs its maximum of %d kind clusters", cluster, len(running))
		}
		if _, err := a.labDockerCall(ctx, append([]string{labStart}, nodes...)...); err != nil {
			return nil, err
		}
		delete(st.LabIdle, res)
		return []state.Event{event(me, "lab.start", "%s (%s): restarted %d kind nodes", res, cluster, len(nodes))}, nil
	})
}
