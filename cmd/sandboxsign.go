package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/sandbox"
)

// Commit signing in the agent sandbox: git's gpg.program there is
// beekeeper sandbox gpg (the policy's egress script), which hands the
// payload to the host's broker; the broker signs it with gpg, where gpg
// reaches the person's agent and key, and answers the signature and gpg's
// status lines. The sandbox never reaches the agent's socket or the key.

// signTimeout bounds a brokered signature: gpg answers at once from its
// agent, or asks for the passphrase.
const signTimeout = 2 * time.Minute

// gpgSignFlags are the arguments git gives gpg.program to sign, ahead of
// the key: a detached armored signature, status lines on stderr.
var gpgSignFlags = []string{"--status-fd=2", "-bsau"}

// gpgSignKey is the key of git's signing call: user.signingkey or the
// committer's identity, never a flag or a control character.
func gpgSignKey(args []string) (string, bool) {
	if len(args) != len(gpgSignFlags)+1 || !slices.Equal(args[:len(gpgSignFlags)], gpgSignFlags) {
		return "", false
	}
	key := args[len(gpgSignFlags)]
	if key == "" || strings.HasPrefix(key, "-") || strings.ContainsFunc(key, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return "", false
	}
	return key, true
}

func (a *app) sandboxGPGCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "gpg --status-fd=2 -bsau KEY",
		Short: "git's gpg.program in the agent sandbox: sign through the host's broker",
		Long: `gpg is git's gpg.program in the agent sandbox, which reaches no gpg-agent:
it reads the payload git signs and has the host's broker sign it with the
person's key, answering the signature and gpg's status lines as gpg would.
It signs only (git's --status-fd=2 -bsau <key>); outside the sandbox it is
gpg itself.`,
		Hidden:             true,
		DisableFlagParsing: true,
		RunE: func(_ *cobra.Command, args []string) error {
			if !inSandbox() {
				gpg, err := exec.LookPath("gpg")
				if err != nil {
					return err
				}
				return syscall.Exec(gpg, append([]string{"gpg"}, args...), os.Environ()) //nolint:gosec // gpg with git's own arguments
			}
			if _, ok := gpgSignKey(args); !ok {
				return refused("the agent sandbox signs only (git's %s <key>), not gpg %s", strings.Join(gpgSignFlags, " "), strings.Join(args, " "))
			}
			payload, err := io.ReadAll(io.LimitReader(os.Stdin, signMax+1))
			if err != nil {
				return err
			}
			if len(payload) > signMax {
				return refused("a payload over %d bytes is not signed in the agent sandbox", signMax)
			}
			return a.brokeredAnswer(sandbox.Request{Op: sandbox.OpSign, Args: args, Input: payload}, signTimeout, false)
		},
	}
}

// signMax bounds a payload to sign: it rides base64-encoded in a request.
const signMax = 512 << 10

// brokeredSign signs a sandboxed session's payload with gpg on the host:
// git's signing call only, its output and exit code answered.
func brokeredSign(gpg string) sandbox.Handler {
	return func(ctx context.Context, _ int, req sandbox.Request) (sandbox.Reply, error) {
		key, ok := gpgSignKey(req.Args)
		if !ok {
			return sandbox.Reply{}, errors.New("the sandbox broker signs only: " + strings.Join(gpgSignFlags, " ") + " <key>")
		}
		ctx, cancel := context.WithTimeout(ctx, signTimeout)
		defer cancel()
		c := exec.CommandContext(ctx, gpg, append(append([]string{}, gpgSignFlags...), key)...) //nolint:gosec // gpg, the key checked
		c.Stdin = bytes.NewReader(req.Input)
		var out, errOut bytes.Buffer
		c.Stdout, c.Stderr = &out, &errOut
		err := c.Run()
		var exit *exec.ExitError
		if err != nil && !errors.As(err, &exit) {
			return sandbox.Reply{}, fmt.Errorf("gpg: %w", err)
		}
		return sandbox.Reply{Out: out.String(), Err: errOut.String(), Code: c.ProcessState.ExitCode()}, nil
	}
}

// hostGPG is the gpg git runs on the host: the person's gpg.program, gpg
// when none is set, as git does.
func hostGPG(ctx context.Context) string {
	out, err := exec.CommandContext(ctx, "git", "config", "--global", "--get", "gpg.program").Output()
	if p := strings.TrimSpace(string(out)); err == nil && p != "" {
		return p
	}
	return "gpg"
}
