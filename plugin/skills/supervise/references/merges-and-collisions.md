# supervise — merges, hand-overs and collisions

The supervisor reads this at its first tick and when sessions overlap, a lane stalls or a hold is
due.

- **Merges and hand-overs go by name.** Merges go through the gate; beekeeper never replaces the
  merge command, it only decides when it runs. Sessions merge their own green PRs at once and
  announce `merging <n>` and `merged <n>`; the supervisor never answers `merge now` and never holds
  a merge by staying silent. It sequences with `hold <repo>` / `release <repo>` (`beekeeper hold
  set`, `--lane <name>` for a lane) and with the config's `lanes`, which the gate enforces
  (`beekeeper lanes`; `lanes queue <repo> <n> --for <session>` seeds a place, `lanes settle <repo>
  <n> --for <session>` registers a merge that ran outside the gate, `hold set --lane <name> --except
  <repo#n>` keeps a proving window open for the one merge it waits for, `--except-session
  "<promoter>"` keeps a line for its promoter's own promote): one merge or promotion at
  a time per lane, the lane's HelmReleases Ready on the release before the next. A merge behind a
  busy lane waits on in its own run and wakes its session with the outcome, so the lane needs no
  `clear` message and a promotion no separate `go`; only a hold stops a merge, and `lanes clear` is
  the repair for a lane whose release will not roll. A hold reaches a merge only before its devctl
  started; after that the merge is a fact to sequence around. Resources are handed over as `yours <env>` / `browser yours` (the message
  records the grant) and returned as `<env> free` / `browser free`; the session's claim follows. The
  worker's side of the vocabulary is in the `lease` and `merge` skills.
- **Coordination between sessions.** Sessions that work on the same thing collide: two worktrees on
  one repo racing to the default branch, two proofs wanting the same lab, two fixes for one issue, a
  session bumping a chart another is still releasing. At every tick the supervisor maps what each
  busy session is on (repo, issue, PR, environment) and looks for overlap. When two overlap, both
  are told by name what the other is doing and the split is proposed: one owns the shared piece,
  the other waits for its release or takes the disjoint part; the one further along keeps going.
  Where order matters, the order and who waits are stated. A session that ignores the message is
  reported to the person, not stopped. Sessions waiting for the same release or CI are not a
  collision; sessions rediscovering the same bug are: the second is pointed at the first's PR.
