# Durable Incremental Review Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Rebuild the review run around remembered state on the pull request, so an oversized delta is declined, a normal one is reviewed in checkpointed chunks that survive death, and the verdict is recomputed from current state on every run.

**Architecture:** All durable state lives on the pull request: one top level issue comment carries an HTML state marker with `last_reviewed`, the run identifier, status, and the pending chunk list. Each invocation reviews the delta since `last_reviewed`, posts findings per chunk, and advances the marker after each chunk. The verdict is a pure function of the service's own open threads plus whether the head is fully reviewed, submitted only when the pending list is empty. A failed run writes its cause into the comment and the check, and touches no review object. The Worker keeps a pending set in Durable Object storage and a cron trigger re-invokes the service when a container died mid review.

**Tech Stack:** Go 1.26.6, `goodkind.io/gklog` (already pinned, `correlation` subpackage), GitHub REST and GraphQL, Cloudflare Worker with `@cloudflare/containers` 0.3.7, `node --test` for Worker tests.

**Spec:** `docs/superpowers/specs/2026-08-29-durable-review-design.md`

## Global Constraints

- Go version is `1.26.6`, matching `go.mod`. Do not change it.
- Run `make fmt`, then `make build`, then `make test` before every commit. Never run `go build` or `go vet` directly; agent-gate blocks them. `go test` and `go doc` are fine.
- agent-gate also blocks `grep`, `sed`, `awk`, `cat`, `head` against repo files in shell commands. Use the Read and Grep tools.
- Never use an em dash or en dash anywhere, including comments and commit messages. agent-gate blocks the write.
- `exhaustruct` requires every struct literal field spelled out, including zero values.
- Comments explain why, not what. Match the density of the surrounding file.
- Sign every commit with `git commit -S`.
- Test packages: `internal/review/review_test.go` is `package review_test`. `internal/review/stream_test.go` is `package review` (it is deleted in Task 7). `internal/app` tests are `package app`. `internal/githubapp/client_test.go` is `package githubapp_test`. A test of an unexported symbol needs an internal test file, for example `internal/review/state_internal_test.go` declared `package review`.
- Worker tests run with `cd deploy/cloudflare && npm test` (`node --test test/*.test.js`). `npm run check` adds a wrangler dry run.
- The bot login lives in `service.botLogin`. The run identifier is `job.DeliveryID`.
- The spec is the binding authority. Where this plan and the spec disagree, the spec wins.

## File map

| File | Responsibility after this plan |
| --- | --- |
| `internal/marker/state.go` (new) | Encode and decode the state marker |
| `internal/githubapp/comments.go` (new) | Issue comment list, create, update |
| `internal/review/summary_comment.go` (new) | Find or create the one top level comment, update in place |
| `internal/review/admission.go` (new) | Measure the delta, decide review or skip |
| `internal/review/delta.go` (new) | Compute the file set to review from marker state |
| `internal/review/run.go` (new) | The checkpointed chunk loop |
| `internal/review/publication.go` | `reviewerDecision` pure function |
| `internal/review/service.go` | Orchestration, thinned |
| `internal/review/stream.go`, `stream_test.go` | Deleted |
| `internal/review/notice.go` | Failure written to comment and check only |
| `internal/config/config.go` | `ReviewMaxFiles`, `ReviewMaxChunks`; `MaximumUnresolvedComments` removed |
| `internal/app/handler.go` | Correlation at admission; `/api/v1/continue` route |
| `deploy/cloudflare/worker/pending.js` (new) | Signed pending set endpoints |
| `deploy/cloudflare/worker/index.js` | Pending storage methods on the container class; `scheduled` handler |
| `deploy/cloudflare/wrangler.jsonc` | Cron trigger; new vars; stale var removed |

---

### Task 1: Land the correlation wiring already in the worktree

**Files:**
- Modify (already modified, uncommitted): `cmd/pr-review-agent/main.go`, `internal/app/handler.go`
- Test: `internal/app/app_test.go`

**Interfaces:**
- Consumes: `goodkind.io/gklog/correlation`, already in `go.mod` at the pinned version.
- Produces: every log record written through a webhook's context carries `request_id` (the delivery id) and a minted `trace_id`. Later tasks treat `job.DeliveryID` as the run identifier.

The worktree holds an unreviewed 19 line diff from an earlier session: `correlation.SlogHandler` wrapped around the stdout handler in `main.go`, and `correlation.Ensure(request.Context(), deliveryID)` in `handler.go`. Your job is to verify it, prove it with a test, and commit it, or revert it and redo it if it is broken.

- [ ] **Step 1: Read the uncommitted diff**

Run: `git diff cmd/pr-review-agent/main.go internal/app/handler.go`
Confirm: the handler chain wraps stdout in `correlation.SlogHandler(gklog.StdoutJSON(level), correlation.HandlerOptions{})` and the webhook path calls `correlation.Ensure` with the `X-GitHub-Delivery` value before the logger is attached to the context.

- [ ] **Step 2: Write the failing test (if the diff does not already include one)**

Add to `internal/app/app_test.go` (`package app`). If the fixture has no captured-log reader, add `logLinesContaining(substring string) []map[string]any` that decodes each captured JSON log line and returns those whose `msg` contains the substring.

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

Adapt fixture and helper names to what `internal/app/app_test.go` actually contains; read it first.

- [ ] **Step 3: Prove the test bites**

Temporarily remove the `correlation.Ensure` call, run `go test ./internal/app/ -run TestWebhookAdmissionMintsOneRunIdentifier -v`, confirm FAIL with `trace_id` absent, restore the call, confirm PASS. Record both outputs as RED and GREEN evidence.

- [ ] **Step 4: Gate and commit**

```bash
make fmt && make build && make test
git add cmd/pr-review-agent/main.go internal/app/ go.mod go.sum
git commit -S -m "Mint one correlation identifier per webhook delivery

Wrap the stdout handler in correlation.SlogHandler and call
correlation.Ensure with the delivery id at admission, so every log
record a run writes carries its run identifier.

Co-authored-by: Claude <noreply@anthropic.com>"
```

---

### Task 2: Issue comment operations on the GitHub client

**Files:**
- Create: `internal/githubapp/comments.go`
- Test: `internal/githubapp/client_test.go`

**Interfaces:**
- Consumes: `Client.doREST` and `Client.doRESTPaginated`, present in `internal/githubapp/client.go`.
- Produces:
  - `type IssueComment struct { ID int64; Author string; Body string }`
  - `func (client *Client) ListIssueComments(ctx context.Context, installationID int64, repo domain.Repository, number int) ([]IssueComment, error)`
  - `func (client *Client) CreateIssueComment(ctx context.Context, installationID int64, repo domain.Repository, number int, body string) (IssueComment, error)`
  - `func (client *Client) UpdateIssueComment(ctx context.Context, installationID int64, repo domain.Repository, commentID int64, body string) (IssueComment, error)`

- [ ] **Step 1: Write the failing test**

Add to `internal/githubapp/client_test.go` (`package githubapp_test`), using that file's existing test server construction helpers. Read the file first and reuse its client constructor.

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

If `newTestGitHubClient` does not exist under that name, use whatever constructor the file's other tests use, adapted to point at `server.URL`.

- [ ] **Step 2: Run to verify it fails**

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

// IssueComment is one top level pull request comment, which GitHub models as
// an issue comment rather than a review comment.
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

- [ ] **Step 4: Run to verify it passes**

Run: `make fmt && go test ./internal/githubapp/ -run TestCreateAndUpdateIssueComment -v`
Expected: PASS

- [ ] **Step 5: Gate and commit**

```bash
make build && make test
git add internal/githubapp/
git commit -S -m "Add issue comment list, create, and update to the GitHub client

The durable review keeps its state and summary on one top level
comment, which GitHub models as an issue comment.

Co-authored-by: Claude <noreply@anthropic.com>"
```

---

### Task 3: The state marker

**Files:**
- Create: `internal/marker/state.go`
- Test: `internal/marker/state_test.go` (`package marker_test`, matching the package's existing test style; read `internal/marker/marker_test.go` first if it exists, otherwise follow `marker.go` conventions)

**Interfaces:**
- Consumes: `domain.HeadSHA`.
- Produces:
  - `type State struct { LastReviewed domain.HeadSHA; RunID string; Status string; Pending []string }`
  - Status constants: `StateReviewing = "reviewing"`, `StateDone = "done"`, `StateSkipped = "skipped"`, `StateFailed = "failed"`
  - `func EncodeState(state State) string` rendering one line: `<!-- pr-review-agent:state:v1 last_reviewed=<sha> run=<id> status=<status> pending=<id1,id2> -->`
  - `func DecodeState(body string) (State, bool)` finding that line anywhere in a comment body
  - `func HasState(body string) bool`

- [ ] **Step 1: Write the failing test**

```go
func TestStateMarkerRoundTrip(t *testing.T) {
	original := marker.State{
		LastReviewed: domain.HeadSHA("a3c4f1cac7f595bc824704b9d2a1f1191630dc32"),
		RunID:        "delivery-abc",
		Status:       marker.StateReviewing,
		Pending:      []string{"0011aabbccdd", "2233eeff0011"},
	}

	body := "## Review\n\nsummary prose\n\n" + marker.EncodeState(original) + "\n"

	decoded, ok := marker.DecodeState(body)
	if !ok {
		t.Fatal("DecodeState: marker not found")
	}
	if decoded.LastReviewed != original.LastReviewed {
		t.Fatalf("last reviewed = %q, want %q", decoded.LastReviewed, original.LastReviewed)
	}
	if decoded.RunID != original.RunID || decoded.Status != original.Status {
		t.Fatalf("run/status = %q/%q, want %q/%q", decoded.RunID, decoded.Status, original.RunID, original.Status)
	}
	if len(decoded.Pending) != 2 || decoded.Pending[0] != "0011aabbccdd" {
		t.Fatalf("pending = %v, want the two chunk ids", decoded.Pending)
	}
}

func TestStateMarkerWithNoPendingDecodesEmpty(t *testing.T) {
	body := marker.EncodeState(marker.State{
		LastReviewed: domain.HeadSHA("a3c4f1cac7f595bc824704b9d2a1f1191630dc32"),
		RunID:        "delivery-abc",
		Status:       marker.StateDone,
		Pending:      nil,
	})
	decoded, ok := marker.DecodeState(body)
	if !ok || len(decoded.Pending) != 0 {
		t.Fatalf("decoded = %+v ok=%v, want ok with empty pending", decoded, ok)
	}
	if !marker.HasState(body) {
		t.Fatal("HasState: want true")
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/marker/ -run TestStateMarker -v`
Expected: FAIL to compile, `State` undefined.

- [ ] **Step 3: Write the implementation**

Create `internal/marker/state.go`. Follow the encoding style of the existing finding marker in `marker.go` (read it first). Shape:

```go
package marker

import (
	"strings"

	"goodkind.io/pr-review-agent/internal/domain"
)

// State statuses. A run in progress is reviewing; a finished run is done; an
// over budget delta is skipped; a run that could not finish is failed.
const (
	StateReviewing = "reviewing"
	StateDone      = "done"
	StateSkipped   = "skipped"
	StateFailed    = "failed"
)

const statePrefix = "<!-- pr-review-agent:state:v1 "
const stateSuffix = " -->"

// State is the durable review position kept on the one top level comment. It
// is the only memory the service has, so a new invocation reads it to learn
// what has been reviewed and what remains.
type State struct {
	LastReviewed domain.HeadSHA
	RunID        string
	Status       string
	Pending      []string
}

// EncodeState renders the state as one HTML comment line.
func EncodeState(state State) string {
	var builder strings.Builder
	builder.WriteString(statePrefix)
	builder.WriteString("last_reviewed=")
	builder.WriteString(string(state.LastReviewed))
	builder.WriteString(" run=")
	builder.WriteString(state.RunID)
	builder.WriteString(" status=")
	builder.WriteString(state.Status)
	builder.WriteString(" pending=")
	builder.WriteString(strings.Join(state.Pending, ","))
	builder.WriteString(stateSuffix)
	return builder.String()
}

// DecodeState finds and parses the state line anywhere in a comment body.
func DecodeState(body string) (State, bool) {
	start := strings.Index(body, statePrefix)
	if start < 0 {
		return State{LastReviewed: "", RunID: "", Status: "", Pending: nil}, false
	}
	rest := body[start+len(statePrefix):]
	end := strings.Index(rest, stateSuffix)
	if end < 0 {
		return State{LastReviewed: "", RunID: "", Status: "", Pending: nil}, false
	}
	fields := strings.Fields(rest[:end])
	state := State{LastReviewed: "", RunID: "", Status: "", Pending: nil}
	for _, field := range fields {
		key, value, found := strings.Cut(field, "=")
		if !found {
			continue
		}
		switch key {
		case "last_reviewed":
			state.LastReviewed = domain.HeadSHA(value)
		case "run":
			state.RunID = value
		case "status":
			state.Status = value
		case "pending":
			if value != "" {
				state.Pending = strings.Split(value, ",")
			}
		}
	}
	return state, true
}

// HasState reports whether one comment body carries the state marker.
func HasState(body string) bool {
	return strings.Contains(body, statePrefix)
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `make fmt && go test ./internal/marker/ -run TestStateMarker -v`
Expected: PASS

- [ ] **Step 5: Gate and commit**

```bash
make build && make test
git add internal/marker/
git commit -S -m "Add the durable state marker

Encode last reviewed commit, run id, status, and the pending chunk list
as one HTML comment line, the service's only memory between runs.

Co-authored-by: Claude <noreply@anthropic.com>"
```

---

### Task 4: The one top level comment

**Files:**
- Create: `internal/review/summary_comment.go`
- Modify: `internal/review/service.go` (the `GitHub` interface)
- Test: `internal/review/review_test.go` (fixture routes plus one test)

**Interfaces:**
- Consumes: Task 2's client methods, Task 3's marker.
- Produces:
  - The review `GitHub` interface gains:
    `ListIssueComments(context.Context, int64, domain.Repository, int) ([]githubapp.IssueComment, error)`,
    `CreateIssueComment(context.Context, int64, domain.Repository, int, string) (githubapp.IssueComment, error)`,
    `UpdateIssueComment(context.Context, int64, domain.Repository, int64, string) (githubapp.IssueComment, error)`
  - `func (service *Service) upsertSummaryComment(ctx context.Context, job domain.ReviewJob, body string) error`, finding the bot's marked comment and updating it in place, creating it only when absent. Matching is `comment.Author == service.botLogin && marker.HasState(comment.Body)`.
  - `func (service *Service) readState(ctx context.Context, job domain.ReviewJob) (marker.State, bool, error)`, reading the state from that same comment.

- [ ] **Step 1: Extend the service fixture**

In `internal/review/review_test.go`, extend `serviceServerState` with:

```go
issueComments       []map[string]any
issueCommentUpdates int
```

Add routes in `handleServiceRequest`:
- `POST /repos/owner/repo/issues/{n}/comments`: append `{"id": next id, "body": body, "user": {"login": testBotLogin}}` to `issueComments`, return 201 with that object.
- `GET /repos/owner/repo/issues/{n}/comments`: return `issueComments` as a JSON array.
- `PATCH /repos/owner/repo/issues/comments/{id}`: replace that entry's body, increment `issueCommentUpdates`, return 200 with the object.

Follow the file's existing route style exactly; it dispatches on method and path substrings.

- [ ] **Step 2: Write the failing test**

```go
func TestTheSummaryCommentIsCreatedOnceThenUpdatedInPlace(t *testing.T) {
	fixture := newServiceFixture(t, serviceFixtureOptions{})

	if err := fixture.run(context.Background(), fixture.job()); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if len(fixture.state.issueComments) != 1 {
		t.Fatalf("issue comments = %d, want 1 after the first run", len(fixture.state.issueComments))
	}
	firstID := fixture.state.issueComments[0]["id"]

	fixture.state.reviewPages = nil
	if err := fixture.run(context.Background(), fixture.job()); err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if len(fixture.state.issueComments) != 1 {
		t.Fatalf("issue comments = %d, want still 1 after the second run", len(fixture.state.issueComments))
	}
	if fixture.state.issueComments[0]["id"] != firstID {
		t.Fatalf("comment id changed, want the first comment updated in place")
	}
	if fixture.state.issueCommentUpdates < 1 {
		t.Fatalf("updates = %d, want at least one in place update", fixture.state.issueCommentUpdates)
	}
}
```

This test fully passes only after Task 7 wires the loop; at this task's end, assert the upsert directly instead if `runLocked` does not yet call it: call `service` through a small exported hook or defer the full assertion to Task 7 and here test `upsertSummaryComment` through the fixture by invoking two runs and accepting that the call site lands in Task 7. Prefer wiring a minimal call now: at the end of the existing `publish`, call `service.upsertSummaryComment(ctx, job, RenderBody(summary)+"\n"+marker.EncodeState(state))` with a `StateDone` state, so the test passes end to end in this task.

- [ ] **Step 3: Run to verify it fails**

Run: `go test ./internal/review/ -run TestTheSummaryCommentIsCreatedOnce -v`
Expected: FAIL, zero issue comments.

- [ ] **Step 4: Write the implementation**

Create `internal/review/summary_comment.go`:

```go
package review

// This file keeps the one top level comment the service owns.
//
// The comment is the service's memory and its face. The state marker inside
// it is what a later invocation resumes from, and the prose above the marker
// is what a person reads. Splitting those across objects is how the summary,
// the verdict, and the head drifted apart in production.

import (
	"context"
	"fmt"
	"log/slog"

	"goodkind.io/gklog"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/marker"
)

// upsertSummaryComment writes the body into the service's one top level
// comment, creating it only when this pull request has none.
func (service *Service) upsertSummaryComment(
	ctx context.Context,
	job domain.ReviewJob,
	body string,
) error {
	logger := gklog.L(ctx)
	comments, err := service.github.ListIssueComments(ctx, job.InstallationID, job.Repository, job.Number)
	if err != nil {
		return fmt.Errorf("list issue comments: %w", err)
	}
	for _, comment := range comments {
		if comment.Author != service.botLogin || !marker.HasState(comment.Body) {
			continue
		}
		if _, err := service.github.UpdateIssueComment(ctx, job.InstallationID, job.Repository, comment.ID, body); err != nil {
			return fmt.Errorf("update summary comment: %w", err)
		}
		logger.InfoContext(ctx, "summary comment updated", slog.Int64("comment_id", comment.ID))
		return nil
	}
	created, err := service.github.CreateIssueComment(ctx, job.InstallationID, job.Repository, job.Number, body)
	if err != nil {
		return fmt.Errorf("create summary comment: %w", err)
	}
	logger.InfoContext(ctx, "summary comment created", slog.Int64("comment_id", created.ID))
	return nil
}

// readState returns the durable state from the service's comment, and false
// when this pull request has never been reviewed.
func (service *Service) readState(
	ctx context.Context,
	job domain.ReviewJob,
) (marker.State, bool, error) {
	comments, err := service.github.ListIssueComments(ctx, job.InstallationID, job.Repository, job.Number)
	if err != nil {
		return marker.State{LastReviewed: "", RunID: "", Status: "", Pending: nil}, false, fmt.Errorf("list issue comments: %w", err)
	}
	for _, comment := range comments {
		if comment.Author != service.botLogin {
			continue
		}
		if state, ok := marker.DecodeState(comment.Body); ok {
			return state, true, nil
		}
	}
	return marker.State{LastReviewed: "", RunID: "", Status: "", Pending: nil}, false, nil
}
```

Add the three methods to the `GitHub` interface in `service.go`. `githubapp.Client` already satisfies them after Task 2.

- [ ] **Step 5: Run, gate, commit**

Run: `make fmt && go test ./internal/review/ -run TestTheSummaryCommentIsCreatedOnce -v`, then `make build && make test`.

```bash
git add internal/review/
git commit -S -m "Keep one top level comment holding the summary and durable state

Find the service's own comment by state marker and update it in place.
The comment is both the reader's summary and the run's memory.

Co-authored-by: Claude <noreply@anthropic.com>"
```

---

### Task 5: Chunk identity and the admission gate

**Files:**
- Create: `internal/review/admission.go`
- Modify: `internal/config/config.go` and its test
- Test: `internal/review/admission_internal_test.go` (new, `package review`)

**Interfaces:**
- Consumes: `diff.Chunk`.
- Produces:
  - `func chunkID(chunk diff.Chunk) string` returning the first 12 hex characters of the SHA-256 of `chunk.Text`.
  - `type admissionVerdict struct { Skip bool; Reason string }`
  - `func admitDelta(fileCount int, chunkCount int, maxFiles int, maxChunks int) admissionVerdict`
  - Config gains `ReviewMaxFiles int` (env `REVIEW_MAX_FILES`, default `100`) and `ReviewMaxChunks int` (env `REVIEW_MAX_CHUNKS`, default `60`). Both must parse as positive integers; follow the loading and validation pattern the file uses for `REVIEW_WORKERS`.

- [ ] **Step 1: Write the failing tests**

`internal/review/admission_internal_test.go`:

```go
package review

import (
	"strings"
	"testing"

	"goodkind.io/pr-review-agent/internal/diff"
)

func TestChunkIDIsStableAndShort(t *testing.T) {
	chunk := diff.Chunk{Index: 1, Total: 2, Text: "@@ -1 +1 @@\n-a\n+b\n", Pieces: nil, Paths: []string{"a.go"}, CoverageComplete: true}
	first := chunkID(chunk)
	second := chunkID(chunk)
	if first != second || len(first) != 12 {
		t.Fatalf("chunkID = %q then %q, want a stable 12 character id", first, second)
	}
	if strings.ToLower(first) != first {
		t.Fatalf("chunkID = %q, want lowercase hex", first)
	}
}

func TestAdmissionSkipsAnOverBudgetDelta(t *testing.T) {
	over := admitDelta(150, 10, 100, 60)
	if !over.Skip || !strings.Contains(over.Reason, "150") {
		t.Fatalf("verdict = %+v, want skip naming the measured size", over)
	}
	tooManyChunks := admitDelta(10, 173, 100, 60)
	if !tooManyChunks.Skip || !strings.Contains(tooManyChunks.Reason, "173") {
		t.Fatalf("verdict = %+v, want skip naming the chunk count", tooManyChunks)
	}
	within := admitDelta(100, 60, 100, 60)
	if within.Skip {
		t.Fatalf("verdict = %+v, want admission at the boundary", within)
	}
}
```

Add config tests to `internal/config/config_test.go` following its existing pattern: defaults applied when unset, rejection of a non-positive value.

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/review/ -run 'TestChunkID|TestAdmission' -v`
Expected: FAIL to compile.

- [ ] **Step 3: Write the implementation**

`internal/review/admission.go`:

```go
package review

// Admission decides whether a delta is worth attempting at all. An oversized
// review is slow and shallow, so past the configured budget the service says
// it is skipping and why, rather than grinding to a timeout and a red check.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"goodkind.io/pr-review-agent/internal/diff"
)

const chunkIDLength = 12

// chunkID names one chunk by its content, so a checkpoint written by one
// invocation still matches the same chunk when a later invocation re-derives
// the delta.
func chunkID(chunk diff.Chunk) string {
	sum := sha256.Sum256([]byte(chunk.Text))
	return hex.EncodeToString(sum[:])[:chunkIDLength]
}

// admissionVerdict says whether to review the delta, and when not, why.
type admissionVerdict struct {
	Skip   bool
	Reason string
}

// admitDelta measures the delta against the configured budgets.
func admitDelta(fileCount int, chunkCount int, maxFiles int, maxChunks int) admissionVerdict {
	if fileCount > maxFiles {
		return admissionVerdict{
			Skip:   true,
			Reason: fmt.Sprintf("%d files changed, over the %d file review budget", fileCount, maxFiles),
		}
	}
	if chunkCount > maxChunks {
		return admissionVerdict{
			Skip:   true,
			Reason: fmt.Sprintf("%d diff chunks, over the %d chunk review budget", chunkCount, maxChunks),
		}
	}
	return admissionVerdict{Skip: false, Reason: ""}
}
```

Add `ReviewMaxFiles` and `ReviewMaxChunks` to `config.Config` and `Load`, mirroring how `ReviewWorkers` is parsed and validated.

- [ ] **Step 4: Run, gate, commit**

Run: `make fmt && go test ./internal/review/ ./internal/config/ -v -run 'TestChunkID|TestAdmission|Config'`, then `make build && make test`.

```bash
git add internal/review/ internal/config/
git commit -S -m "Add chunk identity and the review admission gate

Name each chunk by a content hash so checkpoints survive re-derivation,
and refuse a delta over the configured file or chunk budget with the
measured size in the reason.

Co-authored-by: Claude <noreply@anthropic.com>"
```

---

### Task 6: Delta computation

**Files:**
- Create: `internal/review/delta.go`
- Test: `internal/review/review_test.go` (fixture route for compare) and `internal/review/delta_internal_test.go` if unit scope fits better

**Interfaces:**
- Consumes: `githubapp.Compare(ctx, installationID, repo, base, head)` and `githubapp.ListChangedFiles(ctx, installationID, repo, number)`; Task 3's `marker.State`. The review `GitHub` interface must gain `Compare(context.Context, int64, domain.Repository, domain.HeadSHA, domain.HeadSHA) ([]githubapp.ChangedFile, error)` and `ListChangedFiles(context.Context, int64, domain.Repository, int) ([]githubapp.ChangedFile, error)` if not already present; check with `go doc ./internal/review GitHub` first.
- Produces: `func (service *Service) deltaFiles(ctx context.Context, job domain.ReviewJob, head domain.HeadSHA, state marker.State, hasState bool) ([]githubapp.ChangedFile, error)`. No marker, or an empty `LastReviewed`: the full pull request file list. Otherwise: `Compare(state.LastReviewed, head)`. When `state.LastReviewed == head` and `len(state.Pending) == 0`, return an empty slice, meaning nothing to do.

- [ ] **Step 1: Add the fixture route**

`GET /repos/owner/repo/compare/{base}...{head}` returning `{"files": [...]}` in the shape `githubapp.Compare` decodes; read `internal/githubapp/pulls.go` for the exact response fields before writing the route. Record each call's base and head on `serviceServerState` as `compareCalls []string`.

- [ ] **Step 2: Write the failing test**

```go
func TestDeltaIsTheCompareRangeWhenAMarkerExists(t *testing.T) {
	fixture := newServiceFixture(t, serviceFixtureOptions{})
	fixture.state.issueComments = append(fixture.state.issueComments, map[string]any{
		"id":   float64(1),
		"body": marker.EncodeState(marker.State{LastReviewed: domain.HeadSHA(testStaleHeadSHA), RunID: "r1", Status: marker.StateDone, Pending: nil}),
		"user": map[string]any{"login": testBotLogin},
	})

	if err := fixture.run(context.Background(), fixture.job()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(fixture.state.compareCalls) == 0 {
		t.Fatalf("compare calls = %v, want the stale to current range", fixture.state.compareCalls)
	}
	if !strings.Contains(fixture.state.compareCalls[0], testStaleHeadSHA) {
		t.Fatalf("compare base = %q, want the last reviewed commit", fixture.state.compareCalls[0])
	}
}
```

This asserts through the full run once Task 7 lands; at this task's end, wire `deltaFiles` into `readAndAnalyze`'s input collection so the compare path executes, or test `deltaFiles` directly from an internal test with a stub `GitHub`. Choose the internal test if the run wiring is not yet reachable; the run level assertion then moves to Task 7's tests.

- [ ] **Step 3: Implement**

`internal/review/delta.go`:

```go
package review

// The delta is the unit of work. Reviewing only what changed since the last
// reviewed commit keeps every run proportional to the push, which is what
// makes a big pull request reviewable at all.

import (
	"context"
	"fmt"

	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
	"goodkind.io/pr-review-agent/internal/marker"
)

func (service *Service) deltaFiles(
	ctx context.Context,
	job domain.ReviewJob,
	head domain.HeadSHA,
	state marker.State,
	hasState bool,
) ([]githubapp.ChangedFile, error) {
	if !hasState || state.LastReviewed == "" {
		files, err := service.github.ListChangedFiles(ctx, job.InstallationID, job.Repository, job.Number)
		if err != nil {
			return nil, fmt.Errorf("list changed files: %w", err)
		}
		return files, nil
	}
	if state.LastReviewed == head && len(state.Pending) == 0 {
		return nil, nil
	}
	files, err := service.github.Compare(ctx, job.InstallationID, job.Repository, state.LastReviewed, head)
	if err != nil {
		return nil, fmt.Errorf("compare %s to %s: %w", state.LastReviewed, head, err)
	}
	return files, nil
}
```

- [ ] **Step 4: Run, gate, commit**

```bash
make fmt && make build && make test
git add internal/review/
git commit -S -m "Compute the review delta from the durable state

Review the compare range since the last reviewed commit, the full file
list on first contact, and nothing when the head is already done.

Co-authored-by: Claude <noreply@anthropic.com>"
```

---

### Task 7: The checkpointed chunk loop

This is the core task. It replaces the streaming capacity machinery with a loop that posts findings per chunk and advances the marker after each chunk, and it rewires `runLocked` around admission, delta, checkpoints, and status.

**Files:**
- Delete: `internal/review/stream_test.go`
- Rewrite: `internal/review/stream.go` into `internal/review/run.go` (delete the old file)
- Modify: `internal/review/service.go` (`runLocked`, `publish`, `Service` fields), `internal/review/publication.go` (`collectPublicationState` capacity parts), `internal/config/config.go` (remove `MaximumUnresolvedComments` and `REVIEW_MAX_UNRESOLVED_COMMENTS`)
- Test: `internal/review/review_test.go`

**Interfaces:**
- Consumes: everything from Tasks 3 through 6, the existing `Analyze`/`reviewChunk` machinery in `analyze.go` and `concurrency.go`, `RenderInline`, the reconciler.
- Produces:
  - `func (service *Service) reviewDelta(ctx context.Context, job domain.ReviewJob, head domain.HeadSHA, chunks []diff.Chunk, state marker.State) (marker.State, []domain.Finding, error)`: reviews the chunks whose ids are pending (or all, on a fresh state), posts each chunk's qualifying findings as inline comments immediately, calls `upsertSummaryComment` with the advanced state after each chunk, honors `service.invocationBudget` (the old `ReviewTimeout`) by stopping after the current chunk and returning with pending remainder, and never fails the whole run for one chunk: a failed chunk stays in `Pending`.
  - `runLocked` becomes: refresh head, read state, delta, admission (skip path), reviewDelta, reconcile, verdict when `len(state.Pending) == 0`, summary, check completion with status mapped from state (`done` success, `skipped` skipped, pending remainder success with an "in progress, continuing" title).
  - The global deadline kill is gone: `service.publicationContext` and per model request timeouts stay; nothing cancels the invocation as a whole. Rename the `ReviewTimeout` field usage to an invocation budget check between chunks, not a context deadline.

- [ ] **Step 1: Delete the capacity machinery**

```bash
git rm internal/review/stream_test.go
git mv internal/review/stream.go internal/review/run.go
```

Then rewrite `run.go` from scratch. Remove: `publicationState.capacity`, `hasTailSlot`, `heldFinalist`, the overflow pool, `pending` and `rejected` key sets, `takeFinalist`, `takeOverflow`, `claimed`, `Finalize`, `outranks`, the three way `settle`. Keep and reuse: suppression (`suppressed`, `remember`, `historyIDs`, `historyAnchors`), `RenderInline`, head currency checking, and the detached publication context.

- [ ] **Step 2: Write the failing tests**

Add to `internal/review/review_test.go`:

```go
func TestEveryQualifyingFindingIsPublished(t *testing.T) {
	findings := make([]domain.Finding, 0, 12)
	for index := range 12 {
		findings = append(findings, domain.Finding{
			Path:       "main.go",
			StartLine:  2,
			EndLine:    2,
			Title:      fmt.Sprintf("Defect %d", index),
			Body:       "A real defect on a changed line.",
			Importance: 9,
		})
	}
	_ = findings
	// Build the fixture so the model returns all 12 on one chunk, run, and
	// assert 12 inline comments were posted. A reviewer does not ration
	// comments; the old cap of 3 must be gone.
}

func TestTheMarkerAdvancesOnlyAfterAChunksFindingsPost(t *testing.T) {
	// Force the inline comment route to fail for one chunk's findings.
	// After the run, the state read from the summary comment must still
	// list that chunk id in pending, and the check must not be a failure.
}

func TestAnOverBudgetDeltaIsSkippedWithoutTouchingReviewState(t *testing.T) {
	// Configure ReviewMaxChunks to 1 and feed a two chunk diff. After the
	// run: no submitted review, no dismissals, check conclusion "skipped",
	// and the summary comment contains the reason with the measured size.
}

func TestAnInvocationStopsAtItsBudgetAndKeepsThePendingList(t *testing.T) {
	// Set the invocation budget to zero so the loop stops after the first
	// chunk. After the run: at least one chunk id remains in pending, the
	// check is not red, and no verdict review was submitted.
}
```

Flesh each comment into real assertions against the fixture state while writing them; the fixture routes from Tasks 4 and 6 provide the observation points. Each test must fail before Step 3 and pass after.

- [ ] **Step 3: Write `run.go`**

The loop, shape:

```go
// reviewDelta reviews the pending chunks one at a time, posting each chunk's
// findings before advancing the checkpoint, so a death at any moment loses at
// most the chunk in flight.
func (service *Service) reviewDelta(
	ctx context.Context,
	job domain.ReviewJob,
	head domain.HeadSHA,
	chunks []diff.Chunk,
	state marker.State,
) (marker.State, []domain.Finding, error) {
	logger := gklog.L(ctx)
	byID := make(map[string]diff.Chunk, len(chunks))
	for _, chunk := range chunks {
		byID[chunkID(chunk)] = chunk
	}
	pending := state.Pending
	if len(pending) == 0 {
		pending = make([]string, 0, len(chunks))
		for _, chunk := range chunks {
			pending = append(pending, chunkID(chunk))
		}
	}

	started := service.now()
	admitted := make([]domain.Finding, 0)
	remaining := append([]string{}, pending...)
	for len(remaining) > 0 {
		if service.invocationBudget > 0 && service.now().Sub(started) > service.invocationBudget {
			logger.InfoContext(ctx, "invocation budget reached, stopping after checkpoint",
				slog.Int("pending", len(remaining)))
			break
		}
		id := remaining[0]
		chunk, ok := byID[id]
		if !ok {
			// The delta changed shape since this id was recorded; drop it.
			remaining = remaining[1:]
			continue
		}
		findings, err := service.reviewOneChunk(ctx, job, head, chunk)
		if err != nil {
			// The chunk stays pending; a later invocation resumes it. This
			// is resume, not retry: nothing loops in place.
			logger.ErrorContext(ctx, "chunk review failed, leaving it pending",
				slog.String("chunk", id), slog.String("err", err.Error()))
			remaining = append(remaining[1:], id)
			if service.everyRemainingChunkJustFailed(remaining) {
				break
			}
			continue
		}
		posted, postErr := service.postChunkFindings(ctx, job, head, findings)
		if postErr != nil {
			logger.ErrorContext(ctx, "posting chunk findings failed, leaving the chunk pending",
				slog.String("chunk", id), slog.String("err", postErr.Error()))
			break
		}
		admitted = append(admitted, posted...)
		remaining = remaining[1:]
		state.Pending = remaining
		state.RunID = job.DeliveryID
		state.Status = marker.StateReviewing
		if err := service.upsertSummaryComment(ctx, job, service.renderProgressBody(job, head, state)); err != nil {
			return state, admitted, fmt.Errorf("advance checkpoint: %w", err)
		}
	}
	state.Pending = remaining
	if len(remaining) == 0 {
		state.LastReviewed = head
		state.Status = marker.StateDone
	}
	return state, admitted, nil
}
```

Supporting pieces to write in the same file, reusing existing code: `reviewOneChunk` wraps the existing single chunk review path from `concurrency.go` (one model call, its own request timeout, truncation split allowed within the call since it is bounded); `postChunkFindings` filters by importance and anchoring via `eligibleFindings`, dedupes against suppression, renders with `RenderInline`, posts via `CreateReviewComment`, and returns what posted; `everyRemainingChunkJustFailed` prevents an infinite loop when every chunk fails in one invocation by tracking ids seen failed this invocation; `renderProgressBody` composes prose plus `marker.EncodeState(state)`.

Rewire `runLocked`:

1. Existing head refresh and check run creation stay.
2. `state, hasState, err := service.readState(ctx, job)`.
3. `files, err := service.deltaFiles(...)`. Empty files and empty pending: complete the check "Already reviewed" and return, replacing the old marker based dedupe (`hasBotReviewMarker` path can remain as a second guard or be deleted; delete it if all its tests are updated).
4. Chunk with the existing `diff.ChunkInput`; compute `admitDelta(len(files), len(chunks), service.maxFiles, service.maxChunks)`. On skip: `state.Status = StateSkipped`, upsert comment with the reason, complete check with conclusion `skipped`, return nil. No review object is touched.
5. Reconcile (existing reconciler call, unchanged position or after review; keep existing order: reconcile first).
6. `state, findings, err := service.reviewDelta(...)`; on err, fall to the failure path (Task 8).
7. If `len(state.Pending) > 0`: upsert comment (progress body), complete check success with title "Review in progress, continuing", trigger continuation (Task 9 fills this; leave a `service.requestContinuation(ctx, job)` no-op stub here), return nil. No verdict.
8. Else: verdict (Task 8's `reviewerDecision`), submit verdict review, upsert comment with the final summary, complete check success.

Remove `maximumUnresolvedComments` from `Service`, `selectFindingsForPublication`'s cap, and `REVIEW_MAX_UNRESOLVED_COMMENTS` from config and its tests. Keep suppression so a finding an earlier run carried is not reposted.

- [ ] **Step 4: Run, gate, commit**

Run the four new tests, then `make fmt && make build && make test`. Existing tests that asserted cap behavior or the old streaming sink will fail; update or delete them per the spec, never weaken a test that guards a spec invariant.

```bash
git add -A internal/
git commit -S -m "Review the delta in checkpointed chunks with no global deadline

Post each chunk's findings before advancing the marker, stop at the
invocation budget leaving the remainder pending, skip an over budget
delta outright, and delete the comment cap machinery.

Co-authored-by: Claude <noreply@anthropic.com>"
```

---

### Task 8: The verdict and the failure path

**Files:**
- Modify: `internal/review/publication.go`, `internal/review/notice.go`, `internal/review/service.go`
- Test: `internal/review/review_test.go`, `internal/review/publication_internal_test.go` (new, `package review`)

**Interfaces:**
- Consumes: `hasUnresolvedBotThread` (unchanged), Task 4's `upsertSummaryComment`.
- Produces:
  - `func reviewerDecision(threads []githubapp.ReviewThread, botLogin string, headFullyReviewed bool) domain.ReviewDecision` replacing `standingDecision`. `REQUEST_CHANGES` when any bot thread is open. `REQUEST_CHANGES` withheld and no review submitted when the head is not fully reviewed (the caller skips submission; the function is only called with `headFullyReviewed == true` from the loop, and the unit test locks the semantics).
  - The failure path: `failCheck` writes the cause into the summary comment via `upsertSummaryComment` with `state.Status = StateFailed`, completes the check as failure, and touches no review. Delete `clearFailedReviewState`, `dismissStaleVerdicts`, `listReviewsForFailure`, `standingVerdict`, `dismissalMessageFor`, `approvalDismissalMessage`, `blockDismissalMessage`, and `DismissReview` from the `GitHub` interface if nothing else uses it.

- [ ] **Step 1: Write the failing tests**

`publication_internal_test.go`:

```go
package review

import (
	"testing"

	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
)

func TestReviewerDecisionIsAPureFunctionOfThreadsAndCoverage(t *testing.T) {
	open := []githubapp.ReviewThread{{
		NodeID: "t1", Resolved: false, Outdated: false,
		ViewerCanResolve: true, ViewerCanUnresolve: true,
		RootComment: domain.ReviewComment{Author: "bot[bot]", Body: "open finding"},
	}}
	resolved := []githubapp.ReviewThread{{
		NodeID: "t1", Resolved: true, Outdated: false,
		ViewerCanResolve: true, ViewerCanUnresolve: true,
		RootComment: domain.ReviewComment{Author: "bot[bot]", Body: "fixed finding"},
	}}

	if got := reviewerDecision(open, "bot[bot]", true); got != domain.ReviewDecisionRequestChanges {
		t.Fatalf("open thread: decision = %q, want request changes", got)
	}
	if got := reviewerDecision(resolved, "bot[bot]", true); got != domain.ReviewDecisionApprove {
		t.Fatalf("clean and reviewed: decision = %q, want approve", got)
	}
}
```

Adapt the `ReviewComment` literal fields to `domain.ReviewComment`'s real shape; read the type first, `exhaustruct` requires every field.

Service level, in `review_test.go`:

```go
func TestAFailedRunChangesNoReviewState(t *testing.T) {
	fixture := newServiceFixture(t, serviceFixtureOptions{
		model: &failingModel{err: errors.New("provider exploded")},
	})

	if err := fixture.run(context.Background(), fixture.job()); err == nil {
		t.Fatal("Run: want the model failure surfaced")
	}
	if fixture.state.lastSubmitReview != nil {
		t.Fatalf("submitted review = %v, want none", fixture.state.lastSubmitReview)
	}
	if len(fixture.state.issueComments) == 0 ||
		!strings.Contains(fixture.state.issueComments[0]["body"].(string), "provider exploded") {
		t.Fatal("summary comment must carry the real cause")
	}
}
```

Note the loop of Task 7 leaves a failed chunk pending rather than failing the run, so `failingModel` here must fail in a way that reaches the failure path: every chunk failing ends the invocation with all chunks pending and no run error. Assert instead on whichever path the failure actually takes: pending remainder with no verdict, or hard failure for a non chunk error (for example the state read failing). Write one test per path: `TestEveryChunkFailingLeavesAllPendingAndNoVerdict` and `TestAHardFailureWritesTheCauseAndTouchesNoReview` (force `ListIssueComments` to 500 on the second call to break `upsertSummaryComment`). The invariant under test is identical: no review object changes.

- [ ] **Step 2: Verify they fail, implement, verify they pass**

`reviewerDecision` in `publication.go`:

```go
// reviewerDecision is the whole verdict policy. A reviewer blocks while
// something they raised is open, and approves when nothing is. Anything more
// clever than this is how stale blocks happened.
func reviewerDecision(
	threads []githubapp.ReviewThread,
	botLogin string,
	headFullyReviewed bool,
) domain.ReviewDecision {
	if hasUnresolvedBotThread(threads, botLogin) {
		return domain.ReviewDecisionRequestChanges
	}
	if !headFullyReviewed {
		return domain.ReviewDecisionRequestChanges
	}
	return domain.ReviewDecisionApprove
}
```

Delete `standingDecision` and every dismissal helper listed above. Rewrite `failCheck`'s review side to a single `upsertSummaryComment` carrying `RenderFailureBody` output plus the failed state marker; keep its check completion.

- [ ] **Step 3: Gate and commit**

```bash
make fmt && make build && make test
git add internal/review/
git commit -S -m "Recompute the verdict from open threads and stop failure touching reviews

Replace standingDecision with the pure reviewerDecision, submit the
verdict only for a fully reviewed head, and delete the failure path's
review dismissal machinery.

Co-authored-by: Claude <noreply@anthropic.com>"
```

---

### Task 9: Continuation, service side

**Files:**
- Modify: `internal/app/handler.go`, `internal/review/service.go`
- Test: `internal/app/app_test.go` or `handler_test.go`, `internal/review/review_test.go`

**Interfaces:**
- Consumes: the queue admission path `handleGitHubWebhook` already uses (read it and mirror it), the webhook HMAC verification already in `internal/webhook`, Task 7's `requestContinuation` stub.
- Produces:
  - Route `POST /api/v1/continue` on the handler. Body: `{"installation_id": int64, "owner": string, "repo": string, "number": int}`. Verified with the same HMAC scheme and secret as the webhook (`X-Hub-Signature-256` over the raw body). On success it fetches the pull request head via the service and admits a job with `DeliveryID` set to `continue-` plus a fresh UUID, then dispatches exactly as a webhook does. Draft or closed pull requests are answered 200 with nothing enqueued.
  - `func (service *Service) AdmitContinuation(ctx context.Context, installationID int64, repo domain.Repository, number int, deliveryID string) (domain.ReviewJob, error)`: reads the head with `GetPullRequest`, builds the `domain.ReviewJob`, and calls the existing `Admit`.
  - In process self continuation: `requestContinuation` re-enqueues the same job through the dispatcher when an invocation ends with pending chunks, so continuation normally never waits for the Worker cron. Guard against a tight loop: re-enqueue only when this invocation completed at least one chunk; otherwise leave continuation to the cron.
  - Pending report: at the end of every invocation, POST to `cfg.LogForwardURL`'s host at path `/internal/v1/pending` (new config value `PENDING_REPORT_URL`, optional like `LOG_FORWARD_URL`) a signed JSON body `{"installation_id":..,"owner":..,"repo":..,"number":..,"pending":true|false}` using the same HMAC header the log shipper uses (`X-Pr-Agent-Signature-256`). Reuse the telemetry forwarder's signing helper; read `internal/telemetry` first and extract shared signing if needed. `pending` is true when chunks remain, false when the state is done, skipped, or failed. A send failure is logged and never fails the run.

- [ ] **Step 1: Write the failing tests**

Handler test (`package app`): POST a signed continue body, assert a job was admitted with a `continue-` delivery id and the correct ref; POST with a bad signature, assert 401 and nothing admitted. Reuse the app fixture's admitter recording.

Service test: run a fixture whose invocation budget forces a pending remainder, assert the fixture recorded a POST to the pending route with `"pending":true`; run a normal completing fixture, assert `"pending":false`.

- [ ] **Step 2: Implement, verify, gate**

Follow `handleGitHubWebhook` for parsing, verification, and enqueue mechanics. Keep the continue handler small: verify, decode, `AdmitContinuation`, dispatch, 202.

- [ ] **Step 3: Commit**

```bash
git add internal/
git commit -S -m "Add the continue entry point and the pending report

Accept a signed continue request that re-admits a pull request by ref,
re-enqueue in process when an invocation checkpoints with work left,
and report pending state to the Worker at the end of every invocation.

Co-authored-by: Claude <noreply@anthropic.com>"
```

---

### Task 10: Continuation, Worker side

**Files:**
- Create: `deploy/cloudflare/worker/pending.js`
- Modify: `deploy/cloudflare/worker/index.js`, `deploy/cloudflare/worker/router.js`, `deploy/cloudflare/wrangler.jsonc`
- Test: `deploy/cloudflare/test/pending.test.js` (new), following the style of `test/servicelogs.test.js`

**Interfaces:**
- Consumes: `verifyServiceLogSignature` and `readBoundedText` from `servicelogs.js` (import and reuse; do not duplicate), the `PR_AGENT` Durable Object binding.
- Produces:
  - `pending.js` exports `PENDING_PATH = "/internal/v1/pending"` and `handlePending(request, env)`: verify the signature with `GITHUB_WEBHOOK_SECRET`, parse `{installation_id, owner, repo, number, pending}`, and call `env.PR_AGENT.getByName("github-app").setPending(key, ref)` or `clearPending(key)` where `key` is `owner + "/" + repo + "#" + number`.
  - `PrAgentContainer` gains RPC methods backed by `this.ctx.storage`:
    `async setPending(key, ref)` storing under `"pending:" + key`,
    `async clearPending(key)`,
    `async listPending()` returning the stored refs.
  - `index.js` default export gains `async scheduled(controller, env)`: list pending refs, and for each, POST a signed continue body to the container (`env.PR_AGENT.getByName("github-app").fetch` with a synthetic request to `http://container/api/v1/continue`), computing the `X-Hub-Signature-256` HMAC with `GITHUB_WEBHOOK_SECRET` over the exact body bytes. Log one line per attempt with the key and response status.
  - `wrangler.jsonc` gains `"triggers": { "crons": ["*/5 * * * *"] }`, gains vars `"REVIEW_MAX_FILES": "100"` and `"REVIEW_MAX_CHUNKS": "60"` and `"PENDING_REPORT_URL": "https://agoodkind-nano-pr-reviewer.alex-ee7.workers.dev/internal/v1/pending"`, and drops `"REVIEW_MAX_UNRESOLVED_COMMENTS"`.
  - `router.js` routes `PENDING_PATH` to `handlePending` before the container forward, exactly as it routes `SERVICE_LOG_PATH`.

- [ ] **Step 1: Write the failing tests**

`test/pending.test.js` with `node --test`: signature rejection returns 401 and stores nothing; a valid `pending:true` body calls `setPending` with the composed key; a valid `pending:false` calls `clearPending`. Stub the DO with a plain object recording calls, passed through a fake `env`. For the HMAC, reuse the signing helper the servicelogs test uses to build valid signatures; read `test/servicelogs.test.js` first.

- [ ] **Step 2: Run to verify they fail**

Run: `cd deploy/cloudflare && npm test`
Expected: the new file fails, existing tests stay green.

- [ ] **Step 3: Implement, verify, dry run**

Run: `cd deploy/cloudflare && npm run check`
Expected: tests pass and `wrangler deploy --dry-run` accepts the config with the cron trigger.

- [ ] **Step 4: Commit**

```bash
git add deploy/cloudflare/
git commit -S -m "Store pending reviews in the Durable Object and continue them on a cron

Accept the service's signed pending reports into Durable Object
storage, and every five minutes re-invoke the container with a signed
continue request for each ref still pending, so a died container delays
a review instead of losing it.

Co-authored-by: Claude <noreply@anthropic.com>"
```

---

### Task 11: The invariant suite

**Files:**
- Create: `internal/review/invariants_test.go` (`package review_test`)

**Interfaces:**
- Consumes: everything above.

The spec lists eight invariants. Five already have homes: invariant 1 (Task 8's decision tests), 2 (Task 7's marker test), 5 (Task 8's failure tests), 6 (Task 4's comment test), 8 (Task 7's skip test). Write the three without one, and add a comment at the top of the file naming where the other five live so the suite reads as a complete list.

- [ ] **Step 1: Write the tests**

1. `TestKillingTheProcessLosesAtMostOneChunk` (invariant 3): run with a fixture that fails hard after the second chunk's checkpoint (make the third chunk's model call panic or make the summary upsert return 500 on the third update). Then run a second fixture invocation against the surviving fixture state and assert every chunk's findings end up posted exactly once, none duplicated, none missing.
2. `TestADiffOfAnySizeCompletesAcrossBoundedInvocations` (invariant 4): invocation budget forcing one chunk per invocation, a six chunk diff within admission budget, loop `fixture.run` until the state reads done with a hard cap of 10 iterations, assert done in at most 6 plus 1 and all findings posted.
3. `TestTheRunIdentifierIsTheSameStringEverywhere` (invariant 7): after one completed run, read the run id from the summary comment marker, the check run output text, and assert both equal `job.DeliveryID`. Extend the check completion in Task 7 to include `Run ID: <id>` in the check output text if it does not already; that line is this invariant's carrier.

- [ ] **Step 2: Run and fix what fails**

Any failure here is a real gap in Tasks 1 through 10. Fix the implementation, never the invariant.

- [ ] **Step 3: Gate and commit**

```bash
make fmt && make build && make test
git add internal/review/invariants_test.go internal/
git commit -S -m "Lock the durable review invariants

Prove at most one chunk is lost to a death, any admitted diff completes
across bounded invocations, and one run identifier reaches every
artifact.

Co-authored-by: Claude <noreply@anthropic.com>"
```

---

### Task 12: Operations documentation

**Files:**
- Modify: `docs/operations.md`

- [ ] **Step 1: Rewrite the review lifecycle section**

Describe: the delta based loop, the one top level comment and its state marker, admission and the skip experience, the verdict policy, the failure experience, continuation, and the new configuration (`REVIEW_MAX_FILES`, `REVIEW_MAX_CHUNKS`, `PENDING_REPORT_URL`; `REVIEW_MAX_UNRESOLVED_COMMENTS` removed; `REVIEW_TIMEOUT` now the per invocation budget). Follow `~/.claude/rules/writing.md`: behavior before implementation, one durable home per fact, no filename in prose unless the reader must open it. Delete every sentence describing the comment cap, the mutable summary review, and failure time dismissals.

- [ ] **Step 2: Commit**

```bash
git add docs/operations.md
git commit -S -m "Document the durable incremental review lifecycle

Co-authored-by: Claude <noreply@anthropic.com>"
```

---

## Self-Review

**Spec coverage.** Admission gate: Task 5 plus the Task 7 skip path. Marker state: Tasks 3 and 4. Incremental delta: Task 6. Checkpoints and no global deadline: Task 7. Verdict recomputation and failure isolation: Task 8. Continuation: Tasks 9 and 10. Invariants 1 through 8: Tasks 4, 7, 8, 11 as mapped in Task 11. Run identifier: Tasks 1 and 11. Deletions: Tasks 7 and 8. Live acceptance (mlx-swift-lm 8 declined, a normal pull request resuming after a forced death) is a deploy time proof and deliberately not a plan task; it runs after merge and deploy.

**Placeholders.** Task 7 Step 2 sketches four tests with comment bodies; each names its exact assertions and observation points, and the implementer fleshes them against the fixture. Task 8 Step 1 names the two failure path tests and their forcing functions. No TBDs remain.

**Type consistency.** `marker.State` (Task 3) is consumed by Tasks 4, 6, 7, 8. `chunkID` (Task 5) is consumed by Task 7. `deltaFiles` (Task 6) is consumed by Task 7. `upsertSummaryComment` and `readState` (Task 4) are consumed by Tasks 7 and 8. `reviewerDecision(threads, botLogin, headFullyReviewed)` matches between Tasks 8 and 11. `AdmitContinuation` (Task 9) is invoked by the Worker's continue POST from Task 10.

**Known integration risks, named for the executor.** The `review_test.go` fixture is large; Tasks 4, 6, 7 each extend it, so later tasks must read the file as it stands, not as this plan quotes it. The `Container` class owns the Durable Object alarm for its idle timer; Task 10 therefore uses a Worker cron trigger and never overrides `alarm()`. GitHub counts only each reviewer's latest APPROVE or REQUEST_CHANGES toward the merge decision, which is what makes replacing the verdict by submitting a new review sufficient.
