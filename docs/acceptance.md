# Prove the durable review works

The tests prove the design holds in isolation. This page proves it holds against
the real service, which is the only evidence that counts before trusting it on
your own pull requests. Run it once after each deploy of a review behavior
change.

Nothing here is automatic. Merging publishes a container image and stops there,
so a deploy is a deliberate act and so is this.

## Deploy first

```bash
cd deploy/cloudflare
npx wrangler deploy
```

The container image the Worker runs is published by the release workflow on
every push to the trunk. Check that the image you expect is the one live before
reading anything below, or you will be testing the previous build.

## Watch what happens

Every check below is easier with the log open in another terminal:

```bash
cd deploy/cloudflare
npx wrangler tail agoodkind-nano-pr-reviewer --format json
```

The run identifier on the check and in the top level comment is the same string
as the `request_id` on every log line, so a single run reads end to end. When a
run has already finished, pull its lines from Workers Logs instead, using the
procedure in [logs.md](logs.md).

## The three proofs

### 1. A pull request too large is declined, and cannot merge

Push to a pull request whose delta exceeds `REVIEW_MAX_FILES` or
`REVIEW_MAX_CHUNKS`. The 173 chunk pull request on mlx-swift-lm is the one this
design was built against.

Expect all of these, and treat any one of them failing as a failed acceptance:

- the top level comment says the review was skipped, names the measured size,
  and gives the reason
- the check does NOT reach a passing conclusion, so the pull request cannot
  merge on the strength of having been declined
- no review is submitted, none is dismissed, and no inline comment appears
- the logs show no model call for that run

Then push one small commit to the same pull request. The delta still contains
the oversized range, so it must be declined again rather than reviewed. A pull
request that suddenly passes here means the baseline moved when it should not
have, and unreviewed code is about to merge.

### 2. A normal pull request completes in one run, traceable end to end

Open a pull request of ordinary size. Expect:

- one check moving from in progress to a conclusion
- exactly one top level comment, carrying the summary and the state marker
- findings inline, and silence below the configured bar
- the same run identifier on the check, in the comment, and on every log line
  for that run

Push a fix for something it raised. Expect the thread it opened to resolve
itself and the verdict to be recomputed, so the pull request unblocks without
anyone dismissing a review.

### 3. A failed chunk stays visible and finishes on the next push

Force one chunk to fail. The cheapest way is to exhaust the model provider's
usage, which is what produced the failure this design was built around; failing
that, point the service at an unreachable provider for a single run.

Expect:

- the comment names the chunk that went unread and says the next push reviews it
- the check does NOT pass, because the head was not fully read
- no review object is touched by the failure itself
- the raw provider error appears in the logs and NOWHERE on the pull request

Push again with the provider healthy. The pending chunk must be reviewed, the
chunks already read must not be re-analyzed, and no finding may be posted twice.

## What a failure here means

These three cases are the ones the recorded failures came from. If any of them
behaves differently on the live service than the tests say, trust the live
service and treat the difference as the defect. Record what you saw, pull the
run's log by its identifier, and stop using the service on real pull requests
until it is explained.
