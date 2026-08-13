// Package webhook verifies GitHub webhooks and parses pull request events.
package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"

	"goodkind.io/pr-review-agent/internal/domain"
)

// ErrInvalidSignature means the webhook HMAC signature did not verify.
var ErrInvalidSignature = errors.New("invalid webhook signature")

// PullRequestEvent is one supported pull request webhook delivery.
type PullRequestEvent struct {
	Action         string
	DeliveryID     string
	InstallationID int64
	Repository     domain.Repository
	Number         int
	Head           domain.HeadSHA
	Draft          bool
}

// Job converts the webhook event into a review job.
func (event PullRequestEvent) Job() domain.ReviewJob {
	return domain.ReviewJob{
		DeliveryID: event.DeliveryID,
		PullRequestRef: domain.PullRequestRef{
			Repository:     event.Repository,
			Number:         event.Number,
			InstallationID: event.InstallationID,
			Head:           event.Head,
		},
	}
}

// VerifySHA256 checks the GitHub webhook HMAC SHA-256 signature header.
func VerifySHA256(signatureHeader string, secret []byte, body []byte) error {
	if signatureHeader == "" {
		return ErrInvalidSignature
	}
	const prefix = "sha256="
	if !strings.HasPrefix(signatureHeader, prefix) {
		return ErrInvalidSignature
	}
	providedHex := strings.TrimPrefix(signatureHeader, prefix)
	provided, err := hex.DecodeString(providedHex)
	if err != nil {
		return ErrInvalidSignature
	}

	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(body)
	expected := mac.Sum(nil)
	if !hmac.Equal(provided, expected) {
		return ErrInvalidSignature
	}
	return nil
}

type pullRequestAction string

const (
	actionOpened         pullRequestAction = "opened"
	actionReopened       pullRequestAction = "reopened"
	actionReadyForReview pullRequestAction = "ready_for_review"
	actionSynchronize    pullRequestAction = "synchronize"
)

func (action pullRequestAction) supported() bool {
	switch action {
	case actionOpened, actionReopened, actionReadyForReview, actionSynchronize:
		return true
	default:
		return false
	}
}

// ParsePullRequest parses a supported pull request webhook payload.
func ParsePullRequest(eventType string, deliveryID string, body []byte) (PullRequestEvent, bool, error) {
	empty := PullRequestEvent{
		Action:         "",
		DeliveryID:     "",
		InstallationID: 0,
		Repository:     domain.Repository{Owner: "", Name: ""},
		Number:         0,
		Head:           "",
		Draft:          false,
	}
	if eventType != "pull_request" {
		return empty, false, nil
	}
	if strings.TrimSpace(deliveryID) == "" {
		return empty, false, errors.New("missing delivery id")
	}

	var payload pullRequestPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return empty, false, errors.New("decode payload failed")
	}

	action := pullRequestAction(payload.Action)
	if !action.supported() {
		return empty, false, nil
	}

	if action != actionReadyForReview && payload.PullRequest.Draft {
		return empty, false, nil
	}

	if payload.Installation.ID == 0 {
		return empty, false, errors.New("missing installation id")
	}
	if payload.Repository.Owner.Login == "" || payload.Repository.Name == "" {
		return empty, false, errors.New("missing repository")
	}
	if payload.PullRequest.Number == 0 {
		return empty, false, errors.New("missing pull request number")
	}
	if payload.PullRequest.Head.SHA == "" {
		return empty, false, errors.New("missing head sha")
	}

	head, err := domain.ParseHeadSHA(payload.PullRequest.Head.SHA)
	if err != nil {
		return empty, false, errors.New("invalid head sha")
	}

	return PullRequestEvent{
		Action:         payload.Action,
		DeliveryID:     deliveryID,
		InstallationID: payload.Installation.ID,
		Repository: domain.Repository{
			Owner: payload.Repository.Owner.Login,
			Name:  payload.Repository.Name,
		},
		Number: payload.PullRequest.Number,
		Head:   head,
		Draft:  payload.PullRequest.Draft,
	}, true, nil
}

type pullRequestPayload struct {
	Action       string `json:"action"`
	Installation struct {
		ID int64 `json:"id"`
	} `json:"installation"`
	Repository struct {
		Name  string `json:"name"`
		Owner struct {
			Login string `json:"login"`
		} `json:"owner"`
	} `json:"repository"`
	PullRequest struct {
		Number int  `json:"number"`
		Draft  bool `json:"draft"`
		Head   struct {
			SHA string `json:"sha"`
		} `json:"head"`
	} `json:"pull_request"`
}
