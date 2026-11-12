# Secrets

No agent reads a secret value. This page lists what the hook refuses and the
`beekeeper secret` operations that do the credential work instead.

## Secret reads

A value a command prints lands in the session's transcript and goes to the model API with the next
turn; a value written to a file, a variable or the clipboard, hashed or diffed, is one any later
command can read or brute-force. One exposed credential is one rotation. For credential tools the
PreToolUse hook is an allow list: it refuses every command that touches a secret except the forms
below, names the part it refused and gives the safe forms. Every refusal names `beekeeper secret`:
`compare` or `fingerprint` for equality, `copy` or `set` for changes, `rotate` to replace a value
everywhere it is carried ([Secret operations](#secret-operations)).

Refused in every form, since beekeeper is the only process that reads, creates, rotates, encrypts
and decrypts secrets:

- `sops` (decrypting and encrypting alike, `helm secrets` included) and `op` in every form (`op
  read`, `op item`, `op whoami`, `op run`, …), also behind `sudo`, `env`, `timeout`, `xargs` and
  `beekeeper run`; `age -d` and `gpg --decrypt`.
- Every sign-in to or unlock of a vault: `op signin`, `op account add`, `op unlock`, the person's
  own unlock helpers (`secret.unlockCommands`, by name under any path; the broker alone runs one, as
  `secret.signinCommand`), `beekeeper secret unlock` and `beekeeper secret store`, which are the
  person's; and keyring reads (`secret-tool lookup|search`, macOS `security find-*-password`). No
  agent session holds or opens a vault session: it lives in beekeeper alone, and the refusal says
  so.
- `kubectl edit` of a Secret and `kubectl view-secret`.
- A hash (`sha*sum`, `md5sum`, `b2sum`, `cksum`, `openssl dgst`) or a diff (`diff`, `cmp`, `git diff
  --no-index`, …) of a file whose name says it holds secrets in plaintext (`secrets.yaml`,
  `token.txt`, a kubeconfig; code files and SOPS-encrypted `*.enc.*` or `*.sops.*` files pass).
- `vault` beyond `status`, `version`, `list`, `kv list`, `kv metadata get` and the `kv get`/`read`
  forms below.

Files known to hold secret values are never read whole: omp's provider configuration
(`~/.omp/agent/models.yml`, `config.yml`, `agent.db`), `~/.claude/.credentials.json`,
`~/.config/gh/hosts.yml`, `~/.docker/config.json`, `~/.netrc`, `~/.git-credentials`, the vault's
`secret.tokenFile`, and the files `secret.files` adds (globs and `~/` allowed). The hook refuses a
`Read` of one, a content `Grep` of it or of a directory holding it below the home directory, and a
command that names it (`cat`, `less`, `head`, `tail`, `cp`, `jq .`, `yq .`, `grep`, `sqlite3`, a
symlink to it, a glob that expands to it, a path relative to a `cd`), also behind `sh -c`.

The refusal names the read that passes, built from the key names the file holds (never a value): a
`jq` or `yq` filter whose first stage deletes every credential key wherever it stands (`yq -y
'del(.. | .apiKey?, ."X-Auth-Token"?)' <file>`, piped or not), a keys-only filter (`yq 'keys'`),
`grep -c` or `-l`. Metadata commands pass (`ls`, `stat`, `wc`, `find`, `mv`, `chmod`), and so do
`omp` and `beekeeper`, which read the file without printing it. A file that is no YAML or JSON
document has no key-stripped read.

A link to a credential file reads none of it, so `ln`, `ln -s` (any options, `-t` included) and
`link` with the file as the link's source pass wherever they stand: on the command line, in a script
it runs (`bash scripts/install.sh --pull`) or in a helper function it calls. A helper whose body is
more than one `ln` is judged by its body with the call's arguments in place of `"$1"`… and of its
`local` assignments; one whose body uses `$@`, `$*`, `$#`, a parameter operator (`${1:-x}`),
`shift` or `set`, a helper more than three helper calls deep, and any helper while `ln` itself is a
function keep the refusal. What reads the link stays refused: on the command line that makes it,
before the link exists, a read of its destination (the path, the word as written, a glob over it,
a recursive search above it, a link to it or to a directory above it, the path in an
interpreter's code or another shell's), as a read of the file itself is, with no key-stripped
exception; once it exists, a symbolic link resolves to the credential file and a hard link shares
its inode, both read as the file itself. A source that is a directory holding a credential file or
a glob, and a destination that is an option or comes from a substitution, keep the refusal.

The same holds for a link to a directory that holds a credential file (the home directory and `/`
included), and for a relative link source, resolved from the link's own directory as well as the
working directory. `ln` or a helper defined on the command line that links such a directory is
refused like a read of the file itself. Directory copies, including `cp -rs`, are refused when their
source holds a guarded file. Relative paths also account for `pushd` and `popd`.

A `find` whose `-exec`, `-execdir`, `-ok` or `-okdir` runs anything but a metadata command (or
`shred`, `unlink`, `rmdir`, `echo`, `printf`) on a start path holding a credential file its
`-name`/`-iname` tests accept is a read of it; moving a found file with `mv` counts too, since the
destination could expose it. A command that prints a credential file's path (`readlink`, `realpath`,
`find`, `ls`, `stat` with its path in the format, …) is a read too when its output is piped into a
stage that may open it or substituted into a command other than `echo`, `printf`, `test`, `[`, `[[`,
`basename`, `dirname` or `mkdir` (`readlink -f link | xargs cat`, `cat "$(readlink -f link)"`); `|
wc -l`, `>/dev/null`, `[ -n "$(ls …)" ]`, `[[ "$(readlink -f "$dst")" == "$(readlink -f "$src")" ]]`
and `stat -c %s` stay metadata, and the operands of a `[[ … ]]` after its `&&` or `||` are the
test's.

Codex refuses these in the same way, and in addition refuses a helper defined on the command line
that is handed a path in its protected home, since its body (parameter operators, `$*`, `printf -v`,
`mkdir`) may write it. Helper arguments assembled by concatenation or by joining path components are
checked together: splitting a guarded file path, or a protected Codex home path under Codex, does
not authorize access.

Static checks such as `bash -n scripts/install.sh`, `sh -n scripts/install.sh` and
`shellcheck scripts/install.sh` inspect script text without executing its credential operations.
The guard still checks command substitutions and refuses a credential file supplied directly
as the check's input. Shell no-execution options must precede the script operand.

An installer that links a credential file needs no separate setup script: its other commands are
checked as executable code as before, and only a read of the credential file or of the new link is
refused. An installer that needs to copy a value instead uses
`beekeeper secret copy <ref> <file#path>`; see `beekeeper secret copy --help` for supported
sources and destinations. Neither route prints the credential.

Refused unless their output ends in an allowed form:

- `kubectl get` of Secrets (by kind, `secret/<name>`, in a list such as `cm,secret`, by `--raw`
  path) with `-o yaml|json` or a `jsonpath`, `go-template` or `custom-columns` template that names
  more than the kind, the type and the metadata other than annotations (which carry the last applied
  object); `kubectl create|apply|replace|patch secret … -o yaml|json`; also through a shell function
  or variable that runs kubectl (`kc(){ kubectl --context x "$@"; }`, `K="kubectl …"`), and `kubectl
  get -o yaml` of names piped in from a Secret listing.
- `vault kv get` and `vault read`; `base64 -d` of a secret's `.data`.
- A render fed with secret values: `helm template|install|upgrade` with a values file (`-f`,
  `--values`, `--set-file`) whose name says it holds secrets, `kustomize build` or `kubectl
  kustomize` with `--enable-alpha-plugins` or `--enable-exec` (which run decrypting plugins).
- Whole command lines and environments, readable by every process: a program started with `-e
  PASSWORD=…`, `--token …` or `-p …` carries the credential there. `ps` with its
  `args`/`cmd`/`command` column (`ps aux`, `ps -ef`, `ps -eo pid,args`, BSD-style words without `c`,
  BSD `e`), `pgrep -a`, `pstree -a`, a read of `/proc/<pid>/cmdline` or `environ` (`ls`, `stat` and
  `wc` pass), `docker|podman inspect` without a `--format` limited to names and state, and `docker
  ps --no-trunc` with the command column.

  `ps` counts where it runs as the command (past `sudo`, `timeout` and the other wrappers, after
  `watch`, `ssh <host>`, `docker exec <c>` or `kubectl exec <pod> --`), never as another program's
  subcommand (`ollama ps`, `docker ps`) or a URL's path (`/api/ps`). The safe forms: `beekeeper ps`
  (below), `ps -eo pid,ppid,etime,time,rss,comm`, `pgrep -l`, `pgrep -c`, `docker inspect --format
  '{{.Name}} {{.State.Status}}'`.

The allowed forms: a `jq`/`yq` filter that keeps only keys or metadata (`jq '.data|keys'`,
`.items[].metadata.name`, `(.value|length)`; no `del(…)` or assignment, which print the rest), `wc`,
a consumer that prints nothing of it (`kubectl apply|create|replace -f -` without `-o`,
`--password-stdin`, `gh secret set`, `kubeseal`, `openssl x509 -noout`, `age-keygen -y`), or `>
/dev/null`; for a render also a filter that blanks the Secret data (`yq 'del(.data, .stringData)'`,
`.data |= keys` with `.stringData |= keys`), so a diff of two blanked renders passes. A file, `tee`,
a variable or a flag's value (`T=$(…)`, `--from-literal=k=$(…)`), a hash, `grep -c|-q` and the
clipboard are no allowed end. `-o name`, `-o wide`, the table and `kubectl describe secret` (sizes
only) pass.

The same holds inside what a command line runs besides itself: every argument of `sh|bash|zsh` (a
`-c` or `-ic` string, a single word such as `zsh -ic <helper>` included, and `"$SHELL"`), `ssh` and
`watch`; `eval`'s words; the body of a shell function or group on the line (`f(){ …; }; f`);
here-documents fed to a shell; and a script it runs (`bash x.sh`, `./x.sh`, `source x.sh`, read from
the session's working directory, a text file up to 64 KiB, nested scripts four deep). Quoted text,
comments and other here-documents (a commit message, an issue body) are not commands and pass.

The agent shell prelude (`beekeeper hook sessionstart`) also drops `OP_SESSION_*`,
`OP_SERVICE_ACCOUNT_TOKEN` and `OP_CONNECT_TOKEN` from the environment before every command, and the
aliases and shell functions of `secret.unlockCommands`, so a vault credential in the environment
never reaches an agent's command.

For short-lived read-only API calls through `grpcurl`, see [API reads](api-reads.md).

## Secret operations

`beekeeper secret` is how an agent gets a credential job done without reading a value: sops and op
run in beekeeper's process, a value stays in its memory for the one operation and is written nowhere
in plaintext (sops gets the plaintext on stdin, the file is written from its ciphertext), and an
answer is key names, lengths, equality or keyed fingerprints. Every call is a `secret.<operation>`
event in `beekeeper log` with the session, the references, the outcome and the operation's duration,
never a value: the entry waits for a busy state lock, a call whose entry cannot be written fails
with the reason and the entry, and a call the broker refused, left waiting on the locked vault or
ended on its deadline is logged by the broker as the requester's.

A reference is a SOPS file (every value in it), one value of a SOPS file (`file#a.b.c`, the dotted
key path; a key holding dots resolves as it is, `file#stringData.credentials.json`, the whole
remaining path tried as one key before it is split, or quoted, `file#stringData."credentials.json"`,
or escaped, `file#stringData.credentials\.json`, the form a new key takes; `sops://` in front
optional), a field of the shared 1Password vault (`op://<vault>/<item>/<field>`; as a source,
`op://<vault>/<item>/<field>#<KEY>` is the value of one `KEY=VALUE` line of an env-style field, an
item's `notesPlain` for one, which `items --env-keys` lists) or, as a source, a key of a Kubernetes
Secret (`k8s://<context>/<namespace>/<name>/<key>[#<path>]`, the `copy` row below), or, as a source,
one value of a plain file on the host (`file://<absolute path>#<path>` under `secret.fileRoots`, the
`copy` row below). beekeeper reads only the vault `secret.vault` names, and only as its service
account, whose token it reads from `secret.tokenFile` and gives to its own `op` calls alone; with
either unset, an `op://` reference is refused.

`secret.session: true` reads and writes the vault through the person's own `op` session instead
(`tokenFile` unused, `setup` refused): the way to a vault no service account can be granted, such as
a person's Employee vault. That session lives in the broker's memory alone ([The vault
session](#the-vault-session)), so every call on an `op://` reference runs there, from an agent
session, a sandboxed one or the person's terminal alike. Every command exits 78 when the vault
cannot give a value: none configured, no token, the vault locked past `secret.unlockWait`, or `op`
failing or answering nothing within a minute.

A SOPS file is encrypted under the creation rules of the `.sops.yaml` nearest above it, run from
that directory, so a `path_regex` relative to the repository matches. beekeeper reads YAML
documents: a file whose name sops does not type (a Helm values patch, `*.yaml.patch`) is read and
written as YAML.

- `compare <a> <b>`: `equal` or `different` for two values, and for two files each key's state
  (`equal`, `different`, `only in a`, `only in b`); exit 1 when anything is not equal.
- `fingerprint <ref>`: HMAC-SHA256 of each value under the value scanner's key (`scan/key`), cut to
  16 hex digits: equal values, equal fingerprints; only beekeeper can make one.
- `fingerprint <ref> --encode <encoding>` | `fingerprint --secret <context>/<namespace>/<name>/<key>`:
  The fingerprint of one value in an encoding (the form `copy --encode` writes), or of a key of a
  Secret in a kind lab whose lab lease the caller holds: a delivery checked without a value read.
- `copy <src.sops.yaml> <dst.sops.yaml> [--name n] [--namespace ns]`: A new SOPS file with src's
  values, encrypted under dst's rules; `--name` and `--namespace` rewrite a Kubernetes object's
  metadata. It answers each key and its length (a Secret's `data` decoded); dst must not exist.
- `copy <ref> <file#path>`: One value into a SOPS path, creating the file or the key when absent,
  the file's other values kept.
- `copy k8s://<context>/<namespace>/<name>/<key>[#<path>] <file#path>`: A key of a Kubernetes Secret
  as the source of any copy (a SOPS path, `<ref>=<path>…`, a consumer, `--to-secret`), of `compare`
  and of `fingerprint`; `#<path>` picks one value inside what the key holds: a dotted key of its
  YAML or JSON mapping (tab-indented too), a line of its dotenv lines (`KEY=value`, an `export` in
  front and matching quotes stripped, `#` comments ignored) or key-value lines (`key = value` or
  `key: value` with any spacing, CRLF line ends, quotes stripped; a bare line right before them a
  section, `#<section>.<key>`, each line also by its own key), or, one level down, a value of the
  YAML mapping or dotenv lines a block scalar holds (`#config.yaml.oidc.clientID`; a key holding
  dots takes as many segments as it needs).

  A credential its owner placed in a cluster Secret (an identity provider's client secret for a
  connector, for one) reaches a SOPS path without a person and without its value reaching a session:
  one Secret GET in beekeeper's process, through kind for a lab's context (`kind-<cluster>`, its lab
  lease held by the caller) and through the kubeconfig files `kube.kubeconfig` names
  (`KUBECONFIG`'s, else `~/.kube/config`) for any other, a production context included, nothing
  written to that cluster.

  A Secret or key absent fails in one line naming it; a path that reaches nothing names the shape of
  what the key, or the node on the way, holds (a YAML or JSON mapping with its top-level keys,
  dotenv or key-value lines with their keys, a list's length, a scalar, text with each line's head
  before its first `:` or `=` and a line without either by its length, binary; its lines and bytes),
  a key named like a generated value counted, no content quoted; the answer is the value's length.
- `copy 'file://<absolute path>#<path>' <file#path>`: One value of a plain file on the host as the
  source of any copy (a SOPS path, `<ref>=<path>…`, a consumer, `--to-secret`), of `compare` and of
  `fingerprint`: a credential a CLI writes as a plain JSON file (the Codex CLI's `auth.json` in its
  `CODEX_HOME` after `codex login --device-auth`, for one) reaches a lab's Secret without a session
  reading the file. `#<path>` is a dotted key of the file's JSON or YAML mapping or a line of its
  dotenv lines, read as a `k8s://` key's content is.

  The file, its symlinks resolved, must lie under a directory of `secret.fileRoots`; it is read in
  beekeeper's process and never written, moved or deleted, and a `file://` reference as a
  destination is refused. A path outside the roots (the refusal names them), a missing file or a
  path that reaches nothing fails in one line naming it, no content quoted; the answer is the
  value's length, and every read is logged like any other.
- `copy 'cmd://<producer>#<path>' <file#path>`: A credential an allowed producer generates as the
  source of a copy (a SOPS path, `<ref>=<path>…`, `--dockerconfigjson`, a consumer, `--to-secret`),
  never of `compare` or `fingerprint`: the producers are beekeeper's own list, `az acr token
  credential generate`, `openssl genpkey` and `openssl rand`, each with the options it lists alone,
  any other command or option (`--debug` or `--verbose`, which log the credential, `-out`, a file,
  `-engine`, code) refused before it runs.

  The producer runs in beekeeper's process from the path beekeeper resolves itself, never the
  caller's `PATH` (the first of `/usr/bin`, `/bin` and `/usr/local/bin` that holds it, refused
  unless root owns it and every directory above it, none writable by a group or other users), in
  `/`, with a clean environment: `PATH=/usr/bin:/bin`, `HOME` from the user database, and of the
  caller's variables only the locale and `TZ`, minus the names `secret.env` lists; `#<path>` is a
  dotted key of its JSON or YAML stdout (`#passwords.0.value`), none the whole stdout without its
  line end.

  Its stderr is discarded and a failure names its exit code alone; a path that reaches nothing or
  `null` stores nothing. Every check of the destination runs before the producer does, so a
  credential is not spent on a write that could not happen. Refused in the agent sandbox, like a
  consumer.
- `copy <ref> <file> --dockerconfigjson <registry> --username <user> [--name n --namespace ns]`: One
  value as the password of a `kubernetes.io/dockerconfigjson` Secret in a SOPS file: its
  `stringData` holds `.dockerconfigjson` (the registry's `auths` entry with the user, the password
  and their base64 `auth`), its type `kubernetes.io/dockerconfigjson`; `--name` and `--namespace`
  start an absent file as that Secret. It answers the length written; `beekeeper log` records the
  source, the file and the registry, for example `beekeeper secret copy 'cmd://az acr token
  credential generate --registry <r> --name <token> --password1 -o json#passwords.0.value'
  pull-secret.enc.yaml --dockerconfigjson <r>.azurecr.io --username <token> --name <name> --namespace
  flux-giantswarm`.
- `copy <ref>=<path>… <new-file> [--name n --namespace ns]`: Several values (vault fields, SOPS
  paths) into a new SOPS file in one encryption: only the recipients of the nearest `.sops.yaml` are
  needed, nothing is decrypted, so no age identity of the new file. `--name` and `--namespace` start
  it as that Secret, a bare path under `stringData`. Answers key names and lengths.
- `copy <ref> -- <consumer…>`: One value on the stdin of `gh secret set`, `garage json-api <endpoint> -`,
  `gitlab-rails runner [-e <env>] '<code>'` whose Ruby code reads `STDIN`, a command with
  `--password-stdin` or one with `--secret <name>=-`, or of one of them in a pod through `kubectl
  exec -i --context <context> <pod> -- <consumer…>`, where `valkey-cli` or `redis-cli --askpass [-n <db>]
  [--user <user>]` is one too, against the pod's own server (no `-h`, no `-p`), with `--scan
  [--pattern <glob>]` or one of `DEL`, `UNLINK`, `EXISTS`, `TYPE`, `TTL` and its keys: key names and
  replies, never a value (a lab proof drops a component's server-side session in its Valkey this
  way) (no TTY, which would echo stdin, no `-v`, never a context of `kube.production`): the value
  travels on the exec stream, never on an argv.

  Any other consumer is refused. `--stdin-json '<object>' --stdin-field <key>` hands the consumer
  that JSON object with the value at `<key>` instead of the bare value, for a command that reads a
  JSON request, such as Garage's `garage json-api ImportKey -` with `--stdin-json
  '{"accessKeyId":"GK…","name":"app"}' --stdin-field secretAccessKey`; `kubectl exec` starts
  `/garage` itself, so an image without a shell takes it, and the CLI reaches the admin API over RPC
  with the pod's own configuration, no admin token.

  It answers the consumer's exit code and its output with the value redacted (its JSON-escaped and
  base64 forms included).
- `copy <ref> --git-askpass -- git …`: A git operation over HTTPS (`git [-C <dir>]
  clone|fetch|push|ls-remote …`) with the value as the password: `GIT_ASKPASS` names a helper
  beekeeper writes for the one call, which asks beekeeper on a socket only git's own processes
  reach; it answers `--git-user` (default `oauth2`) to the Username prompt and the value to the
  Password prompt. The value is on no argv, in no file, in no environment and not in the answer; git
  runs without the global and system configuration and without hooks, a repository whose
  configuration names a credential helper is refused, and so are `-c`, `--config`, `--upload-pack`,
  `--receive-pack` and `--exec`. git runs in the caller's working directory, a brokered call's too,
  so `-C .` is the caller's checkout. The output comes back redacted with the exit code.
- `copy <ref> --to-secret <context>/<namespace>/<name>/<key>`: One value into a key of a Secret in a
  kind lab (`kind-<cluster>`) whose lab lease (`labs`) the caller holds: a merge patch of that one
  key under the field manager `beekeeper-secret`, which creates the Secret when absent and leaves
  its other keys, labels and annotations as they are (a second copy into another key of the same
  Secret keeps the first), through the admin kubeconfig `kind get kubeconfig` answers, which stays
  in beekeeper's memory like the value. Any other context, and a lab the caller does not hold, is
  refused (exit 3). It answers the value's length.
- `copy <ref> ssh://[<user>@]<host>[:<port>]/<path> [--mode 0600] [--owner user[:group]] [--sudo]
  [--template '…{{value}}…']`: One value into a file on a host reached by ssh (a name or an alias of
  the ssh configuration, its keys authenticating: `ssh -T -o BatchMode=yes`, no TTY, no prompt). The
  value travels on ssh's stdin alone, never on a command line, in a log or in the answer; the remote
  command is a fixed script beekeeper builds, every argument quoted and checked first (an absolute,
  clean path of plain characters, an octal mode, `user[:group]`, a host that is no ssh option, an
  IPv6 address in brackets), and run under `sh -c` whatever the login shell: under `umask 077` it
  writes a temp file next to the path, checks its length against what was sent (a dropped connection
  ends the stream early), sets the owner and then the mode (default `0600`, setuid and setgid kept),
  and moves it into place; a symlink target is replaced with a regular file, leaving its referent
  unchanged; a path that is a directory is refused.

  A remote script failure or a signal removes the temp file and leaves the path as it was; SSH exit
  255 means the target state is unknown, so use `secret fingerprint` to check it; a source that
  resolves to nothing, an empty or blank value, a whole file and a bad mode or owner fail before ssh
  runs. An omitted `--owner` keeps the writing user's (root under `--sudo`); each part of
  `user[:group]` accepts a name or numeric id. `--sudo` runs the script under `sudo -n`, for a path
  only root writes, by the host's own sudo rules (a password prompt fails, never waits).

  `--template` writes a file whose other lines are public (`URL=…` beside `TOKEN={{value}}`), the
  value at `{{value}}`, once; a template with the shape of a credential is refused. It answers the
  file, mode, owner, bytes and the value's keyed fingerprint, the one `fingerprint <ref>` answers;
  refused in the agent sandbox, like a consumer.
- `set <file> <path> --generate|--literal <value> [--vault op://…] [--to-secret <context>/<namespace>/<name>/<key> | -- <consumer…>]`:
  A new value (`--length`, 32; `--charset`, `alnum`, `hex` or `ascii`), drawn in beekeeper's
  process, into the SOPS path (the file or key created when absent); it answers the fingerprint.
  `--literal <value>` writes a known, public value instead, such as a GitHub App's client ID beside
  its client secret, the file's other values kept, and answers its key, length and fingerprint; a
  literal with the shape of a credential (a private key, a token's prefix: the outbound guard's
  token rules) is refused naming the rule, and no vault holds a literal.

  A plaintext Kubernetes Secret without values (apiVersion, kind, metadata, an empty `stringData`),
  a skeleton, becomes the SOPS file, encrypted to its `.sops.yaml` recipients; any other plaintext
  file (a ConfigMap, a Secret holding a value, no YAML mapping) is refused by what it is, naming the
  way on, before sops sees it. A Secret's value goes under `stringData` unless the path names `data`
  or `stringData` (`set <skeleton> default` writes `stringData.default`).

  A path the file's `.sops.yaml` creation rule would leave in plaintext (outside its
  `encrypted_regex` or `encrypted_suffix`, matching its `unencrypted_regex` or `unencrypted_suffix`,
  or under a rule that decides by comments) is refused before any value is drawn, naming the rule.
  `--name` and `--namespace` start an absent file as that Secret (`type: Opaque`), ready for Flux.
  Without `--vault` the SOPS file is the value's only home, for a credential no vault may hold.

  `--vault` writes the vault field first (the item or field created when absent, the item passed as
  JSON on stdin, never on a command line). `--to-secret` (a held lab, as for `copy`) or a consumer
  (the `copy` consumers, its output redacted and its exit code answered) receives the same value in
  the same call, after the SOPS path; a delivery that fails leaves the value in the SOPS path, for
  `copy` to finish. A refused lab or consumer is refused before any value is drawn.

  A path through a string leaf holding a YAML mapping document (dex-operator's provider
  `credentials` block scalar) sets one top-level key inside it: that line rewritten in place, an
  absent key appended, every other line kept byte for byte, the edit refused unless that key alone
  changed; `copy` into such a path does the same, and `reveal`, `compare` and `fingerprint` read a
  key inside the document. A leaf holding no such document is refused by its shape, never its value.
- `capture <file#path> [--jq <filter>] [--encode …] [--vault op://…] [--name n --namespace ns] -- <producer…>`:
  A credential its issuer generates and prints once (`az acr token credential generate`, a token
  created over an API) into a SOPS path, and the shared vault's field first with `--vault`: the
  producer is one beekeeper allows (`az acr token credential generate`, `openssl genpkey`, `openssl
  rand`, each with the options it lists alone: `--debug`, `--verbose`, `-out`, `-engine`,
  `-provider` and every other option refused, every other command too, `sh -c` among them), no
  configuration widens the list; it runs in beekeeper's process as a `cmd://` source's does, from
  the path beekeeper resolves, in `/`, with the clean environment, its stderr discarded, a failure
  naming its exit code alone; the value is its stdout without the line end, or the one string `--jq`
  picks from its JSON (`.passwords[0].value`).

  Every check (producer, references, creation rule, filter) runs before the producer; a non-zero
  exit, empty stdout or a filter that picks no single non-empty string is refused in one line,
  nothing stored. `--dockerconfigjson <registry> --username <user>` takes the file alone and writes
  a `kubernetes.io/dockerconfigjson` Secret, the value the registry's password. It answers the
  length as written and the keyed fingerprint, never the producer's output; `beekeeper log` records
  the producer's command and the references.
- `setup [--service-account <name>]`: Gives beekeeper the shared vault, once: `secret.vault` created
  when the person's 1Password session finds none, a service account (default `beekeeper-<host>`)
  that reads and writes that vault only, and its token written from `op`'s output straight to
  `secret.tokenFile` (mode 0600). It runs `op` as the person, in the caller's signed-in session, and
  answers the vault, the account and the token's length; a token file that holds a token is refused
  (exit 3).
- `import op://<vault>/<item>/<field> op://<shared>/<item>/<field>`: One field of a vault outside
  the shared one, read with the person's session, into a field of the shared vault, written as the
  service account (the item or field created when absent); it answers the length. From then on the
  shared reference is the one to use. With `secret.session` the broker runs it in the person's
  session it holds, for a sandboxed agent too; without, it runs on the host only.
- `import op://<vault>/<item>/<field> --recipient <age1…>`: A SOPS recipient's age identity into the
  shared vault's `sops age key <recipient>` item: the source (an identity, or a `keys.txt`-shaped
  notes field with comments above it) is refused unless beekeeper derives that recipient from it,
  and only the identity's line is stored.
- `import op://<vault>/<item>/<field> --for <sops-file|directory>`: As `--recipient`, the recipient
  taken from the `.sops.yaml` the repository holds: a SOPS file's recipient (its metadata's when
  encrypted, else its creation rule's), or for a directory that of the creation rule for the files
  below it, else the one recipient all its rules share. No rule, or several recipients, is refused,
  naming the path; `--recipient` wins when both are given.
- `rotate op://… --generate`: A value beekeeper made gets a new one (`--length`, `--charset` as for
  `set`): the vault field first, then every path of the SOPS files `scan.sops` names that carried
  the old value, the value itself or its base64 form (a Secret's `data`), matched by fingerprint.
- `rotate op://…`: A value a third party issues: the person rotates it at its issuer into the vault
  field, and `rotate` writes the vault's new value into every path that carried the old one, known
  by the fingerprint `beekeeper scan index` recorded before the change. It refuses while the vault
  still holds the recorded value.
- `unlock [--account <a>]`: The person's, in their own terminal: `op signin` on that terminal, the
  session handed to the broker ([The vault session](#the-vault-session)). Refused in an agent
  session and without a terminal; the hook refuses it in agent sessions too.
- `lock`: The broker forgets the vault session.
- `status`: Whether the broker holds the vault session, never the session.
- `store op://<vault>/<item>/<field> [--from-file <path>] [--keep-file]`: The person's, in their own
  terminal: a value a web page showed once, or a file it downloaded, into a field of the shared
  vault (the item created when absent), from the Wayland clipboard (`wl-paste`, cleared afterwards)
  or from the file `--from-file` names (shredded afterwards unless `--keep-file`). The value travels
  in beekeeper's memory alone, with `secret.session` to the broker over the keeper's socket and from
  there to op's stdin: never on a command line, in a spool or state file or in `beekeeper log`,
  which records the reference, the length and the keyed fingerprint, which `store` prints too.

  While the broker holds no session it says so and stops (exit 78) before it asks for anything.
  Refused in an agent session and without a terminal, and by the hook in agent sessions, which names
  the way: a one-line question to the supervisor, the person runs it.
- `store --github-app <item> [--from-file <key.pem>] [--keep-file]`: A GitHub App's credentials in
  one call: the client secret from the clipboard into the item's `client-secret`, the private key
  from the newest `*.private-key.pem` in `~/Downloads` (or `--from-file`) into `private-key`, the
  file shredded. It names the click that makes each (Generate a new client secret, Generate a
  private key) and waits for Enter while the clipboard is empty or the download absent.
- `page op://<vault>/<item>/<field> --github-client-secret <org>/<app> | --github-private-key <org>/<app> | --anthropic-key <name>
  [--workspace <w>] | --create "<what, where>" (--match <regex> | --download <glob>) -- <steps>`: An
  agent's, holding the browser lease: a credential a web page shows once (a GitHub App's client
  secret, an API key a provider's console shows on creation) or downloads once (an App's private
  key) is created in the person's Chrome and stored into a field of the shared vault, seen by no
  session.

  The steps run in a capture turn, a `browse` turn made blind: the page-reading tools are removed
  (`--disallowedTools`), and its own `hook capturepage` passes an allow list of Chrome calls alone,
  refusing every screenshot, zoom, page read and batch, every drag, double or triple click, key
  chord and function key, every navigate but to an http or https page and every script but
  beekeeper's reader, which answers the matches of the pattern on the page; controls are found with
  `find` and clicked by ref.

  The turn's hook, a beekeeper process, takes the one match from the reader's result and hands it
  over a private socket (a 0700 directory under `$XDG_RUNTIME_DIR`) to `page`; every result reaches
  the model with images dropped and every match and every run of 16 or more token characters masked;
  the masked answer never waits on the delivery (3 s, the hook's timeout 30 s). A download is the
  newest file in `~/Downloads` the glob matches that arrived during the turn, shredded once stored.

  The value goes into the field as `store` writes one (the keeper's session with `secret.session`),
  the item created when absent; then every place the turn's transcript and report carry it, or any
  match of the pattern, is masked before the report is printed, a stored value or not. It answers
  the field, the length and the keyed fingerprint, as `beekeeper log` (`secret.page`) records them
  with the source and the create. The preset or `--create` is the one creating click the turn's auto
  mode allows.

  Every check runs before the turn (the field writable, the vault session held, exit 78 otherwise);
  a turn that leaves no value, or several different matches, stores nothing and says the page may
  hold a new value no vault records.
- `github-app <installation> --dry-run [--replace]`: Checks the existing target Secret, its matching
  creation rule, valid age recipients and encryption of each planned key. Prints the file, Secret
  name and namespace, public recipients and key paths (`webhook-secret` is conditional). No GitHub
  request, manifest code consumption, decryption, vault access, fingerprint or audit write. An
  absent file is named as missing; create a Secret skeleton first. `--json` returns the same plan.
  This checks configuration, not possession of a recipient’s private identity.
- `github-app <installation> --code <code> [--replace]`: A GitHub App created on GitHub's manifest
  page outside `app create`: `--code` takes the one-time code GitHub redirects with (`?code=…`), or
  the whole redirect URL. beekeeper converts it in its own process with GitHub's unauthenticated
  `POST /app-manifests/<code>/conversions` (the code is the credential; no token goes along) and
  writes `private-key` and `client-secret`, and `webhook-secret` for an App with a webhook, under
  `stringData` of the Kubernetes Secret in the SOPS file `apps.secrets` names for `<installation>`,
  in one sops write (an absent file starts as the entry's `name` and `namespace`). stdout is `app-id <id>`
  and `client-id <id>`, the values a consumer's configuration takes in plain; stderr names the keys
  written with their lengths and keyed fingerprints; `beekeeper log` (`secret.github-app`) records
  the installation, the file, the ids, the slug and the fingerprints, never the code or a value.

  Every check runs before the code is spent: the installation's entry, the file's directory, the
  file a Secret (or absent with a name), each key encrypted by the creation rule, no key taken
  unless `--replace`. A conversion GitHub refuses is refused in one line with GitHub's HTTP status
  and message (`the conversion: GitHub answered HTTP 404: Not Found`); a code converts once, within
  an hour of the redirect. A browser turn cannot hand a code over (the Chrome tools show it as
  `REDACTED`): `app create <installation> --manifest <file>` takes the redirect itself ([GitHub
  Apps](github-apps.md#github-apps)).
- `beekeeper app create <name> [--timeout <duration>]`: A GitHub App from the manifest the
  organisation declares (`apps.repo` at `apps.manifest`, `{name}` for the App's name, read from the
  default branch), its credentials into the shared vault item `<name>` without a person copying a
  value. The manifest, with this run's redirect URL and a `state` nonce, goes to
  `https://github.com/organizations/<org>/settings/apps/new` from a page on a loopback listener,
  opened in the person's browser (in an agent session the caller holds the browser lease, as for
  `browse`; the person's own terminal needs none); the person clicks **Create GitHub App**; GitHub
  redirects to the listener with a one-time code, whose `state` must be this run's; beekeeper
  converts the code in its own process (`POST /app-manifests/<code>/conversions`) and writes the
  answer into the item in one `op` call: `app-id`, `client-id` and `slug` as configuration (string
  fields), `client-secret`, `private-key` and, for an App with a webhook, `webhook-secret`
  concealed.

  The browser then goes on to `https://github.com/apps/<slug>/installations/new` with the
  organisation and the repositories preselected that `installation.json` next to the manifest
  declares (`{"repositories": "all" | ["<name>", …]}`, names of the organisation's repositories, at
  most 100; absent: all); the person clicks **Install**, and the installation's id, found as the App
  itself with a token signed by the key still in beekeeper's memory, is recorded as
  `installation-id`.

  Each click is named and waited for up to `--timeout` (10m). The answer and `beekeeper log`
  (`app.create`) carry the ids, the slug and each secret's length and keyed fingerprint, never a
  value. Every check runs before the page opens: the declaration under the App's name, that GitHub
  knows no App of that name, the organisation's and the repositories' ids, the vault writable and,
  with `secret.session`, unlocked (exit 78). Refused in one line, nothing written: an App no
  manifest declares under its name, one GitHub already knows by that name, a redirect whose `state`
  is not this run's, and a Create click that does not come in time; an Install click that does not
  come leaves the credentials as written and names `secret store` for the id.

  With `secret.session` the write is a `store-item` request to the broker over the keeper's socket,
  one item in one call, each field plain or concealed.
- `rotate platform://<installation>/<capability>/<name> --reason <text>`: A credential the platform
  manager generates: `platformctl installation reconcile <installation> <capability> --commit --rotate <name>`
  on the host, the manager writing the new value into the installation's SOPS files in a pull
  request; nothing is decrypted and no copy reaches the vault. `--dry-run` shows the files that hold
  it.
- `recipients [<directory> | <sops-file>]`: For a directory (the working directory by default),
  every age recipient of the creation rules of the `.sops.yaml` nearest above it, a gitops
  repository's installations' for one (`management-clusters/graveler/.*` → its recipient); for a
  SOPS file, the file's recipients (its metadata when encrypted, else its creation rule's). Each
  comes with where its identity is: sops' own sources, the entry of `secret.ageIdentities`, the
  shared vault's item per recipient or an installation's item `<installation>.agekey` in
  `secret.vault` or `secret.ageVaults` (found in the vaults' item listings, metadata only) or
  `none`, naming the items the vaults lack.

  No value is read; exit 1 when a recipient has no identity. With `secret.session` it runs in the
  broker, like a call on the vault.
- `move op://<source-vault>/<item> <destination-vault>`: Move a whole item through the broker using
  native `op item move` and 1Password desktop app authentication. The source must be in
  `secret.vault` or `secret.vaults`; the destination must be in `secret.moveVaults` (empty refuses
  all). Configure desktop CLI integration first and approve in the app. Cached broker sessions and
  automatic sign-in are not used. Returns and logs metadata only; tool output and diagnostics are
  discarded. The item gets a new ID and source references must be updated. Never retries
  automatically: after failure or interruption inspect both vaults before retrying.
- `items op://<vault> [<match>] [--fields] [--env-keys]`: The items of a vault beekeeper reads
  (`secret.vault`, `secret.vaults` or `secret.ageVaults`): each item's title, category, tags and
  last change, those whose title, category or a tag contains `<match>` (case-insensitive) when one
  is given. `--fields` reads each listed item in beekeeper's process and answers its fields' labels,
  sections, types and value lengths; `--env-keys` answers the `KEY` of each `KEY=VALUE` line of a
  plain text field, notes for one, never a concealed one (blank lines, comments and an `export ` in
  front skipped, matching quotes dropped) with its value's length, listed as `<field>#<KEY>`, the
  reference `copy`, `compare` and `fingerprint` take; no value is printed, logged or returned.

  Another vault is refused before `op` runs, naming the vaults listed. With `secret.session` it runs
  in the broker, like a call on the vault.
- `reveal <sops-file> <path>…`: The configuration a SOPS file encrypts next to its secrets, in
  plaintext: each path one scalar or every scalar under it (an OAuth client's `id`, `name`,
  `redirectURIs`, a `trustedPeers` list), for a split that moves the secrets into SOPS files of
  their own and the configuration into a plaintext values file. Every leaf is classified first, and
  one that looks secret refuses the call (exit 3), named by its path and the reason, never its
  value: a key on its path named like a secret (`secret`, `password`, `token`, `key`, `credential`,
  `private`, `cookie`, `salt`, `hmac`, `cert`), a value the value scanner matches (a token pattern
  or an indexed secret), a URL carrying a password, a run of 16 or more characters at a key's
  entropy, except in a client or App id (a leaf named `id`, or `client`, `app` or `application`
  joined to `id` plain, by a hyphen or an underscore: `clientID`, `client-id`, `appId`,
  `application_id`; an item of a `trustedPeers` or `peers` list), public in every authorize URL
  however random, which is refused instead when it equals a value under a secret-named key of the
  same file.

  `--json` answers `[{"path", "value"}]`, a tool's input. `--to <plain.yaml#path>` writes what one
  path names into a path of a plaintext YAML file instead (created when absent, its other keys
  kept), printing no value: a dry run without `--write`, `unchanged` when the destination holds it
  already; a SOPS file as the destination is refused.
- `unset <sops-file> <path>… [--write]`: Keys out of a SOPS file (a value, a mapping or a list item;
  a number in the dotted path indexes a list; a key holding dots, `stringData.credentials.json`,
  resolves unquoted, quoted or escaped as in `copy`, and two spellings of one key unset it once),
  with `sops unset` in beekeeper's process and the file's age identity: the file keeps its
  recipients, sops renews its MAC and `lastmodified`. A dry run without `--write`. A path the file
  holds no longer is answered `absent`, so a second run removes nothing; a path under another named
  one goes with its parent, a list's later items first. It answers the paths removed, those absent
  and the key names the file keeps, never a value.

### Encoded values

Some consumers inject a value verbatim: a gateway's egress credential that sends `Authorization:
Basic <value>` needs the Secret to hold `base64("<user>:<token>")`, not the token. `--encode` on
`copy` (one value: into a SOPS path, a consumer's stdin or a lab's Secret) and on `set` makes that
form in beekeeper's process, after the value is read or drawn and before it is written; the encoded
value is never printed, logged or returned, and the answer is its length.

| Encoding | Written |
|---|---|
| `base64` | the value in standard base64 |
| `basic:<user>` | `base64("<user>:<value>")`, an HTTP Basic credential (a user without a colon) |

An unknown encoding, a whole SOPS file and `copy <ref>=<path>…` are refused before any value is
read. `set --encode` writes the encoded form to the SOPS path, the Secret and the consumer, and
keeps the generated value in the vault field; the fingerprint it answers is the encoded form's. A
delivery is checked with two fingerprints:

```sh
beekeeper secret copy op://<vault>/<item>/<field> --encode basic:x-access-token \
  --to-secret kind-<lab>/<namespace>/<name>/<key>
beekeeper secret fingerprint --secret kind-<lab>/<namespace>/<name>/<key>
beekeeper secret fingerprint op://<vault>/<item>/<field> --encode basic:x-access-token
```

A beekeeper without `--encode` reaches the same form through a consumer that encodes on the host,
`copy <ref> -- <consumer…>`, as long as the consumer reads the value on stdin and prints nothing of
it.

### Age identities

sops decrypts a file encrypted to age recipients with an identity from its own sources:
`SOPS_AGE_KEY`, the file `SOPS_AGE_KEY_FILE` names, and `sops/age/keys.txt` in the user's config
directory. Before sops runs, beekeeper reads the file's recipients from its plaintext metadata and
checks those sources for an identity of one of them; without one, it takes the entry of
`secret.ageIdentities` that names one of the recipients or the file's path, and for a recipient no
entry names, the shared vault's item per recipient (below). A file with none fails before sops, in
one line. A file with another key group (KMS, PGP, Vault, key groups), an SSH or plugin recipient,
or with `SOPS_AGE_KEY_CMD` or `SOPS_AGE_SSH_PRIVATE_KEY_FILE` set goes to sops unchecked.

`secret.ageIdentities` supplies an identity no local source holds, from the shared vault, from an
identity file on the host that is to stay out of every vault, or from the person's own credential
store:

```yaml
secret:
  ageIdentities:
    - recipient: age1…                       # the files encrypted to this recipient
      ref: op://<vault>/<item>/<field>        # the AGE-SECRET-KEY-1… identity
    - pathRegex: /installations/[^/]+/secrets/  # or every file under a path (absolute, unanchored)
      ref: op://<vault>/<item>/<field>
    - recipient: age1…
      ref: file:///home/<person>/<identity file>  # an age identity file (absolute path)
    - recipient: age1…
      ref: store://<entry>                    # an entry of the person's own credential store
    - recipient: age1…
      ref: store://                           # the entry the store's search finds for the recipient
  store:                                      # the person's own commands, run by the broker
    read: [<command>, <args>…]                # prints the entry appended as last argument
    search: [<command>, <args>…]              # prints the names of the entries matching the term
```

The first entry whose recipient is one of the file's, or whose `pathRegex` matches the file's
absolute path, is read like any `op://` reference (the vault must be `secret.vault` or one of
`secret.ageVaults`) or, for a
`file://` reference, from the file as `age-keygen` writes it (comment lines and several identities
allowed), checked to hold the identity of one of the file's recipients, and only that identity is
given as `SOPS_AGE_KEY` to the one sops call's environment, never written anywhere. With
`secret.session` such a call runs in the broker, like a call on an `op://` reference: an identity
file is read in the broker's process alone, without the vault session, and never copied, printed or
fingerprinted.

A `store://` reference reads an entry of the person's own credential store (a password manager, or
a keyring behind the freedesktop Secret Service API, `secret-tool lookup` for one) through the
person's own commands in `secret.store`, which the broker runs like a vault field without the vault
session: `read` prints the entry's secret on stdout, the entry appended as its last argument.
`store://` with no entry asks the store's own search (`search`, the term appended) for each of the
file's recipients, and reads the entries it names, one per line: an identity is found by its public
recipient, never by listing values. beekeeper never handles the store's password; the store shows
whatever unlock prompt it shows the person, and a command answers within five minutes or fails. Its
output stays in the broker; an error names the entry or the recipient, never a value.

For credential sources, `store://<entry>[#<field>]` works alongside `op://`, `k8s://` and `file://`
in `secret copy` (SOPS paths, multi-value copies, consumers after `--`, `ssh://` destinations and
`--to-secret`), `secret compare` and `secret fingerprint`. These reads run in the broker in the
agent sandbox or with `secret.session`; otherwise beekeeper reads in its own process, including on
macOS and hosts without a broker. No shared vault is required.

The default field is the store's secret/password. An explicit field adds `--attributes <field>`
before the entry, replacing a configured `-a`/`--attributes` selector so KeePassXC returns only the
requested attribute. For other adapters, put `{field}` in an argument of `secret.store.read`: it is
replaced with the requested field, or `password` when omitted, and no attribute option is added.
This substitution applies only to credential sources: age-identity reads and `secret.store.search`
receive their configured arguments unchanged.

The entry remains the final argument; no shell evaluates it. Store-command errors name the reference
and suppress command output, which might contain credentials. Store copies return no automatic
fingerprint, including SSH delivery; request one with `secret fingerprint` when needed. An
unconfigured source is refused with `secret.store.read` named. A store reference is a source only;
`secret store` continues to write into the shared vault.

```bash
beekeeper secret copy 'store://services/api#password' credentials.sops.yaml#stringData.token
beekeeper secret compare 'store://services/api#password' credentials.sops.yaml#stringData.token
beekeeper secret fingerprint 'store://services/api#username'
```

A `store://` reference and `secret.store` are set together, in one `beekeeper config set` call (see
[Configuration](configuration.md#configuration)). A reference whose `secret.store` is still missing
leaves the configuration loadable: every command works, and `beekeeper secret …` warns once per such
reference until the store is set.

#### The vault's item per recipient

A recipient no entry of `secret.ageIdentities` names, a test installation's for one, has its
identity in the shared vault (`secret.vault`) under one naming convention: an item titled `sops age
key <recipient>` (the recipient as the gitops repository's `.sops.yaml` names it, `sops age key
age1…`), whose `password` field holds the `AGE-SECRET-KEY-1…` identity. A person creates the item
once, as a Password item in the vault; the item is the grant. Nothing else is configured: the broker
resolves a file's recipients from its metadata and, for a file still to be written, from the
creation rule of the `.sops.yaml` for its path (`management-clusters/<installation>/…` by
`path_regex`), finds the item in the vault's item listing (titles only, metadata, no value) and
reads it like any `op://` field for the one sops call.

`beekeeper secret recipients <directory>` shows, for a gitops repository's path, each creation
rule's recipient and where its identity is, reading no value. A file whose recipient has neither an
entry nor an item fails before sops in one line, naming the recipient and the items checked, never
the path's layout: a Secret for it is one no agent can change, and no person is asked to decrypt it.
With `secret.session` the call runs in the broker, like a call on the vault.

#### The installation's item

A customer's gitops repository keeps an installation's files under `installations/<installation>/`,
and the identity of the installation's recipient lives in an item titled `<installation>.agekey`,
the identity file's text in one of its fields (a Secure Note's `notesPlain`, for one) or as the
document it is. beekeeper finds the item by the recipient the file's `sops.age` block names, at
any path (a bare path, `management-clusters/<installation>/…` outside a checkout): it reads the
`*.agekey` items of `secret.vault` and of every vault of `secret.ageVaults`, the vaults the same
session or service account reads age identities from, one at a time until one holds the
recipient's identity, the item of the installation the file's path names first:

```yaml
secret:
  vault: <shared vault>
  ageVaults: [<vault holding the installations' .agekey items>]
```

The items are found in the vaults' item listings (titles only, metadata, no value); the broker reads
their fields in its own process for the one sops call and gives sops only the identity of the file's
recipient, so items holding other recipients' identities are refused, naming the recipient. No entry
per installation is configured. An `op://` entry of `secret.ageIdentities` may name a field of one
of `secret.ageVaults`; no other `op://` reference reads them. `beekeeper secret recipients` names
the item and its vault, or, when no vault holds it, the items the vaults lack.

### The vault session

With `secret.session`, no agent ever holds the vault session or opens one. It lives in the memory of
`beekeeper sandbox broker` (`beekeeper-sandbox.service`), a process of the host that no agent
session starts; never in a file, a keyring entry or an agent's environment. The broker makes itself
undumpable (no other process of the user reads its memory or environment), drops every vault
credential from what its calls inherit, and gives the session to its own `op` calls alone, in their
environment.

The broker signs in by itself: it runs `secret.signinCommand` when it starts and whenever a call
needs the vault while it holds no session, one sign-in at a time that every waiting call shares. The
command signs in without the person (a helper that reads the account password from a local password
store, for example) and prints the session as `op signin` does (`export OP_SESSION_<id>="<token>"`)
on stdout, which the broker reads into its memory; its stderr goes to the broker's journal.

It runs with the broker's environment, the vault credentials removed; a command that needs more
memory than the broker's unit allows runs in a unit of its own (`systemd-run --user --pipe --wait --quiet -p
MemoryMax=1G -- <helper>`). The broker holds the session for `secret.sessionLifetime` (12 h),
touches it every 10 minutes with a vault listing (a call that reaches 1Password: `op whoami` does
not reset op's 30-minute idle timeout) so that op does not let it idle out, and forgets it at the
end of the lifetime.

A session op no longer takes, found by the touch or by a call that op answers "not signed in", is
dropped and signed in again at once through `secret.signinCommand`, the call retried once with the
new session; the journal and the watch (`VAULT SESSION DROPPED at <t>: op no longer took it
(<reason>); the broker signs in again`, ended by the new sign-in) say so.

A sign-in that fails is tried again, after 30 s, 1 m, 2 m and then every 5 m, within
`secret.unlockWait` of the ask, each try bounded by two minutes. Before each try the broker asks the
session bus for the person's credential store (the freedesktop Secret Service: KeePassXC, GNOME
Keyring, KWallet) and waits, looking every 15 s, while it is locked and, within ten minutes of the
boot, while nothing serves it yet; later an absent store may be no Secret Service at all, and the
command runs.

The journal names each failure's cause (`the credential store is locked`, `no credential store
answers on the session bus`, `the network did not reach 1Password`, `the sign-in did not finish in
time`, `1Password rejected the password`) with the command's last line, and the watch says `VAULT
SIGN-IN RETRYING: <cause> (<line>); try <n> at <t>`, or `… waiting for the credential store: <state>`,
until the sign-in unlocks or gives up. A sign-in that gives up is `VAULT SIGN-IN FAILED: <cause>
after <n> tries: <reason>` and a `vault.signin` line in `beekeeper log`; the broker signs in again
at the next call on the vault.

Only a password 1Password rejected while the store answered unlocked (or could not be asked) is for
the person: one sign-in note for `guide.person`, which closes by itself once `beekeeper secret
status` finds the broker holding a session (exit 78 while it holds none). A store that was locked or
absent is the cause whatever the command said: a helper that read no password reports what 1Password
said to the empty one, and a note asking the person to fix an entry that is fine would be wrong.

While the broker holds no session, a call on the vault prints `vault locked: waiting for the
broker's sign-in` and waits up to `secret.unlockWait` (8 m; the hook gives such a call the Bash
tool's 10 minutes), then exits 78. Nothing asks the person. The watch says `VAULT UNLOCKED: the
broker holds the vault session since <t> until <t>` for the session's lifetime and an ENDED line
when the broker forgets it; `VAULT SIGN-IN FAILED: <reason>` once a sign-in gave up; `VAULT LOCKED: <who>
waits on <ref>` for each waiting call, with an ENDED line once it goes on, or `VAULT LOCKED: <who>'s
call on <ref> timed out …, still locked` when it gives up.

`beekeeper status` names the waiting sessions. `beekeeper secret lock` forgets the session at once.
The broker runs on Linux only, so `secret.session` needs it there.

Without `secret.signinCommand`, the person hands the broker a session with `beekeeper secret unlock`
in their own terminal: `op signin` runs on that terminal and the person types the account password
into op's own prompt; the session goes from beekeeper's memory to the broker over a Unix socket in
the runtime directory (`$XDG_RUNTIME_DIR/beekeeper/vault.sock`, mode 0600 in a 0700 directory, which
the sandbox can neither reach nor write), after `unlock` checked that the listener is the main
process of `beekeeper-sandbox.service` running this beekeeper binary as this user (from systemd: the
broker's own `/proc/<pid>/exe` is root's, since it is undumpable).

The socket answers only whether a session is held. `unlock` refuses in an agent session (Claude
Code's, omp's or the sandbox's variables set) and without a terminal, and logs a refusal; the hook
refuses it in agent sessions before it runs.

The 1Password desktop app's CLI integration (op asks the app, the app asks the person through the
system authentication prompt) is not used: it lets any process of the user that calls op raise the
prompt, so an unsandboxed agent calling op directly would raise the same prompt as beekeeper, and
the person could not tell them apart.

`rotate` reads every SOPS file before it writes anything, so one it cannot read stops the rotation
with nothing changed; it answers the new value's fingerprint and the paths it went to, indexes the
new value for the scanner, and closes the open rotation notes of the reference and of each path.

```console
$ beekeeper secret copy team-a/app.sops.yaml team-b/app.sops.yaml --name app-copy --namespace team-b
wrote team-b/app.sops.yaml: 2 keys
  data.token                                                 40 bytes
  stringData.password                                        32 bytes
```
