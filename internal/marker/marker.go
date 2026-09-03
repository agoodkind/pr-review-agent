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
	// reviewPattern accepts a marker with or without the decision. A verdict
	// body carries no prose any more, so the decision a dismissal erased from
	// GitHub's own record survives only here; markers written before this field
	// existed carry none, and those reviews outlive this change.
	reviewPattern = regexp.MustCompile(
		`<!-- pr-review-agent:review:v1 head=([0-9a-f]{40}|[0-9a-f]{64})(?: decision=(approve|request_changes))? -->`,
	)
	// findingPattern accepts a marker with either claim key, both, or neither.
	// Every comment published before a key existed has none, and those comments
	// outlive this change on every open pull request, so a pattern requiring one
	// would stop recognizing the service's own findings.
	findingPattern = regexp.MustCompile(
		`<!-- pr-review-agent:finding:v1 head=([0-9a-f]{40}|[0-9a-f]{64}) importance=([1-9]|10) id=([0-9a-f]{64})(?: claim=([0-9a-f]{64}))?(?: claimtext=([0-9a-f]{64}))? -->`,
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
	// ClaimTextKey identifies the defect the finding named, independent of the
	// line it anchored to. It is empty on a marker written before the key
	// existed or by an answer that carried no claim, and an empty key matches
	// nothing.
	ClaimTextKey string
}

// Review returns the review marker for one head SHA and the decision the review
// carried.
//
// The decision is recorded because a verdict body carries no prose. Dismissing a
// review rewrites its state to DISMISSED, so GitHub no longer says what it was,
// and this marker becomes the only surviving record of whether a person withdrew
// a block or an approval.
func Review(head domain.HeadSHA, decision domain.ReviewDecision) string {
	body := reviewPrefix + string(head)
	if wire := reviewDecisionWire(decision); wire != "" {
		body += " decision=" + wire
	}
	return body + markerSuffix
}

// reviewDecisionWire names a decision in the marker, or nothing for a decision
// that carries no verdict.
func reviewDecisionWire(decision domain.ReviewDecision) string {
	switch decision {
	case domain.ReviewDecisionApprove:
		return "approve"
	case domain.ReviewDecisionRequestChanges:
		return "request_changes"
	case domain.ReviewDecisionComment:
		// A comment review states no verdict, so there is none to record and
		// nothing a later dismissal could have withdrawn.
		return ""
	default:
		return ""
	}
}

// FindReview extracts a review marker head SHA from a review body.
func FindReview(body string) (domain.HeadSHA, bool) {
	matches := reviewPattern.FindStringSubmatch(body)
	if len(matches) < 2 {
		return "", false
	}
	head, err := domain.ParseHeadSHA(matches[1])
	if err != nil {
		return "", false
	}
	return head, true
}

// ReviewBlocked reports whether a review body's marker records a blocking
// verdict. A marker written before the decision was recorded reports false, so
// a block dismissed in that era is restated rather than withheld, which is the
// safe direction to be wrong in.
func ReviewBlocked(body string) bool {
	matches := reviewPattern.FindStringSubmatch(body)
	if len(matches) < 3 {
		return false
	}
	return matches[2] == "request_changes"
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
// Each claim key is included when the finding carries the field to derive it
// from, and left out otherwise. A finding with no evidence cannot be published
// anyway, so in practice every new marker carries the evidence key; an answer
// from an older schema carries no claim sentence, so it carries no claim text
// key. Leaving a key out rather than writing an empty value keeps a keyless
// marker exactly the shape the ones already on open pull requests have.
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
	if claimText, claimTextErr := ClaimTextKey(finding.Claim); claimTextErr == nil {
		body += " claimtext=" + claimText
	}
	return body + markerSuffix, nil
}

// FindFinding extracts a finding marker from a comment body.
func FindFinding(body string) (FindingMarker, bool) {
	matches := findingPattern.FindStringSubmatch(body)
	if len(matches) != 6 {
		return FindingMarker{Head: "", Importance: 0, ID: "", ClaimKey: "", ClaimTextKey: ""}, false
	}
	head, err := domain.ParseHeadSHA(matches[1])
	if err != nil {
		return FindingMarker{Head: "", Importance: 0, ID: "", ClaimKey: "", ClaimTextKey: ""}, false
	}
	var importance int
	if matches[2] == "10" {
		importance = 10
	} else {
		importance = int(matches[2][0] - '0')
	}
	// matches[4] and matches[5] are empty for a marker written before their key
	// existed, which is every marker already on an open pull request.
	return FindingMarker{
		Head:         head,
		Importance:   importance,
		ID:           matches[3],
		ClaimKey:     matches[4],
		ClaimTextKey: matches[5],
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

// ClaimTextKey identifies the defect a finding names, independent of the line
// it was anchored to.
//
// The evidence key covers two findings resting on the same source line. It does
// not cover the rest of the same failure: one live pull request received the
// same ask five times under five titles across two paths, and restatements that
// cite different lines of one function share no evidence key at all. The claim
// sentence is the model's own canonical label for the defect, so two findings
// carrying the same claim are making the same claim wherever each anchored it.
//
// The key is a hash rather than the sentence. The marker sits in a published
// comment, and the claim is a label the model wrote for this service rather than
// for the reader, so printing it would put text in front of a person that was
// never addressed to them. A hash compares exactly and shows nothing.
func ClaimTextKey(claim string) (string, error) {
	normalized := normalizeClaim(claim)
	if normalized == "" {
		return "", errors.New("finding carries no claim sentence")
	}

	var buffer bytes.Buffer
	writeLengthHex(&buffer, len(normalized))
	buffer.WriteString(normalized)

	sum := sha256.Sum256(buffer.Bytes())
	return hex.EncodeToString(sum[:]), nil
}

// normalizeClaim reduces one claim sentence to the form two findings about the
// same defect agree on.
//
// The model writes the sentence again every time it reports the defect, so one
// label arrives capitalized differently, spaced differently, and with or without
// a closing period. None of those is a different claim.
func normalizeClaim(claim string) string {
	normalized := strings.ToLower(strings.Join(strings.Fields(claim), " "))
	normalized = strings.TrimRight(normalized, ".,;:!?")
	return strings.TrimSpace(normalized)
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
