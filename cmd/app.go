package cmd

import (
	"context"
	"crypto/subtle"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/guard"
	"github.com/giantswarm/beekeeper/internal/proc"
	"github.com/giantswarm/beekeeper/internal/secret"
	"github.com/giantswarm/beekeeper/internal/state"
)

// The events of the Apps on record.
const (
	appAllowed = "app.allow"
	appRevoked = "app.revoke"
)

// appsGH reads an App's declaration; a test stubs it.
var appsGH github.GH = github.RunGH

func (a *app) appCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "app",
		Short: "The organisation's GitHub Apps: create one from its declared manifest; record the ones the person asked a consent for",
		Long: `The organisation declares its GitHub Apps in the config's apps.repo, one
manifest each at apps.manifest ({name} for the App's name, e.g.
apps/{name}/manifest.json), read from the default branch.

create <name> creates the declared App from its manifest through GitHub's
manifest flow, with the person's two clicks (Create, Install), and writes
every credential the flow answers into the shared vault item <name>, where
no person copies a value from a page (app create --help).

installation <installation> --app <id> reads, as the App itself, an
installation's repository selection and suspension and the App's optional
features, suspends it, and applies a declared repository selection, which
GitHub's settings pages keep behind sudo mode (app installation --help).

An App on record is one the organisation declares and the person owns and
asked a grant for, on their word (a note's answer): a consent, one change
of its permissions, or one callback URL added to its settings. allow reads the declaration: its name must be the App's;
an App no manifest declares is not recorded. An App on the person's own
GitHub account, which apps.repo does not declare, is one app create
--manifest created: beekeeper's ledger holds it, its local manifest declares
it, and the App itself, signed with the key beekeeper stored, answers that
its owner is the person's account (the one gh is signed in as) and its
client id; such an App is recorded for a consent or its deletion. For a consent (--client-id) the
record is the App's name as GitHub's consent page titles it ("Authorize
<name>"), the OAuth client id its consent URL carries and the callback hosts
of its manifest's callback_urls; with it, beekeeper hook pretooluse answers a
Chrome click on GitHub's consent page itself: allowed on the recorded App's
page with one of its declared callback hosts (the client id, or the name in
GitHub's title where the tool redacts the id), refused on GitHub's consent
page of any other App or callback host, naming the page and this record's
form. For a permission change (--permission "<scope>: <level>", an App
without callback URLs too, one used through the device flow) the hook
answers the navigation to the App's permissions page under the
organisation's settings, the clicks there that select that level for that
permission and Save changes, and the installation's Review request and
Accept new permissions: allowed for the App and the change on record,
refused on the permissions page of any other App and for any other call
there, naming this record's form. For a callback URL (--callback-url <url>,
on a callback host the manifest declares) the hook answers the navigation
to the App's settings page under the organisation's settings, the clicks
there that name Add Callback URL, that URL or Save changes, and typing
exactly that URL, refusing another URL or field, a removal, a key or other
input there. A sign-in or consent page of the lab's
own identity provider on a loopback host (*.127.0.0.<n>.nip.io, localhost) is
allowed without a record: a fixture user's sign-in there is the person's
own lab's. A Google OAuth client the person owns and asked a consent for is
recorded with --google --callback <host> --account <address>, on the
person's word alone: on Google's sign-in (accounts.google.com) the hook
follows the tab's flow to the callback host of its authorization request
(the account chooser's redirect_uri; the pages after it carry only a
redacted client id), and answers the clicks of that flow (the recorded
account and no other, "Sign in to <app>"'s Continue, the consent of scopes) for a recorded callback host, refusing
Google's consent pages of a flow for any other host, a third party's. A
self-hosted GitLab's OAuth application the person owns and asked a consent
for is recorded with --gitlab <host> --client-id <uid> --callback <host>
--declared <ref>: on that GitLab's authorize page the hook answers the
consent of that application for a recorded callback host and refuses any
other application's there. A
consent page on any other host (a path or title that names a
consent or a grant, or an OAuth authorization page where the call's own
words name one) is refused, not left to the browse turn's classifier. An App's creation, installation and deletion are grants too: the
hook allows GitHub's "Create GitHub App" on the manifest page while a
create runs for the App (its loopback listener answers), Install and its
confirmation on the installation page of an App a create runs for or
created (beekeeper's ledger), and the Advanced page's Delete GitHub App and
its confirmation for an App on record with --delete or one the ledger holds
no stored credentials of; the same clicks on any other App's page are
refused, naming the form. Every other grant stays refused. A browse turn whose steps name
an App on record runs with the matching auto mode rules (beekeeper browse
--help).

Without a subcommand, lists the Apps on record.`,
		RunE: func(*cobra.Command, []string) error { return a.appList() },
	}
	var rec state.App
	var permission, manifest, declaredRef string
	var hosts []string
	allow := &cobra.Command{
		Use:   "allow <name>",
		Short: "Record an App the organisation declares and the person asked a grant for (a consent, a permission change, a callback URL, a deletion), on their word",
		Long: `allow records the App <name> the organisation declares, on the person's
word, for the grant the person asked for; a record of the same name is
replaced:

  --client-id <id>: a consent, GitHub's page "Authorize <name>" redirecting
  to one of the manifest's callback_urls. An App without callback URLs,
  one used through the device flow, has no consent page to record.
  --permission "<scope>: <level>": one change of the App's permissions,
  e.g. "Members: Read-only": the navigation to its permissions page under
  the organisation's settings, the clicks there that select that level for
  that permission and Save changes, and the installation's Review request
  and Accept new permissions. A record carries one change.
  --callback-url <url>: one callback URL added to the App's settings, on a
  callback host its manifest's callback_urls declare: the navigation to its
  settings page under the organisation's settings, the clicks there that
  name Add Callback URL, that URL or Save changes, and typing exactly that
  URL. Another URL or field, a removal, a key or other input is refused. A
  record carries one URL, http(s) without user info, as written back.
  --delete: the App's deletion, on its Advanced settings page under the
  organisation's or the person's own account: Delete GitHub App, the
  App's name typed into GitHub's confirmation and its confirm click, for
  an App whose credentials were lost or never taken (beekeeper app create
  refuses a name GitHub already holds). With --manifest <file>, the local
  manifest of app create --manifest declares the App instead of apps.repo.
  An App beekeeper app create created and stored no credentials of is
  deletable without a record (beekeeper's ledger, app list).
  --google --callback <host> --account <address>: a consent on Google's
  sign-in for the person's own Google OAuth client, whose redirect goes to
  <host> (repeatable; a URL's host is taken), signed in with the Google
  account <address>, the only one the hook answers on Google's account
  chooser. No repository declares such a client: the record is the
  person's word alone, kept in the machine's state. --client-id, when
  given, must match where Google's URL carries it unredacted.
  --gitlab <host> --client-id <uid> --callback <host> --declared <ref>: a
  consent on a self-hosted GitLab's authorize page (<host>/oauth/authorize,
  "Authorize <name>") for the person's own OAuth application with that
  client id, whose redirect goes to a --callback host (repeatable). GitLab
  names an instance-wide application's owner to its administrators alone:
  the person's own declaration of the client id (a vault field or a SOPS
  file's path, as beekeeper secret names one) must carry it, compared in
  this process, no value in any output. The record is kept in the
  machine's state.

An App apps.repo does not declare that app create --manifest created on the
person's own account (beekeeper's ledger, app list) is recorded for a
consent or a deletion: its manifest is the local one the ledger names, or
--manifest <file>; GitHub's GET /app, signed with the key app create stored,
must answer the person's account (gh api user) as its owner, the ledger's
id and, with --client-id, that client id. Refused otherwise, naming where
it looked.

Any of them, with --word, the person's words that asked for it.`,
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			rec.Name = strings.TrimSpace(args[0])
			rec.ClientID, rec.Word, rec.CallbackURL = strings.TrimSpace(rec.ClientID), strings.TrimSpace(rec.Word), strings.TrimSpace(rec.CallbackURL)
			var err error
			switch {
			case rec.Name == "":
				return usageErr("an App is recorded by its name")
			case rec.Word == "":
				return usageErr("--word: the person's words that asked for the grant")
			}
			rec.GitLab = strings.ToLower(strings.TrimSpace(rec.GitLab))
			switch {
			case rec.Google && rec.GitLab != "":
				return usageErr("--google or --gitlab: a record is of one provider's consent")
			case declaredRef != "" && rec.GitLab == "":
				return usageErr("--declared names the person's own declaration of a GitLab application's client id, with --gitlab")
			}
			switch {
			case rec.Google:
				err = googleClient(&rec, hosts, permission, manifest)
			case rec.GitLab != "":
				err = a.gitlabApp(c.Context(), &rec, hosts, permission, manifest, declaredRef)
			default:
				err = a.githubApp(c.Context(), &rec, hosts, permission, manifest)
			}
			if err != nil {
				return err
			}
			me, err := a.caller()
			if err != nil {
				return err
			}
			rec.By, rec.At = me, a.now.UTC()
			err = a.store.Update(func(st *state.State) ([]state.Event, error) {
				st.Apps = slices.DeleteFunc(st.Apps, func(x state.App) bool { return strings.EqualFold(x.Name, rec.Name) })
				st.Apps = append(st.Apps, rec)
				return []state.Event{event(me, appAllowed, "%s: %s: %s", rec.Name, strings.Join(recordParts(rec), ", "), rec.Word)}, nil
			})
			if err != nil {
				return err
			}
			var answers []string
			switch {
			case rec.Google:
				answers = append(answers, "its consent on Google's sign-in with callback "+strings.Join(rec.Callbacks, " or ")+" as "+rec.Account)
			case rec.GitLab != "":
				answers = append(answers, "its consent on "+rec.GitLab+"'s authorize page with callback "+strings.Join(rec.Callbacks, " or "))
			case len(rec.Callbacks) > 0:
				answers = append(answers, "its consent page with callback "+strings.Join(rec.Callbacks, " or "))
			}
			if rec.Permission != "" {
				answers = append(answers, "the change "+rec.Permission+" on its permissions page and its installation's Review request and Accept new permissions")
			}
			if rec.CallbackURL != "" {
				answers = append(answers, "the callback URL "+rec.CallbackURL+" added on its settings page")
			}
			if rec.Delete {
				answers = append(answers, "its deletion on its Advanced page under the organisation's or the person's own account")
			}
			verb := "is"
			if len(answers) > 1 {
				verb = "are"
			}
			_, err = fmt.Fprintf(a.out, "App %s on record, declared in %s: %s %s answered by the hook\n", rec.Name, rec.Declared, strings.Join(answers, " and "), verb)
			return err
		},
	}
	allow.Flags().StringVar(&rec.ClientID, "client-id", "", "a consent: the OAuth client id the App's consent URL carries")
	allow.Flags().StringVar(&permission, "permission", "", "one change of the App's permissions, \"<scope>: <level>\" (e.g. \"Members: Read-only\")")
	allow.Flags().StringVar(&rec.CallbackURL, "callback-url", "", "one callback URL to add on the App's settings page, on a callback host its manifest declares")
	allow.Flags().BoolVar(&rec.Delete, "delete", false, "the App's deletion: its Advanced page's Delete GitHub App and GitHub's confirmation")
	allow.Flags().StringVar(&manifest, "manifest", "", "the local App manifest (app create --manifest) that declares the App: with --delete alone, or of an App on the person's own account")
	allow.Flags().StringVar(&rec.Word, "word", "", "the person's words that asked for the grant")
	allow.Flags().BoolVar(&rec.Google, "google", false, "a consent on Google's sign-in: the person's own Google OAuth client, by --callback")
	allow.Flags().StringArrayVar(&hosts, "callback", nil, "with --google or --gitlab, a callback host (or URL) of the client's redirect (repeatable)")
	allow.Flags().StringVar(&rec.GitLab, "gitlab", "", "a consent on a self-hosted GitLab's authorize page: the GitLab's host, with --client-id, --callback and --declared")
	allow.Flags().StringVar(&declaredRef, "declared", "", "with --gitlab, the person's own declaration of the application's client id: op://<vault>/<item>/<field> or <SOPS file>#<path>")
	allow.Flags().StringVar(&rec.Account, "account", "", "with --google, the Google account the sign-in chooses on Google's account chooser")
	revoke := &cobra.Command{
		Use:   "revoke <name>",
		Short: "Take an App off the record: its consent page is refused again",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			name := strings.TrimSpace(args[0])
			me, err := a.caller()
			if err != nil {
				return err
			}
			err = a.store.Update(func(st *state.State) ([]state.Event, error) {
				i := slices.IndexFunc(st.Apps, func(x state.App) bool { return strings.EqualFold(x.Name, name) })
				if i < 0 {
					return nil, refused("no App %s is on record", name)
				}
				r := st.Apps[i]
				st.Apps = slices.Delete(st.Apps, i, i+1)
				return []state.Event{event(me, appRevoked, "%s: %s", r.Name, strings.Join(recordParts(r), ", "))}, nil
			})
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(a.out, "App %s off the record\n", name)
			return err
		},
	}
	var title, text, words string
	var navigate bool
	check := &cobra.Command{
		Use:   "check <url>",
		Short: "The hook's decision for a click on a page, or a navigation to it, dry: allow, refuse or no decision, and why",
		Long: `check prints what the hook answers a Chrome click on the page at <url>
from the Apps on record, without a session or a click: "allow:", "refuse:"
or "no decision:" and the reason. --title is the page's title, which GitHub's
consent page carries the App's name in ("Authorize <name>"): it decides a
URL whose client id the Chrome tools redacted (client_id=REDACTED), as the
tool results show it; --text is what the Chrome tools read from the page
(its text, its accessibility tree, a find's match), which carries a GitLab
application's name ("Authorize <name>") where its title is "GitLab".
--words are the call's own words (the summary the
model wrote for it, a typed text, a form value), which decide a call on an
App's permissions page and an App's creation, installation and deletion
pages; --navigate asks for a navigation to <url> instead of a click on it.

Besides the records of app allow, the hook reads the running app create
commands, whose loopback listener answers (GitHub's manifest page:
"Create GitHub App" for the App being created; the App's installation
page: Install and its confirmation), and beekeeper's ledger of the Apps
app create created (their installation page; the deletion of one whose
credentials were stored nowhere). check decides from the same.`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			st, err := a.store.Read()
			if err != nil {
				return err
			}
			apps := guardAppsOf(st)
			var d guard.Decision
			if navigate {
				d = guard.Navigation(apps, args[0])
			} else {
				d = guard.Click(apps, guard.Page{URL: args[0], Title: title, Text: text}, words)
			}
			line := "no decision: not a page the hook decides: the classifier decides it as before"
			switch d.Decision {
			case guard.Allow:
				line = "allow: " + d.Reason
			case guard.Deny:
				line = "refuse: " + d.Reason
			}
			_, err = fmt.Fprintln(a.out, line)
			return err
		},
	}
	check.Flags().StringVar(&title, "title", "", "the page's title (GitHub's consent page: \"Authorize <name>\")")
	check.Flags().StringVar(&text, "text", "", "what the Chrome tools read from the page (a GitLab authorize page: \"Authorize <name>\")")
	check.Flags().StringVar(&words, "words", "", "the call's own words (its summary, a typed text, a form value)")
	check.Flags().BoolVar(&navigate, "navigate", false, "the decision for a navigation to <url> instead of a click on it")
	c.AddCommand(a.appCreateCmd(), a.appInstallationCmd(), allow, listCmd("List the Apps on record", a.appList), revoke, check)
	return c
}

// githubApp fills rec, a GitHub App's record, from its flags and its
// declaration: a consent, a permission change or a deletion.
func (a *app) githubApp(ctx context.Context, rec *state.App, hosts []string, permission, manifest string) error {
	var err error
	if len(hosts) > 0 {
		return usageErr("--callback names a Google OAuth client's or a GitLab application's callback hosts, with --google or --gitlab: a GitHub App's are its manifest's")
	}
	if rec.Account != "" {
		return usageErr("--account names the account a Google OAuth client's sign-in chooses, with --google")
	}
	if rec.Permission, err = permissionChange(permission); err != nil {
		return err
	}
	if rec.ClientID == "" && rec.Permission == "" && rec.CallbackURL == "" && !rec.Delete {
		return usageErr("--client-id <id> for a consent (the OAuth client id the App's consent URL carries), --permission \"<scope>: <level>\" for one permission change, " +
			"--callback-url <url> for one callback URL, --delete for the App's deletion, --google --callback <host> for a Google OAuth client's consent, or several")
	}
	switch {
	case manifest != "" && rec.ClientID == "" && rec.Permission == "" && rec.CallbackURL == "":
		err = a.declaredLocally(manifest, rec)
	case manifest != "":
		err = a.declaredPersonal(ctx, rec, manifest, nil)
	default:
		if err = a.declared(ctx, rec); err != nil {
			err = a.declaredPersonal(ctx, rec, "", err)
		}
	}
	if err != nil {
		return err
	}
	if err := declaredCallback(rec); err != nil {
		return err
	}
	if rec.ClientID != "" && len(rec.Callbacks) == 0 {
		return refused("App %s has no consent page to record: %s declares no callback URL (an App used through the device flow has none); "+
			"a change of its permissions is recorded with --permission \"<scope>: <level>\"", rec.Name, rec.Declared)
	}
	if rec.ClientID == "" {
		rec.Callbacks = nil
	}
	return nil
}

// declaredCallback checks rec's callback URL, if any, against its
// declaration: an http(s) URL on one of the callback hosts its manifest's
// callback_urls declare.
func declaredCallback(rec *state.App) error {
	if rec.CallbackURL == "" {
		return nil
	}
	u, err := url.Parse(rec.CallbackURL)
	if err != nil || !guard.CanonicalCallback(rec.CallbackURL) {
		return usageErr("--callback-url names one callback URL, http(s)://<host>/<path> without user info or characters a URL escapes, not %q", rec.CallbackURL)
	}
	if !slices.Contains(rec.Callbacks, strings.ToLower(u.Host)) {
		return refused("App %s declares no callback host %s in %s: a callback URL is recorded on a host the App's manifest declares, "+
			"so the declaration gains it first", rec.Name, u.Host, rec.Declared)
	}
	return nil
}

// googleDeclared is the declaration of a Google OAuth client on record: no
// repository declares one, the person's word does.
const googleDeclared = "the person's word (a Google OAuth client)"

// googleClient fills rec, a Google OAuth client's record, from its flags:
// its consent, by the callback hosts named, signed in with the account
// named.
func googleClient(rec *state.App, hosts []string, permission, manifest string) error {
	if permission != "" || rec.CallbackURL != "" || rec.Delete || manifest != "" {
		return usageErr("--google records a consent on Google's sign-in alone: --permission, --callback-url, --delete and --manifest are a GitHub App's")
	}
	var err error
	if rec.Callbacks, err = callbackHosts("--google", hosts); err != nil {
		return err
	}
	rec.Account = strings.ToLower(strings.TrimSpace(rec.Account))
	if !strings.Contains(rec.Account, "@") || strings.ContainsAny(rec.Account, " /?#") {
		return usageErr("--google needs --account <address>: the Google account the sign-in chooses on Google's account chooser, not %q", rec.Account)
	}
	rec.Declared = googleDeclared
	return nil
}

// callbackHosts reads --callback for kind (--google, --gitlab): the hosts
// named, a URL's host taken, lower-case, each once; at least one.
func callbackHosts(kind string, hosts []string) ([]string, error) {
	var out []string
	for _, h := range hosts {
		h = strings.ToLower(strings.TrimSpace(h))
		if u, err := url.Parse(h); err == nil && u.Host != "" {
			h = u.Host
		}
		if h == "" || strings.ContainsAny(h, "/?# ") {
			return nil, usageErr("--callback names a callback host (or a URL), not %q", h)
		}
		if !slices.Contains(out, h) {
			out = append(out, h)
		}
	}
	if len(out) == 0 {
		return nil, usageErr("%s needs --callback <host>: the host the client's redirect_uri goes to", kind)
	}
	return out, nil
}

// gitlabApp fills rec, a self-hosted GitLab OAuth application's record,
// from its flags: its consent on the GitLab's authorize page, by its client
// id and the callback hosts named. GitLab's API names an instance-wide
// application's owner to no one but its administrators, so ownership is the
// person's own declaration: the value at declared (a vault field, a SOPS
// file's path) must be the client id, compared in this process, no value in
// any output.
func (a *app) gitlabApp(ctx context.Context, rec *state.App, hosts []string, permission, manifest, declared string) error {
	if permission != "" || rec.CallbackURL != "" || rec.Delete || manifest != "" || rec.Account != "" {
		return usageErr("--gitlab records a consent on a GitLab's authorize page alone: --permission, --callback-url, --delete, --manifest and --account are a GitHub App's or a Google client's")
	}
	if u, err := url.Parse(rec.GitLab); err == nil && u.Host != "" {
		rec.GitLab = strings.ToLower(u.Host)
	}
	if strings.ContainsAny(rec.GitLab, "/?#@ ") || !strings.Contains(rec.GitLab, ".") {
		return usageErr("--gitlab names the GitLab's host (or its URL), not %q", rec.GitLab)
	}
	if rec.ClientID == "" {
		return usageErr("--gitlab needs --client-id <uid>: the application's client id its authorize URL carries")
	}
	var err error
	if rec.Callbacks, err = callbackHosts("--gitlab", hosts); err != nil {
		return err
	}
	if declared == "" {
		return usageErr("--gitlab needs --declared <ref>: the person's own declaration of the application's client id (op://<vault>/<item>/<field> or <SOPS file>#<path>)")
	}
	ref, err := secret.ParseRef(declared)
	if err != nil {
		return usageErr("--declared: %v", err)
	}
	if err := a.sandboxFiles([]secret.Ref{ref}, nil); err != nil {
		return err
	}
	ops, err := a.secretOpsKeyed()
	if err != nil {
		return err
	}
	same := false
	if err := ops.Use(ctx, ref, func(v string) error {
		same = subtle.ConstantTimeCompare([]byte(strings.TrimSpace(v)), []byte(rec.ClientID)) == 1
		return nil
	}); err != nil {
		return vaultExit(fmt.Errorf("the declaration %s: %w", ref, err))
	}
	if !same {
		return refused("GitLab application %s is not declared: %s carries another client id than %s", rec.Name, ref, rec.ClientID)
	}
	rec.Declared = ref.String()
	return nil
}

// permissionChange reads --permission: one change as "<scope>: <level>",
// both named, answered as "<scope>: <level>"; "" for none.
func permissionChange(flag string) (string, error) {
	flag = strings.TrimSpace(flag)
	if flag == "" {
		return "", nil
	}
	scope, level, ok := strings.Cut(flag, ":")
	scope, level = strings.TrimSpace(scope), strings.TrimSpace(level)
	if !ok || scope == "" || level == "" {
		return "", usageErr("--permission names one change as \"<scope>: <level>\" (e.g. \"Members: Read-only\"), not %q", flag)
	}
	return scope + ": " + level, nil
}

// recordParts name a record's facts: its client id and callback hosts for
// a consent, its permission change, and where the App is declared.
func recordParts(x state.App) []string {
	var parts []string
	if x.Google {
		parts = append(parts, "Google")
	}
	if x.GitLab != "" {
		parts = append(parts, "GitLab "+x.GitLab)
	}
	if x.ClientID != "" {
		parts = append(parts, "client id "+x.ClientID)
	}
	if len(x.Callbacks) > 0 {
		parts = append(parts, "callbacks "+strings.Join(x.Callbacks, " "))
	}
	if x.Account != "" {
		parts = append(parts, "account "+x.Account)
	}
	if x.Permission != "" {
		parts = append(parts, "permission "+x.Permission)
	}
	if x.CallbackURL != "" {
		parts = append(parts, "callback URL "+x.CallbackURL)
	}
	if x.Delete {
		parts = append(parts, "delete")
	}
	return append(parts, "declared in "+x.Declared)
}

// declared fills rec's organisation, callback hosts and declaration from
// the App's manifest in the organisation's repository, refusing an App it
// does not declare under rec's name.
func (a *app) declared(ctx context.Context, rec *state.App) error {
	path := a.cfg.Apps.ManifestOf(rec.Name)
	if path == "" {
		return refused("no repository declares the organisation's Apps: set apps.repo and apps.manifest (with %s) in the config", config.AppName)
	}
	m, err := github.ReadAppManifest(ctx, appsGH, a.cfg.Apps.Repo, path)
	if err != nil {
		return refused("App %s is not declared: %v", rec.Name, err)
	}
	if !strings.EqualFold(m.Name, rec.Name) {
		return refused("App %s is not declared: %s names the App %s", rec.Name, m.Source, m.Name)
	}
	rec.Callbacks, rec.Declared = m.Callbacks, m.Source
	rec.Org, _, _ = strings.Cut(a.cfg.Apps.Repo, "/")
	return nil
}

// declaredLocally fills rec's organisation and declaration from the local
// manifest file of app create --manifest, refusing one that names another
// App.
func (a *app) declaredLocally(file string, rec *state.App) error {
	_, name, err := readManifest(file)
	if err != nil {
		return usageErr("--manifest: %v", err)
	}
	if !strings.EqualFold(name, rec.Name) {
		return refused("App %s is not declared: %s names the App %s", rec.Name, file, name)
	}
	if rec.Declared, err = filepath.Abs(file); err != nil {
		return err
	}
	rec.Callbacks = nil
	rec.Org, _, _ = strings.Cut(a.cfg.Apps.Repo, "/")
	return nil
}

// declaredPersonal fills rec from an App app create --manifest created on
// the person's own account, which apps.repo does not declare: beekeeper's
// ledger holds it, its local manifest (manifest, else the one the ledger
// names) declares its callback hosts, and the App itself, signed with the
// key beekeeper stored, answers its owner and client id. The owner must be
// the person's GitHub account, the one gh is signed in as. orgErr is why
// apps.repo does not declare it, nil when --manifest named the declaration;
// an App the ledger holds on the organisation's account keeps that refusal.
func (a *app) declaredPersonal(ctx context.Context, rec *state.App, manifest string, orgErr error) error {
	st, err := a.store.Read()
	if err != nil {
		return err
	}
	i := len(st.AppsCreated) - 1
	for i >= 0 && !strings.EqualFold(st.AppsCreated[i].Name, rec.Name) {
		i--
	}
	org, _, _ := strings.Cut(a.cfg.Apps.Repo, "/")
	switch {
	case i < 0 && orgErr != nil:
		return refused("%v; nor does beekeeper's ledger hold an App %s that app create --manifest created on the person's own account (app list)", orgErr, rec.Name)
	case i < 0:
		return refused("App %s is not declared: beekeeper's ledger holds no App of that name that app create --manifest created on the person's own account (app list)", rec.Name)
	}
	created := st.AppsCreated[i]
	if orgErr != nil && strings.EqualFold(created.Org, org) {
		return orgErr
	}
	if rec.Permission != "" || rec.CallbackURL != "" {
		return refused("App %s is on %s's own account: the hook answers a permission change or a callback URL on an organisation's settings pages alone; "+
			"a consent (--client-id) and a deletion (--delete) are recorded for it", rec.Name, created.Org)
	}
	if manifest == "" {
		manifest = created.Declared
	}
	file, err := filepath.Abs(manifest)
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(file) //nolint:gosec // the App's local manifest, from the person or beekeeper's ledger
	if err != nil {
		return refused("App %s is not declared: its manifest %s is unreadable (%v); --manifest names a copy", rec.Name, file, err)
	}
	m, err := github.ParseAppManifest(raw, file)
	if err != nil {
		return refused("App %s is not declared: %v", rec.Name, err)
	}
	if !strings.EqualFold(m.Name, rec.Name) {
		return refused("App %s is not declared: %s names the App %s", rec.Name, file, m.Name)
	}
	self, err := a.appSelf(ctx, created)
	if err != nil {
		return err
	}
	login, err := appsGH(ctx, "api", "user", "--jq", ".login")
	if err != nil {
		return fmt.Errorf("the person's GitHub account (gh api user): %w", err)
	}
	person := strings.TrimSpace(string(login))
	switch {
	case self.Owner.Type != "User" || person == "" || !strings.EqualFold(self.Owner.Login, person):
		return refused("App %s (id %d) belongs to the %s %s, not to the person's own account %s: an App on another account is recorded from apps.repo's declaration",
			rec.Name, self.ID, strings.ToLower(self.Owner.Type), self.Owner.Login, person)
	case rec.ClientID != "" && rec.ClientID != self.ClientID:
		return refused("App %s (id %d) has the client id %s, not %s", rec.Name, self.ID, self.ClientID, rec.ClientID)
	}
	rec.Callbacks, rec.Org = m.Callbacks, self.Owner.Login
	rec.Declared = fmt.Sprintf("%s (App %d on %s's own account)", file, self.ID, self.Owner.Login)
	return nil
}

// selfApp is what GitHub answers an App about itself (GET /app).
type selfApp struct {
	ID       int64  `json:"id"`
	Slug     string `json:"slug"`
	ClientID string `json:"client_id"`
	Owner    struct {
		Login string `json:"login"`
		Type  string `json:"type"`
	} `json:"owner"`
}

// appSelf reads the App the ledger entry created names as the App itself,
// with the private key stored where app create wrote its credentials (a
// SOPS Secret, or a vault item), in this process: no value in any output.
func (a *app) appSelf(ctx context.Context, created state.AppCreated) (*selfApp, error) {
	var ref secret.Ref
	switch {
	case created.Stored == "":
		return nil, refused("App %s (id %d): beekeeper stored no credentials of it, so the App cannot answer who owns it: "+
			"a consent is recorded for an App whose key app create stored", created.Name, created.ID)
	case strings.HasPrefix(created.Stored, guard.OpRef):
		var err error
		if ref, err = secret.ParseRef(created.Stored + "/" + githubPrivateKey); err != nil {
			return nil, err
		}
	default:
		ref = secret.Ref{File: created.Stored, Path: "stringData." + githubPrivateKey}
	}
	if err := a.sandboxFiles([]secret.Ref{ref}, nil); err != nil {
		return nil, err
	}
	ops, err := a.secretOpsKeyed()
	if err != nil {
		return nil, err
	}
	var self selfApp
	err = ops.Use(ctx, ref, func(pem string) error {
		key, err := parseAppKey(pem)
		if err != nil {
			return err
		}
		jwt, err := appJWT(created.ID, key, time.Now())
		if err != nil {
			return err
		}
		return githubJSON(ctx, http.MethodGet, "/app", "Bearer "+jwt, http.StatusOK, &self)
	})
	if err != nil {
		return nil, vaultExit(fmt.Errorf("the App %s (id %d) as itself, with the key in %s: %w", created.Name, created.ID, ref, err))
	}
	if self.ID != created.ID {
		return nil, refused("the key in %s signs for App %d (%s), not %s (id %d)", ref, self.ID, self.Slug, created.Name, created.ID)
	}
	return &self, nil
}

// appList prints the Apps on record, one per line.
func (a *app) appList() error {
	st, err := a.store.Read()
	if err != nil {
		return err
	}
	if a.json {
		return a.printJSON(st.Apps)
	}
	if len(st.Apps) == 0 {
		if _, err := fmt.Fprintln(a.out, "no App on record: every consent page is refused"); err != nil {
			return err
		}
	}
	for _, x := range st.Apps {
		parts := append([]string{x.Name}, recordParts(x)...)
		if _, err := fmt.Fprintf(a.out, "%s  since %s by %s: %s\n", strings.Join(parts, "  "), x.At.Local().Format("2006-01-02 15:04"), x.By.Name, x.Word); err != nil {
			return err
		}
	}
	for _, x := range st.AppsCreated {
		stored := "credentials stored nowhere"
		if x.Stored != "" {
			stored = "credentials in " + x.Stored
		}
		if _, err := fmt.Fprintf(a.out, "created %s  slug %s  id %d  of %s  %s  declared in %s  on %s by %s\n", x.Name, x.Slug, x.ID, x.Org, stored, x.Declared,
			x.At.Local().Format("2006-01-02 15:04"), x.By.Name); err != nil {
			return err
		}
	}
	return nil
}

// allowedApps are the Apps on record, for the hook's consent decision: nil
// when the record cannot be read, which decides no consent page.
func (a *app) allowedApps() []guard.App {
	if a.loadConfig() != nil {
		return nil
	}
	store, err := state.Open(a.cfg.StateDir)
	if err != nil {
		return nil
	}
	st, err := store.Peek()
	if err != nil {
		return nil
	}
	return guardAppsOf(st)
}

// guardApps are the Apps on record as the guard reads them.
func guardApps(apps []state.App) []guard.App {
	out := make([]guard.App, 0, len(apps))
	for _, x := range apps {
		out = append(out, guard.App{Name: x.Name, ClientID: x.ClientID, Callbacks: x.Callbacks, Org: x.Org, Permission: x.Permission,
			CallbackURL: x.CallbackURL, Delete: x.Delete, Google: x.Google, Account: x.Account, GitLab: x.GitLab, Declared: x.Declared, Word: x.Word, By: x.By.Name, At: x.At})
	}
	return out
}

// listenerUp reports whether a running app create's listener answers: its
// process lives and its loopback address takes a connection. A test
// replaces it.
var listenerUp = func(r state.AppRun) bool {
	if !proc.Alive(r.PID) {
		return false
	}
	c, err := net.DialTimeout("tcp", r.Listener, 300*time.Millisecond)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

// guardAppsOf are the Apps the hook decides from, as the guard reads them:
// the records, each with the running app create of the same name whose
// listener answers and beekeeper's latest ledger entry of it; an App that
// only runs or only the ledger holds is one of its own.
func guardAppsOf(st *state.State) []guard.App {
	out := guardApps(st.Apps)
	of := func(name, org, declared string) *guard.App {
		i := slices.IndexFunc(out, func(g guard.App) bool { return strings.EqualFold(g.Name, name) })
		if i < 0 {
			out = append(out, guard.App{Name: name})
			i = len(out) - 1
		}
		g := &out[i]
		if g.Org == "" {
			g.Org = org
		}
		if g.Declared == "" {
			g.Declared = declared
		}
		return g
	}
	for _, r := range st.AppRuns {
		if listenerUp(r) {
			of(r.Name, r.Org, r.Declared).Run = &guard.CreateRun{Listener: r.Listener, By: r.By.Name, At: r.At}
		}
	}
	for _, c := range st.AppsCreated {
		of(c.Name, c.Org, c.Declared).Created = &guard.Created{Slug: c.Slug, ID: c.ID, Stored: c.Stored, At: c.At}
	}
	return out
}
