# The browser

Sessions beekeeper starts never drive the desktop's browser. They run browser steps through
`beekeeper browse`, under the rules on this page.

## The desktop's browser

**The browser is the desktop's, not a permission request.** Claude in Chrome's site requests
(`browser:navigate`, one per session and site) are held by Claude Desktop itself, in the session's
desktop row; Claude Code's permission layer never sees them, so neither `bypassPermissions` nor the
hook answers them. The desktop skips them only for a session in auto or bypass mode; the import
turns bypass into acceptEdits, and raising a session's mode again takes the person's approval in
the desktop. (A desktop-wide "Allow all sites", `preferences.allowAllBrowserActions` in the
desktop's `claude_desktop_config.json`, does not reach an acceptEdits session on a Team or
Enterprise account either.) With nobody at the desktop such a navigate waits until the desktop
aborts it, sometimes hours later, and the model reads "Claude in Chrome is not connected".

For x.com reads, use `beekeeper browse "Read <URL> and report the page text."`.
WebFetch returns HTTP 402 for x.com; read the page through `beekeeper browse`.
Hold the browser lease while it runs.

So the sessions beekeeper starts never use the desktop's browser. In their desktop turns
(acceptEdits) `beekeeper hook pretooluse` refuses every `mcp__claude-in-chrome__*` call and names
`beekeeper browse "<steps>"`, which runs the steps in a headless `claude -p --chrome` turn that has
the CLI's own Claude in Chrome tools and nothing else (`--tools '' --strict-mcp-config --permission-mode
dontAsk --allowedTools 'mcp__claude-in-chrome__*'`: no shell, no file tools, no other MCP server,
every other call refused rather than asked).

That connection takes the Chrome extension's site permissions and never asks. It prints the turn's
report, its transcript and each screenshot it took as an image file under `<stateDir>/browse/<id>/`.
Every headless turn of a start (the task's when the desktop runs none, an `agents wake`) gets
`--chrome` too, on the same CLI connection and the same extension site permissions. That turn keeps
its own Claude in Chrome tools rather than going through `browse`, by design: it runs in
bypassPermissions, so its shell could run `beekeeper browse` or `claude -p --chrome` itself, and
taking the Chrome tools out of it would narrow nothing.

A bypass turn's scope is the bypass; `browse`'s narrowing serves the desktop turns, whose
acceptEdits it keeps. So each kind of turn has one browser path: a desktop turn browses through
`browse`, a headless turn with its own Chrome tools. A person's own sessions are untouched. `agents
start` says which Chrome mode the desktop recorded, and `beekeeper agents` shows it per agent in its
BROWSER column: `asks` or `skips`.

## Clicks, consents and App grants

**A click targets only the element its step names; a subscription control is never clicked.** The
controls no browser call touches are a page's Subscribe, Unsubscribe, Watch, Star and Notifications
buttons (GitHub's thread subscription, repository watch and star, notification settings): their
click changes the person's own notifications, which an agent's token cannot restore (GitHub answers
the App's `updateSubscription` with `Resource not accessible by integration`). The browse turn's
prompt says so, and `beekeeper hook pretooluse`, in every session, refuses a Chrome click (a
computer click, a form value, a batch's action) whose `action_summary` names such a control or
whose ref the session's last page read (`read_page`, `find`) names one, a scripted click
(`javascript_tool` code that calls `.click()`, `dispatchEvent()` or `.submit()`) whose code names
one, and a scripted click that picks its element from a sweep over the page's buttons or links
(`querySelectorAll('button')`, `getElementsByTagName('a')`), which can land on one. A scripted click
selects its one element by its own selector (an id, a `data-testid`, an `aria-label`). `browse`
names each such refusal below the report in the call's own words, a script's code among them:
`refused: <the call>: beekeeper's hook refused it as [Subscription Control]`.

**A consent for an App the person owns and asked for is beekeeper's own decision; every other grant
stays refused.** A consent page (an OAuth grant: GitHub's green Authorize button, an identity
provider's Allow) is a permission grant to the browse turn's classifier, which refuses the click, or
the whole task at its first call, and judges every run anew: it holds no record of whose App a page
names. beekeeper does. `beekeeper app allow <name> --client-id <id> --word "<the person's words>"`
records an App the organisation declares and the person asked a consent for: its name as GitHub's
consent page titles it, the OAuth client id its consent URL carries, the callback hosts of its
declaration, and the person's words (a note's answer). The declaration is the App's manifest
(GitHub's App manifest: `name`, `callback_urls`) in the repository the config names, read on its
default branch when the App is recorded:

```yaml
apps:
  repo: example/github                # the repository that declares the organisation's Apps
  manifest: apps/{name}/manifest.json # an App's manifest, {name} for the App's name
```

An App no manifest declares under its name is not recorded, and without `apps.repo` none is; the
record names the manifest and the commit it was read at. `beekeeper app create <name>` creates a
declared App from that manifest through GitHub's manifest flow, its credentials into the shared
vault item `<name>` ([Secret operations](secrets.md#secret-operations)); the installation it is
given is `installation.json` next to the manifest, `{"repositories": "all"}` or `{"repositories":
["<name>", …]}` (names of the organisation's repositories; the file absent, all of them), which
`create` preselects on GitHub's installation page.

With it, `beekeeper hook pretooluse`, which runs in the browse turn too, answers a Chrome call that
acts on GitHub's consent page itself: on the recorded App's page with one of its declared callback
hosts (the client id, or the name in GitHub's title, since the Chrome tools report the URL's client
id as `REDACTED`) the click is allowed, the record in its reason; on GitHub's consent page of any
other App or callback host it is refused, naming the page and the record's form.

The page is the tab's last known URL and title in the turn's transcript, which every Chrome tool
result ends with; a call whose page the transcript does not show gets no decision, as before. A
sign-in or consent page of the lab's own identity provider on a loopback host
(`*.127.0.0.<n>.nip.io`, `localhost`) is allowed without a record: a fixture user's sign-in there is
the person's own lab's. A consent page on any other host is refused by the hook itself, never left
to the classifier: a page whose path names a consent (`approval`, `consent`, `grant`) or whose title
names a grant (`Authorize …`, `Grant Access`), every call acting on it; an OAuth authorization page
(an `authorize` or `oauth` path, a `client_id` with its `response_type` or `redirect_uri`), which
also signs in, a call whose own words name a grant (Authorize, Allow, Grant, Approve, Accept), while
the SSO button there gets no decision.

The browse turn's own start record (`beekeeper browse`, mode `dontAsk`, gone when the turn ends) has
the hook act in it wherever its `--dir` lies. Steps that name an App on record, by name or one of
its callback hosts, give the turn that App's auto mode allow rule after the shipped rules
(`--settings`, this run only), so the classifier refuses neither the task at its first call nor the
click, and the prompt's words to click that page's Authorize once and nothing else that grants
access; steps that name a loopback host give it the lab's rule and the prompt's exception for the
fixture user's credential the steps give; the report says `allowed consent: <name> (callback <host>,
declared in <manifest>)` and `allowed lab sign-in:` first.

**One permission change of the person's own App is recorded the same way.** The navigation to an
App's permissions page (`organizations/<org>/settings/apps/<slug>/permissions`) and the click that
selects a level there are permission grants to the classifier too. `beekeeper app allow <name>
--permission "<scope>: <level>" --word "<the person's words>"` records the one change the person
asked for (`"Members: Read-only"`), for an App without callback URLs too, one used through the
device flow, which has no consent page (a consent record of such an App is refused naming the
missing callback URLs). With it, the hook answers the navigation to that App's permissions page
under the organisation's settings, the calls there whose own words name the permission and no
other level (the row opened, the level selected, a form value) and Save changes, and on the
organisation's installation of the App a Review request or Accept new permissions that names the
App, in the page's title or the call's words; the permissions page of any other App and any other
call on the recorded one are refused, naming the record's form; a call on the installation's pages
that names no App on record is left to the classifier. A batch is allowed only when each of its
calls is. Steps that name the App give the turn that change's auto mode rule and the prompt's words
to set exactly that permission, Save changes once, Review request and Accept new permissions once,
naming the App and the permission in each call's summary, and the report says `allowed
permission: <name> (<change>, declared in <manifest>)`.

**One callback URL of the person's own App is recorded the same way.** Adding a callback URL on an
App's settings page (`organizations/<org>/settings/apps/<slug>`) is a change of its settings the
classifier refuses too. `beekeeper app allow <name> --callback-url <url> --word "<the person's
words>"` records the one URL the person asked for, on a callback host the App's manifest declares.
With it, the hook answers the navigation to that App's settings page under the organisation's
settings, the clicks there whose own words name Add Callback URL, that URL (scheme and host in
any case, a trailing slash dropped) or Save changes, and input that is exactly that URL; a call
there that names another URL or another part of the page (a webhook, a secret, a private key),
removes anything, types anything else or presses a key is refused, naming the record. The URL is
recorded only as an http(s) URL without user info, as Go's URL parser writes it back. Without such a
record the settings page is left to the classifier. Steps that name the App give the turn that
URL's auto mode rule and the prompt's words to add exactly that URL and Save changes once, and the
report says `allowed callback URL: <name> (<url>, declared in <manifest>)`.

**An App's creation, installation and deletion are grants too.** While `beekeeper app create` runs
for an App (its run on record, its loopback listener answering), the hook answers GitHub's **Create
GitHub App** on the manifest page (`settings/apps/manifest`, the organisation's
`settings/apps/new`); for an App that run creates or beekeeper's ledger of the Apps `app create`
created holds (`app list` shows it), the **Install** click and its confirmation on
`apps/<slug>/installations/new`; and the Advanced page's **Delete GitHub App**, the App's name typed
into GitHub's confirmation and its confirm click (`settings/apps/<slug>/advanced`, under the
organisation or the person's own account) for an App on record with `beekeeper app allow <name> --delete --word
"<the person's words>"` (its declaration in `apps.repo`, or with `--manifest <file>` the local
manifest of `app create --manifest`) or one the ledger holds whose credentials were stored nowhere.

The same clicks on any other App's page are refused, naming the form. Steps that name such an App
(its name, slug or the create's loopback address) give the turn its rule and prompt, and the report
says `allowed create:` or `allowed delete:`. `app create` refuses a name GitHub already knows saying
where GitHub holds the App (the organisation's or the account's own Advanced page), and a Create
click that never comes names the pages where a name GitHub's page called taken is held.

`beekeeper app check <url> [--title <title>] [--words <words>] [--navigate]` prints the hook's
decision for a page, dry. A call the classifier refused is named below the report, one `refused:`
line per call with the call's own words and the classifier's reason; a `[Permission Grant]` line
quotes the step that reads as a grant and names the record. A tab behind another tab of its window
is not rendered: its `document.visibilityState` is `hidden`, GitHub keeps its Authorize button
disabled there, every screenshot times out and the extension refuses its scripts and navigations.

The turn works in a tab in front: it checks the tab's `visibilityState` before each screenshot,
leaves a hidden tab for a fresh one (a new tab opens in front), and closes the tabs it created
before it ends, so the tab in front before the run is in front again; screenshots that timed out all
the same are reported as `not rendered:` below the report. A window a page opens (an identity
provider's sign-in popup, as a portal's OIDC sign-in opens) opens outside the turn's tab group,
where no tool reaches it: the turn replaces the page's `window.open` before the click with one that
keeps the window's address, follows that address in its own tab through the person's SSO button (a
Backstage portal's sign-in window with `flow=redirect`, so the portal returns to the page signed
in), and `windows:` below the report names the tabs the turn worked in and each window it followed,
by its address without the query.

## Deletes, deploys and uploads

A permanent deletion of data is the person's own click: deleting an edit-history
revision (Delete revision), a comment, an issue, a repository or a branch, or
emptying a trash. No browse turn makes these clicks, even when the owner asks.
Steps asking for one are refused before the turn starts, before any Chrome
call. Report the refusal to the supervisor at once.

**A deploy click the task covers is declared; every other deploy stays refused.** A portal's deploy
or create click (a review step's Deploy button, which applies a release to a cluster) is a
production deploy to the browse turn's classifier, which refuses it, so a portal proof stops one
click short. `beekeeper browse --allow-deploy "<what, where>" "<steps>"` declares the one click the
task covers, the object and its target as the steps name them: the turn runs with an auto mode allow
rule for exactly that action after the shipped rules (`--settings
'{"autoMode":{"allow":["$defaults",…]}}'`, this run only; the desktop turns' rules are untouched),
its prompt says to click that control once and nothing else that deploys, and the first line below
the turn's report says `allowed deploy: <what, where>`.

Without the flag the refusal stays: a `refused:` line names the click with the classifier's reason
(`[Production Deploy]`) and a line names the flag. A deploy the person did not ask for is never
declared.

**A file the steps upload is shared with `--file`; every other path stays refused.** A browse turn
has no file tool, and Claude in Chrome's `file_upload` takes only a file the session may read, so
without the flag it refuses every path, the turn's `--dir` included. `beekeeper browse --file
./shot.png "<steps that attach ./shot.png>"` (repeatable) refuses a path that does not exist, is not
a regular file, or lies outside the worker's directories (the working directory, the turn's `--dir`,
beekeeper's state directory, the calling session's scratchpad; links resolved), and files over
`file_upload`'s 10 MiB, before the turn starts, copies each file into a stage root of the turn's own
(`<stateDir>/browse/<id>/upload/<n>/<name>`, removed after the turn) and names that root to the CLI
(`CLAUDE_STAGE_FILE_ROOT`) as the folder of the files shared with the session; the prompt names each
file and the copy's path `file_upload` takes, and a line below the report says `shared for upload: <path>`.

The hook refuses a `file_upload` of any other path in the turn (`BEEKEEPER_BROWSE_SHARED` names the
copies to it), and the report names the refusal: `refused: file_upload <path>: beekeeper's hook
refused it as [Not Shared]`, with a line naming the flag. On GitHub's comment editor, which renders
no file input until a native picker opens, the steps mark the nearest common ancestor of the comment
textarea and its Comment button (`data-beekeeper-upload`), add a temporary visible `<input type=file
multiple>` to it, scope the upload-target check to it, upload to the input and drop its files on the
textarea (`beekeeper browse --help`).

The turn still has no Read tool: a `Read(<path>)` allow rule does not reach `file_upload` while the
Read tool is off, and turning it on would hand the turn every path the person's own allow rules
cover.

**A file is uploaded only into the visible input of the intended form or comment box.** A page keeps
hidden file inputs (GitHub's review-reply boxes beside the comment box) that take a `file_upload` as
readily as the intended one and attach the file nowhere the person sees. Before each `file_upload`
the turn runs beekeeper's upload-target check with `javascript_tool`, scoped to the intended form or
comment box: it marks the one enabled file input whose box is rendered (accessible name `beekeeper
upload target`), and marks none when the scope holds only hidden or disabled inputs, or several
visible ones.

The turn finds that name for the input's ref, and the hook (`beekeeper hook pretooluse`, in every
session) refuses a `file_upload` to any ref but the one the last check marked, naming why and
carrying the check.

**A frozen renderer costs one timeout.** A screenshot that times out on a frozen renderer (Chrome's
window occluded or minimised, which the compositor does not draw) is the turn's last: the hook
refuses every further screenshot and zoom of the session, each of which would wait out its own
timeout, and the turn reads the page's text instead, naming the page `not rendered`.

## Proofs

**A proof reaches a thread through `beekeeper proof upload`.** A screenshot `browse` saved (or one
taken from a transcript) goes into a public, private or internal issue or pull request thread with
`beekeeper proof upload <image> --repo <owner/repo>`, the repository of that thread. It re-encodes
the image as PNG, drops metadata, and commits it to that repository's images-only orphan branch
`beekeeper-proofs` through the session's `gh` and the broker's App identity.

The printed Markdown is pinned to the commit. Public repositories use a raw image URL; private and
internal repositories use a repository-relative Markdown image link ending in `?raw=true`, which
requires read access to that repository. No access token goes in the link. See [GitHub's repository
image link
documentation](https://docs.github.com/en/get-started/writing-on-github/getting-started-with-writing-and-formatting-on-github/basic-writing-and-formatting-syntax#images).

For either visibility, `beekeeper browse --proof-repo <owner/repo> "<steps>"` uploads its saved
screenshots and names the hosted Markdown links in its report. Paste these into the authorised
issue comment or PR body through `gh` under the App identity. This avoids attachment upload and
Comment clicks under the browser identity, including when that identity cannot write to the
repository. The App must have contents write access to the target repository. An upload failure
fails the command and is named in the report; a local screenshot path is not hosted proof.
What the pixels show remains the worker's to check: public threads carry nothing internal.

## The person's take-over

The same hook serves the person's take-over on `beekeeper ui`: while a running screen has taken a
session over (a flag file per session in the state folder's `takeover/`, naming the screen's
process), the hook holds that session's request for the screen and answers with the screen's allow
or deny. It gives up itself after 290 s, and at once when the take-over is released or the screen
is gone: no answer, and the request goes to the session's own window. Telling a session not taken
over reads that one file, without a lock; nothing waits on beekeeper.

Install it in `~/.claude/settings.json`, next to the PreToolUse hook (`beekeeper install` writes
it); the timeout is above the hook's own 290 s, so the hook, not Claude Code, ends a held request:

```json
"PermissionRequest": [{"matcher": "*", "hooks": [{"type": "command",
  "command": "~/.go/bin/beekeeper hook permissionrequest", "timeout": 300}]}]
```
