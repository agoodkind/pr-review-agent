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
	reviewPrefix  = "<!-- pr-review-agent:review:v1 head="
	findingPrefix = "<!-- pr-review-agent:finding:v1 head="
	markerSuffix  = " -->"
)

var (
	reviewPattern  = regexp.MustCompile(`<!-- pr-review-agent:review:v1 head=([0-9a-f]{40}|[0-9a-f]{64}) -->`)
	findingPattern = regexp.MustCompile(
		`<!-- pr-review-agent:finding:v1 head=([0-9a-f]{40}|[0-9a-f]{64}) importance=([1-9]|10) id=([0-9a-f]{64}) -->`,
	)
)

// FindingMarker is the parsed finding marker embedded in a review comment body.
type FindingMarker struct {
	Head       domain.HeadSHA
	Importance int
	ID         string
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

// Finding returns the finding marker for one head and finding pair.
func Finding(head domain.HeadSHA, finding domain.Finding) (string, error) {
	id, err := findingID(head, finding)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(
		"%s%s importance=%d id=%s%s",
		findingPrefix,
		head,
		finding.Importance,
		id,
		markerSuffix,
	), nil
}

// FindFinding extracts a finding marker from a comment body.
func FindFinding(body string) (FindingMarker, bool) {
	matches := findingPattern.FindStringSubmatch(body)
	if len(matches) != 4 {
		return FindingMarker{Head: "", Importance: 0, ID: ""}, false
	}
	head, err := domain.ParseHeadSHA(matches[1])
	if err != nil {
		return FindingMarker{Head: "", Importance: 0, ID: ""}, false
	}
	var importance int
	if matches[2] == "10" {
		importance = 10
	} else {
		importance = int(matches[2][0] - '0')
	}
	return FindingMarker{Head: head, Importance: importance, ID: matches[3]}, true
}

// EncodeFindingBody renders the inline finding body with its marker.
func EncodeFindingBody(head domain.HeadSHA, finding domain.Finding) (string, error) {
	markerText, err := Finding(head, finding)
	if err != nil {
		return "", err
	}
	title := strings.TrimSpace(finding.Title)
	body := strings.TrimSpace(finding.Body)
	return fmt.Sprintf(
		"### %s\n\n%s\n\n%s",
		title,
		body,
		markerText,
	), nil
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

	finding := domain.Finding{
		Path:       comment.Path,
		StartLine:  comment.StartLine,
		EndLine:    comment.EndLine,
		Title:      title,
		Body:       body,
		Importance: marker.Importance,
	}
	if err := finding.Validate(); err != nil {
		return "", domain.Finding{}, errors.New("invalid finding")
	}

	recomputedID, err := findingID(marker.Head, finding)
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

func findingID(head domain.HeadSHA, finding domain.Finding) (string, error) {
	normalizedPath, err := NormalizePath(finding.Path)
	if err != nil {
		return "", err
	}
	title := strings.TrimSpace(finding.Title)
	body := strings.TrimSpace(finding.Body)
	headText := string(head)

	var buffer bytes.Buffer
	writeLengthHex(&buffer, len(headText))
	buffer.WriteString(headText)
	writeLengthHex(&buffer, len(normalizedPath))
	buffer.WriteString(normalizedPath)
	if finding.StartLine < 0 {
		return "", errors.New("start line out of range for marker hash")
	}
	if finding.EndLine < 0 {
		return "", errors.New("end line out of range for marker hash")
	}
	writeUint64Hex(&buffer, uint64(finding.StartLine))
	writeUint64Hex(&buffer, uint64(finding.EndLine))
	if finding.Importance < 0 || finding.Importance > 255 {
		return "", errors.New("importance out of range for marker hash")
	}
	fmt.Fprintf(&buffer, "%02x", finding.Importance)
	writeLengthHex(&buffer, len(title))
	buffer.WriteString(title)
	writeLengthHex(&buffer, len(body))
	buffer.WriteString(body)

	sum := sha256.Sum256(buffer.Bytes())
	return hex.EncodeToString(sum[:]), nil
}

func writeLengthHex(buffer *bytes.Buffer, length int) {
	fmt.Fprintf(buffer, "%08x", length)
}

func writeUint64Hex(buffer *bytes.Buffer, value uint64) {
	fmt.Fprintf(buffer, "%016x", value)
}
