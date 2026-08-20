# Operations

The service turns each supported pull request push into one check and one final review decision for that head.

## Review lifecycle

The service accepts `opened`, `reopened`, `ready_for_review`, and `synchronize` pull request events. It ignores draft pull requests until they become ready.

Each head receives one `PR-Agent Review` check. The final decision is `APPROVE` or `REQUEST_CHANGES`.

The configured importance threshold controls publication. At least one anchored finding at or above the threshold requests changes. Otherwise, the service approves the head without visible commentary.

Published findings appear only as concise inline comments on changed lines. One editable top level body identifies the active findings review. Later decisions use hidden markers, so the pull request never gains another visible summary.

When a review cannot finish, the service rewrites that same top level body with the stage that failed and the reported cause, and creates it if the pull request has none yet. It carries no review marker, so the next push still reviews the head. When the model provider reports no remaining usage, the check run and the body both say so instead of naming a stage.

Three failures leave the body untouched, because writing it needs the GitHub call that just failed: reading reviews, updating a review, and submitting a review. The check run still reports those. A body that cannot be written never changes or hides the failure the check reports.

Before each decision, the service reads every prior bot review and thread. It resolves fixed threads before publishing new findings.

When the model judges a finding fixed, the service resolves that thread and then replies on it with the head it checked and the reason the model gave. The reply is posted only after the resolve succeeds, so it never claims a resolution that did not happen. A reply that fails to post is dropped rather than failing the review, and the thread stays resolved.

The service also resolves a thread when the finding anchor no longer exists. That is not a model judgement, so it posts no reply.

Stable finding identities prevent republication across pull request history. The configured unresolved thread limit admits the highest importance new findings first. The final decision still considers every current finding.

The service never posts issue comments, progress messages, or commands. Its only reply is the one it posts on a thread it just resolved.

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

When the primary provider reports that it has no remaining usage, the service repeats the same request against a second endpoint. A review that succeeds there is published exactly as it would be otherwise, and the pull request never says which provider answered. The fallback appears only in the service log.

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
