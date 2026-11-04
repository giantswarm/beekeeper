# Browser and Dependabot alerts

- **Dependabot alerts:** before/after remediation, `beekeeper security alert <owner/repo> <number>`.
  Unreadable or dismissed is not fixed. App: Dependabot alerts read, accepted on installation.
  No personal scopes or security-scan bypass.

- **Browser:** headless: CLI Claude in Chrome tools; desktop: `beekeeper browse "<steps>"`.
  Never wait on site approval. Own App grants: `beekeeper app allow` first; refusals are
  barriers. Sign-in: `muster auth login --no-browser`, the printed URL in a browse step.
