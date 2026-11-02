package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/Masterminds/semver/v3"
	"github.com/spf13/cobra"
	"golang.org/x/mod/module"

	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/update"
	"github.com/giantswarm/beekeeper/pkg/project"
)

// Seams the tests replace: the coordinator's release, the host's install of
// a release, its rollback and the installed release's prepare.
var (
	coordinatorVersion = project.Version
	installVersion     = update.InstallVersion
	rollbackInstall    = update.Rollback
	// runHostPrepare runs the release an update installed at path and
	// returns its standard output: its answer, never its diagnostics.
	runHostPrepare = func(ctx context.Context, path string, args ...string) ([]byte, error) {
		return exec.CommandContext(ctx, path, args...).Output() //nolint:gosec // the release this update verified and installed
	}
)

// hostUpdateRetry paces a held host's or a busy host's next update attempt.
const hostUpdateRetry = time.Hour

// hostUpdateCmdName is the coordinator's update of an execution host.
const hostUpdateCmdName = "update"

// releaseVersion says whether v is a release tag a host can install
// ("v0.3.0", "v0.3.0-rc.2"), not a development or pseudo-version build.
func releaseVersion(v string) bool {
	_, err := semver.StrictNewVersion(strings.TrimPrefix(v, "v"))
	return err == nil && strings.HasPrefix(v, "v") && !strings.Contains(v, "+") && !module.IsPseudoVersion(v)
}

func (a *app) executionHostUpdateCmds() []*cobra.Command {
	updateCmd := &cobra.Command{Use: hostUpdateCmdName + " <host>", Short: "Update an execution host to this coordinator's release, prepare its policy and pin its digest in beekeeper's state", Long: `Update the execution host to this coordinator's release and record the
policy digest its starts are pinned to in beekeeper's own state, where
agents.hosts no longer needs one.

Over the host's SSH route, the host's beekeeper (agents hosts self-update)
installs exactly this coordinator's release once its Sigstore bundle
verifies, the replaced binary kept beside it, and the installed release
prepares its policy. A release that does not prepare is rolled back. The
host answers its release and policy digest; once its check (agents hosts
check) passes with that digest, the digest is the host's pin.

While executions run on the host nothing changes and the host drains: it
takes no new start until it runs this release (exit 3). An update that
fails holds the host: it takes no start until an update succeeds (exit 3),
and the supervisor loop hands the hold on as one finding. The other hosts
keep running. The supervisor loop runs this for every agents.hosts entry
once the coordinator runs a newer release, and retries a held host hourly.
A development build pins no release.`, Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		line, err := a.updateExecutionHost(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(cmd.OutOrStdout(), line)
		return err
	}}
	var version string
	var protocol int
	selfUpdate := &cobra.Command{Use: "self-update --version <release>", Short: "On an execution host: install the coordinator's release, verified, and prepare its policy", Long: `Run on the execution host by the coordinator's agents hosts update, over the
host's SSH route: with no execution running, install exactly the release
--version names once its Sigstore bundle verifies (self-update's check),
keeping the replaced binary beside it, and have the installed release
prepare its own policy. A release that does not prepare is rolled back and
this one prepares again. The answer is one JSON line: the release, the
policy digest, the executions still running (busy, nothing changed) or the
error.`, Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(a.hostSelfUpdate(cmd.Context(), protocol, version))
	}}
	selfUpdate.Flags().StringVar(&version, "version", "", "the release to install, the coordinator's")
	selfUpdate.Flags().IntVar(&protocol, "protocol", 0, "execution protocol")
	return []*cobra.Command{updateCmd, selfUpdate}
}

// hostSelfUpdate is agents hosts self-update on an execution host. It always
// answers, an error included, so that the coordinator reads why.
func (a *app) hostSelfUpdate(ctx context.Context, protocol int, version string) execution.HostUpdate {
	out := execution.HostUpdate{Protocol: execution.Protocol}
	fail := func(err error) execution.HostUpdate {
		out.Error = err.Error()
		return out
	}
	if protocol != execution.Protocol || a.managedExecutionID() != "" {
		return fail(errors.New("invalid host update caller or protocol"))
	}
	if !releaseVersion(version) {
		return fail(fmt.Errorf("%q is no release version", version))
	}
	s, err := a.hostRuntime()
	if err != nil {
		return fail(err)
	}
	out.Identity, out.Version = s.Config.Identity, s.Version
	busy, release, err := s.Quiesce(ctx)
	if err != nil {
		return fail(err)
	}
	if busy > 0 {
		release()
		out.Busy = busy
		return out
	}
	if s.Version == version {
		release()
		digest, err := s.Prepare(s.Policy())
		if err != nil {
			return fail(err)
		}
		out.PolicyDigest = digest
		return out
	}
	// Admission stays held through the install: no execution starts on
	// this binary's policy to find it replaced. Once released, a start
	// finds the installed policy stale and fails closed until the new
	// release prepared it.
	res, err := installVersion(ctx, io.Discard, version)
	release()
	if err != nil {
		return fail(err)
	}
	raw, err := runHostPrepare(ctx, res.Path, "--"+configFlag, s.ConfigPath, agentsName, "hosts", "self-update", "--protocol", strconv.Itoa(execution.Protocol), "--version", version)
	var next execution.HostUpdate
	reason := ""
	switch {
	case err != nil:
		reason = err.Error()
	case execution.Decode(raw, &next) != nil:
		reason = "no answer"
	case next.Error != "":
		reason = next.Error
	case next.Version != version || !execution.ValidDigest(next.PolicyDigest):
		reason = "no policy digest of " + version
	default:
		return next
	}
	if !res.Updated {
		return fail(fmt.Errorf("%s did not prepare its policy: %s", version, reason))
	}
	if err := rollbackInstall(res.Path); err != nil {
		return fail(fmt.Errorf("%s did not prepare its policy (%s) and the rollback to %s failed: %w", version, reason, s.Version, err))
	}
	if _, err := s.Prepare(s.Policy()); err != nil {
		return fail(fmt.Errorf("%s did not prepare its policy (%s); rolled back to %s, which did not prepare either: %w", version, reason, s.Version, err))
	}
	return fail(fmt.Errorf("%s did not prepare its policy (%s); rolled back to %s", version, reason, s.Version))
}

// errHostDrains is an update that waits for the executions on a host.
var errHostDrains = errors.New("executions run")

// updateExecutionHost is agents hosts update: the host on this
// coordinator's release, its digest pinned, or held with why.
func (a *app) updateExecutionHost(ctx context.Context, selector string) (string, error) {
	h, err := a.catalogHost(selector)
	if err != nil {
		return "", err
	}
	version := coordinatorVersion()
	if !releaseVersion(version) {
		return "", refused("this coordinator runs %s, no release: a development build pins no execution host release", version)
	}
	st, err := a.store.Read()
	if err != nil {
		return "", err
	}
	h = pinnedHost(st, selector, h)
	if n := hostExecutions(st, selector); n > 0 {
		return "", refused("execution host %s drains: %d executions run on it; it updates to %s once they end", selector, n, version)
	}
	x, err := a.updateHost(ctx, h, version)
	if err == nil && x.Busy > 0 {
		err = fmt.Errorf("%w: %d", errHostDrains, x.Busy)
	}
	if err == nil {
		h.PolicyDigest = x.PolicyDigest
		if _, perr := a.probeExecution(ctx, h); perr != nil {
			err = fmt.Errorf("its check with the prepared policy failed: %w", perr)
		}
	}
	now := a.now.UTC()
	by, _ := a.caller()
	if serr := a.store.Update(func(st *state.State) ([]state.Event, error) {
		pin := st.HostPin(selector)
		if pin == nil {
			st.HostPins = append(st.HostPins, state.HostPin{Selector: selector})
			pin = &st.HostPins[len(st.HostPins)-1]
		}
		pin.Identity, pin.Tried = h.Identity, now
		switch {
		case errors.Is(err, errHostDrains):
			return nil, nil
		case err != nil:
			if pin.Held == "" || pin.HeldVersion != version {
				pin.HeldAt, pin.Reported = now, false
			}
			pin.Held, pin.HeldVersion = oneLine(err.Error()), version
			return []state.Event{event(by, "agents.host-held", "%s on %s: %s", selector, version, pin.Held)}, nil
		}
		pin.Version, pin.PolicyDigest, pin.PinnedAt = version, x.PolicyDigest, now
		pin.Held, pin.HeldAt, pin.HeldVersion, pin.Reported = "", time.Time{}, "", false
		return []state.Event{event(by, "agents.host-pinned", "%s on %s, policy %s", selector, version, x.PolicyDigest)}, nil
	}); serr != nil {
		return "", serr
	}
	switch {
	case errors.Is(err, errHostDrains):
		return "", refused("execution host %s drains: %d executions run on it; it updates to %s once they end", selector, x.Busy, version)
	case err != nil:
		return "", refused("execution host %s is held, it takes no start: its update to %s failed: %v", selector, version, err)
	}
	return fmt.Sprintf("%s runs %s, policy %s pinned", selector, version, x.PolicyDigest), nil
}

// updateHost has host h install and prepare release version.
func (a *app) updateHost(ctx context.Context, h execution.Host, version string) (execution.HostUpdate, error) {
	if a.executionUpdate != nil {
		return a.executionUpdate(ctx, h, version)
	}
	return execution.UpdateHost(ctx, h, version, nil)
}

// hostExecutions counts the executions the coordinator recorded on the
// host selector that have not ended.
func hostExecutions(st *state.State, selector string) int {
	n := 0
	for i := range st.Starts {
		if x := st.Starts[i].Execution; x != nil && x.Selector == selector && !x.Terminal() {
			n++
		}
	}
	return n
}

// pinnedHost is h with the policy digest beekeeper pinned for selector,
// the entry's own (deprecated) digest for a host it never pinned.
func pinnedHost(st *state.State, selector string, h execution.Host) execution.Host {
	if p := st.HostPin(selector); p != nil && p.Identity == h.Identity && p.PolicyDigest != "" {
		h.PolicyDigest = p.PolicyDigest
	}
	return h
}

// admitExecution says whether the host selector takes a new start: not
// while it is held, and, on a coordinator that runs a release, not before
// the host runs that release (it drains, then updates).
func (a *app) admitExecution(selector string, h execution.Host) error {
	st, err := a.store.Peek()
	if err != nil {
		return err
	}
	pin := st.HostPin(selector)
	if pin != nil && pin.Identity != h.Identity {
		pin = nil
	}
	if pin != nil && pin.Held != "" {
		return refused("execution host %s is held since %s, it takes no start: %s (beekeeper agents hosts update %s lifts it)", selector, pin.HeldAt.Format(time.RFC3339), pin.Held, selector)
	}
	if version := coordinatorVersion(); releaseVersion(version) && (pin == nil || pin.Version != version) {
		return refused("execution host %s does not run this coordinator's %s yet, it takes no start until beekeeper agents hosts update %s pins it", selector, version, selector)
	}
	return nil
}
