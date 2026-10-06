# Prove the durable review works

Run these live checks after a release changes review behavior. Confirm the
deployed image digest before testing.

## Verify one provider

1. Set `PR_AGENT_LIVE_PROVIDER_ID` to a configured provider ID and `PR_AGENT_LIVE_API_KEY_FILE` to its private credential file. Set `PR_AGENT_LIVE_SIGNING_KEY_FILE` when the provider has an app quota.
2. Run the live test from the repository root:

   ```bash
   go test -tags=integration ./cmd/pragent-ops -run TestLiveProviderProbe -count=1
   ```

3. Confirm that the test passes. It requires one successful model request with reported token usage. It uses the configured API, model, reasoning effort, and output limit. It publishes no GitHub review.
4. Verify quota fallback with the configured provider credential. This test uses three real model responses and waits for a one-minute window to expire:

   ```bash
   PR_AGENT_LIVE_QUOTA_TEST=1 go test -tags=integration ./internal/quota \
       -run TestLiveReviewChecksMultipleWindowsAndChargesEachResponseOnce -count=1
   ```

   The test checks minute-limit denial, fallback, recovery after expiry, and one usage record per response in real local Cloudflare storage.

5. Verify unexpected-error fallback with an enabled provider that has no app quota:

   ```bash
   go test -tags=integration ./cmd/pragent-ops \
       -run 'TestLiveUnexpectedFailureSelectsNextProvider|TestLiveProviderTimeoutSelectsNextProvider|TestCancelledRequestStartsNoProvider' \
       -count=1
   ```

   The tests verify fallback after authentication errors across all review stages and after a provider timeout. Parent cancellation must stop provider selection. The fallback must receive a response from the configured endpoint; a quota error satisfies that request assertion. These tests do not establish review completion when the provider has exhausted its allowance.

## Verify prose findings and corrections

1. Select an enabled provider and its existing private credential file.
2. Run the source-derived cases through the production reviewer:

   ```bash
   go run ./cmd/pragent-ops prose-eval \
       --dataset testdata/prose-eval.json \
       --provider gemini \
       --credential-file /path/to/provider-key
   ```

   Capped providers require `--signing-key-file`. Use repeated `--case` arguments to select exact case IDs. The command applies the configured model, reasoning, output limit, and quota.
3. Read the private result artifact. Require completed responses with reported usage. Check missed findings, unexpected findings, and unsafe corrections before accepting the result.
4. Compare every proposed correction with the current source. Confirm that the replacement preserves necessary conditions, constraints, reasons, and identifiers. A passing detection count alone does not prove a safe correction.
5. Confirm that compliant comparison passages produce no findings. Preserve the source provenance and policy hashes when comparing releases.

The command publishes no GitHub review. A request failure or missing usage is an incomplete evaluation. The command returns a failure when a completed case misses an expected violation, reports an unexpected finding, or proposes a prohibited correction action.

## Verify metadata findings and resumed reviews

1. Save the existing test pull request's title, description, head, labels, and thread state before changing the fixture.
2. Introduce a known prose violation separately in the title, description, and a commit message. Require a current finding at the actual field lines in the existing summary. Require `request_changes` and the actual commit SHA for commit-message findings. Metadata findings must not use fabricated file anchors. Require an exact replacement or deletion when a safe correction is available. Feedback without an exact correction must request changes without printing an empty replacement.
3. Correct the title or description without pushing a commit. Require an `edited` event to produce a new completed check on the unchanged head. Confirm that the corrected field no longer blocks approval.
4. Resolve file threads while retaining a metadata defect. Require `request_changes` for the metadata defect. Confirm that required commit attribution remains unchanged.
5. Interrupt a review after a metadata chunk is checkpointed. Push another commit while retaining the earlier bad commit message. Require a current finding for that commit after the resumed review completes.
6. Restore the fixture's original metadata, labels, and thread state after the check, including after a failed assertion. Keep the check identifiers and source revisions as evidence.

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
- the detail table carries duration, every reported token category, estimated
  cost, and the current thread identifiers and counts
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

Replying to or changing a thread triggers a verdict refresh without a push.
A new required check enters progress before the asynchronous refresh starts,
then completes with the verdict recomputed from current thread state.

## 4. Requested changes require an actionable inline finding

On a pull request where the run posts a new finding, expect the same run to
request changes with `Resolve the open inline findings.` The visible verdict
does not repeat their locations. GitHub connects the review to the inline
comments, and the collapsed details carry their thread identifiers and counts.
The verdict reads its threads after publication precisely so it cannot approve
over a defect it raised minutes earlier.

Make GitHub refuse the inline comment, or reject an omission without finding
a defect. Expect `COMMENT` and an `action_required` check when no actionable
inline finding remains. Repeat the delivery and refresh the threads: neither
operation may turn that result into approval or requested changes. The summary
retains any finding GitHub could not place inline.

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

## 6. Verify checkpoint recovery after a failed chunk

1. Select a test pull request with multiple chunks. Cause a provider request to fail after another chunk completes.
2. Confirm that the summary marker records unread chunks in `pending` and reviewed chunks in `completed`. Check the conclusion against `SERVICE_FAILURE_APPEARANCE`.
3. Confirm that the service submits no new approval or rejection after the failure. An unexpected failure must dismiss only the latest bot rejection. Quota and rate-limit failures must retain that decision.
4. Restore the provider and trigger reassessment. Require reuse of completed chunks, review of pending chunks, and no duplicate findings.
5. Confirm that public diagnostics contain the provider and recognized error details without raw provider messages or credentials.


## 7. Verify dismissal after an unexpected failure

The live test dismisses the selected bot rejection and publishes a failed check. Use an open, non-draft test pull request with another reviewer's submitted decision.

1. Set the explicit test inputs.

   | Variable | Required input |
   | --- | --- |
   | `PR_AGENT_LIVE_REPOSITORY` | Repository in `owner/repo` format |
   | `PR_AGENT_LIVE_PULL_REQUEST` | Test pull request number |
   | `PR_AGENT_LIVE_FAILURE_REVIEW_ID` | Latest bot rejection's review ID |
   | `PR_AGENT_LIVE_INSTALLATION_ID` | GitHub App installation ID for the repository |
   | `PR_AGENT_LIVE_GITHUB_APP_KEY_FILE` | Matching GitHub App private-key file |
   | `PR_AGENT_LIVE_FAILURE_PROVIDER_ID` | Runtime provider ID without Cloudflare Access requirements |

2. Run the production service against GitHub and the selected provider with an invalid test credential.

   ```bash
   PR_AGENT_LIVE_FAILURE_VERDICT_TEST=1 go test -tags=integration ./cmd/pragent-ops \
       -run TestLiveUnexpectedFailureDismissesLatestOwnRejection -count=1
   ```

   Require a real provider authentication or request error, dismissal of the expected review ID, unchanged decisions from other reviewers, a safe failure summary, and the failed assessment receipt.

3. Set `PR_AGENT_LIVE_FAILURE_APPROVAL_PULL_REQUEST` and `PR_AGENT_LIVE_FAILURE_APPROVAL_REVIEW_ID` to a separate fixture with an earlier bot rejection and a later bot approval. Verify that failure preserves both decisions.

   ```bash
   PR_AGENT_LIVE_FAILURE_VERDICT_TEST=1 go test -tags=integration ./cmd/pragent-ops \
       -run TestLiveUnexpectedFailurePreservesLaterOwnApproval -count=1
   ```

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

Expect the marker to keep every checkpointed chunk in `completed`. The replayed
delivery reviews only pending chunks and posts no duplicate findings.
The pre-redesign service lost the whole run: 31 logged timeouts came from one
shared ten minute clock over a 173 chunk diff, and death kept no progress at
all.
