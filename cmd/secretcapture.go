package cmd

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/secret"
)

func (a *app) secretCaptureCmd() *cobra.Command {
	var vault, jq, encode, registry, username, name, namespace string
	c := &cobra.Command{
		Use:   "capture <sops-file#path> [--jq <filter>] [--encode <encoding>] [--vault op://<vault>/<item>/<field>] [--name n --namespace ns] -- <producer…> | capture <sops-file> --dockerconfigjson <registry> --username <user> [--vault op://…] [--name n --namespace ns] -- <producer…>",
		Short: "Run a producer that prints a credential once and store its value into a SOPS path, and the shared vault",
		Long: `capture stores a credential its issuer generates and prints once (a
registry token's password, an API token created over an API, a cloud
access key), which no caller could store without reading it. The producer
runs in beekeeper's process with the caller's environment minus every
variable that carries a secret (the vault's credentials, SOPS_AGE_KEY and
the names secret.env lists), its stderr passed through. The value is its
stdout without the line end, or with --jq <filter> the one string the
filter picks from its JSON output; the value goes into the SOPS file's
dotted path as set writes one (the creation rules of the nearest .sops.yaml,
a Secret's value under stringData, --name and --namespace starting an
absent file as that Secret).

Every check runs before the producer does: the references, the path
against the creation rule, the filter. A producer that exits non-zero or
prints nothing on stdout, a filter that picks no single non-empty string,
is refused in one line, nothing stored.

--vault op://<vault>/<item>/<field> writes the shared vault's field first,
then the SOPS path; --encode base64 or basic:<user> writes the SOPS path
the value's encoded form, the vault's field keeping the printed value.

--dockerconfigjson <registry> --username <user> writes the value as the
password of a kubernetes.io/dockerconfigjson Secret in one step: the file
names no path, its stringData holds .dockerconfigjson (the registry's auths
entry with the user, the password and their base64 auth), and its type is
kubernetes.io/dockerconfigjson.

It answers where the value went, its length as written and its keyed
fingerprint; the producer's stdout is never shown, and beekeeper log
records the producer's command and the references, never the value:

  beekeeper secret capture pull.sops.yaml --dockerconfigjson <registry> \
    --username <token> --name pull --namespace flux-system \
    --jq '.passwords[0].value' -- az acr token credential generate \
    --registry <registry> --name <token> --password1`,
		Args: func(cmd *cobra.Command, args []string) error {
			if dash := cmd.ArgsLenAtDash(); dash != 1 || len(args) < 2 {
				return errors.New("capture <sops-file#path> -- <producer…>")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			enc, err := secret.ParseEncoding(encode)
			if err != nil {
				return usageErr("%v", err)
			}
			dst, err := parseRefs(args[0])
			if err != nil {
				return err
			}
			opt := secret.CaptureOptions{Producer: args[1:], Env: secret.ProducerEnv(os.Environ(), a.cfg.Secret.Env), Stderr: cmd.ErrOrStderr(), JQ: jq}
			if registry != "" || username != "" {
				if registry == "" || username == "" {
					return usageErr("--dockerconfigjson and --username shape a registry's pull credential together")
				}
				opt.Docker = &secret.DockerConfig{Registry: registry, Username: username}
			}
			if vault != "" {
				v, err := parseRefs(vault)
				if err != nil {
					return usageErr("--vault: %v", err)
				}
				opt.Vault = v[0]
			}
			if name != "" || namespace != "" {
				if name == "" || namespace == "" {
					return usageErr("--name and --namespace start a new Secret together")
				}
				opt.New = &secret.NewSecret{Name: name, Namespace: namespace}
			}
			if err := a.sandboxFiles(nil, dst); err != nil {
				return err
			}
			to := []string{dst[0].String()}
			if opt.Vault != (secret.Ref{}) {
				to = append([]string{opt.Vault.String()}, to...)
			}
			ops, err := a.secretOpsKeyed()
			if err != nil {
				return err
			}
			ops.Encode = enc
			res, err := ops.Capture(cmd.Context(), dst[0], opt)
			if err != nil && !errors.Is(err, secret.ErrVault) {
				err = refused("%v", err)
			}
			if res.Key != "" {
				to[len(to)-1] = res.Key
			}
			form := encodedAs(enc)
			if opt.Docker != nil {
				form = " as the dockerconfigjson of " + registry
			}
			if err := a.secretLog(err, "capture", "%s%s from %s: %s", strings.Join(to, " and "), form, strings.Join(opt.Producer, " "),
				outcome(err, fmt.Sprintf("%d bytes, %s", res.Bytes, res.Fingerprint))); err != nil {
				return err
			}
			return a.secretPrint(res, fmt.Sprintf("wrote %s: %d bytes%s, %s\n", strings.Join(to, " and "), res.Bytes, form, res.Fingerprint))
		},
	}
	f := c.Flags()
	f.StringVar(&jq, "jq", "", "a jq filter that picks the value, one string, from the producer's JSON output (.passwords[0].value)")
	f.StringVar(&vault, "vault", "", "the shared vault's field that holds the value first (op://<vault>/<item>/<field>)")
	f.StringVar(&registry, "dockerconfigjson", "", "write a kubernetes.io/dockerconfigjson Secret for this registry, the value its password")
	f.StringVar(&username, "username", "", "the user of --dockerconfigjson")
	f.StringVar(&name, "name", "", "an absent file starts as a Secret of this metadata.name")
	f.StringVar(&namespace, "namespace", "", "an absent file starts as a Secret in this metadata.namespace")
	encodeFlag(c, &encode)
	return c
}
