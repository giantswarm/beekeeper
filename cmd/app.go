package cmd

import (
	"fmt"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/guard"
	"github.com/giantswarm/beekeeper/internal/state"
)

// The events of the Apps on record.
const (
	appAllowed = "app.allow"
	appRevoked = "app.revoke"
)

func (a *app) appCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "app",
		Short: "The Apps the person owns and asked a consent for: what the hook answers a consent click for",
		Long: `An App on record is one the person owns and asked a consent for, on their
word: its name as GitHub's consent page titles it ("Authorize <name>"), the
OAuth client id its consent URL carries, the callback host the page's
redirect must name, and the person's words (a note's answer). With it,
beekeeper hook pretooluse answers a Chrome click on GitHub's consent page
itself: allowed on the recorded App's page with the recorded callback host
(the client id, or the name in GitHub's title where the tool redacts the
id), refused on GitHub's consent page of any other App or callback host,
naming the page and this record's form. A sign-in or consent page of the
lab's own identity provider on a loopback host (*.127.0.0.1.nip.io,
localhost) is allowed without a record: a fixture user's sign-in there is
the person's own lab's. Every other grant stays refused. A browse turn whose
steps name an App on record runs with the matching auto mode rule
(beekeeper browse --help).

Without a subcommand, lists the Apps on record.`,
		RunE: func(*cobra.Command, []string) error { return a.appList() },
	}
	var rec state.App
	allow := &cobra.Command{
		Use:   "allow <name>",
		Short: "Record an App the person owns and asked a consent for, on their word",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			rec.Name = strings.TrimSpace(args[0])
			rec.ClientID, rec.Callback, rec.Word = strings.TrimSpace(rec.ClientID), strings.ToLower(strings.TrimSpace(rec.Callback)), strings.TrimSpace(rec.Word)
			switch {
			case rec.Name == "":
				return usageErr("an App is recorded by its name")
			case rec.ClientID == "":
				return usageErr("--client-id: the OAuth client id the App's consent URL carries")
			case rec.Callback == "" || strings.Contains(rec.Callback, "/"):
				return usageErr("--callback: the host the consent page's redirect must name (a host, with its port when it has one; no scheme or path)")
			case rec.Word == "":
				return usageErr("--word: the person's words that asked for the consent")
			}
			me, err := a.caller()
			if err != nil {
				return err
			}
			rec.By, rec.At = me, a.now.UTC()
			err = a.store.Update(func(st *state.State) ([]state.Event, error) {
				st.Apps = slices.DeleteFunc(st.Apps, func(x state.App) bool { return strings.EqualFold(x.Name, rec.Name) })
				st.Apps = append(st.Apps, rec)
				return []state.Event{event(me, appAllowed, "%s: client id %s, callback %s: %s", rec.Name, rec.ClientID, rec.Callback, rec.Word)}, nil
			})
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(a.out, "App %s on record: its consent page with callback %s is answered by the hook\n", rec.Name, rec.Callback)
			return err
		},
	}
	allow.Flags().StringVar(&rec.ClientID, "client-id", "", "the OAuth client id the App's consent URL carries")
	allow.Flags().StringVar(&rec.Callback, "callback", "", "the host the consent page's redirect must name")
	allow.Flags().StringVar(&rec.Word, "word", "", "the person's words that asked for the consent")
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
				return []state.Event{event(me, appRevoked, "%s: client id %s, callback %s", r.Name, r.ClientID, r.Callback)}, nil
			})
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(a.out, "App %s off the record\n", name)
			return err
		},
	}
	var title string
	check := &cobra.Command{
		Use:   "check <url>",
		Short: "The hook's decision for a click on a page, dry: allow, refuse or no decision, and why",
		Long: `check prints what the hook answers a Chrome click on the page at <url>
from the Apps on record, without a session or a click: "allow:", "refuse:"
or "no decision:" and the reason. --title is the page's title, which GitHub's
consent page carries the App's name in ("Authorize <name>"): it decides a
URL whose client id the Chrome tools redacted (client_id=REDACTED), as the
tool results show it.`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			st, err := a.store.Read()
			if err != nil {
				return err
			}
			d := guard.Consent(guardApps(st.Apps), guard.Page{URL: args[0], Title: title})
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
	c.AddCommand(allow, listCmd("List the Apps on record", a.appList), revoke, check)
	return c
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
		if _, err := fmt.Fprintf(a.out, "%s  client id %s  callback %s  since %s by %s: %s\n", x.Name, x.ClientID, x.Callback, x.At.Local().Format("2006-01-02 15:04"), x.By.Name, x.Word); err != nil {
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
		out = append(out, guard.App{Name: x.Name, ClientID: x.ClientID, Callback: x.Callback, Word: x.Word, By: x.By.Name, At: x.At})
	}
	return out
}
