//go:build integration

package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"goodkind.io/pr-review-agent/internal/cloudflareops"
	"goodkind.io/pr-review-agent/internal/config"
	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
	"goodkind.io/pr-review-agent/internal/openai"
	"goodkind.io/pr-review-agent/internal/review"
	"goodkind.io/pr-review-agent/internal/reviewrules"
)

func TestLiveReviewScopeWithoutProviderRequests(t *testing.T) {
	if os.Getenv("PR_AGENT_LIVE_SCOPE_AUDIT") != "1" {
		t.Skip("The read-only live scope audit is not enabled.")
	}
	expectedHead := os.Getenv("PR_AGENT_LIVE_SCOPE_HEAD")
	if expectedHead == "" {
		t.Fatal("The scope audit requires an exact expected pull-request head.")
	}
	opts := liveReviewBodyOptions(t)
	repo, err := reviewBodyRepository(opts.repository)
	if err != nil {
		t.Fatal("The scope audit repository is invalid.")
	}
	identity, err := reviewBodyIdentity(opts)
	if err != nil {
		t.Fatal("The read-only GitHub app identity could not be loaded.")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	github := githubapp.NewClient(identity, &http.Client{Timeout: 30 * time.Second}, time.Now, slog.Default())
	pr, err := github.GetPullRequest(ctx, opts.installationID, repo, opts.pullRequest)
	if err != nil || string(pr.Head) != expectedHead {
		t.Fatal("The live pull-request head does not match the requested audit revision.")
	}
	input, err := diff.NewCollector(github).Collect(ctx, domain.PullRequestRef{Repository: repo, Number: opts.pullRequest, InstallationID: opts.installationID, Head: pr.Head}, pr)
	if err != nil {
		t.Fatalf("The production GitHub collector could not read the audit input: %v", err)
	}
	cfg := scopeAuditConfiguration(t, opts)
	model := openai.NewClient(cfg, nil)
	maximum := cfg.PromptBytes()
	raw, err := diff.ChunkInput(input, maximum)
	if err != nil {
		t.Fatal(err)
	}
	decisions, unresolved, err := review.PlanFileAssessments(input, cfg.ReviewPolicy)
	if err != nil {
		t.Fatal(err)
	}
	excluded := 0
	for _, decision := range decisions {
		if decision.PermitsContentExclusion() && cfg.ReviewPolicy.AllowsContentExclusion(decision.ContentKind) {
			excluded++
		}
	}
	t.Logf("head=%s files=%d raw_chunks=%d metadata_surfaces=%d deterministic_decisions=%d excluded_files=%d unresolved_files=%d maximum_bytes=%d configured_chunk_limit=%d", pr.Head, len(input.Files), len(raw), len(input.Metadata), len(decisions), excluded, len(unresolved), maximum, cfg.ReviewMaxChunks)
	for _, file := range unresolved {
		t.Logf("unresolved_file=%s gap=%s patch_bytes=%d", file.Path, file.Gap, len(file.Patch))
	}
	if len(unresolved) > 0 {
		t.Fatal("The read-only audit cannot replace an unresolved file's model classification with an invented decision.")
	}
	planned, _, err := review.PlanFileContentReview(input, decisions, cfg.ReviewPolicy)
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range planned.Files {
		if file.Gap != diff.CoverageGapNone || !file.CoverageComplete {
			t.Fatalf("The audit cannot assume ordinary source coverage for %s with gap %s.", file.Path, file.Gap)
		}
	}
	initial, err := diff.ChunkInput(planned, maximum)
	if err != nil {
		t.Fatal(err)
	}
	var data reviewrules.PromptData
	required, err := cfg.ReviewPolicy.Render("omissions.none", data)
	if err != nil {
		t.Fatal(err)
	}
	optional, err := review.BuildPullRequestContext(input.PullRequest, input.Files, cfg.ReviewPolicy)
	if err != nil {
		t.Fatal(err)
	}
	bounded, err := review.BoundModelReviewChunksWithContext(model, initial, input.Files, cfg.MinimumImportance, required, optional, cfg.ReviewPolicy, maximum)
	if err != nil {
		t.Fatal(err)
	}
	combined, err := review.PrepareRelationshipChunks(model, bounded, input.Files, cfg.MinimumImportance, required, optional, cfg.ReviewPolicy, maximum)
	if err != nil {
		t.Fatal(err)
	}
	ordinary, metadataOnly, relationships, gaps := 0, 0, 0, 0
	for _, chunk := range combined {
		if chunk.RelationshipReview {
			if chunk.CoverageComplete {
				relationships++
			} else {
				gaps++
				t.Logf("relationship_gap=%s", strings.TrimSpace(chunk.Text))
			}
			continue
		}
		ordinary++
		if len(chunk.Metadata) == len(chunk.Pieces) {
			metadataOnly++
		}
		if !chunk.CoverageComplete {
			t.Fatal("The SDK request-size audit requires explicit accounting for an ordinary source gap.")
		}
	}
	current, err := github.GetPullRequest(ctx, opts.installationID, repo, opts.pullRequest)
	if err != nil || current.Head != pr.Head || diff.PullRequestRevision(current) != diff.PullRequestRevision(pr) {
		t.Fatal("The pull-request head or metadata changed during the read-only scope audit.")
	}
	t.Logf("reviewable_files=%d post_classification_chunks=%d sdk_bounded_ordinary_chunks=%d final_ordinary_chunks=%d metadata_only_chunks=%d joint_relationship_requests=%d relationship_gaps=%d total_assessment_chunks=%d", len(planned.Files), len(initial), len(bounded), ordinary, metadataOnly, relationships, gaps, len(combined))
	decision := review.CheckReviewBudget(len(planned.Files), combined, cfg.ReviewMaxFiles, cfg.ReviewMaxChunks)
	if decision.ReviewableChunks != ordinary || decision.Skip != (len(planned.Files) > cfg.ReviewMaxFiles || ordinary > cfg.ReviewMaxChunks) {
		t.Fatalf("The production budget decision counted derived relationships or gap records as ordinary content: %+v; ordinary=%d total=%d.", decision, ordinary, len(combined))
	}
	t.Logf("production_admission_skip=%t reviewable_chunk_count=%d reason=%q", decision.Skip, decision.ReviewableChunks, decision.Reason)
}

func scopeAuditConfiguration(t *testing.T, opts reviewBodyOptions) config.Config {
	t.Helper()
	key, err := cloudflareops.ReadCredential(opts.privateKeyFile)
	if err != nil {
		t.Fatal("The GitHub private-key file could not be read.")
	}
	data, err := os.ReadFile(opts.runtime)
	if err != nil {
		t.Fatal(err)
	}
	var runtime map[string]json.RawMessage
	if err := json.Unmarshal(data, &runtime); err != nil {
		t.Fatal(err)
	}
	var definitions []struct {
		ID                   string `json:"id"`
		CFAccessClientID     string `json:"cf_access_client_id_binding"`
		CFAccessClientSecret string `json:"cf_access_client_secret_binding"`
	}
	if err := json.Unmarshal(runtime["PROVIDERS"], &definitions); err != nil {
		t.Fatal(err)
	}
	secrets := map[string]bool{"GITHUB_WEBHOOK_SECRET": true}
	for _, definition := range definitions {
		prefix := "PROVIDER_" + strings.ToUpper(definition.ID)
		secrets[prefix+"_API_KEY"] = true
		if definition.CFAccessClientID != "" {
			secrets[prefix+"_CF_ACCESS_CLIENT_ID"] = true
		}
		if definition.CFAccessClientSecret != "" {
			secrets[prefix+"_CF_ACCESS_CLIENT_SECRET"] = true
		}
	}
	for _, name := range []string{"REVIEW_RULES_FILE", "REVIEW_PROMPTS_FILE"} {
		var path string
		if json.Unmarshal(runtime[name], &path) != nil || path == "" {
			t.Fatal("The public runtime policy path is missing.")
		}
		if !filepath.IsAbs(path) {
			path = filepath.Join(filepath.Dir(opts.runtime), path)
		}
		runtime[name], err = json.Marshal(path)
		if err != nil {
			t.Fatal(err)
		}
	}
	data, err = json.Marshal(runtime)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadRuntime(data, func(name string) (string, bool) {
		if name == "GITHUB_PRIVATE_KEY" {
			return string(key), true
		}
		if secrets[name] {
			return "scope-audit-unused-credential", true
		}
		return "", false
	})
	if err != nil {
		t.Fatalf("The public runtime could not configure SDK request serialization: %v", err)
	}
	return cfg
}
