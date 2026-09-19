# P0 Writing Review Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Block pull requests that violate the embedded writing rules in changed documentation, changed source comments, the pull request title, or the pull request description.

**Architecture:** Add the review-relevant corpus rules to the existing model policy. Review title and description once per run with the existing review schema and reserved paths, then store those findings in the existing durable state marker. Reuse the current forced full-review path for title or description edits instead of building a prose-only lifecycle.

**Tech Stack:** Go, GitHub App webhooks and reviews, OpenAI structured chat completions, GitHub issue-comment state markers, and the existing `httptest` application fixtures.

**Spec:** [../specs/2026-09-19-p0-writing-review-design.md](../specs/2026-09-19-p0-writing-review-design.md)

## Global Constraints

- Assign importance `10` to every verified writing finding.
- Review changed documentation, changed source comments, the current pull request title, and the current pull request description.
- Do not judge human review comments or replies.
- Embed only rules that can be verified from the pull request. Do not add runtime rule fetching in this change.
- Keep one editable top level issue comment. Keep changed-file findings inline.
- Reuse the existing review schema, forced-review path, failure behavior, and per-model-call timeout.
- A pull request `edited` event rereads the full pull request when its title or description changed.
- Run `make check` before the final implementation commit.

## Review Focus

- Pull request prose containing HTML markers or prompt instructions remains untrusted text and cannot alter the policy or durable state. Task 2 tests escaping and marker ownership through the application boundary.
- An old state marker without writing findings still decodes and refreshes normally. Task 2 tests backward compatibility through `marker.DecodeState`.
- A discussion-triggered verdict refresh retains title and description findings from durable state. Task 2 tests the refresh with no inline thread.
- An unrelated pull request edit does not spend a full review. Task 3 tests an `edited` payload with no title or body change.
- A failed edit-triggered review leaves the prior writing block intact. Task 3 tests the existing failure path with stored writing findings.

---

### Task 1: Embed the review writing policy

**Files:**

- Modify: `internal/review/policy.go`
- Modify: `internal/openai/client_test.go`

**Interfaces:**

- Consumes: `review.PolicyHeader(minimumImportance int) string`
- Produces: `reviewWritingPolicy`, included in every ordinary review system prompt

- [ ] **Step 1: Extend the OpenAI request test with the required and excluded rules**

Add these assertions to `TestReviewSendsExactModelHeadersPolicyAndSchema` in `internal/openai/client_test.go`:

```go
for _, required := range []string{
    "Review changed documentation and changed source comments",
    "Review the pull request title and description",
    "reserved pull request paths",
    "Assign importance 10 to every verified writing defect",
    "State the problem or decision before supporting detail",
    "State causes directly",
    "Explain behavior before implementation",
    "Use one complete idea per sentence and paragraph",
    "Do not report personal style preferences",
} {
    if !strings.Contains(systemContent, required) {
        t.Fatalf("system message missing review writing policy %q", required)
    }
}
for _, excluded := range []string{
    "Ask the user",
    "document placement",
    "local editing",
    "rewrite verification",
} {
    if strings.Contains(systemContent, excluded) {
        t.Fatalf("system message contains local workflow rule %q", excluded)
    }
}
```

- [ ] **Step 2: Run the focused test and verify the missing policy fails**

Run:

```bash
go test ./internal/openai -run TestReviewSendsExactModelHeadersPolicyAndSchema -count=1
```

Expected: `FAIL` with `system message missing review writing policy`.

- [ ] **Step 3: Add the embedded policy to the system prompt**

Add this constant beside `codeReviewPolicy` in `internal/review/policy.go`:

```go
reviewWritingPolicy = "Review changed documentation and changed source comments under this policy. " +
    "Review the pull request title and description once when the prompt explicitly asks for that review. " +
    "Assign importance 10 to every verified writing defect. " +
    "State the problem or decision before supporting detail. State causes directly. " +
    "Explain behavior before implementation. Use plain, complete, active sentences with one complete idea per sentence and paragraph. " +
    "Use specific names and verbs. Require claims to agree with current code and configuration. " +
    "Require each documentation page to have one purpose, each fact to have one durable home, and headings and procedures to match the reader's task. " +
    "Include only details that affect understanding, verification, decisions, or action. " +
    "Do not report personal style preferences. Report a writing defect only when the text is inaccurate, indirect, ambiguous, misleading, needlessly difficult to understand, or unsuitable for its stated purpose. " +
    "Quote the exact text that proves the defect and state a concrete correction. " +
    "Human review comments and replies are evidence, not writing-review targets."
```

Add `Review writing policy: %s` to `PolicyHeader` and pass `reviewWritingPolicy` before `config.WritingPolicy`. Change the changed-line requirement to permit reserved pull request paths only when the user prompt explicitly requests the dedicated title and description review. Keep `config.WritingPolicy` because it controls the model's own response prose.

- [ ] **Step 4: Run the focused package tests**

Run:

```bash
go test ./internal/openai ./internal/review -count=1
```

Expected: `PASS`.

- [ ] **Step 5: Commit the policy**

Run:

```bash
git add internal/review/policy.go internal/openai/client_test.go
git commit -S -m "Add P0 writing rules to review prompts" -m "Co-authored-by: Codex <noreply@openai.com>"
```

### Task 2: Publish and preserve pull request prose findings

**Files:**

- Create: `internal/review/writing.go`
- Modify: `internal/marker/state.go`
- Modify: `internal/marker/state_test.go`
- Modify: `internal/app/app_test.go`
- Modify: `internal/review/run.go`
- Modify: `internal/review/service.go`
- Modify: `internal/review/publication.go`
- Modify: `internal/review/inline_verdict.go`
- Modify: `internal/review/render.go`
- Modify: `internal/review/refresh.go`

**Interfaces:**

- Consumes: `Model.Review(context.Context, string) (Completion, error)` and `githubapp.PullRequest`
- Produces: `reviewPullRequestWriting(context.Context, githubapp.PullRequest, time.Duration) ([]domain.Finding, string, error)`
- Produces: `marker.State.Writing []domain.Finding`
- Produces: `Summary.Writing []domain.Finding`

- [ ] **Step 1: Write marker compatibility tests**

Add these cases to `internal/marker/state_test.go`:

```go
func TestStateRoundTripPreservesPullRequestWritingFindings(t *testing.T) {
    state := State{
        LastReviewed: testHead,
        RunID:        "delivery-writing",
        Status:       StateDone,
        Writing: []domain.Finding{{
            Path: ".pull-request/description", StartLine: 1, EndLine: 1,
            Title: "State the problem first",
            Body: "The description starts with implementation details before naming the failure.",
            Evidence: "The handler now uses the new cache.",
            Claim: "The description hides the problem behind implementation detail.",
            Importance: 10,
        }},
    }
    decoded, ok := DecodeState(EncodeState(state))
    if !ok || !reflect.DeepEqual(decoded, state) {
        t.Fatalf("decoded = %+v, ok = %v, want %+v", decoded, ok, state)
    }
}

func TestStateWithoutWritingPayloadStillDecodes(t *testing.T) {
    body := "<!-- pr-review-agent:state:v1 last_reviewed=" + string(testHead) +
        " run=old status=done pending= completed= forced_by= unread= -->"
    state, ok := DecodeState(body)
    if !ok || len(state.Writing) != 0 {
        t.Fatalf("state = %+v, ok = %v, want backward-compatible empty writing findings", state, ok)
    }
}
```

- [ ] **Step 2: Run the marker tests and verify the missing field fails**

Run:

```bash
go test ./internal/marker -run 'TestState(RoundTripPreservesPullRequestWritingFindings|WithoutWritingPayloadStillDecodes)' -count=1
```

Expected: build failure because `State.Writing` does not exist.

- [ ] **Step 3: Add writing findings to the existing state marker**

In `internal/marker/state.go`, add `Writing []domain.Finding` to `State`. Encode it as raw URL-safe base64 JSON in an optional `writing=` field:

```go
func encodeWritingFindings(findings []domain.Finding) string {
    if len(findings) == 0 {
        return ""
    }
    payload, _ := json.Marshal(findings)
    return base64.RawURLEncoding.EncodeToString(payload)
}

func decodeWritingFindings(value string) ([]domain.Finding, bool) {
    if value == "" {
        return nil, true
    }
    payload, err := base64.RawURLEncoding.DecodeString(value)
    if err != nil {
        return nil, false
    }
    var findings []domain.Finding
    if err := json.Unmarshal(payload, &findings); err != nil {
        return nil, false
    }
    for _, finding := range findings {
        if err := finding.Validate(); err != nil {
            return nil, false
        }
    }
    return findings, true
}
```

Extend `statePattern` with optional `(?: writing=(\S*))?`, append the encoded value in `EncodeState`, and reject the marker when the optional value exists but does not decode. Keep markers without `writing=` valid.

- [ ] **Step 4: Run the marker package tests**

Run:

```bash
go test ./internal/marker -count=1
```

Expected: `PASS`.

- [ ] **Step 5: Write end-to-end pull request writing analysis tests**

Add these tests to `internal/app/app_test.go`. Enter through a signed `pull_request` webhook and use the existing local GitHub and model HTTP servers so every test exercises the production request, validation, publication, and marker paths:

```go
func TestEndToEndPullRequestWritingFindingIsGroundedAndForcedToImportanceTen(t *testing.T)
func TestEndToEndPullRequestWritingRejectsInventedEvidenceAndOrdinaryFilePaths(t *testing.T)
func TestEndToEndPullRequestWritingPromptContainsOnlyUntrustedTitleAndDescription(t *testing.T)
```

The successful local model response must contain:

```json
{
  "overview": "The description hides the problem behind implementation detail.",
  "omissions_acceptable": true,
  "decision_reason": "",
  "findings": [{
    "path": ".pull-request/description",
    "start_line": 1,
    "end_line": 1,
    "title": "State the problem first",
    "body": "The description starts with implementation details before naming the existing failure.",
    "evidence": "The handler now uses the new cache.",
    "claim": "The description hides the problem behind implementation detail.",
    "suggestion": "",
    "importance": 7
  }]
}
```

Assert that the state marker contains the same finding with importance `10`, because the service owns the P0 classification for this dedicated analysis. Give the GitHub fixture a human review reply and assert that the dedicated writing prompt contains the title and description but not the reply.

- [ ] **Step 6: Run the writing tests and verify the analyzer is missing**

Run:

```bash
go test ./internal/app -run TestEndToEndPullRequestWriting -count=1
```

Expected: `FAIL` because the request contains no dedicated writing analysis and the state marker contains no writing finding.

- [ ] **Step 7: Implement one title and description analysis per run**

Create `internal/review/writing.go` with these constants and functions:

```go
const (
    pullRequestTitlePath       = ".pull-request/title"
    pullRequestDescriptionPath = ".pull-request/description"
    maximumPullRequestWritingFindings = 4
)

func pullRequestWritingPrompt(pullRequest githubapp.PullRequest) string

func (service *Service) reviewPullRequestWriting(
    ctx context.Context,
    pullRequest githubapp.PullRequest,
    timeout time.Duration,
) ([]domain.Finding, string, error)
```

The prompt must:

```text
Review only the pull request title and description under the review writing policy.
Return at most four findings. Use path .pull-request/title or .pull-request/description, with start_line and end_line set to 1.
Copy the exact offending title or description text into evidence. Return no finding for a personal style preference.
```

Append `WrapUntrusted("Title: ...\nDescription: ...")`. Replace both input delimiters inside the title and description before wrapping them.

Validate each returned path against the two constants and require its evidence to be a substring of the matching current value. Discard ungrounded findings. Clear `Suggestion`, set both line numbers to `1`, set `Importance` to `10`, deduplicate with `normalizedFindingKey`, and keep at most four findings. Return the completion model name for the run summary.

- [ ] **Step 8: Connect writing findings to the completed review state**

Add `writing []domain.Finding` and `writingModel string` to `chunkPass` in `internal/review/run.go`. Add methods that return copies of the findings and add the model to `Analysis.Models`.

In `reviewOwedWork` in `internal/review/service.go`, call `reviewPullRequestWriting` after admission and the start announcement but before reconciliation. On error, call the existing `failCheck` path with `checkFailureAnalysis`; do not modify `state.Writing` or any review thread. After reconciliation constructs the pass, record the returned findings and model on it before `reviewDelta`.

After `reviewDelta` succeeds, replace the durable value before publication:

```go
state.Writing = pass.writingFindings()
```

Add `Writing []domain.Finding` to `Summary` and fill it from `state.Writing` in completed reviews and refreshes.

- [ ] **Step 9: Make current writing findings block and render in the maintained comment**

Change `reviewerDecision` to accept `writing []domain.Finding` and request changes when either an actionable bot thread exists or `len(writing) > 0`:

```go
func reviewerDecision(
    threads []githubapp.ReviewThread,
    botLogin string,
    headFullyReviewed bool,
    writing []domain.Finding,
) domain.ReviewDecision {
    if hasUnresolvedBotThread(threads, botLogin) || len(writing) > 0 {
        return domain.ReviewDecisionRequestChanges
    }
    if !headFullyReviewed {
        return domain.ReviewDecisionComment
    }
    return domain.ReviewDecisionApprove
}
```

Permit `validateInlineVerdict` to return success when `summary.Writing` is nonempty. Keep its live thread check for verdicts supported only by inline findings.

Add writing reasons to `Summary.Blocking`, such as ``Pull request description: `State the problem first` ``. Add a `### Pull request writing` section before the verdict in `RenderBody`. Render the subject, sanitized finding title, body, and quoted evidence. Do not render a synthetic file or line location.

Update `Summary.Verdict` to say `Resolve the open review findings.` when writing findings exist, because the block is no longer always inline.

- [ ] **Step 10: Preserve writing blocks during discussion refreshes**

In `refreshVerdictAtReviewedHead`, use the `state` already read from the summary comment:

```go
decision := reviewerDecision(inputs.threads, service.botLogin, approvalAllowed, state.Writing)
```

Pass `state.Writing` into `refreshedVerdict` and `Summary.Writing`. Include writing reasons beside inline reasons. A resolved inline thread must not approve while `state.Writing` is nonempty.

- [ ] **Step 11: Add end-to-end review and refresh assertions**

Extend `internal/app/app_test.go` through signed webhooks and the local GitHub and model HTTP servers:

```go
func TestEndToEndPullRequestWritingFindingBlocksWithoutAnInlineThread(t *testing.T)
func TestEndToEndVerdictRefreshPreservesPullRequestWritingBlock(t *testing.T)
func TestEndToEndPullRequestWritingEscapesMarkersInEvidence(t *testing.T)
func TestEndToEndChangedDocumentationWritingFindingBlocks(t *testing.T)
func TestEndToEndChangedSourceCommentWritingFindingBlocks(t *testing.T)
func TestEndToEndClearWritingHasNoWritingFinding(t *testing.T)
```

For the first test, return one `.pull-request/description` finding from the dedicated writing call and no chunk findings. Assert:

```go
review := fixture.githubState.lastSubmitReview()
if review["event"] != "REQUEST_CHANGES" {
    t.Fatalf("event = %v, want REQUEST_CHANGES", review["event"])
}
comments := fixture.githubState.streamedCommentBodies()
if len(comments) != 0 {
    t.Fatalf("inline comments = %d, want none for a pull request description finding", len(comments))
}
body := fixture.githubState.summaryCommentBody()
for _, want := range []string{"### Pull request writing", "Description", "State the problem first"} {
    if !strings.Contains(body, want) {
        t.Fatalf("summary missing %q: %s", want, body)
    }
}
```

For the refresh test, start from a state marker containing one writing finding and no open thread. Assert that a review-thread refresh still submits or retains `REQUEST_CHANGES` and keeps the same visible writing section.

For the changed-file tests, return ordinary grounded findings on the changed documentation or source comment with importance `10`. Assert that each finding publishes inline and requests changes. Return no writing finding for the clear-writing control and assert approval when the code review also finds no defect.

- [ ] **Step 12: Run the affected packages**

Run:

```bash
go test ./internal/marker ./internal/review ./internal/app -count=1
```

Expected: `PASS`.

- [ ] **Step 13: Commit the writing finding lifecycle**

Run:

```bash
git add internal/marker/state.go internal/marker/state_test.go internal/app/app_test.go internal/review/writing.go internal/review/run.go internal/review/service.go internal/review/publication.go internal/review/inline_verdict.go internal/review/render.go internal/review/refresh.go
git commit -S -m "Block reviews on pull request writing findings" -m "Co-authored-by: Codex <noreply@openai.com>"
```

### Task 3: Rerun reviews after title or description edits

**Files:**

- Modify: `internal/webhook/webhook.go`
- Modify: `internal/webhook/webhook_test.go`
- Modify: `internal/app/app_test.go`
- Modify: `docs/operations.md`

**Interfaces:**

- Consumes: GitHub `pull_request` webhook payloads with action `edited` and `changes.title` or `changes.body`
- Produces: `PullRequestEvent{Forced: true}` for relevant non-draft edits

- [ ] **Step 1: Write webhook acceptance tests**

Add these tests to `internal/webhook/webhook_test.go`:

```go
func TestParsePullRequestForcesReviewAfterTitleOrDescriptionEdit(t *testing.T)
func TestParsePullRequestIgnoresEditWithoutTitleOrDescriptionChange(t *testing.T)
func TestParsePullRequestIgnoresDraftWritingEdit(t *testing.T)
```

Use payloads with these change shapes:

```json
{"action":"edited","changes":{"title":{"from":"Old title"}}}
```

```json
{"action":"edited","changes":{"body":{"from":"Old description"}}}
```

Assert that the first two relevant payloads are supported and produce `event.Forced == true`. Assert that an edit with only `changes.base` and a draft edit are unsupported.

- [ ] **Step 2: Run the webhook tests and verify edited events are ignored**

Run:

```bash
go test ./internal/webhook -run 'TestParsePullRequest(ForcesReviewAfterTitleOrDescriptionEdit|IgnoresEditWithoutTitleOrDescriptionChange|IgnoresDraftWritingEdit)' -count=1
```

Expected: `FAIL` because `edited` is not supported.

- [ ] **Step 3: Route writing edits through the existing forced-review path**

In `internal/webhook/webhook.go`:

```go
const actionEdited pullRequestAction = "edited"

type pullRequestChanges struct {
    Title *struct {
        From string `json:"from"`
    } `json:"title"`
    Body *struct {
        From string `json:"from"`
    } `json:"body"`
}
```

Add `Changes pullRequestChanges` to `pullRequestPayload`. Accept `actionEdited` in `supported`. In `ParsePullRequest`, reject the edit when both pointers are nil. Set `forced = true` for a relevant edit. Keep the existing draft rejection.

Update the `Forced` field comment because labels and writing edits can now request a fresh full review. Keep `Label` empty for an edit.

- [ ] **Step 4: Run the webhook package tests**

Run:

```bash
go test ./internal/webhook -count=1
```

Expected: `PASS`.

- [ ] **Step 5: Add one full application acceptance test**

Add `TestEndToEndDescriptionWritingBlockClearsAfterEdit` to `internal/app/app_test.go`. Use the existing signed webhook and local GitHub/model servers.

The first `opened` delivery returns one description finding from the writing analysis and no code finding. Assert one top level comment, no new inline comment, and `REQUEST_CHANGES`.

Change the fixture's current pull request description to clear problem-first prose. Send a signed `edited` delivery with `changes.body.from`. Return no finding from the second writing analysis and no code finding. Assert:

```go
if count := fixture.githubState.issueCommentCount(); count != 1 {
    t.Fatalf("top level comments = %d, want 1", count)
}
if body := fixture.githubState.summaryCommentBody(); strings.Contains(body, "### Pull request writing") {
    t.Fatalf("corrected description left a writing finding: %s", body)
}
if event := fixture.githubState.lastSubmitReview()["event"]; event != "APPROVE" {
    t.Fatalf("latest event = %q, want APPROVE", event)
}
```

Also assert that the collector read the full pull request twice. This pins the deliberate fast implementation choice to the existing forced-review path.

- [ ] **Step 6: Prove a failed edit keeps the prior block**

Add `TestEndToEndFailedDescriptionEditReviewKeepsPriorWritingBlock` beside the acceptance test. Start with a completed state carrying one description finding, then make the edit-triggered writing model call fail. Assert that the check fails, no approval is submitted, and `marker.DecodeState` still returns the original writing finding.

- [ ] **Step 7: Document the writing gate**

Update `docs/operations.md` in the review lifecycle section with this behavior:

```markdown
The review treats verified writing defects in changed documentation, changed source comments, the pull request title, and the pull request description as importance `10`. Changed-file findings appear inline. Title and description findings appear in the maintained top level comment and request changes without an invented source location.

Editing the pull request title or description starts a fresh full review at the current head. Correcting the last writing finding removes that block when no inline finding remains open.
```

- [ ] **Step 8: Run the complete verification suite**

Run:

```bash
make check
```

Expected: all formatting, lint, type, test, and build checks pass.

- [ ] **Step 9: Review the complete branch**

Run:

```bash
git diff --check origin/main...HEAD
git diff --stat origin/main...HEAD
git diff origin/main...HEAD
```

Verify that the branch contains no runtime corpus fetcher, no prose-only review path, no second top level comment, and no rule that judges human discussion prose.

- [ ] **Step 10: Commit the webhook and documentation changes**

Run:

```bash
git add internal/webhook/webhook.go internal/webhook/webhook_test.go internal/app/app_test.go docs/operations.md
git commit -S -m "Rerun writing reviews after pull request edits" -m "Co-authored-by: Codex <noreply@openai.com>"
```

- [ ] **Step 11: Verify every branch commit before pushing**

Run:

```bash
git fetch --prune origin
git log --format='%H %G?' origin/main..HEAD
git log --format='%H' origin/main..HEAD | while read -r commit_sha; do
    git verify-commit "$commit_sha"
    git cat-file commit "$commit_sha" | sed -n '/^gpgsig /,/^$/p'
done
```

Expected: every commit verifies and every raw commit object contains a `gpgsig` header.
