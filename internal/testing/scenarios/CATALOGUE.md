# Phase 1 scenario catalogue

Generated from the scenario files; don't edit by hand. 128 scenarios. The format and vocabulary are in `docs/phase1/scenarios.md`.

| Concept | Scenarios |
|---|---|
| [`board`](#board) | 20 |
| [`eligibility`](#eligibility) | 13 |
| [`epic`](#epic) | 4 |
| [`pick`](#pick) | 8 |
| [`gate`](#gate) | 7 |
| [`decision`](#decision) | 9 |
| [`wait`](#wait) | 20 |
| [`source`](#source) | 8 |
| [`budget`](#budget) | 10 |
| [`webhook`](#webhook) | 11 |
| [`invariant`](#invariant) | 8 |
| [`history`](#history) | 4 |
| [`leader`](#leader) | 4 |
| [`shadow`](#shadow) | 2 |

## board

- **[board-assignee-change-observed](board/board-assignee-change-observed.yaml)** `human-edits`\
  Given a ready, unassigned Up Next item that is eligible When a person assigns it to carol, who is
  not a configured Person (issues.assigned) Then the Item's assignees are [carol] and its
  eligibility is recomputed to AssignedToNonParticipant, recorded once through the webhook.
- **[board-dependency-link-observed](board/board-dependency-link-observed.yaml)** `links`\
  Given an eligible Up Next item and an open issue it doesn't depend on yet When a person marks the
  item as blocked by that issue (issue_dependencies blocked_by_added), and later removes the
  dependency again Then blockedBy lists the open blocker and eligibility is BlockedByOpenIssue, and
  after the removal blockedBy is empty and the item is eligible again.
- **[board-filter-option-checked-at-startup](board/board-filter-option-checked-at-startup.yaml)** `startup` `configuration`\
  Given the team's boardOption is misspelt as "Bumblebee 🐝" (with a space), and the board has a
  Bumblebee🐝 item in Up Next When the engine starts, checks the option against the board schema,
  and its resync interval passes Then it doesn't silently observe nothing: the Installation
  condition BoardFilterValid is False naming the unknown option, the board source is degraded
  (filter-invalid), no Items exist, and no board query runs with the bad filter. When the
  configuration is fixed, the condition turns True and the team's items are observed.
- **[board-issue-closed-means-done](board/board-issue-closed-means-done.yaml)** `lifecycle`\
  Given alice's In Progress item with no PR, in flight and with no wait (nothing is pending that the
  engine observes) When a person closes the issue but leaves the board Status at In Progress Then
  the Item's phase is Done (the closed issue wins over the Status) and it no longer counts as in
  flight.
- **[board-item-archived-leaves-scope](board/board-item-archived-leaves-scope.yaml)** `scope`\
  Given a Bumblebee item in Up Next, assigned to alice and blocked by an open issue, so it waits on
  that dependency When a person archives its board item (projects_v2_item.archived) Then the Item is
  deleted at once, with one item.removed event (reason archived), and so is its wait.
- **[board-item-in-team-becomes-item](board/board-item-in-team-becomes-item.yaml)** `scope`\
  Given an open issue that is not on the board yet When a person adds it to the board as a Bumblebee
  story in Up Next (projects_v2_item.created) Then the engine resolves the board item, and an Item
  exists with the issue and board fields, class Implementable, phase Ready, observed through the
  webhook.
- **[board-item-leaves-team-scope](board/board-item-leaves-team-scope.yaml)** `scope` `wake-index`\
  Given an In Progress Bumblebee item whose PR waits on CI for its head SHA When a person changes
  the item's Team field from Bumblebee to Atlas Then the Item is deleted at once with one
  item.removed event (reason team-changed), its Change goes with it, and their waits leave the wake
  index, so a later check_run on that head SHA reconciles nothing and costs no reads.
- **[board-item-of-other-team-ignored](board/board-item-of-other-team-ignored.yaml)** `scope` `learning-api-budget`\
  Given an open issue that is not on the board yet When a person adds it to the board for team
  Atlas, which is not configured Then no Item is created, the engine spends only the one read that
  resolves the board item's node id, and later webhooks for that issue cost no reads at all.
- **[board-kind-mapping](board/board-kind-mapping.yaml)** `kinds`\
  Given Bumblebee items in Up Next of every Kind, and one with no Kind When the engine starts and
  observes them Then Epic maps to Epic; Story, Task, Feature and Bug to Implementable; Alert to
  Incident; Note and no Kind to Ignored. The Alert is ready and has the best priority, yet it is
  never picked: incidents are counted, not handled, in phase 1.
- **[board-long-repo-name-item-tracked](board/board-long-repo-name-item-tracked.yaml)** `review`\
  Given two Up Next ready items of the team, one an issue in a repo whose name is too long for a
  Kubernetes label value (more than 63 bytes with its owner) When the engine starts Then both become
  Ready Items and the team is decided with both: an Item's identity is in its name and annotation,
  never in a label, so a long repo name stops nothing.
- **[board-merged-item-silent-done-observed](board/board-merged-item-silent-done-observed.yaml)** `review`\
  Given an item whose PR alice merged, which closed its issue (Merged, so the filtered board query
  no longer returns it) When someone moves it to Done on the board and GitHub never sends (nor logs)
  the projects_v2_item webhook Then the next board resync reads the Merged item by its node id
  (Merged and Validating items are read every resync until the board says Done), observes Done and
  counts one missed projects_v2_item webhook; the item is retained, no longer in flight, and the
  board source is fresh again after the probe's gap.
- **[board-pr-link-creates-change](board/board-pr-link-creates-change.yaml)** `links`\
  Given an In Progress item with no PR, so its phase is Implementing When alice opens a PR in
  giantswarm/klaus that closes it (pull_request.opened) Then a Change exists for the PR, linked to
  the Item, the Item lists it in its changes, and the Item's phase is Published.
- **[board-pr-outside-org-found-by-resync](board/board-pr-outside-org-found-by-resync.yaml)** `links` `resync`\
  Given an In Progress item and alice's personal public repo, which is outside the App's
  installation (the installation covers the giantswarm org only) When alice opens a PR there that
  closes the item; GitHub sends the App no webhook for it, and none is logged Then the next
  10-minute board resync finds the PR among the item's closing references and creates a Change
  observed by the resync, and this is not counted as a missed webhook, since no webhook could exist.
- **[board-pr-webhook-vouches-only-for-its-link](board/board-pr-webhook-vouches-only-for-its-link.yaml)** `review`\
  Given an Up Next item that alice moves to In Progress, and GitHub never sends (nor logs) the
  projects_v2_item webhook When alice opens a PR that closes the item and its pull_request webhook
  is delivered Then the engine reads the PR's closing issues and re-reads that item, which links the
  PR (a Change) and shows the move; the link came with its webhook, but the move counts as one
  missed projects_v2_item webhook all the same.
- **[board-resync-catches-missed-board-change](board/board-resync-catches-missed-board-change.yaml)** `resync`\
  Given a ready Up Next item assigned to alice When alice moves it to In Progress and GitHub never
  sends the projects_v2_item webhook (nothing is logged either) Then the engine doesn't see it until
  the 10-minute board resync, which updates the Item from the filtered query, and records the change
  as one missed projects_v2_item webhook.
- **[board-resync-reads-only-team-active-statuses](board/board-resync-reads-only-team-active-statuses.yaml)** `resync` `learning-api-budget`\
  Given a board with Bumblebee items in the active statuses, plus a Bumblebee Backlog item and an
  Atlas item in Up Next When the 10-minute board resync runs Then the engine runs one filtered
  GraphQL board query (BoardTeamItems) for the team's open items in the active statuses (and its
  Inbox bugs), never pages the board unfiltered, and stays within a few points; the Backlog story
  and the Atlas item don't become Items.
- **[board-startup-initial-sync](board/board-startup-initial-sync.yaml)** `startup` `learning-api-budget`\
  Given a board with Bumblebee items in each active status (one with an open PR), a Bumblebee
  Backlog item and an Atlas item When the engine starts, and later restarts with nothing changed on
  GitHub Then after the start Items exist for exactly the team's active items, observed by startup,
  for a bounded number of GraphQL points and REST calls; and the restart re-reads with conditional
  REST requests that GitHub answers with free 304s.
- **[board-status-change-is-input](board/board-status-change-is-input.yaml)** `human-edits`\
  Given a ready Up Next item assigned to alice, which is alice's pick When a person moves it back to
  Backlog Then the Item stays (it left the active statuses by status, not by scope) with phase
  Backlog and no wait, it is no longer eligible (NotUpNext), and alice's pick is dropped.
- **[board-sub-issue-link-observed](board/board-sub-issue-link-observed.yaml)** `links`\
  Given a Bumblebee epic and a Bumblebee story that are not linked When a person adds the story as a
  sub-issue of the epic (sub_issues webhook) Then the story's parent is the epic and the epic's
  subItems list the story, both observed through the webhook.
- **[board-unrelated-pr-does-not-mask-missed-board-change](board/board-unrelated-pr-does-not-mask-missed-board-change.yaml)** `review`\
  Given an Up Next item that alice moves to In Progress, and GitHub never sends (nor logs) the
  projects_v2_item webhook When an unrelated PR that closes nothing is opened and its pull_request
  webhook is delivered Then the engine reads only that PR's closing issues (one PullClosingIssues
  query, no board resync), so the item stays as last observed; the 10-minute board resync then
  observes the move and counts it as one missed projects_v2_item webhook.

## eligibility

- **[eligibility-assigned-to-non-participant-never-touched](eligibility/eligibility-assigned-to-non-participant-never-touched.yaml)**\
  Given an Up Next item without acceptance criteria, assigned to carol, who is not a configured
  Person When the engine observes it at startup and through two board resyncs Then it is not
  eligible (AssignedToNonParticipant) and never touched: no pick, no decision point asked about it
  (so no "refine" todo goes to carol), and no would-action of any kind.
- **[eligibility-assigned-to-participant-only-for-them](eligibility/eligibility-assigned-to-participant-only-for-them.yaml)** `pick`\
  Given a ready Up Next item assigned to bob, and alice with free WIP and nothing else to do When
  the engine decides Then the item is eligible and picked for bob only; alice has no pick, and no
  would-action reassigns it.
- **[eligibility-becomes-ready-when-body-edited](eligibility/eligibility-becomes-ready-when-body-edited.yaml)** `ready`\
  Given an Up Next item assigned to alice with a Repo line but no acceptance criteria, so it is
  NotReady with one "refine" would-todo When alice adds acceptance criteria to the body
  (issues.edited) Then ready? is decided again on the new inputs and says "ready", the item is
  eligible, and one eligibility.changed event is recorded from the webhook.
- **[eligibility-bug-needs-up-next](eligibility/eligibility-bug-needs-up-next.yaml)** `bugs`\
  Given no item on the board yet When a person files a new Bug for Bumblebee in Inbox Then the bug
  is classified by bug-severity? (the rule says medium), the only would-action is to set the
  severity to medium, and the bug is not eligible (NotUpNext) and not moved: a medium bug waits for
  a person to put it in Up Next.
- **[eligibility-closed-issue](eligibility/eligibility-closed-issue.yaml)**\
  Given a ready Up Next item assigned to alice, picked for her When a person closes the issue,
  leaving it in Up Next on the board Then the item is not eligible for the single reason Closed, its
  phase is Done, and alice's pick is dropped.
- **[eligibility-epic-itself-not-picked](eligibility/eligibility-epic-itself-not-picked.yaml)** `kinds`\
  Given an unassigned Epic in Up Next with the best priority, and its ready sub-item in Up Next
  assigned to alice When the engine decides Then the Epic is not eligible (KindNotImplementable) and
  is picked for nobody, not even bob who has nothing else to pick; its sub-item is picked for alice.
- **[eligibility-issue-in-code-repo-has-target](eligibility/eligibility-issue-in-code-repo-has-target.yaml)** `ready`\
  Given an Up Next issue that lives in giantswarm/klaus (a code repo) with acceptance criteria and
  no Repo line When the engine observes it Then its target repo is the repo it lives in, ready?
  decides "ready", and the item is eligible.
- **[eligibility-no-target-repo-not-ready](eligibility/eligibility-no-target-repo-not-ready.yaml)** `ready`\
  Given an Up Next issue in giantswarm/roadmap (a planning repo) with acceptance criteria but no
  Repo line, assigned to alice When the engine observes it Then no target repo is known, ready?
  decides "not-ready", the item is NotReady, and alice gets the one "refine" would-todo.
- **[eligibility-not-ready-records-one-todo](eligibility/eligibility-not-ready-records-one-todo.yaml)** `ready`\
  Given two Up Next items with a Repo line but no acceptance criteria, one assigned to alice and one
  unassigned When the engine observes them, and two board resyncs pass with nothing changed Then
  ready? decides "not-ready" once per item, both are NotReady, and each has exactly one would-todo
  "refine with `/gs-plans:gs-plan`": to alice for hers, to team:bumblebee for the unassigned one.
  The resyncs ask nothing again and add no second todo.
- **[eligibility-open-blocker-waits-on-dependency](eligibility/eligibility-open-blocker-waits-on-dependency.yaml)** `wake-index`\
  Given a ready Up Next item assigned to alice that is blocked by an open issue When the engine
  observes it, and later the blocker is closed (issues.closed) Then the item is BlockedByOpenIssue
  with a dependency wait on the blocker (168h deadline, re-observe); closing the blocker wakes it
  through the wake index, the wait ends, and the item is eligible.
- **[eligibility-requires-up-next](eligibility/eligibility-requires-up-next.yaml)**\
  Given two ready Bumblebee stories in Up Next, one assigned to alice and one to bob, each its
  person's pick When people move alice's back to Backlog and bob's to Inbox, and later file a ready
  story straight into Backlog Then both Items stay, in phase Backlog, not eligible for the single
  reason NotUpNext; nobody has a pick and no would-action is recorded after the moves:
  prioritisation stays with people. The story filed into Backlog never becomes an Item.
- **[eligibility-team-not-opted-in-for-work](eligibility/eligibility-team-not-opted-in-for-work.yaml)** `configuration`\
  Given a ready Up Next item assigned to alice, eligible and picked for her When the configuration
  is changed so the team opts in to no kinds of work (optIn.work: []) Then the item is still
  observed but not eligible (TeamNotOptedIn), nobody has a pick, and no further would-action is
  recorded.
- **[eligibility-up-next-ready-item-is-eligible](eligibility/eligibility-up-next-ready-item-is-eligible.yaml)** `ready`\
  Given an unassigned Story in Bumblebee's Up Next with acceptance criteria and a Repo line When the
  engine observes it Then ready? decides "ready" (JEV asked once and agreeing), the Item knows its
  target repo and acceptance criteria, and it is eligible with no reasons.

## epic

- **[epic-active-when-a-sub-item-passes-ready](epic/epic-active-when-a-sub-item-passes-ready.yaml)**\
  Given a Planned Bumblebee epic with two sub-items in Up Next When alice moves one sub-item to In
  Progress Then the epic is Active, through the sub-item's webhook, and the sub-item is in flight;
  since the engine started none of its sub-items, the epic takes no slot of the engine's epic WIP
  (status.wip.epics stays 0).
- **[epic-done-when-all-sub-items-done](epic/epic-done-when-all-sub-items-done.yaml)**\
  Given an Active Bumblebee epic whose two sub-items are In Progress When a person closes one
  sub-item's issue, and later moves the other to Done while its issue stays open Then the epic stays
  Active after the first and is Done after the second.
- **[epic-planned-with-no-started-sub-items](epic/epic-planned-with-no-started-sub-items.yaml)**\
  Given a Bumblebee epic with two sub-items, both still in Up Next When the engine observes them
  Then the epic is Planned and lists both sub-items.
- **[epic-sub-items-inherit-owner](epic/epic-sub-items-inherit-owner.yaml)** `pick`\
  Given a Bumblebee epic assigned to alice and its ready, unassigned sub-item in Up Next, with alice
  and bob both free When the engine decides Then the sub-item's owner is alice, it is picked for
  alice (not bob), and its would-assign names alice.

## pick

- **[pick-active-epic-items-first](pick/pick-active-epic-items-first.yaml)**\
  Given an Active epic owned by alice (one sub-item done) with an unassigned sub-item in Up Next
  that has no Priority, and an older standalone item with Priority 1 When the engine orders the
  team's eligible items Then the Active epic's sub-item ranks first and is alice's pick, ahead of
  the higher-priority standalone item, which goes to bob: finishing comes before starting.
- **[pick-assigned-item-only-for-its-assignee](pick/pick-assigned-item-only-for-its-assignee.yaml)**\
  Given an eligible Up Next item assigned to bob that is first in order, and an unassigned item
  after it When the engine picks for alice first (login order) Then alice gets the unassigned item,
  never bob's, even though bob's ranks higher; bob's item is picked for bob, and since it's already
  his, its would-actions are setting it In Progress and starting the run, with no assign.
- **[pick-epic-sub-items-only-for-epic-owner](pick/pick-epic-sub-items-only-for-epic-owner.yaml)**\
  Given an Active epic owned by alice with two unassigned sub-items in Up Next (#111, Priority 1,
  and #112, Priority 2), which rank above a standalone item (#102, Priority 1) When the engine picks
  Then alice's one item of WIP goes to #111; #112 still ranks above #102 but is only for alice, so
  bob gets #102, never her epic's sub-item, and #112 is neither picked nor held.
- **[pick-order-priority-then-age](pick/pick-order-priority-then-age.yaml)**\
  Given four eligible unassigned items: #101 Priority 1 (new), #102 Priority 2 (older), #103
  Priority 2 (newer) and #104 without a Priority (the oldest) When the engine picks, and then a
  person moves the top item back to Backlog, twice Then the order is Priority 1 before 2, older
  before newer within a priority, and unset Priority last however old: alice and bob take #101 and #102,
  then #102 and #103, then #103 and #104.
- **[pick-recomputed-when-priority-changes](pick/pick-recomputed-when-priority-changes.yaml)** `human-edits`\
  Given two eligible unassigned items, #101 (Priority 2) picked for alice and #102 (Priority 3)
  picked for bob When a person sets #102's Priority to 1 on the board Then the engine reorders from
  the webhook: #102 ranks first and becomes alice's pick, #101 becomes bob's, and each changed pick
  is recorded once.
- **[pick-recorded-once-not-executed](pick/pick-recorded-once-not-executed.yaml)**\
  Given one eligible unassigned item, picked for alice, and an Up Next item of the team assigned to
  carol (not a Person, so never eligible) When the board resync runs, carol's item changes, and the
  picked item is edited in a way that doesn't change the pick Then the pick is recorded once (one
  shadow.pick event, its would-actions once) and shown on the Person and the Team; the engine
  decides again each time but adds no second pick event or would-action while the pick is unchanged.
- **[pick-unassigned-items-for-free-people](pick/pick-unassigned-items-for-free-people.yaml)**\
  Given alice and bob, each with the default WIP of one item and no engine WIP, and two eligible
  unassigned items in Up Next When the engine decides Then the first item in order is picked for
  alice and the next for bob (people in login order), each pick records the would-actions "assign
  the person", "set In Progress" and "start an implement run", and nothing is written: both items
  stay unassigned in Up Next.
- **[pick-unblocking-work-first](pick/pick-unblocking-work-first.yaml)**\
  Given bob's In Progress item (in flight), blocked by an open Up Next item (#120, no Priority, the
  newest), and an Active epic owned by alice with an unassigned Up Next sub-item (#111, Priority 1,
  older) When the engine orders the team's eligible items Then the item that unblocks in-flight work
  ranks first, ahead of the Active epic's sub-item, and is alice's pick.

## gate

- **[gate-epic-wip-holds-new-epic](gate/gate-epic-wip-holds-new-epic.yaml)**\
  Given alice with a WIP limit of 2 items and 1 epic, two Planned epics she owns (#90 with sub-item #111,
  Priority 1; #95 with sub-item #121, Priority 2), and a standalone item assigned to her (#102,
  Priority 3) When the engine picks Then #111 is picked and opens epic #90, which takes her epic
  slot; #121 would open a second epic, so it is held for her by EpicWIP and skipped, and her next
  startable item, #102, is picked instead.
- **[gate-limit-change-applies-at-once](gate/gate-limit-change-applies-at-once.yaml)**\
  Given a team cap of 1 item, taken by alice's pick (#101), and bob's top candidate (#102) held for
  him by TeamCap When the configuration raises the team cap to 2 (applied as Flux would, with no
  restart and no clock advance) Then #102 is no longer held and is picked for bob at once, with its
  would-actions.
- **[gate-person-wip-bounds-picks](gate/gate-person-wip-bounds-picks.yaml)**\
  Given alice with a WIP limit of 2 items, bob with the default 1, no team cap, and four eligible
  unassigned items, each in its own repo (#101 to #104, Priority 1 to 4) When the engine picks Then
  the participants take turns in login order until their WIP is used: alice gets #101 and #103, bob
  gets #102, each with its would-actions; #104 is neither picked nor held, because a person's own
  WIP limit only bounds the number of picks and never shows as a hold.
- **[gate-repo-concurrency-disjoint-footprints-pass](gate/gate-repo-concurrency-disjoint-footprints-pass.yaml)**\
  Given two eligible unassigned klaus items, #101 (Priority 1, `Footprint: pkg/client`) and #102
  (Priority 2, `Footprint: pkg/metrics`) When the engine picks #101 for alice, which takes the repo,
  and then considers #102 for bob Then `parallel-safe?` is decided "yes" (both name footprints and
  they are disjoint) and recorded, and #102 is picked for bob, not held, with its would-actions.
- **[gate-repo-concurrency-sequential-by-default](gate/gate-repo-concurrency-sequential-by-default.yaml)**\
  Given three eligible unassigned items, none naming a footprint: #101 and #102 in giantswarm/klaus
  (Priority 1 and 2) and #103 in giantswarm/muster (Priority 3) When the engine picks #101 for
  alice, which takes klaus, and then picks for bob Then `parallel-safe?` is decided "no" for #102 by
  the rule (sequential per repo by default) and recorded; #102 is held for bob by RepoConcurrency
  and skipped, and bob's next startable item, #103, is picked instead.
- **[gate-team-cap-holds-picks](gate/gate-team-cap-holds-picks.yaml)**\
  Given a team cap of 1 item, alice and bob with the default WIP of 1, and two eligible unassigned
  items in different repos (#101 Priority 1, #102 Priority 2) When the engine picks Then alice,
  first in turn, gets #101, which takes the cap; bob gets no pick, and his top candidate #102 is
  recorded as held for him by TeamCap: on the Item, the Person and the Team, with one shadow.held
  event and no would-action.
- **[gate-unblocking-work-exempt](gate/gate-unblocking-work-exempt.yaml)** `learning-gate-blocks-repair`\
  Given a team cap of 1 item; bob's In Progress muster item (#100, in flight), blocked by an open Up
  Next klaus item assigned to bob (#120); and an eligible unassigned klaus item (#101, Priority 1)
  When the engine picks, alice first: she gets #101, which takes the team cap and the klaus repo
  Then #120 ranks first as work that unblocks in-flight work and is still picked for bob, held by no
  gate (neither TeamCap nor RepoConcurrency); nothing is held, and gate-not-holding-repair stays 0.

## decision

- **[decision-bug-severity-shadow](decision/decision-bug-severity-shadow.yaml)**\
  Given `bug-severity?` in mode shadow and JEV that will classify the next bug as critical When a
  new Bug is created in the team's Inbox Then the rule's answer (medium) is chosen and JEV's
  (critical) recorded next to it, the only would-action is writing severity medium, and the bug is
  not moved to Up Next: it stays in the Inbox, not eligible.
- **[decision-jev-asked-once-per-inputs](decision/decision-jev-asked-once-per-inputs.yaml)**\
  Given two ready items whose `ready?` JEV was asked once each at startup When the board and change
  resyncs run, and webhooks arrive that don't touch the inputs of `ready?` (a Priority change, an
  assignment) Then JEV is not asked again; and when a person edits #101's body (new inputs), JEV is
  asked once more, for #101 only, and a second `ready?` decision is recorded.
- **[decision-jev-error-rule-stands](decision/decision-jev-error-rule-stands.yaml)**\
  Given an eligible ready item and JEV timing out on `ready?` for it When a person edits the body
  (new inputs, so `ready?` is decided again) and the edit keeps it ready Then the decision records
  the JEV timeout as jevError with no JEV answer, the rule's answer (ready) is chosen, outcome
  jev_error is counted, and the item's eligibility doesn't change.
- **[decision-jev-gets-only-needed-fields](decision/decision-jev-gets-only-needed-fields.yaml)**\
  Given two ready items, one assigned to alice and one to bob When the engine asks JEV `ready?` for
  each Then every request carries exactly the item's title and body: no assignees or other logins,
  and nothing of the other item.
- **[decision-jev-low-confidence-recorded](decision/decision-jev-low-confidence-recorded.yaml)**\
  Given a ready item for which JEV answers `ready?` with "not-ready" at confidence 0.3 When the
  engine decides `ready?` for it Then the answer and its confidence are recorded, the outcome
  counted is jev_low_confidence (not disagree), and the rule's answer stands: the item is eligible.
- **[decision-mode-rule-skips-jev](decision/decision-mode-rule-skips-jev.yaml)**\
  Given `ready?` configured in mode rule (the other points stay in shadow) and a ready item When the
  engine decides `ready?` at startup and again after a body edit Then JEV is never asked `ready?`,
  the decisions are recorded in mode rule with no JEV answer, and `next?` is still asked of JEV.
- **[decision-next-shadow-recorded](decision/decision-next-shadow-recorded.yaml)**\
  Given `next?` in mode shadow and two eligible items assigned to alice, #101 (Priority 1) and #102
  (Priority 2), so only alice has candidates When the engine decides `next?` for alice and JEV
  prefers #102 Then the rule's answer (#101) is chosen and is alice's pick, JEV's answer is recorded
  next to it, and the disagreement is counted.
- **[decision-ready-shadow-records-both](decision/decision-ready-shadow-records-both.yaml)**\
  Given `ready?` in mode shadow and a ready item (acceptance criteria and a Repo line), for which
  JEV answers "not-ready" with confidence 0.8 When the engine decides `ready?` for it Then the
  rule's answer (ready) is chosen, both answers are recorded on the decision, the item stays
  eligible with no refine todo, and engine_decisions_total{point="ready?",outcome=disagree} is 1.
- **[decision-replay-reproduces-every-decision](decision/decision-replay-reproduces-every-decision.yaml)**\
  Given two ready items, JEV disagreeing on one of them and failing on the other When a body is
  edited, a Priority changes and a new Bug arrives, so `ready?`, `next?` and `bug-severity?` are
  decided several times Then `replay` re-runs every recorded decision through `decide` from its
  recorded inputs (JEV answers and errors included) with 0 mismatches, and calls neither JEV nor
  GitHub.

## wait

- **[wait-changes-requested-means-fixing](wait/wait-changes-requested-means-fixing.yaml)** `wake-index`\
  Given a Published item whose PR is green and waits on review When bob submits a "changes
  requested" review at 09:30, and alice pushes b2 at 10:00 Then on the review the Change records
  review state changes-requested and has no wait (the engine expects nothing of a colleague's fix),
  and the Item moves to Fixing with no wait of its own; the push still wakes the Change through its
  pr: ref, and the Change follows the new head and waits on CI for b2 (since 10:00, deadline 11:00),
  while the open "changes requested" review keeps the Item in Fixing.
- **[wait-ci-deadline-from-observed-durations](wait/wait-ci-deadline-from-observed-durations.yaml)** `deadline`\
  Given observed pipeline durations of 20m, 25m and 30m for giantswarm/klaus (as many as minHistory
  requires) and a PR there waiting on CI since 09:00 When the engine sets the ci wait's deadline
  Then the deadline is deadlineFactor × p90 = 3 × 30m (p90 by nearest rank) = 90m after since, not
  the 1h defaultDeadline: nothing fires at 10:29, the deadline fires at 10:30.
- **[wait-ci-wakes-on-check-run](wait/wait-ci-wakes-on-check-run.yaml)** `wake-index`\
  Given an In Progress item whose PR is waiting on CI for its head SHA When GitHub delivers the
  check_run completed webhook for the last required check Then the Change's ci wait ends through the
  webhook, only that Change is reconciled, and the event table records the wake with source webhook.
- **[wait-ci-wakes-on-commit-status](wait/wait-ci-wakes-on-commit-status.yaml)** `wake-index`\
  Given an In Progress item whose PR waits on CI for its head SHA, where CircleCI reports through
  commit statuses (one required context green, the other pending) When GitHub delivers the status
  webhook that turns the last required context green Then the Change's ci wait is woken by that
  webhook and ends: CI is green and the Change now waits on review.
- **[wait-deadline-reobserves-once](wait/wait-deadline-reobserves-once.yaml)** `deadline` `learning-failed-wakeups`\
  Given a Change waiting on CI, with the ci defaultDeadline configured to 4m so that the deadline
  (09:04) comes before the first 5-minute delivery-log check (09:05) and the 15-minute changes
  resync (09:15) When CI completes green but the check_run delivery is lost (GitHub logs it as
  failed) Then at the deadline the engine re-observes exactly once, with a conditional read, finds
  CI green and ends the wait with source reobserve, so it never goes overdue; the change counts as a
  missed check_run webhook.
- **[wait-deadline-unchanged-goes-overdue](wait/wait-deadline-unchanged-goes-overdue.yaml)** `deadline` `learning-failed-wakeups`\
  Given a Change waiting on CI since 09:00 with the default 1h deadline and atDeadline re-observe
  When the deadline passes and CI is still pending on the head SHA Then the engine re-observes once,
  finds nothing changed, and the wait becomes overdue; from then on it re-observes after 1m,
  doubling, capped at overdueBackoffCap (10m): at 10:01, 10:03, 10:07, 10:15, 10:25, 10:35, 10:45,
  10:55 (8 in the first overdue hour), then every 10 minutes (6 in the second), never every minute.
  Nothing is escalated and no would-action is recorded: phase 1 escalates nothing.
- **[wait-delayed-delivery-processed-once](wait/wait-delayed-delivery-processed-once.yaml)** `wake-index`\
  Given a Change waiting on CI When CI completes green at 09:00 but GitHub delays the check_run
  delivery by 20 minutes Then the 09:15 changes resync observes the green CI first and ends the wait
  (one wake, source resync); when the delivery arrives at 09:20 it is accepted but changes nothing:
  no second wake, no new observation, and the Change still waits on review.
- **[wait-dependency-wakes-on-blocker-closed](wait/wait-dependency-wakes-on-blocker-closed.yaml)** `wake-index`\
  Given two ready Up Next items, #101 blocked by the open issue #100 and #102 blocked by the open
  issue #103, each holding a dependency wait on its blocker When #100 is closed Then the issues
  webhook wakes only #101: its blocker is recorded closed, its dependency wait ends and it becomes
  eligible; #102 is not reconciled and still waits on #103.
- **[wait-lost-webhook-woken-by-deadline](wait/wait-lost-webhook-woken-by-deadline.yaml)** `deadline` `learning-waits-dont-wake`\
  Given a Change waiting on CI in giantswarm/klaus, whose observed pipelines take 4m, so the ci
  deadline is 3 × 4m = 12m (09:12), earlier than the 15-minute changes resync When CI completes
  green and GitHub never sends the check_run webhook (so it is not in the delivery log either) Then
  the delivery-log checks at 09:05 and 09:10 see no gap and the webhooks source stays fresh, and at
  the deadline the re-observe finds CI green and ends the wait; the change counts as a missed
  check_run webhook.
- **[wait-lost-webhook-woken-by-resync](wait/wait-lost-webhook-woken-by-resync.yaml)** `learning-waits-dont-wake`\
  Given a Change waiting on CI with the default 1h deadline When CI completes green and GitHub never
  sends the check_run webhook Then the 15-minute changes resync (09:15) finds CI green, long before
  the deadline, and ends the wait with source resync; the change counts as a missed check_run
  webhook and no deadline is involved.
- **[wait-merge-result-ignores-irrelevant-check](wait/wait-merge-result-ignores-irrelevant-check.yaml)** `merge-result` `learning-false-alarm`\
  Given an approved, green PR in giantswarm/muster (required set: "ci/circleci: go-test") that alice
  merges at 09:00 as m1 When on m1 the board automation check "add-to-project", which is not in the
  required set, fails first, and the required status then passes Then the failing check never turns
  anything red: while the required status is pending the merge result stays pending and the Change's
  ci wait on m1 keeps watching, then mergeResult is green, with "add-to-project" in
  mergeResult.ignored, not in mergeResult.relevant. The Change's ci keeps describing the head SHA a1
  throughout, and once the merge result is in neither the Change nor the Merged Item has a wait.
- **[wait-merge-result-never-reported-violates-invariant](wait/wait-merge-result-never-reported-violates-invariant.yaml)** `merge-result`\
  Given an approved, green PR in giantswarm/muster that alice merges at 09:00 as m1 When no check
  ever reports on m1, up to and including the re-observe at the mergeResult deadline (11:00) Then
  the invariant merge-reaches-result-check holds until the deadline and is violated (1) once the
  deadline's single re-observe finds no result; the wait becomes overdue.
- **[wait-merge-result-observed](wait/wait-merge-result-observed.yaml)** `merge-result`\
  Given an approved PR in giantswarm/muster that is green on its head SHA (Item phase Merging); the
  repo's only required check is "ci/circleci: go-test" When alice merges it at 09:00 (merge commit
  m1), and at 09:20 the required status on m1 turns green Then before the merge neither the Merging
  Item nor its Change has a wait (nothing is pending until someone merges); the merge is observed:
  the Change is merged, its ci keeps the head's verdict (a1, green) while it waits on the merge
  commit with the mergeResult deadline (2h, atDeadline re-observe), and the Item is Merged with no
  wait of its own; the green status on m1 makes mergeResult green and ends the Change's wait, and
  the Item stays Merged with no wait until the board says Validation or Done.
- **[wait-merge-result-watched-after-hand-merge](wait/wait-merge-result-watched-after-hand-merge.yaml)** `merge-result` `learning-hand-merge`\
  Given a PR in giantswarm/muster whose required check is red on its head SHA a1 (Item phase Fixing,
  no wait on the Item or the Change) When alice merges it anyway at 09:00 (merge commit m1, past the
  red check), and the required check then fails on m1 too Then the hand merge is observed like any
  other change (one observed row, from the webhook), the Change's ci keeps the red head verdict it
  went past (a1, red), the merge-result wait on m1 is created and watched as for any merge, and the
  red merge result is seen: main is watched no matter who merged. With the merge result in, the
  Merged Item and its Change have no wait.
- **[wait-record-has-deadline-and-action](wait/wait-record-has-deadline-and-action.yaml)** `deadline`\
  Given an In Progress item in giantswarm/klaus and no observed pipeline durations for that repo
  When its PR is opened at 09:07 and CI has not finished on the head SHA Then the Change carries a
  ci wait on that SHA with since = the engine clock at the observation, deadline = since + 1h (the
  configured defaultDeadline, used while there is no history) and atDeadline re-observe, and a
  wait.started row records it.
- **[wait-red-ci-then-push-waits-on-new-sha](wait/wait-red-ci-then-push-waits-on-new-sha.yaml)**\
  Given a Published item whose PR waits on CI for head SHA a1 When a required check fails on a1 at
  09:10, and alice pushes b2 at 09:30 Then on red the Change's ci wait ends (red is final for a1)
  and the Item moves to Fixing, with no wait on either (nothing is pending until a new head is
  pushed); on the push the Change follows the new head and waits on CI for b2 (since 09:30, deadline
  10:30), and the Item is Published again with no wait of its own.
- **[wait-review-wakes-on-approval](wait/wait-review-wakes-on-approval.yaml)** `wake-index`\
  Given a Published item whose PR is green on its head SHA and waits on review (the repo's ruleset
  requires one approval; author and reviewer are both on the owning team) When bob approves the PR
  Then the pull_request_review webhook wakes the review wait, the Change records the approval and
  has no wait any more, and the Item moves to Merging with no wait of its own: nothing is pending
  until someone merges.
- **[wait-survives-restart](wait/wait-survives-restart.yaml)** `deadline`\
  Given a Change waiting on CI since 09:00 with its deadline at 10:00 When the engine restarts at
  09:30 Then the wait is kept as it was on the Change (same since and deadline, no new wait
  started), and at 10:00 its deadline still fires: one re-observe and, CI being unchanged, the wait
  becomes overdue.
- **[wait-unobserved-when-reobserve-fails](wait/wait-unobserved-when-reobserve-fails.yaml)** `deadline` `learning-unobserved-not-stalled`\
  Given a Change waiting on CI with its deadline at 10:00 and atDeadline re-observe When GitHub
  answers every read of giantswarm/klaus with 502 from 09:59 until 10:10, so the re-observe at the
  deadline fails Then the wait shows unobserved with reason http-502, not overdue, it is not treated
  as a refusal, and wait-within-deadline holds past deadline plus grace; once GitHub recovers, the
  next transient retry (10s, doubling, capped at 5m: 10:10:10) re-observes, finds CI still pending,
  and only then handles the deadline, once: the wait becomes overdue.
- **[wait-unrelated-webhook-wakes-nothing](wait/wait-unrelated-webhook-wakes-nothing.yaml)** `wake-index`\
  Given a Change waiting on CI for giantswarm/klaus@a1 When a status webhook arrives for
  giantswarm/klaus@f9, a SHA nobody waits on Then the wake index maps it to no waiter: nothing is
  reconciled, no GitHub call is made, and the waiting Change is unchanged.

## source

- **[source-blind-45-minutes-catches-up-everything](source/source-blind-45-minutes-catches-up-everything.yaml)** `learning-blind-watcher` `learning-unobserved-not-stalled`\
  Given two In Progress items with open PRs waiting on CI (klaus#55 and muster#12) When every
  webhook delivery fails for 45 minutes (GitHub logs each one as failed) while CI turns green and
  red, the authors push, and reviews request changes Then the 09:05 delivery-log check degrades the
  webhooks source (deliveries-failed) and its waiters poll every 5 minutes from 09:10; each change
  is observed within one interval of happening (each is checked exactly 5 minutes after it, by the
  log check, whose failed deliveries name the refs to re-observe, and by the polls), including
  changes to a Change that holds no wait (red CI, changes requested); the polled waits keep
  watching, so nothing is unobserved or overdue; every lost delivery counts once as a missed
  webhook; and after deliveries succeed again, the first clean check with a successful delivery
  since the gap (09:50) makes the source fresh and polling stops.
- **[source-blind-webhooks-degrade-and-poll](source/source-blind-webhooks-degrade-and-poll.yaml)** `learning-blind-watcher` `learning-unobserved-not-stalled`\
  Given a Change waiting on CI and webhooks that are all failing to reach the engine (GitHub logs
  every delivery as failed, as when the receiver is unreachable) When CI completes and the 09:05
  delivery-log check runs Then the webhooks source is marked degraded (deliveries-failed), the
  failed delivery's payload names the SHA to re-observe so the green CI is seen at once, its waiters
  poll with conditional requests from 09:10 (one polling interval later), nothing is declared
  stalled (no wait goes overdue), and once deliveries succeed again the first clean check with a
  successful delivery since the gap (09:15) makes the source fresh and polling stops.
- **[source-board-5xx-degrades-at-first-failed-check](source/source-board-5xx-degrades-at-first-failed-check.yaml)** `review`\
  Given a fresh board source When every BoardProbe and BoardTeamItems call answers 502 from 09:00 on
  Then the board source degrades (board-unreadable) at the 09:10 checks, the first failed scheduled
  checks, and polls; the webhooks source stays fresh. When GitHub answers again, the board is
  resynced and fresh again after a full probe interval without a gap.
- **[source-degraded-board-polls-then-recovers](source/source-degraded-board-polls-then-recovers.yaml)**\
  Given two In Progress items and a board change whose webhook GitHub never sends, so the 09:10
  board probe marks the board source degraded When a second unannounced change follows at 09:11 and
  is found by the 09:20 probe, and the 09:30 probe then finds no new gap Then the board is degraded
  (probe-gap) from 09:10 and, from one polling interval later (09:15), its filtered board query runs
  every 5 minutes instead of every 10; the second change is caught by a poll, and once the board is
  fresh again the query is back to every 10 minutes. (Windows are half-open and as long as a
  multiple of the period, so the counts don't depend on where the timers sit.)
- **[source-delivery-log-unreadable-means-degraded](source/source-delivery-log-unreadable-means-degraded.yaml)** `learning-unobserved-not-stalled`\
  Given a Change waiting on CI and webhooks that are being delivered normally When the webhook
  delivery log starts answering 502, so the 5-minute check can't tell whether any delivery failed
  Then freshness can't be verified, so at the first failed check (09:05) the webhooks source is
  degraded with reason delivery-log-unreadable (once, not again at every failed check) and from
  09:10 its waiters poll with conditional requests; an unverifiable log is not counted as missed
  webhooks; and once the log is readable again and shows no failed delivery, and a delivery has
  succeeded since the gap, the source is fresh and polling stops.
- **[source-fresh-on-a-quiet-day](source/source-fresh-on-a-quiet-day.yaml)** `learning-unobserved-not-stalled`\
  Given an item whose PR is green and waiting on review, and an Up Next item blocked by an open
  issue, with nothing failing in the webhook delivery log When six hours pass without a single
  change, so GitHub sends no deliveries at all Then silence is not blindness: the delivery log and
  the board probe keep being checked, both sources stay fresh, both waits (review, dependency) keep
  watching without polling or showing unobserved, and no invariant fires.
- **[source-missed-delivery-reread-from-log](source/source-missed-delivery-reread-from-log.yaml)**\
  Given two PRs in giantswarm/klaus waiting on CI, klaus#55 on a1 and klaus#56 on b1 When CI
  completes on a1 and GitHub logs that check_run delivery as failed Then at the next delivery-log
  check the engine reads that one delivery from the log (a GET of the delivery, to learn from its
  payload what to re-observe), re-reads only the ref it names (CI on a1), and wakes klaus#55 as a
  re-observe; it doesn't redeliver (no POST, since phase 1 writes nothing to GitHub) and doesn't
  touch klaus#56. The degraded source's regular polling only starts one polling interval later.
- **[source-silent-gap-found-by-board-probe](source/source-silent-gap-found-by-board-probe.yaml)**\
  Given an In Progress item with no PR yet (Implementing) When someone moves it to Validation on the
  board and GitHub never sends the projects_v2_item webhook (nothing is sent and nothing is logged,
  so the delivery log can't show the gap) Then the 10-minute board probe sees a board change newer
  than anything the board webhooks reported and marks the board source degraded while the webhooks
  source stays fresh, the board resync catches the item up, and the change is recorded as a missed
  webhook.

## budget

- **[budget-304-costs-nothing](budget/budget-304-costs-nothing.yaml)**\
  Given a PR that is green and waiting on review, already read once with an ETag When a Changes
  resync re-reads it and nothing has changed, and later a review arrives and the engine reads what
  changed Then the resync's read is conditional and answered 304, and the Budget's REST remaining
  (taken from the rate-limit headers) is unchanged after it; the reads after the review are answered
  200 and do lower it, so the Budget is not simply standing still.
- **[budget-5xx-backs-off-on-engine-clock](budget/budget-5xx-backs-off-on-engine-clock.yaml)** `learning-failed-wakeups`\
  Given a Change waiting on CI When CI completes, the webhook arrives, and GitHub answers the next
  three reads of the check runs with 502 Then the engine never retries at once: each retry is a
  timer on the engine's clock, and the gaps grow (10s, doubling, capped at 5m, no jitter in tests):
  the reads happen at 09:00:00 (502), 09:00:10 (502), 09:00:30 (502) and 09:01:10, which succeeds.
  Four reads in all; a 5xx is transient, so it is not recorded as a refusal.
- **[budget-calls-labelled-by-class](budget/budget-calls-labelled-by-class.yaml)**\
  Given a PR that is green and waiting on review, and other users of the same GitHub App who have
  brought its REST remaining down to 3,000 When the engine's resyncs and freshness checks run for
  half an hour Then every call is counted in engine_github_requests_total under the observation
  class with its API (rest, graphql, or app for the delivery log read with the App's JWT), operation
  (GitHub's REST operation id, or the GraphQL operation name), status code and whether it was
  conditional, GraphQL points are counted per class, and the Budget shows the remaining and reset
  time from GitHub's rate-limit headers, not from the engine's own count.
- **[budget-graphql-exhausted-pauses-board](budget/budget-graphql-exhausted-pauses-board.yaml)**\
  Given an In Progress item with no PR yet (Implementing), and a GraphQL budget that is exhausted
  until 09:30 (the engine doesn't know yet) When the item is moved to Validation and its
  projects_v2_item webhook makes the engine read it over GraphQL Then exactly one GraphQL call
  (BoardItemsByNode) fails as rate limited; a primary rate limit stops that API's bucket, so from
  then on the engine makes no GraphQL call (no board resync, no probe, no retry) until the reset
  time on its clock; the board source is degraded with reason budget and the item stays as last
  observed; the delivery log, on the App's own bucket, carries on. When the reset passes, board
  reads resume and the item catches up.
- **[budget-idle-hour-costs-almost-nothing](budget/budget-idle-hour-costs-almost-nothing.yaml)** `learning-api-budget`\
  Given a steady team: three In Progress items, two of them with open PRs that are green and waiting
  on review, and the engine past its startup reads (so it holds an ETag for everything it reads over
  REST) When an hour passes in which nothing changes on GitHub Then every REST read is conditional
  and answered 304, so the App's REST remaining doesn't move, and the GraphQL spend is only the
  safety net's. The bounds, per hour: - REST: 4 Changes resyncs (every 15m) of at most 4 reads per
  PR (the PR, its head's check runs, its head's statuses, its reviews): at most 4 x 2 x 4 = 32
  calls, all 304s. - The delivery log, read with the App's JWT on its own bucket (api app): 12
  reads. - GraphQL: 6 board resyncs (every 10m), each one filtered query of at most 2 points (the
  bound in board-resync-reads-only-team-active-statuses), and 6 board probes of 1 point: 12 queries
  and at most 6 x 2 + 6 x 1 = 18 points, under 0.4% of the 5,000-point hourly budget.
- **[budget-refusal-not-retried-until-inputs-change](budget/budget-refusal-not-retried-until-inputs-change.yaml)** `learning-repeated-refusal`\
  Given an In Progress item closed by a PR in giantswarm/happa, a private repo the App is not
  installed on, so reading the PR is answered 404 When the engine reads it at startup, and then an
  hour of board and Changes resyncs pass with nothing changed Then the 404 is recorded once as a
  refusal with its inputs, shown on the Change while it stands, and never retried on those unchanged
  inputs; when the repo is added to the installation (an installation_repositories webhook, one of
  the inputs), the PR is read again, succeeds, the Change no longer shows a refusal, and no new
  refusal is recorded.
- **[budget-refused-board-query-not-retried](budget/budget-refused-board-query-not-retried.yaml)** `review` `learning-repeated-refusal`\
  Given a fresh board source, and GitHub refusing the team's board query (404) When the 09:10 board
  resync is refused and an hour passes with nothing changing Then the refusal is recorded once and
  the query isn't sent again on unchanged inputs: one BoardTeamItems call in the hour, one refusal
  counted, and the board source stays degraded (board-unreadable) while the team is still decided on
  what was observed. When GitHub answers again and the App is installed on another repo, the next
  resync sends the query, and the board source is fresh after a full probe interval without a gap.
- **[budget-reserve-defers-observation](budget/budget-reserve-defers-observation.yaml)** `learning-api-budget`\
  Given a Change waiting on CI, and other users of the same GitHub App who leave only 900 of the
  5,000 REST calls, below the 20% observation reserve, until 09:30 When the engine learns this from
  the rate-limit headers of its next REST read (the 09:15 Changes resync; the delivery log is read
  on the App's own bucket), and CI then completes and its webhook arrives Then REST observation is
  deferred until the reset on the engine's clock: the Budget shows it deferred, the wait the webhook
  woke shows unobserved with reason budget, and no REST call is made; GraphQL and the delivery log,
  tracked separately, carry on, so the webhooks source stays fresh. When the reset time passes the
  deferred reads run, the CI result is seen, and the delivered webhook is not counted as missed.
- **[budget-secondary-rate-limit-honours-retry-after](budget/budget-secondary-rate-limit-honours-retry-after.yaml)**\
  Given a Change waiting on CI, at 09:04:30 When CI completes, the webhook arrives, and the engine's
  read of the check runs is answered 403 with a secondary rate limit and Retry-After: 60 Then a
  secondary rate limit pauses every REST call, with the installation's token and with the App's JWT
  alike, until 60 seconds have passed on the engine's clock (the delivery-log check due at 09:05
  waits too); at 09:05:30 it retries the read exactly once, it succeeds, and the CI result is seen.
  The 403 is not a refusal.
- **[budget-startup-cost-bounded](budget/budget-startup-cost-bounded.yaml)** `learning-api-budget`\
  Given a team with 20 items on the board, 10 In Progress with an open PR each (5 in
  giantswarm/klaus, 5 in giantswarm/muster) and 10 in Up Next When the engine starts and makes its
  initial sync Then every item and PR is observed, and the sync costs at most: - GraphQL: 3 queries
  and 4 points: the board schema check (1 point), one page of the filtered board query (at most 2
  points, as in board-startup-initial-sync; 20 items fit in one page of 100, and GitHub prices a
  query by its shape, not by the items returned) and at most one probe (1 point). - REST: 43 calls:
  4 per PR (the PR, its head's check runs, its head's statuses, its reviews) for 40, plus at most 3
  installation-wide reads (the installation's repositories and the merge rules of the two code
  repos). - The delivery log, read with the App's JWT on its own bucket: 1 call. Issues come with
  the board query: there is no REST read per issue.

## webhook

- **[webhook-invalid-signature-rejected](webhook/webhook-invalid-signature-rejected.yaml)**\
  Given a Change waiting on CI whose last required check has completed on GitHub, but whose webhook
  GitHub hasn't sent When a check_run delivery for that SHA is POSTed with an X-Hub-Signature-256
  that doesn't match the App's webhook secret Then the receiver answers 401 and counts it as
  bad_signature, and nothing is processed: no GitHub read, no reconcile, and the Change still waits
  on CI.
- **[webhook-missing-signature-rejected](webhook/webhook-missing-signature-rejected.yaml)**\
  Given an Up Next item on the board When an issues delivery for it is POSTed to /github/webhook
  without an X-Hub-Signature-256 header Then the receiver answers 401 and counts it as
  missing_signature, and nothing is processed: no GitHub read and no reconcile.
- **[webhook-nul-escape-in-payload-stored](webhook/webhook-nul-escape-in-payload-stored.yaml)** `review`\
  Given a Change waiting on CI whose last required check has completed on GitHub, but whose webhook
  GitHub hasn't sent When the validly signed check_run delivery is POSTed and its payload carries a
  NUL character as the JSON escape \u0000 (in the check run's output, which GitHub copies verbatim
  from the CI tool) Then the receiver stores the payload as sent and answers 2xx, counted accepted
  and never unavailable, and the engine processes it: it wakes the waiting Change, which reads the
  check state from GitHub and ends the CI wait through the webhook.
- **[webhook-only-one-path-and-method](webhook/webhook-only-one-path-and-method.yaml)**\
  Given the receiver, which serves exactly one route: POST /github/webhook When validly signed
  requests arrive with GET on that path, and with POST on other paths Then GET is answered 405 and
  the other paths 404, none of them counts as a delivery, and nothing reads from GitHub.
- **[webhook-other-project-ignored](webhook/webhook-other-project-ignored.yaml)**\
  Given an Up Next item on the roadmap board (project 273), whose issue is also on another org
  project When a validly signed projects_v2_item delivery arrives for the other project's item Then
  the receiver answers 2xx and counts it as ignored, the engine reads nothing from GitHub, and the
  Item isn't reconciled.
- **[webhook-oversize-rejected](webhook/webhook-oversize-rejected.yaml)**\
  Given a Change waiting on CI for its head SHA When a validly signed check_run delivery for that
  SHA is POSTed with a body one byte over 10 MiB Then the receiver answers 413 and counts it as
  too_large, and nothing is processed: no GitHub read, no reconcile. A body of exactly 10 MiB is
  still accepted.
- **[webhook-payload-not-trusted](webhook/webhook-payload-not-trusted.yaml)**\
  Given a ready, unassigned Story of the team that is in Backlog on the board, so it isn't an Item
  When a validly signed projects_v2_item delivery claims its Status changed to Up Next, while GitHub
  still says Backlog Then the delivery is accepted and only wakes the engine: it resolves the node
  id by reading the board item from GitHub, sees it in Backlog (out of scope) and drops it, so no
  Item is created, no ready? decision is made and nothing is picked.
- **[webhook-ping-acknowledged](webhook/webhook-ping-acknowledged.yaml)**\
  Given the receiver with the App's webhook secret When GitHub sends its validly signed `ping` (as
  when the App's webhook is set up) Then the receiver answers 2xx, counts it as ignored, and the
  engine reads nothing from GitHub.
- **[webhook-replayed-delivery-processed-once](webhook/webhook-replayed-delivery-processed-once.yaml)**\
  Given a Change waiting on CI for its head SHA, and one of its two required checks completed on
  GitHub When the validly signed check_run delivery is POSTed, and then POSTed again with the same
  X-GitHub-Delivery id (a replay) Then the first is accepted and wakes the Change, which still waits
  on CI; the second is answered 2xx, counted as duplicate and not processed again: no GitHub read,
  no reconcile.
- **[webhook-unknown-event-ignored](webhook/webhook-unknown-event-ignored.yaml)**\
  Given a Change waiting on CI for head SHA a1 When a validly signed `deployment` delivery (an event
  the engine doesn't subscribe to) naming the same SHA is POSTed Then the receiver answers 2xx and
  counts it as ignored (event label other), and nothing is processed: no GitHub read, and the
  waiting Change isn't reconciled.
- **[webhook-valid-signature-accepted](webhook/webhook-valid-signature-accepted.yaml)**\
  Given a Change waiting on CI whose last required check has completed on GitHub, but whose webhook
  GitHub hasn't sent When the check_run delivery is POSTed to /github/webhook with a valid
  X-Hub-Signature-256 Then the receiver answers 2xx and counts it accepted, and the engine wakes the
  waiting Change, reads the check state from GitHub itself, and ends the CI wait through the
  webhook.

## invariant

- **[invariant-all-hold-in-steady-state](invariant/invariant-all-hold-in-steady-state.yaml)**\
  Given a busy team with a team cap of 1 item: an item implementing, one with its PR waiting on CI,
  one whose PR is merged and waiting on the merge-result check, and two Up Next items: the older one
  picked for alice, the other held for bob by the team cap When half an hour of resyncs and
  freshness checks pass without a fault Then every invariant of §1.5 is exported (one series each)
  with value 0, including stage-age-under-p90 and pending-run-placed, which are always 0 in phase 1.
- **[invariant-gate-holding-exempt-work-detected](invariant/invariant-gate-holding-exempt-work-detected.yaml)**\
  Given alice's In Progress item blocked by her open story #102, and a gate that holds everything
  When a person moves #102 from Backlog (where it isn't an Item) to Up Next Then the engine holds it
  for alice although work that unblocks in-flight work is exempt from all gates, and
  gate-not-holding-repair reports it.
- **[invariant-leader-stops-deciding-detected](invariant/invariant-leader-stops-deciding-detected.yaml)**\
  Given an Up Next item picked for alice, and a leader that stops deciding at 09:00 When more than
  two board resync intervals (20m) pass, to 09:21, with the board resyncs at 09:10 and 09:20 running
  no decision pass Then leader-decides is violated (the last decision pass is older than 2 × 10m),
  and the Installation shows the last decision at 09:00.
- **[invariant-overnight-stall-alerts-within-minutes](invariant/invariant-overnight-stall-alerts-within-minutes.yaml)** `learning-overnight-epic`\
  Given an epic whose sub-items (one in giantswarm/klaus, one in giantswarm/muster) are Up Next at
  20:00 on a Monday evening, picked for alice and bob When the leader stops deciding at 21:00, right
  after the 21:00 board resync's decision pass Then leader-decides is violated by 21:21, as soon as
  the last decision pass is older than two board resync intervals (20m), so the stall alerts the
  same evening instead of being discovered in the morning.
- **[invariant-pending-ci-without-wait-detected](invariant/invariant-pending-ci-without-wait-detected.yaml)**\
  Given an In Progress item in giantswarm/klaus, and an engine that no longer creates waits When
  alice opens a PR that closes it, with CI not yet finished on its head SHA Then the Change sees CI
  pending but holds no ci wait, and wip-has-run-or-wait counts that one Change.
- **[invariant-repeated-refusal-detected](invariant/invariant-repeated-refusal-detected.yaml)**\
  Given a Change whose PR read is refused with 404, and an engine that forgets its refusals When two
  Changes resyncs read the PR again with nothing changed in between Then the Change shows the
  refusal while it stands, the refusal is retried on unchanged inputs, and refusal-not-repeated
  counts the one Change.
- **[invariant-source-without-verdict-detected](invariant/invariant-source-without-verdict-detected.yaml)**\
  Given both sources checked and fresh at 09:10, and freshness checks that stop running then When 21
  minutes pass, so each verdict is older than twice its check interval (2 × 5m for the delivery
  log, 2 × 10m for the board probe) Then source-fresh-or-degraded counts both sources (2), although
  they still show their last verdict (fresh): a verdict nobody renews is no verdict.
- **[invariant-wait-past-deadline-detected](invariant/invariant-wait-past-deadline-detected.yaml)**\
  Given a Change waiting on CI with a deadline at 10:00, and a timer queue that stops firing When
  the clock passes the deadline plus the 5m grace Then wait-within-deadline reports the unhandled
  wait while the wait still says watching, and once the timer queue works again the deadline is
  handled (the wait becomes overdue) and the invariant is back to 0.

## history

- **[history-decisions-recorded-with-inputs](history/history-decisions-recorded-with-inputs.yaml)**\
  Given a ready Up Next item and JEV answering "not-ready" for it with confidence 0.9 When the
  engine decides ready? for it at startup Then the decisions table holds one row for that point and
  item (the subject) with its inputs hash, the rule's answer, JEV's answer and confidence, the
  chosen answer (the rule's), the mode shadow and the outcome disagree; and replaying every recorded
  decision from its recorded inputs reproduces it.
- **[history-item-timeline](history/history-item-timeline.yaml)**\
  Given a ready Up Next item assigned to alice, for giantswarm/muster (one required check) When
  alice moves it to In Progress at 09:10, opens a PR at 10:10, CI turns green at 10:30, bob approves
  at 10:45 and alice merges at 11:00 Then the event table holds the item's timeline in order,
  stamped with the engine clock and the source of each observation: its phase changes (Implementing,
  Published, Merging, Merged), and the Change's waits started, woken and ended (ci on a1, review, ci
  on the merge commit); the Item itself, in flight and not started by the engine, holds no wait.
- **[history-observation-source-attribution](history/history-observation-source-attribution.yaml)**\
  Given a ready Up Next item #101 and an In Progress item #102 whose PR waits on CI When at 09:00 #101
  is moved to In Progress (webhook delivered) and CI on #102's PR completes green without a webhook
  ever being sent, and the clock reaches the 09:15 changes resync Then each change is attributed to
  how it was first seen: #101's to webhook (09:00) and the PR's to resync (09:15), in the event
  table, on the Change's observed field and in engine_observations_total; the resync-only change
  counts as a missed webhook.
- **[history-postgres-down-engine-continues](history/history-postgres-down-engine-continues.yaml)**\
  Given an In Progress item whose PR waits on CI, a ready Story #102 in Backlog (so not yet an
  Item), and Postgres going down at 09:00 When CI completes green and #102 is moved to Up Next, both
  with their webhooks delivered, and Postgres comes back after the 09:05 delivery-log check Then the
  receiver refuses both deliveries with 503 (outcome unavailable) because it can't store them,
  GitHub logs them as failed, and at 09:05 the webhooks source is degraded (deliveries-failed) and
  the engine re-observes what the failed deliveries name; it keeps observing, deciding and updating
  objects meanwhile (the Change sees green, #102 becomes an eligible Item, the team's last decision
  is 09:05); once Postgres is up the buffered history rows are written with their engine-clock times
  and no history row is dropped.

## leader

- **[leader-deadline-during-handover-fires](leader/leader-deadline-during-handover-fires.yaml)**\
  Given two engine replicas, engine-0 leading and engine-1 standing by, and a Change waiting on CI
  with a deadline at 10:00 When the leader dies and the engine's clock passes the deadline before
  the standby leads Then engine-1, once it leads, handles the missed deadline exactly once (one
  re-observe, CI unchanged, so the wait becomes overdue), the dead leader writes nothing more, and
  wait-within-deadline holds past the grace.
- **[leader-handover-continues-deciding](leader/leader-handover-continues-deciding.yaml)** `learning-self-update-stall`\
  Given two engine replicas, engine-0 leading and engine-1 standing by, an Up Next item in
  giantswarm/klaus picked for alice, and bob's ready item for giantswarm/muster in Backlog (not yet
  an Item; another repo, so alice's pick doesn't hold it) When the leader dies (as in an upgrade
  that goes wrong) and, during the handover, a person moves bob's item to Up Next Then engine-1
  takes the lead within seconds, processes the delivery that arrived while no replica led, and
  decides: bob's item becomes an Item and is picked for him, alice's item keeps its state and pick,
  nothing already recorded for it is recorded again, and the dead leader writes nothing more.
- **[leader-restart-rebuilds-wake-index](leader/leader-restart-rebuilds-wake-index.yaml)** `wake-index`\
  Given two engine replicas and two Changes waiting on CI for different SHAs When the engine
  restarts and, once it leads again, CI completes on one SHA Then the check_run webhook still wakes
  exactly the Change waiting on that SHA: the wake index is rebuilt from the objects, not lost with
  the process.
- **[leader-standby-makes-no-calls-or-writes](leader/leader-standby-makes-no-calls-or-writes.yaml)**\
  Given two engine replicas, engine-0 (started first, so it leads) and engine-1, and a Change
  waiting on CI When the delivery-log check comes due and a check_run webhook wakes the Change Then
  only the leader acts: the Installation names engine-0 as leader, the delivery log is read once,
  not once per replica, and the wake is recorded once. The standby engine-1 makes no GitHub calls,
  reconciles nothing and writes no object.

## shadow

- **[shadow-busy-day-never-writes](shadow/shadow-busy-day-never-writes.yaml)**\
  Given a team's Monday morning: an Up Next item picked for alice, an item of bob's that isn't
  ready, a new bug in the Inbox, and bob's PR waiting on CI When the working day runs to 17:00: the
  engine would start alice's item (set its status, start an implement run), send bob a todo and set
  the bug's severity, bob's pipeline runs past its deadline and goes overdue, and meanwhile alice
  starts her item and opens a PR, bob's CI goes green, alice approves, bob merges and main goes
  green Then all would-actions are recorded and none is executed: no mutating REST call and no
  GraphQL mutation all day, and every POST to GitHub is a GraphQL query or the minting of an
  installation token.
- **[shadow-would-actions-recorded-once](shadow/shadow-would-actions-recorded-once.yaml)**\
  Given an Up Next item picked for alice and an Up Next item of bob's that isn't ready, whose
  would-actions are recorded (set-status and start-run for alice's pick, already assigned to her,
  and bob's refinement todo) When the items are reconciled again and again: an edit that changes
  nothing the decisions depend on, an unrelated check_run webhook, and an hour of board and Changes
  resyncs Then no shadow.action event is recorded again: each action key is in the event table once,
  and bob's item still carries exactly one todo.
