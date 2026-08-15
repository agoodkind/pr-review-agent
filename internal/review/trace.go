package review

import (
	"context"
	"fmt"
	"log/slog"
	"sort"

	"goodkind.io/gklog"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
	"goodkind.io/pr-review-agent/internal/marker"
)

type findingTrace struct {
	ID         string `json:"id"`
	Path       string `json:"path"`
	StartLine  int    `json:"start_line"`
	EndLine    int    `json:"end_line"`
	Importance int    `json:"importance"`
}

type reviewTrace struct {
	ID              int64  `json:"id"`
	CommitID        string `json:"commit_id"`
	State           string `json:"state"`
	HasReviewMarker bool   `json:"has_review_marker"`
	HasSummary      bool   `json:"has_summary"`
}

type threadTrace struct {
	NodeID      string `json:"node_id"`
	CommentID   int64  `json:"comment_id"`
	Resolved    bool   `json:"resolved"`
	Outdated    bool   `json:"outdated"`
	FindingID   string `json:"finding_id,omitempty"`
	FindingHead string `json:"finding_head,omitempty"`
	Importance  int    `json:"importance,omitempty"`
}

func traceFindings(ctx context.Context, findings []domain.Finding) ([]findingTrace, error) {
	logger := gklog.L(ctx)
	traces := make([]findingTrace, 0, len(findings))
	for _, finding := range findings {
		findingID, err := marker.FindingID(finding)
		if err != nil {
			logger.ErrorContext(ctx, "identify finding for trace", slog.String("err", err.Error()))
			return nil, fmt.Errorf("identify finding for trace: %w", err)
		}
		traces = append(traces, findingTrace{
			ID:         findingID,
			Path:       finding.Path,
			StartLine:  finding.StartLine,
			EndLine:    finding.EndLine,
			Importance: finding.Importance,
		})
	}
	return traces, nil
}

func traceReviews(reviews []githubapp.Review, botLogin string) []reviewTrace {
	traces := make([]reviewTrace, 0)
	for _, item := range reviews {
		if item.Author != botLogin {
			continue
		}
		_, hasReviewMarker := marker.FindReview(item.Body)
		traces = append(traces, reviewTrace{
			ID:              item.ID,
			CommitID:        string(item.CommitID),
			State:           item.State,
			HasReviewMarker: hasReviewMarker,
			HasSummary:      marker.HasSummary(item.Body),
		})
	}
	return traces
}

func traceThreads(threads []githubapp.ReviewThread, botLogin string) []threadTrace {
	traces := make([]threadTrace, 0)
	for _, thread := range threads {
		if thread.RootComment.Author != botLogin {
			continue
		}
		trace := threadTrace{
			NodeID:      thread.NodeID,
			CommentID:   thread.RootComment.DatabaseID,
			Resolved:    thread.Resolved,
			Outdated:    thread.Outdated,
			FindingID:   "",
			FindingHead: "",
			Importance:  0,
		}
		findingMarker, ok := marker.FindFinding(thread.RootComment.Body)
		if ok {
			trace.FindingID = findingMarker.ID
			trace.FindingHead = string(findingMarker.Head)
			trace.Importance = findingMarker.Importance
		}
		traces = append(traces, trace)
	}
	sort.Slice(traces, func(left, right int) bool {
		return traces[left].NodeID < traces[right].NodeID
	})
	return traces
}
