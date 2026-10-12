package cmd

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"html"
	"io"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/guard"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/secret"
	"github.com/giantswarm/beekeeper/internal/state"
	"github.com/giantswarm/beekeeper/pkg/project"
)

// app create runs GitHub's manifest flow for an App the organisation
// declares: the manifest goes to GitHub from a page on a loopback listener,
// the person clicks Create there, GitHub redirects to the listener with a
// one-time code, beekeeper converts the code in its own process and writes
// the credentials GitHub answers into one item of the shared vault, then
// sends the browser on to the App's installation page, where the person
// clicks Install, and records the installation's id once it exists. No
// value reaches the session: the answer is ids, the slug, lengths and
// fingerprints.

// appCreated is the event of an App created from its manifest.
const appCreated = "app.create"

// The fields of an App's vault item: the configuration, plain, and the
// secrets, concealed (githubClientSecret and githubPrivateKey are
// secret store's).
const (
	appIDField          = "app-id"
	appClientIDField    = "client-id"
	appSlugField        = "slug"
	appWebhookSecret    = "webhook-secret"
	appInstallationID   = "installation-id"
	appCreateWait       = 10 * time.Minute
	browserResource     = "browser"
	appManifestField    = "manifest"
	appRedirectField    = "redirect_url"
	appCallbackPath     = "/callback"
	maxPreselectedRepos = 100
)

// The GitHub hosts the flow talks to: the web site that takes the manifest
// and shows the installation page, the API that converts the code and
// lists the installations. Tests point both at a fake.
var (
	githubWeb = "https://github.com"
	githubAPI = "https://api.github.com"
)

// appInstallPoll is how often the installation is looked for after the
// Install click.
var appInstallPoll = 3 * time.Second

// openBrowser opens url in the person's browser, through the desktop's
// opener; tests replace it.
var openBrowser = func(ctx context.Context, url string) error {
	c := exec.CommandContext(ctx, "xdg-open", url) //nolint:gosec // the desktop's opener with this run's loopback URL
	c.Stdout, c.Stderr = io.Discard, io.Discard
	return c.Run()
}

func (a *app) appCreateCmd() *cobra.Command {
	var wait time.Duration
	var manifest, org string
	var replace, noOpen bool
	c := &cobra.Command{
		Use:   "create <name> | create <installation> --manifest <file>",
		Short: "Create a GitHub App from its declared manifest: the credentials into the shared vault, then its installation",
		Long: `create runs GitHub's manifest flow for the App <name> the organisation
declares (its manifest in the config's apps.repo at apps.manifest, read
from the default branch) and takes every credential the flow answers into
the shared vault, where no person copies a value from a web page:

  1. The manifest, with this run's redirect URL and a state nonce, goes
     to https://github.com/organizations/<org>/settings/apps/new from a
     page on a loopback listener, opened in the person's browser. The
     person's first click is GitHub's "Create GitHub App".
  2. GitHub redirects to the listener with a one-time code. beekeeper
     checks the state and converts the code in its own process
     (POST /app-manifests/<code>/conversions).
  3. The conversion's answer goes into the vault item <name> in one
     write: app-id, client-id and slug as configuration, client-secret,
     private-key and webhook-secret (when the App has a webhook) as
     secrets. The answer here is the ids, the slug and each secret's
     length and keyed fingerprint; beekeeper log records the same.
  4. The browser goes on to https://github.com/apps/<slug>/installations/new
     with the organisation and the repositories preselected that
     installation.json next to the manifest declares ({"repositories":
     "all" | ["<name>", …]}; absent: all). The person's second click is
     GitHub's "Install". Once the installation exists it is recorded as
     installation-id, as configuration.

The command names each click and waits for it, up to --timeout each, in an
agent session too: no value reaches the session. In an agent session the
caller holds the browser lease, as for beekeeper browse; the person's own
terminal needs none. Refused in one line, nothing written: an App no
manifest declares under its name, one GitHub already knows by that name, a
redirect whose state is not this run's, and a Create click that does not
come within --timeout. An Install click that does not come leaves the
vault's credentials as written and says how to record the installation's
id. With secret.session the vault write goes to the broker over the
keeper's socket, a locked vault refusing before any click (exit 78).

With --manifest <file>, the argument is an installation of apps.secrets and
the App is the one the local manifest (JSON, with its "name") declares; its
credentials go into that installation's SOPS Secret, as secret github-app
writes them:

  1. Every check runs first: the installation's entry, the Secret's file
     and its creation rules, no key it holds already unless --replace, and
     no App GitHub knows by the manifest's name.
  2. A listener on 127.0.0.1 (a random port, bound before the page exists)
     serves the page that posts the manifest, with the listener's callback
     as redirect_url and a state nonce, to GitHub's page for a new App of
     --org (default: apps.repo's owner). The browser opens it, or with
     --no-open the caller does (beekeeper browse "open <url> ..."); the
     click is GitHub's "Create GitHub App".
  3. The listener takes GitHub's redirect once, checks the state, converts
     the code in this process (unauthenticated, as GitHub documents it) and
     writes private-key, client-secret and webhook-secret (when the App has
     one) in one sops write. The browser goes on to the App's installation
     page.

stdout is the App's id and client id alone; stderr is the progress and the
keys written with their lengths and fingerprints. The code, the key and the
secrets reach no output, log entry or error.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if manifest != "" {
				return a.appCreateSecret(cmd, strings.TrimSpace(args[0]), manifest, org, wait, replace, !noOpen)
			}
			if org != "" || replace || noOpen {
				return usageErr("--org, --replace and --no-open go with --manifest")
			}
			return a.appCreate(cmd.Context(), strings.TrimSpace(args[0]), wait, cmd.ErrOrStderr())
		},
	}
	c.Flags().DurationVar(&wait, "timeout", appCreateWait, "how long each of the person's clicks (Create, Install) is waited for")
	c.Flags().StringVar(&manifest, "manifest", "", "a local App manifest (JSON): create that App and write its credentials into the SOPS Secret apps.secrets names for <installation>")
	c.Flags().StringVar(&org, "org", "", "with --manifest, the organisation that owns the App (default: apps.repo's owner)")
	c.Flags().BoolVar(&replace, "replace", false, "with --manifest, overwrite keys the Secret holds already")
	c.Flags().BoolVar(&noOpen, "no-open", false, "with --manifest, print the loopback page's URL and open no browser: the caller opens it (beekeeper browse)")
	return c
}

// appPlan is what a creation checks before any click: the declaration,
// the organisation and its id, the slug GitHub gives the name, the vault
// item, and the ids of the declared repositories.
type appPlan struct {
	name, org, slug, item string
	decl                  github.AppDeclaration
	orgID                 int64
	repoIDs               []int64
}

// appCreation is a creation's answer.
type appCreation struct {
	Item                string          `json:"item"`
	AppID               int64           `json:"appID"`
	ClientID            string          `json:"clientID"`
	Slug                string          `json:"slug"`
	URL                 string          `json:"url"`
	Declared            string          `json:"declared"`
	Installation        string          `json:"installation,omitempty"`
	Fields              []secret.Stored `json:"fields"`
	InstallationID      int64           `json:"installationID,omitempty"`
	RepositorySelection string          `json:"repositorySelection,omitempty"`
}

// appCreate is app create: the checks, the flow and the answer, logged.
func (a *app) appCreate(ctx context.Context, name string, wait time.Duration, progress io.Writer) error {
	if name == "" {
		return usageErr("an App is created by its name")
	}
	say := a.out
	if a.json {
		say = progress
	}
	res, err := a.createApp(ctx, name, wait, say)
	if err != nil && !errors.Is(err, secret.ErrVault) && Code(err) == ExitError {
		err = refused("%v", err)
	}
	done := "nothing written"
	if res != nil {
		done = fmt.Sprintf("%s: app-id %d, client-id %s, slug %s", res.Item, res.AppID, res.ClientID, res.Slug)
		for _, s := range res.Fields {
			done += fmt.Sprintf("; %s %d bytes, %s", s.Ref, s.Bytes, s.Fingerprint)
		}
		if res.InstallationID != 0 {
			done += fmt.Sprintf("; installation-id %d (%s)", res.InstallationID, res.RepositorySelection)
		}
	}
	if err := a.opLog(err, appCreated, "%s: %s", name, outcome(err, done)); err != nil {
		return err
	}
	if a.json && res != nil {
		return a.printJSON(res)
	}
	return nil
}

// createApp runs the flow and answers what it wrote, nil before GitHub
// was touched; what it wrote travels with a failure after that point.
func (a *app) createApp(ctx context.Context, name string, wait time.Duration, say io.Writer) (*appCreation, error) {
	if inSandbox() {
		return nil, refused("app create runs on the host: the agent sandbox reaches neither the person's browser nor the vault keeper")
	}
	if a.cfg.Secret.Vault == "" {
		return nil, &exitError{code: ExitVault, msg: "no shared vault is configured (secret.vault): app create writes the App's credentials there"}
	}
	plan, err := a.appPlan(ctx, name)
	if err != nil {
		return nil, err
	}
	if err := a.vaultReady([]intake{{ref: secret.Ref{Op: plan.item + "/" + appIDField}}}); err != nil {
		return nil, err
	}
	if err := a.holdsBrowser(); err != nil {
		return nil, err
	}
	flow, err := serveManifest(plan.org, plan.name, plan.decl.Source, plan.decl.Manifest)
	if err != nil {
		return nil, err
	}
	defer flow.stop()
	unrecord, err := a.recordRun(plan.name, plan.org, plan.decl.Source, flow)
	if err != nil {
		return nil, err
	}
	defer unrecord()

	repos := "all repositories"
	if plan.decl.Repositories != nil {
		repos = fmt.Sprintf("%d repositories (%s)", len(plan.repoIDs), strings.Join(plan.decl.Repositories, ", "))
	}
	if _, err := fmt.Fprintf(say, "App %s, declared in %s, to be installed on %s of %s\n", plan.name, plan.decl.Source, repos, plan.org); err != nil {
		return nil, err
	}
	code, err := flow.awaitCode(ctx, say, true, wait)
	if err != nil {
		return nil, err
	}
	if _, err := fmt.Fprintln(say, "GitHub redirected with the App's code: converting it and storing the credentials"); err != nil {
		return nil, err
	}
	created, err := convertManifest(ctx, code)
	if err != nil {
		flow.installs <- flowOutcome{err: err}
		return nil, err
	}
	stored, err := a.storeFields(ctx, plan.item, appFields(created))
	where := plan.item
	if err != nil {
		where = ""
	}
	a.recordCreated(say, plan.name, plan.org, plan.decl.Source, where, created)
	if err != nil {
		flow.installs <- flowOutcome{err: err}
		return nil, fmt.Errorf("the App %s exists at %s and its credentials are stored nowhere (%w): delete it there (Advanced, Delete GitHub App) and run again",
			created.Slug, created.HTMLURL, err)
	}
	res := &appCreation{Item: plan.item, AppID: created.ID, ClientID: created.ClientID, Slug: created.Slug, URL: created.HTMLURL,
		Declared: plan.decl.Source, Installation: plan.decl.Installation, Fields: stored}
	installURL := installURL(created.Slug, plan.orgID, plan.repoIDs)
	flow.installs <- flowOutcome{installURL: installURL}
	if _, err := fmt.Fprintf(say, "created App %s (id %d, client id %s): %s\n%s\n", created.Slug, created.ID, created.ClientID, created.HTMLURL, wroteLine(res)); err != nil {
		return res, err
	}
	if _, err := fmt.Fprintf(say, "the browser goes on to %s with %s preselected. Click \"Install\" there; waiting up to %s for the installation.\n",
		installURL, repos, wait); err != nil {
		return res, err
	}
	inst, err := waitInstallation(ctx, created, plan.org, wait)
	if err != nil {
		return res, refused("the vault holds the App's credentials; %v: once it is installed, its id is the item's %s (beekeeper secret store %s/%s, the person's)",
			err, appInstallationID, plan.item, appInstallationID)
	}
	idStored, err := a.storeFields(ctx, plan.item, []secret.ItemField{{Label: appInstallationID, Value: strconv.FormatInt(inst.ID, 10), Plain: true}})
	if err != nil {
		return res, fmt.Errorf("the vault holds the App's credentials, not its installation id %d: %w", inst.ID, err)
	}
	res.Fields = append(res.Fields, idStored...)
	res.InstallationID, res.RepositorySelection = inst.ID, inst.RepositorySelection
	_, err = fmt.Fprintf(say, "installed: installation-id %d (repositories: %s), wrote %s\n", inst.ID, inst.RepositorySelection, idStored[0].Ref)
	return res, err
}

// serveManifest binds this run's loopback listener and serves the page
// that submits manifest, with the listener's callback as its redirect URL
// and a fresh state, to GitHub's page for a new App of org. The listener
// is bound before the page exists; stop closes it.
func serveManifest(org, name, source string, manifest map[string]any) (*manifestFlow, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	state, err := nonce()
	if err != nil {
		_ = l.Close()
		return nil, err
	}
	local := "http://" + l.Addr().String()
	manifest = maps.Clone(manifest)
	manifest[appRedirectField] = local + appCallbackPath
	raw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		_ = l.Close()
		return nil, err
	}
	createURL := githubWeb + "/organizations/" + url.PathEscape(org) + "/settings/apps/new?state=" + url.QueryEscape(state)
	flow := &manifestFlow{org: org, name: name, local: local, state: state, codes: make(chan string, 1), fails: make(chan error, 1), installs: make(chan flowOutcome, 1),
		page: fmt.Sprintf(manifestPage, html.EscapeString(name), html.EscapeString(createURL), html.EscapeString(name),
			html.EscapeString(source), html.EscapeString(string(raw)))}
	srv := &http.Server{Handler: flow.mux(), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(l) }()
	// stop lets a callback in flight answer the browser, then closes
	flow.stop = func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if srv.Shutdown(ctx) != nil {
			_ = srv.Close()
		}
	}
	return flow, nil
}

// awaitCode names the loopback page, opens it in the browser when open,
// and waits up to wait for GitHub's redirect with the code.
func (f *manifestFlow) awaitCode(ctx context.Context, say io.Writer, open bool, wait time.Duration) (string, error) {
	how := "opening " + f.local + "/ in the browser"
	if !open {
		how = "open " + f.local + "/ in the browser"
	}
	if _, err := fmt.Fprintf(say, "%s: it submits the manifest to GitHub (organizations/%s/settings/apps/new). "+
		"Click \"Create GitHub App\" there; waiting up to %s for GitHub's redirect.\n", how, f.org, wait); err != nil {
		return "", err
	}
	if open {
		if err := openBrowser(ctx, f.local+"/"); err != nil {
			if _, err := fmt.Fprintf(say, "the browser did not open (%v): open %s/ yourself\n", err, f.local); err != nil {
				return "", err
			}
		}
	}
	select {
	case code := <-f.codes:
		return code, nil
	case err := <-f.fails:
		return "", err
	case <-time.After(wait):
		return "", refused("no redirect from GitHub within %s: the Create click did not come; nothing written"+takenWords, wait, f.name, githubWeb, githubWeb, f.org, guard.DeleteForm)
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// wroteLine is the answer's line of what the vault item holds: the
// configuration by value, the secrets by length and fingerprint.
func wroteLine(res *appCreation) string {
	line := fmt.Sprintf("wrote %s: %s %d, %s %s, %s %s (configuration)", res.Item, appIDField, res.AppID, appClientIDField, res.ClientID, appSlugField, res.Slug)
	for _, s := range res.Fields {
		if label := s.Ref[strings.LastIndex(s.Ref, "/")+1:]; label == githubClientSecret || label == githubPrivateKey || label == appWebhookSecret {
			line += fmt.Sprintf("; %s %d bytes, %s", label, s.Bytes, s.Fingerprint)
		}
	}
	return line
}

// appPlan reads and checks everything a creation needs before any click:
// the declaration under the App's name, the organisation of the declaring
// repository and its id, that GitHub knows no App of that name, the vault
// item, and the ids of the repositories the installation declares.
func (a *app) appPlan(ctx context.Context, name string) (*appPlan, error) {
	path := a.cfg.Apps.ManifestOf(name)
	if path == "" {
		return nil, refused("no repository declares the organisation's Apps: set apps.repo and apps.manifest (with %s) in the config", config.AppName)
	}
	decl, err := github.ReadAppDeclaration(ctx, appsGH, a.cfg.Apps.Repo, path)
	if err != nil {
		return nil, refused("App %s is not declared: %v", name, err)
	}
	if !strings.EqualFold(decl.Name, name) {
		return nil, refused("App %s is not declared: %s names the App %s", name, decl.Source, decl.Name)
	}
	p := &appPlan{name: name, decl: decl, slug: guard.AppSlug(decl.Name), item: guard.OpRef + a.cfg.Secret.Vault + "/" + name}
	p.org, _, _ = strings.Cut(a.cfg.Apps.Repo, "/")
	if known, err := a.appKnown(ctx, p.slug); err != nil {
		return nil, err
	} else if known != nil {
		return nil, knownRefusal(decl.Name, known)
	}
	if p.orgID, err = a.githubID(ctx, "orgs/"+p.org); err != nil {
		return nil, fmt.Errorf("the organisation %s: %w", p.org, err)
	}
	if len(decl.Repositories) > maxPreselectedRepos {
		return nil, refused("%s names %d repositories: GitHub preselects at most %d on the installation page", decl.Installation, len(decl.Repositories), maxPreselectedRepos)
	}
	for _, r := range decl.Repositories {
		id, err := a.githubID(ctx, "repos/"+p.org+"/"+r)
		if err != nil {
			if strings.Contains(err.Error(), "HTTP 404") {
				return nil, refused("%s names the repository %s, which %s lacks; nothing written", decl.Installation, r, p.org)
			}
			return nil, fmt.Errorf("the repository %s/%s: %w", p.org, r, err)
		}
		p.repoIDs = append(p.repoIDs, id)
	}
	return p, nil
}

// knownApp is what GitHub says of an App it knows by its slug.
type knownApp struct {
	ID      int64  `json:"id"`
	Slug    string `json:"slug"`
	HTMLURL string `json:"html_url"`
	Owner   struct {
		Login string `json:"login"`
		Type  string `json:"type"`
	} `json:"owner"`
}

// knownRefusal refuses a creation under a name GitHub already knows, saying
// where GitHub holds the App: its Advanced page under the organisation or
// under the person's own account, where its deletion goes once on record.
func knownRefusal(name string, k *knownApp) error {
	where := fmt.Sprintf("under the account %s's own settings, %s/settings/apps/%s/advanced (that account's alone)", k.Owner.Login, githubWeb, k.Slug)
	if strings.EqualFold(k.Owner.Type, "Organization") {
		where = fmt.Sprintf("under the organisation %s's settings, %s/organizations/%s/settings/apps/%s/advanced", k.Owner.Login, githubWeb, k.Owner.Login, k.Slug)
	}
	return refused("GitHub already knows an App named %s (%s, id %d, owned by %s): %s; nothing written. GitHub holds it %s; "+
		"its deletion is answered by the browse hook once on record (%s)", name, k.Slug, k.ID, k.Owner.Login, k.HTMLURL, where, guard.DeleteForm)
}

// takenWords tell a run whose Create click did not come where GitHub holds
// a name its page answered "Name is already taken" for, which the API
// does not show: an App or a reservation under the person's own account or
// the organisation's.
const takenWords = `. Where GitHub's page answered "Name is already taken", it holds an App or a reservation of the name %s that its API does not show: ` +
	"under the person's own account (%s/settings/apps) or the organisation's (%s/organizations/%s/settings/apps): read those pages in a browse turn, " +
	"and record its deletion with %s"

// recordRun puts this run of the App name on record while its listener
// serves, for the browse hook's Create and Install clicks, dropping the
// runs whose process is gone; the func it answers takes it off.
func (a *app) recordRun(name, org, declared string, flow *manifestFlow) (func(), error) {
	me, _ := a.caller()
	run := state.AppRun{Name: name, Org: org, Declared: declared, Listener: strings.TrimPrefix(flow.local, "http://"), PID: os.Getpid(), By: me, At: a.now.UTC()}
	err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.AppRuns = append(slices.DeleteFunc(st.AppRuns, func(x state.AppRun) bool { return !proc.Alive(x.PID) }), run)
		return nil, nil
	})
	if err != nil {
		return nil, err
	}
	return func() {
		_ = a.store.Update(func(st *state.State) ([]state.Event, error) {
			st.AppRuns = slices.DeleteFunc(st.AppRuns, func(x state.AppRun) bool { return x.PID == run.PID && x.Listener == run.Listener })
			return nil, nil
		})
	}, nil
}

// recordCreated enters the App the conversion created into beekeeper's
// ledger, with where its credentials went ("" for nowhere): the browse
// hook answers its Install click, and its deletion when nothing holds its
// credentials. A ledger that does not take it is said on say.
func (a *app) recordCreated(say io.Writer, name, org, declared, stored string, created *githubApp) {
	me, _ := a.caller()
	entry := state.AppCreated{Name: name, Slug: created.Slug, ID: created.ID, Org: org, Declared: declared, Stored: stored, By: me, At: a.now.UTC()}
	err := a.store.Update(func(st *state.State) ([]state.Event, error) {
		st.AppsCreated = append(slices.DeleteFunc(st.AppsCreated, func(x state.AppCreated) bool { return x.ID == created.ID }), entry)
		return nil, nil
	})
	if err != nil {
		_, _ = fmt.Fprintf(say, "beekeeper's ledger of its Apps did not take %s (id %d): %v; the browse hook does not answer its Install click\n", created.Slug, created.ID, err)
	}
}

// appKnown asks GitHub for the App of slug: the App when it knows one, nil
// when it answers 404.
func (a *app) appKnown(ctx context.Context, slug string) (*knownApp, error) {
	out, err := appsGH(ctx, "api", "apps/"+url.PathEscape(slug))
	if err != nil {
		if strings.Contains(err.Error(), "HTTP 404") {
			return nil, nil
		}
		return nil, fmt.Errorf("whether GitHub knows an App %s: %w", slug, err)
	}
	var k knownApp
	if err := json.Unmarshal(out, &k); err != nil || k.ID == 0 {
		return nil, fmt.Errorf("whether GitHub knows an App %s: gh api answered no App", slug)
	}
	return &k, nil
}

// githubID is the id of what the API path names.
func (a *app) githubID(ctx context.Context, path string) (int64, error) {
	out, err := appsGH(ctx, "api", path, "--jq", ".id")
	if err != nil {
		return 0, err
	}
	id, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("gh api %s answered no id", path)
	}
	return id, nil
}

// holdsBrowser refuses, in an agent session, unless the caller holds the
// browser lease: the manifest page opens in the person's browser, in front
// of whatever they work in. The person's own terminal needs no lease.
func (a *app) holdsBrowser() error {
	if !agentSession() {
		return nil
	}
	return a.holdsLease(browserResource, "app create opens the manifest page in the person's browser")
}

// agentSession reports whether an agent session runs this command.
func agentSession() bool {
	for _, k := range agentMarkers {
		if os.Getenv(k) != "" {
			return true
		}
	}
	return false
}

// nonce is the flow's state: 32 random bytes, URL-safe.
func nonce() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// manifestPage is the loopback page that submits the manifest to GitHub:
// the App's name, the create URL, the name and the declaration again, and
// the manifest, each HTML-escaped. It submits itself; the button is for a
// browser without scripts.
const manifestPage = `<!doctype html>
<meta charset="utf-8">
<title>beekeeper: create the GitHub App %s</title>
<body style="font: 16px system-ui, sans-serif; margin: 3em; max-width: 60em">
<form method="post" action="%s">
<p>beekeeper submits the manifest of the App <b>%s</b>, declared in <code>%s</code>, to GitHub.
On GitHub's page, click <b>Create GitHub App</b>.</p>
<p><textarea name="manifest" rows="24" cols="100" readonly>%s</textarea></p>
<p><button type="submit">Continue to GitHub</button></p>
</form>
<script>document.forms[0].submit()</script>
`

// flowOutcome is what the callback answers the browser with after the
// conversion: the installation page, or a failure.
type flowOutcome struct {
	installURL string
	err        error
}

// manifestFlow is the loopback listener's state: the page, the state nonce,
// and the channels between the callback and the command.
type manifestFlow struct {
	org, name string
	local     string
	state     string
	page      string
	stop      func()
	// codes carries the redirect's code to the command, fails a redirect
	// the callback refused, installs the command's outcome back.
	codes    chan string
	fails    chan error
	installs chan flowOutcome
	taken    atomic.Bool
}

// mux serves the manifest page at / and GitHub's redirect at the callback.
func (f *manifestFlow) mux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, f.page)
	})
	mux.HandleFunc("GET "+appCallbackPath, f.callback)
	return mux
}

// callback takes GitHub's redirect once: its state must be this run's and
// it must carry a code; then it waits for the command's outcome and sends
// the browser on to the installation page.
func (f *manifestFlow) callback(w http.ResponseWriter, r *http.Request) {
	if !f.taken.CompareAndSwap(false, true) {
		http.Error(w, "beekeeper took GitHub's redirect already", http.StatusConflict)
		return
	}
	q := r.URL.Query()
	if q.Get("state") != f.state {
		http.Error(w, "beekeeper refused the redirect: its state is not this run's", http.StatusBadRequest)
		f.fails <- refused("the redirect's state is not this run's: nothing written")
		return
	}
	code := q.Get("code")
	if code == "" {
		http.Error(w, "beekeeper refused the redirect: it carries no code", http.StatusBadRequest)
		f.fails <- refused("GitHub's redirect carries no code: nothing written")
		return
	}
	f.codes <- code
	select {
	case out := <-f.installs:
		if out.err != nil {
			http.Error(w, "beekeeper could not store the App's credentials: its answer in the terminal says why", http.StatusInternalServerError)
			return
		}
		http.Redirect(w, r, out.installURL, http.StatusFound)
	case <-r.Context().Done():
	}
}

// githubApp is what GitHub's conversion answers: the App and its
// credentials, which stay in this process.
type githubApp struct {
	ID            int64   `json:"id"`
	Slug          string  `json:"slug"`
	Name          string  `json:"name"`
	ClientID      string  `json:"client_id"`
	ClientSecret  string  `json:"client_secret"`
	WebhookSecret *string `json:"webhook_secret"`
	PEM           string  `json:"pem"`
	HTMLURL       string  `json:"html_url"`
}

// githubClient is the API client of the flow's own calls, bounded.
var githubClient = &http.Client{Timeout: 30 * time.Second}

// githubRequest is one API call with GitHub's headers; auth is the
// Authorization header, "" for none.
func githubRequest(ctx context.Context, method, path, auth string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, githubAPI+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", project.Name)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	return githubClient.Do(req)
}

// apiError is an error answer's message, bounded, never a body with values.
func apiError(what string, resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var e struct {
		Message string `json:"message"`
	}
	msg := strings.TrimSpace(string(body))
	if json.Unmarshal(body, &e) == nil && e.Message != "" {
		msg = e.Message
	}
	return fmt.Errorf("%s: GitHub answered HTTP %d: %s", what, resp.StatusCode, bounded(msg, phraseRunes))
}

// convertManifest turns the redirect's one-time code into the App and its
// credentials with GitHub's unauthenticated conversion: the code is the
// credential, and a token sent along is one GitHub can answer 404 for. A
// refusal names GitHub's status and message; no error carries the code.
func convertManifest(ctx context.Context, code string) (*githubApp, error) {
	resp, err := githubRequest(ctx, http.MethodPost, "/app-manifests/"+url.PathEscape(code)+"/conversions", "")
	if err != nil {
		// the request's URL carries the code: only the cause travels
		var uerr *url.Error
		if errors.As(err, &uerr) {
			err = uerr.Err
		}
		return nil, fmt.Errorf("the conversion: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		return nil, apiError("the conversion", resp)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("the conversion: %w", err)
	}
	return parseConversion(raw)
}

// parseConversion reads the conversion's answer; its errors carry no part
// of it.
func parseConversion(raw []byte) (*githubApp, error) {
	var app githubApp
	if err := json.Unmarshal(raw, &app); err != nil {
		return nil, errors.New("the conversion: GitHub's answer is no App")
	}
	if app.ID == 0 || app.Slug == "" || app.ClientID == "" || app.ClientSecret == "" || app.PEM == "" {
		return nil, errors.New("the conversion: GitHub's answer lacks the App's id, slug, client id, client secret or private key")
	}
	return &app, nil
}

// appFields are the vault item's fields of a created App: the
// configuration plain, the secrets concealed, the webhook secret only
// when the App has one.
func appFields(app *githubApp) []secret.ItemField {
	fields := []secret.ItemField{
		{Label: appIDField, Value: strconv.FormatInt(app.ID, 10), Plain: true},
		{Label: appClientIDField, Value: app.ClientID, Plain: true},
		{Label: appSlugField, Value: app.Slug, Plain: true},
		{Label: githubClientSecret, Value: app.ClientSecret},
		{Label: githubPrivateKey, Value: app.PEM},
	}
	if app.WebhookSecret != nil && *app.WebhookSecret != "" {
		fields = append(fields, secret.ItemField{Label: appWebhookSecret, Value: *app.WebhookSecret})
	}
	return fields
}

// storeFields writes fields into the vault item in one call: with
// secret.session through the broker's session over the keeper's socket,
// else as the service account in this process.
func (a *app) storeFields(ctx context.Context, item string, fields []secret.ItemField) ([]secret.Stored, error) {
	if !a.cfg.Secret.Session {
		vault, title, err := secret.ParseItem(item)
		if err != nil {
			return nil, err
		}
		ops, err := a.secretOpsKeyed()
		if err != nil {
			return nil, err
		}
		out, err := ops.StoreFields(ctx, vault, title, fields)
		return out, vaultExit(err)
	}
	path, err := secret.SocketPath()
	if err != nil {
		return nil, err
	}
	st, err := secret.AskVault(path, secret.VaultRequest{Op: secret.VaultStoreItem, Item: item, Fields: fields})
	if err != nil {
		return nil, &exitError{code: ExitVault, msg: err.Error()}
	}
	if len(st.Fields) == 0 {
		return nil, &exitError{code: ExitVault, msg: "the vault keeper answered no store"}
	}
	return st.Fields, nil
}

// installURL is the App's installation page with the organisation and the
// repositories preselected (all of them without ids).
func installURL(slug string, orgID int64, repoIDs []int64) string {
	q := url.Values{"suggested_target_id": {strconv.FormatInt(orgID, 10)}}
	for _, id := range repoIDs {
		q.Add("repository_ids[]", strconv.FormatInt(id, 10))
	}
	return githubWeb + "/apps/" + url.PathEscape(slug) + "/installations/new/permissions?" + q.Encode()
}

// installation is what GitHub says of one installation of the App.
type installation struct {
	ID                  int64  `json:"id"`
	RepositorySelection string `json:"repository_selection"`
	Account             struct {
		Login string `json:"login"`
	} `json:"account"`
}

// waitInstallation waits for the App's installation on the organisation,
// looking every appInstallPoll as the App itself, with a token signed by
// the private key the conversion answered, until wait has passed.
func waitInstallation(ctx context.Context, app *githubApp, org string, wait time.Duration) (*installation, error) {
	key, err := parseAppKey(app.PEM)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(wait)
	for {
		insts, err := appInstallations(ctx, app.ID, key)
		if err == nil {
			for _, in := range insts {
				if strings.EqualFold(in.Account.Login, org) {
					return &in, nil
				}
			}
			err = errors.New("the Install click did not come")
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("no installation on %s within %s (%v)", org, wait, err)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(appInstallPoll):
		}
	}
}

// appInstallations lists the App's installations as the App.
func appInstallations(ctx context.Context, appID int64, key *rsa.PrivateKey) ([]installation, error) {
	token, err := appJWT(appID, key, time.Now())
	if err != nil {
		return nil, err
	}
	resp, err := githubRequest(ctx, http.MethodGet, "/app/installations", "Bearer "+token)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, apiError("the installations", resp)
	}
	var insts []installation
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&insts); err != nil {
		return nil, errors.New("the installations: GitHub's answer is no list")
	}
	return insts, nil
}

// parseAppKey reads the App's private key, as GitHub answers it (PKCS #1)
// or as PKCS #8.
func parseAppKey(pemText string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(pemText))
	if block == nil {
		return nil, errors.New("the App's private key is no PEM block")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, errors.New("the App's private key is no RSA key")
	}
	key, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("the App's private key is no RSA key")
	}
	return key, nil
}

// appJWT is the App's own token: RS256, issued a minute ago against clock
// drift, good for nine minutes.
func appJWT(appID int64, key *rsa.PrivateKey, now time.Time) (string, error) {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, err := json.Marshal(map[string]any{"iat": now.Add(-time.Minute).Unix(), "exp": now.Add(9 * time.Minute).Unix(), "iss": strconv.FormatInt(appID, 10)})
	if err != nil {
		return "", err
	}
	signing := header + "." + base64.RawURLEncoding.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}
