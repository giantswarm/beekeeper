const {test} = require('node:test');
const assert = require('node:assert/strict');
const {evaluate, run, requiredStatuses, requiredChecks} = require('./batch-ci.cjs');
const sha = 'a'.repeat(40);
function green() {
  return {
    statuses: requiredStatuses.map(context => ({context, state: 'success'})),
    checks: requiredChecks.map((name, id) => ({name, id, head_sha: sha, app: {id: 15368}, status: 'completed', conclusion: 'success'})),
  };
}
test('all full checks must succeed on the exact queue SHA', () => {
  const {statuses, checks} = green();
  assert.deepEqual(evaluate(statuses, checks, sha), {missing: [], failed: []});
  assert.equal(evaluate(statuses, checks, 'b'.repeat(40)).missing.length, 2);
  assert.equal(evaluate([], checks, sha).missing.length, 8);
});
test('new status and check reruns override old success', () => {
  const {statuses, checks} = green();
  statuses.unshift({...statuses[0], state: 'failure'});
  checks.push({...checks[0], id: 100, conclusion: 'cancelled'});
  assert.deepEqual(evaluate(statuses, checks, sha).failed, [requiredStatuses[0], requiredChecks[0]]);
  statuses[0].state = 'pending';
  checks.at(-1).status = 'in_progress';
  assert.equal(evaluate(statuses, checks, sha).missing.length, 2);
});
test('skipped, neutral and foreign-app checks cannot pass full CI', () => {
  for (const conclusion of ['skipped', 'neutral', 'timed_out', 'failure']) {
    const {statuses, checks} = green();
    checks[0].conclusion = conclusion;
    assert.deepEqual(evaluate(statuses, checks, sha).failed, [requiredChecks[0]]);
  }
  const {statuses, checks} = green();
  checks[0].app.id = 1;
  assert.deepEqual(evaluate(statuses, checks, sha).missing, [requiredChecks[0]]);
});
test('PR admission is fast and never queries full CI', async () => {
  await run({context: {eventName: 'pull_request', payload: {pull_request: {base: {ref: 'main'}, title: 'ci: stage batch gate'}}}, core: {info() {}}});
});
test('reject unrelated events, refs and SHA mismatches before querying CI', async () => {
  const group = {base_ref: 'refs/heads/main', head_ref: 'refs/heads/gh-readonly-queue/main/pr-1', head_sha: sha};
  for (const context of [
    {eventName: 'push', sha, payload: {merge_group: group}},
    {eventName: 'merge_group', sha: 'b'.repeat(40), payload: {merge_group: group}},
    {eventName: 'merge_group', sha, payload: {merge_group: {...group, base_ref: 'refs/heads/other'}}},
  ]) await assert.rejects(run({context}), /exact main merge queue/);
});
test('missing CircleCI evidence times out instead of passing', async () => {
  let clock = 0;
  const context = {eventName: 'merge_group', sha, repo: {owner: 'example', repo: 'example'}, payload: {
    merge_group: {base_ref: 'refs/heads/main', head_ref: 'refs/heads/gh-readonly-queue/main/pr-1', head_sha: sha},
  }};
  const github = {rest: {repos: {listCommitStatusesForRef() {}}, checks: {listForRef() {}}}, paginate: async () => []};
  await assert.rejects(run({context, github, core: {info() {}}, now: () => clock, sleep: async () => { clock += 40 * 60 * 1000; }}), /within 40 minutes/);
});
test('full success queries only the queue SHA and records its evidence', async () => {
  const {statuses, checks} = green();
  const context = {eventName: 'merge_group', sha, repo: {owner: 'example', repo: 'example'}, payload: {
    merge_group: {base_ref: 'refs/heads/main', head_ref: 'refs/heads/gh-readonly-queue/main/pr-1', head_sha: sha},
  }};
  const statusAPI = () => {};
  let requests = 0;
  let written = false;
  const github = {rest: {repos: {listCommitStatusesForRef: statusAPI}, checks: {listForRef() {}}}, paginate: async (api, params) => {
    assert.equal(params.ref, sha);
    requests++;
    return api === statusAPI ? statuses : checks;
  }};
  const summary = {addHeading() { return this; }, addRaw(text) { assert.match(text, new RegExp(sha)); return this; }, addList() { return this; }, async write() { written = true; }};
  await run({context, github, core: {summary}, sleep: async () => { assert.fail('green CI must not wait'); }});
  assert.equal(requests, 2);
  assert.equal(written, true);
});
