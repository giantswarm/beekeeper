package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/giantswarm/beekeeper/internal/guard"
	"github.com/giantswarm/beekeeper/internal/sandbox"
	"github.com/giantswarm/beekeeper/internal/secret"
	"github.com/giantswarm/beekeeper/internal/state"
)

// secretRun is the runner of sops and op; tests replace it.
var secretRun secret.Runner = secret.Exec

// secretApply writes a lab Secret's key; nil is the real cluster's, tests
// replace it.
var secretApply secret.SecretApplier

// secretRead reads a lab Secret's key; nil is the real cluster's, tests
// replace it.
var secretRead secret.SecretReader

func (a *app) secretCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "secret",
		Short: "Credential operations that never return a value: compare, fingerprint, copy, set, rotate",
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
.sops.yaml nearest above it.

A SOPS file encrypted to age recipients decrypts with an identity from
sops' own sources (SOPS_AGE_KEY, SOPS_AGE_KEY_FILE, sops/age/keys.txt in
the user's config directory), from secret.ageIdentities (entries that map
a recipient or a pathRegex to the op:// field of the shared vault or of
secret.ageVaults, the file:// identity file or the store:// entry of the
person's own credential store holding its identity; store:// alone
searches the store for the file's recipients) or, for a recipient no entry
names, from a vault item: the shared vault's item per recipient, titled
"sops age key <recipient>", its password field the AGE-SECRET-KEY-1…
identity, and for a file of installations/<name>/ the item "<name>.agekey"
of the shared vault or of secret.ageVaults, one of its fields or its
document the identity file. Each is read in beekeeper's process for the
one sops call, and only the identity of the file's recipient is used. A
file none of them has an identity for fails before sops runs, in one line
naming the installation, the recipient and the items the vaults lack;
recipients shows, for a gitops repository's path, each
creation rule's recipient and where its identity is, no value read.`,
		Args: cobra.NoArgs,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			if a.cfg == nil {
				if err := a.load(); err != nil {
					return err
				}
			}
			warnIncomplete(cmd.ErrOrStderr(), a.cfg)
			return nil
		},
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
			if err := a.sandboxFiles(r, nil); err != nil {
				return err
			}
			ra, rb := r[0], r[1]
			ops, err := a.secretOps()
			if err != nil {
				return err
			}
			vs, err := ops.Compare(cmd.Context(), ra, rb)
			if err := a.secretLog(err, "compare", "%s %s: %s", ra, rb, outcome(err, fmt.Sprintf("%d keys", len(vs)))); err != nil {
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
	c.AddCommand(a.secretFingerprintCmd(), a.secretCopyCmd(), a.secretSetCmd(), a.secretRotateCmd(), a.secretSetupCmd(), a.secretImportCmd(), a.secretRecipientsCmd())
	for _, sub := range c.Commands() {
		run := sub.RunE
		sub.RunE = func(cmd *cobra.Command, args []string) error {
			if os.Getenv(sandbox.Brokered) != "" {
				// the broker's call: the vault session in its environment
				// stays out of every other process's reach
				_ = secret.Protect()
				return vaultExit(run(cmd, args))
			}
			inSandbox := os.Getenv(sandbox.Env) != ""
			if inSandbox || a.cfg.Secret.Session && a.secretNeedsBroker(append([]string{cmd.Name()}, callArgs(cmd, args)...)) {
				return a.secretBrokered(cmd, args, inSandbox)
			}
			return vaultExit(run(cmd, args))
		}
	}
	c.AddCommand(a.secretUnlockCmd(), a.secretLockCmd(), a.secretStatusCmd())
	return c
}

func (a *app) secretFingerprintCmd() *cobra.Command {
	var encode, fromSecret string
	c := &cobra.Command{
		Use:   "fingerprint <ref> [--encode <encoding>] | fingerprint --secret <context>/<namespace>/<name>/<key>",
		Short: "The keyed fingerprint of a value, of each value of a SOPS file, or of a key of a lab's Secret",
		Long: `fingerprint answers an HMAC-SHA256 of each value under beekeeper's own key
(the value scanner's, scan/key in the state directory), cut to 16 hex
digits: two fingerprints are equal when the values are, and only beekeeper
can make one.

--encode <encoding> answers the fingerprint of one value in that encoding,
the form copy --encode writes; --secret <context>/<namespace>/<name>/<key>
the fingerprint of a key of a Secret in a kind lab whose lease the caller
holds. The two together check a delivery without reading a value:

  beekeeper secret fingerprint op://<vault>/<item>/<field> --encode basic:<user>
  beekeeper secret fingerprint --secret kind-<lab>/<namespace>/<name>/<key>`,
		Args: func(cmd *cobra.Command, args []string) error {
			if cmd.Flags().Changed("secret") {
				return cobra.NoArgs(cmd, args)
			}
			return cobra.ExactArgs(1)(cmd, args)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			enc, err := secret.ParseEncoding(encode)
			if err != nil {
				return usageErr("%v", err)
			}
			if cmd.Flags().Changed("secret") {
				if !enc.IsZero() {
					return usageErr("--encode encodes a source's value; a Secret's key is fingerprinted as it is")
				}
				return a.secretFingerprintSecret(cmd.Context(), fromSecret)
			}
			r, err := parseRefs(args[0])
			if err != nil {
				return err
			}
			if !enc.IsZero() && !r[0].Single() {
				return usageErr("--encode encodes one value: file#path or op://…, not a whole file")
			}
			if err := a.sandboxFiles(r, nil); err != nil {
				return err
			}
			ops, err := a.secretOpsKeyed()
			if err != nil {
				return err
			}
			ops.Encode = enc
			ps, err := ops.Fingerprints(cmd.Context(), r[0])
			if err := a.secretLog(err, "fingerprint", "%s%s: %s", r[0], encodedAs(enc), outcome(err, fmt.Sprintf("%d keys", len(ps)))); err != nil {
				return err
			}
			return a.secretPrintPrints(ps)
		},
	}
	encodeFlag(c, &encode)
	c.Flags().StringVar(&fromSecret, "secret", "", "a key of a Secret in a lab you hold: <context>/<namespace>/<name>/<key>")
	return c
}

// secretFingerprintSecret is fingerprint --secret: a key of a Secret in a
// lab whose lease the caller holds.
func (a *app) secretFingerprintSecret(ctx context.Context, spec string) error {
	t, err := secret.ParseKubeTarget(spec)
	if err != nil {
		return usageErr("--secret: %v", err)
	}
	if err := a.checkLabHeld(t); err != nil {
		return a.secretLog(err, "fingerprint", "%s: %s", t, outcome(err, ""))
	}
	ops, err := a.secretOpsKeyed()
	if err != nil {
		return err
	}
	p, err := ops.SecretFingerprint(ctx, t)
	if err := a.secretLog(err, "fingerprint", "%s: %s", t, outcome(err, "1 key")); err != nil {
		return err
	}
	return a.secretPrintPrints([]secret.Print{p})
}

// secretPrintPrints prints fingerprints, one key per line.
func (a *app) secretPrintPrints(ps []secret.Print) error {
	var b strings.Builder
	for _, p := range ps {
		fmt.Fprintf(&b, "%-60s %s\n", p.Key, p.Fingerprint)
	}
	return a.secretPrint(ps, b.String())
}

// encodeFlag is the flag that encodes a value before it is written.
func encodeFlag(c *cobra.Command, encode *string) {
	c.Flags().StringVar(encode, "encode", "", "encode the value inside beekeeper before it is written: base64, or basic:<user> for base64(<user>:<value>)")
}

// encodedAs is the log's note of an encoding, "" for none.
func encodedAs(e secret.Encoding) string {
	if e.IsZero() {
		return ""
	}
	return " encoded " + e.String()
}

// callArgs are a call's arguments and its flags' values.
func callArgs(cmd *cobra.Command, args []string) []string {
	all := slices.Clone(args)
	cmd.Flags().Visit(func(f *pflag.Flag) { all = append(all, f.Value.String()) })
	return all
}

// secretBrokered is a secret call the host's broker runs as this session,
// answering its output and exit code through the spool: every call in the
// agent sandbox, which holds no sops key and no op session, and with
// secret.session every call on the vault, whose session lives in the broker
// alone. While the broker holds no session the call says so and waits for
// the person's unlock.
func (a *app) secretBrokered(cmd *cobra.Command, args []string, inSandbox bool) error {
	dash := cmd.ArgsLenAtDash()
	if inSandbox && dash >= 0 {
		return refused("the agent sandbox runs no consumer on the host: copy or set with -- <consumer> runs outside the sandbox only")
	}
	if dash < 0 {
		dash = len(args)
	}
	argv, err := brokerFlags(cmd)
	if err != nil {
		return err
	}
	for _, arg := range args[:dash] {
		if strings.HasPrefix(arg, "-") {
			return usageErr("%s: an argument the broker would take for a flag", arg)
		}
	}
	argv = append(argv, args[:dash]...)
	if dash < len(args) {
		argv = append(append(argv, "--"), args[dash:]...)
	}
	if err := brokeredSecretArgs(argv, inSandbox, a.cfg.Secret.Session); err != nil {
		return refused("%v; setup runs on the host, by the person", err)
	}
	if !a.cfg.Secret.Session || !a.secretNeedsVault("", argv) {
		return a.brokeredReply(sandbox.Request{Op: sandbox.OpSecret, Args: argv})
	}
	r, err := sandbox.Call(sandbox.SpoolDir(a.cfg.StateDir), sandbox.Request{Op: sandbox.OpVault}, 10*time.Second)
	if err == nil && r.Out != vaultUnlocked {
		_, _ = fmt.Fprintln(os.Stderr, "beekeeper: "+secret.Locked)
	}
	return a.brokeredReplyWithin(sandbox.Request{Op: sandbox.OpSecret, Args: argv}, a.cfg.Secret.UnlockWait.Duration+brokeredCallTimeout+time.Minute)
}

// brokeredReply asks the host's broker for req and passes on its output
// and exit code.
func (a *app) brokeredReply(req sandbox.Request) error {
	return a.brokeredReplyWithin(req, brokeredCallTimeout+time.Minute)
}

// brokeredReplyWithin is brokeredReply waiting up to timeout.
func (a *app) brokeredReplyWithin(req sandbox.Request, timeout time.Duration) error {
	return a.brokeredAnswer(req, timeout, false)
}

// sandboxFiles holds the SOPS files of a sandboxed session's brokered call
// to the agent sandbox's lists: through the broker it reads and writes no
// file its sandbox closes to it.
func (a *app) sandboxFiles(read, write []secret.Ref) error {
	files := func(refs []secret.Ref) []string {
		var out []string
		for _, r := range refs {
			out = append(out, r.File)
		}
		return out
	}
	return a.sandboxPaths(files(read), files(write))
}

// vaultExit gives an error reading the shared vault its own exit code,
// ExitVault, so that a caller tells a locked vault from a refusal.
func vaultExit(err error) error {
	if errors.Is(err, secret.ErrVault) {
		return &exitError{code: ExitVault, msg: err.Error()}
	}
	return err
}

func (a *app) secretCopyCmd() *cobra.Command {
	var name, namespace, toSecret, encode string
	var in secret.Stdin
	c := &cobra.Command{
		Use:   "copy <from> <to> | copy <ref>=<path>… <new-file> [--name n --namespace ns] | copy <from> -- <consumer…> | copy <from> --to-secret <context>/<namespace>/<name>/<key>",
		Short: "Copy a SOPS file, values into a new SOPS file, or one value into a SOPS path, a consumer's stdin or a lab's Secret",
		Long: `copy <src.sops.yaml> <dst.sops.yaml> writes a new SOPS file with the
values of src, encrypted under dst's creation rules; --name and --namespace
rewrite a Kubernetes object's metadata.name and metadata.namespace on the
way. It answers the key names and value lengths (a Secret's data decoded).
dst must not exist.

copy <ref> <file#path> puts one value into a SOPS path, creating the file
or the key when absent, the file's other values kept.

copy <ref>=<path> [<ref>=<path>…] <new-file> writes several values, each
<ref> one value (op://<vault>/<item>/<field>, file#path), into a new SOPS
file in one encryption: sops needs only the recipients of the nearest
.sops.yaml and decrypts nothing, so no age identity of the new file is
needed. --name and --namespace start the file as that Secret, a bare path
going under stringData; a plaintext Secret skeleton is filled; an encrypted
file is refused. Every path is checked against the creation rule before a
value is read. It answers the key names and value lengths, for example:

  beekeeper secret copy op://<vault>/<item>/username=client-id \
    op://<vault>/<item>/credential=client-secret app.sops.yaml \
    --name app --namespace team

copy <ref> -- <command…> runs a consumer with the value on stdin: gh secret
set, garage json-api <endpoint> -, a command with --password-stdin or one
with --secret <name>=-, or kubectl exec -i --context <context> <pod> --
<one of them> for a command in a pod (no TTY, no -v, never a production
context). --stdin-json '<object>' --stdin-field <key> hands the consumer
that JSON object with the value at <key> instead of the bare value (garage
json-api ImportKey's request, for one). It answers the consumer's output
with the value redacted, and its exit code.

copy <ref> --to-secret <context>/<namespace>/<name>/<key> writes one value
into a key of a Secret in a kind lab, kind-<cluster>, whose lab lease the
caller holds: a patch of that one key that creates the Secret when absent
and keeps its other keys. kind's admin kubeconfig stays in beekeeper's memory
like the value; it answers the value's length. A context of a lab the
caller holds no lease for is refused.

--encode base64 or --encode basic:<user> writes one value's encoded form
instead of the value, made in beekeeper's process: base64(<user>:<value>)
for basic, the credential a gateway injects verbatim after "Basic ". It
takes one value (copy <ref> <file#path>, -- <consumer…>, --to-secret), the
answer is the encoded form's length, and fingerprint <ref> --encode answers
the fingerprint a delivery is checked against:

  beekeeper secret copy op://<vault>/<item>/<field> --encode basic:x-access-token \
    --to-secret kind-<lab>/<namespace>/<name>/<key>

An op:// value the shared vault cannot give (none configured, no token, op
failing or answering nothing within a minute) exits 78.`,
		Args: func(cmd *cobra.Command, args []string) error {
			if cmd.Flags().Changed("to-secret") {
				if cmd.ArgsLenAtDash() >= 0 {
					return fmt.Errorf("copy <from> --to-secret <context>/<namespace>/<name>/<key> takes no consumer")
				}
				return cobra.ExactArgs(1)(cmd, args)
			}
			if dash := cmd.ArgsLenAtDash(); dash >= 0 {
				if dash != 1 || len(args) < 2 {
					return fmt.Errorf("copy <from> -- <consumer…>")
				}
				return nil
			}
			if len(args) > 2 {
				if _, err := copyPairs(args); err != nil {
					return err
				}
				return nil
			}
			return cobra.ExactArgs(2)(cmd, args)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if in != (secret.Stdin{}) && cmd.ArgsLenAtDash() != 1 {
				return usageErr("--stdin-json and --stdin-field shape a consumer's stdin: copy <from> -- <consumer…>")
			}
			enc, err := secret.ParseEncoding(encode)
			if err != nil {
				return usageErr("%v", err)
			}
			if cmd.ArgsLenAtDash() < 0 && !cmd.Flags().Changed("to-secret") {
				if pairs, err := copyPairs(args); err == nil {
					if !enc.IsZero() {
						return usageErr("--encode encodes one value: copy <ref> <file#path>, copy <ref> -- <consumer…> or copy <ref> --to-secret …")
					}
					return a.secretCopyValues(cmd.Context(), pairs, args[len(args)-1], name, namespace)
				}
			}
			src, err := parseRefs(args[0])
			if err != nil {
				return err
			}
			if !enc.IsZero() && !src[0].Single() {
				return usageErr("--encode encodes one value: file#path or op://…, not a whole file")
			}
			if err := a.sandboxFiles(src, nil); err != nil {
				return err
			}
			ops, err := a.secretOps()
			if err != nil {
				return err
			}
			ops.Encode = enc
			ctx := cmd.Context()
			if cmd.Flags().Changed("to-secret") {
				if name != "" || namespace != "" {
					return usageErr("--name and --namespace rewrite a copied file, not a Secret's key")
				}
				return a.secretCopyToSecret(ctx, ops, src[0], toSecret)
			}
			if cmd.ArgsLenAtDash() == 1 {
				if name != "" || namespace != "" {
					return usageErr("--name and --namespace rewrite a copied file, not a consumer's value")
				}
				argv := args[1:]
				if err := a.checkConsumerContext(argv); err != nil {
					return err
				}
				code, out, err := ops.CopyToConsumer(ctx, src[0], argv, in)
				if err != nil && !errors.Is(err, secret.ErrVault) {
					// exit ExitVault stays: the broker signs in again and retries
					err = refused("%v", err)
				}
				if err := a.secretLog(err, "copy", "%s%s to %s: %s", src[0], encodedAs(enc), argv[0], outcome(err, fmt.Sprintf("exit %d", code))); err != nil {
					return err
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
			if err := a.sandboxFiles(nil, dst); err != nil {
				return err
			}
			if dst[0].Path != "" || dst[0].Op != "" {
				if name != "" || namespace != "" {
					return usageErr("--name and --namespace rewrite a copied file, not one value")
				}
				n, err := ops.CopyValue(ctx, src[0], dst[0])
				if err := a.secretLog(err, "copy", "%s%s to %s: %s", src[0], encodedAs(enc), dst[0], outcome(err, fmt.Sprintf("%d bytes", n))); err != nil {
					return err
				}
				return a.secretPrint(secret.Key{Name: dst[0].String(), Bytes: n}, fmt.Sprintf("wrote %s: %d bytes\n", dst[0], n))
			}
			keys, err := ops.CopyFile(ctx, src[0], dst[0].File, name, namespace)
			if err := a.secretLog(err, "copy", "%s to %s: %s", src[0], dst[0], outcome(err, fmt.Sprintf("%d keys", len(keys)))); err != nil {
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
	c.Flags().StringVar(&name, "name", "", "the copy's metadata.name; with <ref>=<path>… the new Secret's")
	c.Flags().StringVar(&namespace, "namespace", "", "the copy's metadata.namespace; with <ref>=<path>… the new Secret's")
	c.Flags().StringVar(&toSecret, "to-secret", "", "a key of a Secret in a lab you hold: <context>/<namespace>/<name>/<key>")
	stdinFlags(c, &in)
	encodeFlag(c, &encode)
	return c
}

// copyPairs are the <ref>=<path> pairs of copy into a new file: every
// argument but the last one, which names a whole file.
func copyPairs(args []string) ([]secret.Pair, error) {
	dst, err := secret.ParseRef(args[len(args)-1])
	if err != nil {
		return nil, usageErr("%v", err)
	}
	if dst.Op != "" || dst.Path != "" {
		return nil, usageErr("%s: copy <ref>=<path>… <file> writes a whole new file, no path or op:// in it", dst)
	}
	pairs := make([]secret.Pair, 0, len(args)-1)
	for _, s := range args[:len(args)-1] {
		p, err := secret.ParsePair(s)
		if err != nil {
			return nil, usageErr("copy <ref>=<path>… <file>: %v", err)
		}
		pairs = append(pairs, p)
	}
	return pairs, nil
}

// secretCopyValues is copy <ref>=<path>… <file>: several values into a new
// SOPS file in one encryption.
func (a *app) secretCopyValues(ctx context.Context, pairs []secret.Pair, file, name, namespace string) error {
	var nw *secret.NewSecret
	if name != "" || namespace != "" {
		if name == "" || namespace == "" {
			return usageErr("--name and --namespace start a new Secret together")
		}
		nw = &secret.NewSecret{Name: name, Namespace: namespace}
	}
	src := make([]secret.Ref, len(pairs))
	from := make([]string, len(pairs))
	for i, p := range pairs {
		src[i], from[i] = p.Src, p.Src.String()
	}
	if err := a.sandboxFiles(src, []secret.Ref{{File: file}}); err != nil {
		return err
	}
	ops, err := a.secretOps()
	if err != nil {
		return err
	}
	keys, err := ops.CopyValues(ctx, pairs, file, nw)
	if err := a.secretLog(err, "copy", "%s to %s: %s", strings.Join(from, " and "), file, outcome(err, fmt.Sprintf("%d keys", len(keys)))); err != nil {
		return err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "wrote %s: %d keys\n", file, len(keys))
	for _, k := range keys {
		fmt.Fprintf(&b, "  %-58s %d bytes\n", k.Name, k.Bytes)
	}
	return a.secretPrint(keys, b.String())
}

// stdinFlags are the flags that shape a consumer's stdin.
func stdinFlags(c *cobra.Command, in *secret.Stdin) {
	c.Flags().StringVar(&in.Template, "stdin-json", "", "the consumer reads this JSON object with the value at --stdin-field, not the bare value")
	c.Flags().StringVar(&in.Field, "stdin-field", "", "the key of --stdin-json the value goes to")
}

// checkConsumerContext refuses a kubectl exec consumer into production:
// agents never write to its clusters.
func (a *app) checkConsumerContext(argv []string) error {
	if ctx := secret.ConsumerContext(argv); guard.IsProduction(ctx, a.cfg.Kube.Production) {
		return refused("%s: agents never write to the production installation's clusters", ctx)
	}
	return nil
}

// secretCopyToSecret is copy --to-secret: one value into a key of a
// Secret in a lab whose lease the caller holds.
func (a *app) secretCopyToSecret(ctx context.Context, ops *secret.Ops, src secret.Ref, spec string) error {
	t, err := secret.ParseKubeTarget(spec)
	if err != nil {
		return usageErr("--to-secret: %v", err)
	}
	if err := a.checkLabHeld(t); err != nil {
		return a.secretLog(err, "copy", "%s to %s: %s", src, t, outcome(err, ""))
	}
	n, err := ops.CopyToSecret(ctx, src, t)
	if err := a.secretLog(err, "copy", "%s%s to %s: %s", src, encodedAs(ops.Encode), t, outcome(err, fmt.Sprintf("%d bytes", n))); err != nil {
		return err
	}
	return a.secretPrint(secret.Key{Name: t.String(), Bytes: n}, fmt.Sprintf("wrote %s: %d bytes\n", t, n))
}

// checkLabHeld refuses a Secret's context unless it is a kind lab's whose
// lab lease the caller holds.
func (a *app) checkLabHeld(t secret.KubeTarget) error {
	res := a.cfg.LabLease(t.KindCluster())
	if res == "" {
		labs := make([]string, 0, len(a.cfg.Labs))
		for _, res := range a.cfg.Resources {
			if cl := a.cfg.LabCluster(res); cl != "" {
				labs = append(labs, "kind-"+cl)
			}
		}
		return refused("%s: a Secret is written only into a lab's context (%s), held under its lease", t.Context, strings.Join(labs, ", "))
	}
	return a.holdsLease(res, t.Context)
}

func (a *app) secretSetCmd() *cobra.Command {
	var vault, charset, toSecret, name, namespace, encode string
	var length int
	var generate bool
	var in secret.Stdin
	c := &cobra.Command{
		Use:   "set <sops-file> <path> --generate [--name n --namespace ns] [--vault op://<vault>/<item>/<field>] [--to-secret <context>/<namespace>/<name>/<key> | -- <consumer…>]",
		Short: "Generate a value into a SOPS path, and the shared vault, a lab's Secret or a consumer",
		Long: `set --generate draws a new value in beekeeper's process and writes it into
the SOPS file's dotted path (creating the file or the key when absent,
encrypted to the recipients of its .sops.yaml), and answers its
fingerprint. Without --vault the SOPS file is the value's only home: no
vault holds a copy. A plaintext Kubernetes Secret without values (apiVersion,
kind, metadata, an empty stringData), a skeleton, becomes the SOPS file with
the value in it; --name and --namespace start an absent file as that Secret.
Any other plaintext file (a ConfigMap, a Secret holding a value, no YAML
mapping) is refused by what it is, before sops sees it.
A Secret's value goes under stringData unless the path names data or
stringData. A path the file's .sops.yaml creation rule would leave in
plaintext is refused before any value is drawn, naming the rule.

--vault op://<vault>/<item>/<field> writes the shared vault's field first
(creating the item or the field when absent), then the SOPS path.

--to-secret <context>/<namespace>/<name>/<key> also writes the value into
a key of a Secret in a kind lab whose lease the caller holds, as copy
--to-secret does; -- <consumer…> also runs a consumer with the value on
stdin, as copy <ref> -- <consumer…> does (kubectl exec -i into a pod and
--stdin-json included), and answers its output with the value redacted and
its exit code. Both come after the SOPS path is
written: when one fails, the SOPS path holds the value and copy finishes
the delivery.

--encode base64 or --encode basic:<user> writes the SOPS path, the Secret
and the consumer the value's encoded form (base64(<user>:<value>) for
basic, an HTTP Basic credential), made in beekeeper's process; the vault's
field keeps the generated value, and the fingerprint answered is the
encoded form's.`,
		Args: func(cmd *cobra.Command, args []string) error {
			if dash := cmd.ArgsLenAtDash(); dash >= 0 {
				if dash != 2 || len(args) < 3 {
					return fmt.Errorf("set <sops-file> <path> --generate -- <consumer…>")
				}
				if cmd.Flags().Changed("to-secret") {
					return fmt.Errorf("set takes --to-secret or a consumer, not both")
				}
				return nil
			}
			return cobra.ExactArgs(2)(cmd, args)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if !generate {
				return usageErr("set takes no value: --generate makes one")
			}
			enc, err := secret.ParseEncoding(encode)
			if err != nil {
				return usageErr("%v", err)
			}
			opt := secret.SetOptions{Length: length, Charset: charset}
			if vault != "" {
				v, err := parseRefs(vault)
				if err != nil {
					return usageErr("--vault: %v", err)
				}
				opt.Vault = v[0]
			}
			dst := secret.Ref{File: args[0], Path: args[1]}
			if err := a.sandboxFiles(nil, []secret.Ref{dst}); err != nil {
				return err
			}
			to := []string{dst.String()}
			if opt.Vault != (secret.Ref{}) {
				to = append([]string{opt.Vault.String()}, to...)
			}
			if cmd.Flags().Changed("to-secret") {
				t, err := secret.ParseKubeTarget(toSecret)
				if err != nil {
					return usageErr("--to-secret: %v", err)
				}
				to = append(to, t.String())
				if err := a.checkLabHeld(t); err != nil {
					return a.secretLog(err, "set", "%s: %s", strings.Join(to, " and "), outcome(err, ""))
				}
				opt.Secret = &t
			}
			if cmd.ArgsLenAtDash() == 2 {
				opt.Consumer, opt.Stdin = args[2:], in
				to = append(to, opt.Consumer[0])
				err := secret.Consumer(opt.Consumer)
				if err == nil {
					err = a.checkConsumerContext(opt.Consumer)
				}
				if err != nil {
					return a.secretLog(refused("%v", err), "set", "%s: %s", strings.Join(to, " and "), outcome(err, ""))
				}
			} else if in != (secret.Stdin{}) {
				return usageErr("--stdin-json and --stdin-field shape a consumer's stdin: set … -- <consumer…>")
			}
			if name != "" || namespace != "" {
				if name == "" || namespace == "" {
					return usageErr("--name and --namespace start a new Secret together")
				}
				opt.New = &secret.NewSecret{Name: name, Namespace: namespace}
			}
			ops, err := a.secretOpsKeyed()
			if err != nil {
				return err
			}
			ops.Encode = enc
			res, err := ops.Set(cmd.Context(), dst, opt)
			if res.Key != "" {
				to[slices.Index(to, dst.String())] = res.Key
			}
			done := "generated" + encodedAs(enc) + " " + res.Fingerprint
			if opt.Consumer != nil {
				done += fmt.Sprintf(", consumer exit %d", res.Code)
			}
			if err := a.secretLog(err, "set", "%s: %s", strings.Join(to, " and "), outcome(err, done)); err != nil {
				return err
			}
			text := fmt.Sprintf("wrote %s: %d characters%s, %s\n", strings.Join(to, " and "), length, encodedAs(enc), res.Fingerprint)
			if err := a.secretPrint(res, text+res.Output); err != nil {
				return err
			}
			if res.Code != 0 {
				return &exitError{code: res.Code}
			}
			return nil
		},
	}
	f := c.Flags()
	f.BoolVar(&generate, "generate", false, "generate the value")
	f.StringVar(&vault, "vault", "", "the shared vault's field that holds the value first (op://<vault>/<item>/<field>); none keeps it in the SOPS file alone")
	f.StringVar(&toSecret, "to-secret", "", "also a key of a Secret in a lab you hold: <context>/<namespace>/<name>/<key>")
	f.IntVar(&length, "length", 32, "the value's length")
	f.StringVar(&charset, "charset", "alnum", "the characters: "+strings.Join(secret.Charsets(), ", "))
	f.StringVar(&name, "name", "", "an absent file starts as a Secret of this metadata.name")
	f.StringVar(&namespace, "namespace", "", "an absent file starts as a Secret in this metadata.namespace")
	stdinFlags(c, &in)
	encodeFlag(c, &encode)
	return c
}

func (a *app) secretSetupCmd() *cobra.Command {
	var account string
	c := &cobra.Command{
		Use:   "setup [--service-account <name>]",
		Short: "Create the shared vault and beekeeper's service account, once",
		Long: `setup gives beekeeper the shared vault: it creates secret.vault when the
person's 1Password session finds none, creates a service account that reads
and writes that vault only, and writes the account's token to
secret.tokenFile (mode 0600), from op's output straight to the file. It
runs op as the person, in the caller's signed-in session (op signin first),
and answers the vault, the account and the token's length. A token file
that holds a token is refused: revoke that account and move the file aside
before a new one.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ops := &secret.Ops{Run: secretRun, Apply: secretApply, Vault: a.cfg.Secret.Vault, Session: a.cfg.Secret.Session}
			s, err := ops.Setup(cmd.Context(), account, a.cfg.Secret.TokenFile)
			if errors.Is(err, secret.ErrSetUp) {
				err = refused("%v", err)
			}
			if err := a.secretLog(err, "setup", "vault %s, service account %s: %s", s.Vault, account, outcome(err, fmt.Sprintf("token of %d bytes", s.TokenBytes))); err != nil {
				return err
			}
			created := "existing"
			if s.VaultCreated {
				created = "created"
			}
			return a.secretPrint(s, fmt.Sprintf("vault %s (%s, %s), service account %s: token of %d bytes in %s\n",
				s.Vault, s.VaultID, created, s.ServiceAccount, s.TokenBytes, s.TokenFile))
		},
	}
	host, _ := os.Hostname()
	c.Flags().StringVar(&account, "service-account", "beekeeper-"+host, "the service account's name")
	return c
}

func (a *app) secretImportCmd() *cobra.Command {
	var recipient string
	c := &cobra.Command{
		Use:   "import op://<vault>/<item>/<field> op://<shared-vault>/<item>/<field> | import op://<vault>/<item>/<field> --recipient <age1…>",
		Short: "Copy a field of the person's vault into the shared vault",
		Long: `import reads one field of a vault outside the shared vault with the
person's own 1Password session and writes it into a field of the shared
vault, creating the item or the field when absent. It answers the value's
length; from then on the shared vault's reference is the one to use.

With secret.session the broker runs it, in the person's vault session it
holds, for a session in the agent sandbox as well: the value stays in the
broker's call. Without secret.session the source is read in the caller's
own session (op signin first) and the destination written as beekeeper's
service account, on the host only: the service account reads no other
vault.

--recipient <age1…> imports an age identity for a SOPS recipient: the
destination is the shared vault's item of that recipient ("sops age key
<recipient>", its password field), and the source, an identity or an
identity file's text (comments above the AGE-SECRET-KEY-1… line, as
keys.txt holds it), is refused unless it holds that recipient's identity;
only the identity's line is stored:

  beekeeper secret import "op://<vault>/<installation>.agekey/notesPlain" --recipient age1…`,
		Args: func(cmd *cobra.Command, args []string) error {
			if recipient != "" {
				return cobra.ExactArgs(1)(cmd, args)
			}
			return cobra.ExactArgs(2)(cmd, args)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := parseRefs(args...)
			if err != nil {
				return err
			}
			ops, err := a.secretOps()
			if err != nil {
				return err
			}
			var dst secret.Ref
			var n int
			if recipient != "" {
				dst, n, err = ops.ImportAge(cmd.Context(), r[0], recipient)
				if dst.Op == "" {
					dst = secret.Ref{Op: "recipient " + recipient}
				}
			} else {
				dst = r[1]
				n, err = ops.Import(cmd.Context(), r[0], dst)
			}
			if err := a.secretLog(err, "import", "%s to %s: %s", r[0], dst, outcome(err, fmt.Sprintf("%d bytes", n))); err != nil {
				return err
			}
			return a.secretPrint(secret.Key{Name: dst.String(), Bytes: n}, fmt.Sprintf("wrote %s: %d bytes\n", dst, n))
		},
	}
	c.Flags().StringVar(&recipient, "recipient", "", "an age recipient (age1…): import its identity into the shared vault's item \"sops age key <recipient>\"")
	return c
}

func (a *app) secretRotateCmd() *cobra.Command {
	var charset, reason string
	var length int
	var generate, dryRun bool
	c := &cobra.Command{
		Use:   "rotate op://<vault>/<item>/<field> [--generate] | rotate platform://<installation>/<capability>/<name> --reason <text>",
		Short: "Replace a value everywhere it is carried",
		Long: `rotate op://… --generate replaces a value beekeeper generated: a new one
goes into the shared vault's field first, then into every path of the SOPS
files scan.sops names that carried the old one (the value itself, or its
base64 form in a Secret's data), matched by fingerprint.

rotate op://… without --generate carries a value a third party issued:
the person rotates it at its issuer into the vault's field, and rotate
writes the vault's new value into every path that carried the old one,
known by the fingerprint beekeeper scan index recorded before the change.

rotate platform://<installation>/<capability>/<name> --reason <text> runs
the platform manager's own rotation on the host (platformctl installation
reconcile --commit --rotate <name>): the manager writes the new value into
the installation's SOPS files in a pull request, and no copy goes to the
vault. --dry-run shows the files that hold it.

A rotation answers the new value's fingerprint and the paths it went to,
or platformctl's answer, and closes the open rotation notes of the
reference and of each path. Every SOPS file is read before anything is
written: one that cannot be read stops the rotation with nothing changed.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.HasPrefix(args[0], secret.PlatformPrefix) {
				if generate {
					return usageErr("--generate: the platform manager generates its credentials itself")
				}
				return a.secretRotatePlatform(cmd, args[0], reason, dryRun)
			}
			if dryRun || reason != "" {
				return usageErr("--dry-run and --reason are for a platform:// reference")
			}
			r, err := parseRefs(args[0])
			if err != nil {
				return err
			}
			ops, ix, err := a.secretOpsIndexed()
			if err != nil {
				return err
			}
			files, err := a.scanFiles()
			if err != nil {
				return err
			}
			var rot secret.Rotation
			if generate {
				rot, err = ops.RotateGenerated(cmd.Context(), r[0], files, ix, length, charset)
			} else {
				rot, err = ops.RotateIssued(cmd.Context(), r[0], files, ix)
			}
			if rot.Ref != "" {
				if serr := ix.Save(a.now); serr != nil {
					err = errors.Join(err, serr)
				}
			}
			err = a.secretLog(err, "rotate", "%s: %s", r[0], outcome(err, fmt.Sprintf("%s into %d carriers", rot.Fingerprint, len(rot.Carriers))))
			if rot.Ref != "" {
				refs := []string{rot.Ref}
				for _, c := range rot.Carriers {
					refs = append(refs, guard.SOPSRef+c.Ref)
				}
				a.closeRotationNotes(refs)
			}
			if err != nil {
				return err
			}
			var b strings.Builder
			fmt.Fprintf(&b, "rotated %s: %s, %d carriers\n", rot.Ref, rot.Fingerprint, len(rot.Carriers))
			for _, c := range rot.Carriers {
				form := ""
				if c.Base64 {
					form = " (base64)"
				}
				fmt.Fprintf(&b, "  %s%s\n", c.Ref, form)
			}
			return a.secretPrint(rot, b.String())
		},
	}
	f := c.Flags()
	f.BoolVar(&generate, "generate", false, "generate the new value (a value beekeeper made)")
	f.IntVar(&length, "length", 32, "the generated value's length")
	f.StringVar(&charset, "charset", "alnum", "the generated value's characters: "+strings.Join(secret.Charsets(), ", "))
	f.StringVar(&reason, "reason", "", "why a platform credential is rotated, for the manager's record")
	f.BoolVar(&dryRun, "dry-run", false, "show a platform credential's rotation without committing it")
	return c
}

// secretRotatePlatform is rotate of a platform:// reference.
func (a *app) secretRotatePlatform(cmd *cobra.Command, arg, reason string, dryRun bool) error {
	r, err := secret.ParsePlatformRef(arg)
	if err != nil {
		return usageErr("%v", err)
	}
	ops, err := a.secretOps()
	if err != nil {
		return err
	}
	out, err := ops.RotatePlatform(cmd.Context(), r, reason, dryRun)
	mode := "committed"
	if dryRun {
		mode = "dry run"
	}
	if err := a.secretLog(err, "rotate", "%s: %s", r, outcome(err, mode)); err != nil {
		return err
	}
	if !dryRun {
		a.closeRotationNotes([]string{r.String()})
	}
	_, err = io.WriteString(a.out, out)
	return err
}

// closeRotationNotes marks done the open rotation notes of refs.
func (a *app) closeRotationNotes(refs []string) {
	who, err := a.caller()
	if err != nil {
		who = state.Party{Name: noSession}
	}
	err = a.store.Update(func(st *state.State) ([]state.Event, error) {
		var evs []state.Event
		st.Notes = slices.DeleteFunc(st.Notes, func(n state.Note) bool {
			done := slices.ContainsFunc(refs, func(r string) bool { return strings.HasPrefix(n.Text, rotateNote+r+":") })
			if done {
				evs = append(evs, event(who, noteDone, "#%d %s", n.ID, n.Text))
			}
			return done
		})
		return evs, nil
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, guard.LogPrefix+"secret: the rotation notes stay open: "+err.Error())
	}
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
	ops := &secret.Ops{Run: secretRun, Apply: secretApply, Read: secretRead, Vault: a.cfg.Secret.Vault, Session: a.cfg.Secret.Session, Ages: a.ageIdentities(),
		AgeVaults: a.cfg.Secret.AgeVaults, Store: secret.Store{Read: a.cfg.Secret.Store.Read, Search: a.cfg.Secret.Store.Search}, Installations: a.installationNames()}
	if ops.Vault == "" || ops.Session || a.cfg.Secret.TokenFile == "" {
		return ops, nil
	}
	raw, err := os.ReadFile(a.cfg.Secret.TokenFile)
	if err != nil {
		return nil, fmt.Errorf("secret.tokenFile: %w", err)
	}
	ops.Token = strings.TrimSpace(string(raw))
	return ops, nil
}

// ageIdentities are secret.ageIdentities, their path patterns compiled
// (the config's validation compiled each once).
func (a *app) ageIdentities() []secret.AgeIdentity {
	out := make([]secret.AgeIdentity, 0, len(a.cfg.Secret.AgeIdentities))
	for _, id := range a.cfg.Secret.AgeIdentities {
		ai := secret.AgeIdentity{Recipient: id.Recipient, Ref: id.Ref}
		if id.PathRegex != "" {
			ai.Path = regexp.MustCompile(id.PathRegex)
		}
		out = append(out, ai)
	}
	return out
}

// installationNames are the installations the configuration names
// (alerts.installations).
func (a *app) installationNames() []string {
	out := make([]string, 0, len(a.cfg.Alerts.Installations))
	for _, in := range a.cfg.Alerts.Installations {
		out = append(out, in.Name)
	}
	return out
}

// secretNeedsVault reports whether a call's arguments, the operation first,
// take the shared vault: an op:// reference, a SOPS file, relative to dir
// (the working directory when empty), whose age identity lives there (an
// entry of secret.ageIdentities, or the vault's item per recipient), the
// recipients listing's check of those items, or an omp agent's start on a
// provider whose key is there. The client and the broker decide on it
// alike.
func (a *app) secretNeedsVault(dir string, args []string) bool {
	return a.secretArgsNeedVault(args) || a.ageOps().AgeNeedsVault(dir, args)
}

// secretArgsNeedVault is [app.secretNeedsVault] decided from the arguments
// alone, no file read: an op:// reference, a recipients listing or an omp
// start on a vault provider.
func (a *app) secretArgsNeedVault(args []string) bool {
	return secret.NeedsVault(args) || a.recipientsNeedVault(args) || a.ompStartNeedsVault(args)
}

// secretNeedsBroker reports whether a call, the operation first, goes to
// the broker with secret.session: one on the vault, or on a SOPS file whose
// age identity secret.ageIdentities names, a file's included, which the
// broker alone reads.
func (a *app) secretNeedsBroker(args []string) bool {
	return secret.NeedsVault(args) || a.ageOps().AgeNeedsIdentity("", args) || a.recipientsNeedVault(args)
}

// ageOps decide where a SOPS file's age identity lives.
func (a *app) ageOps() *secret.Ops {
	return &secret.Ops{Ages: a.ageIdentities(), Vault: a.cfg.Secret.Vault, AgeVaults: a.cfg.Secret.AgeVaults}
}

// recipientsNeedVault reports whether args are a recipients listing that
// checks the shared vault's items.
func (a *app) recipientsNeedVault(args []string) bool {
	return len(args) > 0 && args[0] == "recipients" && a.cfg.Secret.Vault != ""
}

func (a *app) secretRecipientsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "recipients [<directory> | <sops-file>]",
		Short: "The age recipients of a path's creation rules, or of a SOPS file, and where each identity is",
		Long: `recipients answers, for a directory (the working directory by default),
every age recipient of the creation rules of the .sops.yaml nearest above
it, a gitops repository's installations' for one, and for a SOPS file the
file's recipients (its metadata when encrypted, else its creation rule's),
each with where its identity is: sops' own sources, the entry of
secret.ageIdentities, the shared vault's item per recipient ("sops age key
<recipient>") or the installation's item ("<installation>.agekey" in the
shared vault or secret.ageVaults), each found in the vaults' item
listings, metadata only, or none, naming the items the vaults lack. No
value is read. It exits 1 when a
recipient has no identity: a Secret for it is one no agent can change.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path := "."
			if len(args) == 1 {
				path = args[0]
			}
			if err := a.sandboxPaths([]string{path}, nil); err != nil {
				return err
			}
			ops, err := a.secretOps()
			if err != nil {
				return err
			}
			rs, err := ops.Recipients(cmd.Context(), path)
			missing := 0
			for _, r := range rs {
				if r.Missing() {
					missing++
				}
			}
			if err := a.secretLog(err, "recipients", "%s: %s", path, outcome(err, fmt.Sprintf("%d recipients, %d without an identity", len(rs), missing))); err != nil {
				return err
			}
			var b strings.Builder
			for _, r := range rs {
				fmt.Fprintf(&b, "%-48s %s  %s\n", r.Rule, r.Recipient, r.Identity)
			}
			if err := a.secretPrint(rs, b.String()); err != nil {
				return err
			}
			if missing > 0 {
				return &exitError{code: ExitError}
			}
			return nil
		},
	}
}

// secretOpsKeyed are the operations with the fingerprint key, created on
// first use.
func (a *app) secretOpsKeyed() (*secret.Ops, error) {
	ops, _, err := a.secretOpsIndexed()
	return ops, err
}

// secretOpsIndexed are [app.secretOpsKeyed] and the index whose key they
// fingerprint with.
func (a *app) secretOpsIndexed() (*secret.Ops, *guard.Index, error) {
	ix, err := guard.OpenIndex(a.scanDir())
	if err != nil {
		return nil, nil, err
	}
	ix.MinLen = a.cfg.Scan.MinLength
	ops, err := a.secretOps()
	if err != nil {
		return nil, nil, err
	}
	ops.Fingerprint = func(v string) string { return "hmac:" + ix.Fingerprint(v)[:16] }
	return ops, ix, nil
}

// secretPrint prints text, or v with --json.
func (a *app) secretPrint(v any, text string) error {
	if a.json {
		return a.printJSON(v)
	}
	_, err := io.WriteString(a.out, text)
	return err
}
