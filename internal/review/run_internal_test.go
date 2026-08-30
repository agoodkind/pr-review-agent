package review

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
	"goodkind.io/pr-review-agent/internal/marker"
)

// internalTestHeadSHA is the head every internal test in this package uses.
const internalTestHeadSHA = "0123456789abcdef0123456789abcdef01234567"

// internalTestMovedHeadSHA is the commit a push moves the pull request to.
const internalTestMovedHeadSHA = "89abcdef0123456789abcdef0123456789abcdef"

// chunkIDPattern is the shape the durable marker's pending list accepts. A
// chunk id that falls outside it encodes a marker the decoder throws away,
// which loses the pending list the next run resumes from.
var chunkIDPattern = regexp.MustCompile(`^[0-9a-f]{12}$`)

func testChunk(text string) diff.Chunk {
	return diff.Chunk{
		Index:            1,
		Total:            1,
		Text:             text,
		Pieces:           nil,
		Paths:            []string{"main.go"},
		CoverageComplete: true,
	}
}

// A chunk id has to survive being written into a marker and read back by a
// later run, so it must match what the marker's own decoder accepts.
func TestChunkIDIsTwelveLowercaseHexCharacters(t *testing.T) {
	id := chunkID(testChunk("File: main.go\nStatus: modified\n\n@@ -1,1 +1,2 @@\n+added\n"))

	if !chunkIDPattern.MatchString(id) {
		t.Fatalf("chunk id = %q, want twelve lowercase hex characters", id)
	}
}

// The id has to name the same chunk across runs, because a checkpoint written
// by one run is matched against a delta a later run re-derives. Position does
// not survive that, so the id comes from the text.
func TestChunkIDFollowsTheTextNotThePosition(t *testing.T) {
	first := testChunk("the same chunk text")
	moved := first
	moved.Index = 7
	moved.Total = 9

	if chunkID(first) != chunkID(moved) {
		t.Fatalf("chunk id changed with position: %q then %q", chunkID(first), chunkID(moved))
	}
	if chunkID(first) == chunkID(testChunk("different chunk text")) {
		t.Fatal("two different chunk texts share one id")
	}
}

func TestRemoveChunkIDDropsOnlyTheNamedID(t *testing.T) {
	remaining := removeChunkID([]string{"aaaaaaaaaaaa", "bbbbbbbbbbbb", "cccccccccccc"}, "bbbbbbbbbbbb")

	if len(remaining) != 2 || remaining[0] != "aaaaaaaaaaaa" || remaining[1] != "cccccccccccc" {
		t.Fatalf("remaining = %v, want the other two ids in order", remaining)
	}
}

// The work list is the delta minus what is already read. Both halves matter:
// subtracting nothing pays for every chunk again after one failure, and
// starting from the pending list instead of the delta skips the chunks a new
// commit introduced and then calls the range reviewed.
func TestPendingWorkIsTheDeltaMinusWhatIsAlreadyRead(t *testing.T) {
	first := testChunk("chunk one")
	second := testChunk("chunk two")
	fresh := testChunk("a chunk a later commit added")
	state := marker.State{
		LastReviewed: "",
		RunID:        "delivery-0",
		Status:       marker.StateReviewing,
		Pending:      []string{chunkID(second)},
		Completed:    []string{chunkID(first)},
	}

	work := pendingWork(context.Background(), state, []diff.Chunk{first, second, fresh})

	if len(work.owed) != 2 || work.owed[0] != chunkID(second) || work.owed[1] != chunkID(fresh) {
		t.Fatalf("owed = %v, want the pending chunk and the new one", work.owed)
	}
	if len(work.chunks) != 2 {
		t.Fatalf("chunks = %d, want only the ones still owed", len(work.chunks))
	}
	if len(work.completed) != 1 || work.completed[0] != chunkID(first) {
		t.Fatalf("completed = %v, want the already read chunk carried forward", work.completed)
	}
}

// The completed set keeps only ids the delta still holds. A chunk whose text
// has changed can never match again, so carrying its id would grow the marker
// for nothing; pruning bounds the set by the delta, which admission bounds.
func TestPendingWorkDropsCompletedIDsTheDeltaNoLongerHolds(t *testing.T) {
	surviving := testChunk("a chunk nobody touched")
	state := marker.State{
		LastReviewed: "",
		RunID:        "delivery-0",
		Status:       marker.StateReviewing,
		Pending:      nil,
		Completed:    []string{chunkID(surviving), chunkID(testChunk("a chunk since rewritten"))},
	}

	work := pendingWork(context.Background(), state, []diff.Chunk{surviving})

	if len(work.owed) != 0 {
		t.Fatalf("owed = %v, want none: the only chunk left was already read", work.owed)
	}
	if len(work.completed) != 1 || work.completed[0] != chunkID(surviving) {
		t.Fatalf("completed = %v, want only the id the delta still holds", work.completed)
	}
}

// headStubGitHub answers the one head read confirmHead makes.
type headStubGitHub struct {
	GitHub
	head domain.HeadSHA
	err  error
}

func (github *headStubGitHub) GetPullRequest(
	context.Context,
	int64,
	domain.Repository,
	int,
) (githubapp.PullRequest, error) {
	return githubapp.PullRequest{Head: github.head}, github.err
}

func headCheckService(head domain.HeadSHA, err error) *Service {
	return &Service{
		github:   &headStubGitHub{head: head, err: err},
		botLogin: summaryCommentTestBotLogin,
	}
}

// A head read that answers on an already dead context proves nothing. The
// comment posts that follow it would every one fail, while the findings stayed
// admitted as objections nobody can see, so the dead context is reported as the
// failure it is rather than as a current head.
func TestConfirmHeadRefusesADeadContext(t *testing.T) {
	service := headCheckService(domain.HeadSHA(internalTestHeadSHA), nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := service.confirmHead(ctx, summaryCommentTestJob(), domain.HeadSHA(internalTestHeadSHA))
	if err == nil {
		t.Fatal("confirmHead: want the dead context reported rather than a current head")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want the context cause", err)
	}
	if errors.Is(err, errHeadMoved) {
		t.Fatalf("err = %v, want a dead context reported as itself and not as a moved head", err)
	}
}

// A read that fails on a live context is treated as current, because dropping
// real findings over one failed lookup costs the reader more than a comment on
// a commit that just moved.
func TestConfirmHeadTreatsAFailedReadAsCurrent(t *testing.T) {
	service := headCheckService("", errors.New("github is unreachable"))

	err := service.confirmHead(
		context.Background(),
		summaryCommentTestJob(),
		domain.HeadSHA(internalTestHeadSHA),
	)
	if err != nil {
		t.Fatalf("confirmHead: %v, want a failed read treated as current", err)
	}
}

func TestConfirmHeadReportsAMovedHead(t *testing.T) {
	service := headCheckService(domain.HeadSHA(internalTestMovedHeadSHA), nil)

	err := service.confirmHead(
		context.Background(),
		summaryCommentTestJob(),
		domain.HeadSHA(internalTestHeadSHA),
	)
	if !errors.Is(err, errHeadMoved) {
		t.Fatalf("err = %v, want the moved head reported", err)
	}
}

// postPlanGitHub answers each comment post from a scripted plan, so a test can
// put a refusal and a transient failure in one batch.
type postPlanGitHub struct {
	headStubGitHub
	plan []error
	call int
}

func (github *postPlanGitHub) CreateReviewComment(
	context.Context, int64, domain.Repository, int, domain.HeadSHA, githubapp.InlineComment,
) error {
	answer := github.plan[github.call%len(github.plan)]
	github.call++
	return answer
}

func postFindingsWithPlan(t *testing.T, plan []error) error {
	t.Helper()
	postHead := domain.HeadSHA(internalTestHeadSHA)
	work := deltaWork{
		Files: []diff.FileContext{{
			Path:              "main.go",
			Status:            "modified",
			Patch:             "@@ -1,2 +1,4 @@\n context\n+two\n+three\n context2\n",
			CurrentContent:    "context\ntwo\nthree\ncontext2\n",
			ChangedRightLines: map[int]struct{}{2: {}, 3: {}},
			ChangedRightHunks: map[int]int{2: 1, 3: 1},
			CoverageComplete:  true,
		}},
		Chunks: nil,
	}
	selection := collectPublicationState(nil, nil, summaryCommentTestBotLogin)
	pass := newChunkPass(work, 1, &selection)
	service := &Service{
		github:             &postPlanGitHub{headStubGitHub: headStubGitHub{head: postHead}, plan: plan},
		publicationTimeout: time.Second,
		now:                time.Now,
	}
	findings := []domain.Finding{
		{Path: "main.go", StartLine: 2, EndLine: 2, Title: "First defect", Body: "body one", Importance: 8},
		{Path: "main.go", StartLine: 3, EndLine: 3, Title: "Second defect", Body: "body two", Importance: 8},
	}
	return service.postChunkFindings(context.Background(), summaryCommentTestJob(), postHead, findings, pass)
}

// Each failed post is classified by its own error, never by the batch it
// shares. A batch mixing a refusal with a dropped connection must report the
// transient as the cause the chunk stays pending for: the refusal is final for
// its one comment and no later attempt can change it, so blaming the batch on
// the refusal misattributed a retryable failure to an unretryable one.
func TestAMixedPostBatchPendsOnTheTransientNotTheRefusal(t *testing.T) {
	refusal := githubapp.APIError{StatusCode: 422, Message: "Unprocessable"}
	transient := errors.New("connection reset by peer")

	err := postFindingsWithPlan(t, []error{refusal, transient})
	if err == nil {
		t.Fatal("no error, want the chunk left pending for the transient failure")
	}
	if errors.Is(err, errCommentRefused) {
		t.Fatalf("err = %v, want the pending path: a transient failure was in the batch", err)
	}
	if !strings.Contains(err.Error(), "connection reset by peer") {
		t.Fatalf("err = %v, want the transient failure as the recorded cause", err)
	}
}

// A batch whose every failure is GitHub answering no is finished, not owed.
func TestAPureRefusalBatchTakesTheRefusedPath(t *testing.T) {
	refusal := githubapp.APIError{StatusCode: 422, Message: "Unprocessable"}

	err := postFindingsWithPlan(t, []error{refusal, refusal})
	if !errors.Is(err, errCommentRefused) {
		t.Fatalf("err = %v, want errCommentRefused when no failure is retryable", err)
	}
}
