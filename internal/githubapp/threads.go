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
          comments(first: 50) {
            pageInfo {
              hasNextPage
              endCursor
            }
            nodes {
              databaseId
              body
              path
              line
              startLine
              originalLine
              originalStartLine
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

// threadCommentsQuery reads the rest of one thread's comments.
//
// The listing above takes only the first page of each thread's comments, and a
// long argument runs past it. The replies it would drop are the author's answer
// to the finding, which is what reconciliation weighs and what keeps the service
// from raising a claim the author has already answered, so a thread truncated in
// silence decides from partial context.
const threadCommentsQuery = `
query($threadID: ID!, $cursor: String) {
  node(id: $threadID) {
    ... on PullRequestReviewThread {
      comments(first: 100, after: $cursor) {
        pageInfo {
          hasNextPage
          endCursor
        }
        nodes {
          databaseId
          body
          path
          line
          startLine
          originalLine
          originalStartLine
          author {
            __typename
            login
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
	ID                 string             `json:"id"`
	IsResolved         bool               `json:"isResolved"`
	IsOutdated         bool               `json:"isOutdated"`
	ViewerCanResolve   bool               `json:"viewerCanResolve"`
	ViewerCanUnresolve bool               `json:"viewerCanUnresolve"`
	Comments           commentsConnection `json:"comments"`
}

// commentsConnection is one page of a thread's comments.
type commentsConnection struct {
	PageInfo struct {
		HasNextPage bool   `json:"hasNextPage"`
		EndCursor   string `json:"endCursor"`
	} `json:"pageInfo"`
	Nodes []reviewCommentNode `json:"nodes"`
}

type threadCommentsResponse struct {
	Node struct {
		Comments commentsConnection `json:"comments"`
	} `json:"node"`
}

type threadCommentsVariables struct {
	ThreadID string  `json:"threadID"`
	Cursor   *string `json:"cursor"`
}

type reviewCommentNode struct {
	DatabaseID        int64  `json:"databaseId"`
	Body              string `json:"body"`
	Path              string `json:"path"`
	Line              int    `json:"line"`
	StartLine         int    `json:"startLine"`
	OriginalLine      int    `json:"originalLine"`
	OriginalStartLine int    `json:"originalStartLine"`
	Author            struct {
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
			if node.Comments.PageInfo.HasNextPage {
				rest, err := client.listRemainingThreadComments(
					ctx, installationID, node.ID, node.Comments.PageInfo.EndCursor,
				)
				if err != nil {
					return nil, err
				}
				thread.Replies = append(thread.Replies, rest...)
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

// listRemainingThreadComments reads one thread's comments from cursor onward,
// following every page. They are all replies, because the root comment is on the
// first page the listing already read.
func (client *Client) listRemainingThreadComments(
	ctx context.Context,
	installationID int64,
	threadNodeID string,
	cursor string,
) ([]domain.ReviewComment, error) {
	replies := make([]domain.ReviewComment, 0)
	for cursor != "" {
		cursorCopy := cursor
		encoded, err := json.Marshal(threadCommentsVariables{
			ThreadID: threadNodeID,
			Cursor:   &cursorCopy,
		})
		if err != nil {
			client.logger.ErrorContext(ctx, "marshal thread comments variables", slog.String("err", err.Error()))
			return nil, errors.New("marshal thread comments variables")
		}

		body, err := client.doGraphQL(ctx, installationID, threadCommentsQuery, encoded)
		if err != nil {
			return nil, err
		}

		var envelope struct {
			Data threadCommentsResponse `json:"data"`
		}
		if err := json.Unmarshal(body, &envelope); err != nil {
			client.logger.ErrorContext(ctx, "decode thread comments", slog.String("err", err.Error()))
			return nil, errors.New("decode thread comments")
		}

		comments := envelope.Data.Node.Comments
		for _, comment := range comments.Nodes {
			replies = append(replies, decodeThreadComment(comment))
		}
		// A page that reports another page without moving the cursor would loop
		// forever, so an unchanged or empty cursor ends the walk.
		if !comments.PageInfo.HasNextPage || comments.PageInfo.EndCursor == cursor {
			break
		}
		cursor = comments.PageInfo.EndCursor
	}
	return replies, nil
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
	replies := make([]domain.ReviewComment, 0, len(node.Comments.Nodes)-1)
	for _, reply := range node.Comments.Nodes[1:] {
		replies = append(replies, decodeThreadComment(reply))
	}

	return ReviewThread{
		NodeID:             node.ID,
		Resolved:           node.IsResolved,
		Outdated:           node.IsOutdated,
		ViewerCanResolve:   node.ViewerCanResolve,
		ViewerCanUnresolve: node.ViewerCanUnresolve,
		RootComment:        decodeThreadComment(node.Comments.Nodes[0]),
		Replies:            replies,
	}, nil
}

// decodeThreadComment maps one GraphQL comment node, falling back to the
// original coordinates GitHub reports for outdated threads.
func decodeThreadComment(comment reviewCommentNode) domain.ReviewComment {
	author := comment.Author.Login
	if comment.Author.TypeName == "Bot" && !strings.HasSuffix(author, "[bot]") {
		author += "[bot]"
	}
	endLine := comment.Line
	if endLine < 1 {
		endLine = comment.OriginalLine
	}
	startLine := comment.StartLine
	if startLine < 1 {
		startLine = comment.OriginalStartLine
	}
	if startLine < 1 {
		startLine = endLine
	}
	if endLine < 1 {
		endLine = startLine
	}
	return domain.ReviewComment{
		DatabaseID: comment.DatabaseID,
		Author:     author,
		Body:       comment.Body,
		Path:       comment.Path,
		StartLine:  startLine,
		EndLine:    endLine,
	}
}
