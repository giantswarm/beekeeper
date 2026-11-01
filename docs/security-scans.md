# Security scans and Dependabot alert state

A worker reads an alert's current state with:

```sh
beekeeper security alert giantswarm/example 8
beekeeper --json security alert giantswarm/example 8
```

The command makes one REST GET through the worker's `gh` identity, with the
usual bounded retries for transient read failures. A sandbox's egress proxy
supplies the devctl App's token. The command never changes scopes, reads a
token itself, or falls back to a person's login. JSON contains `number`,
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
