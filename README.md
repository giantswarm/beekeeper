# beekeeper

beekeeper keeps the Claude Code sessions that share one machine working together.

A dozen sessions on one workstation compete for the same things:

- RAM and swap: one OOM kill of the desktop scope ends every session.
- A handful of kind labs and shared installations.
- The one browser the Claude in Chrome extension drives.
- The merges into a repository that rolls an installation.
- The one GitHub budget every `gh` and `devctl` call draws from.

beekeeper reads what is already on disk (the process table, the session records, the
transcripts and the git checkouts) and keeps what must be shared in a small state directory.
Every session and the supervisor use the same binary.

On top of that it runs a desk: a supervisor loop that fills the desk from a project board,
starts workers on Claude Code, omp, agy or codex, gates every merge, and a guide session that
walks the person through the decisions waiting on them.

## Install

Download the binary for your OS and architecture into a directory on your `PATH`:

```bash
dir=~/.local/bin   # any directory on PATH
curl -fsSL -o "$dir/beekeeper" https://github.com/giantswarm/beekeeper/releases/latest/download/beekeeper-linux-amd64
chmod +x "$dir/beekeeper"
```

Then look at what the install would do, and install it for your user:

```bash
beekeeper install --dry-run   # every file, hook and service step; changes nothing
beekeeper install
```

`install` adds beekeeper's hooks to Claude Code's settings and installs the supervisor loop
as a user service. `beekeeper self-update` keeps it current.

Releases are signed. [Install](docs/install.md) shows how to verify a download, what the
install changes and how to add the role plugin:

```bash
claude plugin marketplace add giantswarm/beekeeper
claude plugin install beekeeper@beekeeper
```

The session and machine views need Linux. Leases, holds and the budget work anywhere.

## First commands

| Command | What it shows or does |
|---|---|
| `beekeeper status` | One line: who supervises, leases and holds, due notes and timers, busy agents. |
| `beekeeper sessions` | Every running session, what it works on and what it waits for. |
| `beekeeper capacity` | Busy and idle agents against the ceiling, and whether a new start has room. |
| `beekeeper ui` | A terminal screen over the machine, the sessions, the agents and the merges. |
| `beekeeper lease claim <resource> -p "<why>"` | Takes a shared lab, installation or the browser. |

Every command takes `--json`. `beekeeper --help` lists them all, and
[Commands](docs/commands.md) describes each one.

## Documentation

Start here:

- [How beekeeper works](docs/overview.md): what it reads, what it keeps.
- [Install](docs/install.md): download, verify, hooks, updates and the role plugin.
- [Commands](docs/commands.md): every command and the conventions they share.
- [Configuration](docs/configuration.md): every key, with a complete
  [example](docs/examples/config.yaml).

Running a desk:

- [The supervisor loop](docs/supervisor-loop.md): filling the desk, outages, alerts.
- [The merge gate](docs/merge-gate.md): lanes, turns, releases and stuck merges.
- [The guide and the desk](docs/guide.md): roles, relays and claims across desks.
- [Agents and their turns](docs/agents.md): starting, waking, stopping and handing over.
- [Worker sessions](docs/worker-sessions.md): headless and desktop turns, reviews, parking.
- [omp, agy and codex agents](docs/harnesses.md): workers beyond Claude Code.
- [Capacity, models and subscriptions](docs/capacity.md): who starts on which model.
- [Notifications and reports](docs/notifications.md): desktop notifications, deliveries,
  the status reporter.
- [Remote hosts and the central instance](docs/remote-hosts.md): SSH execution hosts and
  shared state.
- [Disk](docs/disk.md): automatic reclamation and the disk floor.

Safety:

- [Threat model](docs/threat-model.md): who reaches what, what leaves the machine.
- [Secrets](docs/secrets.md) and [GitHub Apps](docs/github-apps.md): credential work
  without reading a value.
- [The agent sandbox](docs/sandbox.md) and [Folders, deletes and guards](docs/safety.md).
- [Clusters](docs/clusters.md): kube contexts, production writes, Teleport, upgrades.
- [The browser](docs/browser.md): `beekeeper browse`, consents, uploads and proofs.

Everything else, including design notes and proofs, is listed in the
[documentation index](docs/README.md).

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) and [Developing on beekeeper](docs/development.md).

Document new behaviour in its `docs/` page, not here. CI keeps this file under 150 lines and
every Markdown line in it and in `docs/` under 200 characters
([check](.github/check-markdown-budget.sh)).
