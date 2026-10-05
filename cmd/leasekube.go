package cmd

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/lease"
	"github.com/giantswarm/beekeeper/internal/sandbox"
)

func (a *app) leaseKubeconfigCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "kubeconfig <lab>",
		Short: "Write the kubeconfig of a kind lab whose lease you hold into its lease, and print its path",
		Long: `kubeconfig writes kind's kubeconfig of the lab's cluster into the lease
(<leaseDir>/<lab>/kubeconfig, mode 0600) and prints its path; release frees
the lease and the kubeconfig with it. Only the session that holds the lab's
lease gets one. A lab claim writes it already when the cluster runs; this
writes it once a lab started after its claim does.

In the agent sandbox, which closes the container runtime's socket kind
needs, the host's broker writes it for the session.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			res := args[0]
			if err := a.checkResource(res); err != nil {
				return err
			}
			if os.Getenv(sandbox.Env) != "" && os.Getenv(sandbox.Brokered) == "" {
				return a.brokeredReply(sandbox.Request{Op: sandbox.OpKubeconfig, Resource: res})
			}
			path, err := a.writeLabKubeconfig(cmd.Context(), res)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(a.out, "export KUBECONFIG=%s\n", path)
			return err
		},
	}
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
	var path string
	var err error
	if os.Getenv(sandbox.Env) != "" && os.Getenv(sandbox.Brokered) == "" {
		dir := sandbox.SpoolDir(a.cfg.StateDir)
		var r sandbox.Reply
		if !(sandbox.Capper{Dir: dir}).Available() {
			err = fmt.Errorf("no sandbox broker answers in %s", dir)
		} else {
			r, err = sandbox.Call(dir, sandbox.Request{Op: sandbox.OpKubeconfig, Resource: res}, brokeredCallTimeout)
		}
		if err == nil && r.Code != 0 {
			err = fmt.Errorf("%s", strings.TrimSpace(r.Err))
		}
		if err == nil {
			return "\n" + strings.TrimSpace(r.Out)
		}
	} else {
		path, err = a.writeLabKubeconfig(ctx, res)
	}
	if err != nil {
		return fmt.Sprintf("\nno kubeconfig yet (%v): beekeeper lease kubeconfig %s once the lab runs", err, res)
	}
	return "\nexport KUBECONFIG=" + path
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
