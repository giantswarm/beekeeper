package cmd

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/lease"
	"github.com/giantswarm/beekeeper/internal/sandbox"
)

func (a *app) leaseKubeconfigCmd() *cobra.Command {
	var refresh bool
	c := &cobra.Command{
		Use:   "kubeconfig <lab>",
		Short: "Write the kubeconfig of a kind lab whose lease you hold into its lease, and print its path",
		Long: `kubeconfig writes kind's kubeconfig of the lab's cluster into the lease
(<leaseDir>/<lab>/kubeconfig, mode 0600) and prints its path; release frees
the lease and the kubeconfig with it. Only the session that holds the lab's
lease gets one. A lab claim writes it already when the cluster runs; this
writes it once a lab started after its claim does.

In the agent sandbox, which closes the container runtime's socket kind
needs, the host's broker writes it for the session, and the session points
it at its sandbox's SOCKS proxy: the API server is on loopback, which the
sandbox reaches only through the proxy and only on the ports
sandbox.domains lists (127.0.0.1:<port>). The proxy's credentials change
with every Claude Code process, so beekeeper's hook refreshes them before
every command of a sandboxed session that holds a lab lease (--refresh).`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			sandboxed := os.Getenv(sandbox.Env) != "" && os.Getenv(sandbox.Brokered) == ""
			if refresh {
				if !sandboxed {
					return nil
				}
				var errs []error
				for _, res := range args {
					if err := proxyLabKubeconfig(labKubeconfig(a.cfg.LeaseDir, res)); err != nil && !errors.Is(err, fs.ErrNotExist) {
						errs = append(errs, err)
					}
				}
				return errors.Join(errs...)
			}
			if len(args) != 1 {
				return usageErr("kubeconfig takes one lab")
			}
			res := args[0]
			if err := a.checkResource(res); err != nil {
				return err
			}
			line, err := a.labKubeconfigLine(cmd.Context(), res)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(a.out, line)
			return err
		},
	}
	c.Flags().BoolVar(&refresh, "refresh", false, "in the sandbox, point the labs' existing kubeconfigs at this process's sandbox proxy; quiet, a missing one skipped")
	_ = c.Flags().MarkHidden("refresh")
	return c
}

// brokeredKubeconfig has the host's broker write the kubeconfig of the lab
// res for a sandboxed session, points it at the session's sandbox proxy,
// and returns the broker's export line.
func (a *app) brokeredKubeconfig(res string) (string, error) {
	dir := sandbox.SpoolDir(a.cfg.StateDir)
	if !(sandbox.Capper{Dir: dir}).Available() {
		return "", refused("no sandbox broker answers in %s: beekeeper-sandbox.service on the host runs this for the sandbox (beekeeper install)", dir)
	}
	r, err := sandbox.Call(dir, sandbox.Request{Op: sandbox.OpKubeconfig, Resource: res}, brokeredCallTimeout)
	if err != nil {
		return "", refused("%v", err)
	}
	if r.Code != 0 {
		return "", &exitError{code: r.Code, msg: strings.TrimPrefix(strings.TrimSpace(r.Err), "beekeeper: ")}
	}
	if err := proxyLabKubeconfig(labKubeconfig(a.cfg.LeaseDir, res)); err != nil {
		return "", err
	}
	return strings.TrimSpace(r.Out), nil
}

// proxyLabKubeconfig points the lab kubeconfig at path at the SOCKS proxy
// of the sandbox the process runs in, rewriting it only when that changes.
func proxyLabKubeconfig(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	proxy, err := sandbox.SOCKSProxy(os.Getenv)
	if err != nil {
		return err
	}
	out, changed, err := sandbox.ProxyKubeconfig(raw, proxy)
	if err != nil || !changed {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// labKubeconfig is where a lab lease's kubeconfig lies: in the lease, so
// that it goes with it.
func labKubeconfig(leaseDir, res string) string { return filepath.Join(leaseDir, res, "kubeconfig") }

// writeLabKubeconfig writes the kubeconfig of the lab res, whose lease the
// caller holds, and returns its path.
func (a *app) writeLabKubeconfig(ctx context.Context, res string) (string, error) {
	cl := a.cfg.LabCluster(res)
	if cl == "" {
		return "", usageErr("%s is not a lab: a kubeconfig is written for a kind lab's lease only", res)
	}
	if err := a.holdsLease(res, res); err != nil {
		return "", err
	}
	kc, err := secretRun(ctx, "", nil, nil, "kind", "get", "kubeconfig", "--name", cl)
	if err != nil {
		return "", fmt.Errorf("kind cluster %s: %w", cl, err)
	}
	path := labKubeconfig(a.cfg.LeaseDir, res)
	tmp := path + ".tmp"
	// a lease released meanwhile took its directory: the write fails
	if err := os.WriteFile(tmp, kc, 0o600); err != nil {
		return "", err
	}
	return path, os.Rename(tmp, path)
}

// claimKubeconfig is the line a lab claim adds: the kubeconfig's path, or
// why there is none yet.
func (a *app) claimKubeconfig(ctx context.Context, res string) string {
	line, err := a.labKubeconfigLine(ctx, res)
	if err != nil {
		return fmt.Sprintf("\nno kubeconfig yet (%v): beekeeper lease kubeconfig %s once the lab runs", err, res)
	}
	return "\n" + line
}

// labKubeconfigLine writes the kubeconfig of the lab res, whose lease the
// caller holds, through the broker in the sandbox, and returns its export
// line.
func (a *app) labKubeconfigLine(ctx context.Context, res string) (string, error) {
	if os.Getenv(sandbox.Env) != "" && os.Getenv(sandbox.Brokered) == "" {
		return a.brokeredKubeconfig(res)
	}
	path, err := a.writeLabKubeconfig(ctx, res)
	return "export KUBECONFIG=" + path, err
}

// holdsLease refuses, for what, unless the caller holds the lease of res.
func (a *app) holdsLease(res, what string) error {
	me, err := a.caller()
	if err != nil {
		return err
	}
	h, err := lease.Dir(a.cfg.LeaseDir).Get(res)
	if err != nil {
		return err
	}
	if h == nil || !h.Party().Is(me) {
		holder := "nobody"
		if h != nil {
			holder = fmt.Sprintf("%q", cmp.Or(h.Name, h.Holder))
		}
		return refused("%s: the lab lease %s is held by %s, not by you: claim it first (beekeeper lease claim %s)", what, res, holder, res)
	}
	return nil
}
