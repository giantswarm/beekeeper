// Queue admission and full-CI evidence are deliberately separate. A PR success
// is useful only with an enforced merge queue: never require this gate alone
// on a branch that permits direct merges.
const requiredStatuses = [
  'go-build', 'test-envtest', 'build-chart', 'build-image', 'chart-test',
  'test-stable-version', 'pin-version', 'test-loop-workflow',
].map(name => `ci/circleci: ${name}`);
const requiredChecks = ['pre-commit', 'check-values-schema / validate'];

function evaluate(statuses, checks, sha) {
  const missing = [];
  const failed = [];
  for (const context of requiredStatuses) {
    // The status API returns newest first. Never allow an older success to
    // hide a newer pending, failed or cancelled rerun.
    // Commit statuses have no SHA field; the caller queries the exact SHA.
    const status = statuses.find(item => item.context === context);
    if (!status || status.state === 'pending') missing.push(context);
    else if (status.state !== 'success') failed.push(context);
  }
  for (const name of requiredChecks) {
    const check = checks.filter(item => item.name === name && item.head_sha === sha && item.app?.id === 15368)
      .sort((a, b) => b.id - a.id)[0];
    if (!check || check.status !== 'completed') missing.push(name);
    else if (check.conclusion !== 'success') failed.push(name);
  }
  return {missing, failed};
}

async function run({github, context, core, sleep = ms => new Promise(resolve => setTimeout(resolve, ms)), now = Date.now}) {
  const event = context.payload;
  if (context.eventName === 'pull_request') {
    if (event.pull_request.base.ref !== 'main') throw new Error('Batch CI admission targets main only');
    if (!/^(feat|fix|docs|style|refactor|perf|test|build|ci|chore|revert|security)(\([^\r\n)]+\))?!?: \S/.test(event.pull_request.title) || event.pull_request.title.includes('!!:')) {
      throw new Error('Use a conventional PR title');
    }
    core.info('PR admission passed. Full CI is required separately on the merge queue commit.');
    return;
  }
  const group = event.merge_group;
  if (context.eventName !== 'merge_group' || group?.base_ref !== 'refs/heads/main' ||
      !group.head_ref?.startsWith('refs/heads/gh-readonly-queue/main/') || group.head_sha !== context.sha) {
    throw new Error('Expected an exact main merge queue commit');
  }
  const sha = group.head_sha;
  const deadline = now() + 40 * 60 * 1000;
  do {
    const params = {...context.repo, ref: sha, per_page: 100};
    const statuses = await github.paginate(github.rest.repos.listCommitStatusesForRef, params);
    const checks = await github.paginate(github.rest.checks.listForRef, {...params, filter: 'latest'});
    const result = evaluate(statuses, checks, sha);
    if (result.failed.length) throw new Error(`Full CI failed on ${sha}: ${result.failed.join(', ')}`);
    if (!result.missing.length) {
      await core.summary.addHeading('Batch full CI passed').addRaw(`Exact queue commit: ${sha}\n`)
        .addList([...requiredStatuses, ...requiredChecks]).write();
      return;
    }
    core.info(`Waiting for ${sha}: ${result.missing.join(', ')}`);
    await sleep(30000);
  } while (now() < deadline);
  throw new Error(`Full CI did not report success on queue commit ${sha} within 40 minutes`);
}

module.exports = {evaluate, run, requiredStatuses, requiredChecks};
