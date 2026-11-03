package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/execution"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/internal/update"
	"github.com/giantswarm/beekeeper/pkg/project"
)

// Seams the tests replace: the coordinator's release, the host's install of
// a release, the installed release's prepare and the restarts
// onto it.
var (
	coordinatorVersion = project.Version
	installVersion     = update.InstallVersion
	// lookupRelease is the coordinator's authenticated lookup of the
	// release a host installs; installRelease the host's install of it.
	lookupRelease  = update.LookupVersion
	installRelease = update.InstallRelease
	// runHostPrepare runs the release an update installed at path and
	// returns its standard output: its answer, never its diagnostics.
	runHostPrepare = func(ctx context.Context, path string, args ...string) ([]byte, error) {
		return exec.CommandContext(ctx, path, args...).Output() //nolint:gosec // the release this update verified and installed
	}
	// restartOnInstalled moves this host's watches, waiting reopens and
	// sandbox broker onto the release installed at path, as self-update
	// does.
	restartOnInstalled = func(ctx context.Context, a *app, path string, written time.Time) {
		t, err := proc.Read()
		if err != nil {
			return
		}
		_, _ = a.restartReplaced(io.Discard, path, t, false)
		restartBrokerOn(ctx, io.Discard, path, written, t, false, a.cfg.Secret.Session)
	}
)

// hostUpdateRetry paces a held host's or a busy host's next update attempt.
const hostUpdateRetry = time.Hour

// hostUpdateCmdName is the coordinator's update of an execution host.
const hostUpdateCmdName = "update"

// hostVersionFlag names the release a host's self-update installs.
const hostVersionFlag = "version"

// releaseVersion says whether v is a release tag a host can install, a
// release ("v0.3.0") or a release candidate ("v0.3.0-rc.2"): not a
// development, pseudo-version or make build
// ("v0.3.0-r123t20261102031105habc1234").
func releaseVersion(v string) bool {
	return releaseTag.MatchString(v)
}

// releaseTag is the shape of the release tags.
var releaseTag = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-rc\.(0|[1-9][0-9]*))?$`)

func (a *app) executionHostUpdateCmds() []*cobra.Command {
	updateCmd := &cobra.Command{Use: hostUpdateCmdName + " <host>", Short: "Update an execution host to this coordinator's release, prepare its policy and pin its digest in beekeeper's state", Long: `Update the execution host to this coordinator's release and record the
policy digest its starts are pinned to in beekeeper's own state, where
agents.hosts no longer needs one.

Over the host's SSH route, the host's beekeeper (agents hosts self-update)
installs exactly this coordinator's release once its Sigstore bundle
verifies, replacing the binary atomically without a backup. The installed
release prepares its policy. A release that does not prepare holds the host. The
host answers its release and policy digest; once its check (agents hosts
check) passes with that digest, the digest is the host's pin.

While executions run on the host nothing changes and the host drains: it
takes no new start until it runs this release (exit 3). An update that
fails holds the host: it takes no start until an update succeeds (exit 3),
and the supervisor loop hands the hold on as one finding. The other hosts
keep running. The supervisor loop runs this for every agents.hosts entry
once the coordinator runs a newer release, and retries a held host hourly.
A development build pins no release.

The coordinator looks the release up on GitHub itself, with the gh login
(or GH_TOKEN, GITHUB_TOKEN) as every other GitHub call of beekeeper, never
anonymously, and hands it to the host: the host downloads the binary and
its bundle from the release's public download URLs and asks GitHub's API
nothing, so a host without a GitHub login, whose anonymous API limit its IP
may share, updates all the same. A lookup GitHub refuses holds the host,
a rate limit named with its reset.`, Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
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
replacing the binary atomically without a backup, and have the installed
release prepare its own policy. A release that does not prepare holds the
host until a retry succeeds. The answer is one JSON line: the release, the
policy digest, the executions still running (busy, nothing changed) or the
error.

The coordinator sends its lookup of the release on standard input: the
binary and its bundle are downloaded from the release's public download
URLs, which GitHub's API limits do not cover, and the host asks GitHub's API
nothing. Without it (a terminal, or an empty input) the host looks the
release up itself, with its own gh login or GH_TOKEN, GITHUB_TOKEN, never
anonymously.`, Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		given, err := hostReleaseInput(cmd.InOrStdin())
		if err != nil {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(execution.HostUpdate{Protocol: execution.Protocol, Error: err.Error()})
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(a.hostSelfUpdate(cmd.Context(), protocol, version, given))
	}}
	selfUpdate.Flags().StringVar(&version, hostVersionFlag, "", "the release to install, the coordinator's")
	selfUpdate.Flags().IntVar(&protocol, "protocol", 0, "execution protocol")
	return []*cobra.Command{updateCmd, selfUpdate}
}

// hostReleaseInput is the coordinator's release lookup on the standard
// input r of agents hosts self-update; nil from a terminal, which nobody
// writes it to, and from an empty input.
func hostReleaseInput(r io.Reader) ([]byte, error) {
	if f, ok := r.(*os.File); ok {
		if info, err := f.Stat(); err != nil || info.Mode()&os.ModeCharDevice != 0 {
			return nil, nil
		}
	}
	raw, err := io.ReadAll(io.LimitReader(r, execution.MaxWire+1))
	if err != nil {
		return nil, fmt.Errorf("reading the coordinator's release: %w", err)
	}
	if len(raw) > execution.MaxWire {
		return nil, errors.New("the coordinator's release exceeds the limit")
	}
	return bytes.TrimSpace(raw), nil
}

// hostSelfUpdate is agents hosts self-update on an execution host. It always
// answers, an error included, so that the coordinator reads why. given is
// the coordinator's lookup of version (update.Release), nil for none: the
// host then looks the release up itself.
func (a *app) hostSelfUpdate(ctx context.Context, protocol int, version string, given []byte) execution.HostUpdate {
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
	install := func(ctx context.Context) (update.Result, error) { return installVersion(ctx, io.Discard, version) }
	if len(given) > 0 {
		// Not strict: the host runs the older release while it updates,
		// and a newer coordinator may send more than it knows.
		var rel update.Release
		if err := json.Unmarshal(given, &rel); err != nil {
			return fail(fmt.Errorf("the coordinator's release: %w", err))
		}
		if rel.Tag != version {
			return fail(fmt.Errorf("the coordinator sent release %q, not %s", rel.Tag, version))
		}
		install = func(ctx context.Context) (update.Result, error) { return installRelease(ctx, io.Discard, rel) }
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
		if err := s.ProvisionTools(ctx); err != nil {
			release()
			return fail(err)
		}
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
	written := time.Now()
	res, err := install(ctx)
	release()
	if err != nil {
		return fail(err)
	}
	raw, err := runHostPrepare(ctx, res.Path, "--"+configFlag, s.ConfigPath, agentsName, "hosts", "self-update", "--protocol", strconv.Itoa(execution.Protocol), "--"+hostVersionFlag, version)
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
		restartOnInstalled(ctx, a, res.Path, written)
		return next
	}
	return fail(fmt.Errorf("%s did not prepare its policy: %s; host remains held until an update succeeds", version, reason))
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

// updateHost has host h install and prepare release version, which the
// coordinator looks up for it.
func (a *app) updateHost(ctx context.Context, h execution.Host, version string) (execution.HostUpdate, error) {
	rel, err := lookupRelease(ctx, version)
	if err != nil {
		return execution.HostUpdate{}, err
	}
	given, err := json.Marshal(rel)
	if err != nil {
		return execution.HostUpdate{}, err
	}
	if a.executionUpdate != nil {
		return a.executionUpdate(ctx, h, version, given)
	}
	return execution.UpdateHost(ctx, h, version, given, nil)
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
