# Failure reassessment queue design

The queue automatically reassesses failed or incomplete reviews while the pull request remains open, unmerged, and ready for review. The queue resumes unfinished coverage and reevaluates existing findings against the current head.

## Assessment outcomes

The Worker stores signed webhook deliveries in a Durable Object before container admission. Its alarm replays unsettled deliveries with exponential backoff. The service suppresses completed dedicated deliveries by their GitHub check identifiers.

A completed check does not establish complete coverage. An incomplete review can return no error and conclude with success when the configured failure appearance permits quota exhaustion. Typed outcomes determine whether the coordinator schedules reassessment before the original delivery settles.

Fresh GitHub reads supply open, merged, and draft status before automatic admission and model work.

## Persistent records

Extend the existing replay Durable Object with a separate reassessment record keyed by installation, repository, and pull request number. Retain delivery replay for interrupted admissions. Store at most one pending reassessment per pull request.

Each record identifies the target, original delivery, generation, assessment outcome, and scheduled attempt. Its version is a storage revision for conditional updates. The stored job selects source review or verdict refresh. A source review includes reconciliation before publication.

Use `waiting`, `running`, `confirming`, and terminal phases. The confirming phase waits for GitHub to acknowledge the assessment outcome. Store terminal reasons for completion, closure, merge, draft conversion, disabled configuration, expiration, attempt exhaustion, and supersession. Terminal records retain their diagnostic identifiers for the configured retention period.

Update records with a generation comparison in one storage transaction. A result from an older head or generation cannot delete or replace a newer pending assessment. A newer trigger updates the head and metadata revision without creating another queue entry. A retry failure does not restart the expiration period or reset its attempt count.

## Go interfaces

The Go coordinator owns eligibility, failure classification, quota evaluation, backoff, attempt allocation, and completion decisions. The Cloudflare adapter stores records, applies generation comparisons, arms alarms, and forwards due records.

Expose a signed Worker endpoint at `/internal/v1/reassessments`. Support `transition` and `query` operations. Authenticate service mutations with the existing webhook signing key and a dedicated signature header. Reuse operator authentication for read-only queries. Store no provider messages or credentials in a queue record.

Expose a signed Go callback at `/internal/v1/reassessment`. Support `plan` and `execute` operations. The plan response contains the next record, phase, generation, dispatch flag, and next attempt time.

The alarm requests a plan, persists the returned transition, and executes only a successfully persisted start or resume. A start allocates a stable attempt identifier before admission. A resume reuses that identifier. Duplicate callbacks reuse the attempt's dedicated GitHub check and the existing delivery claim.

Classify the review outcome as `completed`, `failed`, `incomplete`, `declined`, or `interrupted`. Include the assessed head, metadata revision, failure classes, provider availability, coverage status, and check identifier. Record the outcome through the review completion paths. An error return or check conclusion cannot substitute for that outcome.

Persist a machine-readable outcome with the GitHub check. The replay admission path reconstructs retry intent from that outcome after a process restart. Persist the resulting reassessment transition before reporting the original delivery settled. A failed queue write keeps the original delivery unsettled and reports the queue failure. The next replay repeats the idempotent persistence operation without repeating completed model work.

The app runner observes outcomes after review completion. The internal callback uses the existing admission and dispatcher components. An automatic source retry receives a dedicated check without setting the full-review force flag. Completed chunks and the last reviewed baseline retain their existing meaning. An automatic verdict retry reevaluates current threads and metadata without reanalyzing completed file chunks.

## Eligibility and completion

Read the pull request immediately before admission and again under its existing execution lock before model requests. Start work only when the state is open, merged is false, and draft is false. A failed GitHub read defers work without assuming eligibility.

Read the latest head and metadata revision during each eligibility check. Replace stale queued targets with the current target. A successful ordinary webhook review cancels matching queued work only when its outcome confirms complete coverage and completed reconciliation and publication for that target.

Retry operational failures and incomplete coverage caused by unsuccessful model or GitHub operations. Retry failed discussion reconciliation and verdict refreshes. Retry admission refusals only when the operator enables `retry_declined`. A completed requested-changes verdict terminates automatic retry just as a completed approval does.

Queue completion requires a completed assessment, not approval or a successful check conclusion. An open finding with a returned `open` decision has been reassessed. An uncertain model decision also establishes reassessment but does not resolve the thread. Preserve existing review decisions during unsuccessful retry attempts.

The eligibility checks cancel queued work after closure, merge, or draft conversion. A later ready-for-review or reopened event starts another ordinary review.

## Runtime configuration

Configure `REASSESSMENT` in [runtime.json](../../../runtime.json). Omission disables the feature for compatibility with older releases.

| Field | Behavior |
| --- | --- |
| `enabled` | Enables automatic reassessment. |
| `queue_url` | Selects the persistent queue endpoint. |
| `initial_delay` | Delays the first retry and supplies the minimum transient backoff. |
| `maximum_delay` | Caps exponential backoff when no known quota recovery time exists. |
| `maximum_attempts` | Limits allocated attempts when positive. |
| `ttl` | Limits unfinished reassessment work when positive. |
| `terminal_retention` | Retains terminal queue records for diagnostics. |
| `retry_declined` | Enables periodic admission checks for oversized or structurally declined reviews. |

Validate positive retry delays, nonnegative attempt and age limits, and a maximum delay at least as long as the initial delay. A new failed head starts a new generation. Repeated failures on that generation retain the original limits. Configuration changes apply when the Go coordinator next evaluates a record.

Count an attempt when the coordinator allocates its stable identifier before admission. Execution logs establish whether the assessment started. Duplicate callbacks and interrupted admissions reuse the allocated identifier. Quota deferral and eligibility-read failures allocate no attempt.

## Quota and backoff

Use measured app quota snapshots before starting model work. A provider becomes available only when every configured quota for that provider permits admission. When every enabled provider has a known app denial, defer until the earliest available provider can satisfy all of its constraints. Add bounded jitter after that time to separate simultaneously due pull requests.

Use the existing quota evaluator's `AvailableAtMS` for rolling, fixed, cron, and legacy daily limits. Recheck quota when the alarm fires because concurrent reviews can consume newly available capacity. Quota deferral updates the next attempt time without sending a model request.

Use a provider's valid retry time when the adapter reports one. Provider subscription exhaustion without a recovery time uses capped exponential backoff. A provider without an app quota has unknown availability until the production request tests it. Do not invent a reset time from an external quota error.

Increase transient backoff after each failed started reassessment. Bound jitter by ten percent of the selected delay. The existing dispatcher bounds model work across pull requests. The adapter processes due records in batches no larger than the configured review worker count.

## Restart and deployment behavior

The Durable Object stores a running attempt before the callback enqueues its job. A crash before enqueue resumes that attempt. A crash during analysis resumes checkpointed coverage through the original attempt's dedicated check. A crash after check completion reconstructs the recorded outcome and repeats its queue transition.

Container draining stops new retry admissions. Active retries use the existing drain period. Interrupted retries retain their durable running records and attempt identifiers. The replacement process resumes them after checking current pull request eligibility.

An older container that does not support the reassessment callback cannot complete a queue record. The adapter retains the record and applies transport backoff. Rollback disables new automatic retries through runtime configuration without deleting unfinished webhook deliveries.

## Diagnostics and public wording

Emit structured `reassessment queued`, `reassessment deferred`, `reassessment started`, `reassessment resumed`, `reassessment completed`, `reassessment cancelled`, and `reassessment exhausted` events. Include reassessment identifier, generation, originating delivery identifier, attempt identifier, check identifier, repository, pull request number, head, metadata revision, attempt count, phase, failure classes, next attempt time, and terminal reason when applicable.

The statistics command counts attempts by stable attempt identifier. It counts completed reassessments from explicit assessment outcomes. Queue writes, model attempts, quota deferrals, and transport replay do not count as completed reassessments.

Update the existing summary comment with the scheduled retry time and queue identifier after durable persistence succeeds. Use exponential backoff when provider recovery time is unknown. Record expiration and attempt exhaustion in the terminal queue record. A successful retry restores the ordinary verdict prose. Preserve private provider diagnostics in the existing log capture.

## Verification requirements

Use the real local Cloudflare runtime and persistent storage for queue integration tests. Stop and restart that runtime with the same storage directory. Enter through signed HTTP boundaries and the production admission and review paths. Verify these cases without mocks or recorded provider responses.

| Case | Required observable result |
| --- | --- |
| A quota failure concludes with a successful check. | The queue schedules reassessment and resumes pending coverage after measured quota recovery. |
| A reply reconciliation fails. | A scheduled retry reevaluates that discussion and refreshes the standing verdict. |
| The pull request closes, merges, or becomes draft. | The queue records cancellation before sending a model request. |
| A new push replaces a queued head. | One record targets the latest head and an older outcome cannot remove it. |
| A duplicate alarm arrives during analysis. | One attempt identifier produces one running check and one assessment. |
| The process stops before enqueue or after check completion. | Restart resumes the recorded attempt or applies the recorded outcome without creating another assessment. |
| A retry completes with an unresolved finding. | The queue records completion and the requested-changes verdict remains. |
| A permanent admission refusal occurs. | The queue records a terminal decision without retrying identical refused work. |
| The attempt limit or expiration occurs. | The queue records exhaustion and the operator snapshot reports stopped automatic reassessment. |
| A newer ordinary review completes during retry delay. | The matching queued assessment terminates without another model call. |

The implementation must pass `make check` and the Cloudflare deployment's existing test command. Live acceptance requires the configured test pull request, a recoverable provider failure, an unchanged-head automatic retry, and verified closure and draft cancellation. Preserve queue, attempt, check, and thread identifiers with the acceptance results.
