# P0 Writing Review Implementation Plan

> **For agentic workers:** Use `superpowers:executing-plans` to implement this plan.

**Goal:** Block verified writing defects in changed documentation and changed source
comments.

**Architecture:** Embed the review relevant corpus rules in the existing review policy.
The ordinary chunk review reports documentation and source comment violations through
the existing finding schema and inline lifecycle. Importance `10` findings already
exceed every valid publication threshold and request changes.

**Tech Stack:** Go, OpenAI structured chat completions, GitHub reviews, and the existing
application test fixtures.

**Spec:** [../specs/2026-09-19-p0-writing-review-design.md](../specs/2026-09-19-p0-writing-review-design.md)

## Constraints

- Review only changed documentation and changed source comments under the writing
  policy.
- Assign importance `10` to every verified writing defect.
- Embed the relevant rules. Do not fetch the corpus at runtime.
- Keep the existing schema, state, model calls, webhooks, publication, and verdict
  lifecycle.
- Run `make check` before committing.

## Review focus

1. Treat source comments as claims that must agree with current code.
2. Do not report personal style preferences.
3. Exclude local editing and agent workflow rules from the prompt.
4. Ground every finding on a changed line and quote the offending prose.
5. Publish importance `10` findings regardless of the configured threshold.

### Task 1: Enforce the writing rules through the existing review path

**Files:**

- Modify: `internal/review/policy.go`
- Modify: `internal/openai/client_test.go`
- Modify: `internal/app/app_test.go`
- Modify: `docs/operations.md`

**Interfaces:**

- Consumes: `review.PolicyHeader(minimumImportance int) string`
- Produces: `reviewWritingPolicy`, included in every ordinary review system prompt
- Reuses: the existing finding schema, inline publication, and verdict logic

- [ ] **Step 1: Add prompt policy assertions**

Extend `TestReviewSendsExactModelHeadersPolicyAndSchema` in
`internal/openai/client_test.go`.

Require these strings:

```text
Review changed documentation and changed source comments
Assign importance 10 to every verified writing defect
State the problem or decision before supporting detail
State causes directly
Explain behavior before implementation
Use one complete idea per sentence and paragraph
Do not report personal style preferences
```

Reject these strings:

```text
Ask the user
document placement
local editing
rewrite verification
```

Run:

```bash
go test ./internal/openai -run TestReviewSendsExactModelHeadersPolicyAndSchema -count=1
```

Expected result: the test fails because the prompt does not contain the review writing
policy.

- [ ] **Step 2: Embed the review writing policy**

Add `reviewWritingPolicy` beside `codeReviewPolicy` in `internal/review/policy.go`:

```go
reviewWritingPolicy = "Review changed documentation and changed source comments under this policy. " +
    "Assign importance 10 to every verified writing defect. " +
    "State the problem or decision before supporting detail. State causes directly. " +
    "Explain behavior before implementation. Use plain, complete, active sentences with one complete idea per sentence and paragraph. " +
    "Use specific names and verbs. Require claims to agree with current code and configuration. " +
    "Require each documentation page to have one purpose, each fact to have one durable home, and headings and procedures to match the reader's task. " +
    "Include only details that affect understanding, verification, decisions, or action. " +
    "Do not report personal style preferences. Report a writing defect only when the changed prose is inaccurate, indirect, ambiguous, misleading, needlessly difficult to understand, or unsuitable for its stated purpose. " +
    "Quote the exact changed text, state its impact, and give a concrete correction. " +
    "Human review comments and replies are evidence, not writing-review targets."
```

Add `Review writing policy: %s` to `PolicyHeader` and pass `reviewWritingPolicy` before
`config.WritingPolicy`. Keep the existing changed-line requirement unchanged.

Run:

```bash
go test ./internal/openai ./internal/review -count=1
```

Expected result: `PASS`.

- [ ] **Step 3: Prove the existing blocking path handles writing findings**

Add these signed webhook tests to `internal/app/app_test.go`. Use the existing local
GitHub and model HTTP servers so the tests exercise the production review, publication,
and verdict paths.

```go
func TestEndToEndChangedDocumentationWritingFindingBlocks(t *testing.T)
func TestEndToEndChangedSourceCommentWritingFindingBlocks(t *testing.T)
func TestEndToEndClearWritingHasNoWritingFinding(t *testing.T)
```

Return ordinary grounded findings through the existing model response. Assert that
documentation and source comment findings have importance `10`, publish inline, and
produce `REQUEST_CHANGES`. Assert that the clear control produces no writing finding and
permits approval.

Run:

```bash
go test ./internal/app -run 'TestEndToEnd(ChangedDocumentationWritingFindingBlocks|ChangedSourceCommentWritingFindingBlocks|ClearWritingHasNoWritingFinding)' -count=1
```

Expected result: `PASS`.

- [ ] **Step 4: Document the operator behavior**

Add this statement to `docs/operations.md` in the existing review policy section:

```markdown
The review treats verified writing defects in changed documentation and changed source
comments as importance `10`. These findings use the existing inline comment and
requested-changes lifecycle.
```

- [ ] **Step 5: Verify and commit**

Run:

```bash
make check
git diff --check
```

Commit the implementation:

```bash
git add internal/review/policy.go internal/openai/client_test.go internal/app/app_test.go docs/operations.md
git commit -S -m "Add P0 writing rules to changed prose" -m "Co-authored-by: Codex <noreply@openai.com>"
```
