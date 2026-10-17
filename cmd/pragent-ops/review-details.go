package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"goodkind.io/pr-review-agent/internal/config"
	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/githubapp"
	"goodkind.io/pr-review-agent/internal/marker"
	"goodkind.io/pr-review-agent/internal/review"
)

type reviewDetailsOptions struct {
	identity      reviewBodyOptions
	commentID     int64
	sourceCheckID int64
}

type reviewDetailsSnapshot struct {
	Original reviewBodySnapshot        `json:"original"`
	Source   githubapp.CheckRunDetails `json:"source_check"`
	Comment  githubapp.IssueComment    `json:"original_comment"`
	Repaired string                    `json:"repaired_body"`
}

func reviewDetails(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	opts, err := reviewDetailsFlags(args, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	if err != nil {
		return err
	}
	repo, err := reviewBodyRepository(opts.identity.repository)
	if err != nil {
		return err
	}
	cfg, err := reviewBodyIdentity(opts.identity)
	if err != nil {
		return err
	}
	client := githubapp.NewClient(cfg, &http.Client{Timeout: reviewBodyRequestTimeout}, time.Now, slog.Default())
	var snapshot reviewDetailsSnapshot
	snapshot.Original, err = captureReviewBodies(ctx, client, opts.identity.installationID, repo, opts.identity.pullRequest)
	if err != nil {
		return err
	}
	snapshot.Source, err = client.GetCheckRunDetails(ctx, opts.identity.installationID, repo, opts.sourceCheckID)
	if err != nil {
		return errors.New("the selected source check run could not be read")
	}
	if err := prepareReviewDetails(&snapshot, opts, cfg); err != nil {
		return err
	}
	changed := snapshot.Comment.Body != snapshot.Repaired
	if opts.identity.apply && changed {
		if err := writeReviewDetailsBackup(opts.identity.backupFile, snapshot); err != nil {
			return err
		}
		if err := applyReviewDetails(ctx, client, snapshot, cfg.GitHubBotLogin); err != nil {
			return err
		}
	}
	_, err = fmt.Fprintf(stdout, "Summary comment %d requires repair: %t. Check run %d matches head %s. The command applied changes: %t.\n", snapshot.Comment.ID, changed, snapshot.Source.Run.ID, snapshot.Original.PullRequest.Head, opts.identity.apply && changed)
	return err
}

func reviewDetailsFlags(args []string, output io.Writer) (reviewDetailsOptions, error) {
	var opts reviewDetailsOptions
	set := flag.NewFlagSet("review-details", flag.ContinueOnError)
	set.SetOutput(output)
	set.StringVar(&opts.identity.runtime, "runtime", "runtime.json", "Read the GitHub app ID and bot login from this public runtime file.")
	set.StringVar(&opts.identity.appID, "app-id", "", "Use this GitHub app ID instead of the runtime value.")
	set.StringVar(&opts.identity.botLogin, "bot-login", "", "Use this exact bot login instead of the runtime value.")
	set.StringVar(&opts.identity.privateKeyFile, "app-private-key-file", "", "Read the GitHub app RSA private key from this file.")
	set.StringVar(&opts.identity.repository, "repository", "", "Select the target repository using OWNER/REPO.")
	set.IntVar(&opts.identity.pullRequest, "pull-request", 0, "Select this pull request number.")
	set.Int64Var(&opts.identity.installationID, "installation-id", 0, "Authenticate with this GitHub app installation ID.")
	set.Int64Var(&opts.commentID, "summary-comment-id", 0, "Repair this exact existing summary comment.")
	set.Int64Var(&opts.sourceCheckID, "source-check-run-id", 0, "Restore details from this exact successful check run.")
	set.StringVar(&opts.identity.backupFile, "backup-file", "", "Create this private original snapshot before applying repair.")
	set.BoolVar(&opts.identity.apply, "apply", false, "Update the summary after saving and verifying its original snapshot.")
	if err := set.Parse(args); err != nil {
		return opts, err
	}
	if set.NArg() != 0 {
		return opts, errors.New("review-details does not accept positional arguments")
	}
	if opts.identity.repository == "" || opts.identity.pullRequest <= 0 || opts.identity.installationID <= 0 || opts.identity.privateKeyFile == "" || opts.commentID <= 0 || opts.sourceCheckID <= 0 {
		return opts, errors.New("review-details requires repository, pull-request, installation-id, app-private-key-file, summary-comment-id, and source-check-run-id")
	}
	if opts.identity.apply && opts.identity.backupFile == "" {
		return opts, errors.New("review-details --apply requires a new backup-file")
	}
	return opts, nil
}

func prepareReviewDetails(snapshot *reviewDetailsSnapshot, opts reviewDetailsOptions, cfg config.Config) error {
	source := snapshot.Source
	if source.Run.ID != opts.sourceCheckID || source.AppID != cfg.GitHubAppID || source.Run.Name != config.ReviewCheckName || source.Run.Head != snapshot.Original.PullRequest.Head || source.Run.Status != "completed" || source.Run.Conclusion != "success" {
		return errors.New("the source must be the configured app's successful completed review check on the current head")
	}
	count := 0
	for _, comment := range snapshot.Original.Comments {
		if comment.Author == cfg.GitHubBotLogin && marker.HasSummary(comment.Body) {
			count++
		}
		if comment.ID == opts.commentID {
			snapshot.Comment = comment
		}
	}
	if count != 1 || snapshot.Comment.ID != opts.commentID || snapshot.Comment.Author != cfg.GitHubBotLogin || !marker.HasSummary(snapshot.Comment.Body) {
		return errors.New("the selected comment must be the single summary owned by the configured bot")
	}
	if strings.Count(snapshot.Comment.Body, "<!-- pr-review-agent:state:v1 ") != 1 {
		return errors.New("the summary must contain exactly one checkpoint")
	}
	head, valid := marker.FindReview(snapshot.Comment.Body)
	if !valid || head != snapshot.Original.PullRequest.Head {
		return errors.New("the selected summary verdict must identify the current head")
	}
	state, valid := marker.DecodeState(snapshot.Comment.Body)
	if !valid || state.LastReviewed != head || len(state.Pending) != 0 || len(state.Unread) != 0 {
		return errors.New("the selected summary must record complete coverage of the current head")
	}
	var err error
	snapshot.Repaired, err = review.RestoreReviewDetails(snapshot.Comment.Body, source.Summary, snapshot.Original.Threads, cfg.GitHubBotLogin)
	if err != nil {
		return errors.New("the source or destination review details are incomplete or ambiguous")
	}
	if state.Status == marker.StateFailed {
		if !review.MetadataMatches(snapshot.Comment.Body, head, diff.PullRequestRevision(snapshot.Original.PullRequest)) {
			return errors.New("a failed checkpoint requires a metadata assessment matching the current pull request revision")
		}
		latest := githubapp.LatestDecisionReview(snapshot.Original.Reviews, cfg.GitHubBotLogin)
		if latest.State != "APPROVED" || latest.CommitID != head {
			return errors.New("a failed checkpoint requires the latest bot decision to approve the current head")
		}
		snapshot.Repaired, err = completeReviewDetailsCheckpoint(snapshot.Repaired)
		return err
	}
	if state.Status != marker.StateDone {
		return errors.New("review-details requires a completed checkpoint or a recoverable failed checkpoint")
	}
	return nil
}

func completeReviewDetailsCheckpoint(body string) (string, error) {
	const prefix = "<!-- pr-review-agent:state:v1 "
	if strings.Count(body, prefix) != 1 {
		return "", errors.New("the summary must contain exactly one checkpoint")
	}
	start := strings.Index(body, prefix)
	end := strings.Index(body[start:], " -->")
	if end < 0 {
		return "", errors.New("the summary checkpoint is incomplete")
	}
	end += start + len(" -->")
	original := body[start:end]
	updated := strings.Replace(original, " status=failed ", " status=done ", 1)
	if updated == original {
		return "", errors.New("the failed checkpoint status could not be replaced")
	}
	return body[:start] + updated + body[end:], nil
}
