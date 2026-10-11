package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"goodkind.io/pr-review-agent/internal/cloudflareops"
	"goodkind.io/pr-review-agent/internal/config"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
	"goodkind.io/pr-review-agent/internal/marker"
)

const reviewBodyRequestTimeout = 30 * time.Second
const reviewBodyRollbackTimeout = 2 * time.Minute

type reviewBodyOptions struct {
	runtime        string
	appID          string
	botLogin       string
	privateKeyFile string
	repository     string
	pullRequest    int
	installationID int64
	backupFile     string
	apply          bool
}

type reviewBodyRuntime struct {
	AppID    string `json:"GITHUB_APP_ID"`
	BotLogin string `json:"GITHUB_BOT_LOGIN"`
}

type reviewBodySnapshot struct {
	Repository     domain.Repository        `json:"repository"`
	Number         int                      `json:"pull_request"`
	InstallationID int64                    `json:"installation_id"`
	CapturedAt     time.Time                `json:"captured_at"`
	PullRequest    githubapp.PullRequest    `json:"pull_request_metadata"`
	Reviews        []githubapp.Review       `json:"reviews"`
	Comments       []githubapp.IssueComment `json:"issue_comments"`
	Threads        []githubapp.ReviewThread `json:"review_threads"`
}

func reviewBodies(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	opts, err := reviewBodyFlags(args, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	if err != nil {
		return err
	}
	repo, err := reviewBodyRepository(opts.repository)
	if err != nil {
		return err
	}
	cfg, err := reviewBodyIdentity(opts)
	if err != nil {
		return err
	}
	client := githubapp.NewClient(cfg, &http.Client{Timeout: reviewBodyRequestTimeout}, time.Now, slog.Default())
	snapshot, err := captureReviewBodies(ctx, client, opts.installationID, repo, opts.pullRequest)
	if err != nil {
		return err
	}
	if err := validateReviewBodySummary(snapshot, cfg.GitHubBotLogin); err != nil {
		return err
	}
	selected, clear := selectReviewBodies(snapshot.Reviews, cfg.GitHubBotLogin)
	if !opts.apply {
		return printReviewBodyPlan(stdout, selected, clear, false)
	}
	if len(selected) == 0 {
		return printReviewBodyPlan(stdout, selected, clear, true)
	}
	if err := writeReviewBodyBackup(opts.backupFile, snapshot); err != nil {
		return err
	}
	if err := applyReviewBodies(ctx, client, snapshot, selected); err != nil {
		return err
	}
	return printReviewBodyPlan(stdout, selected, clear, true)
}

func reviewBodyFlags(args []string, output io.Writer) (reviewBodyOptions, error) {
	var opts reviewBodyOptions
	set := flag.NewFlagSet("review-bodies", flag.ContinueOnError)
	set.SetOutput(output)
	set.StringVar(&opts.runtime, "runtime", "runtime.json", "Read the GitHub app ID and bot login from this public runtime file.")
	set.StringVar(&opts.appID, "app-id", "", "Use this GitHub app ID instead of the runtime value.")
	set.StringVar(&opts.botLogin, "bot-login", "", "Use this exact bot login instead of the runtime value.")
	set.StringVar(&opts.privateKeyFile, "app-private-key-file", "", "Read the GitHub app RSA private key from this file.")
	set.StringVar(&opts.repository, "repository", "", "Select the target repository using OWNER/REPO.")
	set.IntVar(&opts.pullRequest, "pull-request", 0, "Select this pull request number.")
	set.Int64Var(&opts.installationID, "installation-id", 0, "Authenticate with this GitHub app installation ID.")
	set.StringVar(&opts.backupFile, "backup-file", "", "Create this private backup file before applying cleanup.")
	set.BoolVar(&opts.apply, "apply", false, "Clear eligible bodies after backing up and verifying the target.")
	if err := set.Parse(args); err != nil {
		return opts, err
	}
	if set.NArg() != 0 {
		return opts, errors.New("review-bodies does not accept positional arguments")
	}
	if opts.repository == "" || opts.pullRequest <= 0 || opts.installationID <= 0 || opts.privateKeyFile == "" {
		return opts, errors.New("review-bodies requires repository, pull-request, installation-id, and app-private-key-file")
	}
	if opts.apply && opts.backupFile == "" {
		return opts, errors.New("review-bodies --apply requires a new backup-file")
	}
	return opts, nil
}

func reviewBodyRepository(value string) (domain.Repository, error) {
	var repo domain.Repository
	parts := strings.Split(value, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" || strings.ContainsAny(value, "?#\\ \t\r\n") {
		return repo, errors.New("repository must use OWNER/REPO without whitespace or URL parameters")
	}
	repo.Owner, repo.Name = parts[0], parts[1]
	return repo, nil
}

func reviewBodyIdentity(opts reviewBodyOptions) (config.Config, error) {
	var cfg config.Config
	if opts.appID == "" || opts.botLogin == "" {
		data, err := os.ReadFile(opts.runtime)
		if err != nil {
			return cfg, errors.New("public runtime configuration could not be read")
		}
		var runtime reviewBodyRuntime
		if err := json.Unmarshal(data, &runtime); err != nil {
			return cfg, errors.New("public runtime GitHub identity could not be decoded")
		}
		if opts.appID == "" {
			opts.appID = runtime.AppID
		}
		if opts.botLogin == "" {
			opts.botLogin = runtime.BotLogin
		}
	}
	appID, err := strconv.ParseInt(opts.appID, 10, 64)
	if err != nil || appID <= 0 {
		return cfg, errors.New("GitHub app ID must be a positive integer")
	}
	key, err := cloudflareops.ReadCredential(opts.privateKeyFile)
	if err != nil {
		return cfg, errors.New("GitHub app private-key file could not be read")
	}
	return config.LoadGitHubIdentity(appID, opts.botLogin, key)
}

func captureReviewBodies(ctx context.Context, client *githubapp.Client, installationID int64, repo domain.Repository, number int) (reviewBodySnapshot, error) {
	var snapshot reviewBodySnapshot
	snapshot.Repository, snapshot.Number, snapshot.InstallationID = repo, number, installationID
	snapshot.CapturedAt = time.Now().UTC()
	var err error
	snapshot.PullRequest, err = client.GetPullRequest(ctx, installationID, repo, number)
	if err != nil {
		return snapshot, errors.New("pull request metadata could not be read")
	}
	snapshot.Reviews, err = client.ListReviews(ctx, installationID, repo, number)
	if err != nil {
		return snapshot, errors.New("pull request reviews could not be read")
	}
	snapshot.Comments, err = client.ListIssueComments(ctx, installationID, repo, number)
	if err != nil {
		return snapshot, errors.New("pull request summary comments could not be read")
	}
	snapshot.Threads, err = client.ListReviewThreads(ctx, installationID, repo, number)
	if err != nil {
		return snapshot, errors.New("pull request inline comments could not be read")
	}
	return snapshot, nil
}

func validateReviewBodySummary(snapshot reviewBodySnapshot, botLogin string) error {
	count := 0
	for _, comment := range snapshot.Comments {
		if comment.Author != botLogin || !marker.HasState(comment.Body) {
			continue
		}
		count++
		state, valid := marker.DecodeState(comment.Body)
		if !valid || state.Status != marker.StateDone || state.LastReviewed != snapshot.PullRequest.Head || len(state.Pending) != 0 || len(state.Unread) != 0 {
			return errors.New("the bot summary must record a completed review of the current head")
		}
	}
	if count != 1 {
		return fmt.Errorf("the pull request has %d bot summary checkpoints and requires one", count)
	}
	return nil
}

func selectReviewBodies(reviews []githubapp.Review, botLogin string) ([]githubapp.Review, []githubapp.Review) {
	var selected []githubapp.Review
	var clear []githubapp.Review
	for _, item := range reviews {
		if item.Author != botLogin {
			continue
		}
		if item.State != "APPROVED" && item.State != "CHANGES_REQUESTED" && item.State != "DISMISSED" {
			continue
		}
		if item.Body == "" {
			clear = append(clear, item)
			continue
		}
		if _, valid := marker.FindReview(item.Body); !valid || !reviewBodyHidden(item.Body) {
			continue
		}
		selected = append(selected, item)
	}
	return selected, clear
}

func reviewBodyHidden(body string) bool {
	remaining := strings.TrimSpace(body)
	for remaining != "" {
		if !strings.HasPrefix(remaining, "<!--") {
			return false
		}
		end := strings.Index(remaining[4:], "-->")
		if end < 0 {
			return false
		}
		remaining = strings.TrimSpace(remaining[end+7:])
	}
	return true
}

func writeReviewBodyBackup(path string, snapshot reviewBodySnapshot) (returnErr error) {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return errors.New("the new private review-body backup could not be created")
	}
	defer func() {
		returnErr = errors.Join(returnErr, file.Close())
	}()
	if err := file.Chmod(0o600); err != nil {
		return errors.New("the review-body backup permissions could not be set")
	}
	if err := json.NewEncoder(file).Encode(snapshot); err != nil {
		return errors.New("the original review bodies could not be backed up")
	}
	if err := file.Sync(); err != nil {
		return errors.New("the review-body backup could not be synchronized")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return errors.New("the review-body backup could not be read for verification")
	}
	var restored reviewBodySnapshot
	if json.Unmarshal(data, &restored) != nil || !reflect.DeepEqual(restored, snapshot) {
		return errors.New("the review-body backup does not match the original snapshot")
	}
	return nil
}

func applyReviewBodies(ctx context.Context, client *githubapp.Client, snapshot reviewBodySnapshot, selected []githubapp.Review) (returnErr error) {
	attempted := make([]githubapp.Review, 0, len(selected))
	defer func() {
		if returnErr == nil || len(attempted) == 0 {
			return
		}
		rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), reviewBodyRollbackTimeout)
		defer cancel()
		returnErr = errors.Join(returnErr, rollbackReviewBodies(rollbackCtx, client, snapshot, attempted))
	}()
	for _, original := range selected {
		current, err := client.GetReview(ctx, snapshot.InstallationID, snapshot.Repository, snapshot.Number, original.ID)
		if err != nil || current != original {
			return fmt.Errorf("review %d no longer matches its original snapshot", original.ID)
		}
		// A failed response may follow a successful remote write.
		attempted = append(attempted, original)
		updated, err := client.UpdateReview(ctx, snapshot.InstallationID, snapshot.Repository, snapshot.Number, original.ID, "")
		if err != nil {
			return fmt.Errorf("review %d body could not be cleared", original.ID)
		}
		expected := original
		expected.Body = ""
		if updated != expected {
			return fmt.Errorf("review %d did not retain its original decision after body cleanup", original.ID)
		}
	}
	return verifyReviewBodySnapshot(ctx, client, snapshot, selected)
}

func rollbackReviewBodies(ctx context.Context, client *githubapp.Client, snapshot reviewBodySnapshot, attempted []githubapp.Review) error {
	var failures []error
	for _, original := range slices.Backward(attempted) {
		current, err := client.GetReview(ctx, snapshot.InstallationID, snapshot.Repository, snapshot.Number, original.ID)
		if err != nil || current.ID != original.ID || current.Author != original.Author {
			failures = append(failures, fmt.Errorf("review %d could not be read for body restoration", original.ID))
			continue
		}
		if current.Body == original.Body {
			continue
		}
		if current.Body != "" {
			failures = append(failures, fmt.Errorf("review %d contains a different body and was not overwritten during restoration", original.ID))
			continue
		}
		restored, err := client.UpdateReview(ctx, snapshot.InstallationID, snapshot.Repository, snapshot.Number, original.ID, original.Body)
		if err != nil || restored.ID != original.ID || restored.Author != original.Author || restored.Body != original.Body || restored.State != current.State {
			failures = append(failures, fmt.Errorf("review %d body could not be restored without changing its decision", original.ID))
		}
	}
	rollbackErr := errors.Join(errors.Join(failures...), verifyReviewBodySnapshot(ctx, client, snapshot, nil))
	if rollbackErr != nil {
		slog.WarnContext(ctx, "Review body restoration failed", "review_count", len(attempted))
	}
	return rollbackErr
}

func verifyReviewBodySnapshot(ctx context.Context, client *githubapp.Client, original reviewBodySnapshot, cleared []githubapp.Review) error {
	current, err := captureReviewBodies(ctx, client, original.InstallationID, original.Repository, original.Number)
	if err != nil {
		return err
	}
	if current.PullRequest != original.PullRequest {
		return errors.New("pull request metadata changed during review-body cleanup")
	}
	if len(current.Reviews) != len(original.Reviews) {
		return errors.New("the review count changed during review-body cleanup")
	}
	for _, item := range original.Reviews {
		expected := item
		if slices.ContainsFunc(cleared, func(review githubapp.Review) bool { return review.ID == item.ID }) {
			expected.Body = ""
		}
		if !slices.Contains(current.Reviews, expected) {
			return fmt.Errorf("review %d no longer matches its expected decision and body", item.ID)
		}
	}
	if !reflect.DeepEqual(current.Comments, original.Comments) {
		return errors.New("a summary comment changed during review-body cleanup")
	}
	if !reflect.DeepEqual(current.Threads, original.Threads) {
		return errors.New("an inline comment changed during review-body cleanup")
	}
	return nil
}

func printReviewBodyPlan(output io.Writer, selected, clear []githubapp.Review, applied bool) error {
	for _, item := range selected {
		if applied {
			if _, err := fmt.Fprintf(output, "Review %d retains state %s after body cleanup.\n", item.ID, item.State); err != nil {
				return err
			}
		} else {
			if _, err := fmt.Fprintf(output, "Review %d has state %s and requires body cleanup.\n", item.ID, item.State); err != nil {
				return err
			}
		}
	}
	for _, item := range clear {
		if _, err := fmt.Fprintf(output, "Review %d has state %s and an empty body.\n", item.ID, item.State); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintf(output, "The selection includes %d bodies for cleanup and %d empty bodies.\n", len(selected), len(clear))
	return err
}
