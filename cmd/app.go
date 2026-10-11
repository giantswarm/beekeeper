package cmd

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/config"
	"github.com/giantswarm/beekeeper/internal/github"
	"github.com/giantswarm/beekeeper/internal/guard"
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

An App on record is one the organisation declares and the person owns and
asked a grant for, on their word (a note's answer): a consent, or one change
of its permissions. allow reads the declaration: its name must be the App's;
an App no manifest declares is not recorded. For a consent (--client-id) the
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
there, naming this record's form. A sign-in or consent page of the lab's
own identity provider on a loopback host (*.127.0.0.1.nip.io, localhost) is
allowed without a record: a fixture user's sign-in there is the person's
own lab's. Every other grant stays refused. A browse turn whose steps name
an App on record runs with the matching auto mode rules (beekeeper browse
--help).

Without a subcommand, lists the Apps on record.`,
		RunE: func(*cobra.Command, []string) error { return a.appList() },
	}
	var rec state.App
	var permission string
	allow := &cobra.Command{
		Use:   "allow <name>",
		Short: "Record an App the organisation declares and the person asked a consent, or one permission change, for, on their word",
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

Either or both, with --word, the person's words that asked for it.`,
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			rec.Name = strings.TrimSpace(args[0])
			rec.ClientID, rec.Word = strings.TrimSpace(rec.ClientID), strings.TrimSpace(rec.Word)
			var err error
			switch {
			case rec.Name == "":
				return usageErr("an App is recorded by its name")
			case rec.Word == "":
				return usageErr("--word: the person's words that asked for the grant")
			}
			if rec.Permission, err = permissionChange(permission); err != nil {
				return err
			}
			if rec.ClientID == "" && rec.Permission == "" {
				return usageErr("--client-id <id> for a consent (the OAuth client id the App's consent URL carries), --permission \"<scope>: <level>\" for one permission change, or both")
			}
			if err := a.declared(c.Context(), &rec); err != nil {
				return err
			}
			if rec.ClientID != "" && len(rec.Callbacks) == 0 {
				return refused("App %s has no consent page to record: %s declares no callback URL (an App used through the device flow has none); "+
					"a change of its permissions is recorded with --permission \"<scope>: <level>\"", rec.Name, rec.Declared)
			}
			if rec.ClientID == "" {
				rec.Callbacks = nil
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
			if len(rec.Callbacks) > 0 {
				answers = append(answers, "its consent page with callback "+strings.Join(rec.Callbacks, " or "))
			}
			if rec.Permission != "" {
				answers = append(answers, "the change "+rec.Permission+" on its permissions page and its installation's Review request and Accept new permissions")
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
	allow.Flags().StringVar(&rec.Word, "word", "", "the person's words that asked for the grant")
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
	var title, words string
	var navigate bool
	check := &cobra.Command{
		Use:   "check <url>",
		Short: "The hook's decision for a click on a page, or a navigation to it, dry: allow, refuse or no decision, and why",
		Long: `check prints what the hook answers a Chrome click on the page at <url>
from the Apps on record, without a session or a click: "allow:", "refuse:"
or "no decision:" and the reason. --title is the page's title, which GitHub's
consent page carries the App's name in ("Authorize <name>"): it decides a
URL whose client id the Chrome tools redacted (client_id=REDACTED), as the
tool results show it. --words are the call's own words (the summary the
model wrote for it, a typed text, a form value), which decide a call on an
App's permissions page; --navigate asks for a navigation to <url> instead
of a click on it.`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			st, err := a.store.Read()
			if err != nil {
				return err
			}
			var d guard.Decision
			if navigate {
				d = guard.Navigation(guardApps(st.Apps), args[0])
			} else {
				d = guard.Click(guardApps(st.Apps), guard.Page{URL: args[0], Title: title}, words)
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
	check.Flags().StringVar(&words, "words", "", "the call's own words (its summary, a typed text, a form value)")
	check.Flags().BoolVar(&navigate, "navigate", false, "the decision for a navigation to <url> instead of a click on it")
	c.AddCommand(a.appCreateCmd(), allow, listCmd("List the Apps on record", a.appList), revoke, check)
	return c
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
	if x.ClientID != "" {
		parts = append(parts, "client id "+x.ClientID, "callbacks "+strings.Join(x.Callbacks, " "))
	}
	if x.Permission != "" {
		parts = append(parts, "permission "+x.Permission)
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
		_, err = fmt.Fprintln(a.out, "no App on record: every consent page is refused")
		return err
	}
	for _, x := range st.Apps {
		parts := append([]string{x.Name}, recordParts(x)...)
		if _, err := fmt.Fprintf(a.out, "%s  since %s by %s: %s\n", strings.Join(parts, "  "), x.At.Local().Format("2006-01-02 15:04"), x.By.Name, x.Word); err != nil {
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
	return guardApps(st.Apps)
}

// guardApps are the Apps on record as the guard reads them.
func guardApps(apps []state.App) []guard.App {
	out := make([]guard.App, 0, len(apps))
	for _, x := range apps {
		out = append(out, guard.App{Name: x.Name, ClientID: x.ClientID, Callbacks: x.Callbacks, Org: x.Org, Permission: x.Permission,
			Declared: x.Declared, Word: x.Word, By: x.By.Name, At: x.At})
	}
	return out
}
