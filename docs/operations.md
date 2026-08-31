# Operations

The service reviews the commits pushed since the commit it last reviewed, and reports one verdict for the current head.

## Review lifecycle

The service accepts `opened`, `reopened`, `ready_for_review`, and `synchronize` pull request events. It ignores draft pull requests until they become ready. Each head receives one `PR-Agent Review` check.

The unit of work is the delta: everything changed between the last commit the service reviewed and the current head. On first contact that is the whole pull request. No commit range is ever reviewed twice, so a push costs a review proportional to the push rather than to the pull request.

The service owns one top level comment per pull request, created once and edited in place forever. It posts no other issue comment, and no progress message, reply, or command. The comment is the service's memory as well as its face. A reader sees the verdict, what that verdict is waiting on, and a collapsed table of review details: the model that answered, how long the review took, the head, the files and diff chunks read, and the finding and thread counts behind the decision. The check run renders the same table from the same values, so the two can never report different numbers.

Below the prose, hidden from the reader, the comment carries a state marker. It records the commit last reviewed, the chunks still owed, the chunks already read since that commit, the run identifier, and the run's status. That marker is what the next run resumes from, so it neither repeats a chunk that answered nor skips one that did not. Every write to the comment carries it, failure and skip notices included, because a body written without it makes the next run miss the comment and open a second one.

Before any model call, the service measures the delta and declines one that is over budget. The comment says the review was skipped and names the measured size, and no review object changes. The check concludes `action_required`, which stops short of any conclusion GitHub counts as passing, so an entirely unreviewed delta cannot merge on the strength of having been declined. A declined delta also leaves the last reviewed commit where it was. That oversized range therefore appears in every later delta and is declined again, however small the later pushes are. The way out is a person's: split the pull request, raise its budget, or ask for the review.

The service reads an admitted delta in chunks, one model request each, several chunks at a time. Every request carries its own timeout, so no clock spans two of them and a slow chunk takes nothing from the chunks beside it. A chunk's findings post as soon as that chunk answers, and only then is the chunk recorded as read, so an interrupted run loses only the chunks in flight.

A chunk the service could not read stays pending, and the comment says how many are left. Nothing is retried inside one run: the next push reviews what is still owed along with whatever it adds. A run that leaves anything pending does not move the last reviewed commit and touches no review object, because a failure to read is not a finding and requesting changes over one would let a provider outage block every open pull request with objections nobody raised. The merge gate holds anyway: the check concludes `action_required`, which GitHub does not count as passing, so a head with unread chunks cannot merge in a repository that requires the check. A comment GitHub answers and refuses is different, because no later attempt can change that answer: its chunk is recorded as read so the next run does not retry a post that can never land.

A model stops mid answer when it reaches its completion token budget, which reasoning and findings share, so a chunk yielding many findings can exhaust it. The service then splits that chunk in half and reviews each half, repeating while answers keep stopping early. A chunk holding one diff hunk cannot split, so the service skips it and reports incomplete coverage in the review details. One truncated answer never fails the whole review.

Findings appear as inline comments on the changed lines they object to. A finding is published when it anchors to a changed line and meets the configured importance threshold, and every finding that does is published. The only thing that withholds one is a stable identity matching a finding the pull request already carries, so the same defect is raised once rather than once per push. Before publishing new findings, the service re-reads its own open threads and silently resolves the ones the new code fixed.

The verdict is recomputed from scratch on every run, and its input is the service's own review threads. One of them still open requests changes. None open on a fully read head approves. No decision carries over from an earlier run, so a block never outlives the finding behind it, and resolving the last open thread turns the verdict around on the next push. When the verdict requests changes, the comment names each open thread holding it, linked to the comment it objects to, so a reader can go straight to the thing to act on.

Both inputs to that verdict are read after this run's findings are on the page. Threads read earlier would omit the ones the same run just opened, and a run would approve over defects it had raised minutes before. A head that moved while the run was working ends the run with no verdict at all, and the push that moved it gets the review.

A run that fails touches no review object. It has no verdict to publish and has not earned the right to withdraw the one standing, so it turns the check red and writes the cause into the comment, leaving the last reviewed commit and the pending chunks exactly as it found them. The next run neither repeats work already done nor skips work never done.

Neither the check nor the comment reprints what the failure said. A model provider error can carry the request it failed on, an internal endpoint, or a credential, and a check run is as public and as permanent as a comment. Both name the class of failure the service recognized and carry the run identifier instead. When the model provider reports no remaining usage, both say so rather than naming a stage.

The check also publishes the run's own log, every line and every field. A field's value is printed only when that field is one the service vouched for as its own measurement, identifier, or wording. Every other value is withheld, and the field says so where the value would be. Read the withheld values from the service log for that run identifier, following [logs.md](logs.md).

A run that stopped early carries the same detail table as one that finished, filled with what it had learned when it stopped. In place of the coverage row it names the last stage it completed, and it names no model when none answered. Findings post while the review runs, so a failed run can still leave comments on the page, and the table reports them.

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
| `REVIEW_WORKERS` | Maximum reviews that can run at once |
| `REVIEW_MODEL` | Model the primary provider serves, such as `gpt-5.6-sol` |

No variable bounds a whole review. Admission bounds what one run accepts, and the only clock is the one around a single model call, so a review can never run out of time part way through and discard what it already read.

| Variable | Value | Default |
| --- | --- | --- |
| `PORT` | Port the container listens on | `3000` |
| `REVIEW_MAX_FILES` | Files in one delta above which the review is skipped | `100` |
| `REVIEW_MAX_CHUNKS` | Diff chunks in one delta above which the review is skipped | `60` |
| `REVIEW_CHUNK_TIMEOUT` | Maximum duration for one model call, such as `5m` | `5m` |

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
