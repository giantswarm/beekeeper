# The agent sandbox

The sandbox policy Claude Code enforces on every session's commands.

An agent's commands run as the person's Unix user. A deny list of credential paths leaves every path
nobody listed readable, so the agent sandbox turns it around: the home directory is denied and only
what a session needs is re-allowed. `beekeeper sandbox render` prints the policy as Claude Code
settings; `beekeeper sandbox install` stages it and prints the root commands that put it into Claude
Code's managed settings (`/etc/claude-code/managed-settings.d/beekeeper-sandbox.json` on Linux,
`/Library/Application Support/ClaudeCode/managed-settings.d/` on macOS).

On a fresh machine they create the managed settings file (`managed-settings.json`, an empty `{}`)
and its `managed-settings.d` directory first: the sandbox mounts both read-only and cannot create
them, so a session held by `claude --settings` needs them as well. From there it holds every Claude
Code session on the machine, the headless turns beekeeper starts and the CLI the desktop app spawns
alike, enforced by Anthropic's sandbox runtime (bubblewrap on Linux, Seatbelt on macOS). The same
file passed to `claude --settings` holds one session to it, to try a change first.

- **Reads:** the home directory is denied; beekeeper's binary, config and state, the harness's
  transcripts, plans, skills and plugins, git's config and `sandbox.allowRead` are re-allowed. A
  symlinked config (the config directory, `~/.gitconfig`, beekeeper's config) is mounted at its
  target only, so the policy names it there (`XDG_CONFIG_HOME`, `GIT_CONFIG_GLOBAL`,
  `BEEKEEPER_CONFIG`). Kubeconfigs, the Teleport profile, the GitHub CLI's token file and every
  other credential nobody listed stay unreadable. Only the managed settings' read paths count.
- **Writes:** the session's working directory (when it is readable itself), the temporary directory,
  beekeeper's state, the build slots and `sandbox.allowWrite`. The sandbox mounts a readable path
  read-only over a writable one inside it, so a readable path that holds a writable one (beekeeper's
  state under its config directory, a `sandbox.allowWrite` under a `sandbox.allowRead`) is listed by
  its entries around the writable path; an entry made in it later is readable once the policy is
  installed again.
- **The harness's config directory** (`~/.claude`, or `CLAUDE_CONFIG_DIR`) is denied for writing:
  Claude Code mounts it writable in the sandbox and masks only the entries it knows, so a command
  could otherwise leave a new file there that the harness reads outside the sandbox. The file tools
  still write the sessions' memory and plans.
- **The scanner's key and index** (`scan/` in beekeeper's state) are denied for reading and writing
  inside the writable state directory: the narrower deny holds, so no session reads the fingerprint
  key or rewrites the index the redaction matches against. `beekeeper sandbox install` and the
  broker at its start create `scan/`, the mount point the sandbox cannot create itself.
- **Egress:** through beekeeper's egress proxy alone, to GitHub and `sandbox.domains`, nothing else,
  with no prompt to widen it. The policy points Claude Code's `httpProxyPort` and `socksProxyPort`
  at the broker's proxy (`127.0.0.1:<sandbox.proxyPort>`, 3190), so Claude Code bridges the
  sandbox's HTTP and SOCKS5 proxies there and runs no proxy of its own; the broker's proxy answers
  its own user only. An entry is a host, `*.<domain>` for its subdomains, or `host:port` for one
  port.

  Loopback is listed by port only (`127.0.0.1:<port>`, a lab's API server): a bare loopback host or
  a wildcard port would open every listener on the host (other sessions' port-forwards, local
  servers), so the configuration refuses it, and a name that resolves to a loopback, link-local,
  unspecified or multicast address is refused when the proxy dials it. A refused target gets a 403
  (SOCKS: not allowed) and a line in the broker's log.
- **The runtime directory** (`$XDG_RUNTIME_DIR`) is denied for reading like the home directory (the
  container runtime's registry login, the agents' sockets), apart from the egress proxy's directory.
- **The GitHub token:** `gh`, `git push` and `devctl` act with devctl's App user token (eight hours,
  capped by the App's permissions), and the sandbox never holds it, not even as a placeholder. The
  broker reads it every five minutes with `devctl auth exec` (`sandbox.devctl`) into its memory, a
  read bounded by a minute, by three while it holds no token (a cold devctl right after a boot); a
  failed read is tried again after 30 s, 1 m and 2 m, its journal line naming the cause (killed at
  the bound, no devctl login, devctl's own error, no token in its answer).

  Its egress proxy terminates TLS for `github.com`, `api.github.com` and `uploads.github.com` with a
  CA it makes at start, its key in memory alone and name-constrained to those hosts, and sets each
  request's `Authorization` header itself (Basic for git on `github.com`, a token for the API),
  replacing what the client sent; bodies pass untouched, so no endpoint can echo the token back.
  Every other host is an opaque tunnel. The broker writes the CA, a bundle of the system's roots and
  the CA, and gh's login (a fixed word, `beekeeper-egress-proxy`, which the proxy replaces) to
  `$XDG_RUNTIME_DIR/beekeeper/egress`; the policy points `SSL_CERT_FILE`, `GIT_SSL_CAINFO`,
  `CURL_CA_BUNDLE` and `REQUESTS_CA_BUNDLE` at the bundle, `NODE_EXTRA_CA_CERTS` at the CA and
  `GH_CONFIG_DIR` at gh's login, and git's `gpg.program` at the signing script.

  These variables reach the harness process too; the bundle keeps the system's roots, and the CA is
  good for GitHub's three hosts alone. git reaches GitHub over HTTPS (`git@github.com:` is
  rewritten), needs no credential helper, since the proxy authenticates its first request, and runs
  none of the person's for GitHub. `agents.shell.path` stays off `PATH` in the sandbox, an inherited
  `PATH` included, since a `gh` link to devctl reads the keychain.
- **The policy's environment without its sandbox.** Claude Code can apply the policy's `env` and
  still run a session's commands unconfined (its `sandbox` block not in force). No proxy then
  completes gh's login, so the variables are only in force where the sandbox runtime holds the
  command (`SANDBOX_RUNTIME`, which the host never has). Elsewhere the agent shell's prelude, and
  beekeeper itself at start (outside its hooks), drop the egress variables and `BEEKEEPER_SANDBOX`:
  gh, git and devctl act as on the host, with `agents.shell.path` first on `PATH` as outside the
  sandbox. With `BEEKEEPER_DEBUG` set, beekeeper says so on stderr (`beekeeper: debug:
  BEEKEEPER_SANDBOX set without SANDBOX_RUNTIME: …`), naming the variables it dropped.
- **Proving a brokered call.** A proof against a scratch broker (`beekeeper sandbox broker` with
  its own state directory and port) runs its client with `SANDBOX_RUNTIME=1` beside
  `BEEKEEPER_SANDBOX=1`: with `BEEKEEPER_SANDBOX` alone the environment is unheld, beekeeper drops
  the variables at start and the call runs locally, so the proof passes on an unfixed binary. Every
  such proof also makes a control call with no broker answering, which must fail; one that succeeds
  shows the client went local. `BEEKEEPER_DEBUG=1` on the client puts the drop in the transcript.
- **Commit signing.** The sandbox reaches no gpg-agent, so the policy sets git's `gpg.program` to a
  script the broker writes into the egress directory, which runs `beekeeper sandbox gpg`: it hands
  the payload of git's signing call (`--status-fd=2 -bsau <key>`, nothing else) to the broker, which
  runs the person's gpg (`gpg.program` of their git config, else `gpg`) on the host and answers the
  signature and gpg's status lines. `commit.gpgsign` and `tag.gpgSign` work unchanged; the key and
  the agent never enter the sandbox.
- **devctl.** devctl's gated commands (`pr merge`, `pr wait`, `release promote`, `release wait`,
  `rollout wait`) read the keychain over the user bus and start user units, both closed in the
  sandbox, so a sandboxed `beekeeper gate` hands its command to the broker: it runs the same gate on
  the host, as the session and in its working directory, with `sandbox.devctl`'s directory first on
  `PATH`, and its output and exit code come back through the spool once it ends (up to three hours).
  Only those commands are brokered, and a queued merge's own run is refused from the sandbox.
- **The roles' host commands.** `beekeeper agents start`, `wake` and `resume` start transient user
  units over the user bus, the watch reads the installations through the person's kubeconfig and
  Teleport login, `beekeeper person` reads the org's roster with the person's own `gh` login, and a
  lab's creation needs the container runtime: all closed in the sandbox. A sandboxed call of each
  hands its command line to the broker, which runs it on the host as the session, in its working
  directory, with the command's own checks: a start's brief and `--dir` held to the session's lists,
  a lab only for the holder of its lease.

  Their output streams back while they run, and a call whose sandboxed command ended (a stopped
  `Monitor`) is ended with it. The watch moves into a scope of its own (512M), out of the broker's
  unit; a lab's `agentlab up` or `down` runs capped as `beekeeper run` caps it. In the sandbox the
  hook refuses `agentlab up|down` and `kind create|delete cluster` and names `beekeeper lease
  up|down <lab>` instead. Nothing of the person's kubeconfig or Teleport profile enters the sandbox:
  only the watch's lines and the lease's own lab kubeconfig do.

  A start, wake or resume with `--config <scratch>` (or under `$BEEKEEPER_STATE_FROM`) keeps the
  scratch configuration's state: the call still asks the host's broker, which takes only the scratch
  file's `stateDir` and `leaseDir`, held to the session's lists, and runs the installed beekeeper
  under the host's configuration with them (`$BEEKEEPER_STATE_FROM`, which the started agent keeps
  too). The roster entry and events land in the scratch state, the live ones untouched; nothing else
  of the scratch file reaches the host, whose commands would run outside the sandbox.
- **No way out:** unsandboxed retries are off, and a session whose sandbox cannot start does not
  start.
- **Required.** `sandbox.required: true` refuses every session the policy does not hold: one whose
  environment lacks `BEEKEEPER_SANDBOX` because the policy is not installed or the session started
  before it was. `beekeeper hook pretooluse` refuses its every tool call but `SendMessage`, by which
  it tells the supervisor, in the hooks' scope or not, and `beekeeper hook sessionstart` gives it
  the refusal as context at its start. Off by default: it is set once the policy is installed and
  sessions work under it. A configuration that does not load refuses nothing.
- **The file tools.** Read, Grep, Glob, Edit, Write and NotebookEdit run in the harness, outside the
  sandbox. The policy runs `beekeeper hook pretooluse` for them and sets `BEEKEEPER_SANDBOX`, and
  the hook holds them to the same lists in every session, in the hooks' scope or not: a path is
  resolved (symlinks included, a missing one through its nearest parent) and refused outside the
  lists. The file tools may also write the sessions' memory and plans, which no command may.
- **Capped runs.** On Linux the sandbox blocks every Unix socket (seccomp cannot filter them by
  path), the user bus included, so `beekeeper run` cannot start its scope there. It asks the broker
  instead, `beekeeper sandbox broker` on the host (the user unit `beekeeper-sandbox.service`, which
  `beekeeper install` puts in place), through request files in `<stateDir>/sandbox`. The broker caps
  the slot's slice and moves the asking process into its memcap scope; the command runs there, still
  in the sandbox. It answers each request as the one process of the user that holds it open, never a
  process the request names, and only for memcap's own slices and scopes. With no broker answering,
  a sandboxed `beekeeper run` refuses (exit 1) instead of running a build uncapped.
- **Secret operations.** The sandbox holds no sops key and no op session, so a sandboxed `beekeeper
  secret compare|fingerprint|copy|set|rotate|recipients|import` goes through the same broker: it
  runs on the host as the session that holds the request (its name and id, nothing else of its
  environment), in its working directory, and the SOPS files it reads and writes are held to the
  sandbox's lists. Its output and exit code come back through the spool, and that output is never a
  value.

  A consumer (`copy <ref> -- <command>`) would run on the host, outside the sandbox, so the sandbox
  refuses it, as it refuses `--as`, `--config`, the vault's `setup`, which the person runs, and
  `import` without `secret.session`. The broker tells a sandboxed requester by its mount namespace,
  which no process in the sandbox leaves: an unsandboxed session's vault call ([The vault
  session](secrets.md#the-vault-session)) runs outside the sandbox's lists, its consumer included.
- **Lab kubeconfigs.** The machine kubeconfig stays denied, and kind needs the container runtime's
  socket, so a lab's kubeconfig exists only while its lease is held: the claim (or `beekeeper lease
  kubeconfig <lab>` once the lab runs) has the broker write it into the lease for the session that
  holds it, and release takes it away. The broker's calls run with its unit's PATH, the service
  manager's, so `kind` and the credential tools of a brokered `secret` call are found there or in
  the Go tool directories: beside the beekeeper binary, `$GOBIN`, `$GOPATH/bin`, `~/go/bin` and
  `~/.go/bin`.

  The lab's API server goes into `sandbox.domains` as `127.0.0.1:<port>`, so its port is fixed
  (agentlab's `apiServerPort`). Every HTTP client exempts loopback from the proxy environment, so
  the kubeconfig points the lab's cluster at the sandbox's SOCKS proxy (`proxy-url`), which ends at
  the egress proxy as an opaque tunnel held to the same allow list. In a session that holds a lab
  lease the hook puts `beekeeper lease kubeconfig --refresh <lab>` in front of every command, which
  points the lease's kubeconfig at this process's proxy.

  The policy is one for the machine, so a held lab's kubeconfig is readable by every sandboxed
  session, not by its holder alone.
