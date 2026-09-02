// Package marker encodes and decodes durable review markers in GitHub bodies.
package marker

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"

	"goodkind.io/pr-review-agent/internal/domain"
)

const (
	reviewPrefix     = "<!-- pr-review-agent:review:v1 head="
	findingPrefix    = "<!-- pr-review-agent:finding:v1 head="
	summaryMarker    = "<!-- pr-review-agent:summary:v1 -->"
	markerSuffix     = " -->"
	suggestionPrefix = "```suggestion\n"
	suggestionSuffix = "\n```"
)

var (
	reviewPattern = regexp.MustCompile(`<!-- pr-review-agent:review:v1 head=([0-9a-f]{40}|[0-9a-f]{64}) -->`)
	// findingPattern accepts a marker with or without a claim key. Every comment
	// published before the key existed has none, and those comments outlive this
	// change on every open pull request, so a pattern requiring one would stop
	// recognizing the service's own findings.
	findingPattern = regexp.MustCompile(
		`<!-- pr-review-agent:finding:v1 head=([0-9a-f]{40}|[0-9a-f]{64}) importance=([1-9]|10) id=([0-9a-f]{64})(?: claim=([0-9a-f]{64}))? -->`,
	)
)

// FindingMarker is the parsed finding marker embedded in a review comment body.
type FindingMarker struct {
	Head       domain.HeadSHA
	Importance int
	ID         string
	// ClaimKey identifies what the finding is about rather than how it was
	// worded. It is empty on a marker written before the key existed, and an
	// empty key matches nothing.
	ClaimKey string
}

// Review returns the review marker for one head SHA.
func Review(head domain.HeadSHA) string {
	return reviewPrefix + string(head) + markerSuffix
}

// FindReview extracts a review marker head SHA from a review body.
func FindReview(body string) (domain.HeadSHA, bool) {
	matches := reviewPattern.FindStringSubmatch(body)
	if len(matches) != 2 {
		return "", false
	}
	head, err := domain.ParseHeadSHA(matches[1])
	if err != nil {
		return "", false
	}
	return head, true
}

// Summary returns the marker for the single editable review summary.
func Summary() string {
	return summaryMarker
}

// HasSummary reports whether a review owns the editable summary.
func HasSummary(body string) bool {
	return strings.Contains(body, summaryMarker)
}

// Finding returns the finding marker for one head and finding pair.
//
// The claim key is included when the finding carries an evidence line to derive
// it from, and left out otherwise. A finding with no evidence cannot be
// published anyway, so in practice every new marker carries one; leaving it out
// rather than writing an empty value keeps a keyless marker exactly the shape
// the ones already on open pull requests have.
func Finding(head domain.HeadSHA, finding domain.Finding) (string, error) {
	id, err := FindingID(finding)
	if err != nil {
		return "", err
	}
	body := fmt.Sprintf(
		"%s%s importance=%d id=%s",
		findingPrefix,
		head,
		finding.Importance,
		id,
	)
	if claim, claimErr := ClaimKey(finding.Path, finding.Evidence); claimErr == nil {
		body += " claim=" + claim
	}
	return body + markerSuffix, nil
}

// FindFinding extracts a finding marker from a comment body.
func FindFinding(body string) (FindingMarker, bool) {
	matches := findingPattern.FindStringSubmatch(body)
	if len(matches) != 5 {
		return FindingMarker{Head: "", Importance: 0, ID: "", ClaimKey: ""}, false
	}
	head, err := domain.ParseHeadSHA(matches[1])
	if err != nil {
		return FindingMarker{Head: "", Importance: 0, ID: "", ClaimKey: ""}, false
	}
	var importance int
	if matches[2] == "10" {
		importance = 10
	} else {
		importance = int(matches[2][0] - '0')
	}
	// matches[4] is empty for a marker written before the claim key existed.
	return FindingMarker{
		Head:       head,
		Importance: importance,
		ID:         matches[3],
		ClaimKey:   matches[4],
	}, true
}

// ClaimKey identifies what a finding is about rather than how it is worded.
//
// Rewording is the whole failure this exists for. One live pull request received
// the same ask five times under five different titles across two different
// paths, so any key derived from the title is a key that changes every time the
// model reaches for different words. The evidence line does not change: it is
// the one field the model must copy verbatim out of the source it was shown, so
// two findings resting on the same line are making a claim about the same code.
//
// The key is a hash rather than the line itself. The marker sits in a published
// comment, and a quoted source line there would put code in the reader's face
// for no reason. A hash compares exactly and shows nothing.
func ClaimKey(pathValue string, evidence string) (string, error) {
	normalizedPath, err := NormalizePath(pathValue)
	if err != nil {
		return "", err
	}
	line := normalizeEvidenceLine(evidence)
	if line == "" {
		return "", errors.New("finding carries no evidence line")
	}

	var buffer bytes.Buffer
	writeLengthHex(&buffer, len(normalizedPath))
	buffer.WriteString(normalizedPath)
	writeLengthHex(&buffer, len(line))
	buffer.WriteString(line)

	sum := sha256.Sum256(buffer.Bytes())
	return hex.EncodeToString(sum[:]), nil
}

// normalizeEvidenceLine reduces one quoted source line to the form two findings
// about the same code agree on.
//
// The source shown to the model is a diff, so the same line arrives with or
// without the marker the diff put on it depending on whether the model kept it.
// Indentation and inner spacing also move under a reformat while the line stays
// the same line. Neither difference is a different claim.
func normalizeEvidenceLine(evidence string) string {
	line := strings.TrimSpace(evidence)
	if line == "" {
		return ""
	}
	if line[0] == '+' || line[0] == '-' {
		line = strings.TrimSpace(line[1:])
	}
	return strings.Join(strings.Fields(line), " ")
}

// EncodeFindingBody renders the inline finding body with its marker.
func EncodeFindingBody(head domain.HeadSHA, finding domain.Finding) (string, error) {
	markerText, err := Finding(head, finding)
	if err != nil {
		return "", err
	}
	title := strings.TrimSpace(finding.Title)
	body := strings.TrimSpace(finding.Body)
	parts := []string{
		"### " + title,
		body,
	}
	if finding.Suggestion != "" {
		parts = append(parts, suggestionPrefix+finding.Suggestion+suggestionSuffix)
	}
	parts = append(parts, markerText)
	return strings.Join(parts, "\n\n"), nil
}

// DecodeFindingBody parses a finding from a GitHub root review comment body.
func DecodeFindingBody(comment domain.ReviewComment) (domain.HeadSHA, domain.Finding, error) {
	marker, ok := FindFinding(comment.Body)
	if !ok {
		return "", domain.Finding{}, errors.New("finding marker not found")
	}

	withoutMarker := findingPattern.ReplaceAllString(comment.Body, "")
	withoutMarker = strings.TrimSpace(withoutMarker)
	if !strings.HasPrefix(withoutMarker, "### ") {
		return "", domain.Finding{}, errors.New("finding title missing")
	}

	titleEnd := strings.Index(withoutMarker, "\n\n")
	if titleEnd < 0 {
		return "", domain.Finding{}, errors.New("finding title format invalid")
	}
	title := strings.TrimSpace(withoutMarker[len("### "):titleEnd])
	body := strings.TrimSpace(withoutMarker[titleEnd+2:])
	suggestion := ""
	suggestionSeparator := "\n\n" + suggestionPrefix
	if suggestionStart := strings.LastIndex(body, suggestionSeparator); suggestionStart >= 0 {
		if !strings.HasSuffix(body, suggestionSuffix) {
			return "", domain.Finding{}, errors.New("finding suggestion format invalid")
		}
		suggestion = strings.TrimSuffix(body[suggestionStart+len(suggestionSeparator):], suggestionSuffix)
		body = strings.TrimSpace(body[:suggestionStart])
	}

	finding := domain.Finding{
		Path:      comment.Path,
		StartLine: comment.StartLine,
		EndLine:   comment.EndLine,
		Title:     title,
		Body:      body,
		// The published comment never carries evidence or the claim sentence,
		// only their hashes, so a decoded finding has neither to recover.
		Evidence:   "",
		Claim:      "",
		Suggestion: suggestion,
		Importance: marker.Importance,
	}
	if err := finding.Validate(); err != nil {
		return "", domain.Finding{}, errors.New("invalid finding")
	}

	recomputedID, err := FindingID(finding)
	if err != nil {
		return "", domain.Finding{}, err
	}
	if recomputedID != marker.ID {
		return "", domain.Finding{}, errors.New("finding marker id mismatch")
	}

	return marker.Head, finding, nil
}

// NormalizePath canonicalizes a repository-relative path for marker hashing.
func NormalizePath(value string) (string, error) {
	normalized := strings.ReplaceAll(value, "\\", "/")
	normalized = path.Clean(normalized)
	if normalized == "." || normalized == "" {
		return "", errors.New("path is empty")
	}
	if strings.HasPrefix(normalized, "/") {
		return "", errors.New("absolute path is not allowed")
	}
	if normalized == ".." || strings.HasPrefix(normalized, "../") {
		return "", errors.New("path traversal is not allowed")
	}
	return normalized, nil
}

// FindingID returns the stable identity for a finding across pull request heads.
func FindingID(finding domain.Finding) (string, error) {
	normalizedPath, err := NormalizePath(finding.Path)
	if err != nil {
		return "", err
	}
	title := strings.TrimSpace(finding.Title)
	title = strings.ToLower(strings.Join(strings.Fields(title), " "))

	var buffer bytes.Buffer
	writeLengthHex(&buffer, len(normalizedPath))
	buffer.WriteString(normalizedPath)
	writeLengthHex(&buffer, len(title))
	buffer.WriteString(title)

	sum := sha256.Sum256(buffer.Bytes())
	return hex.EncodeToString(sum[:]), nil
}

func writeLengthHex(buffer *bytes.Buffer, length int) {
	fmt.Fprintf(buffer, "%08x", length)
}
