package review_test

// This file is the ledger of the design's invariants. Every promise the spec
// makes is either tested here or pointed at the test that already proves it, so
// a reader can see at a glance that none is unproven without hunting for one.
//
// The spec is docs/superpowers/specs/2026-08-29-durable-review-design.md.
//
//  1. The verdict equals the pure function of own thread state and reviewed
//     head. Proven by TestServiceApprovesWhenEveryFindingIsAlreadyResolved,
//     TestServiceKeepsRequestingChangesWhileABotThreadStaysOpen, and
//     TestAHunkThatCannotSplitLeavesTheHeadUnapproved, all in review_test.go.
//  2. The last reviewed commit advances only after its chunks' findings are on
//     the page. Proven by TestTheMarkerAdvancesOnlyAfterAChunksFindingsPost in
//     review_test.go.
//  3. Killing the process loses only the chunks in flight. Proven here in the
//     narrowest case: the test holds exactly one chunk at the model and kills
//     the run there, which is what lets it prove the strong half of the
//     invariant exactly rather than as a range, that no checkpointed chunk is
//     lost and the held chunk is re-analyzed once and only once. Chunks run
//     several at a time, so a real kill can lose as many as are in flight, and
//     each of those is the held chunk's case repeated.
//  4. An admitted delta completes in one invocation and no clock spans more
//     than one model call. Proven here. The clock half is also held from the
//     other side by TestNoModelCallInheritsAnEarlierChunksClock.
//  5. A failed run leaves the check red and every review object untouched.
//     Proven by TestAFailedRunChangesNoReviewState and
//     TestAFailedRunKeepsTheLastReviewedCommit in review_test.go.
//  6. One top level comment exists per pull request, forever. Proven by
//     TestEveryTopLevelCommentWriteCarriesTheStateMarker and
//     TestServiceFailureNoticeEditsTheExistingSummaryComment in review_test.go,
//     and by TestUpsertSummaryCommentCreatesOnceThenUpdatesInPlace in
//     summary_comment_test.go.
//  7. One run identifier reaches the check, the comment, and the log lines.
//     Proven here. Retrieving those lines from the deployed service is an
//     operational procedure, not a unit; docs/logs.md owns it.
//  8. A delta over budget is never attempted, and the check does not reach a
//     passing conclusion. Proven by
//     TestServiceDeclinesAnOverBudgetDeltaBeforeAnyModelCall,
//     TestAnOverBudgetDeltaReconcilesNothingAndCallsNoModel,
//     TestADeclinedDeltaKeepsItsBaselineSoALaterPushCannotBypassTheBudget,
//     TestARedeliveryOfADeclinedHeadIsDeclinedAgain, and
//     TestADeclinedDeltaKeepsTheWorkAnEarlierRunRecorded in review_test.go.
//  9. Every blocking verdict names the open threads holding it. Proven by
//     TestABlockingVerdictNamesTheOpenThreadsHoldingIt in review_test.go.
//  10. No run approves over its own fresh findings, and none approves a commit
//     it did not analyze. The first clause is proven by
//     TestARunThatPostsANewFindingDoesNotApprove in review_test.go. The second
//     is proven here by TestARunWhoseHeadMovedUnderItSubmitsNoVerdict.
//     TestServiceCancelsWhenHeadChangesBeforePublication covers the head guard
//     on the comment path rather than the reload in publish, and does not fail
//     when that reload is removed, so it is not a home for this clause.

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"goodkind.io/pr-review-agent/internal/config"
	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
	"goodkind.io/pr-review-agent/internal/marker"
	"goodkind.io/pr-review-agent/internal/review"
)

// invariantChunkCount is the delta size these tests review. It is deliberately
// larger than the concurrency bound, so the chunks run in more than one wave.
// That is what lets a kill land between chunks rather than before any of them,
// and what makes a later wave's clock observable at all.
const invariantChunkCount = 6

// invariantChunkTimeout is the per call budget these tests configure. It is
// long enough that no call can reach it, so a short budget means a call
// inherited a clock rather than that the work was slow.
const invariantChunkTimeout = 5 * time.Second

// sixChunkCollector returns one padded file per chunk, each past the prompt
// size so the chunker cannot merge two of them.
type sixChunkCollector struct{}

func (sixChunkCollector) CollectRange(
	_ context.Context,
	_ domain.PullRequestRef,
	pullRequest githubapp.PullRequest,
	_ domain.HeadSHA,
) (diff.ReviewInput, error) {
	return paddedFiles(pullRequest, invariantChunkCount)
}

// promptFilePath reads which file a chunk prompt is about, so a model double
// can answer per chunk and a test can count what actually reached the model.
func promptFilePath(prompt string) string {
	const field = "File: "
	start := strings.Index(prompt, field)
	if start < 0 {
		return ""
	}
	rest := prompt[start+len(field):]
	if end := strings.IndexByte(rest, '\n'); end >= 0 {
		return rest[:end]
	}
	return rest
}

// chunkFinding is the defect a model double reports against one chunk's own
// file, anchored to the line paddedFiles adds.
func chunkFinding(path string) domain.Finding {
	return domain.Finding{
		Path:       path,
		StartLine:  2,
		EndLine:    2,
		Title:      "Defect in " + path,
		Body:       "The changed line breaks core behavior.",
		Suggestion: "",
		Importance: 9,
	}
}

func chunkAnswer(path string) review.Completion {
	return review.Completion{
		Result: domain.ReviewResult{
			CoverageComplete: true,
			Findings:         []domain.Finding{chunkFinding(path)},
		},
		Model: testReviewModel,
	}
}

// requireMultipleWaves fails a test whose premise has quietly stopped holding.
// These tests only mean what they claim while the delta needs more than one
// wave of chunks, so a change to the concurrency bound must be seen here rather
// than silently weakening them.
func requireMultipleWaves(t *testing.T) {
	t.Helper()
	if invariantChunkCount <= config.MaximumChunkConcurrency {
		t.Fatalf("delta of %d chunks fits one wave of %d: these tests need more than one wave",
			invariantChunkCount, config.MaximumChunkConcurrency)
	}
}

// killableModel answers every chunk with a finding on that chunk's own file,
// except the one path it is told to hold. That call blocks until the run's
// context dies, which is the moment a kill lands: several chunks have posted
// and checkpointed, and one is still in flight.
type killableModel struct {
	mu       sync.Mutex
	holdPath string
	released bool
	seen     []string
}

func (model *killableModel) Review(ctx context.Context, prompt string) (review.Completion, error) {
	path := promptFilePath(prompt)

	model.mu.Lock()
	model.seen = append(model.seen, path)
	holding := !model.released && path == model.holdPath
	model.mu.Unlock()

	if holding {
		<-ctx.Done()
		return review.Completion{}, ctx.Err()
	}
	return chunkAnswer(path), nil
}

// release lets the held chunk answer, which is what the run after the kill
// finds when it asks for it again.
func (model *killableModel) release() {
	model.mu.Lock()
	defer model.mu.Unlock()
	model.released = true
}

func (model *killableModel) timesAsked(path string) int {
	model.mu.Lock()
	defer model.mu.Unlock()
	count := 0
	for _, seen := range model.seen {
		if seen == path {
			count++
		}
	}
	return count
}

// currentSummaryBody returns the service's own top level comment while a run is
// still going, under the lock the fixture server writes it with.
func (state *serviceServerState) currentSummaryBody() string {
	state.mu.Lock()
	defer state.mu.Unlock()
	for _, comment := range state.issueComments {
		body, _ := comment["body"].(string)
		if marker.HasState(body) {
			return body
		}
	}
	return ""
}

// awaitCompletedChunks waits until the durable comment records count chunks as
// read. A kill before that point proves nothing, because no checkpoint has
// happened yet and there is no surviving progress to lose.
func awaitCompletedChunks(t *testing.T, fixture *serviceFixture, count int) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if state, ok := marker.DecodeState(fixture.state.currentSummaryBody()); ok {
			if len(state.Completed) >= count {
				return
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("only %q was ever recorded, want %d chunks read before the kill",
		fixture.state.currentSummaryBody(), count)
}

// postedPaths returns the file each posted inline comment objects to.
func postedPaths(fixture *serviceFixture) []string {
	fixture.state.mu.Lock()
	defer fixture.state.mu.Unlock()
	paths := make([]string, 0, len(fixture.state.streamedComments))
	for _, comment := range fixture.state.streamedComments {
		path, _ := comment["path"].(string)
		paths = append(paths, path)
	}
	return paths
}

// Invariant 3. Killing the process loses only the chunks in flight.
//
// Exactly one chunk is in flight here, by construction: the model holds
// file3.go while every other chunk checkpoints, and the kill lands there. That
// is deliberately narrower than the invariant, and it is what makes the
// surviving work an exact number rather than a range. A kill with more chunks
// in flight is this same case repeated once per chunk held.
//
// The kill is a cancelled run context, which is the closest a test gets to the
// real thing: it stops the model calls in flight, and publication is bound to
// the service lifetime, so nothing further reaches GitHub. What survives is
// exactly what the checkpoints already wrote, which is the whole point of
// checkpointing after every chunk.
//
// The run after the kill must therefore ask the model about the lost chunk and
// nothing else, and the pull request must end up carrying every chunk's finding
// exactly once, none duplicated and none missing.
func TestKillingTheProcessLosesOnlyTheChunksInFlight(t *testing.T) {
	requireMultipleWaves(t)
	const heldPath = "file3.go"

	model := &killableModel{holdPath: heldPath}
	fixture := newServiceFixture(t, serviceFixtureOptions{
		collector:         sixChunkCollector{},
		minimumImportance: 9,
		model:             model,
	})

	ctx, kill := context.WithCancel(context.Background())
	killed := make(chan error, 1)
	go func() {
		killed <- fixture.run(ctx, fixture.job())
	}()

	awaitCompletedChunks(t, fixture, invariantChunkCount-1)
	kill()
	if err := <-killed; err == nil {
		t.Fatal("Run: want the killed run to report that it stopped")
	}

	survived := decodedSummaryState(t, fixture)
	if len(survived.Completed) != invariantChunkCount-1 {
		t.Fatalf("completed after the kill = %v, want every chunk but the one in flight", survived.Completed)
	}
	if len(survived.Pending) != 1 {
		t.Fatalf("pending after the kill = %v, want the one chunk that was in flight", survived.Pending)
	}
	if survived.LastReviewed != "" {
		t.Fatalf("last reviewed = %q, want no claim on a head that was never finished", survived.LastReviewed)
	}
	if posted := len(postedPaths(fixture)); posted != invariantChunkCount-1 {
		t.Fatalf("comments posted before the kill = %d, want one per checkpointed chunk", posted)
	}

	// The next invocation finds the surviving state and finishes the work.
	model.release()
	if err := fixture.run(context.Background(), fixture.job()); err != nil {
		t.Fatalf("Run after the kill: %v", err)
	}

	// Only the lost chunk is paid for twice. A checkpointed chunk that came back
	// to the model would mean the death cost more than the one chunk in flight.
	for index := range invariantChunkCount {
		path := "file" + string(rune('0'+index)) + ".go"
		want := 1
		if path == heldPath {
			want = 2
		}
		if asked := model.timesAsked(path); asked != want {
			t.Fatalf("%s was analyzed %d times, want %d", path, asked, want)
		}
	}

	// Every chunk's finding is on the page, once each.
	paths := postedPaths(fixture)
	if len(paths) != invariantChunkCount {
		t.Fatalf("comments posted = %d, want one per chunk: %v", len(paths), paths)
	}
	seen := make(map[string]int, len(paths))
	for _, path := range paths {
		seen[path]++
	}
	for index := range invariantChunkCount {
		path := "file" + string(rune('0'+index)) + ".go"
		if seen[path] != 1 {
			t.Fatalf("%s carries %d comments, want exactly one: %v", path, seen[path], paths)
		}
	}

	finished := decodedSummaryState(t, fixture)
	if finished.Status != marker.StateDone {
		t.Fatalf("status = %q, want %q", finished.Status, marker.StateDone)
	}
	if finished.LastReviewed != domain.HeadSHA(testHeadSHA) {
		t.Fatalf("last reviewed = %q, want the head now that every chunk answered", finished.LastReviewed)
	}
	if len(finished.Pending) != 0 {
		t.Fatalf("pending = %v, want none", finished.Pending)
	}
}

// budgetProbeModel answers each chunk with a finding on its own file and
// records the budget that call was given, measured from that call's own start.
// It burns real time in the first wave so a later wave starts visibly later,
// which is where a clock shared across calls would show up.
type budgetProbeModel struct {
	mu        sync.Mutex
	firstWait time.Duration
	calls     int
	budgets   []time.Duration
	undated   int
}

func (model *budgetProbeModel) Review(ctx context.Context, prompt string) (review.Completion, error) {
	started := time.Now()
	deadline, dated := ctx.Deadline()

	model.mu.Lock()
	if !dated {
		model.undated++
	}
	model.budgets = append(model.budgets, deadline.Sub(started))
	model.calls++
	inFirstWave := model.calls <= config.MaximumChunkConcurrency
	model.mu.Unlock()

	if inFirstWave {
		time.Sleep(model.firstWait)
	}
	return chunkAnswer(promptFilePath(prompt)), nil
}

// shortestBudget reports the least time any one call was given, how many calls
// there were, and how many carried no deadline at all.
func (model *budgetProbeModel) shortestBudget() (time.Duration, int, int) {
	model.mu.Lock()
	defer model.mu.Unlock()
	shortest := time.Duration(0)
	for index, budget := range model.budgets {
		if index == 0 || budget < shortest {
			shortest = budget
		}
	}
	return shortest, len(model.budgets), model.undated
}

// Invariant 4. An admitted delta completes in one invocation, and no clock
// spans more than one model call.
//
// Both halves are one property. Admission is what bounds a run, so a delta it
// admitted has to finish now rather than leaving a remainder for a later push,
// and it can only do that if no timer above the chunks can expire part way
// through and throw away what was already read.
func TestAnAdmittedDeltaCompletesInOneInvocation(t *testing.T) {
	requireMultipleWaves(t)
	const firstWaveWork = 500 * time.Millisecond

	model := &budgetProbeModel{firstWait: firstWaveWork}
	fixture := newServiceFixture(t, serviceFixtureOptions{
		collector:         sixChunkCollector{},
		minimumImportance: 9,
		model:             model,
		chunkTimeout:      invariantChunkTimeout,
	})

	if err := fixture.run(context.Background(), fixture.job()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// One invocation finished the whole delta.
	state := decodedSummaryState(t, fixture)
	if state.Status != marker.StateDone {
		t.Fatalf("status = %q, want %q after one invocation", state.Status, marker.StateDone)
	}
	if state.LastReviewed != domain.HeadSHA(testHeadSHA) {
		t.Fatalf("last reviewed = %q, want the head", state.LastReviewed)
	}
	if len(state.Pending) != 0 {
		t.Fatalf("pending = %v, want none: an admitted delta leaves no remainder", state.Pending)
	}
	if conclusion := fixture.state.lastUpdateCheckRun["conclusion"]; conclusion != "success" {
		t.Fatalf("conclusion = %v, want success", conclusion)
	}

	// Every chunk's findings reached the page in that one invocation.
	paths := postedPaths(fixture)
	if len(paths) != invariantChunkCount {
		t.Fatalf("comments posted = %d, want one per chunk: %v", len(paths), paths)
	}

	// No clock spanned two calls.
	shortest, calls, undated := model.shortestBudget()
	if undated != 0 {
		t.Fatalf("%d model calls carried no deadline, want every call bounded", undated)
	}
	if calls != invariantChunkCount {
		t.Fatalf("model calls = %d, want one per chunk", calls)
	}
	// A single clock over the whole pass would leave the later wave short by the
	// time the first wave spent. A clock per call leaves every call its whole
	// budget, whenever it starts.
	if shortest < invariantChunkTimeout-firstWaveWork/2 {
		t.Fatalf("shortest call budget = %s, want close to %s: a later call inherited an earlier clock",
			shortest, invariantChunkTimeout)
	}
}

// syncBuffer collects the service log. A review writes it from the several
// goroutines it runs chunks on, so the writes are guarded, and it is read once
// the run has returned.
type syncBuffer struct {
	mu      sync.Mutex
	written bytes.Buffer
}

func (sink *syncBuffer) Write(payload []byte) (int, error) {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	return sink.written.Write(payload)
}

func (sink *syncBuffer) String() string {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	return sink.written.String()
}

// runIdentifiersIn returns every run identifier a rendered surface names, so a
// test can prove one string reaches it rather than one string beside another.
func runIdentifiersIn(text string) []string {
	const field = "delivery_id="
	found := make([]string, 0)
	rest := text
	for {
		start := strings.Index(rest, field)
		if start < 0 {
			return found
		}
		rest = rest[start+len(field):]
		end := strings.IndexAny(rest, " \n")
		if end < 0 {
			return append(found, rest)
		}
		found = append(found, rest[:end])
		rest = rest[end:]
	}
}

// Invariant 7. The run identifier on the check, the comment, and the log lines
// is the same string.
//
// It is the only handle a person has for pulling one run out of the service
// log, which is where every cause now lives: no public surface reprints what a
// failure said. A second identifier on any of the three surfaces makes that
// lookup a guess, so this asserts every identifier each surface names, not
// merely that the right one appears somewhere.
//
// Retrieving those lines from the deployed service is a procedure rather than a
// unit, and docs/logs.md owns it.
func TestTheRunIdentifierIsTheSameStringEverywhere(t *testing.T) {
	logs := &syncBuffer{}
	fixture := newServiceFixture(t, serviceFixtureOptions{
		minimumImportance: 9,
		logWriter:         logs,
		model: &sequenceModel{
			results: []domain.ReviewResult{{CoverageComplete: true, Findings: nil}},
		},
	})

	job := fixture.job()
	if job.DeliveryID == "" {
		t.Fatal("the job carries no delivery id, so this test could pass on an empty string")
	}
	if err := fixture.run(context.Background(), job); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// The comment. The marker is what the next invocation reads, so the
	// identifier a reader sees is the one the durable state records.
	if state := decodedSummaryState(t, fixture); state.RunID != job.DeliveryID {
		t.Fatalf("comment run id = %q, want %q", state.RunID, job.DeliveryID)
	}

	// The check. The run's own log is published in the check run body, and
	// every line of it carries the identifier.
	checkText, ok := checkOutput(t, fixture)["text"].(string)
	if !ok {
		t.Fatalf("check output text = %v, want the published run log", checkOutput(t, fixture)["text"])
	}
	assertOnlyRunIdentifier(t, "the check run", checkText, job.DeliveryID)

	// The logs. This is the surface the failure causes are pulled from.
	assertOnlyRunIdentifier(t, "the service log", logs.String(), job.DeliveryID)
}

// Invariant 10, second clause. No run approves a commit it did not analyze.
//
// The head guard that protects the inline comments runs only when there are
// comments to post, so a run that finds nothing never reaches it. The reload in
// publish is then the only thing between a push landing mid run and an approval
// earned by the commit before it. Removing that reload broke no test before
// this one, so the clause was carried by a guard that a clean review skips.
func TestARunWhoseHeadMovedUnderItSubmitsNoVerdict(t *testing.T) {
	fixture := newServiceFixture(t, serviceFixtureOptions{
		minimumImportance: 9,
		headAfterAnalysis: testStaleHeadSHA,
		model: &sequenceModel{
			results: []domain.ReviewResult{{CoverageComplete: true, Findings: nil}},
		},
	})

	if err := fixture.run(context.Background(), fixture.job()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Nothing was posted, so the comment path's head guard never ran and the
	// reload in publish is the only guard this run passed through.
	if posted := len(postedPaths(fixture)); posted != 0 {
		t.Fatalf("comments posted = %d, want none: this run found nothing", posted)
	}
	if fixture.state.lastSubmitReview != nil {
		t.Fatalf("submitted review = %v, want none: the head moved off the commit this run analyzed",
			fixture.state.lastSubmitReview)
	}
	if fixture.state.lastUpdateReview != nil {
		t.Fatalf("updated review = %v, want no verdict at all", fixture.state.lastUpdateReview)
	}
	if conclusion := fixture.state.lastUpdateCheckRun["conclusion"]; conclusion != "cancelled" {
		t.Fatalf("conclusion = %v, want cancelled", conclusion)
	}
}

// assertOnlyRunIdentifier proves one surface names the run identifier and names
// no other, so a reader who filters on it gets this run and only this run.
func assertOnlyRunIdentifier(t *testing.T, surface string, text string, want string) {
	t.Helper()
	found := runIdentifiersIn(text)
	if len(found) == 0 {
		t.Fatalf("%s names no run identifier:\n%s", surface, text)
	}
	for _, identifier := range found {
		if identifier != want {
			t.Fatalf("%s names run identifier %q beside %q, so filtering on one loses the rest",
				surface, identifier, want)
		}
	}
}
