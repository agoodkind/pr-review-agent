# Operations

The service turns each supported pull request push into one check and one final review decision for that head.

## Review lifecycle

The service accepts `opened`, `reopened`, `ready_for_review`, and `synchronize` pull request events. It ignores draft pull requests until they become ready.

Each head receives one `PR-Agent Review` check. The final decision is `APPROVE` or `REQUEST_CHANGES`.

The configured importance threshold controls publication. At least one anchored finding at or above the threshold requests changes. Otherwise, the service approves the head without visible commentary.

Published findings appear only as concise inline comments on changed lines. One editable top level body identifies the active findings review. Later decisions use hidden markers, so the pull request never gains another visible summary.

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

`PORT` defaults to `3000`.

Keep every credential in the deployment secret store. Do not place values in source, commands, logs, or evidence.

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
