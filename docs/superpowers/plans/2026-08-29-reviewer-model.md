# Reviewer Model and Durable Run Tracing Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Rebuild the review publication path on the reviewer model, so a run resolves its own threads, posts findings, keeps one top-level comment, and decides approve or request changes from open threads alone, with every run traceable end to end.

**Architecture:** Three GitHub objects each get one owner. A single issue comment, found by an HTML marker and updated in place, holds the summary and the run identifier. `COMMENTED` reviews hold inline findings. One verdict review holds `APPROVE` or `REQUEST_CHANGES` and is the only object branch protection reads. The check run reports whether the run finished, never the verdict. Every run mints one correlation identifier at admission that reaches all three objects and every log line.

**Tech Stack:** Go 1.26.6, `goodkind.io/gklog` with its `correlation` and `trace` subpackages, GitHub REST and GraphQL, OpenTelemetry via `gklog/trace`.

**Spec:** `docs/superpowers/specs/2026-08-29-reviewer-model-design.md`

## Global Constraints

- Go version is `1.26.6`, matching `go.mod`.
- `gklog` is already at the newest version. `go.mod` pins `goodkind.io/gklog v0.4.5-0.20260805222409-15e95d9fb619`, and `origin/main` in `~/Sites/gklog` is that same commit, `15e95d9`. Both `goodkind.io/gklog/correlation` and `goodkind.io/gklog/trace` exist there. No version bump is needed; the work is wiring, not upgrading.
- Run `make build` and `make test` before every commit. Never run `go build` or `go vet` directly; `agent-gate` blocks them.
- `make fmt` owns all whitespace. Run it before building.
- Never use an em dash or en dash anywhere, including code comments. `agent-gate` blocks them at PreToolUse.
- Comments explain why, not what. Match the density of the surrounding file.
- `exhaustruct` requires every struct literal field spelled out.
- The bot login is `service.botLogin`. The review marker helpers live in `internal/marker`.
- Sign every commit with `git commit -S`.

---

### Task 1: Update gklog and inject correlation identifiers into every log record

**Files:**
- Modify: `go.mod`, `go.sum`
- Modify: `cmd/pr-review-agent/main.go`
- Test: `internal/app/app_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces: every `slog` record written through a context carrying a correlation context gains `request_id`, `trace_id`, and `span_id` fields. Later tasks read the identifier with `trace.IDFromContext(ctx) string`.

- [ ] **Step 1: Confirm the dependency already carries the subpackages**

```bash
cd /Users/agoodkind/.worktrees/-Users-agoodkind-Sites-pr-review-agent/observability-stack
go list -m goodkind.io/gklog
go list goodkind.io/gklog/correlation goodkind.io/gklog/trace
```

Expected: the module resolves to `v0.4.5-0.20260805222409-15e95d9fb619` and both package paths list without error. Verified on 2026-08-29 against `origin/main` at `15e95d9`. Do not bump the version.

Importing `gklog/trace` pulls in OpenTelemetry, `net/http`, and `pgx/v5`. Run `go mod tidy` after the first import lands so `go.mod` records them.

- [ ] **Step 2: Write the failing test**

Add to `internal/app/app_test.go`:

```go
func TestEveryLogRecordCarriesTheRunIdentifiers(t *testing.T) {
	var captured []map[string]any
	handler := correlation.SlogHandler(
		slog.NewJSONHandler(io.Discard, nil),
		correlation.HandlerOptions{},
	)
	_ = handler

	ctx, corr := correlation.Ensure(context.Background(), "delivery-abc")
	if corr.TraceID == "" {
		t.Fatal("Ensure: want a minted trace id")
	}
	attrs := correlation.AttrsFromContext(ctx)
	if len(attrs) == 0 {
		t.Fatal("AttrsFromContext: want the correlation attributes")
	}
	keys := make(map[string]bool, len(attrs))
	for _, attribute := range attrs {
		keys[attribute.Key] = true
	}
	for _, want := range []string{"request_id", "trace_id"} {
		if !keys[want] {
			t.Fatalf("attributes = %v, want a %q field", keys, want)
		}
	}
	_ = captured
}
```

- [ ] **Step 3: Run the test to verify it fails**

Run: `go test ./internal/app/ -run TestEveryLogRecordCarriesTheRunIdentifiers -v`
Expected: FAIL to compile, because `correlation` is not imported anywhere yet.

- [ ] **Step 4: Add the correlation handler to the logger chain**

In `cmd/pr-review-agent/main.go`, find where the handler slice is built and passed to `gklog.New`. Wrap each handler:

```go
handlers := []slog.Handler{
	correlation.SlogHandler(gklog.StdoutJSON(level), correlation.HandlerOptions{}),
}
```

Add the import `"goodkind.io/gklog/correlation"`.

- [ ] **Step 5: Run the test to verify it passes**

Run: `make fmt && go test ./internal/app/ -run TestEveryLogRecordCarriesTheRunIdentifiers -v`
Expected: PASS

- [ ] **Step 6: Run the full gate and commit**

```bash
make build && make test
git add go.mod go.sum cmd/pr-review-agent/main.go internal/app/app_test.go
git commit -S -m "Update gklog and inject correlation identifiers into every log record

Wrap the stdout handler in correlation.SlogHandler so a record written
through a correlated context carries its request, trace, and span ids
with no new call sites.

Co-authored-by: Claude <noreply@anthropic.com>"
```

---

### Task 2: Mint one run identifier at webhook admission

**Files:**
- Modify: `internal/app/handler.go:115-160`
- Test: `internal/app/app_test.go`

**Interfaces:**
- Consumes: `correlation.SlogHandler` wiring from Task 1.
- Produces: every review run's context carries a correlation context keyed on the webhook delivery identifier. `trace.IDFromContext(ctx) string` returns that run's identifier from any point in the run.

- [ ] **Step 1: Write the failing test**

Add to `internal/app/app_test.go`:

```go
func TestWebhookAdmissionMintsOneRunIdentifierPerDelivery(t *testing.T) {
	fixture := newAppFixture(t)
	defer fixture.close()

	fixture.deliverSignedWebhook(t, "delivery-run-id", openedPullRequestPayload())

	logged := fixture.logLinesContaining("webhook delivery accepted")
	if len(logged) == 0 {
		t.Fatal("no accepted delivery line was logged")
	}
	if logged[0]["request_id"] != "delivery-run-id" {
		t.Fatalf("request_id = %v, want the delivery id", logged[0]["request_id"])
	}
	if logged[0]["trace_id"] == "" || logged[0]["trace_id"] == nil {
		t.Fatalf("trace_id = %v, want a minted trace id", logged[0]["trace_id"])
	}
}
```

If `logLinesContaining` does not exist on the fixture, add it. It reads the fixture's captured log output and returns each JSON line whose `msg` contains the argument, decoded into `map[string]any`.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/app/ -run TestWebhookAdmissionMintsOneRunIdentifier -v`
Expected: FAIL, `trace_id` absent, because nothing calls `correlation.Ensure`.

- [ ] **Step 3: Call Ensure at admission**

In `internal/app/handler.go`, inside `handleGitHubWebhook`, immediately after the delivery identifier is known and before the logger is built:

```go
ctx, _ := correlation.Ensure(request.Context(), deliveryID)
```

Then build the logger from that context so the correlation handler sees it, replacing the existing `ctx := gklog.WithLogger(request.Context(), logger)` line with one that starts from the correlated context.

- [ ] **Step 4: Run the test to verify it passes**

Run: `make fmt && go test ./internal/app/ -run TestWebhookAdmissionMintsOneRunIdentifier -v`
Expected: PASS

- [ ] **Step 5: Run the full gate and commit**

```bash
make build && make test
git add internal/app/
git commit -S -m "Mint one correlation identifier per webhook delivery

Call correlation.Ensure with the delivery id at admission so one
identifier covers the whole run and reaches every log line.

Co-authored-by: Claude <noreply@anthropic.com>"
```

---

### Task 3: Open a span for each step of the review run

**Files:**
- Modify: `internal/review/service.go:220-310`
- Test: `internal/review/review_test.go`

**Interfaces:**
- Consumes: the correlated context from Task 2.
- Produces: a span per named step. `trace.Op(ctx, name) func(err *error)` records the step's duration and error.

- [ ] **Step 1: Write the failing test**

Add to `internal/review/review_test.go`:

```go
func TestEachReviewStepRecordsItsOwnSpan(t *testing.T) {
	fixture := newServiceFixture(t, serviceFixtureOptions{})

	if err := fixture.run(context.Background(), fixture.job()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	want := []string{
		"review.reconcile",
		"review.analyze",
		"review.publish_findings",
		"review.update_summary_comment",
		"review.decide",
	}
	for _, name := range want {
		if !fixture.spanRecorded(name) {
			t.Fatalf("span %q was not recorded", name)
		}
	}
}
```

Add `spanRecorded(name string) bool` to the fixture. It scans captured log records for one whose `msg` equals the span name, which is what `trace.Op` writes when the operation closes.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/review/ -run TestEachReviewStepRecordsItsOwnSpan -v`
Expected: FAIL, no spans recorded.

- [ ] **Step 3: Wrap each step**

In `runLocked`, wrap each step. The pattern is:

```go
reconcileErr := error(nil)
done := trace.Op(ctx, "review.reconcile")
threads, reconcileErr := service.reconciler.Reconcile(ctx, job)
done(&reconcileErr)
if reconcileErr != nil {
	return service.failCheck(ctx, job, checkRun.ID, progress.summary(service.now()), checkFailureReconcile, reconcileErr)
}
```

Apply the same shape to analysis, finding publication, the summary comment update, and the decision.

- [ ] **Step 4: Run the test to verify it passes**

Run: `make fmt && go test ./internal/review/ -run TestEachReviewStepRecordsItsOwnSpan -v`
Expected: PASS

- [ ] **Step 5: Run the full gate and commit**

```bash
make build && make test
git add internal/review/
git commit -S -m "Open a span for each step of the review run

Wrap reconcile, analyze, publish, summary, and decide in trace.Op so a
slow or failed step is attributable rather than inferred.

Co-authored-by: Claude <noreply@anthropic.com>"
```

---

### Task 4: Add issue comment operations to the GitHub client

**Files:**
- Create: `internal/githubapp/comments.go`
- Test: `internal/githubapp/githubapp_test.go`

**Interfaces:**
- Consumes: `Client.doREST` and `Client.doRESTPaginated`, already present in `internal/githubapp/client.go`.
- Produces:
  - `func (client *Client) ListIssueComments(ctx context.Context, installationID int64, repo domain.Repository, number int) ([]IssueComment, error)`
  - `func (client *Client) CreateIssueComment(ctx context.Context, installationID int64, repo domain.Repository, number int, body string) (IssueComment, error)`
  - `func (client *Client) UpdateIssueComment(ctx context.Context, installationID int64, repo domain.Repository, commentID int64, body string) (IssueComment, error)`
  - `type IssueComment struct { ID int64; Author string; Body string }`

- [ ] **Step 1: Write the failing test**

Add to `internal/githubapp/githubapp_test.go`:

```go
func TestCreateAndUpdateIssueComment(t *testing.T) {
	var lastMethod, lastPath, lastBody string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		payload, _ := io.ReadAll(request.Body)
		lastMethod, lastPath, lastBody = request.Method, request.URL.Path, string(payload)
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"id": 4242, "body": "hello", "user": {"login": "bot"}}`))
	}))
	t.Cleanup(server.Close)

	client := newTestGitHubClient(t, server.URL)
	repo := domain.Repository{Owner: "owner", Name: "repo"}

	created, err := client.CreateIssueComment(context.Background(), 1, repo, 7, "hello")
	if err != nil {
		t.Fatalf("CreateIssueComment: %v", err)
	}
	if created.ID != 4242 {
		t.Fatalf("id = %d, want 4242", created.ID)
	}
	if lastMethod != "POST" || lastPath != "/repos/owner/repo/issues/7/comments" {
		t.Fatalf("request = %s %s, want POST the issue comments path", lastMethod, lastPath)
	}

	if _, err := client.UpdateIssueComment(context.Background(), 1, repo, 4242, "second"); err != nil {
		t.Fatalf("UpdateIssueComment: %v", err)
	}
	if lastMethod != "PATCH" || lastPath != "/repos/owner/repo/issues/comments/4242" {
		t.Fatalf("request = %s %s, want PATCH the single comment path", lastMethod, lastPath)
	}
	if !strings.Contains(lastBody, "second") {
		t.Fatalf("body = %q, want the new text", lastBody)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/githubapp/ -run TestCreateAndUpdateIssueComment -v`
Expected: FAIL to compile, `CreateIssueComment` undefined.

- [ ] **Step 3: Write the implementation**

Create `internal/githubapp/comments.go`:

```go
package githubapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"goodkind.io/pr-review-agent/internal/domain"
)

// IssueComment is one top level pull request comment, which GitHub models as an
// issue comment rather than a review comment.
type IssueComment struct {
	ID     int64
	Author string
	Body   string
}

type issueCommentResponse struct {
	ID   int64  `json:"id"`
	Body string `json:"body"`
	User struct {
		Login string `json:"login"`
	} `json:"user"`
}

type issueCommentBody struct {
	Body string `json:"body"`
}

// ListIssueComments returns every top level comment on one pull request.
func (client *Client) ListIssueComments(
	ctx context.Context,
	installationID int64,
	repo domain.Repository,
	number int,
) ([]IssueComment, error) {
	path := client.repoPath(repo, fmt.Sprintf("/issues/%d/comments", number))
	comments := make([]IssueComment, 0)
	err := client.doRESTPaginated(ctx, installationID, "GET", path, nil, func(page []byte) (int, error) {
		var decoded []issueCommentResponse
		if err := json.Unmarshal(page, &decoded); err != nil {
			client.logger.ErrorContext(ctx, "decode issue comments page", slog.String("err", err.Error()))
			return 0, errors.New("decode issue comments page")
		}
		for _, item := range decoded {
			comments = append(comments, IssueComment{
				ID:     item.ID,
				Author: item.User.Login,
				Body:   item.Body,
			})
		}
		return len(decoded), nil
	})
	if err != nil {
		return nil, err
	}
	return comments, nil
}

// CreateIssueComment posts one new top level comment.
func (client *Client) CreateIssueComment(
	ctx context.Context,
	installationID int64,
	repo domain.Repository,
	number int,
	body string,
) (IssueComment, error) {
	path := client.repoPath(repo, fmt.Sprintf("/issues/%d/comments", number))
	return client.writeIssueComment(ctx, installationID, "POST", path, body, "create")
}

// UpdateIssueComment replaces the body of one existing top level comment.
func (client *Client) UpdateIssueComment(
	ctx context.Context,
	installationID int64,
	repo domain.Repository,
	commentID int64,
	body string,
) (IssueComment, error) {
	path := client.repoPath(repo, fmt.Sprintf("/issues/comments/%d", commentID))
	return client.writeIssueComment(ctx, installationID, "PATCH", path, body, "update")
}

func (client *Client) writeIssueComment(
	ctx context.Context,
	installationID int64,
	method string,
	path string,
	body string,
	operation string,
) (IssueComment, error) {
	encoded, err := json.Marshal(issueCommentBody{Body: body})
	if err != nil {
		client.logger.ErrorContext(ctx, "marshal "+operation+" issue comment", slog.String("err", err.Error()))
		return IssueComment{ID: 0, Author: "", Body: ""}, errors.New("marshal " + operation + " issue comment")
	}
	responseBody, err := client.doREST(ctx, installationID, method, path, nil, encoded)
	if err != nil {
		return IssueComment{ID: 0, Author: "", Body: ""}, err
	}
	var decoded issueCommentResponse
	if err := json.Unmarshal(responseBody, &decoded); err != nil {
		client.logger.ErrorContext(ctx, "decode "+operation+" issue comment", slog.String("err", err.Error()))
		return IssueComment{ID: 0, Author: "", Body: ""}, errors.New("decode " + operation + " issue comment")
	}
	return IssueComment{
		ID:     decoded.ID,
		Author: decoded.User.Login,
		Body:   decoded.Body,
	}, nil
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `make fmt && go test ./internal/githubapp/ -run TestCreateAndUpdateIssueComment -v`
Expected: PASS

- [ ] **Step 5: Run the full gate and commit**

```bash
make build && make test
git add internal/githubapp/
git commit -S -m "Add issue comment list, create, and update to the GitHub client

The reviewer model keeps one top level comment per pull request, which
GitHub models as an issue comment rather than a review comment.

Co-authored-by: Claude <noreply@anthropic.com>"
```

---

### Task 5: Keep exactly one top-level comment, updated in place

**Files:**
- Create: `internal/review/summary_comment.go`
- Modify: `internal/marker/marker.go`
- Test: `internal/review/review_test.go`

**Interfaces:**
- Consumes: `githubapp.ListIssueComments`, `CreateIssueComment`, `UpdateIssueComment` from Task 4.
- Produces: `func (service *Service) upsertSummaryComment(ctx context.Context, job domain.ReviewJob, body string) error`, which finds the bot's marked comment and updates it, or creates it when absent. Also `marker.SummaryComment() string` returning the HTML marker, and `marker.HasSummaryComment(body string) bool`.

- [ ] **Step 1: Write the failing test**

```go
func TestTheSummaryCommentIsCreatedOnceThenUpdatedInPlace(t *testing.T) {
	fixture := newServiceFixture(t, serviceFixtureOptions{})

	if err := fixture.run(context.Background(), fixture.job()); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if len(fixture.state.issueComments) != 1 {
		t.Fatalf("issue comments = %d, want 1 after the first run", len(fixture.state.issueComments))
	}
	firstID := fixture.state.issueComments[0].id

	fixture.state.reviews = nil
	if err := fixture.run(context.Background(), fixture.job()); err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if len(fixture.state.issueComments) != 1 {
		t.Fatalf("issue comments = %d, want still 1 after the second run", len(fixture.state.issueComments))
	}
	if fixture.state.issueComments[0].id != firstID {
		t.Fatalf("comment id = %d, want the first comment %d updated in place", fixture.state.issueComments[0].id, firstID)
	}
	if fixture.state.issueCommentUpdates != 1 {
		t.Fatalf("updates = %d, want exactly one in place update", fixture.state.issueCommentUpdates)
	}
}
```

Extend `serviceServerState` with `issueComments []fixtureIssueComment` and `issueCommentUpdates int`, and add fixture routes for `POST /repos/{owner}/{repo}/issues/{n}/comments`, `GET` the same path, and `PATCH /repos/{owner}/{repo}/issues/comments/{id}`.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/review/ -run TestTheSummaryCommentIsCreatedOnce -v`
Expected: FAIL, zero issue comments, because nothing posts one.

- [ ] **Step 3: Add the marker**

In `internal/marker/marker.go`, alongside the existing summary and review markers:

```go
const summaryCommentMarker = "<!-- pr-review-agent:summary-comment:v1 -->"

// SummaryComment returns the marker identifying the one top level comment this
// service owns, so a later run finds and updates it rather than posting again.
func SummaryComment() string {
	return summaryCommentMarker
}

// HasSummaryComment reports whether one comment body carries that marker.
func HasSummaryComment(body string) bool {
	return strings.Contains(body, summaryCommentMarker)
}
```

- [ ] **Step 4: Write the upsert**

Create `internal/review/summary_comment.go`:

```go
package review

// This file keeps the one top level comment the service owns.
//
// A reader looking at a pull request should find the whole story in one place
// that never moves: what the last run decided, which model answered, and the
// identifier that reaches that run's logs. Scattering the summary across review
// bodies put the prose on one object and the verdict on another, and the two
// drifted apart.

import (
	"context"
	"fmt"
	"log/slog"

	"goodkind.io/gklog"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/marker"
)

// upsertSummaryComment writes the run's summary into the service's one top
// level comment, creating it only when this pull request has none.
func (service *Service) upsertSummaryComment(
	ctx context.Context,
	job domain.ReviewJob,
	body string,
) error {
	logger := gklog.L(ctx)
	comments, err := service.github.ListIssueComments(ctx, job.InstallationID, job.Repository, job.Number)
	if err != nil {
		logger.ErrorContext(ctx, "list issue comments", slog.String("err", err.Error()))
		return fmt.Errorf("list issue comments: %w", err)
	}

	for _, comment := range comments {
		if comment.Author != service.botLogin || !marker.HasSummaryComment(comment.Body) {
			continue
		}
		if _, err := service.github.UpdateIssueComment(
			ctx,
			job.InstallationID,
			job.Repository,
			comment.ID,
			body,
		); err != nil {
			logger.ErrorContext(ctx, "update summary comment", slog.String("err", err.Error()))
			return fmt.Errorf("update summary comment: %w", err)
		}
		logger.InfoContext(ctx, "summary comment updated", slog.Int64("comment_id", comment.ID))
		return nil
	}

	created, err := service.github.CreateIssueComment(
		ctx,
		job.InstallationID,
		job.Repository,
		job.Number,
		body,
	)
	if err != nil {
		logger.ErrorContext(ctx, "create summary comment", slog.String("err", err.Error()))
		return fmt.Errorf("create summary comment: %w", err)
	}
	logger.InfoContext(ctx, "summary comment created", slog.Int64("comment_id", created.ID))
	return nil
}
```

Add the three new methods to the `GitHub` interface in `internal/review/service.go`.

- [ ] **Step 5: Call it from the run and render the body with the run identifier**

In `runLocked`, after analysis and finding publication, call `service.upsertSummaryComment(ctx, job, RenderSummaryComment(summary, trace.IDFromContext(ctx)))`. Extend `RenderSummaryComment` in `internal/review/render.go` to lead with the marker and include a `Run ID` row in the details table.

- [ ] **Step 6: Run the test to verify it passes**

Run: `make fmt && go test ./internal/review/ -run TestTheSummaryCommentIsCreatedOnce -v`
Expected: PASS

- [ ] **Step 7: Run the full gate and commit**

```bash
make build && make test
git add internal/
git commit -S -m "Keep one top level comment carrying the summary and run id

Find the service's own comment by marker and update it in place, so the
summary lives on one durable object instead of drifting across review
bodies.

Co-authored-by: Claude <noreply@anthropic.com>"
```

---

### Task 6: Delete the publication capacity machinery

**Files:**
- Delete: `internal/review/stream.go`, `internal/review/stream_test.go`
- Modify: `internal/review/publication.go`, `internal/review/concurrency.go`, `internal/review/analyze.go`, `internal/review/service.go`
- Modify: `internal/config/config.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `publicationState` reduced to suppression only, with fields `historyIDs map[string]struct{}` and `historyAnchors map[string]struct{}`. `func (state *publicationState) suppressed(keys findingKeys) bool` and `func (state *publicationState) remember(keys findingKeys)` survive unchanged. The `capacity` and `hasTailSlot` fields, the `heldFinalist` type, the `candidate` overflow pool, `pending`, `rejected`, and `REVIEW_MAX_UNRESOLVED_COMMENTS` are gone.

- [ ] **Step 1: Write the failing test**

```go
func TestEveryEligibleFindingIsPublished(t *testing.T) {
	findings := make([]domain.Finding, 0, 12)
	for index := range 12 {
		findings = append(findings, domain.Finding{
			Path:       fmt.Sprintf("file%d.go", index),
			StartLine:  2,
			EndLine:    2,
			Title:      fmt.Sprintf("Defect %d", index),
			Body:       "A real defect on a changed line.",
			Importance: 8,
		})
	}
	fixture := newServiceFixture(t, serviceFixtureOptions{
		input: manyChunkInput(),
		model: &sequenceModel{results: []domain.ReviewResult{{
			CoverageComplete: true,
			Findings:         findings,
		}}},
	})

	if err := fixture.run(context.Background(), fixture.job()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(fixture.state.streamedComments) != len(findings) {
		t.Fatalf(
			"published = %d comments, want all %d: a reviewer does not ration comments",
			len(fixture.state.streamedComments), len(findings),
		)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/review/ -run TestEveryEligibleFindingIsPublished -v`
Expected: FAIL, fewer comments than findings, because the cap truncates.

- [ ] **Step 3: Delete the machinery**

```bash
git rm internal/review/stream_test.go
```

Rewrite `internal/review/stream.go` down to the sink's real job: render each admitted finding and post it. Remove `capacity`, `hasTailSlot`, `finalist`, `heldFinalist`, `overflow`, `pending`, `rejected`, `takeFinalist`, `takeOverflow`, `claimed`, `Finalize`, `outranks`, and `settle`'s three-way split. `Publish` becomes: filter by `suppressed`, dedupe within the batch, render, post, and `remember` what delivered.

Remove `maximumUnresolvedComments` from `Service` and `collectPublicationState`, and remove `REVIEW_MAX_UNRESOLVED_COMMENTS` from `internal/config/config.go` and its tests.

Remove the `sink.Finalize(ctx)` call from `runLocked`.

- [ ] **Step 4: Run the test to verify it passes**

Run: `make fmt && go test ./internal/review/ -run TestEveryEligibleFindingIsPublished -v`
Expected: PASS

- [ ] **Step 5: Run the full gate and commit**

```bash
make build && make test
git add -A internal/
git commit -S -m "Delete the publication capacity machinery

Remove the comment cap and everything built to serve it: the tail slot,
the overflow pool, the pending and rejected key sets, and their tests. A
reviewer does not ration comments.

Co-authored-by: Claude <noreply@anthropic.com>"
```

---

### Task 7: Decide from open threads and whether this run reviewed the head

**Files:**
- Modify: `internal/review/publication.go:23-53`
- Modify: `internal/review/service.go`
- Test: `internal/review/review_test.go`

**Interfaces:**
- Consumes: `hasUnresolvedBotThread` unchanged.
- Produces: `func reviewerDecision(ctx context.Context, threads []githubapp.ReviewThread, botLogin string, reviewedCurrentHead bool) domain.ReviewDecision`, replacing `standingDecision`. It returns `domain.ReviewDecisionApprove` only when no bot thread is open and `reviewedCurrentHead` is true.

- [ ] **Step 1: Write the failing test**

```go
func TestApprovalRequiresBothNoOpenThreadAndACurrentHeadReview(t *testing.T) {
	openThread := []githubapp.ReviewThread{{
		NodeID:      "thread-1",
		Resolved:    false,
		RootComment: domain.ReviewComment{Author: testBotLogin, Body: "an open finding"},
	}}
	resolvedThread := []githubapp.ReviewThread{{
		NodeID:      "thread-1",
		Resolved:    true,
		RootComment: domain.ReviewComment{Author: testBotLogin, Body: "a fixed finding"},
	}}

	cases := []struct {
		name        string
		threads     []githubapp.ReviewThread
		reviewedNow bool
		want        domain.ReviewDecision
	}{
		{"open thread blocks", openThread, true, domain.ReviewDecisionRequestChanges},
		{"stale review blocks", resolvedThread, false, domain.ReviewDecisionRequestChanges},
		{"clean and current approves", resolvedThread, true, domain.ReviewDecisionApprove},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := reviewerDecision(context.Background(), testCase.threads, testBotLogin, testCase.reviewedNow)
			if got != testCase.want {
				t.Fatalf("decision = %q, want %q", got, testCase.want)
			}
		})
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/review/ -run TestApprovalRequiresBoth -v`
Expected: FAIL to compile, `reviewerDecision` undefined.

- [ ] **Step 3: Write the implementation**

Replace `standingDecision` in `internal/review/publication.go`:

```go
// reviewerDecision reports what this run stands behind.
//
// A reviewer approves when nothing they raised is still open and they have
// actually read the code in front of them. Dropping the second condition is how
// a verdict from an older head kept blocking a pull request whose fix had
// already landed.
func reviewerDecision(
	ctx context.Context,
	threads []githubapp.ReviewThread,
	botLogin string,
	reviewedCurrentHead bool,
) domain.ReviewDecision {
	if hasUnresolvedBotThread(threads, botLogin) {
		return domain.ReviewDecisionRequestChanges
	}
	if !reviewedCurrentHead {
		gklog.L(ctx).InfoContext(
			ctx,
			"verdict withheld",
			slog.String("reason", "this run did not review the current head"),
		)
		return domain.ReviewDecisionRequestChanges
	}
	return domain.ReviewDecisionApprove
}
```

Update the call site in `publish` to pass `true` for `reviewedCurrentHead`, since reaching `publish` means analysis ran against the refreshed head. Delete `standingDecision` and the now-unused `published` parameter threading.

- [ ] **Step 4: Run the test to verify it passes**

Run: `make fmt && go test ./internal/review/ -run TestApprovalRequiresBoth -v`
Expected: PASS

- [ ] **Step 5: Run the full gate and commit**

```bash
make build && make test
git add internal/review/
git commit -S -m "Decide from open threads and a current head review

Approve only when no thread the service raised is open and this run read
the current head, which is the condition a stale blocking verdict was
missing.

Co-authored-by: Claude <noreply@anthropic.com>"
```

---

### Task 8: Stop a failed run from touching review state

**Files:**
- Modify: `internal/review/notice.go`
- Modify: `internal/review/service.go`
- Test: `internal/review/review_test.go`

**Interfaces:**
- Consumes: `upsertSummaryComment` from Task 5.
- Produces: `clearFailedReviewState` is deleted. A failed run calls `upsertSummaryComment` with a failure body and completes the check run. No review is submitted, updated, or dismissed.

- [ ] **Step 1: Write the failing test**

```go
func TestAFailedRunChangesNoReviewState(t *testing.T) {
	fixture := newServiceFixture(t, serviceFixtureOptions{
		reviewPages: botReviewPage("CHANGES_REQUESTED"),
		model:       &failingModel{err: errors.New("provider exploded")},
	})

	if err := fixture.run(context.Background(), fixture.job()); err == nil {
		t.Fatal("Run: want the model failure")
	}

	if len(fixture.state.dismissals) != 0 {
		t.Fatalf("dismissals = %v, want none: a failure is not a judgment", fixture.state.dismissals)
	}
	if fixture.state.lastSubmitReview != nil {
		t.Fatalf("submitted review = %v, want none", fixture.state.lastSubmitReview)
	}
	if len(fixture.state.issueComments) != 1 {
		t.Fatalf("issue comments = %d, want the failure written to the one summary comment", len(fixture.state.issueComments))
	}
	if !strings.Contains(fixture.state.issueComments[0].body, "provider exploded") {
		t.Fatalf("summary comment = %q, want the real cause", fixture.state.issueComments[0].body)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/review/ -run TestAFailedRunChangesNoReviewState -v`
Expected: FAIL, dismissals present.

- [ ] **Step 3: Rewrite the failure path**

In `internal/review/notice.go`, delete `clearFailedReviewState`, `dismissStaleVerdicts`, `listReviewsForFailure`, `standingVerdict`, `dismissalMessageFor`, `approvalDismissalMessage`, and `blockDismissalMessage`. Replace `publishFailureNotice` with a call to `upsertSummaryComment` carrying `RenderFailureBody`. Remove `DismissReview` from the `GitHub` interface if nothing else uses it.

- [ ] **Step 4: Run the test to verify it passes**

Run: `make fmt && go test ./internal/review/ -run TestAFailedRunChangesNoReviewState -v`
Expected: PASS

- [ ] **Step 5: Run the full gate and commit**

```bash
make build && make test
git add internal/review/
git commit -S -m "Stop a failed run from touching review state

A run that cannot finish writes its cause into the summary comment and
turns the check red. It submits, updates, and dismisses no review, so an
outage can never block a person.

Co-authored-by: Claude <noreply@anthropic.com>"
```

---

### Task 9: Put the run identifier on the check run with a resolving details link

**Files:**
- Modify: `internal/review/service.go`
- Modify: `internal/githubapp/checks.go`
- Test: `internal/review/review_test.go`

**Interfaces:**
- Consumes: `trace.IDFromContext` from Task 2.
- Produces: `CompleteCheckRunRequest` gains a `DetailsURL string` field. Every completed check run carries the run identifier in its output text and a details link built from `service.runLogBaseURL`.

- [ ] **Step 1: Write the failing test**

```go
func TestTheCheckRunCarriesTheRunIdentifierAndAResolvingLink(t *testing.T) {
	fixture := newServiceFixture(t, serviceFixtureOptions{})

	if err := fixture.run(context.Background(), fixture.job()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	completed := fixture.state.lastCheckUpdate
	if completed == nil {
		t.Fatal("no check run was completed")
	}
	details, _ := completed["details_url"].(string)
	if details == "" || !strings.Contains(details, "run=") {
		t.Fatalf("details_url = %q, want a link carrying the run id", details)
	}
	output, _ := completed["output"].(map[string]any)
	text, _ := output["text"].(string)
	if !strings.Contains(text, "Run ID") {
		t.Fatalf("check text = %q, want the run id", text)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/review/ -run TestTheCheckRunCarriesTheRunIdentifier -v`
Expected: FAIL, `details_url` empty.

- [ ] **Step 3: Write the implementation**

Add `DetailsURL` to the check completion payload in `internal/githubapp/checks.go`. In `internal/review/service.go`, add a `runLogBaseURL string` field to `Service`, read from a new `REVIEW_RUN_LOG_BASE_URL` config value, and set `DetailsURL` to that base with `?run=<identifier>` appended. Include a `Run ID` row in `RenderDetails`.

- [ ] **Step 4: Run the test to verify it passes**

Run: `make fmt && go test ./internal/review/ -run TestTheCheckRunCarriesTheRunIdentifier -v`
Expected: PASS

- [ ] **Step 5: Run the full gate and commit**

```bash
make build && make test
git add internal/
git commit -S -m "Put the run identifier and a resolving link on the check run

A failed check pointed at the repository root with no run to open, so
nobody could tell why it failed without querying Cloudflare.

Co-authored-by: Claude <noreply@anthropic.com>"
```

---

### Task 10: Lock the six invariants

**Files:**
- Create: `internal/review/invariants_test.go`

**Interfaces:**
- Consumes: everything above.
- Produces: one test per invariant from the spec.

- [ ] **Step 1: Write the tests**

One test each, named for the invariant:

1. `TestAReviewAttachesToTheHeadItAnalyzed`
2. `TestANewPushProducesAFreshDecision`
3. `TestRetryingOneHeadDoesNotDuplicateADecision`
4. `TestApprovalRequiresNoOpenThreadAndACurrentHeadReview` (delegates to Task 7's table)
5. `TestAFailedRunChangesNoReviewState` (move from Task 8)
6. `TestEveryRunIdentifierInAPullRequestResolvesToItsLogs`

For invariant 6, assert that the identifier in the summary comment body, the identifier in the check run output, and the identifier on the run's log records are the same string.

- [ ] **Step 2: Run them and fix what fails**

Run: `go test ./internal/review/ -run 'Test(AReview|ANewPush|TestRetrying|Approval|AFailed|EveryRunIdentifier)' -v`
Expected: PASS. Any failure is a real gap in Tasks 1 through 9; fix the implementation, not the test.

- [ ] **Step 3: Run the full gate and commit**

```bash
make build && make test
git add internal/review/invariants_test.go
git commit -S -m "Lock the six reviewer invariants

Five of these were stated in the original design and never encoded, and
all of them have since broken in production.

Co-authored-by: Claude <noreply@anthropic.com>"
```

---

### Task 11: Update the operations documentation

**Files:**
- Modify: `docs/operations.md`

- [ ] **Step 1: Rewrite the review lifecycle section**

Describe the six step loop, the three objects and their owners, and how to take a run identifier from a pull request to that run's logs. Remove any description of the comment cap and of failure-time review dismissal. Follow the writing rules in `~/.claude/rules/writing.md`: behavior before implementation, one durable home per fact, no filename in prose unless the reader must open that file.

- [ ] **Step 2: Commit**

```bash
git add docs/operations.md
git commit -S -m "Document the reviewer model and the run identifier trail

Co-authored-by: Claude <noreply@anthropic.com>"
```

---

## Self-Review

**Spec coverage.** Failure A is Task 7. B is Task 5. C is Task 8. D is Task 7's `reviewedCurrentHead` condition. E is already shipped in pull requests 69 through 75. F is Task 9. The tracing requirements are Tasks 1, 2, 3, and 9. The six invariants are Task 10. The deletion is Task 6.

**Placeholders.** None. Every code step carries the code.

**Type consistency.** `IssueComment` is defined in Task 4 and consumed in Task 5. `reviewerDecision` is defined in Task 7 and referenced in Task 10. `upsertSummaryComment` is defined in Task 5 and reused in Task 8. `trace.IDFromContext` is introduced in Task 2 and used in Tasks 5 and 9.

**One gap worth naming.** Task 9 adds `REVIEW_RUN_LOG_BASE_URL`. That value must point at something that actually serves a run's logs. If the Worker has no such route, Task 9 needs a preceding task to add one, and the plan should be extended rather than the link left pointing nowhere.
