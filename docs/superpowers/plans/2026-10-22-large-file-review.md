# Large File Review Implementation Plan

> Use `executing-plans` to implement the plan in dependency order. Track completed steps with checkbox syntax.

**Goal:** Review necessary large text completely and record evidence for each justified content exclusion.

**Architecture:** Recover complete changes at pinned revisions before dividing review work. Extend the existing diff fragments, omission assessment, checkpoints, and maintained summary.

**Tech Stack:** Go, the existing GitHub client and model interfaces, configurable JSON policy, and real Git repositories for regression tests.

**Spec:** [Large file review design](../specs/2026-10-22-large-file-review-design.md).

## Global Constraints

Keep necessary changed rows and original line coordinates.
Keep classification kind separate from the review action.
Require evidence for each accepted content exclusion.
Keep prompts and resource limits in configuration.
Preserve streamed findings, durable checkpoints, provider behavior, and one top-level comment.
Keep retrieval and model failures visible as gaps.
Use real dependencies rather than mocks, recorded responses, or fake providers in new regression tests.
Do not require Git in the production container.

## Review Focus

Preserve deletion-only fragments with a zero new-side count.
Detect an omitted later hunk even when the supplied earlier hunk parses completely.
Keep Unicode and final-newline markers intact when dividing a hunk.
Keep a gap for one indivisible line that exceeds the complete request budget.
Invalidate progress and exclusions when the comparison or relevant policy changes.

## Task 1: Divide one oversized hunk

Modify [collect.go](../../../internal/diff/collect.go), [patch.go](../../../internal/diff/patch.go), and [budget.go](../../../internal/diff/budget.go). Add [split.go](../../../internal/diff/split.go) for hunk partitioning and [large_file_integration_test.go](../../../internal/diff/large_file_integration_test.go) for real Git regression tests.

**Interfaces:** Preserve `ChunkInput(ReviewInput, int) ([]Chunk, error)`, `BoundChunks`, `Chunk.Split`, and `ChangedSourceRows`. Add an internal partitioner that consumes a complete `parsedHunk` and produces smaller complete hunks or reports that division is impossible.

This milestone enforces the existing rendered review-input limit. Task 4 adds accounting for provider system instructions and response schemas before claiming a complete provider request limit.

- [x] Partition content rows while advancing old and new coordinates independently. Keep newline markers attached to their source row.
- [x] Extend `Chunk.Split` to divide one file piece. Keep metadata splitting unchanged.
- [x] Partition large hunks before constructing oversized placeholders in `ChunkInput` and the rendered input budget path. Keep an indivisible oversized line explicit.
- [x] Preserve original file anchor maps and render context around each fragment.
- [x] Add `TestLargeGitHunkRetainsChangedRowsWithinPromptBudget` through a real temporary Git repository and production `ChunkInput`, `BoundReviewChunks`, and `ChangedSourceRows`. Assert exact added-line coordinates, deletion preservation, bounded prompts, and complete coverage.
- [x] Exercise additions, deletion-only changes, replacements, Unicode, and missing final newlines. Rewrite tests that require large multiline hunks to remain unreadable.
- [x] Run `go test ./internal/diff ./internal/review` and scoped quality checks. Commit this change independently.

## Task 2: Recover omitted or incomplete patches

Modify [client.go](../../../internal/githubapp/client.go), [pulls.go](../../../internal/githubapp/pulls.go), and collection code. Add [recover.go](../../../internal/diff/recover.go) for reconstruction and [recovery_integration_test.go](../../../internal/diff/recovery_integration_test.go) for real Git collection tests. Generate unified patches with [go-udiff](https://github.com/aymanbagabas/go-udiff).

**Interfaces:** Extend `githubapp.ChangedFile` with validated addition and deletion metadata. Distinguish absent counts from zero. Retain the collector's public source interface where possible. The internal reconstruction function consumes pinned old and new bytes and returns a complete unified text patch or an explicit non-text result.

- [x] Preserve change counts at the GitHub JSON boundary.
- [x] Resolve the effective merge base before full pull request reconstruction. Use `PreviousPath` for renamed files.
- [x] Retrieve full bytes when the Contents API omits inline content. Reuse authenticated endpoints and bounded responses.
- [x] Reconstruct absent, malformed, incomplete, or count-mismatched text patches with a selected Go library. Validate reconstructed rows against the head content.
- [x] Keep binary, unavailable, and over-limit content explicit.
- [x] Add production-path tests with real Git objects and an opt-in real GitHub check. Cover comparison bases, additions, removals, renames, and truncation after a complete earlier hunk.
- [x] Run collection, GitHub client, review, and scoped quality checks. Commit this change independently.

## Task 3: Add contextual per-file assessments

Extend [review.go](../../../internal/domain/review.go), the shared [schema.go](../../../internal/openai/schema.go), configurable [review-prompts.json](../../../config/review-prompts.json), and the existing [coverage.go](../../../internal/review/coverage.go) assessment.

**Interfaces:** Add per-file assessments to `domain.ReviewResult`. Each assessment contains a current file identity, content kind, review action, reason, and references to supplied evidence. The model schema and configured prompts use the same contract.

- [x] Collect bounded evidence from actual content, change metadata, related changes, and supplied generation or validation information. Do not invent unavailable check results.
- [x] Validate file identities and evidence references. Reject fabricated evidence, missing decisions, and incompatible saved state.
- [x] Review necessary readable content through the recovered and partitioned text path. Require reasons for exclusions.
- [x] Derive aggregate omission acceptance from complete per-file decisions. Keep actual content coverage separate from approval eligibility.
- [ ] Verify the specification's contextual distinctions with actual configured providers through existing opt-in evaluation tooling. Include generated SQL and both encrypted-vault scenarios.
- [ ] Run schema, policy, provider, coverage, and live evaluation checks. Commit this change independently.

## Task 4: Preserve context, progress, and diagnostic detail

Extend existing chunk identities, assessment snapshots, and omission records. Reuse the maintained summary and its configured details section.

**Interfaces:** Save comparison revisions, region coordinates, and relevant policy revision with compatible progress. Record reviewed regions, accepted content exclusions, and unresolved regions separately.

- [x] Resume unfinished work without reposting existing inline findings. Invalidate incompatible checkpoints and exclusions.
- [x] Include provider system instructions and response schemas in complete request accounting before partitioning input. Verify the actual provider request boundary rather than only `review.input`.
- [x] Assess shared behavior across fragments with bounded source context and a configured relationship review. Keep necessary unseen dependencies as gaps.
- [x] Render classification reasons and evidence references in the existing configurable details section.
- [x] Verify a moved head, changed policy, accepted exclusions, essential missing content, and resumed publication through real-dependency integration boundaries.
- [x] Run complete required checks and review the branch against the specification. Prepare a pull request with the actual implemented scope and evidence.

## Delivery Scope

Complete all four tasks as one delivery. Verify required checks and resolve review findings before merging. Deploy through the independent candidate process, confirm the exact active revision, and run the deployed classification scenarios through real GitHub deliveries. Restore the dedicated fixture after testing. Source verification does not establish production behavior.
