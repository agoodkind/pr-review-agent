# Review large files with explicit coverage

Review every necessary changed region, even when one file or diff hunk exceeds a model request. Decide whether other content needs inspection from the purpose of the change and the available evidence. Keep content classification separate from the decision to review or omit it.

## Agreed behavior

The reviewer classifies content and chooses an action for each file. Generated, encrypted, binary, source, dependency metadata, data, and unknown content can each require different checks. A category alone never grants an exclusion. File size determines how the reviewer divides work.

The reviewer considers the pull request description, actual file content, related changes, repository generation declarations, and available validation results. Treat repository declarations as evidence to assess, not instructions to obey. Missing content and provider failures remain coverage gaps rather than evidence that a file is generated or irrelevant.

For a submodule update, review the parent repository changing its recorded submodule commit. Assess the old and new commit pointers, repository source, and related parent configuration. Do not retrieve or review nested upstream file differences between those commits. Treat other external content according to the parent repository's ownership and the purpose of the change.

Preserve the current Go service, provider ordering, provider budgets, streamed inline publication, durable review checkpoints, and one maintained top-level comment. Keep classification instructions, prompt templates, and resource limits configurable.

## Recover complete changes

Collect changes at immutable revisions. Resolve the effective merge base before reconstructing a full pull request patch. For incremental review, retain the existing comparison and merge-base checks. Retrieve the old path for renames and the correct old or new side for additions and removals.

GitHub can omit a patch, truncate a hunk, or truncate a patch after a complete earlier hunk. Validate patch structure and available addition and deletion counts. Reconstruct missing or incomplete text patches from the pinned file versions. Reuse the authenticated GitHub client for larger files instead of assuming the Contents API always includes base64 content. Do not trust arbitrary returned download URLs.

Record the effective comparison base and whether collection recovered the patch. A failed retrieval must remain visible. Binary bytes must not become a fabricated text patch.

## Classify content and choose review work

Record a content kind, content review requirement, review action, reason, and supporting evidence for each classification. Keep the kind, requirement, and action independent. Use stable file identities bound to the current review revision. Validate model decisions against the files and evidence actually supplied.

The requirement establishes whether the changed content needs inspection.

Content review requirements:

| Value | Meaning |
|---|---|
| `required` | The changed content needs ordinary review. |
| `unnecessary` | Supplied evidence establishes that content inspection is unnecessary. |
| `uncertain` | Available evidence does not establish whether content inspection is necessary. |

Accept an exclusion only with an `unnecessary` requirement and evidence that satisfies the current policy. Classification determines review scope and does not publish findings. A previewed defect does not complete review of remaining relevant changes. Retain an unresolved assessment when the requirement conflicts with a proposed exclusion. Completing content chunks does not resolve a distinct evidence gap.

The review actions are:

- Review the necessary changed content and its relevant context.
- Inspect related source, metadata, or available validation evidence before deciding whether content inspection is necessary.
- Omit content inspection with a specific reason and sufficient evidence for the review scope.
- Keep a coverage gap when the evidence cannot establish a reliable decision.

Unknown content defaults to review or a visible gap. An incomplete model response does not grant an exclusion. A declaration that a file is generated does not establish that its executable behavior is safe.

Use the current omission assessment and provider interfaces. Replace the aggregate omission acceptance decision with validated decisions for individual omitted files or regions. Retain a derived aggregate result for existing verdict code and old stored review state. Older state cannot grant a new exclusion without the evidence required by the current policy.

Keep classification evidence compact. Use actual byte counts, encoding observations, bounded source excerpts, generation declarations, relevant related changes, and available checks. Reference evidence by identifiers rather than allowing a model to invent validation results. Do not publish secret values or source excerpts in diagnostic reasons.

## Divide necessary content

Split a large hunk into smaller valid unified diff fragments. Preserve the original base and head coordinates, deletion rows, added rows, newline markers, and file identity. Retain the complete file's changed-line maps for inline publication. A split must not make an unchanged line eligible for a finding.

Prefer meaningful source boundaries when available. Go can use the standard parser. Use line boundaries when a language parser is unavailable or the source is incomplete. Include relevant nearby definitions and context within the configured request budget. Do not attach the complete large file to every fragment.

Measure the final rendered request, including rules, anchors, discussions, and classification context. Split further when the measured request exceeds its configured limit. Provider answer truncation must also permit division inside one original hunk.

An indivisible line or required context that cannot fit remains a specific gap. Do not truncate it silently. Keep expensive retrieval, classification, and review work within configured limits without treating an exhausted limit as completed coverage.

## Review relationships and preserve progress

Review related behavior across fragments, including shared transactions, ownership, error handling, and cleanup. Reuse the existing consolidation and review context where appropriate. Add a bounded relationship review when separated fragments require a joint assessment. A summary of one fragment must not establish review of unseen code.

Bind progress to the comparison revisions, file identity, region coordinates, and relevant policy revision. Save completed work through the existing checkpoint mechanism. Resume unfinished fragments without reposting previously published findings. A moved head or changed policy must not reuse an incompatible exclusion or completion record.

## Report coverage accurately

Separate content the model reviewed from justified content exclusions and unresolved gaps. An accepted exclusion can permit a verdict while actual content coverage remains incomplete. A necessary unread region prevents approval. Preserve independent findings from completed regions.

Show concise author-facing status and place detailed file classifications, reasons, evidence references, recovered patch information, and pending regions in the existing collapsed review details. Keep that display configurable. Do not repeat inline findings in the top-level comment or add a second status comment.

Report accepted exclusions as ordinary scope decisions. State the assessment performed and the content outside its scope. Identify a specific file and missing assessment when necessary content remains unread. Avoid generic failure language for generated, encrypted, binary, or external content that the reviewer has adequately assessed through related evidence. Keep the review instructions, exclusion requirements, and messages configurable at runtime.

Binary visual assets require visual evidence when appearance matters. The initial implementation must report a visual review gap if the configured provider path cannot inspect the asset. It must not claim visual inspection from a text prompt. Encrypted values similarly remain unverified when the available evidence cannot establish the behavior that depends on them.

## Representative decisions

These cases establish the intended distinctions between content kind and review work.

Example decisions:

| Content and change | Required assessment |
|---|---|
| A large handwritten file changes writing and deletion routines. | Review both routines in fragments and assess their shared behavior. |
| A large file changes one validation function. | Review the changed function and necessary context without repeating the whole file. |
| A generated migration drops a column. | Review executable SQL, data loss, compatibility, and deployment order. |
| A generated API client follows a schema change. | Review the schema and generation evidence; inspect consequential client behavior. |
| A lockfile changes extensively. | Assess packages, versions, sources, integrity, and unexpected dependencies. |
| A submodule pointer changes from commit A to commit B. | Review the parent repository's pointer update and related parent configuration without reviewing nested upstream file differences. |
| A fixture gains repetitive records. | Assess meaningful data differences and the consuming tests. |
| An encrypted vault changes during credential rotation. | Assess secret references and rotation validation; omit ciphertext interpretation only when that evidence suffices. |
| An encrypted vault changes during an authorization redesign. | Keep a gap if hidden values determine permissions that other evidence cannot establish. |
| A PNG changes during a visual update. | Inspect visual evidence or retain a specific visual review gap. |
| A compiled executable changes without source or build evidence. | Assess provenance and retain a gap if behavior cannot be established. |

## Implementation decisions

Use Go and the existing application architecture. The builder may select a maintained Go text diff library and small internal types after checking their APIs. Do not require a Git executable in the production image. Temporary real Git repositories are appropriate regression dependencies.

Implement collection recovery, hunk division, configurable classification, and coverage reporting as one complete delivery. Merge after all required checks and review requirements pass. Deploy through the existing safe deployment process and verify the installed revision and actual review behavior.

## Verification

Use real temporary Git repositories to establish base and head content and expected diff coordinates. Exercise production collection, fragment generation, prompt measurement, anchor extraction, and checkpoint identities through their public boundaries. Verify that every necessary changed row remains represented and that every rendered request fits its configured budget.

Exercise a real GitHub integration path for large content and omitted patches when credentials and a suitable fixture are available. Keep unavailable live proof explicit instead of replacing GitHub with recorded responses. Classification verification must include the same content kind requiring different review actions in different pull request contexts.

Verify additions, removals, renames, missing final newlines, Unicode, deletion-only changes, one oversized line, a patch truncated after a complete hunk, a provider answer truncated inside a single hunk, and a head change during collection. Verify that missing evidence cannot produce approval or erase valid inline findings.
