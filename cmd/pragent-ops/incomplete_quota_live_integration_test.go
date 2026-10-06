//go:build integration

package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"goodkind.io/pr-review-agent/internal/config"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/marker"
)

type incompleteQuotaTarget struct {
	repository string
	number     string
	botLogin   string
	appID      int64
}

type incompleteQuotaPullRequest struct {
	State string `json:"state"`
	Title string `json:"title"`
	Body  string `json:"body"`
	Draft bool   `json:"draft"`
	Head  struct {
		SHA string `json:"sha"`
		Ref string `json:"ref"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
}

type incompleteQuotaCheck struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	ExternalID string `json:"external_id"`
	App        struct {
		ID int64 `json:"id"`
	} `json:"app"`
	Output struct {
		Title   string `json:"title"`
		Summary string `json:"summary"`
	} `json:"output"`
}

type incompleteQuotaReview struct {
	ID       int64  `json:"id"`
	CommitID string `json:"commit_id"`
	State    string `json:"state"`
	User     struct {
		Login string `json:"login"`
	} `json:"user"`
}

func TestLiveIncompleteQuotaPresentation(t *testing.T) {
	if os.Getenv("PR_AGENT_LIVE_INCOMPLETE_QUOTA_TEST") != "1" {
		t.Skip("The live incomplete quota regression is not enabled.")
	}
	target := incompleteQuotaLoadTarget(t)
	ctx, stop := signal.NotifyContext(t.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	original := incompleteQuotaReadPR(t, ctx, target)
	if original.State != "open" || original.Head.SHA == "" {
		t.Fatal("The explicitly selected pull request must be open and have a head commit.")
	}
	previous := incompleteQuotaChecks(t, ctx, target, original.Head.SHA)
	previousReviews := incompleteQuotaReviews(t, ctx, target)
	label := domain.ForceReviewLabelPrefix + "incomplete-quota-" + strings.ToLower(rand.Text())
	labelCreated := false
	t.Cleanup(func() {
		cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), time.Minute)
		defer cancelCleanup()
		if labelCreated {
			if _, err := incompleteQuotaCommand(cleanupCtx, "pr", "edit", target.number, "--repo", target.repository, "--remove-label", label); err != nil {
				t.Errorf("The test label could not be removed from the pull request: %v.", err)
			}
			if _, err := incompleteQuotaCommand(cleanupCtx, "label", "delete", label, "--repo", target.repository, "--yes"); err != nil {
				t.Errorf("The uniquely created repository label could not be deleted: %v.", err)
			}
		}
		current := incompleteQuotaReadPR(t, cleanupCtx, target)
		if !reflect.DeepEqual(current, original) {
			t.Error("The original head, metadata, or labels changed during the live test.")
		}
		incompleteQuotaNoNewVerdict(t, original.Head.SHA, target.botLogin, previousReviews, incompleteQuotaReviews(t, cleanupCtx, target))
	})
	if _, err := incompleteQuotaCommand(ctx, "label", "create", label, "--repo", target.repository, "--description", "Live regression for quota presentation with unread hunks."); err != nil {
		t.Fatalf("The uniquely named test label could not be created: %v.", err)
	}
	labelCreated = true
	if _, err := incompleteQuotaCommand(ctx, "pr", "edit", target.number, "--repo", target.repository, "--add-label", label); err != nil {
		t.Fatalf("The test label could not trigger the real GitHub webhook: %v.", err)
	}
	check := incompleteQuotaAwaitCheck(t, ctx, target, original.Head.SHA, previous)
	body, state := incompleteQuotaSummary(t, ctx, target, check.ExternalID)
	_, details, _ := strings.Cut(body, "<details>")
	quotaRefused := strings.Contains(details, "Provider reported no remaining usage") || strings.Contains(details, "App denied request; API not called")
	for _, otherOutcome := range []string{"API completed the response", "Provider request failed", "Provider rate limited the request", "App quota check failed"} {
		if strings.Contains(details, otherOutcome) {
			quotaRefused = false
		}
	}
	omissionsUndecided := strings.Contains(check.Output.Title, "has not decided whether") &&
		strings.Contains(body, "has not decided whether these unread changes are necessary") &&
		strings.Contains(body, "larger than one model request allows") && len(state.Pending) > 0
	if !quotaRefused || !omissionsUndecided {
		t.Skipf("Check %d completed, but the selected fixture no longer establishes quota exhaustion with undecided oversized hunks.", check.ID)
	}
	if check.Conclusion != "success" {
		t.Fatalf("Check %d concluded %s; quota presentation with undecided omissions must conclude success.", check.ID, check.Conclusion)
	}
	if !strings.Contains(check.Output.Summary, "has not decided whether these unread changes are necessary") ||
		!strings.Contains(body, "The review submitted no verdict.") {
		t.Fatal("The completed check or summary omitted the undecided review notice.")
	}
	t.Logf("Check %d succeeded with undecided oversized hunks, quota-refusal diagnostics, and %d pending chunks.", check.ID, len(state.Pending))
}

func incompleteQuotaLoadTarget(t *testing.T) incompleteQuotaTarget {
	t.Helper()
	repository := os.Getenv("PR_AGENT_LIVE_REPOSITORY")
	number := os.Getenv("PR_AGENT_LIVE_PULL_REQUEST")
	parsed, err := strconv.Atoi(number)
	if err != nil || parsed < 1 || len(strings.Split(repository, "/")) != 2 {
		t.Fatal("The live test requires an explicit repository and positive pull request number.")
	}
	data, err := os.ReadFile(filepath.Join("..", "..", "runtime.json"))
	if err != nil {
		t.Fatal("The configured GitHub app identity could not be read.")
	}
	var identity struct {
		BotLogin string `json:"GITHUB_BOT_LOGIN"`
		AppID    string `json:"GITHUB_APP_ID"`
	}
	if err := json.Unmarshal(data, &identity); err != nil {
		t.Fatal("The configured GitHub app identity could not be decoded.")
	}
	appID, err := strconv.ParseInt(identity.AppID, 10, 64)
	if err != nil || appID < 1 || identity.BotLogin == "" {
		t.Fatal("The runtime configuration must identify the review app and bot.")
	}
	return incompleteQuotaTarget{repository: repository, number: number, botLogin: identity.BotLogin, appID: appID}
}

func incompleteQuotaCommand(ctx context.Context, args ...string) ([]byte, error) {
	output, err := exec.CommandContext(ctx, "gh", args...).Output()
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return nil, fmt.Errorf("the GitHub operation exited with status %d", exit.ExitCode())
		}
		return nil, errors.New("the GitHub executable could not start")
	}
	return output, nil
}

func incompleteQuotaJSON[T any](t *testing.T, ctx context.Context, args ...string) T {
	t.Helper()
	data, err := incompleteQuotaCommand(ctx, args...)
	if err != nil {
		t.Fatalf("The required GitHub data could not be read: %v.", err)
	}
	var result T
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal("The required GitHub data could not be decoded.")
	}
	return result
}

func incompleteQuotaReadPR(t *testing.T, ctx context.Context, target incompleteQuotaTarget) incompleteQuotaPullRequest {
	t.Helper()
	pr := incompleteQuotaJSON[incompleteQuotaPullRequest](t, ctx, "api", fmt.Sprintf("repos/%s/pulls/%s", target.repository, target.number))
	slices.SortFunc(pr.Labels, func(left, right struct {
		Name string `json:"name"`
	}) int {
		return strings.Compare(left.Name, right.Name)
	})
	return pr
}

func incompleteQuotaChecks(t *testing.T, ctx context.Context, target incompleteQuotaTarget, head string) []incompleteQuotaCheck {
	t.Helper()
	type page struct {
		CheckRuns []incompleteQuotaCheck `json:"check_runs"`
	}
	pages := incompleteQuotaJSON[[]page](t, ctx, "api", "--paginate", "--slurp",
		fmt.Sprintf("repos/%s/commits/%s/check-runs?filter=all&per_page=100", target.repository, head))
	var checks []incompleteQuotaCheck
	for _, page := range pages {
		for _, check := range page.CheckRuns {
			if check.Name == config.ReviewCheckName && check.App.ID == target.appID {
				checks = append(checks, check)
			}
		}
	}
	return checks
}

func incompleteQuotaAwaitCheck(t *testing.T, ctx context.Context, target incompleteQuotaTarget, head string, previous []incompleteQuotaCheck) incompleteQuotaCheck {
	t.Helper()
	var admitted int64
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		if admitted == 0 {
			var candidates []incompleteQuotaCheck
			for _, check := range incompleteQuotaChecks(t, ctx, target, head) {
				if !slices.ContainsFunc(previous, func(old incompleteQuotaCheck) bool { return old.ID == check.ID }) {
					candidates = append(candidates, check)
				}
			}
			if len(candidates) > 1 {
				t.Fatal("Multiple new review checks prevent identifying the label-triggered run.")
			}
			if len(candidates) == 1 {
				admitted = candidates[0].ID
			}
		}
		if admitted != 0 {
			check := incompleteQuotaJSON[incompleteQuotaCheck](t, ctx, "api",
				fmt.Sprintf("repos/%s/check-runs/%d", target.repository, admitted))
			if check.Status == "completed" {
				return check
			}
		}
		select {
		case <-ctx.Done():
			t.Fatal("The newly admitted review check did not finish within the bounded live test.")
		case <-ticker.C:
		}
	}
}

func incompleteQuotaSummary(t *testing.T, ctx context.Context, target incompleteQuotaTarget, delivery string) (string, marker.State) {
	t.Helper()
	type comment struct {
		Body string `json:"body"`
		User struct {
			Login string `json:"login"`
		} `json:"user"`
	}
	pages := incompleteQuotaJSON[[][]comment](t, ctx, "api", "--paginate", "--slurp",
		fmt.Sprintf("repos/%s/issues/%s/comments?per_page=100", target.repository, target.number))
	for _, page := range pages {
		for _, comment := range page {
			state, found := marker.DecodeState(comment.Body)
			if found && delivery != "" && state.RunID == delivery && comment.User.Login == target.botLogin && strings.Contains(comment.Body, marker.Summary()) {
				return comment.Body, state
			}
		}
	}
	t.Fatal("The newly completed check has no matching current summary marker.")
	return "", marker.State{}
}

func incompleteQuotaReviews(t *testing.T, ctx context.Context, target incompleteQuotaTarget) []incompleteQuotaReview {
	t.Helper()
	pages := incompleteQuotaJSON[[][]incompleteQuotaReview](t, ctx, "api", "--paginate", "--slurp",
		fmt.Sprintf("repos/%s/pulls/%s/reviews?per_page=100", target.repository, target.number))
	var reviews []incompleteQuotaReview
	for _, page := range pages {
		reviews = append(reviews, page...)
	}
	return reviews
}

func incompleteQuotaNoNewVerdict(t *testing.T, head, bot string, before, after []incompleteQuotaReview) {
	t.Helper()
	for _, review := range after {
		if review.User.Login != bot || review.CommitID != head || (review.State != "APPROVED" && review.State != "CHANGES_REQUESTED") {
			continue
		}
		if !slices.ContainsFunc(before, func(previous incompleteQuotaReview) bool {
			return previous.ID == review.ID && previous.State == review.State
		}) {
			t.Error("The untouched head received a new bot approval or changes-requested verdict.")
		}
	}
}
