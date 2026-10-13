package cmd

import (
	"context"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/beekeeper/internal/secret"
)

// app installation reads and sets what GitHub's sudo mode keeps a browse
// turn from: an installation's repository selection and suspension, read
// and set as the App itself with the private key beekeeper holds, and the
// App's optional features, which no API answers but the device flow shows.

// appInstallationOp is the event of an installation read or changed.
const appInstallationOp = "app.installation"

// Device flow probe outcomes, the user token expiry's one answer, and the
// selections GitHub answers.
const (
	deviceFlowEnabled  = "enabled"
	deviceFlowDisabled = "disabled"
	expiryUnread       = "unread: GitHub's API does not carry it; on for an App unless opted out at its creation"
	selectionSelected  = "selected"
	// selectionPermission is what GitHub accepts for a change of an
	// installation's repositories: a fine-grained token's "GitHub App
	// installation repository access", which no App can declare.
	selectionPermission = "installation_repositories=write"
)

// installationOpts are app installation's flags.
type installationOpts struct {
	app, org, token    string
	repos              []string
	apply              bool
	suspend, unsuspend bool
	reinstall          bool
}

// appInstallationResult is what app installation answers: metadata only.
type appInstallationResult struct {
	App                 string   `json:"app"`
	AppID               int64    `json:"appID"`
	ClientID            string   `json:"clientID"`
	Org                 string   `json:"org"`
	InstallationID      int64    `json:"installationID"`
	RepositorySelection string   `json:"repositorySelection"`
	RepositoryCount     int      `json:"repositoryCount"`
	Repositories        []string `json:"repositories,omitempty"`
	SuspendedAt         string   `json:"suspendedAt,omitempty"`
	Suspended           bool     `json:"suspended,omitempty"`
	Unsuspended         bool     `json:"unsuspended,omitempty"`
	Uninstalled         bool     `json:"uninstalled,omitempty"`
	InstallURL          string   `json:"installURL,omitempty"`
	Added               []string `json:"added,omitempty"`
	Removed             []string `json:"removed,omitempty"`
	DeviceFlow          string   `json:"deviceFlow"`
	TokenExpiry         string   `json:"tokenExpiry"`
}

func (a *app) appInstallationCmd() *cobra.Command {
	var o installationOpts
	c := &cobra.Command{
		Use:   "installation <installation> --app <id|slug> [--suspend | --unsuspend | --repos <name>,… (--reinstall | --token <ref>)]",
		Short: "Read an App's installation scope and optional features, suspend it, or apply a declared repository selection, with no person",
		Long: `installation reads, and changes, what GitHub's settings pages keep behind
sudo mode, which no agent passes: an installation's repository selection and
suspension, and the App's optional features. It acts as the App --app, with
the private-key of the Kubernetes Secret apps.secrets names for
<installation>, in beekeeper's own process: no person, no browser, no value
in any output.

  1. The App's token (a JWT signed with its key) reads the App (GET /app)
     and its installation on --org (default: apps.repo's owner).
  2. --suspend suspends the installation, --unsuspend lifts that
     (PUT and DELETE /app/installations/<id>/suspended), both as the App:
     a suspended installation gets no token and reaches nothing.
  3. An installation token, while not suspended, reads the repositories
     the installation reaches (GET /installation/repositories) and is
     revoked at the end. An installation on all repositories answers their
     count; a selected one names them.
  4. With --repos, the selection becomes exactly those repositories of
     --org: each missing one is added, then each other one removed
     (PUT and DELETE /user/installations/<id>/repositories/<repo id>), and
     read back. GitHub takes these only from a token with "GitHub App
     installation repository access" (` + selectionPermission + `), which
     no App can declare: --token names it, a fine-grained token of an
     organisation owner in the shared vault (op://…) or a SOPS file, read
     in this process. Refused in one line naming the setting: an
     installation on all repositories (no API narrows it; its Configure
     page does, behind sudo mode), a suspended one, and no --token.
  5. --reinstall --repos narrows any installation, one on all repositories
     or suspended too, with no token: the repositories are looked up
     first, the installation is deleted as the App
     (DELETE /app/installations/<id>), and the answer is the App's
     installation page for --org with those repositories preselected
     ("Only select repositories"). Its Install click, which a browse turn
     makes (the hook answers it for an App beekeeper's ledger holds), meets
     GitHub's sudo mode, an organisation owner's confirmation; the next
     read shows the new installation's id and repositories. The
     installation's id changes; the App, its key and its permissions do
     not. Until it is installed again the App reaches nothing.
  6. Device flow: GitHub's device code request with the App's client id,
     the code discarded, answers enabled or device_flow_disabled.
  7. User token expiry: no API answers it. GitHub turns it on for every
     App unless opted out at its creation; it shows on a user token's
     refresh only.

stdout is the App, the installation, its selection, its repositories, the
changes and the two features. beekeeper log records the same.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if o.app == "" {
				return usageErr("--app names the App by its id, or a public one by its slug")
			}
			if o.org == "" {
				o.org, _, _ = strings.Cut(a.cfg.Apps.Repo, "/")
			}
			if o.org == "" {
				return usageErr("--org names the organisation the App is installed on (apps.repo is unset)")
			}
			o.apply = cmd.Flags().Changed("repos")
			switch {
			case o.suspend && o.unsuspend:
				return usageErr("--suspend or --unsuspend, not both")
			case o.reinstall && !o.apply:
				return usageErr("--reinstall goes with --repos: the repositories the new installation is to reach")
			case o.reinstall && o.token != "":
				return usageErr("--reinstall needs no --token")
			case o.apply && (o.suspend || o.unsuspend):
				return usageErr("--repos changes a running installation: not with --suspend or --unsuspend")
			case o.apply && len(o.repos) == 0:
				return usageErr("--repos names at least one repository: GitHub keeps an installation on one at least")
			case o.apply && !o.reinstall && o.token == "":
				return refused("--repos needs --token: GitHub changes an installation's repositories only for a token with %s, which no App can declare", selectionPermission)
			case !o.apply && o.token != "":
				return usageErr("--token goes with --repos")
			}
			return a.appInstallation(cmd.Context(), strings.TrimSpace(args[0]), o)
		},
	}
	c.Flags().StringVar(&o.app, "app", "", "the App: its id, or its slug for a public App")
	c.Flags().StringVar(&o.org, "org", "", "the organisation of the installation (default: apps.repo's owner)")
	c.Flags().StringSliceVar(&o.repos, "repos", nil, "the repositories of --org the installation is to reach, exactly")
	c.Flags().StringVar(&o.token, "token", "", "with --repos, the reference of a fine-grained token with "+selectionPermission)
	c.Flags().BoolVar(&o.suspend, "suspend", false, "suspend the installation, as the App")
	c.Flags().BoolVar(&o.unsuspend, "unsuspend", false, "lift the installation's suspension, as the App")
	c.Flags().BoolVar(&o.reinstall, "reinstall", false, "with --repos, delete the installation as the App and answer its installation page with --repos preselected")
	return c
}

// appInstallation is app installation: the read, the change, logged.
func (a *app) appInstallation(ctx context.Context, installation string, o installationOpts) error {
	target, err := a.appSecret(installation)
	if err != nil {
		return err
	}
	refs := []secret.Ref{{File: target.File, Path: "stringData." + githubPrivateKey}}
	var tokenRef secret.Ref
	if o.apply && !o.reinstall {
		if tokenRef, err = secret.ParseRef(o.token); err != nil {
			return usageErr("--token: %v", err)
		}
		refs = append(refs, tokenRef)
	}
	if err := a.sandboxFiles(refs, nil); err != nil {
		return err
	}
	ops, err := a.secretOpsKeyed()
	if err != nil {
		return err
	}
	var res *appInstallationResult
	err = ops.Use(ctx, refs[0], func(pem string) error {
		key, err := parseAppKey(pem)
		if err != nil {
			return err
		}
		if !o.apply || o.reinstall {
			res, err = a.installationScope(ctx, key, o, "")
			return err
		}
		return ops.Use(ctx, tokenRef, func(token string) error {
			res, err = a.installationScope(ctx, key, o, "Bearer "+strings.TrimSpace(token))
			return err
		})
	})
	if err != nil && Code(err) == ExitError && !errors.Is(err, secret.ErrVault) {
		err = refused("%v", err)
	}
	done := ""
	if res != nil {
		done = res.line()
	}
	if err := a.opLog(vaultExit(err), appInstallationOp, "%s (%s): %s", installation, o.app, outcome(err, done)); err != nil {
		return err
	}
	if a.json {
		return a.printJSON(res)
	}
	_, err = fmt.Fprintln(a.out, res.text())
	return err
}

// installationScope reads the App and its installation on o.org, suspends
// or unsuspends it, reads the repositories it reaches, applies o.repos with
// the token selector carries, and probes the device flow.
func (a *app) installationScope(ctx context.Context, key *rsa.PrivateKey, o installationOpts, selector string) (*appInstallationResult, error) {
	id, err := strconv.ParseInt(o.app, 10, 64)
	if err != nil {
		// a private App's slug is known to its owner alone: its id is the way
		k, err := a.appKnown(ctx, o.app)
		if err != nil {
			return nil, err
		}
		if k == nil {
			return nil, fmt.Errorf("GitHub shows no App %s: a private App is named by its id", o.app)
		}
		id = k.ID
	}
	jwt, err := appJWT(id, key, time.Now())
	if err != nil {
		return nil, err
	}
	asApp := "Bearer " + jwt
	var me struct {
		ID       int64  `json:"id"`
		Slug     string `json:"slug"`
		ClientID string `json:"client_id"`
	}
	if err := githubJSON(ctx, http.MethodGet, "/app", asApp, http.StatusOK, &me); err != nil {
		return nil, err
	}
	if me.ID != id {
		return nil, fmt.Errorf("the key of the Secret signs for App %d (%s), not %s", me.ID, me.Slug, o.app)
	}
	inst, err := orgInstallation(ctx, asApp, o.org)
	if err != nil {
		return nil, fmt.Errorf("the installation of %s on %s: %w", me.Slug, o.org, err)
	}
	res := &appInstallationResult{App: me.Slug, AppID: me.ID, ClientID: me.ClientID, Org: o.org, InstallationID: inst.ID, TokenExpiry: expiryUnread}
	if o.suspend || o.unsuspend {
		method := http.MethodPut
		if o.unsuspend {
			method = http.MethodDelete
		}
		if err := githubJSON(ctx, method, "/app/installations/"+strconv.FormatInt(inst.ID, 10)+"/suspended", asApp, http.StatusNoContent, nil); err != nil {
			return nil, err
		}
		res.Suspended, res.Unsuspended = o.suspend, o.unsuspend
		if inst, err = orgInstallation(ctx, asApp, o.org); err != nil {
			return nil, fmt.Errorf("the read back: %w", err)
		}
	}
	res.RepositorySelection = inst.RepositorySelection
	if inst.SuspendedAt != nil {
		res.SuspendedAt = *inst.SuspendedAt
	}
	if res.DeviceFlow, err = deviceFlow(ctx, me.ClientID); err != nil {
		return nil, err
	}
	if o.reinstall {
		return a.reinstall(ctx, asApp, me.Slug, inst.ID, o, res)
	}
	if res.SuspendedAt != "" {
		if o.apply {
			return nil, fmt.Errorf("installation %d is suspended since %s: --unsuspend it before its repositories change", inst.ID, res.SuspendedAt)
		}
		return res, nil
	}
	if o.apply && inst.RepositorySelection != selectionSelected {
		return nil, fmt.Errorf("installation %d reaches %s repositories of %s: no API narrows such an installation; its Configure page does, behind sudo mode", inst.ID, inst.RepositorySelection, o.org)
	}
	var tok struct {
		Token string `json:"token"`
	}
	if err := githubJSON(ctx, http.MethodPost, "/app/installations/"+strconv.FormatInt(inst.ID, 10)+"/access_tokens", asApp, http.StatusCreated, &tok); err != nil {
		return nil, fmt.Errorf("an installation token: %w", err)
	}
	asInst := "token " + tok.Token
	defer revokeToken(asInst)
	reached, err := installationRepos(ctx, asInst)
	if err != nil {
		return nil, err
	}
	if o.apply {
		if res.Added, res.Removed, err = a.applySelection(ctx, selector, inst.ID, o.org, reached, o.repos); err != nil {
			return nil, err
		}
		if reached, err = installationRepos(ctx, asInst); err != nil {
			return nil, fmt.Errorf("the read back: %w", err)
		}
	}
	res.RepositoryCount = len(reached)
	if inst.RepositorySelection == selectionSelected {
		res.Repositories = slices.Sorted(slices.Values(reached))
	}
	return res, nil
}

// reinstall looks up the repositories of o, deletes installation id as the
// App and answers the App's installation page with them preselected.
func (a *app) reinstall(ctx context.Context, asApp, slug string, id int64, o installationOpts, res *appInstallationResult) (*appInstallationResult, error) {
	if len(o.repos) > maxPreselectedRepos {
		return nil, fmt.Errorf("--repos names %d repositories: GitHub preselects at most %d on the installation page", len(o.repos), maxPreselectedRepos)
	}
	orgID, err := a.githubID(ctx, "orgs/"+o.org)
	if err != nil {
		return nil, fmt.Errorf("the organisation %s: %w", o.org, err)
	}
	var ids []int64
	for _, r := range o.repos {
		rid, err := a.githubID(ctx, "repos/"+o.org+"/"+r)
		if err != nil {
			return nil, fmt.Errorf("the repository %s/%s: %w; nothing deleted", o.org, r, err)
		}
		ids = append(ids, rid)
	}
	if err := githubJSON(ctx, http.MethodDelete, "/app/installations/"+strconv.FormatInt(id, 10), asApp, http.StatusNoContent, nil); err != nil {
		return nil, err
	}
	res.Uninstalled, res.InstallURL = true, installURL(slug, orgID, ids)
	return res, nil
}

// orgInstallation is the App's installation on org, read as the App.
func orgInstallation(ctx context.Context, asApp, org string) (installation, error) {
	var inst installation
	err := githubJSON(ctx, http.MethodGet, "/orgs/"+url.PathEscape(org)+"/installation", asApp, http.StatusOK, &inst)
	if err != nil && strings.Contains(err.Error(), "HTTP 404") {
		return inst, fmt.Errorf("not installed on %s: it reaches none of its repositories", org)
	}
	return inst, err
}

// applySelection adds the repositories of want installation id lacks and
// removes the ones want does not name: the additions first, so that it
// never stands on none.
func (a *app) applySelection(ctx context.Context, auth string, id int64, org string, have, want []string) (added, removed []string, err error) {
	path := func(repo string) (string, error) {
		rid, err := a.githubID(ctx, "repos/"+org+"/"+repo)
		if err != nil {
			return "", fmt.Errorf("the repository %s/%s: %w", org, repo, err)
		}
		return "/user/installations/" + strconv.FormatInt(id, 10) + "/repositories/" + strconv.FormatInt(rid, 10), nil
	}
	for _, r := range want {
		if slices.ContainsFunc(have, func(h string) bool { return strings.EqualFold(h, r) }) {
			continue
		}
		p, err := path(r)
		if err != nil {
			return added, removed, err
		}
		if err := githubJSON(ctx, http.MethodPut, p, auth, http.StatusNoContent, nil); err != nil {
			return added, removed, fmt.Errorf("adding %s: %w", r, err)
		}
		added = append(added, r)
	}
	for _, h := range have {
		if slices.ContainsFunc(want, func(r string) bool { return strings.EqualFold(h, r) }) {
			continue
		}
		p, err := path(h)
		if err != nil {
			return added, removed, err
		}
		if err := githubJSON(ctx, http.MethodDelete, p, auth, http.StatusNoContent, nil); err != nil {
			return added, removed, fmt.Errorf("removing %s: %w", h, err)
		}
		removed = append(removed, h)
	}
	return added, removed, nil
}

// installationRepos are the names of the repositories the installation
// token reaches, every page.
func installationRepos(ctx context.Context, auth string) ([]string, error) {
	var names []string
	for page := 1; ; page++ {
		var out struct {
			Total int `json:"total_count"`
			Repos []struct {
				Name string `json:"name"`
			} `json:"repositories"`
		}
		if err := githubJSON(ctx, http.MethodGet, "/installation/repositories?per_page=100&page="+strconv.Itoa(page), auth, http.StatusOK, &out); err != nil {
			return nil, fmt.Errorf("the installation's repositories: %w", err)
		}
		for _, r := range out.Repos {
			names = append(names, r.Name)
		}
		if len(out.Repos) == 0 || len(names) >= out.Total {
			return names, nil
		}
	}
}

// revokeToken revokes the installation token auth carries, bounded on its
// own: the token is good for an hour and needed no longer.
func revokeToken(auth string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if resp, err := githubRequest(ctx, http.MethodDelete, "/installation/token", auth); err == nil {
		_ = resp.Body.Close()
	}
}

// deviceFlow asks GitHub for a device code with the App's client id, which
// needs no secret: device_flow_disabled is the feature off, a code the
// feature on. The code is discarded and expires unused.
func deviceFlow(ctx context.Context, clientID string) (string, error) {
	resp, err := githubSend(ctx, http.MethodPost, githubWeb+"/login/device/code", "application/json", "", url.Values{"client_id": {clientID}})
	if err != nil {
		return "", fmt.Errorf("the device flow: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out struct {
		DeviceCode string `json:"device_code"`
		Error      string `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&out); err != nil {
		return "", fmt.Errorf("the device flow: GitHub answered HTTP %d, no JSON", resp.StatusCode)
	}
	switch {
	case out.Error == "device_flow_disabled":
		return deviceFlowDisabled, nil
	case out.DeviceCode != "":
		return deviceFlowEnabled, nil
	}
	return "", fmt.Errorf("the device flow: GitHub answered HTTP %d: %s", resp.StatusCode, bounded(out.Error, phraseRunes))
}

// githubJSON is one API call that expects status, its answer decoded into
// out when out is non-nil.
func githubJSON(ctx context.Context, method, path, auth string, status int, out any) error {
	resp, err := githubRequest(ctx, method, path, auth)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	what := method + " " + strings.SplitN(path, "?", 2)[0]
	if resp.StatusCode != status {
		err := apiError(what, resp)
		if p := resp.Header.Get("X-Accepted-GitHub-Permissions"); p != "" && resp.StatusCode == http.StatusForbidden {
			err = fmt.Errorf("%w (GitHub accepts the permission %s)", err, p)
		}
		return err
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(out); err != nil {
		return fmt.Errorf("%s: GitHub's answer is no JSON", what)
	}
	return nil
}

// scope is the installation's reach in words: suspended, or the
// selection and the repositories its token reaches.
func (r *appInstallationResult) scope() string {
	if r.Uninstalled {
		return "uninstalled"
	}
	if r.SuspendedAt != "" {
		return fmt.Sprintf("%s repositories, suspended since %s", r.RepositorySelection, r.SuspendedAt)
	}
	s := fmt.Sprintf("%s repositories, %d reached", r.RepositorySelection, r.RepositoryCount)
	if len(r.Repositories) > 0 {
		s += " (" + strings.Join(r.Repositories, ", ") + ")"
	}
	return s
}

// changes are what the call changed, in words.
func (r *appInstallationResult) changes() []string {
	var out []string
	if r.Suspended {
		out = append(out, "suspended")
	}
	if r.Unsuspended {
		out = append(out, "unsuspended")
	}
	if r.Uninstalled {
		out = append(out, "install on the repositories: click Install at "+r.InstallURL)
	}
	if len(r.Added) > 0 {
		out = append(out, "added "+strings.Join(r.Added, ", "))
	}
	if len(r.Removed) > 0 {
		out = append(out, "removed "+strings.Join(r.Removed, ", "))
	}
	return out
}

// line is the result in one line, for the log.
func (r *appInstallationResult) line() string {
	s := fmt.Sprintf("app %s (%d), installation %d on %s: %s; device flow %s", r.App, r.AppID, r.InstallationID, r.Org, r.scope(), r.DeviceFlow)
	for _, c := range r.changes() {
		s += "; " + c
	}
	return s
}

// text is the result for the person's terminal.
func (r *appInstallationResult) text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "app %s (id %d, client id %s)\n", r.App, r.AppID, r.ClientID)
	fmt.Fprintf(&b, "installation %d on %s: %s\n", r.InstallationID, r.Org, r.scope())
	for _, c := range r.changes() {
		fmt.Fprintf(&b, "%s\n", c)
	}
	fmt.Fprintf(&b, "device flow: %s\n", r.DeviceFlow)
	fmt.Fprintf(&b, "user token expiry: %s", r.TokenExpiry)
	return b.String()
}
