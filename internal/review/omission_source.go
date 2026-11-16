package review

import (
	"fmt"
	"log/slog"
	"reflect"

	"goodkind.io/pr-review-agent/internal/diff"
)

func (hunk unreadHunk) sourceKey() string {
	if hunk.Target == nil {
		return hunk.Path
	}

	return fmt.Sprintf("%s:%s", hunk.Target.Surface, hunk.Target.CommitSHA)
}

func unreadPiece(piece diff.Piece, reason string) unreadHunk {
	hunk := unreadHunk{
		Path:           piece.Path,
		Header:         piece.Header,
		Reason:         "",
		ReasonTemplate: reason,
		Target:         nil,
		Omitted:        0,
	}
	if piece.Metadata != nil {
		target := piece.Metadata.Target()
		hunk.Target = &target
	}

	return hunk
}

func (hunk unreadHunk) sourceLabel() (string, error) {
	if hunk.Omitted > 0 {
		return renderOutputTemplate("output.additional-omissions-label", reflect.ValueOf(nil))
	}

	if hunk.Target == nil {
		return hunk.Path, nil
	}

	source := diff.MetadataSource{
		Surface:   hunk.Target.Surface,
		CommitSHA: hunk.Target.CommitSHA,
		Text:      "",
		StartLine: 0,
		EndLine:   0,
	}

	label, err := source.Label()
	if err != nil {
		slog.Warn("The review service could not render the unread source label.", "err", err)

		return "", fmt.Errorf("review service could not render unread source label: %w", err)
	}

	return label, nil
}

func (hunk unreadHunk) displayReason() (string, error) {
	if hunk.Omitted > 0 {
		return renderOutputTemplate(
			"output.additional-omissions-reason",
			reflect.ValueOf(hunk.Omitted),
		)
	}

	if hunk.ReasonTemplate != "" {
		return renderOutputTemplate(hunk.ReasonTemplate, reflect.ValueOf(nil))
	}

	return hunk.Reason, nil
}
