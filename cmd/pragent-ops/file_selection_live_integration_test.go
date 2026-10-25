//go:build integration

package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
	"goodkind.io/pr-review-agent/internal/openai"
	"goodkind.io/pr-review-agent/internal/review"
)

func TestLiveDependencyReviewExcludesLockfileContentsBeforeChunking(t *testing.T) {
	if os.Getenv("PR_AGENT_LIVE_FILE_SELECTION_TEST") != "1" {
		t.Skip("The live file selection regression is not enabled.")
	}
	opts := liveReviewBodyOptions(t)
	cfg, _ := liveFailureConfig(t)
	repository, err := reviewBodyRepository(opts.repository)
	if err != nil {
		t.Fatal("The explicit repository is invalid.")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	client := githubapp.NewClient(cfg, &http.Client{Timeout: 30 * time.Second}, time.Now, slog.Default())
	pullRequest, err := client.GetPullRequest(ctx, opts.installationID, repository, opts.pullRequest)
	if err != nil {
		t.Fatal("The real dependency pull request could not be read.")
	}
	reference := domain.PullRequestRef{Repository: repository, Number: opts.pullRequest, InstallationID: opts.installationID, Head: pullRequest.Head}
	input, err := diff.NewCollector(client).Collect(ctx, reference, pullRequest)
	if err != nil {
		t.Fatal("The real dependency changes could not be collected.")
	}
	const lockPath = "deploy/cloudflare/package-lock.json"
	const manifestPath = "deploy/cloudflare/package.json"
	if len(input.Files) != 2 || !slices.ContainsFunc(input.Files, func(file diff.FileContext) bool { return file.Path == lockPath }) || !slices.ContainsFunc(input.Files, func(file diff.FileContext) bool { return file.Path == manifestPath }) {
		t.Fatal("Select the dependency regression with its lockfile and manifest changes.")
	}
	assessments, unresolved, err := review.PlanFileAssessments(input, cfg.ReviewPolicy)
	if err != nil || len(unresolved) != 0 || len(assessments) != len(input.Files) {
		t.Fatal("The configured deterministic policy did not assess both dependency files.")
	}
	for _, assessment := range assessments {
		if assessment.Path == lockPath && (!assessment.PermitsContentExclusion() || len(assessment.Evidence) == 0) {
			t.Fatal("The lockfile assessment did not justify its content exclusion.")
		}
		if assessment.Path == manifestPath && assessment.Action != domain.FileActionReview {
			t.Fatal("The manifest change did not retain ordinary content review.")
		}
	}
	planned, decisions, err := review.PlanFileContentReview(input, assessments, cfg.ReviewPolicy)
	if err != nil || len(planned.Files) != 1 || planned.Files[0].Path != manifestPath {
		t.Fatal("The scope plan did not exclude only the generated lockfile.")
	}
	before, err := diff.ChunkInput(input, cfg.PromptBytes())
	if err != nil {
		t.Fatal(err)
	}
	after, err := diff.ChunkInput(planned, cfg.PromptBytes())
	if err != nil || len(after) == 0 {
		t.Fatal("The scope plan did not create review chunks for the manifest and metadata.")
	}
	model := openai.NewClient(cfg, http.DefaultClient)
	manifestReviewed := false
	for _, chunk := range after {
		if slices.Contains(chunk.Paths, lockPath) {
			t.Fatal("A generated lockfile still requires a content review chunk.")
		}
		manifestReviewed = manifestReviewed || slices.Contains(chunk.Paths, manifestPath)
		prompt, err := review.BuildPrompt(chunk, cfg.MinimumImportance, "", cfg.ReviewPolicy, cfg.PromptBytes())
		if err != nil {
			t.Fatal("The actual review prompt could not be built.")
		}
		payloads, err := model.ReviewRequestPayloads(prompt)
		if err != nil || len(payloads) == 0 {
			t.Fatal("The provider SDK could not serialize the selected review content.")
		}
		for _, payload := range payloads {
			if strings.Contains(string(payload.Body), `lockfileVersion`) {
				t.Fatal("The serialized review request still contains the excluded lockfile body.")
			}
		}
	}
	if !manifestReviewed || len(after) >= len(before) {
		t.Fatal("The file selection did not reduce generated review work while retaining the manifest.")
	}
	var summary review.Summary
	summary.FileAssessments = decisions
	summary.FileAssessmentMaximumBytes = cfg.ReviewPolicy.FileAssessmentMaximumBytes()
	summary.FileAssessmentsDisplay = cfg.ReviewPolicy.FileAssessmentsDisplay()
	body := review.RenderBody(summary)
	if !strings.Contains(body, lockPath) || !strings.Contains(body, "<!-- pr-review-agent:file-assessments:v1 ") {
		t.Fatal("The rendered review did not retain the diagnostic assessment and its evidence.")
	}
	t.Logf("Real GitHub collection at %s produced %d chunks before selection and %d afterward. The manifest requires review; the lockfile contents do not require provider requests.", pullRequest.Head, len(before), len(after))
}
