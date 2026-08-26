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

type updateReviewBody struct {
	Body string `json:"body"`
}

type dismissReviewBody struct {
	Message string `json:"message"`
	Event   string `json:"event"`
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
	if _, err := domain.ParseReviewDecision(string(request.Event)); err != nil {
		client.logger.ErrorContext(ctx, "invalid review event", slog.String("err", err.Error()))
		return Review{}, errors.New("invalid review event")
	}

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

	return decodeReview(body, "submitted", ctx, client)
}

// UpdateReview replaces one submitted review summary body.
func (client *Client) UpdateReview(
	ctx context.Context,
	installationID int64,
	repo domain.Repository,
	number int,
	reviewID int64,
	bodyText string,
) (Review, error) {
	path := client.repoPath(repo, fmt.Sprintf("/pulls/%d/reviews/%d", number, reviewID))
	encoded, err := json.Marshal(updateReviewBody{Body: bodyText})
	if err != nil {
		client.logger.ErrorContext(ctx, "marshal update review body", slog.String("err", err.Error()))
		return Review{}, errors.New("marshal update review body")
	}

	body, err := client.doREST(ctx, installationID, "PUT", path, nil, encoded)
	if err != nil {
		return Review{}, err
	}
	return decodeReview(body, "updated", ctx, client)
}

// DismissReview withdraws one submitted review verdict.
//
// Editing a review body cannot change its state, so a verdict the service can
// no longer stand behind keeps counting toward the pull request's review
// decision until it is dismissed. The dismissal message stays on the timeline
// and explains why the verdict was withdrawn.
func (client *Client) DismissReview(
	ctx context.Context,
	installationID int64,
	repo domain.Repository,
	number int,
	reviewID int64,
	message string,
) error {
	path := client.repoPath(repo, fmt.Sprintf("/pulls/%d/reviews/%d/dismissals", number, reviewID))
	encoded, err := json.Marshal(dismissReviewBody{Message: message, Event: "DISMISS"})
	if err != nil {
		client.logger.ErrorContext(ctx, "marshal dismiss review body", slog.String("err", err.Error()))
		return errors.New("marshal dismiss review body")
	}

	_, err = client.doREST(ctx, installationID, "PUT", path, nil, encoded)
	return err
}

func decodeReview(body []byte, operation string, ctx context.Context, client *Client) (Review, error) {
	var response reviewResponse
	if err := json.Unmarshal(body, &response); err != nil {
		client.logger.ErrorContext(ctx, "decode "+operation+" review", slog.String("err", err.Error()))
		return Review{}, errors.New("decode " + operation + " review")
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
