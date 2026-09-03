package review

import (
	"context"

	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
)

// GitHub loads pull request state and publishes review lifecycle updates.
//
// This is split into three narrower interfaces, embedded here, so the
// combined declaration stays under the repository's interface size lint.
type GitHub interface {
	checkRunGitHub
	publicationGitHub
	issueCommentGitHub
}

// checkRunGitHub loads the pull request and runs the visible check lifecycle.
type checkRunGitHub interface {
	GetPullRequest(context.Context, int64, domain.Repository, int) (githubapp.PullRequest, error)
	FindCheckRun(context.Context, int64, domain.Repository, domain.HeadSHA, string) (githubapp.CheckRun, bool, error)
	FindCheckRunByExternalID(
		context.Context, int64, domain.Repository, domain.HeadSHA, string, string,
	) (githubapp.CheckRun, bool, error)
	CreateCheckRun(context.Context, int64, domain.Repository, domain.HeadSHA, string, string) (githubapp.CheckRun, error)
	StartCheckRun(context.Context, int64, domain.Repository, int64, string) error
	CompleteCheckRun(context.Context, int64, domain.Repository, int64, string, string, string, string) error
}

// publicationGitHub publishes the pull request review and its inline
// findings, and reads back the threads those findings become.
type publicationGitHub interface {
	ListReviews(context.Context, int64, domain.Repository, int) ([]githubapp.Review, error)
	ListReviewThreads(context.Context, int64, domain.Repository, int) ([]githubapp.ReviewThread, error)
	SubmitReview(context.Context, int64, domain.Repository, int, githubapp.SubmitReviewRequest) (githubapp.Review, error)
	UpdateReview(context.Context, int64, domain.Repository, int, int64, string) (githubapp.Review, error)
	CreateReviewComment(
		context.Context,
		int64,
		domain.Repository,
		int,
		domain.HeadSHA,
		githubapp.InlineComment,
	) error
}

// issueCommentGitHub keeps the service's one top level comment.
type issueCommentGitHub interface {
	ListIssueComments(context.Context, int64, domain.Repository, int) ([]githubapp.IssueComment, error)
	CreateIssueComment(context.Context, int64, domain.Repository, int, string) (githubapp.IssueComment, error)
	UpdateIssueComment(context.Context, int64, domain.Repository, int64, string) (githubapp.IssueComment, error)
}
