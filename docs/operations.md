# Operations

The service reviews the commits pushed since the commit it last reviewed, and reports one verdict for the current head.

## Review lifecycle

The service accepts `opened`, `reopened`, `ready_for_review`, and `synchronize` pull request events. It ignores draft pull requests until they become ready. Each accepted review request receives a `PR-Agent Review` check.

Mention `@goodkind-io-pr-agent` in a new top level pull request comment to request a full review of the current head. The service also processes replies to its inline findings and changes to review thread resolution. These discussion events refresh the existing verdict without a push. The GitHub App must subscribe to `issue_comment`, `pull_request_review_comment`, and `pull_request_review_thread` in addition to `pull_request`.

Ordinary push reviews examine everything changed between the last reviewed commit and the current head. The first review examines the whole pull request. A tagged comment or review label requests another full review of the current head.

The service owns one top level comment per pull request, created once and edited in place forever. It posts no other issue comment, and no progress message, reply, or command. The visible comment states the pull request purpose, its distinct changes, and the verdict once. Findings stay in their inline comments. The collapsed review details carry the models, duration, token usage, estimated cost, head, coverage, finding counts, and thread identifiers. The check run renders the same details from the same values, so the two cannot report different numbers.

Below the prose, hidden from the reader, the comment carries a state marker. It records the commit last reviewed, the chunks still owed, the chunks already read since that commit, the run identifier, and the run's status. That marker is what the next run resumes from, so it neither repeats a chunk that answered nor skips one that did not. Every write to the comment carries it, failure and skip notices included, because a body written without it makes the next run miss the comment and open a second one.

Before any model call, the service measures the delta and declines one that is over budget. The comment says the review was skipped and names the measured size, and no review object changes. The check concludes `action_required`, which stops short of any conclusion GitHub counts as passing, so an entirely unreviewed delta cannot merge on the strength of having been declined. A declined delta also leaves the last reviewed commit where it was. That oversized range therefore appears in every later delta and is declined again, however small the later pushes are. The way out is a person's: split the pull request, raise its budget, or ask for the review.

The service reads an admitted delta in chunks, one model request each, several chunks at a time. Every request carries its own timeout, so no clock spans two of them and a slow chunk takes nothing from the chunks beside it. A chunk's findings post as soon as that chunk answers, and only then is the chunk recorded as read, so an interrupted run loses only the chunks in flight.

A chunk the service could not read stays pending, and the comment says how many are left. Nothing is retried inside one run: the next push reviews what is still owed along with whatever it adds. A run that leaves anything pending does not move the last reviewed commit and touches no review object, because a failure to read is not a finding and requesting changes over one would let a provider outage block every open pull request with objections nobody raised. The merge gate holds anyway: the check concludes `action_required`, which GitHub does not count as passing, so a head with unread chunks cannot merge in a repository that requires the check. A comment GitHub answers and refuses is different, because no later attempt can change that answer: its chunk is recorded as read so the next run does not retry a post that can never land.

A model stops mid answer when it reaches its completion token budget, which reasoning and findings share, so a chunk yielding many findings can exhaust it. The service then splits that chunk in half and reviews each half, repeating while answers keep stopping early. A chunk holding one diff hunk cannot split, so the service skips it and reports incomplete coverage in the review details. One truncated answer never fails the whole review.

Findings appear as inline comments on the changed lines they object to. A finding is published when it anchors to a changed line and meets the configured importance threshold, and every finding that does is published. The only thing that withholds one is a stable identity matching a finding the pull request already carries, so the same defect is raised once rather than once per push. Before publishing new findings, the service re-reads its own open threads and silently resolves the ones the new code fixed.

The review treats verified writing defects in changed prose as importance `10`. This includes documentation, comments, strings, test names, and messages. These findings use the existing inline comment and requested-changes lifecycle.

The verdict is recomputed from scratch on every run, and its input is the service's own review threads. One of them still open requests changes. None open on a fully read head approves. No decision carries over from an earlier run, so a block never outlives the finding behind it. A reply or thread state change starts a refresh without a push and publishes a dedicated in-progress check while the verdict changes. A requested changes verdict directs the reader to the open inline findings. GitHub connects the review to those comments, and the collapsed details carry their thread identifiers and current counts.

Both inputs to that verdict are read after this run's findings are on the page. Threads read earlier would omit the ones the same run just opened, and a run would approve over defects it had raised minutes before. A head that moved while the run was working ends the run with no verdict at all, and the push that moved it gets the review.

A run that fails touches no review object. It has no verdict to publish and has not earned the right to withdraw the one standing, so it turns the check red and writes the cause into the comment, leaving the last reviewed commit and the pending chunks exactly as it found them. The next run neither repeats work already done nor skips work never done.

Neither the check nor the comment reprints what the failure said. A model provider error can carry the request it failed on, an internal endpoint, or a credential, and a check run is as public and as permanent as a comment. Both name the class of failure the service recognized and carry the run identifier instead. When the model provider reports no remaining usage, both say so rather than naming a stage.

The check also publishes the run's own log, every line and every field. A field's value is printed only when that field is one the service vouched for as its own measurement, identifier, or wording. Every other value is withheld, and the field says so where the value would be. Read the withheld values from the service log for that run identifier, following [logs.md](logs.md).

A run that stopped early carries the same detail table as one that finished, filled with what it had learned when it stopped. In place of the coverage row it names the last stage it completed, and it names no model when none answered. Findings post while the review runs, so a failed run can still leave comments on the page, and the table reports them.

## Configure the service

Edit [runtime.json](../runtime.json) to set the models, publication threshold, review limits, and service port. Merge the change to deploy it. The release packages the file into the Go container and Worker. The Go service reads it at startup. The Worker signs the review limits on each webhook. A new limit applies to the next review without restarting the container.

| Setting | Value |
| --- | --- |
| `GITHUB_APP_ID`, `GITHUB_BOT_LOGIN` | Existing GitHub App identity |
| `PROVIDERS` | Provider IDs, endpoints, models, and Cloudflare secret binding names |
| `PROVIDER_PRIORITY` | Provider IDs in request order |
| `REVIEW_MIN_IMPORTANCE` | Minimum published importance from `1` through `10` |
| `REVIEW_WORKERS` | Maximum reviews that can run at once |
| `REVIEW_MAX_FILES`, `REVIEW_MAX_CHUNKS` | Admission limits for one review |
| `REVIEW_CHUNK_TIMEOUT` | Timeout for one model request |
| `REVIEW_MODEL_PRICING` | Estimated dollars per million input, cached input, and output tokens by model |
| `PORT` | Sets the container listener port and the Worker connection port |
| `CONTAINER_SLEEP_AFTER` | Container idle duration |
| `LOG_FORWARD_URL` | Service log destination |

The configured order selects Clyde with `gpt-6-luna` first. OpenAI's direct API uses `gpt-5.6-luna` when Clyde reports exhausted usage. The usage report estimates API cost from the rates in the runtime file. Eligible OpenAI API traffic may qualify for [complimentary data-sharing tokens](https://help.openai.com/en/articles/10306912-sharing-feedback-evaluation-and-fine-tuning-data-and-api-inputs-and-outputs-with-openai). The estimate does not subtract complimentary tokens. The usage report marks an unpriced model as unknown.

Keep `GITHUB_PRIVATE_KEY`, `GITHUB_WEBHOOK_SECRET`, `OPENAI_KEY`, `FALLBACK_API_KEY`, and the optional `CF_ACCESS_CLIENT_ID` and `CF_ACCESS_CLIENT_SECRET` pair in Cloudflare secret bindings. The existing `FALLBACK_API_KEY` binding authenticates OpenAI. The existing `OPENAI_KEY` binding authenticates Clyde. Each provider selects its bindings in the runtime file. The Go service rejects secret values in the runtime file.

## Configure a fallback provider

List every configured provider ID once in `PROVIDER_PRIORITY`. Put the preferred provider first. The service tries the next provider only when the previous provider reports exhausted API usage. The service rejects incomplete providers and invalid priority lists at startup. Exhausting the complimentary data-sharing quota does not activate the fallback. OpenAI bills requests outside that quota at normal API rates.

Set both `cf_access_client_id_binding` and `cf_access_client_secret_binding` for a provider behind Cloudflare Access. Omit both fields for public endpoints.

Change credentials through Cloudflare secret bindings.

The release packages the runtime file with the container image and Worker source from the same commit. Secrets persist across deployment. The Go service validates the runtime file and available credentials at startup. The release checks routed service readiness after deployment.

## Run the container

Deploy `ghcr.io/agoodkind/pr-review-agent` by digest. The image runs as user `65532:65532` and contains no shell.

Use `GET /health` for container readiness. Use `GET /` for the routed service status. Neither endpoint calls GitHub or Clyde.

## Change review rules

Edit [review-rules.md](../internal/review/review-rules.md). Its opening paragraph resolves scope and importance conflicts. Each heading introduces one review rule. The service includes the whole file in the model's review instructions. The Go build includes this file in the binary, so rule edits require a new release.

Run `go test ./internal/review ./internal/openai` and `make check`. Merge the change into `main`. The Release workflow builds an immutable image, deploys the Cloudflare Worker with that image, and checks the routed service. Inspect the Release run before claiming the new rules are live. Do not deploy the committed Cloudflare configuration directly: it contains an invalid image placeholder. For a manual deployment, set `PR_REVIEW_AGENT_IMAGE` to the verified image digest and run `npm run deploy` from `deploy/cloudflare`.

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
