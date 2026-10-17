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
	"path/filepath"
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
	"goodkind.io/pr-review-agent/internal/review"
	"goodkind.io/pr-review-agent/internal/reviewrules"
)

const reviewBodyRequestTimeout = 30 * time.Second
const reviewBodyRollbackTimeout = 2 * time.Minute

type reviewBodyIDs []int64

func (ids *reviewBodyIDs) String() string {
	values := make([]string, 0, len(*ids))
	for _, id := range *ids {
		values = append(values, strconv.FormatInt(id, 10))
	}
	return strings.Join(values, ",")
}

func (ids *reviewBodyIDs) Set(value string) error {
	id, err := strconv.ParseInt(value, 10, 64)
	if err != nil || id <= 0 {
		return errors.New("review-id must be a positive integer")
	}
	*ids = append(*ids, id)
	return nil
}

type reviewBodyOptions struct {
	reviewIDs      reviewBodyIDs
	runtime        string
	appID          string
	botLogin       string
	privateKeyFile string
	repository     string
	pullRequest    int
	installationID int64
	backupFile     string
	apply          bool
	linkSummary    bool
}

type reviewBodyRuntime struct {
	AppID       string `json:"GITHUB_APP_ID"`
	BotLogin    string `json:"GITHUB_BOT_LOGIN"`
	RulesFile   string `json:"REVIEW_RULES_FILE"`
	PromptsFile string `json:"REVIEW_PROMPTS_FILE"`
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

type reviewBodyOperationError struct {
	operation string
	reviewID  int64
	cause     error
}

func (failure *reviewBodyOperationError) Error() string {
	return fmt.Sprintf("Review %d failed during %s.", failure.reviewID, failure.operation)
}

func (failure *reviewBodyOperationError) Unwrap() error {
	return failure.cause
}

type reviewBodyDiagnostic struct {
	HTTPStatus  int    `json:"http_status"`
	Operation   string `json:"operation,omitempty"`
	ReviewID    int64  `json:"review_id,omitempty"`
	NativeError string `json:"native_error"`
}

type reviewBodyDiagnosticError struct {
	message string
	cause   error
}

func (failure *reviewBodyDiagnosticError) Error() string {
	return failure.message
}

func (failure *reviewBodyDiagnosticError) Unwrap() error {
	return failure.cause
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
	prefix, err := reviewBodySummaryPrefix(opts, snapshot, cfg.GitHubBotLogin)
	if err != nil {
		return err
	}
	if opts.linkSummary {
		selected = slices.DeleteFunc(selected, func(item githubapp.Review) bool { return item.State == "APPROVED" })
		for _, item := range snapshot.Reviews {
			if item.Author != cfg.GitHubBotLogin || item.State == "APPROVED" {
				continue
			}
			if item.State == "COMMENTED" && reviewBodyHidden(item.Body) {
				if _, valid := marker.FindReview(item.Body); valid {
					selected = append(selected, item)
				}
			}
			if !strings.HasPrefix(item.Body, prefix) {
				continue
			}
			checkpoint := strings.TrimPrefix(item.Body, prefix)
			if _, valid := marker.FindReview(checkpoint); valid && reviewBodyHidden(checkpoint) {
				clear = append(clear, item)
			}
		}
	}
	selected, clear, err = restrictReviewBodies(selected, clear, opts.reviewIDs)
	if err != nil {
		return err
	}
	replacements := slices.Clone(selected)
	for index := range replacements {
		if opts.linkSummary {
			replacements[index].Body = prefix + replacements[index].Body
		} else {
			replacements[index].Body = ""
		}
	}
	if !opts.apply {
		return printReviewBodyPlan(stdout, selected, clear, false, opts.linkSummary)
	}
	if len(selected) == 0 {
		return printReviewBodyPlan(stdout, selected, clear, true, opts.linkSummary)
	}
	if err := writeReviewBodyBackup(opts.backupFile, snapshot); err != nil {
		return err
	}
	if err := applyReviewBodies(ctx, client, snapshot, selected, replacements); err != nil {
		var apiError githubapp.APIError
		if errors.As(err, &apiError) {
			return reportReviewBodyError(opts.backupFile, err)
		}
		return err
	}
	return printReviewBodyPlan(stdout, selected, clear, true, opts.linkSummary)
}

func reviewBodySummaryPrefix(opts reviewBodyOptions, snapshot reviewBodySnapshot, bot string) (string, error) {
	if !opts.linkSummary {
		return "", nil
	}
	data, err := os.ReadFile(opts.runtime)
	if err != nil {
		return "", err
	}
	var runtime reviewBodyRuntime
	if err := json.Unmarshal(data, &runtime); err != nil {
		return "", err
	}
	if runtime.RulesFile == "" || runtime.PromptsFile == "" {
		return "", errors.New("runtime configuration requires review rule and prompt files for summary links")
	}
	policy, err := reviewrules.LoadPolicy(filepath.Join(filepath.Dir(opts.runtime), runtime.RulesFile), filepath.Join(filepath.Dir(opts.runtime), runtime.PromptsFile))
	if err != nil {
		return "", err
	}
	var summaryID int64
	for _, comment := range snapshot.Comments {
		if comment.Author == bot && marker.HasState(comment.Body) {
			summaryID = comment.ID
		}
	}
	return review.NativeReviewSummaryLink(policy, snapshot.Repository, snapshot.Number, summaryID, "")
}

func reviewBodyFlags(args []string, output io.Writer) (reviewBodyOptions, error) {
	var opts reviewBodyOptions
	set := flag.NewFlagSet("review-bodies", flag.ContinueOnError)
	set.SetOutput(output)
	set.Var(&opts.reviewIDs, "review-id", "Select an exact review ID. Repeat the flag to select additional IDs.")
	set.StringVar(&opts.runtime, "runtime", "runtime.json", "Read the GitHub app ID and bot login from this public runtime file.")
	set.StringVar(&opts.appID, "app-id", "", "Use this GitHub app ID instead of the runtime value.")
	set.StringVar(&opts.botLogin, "bot-login", "", "Use this exact bot login instead of the runtime value.")
	set.StringVar(&opts.privateKeyFile, "app-private-key-file", "", "Read the GitHub app RSA private key from this file.")
	set.StringVar(&opts.repository, "repository", "", "Select the target repository using OWNER/REPO.")
	set.IntVar(&opts.pullRequest, "pull-request", 0, "Select this pull request number.")
	set.Int64Var(&opts.installationID, "installation-id", 0, "Authenticate with this GitHub app installation ID.")
	set.StringVar(&opts.backupFile, "backup-file", "", "Create this private backup file before applying cleanup.")
	set.BoolVar(&opts.apply, "apply", false, "Apply selected body repairs after backing up and verifying the target.")
	set.BoolVar(&opts.linkSummary, "link-summary", false, "Add the configured summary permalink to hidden review bodies without changing their decisions or checkpoint data.")
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

func restrictReviewBodies(selected, clear []githubapp.Review, ids reviewBodyIDs) ([]githubapp.Review, []githubapp.Review, error) {
	if len(ids) == 0 {
		return selected, clear, nil
	}
	found := make(map[int64]bool, len(ids))
	for _, id := range ids {
		found[id] = false
	}
	exclude := func(review githubapp.Review) bool {
		if _, requested := found[review.ID]; !requested {
			return true
		}
		found[review.ID] = true
		return false
	}
	selected = slices.DeleteFunc(selected, exclude)
	clear = slices.DeleteFunc(clear, exclude)
	for _, id := range ids {
		if !found[id] {
			return nil, nil, fmt.Errorf("review %d is not an eligible bot review with a hidden or empty body", id)
		}
	}
	return selected, clear, nil
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

func applyReviewBodies(ctx context.Context, client *githubapp.Client, snapshot reviewBodySnapshot, selected, replacements []githubapp.Review) (returnErr error) {
	attempted := make([]githubapp.Review, 0, len(selected))
	defer func() {
		if returnErr == nil || len(attempted) == 0 {
			return
		}
		rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), reviewBodyRollbackTimeout)
		defer cancel()
		returnErr = errors.Join(returnErr, rollbackReviewBodies(rollbackCtx, client, snapshot, attempted, replacements))
	}()
	for index, original := range selected {
		current, err := client.GetReview(ctx, snapshot.InstallationID, snapshot.Repository, snapshot.Number, original.ID)
		if err != nil {
			return &reviewBodyOperationError{
				operation: "read_review", reviewID: original.ID,
				cause: fmt.Errorf("read review: %w", err),
			}
		}
		if current != original {
			return fmt.Errorf("review %d no longer matches its original snapshot", original.ID)
		}
		// A failed response may follow a successful remote write.
		attempted = append(attempted, original)
		updated, err := client.UpdateReview(ctx, snapshot.InstallationID, snapshot.Repository, snapshot.Number, original.ID, replacements[index].Body)
		if err != nil {
			return &reviewBodyOperationError{
				operation: "clear_review_body", reviewID: original.ID,
				cause: fmt.Errorf("clear review body: %w", err),
			}
		}
		expected := original
		expected.Body = replacements[index].Body
		if updated != expected {
			return fmt.Errorf("review %d did not retain its original decision after body cleanup", original.ID)
		}
	}
	return verifyReviewBodySnapshot(ctx, client, snapshot, replacements)
}

func reportReviewBodyError(backupPath string, failure error) error {
	var diagnostic reviewBodyDiagnostic
	var apiError githubapp.APIError
	if errors.As(failure, &apiError) {
		diagnostic.HTTPStatus = apiError.StatusCode
		diagnostic.NativeError = apiError.Message
	}
	var operation *reviewBodyOperationError
	if errors.As(failure, &operation) {
		diagnostic.Operation = operation.operation
		diagnostic.ReviewID = operation.reviewID
		if diagnostic.NativeError == "" {
			diagnostic.NativeError = operation.cause.Error()
		}
	}
	path := backupPath + ".error.json"
	publicStatus := fmt.Sprintf("GitHub returned HTTP %d.", diagnostic.HTTPStatus)
	if diagnostic.HTTPStatus == 0 {
		publicStatus = fmt.Sprintf("Review body operation %s failed.", diagnostic.Operation)
	}
	if err := writeReviewBodyDiagnostic(path, diagnostic); err != nil {
		return &reviewBodyDiagnosticError{
			message: fmt.Sprintf("%s Private diagnostics could not be saved at %s.", publicStatus, path),
			cause:   errors.Join(failure, fmt.Errorf("write private review diagnostics: %w", err)),
		}
	}
	return &reviewBodyDiagnosticError{
		message: fmt.Sprintf("%s Private diagnostics are at %s.", publicStatus, path),
		cause:   failure,
	}
}

func writeReviewBodyDiagnostic(path string, diagnostic reviewBodyDiagnostic) (returnErr error) {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		returnErr = errors.Join(returnErr, file.Close())
	}()
	if err := file.Chmod(0o600); err != nil {
		return err
	}
	if err := json.NewEncoder(file).Encode(diagnostic); err != nil {
		return err
	}
	return file.Sync()
}

func rollbackReviewBodies(ctx context.Context, client *githubapp.Client, snapshot reviewBodySnapshot, attempted, replacements []githubapp.Review) error {
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
		index := slices.IndexFunc(replacements, func(item githubapp.Review) bool { return item.ID == original.ID })
		if index < 0 || current.Body != replacements[index].Body {
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
		if index := slices.IndexFunc(cleared, func(review githubapp.Review) bool { return review.ID == item.ID }); index >= 0 {
			expected.Body = cleared[index].Body
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

func printReviewBodyPlan(output io.Writer, selected, clear []githubapp.Review, applied, linked bool) error {
	for _, item := range selected {
		if linked {
			if _, err := fmt.Fprintf(output, "Review %d has state %s with a summary permalink; applied=%t.\n", item.ID, item.State, applied); err != nil {
				return err
			}
			continue
		}
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
		if linked && item.Body != "" {
			if _, err := fmt.Fprintf(output, "Review %d already includes the summary permalink and has state %s.\n", item.ID, item.State); err != nil {
				return err
			}
			continue
		}
		if _, err := fmt.Fprintf(output, "Review %d has state %s and an empty body.\n", item.ID, item.State); err != nil {
			return err
		}
	}
	if linked {
		_, err := fmt.Fprintf(output, "The selection includes %d bodies to link and %d reviews that do not require updates.\n", len(selected), len(clear))
		return err
	}
	_, err := fmt.Fprintf(output, "The selection includes %d bodies for cleanup and %d empty bodies.\n", len(selected), len(clear))
	return err
}
