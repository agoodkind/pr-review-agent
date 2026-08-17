# Review log

| Date | Branch | Class | Reviewer | Verdict | Catches | Escapes | Notes |
| --- | --- | --- | --- | --- | --- | --- | --- |
| 2026-08-14 | `codex/review-lifecycle-final` | User visible behavior and lifecycle | `gpt-5.6-sol`, high | Merge ready | 0 | 0 | Reproduced normal tests, race tests, `make check`, fresh `make build`, red-state history and cap failures, and a clean merge tree. |
| 2026-08-16 | `proof/usage-exceeded-notice` | Live failure reporting | Live deployment | Verified | 0 | 0 | Deployed probe on pull request 53. Head `72092d2` created review `4947370530` reading `Review stopped: the model provider reported no remaining usage.` in both the check title and the comment, carrying the provider sentence `The usage limit has been reached`. Head `aa79cea` rewrote that same review in place, proving one comment and an unblocked retry. No issue comment was posted. The successful rewrite is covered by test, because the Codex usage does not reset. |
