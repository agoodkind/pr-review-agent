# Inline Markdown and Suggestions Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Format code references as inline code and render exact model replacements as native GitHub commit suggestions.

**Architecture:** Extend the strict finding contract with a separate replacement field. Keep explanatory Markdown and replacement source distinct, then let the marker renderer build the GitHub `suggestion` fence before the hidden identity marker.

**Tech Stack:** Go, OpenAI strict structured output, GitHub review comments, `go test`, and `go-makefile` checks.

## Global Constraints

- Preserve one short heading and one concise defect, impact, and fix paragraph.
- Put every code symbol, expression, environment variable, function name, type name, and literal in backticks.
- Emit a suggestion only for a complete replacement of the anchored changed line range.
- Keep the replacement empty when the fix needs other context or another range.
- Keep the hidden marker last and preserve historical finding identity.
- Add no production fixture, issue comment, reply, progress message, or command behavior.
- Never merge the temporary proof pull request.

---

### Task 1: Extend the structured finding contract

**Files:**
- Modify: `internal/domain/review.go`
- Modify: `internal/domain/review_test.go`
- Modify: `internal/openai/schema.go`
- Modify: `internal/openai/client_test.go`
- Modify: `internal/config/config.go`
- Modify: `internal/review/analyze.go`
- Modify: `internal/review/policy.go`
- Test: `internal/domain/review_test.go`
- Test: `internal/openai/client_test.go`
- Test: `internal/review/review_test.go`

**Interfaces:**
- Produces: `domain.Finding.Suggestion string` with JSON name `suggestion`.
- Produces: strict model output that requires `suggestion` for every finding.
- Produces: prompt instructions for exact replacements and inline code formatting.

- [ ] **Step 1: Write the contract tests**

Add a domain case that accepts an empty suggestion and rejects a suggestion containing a Markdown fence:

```go
valid.Suggestion = ""
if err := valid.Validate(); err != nil {
    t.Fatalf("valid finding: %v", err)
}

invalidSuggestion := valid
invalidSuggestion.Suggestion = "```go\nunsafe()\n```"
if err := invalidSuggestion.Validate(); err == nil {
    t.Fatal("fenced suggestion: want validation error")
}
```

Inspect the actual `json_schema` request in the existing HTTP client test. Assert that finding properties contain `suggestion` with type `string` and that the finding `required` array contains `suggestion`.

Assert that the real review prompt contains both `backticks` and `exact replacement`.

- [ ] **Step 2: Run the focused tests and confirm failure**

Run:

```bash
go test ./internal/domain ./internal/openai ./internal/review -count=1
```

Expected: the new field, schema requirement, and prompt assertions fail.

- [ ] **Step 3: Implement the model contract**

Extend `domain.Finding`:

```go
type Finding struct {
    Path       string `json:"path"`
    StartLine  int    `json:"start_line"`
    EndLine    int    `json:"end_line"`
    Title      string `json:"title"`
    Body       string `json:"body"`
    Suggestion string `json:"suggestion"`
    Importance int    `json:"importance"`
}
```

Reject `strings.Contains(finding.Suggestion, "```")`. Leave duplicate detection and stable marker identity independent of the optional replacement.

Add `suggestion` to the strict review schema properties and required fields.

Extend `WritingPolicy` with this exact behavior:

```text
Put every code symbol, expression, environment variable, function name, type name, and literal in backticks. Set suggestion to the exact source replacement for the anchored changed line range only when that replacement is complete and safe; otherwise set suggestion to an empty string.
```

Repeat the replacement rule in the review chunk prompt. Sanitize title and body prose, but preserve suggestion source exactly except for removing trailing carriage returns and line feeds.

- [ ] **Step 4: Run the focused tests and confirm success**

Run:

```bash
go test ./internal/domain ./internal/openai ./internal/review -count=1
```

Expected: all packages pass.

- [ ] **Step 5: Commit the contract**

```bash
git add internal/domain/review.go internal/domain/review_test.go internal/openai/schema.go internal/openai/client_test.go internal/config/config.go internal/review/analyze.go internal/review/policy.go internal/review/review_test.go
git commit -S -m "Add structured review suggestions" -m "Co-authored-by: Codex <noreply@openai.com>"
```

### Task 2: Render native GitHub suggestions

**Files:**
- Modify: `internal/marker/marker.go`
- Modify: `internal/marker/marker_test.go`
- Modify: `internal/review/render.go`
- Modify: `internal/review/review_test.go`
- Test: `internal/marker/marker_test.go`
- Test: `internal/review/review_test.go`

**Interfaces:**
- Consumes: `domain.Finding.Suggestion string`.
- Produces: `marker.EncodeFindingBody(domain.HeadSHA, domain.Finding) (string, error)` with an optional native GitHub suggestion block.
- Produces: `marker.DecodeFindingBody(domain.ReviewComment) (domain.HeadSHA, domain.Finding, error)` that recovers the replacement and still reads comments without one.

- [ ] **Step 1: Write public rendering tests**

Extend the marker round trip with this replacement:

```go
Suggestion: "if err != nil {\n\treturn err\n}",
```

Assert the encoded body contains exactly:

````text
```suggestion
if err != nil {
	return err
}
```
````

Assert `DecodeFindingBody` returns the same suggestion. Keep a second round trip with an empty suggestion to prove historical comment parsing.

Extend the existing `RenderInline` behavior test. Use backticks in its heading and paragraph. Assert the submitted comment body contains the suggestion block and ends with the hidden finding marker.

- [ ] **Step 2: Run the focused tests and confirm failure**

Run:

```bash
go test ./internal/marker ./internal/review -count=1
```

Expected: the encoded body lacks the suggestion fence and decoding does not recover the replacement.

- [ ] **Step 3: Implement suggestion rendering and decoding**

Build the visible body in this order:

```go
parts := []string{
    "### " + title,
    body,
}
if finding.Suggestion != "" {
    parts = append(parts, "```suggestion\n"+finding.Suggestion+"\n```")
}
parts = append(parts, markerText)
return strings.Join(parts, "\n\n"), nil
```

Decode an optional final `suggestion` fence before the hidden marker. Store its source in `Finding.Suggestion`. Keep title and body parsing unchanged when no fence exists.

Pass `Suggestion` through `RenderInline` after title and body sanitization.

- [ ] **Step 4: Run the focused tests and confirm success**

Run:

```bash
go test ./internal/marker ./internal/review -count=1
```

Expected: both packages pass.

- [ ] **Step 5: Commit GitHub rendering**

```bash
git add internal/marker/marker.go internal/marker/marker_test.go internal/review/render.go internal/review/review_test.go
git commit -S -m "Render GitHub review suggestions" -m "Co-authored-by: Codex <noreply@openai.com>"
```

### Task 3: Verify the production change

**Files:**
- Modify: `docs/superpowers/plans/2026-08-16-inline-markdown-suggestions.md`

**Interfaces:**
- Consumes: the complete finding contract and renderer.
- Produces: a branch that passes the canonical consumer gates and contains no temporary proof fixtures.

- [ ] **Step 1: Run all Go tests**

```bash
go test ./... -count=1
go test -race ./... -count=1
```

Expected: both commands pass.

- [ ] **Step 2: Run the required build and canonical check**

```bash
make build
make check
```

Expected: both commands pass.

- [ ] **Step 3: Audit branch scope**

```bash
git diff --name-status origin/main...HEAD
```

Expected: only the design, plan, finding contract, prompt, marker, renderer, and their existing tests changed. No `internal/liveproof`, live proof workflow, deployment, Docker, or infrastructure file appears.

- [ ] **Step 4: Commit the completed plan record**

```bash
git add docs/superpowers/plans/2026-08-16-inline-markdown-suggestions.md
git commit -S -m "Record inline suggestion implementation" -m "Co-authored-by: Codex <noreply@openai.com>"
```

- [ ] **Step 5: Verify every branch commit**

```bash
git log --format='%H %G?' origin/main..HEAD
git cat-file commit HEAD
```

Expected: every commit reports a valid signature, and every raw commit contains `gpgsig`.
