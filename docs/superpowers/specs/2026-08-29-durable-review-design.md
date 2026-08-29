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
4. **No retry loops and no global deadline.** Each model call carries its own
   request timeout. A failed chunk stays on the work list. Continuation is
   resume, not retry.
5. **The verdict is a pure function of current state**, recomputed at the end
   of every run: `REQUEST_CHANGES` while any of the service's own threads is
   open, `APPROVE` when none is open and the current head has been reviewed.
6. **A failed run never touches review state.** It turns the check red with
   the cause and writes the cause into the top level comment.

## Durable state, all on the pull request

| Object | Carries | Lifecycle |
| --- | --- | --- |
| One top level comment | HTML marker with `last_reviewed_commit`, the pending chunk list, run identifier, status, and the short human summary | Created once, found by marker, edited in place forever |
| Inline threads | One finding each, with its stable identity | Opened by a run, resolved when the code fixes it |
| Verdict review | `APPROVE` or `REQUEST_CHANGES`, nothing else | Replaced by every run's recomputation |
| Check run | Not started, running, finished, or failed, plus the run identifier | One per head |

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

An invocation that exhausts its budget mid list simply stops after a
checkpoint. Nothing is lost and nothing is retried in place.

## Continuation

The Worker's Durable Object owns liveness. While the marker lists pending
chunks, it holds an alarm and re-invokes the container until the list is
empty. A container death, deploy, or reclaim delays the review and never
loses it. Any webhook for the pull request also triggers continuation.

## What this deletes

The global review deadline as a run killer, the truncation split and retry,
the comment capacity machinery, the mutable summary held in a review body,
the failure path's review dismissals, and the in-memory queue as the only
queue.

## Invariants, each with a test

1. The verdict always equals the pure function of own-thread state and
   reviewed head.
2. `last_reviewed_commit` advances only after its chunks' findings are on
   the page.
3. Killing the process at any point loses at most one chunk of work.
4. A diff of any size completes across bounded invocations, and no single
   invocation runs unbounded.
5. A failed run leaves the check red and every review object untouched.
6. One top level comment exists per pull request, forever.
7. The run identifier on the check, the comment, and the log lines is the
   same string, and the logs are retrievable per [logs.md](../../logs.md).
8. A delta over budget is never attempted: the comment says skipped and why,
   the check concludes `skipped`, and no review state changes.

## Acceptance

mlx-swift-lm 8 is the live acceptance test. Its 173 chunk delta must be
declined at admission with a visible skip notice and a check that neither
blocks nor goes red, on the deployed service. A second, normal sized pull
request must complete its review across invocations after a forced container
death, with the same run identifier on every artifact.
