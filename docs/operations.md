# Operations

The service turns each supported pull request push into one check and one final review decision for that head.

## Review lifecycle

The service accepts `opened`, `reopened`, `ready_for_review`, and `synchronize` pull request events. It ignores draft pull requests until they become ready.

Each head receives one `PR-Agent Review` check. The final decision is `APPROVE` or `REQUEST_CHANGES`.

The configured importance threshold controls publication. At least one anchored finding at or above the threshold requests changes. Otherwise, the service approves the head.

Published findings appear only as concise inline comments on changed lines. One editable top level body identifies the active findings review. Later decisions use hidden markers, so the pull request never gains another visible summary.

That body opens with the verdict, then carries the review details in a collapsed table: the model that answered, how long the review took, the head, the files and diff chunks read, and the finding and thread counts behind the decision. The check run renders the same table from the same values, so the two can never report different numbers.

A review that the fallback provider answers names the fallback model, because the table reports the model that did the work rather than the one configured first.

The service reads the diff in chunks, one request each. A model stops mid answer when it reaches its completion token budget, which reasoning and findings share, so a chunk yielding many findings can exhaust it. The service then splits that chunk in half and reviews each half, repeating while answers keep stopping early. A chunk holding one diff hunk cannot split, so the service skips it and reports incomplete coverage in the review details. One truncated answer never fails the whole review.

When a review cannot finish, the service rewrites that same top level body with the stage that failed and the reported cause, and creates it if the pull request has none yet. It carries no review marker, so the next push still reviews the head. When the model provider reports no remaining usage, the check run and the body both say so instead of naming a stage.

A failed review carries the same detail table as a successful one, filled with what it had learned when it stopped. In place of the coverage row it reports the last stage it finished, and it names no model when none answered. The check run repeats the cause and the same table, so neither output leaves the reader guessing what ran.

Rewriting that body cannot change a review's state, so a failure withdraws every approval the service still holds. The service can hold more than one, because a decision review carries its own approval separately from the review that owns the visible body. An approval that outlived a failed review would otherwise keep satisfying branch protection for a head nobody reviewed. A later successful review approves again. An approval that cannot be withdrawn is named in the check run with the reason GitHub gave.

A restart is not a review outcome. Shutdown waits for the reviews already running and only cancels the ones that outlast the drain budget, which is one review timeout plus a minute. A review that a restart does cancel reports an interrupted check rather than a failed one, writes no failure comment, and leaves no review marker, so the next push reviews that head.

Every reported GitHub failure names the request method, the path, the status, and the reason GitHub gave. A request that never reaches GitHub reports the stage it failed at and the underlying transport error.

Three failures leave the body untouched, because writing it needs the GitHub call that just failed: reading reviews, updating a review, and submitting a review. The check run still reports those. A body that cannot be written never changes or hides the failure the check reports.

Before each decision, the service reads every prior bot review and thread. It silently resolves fixed threads before publishing new findings.

Stable finding identities prevent republication across pull request history. The configured unresolved thread limit admits the highest importance new findings first. The final decision still considers every current finding.

The service never posts issue comments, progress messages, replies, or commands.

## Configure the service

Set these required environment variables:

| Variable | Value |
| --- | --- |
| `GITHUB_APP_ID` | Existing GitHub App numeric identifier |
| `GITHUB_PRIVATE_KEY` | Existing GitHub App RSA private key |
| `GITHUB_WEBHOOK_SECRET` | Existing webhook signing secret |
| `GITHUB_BOT_LOGIN` | Exact GitHub App bot login, including the `[bot]` suffix |
| `CLYDE_BASE_URL` | HTTPS endpoint for model requests |
| `CLYDE_API_KEY` | Clyde API credential |
| `CF_ACCESS_CLIENT_ID` | Cloudflare Access service token identifier |
| `CF_ACCESS_CLIENT_SECRET` | Cloudflare Access service token secret |
| `REVIEW_MIN_IMPORTANCE` | Minimum published importance from `1` through `10` |
| `REVIEW_MAX_UNRESOLVED_COMMENTS` | Maximum unresolved bot threads, including `0` |
| `REVIEW_TIMEOUT` | Maximum duration for one active review, such as `10m` |
| `REVIEW_WORKERS` | Maximum reviews that can run at once |
| `REVIEW_MODEL` | Model the primary provider serves, such as `gpt-5.6-sol` |

`PORT` defaults to `3000`.

Keep every credential in the deployment secret store. Do not place values in source, commands, logs, or evidence.

## Configure a fallback provider

When the primary provider reports that it has no remaining usage, the service repeats the same request against a second endpoint. A review that succeeds there is published exactly as it would be otherwise, and the review details name the fallback model that answered.

The service tries the primary provider on every request and remembers nothing, so it returns to the primary as soon as that provider has usage again.

Leaving every fallback variable unset keeps the service on one provider and changes nothing.

| Variable | Value |
| --- | --- |
| `FALLBACK_BASE_URL` | HTTPS endpoint for the fallback provider |
| `FALLBACK_MODEL` | Model the fallback provider serves |
| `FALLBACK_API_KEY` | Fallback provider credential |
| `FALLBACK_CF_ACCESS_CLIENT_ID` | Cloudflare Access service token identifier, only for a fallback behind Access |
| `FALLBACK_CF_ACCESS_CLIENT_SECRET` | Cloudflare Access service token secret, paired with the identifier |
| `FALLBACK_ON` | Condition that sends a request to the fallback. Only `usage_exceeded` is supported, and that is the default |

Set `FALLBACK_BASE_URL`, `FALLBACK_MODEL`, and `FALLBACK_API_KEY` together. Setting one without the others stops the service from starting, so a half-configured fallback fails at deployment rather than during a review.

The Cloudflare Access pair is optional and also all-or-nothing. Leave both unset for a public endpoint, which then receives no Access headers.

Set every fallback value as a deployment secret, including the two that hold no credential. A Worker deployment replaces its declared variables with the ones in source while secrets persist, so keeping the group in the secret store means a deployment can never leave it half configured.

## Run the container

Deploy `ghcr.io/agoodkind/pr-review-agent` by digest. The image runs as user `65532:65532`, contains no shell, and listens on port `3000`.

Use `GET /health` for container readiness. Use `GET /` for the routed service status. Neither endpoint calls GitHub or Clyde.

## Verify a release

Verify release archives and the container attestation against this repository:

```bash
gh attestation verify <archive> --repo agoodkind/pr-review-agent
gh attestation verify oci://ghcr.io/agoodkind/pr-review-agent@<digest> \
    --repo agoodkind/pr-review-agent
```

Inspect the immutable image before deployment:

```bash
docker buildx imagetools inspect \
    ghcr.io/agoodkind/pr-review-agent@<digest>
```

Require only `linux/amd64`. Record the digest and current Worker version before deployment.

## Recover a failed deployment

Restore the recorded Worker version and image digest when readiness, routing, startup, or log checks fail. Do not continue live lifecycle testing after rollback.
