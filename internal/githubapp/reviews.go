package githubapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"goodkind.io/pr-review-agent/internal/domain"
)

type reviewResponse struct {
	ID       int64  `json:"id"`
	CommitID string `json:"commit_id"`
	Body     string `json:"body"`
	State    string `json:"state"`
	User     struct {
		Login string `json:"login"`
	} `json:"user"`
}

type submitReviewBody struct {
	CommitID string          `json:"commit_id"`
	Body     string          `json:"body"`
	Event    string          `json:"event"`
	Comments []InlineComment `json:"comments"`
}

// ListReviews returns every review on one pull request.
func (client *Client) ListReviews(
	ctx context.Context,
	installationID int64,
	repo domain.Repository,
	number int,
) ([]Review, error) {
	path := client.repoPath(repo, fmt.Sprintf("/pulls/%d/reviews", number))
	reviews := make([]Review, 0)
	err := client.doRESTPaginated(ctx, installationID, "GET", path, nil, func(page []byte) (int, error) {
		var pageReviews []reviewResponse
		if err := json.Unmarshal(page, &pageReviews); err != nil {
			client.logger.ErrorContext(ctx, "decode reviews page", slog.String("err", err.Error()))
			return 0, errors.New("decode reviews page")
		}
		for _, item := range pageReviews {
			head, err := parseHeadSHA(item.CommitID)
			if err != nil {
				return 0, err
			}
			reviews = append(reviews, Review{
				ID:       item.ID,
				CommitID: head,
				Author:   item.User.Login,
				Body:     item.Body,
				State:    item.State,
			})
		}
		return len(pageReviews), nil
	})
	if err != nil {
		return nil, err
	}
	return reviews, nil
}

// SubmitReview posts one complete pull request review.
func (client *Client) SubmitReview(
	ctx context.Context,
	installationID int64,
	repo domain.Repository,
	number int,
	request SubmitReviewRequest,
) (Review, error) {
	path := client.repoPath(repo, fmt.Sprintf("/pulls/%d/reviews", number))
	payload := submitReviewBody{
		CommitID: string(request.CommitID),
		Body:     request.Body,
		Event:    string(request.Event),
		Comments: request.Comments,
	}
	if payload.Comments == nil {
		payload.Comments = []InlineComment{}
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		client.logger.ErrorContext(ctx, "marshal submit review body", slog.String("err", err.Error()))
		return Review{}, errors.New("marshal submit review body")
	}

	body, err := client.doREST(ctx, installationID, "POST", path, nil, encoded)
	if err != nil {
		return Review{}, err
	}

	var response reviewResponse
	if err := json.Unmarshal(body, &response); err != nil {
		client.logger.ErrorContext(ctx, "decode submitted review", slog.String("err", err.Error()))
		return Review{}, errors.New("decode submitted review")
	}

	head, err := parseHeadSHA(response.CommitID)
	if err != nil {
		return Review{}, err
	}

	return Review{
		ID:       response.ID,
		CommitID: head,
		Author:   response.User.Login,
		Body:     response.Body,
		State:    response.State,
	}, nil
}
