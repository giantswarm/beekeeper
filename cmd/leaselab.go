package cmd

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/guard"
	"github.com/giantswarm/beekeeper/internal/sandbox"
)

// agentlabCmd is the lab tool lease up and lease down run.
const agentlabCmd = "agentlab"

func (a *app) leaseLabCmd(op string) *cobra.Command {
	what := map[string]string{labUp: "Create", labDown: "Tear down"}[op]
	return &cobra.Command{
		Use:   op + " <lab>",
		Short: what + " the kind cluster of a lab whose lease you hold, on the host",
		Long: `up creates the lab's kind cluster with agentlab up and writes its kubeconfig
into the lease (lease kubeconfig), down tears it down with agentlab down;
only the session that holds the lab's lease does either, and only for the
cluster the configuration's labs maps the lease to. In the lab's directory
its agentlab.yaml must name that cluster; anywhere else agentlab runs the
lab it knows under that name (agentlab --lab). up asks nothing: no browser,
no trust store. The run is capped as beekeeper run caps it.

In the agent sandbox, which closes the container runtime's socket kind
needs, the host's broker runs it as the session, in its directory, and
streams its output back; beekeeper's hook refuses agentlab up and down and
kind create and delete cluster there and names this command instead.`,
		Example: "  beekeeper lease " + op + " agentlab-1",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			res := args[0]
			if err := a.checkResource(res); err != nil {
				return err
			}
			if a.cfg.LabCluster(res) == "" {
				return usageErr("%s is not a lab: lease %s takes a kind lab's lease (labs in the configuration)", res, op)
			}
			if inSandbox() {
				if err := a.brokeredAnswer(sandbox.Request{Op: sandbox.OpLab, Resource: res, Args: []string{op}}, labBrokeredTimeout+time.Minute, true); err != nil {
					return err
				}
			} else if err := a.labRun(op, res); err != nil {
				return err
			}
			if op == labDown || os.Getenv(sandbox.Brokered) != "" {
				return nil
			}
			line, err := a.labKubeconfigLine(cmd.Context(), res)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(a.out, line)
			return err
		},
	}
}

// labRun runs agentlab op for the lab res, whose lease the caller holds, in
// the working directory, capped as beekeeper run caps it.
func (a *app) labRun(op, res string) error {
	if err := a.holdsLease(res, "lease "+op); err != nil {
		return err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	argv, err := labArgv(op, a.cfg.LabCluster(res), cwd)
	if err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	c := exec.Command(exe, append([]string{"run", "--"}, argv...)...) //nolint:gosec // this binary running agentlab with checked arguments
	c.Stdout, c.Stderr = a.out, os.Stderr
	var exit *exec.ExitError
	if err := c.Run(); errors.As(err, &exit) {
		return &exitError{code: exit.ExitCode()}
	} else if err != nil {
		return err
	}
	if op == labDown {
		// the lab's kubeconfig in the lease names a cluster that is gone
		if err := os.Remove(labKubeconfig(a.cfg.LeaseDir, res)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

// labArgv is agentlab's command line for op on the kind cluster cluster
// from dir: the lab in dir when its agentlab.yaml names cluster, refused
// when it names another, else the lab agentlab knows by that name.
func labArgv(op, cluster, dir string) ([]string, error) {
	var argv []string
	switch cl, ok := guard.LabCluster(dir); {
	case !ok:
		argv = []string{agentlabCmd, "--lab", cluster, op}
	case cl != cluster:
		return nil, refused("the lab in %s is the kind cluster %s, the lease's is %s: run it in that lab's directory, or claim the lease of %s", dir, cl, cluster, cl)
	default:
		argv = []string{agentlabCmd, op}
	}
	if op == labUp {
		argv = append(argv, "--open=false", "--trust=false")
	}
	return argv, nil
}
