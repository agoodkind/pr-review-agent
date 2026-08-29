# Reviewer model and durable run tracing

## Why

Eight of sixteen recent reviews failed, and the failures reached users in four
repositories. The measured causes are not one bug. They are six, and only one of
them is the provider transport work that pull requests 69 through 76 address.

| # | Failure | Mechanism | Evidence |
| --- | --- | --- | --- |
| A | A stale block survives a fix | The merge gate is GitHub review state, which is sticky per reviewer and never expires | configs 292, tack 123, tack 154, each cleared by hand |
| B | The blocking review shows nothing | One mutable review holds the prose; later reviews hold only a state | Three to four reviews per pull request at `bodylen=80` |
| C | An outage is reported as a code verdict | `UpdateReview` sends only `{body}`, so rewriting an old review leaves its blocking state intact | configs 292 body reading "Review failed during model analysis" |
| D | No re-review after a fix | Only four push-shaped webhook actions start a run | tack 154 pushed a fix and got no new review |
| E | Provider transport failures | Stream refusals, spent quota, dropped connections | The failure inventory; pull requests 69 through 76 |
| F | Failures are unreadable | The check run carries no run identity and its details link resolves to the repository root | iphone-cell-tunnel 120, failed seven seconds in, no log |

configs 292 shows A, B, and the decision defect together. Its summary reads
`Findings eligible: 4`, `Findings published inline: 0`, `Bot threads resolved: 1`
of four. The author had fixed the findings. Reconciliation resolved one, left
three open, and the verdict blocked on those. The review carrying that verdict
had a marker-only body, so the reader saw an HTML comment.

## Prior art

CodeRabbit's live behavior, measured across 25 pull requests in
[langflow](https://github.com/langflow-ai/langflow):

- 24 of 24 reviews are `COMMENTED`. Zero approvals, zero changes-requested.
- Exactly one top-level issue comment per pull request, created once and updated
  in place. On one sampled pull request it was created at 21:49 and updated at
  23:30, same comment.
- That comment opens with an HTML marker, which is how the next run finds it.
- It carries the run configuration and a run identifier, so a reader diagnoses a
  run without leaving the pull request.

The blocking verdict is opt-in through `request_changes_workflow`, which
defaults to `false`. When enabled it approves only when three conditions hold:
its comments are resolved, **the latest commit has been reviewed**, and no
pre-merge check is failing.

The middle condition is the one this service lacks, and it is failure D.

## The reviewer model

Each run, against the current head, in order:

1. Reconcile. Resolve every thread of mine the current code fixes.
2. Analyze the current head.
3. Post inline comments for problems with no open thread of mine, as a
   `COMMENTED` review.
4. Update the one top-level comment with the summary and the run identifier.
5. Decide. `APPROVE` when no thread of mine is open and this run reviewed the
   current head. Otherwise `REQUEST_CHANGES`.
6. Complete the check run with whether the run finished. Never with the verdict.

### Objects and owners

| Object | Holds | Lifecycle |
| --- | --- | --- |
| One top-level issue comment | Summary, run identifier, model, coverage, and any failure notice | Created once, found by an HTML marker, updated in place |
| Review, `COMMENTED` | Inline findings | One per run that has something new to say |
| Verdict review | `APPROVE` or `REQUEST_CHANGES` | Replaced each run; the only object branch protection reads |
| Check run | Whether the run finished, and the run identifier | One per head |

A failed run writes its cause into the top-level comment and turns the check
red. It changes no review state, so an outage can never block a person.

### What this removes

The publication capacity cap and everything built to serve it: the tail slot,
the overflow pool, the pending and rejected key sets, and their tests. A
reviewer does not ration comments. That machinery is the whole of pull request
76 and it exists only because the cap exists.

Also removed: the mutable summary held in a review body, the "published this
run" term in the standing decision, and the failure path's review dismissal.

## Durable run tracing

Today `runlog.Recorder` is an `slog.Handler` that keeps at most
`MaximumRecords = 500` entries in memory and renders them into the check run
body. It has three gaps.

1. **No correlation.** Nothing ties a pull request to the service's own logs.
   The check run carries no run identifier and its details link resolves to the
   repository root.
2. **No durability.** The buffer lives in the container. A run reclaimed before
   it completes its check publishes nothing at all.
3. **Not comprehensive.** The 500-entry cap drops the start of a long run, and
   the start is where admission and configuration are recorded.

### Requirements

- Mint one run identifier when the job is admitted. Carry it as a permanent
  `gklog` attribute so every log line the run writes already has it.
- Write that identifier into three places a person can reach without
  credentials: the check run output, the top-level comment, and every shipped
  log line.
- Give the check run a details link that resolves to that run's logs rather than
  to the repository root.
- Ship log lines as the run produces them, so a run that dies mid-flight still
  left a trail. The Worker already receives and prints the service's logs, which
  was verified live on 2026-08-26.
- Keep the in-check excerpt for immediate reading, and treat the shipped stream
  as the complete record.

### Invariants worth enforcing

The 2026-08-12 design session already stated most of these. None were encoded,
and all have since broken.

1. A review attaches to the head it analyzed.
2. A new push produces a fresh decision for the new head.
3. Retrying one head does not duplicate a decision.
4. A verdict is `APPROVE` only when no thread of mine is open and this run
   reviewed the current head.
5. A failed run changes no review state.
6. Every run identifier appearing in a pull request resolves to that run's logs.

## Testing

Each invariant gets a test that fails against the current code. The live proof
is a real pull request in an installed repository exercising approve, request
changes, and a forced provider failure, with the run identifier from each
artifact resolved back to its logs.
