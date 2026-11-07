# Behavioral failure record

This record preserves observed service failures and their causes. The initial
investigation used live GitHub state on four repositories and 1,387 retained
Workers log events from August 22 through August 29, 2026.

## 1. A block outlives its justification

The recurring issue. The service takes a blocking stance and no path ever
withdraws it. Every block below was cleared by hand.

| Case | The author's side | The service's side |
| --- | --- | --- |
| configs 292 | Fixed all 4 findings | Resolved 1 of 4 threads, kept blocking on the rest |
| tack 154 | Fixed, resolved threads, pushed `f5e5bf0` | The new head's run died mid chunk, so the stale `CHANGES_REQUESTED` stood with no re-review |
| tack 123 | Nothing left open | Blocked a green pull request with an empty body verdict |
| tack 156 | Fixed 2 findings in `b7dd0c4`, answered the third as a deliberate no change | Re-reviewed the new head, found nothing, and blocked again with a marker only body, holding an armed auto merge with 21 green checks |
| configs 298 | Fixed the earlier round's findings | Final round found nothing and still blocked, review id 5059218794 |
| pr-review-agent 69 | Resolved every thread | Nothing reacts to resolution without a push, so the verdict stood |

Mechanism: GitHub review state is sticky per reviewer. The service writes
`CHANGES_REQUESTED` and treats the submission as the end of its job. A fix
and push clears it only if the next run succeeds. Thread resolution triggers
nothing. A failed run touches nothing.

The obvious fix is the wrong one. Operators reading tack 156 proposed that a
round finding nothing should stop requesting changes. That would break as
soon as a real finding from an earlier round is still open, because it ties
the verdict to one run's output rather than to what stands unresolved. The
verdict must follow the service's own open threads, which is what makes a
fixed pull request unblock itself and a disputed one keep blocking with a
visible reason.

No recovery path exists short of dismissal. The check run carries a bare
repository URL with no run identifier, so `gh run rerun` cannot target it: an
external service posts it through the checks API rather than a workflow. A
`/review` comment produced no fresh verdict in four minutes on tack 154. Both
tack blocks cleared only through an operator authorized dismissal of the
review, and on a repository requiring thread resolution the merge fired only
after every thread was also resolved by hand.

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
2026-08-29 (173 chunks). The measured chain, from the retained logs:

1. Every chunk sends 60 to 80KB of diff to the model. `prompt_bytes` reads
   60014 to 79879 on each chunk line.
2. One model call takes 6 seconds to 2 minutes 19 seconds, measured on the
   22 chunks that completed with recorded durations. That spread is normal
   reasoning model inference on prompts that size, not a provider fault.
3. All chunks share one 10 minute `REVIEW_TIMEOUT` across 4 workers. Ten
   minutes of 4 workers at those durations fits roughly 60 chunks. The run
   needed 173. The clock ran out mid run by arithmetic.
4. Once the shared clock expired, every remaining chunk failed in 1.4ms to
   200ms with `context deadline exceeded`. A request cannot time out in
   milliseconds, so the parent context was dead before those requests began.

Three days, three red checks, zero findings delivered, all work discarded
each time. The 31 logged timeouts were never the provider failing. They
were a fixed total budget colliding with unbounded input, repeated daily.

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

## 8. A finding can be wrong about the code it read

Two findings on 2026-08-30 asserted facts the source contradicts. On tack 160
the review said `pgproto3.BackendKeyData.SecretKey` is a `uint32` and the
package therefore could not compile; the pinned pgx v5.10.0 declares it
`[]byte`, and the package builds and tests green. On configs 302 it said the
audit consumer's ledger connection was unbounded and named `DATABASE_URL` as
the override; that binary reads `AUDIT_CONSUMER_YUGABYTE_DSN`, which compose
already sets to a bounded value.

Both were answered with the source and resolved, and both cost an author a
round trip to disprove. This is not the stale block failure and the durable
review does not address it: those runs completed and published exactly what
the model returned. The reviewer states a fact about code it could have read
and does not check it, so a confident wrong finding costs the same as a right
one until a human reads the source.

## 9. A killed run leaves a check nobody can clear

A deploy restarts the container and cancels every review in flight. The
cancelled run leaves its check red, and shutdown cancels the GitHub writes
that would explain it, so the comment never appears. Nothing retries and no
event other than a pull request webhook starts a run, so the red check stands.

Observed on tack 160 at 2026-08-30T15:53:27Z, where the killed run had begun
13 seconds after the push it was reviewing. The escapes are a new head or a
merge, and that pull request merged. A branch whose last commit is the one the
author wants has neither escape, and the check exposes no run identifier an
operator could use to re-trigger it.

## 10. Readable configuration becomes an unresolved exclusion

On November 7, 2026, [Configs pull request 982](https://github.com/agoodkind/configs/pull/982)
renamed a private network and its upstream bridge. Check `128476660938`
reviewed commit `6a8377e31ed7544def575cceda7ada7bcbb5c7f5`. The check log
reports these measurements for its completed main review call:

| Measurement | Value |
| --- | --- |
| Changed paths | 9 |
| Call duration | 4.70 seconds |
| Prompt size | 92,848 bytes |
| Returned findings | 0 |
| Failed chunks | 0 |

The final check required action because the service treated a readable
three-line configuration file as unfinished scope.

The classifier assigned `source`, `unnecessary`, and `exclude` to the forwarding
configuration. Its explanation said that the preview contained the complete
file. The configured exclusion categories did not permit source exclusions.
The planner retained the changed bytes for ordinary review but retained the
contradictory exclusion in the assessment. Later scope evaluation trusted the
action without checking the category permission. The file remained a reported
omission after its changed content entered the main review chunk.

The correction applies one exclusion decision to planning, evidence selection,
admission counts, and scope completion. A forbidden exclusion becomes required
content review. The regression uses an actual Git configuration change and the
production planner, chunker, provider serializers, and summary renderer. A
separate check preserves explicit category configuration and valid artifact
exclusions. A preview does not establish that ordinary content review occurred.

This incident also exposed a separate context limitation. The collector reads
changed files, and the model cannot request unchanged supporting source.
The changed host resource referenced an activation script that the review did
not receive. The exclusion-policy correction does not change repository
retrieval. Defect detection that requires that script remains unverified.

The check log does not preserve the full original provider request or explain why the
model omitted a particular defect. Scope completion and defect detection require
separate verification.

## The common cause

Failures 1 through 7 share the cause below. Later entries establish separate
causes.

Runs are stateless and all or nothing. Every run starts from zero against
the whole diff, must finish inside one fixed deadline, keeps its progress
only in process memory, and records nothing durable except its final GitHub
writes. The verdict is written once and never recomputed. Everything above
follows from that.
