package review

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"goodkind.io/gklog"
	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
)

type metadataCommitReader interface {
	ListPullRequestCommits(context.Context, int64, domain.Repository, int) ([]githubapp.PullRequestCommit, error)
}

func (service *Service) restoreMissingCommitMetadata(ctx context.Context, job domain.ReviewJob, prior []domain.Finding, index map[domain.FindingTarget]diff.MetadataSource, pullRequest githubapp.PullRequest) (map[domain.HeadSHA]bool, error) {
	missing := make(map[domain.HeadSHA]bool)
	for _, finding := range prior {
		if finding.EffectiveSurface() != domain.FindingCommitMessage {
			continue
		}
		if _, found := index[finding.Target()]; !found {
			missing[finding.CommitSHA] = true
		}
	}
	if len(missing) == 0 {
		return missing, nil
	}
	reader, supported := service.github.(metadataCommitReader)
	if !supported {
		return nil, errors.New("GitHub client does not support complete commit metadata verification")
	}
	commits, err := reader.ListPullRequestCommits(ctx, job.InstallationID, job.Repository, job.Number)
	if err != nil {
		logger := gklog.L(ctx)
		logger.WarnContext(ctx, "Current pull request commit metadata verification failed", "err", err)
		return nil, fmt.Errorf("verify current pull request commit metadata: %w", err)
	}
	if err := verifyCurrentCommitSet(commits, pullRequest); err != nil {
		return nil, err
	}
	for _, commit := range commits {
		delete(missing, commit.SHA)
		source := diff.MetadataSource{Surface: domain.FindingCommitMessage, CommitSHA: commit.SHA, Text: commit.Message, StartLine: 1, EndLine: len(strings.Split(commit.Message, "\n"))}
		index[source.Target()] = source
	}
	return missing, nil
}

func verifyCurrentCommitSet(commits []githubapp.PullRequestCommit, pullRequest githubapp.PullRequest) error {
	if pullRequest.CommitCount < 1 || len(commits) != pullRequest.CommitCount {
		return errors.New("current pull request commit metadata is incomplete")
	}
	seen := make(map[domain.HeadSHA]bool, len(commits))
	for _, commit := range commits {
		if seen[commit.SHA] {
			return errors.New("current pull request commit metadata repeats a commit")
		}
		seen[commit.SHA] = true
	}
	if !seen[pullRequest.Head] {
		return errors.New("current pull request commit metadata does not include the reviewed head")
	}
	return nil
}
