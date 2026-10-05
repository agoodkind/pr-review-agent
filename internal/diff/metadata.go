package diff

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
)

// MetadataSource uses field line numbers without inventing repository paths.
type MetadataSource struct {
	Surface   domain.FindingSurface `json:"surface"`
	CommitSHA domain.HeadSHA        `json:"commit_sha"`
	Text      string                `json:"text"`
	StartLine int                   `json:"start_line"`
	EndLine   int                   `json:"end_line"`
}

// Target distinguishes commit messages by their immutable commit identifier.
func (source MetadataSource) Target() domain.FindingTarget {
	return domain.FindingTarget{Surface: source.Surface, CommitSHA: source.CommitSHA}
}

// Label renders metadata independently of file coordinates.
func (source MetadataSource) Label() string {
	switch source.Surface {
	case domain.FindingPullRequestTitle:
		return "Pull request title"
	case domain.FindingPullRequestDescription:
		return "Pull request description"
	case domain.FindingCommitMessage:
		return "Commit message " + string(source.CommitSHA)
	case domain.FindingFile:
		return ""
	default:
		return ""
	}
}

// PullRequestMetadata includes commit attribution in the source for protection checks.
func PullRequestMetadata(pullRequest githubapp.PullRequest, commits []githubapp.PullRequestCommit) []MetadataSource {
	var sources []MetadataSource
	appendSource := func(surface domain.FindingSurface, commit domain.HeadSHA, text string) {
		if strings.TrimSpace(text) == "" {
			return
		}
		sources = append(sources, MetadataSource{
			Surface: surface, CommitSHA: commit, Text: text, StartLine: 1, EndLine: len(strings.Split(text, "\n")),
		})
	}
	appendSource(domain.FindingPullRequestTitle, "", pullRequest.Title)
	appendSource(domain.FindingPullRequestDescription, "", pullRequest.Body)
	for _, commit := range commits {
		appendSource(domain.FindingCommitMessage, commit.SHA, commit.Message)
	}
	return sources
}

// MetadataRevision includes field identity and text length in each digest input.
func MetadataRevision(sources []MetadataSource) string {
	var builder strings.Builder
	for _, source := range sources {
		builder.WriteString(string(source.Surface))
		builder.WriteByte(':')
		builder.WriteString(string(source.CommitSHA))
		builder.WriteByte(':')
		builder.WriteString(strconv.Itoa(len(source.Text)))
		builder.WriteByte(':')
		builder.WriteString(source.Text)
	}
	digest := sha256.Sum256([]byte(builder.String()))
	return hex.EncodeToString(digest[:])
}

// PullRequestRevision includes mutable prose and the immutable commit range.
func PullRequestRevision(pullRequest githubapp.PullRequest) string {
	identity := []MetadataSource{
		{Surface: domain.FindingPullRequestTitle, CommitSHA: "", Text: pullRequest.Title, StartLine: 1, EndLine: 1},
		{Surface: domain.FindingPullRequestDescription, CommitSHA: "", Text: pullRequest.Body, StartLine: 1, EndLine: 1},
	}
	value := MetadataRevision(identity) + ":" + string(pullRequest.Base) + ":" + string(pullRequest.Head) + ":" + strconv.Itoa(pullRequest.CommitCount)
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func metadataPieces(source MetadataSource, maximumBytes int) []Piece {
	lines := strings.Split(source.Text, "\n")
	var pieces []Piece
	for start := 0; start < len(lines); {
		end := start
		for end < len(lines) && strings.TrimSpace(lines[end]) != "" {
			end++
		}
		for end < len(lines) && strings.TrimSpace(lines[end]) == "" {
			end++
		}
		fragment := source
		fragment.StartLine, fragment.EndLine = start+1, end
		fragment.Text = strings.Join(lines[start:end], "\n")
		text := formatMetadataSource(fragment)
		oversized := len(text) > maximumBytes
		if oversized {
			text = formatOversizedMetadataSource(fragment, maximumBytes)
		}
		pieces = append(pieces, Piece{
			Patch: "",
			Path:  "", Text: text, Header: fmt.Sprintf("lines %d-%d", fragment.StartLine, fragment.EndLine),
			CoverageComplete: !oversized, Oversized: oversized, Metadata: &fragment,
		})
		start = end
	}
	return pieces
}

func formatMetadataSource(source MetadataSource) string {
	return fmt.Sprintf("Metadata surface: %s\nCommit SHA: %s\nCurrent prose, lines %d-%d:\n%s\n",
		source.Surface, source.CommitSHA, source.StartLine, source.EndLine, source.Text)
}

func formatOversizedMetadataSource(source MetadataSource, maximumBytes int) string {
	return fmt.Sprintf("Metadata surface: %s\nCommit SHA: %s\nUnread prose, lines %d-%d: source exceeds maximum chunk size %d bytes\n",
		source.Surface, source.CommitSHA, source.StartLine, source.EndLine, maximumBytes)
}
