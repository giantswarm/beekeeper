# Temporary sandbox proof policy

Sandbox proof publication and the updater's sandbox-evidence requirement are
temporarily disabled until dedicated sandbox settings are provisioned.
The single policy is `internal/update/sandbox-proof-required.txt`, currently
`false`. CircleCI halts the evidence job successfully before signing or uploading
evidence; the publisher script also skips when invoked directly. The updater
embeds the same policy at build time, so Run, InstallVersion and InstallRelease
accept releases without sandbox evidence.

Normal release binary Sigstore verification, repository/source identity,
platform selection, digest integrity, local candidate checks, durable rollback,
heartbeat/probation and writer fencing remain in place. This policy neither
publishes fixture evidence nor establishes live canary proof.

## Reactivation

The provisioning follow-up must supply dedicated BEEKEEPER_CANARY_REPO, BEEKEEPER_CANARY_ALLOW_ORG,
BEEKEEPER_CANARY_PROJECT and BEEKEEPER_CANARY_GITHUB_TOKEN settings through the existing CI
configuration. Reactivation is tracked by [the dedicated sandbox provisioning and reactivation
follow-up](https://github.com/giantswarm/beekeeper/issues/3362); dedicated provisioning must be
complete before enabling this policy.

After provisioning and a real sandbox proof, change the shared policy to `true`
and update TestSandboxProofPolicyDefault in the same PR. Run the focused updater
policy/signature tests and publisher-script regression, then publish a signed
release with real sandbox evidence. Required mode retains fail-closed evidence
verification and rejects recorded fixtures; invalid policy values fail closed.
Existing binaries retain the policy embedded when they were built.

Keep that issue open until actual settings and live evidence are established and
publisher/consumer enforcement is reenabled together.
