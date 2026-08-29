# Behavioral failure record

Every way the deployed service ran and still did the wrong thing. Program
faults appear only where a user saw a wrong behavior. Evidence spans
2026-08-17 through 2026-08-29, drawn from live GitHub state on four
repositories and from the retained Workers Logs (1387 events, 2026-08-22 to
2026-08-29, pulled per [logs.md](logs.md)).

## 1. A block outlives its justification

The recurring issue. The service takes a blocking stance and no path ever
withdraws it. Every block below was cleared by hand.

| Case | The author's side | The service's side |
| --- | --- | --- |
| configs 292 | Fixed all 4 findings | Resolved 1 of 4 threads, kept blocking on the rest |
| tack 154 | Fixed, resolved threads, pushed `f5e5bf0` | The new head's run died mid chunk, so the stale `CHANGES_REQUESTED` stood with no re-review |
| tack 123 | Nothing left open | Blocked a green pull request with an empty body verdict |
| pr-review-agent 69 | Resolved every thread | Nothing reacts to resolution without a push, so the verdict stood |

Mechanism: GitHub review state is sticky per reviewer. The service writes
`CHANGES_REQUESTED` and treats the submission as the end of its job. A fix
and push clears it only if the next run succeeds. Thread resolution triggers
nothing. A failed run touches nothing.

## 2. A block that shows nothing to act on

configs 292's summary read `Findings eligible: 4`, `Findings published
inline: 0`, verdict `CHANGES_REQUESTED`. The reviews carrying blocking state
had 80 byte marker-only bodies, so the reader saw an HTML comment.

Mechanism: the comment cap and suppression separated what blocks from what
is shown, and the summary lived on one mutable review whose body drifted
away from its own state.

## 3. An outage reported as a code verdict

A failed run rewrote an old blocking review's body to "Review failed during
model analysis" and left its state blocking. `UpdateReview` sends only a
body and cannot change state.

## 4. The same pull request fails identically, day after day

mlx-swift-lm 8: 2026-08-27 (145 chunks), 2026-08-28 (173 chunks),
2026-08-29 (173 chunks). Each run re-reviewed the entire diff from zero,
raced the 10 minute `REVIEW_TIMEOUT`, and timed out. Chunk errors at 2ms
show the deadline had already expired before those chunks even started.
Three days, three red checks, zero findings ever delivered, all work
discarded each time. Nothing adapts, remembers, or resumes.

## 5. Death loses everything

tack 154 head `f5e5bf0`: the run died on chunk 3 of 7 with a provider
stream error, and the whole review evaporated. Before 2026-08-26 the
container's stdout reached no log sink, so a reclaimed container also left
no evidence a run had started.

## 6. Thread resolution is a model's guess

The reconciler asks a model to decide resolve, keep, or uncertain per
thread. On configs 292 it resolved 1 of 4 threads whose defects the author
had actually fixed, and the verdict then blocked on the other 3.

## 7. Noise and drift

The review body carries a 14 row details table. The summary, the verdict,
and the head identity live on three objects that disagree: one review was
submitted against head `bc860433` while its body now describes `d083f2a`.
The original operator requirements from 2026-08-14, one edited top level
thread, quiet below the threshold, terse output, were never implemented as
stated.

## The common cause

Runs are stateless and all or nothing. Every run starts from zero against
the whole diff, must finish inside one fixed deadline, keeps its progress
only in process memory, and records nothing durable except its final GitHub
writes. The verdict is written once and never recomputed. Everything above
follows from that.
