package cmd

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/guard"
	"github.com/giantswarm/beekeeper/internal/secret"
	"github.com/giantswarm/beekeeper/internal/state"
)

// secretRun is the runner of sops and op; tests replace it.
var secretRun secret.Runner = secret.Exec

func (a *app) secretCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "secret",
		Short: "Credential operations that never return a value: compare, fingerprint, copy, set",
		Long: `beekeeper is the only process that reads, creates, encrypts and decrypts
secrets; an agent asks it with beekeeper secret. sops and op run in
beekeeper's process, a value stays in its memory for the one operation and
is written nowhere in plaintext, and what an operation answers is key
names, lengths, equality and keyed fingerprints, never a value. Every call
is logged (beekeeper log) with the session, the references and the
operation.

A reference is a SOPS file (every value in it), one value in a SOPS file
(file#a.b.c, the dotted key path; sops:// in front optional) or a field of
the shared 1Password vault (op://<vault>/<item>/<field>, the vault being
secret.vault). A SOPS file is encrypted under the creation rules of the
.sops.yaml nearest above it.`,
		Args: cobra.NoArgs,
	}
	c.AddCommand(&cobra.Command{
		Use:   "compare <a> <b>",
		Short: "Whether two values, or two SOPS files key by key, are equal",
		Long: `compare answers equal or different for two values, and for two SOPS files
each key's state: equal, different, only in a, only in b. It exits 1 when
anything is not equal.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := parseRefs(args...)
			if err != nil {
				return err
			}
			ra, rb := r[0], r[1]
			ops, err := a.secretOps()
			if err != nil {
				return err
			}
			vs, err := ops.Compare(cmd.Context(), ra, rb)
			a.secretLog("compare", "%s %s: %s", ra, rb, outcome(err, fmt.Sprintf("%d keys", len(vs))))
			if err != nil {
				return err
			}
			differ := false
			var b strings.Builder
			for _, v := range vs {
				differ = differ || v.State != secret.Equal
				fmt.Fprintf(&b, "%-60s %s\n", v.Key, v.State)
			}
			if err := a.secretPrint(vs, b.String()); err != nil {
				return err
			}
			if differ {
				return &exitError{code: ExitError}
			}
			return nil
		},
	})
	c.AddCommand(&cobra.Command{
		Use:   "fingerprint <ref>",
		Short: "The keyed fingerprint of a value, or of each value of a SOPS file",
		Long: `fingerprint answers an HMAC-SHA256 of each value under beekeeper's own key
(the value scanner's, scan/key in the state directory), cut to 16 hex
digits: two fingerprints are equal when the values are, and only beekeeper
can make one.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := parseRefs(args[0])
			if err != nil {
				return err
			}
			ops, err := a.secretOpsKeyed()
			if err != nil {
				return err
			}
			ps, err := ops.Fingerprints(cmd.Context(), r[0])
			a.secretLog("fingerprint", "%s: %s", r[0], outcome(err, fmt.Sprintf("%d keys", len(ps))))
			if err != nil {
				return err
			}
			var b strings.Builder
			for _, p := range ps {
				fmt.Fprintf(&b, "%-60s %s\n", p.Key, p.Fingerprint)
			}
			return a.secretPrint(ps, b.String())
		},
	})
	c.AddCommand(a.secretCopyCmd(), a.secretSetCmd())
	return c
}

func (a *app) secretCopyCmd() *cobra.Command {
	var name, namespace string
	c := &cobra.Command{
		Use:   "copy <from> <to> | copy <from> -- <consumer…>",
		Short: "Copy a SOPS file, or one value into a SOPS path or a consumer's stdin",
		Long: `copy <src.sops.yaml> <dst.sops.yaml> writes a new SOPS file with the
values of src, encrypted under dst's creation rules; --name and --namespace
rewrite a Kubernetes object's metadata.name and metadata.namespace on the
way. It answers the key names and value lengths (a Secret's data decoded).
dst must not exist.

copy <ref> <file#path> puts one value into a SOPS path, creating the file
or the key when absent, the file's other values kept.

copy <ref> -- <command…> runs a consumer with the value on stdin: gh secret
set, a command with --password-stdin, or one with --secret <name>=-. It
answers the consumer's output with the value redacted, and its exit code.`,
		Args: func(cmd *cobra.Command, args []string) error {
			if dash := cmd.ArgsLenAtDash(); dash >= 0 {
				if dash != 1 || len(args) < 2 {
					return fmt.Errorf("copy <from> -- <consumer…>")
				}
				return nil
			}
			return cobra.ExactArgs(2)(cmd, args)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			src, err := parseRefs(args[0])
			if err != nil {
				return err
			}
			ops, err := a.secretOps()
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			if cmd.ArgsLenAtDash() == 1 {
				if name != "" || namespace != "" {
					return usageErr("--name and --namespace rewrite a copied file, not a consumer's value")
				}
				argv := args[1:]
				code, out, err := ops.CopyToConsumer(ctx, src[0], argv)
				a.secretLog("copy", "%s to %s: %s", src[0], argv[0], outcome(err, fmt.Sprintf("exit %d", code)))
				if err != nil {
					return refused("%v", err)
				}
				if _, err := io.WriteString(a.out, out); err != nil {
					return err
				}
				if code != 0 {
					return &exitError{code: code}
				}
				return nil
			}
			dst, err := parseRefs(args[1])
			if err != nil {
				return err
			}
			if dst[0].Path != "" || dst[0].Op != "" {
				if name != "" || namespace != "" {
					return usageErr("--name and --namespace rewrite a copied file, not one value")
				}
				n, err := ops.CopyValue(ctx, src[0], dst[0])
				a.secretLog("copy", "%s to %s: %s", src[0], dst[0], outcome(err, fmt.Sprintf("%d bytes", n)))
				if err != nil {
					return err
				}
				return a.secretPrint(secret.Key{Name: dst[0].String(), Bytes: n}, fmt.Sprintf("wrote %s: %d bytes\n", dst[0], n))
			}
			keys, err := ops.CopyFile(ctx, src[0], dst[0].File, name, namespace)
			a.secretLog("copy", "%s to %s: %s", src[0], dst[0], outcome(err, fmt.Sprintf("%d keys", len(keys))))
			if err != nil {
				return err
			}
			var b strings.Builder
			fmt.Fprintf(&b, "wrote %s: %d keys\n", dst[0].File, len(keys))
			for _, k := range keys {
				fmt.Fprintf(&b, "  %-58s %d bytes\n", k.Name, k.Bytes)
			}
			return a.secretPrint(keys, b.String())
		},
	}
	c.Flags().StringVar(&name, "name", "", "the copy's metadata.name")
	c.Flags().StringVar(&namespace, "namespace", "", "the copy's metadata.namespace")
	return c
}

func (a *app) secretSetCmd() *cobra.Command {
	var vault, charset string
	var length int
	var generate bool
	c := &cobra.Command{
		Use:   "set <sops-file> <path> --generate --vault op://<vault>/<item>/<field>",
		Short: "Generate a value into the shared vault, then into a SOPS path",
		Long: `set --generate draws a new value, writes it to the shared vault's field
first (creating the item or the field when absent) and then into the SOPS
file's dotted path (creating the file or the key when absent, encrypted to
the recipients of its .sops.yaml), and answers its fingerprint.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !generate {
				return usageErr("set takes no value: --generate makes one")
			}
			v, err := parseRefs(vault)
			if err != nil {
				return usageErr("--vault: %v", err)
			}
			dst := secret.Ref{File: args[0], Path: args[1]}
			ops, err := a.secretOpsKeyed()
			if err != nil {
				return err
			}
			fp, err := ops.Set(cmd.Context(), dst, v[0], length, charset)
			a.secretLog("set", "%s and %s: %s", v[0], dst, outcome(err, "generated "+fp))
			if err != nil {
				return err
			}
			return a.secretPrint(secret.Print{Key: dst.String(), Fingerprint: fp},
				fmt.Sprintf("wrote %s and %s: %d characters, %s\n", v[0], dst, length, fp))
		},
	}
	f := c.Flags()
	f.BoolVar(&generate, "generate", false, "generate the value")
	f.StringVar(&vault, "vault", "", "the shared vault's field that holds the value first (op://<vault>/<item>/<field>)")
	f.IntVar(&length, "length", 32, "the value's length")
	f.StringVar(&charset, "charset", "alnum", "the characters: "+strings.Join(secret.Charsets(), ", "))
	return c
}

func parseRefs(args ...string) ([]secret.Ref, error) {
	out := make([]secret.Ref, len(args))
	for i, s := range args {
		r, err := secret.ParseRef(s)
		if err != nil {
			return nil, usageErr("%v", err)
		}
		out[i] = r
	}
	return out, nil
}

// secretOps are the operations with the service account's token, read
// from secret.tokenFile when the shared vault is configured.
func (a *app) secretOps() (*secret.Ops, error) {
	ops := &secret.Ops{Run: secretRun, Vault: a.cfg.Secret.Vault}
	if ops.Vault == "" || a.cfg.Secret.TokenFile == "" {
		return ops, nil
	}
	raw, err := os.ReadFile(a.cfg.Secret.TokenFile)
	if err != nil {
		return nil, fmt.Errorf("secret.tokenFile: %w", err)
	}
	ops.Token = strings.TrimSpace(string(raw))
	return ops, nil
}

// secretOpsKeyed are the operations with the fingerprint key, created on
// first use.
func (a *app) secretOpsKeyed() (*secret.Ops, error) {
	ix, err := guard.OpenIndex(a.scanDir())
	if err != nil {
		return nil, err
	}
	ops, err := a.secretOps()
	if err != nil {
		return nil, err
	}
	ops.Fingerprint = func(v string) string { return "hmac:" + ix.Fingerprint(v)[:16] }
	return ops, nil
}

// secretPrint prints text, or v with --json.
func (a *app) secretPrint(v any, text string) error {
	if a.json {
		return a.printJSON(v)
	}
	_, err := io.WriteString(a.out, text)
	return err
}

// secretLog records one operation with the calling session; the log never
// holds a value, only references and the outcome.
func (a *app) secretLog(op, format string, args ...any) {
	who, err := a.caller()
	if err != nil {
		who = state.Party{Name: noSession}
	}
	if err := a.store.Log(event(who, "secret."+op, format, args...)); err != nil {
		fmt.Fprintln(os.Stderr, guard.LogPrefix+"secret: the call is not logged: "+err.Error())
	}
}

// outcome is ok, or the error that ended an operation.
func outcome(err error, ok string) string {
	if err != nil {
		return "failed: " + err.Error()
	}
	return ok
}
