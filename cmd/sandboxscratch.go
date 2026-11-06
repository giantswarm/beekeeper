package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/sandbox"
)

const scratchBrokerEnv = "BEEKEEPER_SCRATCH_BROKER"
const scratchStateKey = "stateDir"

// A scratch broker answers secret requests and readiness pings only.
func secretBrokerOnly(only bool, handler sandbox.Handler) sandbox.Handler {
	return func(ctx context.Context, pid int, req sandbox.Request) (sandbox.Reply, error) {
		if only && req.Op != sandbox.OpPing && req.Op != sandbox.OpSecret && req.Op != sandbox.OpVault {
			return sandbox.Reply{}, refused("this broker serves secret calls only")
		}
		return handler(ctx, pid, req)
	}
}

func (a *app) sandboxScratchCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "scratch -- <command> [args...]",
		Short: "Run a command with a temporary state and its own secret broker",
		Long: `scratch copies the selected configuration into a private temporary directory,
with fresh state, lease and runtime directories. It starts this binary's secret
broker there, then runs the command with BEEKEEPER_CONFIG naming that copy.
Secret calls use this broker even without secret.session. The command should
invoke the candidate binary explicitly. The broker stops and the temporary
directory is removed when the command ends, including on interrupt or failure.
No egress proxy or sandbox mounts are installed. Run this outside the agent
sandbox. Configured secret sources and destinations are real: choose test
inputs and outputs in the selected configuration.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if os.Getenv(sandbox.Env) != "" {
				return refused("the scratch broker runs outside the agent sandbox")
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			dir, err := os.MkdirTemp("", "beekeeper-scratch-")
			if err != nil {
				return err
			}
			defer func() { _ = os.RemoveAll(dir) }()
			cfg := *a.cfg
			cfg.StateDir = filepath.Join(dir, "state")
			cfg.LeaseDir = filepath.Join(dir, "leases")
			source, err := config.Path(a.cfgPath)
			if err != nil {
				return err
			}
			raw, err := os.ReadFile(source) //nolint:gosec // the configuration the caller selected
			if err != nil && !os.IsNotExist(err) {
				return err
			}
			values := map[string]any{}
			if err := yaml.Unmarshal(raw, &values); err != nil {
				return err
			}
			if values == nil {
				values = map[string]any{}
			}
			values[scratchStateKey], values["leaseDir"] = cfg.StateDir, cfg.LeaseDir
			data, err := yaml.Marshal(values)
			if err != nil {
				return err
			}
			path := filepath.Join(dir, "config.yaml")
			if err := os.WriteFile(path, data, 0o600); err != nil {
				return err
			}
			runtimeDir := filepath.Join(dir, "runtime")
			if err := os.Mkdir(runtimeDir, 0o700); err != nil {
				return err
			}
			spool := sandbox.SpoolDir(cfg.StateDir)
			env := slices.DeleteFunc(os.Environ(), func(kv string) bool {
				k, _, _ := strings.Cut(kv, "=")
				return k == "BEEKEEPER_CONFIG" || k == "XDG_RUNTIME_DIR" || k == stateFromEnv || k == scratchBrokerEnv || k == sandbox.Brokered
			})
			env = append(env, "BEEKEEPER_CONFIG="+path, "XDG_RUNTIME_DIR="+runtimeDir)
			exe, err := os.Executable()
			if err != nil {
				return err
			}
			broker := exec.Command(exe, "sandbox", "broker", "--secrets-only") //nolint:gosec // this binary, isolated configuration
			broker.Env = env
			broker.Stdout, broker.Stderr = cmd.ErrOrStderr(), cmd.ErrOrStderr()
			if err := broker.Start(); err != nil {
				return err
			}
			done := make(chan error, 1)
			go func() { done <- broker.Wait() }()
			defer func() {
				_ = broker.Process.Signal(syscall.SIGTERM)
				select {
				case <-done: //nolint:errcheck // The broker was deliberately stopped.
				case <-time.After(brokeredStopDelay):
					_ = broker.Process.Kill()
					<-done //nolint:errcheck // Reap the killed broker; its exit error is expected.
				}
			}()
			deadline := time.Now().Add(10 * time.Second)
			for {
				select {
				case err := <-done:
					// Preserve the completion for the deferred reap.
					done <- err
					return fmt.Errorf("scratch broker stopped before becoming ready: %v", err)
				default:
				}
				if ctx.Err() != nil {
					return ctx.Err()
				}
				if err := sandbox.Ask(spool, sandbox.Request{Op: sandbox.OpPing}, 100*time.Millisecond); err == nil {
					break
				}
				if time.Now().After(deadline) {
					return errors.New("scratch broker did not become ready")
				}
			}
			run := exec.CommandContext(ctx, args[0], args[1:]...) //nolint:gosec // the requested command
			run.Env = append(slices.Clone(env), scratchBrokerEnv+"="+spool)
			run.Stdin, run.Stdout, run.Stderr = cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr()
			run.Cancel = func() error { return run.Process.Signal(syscall.SIGTERM) }
			run.WaitDelay = brokeredStopDelay
			if err := run.Run(); err != nil {
				var exit *exec.ExitError
				if errors.As(err, &exit) {
					return &exitError{code: exit.ExitCode()}
				}
				return err
			}
			return nil
		},
	}
}
