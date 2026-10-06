//go:build integration

package main

import (
	_ "embed"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"goodkind.io/pr-review-agent/internal/opsstats"
)

//go:embed stats_verify.graphql
var statsVerificationQuery string

func TestLiveStatsReportsStandingGitHubReviews(t *testing.T) {
	if os.Getenv("PR_AGENT_STATS_LIVE") != "1" {
		t.Skip("Set PR_AGENT_STATS_LIVE=1 to read live GitHub and Cloudflare evidence.")
	}
	input := os.Getenv("PR_AGENT_STATS_INPUT")
	accountToken := os.Getenv("PR_AGENT_STATS_ACCOUNT_TOKEN_FILE")
	if (input == "") == (accountToken == "") {
		t.Fatal("Provide exactly one real cached capture or account token file.")
	}
	directory := filepath.Join(t.TempDir(), "stats")
	if retained := os.Getenv("PR_AGENT_STATS_OUTPUT_DIR"); retained != "" {
		directory = retained
	}
	args := []string{"run", ".", "stats", "--runtime", "../../runtime.json", "--wrangler", "../../deploy/cloudflare/wrangler.jsonc", "--output-dir", directory}
	if input != "" {
		args = append(args, "--input", input)
	} else {
		args = append(args, "--since", "24h", "--account-token-file", accountToken)
	}
	if owner := os.Getenv("PR_AGENT_STATS_OWNER"); owner != "" {
		args = append(args, "--owner", owner)
	}
	if exclude := os.Getenv("PR_AGENT_STATS_EXCLUDE"); exclude != "" {
		args = append(args, "--exclude", exclude)
	}
	if token := os.Getenv("PR_AGENT_STATS_OPERATOR_TOKEN_FILE"); token != "" {
		args = append(args, "--operator-token-file", token)
	}
	command := exec.CommandContext(t.Context(), "go", args...)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("Public stats command failed: %v; diagnostics: %s", err, output)
	}
	data, err := os.ReadFile(filepath.Join(directory, "stats.json"))
	if err != nil {
		t.Fatal(err)
	}
	var report opsstats.Report
	if err = json.Unmarshal(data, &report); err != nil {
		t.Fatal(err)
	}
	if !report.GitHubComplete || len(report.GitHub) == 0 {
		t.Fatal("The public command did not capture complete live GitHub evidence.")
	}
	for _, pr := range report.GitHub {
		if len(pr.Reviews.Nodes) != pr.Reviews.TotalCount || len(pr.ReviewThreads.Nodes) != pr.ReviewThreads.TotalCount {
			t.Fatal("The public command truncated GitHub review or thread pages.")
		}
		for _, thread := range pr.ReviewThreads.Nodes {
			if len(thread.Comments.Nodes) != thread.Comments.TotalCount {
				t.Fatal("The public command truncated a thread comment connection.")
			}
		}
	}
	for _, summary := range report.PullRequests {
		if summary.Excluded {
			continue
		}
		if !statsVerificationTargets(report)[summary.Target.String()] {
			continue
		}
		parts := strings.SplitN(summary.Target.Repository, "/", 2)
		data, err = exec.CommandContext(t.Context(), "gh", "api", "graphql", "-f", "query="+statsVerificationQuery, "-f", "owner="+parts[0], "-f", "name="+parts[1], "-F", "number="+strconv.FormatInt(summary.Target.Number, 10)).Output()
		if err != nil {
			t.Fatalf("Independent live GitHub read failed: %v", err)
		}
		var oracle struct {
			Errors []json.RawMessage `json:"errors"`
			Data   struct {
				Repository struct {
					PullRequest opsstats.PullRequest `json:"pullRequest"`
				} `json:"repository"`
			} `json:"data"`
		}
		if err = json.Unmarshal(data, &oracle); err != nil {
			t.Fatal(err)
		}
		pr := oracle.Data.Repository.PullRequest
		if len(oracle.Errors) > 0 || pr.HeadRefOID == "" {
			t.Fatal("Independent GitHub verification returned incomplete data.")
		}
		if pr.LatestOpinionatedReviews.PageInfo.HasNextPage {
			t.Fatal("Independent standing-review verification requires additional pages.")
		}
		standing := 0
		for _, review := range pr.LatestOpinionatedReviews.Nodes {
			if review.Author != nil && strings.TrimSuffix(review.Author.Login, "[bot]") == strings.TrimSuffix(report.BotLogin, "[bot]") && review.State == "CHANGES_REQUESTED" {
				standing++
			}
		}
		if summary.StandingBotRejections != standing {
			t.Fatalf("Public report counts %d standing rejections; live GitHub reports %d.", summary.StandingBotRejections, standing)
		}
	}
	info, err := os.Stat(filepath.Join(directory, "stats.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatal("The stats report does not use private file permissions.")
	}
}

func statsVerificationTargets(report opsstats.Report) map[string]bool {
	targets := make(map[string]bool)
	selected := make(map[bool]bool)
	for _, pr := range report.GitHub {
		target := opsstats.Target{Repository: pr.Repository.NameWithOwner, Number: pr.Number}
		if slices.Contains(report.Excluded, target.String()) {
			continue
		}
		historical, standing := false, false
		for _, review := range pr.Reviews.Nodes {
			if review.Author != nil && strings.TrimSuffix(review.Author.Login, "[bot]") == strings.TrimSuffix(report.BotLogin, "[bot]") && review.State == "CHANGES_REQUESTED" {
				historical = true
			}
		}
		for _, review := range pr.LatestOpinionatedReviews.Nodes {
			if review.Author != nil && strings.TrimSuffix(review.Author.Login, "[bot]") == strings.TrimSuffix(report.BotLogin, "[bot]") && review.State == "CHANGES_REQUESTED" {
				standing = true
			}
		}
		if historical && !selected[standing] {
			targets[target.String()] = true
			selected[standing] = true
		}
	}
	if len(targets) == 0 && len(report.GitHub) > 0 {
		pr := report.GitHub[0]
		targets[(opsstats.Target{Repository: pr.Repository.NameWithOwner, Number: pr.Number}).String()] = true
	}
	return targets
}
