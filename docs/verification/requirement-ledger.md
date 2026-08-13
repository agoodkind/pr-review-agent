# PR review requirement ledger

Inventory date: 2026-08-13.
Inventory commit: `c24baf3` on `08-13-replace_clyde_client_with_openai_sdk_in_rewrite_plans`.
Plan 1 tip: `2680335` on `08-13-add_go_pr_review_service`.
Trunk: `origin/main` `65b3755`.
Scaffold: `a3c4f1c`.

Rows stay `Not yet proven` until a later Plan 2 task records a command, CI run, image digest, release, deployment, or live GitHub artifact. Source paths and test names are locators, not proof.

## Task 2 local reproduction

Commit: `19a4bcb`.
No defects recorded.

| Command | Start | End | Exit |
| --- | --- | --- | --- |
| `go test ./... -count=1` | 2026-08-13 15:12:15 PDT | 2026-08-13 15:12:20 PDT | 0 |
| `go test -race ./... -count=1` | 2026-08-13 15:12:27 PDT | 2026-08-13 15:12:44 PDT | 0 |
| `go test ./internal/app -count=10 -run TestSignedWebhookProducesOneReviewCheckAndSilentReconciliation` | 2026-08-13 15:12:58 PDT | 2026-08-13 15:13:10 PDT | 0 |
| `go test ./internal/app -count=10 -run TestEndToEndStaleHeadProducesNoReview` | 2026-08-13 15:12:58 PDT | 2026-08-13 15:13:10 PDT | 0 |
| `go test ./internal/queue -race -count=50 -run TestEndToEndConcurrentDuplicateDeliveryOneReview` | 2026-08-13 15:12:58 PDT | 2026-08-13 15:13:10 PDT | 0 |
| `go test ./internal/app -count=1 -run TestEndToEndMoreThanOneHundredFilesAndThreads` | 2026-08-13 15:12:58 PDT | 2026-08-13 15:13:10 PDT | 0 |
| `make check` | 2026-08-13 15:11 PDT | 2026-08-13 15:11 PDT | 0 |

`make check` passed lint-golangci, lint-format, lint-gocyclo, lint-deadcode, and staticcheck-extra. Test servers reject `/issues/comments` and `/pulls/comments/.../replies` in `internal/app/app_test.go`, `internal/review/review_test.go`, and `internal/githubapp/client_test.go`. Passing tests do not close this ledger.

## Task 3 adversarial review

Commit: `9595a60`.
Confirmed findings: none.

Reviewed against source for the 18 Plan 2 attack classes. HMAC compare uses `hmac.Equal`. File content loads `pullRequest.Head`, not the queued webhook SHA. Production GitHub client has no issue-comment or reply write. Queue claim, keyed lock, and stale-head cancel have passing tests from Task 2. No reachable confirmed defect was reproduced, so no Go fix is in this task.

## Plan 1 output

| Position | Branch | PR | State | Base | Tip |
| --- | --- | --- | --- | --- | --- |
| Closed stack 1 | `08-13-add_review_domain_models_and_markers` | [1](https://github.com/agoodkind/pr-review-agent/pull/1) | CLOSED | `main` | `a1c06b6` |
| Closed stack 2 | `08-13-add_bounded_review_job_serialization` | [2](https://github.com/agoodkind/pr-review-agent/pull/2) | CLOSED | PR 1 branch | `ba01b4a` |
| Closed stack 3 | `08-13-add_structured_clyde_review_client` | [3](https://github.com/agoodkind/pr-review-agent/pull/3) | CLOSED | PR 2 branch | `841dd5b` |
| Closed stack 4 | `08-13-add_complete_github_review_publication` | [4](https://github.com/agoodkind/pr-review-agent/pull/4) | CLOSED | PR 3 branch | `3079710` |
| Closed stack 5 | `08-13-add_pr_review_webhook_service` | [5](https://github.com/agoodkind/pr-review-agent/pull/5) | CLOSED | PR 4 branch | unknown |
| Active Plan 1 | `08-13-add_go_pr_review_service` | [6](https://github.com/agoodkind/pr-review-agent/pull/6) | OPEN | `main` | `2680335` |
| Plan 2 docs | `08-13-replace_clyde_client_with_openai_sdk_in_rewrite_plans` | none | local | PR 6 branch | `c24baf3` |

`git diff --name-only origin/main...2680335` is Go files plus `go.mod` and `go.sum` for `github.com/openai/openai-go v1.12.0`. No workflow, image, or infrastructure files in that range.

Webhook path: `POST /api/v1/github_webhooks` verifies HMAC, claims the delivery, enqueues one job. `review.Service.Run` creates or starts `PR-Agent Review`, collects diffs, calls OpenAI, submits one review, then `reconcile.Service.Reconcile` may resolve owned threads. Production GitHub client has no issue-comment or reply write. Those strings appear only in tests that forbid the endpoints.

## Product

| Requirement | Go source | Focused test | Full test | Race test | CI evidence | Image evidence | Release evidence | Deployment evidence | Live GitHub evidence |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| `pull_request` opened, reopened, ready_for_review, synchronize | [webhook.go](../../internal/webhook/webhook.go) | `TestParsePullRequestAcceptsFourSupportedActions` | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven |
| Ignore drafts until ready_for_review | [webhook.go](../../internal/webhook/webhook.go) | `TestParsePullRequestIgnoresDraftAndUnsupportedEvents` | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven |
| Duplicate delivery creates no second review | [handler.go](../../internal/app/handler.go), [cache.go](../../internal/queue/cache.go) | `TestDuplicateDeliveryReturns202WithoutExtraWork`, `TestEndToEndConcurrentDuplicateDeliveryOneReview` | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven |
| Already reviewed head creates no second review | [service.go](../../internal/review/service.go), [marker.go](../../internal/marker/marker.go) | `TestServiceSkipsHeadWithExistingReviewMarker`, `TestEndToEndFreshAppInstanceMarkerDedup` | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven |
| Unsupported event acknowledges without work | [handler.go](../../internal/app/handler.go) | `TestUnsupportedEventReturns202` | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven |
| One PR-Agent Review check queued, in_progress, completed | [checks.go](../../internal/githubapp/checks.go), [service.go](../../internal/review/service.go) | `TestServicePublishesOneCompleteReviewAndCompletesCheck`, `TestCheckRunLifecycleUsesExpectedPayloads` | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven |
| One GitHub review per head with summary and marker | [reviews.go](../../internal/githubapp/reviews.go), [render.go](../../internal/review/render.go) | `TestSignedWebhookProducesOneReviewCheckAndSilentReconciliation` | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven |
| Inline findings in the same review comments array | [render.go](../../internal/review/render.go) | `TestRenderInlineUsesRightSideRangesAndFindingMarkers` | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven |
| APPROVE only when coverage is complete and findings are empty | [policy.go](../../internal/review/policy.go) | `TestDecisionForAllThreeDecisions`, `TestEndToEndApproveWithCompleteCoverage` | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven |
| COMMENT for importance 1 through 6 | [policy.go](../../internal/review/policy.go) | `TestEndToEndCommentWithLowImportanceFindings` | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven |
| COMMENT when coverage is incomplete | [policy.go](../../internal/review/policy.go) | `TestIncompleteCoverageNeverApproves`, `TestEndToEndCommentWithIncompleteCoverage` | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven |
| REQUEST_CHANGES for importance 7 through 10 | [policy.go](../../internal/review/policy.go) | `TestEndToEndRequestChangesWithBlockingFinding` | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven |
| Invalid model output fails the review | [analyze.go](../../internal/review/analyze.go), [client.go](../../internal/openai/client.go) | `TestAnalyzeFailsOnInvalidModelResult`, `TestReviewRejectsInvalidFindings` | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven |
| Stale head cancels and publishes no review | [service.go](../../internal/review/service.go) | `TestServiceCancelsWhenHeadChangesBeforePublication`, `TestEndToEndStaleHeadProducesNoReview` | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven |
| No issue comment or progress comment | [reviews.go](../../internal/githubapp/reviews.go) | `TestEndToEndNeverCallsIssueCommentOrReplyEndpoints` | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven |
| Later head gets its own review | [service.go](../../internal/review/service.go) | `TestSignedWebhookProducesOneReviewCheckAndSilentReconciliation` | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven |
| Owned fixed or invalid findings resolve silently | [service.go](../../internal/reconcile/service.go), [threads.go](../../internal/githubapp/threads.go) | `TestReconcileResolvesOnlyProvenFixedOrInvalidFindings`, `TestReconcilePostsNoReplies` | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven |
| Open or uncertain findings stay unresolved | [service.go](../../internal/reconcile/service.go) | `TestReconcileLeavesOpenAndUncertainFindingsOpen` | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven |
| Reconciliation failure does not change a successful check | [service.go](../../internal/review/service.go) | `TestServiceKeepsSuccessWhenReconciliationFails`, `TestEndToEndReconciliationFailureIsolation` | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven |
| Unanchored findings stay in the review body | [render.go](../../internal/review/render.go) | `TestRenderBodyContainsSummaryUnanchoredAndMarker` | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven |
| Writing policy injected into every prompt | [policy.go](../../internal/review/policy.go), [client.go](../../internal/openai/client.go) | `TestAnalyzePromptsInjectWritingPolicyAndUntrustedInput` | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven |
| No typographic dashes in published prose | [policy.go](../../internal/review/policy.go) | `TestEndToEndPublishedProseHasNoTypographicDashes` | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven |
| Model is gpt-5.6-sol with reasoning high | [config.go](../../internal/config/config.go), [client.go](../../internal/openai/client.go) | `TestReviewSendsExactModelHeadersPolicyAndSchema` | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven |
| OpenAI failure fails the lifecycle | [service.go](../../internal/review/service.go) | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven |
| More than 100 files and 100 threads | [collect.go](../../internal/diff/collect.go), [threads.go](../../internal/githubapp/threads.go) | `TestEndToEndMoreThanOneHundredFilesAndThreads` | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven |

## Security

| Requirement | Go source | Focused test | Full test | Race test | CI evidence | Image evidence | Release evidence | Deployment evidence | Live GitHub evidence |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| HMAC SHA-256 webhook signatures | [webhook.go](../../internal/webhook/webhook.go) | `TestVerifySHA256AcceptsValidSignature`, `TestInvalidSignatureReturns401` | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven |
| Body cap 2 MiB | [handler.go](../../internal/app/handler.go) | `TestOversizedBodyReturns413` | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven |
| Secrets absent from errors and logs | [config.go](../../internal/config/config.go) | `TestLoadErrorsDoNotContainSecretValues` | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven |
| Untrusted input delimiters around repository content | [policy.go](../../internal/review/policy.go) | `TestAnalyzePromptsInjectWritingPolicyAndUntrustedInput` | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven |
| Finding markers reject traversal paths | [marker.go](../../internal/marker/marker.go) | `TestFindingMarkerRejectsUnsafePaths` | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven |
| No issue-comment write in production client | [reviews.go](../../internal/githubapp/reviews.go), [client.go](../../internal/githubapp/client.go) | `TestEndToEndNeverCallsIssueCommentOrReplyEndpoints` | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven |
| No review-comment reply write in production client | [threads.go](../../internal/githubapp/threads.go) | `TestReconcilePostsNoReplies` | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven |
| No commands, mentions, or compatibility flags | inventory of `*.go` | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven |
| GitHub App JWT and installation token cache | [auth.go](../../internal/githubapp/auth.go) | `TestAppJWTContainsExpectedClaimsAndValidSignature`, `TestInstallationTokenIsCachedAndRefreshedBeforeExpiry` | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven |
| Client errors expose status without credentials | [client.go](../../internal/githubapp/client.go) | `TestClientErrorsExposeStatusWithoutCredentials` | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven |
| Strict env load of every required secret | [config.go](../../internal/config/config.go) | `TestLoadRequiresEverySecret` | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven |
| RSA private key parse PKCS1 and PKCS8 only | [config.go](../../internal/config/config.go) | `TestLoadParsesPKCS1AndPKCS8RSAKeys`, `TestLoadRejectsNonRSAAndMalformedKeys` | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven |

## Concurrency

| Requirement | Go source | Focused test | Full test | Race test | CI evidence | Image evidence | Release evidence | Deployment evidence | Live GitHub evidence |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| Single-worker bounded queue | [dispatcher.go](../../internal/queue/dispatcher.go) | `TestDispatcherRunsJobsInOrder`, `TestDispatcherRejectsWhenFull` | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven |
| Same pull request serialized | [keyed.go](../../internal/queue/keyed.go), [service.go](../../internal/review/service.go) | `TestServiceSerializesJobsForTheSamePullRequest` | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven |
| Concurrent duplicate delivery has one winner | [cache.go](../../internal/queue/cache.go) | `TestDeliveryCacheConcurrentClaimHasOneWinner` | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven |
| Shutdown drains accepted jobs | [dispatcher.go](../../internal/queue/dispatcher.go), [app.go](../../internal/app/app.go) | `TestDispatcherShutdownDrainsAcceptedJobs` | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven |
| Head change before publication and before reconcile mutation | [service.go](../../internal/review/service.go), [service.go](../../internal/reconcile/service.go) | `TestServiceCancelsWhenHeadChangesBeforePublication`, `TestReconcileStopsMutationsWhenHeadChanges` | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven |
| One thread failure does not block other resolutions | [service.go](../../internal/reconcile/service.go) | `TestReconcileContinuesAfterOneThreadFailure` | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven |
| Full queue returns 503 and releases the claim | [handler.go](../../internal/app/handler.go) | `TestFullQueueReturns503AndReleasesClaim` | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven |

## Release, image, deployment, live proof

| Requirement | Go source | Focused test | Full test | Race test | CI evidence | Image evidence | Release evidence | Deployment evidence | Live GitHub evidence |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| Distroless nonroot image from release binaries | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven |
| Image has no Python, Node, shell, or second build | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven |
| GHCR amd64 and arm64 under one digest | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven |
| Immutable release attestation and provenance | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven |
| Canonical go-makefile install succeeds | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven |
| Cloudflare consumer pins the verified digest | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven |
| Worker `/health` and container `/` pass | [app.go](../../internal/app/app.go) | `TestHealthNoExternalCalls`, `TestRootStatusNoExternalCalls` | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven |
| GitHub App identity and secrets unchanged | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven |
| GitHub App permissions and events minimized | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven |
| Live PR proves one review per head | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven |
| Live duplicate delivery creates no duplicate | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven |
| Live correcting commit gets a new review | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven |
| Live old finding resolves with no reply | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven |
| Temporary validation PR cleaned up | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven |
| `agoodkind/pr-agent` archived | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven | Not yet proven |

Current consumer pin in `/Users/agoodkind/Sites/pr-agent-cf` is `pragent/pr-agent:0.42.0-github_app@sha256:e0447694...`, not a `pr-review-agent` digest. Latest GitHub release on trunk is `202608131751-2-65b3755`. `agoodkind/pr-agent` is not archived.
