# Prove the durable review works

The tests prove the design holds in isolation. This page proves it holds
against the real service, which is the only evidence that counts before
trusting it on your own pull requests. Run it once after each deploy of a
review behavior change. Each proof below exists because the behavior it checks
failed live at least once; the failure it guards against is named so a
regression is recognizable on sight.

Merging to the trunk deploys: the release workflow builds the container image,
rewrites the deploy configuration to that exact digest, and deploys the Worker.
The proofs below are the deliberate part.

## Never deploy by hand with wrangler alone

The committed configuration carries an unbuildable image placeholder on
purpose, so a raw `wrangler deploy` fails before it can touch production. It
once shipped a fourteen day old digest, because only the release workflow
rewrites the digest at deploy time, and the stale image crash looped on its
next cold start while 33 webhook deliveries died. When a manual deploy is
genuinely needed, name the digest explicitly:

```bash
cd deploy/cloudflare
PR_REVIEW_AGENT_IMAGE=ghcr.io/agoodkind/pr-review-agent@sha256:<digest> npm run deploy
```

A deploy restarts the Worker and kills every review in flight, so check the
log for a running review before deploying. Confirm the image you expect is the
one live before reading anything below, or you will be testing the previous
build.

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

## 1. A normal pull request completes in one run

Open a small pull request with one real defect in it.

Expect:

- one check named PR-Agent Review that runs and concludes `success`
- the defect as an inline comment; below the configured importance, silence
- exactly one top level comment, created once, carrying the summary, the
  detail table, the run identifier, and the state marker with `last_reviewed`
  at the head and `status=done`
- one verdict review that states its decision in prose. An empty verdict
  blocked a live pull request once: the review's whole body was one HTML
  marker, so it named nothing to fix and no edit could satisfy it
- the verdict review carries no detail table. The table lives on the comment;
  a review repeating it rendered as two near identical Review boxes
- one run identifier on the check text, the comment marker, and every log line

## 2. The same delivery and the same head never review twice

Redeliver the webhook, or close and reopen the pull request at the same head.

Expect: `review job suppressed` in the log and no new model call, no new
comment, no new review. Proven live on 2026-08-30, and it is why replaying
webhook deliveries is safe.

## 3. A fixed pull request unblocks itself

On the pull request from proof 1, fix what the inline comment raised and push.

Expect:

- the thread the service opened resolves itself during reconciliation
- the verdict is recomputed and an approval replaces the standing block, with
  nobody dismissing anything. The service once wrote `CHANGES_REQUESTED` and
  never withdrew it; every recorded block before the redesign was cleared by
  hand
- the second run reviews only the delta: the log shows a compare fetch, never
  a second full file listing

Resolving threads without pushing triggers nothing: runs start only on
pull request webhooks. The next push is what recomputes.

## 4. A run never approves over its own findings

On a pull request where the run posts a new finding, expect the same run to
request changes, naming the thread it just opened under "Waiting on". The
verdict reads its threads after publication precisely so it cannot approve
over a defect it raised minutes earlier.

## 5. An oversized delta is declined and stays declined

mlx-swift-lm 8 is the standing target: 200 changed files against the 100 file
budget. Trigger it with a reopen.

Expect:

- the comment says skipped and names the measured size
- the check concludes `action_required`, never `skipped` or `neutral`: GitHub
  counts both of those as satisfying a required check, and each of the two was
  shipped once before the suite caught it
- the marker's `last_reviewed` does NOT advance. It advanced once, and the
  next small push then measured only itself, approved, and would have merged
  the unreviewed range; a plain redelivery opened the same gate without even
  a push
- no review object is touched, and nothing retries
- a later small push is STILL declined, because the un-advanced baseline keeps
  the oversized range in every later delta. The way out is a person's: split
  the pull request, raise its budget, or ask for the review

## 6. A failed chunk costs one chunk and no verdict

Force one chunk to fail: exhaust the provider's usage, or point one run at an
unreachable provider. A real provider outage returning HTTP 502 ran this proof
unprompted on 2026-08-30.

Expect:

- the comment names how many chunks went unread and says the next push reviews
  them; the marker keeps them in `pending` and the chunks already read in
  `completed`
- the check concludes `action_required` so the gate holds
- NO review object is touched. A failure to read is not a finding: the outage
  build submitted `CHANGES_REQUESTED` here, and every open pull request grew a
  blocking review nobody had asked for
- the raw provider error appears in the logs and NOWHERE on the pull request:
  not the comment, not the check title, not the check's run log, which
  publishes only fields the service vouched for and escapes line breaks in
  them
- push again with the provider healthy: the pending chunks are reviewed, the
  completed ones are not re-analyzed, and no finding posts twice

## 7. A failed run reports its cause and touches nothing

Make a run fail outside the chunks, for example by breaking the GitHub token
briefly.

Expect: a red check and a comment that both name the stage and the run
identifier, a sanitized cause, and every review object exactly as the reader
last saw it. One live failure rewrote an older blocking review's body while
leaving its state standing, so an infrastructure outage read as a code
verdict.

## 8. A lost delivery is replayed, not dropped

While the container is cold starting or crash looping, deliveries used to die:
GitHub delivers a webhook once, so 33 deliveries returned 500 during one
outage and the affected pull requests were left with a required check that
never appeared, blocked with nothing a person could point at.

Kill the container (a deploy does it) and open a pull request in the same
minute.

Expect: `webhook queued for replay` in the Worker log, a 202 to GitHub, then
`webhook replayed` once the container answers, and the check appears without
any human touching the pull request. Deliveries the service itself answers,
including 4xx refusals, are never replayed.

## 9. A forged marker cannot skip the review

Post a comment on a test pull request impersonating the state marker with
`last_reviewed` at the head, from any account that is not the app.

Expect the run to ignore it entirely: authorship is the gate and the marker
only locates the comment behind it. Trusting the marker alone would let any
commenter mark code reviewed.

## 10. Killing the process loses only the chunks in flight

Kill the container mid run on a multi chunk pull request.

Expect the marker to keep every checkpointed chunk in `completed`, the next
triggering event to review only what is owed, and no finding to post twice.
The pre-redesign service lost the whole run: 31 logged timeouts came from one
shared ten minute clock over a 173 chunk diff, and death kept no progress at
all.
