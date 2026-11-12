# GitHub Apps

How beekeeper creates, installs and stores the GitHub Apps of a desk.

## GitHub Apps

A GitHub App's credentials exist once, in GitHub's answer to the manifest flow's conversion, and
beekeeper takes them there so no person or session copies a value from a page. Four ways in:

- **Declared App into the vault**: `beekeeper app create <name>` for an App the organisation
  declares in `apps.repo`, its credentials into the shared vault item `<name>`, then its
  installation (above).
- **One App per installation into the vault**: `beekeeper app create <purpose> --installation <name>
  --base-domain <domain>` for a purpose whose Apps exist once per installation, so that no
  credential is shared between two installations. The purpose's manifest template in `apps.repo`
  (`apps.template`, default `apps.manifest`'s file as `manifest.template.json`:
  `apps/<purpose>/manifest.template.json`) is a manifest whose strings may carry `{installation}`
  and `{baseDomain}`; beekeeper renders it with the two flags and creates the rendered App as a
  declared one, its credentials into the vault item of the rendered name and `installation.json`
  next to the template its installation. The rendered name must contain the installation's name
  (`"name": "giantswarm-portal-{installation}"` gives `giantswarm-portal-graveler`) and stay within
  GitHub's 34 characters. Refused before any click, nothing written: a purpose no template
  declares, a rendered name without the installation or too long, a template carrying
  `{baseDomain}` without `--base-domain`, and a name GitHub knows already.
  - `--replace` creates the successor App for a credential past its expiry. The successor's name
    carries the generation suffix after the latest one: `giantswarm-portal-graveler-2`, then `-3`,
    and so on. The latest is the highest the shared vault holds an item of, or GitHub knows; a
    private App is invisible to an installation token's `GET /apps/<slug>`, its item is not. Its
    vault item of that name is written beside the old one. The old App and its item stay untouched
    until their consumers have switched. Deleting the old App on its Advanced settings page is the
    person's step, and the answer names that page. `--json` also carries `replaces`, the old App's
    slug, and `beekeeper log` (`app.create`) says `succeeding <slug>`.
- **Local manifest into an installation's Secret**: `beekeeper app create <installation> --manifest <file>`
  for an App a JSON manifest on disk declares (with its `name`), its credentials into the SOPS
  Secret `apps.secrets` names for `<installation>`:
  1. Every check runs first: the installation's entry, the Secret's file and creation rules, no key
     it holds already unless `--replace`, no App GitHub knows by the manifest's name, and the
     owner's id and those of the repositories `installation.json` beside the manifest names.
  2. beekeeper binds a listener on `127.0.0.1` (a random port) before anything else, and serves
     there over `http` the page that posts the manifest, with the listener's `/callback` as
     `redirect_url` and a `state` nonce, to
     `https://github.com/organizations/<org>/settings/apps/new` (`--org`, default the owner of
     `apps.repo`) or, for an App owned by a personal account, `https://github.com/settings/apps/new`
     (`--user <login>`, the account signed in to GitHub in the browser). The page opens in the
     person's browser, or with `--no-open` the progress on stderr names it (`open
     http://127.0.0.1:<port>/ in the browser`) and the caller opens it, an agent with `beekeeper
     browse` while it holds the browser lease. The one click is GitHub's **Create GitHub App**.
  3. GitHub redirects the browser to the listener with the one-time code. The listener takes one
     redirect, checks its `state`, converts the code in beekeeper's process with GitHub's
     unauthenticated `POST /app-manifests/<code>/conversions`, and writes `private-key`,
     `client-secret` and, for an App with a webhook, `webhook-secret` under `stringData` in one sops
     write. The browser then goes on to the App's installation page with the owner and the
     repositories preselected that `installation.json` beside the manifest declares
     (`{"repositories": "all" | ["<name>", …]}`; absent: all).

  stdout is `app-id <id>` and `client-id <id>`; stderr is the progress and the keys written with
  their lengths and keyed fingerprints; `beekeeper log` (`secret.github-app`) records the ids, the
  slug and the fingerprints. The code passes only between GitHub, the browser and the listener: the
  Chrome tools show it as `REDACTED`, and no output, log entry or error carries it, the key or a
  secret. A redirect of another `state`, or no Create click within `--timeout` (10m), writes
  nothing.
- **A code in hand**: `beekeeper secret github-app <installation> --code <code>` converts a code
  that reached the person some other way, into the same Secret.

A refused conversion names GitHub's HTTP status and message.

Once an App exists, its installation's scope and its optional features sit on settings pages behind
GitHub's sudo mode, which no agent passes. `beekeeper app installation <installation> --app <id>`
reads them as the App itself, with the key in the installation's Secret: the repository selection,
the suspension, the repositories reached, the device flow. It suspends the installation and lifts
that, both without a person. A selected installation's repositories change only with a token holding
`installation_repositories=write`, which no App can declare: an organisation owner's fine-grained
token (`--token`).

No API narrows an installation on all repositories: `--reinstall` deletes it as the App and answers
the installation page with the repositories preselected, whose Install click meets sudo mode.
