
# Operations

The service reviews changed repository content, pull request titles and descriptions, and commit messages. It reports a verdict for the current head and current metadata revision.

## Review lifecycle

The service accepts `opened`, `reopened`, `ready_for_review`, `synchronize`, and `edited` pull request events. Draft pull requests require a `ready_for_review` event. An edit starts a fresh review even when the head is unchanged. Each admitted review receives a `PR-Agent Review` check.

Mention `@goodkind-io-pr-agent` in a new top-level pull request comment to request a full review. A review label also requests a full review. Replies to the service's inline findings and changes to thread resolution refresh the verdict. The GitHub App requires `issue_comment`, `pull_request_review_comment`, `pull_request_review_thread`, and `pull_request` subscriptions.

Ordinary push reviews examine the changes since the last reviewed commit. The first review examines the whole pull request. The service updates one summary comment with the purpose, changes, verdict, and actionable metadata findings. File findings appear inline. Title, description, and commit-message findings identify the source lines in the summary. The reviewer includes an exact replacement or deletion when a safe correction is available. A verified metadata defect still requests changes when an exact correction is unavailable. Collapsed details report models, duration, usage, estimated cost, coverage, and thread identifiers.

The hidden state marker records the last reviewed commit, pending and completed chunks, run identifier, and status. Every summary update includes the marker. Resumed reviews reuse completed file chunks. Completed metadata chunks require a matching head and metadata revision. When the cached metadata record does not match, the service reviews the current metadata chunks again before approval.

The service declines a delta that exceeds its configured admission limits before sending model requests. The summary reports the measured size. The check concludes `action_required`, and the last reviewed commit does not advance. Subsequent pushes include the unreviewed range until a review completes or the pull request is split.

Each model request has a separate timeout. The service publishes a chunk's findings before checkpointing that chunk. Interrupted reviews resume pending chunks. A temporary model or publication failure preserves pending work and existing review decisions. GitHub's permanent refusal of an inline comment is recorded in the summary without repeatedly posting the refused comment.

The service measures the complete rendered input against its configured prompt budget. It trims surplus file context and splits chunks when needed. A provider response that stops at its output limit also triggers splitting. A single hunk that cannot fit or finish requires a decision about whether its unread content is necessary for the verdict.

Each finding requires source evidence and a valid target. The loaded rule policy determines importance before the publication threshold applies. Duplicate identities suppress repeated findings. The service reconciles its existing threads against current source before publication.

An open file thread or an actionable metadata finding requires `request_changes`. Approval requires current source coverage or an explicit decision that omitted content is unnecessary, plus no actionable findings. A discussion refresh reviews missing or changed metadata before approval. The service checks the head and metadata revision again before publishing a verdict.

A failed review changes no standing review verdict. The summary and check report the recognized failure class and run identifier. Raw provider messages remain in the private service log. Public log fields are limited to service-defined measurements, identifiers, and wording. Inspect the private records using [logs.md](logs.md).

## Configure the service

The service uses the Responses API unless a provider selects `chat_completions` or `gemini`. The Gemini integration uses Google's Go SDK. Each API streams one response per review stage over HTTP.

Edit [runtime.json](../runtime.json) to set the models, publication threshold, review limits, and service port. Merge the change to deploy it. The release packages the file into the Go container and Worker. The Go service reads it at startup. The Worker signs the review limits on each webhook. A new limit applies to the next review without restarting the container.

| Setting | Value |
| --- | --- |
| `GITHUB_APP_ID`, `GITHUB_BOT_LOGIN` | Existing GitHub App identity |
| `PROVIDERS` | Provider IDs, endpoints, models, and Cloudflare secret binding names |
| `PROVIDERS[].api_kind` | Select `responses`, `chat_completions`, or Google's native `gemini` API |
| `PROVIDERS[].reasoning_effort` | Reasoning effort for every review stage sent to that provider; select a level supported by the configured model |
| `PROVIDER_PRIORITY` | Provider IDs in request order |
| `PROVIDER_FAILURE_POLICY` | `any_failure` selects the next provider after any unsuccessful attempt; `recoverable` selects it only for quota, rate-limit, or availability errors |
| `PROVIDERS[].request_timeout` | Positive duration for one provider attempt; omit it to use the review timeout |
| `PROVIDERS[].max_output_tokens` | Maximum output tokens per request for one provider; omit or set `0` to use 8,000 |
| `PROVIDERS[].omit_max_output_tokens` | Omit the output token limit for a backend that rejects it |
| `PROVIDERS[].omit_text_format` | Omit JSON schema enforcement for a backend that rejects it; the system prompt still requests JSON matching the schema |
| `PROVIDERS[].auto_router_cost_tier` | Set the OpenRouter auto router's cost tier; `low` favors inexpensive models |
| `PROVIDERS[].disabled` | Keep the provider configured without sending it requests |
| `PROVIDER_BUDGET_URL` | Worker endpoint that records reported usage after model requests |
| `SERVICE_FAILURE_APPEARANCE` | Choose whether each service failure class blocks the check |
| `REVIEW_MIN_IMPORTANCE` | Minimum published importance from `1` through `10` |
| `REVIEW_RULE_IMPORTANCE` | Fixed importance values indexed by stable rule ID |
| `REVIEW_RULES_FILE` | Required path to the external rule catalog |
| `REVIEW_PROMPTS_FILE` | Required path to the external stage prompt templates |
| `REVIEW_WORKERS` | Maximum reviews that can run at once |
| `REVIEW_MAX_FILES`, `REVIEW_MAX_CHUNKS` | Admission limits for one review |
| `REVIEW_CHUNK_TIMEOUT` | Timeout for one model request |
| `REVIEW_CHUNK_CONCURRENCY` | Maximum chunks reviewed concurrently within one review |
| `REVIEW_MAX_PROMPT_BYTES` | Chunk and context text budget, in bytes |
| `REVIEW_MODEL_PRICING` | Estimated dollars per million input, cached input, and output tokens by model |
| `PORT` | Sets the container listener port and the Worker connection port |
| `CONTAINER_SLEEP_AFTER` | Container idle duration |
| `LOG_FORWARD_URL` | Service log destination |

The usage estimate uses configured paid list rates. It does not subtract free-tier usage or [complimentary data-sharing tokens](https://help.openai.com/en/articles/10306912-sharing-feedback-evaluation-and-fine-tuning-data-and-api-inputs-and-outputs-with-openai). The report marks an unpriced model as unknown. The token limit applies to the provider's requests from this service.

Keep `GITHUB_PRIVATE_KEY`, `GITHUB_WEBHOOK_SECRET`, and the provider credentials in Cloudflare secret bindings. Set each provider's binding names in the runtime configuration. The Go service rejects secret values in that file.

## Configure provider order

List every configured provider ID once in `PROVIDER_PRIORITY`, including providers marked `disabled`. Put the preferred provider first. The service rejects incomplete providers and invalid priority lists at startup.

The configured `any_failure` policy selects the next enabled provider after an unsuccessful request, timeout, invalid response, or provider adapter panic. Each provider receives one attempt with a separate timeout. Parent cancellation stops provider selection. The service accepts a provider response after the stage's decoding and validation succeed.

OpenAI bills requests outside its complimentary data-sharing allowance at normal API rates.

Set `disabled` to keep a provider available in the configuration without sending it requests. The service still validates that provider and its credentials. At least one provider must stay enabled.

Set `SERVICE_FAILURE_APPEARANCE` to choose whether each service failure class blocks the check. The available classes are `usage_exceeded`, `rate_limited`, `daily_budget`, `provider_unavailable`, `deadline`, `panic`, and `other`.

Set a class to `fail` to preserve the existing blocking conclusion. An aborted run concludes with `failure`. A run with unread chunks concludes with `action_required`.

Set a class to `pass` to conclude the check with `success`. Every failure class in the run must be set to `pass`. One passing class cannot hide a blocking class. An omitted class remains blocking.

The shipped configuration treats app quota exhaustion and provider allowance exhaustion as nonblocking. An incomplete review publishes no approval.

The selected appearance changes only the check conclusion. The check title and the summary comment still report the recognized failure class. The run publishes no review verdict.

Unread chunks stay pending. The next push reviews those chunks and any new changes.

Configure each provider's quota fields using the [quota reference](quotas.md). Select input and output when matching OpenAI's complimentary data-sharing allowance. Keep the quota endpoint and its Durable Object binding configured when any provider has a cap.

Set both `cf_access_client_id_binding` and `cf_access_client_secret_binding` for a provider behind Cloudflare Access. Omit both fields for public endpoints.

Change credentials through Cloudflare secret bindings. Set each provider's `api_key_binding` to its Cloudflare secret name, and upload the secret before merging the runtime configuration. The Go service validates the runtime configuration and available credentials at startup.

## Run the container

Deploy `ghcr.io/agoodkind/pr-review-agent` by digest. The image runs as user `65532:65532` and contains no shell.

Use `GET /health` for container readiness. Use `GET /` for the routed service status. Neither endpoint sends a model request.

## Change review rules

1. Edit the [rule catalog](../config/review-rules.json) or [prompt templates](../config/review-prompts.json). Preserve each rule's `id` when changing its title or instructions. The service reads both files at startup. The binary contains no default rules or prompts.
2. Set `REVIEW_RULE_IMPORTANCE` in the public runtime configuration to override a technical rule's model score. For example:

   ```json
   "REVIEW_RULE_IMPORTANCE": {
     "shared_boundaries": 8
   }
   ```

   The catalog's category settings define fixed importance and which rules apply to generated review text. The shipped prose category requires importance `10`. A rule without fixed category importance or an override uses the model's score. `REVIEW_MIN_IMPORTANCE` controls publication after these scores are applied.
3. Run `go test ./internal/domain ./internal/review ./internal/openai ./internal/config` and `make check`. Verify findings and corrections with the live procedure in [acceptance.md](acceptance.md), then merge the change to deploy it.

4. Confirm that the Release workflow completed successfully. The committed Cloudflare configuration contains an invalid image placeholder. For a manual deployment, set `PR_REVIEW_AGENT_IMAGE` to the verified image digest and run `npm run deploy` from `deploy/cloudflare`.

## Resume reviews

The GitHub App installation controls webhook delivery. A suspended installation cannot start new reviews. Set `GITHUB_APP_KEY_FILE` to the matching private key path, `GITHUB_APP_ID` to the deployed App ID, and `GITHUB_APP_INSTALLATION_ID` to the account's installation ID. After a successful release, run:

```bash
python3 scripts/resume-github-app.py \
    --key-file "$GITHUB_APP_KEY_FILE" \
    --app-id "$GITHUB_APP_ID" \
    --installation-id "$GITHUB_APP_INSTALLATION_ID"
```

The script requires Python 3 and OpenSSL. Add `--status` to inspect the installation without changing it. Without that flag, the script resumes a suspended installation and confirms that GitHub reports it active. Existing failed checks do not rerun automatically. Add a temporary `test-review-agent-` label to a suitable pull request when a same-head review is required, then remove the label after checking the completed `PR-Agent Review` result.

## Verify a release

The Worker stores each signed GitHub delivery before forwarding the delivery to the container. The Durable Object retains the delivery until the service confirms that the review has finished or the event requires no review. A container rollout can interrupt a review. The replay alarm retries pending deliveries at intervals of at most five minutes after the old container exits.

1. Confirm that the routed service answers `GET /health`.
2. Compare the deployed Worker version and container image digest with the release record.
3. Confirm that a newly admitted delivery produces a completed `PR-Agent Review` check.
4. Inspect Worker logs for `webhook review overdue` if a check remains in progress after the container has recovered. Match the delivery identifier with the GitHub webhook.

Test discussion webhooks against the deployed service:

1. Keep the pull request in [recovery-test.json](../deploy/cloudflare/recovery-test.json) open with at least one unresolved review thread started by the PR Agent.
2. Authenticate `gh`.
3. Run `go run ./cmd/pragent-live-webhooks` from the repository root.

The command confirms that an untagged comment starts no review. A tagged comment must produce a complete forced review. An inline reply, thread resolution, and thread restoration must each create a distinct successful check on the unchanged head. The command deletes its test comments and verifies the original thread state, including after a failed assertion. Completed check runs remain in GitHub history.

The live recovery test sends `SIGKILL` to the single production container. Any other active reviews also stop and depend on delivery replay. Run the test when that interruption is acceptable.

1. Keep the configured test pull request open. Set the repository, pull request number, and label names in [recovery-test.json](../deploy/cloudflare/recovery-test.json) if the test pull request changes.
2. Authenticate `gh` and Wrangler. Run `npm ci` in `deploy/cloudflare`.
3. Run `go run ./cmd/pragent-live-recovery` from the repository root.

The command starts a forced review, interrupts the container after GitHub marks the new check in progress, and requires the original check to complete successfully. It also requires live Worker logs to record the interruption and the resumed delivery. The command removes both labels from the pull request after the test.

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

If the container is unavailable, restore its recorded image digest and verify `GET /health`. The Durable Object retains accepted deliveries during the outage. After recovery, confirm that the replay queue has settled each accepted delivery. For events requiring review, inspect the affected pull request for a completed `PR-Agent Review` check. If a check remains in progress after the next retry interval, inspect the Worker log by delivery identifier and the service log by run identifier before requesting another review.
