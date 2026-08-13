# Validation, Repair, Release, and Cutover Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox syntax for tracking.

**Goal:** Independently validate the pure Go implementation, repair every confirmed defect, publish and verify the production image, perform the Cloudflare hard cut, prove live GitHub behavior, and retire the old fork.

**Architecture:** Treat Plan 1 output as an untrusted implementation candidate. Reproduce every promised behavior through local HTTP boundaries, race tests, repository checks, image execution, release verification, production deployment, and a temporary live pull request. Make focused fixes only after evidence identifies a defect.

**Tech Stack:** Go 1.26.5, go-makefile reusable workflows and installer, Docker Buildx, GitHub Actions, GitHub Container Registry, GitHub App REST and GraphQL APIs, Cloudflare Workers and Containers, Wrangler, and the existing pr-agent-cf consumer.

## Global Constraints

- Start only after Plan 1 reaches its completion gate.
- Work from /Users/agoodkind/Sites/pr-review-agent and /Users/agoodkind/Sites/pr-agent-cf.
- Fetch before every branch comparison, log range, merge-base, or diff.
- Treat Plan 1 comments, tests, and commit messages as claims until reproduced.
- Diagnose failures before changing code.
- Add a failing behavior test before every Go behavior fix.
- Keep fixes focused. Do not broaden the product.
- Preserve the pure Go product contract.
- Add no Python or TypeScript.
- Add no commands, replies, issue comments, progress comments, compatibility flags, shims, or upstream synchronization.
- Use the active harness Git identity, signing, and commit flow.
- Run make check before each commit in the Go repository.
- Never force push.
- Never merge automatically.
- Do not change the GitHub App ID, installation scope, webhook URL, private key, webhook secret, bot name, or stored secret values.
- Keep Cloudflare Worker name agoodkind-nano-pr-reviewer.
- Use one hard production cut. Do not add staging routes, parallel containers, or orchestration.
- Pin deployment to an immutable GHCR digest.
- Preserve the previous image digest for rollback.
- Archive agoodkind/pr-agent only after production and live GitHub acceptance.
- Record direct evidence for every requirement.
- Keep secrets out of commands, logs, files, plan evidence, and chat.

---

## Starting State and Known Facts

### New Go repository

- GitHub: https://github.com/agoodkind/pr-review-agent
- Visibility: public.
- Local checkout: /Users/agoodkind/Sites/pr-review-agent
- Default branch: main.
- Go module: goodkind.io/pr-review-agent.
- Go version: 1.26.5.
- Binary: pr-review-agent.
- Version package: goodkind.io/pr-review-agent/internal/version.
- Initial scaffold commit: a3c4f1cac7f595bc824704b9d2a1f1191630dc32.
- Initial reusable CI run 31721370861 passed.
- Initial release tag 202608131636-1-a3c4f1c published four platform archives and checksums.
- Initial release run 31721370790 failed only in release verification.
- The failure was a persistent HTTP 404 for the release attestation keyed by the release commit.
- Archive build provenance attestations succeeded.
- The first release does not expose the required GitHub release attestation.
- GitHub documents immutable releases as the mechanism that generates release attestations.
- Verify the current repository setting before attributing the missing attestation to configuration.
- Do not delete or rewrite the historical initial release.

### Plan 1 handoff

- Plan 1 ends with five open, ready, dependent Graphite pull requests.
- Their bottom-to-top titles are Add Go review intake contracts, Add GitHub review data pipeline, Add OpenAI review analysis, Add review lifecycle reconciliation, and Add PR review service runtime.
- Every Plan 1 pull request changes Go files. The OpenAI slice also changes go.mod and go.sum for github.com/openai/openai-go.
- The stack remains unmerged when this plan starts.
- Route confirmed Go fixes to the lowest owning branch through Graphite, then restack and resubmit affected branches.
- Do not add validation, image, workflow, infrastructure, or evidence files to the five Plan 1 branches.

### Canonical go-makefile contracts

Repository adoption already used:

    curl -fsSL https://raw.githubusercontent.com/agoodkind/go-makefile/main/bootstrap.sh | bash -s -- --yes --module=goodkind.io/pr-review-agent --binary

Canonical consumer installation is:

    curl -fsSL https://raw.githubusercontent.com/agoodkind/go-makefile/main/install.sh | bash -s -- --repo agoodkind/pr-review-agent --binary pr-review-agent

Keep the canonical reusable callers:

- agoodkind/go-makefile/.github/workflows/_ci.yml@main
- agoodkind/go-makefile/.github/workflows/_release.yml@main

The image must consume Go binaries produced by this release chain. It must not compile a second binary in Docker.

### Current production consumer

- Repository: /Users/agoodkind/Sites/pr-agent-cf
- Worker name: agoodkind-nano-pr-reviewer.
- Durable Object binding: PR_AGENT.
- Container class: PrAgentContainer.
- Container port: 3000.
- Container scale: max_instances 1.
- Container sleep: 1 minute.
- Public webhook route: /api/v1/github_webhooks.
- Worker-only health route: /health.
- Container-backed readiness route: /.
- Current image: ghcr.io/agoodkind/pr-agent@sha256:01434b95aa966db79a94ffc04355719f25fe2089857ea076f5349dc6b7ef7ccf.
- Deployment workflow probes the container-backed root route after deploy.
- Rollback is the previous Worker version and image digest.

### Production identity and model

- GitHub App ID: 4571682.
- Bot login: agoodkind-pr-review-agent[bot].
- Model: gpt-5.6-sol.
- Clyde base URL: https://clyde-suburban.goodkind.io/v1.
- Existing Cloudflare secrets hold the GitHub private key, webhook secret, Clyde bearer token, and Cloudflare Access service-token values.
- Do not rename, recreate, print, or rotate those secrets.
- Map existing stored secret names to the new Go environment names in the Worker only.

---

## Required Final Product

For every supported new pull request head:

- One PR-Agent Review check moves through queued, in_progress, and completed.
- One GitHub review is published.
- Its body is the only top-level bot review comment.
- Its comments array contains valid inline findings.
- It uses APPROVE, COMMENT, or REQUEST_CHANGES.
- It contains no issue comment or progress comment.
- A duplicate delivery or replay creates no second review for the same head.

After a later commit:

- The new head receives its own review.
- Earlier unresolved owned findings are reevaluated.
- Findings proven fixed or invalid are resolved.
- Open or uncertain findings remain unresolved.
- Reconciliation posts no replies.
- Reconciliation failure does not change a successful new-head review or lifecycle result.

---

### Task 1: Inventory and Audit Plan 1 Output

**Files:**

- Read all Plan 1 Go files.
- Create docs/verification/requirement-ledger.md.
- Do not change behavior yet.

- [ ] Fetch origin in pr-review-agent.
- [ ] Run Graphite state and log short from pr-review-agent.
- [ ] Record the five Plan 1 branches and pull requests from bottom to top.
- [ ] Record the Plan 1 starting commit and stack-tip commit.
- [ ] Diff each branch against its immediate parent and require every changed path to end in .go, except go.mod and go.sum for openai-go.
- [ ] Verify no Plan 1 branch changes workflows, documentation, images, or infrastructure.
- [ ] Read every Go source file completely.
- [ ] Trace webhook ingestion to check creation, diff collection, OpenAI completions, review publication, and reconciliation.
- [ ] Search for forbidden behavior:
  - issue_comment
  - pull_request_review_comment
  - issue-comment write endpoints
  - review-comment reply endpoints
  - slash commands
  - mention routing
  - legacy environment variables
  - compatibility flags
  - Python, Node, and upstream references
- [ ] Create the requirement ledger with columns:
  - Requirement
  - Go source
  - Focused test
  - Full test
  - Race test
  - CI evidence
  - Image evidence
  - Release evidence
  - Deployment evidence
  - Live GitHub evidence
- [ ] Add one row for every product, security, concurrency, release, deployment, and live-proof requirement in this plan.
- [ ] Mark missing evidence as Not yet proven. Do not mark it complete from source inspection.
- [ ] Commit only the initial ledger through the active harness Git flow with subject Add PR review verification ledger.

    git add docs/verification/requirement-ledger.md

---

### Task 2: Reproduce Local Behavior and Find Defects

**Commands:**

    go test ./... -count=1
    go test -race ./... -count=1
    make check

- [ ] Delete stale local test binaries and temporary coverage files before running gates.
- [ ] Run every focused package test independently.
- [ ] Run the full test suite without cache.
- [ ] Run the full race suite without cache.
- [ ] Run make check.
- [ ] Capture command, commit SHA, start time, end time, exit status, and concise result in the ledger.
- [ ] Verify failed commands name the exact package and test.
- [ ] Verify test servers reject issue-comment and reply writes.
- [ ] Run the end-to-end webhook test ten times.
- [ ] Run concurrent duplicate-delivery tests fifty times under the race detector.
- [ ] Run stale-head tests ten times.
- [ ] Run pagination tests with 101 files and 101 threads.
- [ ] Run tests with malformed GitHub and OpenAI JSON.
- [ ] Run tests with HTTP 401, 403, 404, 409, 422, 429, and 500 responses.
- [ ] Run tests with missing patches, binary files, deleted files, renamed files, and oversized hunks.
- [ ] Run tests with head changes before publication and before reconciliation mutation.
- [ ] Record each reproducible defect separately before fixing it.

If all tests pass, continue to adversarial review. Passing tests do not establish completion.

---

### Task 3: Perform Adversarial Code Review

Use the repository adversarial-review playbook and strongest available review model for security, concurrency, lifetime, and user-visible behavior.

Review these attack classes:

1. Webhook signature bypass, body truncation, header confusion, and timing leaks.
2. GitHub App JWT claims, installation-token cache races, and token leakage.
3. Prompt injection through pull request title, body, paths, diffs, and code comments.
4. Duplicate deliveries, concurrent synchronize events, queue saturation, and shutdown races.
5. Durable marker spoofing, malformed markers, and cross-repository collisions.
6. Head changes during diff reads, model work, publication, and reconciliation.
7. Incorrect right-side anchors, renamed files, multiline ranges, and truncated patches.
8. Partial coverage reaching APPROVE.
9. Model schema escape, unknown fields, invalid importance, and duplicate findings.
10. Issue-comment or reply publication through an unintended generic client method.
11. GraphQL pagination, ownership verification, and foreign-thread resolution.
12. A failed resolution blocking an independent safe resolution.
13. Reconciliation changing a successful review check.
14. Unbounded response reads, logs, queues, maps, goroutines, or keyed locks.
15. Secret exposure in errors and structured logs.
16. Unhandled process signals and incomplete queue drain.
17. HTTP transport timeouts and leaked response bodies.
18. Model retry amplification and GitHub secondary-rate-limit amplification.

- [ ] Write findings with file, line, trigger, impact, and reproduction.
- [ ] Reject hypothetical findings without a reachable path.
- [ ] For each confirmed finding, add a failing public-boundary test.
- [ ] Run the test and record the expected failure.
- [ ] Apply the smallest Go fix.
- [ ] Run the focused test, full test, race test when relevant, and make check.
- [ ] Route each confirmed Go repair to the lowest owning stack branch through Graphite.
- [ ] Let the active harness create or amend the repair commit.
- [ ] Stage only the exact repaired Go files before each Graphite operation.
- [ ] Restack and resubmit all affected branches through Graphite.

- [ ] Repeat adversarial review against the final diff.
- [ ] Leave no unresolved confirmed severity finding.

---

### Task 4: Validate the Process Locally

Do not use production credentials.

- [ ] Generate an ephemeral RSA key in a temporary directory.
- [ ] Start fake GitHub and OpenAI HTTP servers.
- [ ] Start the compiled pr-review-agent process with dummy secrets and test URLs supported through direct Config construction or a test harness.
- [ ] Verify GET / returns HTTP 200 and exact JSON.
- [ ] Verify GET /health returns HTTP 200 and exact JSON.
- [ ] Send one signed webhook through the real listening socket.
- [ ] Verify the fake GitHub server receives lifecycle and review writes.
- [ ] Send duplicate and stale-head deliveries.
- [ ] Verify the process behavior matches the product contract.
- [ ] Send SIGTERM during idle operation and require clean exit.
- [ ] Send SIGTERM with accepted work and require bounded drain.
- [ ] Send SIGINT and require the documented interrupt exit.
- [ ] Verify logs contain delivery, repository, pull request, head, and check identifiers.
- [ ] Verify logs contain none of the ephemeral secrets.
- [ ] Record the executable SHA-256 and fresh build timestamp.
- [ ] Add any missing process-boundary regression test before fixing a defect.

---

### Task 5: Add the Production Container

**Files in pr-review-agent:**

- Create Dockerfile.
- Create .dockerignore.
- Create .github/scripts/prepare-image-binaries.sh.
- Create .github/scripts/probe-container.sh.
- Create .github/scripts/test-container.sh.
- Update README only with canonical install and container runtime instructions after behavior is proven.

Docker rules:

- Use a pinned distroless static nonroot base image by digest.
- Accept TARGETARCH.
- Copy dist/linux/TARGETARCH/pr-review-agent.
- Set USER to the distroless nonroot identity.
- Expose port 3000.
- Set the binary as the only entrypoint.
- Include no shell or package manager.
- Compile nothing inside Docker.
- Support linux/amd64 and linux/arm64.
- Run with a read-only root filesystem and writable tmpfs at /tmp.
- Add OCI source, revision, version, and license labels.

prepare-image-binaries.sh rules:

- Start with #!/usr/bin/env bash and set -euo pipefail.
- Accept one release asset directory and one output directory.
- Verify checksums.txt before extraction.
- Extract only the Linux AMD64 and ARM64 archives.
- Require one executable named pr-review-agent in each archive.
- Write exact paths:
  - dist/linux/amd64/pr-review-agent
  - dist/linux/arm64/pr-review-agent
- Reject path traversal and duplicate archive members.
- Never download or compile.

probe-container.sh rules:

- Start with #!/usr/bin/env bash and set -euo pipefail.
- Track the container PID or ID.
- Trap INT, TERM, and EXIT.
- Remove only the exact test container.
- Provide ephemeral dummy configuration.
- Poll the container-backed root and health endpoints.
- Require both exact HTTP 200 responses.
- Verify the process UID is nonzero.
- Verify the root filesystem is read only.
- Verify --version output.
- Exit 130 after INT.

- [ ] Add failing container tests before Dockerfile implementation.
- [ ] Build one local architecture image from the exact Go binary.
- [ ] Run the image nonroot with read-only root and tmpfs /tmp.
- [ ] Verify both health routes.
- [ ] Verify Python and Node executables are absent.
- [ ] Verify no shell exists.
- [ ] Verify the binary SHA-256 matches the prepared release input.
- [ ] Run repository checks.
- [ ] Commit this slice through the active harness Git flow with subject Add production Go container.

    git add Dockerfile .dockerignore .github/scripts/prepare-image-binaries.sh .github/scripts/probe-container.sh .github/scripts/test-container.sh README.md

---

### Task 6: Extend CI and Release Automation

Keep the canonical go-makefile callers. Add consumer jobs around them.

#### Continuous integration

After the reusable CI job:

1. Download the build-linux-amd64 artifact from that workflow run.
2. Put the binary at dist/linux/amd64/pr-review-agent.
3. Build the Dockerfile without recompiling.
4. Run the container probe.
5. Inspect the image user and entrypoint.
6. Fail on Python, Node, a shell, or root execution.
7. Do not push an image.

#### Release

After the reusable release job succeeds:

1. Locate exactly one GitHub release targeting github.sha.
2. Download every release archive and checksums.txt.
3. Verify all checksums.
4. Verify GitHub build provenance for both Linux archives.
5. Prepare both image binaries.
6. Build and push a multi-platform image.
7. Enable Buildx provenance mode max.
8. Generate a software bill of materials.
9. Push these tags:
   - ghcr.io/agoodkind/pr-review-agent:$RELEASE_TAG
   - ghcr.io/agoodkind/pr-review-agent:sha-$GITHUB_SHA
   - ghcr.io/agoodkind/pr-review-agent:latest
10. Capture the manifest digest from the build action output.
11. Inspect the remote manifest.
12. Require linux/amd64 and linux/arm64.
13. Pull by digest and run the container probe.
14. Publish the digest in the workflow summary.

Workflow permissions:

- CI: contents read, id-token write, attestations write.
- Release binary job: existing canonical permissions.
- Image job: contents read, packages write, id-token write, attestations write.

Do not use a PAT in pr-review-agent. Use GITHUB_TOKEN for its own GHCR package.

- [ ] Run go run github.com/rhysd/actionlint/cmd/actionlint@v1.7.7 on both workflows and require zero findings.
- [ ] Keep branch CI free of image pushes.
- [ ] Keep the release binary authoritative.
- [ ] Prove the Docker build consumes downloaded release assets.
- [ ] Run make check and workflow checks.
- [ ] Commit this slice through the active harness Git flow with subject Publish release binaries as a GHCR image.

    git add .github/workflows/ci.yml .github/workflows/release.yml .github/scripts/prepare-image-binaries.sh .github/scripts/probe-container.sh .github/scripts/test-container.sh

---

### Task 7: Enable Immutable Releases and Prove Canonical Installation

External change:

- Inspect release immutability for agoodkind/pr-review-agent and enable it if disabled.
- This applies only to future releases.
- Do not alter the initial historical prerelease.

- [ ] Record the repository setting before change.
- [ ] Enable immutable releases when the setting is disabled.
- [ ] Stop and diagnose the missing release attestation before publishing when immutability was already enabled before the initial release.
- [ ] Record the setting after change.
- [ ] Push the reviewed Go branch and open one focused pull request.
- [ ] Wait for every required check.
- [ ] Do not merge automatically.
- [ ] Obtain explicit merge authorization.
- [ ] Merge without bypassing protection.
- [ ] Verify the merge commit is the intended reviewed content.
- [ ] Wait for the canonical release and image jobs.
- [ ] Verify the new release displays immutable status.
- [ ] Verify release attestation retrieval succeeds.
- [ ] Verify every archive build provenance succeeds.
- [ ] Verify the canonical release Verify job succeeds.
- [ ] Resolve the new immutable release tag and create an isolated installation directory:

    release_tag=$(gh release list --repo agoodkind/pr-review-agent --exclude-drafts --limit 1 --json tagName --jq '.[0].tagName')
    install_dir=$(mktemp -d)

- [ ] Run the canonical installer:

    curl -fsSL https://raw.githubusercontent.com/agoodkind/go-makefile/main/install.sh |
        bash -s -- \
            --repo agoodkind/pr-review-agent \
            --binary pr-review-agent \
            --bin-dir "$install_dir" \
            --version "$release_tag" \
            -- \
            --version

- [ ] Require release attestation, provenance, checksum, executable, tag, and source commit verification.
- [ ] Verify the GHCR image digest matches the release commit.
- [ ] Record release URL, tag, asset digests, attestation IDs, image tags, and manifest digest.

If canonical installation still fails, stop deployment. Diagnose go-makefile or repository release configuration from evidence. Do not weaken attestation requirements.

---

### Task 8: Prepare the Cloudflare Consumer Hard Cut

**Repository:** /Users/agoodkind/Sites/pr-agent-cf

**Files:**

- Modify Dockerfile.
- Modify worker/configuration.js.
- Modify tests.
- Update README only where operational behavior changed.
- Do not rename the Worker, Durable Object binding, or class.

Dockerfile change:

- Set IMAGE_DIGEST to the verified sha256 manifest digest from Task 7, then replace the old image reference with ghcr.io/agoodkind/pr-review-agent@$IMAGE_DIGEST.
- Keep HOME=/tmp only if the new image test proves it is required. Otherwise remove it.
- Do not add packages or wrappers.
- Keep nonroot execution.

Map existing stored Cloudflare secrets to exact Go variables:

| Go variable | Existing source |
| --- | --- |
| GITHUB_APP_ID | Literal 4571682 |
| GITHUB_PRIVATE_KEY | secrets.GITHUB_PRIVATE_KEY |
| GITHUB_WEBHOOK_SECRET | secrets.GITHUB_WEBHOOK_SECRET |
| GITHUB_BOT_LOGIN | Literal agoodkind-pr-review-agent[bot] |
| CLYDE_BASE_URL | Literal https://clyde-suburban.goodkind.io/v1 |
| CLYDE_API_KEY | secrets.OPENAI_KEY |
| CF_ACCESS_CLIENT_ID | secrets.CF_ACCESS_CLIENT_ID |
| CF_ACCESS_CLIENT_SECRET | secrets.CF_ACCESS_CLIENT_SECRET |

Remove every legacy container variable beginning with:

- CONFIG__
- GITHUB_APP__
- GITHUB__
- GUNICORN_
- LITELLM__
- OPENAI__
- PR_CODE_SUGGESTIONS__

Keep:

- Worker-only GET /health behavior.
- Container forwarding for all other paths.
- Container-backed root readiness.
- Port 3000.
- max_instances 1.
- sleepAfter 1m.

- [ ] Fetch origin.
- [ ] Create a focused branch through the active harness Git flow.
- [ ] Update the digest and environment mapping.
- [ ] Update tests to assert only the new Go environment.
- [ ] Test Worker /health without starting the container.
- [ ] Test webhook forwarding unchanged.
- [ ] Test root forwarding unchanged.
- [ ] Run npm test and npm run check.
- [ ] Scan the final Worker bundle for secret values.
- [ ] Verify the diff contains no secret mutation.
- [ ] Commit through the active harness Git flow.
- [ ] Open a pull request.
- [ ] Do not merge automatically.

---

### Task 9: Verify and Minimize GitHub App Configuration

Keep unchanged:

- App ID 4571682.
- Installation scope.
- Webhook URL.
- Private key.
- Webhook secret.
- Bot name.

Required repository permissions:

- Pull requests: read and write.
- Checks: read and write.
- Contents: read.
- Metadata: read.

Required event subscription:

- Pull request.

Remove subscriptions no longer used:

- Issue comment.
- Pull request review comment.
- Every event required only by removed upstream features.

- [ ] Capture the current App permission and event configuration.
- [ ] Compare it to the required minimum.
- [ ] Change only excess permissions and event subscriptions.
- [ ] Verify all 74 existing installations remain installed.
- [ ] Verify the production webhook URL is unchanged.
- [ ] Verify a signed pull_request delivery reaches the current production endpoint.
- [ ] Record App ID, event list, permission list, webhook URL hash, and installation count.
- [ ] Do not record keys, secrets, or raw webhook signatures.

Perform this task immediately before the Cloudflare merge so the old Python app has the shortest possible period with a reduced event set.

---

### Task 10: Perform the Cloudflare Hard Cut

- [ ] Confirm the previous Worker version and old image digest are recorded.
- [ ] Confirm the new GHCR digest passes remote pull and readiness.
- [ ] Confirm the pr-agent-cf pull request pins that exact digest.
- [ ] Wait for every consumer check.
- [ ] Obtain explicit merge authorization.
- [ ] Merge without bypassing protection.
- [ ] Monitor the existing deploy workflow.
- [ ] Require successful Worker-only /health.
- [ ] Require successful container-backed / readiness.
- [ ] Confirm the running container image digest equals the consumer pin.
- [ ] Confirm container logs show Go startup and contain no secrets.
- [ ] If readiness or routing fails, roll back to the recorded Worker version and old digest.
- [ ] Do not attempt a second unrelated fix during rollback.
- [ ] After rollback, reproduce and repair the failure in a new focused change.

---

### Task 11: Prove Live GitHub Behavior

Use a temporary branch and pull request in agoodkind/pr-review-agent. Do not merge it.

#### First head

Create one small Go defect on a changed line. Use deterministic behavior that compiles and whose wrong result is directly observable. Do not add a security vulnerability or secret.

- [ ] Push the defect commit through the active harness Git flow.
- [ ] Open the temporary pull request.
- [ ] Record pull request number, head SHA, and webhook delivery ID.
- [ ] Observe the PR-Agent Review check queued timestamp.
- [ ] Observe its in_progress timestamp.
- [ ] Observe its completed timestamp and conclusion.
- [ ] Verify exactly one review exists for the head.
- [ ] Verify the author is agoodkind-pr-review-agent[bot].
- [ ] Verify the review body contains a concise summary and exact review marker.
- [ ] Verify the review state is CHANGES_REQUESTED.
- [ ] Verify at least one inline finding is attached to the defective changed line.
- [ ] Verify the inline body contains a valid finding marker.
- [ ] Verify no bot issue comment exists.
- [ ] Verify no bot reply exists.

#### Duplicate proof

- [ ] Redeliver the same GitHub webhook twice.
- [ ] Wait for both deliveries to complete.
- [ ] Verify the head still has exactly one bot review.
- [ ] Verify the finding thread still has one bot root and no bot replies.
- [ ] Restart or redeploy the container without changing the head.
- [ ] Redeliver once more.
- [ ] Verify the durable review marker prevents a duplicate after process restart.

#### Corrected head

- [ ] Push a commit that fixes the defect through the active harness Git flow.
- [ ] Record the new head SHA and synchronize delivery.
- [ ] Verify a new PR-Agent Review lifecycle completes.
- [ ] Verify exactly one review exists for the new head.
- [ ] Verify the new review is APPROVED when no finding remains and coverage is complete.
- [ ] Verify the old owned thread becomes resolved.
- [ ] Verify reconciliation adds no reply.
- [ ] Verify no issue comment appears.
- [ ] Verify the successful lifecycle stays successful even if an injected independent reconciliation case fails.

#### Prose proof

Inspect the published review body and finding:

- [ ] Finding or result appears first.
- [ ] Sentences are short and active.
- [ ] Paragraphs contain one idea.
- [ ] Exact identifiers appear where needed.
- [ ] Trigger and impact are concrete.
- [ ] No praise, introduction, filler, repetition, generic advice, or closing summary appears.
- [ ] No typographic dash appears.

#### Cleanup

- [ ] Close the temporary pull request without merging.
- [ ] Delete the temporary remote branch.
- [ ] Verify production health again.
- [ ] Preserve URLs and IDs in the ledger.

---

### Task 12: Archive the Old Fork

Repository: https://github.com/agoodkind/pr-agent

Preconditions:

- Production uses the new Go image digest.
- Cloudflare readiness passes.
- Live review and silent reconciliation pass.
- Canonical installation passes.
- No production configuration references ghcr.io/agoodkind/pr-agent.
- The previous image digest remains recorded for emergency rollback history.

- [ ] Search GitHub workflows, Cloudflare configuration, and local configs for old image references.
- [ ] Verify no active deployment consumes the old repository.
- [ ] Disable every workflow in agoodkind/pr-agent.
- [ ] Archive agoodkind/pr-agent.
- [ ] Verify its releases and commit history remain readable.
- [ ] Verify no upstream synchronization exists in pr-review-agent.
- [ ] Record the archive timestamp, final default-branch SHA, and disabled workflow names.
- [ ] Do not delete the repository, tags, releases, images, or history.

---

### Task 13: Close the Requirement Ledger

For every ledger row:

- [ ] Link the exact Go source.
- [ ] Name the focused test.
- [ ] Record the full test command and result.
- [ ] Record the race test where concurrency applies.
- [ ] Record the CI run and job.
- [ ] Record the immutable release and attestations.
- [ ] Record the GHCR manifest digest and both architectures.
- [ ] Record Cloudflare deployment and readiness evidence.
- [ ] Record GitHub App configuration evidence.
- [ ] Record temporary pull request, review, check, inline thread, resolution, duplicate-delivery, and cleanup evidence.
- [ ] Mark no row complete without direct evidence.
- [ ] Re-run go test ./..., go test -race ./..., make check, consumer checks, image probe, production readiness, and live health.
- [ ] Commit the completed ledger and any final documentation through the active harness Git flow with subject Record Go review production evidence.

    git add docs/verification README.md

---

## Plan 2 Completion Gate

Do not claim completion until all conditions hold:

- [ ] The final Go source matches the minimal product contract.
- [ ] Focused, full, race, and make checks pass.
- [ ] Independent adversarial review has no unresolved confirmed finding.
- [ ] The container runs nonroot with a read-only root filesystem.
- [ ] The image contains no Python, Node, shell, or second build.
- [ ] GHCR contains AMD64 and ARM64 under one immutable digest.
- [ ] The digest was built from canonical release assets.
- [ ] Immutable release attestation and build provenance verify.
- [ ] Canonical go-makefile installation succeeds.
- [ ] The Cloudflare consumer pins the verified digest.
- [ ] Worker and container-backed health pass.
- [ ] GitHub App identity, URL, scope, and secrets remain unchanged.
- [ ] GitHub App permissions and events are minimal.
- [ ] The temporary pull request proves one review per head.
- [ ] The review contains summary, decision, and inline finding.
- [ ] No issue comment or reply exists.
- [ ] Duplicate deliveries create no duplicate.
- [ ] A correcting commit receives a new review.
- [ ] The old finding resolves silently.
- [ ] Cleanup completes.
- [ ] agoodkind/pr-agent is archived.
- [ ] Every ledger row contains direct evidence.
