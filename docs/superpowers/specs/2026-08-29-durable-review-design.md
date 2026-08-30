# Durable incremental review

Supersedes the 2026-08-29 reviewer model design and its plan. The failure
evidence lives in [failures.md](../../failures.md).

## The missing piece

The review has no memory. Every failure in the record follows from that one
gap: each run re-reviews the whole diff from zero, races one fixed deadline,
holds its progress only in process memory, and writes a verdict it never
recomputes. A big pull request can therefore never finish, a dead run loses
everything, and a stale block stands until a human clears it.

CodeRabbit's model is the reference experience: a full review once, then
incremental reviews covering only the commits since the last reviewed one,
one walkthrough comment updated in place, and remembered state between runs.
Work per run is proportional to the push, so no run needs a big deadline and
no prompt outgrows the model's context.

## Principles

1. **GitHub is the database.** Every piece of durable state is readable on
   the pull request itself. No external store, nothing hidden in a container.
2. **Work is incremental.** Review the delta since the last reviewed commit.
   Never review the same commit range twice.
3. **Progress checkpoints after every chunk.** A chunk is done when its
   findings are on the page and the checkpoint has advanced. Death loses at
   most one chunk.
4. **One clock per model call, none above it.** Each call carries its own
   request timeout, sized to the measured worst case of about five minutes
   (observed completions ran 6 seconds to 2 minutes 19 seconds). No clock
   spans chunks: total run size is bounded by admission, never by a timer.
   This is the root fix for the 31 logged timeouts, which were one shared
   10 minute clock colliding with unbounded input. A failed chunk stays
   pending and visible.
5. **The verdict is a pure function of current state**, recomputed at the end
   of every run: `REQUEST_CHANGES` while any of the service's own threads is
   open, `APPROVE` when none is open and the current head has been reviewed.
   Both inputs are read after publication, never before. Threads are refetched
   once this run's findings are posted, because a snapshot taken earlier omits
   them and would let a run approve over defects it just raised. The head is
   reloaded and compared with the commit that was analyzed, because a push
   arriving mid run would otherwise receive an approval earned by the previous
   commit. When the head moved, the run submits no verdict and leaves the work
   to the run that push triggers. The verdict itself names the analyzed commit,
   so a push landing between the check and the write cannot leave an approval
   attached to a commit nobody reviewed.

   Naming the commit is not enough on its own to stop a merge. GitHub keeps
   counting an approval after a push unless the repository dismisses stale
   approvals, so an approval written moments before a push could still satisfy
   branch protection. The check run is the enforcement point, because it is
   created per head and a new head has no passing check until a run produces
   one. A repository relying on this service should require that check, or
   enable stale approval dismissal, or both. The verdict expresses the
   reviewer's opinion; the check is what actually holds the gate.
6. **A block always says what is holding it.** When the verdict requests
   changes, the top level comment names the open threads it is waiting on. A
   run that finds nothing new still blocks while an earlier thread is
   unresolved, and without that line it reads as a silent repeat. Live
   evidence: tack 156 carries three blocking reviews, two of them empty, and
   no reader can tell from any of them that one unresolved thread is the only
   cause.
7. **A failed run never touches review state.** It turns the check red with
   the cause and writes the cause into the top level comment. What reaches the
   comment is a stable sanitized message, never the provider's raw error, which
   can carry internal endpoints, request data, or credentials. The raw cause
   goes to the logs, which are private and already retrievable by run
   identifier.
8. **Every write to the top level comment carries the state marker**, including
   failure and skip notices. The marker is how the next run finds the comment.
   A body written without it makes the next run miss the comment and create a
   second one, which breaks the one comment guarantee at exactly the moment
   something has already gone wrong.
9. **Only the app's own comment counts as state.** The lookup matches on
   authenticated author identity first and the marker second. A marker alone is
   not a credential: anyone who can comment on a pull request can write one, and
   a forged marker naming a later commit would make the service skip the very
   changes it was asked to review. That is a review bypass, so authorship is the
   gate and the marker only locates the comment behind it.

## Durable state, all on the pull request

| Object | Carries | Lifecycle |
| --- | --- | --- |
| One top level comment | HTML marker with `last_reviewed_commit`, the pending chunk list, run identifier, status, and the short human summary | Created once, found by marker, edited in place forever |
| Inline threads | One finding each, with its stable identity | Opened by a run, resolved when the code fixes it |
| Verdict review | `APPROVE` or `REQUEST_CHANGES`, nothing else | Replaced by every run's recomputation |
| Check run | Not started, running, finished, or failed, plus the run identifier | One per head |

Every write above is a read then write on shared state, so two runs on the same
pull request must never overlap. Without that, both can publish the same chunks,
and the slower run can overwrite a newer last reviewed commit, pending list,
summary, and verdict with stale values.

Serialization already exists and this design depends on it. Runs are keyed by
pull request: the dispatcher never starts a second job for a key it is already
running, and the service takes a per key lock for the whole run. All webhook
traffic reaches one container instance, so those two layers make the read then
write sequences atomic per pull request.

That guarantee has one boundary worth naming. It holds within a single
container. Running two container instances would break it, because neither the
lock nor the dispatcher spans processes. Should the deployment ever scale out,
this design needs a compare and swap on the marker instead: reject a write whose
observed state no longer matches what the run read.

## Admission: too large is not attempted

CodeRabbit's measured behavior, docs and live: when a pull request exceeds
its file budget it skips the review outright rather than produce a slow or
low quality one, announces the skip with its reason in the one walkthrough
comment, and offers an explicit on demand override. A hard cap exists above
which no override runs.

This service does the same. Before any model call, measure the delta. Over
the configured budget (`REVIEW_MAX_FILES`, `REVIEW_MAX_CHUNKS`), the run
posts "review skipped" with the measured size and the reason into the top
level comment, completes the check as `skipped`, and touches no review
state. A skipped review never blocks and never goes red. The delta, not the
whole pull request, is what is measured, so a large pull request built from
small pushes still gets reviewed increment by increment.

## The loop, every invocation

1. Read the pull request: head, marker, own threads.
2. Admission: measure the delta from `last_reviewed_commit` to head (the
   full diff when no marker exists). Over budget, write the skip notice and
   stop.
3. Compute the work: that delta, chunked, merged with any pending chunks the
   marker already lists.
4. For each chunk: one model call with its own timeout. Post qualifying
   findings inline immediately. Advance the checkpoint in the marker.
5. Reconcile: for each own open thread whose lines the delta touched, decide
   whether the new code fixes it, and resolve the ones it does.
6. Recompute the verdict from the threads now open and whether the head is
   fully reviewed. Replace the verdict review.
7. Rewrite the top level comment's summary. Complete the check.

A chunk whose model call fails stays pending and visible in the marker. It
is not retried in place; the next push reviews it along with the new delta.

## Continuation

None is built. Admission bounds every run to finish in one invocation, so
there is nothing to continue. The rare real failure, two dropped streams in
7 days of logs, costs one pending chunk that the next push covers. Whether
that residue ever earns a reinvocation mechanism is a decision to make
after the root fixes are live and measured, not before.

## What this deletes

The shared `REVIEW_TIMEOUT` clock entirely, replaced by one timeout per
model call, the truncation split and retry,
the comment capacity machinery, the mutable summary held in a review body,
the failure path's review dismissals, and the in-memory queue as the only
queue.

## Invariants, each with a test

1. The verdict always equals the pure function of own-thread state and
   reviewed head.
2. `last_reviewed_commit` advances only after its chunks' findings are on
   the page.
3. Killing the process at any point loses at most one chunk of work.
4. An admitted delta completes in one invocation, and no clock spans more
   than one model call.
5. A failed run leaves the check red and every review object untouched.
6. One top level comment exists per pull request, forever.
7. The run identifier on the check, the comment, and the log lines is the
   same string, and the logs are retrievable per [logs.md](../../logs.md).
8. A delta over budget is never attempted: the comment says skipped and why,
   the check concludes `skipped`, and no review state changes.
9. Every blocking verdict is explained: whenever the run requests changes, the
   top level comment names the open threads holding it.
10. No run approves over its own fresh findings, and none approves a commit it
    did not analyze: the threads and the head that decide the verdict are both
    read after publication.

## Acceptance

mlx-swift-lm 8 is the live acceptance test. Its 173 chunk delta must be
declined at admission with a visible skip notice and a check that neither
blocks nor goes red, on the deployed service. A second, normal sized pull
request must complete in one run with the same run identifier on the check,
the comment, and the logs. A run with one induced chunk failure must show
that chunk pending in the comment and finish on the next push.
