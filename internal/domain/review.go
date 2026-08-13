// Package domain defines review contracts shared across the service.
package domain

import (
	"errors"
	"fmt"
	"strings"
)

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
	// ReviewDecisionComment leaves review comments without blocking merge.
	ReviewDecisionComment ReviewDecision = "COMMENT"
	// ReviewDecisionRequestChanges blocks merge until findings are addressed.
	ReviewDecisionRequestChanges ReviewDecision = "REQUEST_CHANGES"
)

// ParseReviewDecision parses a GitHub review event name.
func ParseReviewDecision(value string) (ReviewDecision, error) {
	switch ReviewDecision(value) {
	case ReviewDecisionApprove, ReviewDecisionComment, ReviewDecisionRequestChanges:
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
	Path       string `json:"path"`
	StartLine  int    `json:"start_line"`
	EndLine    int    `json:"end_line"`
	Title      string `json:"title"`
	Body       string `json:"body"`
	Importance int    `json:"importance"`
}

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
	if finding.StartLine < 1 {
		return errors.New("finding start_line must be positive")
	}
	if finding.EndLine < finding.StartLine {
		return errors.New("finding end_line must be greater than or equal to start_line")
	}
	if finding.Importance < 1 || finding.Importance > 10 {
		return errors.New("finding importance must be between 1 and 10")
	}
	return nil
}

// ReviewResult is the structured model output for one review pass.
type ReviewResult struct {
	Summary          string    `json:"summary"`
	CoverageComplete bool      `json:"coverage_complete"`
	Findings         []Finding `json:"findings"`
}

// Validate rejects empty summaries, invalid findings, and exact duplicates.
func (result ReviewResult) Validate() error {
	if strings.TrimSpace(result.Summary) == "" {
		return errors.New("review summary is required")
	}
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
	DeliveryID string
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
	NodeID      string
	RootComment ReviewComment
	Finding     Finding
	FindingHead HeadSHA
}

// ThreadResolution is the model decision for one owned thread.
type ThreadResolution struct {
	ThreadNodeID string     `json:"thread_node_id"`
	Resolution   Resolution `json:"resolution"`
	Reason       string     `json:"reason"`
}
