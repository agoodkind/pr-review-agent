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
	// Forced marks a delivery that asked for a fresh full review, which only a
	// labeled event carrying a domain.ForceReviewLabelPrefix label does.
	Forced bool
}

// Job converts the webhook event into a review job.
func (event PullRequestEvent) Job() domain.ReviewJob {
	return domain.ReviewJob{
		DeliveryID:         event.DeliveryID,
		CheckRunID:         0,
		CheckRunStatus:     "",
		CheckRunConclusion: "",
		Forced:             event.Forced,
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
	// actionLabeled is supported only for the labels this service owns. Every
	// other label a person adds is answered and ignored, which is decided in
	// ParsePullRequest because the action alone cannot tell them apart.
	actionLabeled pullRequestAction = "labeled"
)

func (action pullRequestAction) supported() bool {
	switch action {
	case actionOpened, actionReopened, actionReadyForReview, actionSynchronize, actionLabeled:
		return true
	default:
		return false
	}
}

// reviewThreadAction is a pull_request_review_thread webhook action.
type reviewThreadAction string

const (
	actionThreadResolved   reviewThreadAction = "resolved"
	actionThreadUnresolved reviewThreadAction = "unresolved"
)

func (action reviewThreadAction) supported() bool {
	switch action {
	case actionThreadResolved, actionThreadUnresolved:
		return true
	default:
		return false
	}
}

func emptyEvent() PullRequestEvent {
	return PullRequestEvent{
		Action:         "",
		DeliveryID:     "",
		InstallationID: 0,
		Repository:     domain.Repository{Owner: "", Name: ""},
		Number:         0,
		Head:           "",
		Draft:          false,
		Forced:         false,
	}
}

// githubEventType names a GitHub webhook event this service understands.
type githubEventType string

const (
	eventPullRequest  githubEventType = "pull_request"
	eventReviewThread githubEventType = "pull_request_review_thread"
)

// ParseEvent parses any supported webhook delivery into a pull request event.
func ParseEvent(eventType string, deliveryID string, body []byte) (PullRequestEvent, bool, error) {
	switch githubEventType(eventType) {
	case eventPullRequest:
		return ParsePullRequest(eventType, deliveryID, body)
	case eventReviewThread:
		return ParseReviewThread(eventType, deliveryID, body)
	default:
		return emptyEvent(), false, nil
	}
}

// ParsePullRequest parses a supported pull request webhook payload.
func ParsePullRequest(eventType string, deliveryID string, body []byte) (PullRequestEvent, bool, error) {
	if githubEventType(eventType) != eventPullRequest {
		return emptyEvent(), false, nil
	}
	payload, ok, err := decodePayload(deliveryID, body)
	if !ok {
		return emptyEvent(), false, err
	}

	action := pullRequestAction(payload.Action)
	if !action.supported() {
		return emptyEvent(), false, nil
	}

	// A label is only a trigger when it is one of this service's own. Any other
	// label on any pull request would otherwise start a review, which is the
	// opposite of what a person adding a label expects.
	forced := false
	if action == actionLabeled {
		if !domain.ForcesReview(payload.Label.Name) {
			return emptyEvent(), false, nil
		}
		forced = true
	}

	// A draft is never reviewed, and a label does not change that. Marking a
	// pull request ready for review remains the way to ask for a first review.
	if action != actionReadyForReview && payload.PullRequest.Draft {
		return emptyEvent(), false, nil
	}
	return eventFromPayload(deliveryID, payload, forced)
}

// ParseReviewThread parses a resolved or unresolved review thread delivery.
//
// A thread flipping at an already reviewed head is the one signal that the
// verdict may no longer match thread state, so it carries the same job shape
// as a pull request event and rides the same admit-and-enqueue path.
func ParseReviewThread(eventType string, deliveryID string, body []byte) (PullRequestEvent, bool, error) {
	if githubEventType(eventType) != eventReviewThread {
		return emptyEvent(), false, nil
	}
	payload, ok, err := decodePayload(deliveryID, body)
	if !ok {
		return emptyEvent(), false, err
	}
	if !reviewThreadAction(payload.Action).supported() {
		return emptyEvent(), false, nil
	}
	// A draft is never reviewed, so no verdict exists for a resolution to move.
	if payload.PullRequest.Draft {
		return emptyEvent(), false, nil
	}
	return eventFromPayload(deliveryID, payload, false)
}

func decodePayload(deliveryID string, body []byte) (pullRequestPayload, bool, error) {
	var payload pullRequestPayload
	if strings.TrimSpace(deliveryID) == "" {
		return payload, false, errors.New("missing delivery id")
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return payload, false, errors.New("decode payload failed")
	}
	return payload, true, nil
}

func eventFromPayload(
	deliveryID string,
	payload pullRequestPayload,
	forced bool,
) (PullRequestEvent, bool, error) {
	if payload.Installation.ID == 0 {
		return emptyEvent(), false, errors.New("missing installation id")
	}
	if payload.Repository.Owner.Login == "" || payload.Repository.Name == "" {
		return emptyEvent(), false, errors.New("missing repository")
	}
	if payload.PullRequest.Number == 0 {
		return emptyEvent(), false, errors.New("missing pull request number")
	}
	if payload.PullRequest.Head.SHA == "" {
		return emptyEvent(), false, errors.New("missing head sha")
	}

	head, err := domain.ParseHeadSHA(payload.PullRequest.Head.SHA)
	if err != nil {
		return emptyEvent(), false, errors.New("invalid head sha")
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
		Forced: forced,
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
	// Label carries the label a labeled delivery added. It is absent on every
	// other action, which decodes as an empty name and matches no prefix.
	Label struct {
		Name string `json:"name"`
	} `json:"label"`
}
