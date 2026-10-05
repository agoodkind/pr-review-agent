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
	Action              string
	DeliveryID          string
	InstallationID      int64
	Repository          domain.Repository
	Number              int
	Head                domain.HeadSHA
	Draft               bool
	ThreadRootCommentID int64
	RefreshVerdict      bool
	// Forced marks a delivery that asked for a fresh full review.
	Forced bool
	// Label is the full name of the label that forced this delivery, and it is
	// an opaque identifier. Nothing reads the text after the prefix. It never
	// names a timeout, a budget, a model, or any other setting, because a label
	// anyone with triage access can add must not be able to change how the
	// service behaves; the label decides only that a review runs, never how.
	//
	// It exists so an operator can tie a run back to the label they added, so
	// it goes to one log line at admission and no further. It is deliberately
	// absent from domain.ReviewJob: the review never sees it, so it cannot
	// reach a pull request comment or the published run log, where a label name
	// would be text a person outside this service chose.
	Label string
}

// Job converts the webhook event into a review job.
func (event PullRequestEvent) Job() domain.ReviewJob {
	return domain.ReviewJob{
		DeliveryID:          event.DeliveryID,
		CheckRunID:          0,
		CheckRunStatus:      "",
		CheckRunConclusion:  "",
		ThreadRootCommentID: event.ThreadRootCommentID,
		RefreshVerdict:      event.RefreshVerdict,
		Forced:              event.Forced,
		// The tuning values ride on the request rather than in the payload, and
		// are read only once the signature has verified, so nothing decoded here
		// can set them.
		Settings: domain.ReviewSettings{
			MinimumImportance: 0, MaxFiles: 0, MaxChunks: 0, ChunkTimeout: 0,
			ChunkConcurrency: 0, MaxPromptBytes: 0,
		},
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
	actionEdited         pullRequestAction = "edited"
	// actionLabeled is supported only for the labels this service owns. Every
	// other label a person adds is answered and ignored, which is decided in
	// ParsePullRequest because the action alone cannot tell them apart.
	actionLabeled pullRequestAction = "labeled"
)

func (action pullRequestAction) supported() bool {
	switch action {
	case actionOpened, actionReopened, actionReadyForReview, actionSynchronize, actionLabeled, actionEdited:
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

type reviewCommentAction string

const (
	actionReviewCommentCreated reviewCommentAction = "created"
	actionReviewCommentEdited  reviewCommentAction = "edited"
)

func (action reviewCommentAction) supported() bool {
	switch action {
	case actionReviewCommentCreated, actionReviewCommentEdited:
		return true
	default:
		return false
	}
}

func emptyEvent() PullRequestEvent {
	return PullRequestEvent{
		Action:              "",
		DeliveryID:          "",
		InstallationID:      0,
		Repository:          domain.Repository{Owner: "", Name: ""},
		Number:              0,
		Head:                "",
		Draft:               false,
		ThreadRootCommentID: 0,
		RefreshVerdict:      false,
		Forced:              false,
		Label:               "",
	}
}

// githubEventType names a GitHub webhook event this service understands.
type githubEventType string

const (
	eventPullRequest   githubEventType = "pull_request"
	eventReviewComment githubEventType = "pull_request_review_comment"
	eventReviewThread  githubEventType = "pull_request_review_thread"
)

// ParseEvent parses any supported webhook delivery into a pull request event.
func ParseEvent(eventType string, deliveryID string, body []byte) (PullRequestEvent, bool, error) {
	switch githubEventType(eventType) {
	case eventPullRequest:
		return ParsePullRequest(eventType, deliveryID, body)
	case eventReviewThread:
		return ParseReviewThread(eventType, deliveryID, body)
	case eventReviewComment:
		return ParseReviewComment(eventType, deliveryID, body)
	default:
		return emptyEvent(), false, nil
	}
}

// ParseMentionedIssueComment accepts a new pull request comment that tags the app.
// The caller loads the current pull request before admitting the returned event.
func ParseMentionedIssueComment(deliveryID string, body []byte, botLogin string) (PullRequestEvent, bool, error) {
	payload, ok, err := decodePayload(deliveryID, body)
	if !ok {
		return emptyEvent(), false, err
	}
	if payload.Action != "created" || len(payload.Issue.PullRequest) == 0 ||
		string(payload.Issue.PullRequest) == "null" ||
		strings.EqualFold(payload.Comment.User.Login, botLogin) ||
		!mentionsBot(payload.Comment.Body, botLogin) {
		return emptyEvent(), false, nil
	}
	if payload.Installation.ID == 0 || payload.Repository.Owner.Login == "" ||
		payload.Repository.Name == "" || payload.Issue.Number == 0 {
		return emptyEvent(), false, errors.New("missing pull request comment fields")
	}
	return PullRequestEvent{
		Action:         payload.Action,
		DeliveryID:     deliveryID,
		InstallationID: payload.Installation.ID,
		Repository: domain.Repository{
			Owner: payload.Repository.Owner.Login,
			Name:  payload.Repository.Name,
		},
		Number:              payload.Issue.Number,
		Head:                "",
		Draft:               false,
		ThreadRootCommentID: 0,
		RefreshVerdict:      false,
		Forced:              true,
		Label:               "",
	}, true, nil
}

func mentionsBot(body string, botLogin string) bool {
	login := strings.TrimSuffix(strings.ToLower(botLogin), "[bot]")
	if login == "" {
		return false
	}
	text := strings.ToLower(body)
	mention := "@" + login
	for offset := 0; offset < len(text); {
		index := strings.Index(text[offset:], mention)
		if index < 0 {
			return false
		}
		index += offset
		end := index + len(mention)
		beforeValid := index == 0 || !isLoginCharacter(text[index-1])
		afterValid := end == len(text) || !isLoginCharacter(text[end])
		if beforeValid && afterValid {
			return true
		}
		offset = end
	}
	return false
}

func isLoginCharacter(character byte) bool {
	return character >= 'a' && character <= 'z' ||
		character >= '0' && character <= '9' || character == '-' || character == '_'
}

// ParseReviewComment accepts a reply on an inline finding so the reviewer can
// weigh the latest discussion without waiting for another push.
func ParseReviewComment(eventType string, deliveryID string, body []byte) (PullRequestEvent, bool, error) {
	if githubEventType(eventType) != eventReviewComment {
		return emptyEvent(), false, nil
	}
	payload, ok, err := decodePayload(deliveryID, body)
	if !ok {
		return emptyEvent(), false, err
	}
	if !reviewCommentAction(payload.Action).supported() {
		return emptyEvent(), false, nil
	}
	if payload.Comment.InReplyToID == 0 || payload.PullRequest.Draft {
		return emptyEvent(), false, nil
	}
	event, supported, err := eventFromPayload(deliveryID, payload, false)
	if supported && err == nil {
		event.ThreadRootCommentID = payload.Comment.InReplyToID
		event.RefreshVerdict = true
	}
	return event, supported, err
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
	forced := action == actionEdited
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
	event, supported, err := eventFromPayload(deliveryID, payload, false)
	if supported && err == nil {
		event.RefreshVerdict = true
	}
	return event, supported, err
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

	// The only label ever recorded is one that forced a run, and it is recorded
	// whole. The prefix is matched, and the rest is never read.
	label := ""
	if forced {
		label = payload.Label.Name
	}

	return PullRequestEvent{
		Action:         payload.Action,
		DeliveryID:     deliveryID,
		InstallationID: payload.Installation.ID,
		Repository: domain.Repository{
			Owner: payload.Repository.Owner.Login,
			Name:  payload.Repository.Name,
		},
		Number:              payload.PullRequest.Number,
		Head:                head,
		Draft:               payload.PullRequest.Draft,
		ThreadRootCommentID: 0,
		RefreshVerdict:      false,
		Forced:              forced,
		Label:               label,
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
	Issue struct {
		Number      int             `json:"number"`
		PullRequest json.RawMessage `json:"pull_request"`
	} `json:"issue"`
	// Label carries the label a labeled delivery added. It is absent on every
	// other action, which decodes as an empty name and matches no prefix.
	Label struct {
		Name string `json:"name"`
	} `json:"label"`
	Comment struct {
		InReplyToID int64  `json:"in_reply_to_id"`
		Body        string `json:"body"`
		User        struct {
			Login string `json:"login"`
		} `json:"user"`
	} `json:"comment"`
}
