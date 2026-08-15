package githubapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"goodkind.io/pr-review-agent/internal/domain"
)

const reviewThreadsQuery = `
query($owner: String!, $repo: String!, $number: Int!, $cursor: String) {
  repository(owner: $owner, name: $repo) {
    pullRequest(number: $number) {
      reviewThreads(first: 100, after: $cursor) {
        pageInfo {
          hasNextPage
          endCursor
        }
        nodes {
          id
          isResolved
          isOutdated
          viewerCanResolve
          viewerCanUnresolve
          comments(first: 1) {
            nodes {
              databaseId
              body
              path
              line
              startLine
              author {
                __typename
                login
              }
            }
          }
        }
      }
    }
  }
}`

const resolveReviewThreadMutation = `
mutation($threadID: ID!) {
  resolveReviewThread(input: {threadId: $threadID}) {
    thread {
      id
      isResolved
    }
  }
}`

type reviewThreadsResponse struct {
	Repository struct {
		PullRequest struct {
			ReviewThreads struct {
				PageInfo struct {
					HasNextPage bool   `json:"hasNextPage"`
					EndCursor   string `json:"endCursor"`
				} `json:"pageInfo"`
				Nodes []reviewThreadNode `json:"nodes"`
			} `json:"reviewThreads"`
		} `json:"pullRequest"`
	} `json:"repository"`
}

type reviewThreadNode struct {
	ID                 string `json:"id"`
	IsResolved         bool   `json:"isResolved"`
	IsOutdated         bool   `json:"isOutdated"`
	ViewerCanResolve   bool   `json:"viewerCanResolve"`
	ViewerCanUnresolve bool   `json:"viewerCanUnresolve"`
	Comments           struct {
		Nodes []reviewCommentNode `json:"nodes"`
	} `json:"comments"`
}

type reviewCommentNode struct {
	DatabaseID int64  `json:"databaseId"`
	Body       string `json:"body"`
	Path       string `json:"path"`
	Line       int    `json:"line"`
	StartLine  int    `json:"startLine"`
	Author     struct {
		TypeName string `json:"__typename"`
		Login    string `json:"login"`
	} `json:"author"`
}

type resolveReviewThreadResponse struct {
	ResolveReviewThread struct {
		Thread struct {
			ID         string `json:"id"`
			IsResolved bool   `json:"isResolved"`
		} `json:"thread"`
	} `json:"resolveReviewThread"`
}

type resolveReviewThreadVariables struct {
	ThreadID string `json:"threadID"`
}

type reviewThreadsVariables struct {
	Owner  string  `json:"owner"`
	Repo   string  `json:"repo"`
	Number int     `json:"number"`
	Cursor *string `json:"cursor"`
}

// ListReviewThreads returns every review thread on one pull request.
func (client *Client) ListReviewThreads(
	ctx context.Context,
	installationID int64,
	repo domain.Repository,
	number int,
) ([]ReviewThread, error) {
	threads := make([]ReviewThread, 0)
	cursor := ""
	for {
		variables := reviewThreadsVariables{
			Owner:  repo.Owner,
			Repo:   repo.Name,
			Number: number,
			Cursor: nil,
		}
		if cursor != "" {
			cursorCopy := cursor
			variables.Cursor = &cursorCopy
		}
		encoded, err := json.Marshal(variables)
		if err != nil {
			client.logger.ErrorContext(ctx, "marshal review threads variables", slog.String("err", err.Error()))
			return nil, errors.New("marshal review threads variables")
		}

		body, err := client.doGraphQL(ctx, installationID, reviewThreadsQuery, encoded)
		if err != nil {
			return nil, err
		}

		var envelope struct {
			Data reviewThreadsResponse `json:"data"`
		}
		if err := json.Unmarshal(body, &envelope); err != nil {
			client.logger.ErrorContext(ctx, "decode review threads", slog.String("err", err.Error()))
			return nil, errors.New("decode review threads")
		}

		for _, node := range envelope.Data.Repository.PullRequest.ReviewThreads.Nodes {
			thread, err := decodeReviewThread(node)
			if err != nil {
				return nil, err
			}
			threads = append(threads, thread)
		}

		pageInfo := envelope.Data.Repository.PullRequest.ReviewThreads.PageInfo
		if !pageInfo.HasNextPage {
			break
		}
		cursor = pageInfo.EndCursor
	}
	return threads, nil
}

// ResolveReviewThread resolves one review thread by GraphQL node ID.
func (client *Client) ResolveReviewThread(
	ctx context.Context,
	installationID int64,
	threadNodeID string,
) error {
	variables := resolveReviewThreadVariables{
		ThreadID: threadNodeID,
	}
	encoded, err := json.Marshal(variables)
	if err != nil {
		client.logger.ErrorContext(ctx, "marshal resolve review thread variables", slog.String("err", err.Error()))
		return errors.New("marshal resolve review thread variables")
	}
	body, err := client.doGraphQL(ctx, installationID, resolveReviewThreadMutation, encoded)
	if err != nil {
		return err
	}

	var envelope struct {
		Data resolveReviewThreadResponse `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		client.logger.ErrorContext(ctx, "decode resolve review thread", slog.String("err", err.Error()))
		return errors.New("decode resolve review thread")
	}

	thread := envelope.Data.ResolveReviewThread.Thread
	if thread.ID != threadNodeID {
		return fmt.Errorf("resolve review thread id mismatch: got %q want %q", thread.ID, threadNodeID)
	}
	if !thread.IsResolved {
		return errors.New("resolve review thread did not mark thread resolved")
	}
	return nil
}

func decodeReviewThread(node reviewThreadNode) (ReviewThread, error) {
	if len(node.Comments.Nodes) == 0 {
		return ReviewThread{}, errors.New("review thread missing root comment")
	}
	root := node.Comments.Nodes[0]
	author := root.Author.Login
	if root.Author.TypeName == "Bot" && !strings.HasSuffix(author, "[bot]") {
		author += "[bot]"
	}
	startLine := root.Line
	if root.StartLine > 0 {
		startLine = root.StartLine
	}
	endLine := root.Line
	if endLine < 1 {
		endLine = startLine
	}

	return ReviewThread{
		NodeID:             node.ID,
		Resolved:           node.IsResolved,
		Outdated:           node.IsOutdated,
		ViewerCanResolve:   node.ViewerCanResolve,
		ViewerCanUnresolve: node.ViewerCanUnresolve,
		RootComment: domain.ReviewComment{
			DatabaseID: root.DatabaseID,
			Author:     author,
			Body:       root.Body,
			Path:       root.Path,
			StartLine:  startLine,
			EndLine:    endLine,
		},
	}, nil
}
