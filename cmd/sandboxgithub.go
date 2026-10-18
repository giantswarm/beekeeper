package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/merge"
	"github.com/giantswarm/beekeeper/internal/sandbox"
)

// githubRenewal is how the broker reads devctl's App token for the egress
// proxy: every five minutes, as devctl renews it once under ten minutes
// are left, so the proxy never holds a token with less than five. A cold
// devctl right after a boot takes over a minute, so a broker without a
// token waits three; a failed read is tried again after 30s, 1m and 2m.
var githubRenewal = sandbox.GitHubRenewal{
	Every: 5 * time.Minute, Bound: time.Minute, ColdBound: 3 * time.Minute,
	Backoff: []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute},
}

// gateBrokeredTimeout bounds a sandboxed session's gated devctl command on
// the host: the wait for its turn, devctl's own --timeout (45m for a merge)
// and the release wait after it.
const gateBrokeredTimeout = 3 * time.Hour

// listenEgress opens the egress proxy's port and writes what the sandbox
// trusts it with; serve runs the proxy until ctx ends. The token stays in
// the broker's memory, renewed from devctl's App login.
func (a *app) listenEgress(ctx context.Context) (serve func() error, err error) {
	runtimeDir := os.Getenv("XDG_RUNTIME_DIR")
	dir := sandbox.EgressDir(runtimeDir)
	if dir == "" {
		return nil, errors.New("no XDG_RUNTIME_DIR: the egress proxy's CA has nowhere to go")
	}
	ca, err := sandbox.NewCA(config.GitHubHosts, time.Now())
	if err != nil {
		return nil, err
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	if err := sandbox.WriteEgress(dir, ca.PEM(), exe); err != nil {
		return nil, fmt.Errorf("egress: %w", err)
	}
	if err := sandbox.RemoveMaskedGitHub(runtimeDir); err != nil {
		return nil, fmt.Errorf("the masked GitHub token files: %w", err)
	}
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(a.cfg.Sandbox.ProxyPort)))
	if err != nil {
		return nil, fmt.Errorf("egress proxy: %w", err)
	}
	say := func(line string) { _, _ = fmt.Fprintln(a.out, line) }
	say(fmt.Sprintf("egress proxy on %s, its CA in %s", ln.Addr(), dir))
	tok := &sandbox.Token{}
	devctl := a.cfg.Sandbox.Devctl
	go sandbox.KeepGitHub(ctx, tok, githubRenewal, func(ctx context.Context) (string, error) {
		return devctlToken(ctx, devctl)
	}, say)
	e := &sandbox.Egress{
		Allow: a.sandboxPolicy().Domains, Inject: config.GitHubHosts, Token: tok.Get, CA: ca,
		Peer: sandbox.SameUser("/proc"), Say: say,
	}
	return func() error { return e.Serve(ctx, ln) }, nil
}

// devctlToken is devctl's App user token: devctl auth exec hands it to a
// command's environment only, so the command prints it into this process.
// ctx bounds the read (githubRenewal). Its error names the cause: no devctl
// login, which devctl answers by naming `devctl auth login`, or devctl's
// own error.
func devctlToken(ctx context.Context, devctl string) (string, error) {
	var out, errOut bytes.Buffer
	c := exec.CommandContext(ctx, devctl, "auth", "exec", "--", "printenv", "GH_TOKEN") //nolint:gosec // the configured devctl
	c.Stdout, c.Stderr = &out, &errOut
	if err := c.Run(); err != nil {
		said := strings.TrimSpace(errOut.String())
		if strings.Contains(said, "devctl auth login") {
			return "", fmt.Errorf("no devctl login (devctl auth exec: %w: %s)", err, said)
		}
		return "", fmt.Errorf("devctl's own error (devctl auth exec: %w: %s)", err, said)
	}
	return strings.TrimSpace(out.String()), nil
}

// gateBrokered is a gated devctl command in the agent sandbox, where devctl
// finds neither its keychain nor the user's service manager: the host's
// broker runs the same gate as this session and answers its output and
// exit code.
func (a *app) gateBrokered(argv []string, wait time.Duration, queued bool) error {
	if queued {
		return gateRefused("a queued merge runs on the host only")
	}
	return a.brokeredReplyWithin(sandbox.Request{Op: sandbox.OpGate, Args: argv, Wait: wait.String()}, gateBrokeredTimeout+time.Minute)
}

// brokeredGateArgv is the command line of a brokered gate: devctl by its
// configured name, one of the gated commands, the wait bounded.
func brokeredGateArgv(req sandbox.Request, _ bool) ([]string, error) {
	if len(req.Args) == 0 || filepath.Base(req.Args[0]) != merge.Tool {
		return nil, errors.New("the sandbox broker gates devctl only")
	}
	argv := append([]string{merge.Tool}, req.Args[1:]...)
	if _, _, ok := parseGated(argv); !ok && !merge.ParseOwned(argv) {
		return nil, errors.New("the sandbox broker runs devctl pr merge, pr wait, release promote, release wait and rollout wait only")
	}
	wait, err := time.ParseDuration(req.Wait)
	if err != nil || wait <= 0 || wait > gateBrokeredTimeout {
		return nil, fmt.Errorf("wait %q: want a duration up to %s", req.Wait, gateBrokeredTimeout)
	}
	return append([]string{gateCmdName, "--wait", wait.String(), "--"}, argv...), nil
}

// devctlPath is PATH with the configured devctl's directory first, for a
// brokered gate that runs devctl by name.
func devctlPath(devctl string) []string {
	if !filepath.IsAbs(devctl) {
		return nil
	}
	return []string{"PATH=" + filepath.Dir(devctl) + string(os.PathListSeparator) + os.Getenv("PATH")}
}
