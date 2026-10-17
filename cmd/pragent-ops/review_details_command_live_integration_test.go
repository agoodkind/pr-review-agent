//go:build integration

package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/marker"
	"goodkind.io/pr-review-agent/internal/review"
)

func TestLiveReviewDetailsCommandRepairsAndRepeats(t *testing.T) {
	if os.Getenv("PR_AGENT_LIVE_REVIEW_DETAILS_COMMAND_TEST") != "1" {
		t.Skip("The live review-details command regression is not enabled.")
	}
	fixture := liveDetailsSetup(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	identifier, err := strconv.ParseInt(os.Getenv("PR_AGENT_LIVE_SOURCE_CHECK_RUN_ID"), 10, 64)
	if err != nil || identifier <= 0 {
		t.Fatal("The command regression requires an explicit successful source check identifier.")
	}
	source, err := fixture.client.GetCheckRunDetails(ctx, fixture.opts.installationID, fixture.before.Repository, identifier)
	if err != nil {
		t.Fatalf("The source check could not be read: %v", err)
	}
	if source.Run.Head != fixture.before.PullRequest.Head || source.Run.Conclusion != "success" {
		t.Fatal("The selected source must be a successful check on the fixture's current head.")
	}
	original := fixture.original.Body
	metadata := liveDetailsSection(t, original, "<!-- pr-review-agent:metadata:v1 ", " -->")
	fixture.write(t, ctx, strings.Replace(original, metadata, "", 1))
	outcome, failure := fixture.run(t, ctx)
	if failure == nil || outcome.Disposition != domain.AssessmentFailed {
		t.Fatal("The real provider refusal did not create the failed summary needed for repair.")
	}
	if err := fixture.logFile.Sync(); err != nil {
		t.Fatalf("The real provider failure log could not be synchronized: %v", err)
	}
	liveFailureAssertProvider(t, fixture.logPath, fixture.cfg.Providers[0].ID, fixture.cfg.Providers[0].Model)
	body := fixture.capture(t, ctx)
	state, valid := marker.DecodeState(body)
	if !valid || state.Status != marker.StateFailed {
		t.Fatal("The provider refusal did not create a failed checkpoint.")
	}
	body = strings.Replace(body, marker.EncodeState(state), metadata+"\n"+marker.EncodeState(state), 1)
	damaged, err := review.RestoreReviewDetails(body, review.RenderDetails(review.Summary{Head: fixture.before.PullRequest.Head}), fixture.before.Threads, fixture.cfg.GitHubBotLogin)
	if err != nil {
		t.Fatalf("The fixture could not reproduce the erased details: %v", err)
	}
	fixture.write(t, ctx, damaged)
	damagedDetails := liveDetailsSection(t, damaged, "<details>\n<summary>Review details</summary>", "</details>")
	sourceDetails := liveDetailsSection(t, source.Summary, "<details>\n<summary>Review details</summary>", "</details>")
	failureNotice := liveDetailsSection(t, damaged, "<!-- pr-review-agent:maintenance-failure:start -->", "<!-- pr-review-agent:maintenance-failure:end -->")
	expectedBody := strings.Replace(damaged, damagedDetails, sourceDetails, 1)
	expectedBody = strings.TrimSpace(strings.Replace(expectedBody, failureNotice, "", 1))
	completedState := state
	completedState.Status = marker.StateDone
	expectedBody = strings.Replace(expectedBody, marker.EncodeState(state), marker.EncodeState(completedState), 1)
	backup := filepath.Join(filepath.Dir(fixture.logPath), "command-before.json")
	arguments := []string{"review-details", "--runtime", fixture.opts.runtime, "--repository", fixture.opts.repository, "--pull-request", strconv.Itoa(fixture.opts.pullRequest), "--installation-id", strconv.FormatInt(fixture.opts.installationID, 10), "--app-private-key-file", fixture.opts.privateKeyFile, "--summary-comment-id", strconv.FormatInt(fixture.original.ID, 10), "--source-check-run-id", strconv.FormatInt(identifier, 10), "--backup-file", backup}
	for index, apply := range []bool{false, true, true} {
		selected := append([]string(nil), arguments...)
		if apply {
			selected = append(selected, "--apply")
		}
		var output bytes.Buffer
		commandErr := run(ctx, selected, &output, io.Discard)
		current := fixture.capture(t, ctx)
		if commandErr != nil {
			t.Fatalf("The real review-details command failed: %v", commandErr)
		}
		if !apply {
			if current != damaged {
				t.Fatal("The dry run changed the summary.")
			}
			continue
		}
		if current != expectedBody {
			t.Fatal("The repair changed content outside the selected details, failure notice, and checkpoint status.")
		}
		info, statErr := os.Stat(backup)
		if statErr != nil {
			t.Fatalf("The private backup could not be inspected: %v", statErr)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatal("The repair did not retain its private original backup.")
		}
		if index == 2 && !strings.Contains(output.String(), "requires repair: false") {
			t.Fatal("The repeated command did not report an unchanged summary.")
		}
	}
	t.Log("The command restored measured details, cleared the failure notice, and saved a private backup. The dry run and repetition did not change the comment.")
}
