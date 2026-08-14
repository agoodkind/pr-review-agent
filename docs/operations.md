# Operations

The service turns supported GitHub pull request events into one check and one review for each head commit.

## Review lifecycle

The service accepts `opened`, `reopened`, `ready_for_review`, and `synchronize` pull request events. It ignores draft pull requests until they become ready.

Each head receives one `PR-Agent Review` check and one top level review. Findings on changed right side lines become inline review comments. Other findings remain in the review body.

Complete coverage with no findings approves the head. A finding with importance 7 or higher requests changes. Incomplete coverage or lower importance findings produce a comment review.

Later heads receive new reviews. The service silently resolves earlier owned findings only when reconciliation classifies them as fixed. It never posts issue comments, progress comments, replies, or commands.

## Configure the service

Set these required environment variables:

| Variable | Value |
| --- | --- |
| `GITHUB_APP_ID` | Existing GitHub App numeric identifier |
| `GITHUB_PRIVATE_KEY` | Existing GitHub App RSA private key |
| `GITHUB_WEBHOOK_SECRET` | Existing webhook signing secret |
| `CLYDE_BASE_URL` | HTTPS endpoint for model requests |
| `CLYDE_API_KEY` | Clyde API credential |
| `CF_ACCESS_CLIENT_ID` | Cloudflare Access service token identifier |
| `CF_ACCESS_CLIENT_SECRET` | Cloudflare Access service token secret |

`PORT` defaults to `3000`. `GITHUB_BOT_LOGIN` defaults to `agoodkind-pr-review-agent[bot]`.

Keep every credential in the deployment secret store. Do not place values in source, commands, logs, or evidence.

## Run the container

Deploy `ghcr.io/agoodkind/pr-review-agent` by digest. The image runs as user `65532:65532`, contains no shell, and listens on port `3000`.

Use `GET /health` for container readiness. Use `GET /` for the routed service status. Neither endpoint calls GitHub or Clyde.

## Verify a release

Verify release archives and the container attestation against this repository:

```bash
gh attestation verify <archive> --repo agoodkind/pr-review-agent \
    --signer-workflow agoodkind/go-makefile/.github/workflows/_package.yml
gh attestation verify oci://ghcr.io/agoodkind/pr-review-agent@<digest> \
    --repo agoodkind/pr-review-agent
```

Inspect the immutable image before deployment:

```bash
docker buildx imagetools inspect \
    ghcr.io/agoodkind/pr-review-agent@<digest>
```

Require `linux/amd64` and `linux/arm64`. Record the digest and current Worker version before deployment.

## Recover a failed deployment

Restore the recorded Worker version and image digest when readiness, routing, startup, or log checks fail. Do not continue live lifecycle testing after rollback.
