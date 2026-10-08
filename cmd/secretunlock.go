package cmd

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/giantswarm/beekeeper/internal/guard"
	"github.com/giantswarm/beekeeper/internal/omp"
	"github.com/giantswarm/beekeeper/internal/sandbox"
	"github.com/giantswarm/beekeeper/internal/secret"
)

// agentMarkers are the variables an agent session's commands carry: Claude
// Code's, omp's, the sandbox's.
var agentMarkers = []string{"CLAUDECODE", "CLAUDE_CODE_SESSION_ID", "CLAUDE_CODE_ENTRYPOINT", omp.EnvAgent, sandbox.Env, sandbox.Brokered}

// personOnly refuses a command an agent session runs, or one without a
// terminal: the person's own vault unlock.
func personOnly(what string) error {
	for _, k := range agentMarkers {
		if os.Getenv(k) != "" {
			return refused("%s is the person's, in their own terminal: no agent session unlocks the vault (%s is set)", what, k)
		}
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stderr.Fd())) {
		return refused("%s needs the person's terminal: op asks for the account password there", what)
	}
	return nil
}

func (a *app) secretUnlockCmd() *cobra.Command {
	var account string
	c := &cobra.Command{
		Use:   "unlock",
		Short: "Hand the broker an op signin session, where it cannot sign in by itself",
		Long: `unlock is the person's, for a machine without secret.signinCommand: with
one, the broker signs in by itself and nobody runs a command. unlock runs op signin on this
terminal, where the person types the account password into op's own
prompt, and hands the session it prints to the broker over its socket in
the runtime directory, after checking that the listener is the main
process of beekeeper-sandbox.service, running this beekeeper binary as
this user. The session lives in the broker's memory alone, never in a
file, a keyring entry or an agent's environment. Calls waiting on the
vault go on at once.

It refuses in an agent session and without a terminal, and the hook refuses
it in agent sessions too: no agent command opens or completes the unlock.
beekeeper secret lock forgets the session; one op no longer takes is
forgotten by itself.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := personOnly("beekeeper secret unlock"); err != nil {
				return err
			}
			if !a.cfg.Secret.Session {
				return refused("secret.session is off: beekeeper reads the vault as its service account (secret.tokenFile), which needs no unlock")
			}
			path, err := secret.SocketPath()
			if err != nil {
				return err
			}
			if _, err := secret.AskVault(path, secret.VaultRequest{Op: secret.VaultStatus}); err != nil {
				return a.secretLog(refused("%v", err), "unlock", "refused: %v", err)
			}
			args := []string{"signin"}
			if account != "" {
				args = append(args, "--account", account)
			}
			op := exec.CommandContext(cmd.Context(), "op", args...) //nolint:gosec // op's own sign-in, on the person's terminal
			var out bytes.Buffer
			op.Stdin, op.Stdout, op.Stderr = os.Stdin, &out, os.Stderr
			op.Env = withoutVaultEnv(os.Environ())
			if err := op.Run(); err != nil {
				return a.secretLog(&exitError{code: ExitVault, msg: "op signin: " + err.Error()}, "unlock", "failed: op signin: %v", err)
			}
			name, token, err := secret.ParseSignin(out.Bytes())
			out.Reset()
			if err != nil {
				return &exitError{code: ExitVault, msg: err.Error()}
			}
			st, err := secret.AskVault(path, secret.VaultRequest{Op: secret.VaultUnlock, Name: name, Token: token})
			if err != nil {
				err = &exitError{code: ExitVault, msg: err.Error()}
			}
			if err := a.secretLog(err, "unlock", "%s", outcome(err, "the broker holds the vault session")); err != nil {
				return err
			}
			_, err = fmt.Fprintf(a.out, "unlocked: the broker holds the vault session since %s; beekeeper secret lock forgets it\n", st.Since.Format(time.TimeOnly))
			return err
		},
	}
	c.Flags().StringVar(&account, "account", "", "the 1Password account op signs in to (default: op's own choice)")
	return c
}

func (a *app) secretLockCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "lock",
		Short: "Make the broker forget the vault session",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			path, err := secret.SocketPath()
			if err != nil {
				return err
			}
			_, err = secret.AskVault(path, secret.VaultRequest{Op: secret.VaultLock})
			if err != nil {
				err = refused("%v", err)
			}
			if err := a.secretLog(err, "lock", "%s", outcome(err, "the broker forgot the vault session")); err != nil {
				return err
			}
			_, err = fmt.Fprintln(a.out, "locked: the broker holds no vault session")
			return err
		},
	}
}

func (a *app) secretStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   secret.VaultStatus,
		Short: "Whether the broker holds the vault session (never the session): exit 78 while it holds none",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			r, err := sandbox.Call(sandbox.SpoolDir(a.cfg.StateDir), sandbox.Request{Op: sandbox.OpVault}, 10*time.Second)
			if err != nil {
				return refused("%v", err)
			}
			if r.Out != vaultUnlocked {
				return &exitError{code: ExitVault, msg: secret.Locked}
			}
			_, err = fmt.Fprintln(a.out, "unlocked: the broker holds the vault session")
			return err
		},
	}
}

// withoutVaultEnv is env without the variables that carry a vault
// credential.
func withoutVaultEnv(env []string) []string {
	var out []string
	for _, kv := range env {
		if k, _, _ := bytes.Cut([]byte(kv), []byte("=")); !guard.VaultVar.Match(k) {
			out = append(out, kv)
		}
	}
	return out
}
