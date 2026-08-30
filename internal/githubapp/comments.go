package githubapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"goodkind.io/pr-review-agent/internal/domain"
)

// IssueComment is one top level pull request comment, which GitHub models as
// an issue comment rather than a review comment.
type IssueComment struct {
	ID     int64
	Author string
	Body   string
}

type issueCommentResponse struct {
	ID   int64  `json:"id"`
	Body string `json:"body"`
	User struct {
		Login string `json:"login"`
	} `json:"user"`
}

type issueCommentBody struct {
	Body string `json:"body"`
}

// ListIssueComments returns every top level comment on one pull request.
func (client *Client) ListIssueComments(
	ctx context.Context,
	installationID int64,
	repo domain.Repository,
	number int,
) ([]IssueComment, error) {
	path := client.repoPath(repo, fmt.Sprintf("/issues/%d/comments", number))
	comments := make([]IssueComment, 0)
	err := client.doRESTPaginated(ctx, installationID, path, func(page []byte) (int, error) {
		var decoded []issueCommentResponse
		if err := json.Unmarshal(page, &decoded); err != nil {
			client.logger.ErrorContext(ctx, "decode issue comments page", slog.String("err", err.Error()))
			return 0, errors.New("decode issue comments page")
		}
		for _, item := range decoded {
			comments = append(comments, IssueComment{
				ID:     item.ID,
				Author: item.User.Login,
				Body:   item.Body,
			})
		}
		return len(decoded), nil
	})
	if err != nil {
		return nil, err
	}
	return comments, nil
}

// CreateIssueComment posts one new top level comment.
func (client *Client) CreateIssueComment(
	ctx context.Context,
	installationID int64,
	repo domain.Repository,
	number int,
	body string,
) (IssueComment, error) {
	path := client.repoPath(repo, fmt.Sprintf("/issues/%d/comments", number))
	return client.writeIssueComment(ctx, installationID, "POST", path, body, "create")
}

// UpdateIssueComment replaces the body of one existing top level comment.
func (client *Client) UpdateIssueComment(
	ctx context.Context,
	installationID int64,
	repo domain.Repository,
	commentID int64,
	body string,
) (IssueComment, error) {
	path := client.repoPath(repo, fmt.Sprintf("/issues/comments/%d", commentID))
	return client.writeIssueComment(ctx, installationID, "PATCH", path, body, "update")
}

func (client *Client) writeIssueComment(
	ctx context.Context,
	installationID int64,
	method string,
	path string,
	body string,
	operation string,
) (IssueComment, error) {
	encoded, err := json.Marshal(issueCommentBody{Body: body})
	if err != nil {
		client.logger.ErrorContext(ctx, "marshal "+operation+" issue comment", slog.String("err", err.Error()))
		return IssueComment{ID: 0, Author: "", Body: ""}, errors.New("marshal " + operation + " issue comment")
	}
	responseBody, err := client.doREST(ctx, installationID, method, path, nil, encoded)
	if err != nil {
		return IssueComment{ID: 0, Author: "", Body: ""}, err
	}
	var decoded issueCommentResponse
	if err := json.Unmarshal(responseBody, &decoded); err != nil {
		client.logger.ErrorContext(ctx, "decode "+operation+" issue comment", slog.String("err", err.Error()))
		return IssueComment{ID: 0, Author: "", Body: ""}, errors.New("decode " + operation + " issue comment")
	}
	return IssueComment{
		ID:     decoded.ID,
		Author: decoded.User.Login,
		Body:   decoded.Body,
	}, nil
}
