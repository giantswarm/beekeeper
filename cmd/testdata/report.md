**01:00–02:00 EEST**

**Merged & shipped**

| PR | change | release | rollout |
|---|---|---|---|
| [tools#41](https://github.com/example/tools/pull/41) | queue merges \| promotions | v1.4.0-rc.1 |  |
| [portal#7](https://github.com/example/portal/pull/7) | the portal's login | v2.0.1 | rolled |
| [tools v1.4.0](https://github.com/example/tools/releases/tag/v1.4.0) | promoted to stable | v1.4.0 |  |
| [portal#8](https://github.com/example/portal/pull/8) | bump x | v2.0.2 | rolling |
| [notebook#12](https://github.com/other/notebook/pull/12) | [tools#41](https://github.com/example/tools/issues/41) in the notebook |  |  |

**Running**

- Supervisor run 3 — supervisor
- Worker [tools#43](https://github.com/example/tools/issues/43) — [tools#39](https://github.com/example/tools/issues/39), CI on [tools#43](https://github.com/example/tools/issues/43), holds lab-1
- [portal#5](https://github.com/example/portal/issues/5)

**Waiting on Ada**

- 5 notes: 2 from Supervisor run 3, 1 from a session on [portal#5](https://github.com/example/portal/issues/5), 2 from a session
- 1 session waiting on an answer: Plan [tools#44](https://github.com/example/tools/issues/44)
- drafts for review: [plans#9](https://github.com/example/plans/pull/9)

**Queue**

- lane example/portal: [portal#8](https://github.com/example/portal/pull/8) settling
- lane example/tools: [tools#43](https://github.com/example/tools/pull/43) waiting
- timer 7 at 02:30: check the rollout of [portal#8](https://github.com/example/portal/issues/8), see note 12

**Machine**

| | used | free |
|---|---|---|
| RAM | ▓▓▓▓░░░░░░ 37 of 86 GiB | 49 GiB |
| Disk | ▓▓▓▓▓▓▓▓▓░ 1726 of 1876 GiB | 70 GiB |

- load 3.2/2.5/2.0 on 24 cores, CPU pressure 0.4%, memory pressure 0.0%
- swap 2.0 of 16.0 GiB used
- kind labs: none
- 1 OOM kill since 01:00
- alerts: staging 3 active, 1 paging; lab 0 active; far unreachable
- GitHub budget: 4800 of 5000 left, resets 02:20
