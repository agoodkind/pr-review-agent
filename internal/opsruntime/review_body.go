package opsruntime

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
	"goodkind.io/pr-review-agent/internal/marker"
	"goodkind.io/pr-review-agent/internal/review"
	"goodkind.io/pr-review-agent/internal/reviewrules"
)

var errMissingPolicyPaths = errors.New(
	"runtime configuration requires review rule, prompt, service, and template paths for summary links",
)

// ReviewBodyRuntime stores identity and policy paths for review body commands.
//
// For example:
//
//	runtime := ReviewBodyRuntime{RulesFile: "config/review-rules.json"}
type ReviewBodyRuntime struct {
	AppID              string `json:"GITHUB_APP_ID"`
	BotLogin           string `json:"GITHUB_BOT_LOGIN"`
	RulesFile          string `json:"REVIEW_RULES_FILE"`
	PromptsFile        string `json:"REVIEW_PROMPTS_FILE"`
	ServiceFile        string `json:"REVIEW_SERVICE_FILE"`
	TemplatesDirectory string `json:"REVIEW_TEMPLATES_DIR"`
}

// ReviewBodySummaryPrefix renders the configured link to a bot summary.
//
// For example:
//
//	prefix, err := ReviewBodySummaryPrefix(path, repo, 7, comments, bot)
func ReviewBodySummaryPrefix(
	runtimePath string,
	repository domain.Repository,
	number int,
	comments []githubapp.IssueComment,
	bot string,
) (string, error) {
	data, err := os.ReadFile(runtimePath)
	if err != nil {
		slog.Warn("Diagnostic runtime operation failed", "err", err)

		return "", fmt.Errorf("load review body policy: %w", err)
	}

	var runtime ReviewBodyRuntime
	if err := json.Unmarshal(data, &runtime); err != nil {
		slog.Warn("Diagnostic runtime operation failed", "err", err)

		return "", fmt.Errorf("load review body policy: %w", err)
	}

	if runtime.RulesFile == "" || runtime.PromptsFile == "" || runtime.ServiceFile == "" ||
		runtime.TemplatesDirectory == "" {
		return "", errMissingPolicyPaths
	}

	for _, reference := range []*string{
		&runtime.RulesFile,
		&runtime.PromptsFile,
		&runtime.ServiceFile,
		&runtime.TemplatesDirectory,
	} {
		if !filepath.IsAbs(*reference) {
			*reference = filepath.Join(filepath.Dir(runtimePath), *reference)
		}
	}

	policy, err := reviewrules.LoadPolicy(
		runtime.RulesFile,
		runtime.PromptsFile,
		runtime.ServiceFile,
		runtime.TemplatesDirectory,
	)
	if err != nil {
		slog.Warn("Diagnostic runtime operation failed", "err", err)

		return "", fmt.Errorf("load review body policy: %w", err)
	}

	var summaryID int64

	for _, comment := range comments {
		if comment.Author == bot && marker.HasState(comment.Body) {
			summaryID = comment.ID
		}
	}

	prefix, err := review.NativeReviewSummaryLink(policy, repository, number, summaryID, "")
	if err != nil {
		slog.Warn("Diagnostic runtime operation failed", "err", err)

		return "", fmt.Errorf("render review summary link: %w", err)
	}

	return prefix, nil
}
