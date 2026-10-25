# Assess every changed file before content review

> Use `executing-plans` to implement these tasks in dependency order. Track completion with checkboxes.

**Goal:** Exclude generated and dependency artifacts before chunk creation while reviewing necessary handwritten changes.

**Architecture:** Extend the existing file assessment, policy, and coverage contracts. Apply configured detectors to every changed file, then request bounded model classification for unresolved files. Collect repository attributes at immutable revisions and retain their evidence in assessment identities.

**Tech Stack:** Go, the existing GitHub client and provider clients, go-enry detection, and the go-git attributes parser.

**Spec:** [Review large files with explicit coverage](../specs/2026-10-22-large-file-review-design.md).

## Global constraints

Preserve streamed findings, provider fallback, checkpoint validation, and the maintained top-level comment.
Keep detector selection, exclusion patterns, review overrides, instructions, and evidence limits configurable.
Use maintained detector and attribute libraries rather than copying their rules.
Do not add provider mocks, fake GitHub servers, or cleanup tests.

## Review focus

Nested attributes and explicit false values must preserve repository precedence.
Deletion assessments must use the old revision and old path when required.
Excluded contents must not reappear as related evidence in later review prompts.
Deterministic assessments must not inflate model request counts.
An exclusion must not resolve an unrelated missing-content or metadata gap.

## Task 1: Collect repository classification evidence

Modify the diff collector and add a focused attributes helper. Extend `diff.FileContext` with structured repository attributes. Use the existing source and immutable comparison revisions to read root and ancestor `.gitattributes` files once per revision. Parse patterns and inheritance with the go-git parser. Missing attribute files are normal; other retrieval failures must remain visible. Include attributes in file assessment identities and bounded prompts.

- [ ] Implement attribute collection and structured evidence.
- [ ] Verify nested patterns, overrides, removals, and retrieval failures through real Git-backed inputs.
- [ ] Commit the evidence changes.

## Task 2: Assess every file before chunk creation

Extend `reviewrules.FileClassificationPolicy` with configurable trusted detectors, path exclusions, review overrides, and bounded evidence limits. Reuse go-enry for common generated, vendor, and binary detection. Put additional lockfile and compiled artifact patterns in runtime configuration. Implement `PlanFileAssessments` as a public deterministic assessment boundary used by service admission. Return a validated assessment or an unresolved target for every file. Keep ordinary handwritten content available for chunking.

Replace the size-only gate in `classifyLargeInput` with this preflight. Use existing `AssessFileContent` for uncertain decisions. Retain parent context and relevant handwritten changes without including excluded artifact bodies. Preserve existing assessment persistence and display. Count actual classification completions separately from deterministic decisions.

- [ ] Implement configuration, deterministic detection, bounded model decisions, and accurate accounting.
- [ ] Update conflicting configured prompts and existing behavior tests.
- [ ] Verify lockfile exclusions, generated headers, handwritten chunking, review overrides, and changed policies through public production boundaries.
- [ ] Commit the classification changes.

## Task 3: Verify and publish the complete change

Use the real PR #198 file list and an existing dedicated live fixture when available. Verify that classification records explain exclusions, ordinary review requests omit generated artifact bodies, and manifest changes still receive review. Keep actual content inspection distinct from assessed scope.

- [ ] Run relevant Go tests, format, vet, and scoped lint through existing Make targets.
- [ ] Run the available real integration path and record exact evidence.
- [ ] Review the complete diff for lost source context, stale identities, budget errors, and incorrect coverage.
- [ ] Create a ready pull request with verified results.
