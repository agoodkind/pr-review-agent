# Block poor documentation and source comments

## Problem

The reviewer does not enforce the writing rules on changed documentation or source
comments. Its current writing policy controls only the prose the model produces, so
unclear or misleading changed prose can pass without a finding.

## Scope

The writing gate reviews:

- changed documentation; and
- changed source comments.

Every verified writing violation has importance `10`. It therefore exceeds every
valid `REVIEW_MIN_IMPORTANCE` value and blocks through the existing inline finding
lifecycle.

## Policy

The embedded policy contains only rules a reviewer can verify from changed prose. It
requires:

- the problem or decision before supporting detail;
- direct statements of cause;
- behavior before implementation;
- plain, complete, active sentences;
- one idea per sentence and paragraph;
- specific names and verbs;
- claims supported by current code and configuration;
- one purpose per documentation page;
- one durable home for each fact;
- headings and procedures that match the reader's task; and
- only details that affect understanding, verification, decisions, or action.

The policy excludes local tools, editing workflow, agent questions, document placement
choices, repository restructuring, and rewrite verification.

A finding must identify a specific violation that makes the changed prose inaccurate,
indirect, ambiguous, misleading, needlessly difficult to understand, or unsuitable for
its stated purpose. Personal style preferences do not qualify. The finding must quote
the changed text, state its impact, and give a concrete correction.

## Review behavior

Changed documentation and source comments use the existing finding schema and inline
publication path. The model anchors each writing finding to the changed prose and
assigns importance `10`.

The existing threshold, location validation, evidence grounding, deduplication,
publication, thread reconciliation, and verdict logic remain unchanged. No new marker
state, webhook action, model schema, model call, or review lifecycle is needed.

## Tests

Tests will prove:

1. The review prompt contains the relevant embedded rules.
2. The prompt assigns importance `10` to verified documentation and source comment
   violations.
3. The prompt excludes local editing and agent workflow instructions.
4. A grounded importance `10` writing finding uses the existing inline blocking path.
5. Clear changed prose produces no writing finding when the model reports none.

The operations guide will state that verified writing defects in changed documentation
and source comments are P0 inline findings.
