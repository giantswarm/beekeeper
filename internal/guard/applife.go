package guard

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// An App's creation, installation and deletion are grants too: GitHub's
// manifest page's Create GitHub App, the App's installation page's Install
// and its confirmation, and the Advanced page's Delete GitHub App and its
// confirmation. The hook answers them for the person's own Apps: the Create
// click while a `beekeeper app create` runs for the App (Run: its loopback
// listener answers), the Install click for an App that run creates or
// beekeeper's ledger holds (Created), and the deletion of an App the person
// asked to delete (Delete) or of one the ledger holds whose credentials were
// stored nowhere. Each is refused on any other App's page; every other call
// there is left to the classifier.

// CreateRun is a running `beekeeper app create` of an App: its loopback listener,
// who runs it and since when.
type CreateRun struct {
	Listener, By string
	At           time.Time
}

// Created is beekeeper's ledger entry of an App its app create created: the
// slug GitHub gave it, its id, where its credentials went ("" for nowhere),
// and when.
type Created struct {
	Slug   string
	ID     int64
	Stored string
	At     time.Time
}

// CreateForm is how a creation is run, DeleteForm how a deletion the person
// asked for is recorded.
const (
	CreateForm = "`beekeeper app create <name>` or `beekeeper app create <installation> --manifest <file>`"
	DeleteForm = "`beekeeper app allow <name> --delete --word \"<the person's words>\"`"
)

// GitHub's pages of an App's life: the manifest page a manifest is posted
// to (the person's or the organisation's), the App's installation page, and
// its Advanced settings page (under the organisation or the person's own
// account).
var (
	manifestPath = regexp.MustCompile(`^/(?:organizations/([^/]+)/)?settings/apps/(?:new|manifest)/?$`)
	installPath  = regexp.MustCompile(`^/apps/([^/]+)/installations/new(?:/permissions)?/?$`)
	advancedPath = regexp.MustCompile(`^/(?:organizations/([^/]+)/)?settings/apps/([^/]+)/advanced/?$`)
)

// lifecycleClick decides a call on GitHub's page u that creates, installs
// or deletes an App, in the call's own words; ok is false for any other
// page or call.
func lifecycleClick(apps []App, u *url.URL, title, words string) (Decision, bool) {
	if m := manifestPath.FindStringSubmatch(u.Path); m != nil && createsApp(words) {
		return createClick(apps, m[1], title, words), true
	}
	if m := installPath.FindStringSubmatch(u.Path); m != nil && containsFold(words, "install") {
		return installClick(apps, m[1]), true
	}
	if m := advancedPath.FindStringSubmatch(u.Path); m != nil && !containsFold(words, "transfer") {
		if d, ok := deleteClick(apps, m[1], m[2], words); ok {
			return d, true
		}
	}
	return Decision{}, false
}

// createsApp reports whether a call's words name the manifest page's
// Create GitHub App, or the submit of its form.
func createsApp(words string) bool {
	return containsFold(words, "create") || containsFold(words, "submit")
}

// createClick decides the Create click on the manifest page, under org's
// settings ("" for the person's): allowed for the App a running app create
// creates whose organisation the page's path or the call's words name, or
// whose name the page's title or the call's words name; refused otherwise.
func createClick(apps []App, org, title, words string) Decision {
	var running []*App
	for i := range apps {
		if a := &apps[i]; a.Run != nil {
			running = append(running, a)
			if strings.EqualFold(org, a.Org) || containsFold(words, a.Name) || containsFold(title, a.Name) || containsFold(words, a.Org) {
				return Decision{Allow, "beekeeper allows the click: GitHub's manifest page of " + runWords(a)}
			}
		}
	}
	which := "no `beekeeper app create` runs"
	if len(running) > 0 {
		which = "the page names none of the Apps the running `beekeeper app create` commands create"
	}
	return Decision{Deny, fmt.Sprintf("Refused: GitHub's manifest page creates an App, and %s. A creation is a grant, answered only for the "+
		"person's own App while %s runs for it; every other grant stays refused.", which, CreateForm)}
}

// installClick decides the Install click, or its confirmation, on the
// installation page of the App slug: allowed for an App a running app
// create creates or beekeeper's ledger holds, refused for any other.
func installClick(apps []App, slug string) Decision {
	for i := range apps {
		a := &apps[i]
		if !strings.EqualFold(a.slug(), slug) {
			continue
		}
		switch {
		case a.Created != nil:
			return Decision{Allow, "beekeeper allows the click: the installation page of " + createdWords(a)}
		case a.Run != nil:
			return Decision{Allow, "beekeeper allows the click: the installation page of " + runWords(a)}
		}
	}
	return Decision{Deny, fmt.Sprintf("Refused: the installation page of the App %s, which beekeeper neither creates nor created. An installation is "+
		"a grant, answered only for the person's own App that %s creates or created; every other grant stays refused.", slug, CreateForm)}
}

// deleteClick decides a call on the Advanced page of the App slug, under
// org's settings ("" for the person's own account): the Delete GitHub App
// click, its confirmation and the App's name typed into it are allowed for
// an App deletable names, refused for any other; ok is false for a call
// that names neither the deletion nor the App.
func deleteClick(apps []App, org, slug, words string) (Decision, bool) {
	a := deletable(apps, org, slug)
	names := containsFold(words, slug) || a != nil && containsFold(words, a.Name)
	if !containsFold(words, "delete") && !names {
		return Decision{}, false
	}
	if a != nil {
		return Decision{Allow, "beekeeper allows the call: the deletion of " + deleteWords(a)}, true
	}
	return Decision{Deny, fmt.Sprintf("Refused: the deletion of the App %s is on no record: a deletion is answered only for the person's own App "+
		"that they asked to delete, which %s records on their word, or one beekeeper's app create created and stored no credentials of; "+
		"every other deletion stays refused.", slug, DeleteForm)}, true
}

// deletable is the App whose Advanced page under org's settings ("" for the
// person's own account) slug names and whose deletion is answered: one the
// person asked to delete, or one beekeeper created and stored no
// credentials of; nil for none.
func deletable(apps []App, org, slug string) *App {
	for i := range apps {
		a := &apps[i]
		if !strings.EqualFold(a.slug(), slug) || org != "" && !strings.EqualFold(a.Org, org) {
			continue
		}
		if a.Delete || a.Created != nil && a.Created.Stored == "" {
			return a
		}
	}
	return nil
}

// slug is the App's slug: the one GitHub gave it where the ledger knows
// it, else the one its name gets.
func (a *App) slug() string {
	if a.Created != nil && a.Created.Slug != "" {
		return a.Created.Slug
	}
	return AppSlug(a.Name)
}

// runWords name an App's running creation: the App, where it is declared,
// the run and its listener.
func runWords(a *App) string {
	return fmt.Sprintf("the person's own App %s of %s, declared in %s, which the `beekeeper app create` run by %s since %s creates (its listener %s up)",
		a.Name, a.Org, a.Declared, a.Run.By, a.Run.At.UTC().Format(time.DateTime), a.Run.Listener)
}

// createdWords name an App in beekeeper's ledger.
func createdWords(a *App) string {
	return fmt.Sprintf("the person's own App %s of %s (id %d), declared in %s, which `beekeeper app create` created on %s, its credentials %s",
		a.Name, a.Org, a.Created.ID, a.Declared, a.Created.At.UTC().Format(time.DateOnly), storedWords(a.Created.Stored))
}

// deleteWords name the record of a deletion: the person's word, or the
// ledger's App whose credentials were stored nowhere.
func deleteWords(a *App) string {
	if a.Delete {
		return fmt.Sprintf("the person's own App %s, declared in %s, on record for its deletion since %s by %s: %s",
			a.Name, a.Declared, a.At.UTC().Format(time.DateOnly), a.By, a.Word)
	}
	return createdWords(a)
}

// storedWords say where a created App's credentials went.
func storedWords(stored string) string {
	if stored == "" {
		return "stored nowhere"
	}
	return "in " + stored
}
