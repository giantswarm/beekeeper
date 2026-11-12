# The scenario's GitHub

A [behavioral scenario](scenarios.md) with `given.github` gets a GitHub of its own: a model of
repositories, issues, pull requests, reviews, comments, checks, workflow runs, releases, labels,
sub-issues and a Projects v2 board, served as GitHub's REST and GraphQL APIs. It lives in
`internal/scenario/fakegithub`.

Everything that reaches GitHub in the scenario reaches that one server:

- **`gh`** is a translator. The fake `gh` turns `gh api`, `gh pr`, `gh issue`, `gh run`,
  `gh release`, `gh repo view`, `gh label` and `gh auth token` into requests to the server and
  renders gh's output and exit codes for a pipe: the body as it is, `--jq`, `--include`,
  `--paginate`, `gh: Not Found (HTTP 404)` with exit 1, `gh: HTTP 304` with exit 1.
- **Code that calls HTTP itself** is pointed at the server's loopback port through
  its API base variables: `BEEKEEPER_GITHUB_API` (the budget probes and the loop's checks) and
  `BEEKEEPER_GITHUB_API_URL` (`app create`).

So budgets, ETags, faults and the call log hold for both. Nothing is matched on a joined argument
string: a call is parsed (gh's flags, the REST route, the GraphQL document) and answered from the
model.

A request for something the model does not have is never answered with a guess. An unknown REST
route or GraphQL field is recorded as unsupported and **fails the scenario**, naming the route or
`Field 'x' doesn't exist on type 'Y'`. A `gh` command, flag or `--json` field the translator does
not serve is an unanswered call, which fails the scenario too. The gap is then closed in
`internal/scenario/fakegithub`, in the schema (`graphql_*.go`), the routes (`rest_routes.go`) or
the translator (`gh_*.go`).

## given.github

```yaml
given:
  github:
    viewer: desk                       # the login gh and every token act as (default desk)
    users: [{login: "renovate[bot]", type: Bot}]   # only for a Bot or a name; others just appear
    organizations: [{login: example, members: [alice]}]
    repositories:
      - name: example/app              # gets a bare origin, like given.repositories
        private: false
        labels: [{name: bug, color: d73a4a}]
        protection:
          - {branch: main, required_reviews: 1, required_checks: [build], strict: false}
        issues:
          - number: 1
            title: First
            state: open                # or closed
            labels: [bug]
            parent: 3                  # a sub-issue of #3; or owner/name#3
            blocked_by: [2]
            board: Ready               # its Status on the scenario's board
            fields: {Kind: Bug}        # the board's other single-select fields
            comments: [{author: alice, body: hello}]
        pulls:
          - number: 4
            title: Fix the first
            head: fix-1                # created in the origin with one commit when missing
            base: main
            body: "Closes #1"          # closing keywords link issues, as on GitHub
            state: open                # or closed, merged
            draft: false
            reviews: [{author: reviewer, state: approved}]
            checks: [{name: build, state: pending}]   # queued, pending, green, red
        releases:
          - {tag: v1.0.0, assets: [{name: notes.txt, content: "text\n"}]}
    projects:                          # optional: see below
      - owner: example
        number: 1
        fields: [{name: Status, options: [Ready, In Progress, Done]}]
    budgets:
      core: {limit: 5000, remaining: 4000, reset_in: 30m}
      graphql: {remaining: 100}
      secondary: {max: 100, per: 1m, retry_after: 1m}
    costs:
      - {operation: repositoryOwner, cost: 3}   # else GitHub's formula
    faults:
      - match: {path: repos/example/app/pulls, conditional: true}
        respond: {status: 304}
```

**Origins.** Every repository has a bare Git origin, and GitHub's branches, heads, commits, files,
diffs and merges are the origin's, read and written with real `git`. A pull request's head is its
branch's head, so a worker's push moves it, and a merge commits to the base branch and closes the
issues the pull request closes.

**The board.** Without `projects`, the scenario's board is the desk's (`board.owner`,
`board.project`), with the Status options Todo, Ready, In Progress, Blocked and Done plus any an
issue's `board` names, and any field an issue's `fields` names. With `projects`, the first is the
scenario's board and holds exactly the fields and options it declares.

**Mergeability.** `mergeStateStatus` and `reviewDecision` follow from the reviews, the checks on
the head and the base's `protection`, as on GitHub: BLOCKED without the required approval or check,
UNSTABLE with a pending or red check, else CLEAN. `mergeable`, `merge_state` and `review_decision`
on a pull request declare them instead.

## Callers and budgets

Every call is logged with its **caller**: the name of the `run` or `background` step whose process
made it. The driver sets `BEEKEEPER_SCENARIO_CALLER` for the fake `gh` and names the caller in the
API base (`http://127.0.0.1:<port>/caller/<name>`). A fake harness gives a worker its own caller
with `Server.Env`.

The server keeps GitHub's buckets: `core` and `graphql` (5000 points an hour), `search` (30 a
minute) and `code_search` (10 a minute). Every answer carries the `X-RateLimit-*` headers of its
bucket, and `GET /rate_limit` reports them all for free. A REST request costs one point. A GraphQL
operation costs what GitHub's formula gives it (the requests needed to fulfil every connection,
over 100, at least 1), or what `costs` declares for it, and the `rateLimit` field shows it. A
spent bucket refuses with GitHub's words and status. A bucket's window resets on the scenario's
clock; `budgets.secondary` adds a secondary rate limit, a 403 with `Retry-After` for requests over
its rate.

**Conditional reads.** A successful GET carries an `ETag`. A request whose `If-None-Match` still
matches is answered `304 Not Modified` and costs nothing.

## Faults

A fault answers the requests its `match` holds, `times` times (0: every time) and, with `for`,
only that long after it was armed. They are checked in order; the first that holds answers.

| `match` | Holds |
| --- | --- |
| `method`, `path`, `caller` | a regular expression that matches whole; `path` has no leading slash or query |
| `operation` | a GraphQL operation's name or one of its top-level fields (`repositoryOwner`) |
| `conditional` | requests with (`true`) or without (`false`) `If-None-Match` |
| `via` | `gh` for the CLI, `http` for direct callers |

| `respond` | Answers |
| --- | --- |
| `status` (and `body`) | that status with GitHub's usual body for it |
| `secondary_rate_limit: true` | GitHub's 403 for a secondary rate limit |
| `outage: true` | no answer: the connection fails (`gh`: `error connecting to api.github.com`) |
| `retry_after` | the `Retry-After` header |
| `delay` | the answer that long late, in real time; alone, a slow answer of the real one |

A fault's answer costs nothing and names the fault in the call log.

## Steps

```yaml
when:
  - github: {push: {repo: example/app, branch: fix-1, file: a.txt, content: "a\n"}}
  - github: {check: {repo: example/app, pull: 4, name: build, state: green}}   # or head: <branch|sha>
  - github: {review: {repo: example/app, pull: 4, state: approved, author: reviewer}}
  - github: {merge: {repo: example/app, pull: 4, method: squash}}
  - github: {close: {repo: example/app, issue: 1}}                             # or pull: 4
  - github: {comment: {repo: example/app, issue: 1, author: alice, body: hello}}
  - github: {budget: {graphql: {remaining: 10}}}
  - fault:
      arm: {name: down, match: {path: graphql}, respond: {status: 502}, times: 3}
  - fault: {clear: down}                                                       # or all
```

A `github` step is something that happens on GitHub, exactly one action each. `check` sets a check
run on a named head to `queued`, `pending`, `green` or `red`, and with it the workflow run of that
commit. `merge` merges as the merge button does, whatever the base's protection says. A `fault`
step arms a fault (one of the same name is replaced) or clears one.

## Expectations

```yaml
then:
  - github:
      calls: {path: repos/example/app/pulls, caller: loop, conditional: true, status: 304, min: 1}
  - github: {budget: {bucket: graphql, caller: loop, spent: {max: 10}}}
  - github: {budget: {bucket: core, remaining: {min: 4000}}}
  - github: {pull: {repo: example/app, number: 4, state: merged, merged_into: main}}
  - github: {issue: {repo: example/app, number: 1, state: closed, open_pulls: 0}}
```

| Kind | Checks |
| --- | --- |
| `calls` | the calls matching `method`, `path`, `operation`, `caller`, `via`, `status`, `conditional` and `fault` number `min` to `max` |
| `budget` | what `caller` (everyone when empty) `spent` of `bucket`, and what `remaining` of it |
| `pull` | a pull request (`number`, or the newest of `head`): `state`, `draft`, `labels`, `checks`, `review_decision`, `merged_into`, `comments` |
| `issue` | an issue: `state`, `labels`, `board`, `comments`, and the `pulls` and `open_pulls` that close it |

A number is exact (`spent: 1`) or a range (`spent: {min: 1, max: 5}`). A `github` expectation is
observed for every step kind, in `then`, inline and in `wait_for`. The call log is in the
scenario's artifacts as `github.jsonl`.

## The contract with the real gh

`make test-gh-contract` runs the cases of `contract_test.go` through the fake `gh` and through the
real `gh` against the same server (as its local instance, proxied to the test's port), and fails
when output or exit code differ. It needs the real `gh` on `PATH` and reaches nothing else. It
runs on demand and in the `scenario-gh-contract` workflow, by hand or when a pull request changes
the fake.
