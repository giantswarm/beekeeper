package cmd

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/secret"
)

// githubAppOp is the secret subcommand that converts an App manifest's
// code into an installation's Secret.
const githubAppOp = "github-app"

// manifestCode is the shape of the one-time code GitHub's manifest flow
// redirects with: it goes into an API path, so nothing else passes.
var manifestCode = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// githubAppResult is what secret github-app answers: the App's
// configuration by value, the Secret's keys by length and fingerprint.
type githubAppResult struct {
	AppID    int64                  `json:"appID"`
	ClientID string                 `json:"clientID"`
	Slug     string                 `json:"slug"`
	Written  []secret.CaptureResult `json:"written"`
}

func (a *app) secretGitHubAppCmd() *cobra.Command {
	var code string
	var replace bool
	c := &cobra.Command{
		Use:   "github-app <installation> --code <code>",
		Short: "Convert a GitHub App manifest's one-time code and write the App's private key and client secret into the installation's SOPS Secret",
		Long: `github-app finishes GitHub's manifest flow for an App created on GitHub's
page: GitHub redirects to the manifest's redirect_url with a one-time code
(?code=…), and --code takes that code or the whole redirect URL. beekeeper
converts it in its own process with GitHub's unauthenticated POST
/app-manifests/<code>/conversions (no token: the code is the credential)
and writes the App's private-key and
client-secret, and webhook-secret when the App has one, into the
Kubernetes Secret of the SOPS file apps.secrets names for <installation>,
under stringData, in one sops write (the creation rules of the nearest
.sops.yaml; an absent file starts as the Secret of the entry's name and
namespace).

Every check runs before the code is spent: the installation's entry, the
file a Secret or absent with a name, each key under a creation rule that
encrypts it, and no key the Secret holds already unless --replace.

stdout is the App's id and client id, the values a consumer's
configuration takes in plain:

  app-id 123456
  client-id Iv23li…

stderr names the keys written with their lengths and keyed fingerprints;
the private key and the client secret appear in no output, log entry or
error. A conversion GitHub refuses is refused in one line with GitHub's
HTTP status and message; a code converts once, within an hour of GitHub's
redirect. beekeeper log records the installation,
the file, the App's id, client id and slug, and the fingerprints.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.secretGitHubApp(cmd, args[0], code, replace)
		},
	}
	c.Flags().StringVar(&code, "code", "", "the manifest flow's one-time code, or the redirect URL that carries it")
	c.Flags().BoolVar(&replace, "replace", false, "overwrite keys the Secret holds already")
	return c
}

// secretGitHubApp is secret github-app: the checks, the conversion and the
// write, logged.
func (a *app) secretGitHubApp(cmd *cobra.Command, installation, code string, replace bool) error {
	target, ok := a.cfg.Apps.Secrets[installation]
	if !ok {
		return usageErr("apps.secrets names no Secret for %q: it names %s", installation, strings.Join(slices.Sorted(maps.Keys(a.cfg.Apps.Secrets)), ", "))
	}
	code, err := parseManifestCode(code)
	if err != nil {
		return usageErr("--code: %v", err)
	}
	if err := a.sandboxFiles(nil, []secret.Ref{{File: target.File}}); err != nil {
		return err
	}
	ops, err := a.secretOpsKeyed()
	if err != nil {
		return err
	}
	opt := secret.SecretKeysOptions{Keys: []string{githubPrivateKey, githubClientSecret, appWebhookSecret}, Replace: replace}
	if target.Name != "" {
		opt.New = &secret.NewSecret{Name: target.Name, Namespace: target.Namespace}
	}
	var created *githubApp
	opt.Produce = func(ctx context.Context) (map[string]string, error) {
		var err error
		if created, err = convertManifest(ctx, code); err != nil {
			return nil, err
		}
		values := map[string]string{githubPrivateKey: created.PEM, githubClientSecret: created.ClientSecret}
		if created.WebhookSecret != nil {
			values[appWebhookSecret] = *created.WebhookSecret
		}
		return values, nil
	}
	written, err := ops.SecretKeys(cmd.Context(), target.File, opt)
	if err != nil && !errors.Is(err, secret.ErrVault) {
		if created != nil {
			err = fmt.Errorf("the App %s (id %d) exists and its credentials are stored nowhere: delete it on GitHub and create it again; %w", created.Slug, created.ID, err)
		}
		err = refused("%v", err)
	}
	done := ""
	res := githubAppResult{Written: written}
	if created != nil {
		res.AppID, res.ClientID, res.Slug = created.ID, created.ClientID, created.Slug
		done = fmt.Sprintf("app %s, %s %d, %s %s", created.Slug, appIDField, created.ID, appClientIDField, created.ClientID)
	}
	for _, w := range written {
		done += fmt.Sprintf("; %s %d bytes, %s", w.Key, w.Bytes, w.Fingerprint)
	}
	if err := a.secretLog(err, githubAppOp, "%s (%s): %s", installation, target.File, outcome(err, done)); err != nil {
		return err
	}
	if a.json {
		return a.printJSON(res)
	}
	for _, w := range written {
		if _, err := fmt.Fprintf(cmd.ErrOrStderr(), "wrote %s: %d bytes, %s\n", w.Key, w.Bytes, w.Fingerprint); err != nil {
			return err
		}
	}
	_, err = fmt.Fprintf(a.out, "%s %s\n%s %s\n", appIDField, strconv.FormatInt(res.AppID, 10), appClientIDField, res.ClientID)
	return err
}

// parseManifestCode is the code itself, or the code a redirect URL
// carries in its query.
func parseManifestCode(s string) (string, error) {
	s = strings.TrimSpace(s)
	if strings.Contains(s, "://") {
		u, err := url.Parse(s)
		if err != nil {
			return "", errors.New("no URL")
		}
		s = u.Query().Get("code")
	}
	if !manifestCode.MatchString(s) {
		return "", errors.New("no manifest code (letters, digits, - and _), nor a redirect URL with ?code=")
	}
	return s, nil
}
