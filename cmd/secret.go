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
the user's config directory) or from secret.ageIdentities: entries that map
a recipient or a pathRegex to the op:// field of the shared vault holding
its identity, read in beekeeper's process for the one sops call. A file
none of them has an identity for fails before sops runs, naming its
recipients and the sources checked.`,
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
			if err := a.sandboxFiles(r, nil); err != nil {
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
			if err := a.sandboxFiles(r, nil); err != nil {
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
	c.AddCommand(a.secretCopyCmd(), a.secretSetCmd(), a.secretRotateCmd(), a.secretSetupCmd(), a.secretImportCmd())
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
			if inSandbox || a.cfg.Secret.Session && a.secretNeedsBroker(callArgs(cmd, args)) {
				return a.secretBrokered(cmd, args, inSandbox)
			}
			return vaultExit(run(cmd, args))
		}
	}
	c.AddCommand(a.secretUnlockCmd(), a.secretLockCmd(), a.secretStatusCmd())
	return c
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
	if err := brokeredSecretArgs(argv, inSandbox); err != nil {
		return refused("%v; %s and %s run on the host, by the person", err, "setup", "import")
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
	var name, namespace, toSecret string
	var in secret.Stdin
	c := &cobra.Command{
		Use:   "copy <from> <to> | copy <from> -- <consumer…> | copy <from> --to-secret <context>/<namespace>/<name>/<key>",
		Short: "Copy a SOPS file, or one value into a SOPS path, a consumer's stdin or a lab's Secret",
		Long: `copy <src.sops.yaml> <dst.sops.yaml> writes a new SOPS file with the
values of src, encrypted under dst's creation rules; --name and --namespace
rewrite a Kubernetes object's metadata.name and metadata.namespace on the
way. It answers the key names and value lengths (a Secret's data decoded).
dst must not exist.

copy <ref> <file#path> puts one value into a SOPS path, creating the file
or the key when absent, the file's other values kept.

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
caller holds: a server-side apply that creates the Secret when absent and
keeps its other keys. kind's admin kubeconfig stays in beekeeper's memory
like the value; it answers the value's length. A context of a lab the
caller holds no lease for is refused.

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
			return cobra.ExactArgs(2)(cmd, args)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if in != (secret.Stdin{}) && cmd.ArgsLenAtDash() != 1 {
				return usageErr("--stdin-json and --stdin-field shape a consumer's stdin: copy <from> -- <consumer…>")
			}
			src, err := parseRefs(args[0])
			if err != nil {
				return err
			}
			if err := a.sandboxFiles(src, nil); err != nil {
				return err
			}
			ops, err := a.secretOps()
			if err != nil {
				return err
			}
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
				a.secretLog("copy", "%s to %s: %s", src[0], argv[0], outcome(err, fmt.Sprintf("exit %d", code)))
				if errors.Is(err, secret.ErrVault) {
					// exit ExitVault: the broker signs in again and retries
					return err
				}
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
			if err := a.sandboxFiles(nil, dst); err != nil {
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
	c.Flags().StringVar(&toSecret, "to-secret", "", "a key of a Secret in a lab you hold: <context>/<namespace>/<name>/<key>")
	stdinFlags(c, &in)
	return c
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
		a.secretLog("copy", "%s to %s: %s", src, t, outcome(err, ""))
		return err
	}
	n, err := ops.CopyToSecret(ctx, src, t)
	a.secretLog("copy", "%s to %s: %s", src, t, outcome(err, fmt.Sprintf("%d bytes", n)))
	if err != nil {
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
	var vault, charset, toSecret, name, namespace string
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

--vault op://<vault>/<item>/<field> writes the shared vault's field first
(creating the item or the field when absent), then the SOPS path.

--to-secret <context>/<namespace>/<name>/<key> also writes the value into
a key of a Secret in a kind lab whose lease the caller holds, as copy
--to-secret does; -- <consumer…> also runs a consumer with the value on
stdin, as copy <ref> -- <consumer…> does (kubectl exec -i into a pod and
--stdin-json included), and answers its output with the value redacted and
its exit code. Both come after the SOPS path is
written: when one fails, the SOPS path holds the value and copy finishes
the delivery.`,
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
					a.secretLog("set", "%s: %s", strings.Join(to, " and "), outcome(err, ""))
					return err
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
					a.secretLog("set", "%s: %s", strings.Join(to, " and "), outcome(err, ""))
					return refused("%v", err)
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
			res, err := ops.Set(cmd.Context(), dst, opt)
			done := "generated " + res.Fingerprint
			if opt.Consumer != nil {
				done += fmt.Sprintf(", consumer exit %d", res.Code)
			}
			a.secretLog("set", "%s: %s", strings.Join(to, " and "), outcome(err, done))
			if err != nil {
				return err
			}
			text := fmt.Sprintf("wrote %s: %d characters, %s\n", strings.Join(to, " and "), length, res.Fingerprint)
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
			a.secretLog("setup", "vault %s, service account %s: %s", s.Vault, account, outcome(err, fmt.Sprintf("token of %d bytes", s.TokenBytes)))
			if errors.Is(err, secret.ErrSetUp) {
				return refused("%v", err)
			}
			if err != nil {
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
	return &cobra.Command{
		Use:   "import op://<vault>/<item>/<field> op://<shared-vault>/<item>/<field>",
		Short: "Copy a field of the person's vault into the shared vault",
		Long: `import reads one field of a vault outside the shared vault with the
person's own 1Password session (op signin first) and writes it into a field
of the shared vault as beekeeper's service account, creating the item or
the field when absent. It answers the value's length; from then on the
shared vault's reference is the one to use.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := parseRefs(args...)
			if err != nil {
				return err
			}
			ops, err := a.secretOps()
			if err != nil {
				return err
			}
			n, err := ops.Import(cmd.Context(), r[0], r[1])
			a.secretLog("import", "%s to %s: %s", r[0], r[1], outcome(err, fmt.Sprintf("%d bytes", n)))
			if err != nil {
				return err
			}
			return a.secretPrint(secret.Key{Name: r[1].String(), Bytes: n}, fmt.Sprintf("wrote %s: %d bytes\n", r[1], n))
		},
	}
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
			a.secretLog("rotate", "%s: %s", r[0], outcome(err, fmt.Sprintf("%s into %d carriers", rot.Fingerprint, len(rot.Carriers))))
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
	a.secretLog("rotate", "%s: %s", r, outcome(err, mode))
	if err != nil {
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
	ops := &secret.Ops{Run: secretRun, Apply: secretApply, Vault: a.cfg.Secret.Vault, Session: a.cfg.Secret.Session, Ages: a.ageIdentities()}
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

// secretNeedsVault reports whether a call's arguments take the shared
// vault: an op:// reference, or a SOPS file, relative to dir (the working
// directory when empty), whose age identity lives there. The client and the
// broker decide on it alike.
func (a *app) secretNeedsVault(dir string, args []string) bool {
	return secret.NeedsVault(args) || (&secret.Ops{Ages: a.ageIdentities()}).AgeNeedsVault(dir, args)
}

// secretNeedsBroker reports whether a call goes to the broker with
// secret.session: one on the vault, or on a SOPS file whose age identity
// secret.ageIdentities names, a file's included, which the broker alone reads.
func (a *app) secretNeedsBroker(args []string) bool {
	return secret.NeedsVault(args) || (&secret.Ops{Ages: a.ageIdentities()}).AgeNeedsIdentity("", args)
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
