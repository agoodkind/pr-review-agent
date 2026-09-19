# Block pull requests with poor writing

## Problem

The reviewer does not enforce the writing rules on pull request content. Its
current writing policy controls only the prose the model produces. The model
may notice inaccurate documentation or a misleading source comment as a code
defect, but it has no instruction to review writing consistently.

Pull request prose cannot produce an actionable finding. Every finding must
name a changed file and line, while the pull request title and description have
neither. Attaching a description problem to an unrelated source line would
misstate the evidence. The service also ignores pull request `edited` events,
so correcting a title or description would not rerun the review.

## Scope

The writing gate reviews these surfaces:

- changed documentation;
- changed source comments;
- the current pull request title; and
- the current pull request description.

Human review comments and replies remain evidence about the change. The gate
does not judge their writing.

A verified violation has importance `10`. The finding therefore exceeds every
valid `REVIEW_MIN_IMPORTANCE` value and blocks the pull request.

The embedded policy contains only rules a reviewer can judge from the current
pull request. It requires:

- the problem or decision before supporting detail;
- direct statements of cause;
- behavior before implementation;
- plain, complete, active sentences;
- one idea per sentence and paragraph;
- specific names and verbs;
- claims supported by the current code and configuration;
- one purpose per documentation page;
- one durable home for each fact;
- headings and procedures that match the reader's task; and
- only details that affect understanding, verification, decisions, or action.

The policy excludes instructions about local tools, editing workflow, agent
questions, document placement choices, repository restructuring, and rewrite
verification. A reviewer cannot establish whether an author followed those
processes from the pull request result.

Minor style preferences do not become findings. A finding must identify a
specific rule violation that makes the text inaccurate, indirect, ambiguous,
misleading, needlessly difficult to understand, or unsuitable for its stated
purpose. The finding must quote the exact text that proves the violation and
state a concrete correction.

## Review model

The embedded review policy becomes a separate input policy. The existing
writing policy continues to control the model's own output.

Changed documentation and source comments use the existing inline finding
schema. The model anchors each finding to the changed prose that violates the
policy. Ordinary code remains subject to the existing code review rules.

The title and description receive one separate analysis per review run. This
analysis returns pull request findings with a subject of `title` or
`description`, a concise heading, direct explanation, quoted evidence, stable
claim, and importance. It cannot return a file path or line range because
those locations do not exist.

The separate analysis prevents every diff chunk from reporting the same pull
request prose defect. It also permits a prose-only review when the title or
description changes without a new commit.

## Publication and durable state

The service keeps its single top level issue comment. Current title and
description findings appear in a `Pull request writing` section of that
comment. Changed documentation and source comment findings remain inline.

The hidden state marker records the current title and description findings
with a digest of the prose they evaluated. A completed run replaces that set.
The marker lets a restarted container preserve the block until a later review
proves the prose is corrected.

The verdict requests changes when either condition is true:

- one of the service's actionable inline threads remains open; or
- one current title or description finding remains in the marker.

The blocking section names both sources. Inline reasons link to their review
threads. Pull request prose reasons name `title` or `description` and appear in
the maintained top level comment.

## Pull request edits

The webhook accepts a non-draft pull request `edited` event when the title or
description changed. That event starts a prose-only review for the current
head. It does not reread unchanged diff chunks or move the last reviewed
commit.

The prose-only run replaces the stored title and description findings and
recomputes the verdict from those findings plus the current inline threads.
Correcting the last prose finding therefore removes the writing block without
a source commit.

A failed prose analysis changes no review state. Its check reports the failed
run, while the previous verdict and stored findings remain intact because the
service has no new result that can justify changing them.

## Tests

End-to-end application tests will enter through signed webhook requests and
exercise the real review, publication, marker, and verdict paths through local
HTTP servers for GitHub and the model provider.

The tests will prove:

1. An indirect or misleading changed documentation sentence produces an
   importance `10` inline finding and a requested-changes verdict.
2. A poor changed source comment produces the same blocking result.
3. A change with clear documentation and source comments produces no writing
   finding.
4. A poor pull request description appears in the maintained top level comment
   and blocks without inventing a source location.
5. An `edited` event with a corrected description removes that finding and
   permits approval when no inline thread remains.
6. A failed prose-only review preserves the previous findings and verdict.
7. The model prompt contains the review-relevant embedded rules and excludes
   local editing and agent workflow instructions.
8. Every writing finding has importance `10`, regardless of the configured
   publication threshold.

The operations guide will describe the writing gate, the `edited` event, and
the two finding locations.
