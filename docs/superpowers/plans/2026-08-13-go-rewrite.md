# Pure Go PR Review Core Graphite Stack Plan

> **For code-writing agents:** REQUIRED SKILLS: Use split-to-prs, then graphite. Execute every task and publish the five dependent pull requests defined below. Steps use checkbox syntax for tracking.

**Goal:** Replace the active PR-Agent review path with a small Go service that reviews every new pull request head, publishes one complete GitHub review, and silently resolves earlier bot findings fixed by later commits.

**Architecture:** Build one standard-library Go process with strict configuration, signed GitHub webhook ingestion, a bounded single-worker queue, a GitHub App client, a direct Clyde client, deterministic review policy, and silent thread reconciliation. Persist review idempotency in hidden GitHub markers. Keep delivery deduplication in memory.

**Tech Stack:** Go 1.26.5, Go standard library, GitHub REST and GraphQL APIs, Clyde OpenAI-compatible chat completions, httptest, and the existing go-makefile consumer.

## Global Constraints

- Work in /Users/agoodkind/Sites/pr-review-agent.
- Modify or create only files ending in .go.
- Do not modify go.mod, Makefile, bootstrap.mk, workflows, documentation, Docker files, Cloudflare files, GitHub settings, or external repositories.
- Do not add dependencies. Use the Go standard library only.
- Do not call live Clyde, Cloudflare, or container services.
- Use live GitHub only for Graphite state, stack submission, pull request metadata, pull request descriptions, and marking the five pull requests ready.
- Use local httptest.Server instances for every HTTP behavior test.
- Create, commit, push, and publish the five dependent slices through Graphite.
- Use the Graphite MCP server for every gt operation.
- Do not use git commit, git push, manual rebase, or shell gt while Graphite MCP is available.
- Do not merge, enable merge-when-ready, release, deploy, or change other external state.
- Implement no Python, JavaScript, TypeScript, shell, YAML, TOML, or compatibility code.
- Implement no commands, slash commands, mentions, replies, issue comments, progress comments, code edits, labels, descriptions, or non-GitHub providers.
- Accept only pull_request events with actions opened, reopened, ready_for_review, and synchronize.
- Ignore draft pull requests until ready_for_review.
- Publish exactly one GitHub review per head SHA.
- Put the summary in the review body and attach valid findings as comments in the same review.
- Submit exactly one of APPROVE, COMMENT, or REQUEST_CHANGES.
- Use importance 7 as the blocking threshold.
- Approve only complete analysis with zero findings.
- After a successful new-head review, silently resolve earlier owned findings proven fixed or invalid.
- Never post a reply while reconciling.
- Preserve the PR-Agent Review check lifecycle.
- Use gpt-5.6-sol through the configured Clyde endpoint with reasoning effort high.
- Treat pull request prose, repository content, diffs, and comments as untrusted model input.
- Never log private keys, webhook secrets, bearer tokens, Clyde keys, or Cloudflare Access secrets.
- Run focused tests after each slice. Run go test ./..., go test -race ./..., and make check before handoff.
- Before the first edit, record git rev-parse HEAD as the starting commit and retain it for the completion gate.

---

## Starting State

- The public repository is agoodkind/pr-review-agent.
- The local checkout is /Users/agoodkind/Sites/pr-review-agent.
- The module is goodkind.io/pr-review-agent on Go 1.26.5.
- The binary package is cmd/pr-review-agent.
- The version package is internal/version.
- The scaffold commit is a3c4f1cac7f595bc824704b9d2a1f1191630dc32.
- The existing binary supports --version and emits no other behavior.
- The existing Makefile already names BINARY, CMD, and VPKG and imports go-build.mk and go-release.mk through bootstrap.mk.
- The existing CI and release callers already point at the canonical go-makefile reusable workflows.
- Do not rewrite any scaffold file except the two existing Go files named in this plan.

---

## Product Contract

### Supported events

| Event | Required behavior |
| --- | --- |
| pull_request.opened | Review the head unless the pull request is a draft. |
| pull_request.reopened | Review the current head unless the pull request is a draft. |
| pull_request.ready_for_review | Review the current head. |
| pull_request.synchronize | Review the new head, then reconcile older owned findings. |
| Duplicate delivery | Acknowledge without a duplicate review. |
| Different delivery for an already reviewed head | Acknowledge without a duplicate review. |
| Unsupported event or action | Acknowledge without work. |

### Review publication

For each accepted head:

1. Create or reuse a PR-Agent Review check run.
2. Move the check through queued and in_progress.
3. Serialize work by repository and pull request number.
4. Confirm the pull request still points at the queued head.
5. Load metadata, changed files, patches, and current file context.
6. Analyze every available changed hunk.
7. Validate every model field and inline anchor.
8. Confirm the pull request still points at the analyzed head.
9. Submit one POST /repos/{owner}/{repo}/pulls/{number}/reviews request.
10. Put the summary and review marker in the review body.
11. Attach valid findings through the request comments array.
12. Put valid findings without a current right-side anchor in the review body under Unanchored findings.
13. Complete the lifecycle check only after GitHub accepts the review.
14. Reconcile earlier unresolved owned findings without changing the successful check result.

### Decision policy

- REQUEST_CHANGES when any validated finding has importance 7 through 10.
- COMMENT when findings exist and every importance is 1 through 6.
- COMMENT when coverage is incomplete.
- APPROVE only when coverage is complete and no findings exist.
- Invalid model output fails the review.
- A stale head cancels publication and creates no review.

### Writing policy

Keep this exact value in one Go constant and inject it into every review prompt:

    State the finding first. Use short sentences with active verbs. Keep one idea per sentence and paragraph. Name exact identifiers when relevant. Explain the concrete trigger and impact. Omit praise, introductions, filler, repetition, generic advice, closing summaries, and typographic dashes. Stop after the actionable information.

The model controls prose only. Go code controls coverage, validation, ownership, resolution, and the final review event.

### Durable markers

Use these exact formats:

    <!-- pr-review-agent:review:v1 head=<lowercase hexadecimal head> -->
    <!-- pr-review-agent:finding:v1 head=<lowercase hexadecimal head> id=<64 lowercase hexadecimal characters> -->

Accept head SHAs containing exactly 40 or 64 lowercase hexadecimal characters.

Calculate a finding ID as SHA-256 over this canonical sequence:

1. Head length as 8 lowercase hexadecimal digits, then head bytes.
2. Path length as 8 lowercase hexadecimal digits, then path bytes.
3. Start line as 16 lowercase hexadecimal digits.
4. End line as 16 lowercase hexadecimal digits.
5. Importance as 2 lowercase hexadecimal digits.
6. Title length as 8 lowercase hexadecimal digits, then title bytes.
7. Body length as 8 lowercase hexadecimal digits, then body bytes.

Normalize paths before hashing. Convert backslashes to slashes. Clean dot segments. Reject absolute paths and traversal. Trim title and body. Do not otherwise rewrite marker input.

---

## File Map

Create these focused Go files:

    cmd/pr-review-agent/main.go
    cmd/pr-review-agent/main_test.go
    internal/config/config.go
    internal/config/config_test.go
    internal/domain/review.go
    internal/domain/review_test.go
    internal/marker/marker.go
    internal/marker/marker_test.go
    internal/webhook/webhook.go
    internal/webhook/webhook_test.go
    internal/queue/cache.go
    internal/queue/keyed.go
    internal/queue/dispatcher.go
    internal/queue/queue_test.go
    internal/githubapp/auth.go
    internal/githubapp/client.go
    internal/githubapp/pulls.go
    internal/githubapp/reviews.go
    internal/githubapp/checks.go
    internal/githubapp/threads.go
    internal/githubapp/client_test.go
    internal/diff/patch.go
    internal/diff/collect.go
    internal/diff/diff_test.go
    internal/clyde/schema.go
    internal/clyde/client.go
    internal/clyde/client_test.go
    internal/review/policy.go
    internal/review/render.go
    internal/review/analyze.go
    internal/review/service.go
    internal/review/review_test.go
    internal/reconcile/service.go
    internal/reconcile/service_test.go
    internal/app/app.go
    internal/app/handler.go
    internal/app/app_test.go

Do not introduce a generic framework. Keep interfaces beside their consumers.

---

## Graphite Stack Contract

These slices are dependent because each upper layer imports the lower Go packages. Build this exact stack from bottom to top:

| Position | Pull request title | Tasks | Parent |
| --- | --- | --- | --- |
| 1 | Add Go review intake contracts | 1 through 3 | main |
| 2 | Add GitHub review data pipeline | 4 through 6 | Pull request 1 branch |
| 3 | Add Clyde review analysis | 7 and 8 | Pull request 2 branch |
| 4 | Add review lifecycle reconciliation | 9 and 10 | Pull request 3 branch |
| 5 | Add PR review service runtime | 11 and 12 | Pull request 4 branch |

Each pull request must pass the repository checks at its stack position. Keep each task's tests in the same commit as its behavior.

### Graphite preflight

- [ ] Work from /Users/agoodkind/Sites/pr-review-agent. Do not create another checkout.
- [ ] Run git status --short and record all existing changes.
- [ ] Save a recoverable snapshot under refs/backup before moving work.
- [ ] Run git fetch origin before comparisons.
- [ ] Run git worktree list and record which checkout owns main.
- [ ] Run gh pr list for the current branch and record any open pull request.
- [ ] Stop if an open pull request already covers this rewrite. It requires a human reuse decision before code writing begins.
- [ ] Run Graphite MCP with args ["state", "--no-interactive"] and cwd /Users/agoodkind/Sites/pr-review-agent.
- [ ] If state does not identify main as trunk, run args ["init", "--trunk", "main", "--no-interactive"], authenticate if required, then rerun state.
- [ ] Run Graphite MCP with args ["log", "short"] and confirm no conflicting active stack contains this work.
- [ ] Confirm origin/main equals the intended stack base.
- [ ] Record git rev-parse HEAD as starting_commit.

For every Graphite call, pass cwd /Users/agoodkind/Sites/pr-review-agent and a one-line purpose. Stage only the exact files named by the task. Let the active harness apply its configured commit identity, signing, and Git flow.

---

### Task 1: Add Domain Types and Markers

**Files:**

- Create internal/domain/review.go and review_test.go.
- Create internal/marker/marker.go and marker_test.go.

**Required domain interface:**

    type HeadSHA string
    func ParseHeadSHA(string) (HeadSHA, error)

    type Repository struct {
        Owner string
        Name  string
    }

    type PullRequestRef struct {
        Repository     Repository
        Number         int
        InstallationID int64
        Head           HeadSHA
    }

    func (PullRequestRef) Key() string

    type ReviewDecision string
    const ReviewDecisionApprove ReviewDecision = "APPROVE"
    const ReviewDecisionComment ReviewDecision = "COMMENT"
    const ReviewDecisionRequestChanges ReviewDecision = "REQUEST_CHANGES"
    func ParseReviewDecision(string) (ReviewDecision, error)

    type Resolution string
    const ResolutionResolved Resolution = "resolved"
    const ResolutionOpen Resolution = "open"
    const ResolutionUncertain Resolution = "uncertain"
    func ParseResolution(string) (Resolution, error)

    type Finding struct {
        Path       string `json:"path"`
        StartLine  int    `json:"start_line"`
        EndLine    int    `json:"end_line"`
        Title      string `json:"title"`
        Body       string `json:"body"`
        Importance int    `json:"importance"`
    }

    func (Finding) Validate() error

    type ReviewResult struct {
        Summary          string    `json:"summary"`
        CoverageComplete bool      `json:"coverage_complete"`
        Findings         []Finding `json:"findings"`
    }

    func (ReviewResult) Validate() error

    type ReviewJob struct {
        DeliveryID string
        PullRequestRef
    }

    type ReviewComment struct {
        DatabaseID int64
        Author     string
        Body       string
        Path       string
        StartLine  int
        EndLine    int
    }

    type OwnedThread struct {
        NodeID      string
        RootComment ReviewComment
        Finding     Finding
        FindingHead HeadSHA
    }

    type ThreadResolution struct {
        ThreadNodeID string     `json:"thread_node_id"`
        Resolution   Resolution `json:"resolution"`
        Reason       string     `json:"reason"`
    }

**Required marker interface:**

    type FindingMarker struct {
        Head domain.HeadSHA
        ID   string
    }

    func Review(domain.HeadSHA) string
    func FindReview(string) (domain.HeadSHA, bool)
    func Finding(domain.HeadSHA, domain.Finding) (string, error)
    func FindFinding(string) (FindingMarker, bool)
    func EncodeFindingBody(domain.HeadSHA, domain.Finding) (string, error)
    func DecodeFindingBody(domain.ReviewComment) (domain.HeadSHA, domain.Finding, error)
    func NormalizePath(string) (string, error)

Encode inline bodies in this exact form:

    **<trimmed title>**

    <trimmed body>

    Importance: <base-10 integer>

    <!-- pr-review-agent:finding:v1 head=<head> id=<id> -->

DecodeFindingBody must use Path, StartLine, and EndLine from the GitHub root comment, parse the exact title, body, importance, and marker format, reconstruct the Finding, and recompute the marker ID. Reject the body when the recomputed ID differs.

- [ ] Write TestParseHeadSHAAcceptsSHA1AndSHA256.
- [ ] Write TestParseHeadSHARejectsUppercaseNonhexadecimalAndWrongLength.
- [ ] Write TestParseReviewDecisionRejectsUnknownValue.
- [ ] Write TestParseResolutionRejectsUnknownValue.
- [ ] Write TestFindingValidateRejectsEmptyFieldsInvalidLinesAndImportance.
- [ ] Write TestReviewResultValidateRejectsDuplicateFindings.
- [ ] Run go test ./internal/domain -count=1 and observe failure.
- [ ] Implement the domain types with explicit enum switches and concrete errors.
- [ ] Write TestReviewMarkerRoundTrip.
- [ ] Write TestFindingMarkerChangesWithHead.
- [ ] Write TestFindingMarkerIsStableForEquivalentPaths.
- [ ] Write TestFindingMarkerRejectsUnsafePaths.
- [ ] Write TestMarkerParserRejectsMalformedValues.
- [ ] Write TestFindingBodyRoundTripAndHashVerification.
- [ ] Run go test ./internal/marker -count=1 and observe failure.
- [ ] Implement marker hashing and exact anchored parsing.
- [ ] Run gofmt -w internal/domain/*.go internal/marker/*.go.
- [ ] Run go test ./internal/domain ./internal/marker -count=1.
- [ ] Start pull request 1 through Graphite MCP:

    git add internal/domain/review.go internal/domain/review_test.go internal/marker/marker.go internal/marker/marker_test.go
    args: ["create", "--message", "Add review domain models and markers"]

---

### Task 2: Add Strict Configuration

**Files:**

- Create internal/config/config.go and config_test.go.

**Required interface:**

    const Model = "gpt-5.6-sol"
    const ReasoningEffort = "high"
    const BotLogin = "agoodkind-pr-review-agent[bot]"
    const ReviewCheckName = "PR-Agent Review"
    const BlockingImportance = 7
    const ReviewTimeout = 600 * time.Second
    const QueueCapacity = 100
    const DeliveryCacheCapacity = 10000
    const DeliveryCacheTTL = 24 * time.Hour
    const MaximumWebhookBytes = 2 * 1024 * 1024
    const MaximumPromptBytes = 80000
    const MaximumOutputTokens = 8000
    const GitHubAPIVersion = "2022-11-28"
    const WritingPolicy = "State the finding first. Use short sentences with active verbs. Keep one idea per sentence and paragraph. Name exact identifiers when relevant. Explain the concrete trigger and impact. Omit praise, introductions, filler, repetition, generic advice, closing summaries, and typographic dashes. Stop after the actionable information."

    type LookupEnv func(string) (string, bool)

    type Config struct {
        Port                 string
        GitHubAppID          int64
        GitHubPrivateKey     *rsa.PrivateKey
        GitHubWebhookSecret  []byte
        GitHubBotLogin       string
        GitHubAPIBaseURL     *url.URL
        GitHubGraphQLURL     *url.URL
        ClydeBaseURL         *url.URL
        ClydeAPIKey          string
        CFAccessClientID     string
        CFAccessClientSecret string
    }

    func Load(LookupEnv) (Config, error)
    func FromEnvironment() (Config, error)

Production keys are PORT, GITHUB_APP_ID, GITHUB_PRIVATE_KEY, GITHUB_WEBHOOK_SECRET, GITHUB_BOT_LOGIN, CLYDE_BASE_URL, CLYDE_API_KEY, CF_ACCESS_CLIENT_ID, and CF_ACCESS_CLIENT_SECRET.

Default PORT to 3000. Default GITHUB_BOT_LOGIN to the bot constant. Default GitHub URLs to api.github.com. Keep custom GitHub URLs constructible in tests, but do not load production override variables.

- [ ] Write TestLoadRequiresEverySecret.
- [ ] Write TestLoadParsesPKCS1AndPKCS8RSAKeys.
- [ ] Write TestLoadRejectsNonRSAAndMalformedKeys.
- [ ] Write TestLoadUsesPortBotAndGitHubDefaults.
- [ ] Write TestLoadRejectsInvalidURLsAndAppIDs.
- [ ] Write TestLoadErrorsDoNotContainSecretValues.
- [ ] Run go test ./internal/config -count=1 and observe failure.
- [ ] Implement strict loading and RSA parsing.
- [ ] Run gofmt -w internal/config/*.go.
- [ ] Run go test ./internal/config -count=1.
- [ ] Add the task commit to pull request 1 through Graphite MCP:

    git add internal/config/config.go internal/config/config_test.go
    args: ["modify", "--commit", "--message", "Add strict service configuration"]

---

### Task 3: Add Signed Webhook Parsing

**Files:**

- Create internal/webhook/webhook.go and webhook_test.go.

**Required interface:**

    var ErrInvalidSignature = errors.New("invalid webhook signature")

    type PullRequestEvent struct {
        Action         string
        DeliveryID     string
        InstallationID int64
        Repository     domain.Repository
        Number         int
        Head           domain.HeadSHA
        Draft          bool
    }

    func VerifySHA256(string, []byte, []byte) error
    func ParsePullRequest(string, string, []byte) (PullRequestEvent, bool, error)
    func (PullRequestEvent) Job() domain.ReviewJob

ParsePullRequest returns supported=false and error=nil for non-pull_request events, unsupported actions, and draft events other than ready_for_review. Allow unrelated GitHub payload fields. Reject missing or invalid consumed fields.

- [ ] Write TestVerifySHA256AcceptsValidSignature.
- [ ] Write TestVerifySHA256RejectsMissingMalformedAndWrongSignatures.
- [ ] Write TestParsePullRequestAcceptsFourSupportedActions.
- [ ] Write TestParsePullRequestIgnoresDraftAndUnsupportedEvents.
- [ ] Write TestParsePullRequestRejectsMissingRequiredFields.
- [ ] Run go test ./internal/webhook -count=1 and observe failure.
- [ ] Implement HMAC SHA-256 verification with hmac.Equal.
- [ ] Implement concrete payload structs.
- [ ] Run gofmt -w internal/webhook/*.go.
- [ ] Run go test ./internal/webhook -count=1.
- [ ] Add the task commit to pull request 1 through Graphite MCP:

    git add internal/webhook/webhook.go internal/webhook/webhook_test.go
    args: ["modify", "--commit", "--message", "Add signed pull request webhook parsing"]

---

### Task 4: Add Bounded Deduplication and Serialization

**Files:**

- Create internal/queue/cache.go, keyed.go, dispatcher.go, and queue_test.go.

**Required interface:**

    type Clock func() time.Time

    func NewDeliveryCache(int, time.Duration, Clock) *DeliveryCache
    func (*DeliveryCache) Claim(string) bool
    func (*DeliveryCache) Release(string)

    func NewKeyedLocker() *KeyedLocker
    func (*KeyedLocker) Lock(string) func()

    type Runner interface {
        Run(context.Context, domain.ReviewJob) error
    }

    func NewDispatcher(int, Runner, *slog.Logger) *Dispatcher
    func (*Dispatcher) Start(context.Context)
    func (*Dispatcher) Enqueue(domain.ReviewJob) bool
    func (*Dispatcher) Shutdown(context.Context) error

Use one worker. Enqueue is nonblocking. Evict expired delivery records first, then the oldest record at capacity. Release a claim only when enqueue fails. Keep claims after runner failure.

- [ ] Write TestDeliveryCacheClaimsOnceUntilExpiry.
- [ ] Write TestDeliveryCacheEvictsOldestAtCapacity.
- [ ] Write TestDeliveryCacheConcurrentClaimHasOneWinner.
- [ ] Write TestKeyedLockerSerializesSameKeyAndAllowsDifferentKeys.
- [ ] Write TestDispatcherRejectsWhenFull.
- [ ] Write TestDispatcherRunsJobsInOrder.
- [ ] Write TestDispatcherShutdownDrainsAcceptedJobs.
- [ ] Run go test -race ./internal/queue -count=1 and observe failure.
- [ ] Implement with mutexes, a buffered channel, and sync.WaitGroup.
- [ ] Run gofmt -w internal/queue/*.go.
- [ ] Run go test -race ./internal/queue -count=1.
- [ ] Start pull request 2 through Graphite MCP:

    git add internal/queue/cache.go internal/queue/keyed.go internal/queue/dispatcher.go internal/queue/queue_test.go
    args: ["create", "--message", "Add bounded review job serialization"]

---

### Task 5: Add the GitHub App Client

**Files:**

- Create internal/githubapp/auth.go, client.go, pulls.go, reviews.go, checks.go, threads.go, and client_test.go.

**Required construction:**

    func NewClient(config.Config, *http.Client, func() time.Time, *slog.Logger) *Client

**Required data types:**

    type PullRequest struct {
        Number int
        Head   domain.HeadSHA
        Base   domain.HeadSHA
        Draft  bool
        Title  string
        Body   string
    }

    type ChangedFile struct {
        Path         string
        PreviousPath string
        Status       string
        Patch        string
        PatchPresent bool
    }

    type Review struct {
        ID       int64
        CommitID domain.HeadSHA
        Author   string
        Body     string
        State    string
    }

    type InlineComment struct {
        Path      string `json:"path"`
        Body      string `json:"body"`
        Line      int    `json:"line"`
        Side      string `json:"side"`
        StartLine int    `json:"start_line,omitempty"`
        StartSide string `json:"start_side,omitempty"`
    }

    type SubmitReviewRequest struct {
        CommitID domain.HeadSHA
        Body     string
        Event    domain.ReviewDecision
        Comments []InlineComment
    }

    type CheckRun struct {
        ID         int64
        Name       string
        Head       domain.HeadSHA
        Status     string
        Conclusion string
    }

    type ReviewThread struct {
        NodeID      string
        Resolved    bool
        RootComment domain.ReviewComment
    }

**Required methods:**

    GetPullRequest
    ListChangedFiles
    GetFile
    Compare
    ListReviews
    SubmitReview
    FindCheckRun
    CreateCheckRun
    StartCheckRun
    CompleteCheckRun
    ListReviewThreads
    ResolveReviewThread

Each method accepts context, installation ID, repository, and its operation-specific identifiers.

GitHub App JWT rules:

- RS256.
- iat equals current time minus 60 seconds.
- exp equals current time plus 9 minutes.
- iss is the decimal App ID.
- Cache installation tokens by installation ID.
- Refresh five minutes before expiration.

HTTP rules:

- Set Accept: application/vnd.github+json.
- Set X-GitHub-Api-Version: 2022-11-28.
- Cap responses at 16 MiB.
- Sanitize errors.
- Follow REST Link pagination.
- Page GraphQL threads 100 at a time.
- Load only the root comment for ownership, including database ID, author login, body, path, start line, and end line.
- Verify resolveReviewThread returns the requested ID and isResolved=true.

Implement only these write endpoints:

    POST  /repos/{owner}/{repo}/pulls/{number}/reviews
    POST  /repos/{owner}/{repo}/check-runs
    PATCH /repos/{owner}/{repo}/check-runs/{id}
    POST  /graphql for resolveReviewThread

- [ ] Write TestAppJWTContainsExpectedClaimsAndValidSignature.
- [ ] Write TestInstallationTokenIsCachedAndRefreshedBeforeExpiry.
- [ ] Write TestClientErrorsExposeStatusWithoutCredentials.
- [ ] Write TestRESTPaginationLoadsEveryPage.
- [ ] Write TestListChangedFilesPreservesMissingPatch.
- [ ] Write TestGetFileDecodesBase64Content.
- [ ] Write TestSubmitReviewSendsOneCompletePayload.
- [ ] Write TestListReviewsPreservesAuthorForMarkerOwnership.
- [ ] Write TestCheckRunLifecycleUsesExpectedPayloads.
- [ ] Write TestListReviewThreadsPaginatesBeyondOneHundred.
- [ ] Write TestResolveReviewThreadVerifiesMutationResult.
- [ ] Write TestGraphQLErrorsRemainVisible.
- [ ] Make the fake server fail on issue-comment and reply endpoints.
- [ ] Run go test ./internal/githubapp -count=1 and observe failure.
- [ ] Implement the JWT, token cache, transport, REST operations, and GraphQL operations.
- [ ] Run gofmt -w internal/githubapp/*.go.
- [ ] Run go test ./internal/githubapp -count=1.
- [ ] Add the task commit to pull request 2 through Graphite MCP:

    git add internal/githubapp/auth.go internal/githubapp/client.go internal/githubapp/pulls.go internal/githubapp/reviews.go internal/githubapp/checks.go internal/githubapp/threads.go internal/githubapp/client_test.go
    args: ["modify", "--commit", "--message", "Add GitHub App review client"]

---

### Task 6: Add Diff Collection and Chunking

**Files:**

- Create internal/diff/patch.go, collect.go, and diff_test.go.

**Required interface:**

    type FileContext struct {
        Path              string
        Status            string
        Patch             string
        CurrentContent    string
        ChangedRightLines map[int]struct{}
        CoverageComplete  bool
    }

    type ReviewInput struct {
        PullRequest githubapp.PullRequest
        Files       []FileContext
    }

    type Chunk struct {
        Index            int
        Total            int
        Text             string
        Paths            []string
        CoverageComplete bool
    }

    type Source interface {
        ListChangedFiles(context.Context, int64, domain.Repository, int) ([]githubapp.ChangedFile, error)
        GetFile(context.Context, int64, domain.Repository, string, domain.HeadSHA) ([]byte, error)
    }

    type Collector struct {
        source Source
    }

    func ChangedRightLines(string) (map[int]struct{}, error)
    func ValidRange(map[int]struct{}, int, int) bool
    func NewCollector(Source) *Collector
    func (*Collector) Collect(context.Context, domain.PullRequestRef, githubapp.PullRequest) (ReviewInput, error)
    func ChunkInput(ReviewInput, int) ([]Chunk, error)

Rules:

- Parse every unified hunk header.
- Record added right-side lines.
- Exclude deleted and context lines as finding end anchors.
- Require a multiline range to stay within one right-side hunk.
- Mark missing patches, binary files, malformed hunks, truncated hunks, and failed required content reads as incomplete coverage.
- Deleted files do not require current content.
- Split only between files or complete hunks.
- Preserve deterministic path order.
- Represent an oversized single hunk with a small incomplete-coverage metadata chunk.

- [ ] Write parser tests for multiple hunks, deletion, context, malformed headers, and multiline ranges.
- [ ] Write collection tests for modified, renamed, deleted, binary, and missing-patch files.
- [ ] Write chunk tests proving each complete hunk appears once and oversized hunks force incomplete coverage.
- [ ] Run go test ./internal/diff -count=1 and observe failure.
- [ ] Implement the parser with explicit old and new line counters.
- [ ] Implement content collection and deterministic chunks.
- [ ] Run gofmt -w internal/diff/*.go.
- [ ] Run go test ./internal/diff -count=1.
- [ ] Add the task commit to pull request 2 through Graphite MCP:

    git add internal/diff/patch.go internal/diff/collect.go internal/diff/diff_test.go
    args: ["modify", "--commit", "--message", "Add pull request diff collection"]

---

### Task 7: Add the Direct Clyde Client

**Files:**

- Create internal/clyde/schema.go, client.go, and client_test.go.

**Required interface:**

    func NewClient(config.Config, *http.Client, func(context.Context, time.Duration) error) *Client
    func (*Client) Review(context.Context, string) (domain.ReviewResult, error)
    func (*Client) Reconcile(context.Context, string) ([]domain.ThreadResolution, error)

Send POST {CLYDE_BASE_URL}/chat/completions with:

- model gpt-5.6-sol.
- reasoning_effort high.
- max_completion_tokens 8000.
- A system message containing the writing and untrusted-input policies.
- A user message containing the delimited input.
- response_format type json_schema.
- Strict review or reconciliation schema.
- additionalProperties=false at every object level.

Set Authorization, Content-Type, CF-Access-Client-ID, and CF-Access-Client-Secret headers.

Retry connection errors, HTTP 429, and HTTP 5xx three times after 1, 2, and 4 seconds. Do not retry other 4xx responses or invalid structured output. Decode choices[0].message.content with DisallowUnknownFields and require end of input.

- [ ] Write TestReviewSendsExactModelHeadersPolicyAndSchema.
- [ ] Write TestReviewRejectsUnknownFieldsAndInvalidFindings.
- [ ] Write TestReviewRetriesTransientFailuresThreeTimes.
- [ ] Write TestReviewDoesNotRetryAuthenticationFailure.
- [ ] Write TestReviewErrorsDoNotContainCredentials.
- [ ] Write TestReconcileAcceptsOnlyKnownResolutionValues.
- [ ] Write TestReconcileRejectsDuplicateThreadIDs.
- [ ] Run go test ./internal/clyde -count=1 and observe failure.
- [ ] Implement strict requests, schemas, retries, and decoding.
- [ ] Run gofmt -w internal/clyde/*.go.
- [ ] Run go test ./internal/clyde -count=1.
- [ ] Start pull request 3 through Graphite MCP:

    git add internal/clyde/schema.go internal/clyde/client.go internal/clyde/client_test.go
    args: ["create", "--message", "Add structured Clyde review client"]

---

### Task 8: Add Review Policy, Analysis, and Rendering

**Files:**

- Create internal/review/policy.go, render.go, analyze.go, and review_test.go.

**Required interface:**

    type Model interface {
        Review(context.Context, string) (domain.ReviewResult, error)
    }

    type Analysis struct {
        Summary          string
        CoverageComplete bool
        Anchored         []domain.Finding
        Unanchored       []domain.Finding
        Decision         domain.ReviewDecision
    }

    func DecisionFor(bool, []domain.Finding) domain.ReviewDecision
    func Analyze(context.Context, Model, diff.ReviewInput) (Analysis, error)
    func RenderBody(domain.HeadSHA, Analysis) string
    func RenderInline(domain.HeadSHA, []domain.Finding) ([]githubapp.InlineComment, error)

Rules:

- Analyze chunks in deterministic order.
- Fail on any invalid model result.
- Join nonempty chunk summaries with blank lines.
- Deduplicate exact normalized findings.
- Aggregate incomplete coverage.
- Anchor only valid normalized paths and right-side ranges.
- Render invalid anchors in the review body under Unanchored findings.
- Replace typographic dash characters in model prose with semicolons.
- Append one review marker to the review body.
- Append one finding marker to each inline finding body.
- Do not add praise, instructions, commands, or a closing summary.

- [ ] Write tests for all three decisions.
- [ ] Prove incomplete coverage never approves.
- [ ] Prove the review body contains summary, unanchored findings, and one marker.
- [ ] Prove inline payloads contain right-side ranges and finding markers.
- [ ] Prove rendered prose has no typographic dashes.
- [ ] Prove analysis aggregates chunks, deduplicates findings, and classifies bad anchors.
- [ ] Prove prompts inject the writing policy and mark repository input untrusted.
- [ ] Run go test ./internal/review -count=1 and observe failure.
- [ ] Implement deterministic analysis and rendering.
- [ ] Run gofmt -w internal/review/*.go.
- [ ] Run go test ./internal/review -count=1.
- [ ] Add the task commit to pull request 3 through Graphite MCP:

    git add internal/review/policy.go internal/review/render.go internal/review/analyze.go internal/review/review_test.go
    args: ["modify", "--commit", "--message", "Add deterministic review analysis"]

---

### Task 9: Add Lifecycle and Complete Review Publication

**Files:**

- Create internal/review/service.go.
- Extend internal/review/review_test.go.

**Required interfaces:**

    type GitHub interface {
        GetPullRequest(context.Context, int64, domain.Repository, int) (githubapp.PullRequest, error)
        ListReviews(context.Context, int64, domain.Repository, int) ([]githubapp.Review, error)
        FindCheckRun(context.Context, int64, domain.Repository, domain.HeadSHA, string) (githubapp.CheckRun, bool, error)
        CreateCheckRun(context.Context, int64, domain.Repository, domain.HeadSHA, string) (githubapp.CheckRun, error)
        StartCheckRun(context.Context, int64, domain.Repository, int64, string) error
        CompleteCheckRun(context.Context, int64, domain.Repository, int64, string, string) error
        SubmitReview(context.Context, int64, domain.Repository, int, githubapp.SubmitReviewRequest) (githubapp.Review, error)
    }

    type Collector interface {
        Collect(context.Context, domain.PullRequestRef, githubapp.PullRequest) (diff.ReviewInput, error)
    }

    type Reconciler interface {
        Reconcile(context.Context, domain.ReviewJob) error
    }

    func NewService(GitHub, Collector, Model, Reconciler, *queue.KeyedLocker, string, *slog.Logger) *Service
    func (*Service) Run(context.Context, domain.ReviewJob) error

Use a 600-second deadline. Treat an existing review marker as durable idempotency only when the review author equals the configured bot login. Complete the check with success after publication or confirmed existing same-head bot review. Use failure for authentication, read, model, validation, and publication failures. Use cancelled for a stale head. Reconciliation errors remain visible in logs but cannot change successful review state.

- [ ] Write TestServicePublishesOneCompleteReviewAndCompletesCheck.
- [ ] Write TestServiceSkipsHeadWithExistingReviewMarker.
- [ ] Write TestServiceIgnoresForeignReviewMarker.
- [ ] Write TestServiceCancelsWhenHeadChangesBeforePublication.
- [ ] Write TestServiceFailsCheckWhenReviewPublicationFails.
- [ ] Write TestServiceKeepsSuccessWhenReconciliationFails.
- [ ] Write TestServiceSerializesJobsForTheSamePullRequest.
- [ ] Use a real httptest GitHub boundary and assert exact request order.
- [ ] Run go test ./internal/review -run TestService -count=1 and observe failure.
- [ ] Implement the state machine. Acquire the keyed lock before durable marker reads. Recheck the head after analysis.
- [ ] Run gofmt -w internal/review/*.go.
- [ ] Run go test -race ./internal/review -count=1.
- [ ] Start pull request 4 through Graphite MCP:

    git add internal/review/service.go internal/review/review_test.go
    args: ["create", "--message", "Add complete GitHub review publication"]

---

### Task 10: Add Silent Finding Reconciliation

**Files:**

- Create internal/reconcile/service.go and service_test.go.

**Required interface:**

    type GitHub interface {
        ListReviewThreads(context.Context, int64, domain.Repository, int) ([]githubapp.ReviewThread, error)
        GetFile(context.Context, int64, domain.Repository, string, domain.HeadSHA) ([]byte, error)
        Compare(context.Context, int64, domain.Repository, domain.HeadSHA, domain.HeadSHA) ([]githubapp.ChangedFile, error)
        GetPullRequest(context.Context, int64, domain.Repository, int) (githubapp.PullRequest, error)
        ResolveReviewThread(context.Context, int64, string) error
    }

    type Model interface {
        Reconcile(context.Context, string) ([]domain.ThreadResolution, error)
    }

    func NewService(GitHub, Model, string, *slog.Logger) *Service
    func (*Service) Reconcile(context.Context, domain.ReviewJob) error

Ownership rules:

- The thread is unresolved.
- Root author equals the configured bot login.
- Root body contains one valid finding marker.
- Marker head differs from the current head.
- Ignore foreign authors, malformed markers, deleted roots, and current-head findings.

Resolution rules:

- Load current code for the normalized path.
- Compare the finding head with the new head.
- Missing or ambiguous context remains uncertain.
- Recheck the exact current head after model completion.
- Resolve only resolved results.
- Never call a reply endpoint.
- Continue independent threads after one failure.
- Return an aggregate error after processing so failures remain visible.

- [ ] Write TestReconcileSelectsOnlyUnresolvedOwnedMarkedThreads.
- [ ] Write TestReconcileIgnoresForeignMalformedAndCurrentHeadThreads.
- [ ] Write TestReconcileResolvesOnlyProvenFixedOrInvalidFindings.
- [ ] Write TestReconcileLeavesOpenAndUncertainFindingsOpen.
- [ ] Write TestReconcilePostsNoReplies.
- [ ] Write TestReconcileContinuesAfterOneThreadFailure.
- [ ] Write TestReconcileStopsMutationsWhenHeadChanges.
- [ ] Write TestReconcileLeavesMissingContextOpen.
- [ ] Run go test ./internal/reconcile -count=1 and observe failure.
- [ ] Implement deterministic batches under 80,000 bytes.
- [ ] Run gofmt -w internal/reconcile/*.go.
- [ ] Run go test ./internal/reconcile -count=1.
- [ ] Add the task commit to pull request 4 through Graphite MCP:

    git add internal/reconcile/service.go internal/reconcile/service_test.go
    args: ["modify", "--commit", "--message", "Add silent finding reconciliation"]

---

### Task 11: Add the HTTP Application and Entrypoint

**Files:**

- Create internal/app/app.go, handler.go, and app_test.go.
- Modify cmd/pr-review-agent/main.go and main_test.go.

**Required interface:**

    func New(config.Config, *http.Client, *http.Client, *slog.Logger) *App
    func (*App) Handler() http.Handler
    func (*App) Start(context.Context)
    func (*App) Shutdown(context.Context) error

Routes:

    GET  /                         200 {"status":"ok"}
    GET  /health                   200 {"status":"ok"}
    POST /api/v1/github_webhooks   signed webhook ingestion

Responses:

- 401 for invalid signature.
- 400 for malformed supported payloads or missing required headers.
- 413 above 2 MiB.
- 202 for accepted, duplicate, ignored, or unsupported deliveries.
- 503 when the queue is full.
- 405 for wrong methods on known paths.
- 404 for unknown paths.

The first HTTP client passed to New is for GitHub. The second is for Clyde. Production main constructs a GitHub client with a 30-second timeout and a Clyde client with a 610-second timeout.

The process must keep --version, reject other arguments with exit 2, load configuration before listening, emit JSON logs, listen on :PORT, handle SIGINT and SIGTERM, stop HTTP admission, and drain accepted work for at most 30 seconds. Configure the inbound http.Server with ReadHeaderTimeout 5 seconds, ReadTimeout 15 seconds, WriteTimeout 15 seconds, IdleTimeout 60 seconds, and MaxHeaderBytes 1 MiB.

- [ ] Write health tests proving no external call.
- [ ] Write signed webhook acceptance tests.
- [ ] Write invalid signature, malformed input, oversized body, unsupported event, duplicate, and full queue tests.
- [ ] Write TestSignedWebhookProducesOneReviewCheckAndSilentReconciliation.
- [ ] The end-to-end test must simulate an opened defective head, one REQUEST_CHANGES review, one inline finding, duplicate replay, a corrected synchronize head, one APPROVE review, and one silent old-thread resolution.
- [ ] The fake GitHub server must fail on issue-comment and reply endpoints.
- [ ] Run go test ./internal/app ./cmd/pr-review-agent -count=1 and observe failure.
- [ ] Wire concrete GitHub, Clyde, diff, review, reconciliation, queue, and HTTP values.
- [ ] Implement signal and server lifecycle in testable functions.
- [ ] Run gofmt -w internal/app/*.go cmd/pr-review-agent/*.go.
- [ ] Run go test -race ./internal/app ./cmd/pr-review-agent -count=1.
- [ ] Start pull request 5 through Graphite MCP:

    git add internal/app/app.go internal/app/handler.go internal/app/app_test.go cmd/pr-review-agent/main.go cmd/pr-review-agent/main_test.go
    args: ["create", "--message", "Add PR review webhook service"]

---

### Task 12: Complete Go-Only Behavior Coverage

**Files:**

- Modify only existing test Go files created by this plan.

Add these public-boundary scenarios:

- APPROVE with complete coverage.
- COMMENT with low-importance findings.
- COMMENT with incomplete coverage.
- REQUEST_CHANGES with a blocking finding.
- Stale head with no review.
- GitHub failure with failed lifecycle.
- Clyde failure with failed lifecycle.
- Concurrent duplicate delivery with one review.
- Replay after a fresh App instance with durable review-marker deduplication.
- More than 100 files and more than 100 threads.
- No issue-comment or reply endpoint under any scenario.
- Reconciliation failure isolation.
- No typographic dashes in published model prose.

- [ ] Add one named end-to-end test per scenario.
- [ ] Run go test ./internal/domain ./internal/marker ./internal/config ./internal/webhook ./internal/queue ./internal/githubapp ./internal/diff ./internal/clyde ./internal/review ./internal/reconcile ./internal/app ./cmd/pr-review-agent -count=1.
- [ ] Run go test ./... -count=1.
- [ ] Run go test -race ./... -count=1.
- [ ] Run make check.
- [ ] Fix only Go files if any gate fails.
- [ ] Add the task commit to pull request 5 through Graphite MCP:

    git add cmd/pr-review-agent/main_test.go internal/domain/review_test.go internal/marker/marker_test.go internal/config/config_test.go internal/webhook/webhook_test.go internal/queue/queue_test.go internal/githubapp/client_test.go internal/diff/diff_test.go internal/clyde/client_test.go internal/review/review_test.go internal/reconcile/service_test.go internal/app/app_test.go
    args: ["modify", "--commit", "--message", "Add end-to-end Go review coverage"]

---

## Submit the Graphite Stack

- [ ] Run go test ./... -count=1, go test -race ./... -count=1, and make check from the stack tip.
- [ ] Run Graphite MCP with args ["log", "short"] and confirm the five-branch parent chain matches the stack contract.
- [ ] Run Graphite MCP with args ["submit", "--stack", "--dry-run", "--no-interactive"].
- [ ] Stop if the dry run includes an unexpected branch, parent, or pull request.
- [ ] Run Graphite MCP with args ["submit", "--stack", "--no-interactive"].
- [ ] Visit each branch from bottom to top and apply the pr skill to set the exact title from the stack contract and a concise body describing only that slice.
- [ ] Read the five pull request numbers from bottom to top.
- [ ] If the numbers are non-sequential, append (PR x/5) to every title through the pr skill. Leave sequential titles unchanged.
- [ ] Mark all five pull requests ready for review.
- [ ] Run Graphite MCP with args ["log", "short"] and confirm the published parent chain.
- [ ] Confirm each pull request base equals its immediate parent branch.
- [ ] Report the bottom pull request URL and the full bottom-to-top stack order.

Do not merge, enable merge-when-ready, babysit checks, alter implementation after submission, or perform Plan 2 work.

---

## Plan 1 Completion Gate

Before handoff:

- [ ] git diff --name-only "$starting_commit"..HEAD lists only .go files.
- [ ] Every planned package exists.
- [ ] go test ./... -count=1 passes.
- [ ] go test -race ./... -count=1 passes.
- [ ] make check passes.
- [ ] No source contains an issue-comment write endpoint.
- [ ] No source contains a review-comment reply endpoint.
- [ ] No source handles issue_comment or pull_request_review_comment.
- [ ] No source implements commands, mentions, replies, or compatibility flags.
- [ ] One local end-to-end test proves publication and silent reconciliation.
- [ ] Graphite shows exactly five dependent branches in the specified order.
- [ ] All five branches are pushed.
- [ ] All five pull requests exist and are ready for review.
- [ ] Every pull request has the correct parent branch, title, and Go-only slice.
- [ ] The stack tip passes all local gates.

Stop here. Do not merge, create images, modify workflows, modify infrastructure, release, deploy, change GitHub App settings, create live product-validation pull requests, or collect production evidence. Those actions belong only to Plan 2.
