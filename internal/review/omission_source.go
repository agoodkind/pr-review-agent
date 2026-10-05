package review

import "goodkind.io/pr-review-agent/internal/diff"

func unreadPiece(piece diff.Piece, reason string) unreadHunk {
	hunk := unreadHunk{Path: piece.Path, Header: piece.Header, Reason: reason, Target: nil}
	if piece.Metadata != nil {
		target := piece.Metadata.Target()
		hunk.Target = &target
	}
	return hunk
}

func (hunk unreadHunk) sourceLabel() string {
	if hunk.Target == nil {
		return hunk.Path
	}
	source := diff.MetadataSource{
		Surface: hunk.Target.Surface, CommitSHA: hunk.Target.CommitSHA, Text: "", StartLine: 0, EndLine: 0,
	}
	return source.Label()
}
