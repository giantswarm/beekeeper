package cmd

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/guard"
	"github.com/giantswarm/beekeeper/internal/secret"
)

// terminalIn is where store reads the person's Enter; tests replace it.
var terminalIn io.Reader = os.Stdin

// The GitHub App shape: the fields an App's credentials go into and the
// download its private key arrives as.
const (
	githubClientSecret = "client-secret"
	githubPrivateKey   = "private-key"
	githubKeyGlob      = "*.private-key.pem"
)

// intake is one value on its way into a vault field: from a file, from the
// newest download a glob matches, or from the clipboard, after the click
// that makes it.
type intake struct {
	ref secret.Ref
	// file holds the value; "" looks for the glob's newest download, or
	// reads the clipboard without a glob.
	file string
	glob string
	// click is what the person does first, said while the value is missing.
	click string
	// keep leaves the file after the store.
	keep bool
}

func (a *app) secretStoreCmd() *cobra.Command {
	var fromFile, githubApp string
	var keepFile bool
	c := &cobra.Command{
		Use:   "store op://<vault>/<item>/<field> [--from-file <path>] [--keep-file] | store --github-app <item> [--from-file <key.pem>] [--keep-file]",
		Short: "The person's: a value from the clipboard or a file into a field of the shared vault, never on a command line",
		Long: `store is the person's, in their own terminal: a value a web page showed
once, or a file it downloaded, goes into a field of the shared vault. The
value is read from the Wayland clipboard (wl-paste), cleared afterwards, or
from the file --from-file names, shredded afterwards unless --keep-file;
the item is created when the vault lacks it. The value travels in
beekeeper's memory alone, with secret.session to the broker over the
keeper's socket and from there to op's stdin: never on a command line, in
a spool or state file, or in beekeeper log, which records the reference,
the length and the keyed fingerprint. store prints those two.

--github-app <item> stores a GitHub App's credentials in one call: the
client secret from the clipboard into the item's client-secret field, the
private key from the newest *.private-key.pem in ~/Downloads (or
--from-file) into private-key, the file shredded afterwards. It names the
click that makes each (Generate a new client secret, Generate a private
key) and waits for Enter while the clipboard is empty or the download
absent.

While the broker holds no vault session the call says so and stops (exit
78) before it asks for a click. It refuses in an agent session and without
a terminal, and the hook refuses it in agent sessions too.`,
		Args: func(cmd *cobra.Command, args []string) error {
			if githubApp != "" {
				return cobra.NoArgs(cmd, args)
			}
			return cobra.ExactArgs(1)(cmd, args)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := personOnly("beekeeper secret store"); err != nil {
				return err
			}
			if keepFile && fromFile == "" && githubApp == "" {
				return usageErr("--keep-file keeps the file --from-file names")
			}
			plan, err := a.storePlan(args, githubApp, fromFile, keepFile)
			if err != nil {
				return err
			}
			if err := a.vaultReady(plan); err != nil {
				return err
			}
			enter := bufio.NewReader(terminalIn)
			var stored []secret.Stored
			for _, in := range plan {
				s, err := a.storeOne(cmd.Context(), cmd.ErrOrStderr(), enter, in)
				if err != nil {
					return err
				}
				stored = append(stored, s)
			}
			if a.json {
				return a.printJSON(stored)
			}
			return nil
		},
	}
	f := c.Flags()
	f.StringVar(&fromFile, "from-file", "", "the file holding the value (a downloaded private key), shredded after the store")
	f.BoolVar(&keepFile, "keep-file", false, "keep the file --from-file names, or the downloaded key, after the store")
	f.StringVar(&githubApp, "github-app", "", "a GitHub App's credentials into this item of the shared vault: the client secret from the clipboard into client-secret, the private key from the newest *.private-key.pem in ~/Downloads into private-key")
	return c
}

// storePlan is what a store call takes in: one field from the clipboard or
// a file, or a GitHub App's two.
func (a *app) storePlan(args []string, githubApp, fromFile string, keep bool) ([]intake, error) {
	if githubApp == "" {
		r, err := parseRefs(args[0])
		if err != nil {
			return nil, err
		}
		if r[0].Op == "" {
			return nil, usageErr("%s: store writes a field of the shared vault, op://<vault>/<item>/<field>", args[0])
		}
		return []intake{{ref: r[0], file: fromFile, keep: keep, click: "copy the value"}}, nil
	}
	if strings.Contains(githubApp, "/") {
		return nil, usageErr("--github-app takes the item's name in the shared vault, not %q", githubApp)
	}
	if a.cfg.Secret.Vault == "" {
		return nil, &exitError{code: ExitVault, msg: "no shared vault is configured (secret.vault): --github-app writes its item there"}
	}
	item := guard.OpRef + a.cfg.Secret.Vault + "/" + githubApp + "/"
	return []intake{
		{ref: secret.Ref{Op: item + githubClientSecret},
			click: `on the App's settings page click "Generate a new client secret" and copy it`},
		{ref: secret.Ref{Op: item + githubPrivateKey}, file: fromFile, glob: githubKeyGlob, keep: keep,
			click: `click "Generate a private key": the browser downloads <app>.<date>.private-key.pem to ~/Downloads`},
	}, nil
}

// vaultReady refuses a store before any click when nothing could take the
// value: a field outside the shared vault, no way into it, or with
// secret.session a broker holding no session (exit 78).
func (a *app) vaultReady(plan []intake) error {
	ops, err := a.secretOps()
	if err != nil {
		return err
	}
	for _, in := range plan {
		if err := ops.Writable(in.ref); err != nil {
			return vaultExit(err)
		}
	}
	if !a.cfg.Secret.Session {
		return nil
	}
	path, err := secret.SocketPath()
	if err != nil {
		return err
	}
	st, err := secret.AskVault(path, secret.VaultRequest{Op: secret.VaultStatus})
	if err != nil {
		return refused("%v", err)
	}
	if !st.Unlocked {
		return &exitError{code: ExitVault, msg: "vault locked: the broker holds no session (beekeeper secret status says once it does); nothing stored"}
	}
	return nil
}

// storeOne takes one value in, stores it, says what was written and
// leaves the value nowhere but the vault.
func (a *app) storeOne(ctx context.Context, say io.Writer, enter *bufio.Reader, in intake) (secret.Stored, error) {
	v, from, err := a.takeIn(ctx, say, enter, &in)
	if err != nil {
		return secret.Stored{}, a.secretLog(err, "store", "%s: %s", in.ref, outcome(err, ""))
	}
	s, err := a.storeValue(ctx, in.ref, v)
	if err := a.secretLog(err, "store", "%s from %s: %s", in.ref, from, outcome(err, fmt.Sprintf("%d bytes, %s", s.Bytes, s.Fingerprint))); err != nil {
		return secret.Stored{}, err
	}
	if err := a.storeSay("wrote %s: %d bytes, %s", s.Ref, s.Bytes, s.Fingerprint); err != nil {
		return s, err
	}
	return s, a.cleanUp(ctx, in)
}

// takeIn reads the value in from where in says it is, waiting for the
// person's Enter after the click while there is nothing yet. It answers the
// value and where it came from.
func (a *app) takeIn(ctx context.Context, say io.Writer, enter *bufio.Reader, in *intake) (string, string, error) {
	for {
		named := in.file != ""
		v, from, missing, err := a.lookIn(ctx, in)
		if err != nil {
			return "", "", err
		}
		if in.file != "" && !named {
			fi, err := os.Stat(in.file)
			if err != nil {
				return "", "", err
			}
			if _, err := fmt.Fprintf(say, "taking %s (modified %s)\n", in.file, fi.ModTime().Format(time.DateTime)); err != nil {
				return "", "", err
			}
		}
		if v != "" {
			return v, from, nil
		}
		if _, err := fmt.Fprintf(say, "%s: %s, then press Enter\n", missing, in.click); err != nil {
			return "", "", err
		}
		if _, err := enter.ReadString('\n'); err != nil {
			return "", "", refused("%s: nothing stored", missing)
		}
	}
}

// lookIn is the value where in says it is, and where that is, or what is
// missing there yet: the newest download in.glob matches becomes in.file.
func (a *app) lookIn(ctx context.Context, in *intake) (value, from, missing string, err error) {
	if in.file == "" && in.glob != "" {
		dir, err := downloadsDir()
		if err != nil {
			return "", "", "", err
		}
		f, err := secret.NewestFile(dir, in.glob)
		if err != nil {
			return "", "", "", err
		}
		if f == "" {
			return "", "", "no " + filepath.Join(dir, in.glob), nil
		}
		in.file = f
	}
	if in.file != "" {
		v, err := secret.ReadValueFile(in.file)
		return v, in.file, "", err
	}
	v, err := secret.Clipboard(ctx, secretRun)
	if err != nil || v == "" {
		return "", "", "the clipboard is empty", err
	}
	return v, "the clipboard", "", nil
}

// storeValue writes v into ref: with secret.session through the broker's
// session over the keeper's socket, else as the service account in this
// process.
func (a *app) storeValue(ctx context.Context, ref secret.Ref, v string) (secret.Stored, error) {
	if !a.cfg.Secret.Session {
		ops, err := a.secretOpsKeyed()
		if err != nil {
			return secret.Stored{}, err
		}
		s, err := ops.StoreValue(ctx, ref, v)
		return s, vaultExit(err)
	}
	path, err := secret.SocketPath()
	if err != nil {
		return secret.Stored{}, err
	}
	st, err := secret.AskVault(path, secret.VaultRequest{Op: secret.VaultStore, Ref: ref.Op, Value: v})
	if err != nil {
		return secret.Stored{}, &exitError{code: ExitVault, msg: err.Error()}
	}
	if st.Stored == nil {
		return secret.Stored{}, &exitError{code: ExitVault, msg: "the vault keeper answered no store"}
	}
	return *st.Stored, nil
}

// cleanUp leaves the value nowhere but the vault: the clipboard cleared,
// the file shredded unless kept.
func (a *app) cleanUp(ctx context.Context, in intake) error {
	if in.file == "" {
		if err := secret.ClearClipboard(ctx, secretRun); err != nil {
			return err
		}
		return a.storeSay("cleared the clipboard")
	}
	if in.keep {
		return a.storeSay("kept %s", in.file)
	}
	if err := secret.Shred(in.file); err != nil {
		return err
	}
	return a.storeSay("shredded %s", in.file)
}

// storeSay prints one line of a store's answer, none with --json.
func (a *app) storeSay(format string, args ...any) error {
	if a.json {
		return nil
	}
	_, err := fmt.Fprintf(a.out, format+"\n", args...)
	return err
}

// downloadsDir is where the browser puts a download: ~/Downloads.
func downloadsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Downloads"), nil
}
