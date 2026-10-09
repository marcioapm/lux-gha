# lux-gha: implementation breakdown

Recovered from the 2026-10-07/08 discussion; refreshed 2026-10-09.

## Goal and boundaries
Keep GitHub Actions as the workflow engine. Supply one-job, just-in-time self-hosted runners as Lux generic Runs. Separate Go repo (lux-gha), no Lux-core GitHub adapter and no second database. Never resume a consumed runner/JIT credential. No changes to existing workflows or production until a pilot is selected and credentials are provisioned.

## Milestones
1. Bridge MVP: config + GitHub App authentication; signed workflow_job ingress; explicit trusted repo/installation/label routing; JIT configuration; thin Lux HTTP client; stable logical identity/dedup; secret delivery; cleanup/reconciliation with restart recovery; behavioural fake-service tests.
2. Runner image: minimal pinned Ubuntu/official actions runner, non-root user, entrypoint that consumes secret JIT config, exits after one job and does not print secrets. Nested Docker/toolchain profiles follow the pilot's needs; do not recreate GitHub's entire hosted image.
3. Verification: perf + code-quality + test-quality review cohort, triaged fixes and re-review, simplifier, parent-run tests/guards. No PR before completion.
4. Deployment: documented GitHub App permissions, key/webhook/Lux credentials; systemd unit and webhook ingress; dedicated gha pool, bounded resources; restricted egress including wildcards now supported by Lux. Infrastructure proposals first, no TF apply or prod mutation.
5. Pilot: synthetic workflow (checkout, one command, artifact upload), then one real workflow. Exercise concurrent jobs, duplicate and reordered delivery, cancellation, bridge restart and host loss. Existing workflows remain unchanged until the selected target is agreed.

## Corrections to old plan
- Dedup must follow logical GitHub job/attempt identity, not only X-GitHub-Delivery: a second delivery must not create a second runner.
- JIT runners match labels; investigate actual runner-to-job attribution before tying completed webhook cleanup to the job that caused provisioning.
- Lux resumePolicy never now ends in terminated, with exitCode/stateReason preserving outcome. Do not interpret terminated alone as failure.
- FEATURES is a release artifact, not an established HTTP capability endpoint. Validate current OpenAPI resumePolicy enum at startup if supported and check echoed spec on submission; do not invent /features.
- Wildcard egress is shipped: unrestricted network is not the default.

## Open pilot decision
Which repo/workflow goes first? This determines Node/Bun/Go/Python/browser requirements and whether services/Compose/rootless Docker are needed. It does not block bridge development.

## Operator-owned steps
GitHub App creation/installation and secret provision; Terraform apply; production enablement. No secret values belong in docs, fixtures, Run labels, argv or logs.
