# Threat model

Who runs what on a desk, what may leave the machine, and what may reach the model.

## Threat model

Agents act for the person but are not the person: a prompt injection, a confused plan or a wrong
command must not reach a credential. The parties:

- The person
  - Runs as: their Unix user, at their terminal
  - Reaches: everything: the vault through their own sign-in, the keyring, the kubeconfigs
- beekeeper's broker (`beekeeper sandbox broker`)
  - Runs as: the person's user, a systemd user unit no agent starts, undumpable
  - Reaches: the vault session (in memory), the SOPS keys, the container runtime, devctl's keychain
    login
- A sandboxed agent session
  - Runs as: the person's user inside the sandbox (bubblewrap, own mount namespace, no Unix sockets)
  - Reaches: its working directory, the temporary directory, beekeeper's state outside `scan/`,
    GitHub and `sandbox.domains` through beekeeper's egress proxy; no credential
- An unsandboxed agent session
  - Runs as: the person's user
  - Reaches: what the person reaches, held back by the PreToolUse hook only
- Remote services (GitHub, the model API, chat)
  - Runs as: elsewhere
  - Reaches: what a session sends: the outbound guard refuses secret values ([What leaves the
    machine](#what-leaves-the-machine)); the mention guard refuses a gh post (issue or pr comment,
    create, edit, review; a comments, reviews, issues or pulls endpoint of gh api, its body files
    included) or a GitHub connector post that @-mentions someone: an agent's text goes out under a
    person's account; the upstream guard refuses every GitHub write outside `github.allowedOrgs`
    ([Writes outside the allowed organizations](safety.md#writes-outside-the-allowed-organizations))

The secrets and where they live:

| Secret | Where | Who reads it |
|---|---|---|
| The vault session (`secret.session`) | the broker's memory; for one call, the environment of the broker's `op` child | the broker, which signs in by itself (`secret.signinCommand`) |
| The vault's service account token (`secret.tokenFile`) | a 0600 file outside the sandbox's lists | beekeeper's own `op` calls |
| The SOPS keys | the person's key files, outside the sandbox's lists | sops in beekeeper's process |
| The GitHub App token of sandboxed sessions | the broker's memory | the egress proxy, into the `Authorization` header of requests to GitHub |
| The person's commit signing key | gpg and its agent on the host, whose socket the sandbox closes | gpg, run by the broker for a sandboxed git's signing call |
| The value scanner's key and index | `scan/` in beekeeper's state, denied to sandboxed sessions | beekeeper |
| Kubeconfigs, the Teleport profile, the GitHub CLI's token, the keyring | the person's home and session bus | the person; a lab kubeconfig for its lease holder |

Why an agent reads no value and opens no vault: a value leaves beekeeper only as a key name, a
length, an equality or a keyed fingerprint ([Secret operations](secrets.md#secret-operations));
every op, sops and decryption command, every vault sign-in or unlock and every keyring read is
refused in agent sessions ([Secret reads](secrets.md#secret-reads)); the agent shell drops vault
credentials from its environment; the vault session exists in the broker alone and only the person's
sign-in at their own terminal puts it there ([The vault session](secrets.md#the-vault-session)); a
sandboxed session can neither read the credential files nor reach a Unix socket, the keeper's and
the session bus included ([The agent sandbox](sandbox.md#the-agent-sandbox)); and a tool result that
carries a value is redacted before the model sees it ([What reaches the
model](#what-reaches-the-model)).

Residual risks:

- **An unsandboxed session is not a boundary.** It runs as the person's user, and the hook reads
  command lines: a compiled program, an interpreter's own code (`python -c`), a binary renamed or
  written on the fly reaches what the user reaches, the keyring, the token file, the keeper's socket
  (to lock it, or to hand it a session) and the environment of the broker's `op` child during a
  call. The sandbox is the boundary; the hook is a guard rail that catches mistakes and the plain
  forms.
- **Credentials in shell startup files.** A credential that shell startup files export comes back in
  every shell an agent's command starts (`zsh -c …` reads them again after the prelude). A vault
  session belongs to the broker only, never in startup files or a store every process of the user
  reads.
- **The keeper's socket.** `unlock` hands the session only to a listener running this beekeeper
  binary as this user; a process of the user that replaces the binary on disk defeats that check.
- **The egress proxy acts on GitHub with the App token** for every sandboxed session: a session
  cannot read the token, but it can make any call the App's permissions allow, as the person's
  merges and pushes do ([The agent sandbox](sandbox.md#the-agent-sandbox)).
- **The broker signs for every sandboxed session**: a session cannot read the signing key, but its
  git can have any commit or tag signed with it, as the person's own commits are.
- **A held lab's kubeconfig** is readable by every sandboxed session, not by its holder alone.
- **Desktop control.** An agent that drives the desktop could type into the person's terminal; the
  unlock still needs the account password, which no agent holds.

## What leaves the machine

The same holds for what a session sends out: an issue body, a commit, a chat post, a plan. The
PreToolUse hook scans what a call would send for secret values and refuses the call, naming the rule
or the phrase that matched and never the match, since the refusal lands in the transcript too:

- the command line, here-documents included, of `gh`, `devctl`, `git commit`, `git tag` and `git
  remote`, also inside `sh|bash|zsh -c` strings; the body of `curl`, `wget`, `http` or `xh` (its
  data flags; a header or URL carries the client's own credential to its service and passes);
- the files these send: `--body-file`, `-F`, `--notes-file`, `--input`, `@file`, `field=@file` and
  `$(cat file)`;
- for `git push`, the messages and added lines of the commits no remote has yet (removed lines
  pass);
- the input of every connector tool (`mcp__…`: Slack, GitHub, mail, documents), which needs
  `|mcp__.*` in the hook's matcher;
- a `Write`, `Edit`, `MultiEdit` or `NotebookEdit` of a file under `outbound.paths` (what it writes,
  not what it replaces).

The patterns are gitleaks' rules by their names: GitHub (`ghp_`, `gho_`, `ghu_`, `ghs_`, `ghr_`,
`github_pat_`), GitLab, AWS, GCP, Slack tokens and webhooks, Anthropic, OpenAI, npm, 1Password
service account and age keys, private keys, JWTs and a password in a URL (a placeholder such as
`${TOKEN}` or `<token>` passes); `outbound.phrases` adds what is specific to the machine
(case-insensitive, named by number). A line marked `gitleaks:allow` is skipped, for a test fixture
that only looks like a token.

An `op item|document create|edit` or a `vault kv put|patch` that an `outbound.storeDeny` rule
matches (vault and item globs; a write naming no vault may go to the default one and matches) is
refused: some credentials do not belong in a shared store. In an agent session the Secret guard
refuses these writes first ([Secret reads](secrets.md#secret-reads)).

The watch sweeps `outbound.sweepRoots` (the home directory) `outbound.sweepDepth` (5) levels deep
every `outbound.sweepEvery` (15m) for world-readable private keys (`id_*`, `*.pem`, `*.key`, `*.ppk`
with a private key header) and git repositories whose remote URL carries a credential, skipping
caches and dependency trees: one `EXPOSED <path>: <what>` line each, never the content, and an ENDED
line once it is fixed.

## What reaches the model

The guards above refuse command shapes they know; a shape they do not know still prints. The value
scanner matches the values themselves. beekeeper keeps an index of keyed fingerprints (HMAC-SHA256
under a key only it reads, `scan/key` in the state directory, both files `0600`) of every value it
can reach, each naming the reference to rotate. The index never holds a value. `beekeeper scan
index` fills it from the SOPS files `scan.sops` names (decrypted by `sops -d` in beekeeper's own
process) and the concealed fields of the 1Password vaults `scan.vaults` names (`op item get --reveal`),
replacing what earlier runs indexed from them; `beekeeper scan add <ref>` indexes one value read
from stdin.

Each value is indexed as written, in its base64 forms, line by line (and the value of a `key: value`
or `KEY=value` line), and, for base64 text, decoded; values shorter than `scan.minLength` (12) are
not, since they would match ordinary output. `beekeeper scan` prints what the index holds.

The PostToolUse hook, `beekeeper hook posttooluse` (`beekeeper install` registers it for every
tool), runs on each tool result before the model sees it. It splits every string of the result into
candidate strings (the tokens between whitespace, quotes and brackets, and their parts between `=`,
`:`, `@` and `/`), fingerprints the candidates of an indexed length, and runs the outbound guard's
token patterns beside them for values the index does not hold (a line marked `gitleaks:allow` keeps
its pattern matches).

It decodes every base64 run of 32 characters or more (a Kubernetes Secret's `data`, an attachment,
`base64` output wrapped over lines, and a run within the decoded text once more) and looks into the
decoded text the same way: a run that carries an indexed value or a token pattern (an age identity,
a private key, a token) is replaced whole. With a hit, the hook replaces the result with the same
result, each hit replaced by `[redacted: <reference or rule>]`, and the session goes on: the value
never reached the model, so ending the session would protect nothing more.

Claude Code writes the replaced result to the transcript on disk as well (checked with Claude Code 2.1.286),
so the value is in neither. Each redaction is a `scan.redact` event naming the tool and the
references and rules with their counts; an indexed reference also gets a rotation note for
`guide.person`, one open note per reference. It is still a filter, matching known values in known
encodings: a value split across tokens or encoded otherwise passes.

`beekeeper scan sweep` reads every transcript under `claude.projectsDir` (the sessions' and
subagents' `.jsonl` files, each line's strings decoded, and the spilled tool results in
``tool-results/`) and prints each reference and token rule it finds, base64-wrapped ones included,
with how often and in which files, never a value or where in a file it was: the list of what leaked
before the hook ran. A maintenance pass, it runs on one core at nice 10, and the PreToolUse hook
runs a session's sweep through `beekeeper run`, in a build slot like a build.

File-backed state updates retain their event append in a durable `eventOutbox`
inside `state.json` until the event log is synced and closed. After an interrupted
append, the next state read or write replays only the missing suffix. A conflicting
log fails recovery and retains the outbox for inspection; it never overwrites
unrelated history. An event-log open failure still leaves the prior state intact.
