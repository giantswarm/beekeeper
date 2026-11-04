# Security scans and Dependabot alert state

A worker reads an alert's current state with:

```sh
beekeeper security alert giantswarm/example 8
beekeeper --json security alert giantswarm/example 8
```

The command makes one REST GET through the worker's `gh` identity, with the
usual bounded retries for transient read failures. A sandbox's egress proxy
supplies the devctl App's token. Outside a sandbox, `gh` may resolve a person's
login: devctl's agent link selects the App for owners it is installed on and
the person's login for other owners. A successful read alone does not identify
the caller as the App. The command uses whichever identity `gh` resolves; it
does not change authentication or scopes or read a token itself. JSON contains `number`,
`state` and `html_url`; plain output contains the state and the alert URL.
States are `open`, `dismissed`, `fixed` and `auto_dismissed`.

## App setup

The GitHub App must have repository permission **Dependabot alerts: Read-only**
(`vulnerability_alerts: read`). `security_events` is the code scanning
permission and does not substitute for it. Declare the permission in the
App's manifest, apply it to the App's permissions settings, and accept the
new permission on its organization installation. The installation must
include the repository; a user access token also requires that its user can
view that repository's alerts. An existing authorization may need renewal
after the App's permission changes. Do not add `security_events` or other
scopes to a person's `gh` login.

See GitHub's [get a Dependabot alert](https://docs.github.com/en/rest/dependabot/alerts#get-a-dependabot-alert)
permission requirements. A 403 means the alert could not be read, and a 404
can mean missing repository access or an unavailable alert. Neither means
the alert is fixed. The command returns an error for these and for malformed
or unknown states; report the access barrier and continue independent work.

## Recorded App permission and live proof

The devctl App manifest is maintained in
`giantswarm/github`, at `apps/giantswarm-devctl/manifest.json`.
[Permission PR #7098](https://github.com/giantswarm/github/pull/7098)
adds `"vulnerability_alerts": "read"` to `default_permissions`.
As checked on 2026-11-04, that PR remains open, so the default branch
does not yet declare the permission.

The PR and the [proof on issue #1618](https://github.com/giantswarm/beekeeper/issues/1618#issuecomment-6579829238)
record the live grant and successful reads on 2026-11-02 at 14:35:22 UTC:

- App identity: `giantswarm-devctl`, client ID `Iv23liWio5REm4MfY2Mw`,
  through `devctl auth exec` with devctl v8.161.17 and beekeeper v0.174.4.
- Dependabot alerts read-only was applied to the App and accepted on
  organization installation `163608675`.
- `beekeeper security alert giantswarm/agent-manager 7 --json` exited 0
  with state `fixed` and URL
  <https://github.com/giantswarm/agent-manager/security/dependabot/7>.
- `beekeeper security alert giantswarm/github 77` exited 0 with state
  `dismissed` and URL <https://github.com/giantswarm/github/security/dependabot/77>.
- No personal token scope was added.

These are historical observations from the linked proof, not a fresh read
or a guarantee of the alerts' current states. Repeat the read under the App
identity when assessing a remediation. Track the remaining declaration
change in [issue #1875](https://github.com/giantswarm/beekeeper/issues/1875).
Keep #1618 open until its documented-path, live-read and authentication
criteria are met and the manifest change has merged.

## Security-scans rule

Read the alert through `beekeeper security alert <owner/repo> <number>` before
and after a remediation. Record its URL and actual state with the tested
commit. An empty GraphQL `vulnerabilityAlerts` response is not proof of a
fix. `dismissed` and `auto_dismissed` are distinct from `fixed`: inspect the
reason and verify the dependency change rather than treating dismissal as
remediation. A failed read leaves the acceptance check open.

Fix security scan findings when a compatible fixed version exists. An
unresolvable upstream finding needs a justified, expiring exception and a
tracking issue. Never bypass a red security check or classify it as harmless
because it predates the change.
