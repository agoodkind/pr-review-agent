// Package domain defines review contracts shared across the service.
package domain

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// ReviewSettings are the review tuning values one delivery carried with it.
//
// The container reads its configuration once, at start, so a value corrected
// after it booted did not reach a running instance: a chunk timeout changed to
// six seconds and restored five minutes later still governed a real pull request
// thirteen minutes after the restore, because nothing had replaced the process.
// Carrying these with the work rather than with the process lifetime is what
// makes a correction take effect on the next delivery instead of on the next
// restart.
//
// A zero field is a delivery that said nothing about that value, and the process
// configuration stands. That is what lets a worker and a container at different
// versions work together: an older worker sends none of this, and every review
// runs on the values it booted with, exactly as before.
//
// Only values that govern one review travel. The model and the worker count size
// the process rather than the review, and no secret travels at all. These arrive
// beside a signed body rather than inside it, so they are honored only on a
// delivery whose signature verified, and nothing here is worth more than the harm
// it could do if a stranger chose it.
type ReviewSettings struct {
	MinimumImportance int
	MaxFiles          int
	MaxChunks         int
	ChunkTimeout      time.Duration
}

// HeadSHA is a validated pull request head commit identifier.
type HeadSHA string

// ParseHeadSHA accepts a 40- or 64-character lowercase hexadecimal SHA.
func ParseHeadSHA(value string) (HeadSHA, error) {
	if len(value) != 40 && len(value) != 64 {
		return "", fmt.Errorf("head sha length %d: want 40 or 64", len(value))
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			if character < 'a' || character > 'f' {
				return "", errors.New("head sha contains non-lowercase hexadecimal")
			}
		}
	}
	return HeadSHA(value), nil
}

// ForceReviewLabelPrefix names the labels that ask for a fresh full review.
//
// It is the only re-trigger a person has. A run that died leaves a red check
// nothing clears, because only a pull request webhook starts a run and the
// check names no run identifier anyone can use. A configuration change is the
// same shape of problem from the other side: a running container keeps the
// environment it booted with, so a value corrected minutes ago is still not the
// value the next review runs under. Adding a label with this prefix answers
// both, by restarting the container and reviewing the whole pull request again.
const ForceReviewLabelPrefix = "test-review-agent-"

// ForcesReview reports whether a label name asks for a fresh full review.
func ForcesReview(labelName string) bool {
	return strings.HasPrefix(labelName, ForceReviewLabelPrefix)
}

// Repository identifies a GitHub repository owner and name pair.
type Repository struct {
	Owner string
	Name  string
}

// PullRequestRef identifies one pull request head under a GitHub installation.
type PullRequestRef struct {
	Repository     Repository
	Number         int
	InstallationID int64
	Head           HeadSHA
}

// Key returns the stable queue key for this pull request.
func (ref PullRequestRef) Key() string {
	return fmt.Sprintf("%s/%s#%d", ref.Repository.Owner, ref.Repository.Name, ref.Number)
}

// ReviewDecision is the GitHub review event submitted for one head.
type ReviewDecision string

const (
	// ReviewDecisionApprove approves a pull request with no findings.
	ReviewDecisionApprove ReviewDecision = "APPROVE"
	// ReviewDecisionRequestChanges blocks merge until findings are addressed.
	ReviewDecisionRequestChanges ReviewDecision = "REQUEST_CHANGES"
	// ReviewDecisionComment states the review outcome without gating merge.
	ReviewDecisionComment ReviewDecision = "COMMENT"
)

// ParseReviewDecision parses a GitHub review event name.
func ParseReviewDecision(value string) (ReviewDecision, error) {
	switch ReviewDecision(value) {
	case ReviewDecisionApprove, ReviewDecisionRequestChanges, ReviewDecisionComment:
		return ReviewDecision(value), nil
	default:
		return "", fmt.Errorf("unknown review decision %q", value)
	}
}

// Resolution describes how an owned review thread should be handled.
type Resolution string

const (
	// ResolutionResolved means the finding is fixed or invalid and the thread may close.
	ResolutionResolved Resolution = "resolved"
	// ResolutionOpen means the finding still applies on the current head.
	ResolutionOpen Resolution = "open"
	// ResolutionUncertain means reconciliation lacks enough context to decide.
	ResolutionUncertain Resolution = "uncertain"
)

// ParseResolution parses a reconciliation resolution value.
func ParseResolution(value string) (Resolution, error) {
	switch Resolution(value) {
	case ResolutionResolved, ResolutionOpen, ResolutionUncertain:
		return Resolution(value), nil
	default:
		return "", fmt.Errorf("unknown resolution %q", value)
	}
}

// Finding is one validated review finding anchored to changed code.
type Finding struct {
	Path      string `json:"path"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
	Title     string `json:"title"`
	Body      string `json:"body"`
	// Evidence is the exact source line the finding relies on, copied verbatim
	// from the code the model was shown. Publication drops a finding whose
	// evidence does not appear in that source, so a claim about code the model
	// never saw cannot reach the pull request. An answer without it decodes,
	// and the missing evidence makes the finding ungrounded.
	Evidence string `json:"evidence"`
	// Claim is one short sentence naming the defect independent of the wording
	// the finding is written in, a canonical label for what is wrong. Two
	// reports of one defect carry the same claim even when their titles,
	// bodies, and anchored lines differ, which is what lets a restatement be
	// recognized as one. It is never published: only its hash reaches the
	// marker. An answer from an older schema carries none, and decodes empty.
	Claim      string `json:"claim"`
	Suggestion string `json:"suggestion"`
	Importance int    `json:"importance"`
}

// The importance a finding may carry. The floor a run publishes at is bound by
// the same range: a threshold above the ceiling publishes nothing while the run
// still reports a successful verdict, which reads as a pull request with no
// defects rather than as a threshold nothing could clear.
const (
	MinimumFindingImportance = 1
	MaximumFindingImportance = 10
)

// Validate rejects empty fields, invalid line ranges, and out-of-range importance.
func (finding Finding) Validate() error {
	if strings.TrimSpace(finding.Path) == "" {
		return errors.New("finding path is required")
	}
	if strings.TrimSpace(finding.Title) == "" {
		return errors.New("finding title is required")
	}
	if strings.TrimSpace(finding.Body) == "" {
		return errors.New("finding body is required")
	}
	if strings.Contains(finding.Suggestion, "```") {
		return errors.New("finding suggestion contains a markdown fence")
	}
	if finding.StartLine < 1 {
		return errors.New("finding start_line must be positive")
	}
	if finding.EndLine < finding.StartLine {
		return errors.New("finding end_line must be greater than or equal to start_line")
	}
	if finding.Importance < MinimumFindingImportance || finding.Importance > MaximumFindingImportance {
		return errors.New("finding importance must be between 1 and 10")
	}
	return nil
}

// ReviewResult is the structured model output for one review pass.
type ReviewResult struct {
	CoverageComplete bool      `json:"coverage_complete"`
	Findings         []Finding `json:"findings"`
}

// Validate rejects invalid findings and exact duplicates.
func (result ReviewResult) Validate() error {
	seen := make(map[string]struct{}, len(result.Findings))
	for _, finding := range result.Findings {
		if err := finding.Validate(); err != nil {
			return err
		}
		key := findingKey(finding)
		if _, exists := seen[key]; exists {
			return errors.New("duplicate finding")
		}
		seen[key] = struct{}{}
	}
	return nil
}

func findingKey(finding Finding) string {
	return fmt.Sprintf(
		"%s:%d:%d:%s:%s:%d",
		finding.Path,
		finding.StartLine,
		finding.EndLine,
		finding.Title,
		finding.Body,
		finding.Importance,
	)
}

// ReviewJob is one webhook delivery queued for review work.
type ReviewJob struct {
	DeliveryID         string
	CheckRunID         int64
	CheckRunStatus     string
	CheckRunConclusion string
	// Forced marks a run a ForceReviewLabelPrefix label asked for. Such a run
	// reviews the whole pull request from scratch: it ignores the commit the
	// last completed run reviewed, the chunks earlier runs read, and every gate
	// that would otherwise report this head as already reviewed. It still
	// respects the admission budgets, because an oversized delta is oversized
	// however it was triggered.
	//
	// The delivery does that once rather than on every attempt of itself. A
	// later attempt of the same delivery, admitted because its check run never
	// completed, resumes from what the earlier attempt recorded instead of
	// paying for the whole pull request again.
	Forced bool
	// Settings are the review tuning values this delivery carried. Every field
	// it left zero falls back to the process configuration.
	Settings ReviewSettings
	PullRequestRef
}

// ReviewComment is the root comment metadata for one review thread.
type ReviewComment struct {
	DatabaseID int64
	Author     string
	Body       string
	Path       string
	StartLine  int
	EndLine    int
}

// OwnedThread is one unresolved bot finding eligible for reconciliation.
type OwnedThread struct {
	NodeID             string
	Outdated           bool
	ViewerCanResolve   bool
	ViewerCanUnresolve bool
	RootComment        ReviewComment
	// Replies are the comments under the finding, in thread order. They carry
	// the pull request author's response, which reconciliation shows the model
	// as untrusted context.
	Replies     []ReviewComment
	Finding     Finding
	FindingHead HeadSHA
}

// ThreadResolution is the model decision for one owned thread.
type ThreadResolution struct {
	ThreadNodeID string     `json:"thread_node_id"`
	Resolution   Resolution `json:"resolution"`
	Reason       string     `json:"reason"`
}

// Validate rejects empty thread ids, unknown resolutions, and empty reasons.
func (resolution ThreadResolution) Validate() error {
	if strings.TrimSpace(resolution.ThreadNodeID) == "" {
		return errors.New("thread_node_id is required")
	}
	if _, err := ParseResolution(string(resolution.Resolution)); err != nil {
		return errors.New("parse resolution")
	}
	if strings.TrimSpace(resolution.Reason) == "" {
		return errors.New("reason is required")
	}
	return nil
}

// ValidateThreadResolutions rejects invalid or duplicate thread resolutions.
func ValidateThreadResolutions(resolutions []ThreadResolution) error {
	seen := make(map[string]struct{}, len(resolutions))
	for _, resolution := range resolutions {
		if err := resolution.Validate(); err != nil {
			return err
		}
		if _, exists := seen[resolution.ThreadNodeID]; exists {
			return errors.New("duplicate thread_node_id")
		}
		seen[resolution.ThreadNodeID] = struct{}{}
	}
	return nil
}
