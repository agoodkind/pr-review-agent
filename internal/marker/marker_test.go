package marker

import (
	"strings"
	"testing"

	"goodkind.io/pr-review-agent/internal/domain"
)

func TestEndToEndFreshAppInstanceMarkerDedup(t *testing.T) {
	head := domain.HeadSHA("a3c4f1cac7f595bc824704b9d2a1f1191630dc32")
	firstBody := "Summary\n\n" + Review(head, domain.ReviewDecisionRequestChanges)
	secondBody := "Updated summary\n\n" + Review(head, domain.ReviewDecisionRequestChanges)

	if _, ok := FindReview(firstBody); !ok {
		t.Fatal("first review marker missing")
	}
	if _, ok := FindReview(secondBody); !ok {
		t.Fatal("second review marker missing")
	}
	if firstBody == secondBody {
		t.Fatal("different prose should not compare equal")
	}
}

func TestReviewMarkerRoundTrip(t *testing.T) {
	head := domain.HeadSHA("a3c4f1cac7f595bc824704b9d2a1f1191630dc32")
	body := "Summary text\n\n" + Review(head, domain.ReviewDecisionApprove)
	parsed, ok := FindReview(body)
	if !ok {
		t.Fatal("FindReview: not found")
	}
	if parsed != head {
		t.Fatalf("head = %q, want %q", parsed, head)
	}
}

// A verdict body carries no prose, so the marker is the only surviving record
// of what the review decided. Dismissing a review erases its state from GitHub,
// and the withheld-block rule reads that decision back from here.
func TestReviewMarkerRecordsTheDecision(t *testing.T) {
	head := domain.HeadSHA("a3c4f1cac7f595bc824704b9d2a1f1191630dc32")

	if !ReviewBlocked(Review(head, domain.ReviewDecisionRequestChanges)) {
		t.Fatal("a blocking marker did not read back as blocking")
	}
	if ReviewBlocked(Review(head, domain.ReviewDecisionApprove)) {
		t.Fatal("an approving marker read back as blocking")
	}
	// A marker written before the decision was recorded reads as not blocking,
	// so a block dismissed in that era is restated rather than withheld. That is
	// the older behavior and the safe direction to be wrong in.
	legacy := "<!-- pr-review-agent:review:v1 head=" + string(head) + " -->"
	if _, ok := FindReview(legacy); !ok {
		t.Fatal("a marker without a decision stopped being recognized")
	}
	if ReviewBlocked(legacy) {
		t.Fatal("a marker without a decision claimed to be blocking")
	}
}

func TestFindingMarkerChangesWithHead(t *testing.T) {
	finding := domain.Finding{
		Path:       "internal/app/handler.go",
		StartLine:  10,
		EndLine:    10,
		Title:      "Issue",
		Body:       "Details",
		Importance: 5,
	}
	headA := domain.HeadSHA("a3c4f1cac7f595bc824704b9d2a1f1191630dc32")
	headB := domain.HeadSHA("b4d5e2dbd8a606cd935815c0e3b2a2202741ed43")

	markerA, err := Finding(headA, finding)
	if err != nil {
		t.Fatalf("Finding headA: %v", err)
	}
	markerB, err := Finding(headB, finding)
	if err != nil {
		t.Fatalf("Finding headB: %v", err)
	}
	if markerA == markerB {
		t.Fatal("finding marker should change with head")
	}
}

func TestFindingMarkerIsStableForEquivalentPaths(t *testing.T) {
	findingA := domain.Finding{
		Path:       "internal\\app\\handler.go",
		StartLine:  10,
		EndLine:    10,
		Title:      "Issue",
		Body:       "Details",
		Importance: 5,
	}
	findingB := domain.Finding{
		Path:       "internal/app/handler.go",
		StartLine:  10,
		EndLine:    10,
		Title:      "Issue",
		Body:       "Details",
		Importance: 5,
	}
	head := domain.HeadSHA("a3c4f1cac7f595bc824704b9d2a1f1191630dc32")

	markerA, err := Finding(head, findingA)
	if err != nil {
		t.Fatalf("Finding findingA: %v", err)
	}
	markerB, err := Finding(head, findingB)
	if err != nil {
		t.Fatalf("Finding findingB: %v", err)
	}
	if markerA != markerB {
		t.Fatalf("markers differ for equivalent paths: %q vs %q", markerA, markerB)
	}
}

func TestFindingMarkerRejectsUnsafePaths(t *testing.T) {
	head := domain.HeadSHA("a3c4f1cac7f595bc824704b9d2a1f1191630dc32")
	cases := []domain.Finding{
		{Path: "../secret.go", StartLine: 1, EndLine: 1, Title: "t", Body: "b", Importance: 1},
		{Path: "/etc/passwd", StartLine: 1, EndLine: 1, Title: "t", Body: "b", Importance: 1},
	}
	for index, finding := range cases {
		if _, err := Finding(head, finding); err == nil {
			t.Fatalf("case %d: want unsafe path error", index)
		}
	}
}

// The claim sentence is how a reworded restatement is recognized, so the marker
// has to carry it across runs. It carries the hash and never the sentence: the
// comment is a public surface, and the claim is a label the model wrote for this
// service rather than for the reader.
func TestFindingMarkerCarriesTheClaimTextHashAndNotTheSentence(t *testing.T) {
	head := domain.HeadSHA("a3c4f1cac7f595bc824704b9d2a1f1191630dc32")
	const claim = "the produce authorization check is skipped for cached tokens"
	finding := domain.Finding{
		Path:       "internal/app/handler.go",
		StartLine:  10,
		EndLine:    10,
		Title:      "Authorize before serving a cached token",
		Body:       "The cached branch serves a token without the authorization check.",
		Evidence:   "return cached.Token, nil",
		Claim:      claim,
		Suggestion: "",
		Importance: 8,
	}

	body, err := EncodeFindingBody(head, finding)
	if err != nil {
		t.Fatalf("EncodeFindingBody: %v", err)
	}
	if strings.Contains(body, claim) {
		t.Fatalf("published body carries the claim sentence itself:\n%s", body)
	}

	wantKey, err := ClaimTextKey(claim)
	if err != nil {
		t.Fatalf("ClaimTextKey: %v", err)
	}
	if !strings.Contains(body, "claimtext="+wantKey) {
		t.Fatalf("published body missing claimtext=%s:\n%s", wantKey, body)
	}

	parsed, ok := FindFinding(body)
	if !ok {
		t.Fatalf("FindFinding: marker not found in\n%s", body)
	}
	if parsed.ClaimTextKey != wantKey {
		t.Fatalf("ClaimTextKey = %q, want %q", parsed.ClaimTextKey, wantKey)
	}
	if parsed.ClaimKey == "" {
		t.Fatal("ClaimKey is empty: the evidence key must survive beside the claim text key")
	}
}

// Rewording is the failure the claim exists to survive, and the smallest
// rewordings are the ones a person cannot see. Two reports of one defect that
// differ only in case, spacing, and a closing period are one claim.
func TestClaimTextKeyIgnoresCaseSpacingAndTrailingPunctuation(t *testing.T) {
	first, err := ClaimTextKey("The produce authorization check is skipped for cached tokens")
	if err != nil {
		t.Fatalf("ClaimTextKey first: %v", err)
	}
	second, err := ClaimTextKey("  the produce   authorization check\tis skipped for CACHED tokens.  ")
	if err != nil {
		t.Fatalf("ClaimTextKey second: %v", err)
	}
	if first != second {
		t.Fatalf("keys differ for one claim: %q vs %q", first, second)
	}

	other, err := ClaimTextKey("the produce authorization check runs twice for cached tokens")
	if err != nil {
		t.Fatalf("ClaimTextKey other: %v", err)
	}
	if other == first {
		t.Fatal("a different claim shares the key: the key states nothing")
	}

	if _, err := ClaimTextKey("   "); err == nil {
		t.Fatal("empty claim: want an error rather than a key every claimless finding shares")
	}
}

// Every finding already on an open pull request was published before this key
// existed. A marker without it has to keep decoding, and has to suppress
// nothing, because an empty key that matched would silence unrelated findings.
func TestAMarkerWithoutAClaimTextKeyDecodesWithAnEmptyKey(t *testing.T) {
	head := domain.HeadSHA("a3c4f1cac7f595bc824704b9d2a1f1191630dc32")
	historical := domain.Finding{
		Path:       "internal/app/handler.go",
		StartLine:  10,
		EndLine:    10,
		Title:      "Authorize before serving a cached token",
		Body:       "The cached branch serves a token without the authorization check.",
		Evidence:   "return cached.Token, nil",
		Claim:      "",
		Suggestion: "",
		Importance: 8,
	}

	body, err := EncodeFindingBody(head, historical)
	if err != nil {
		t.Fatalf("EncodeFindingBody: %v", err)
	}
	if strings.Contains(body, "claimtext=") {
		t.Fatalf("marker carries a claim text key for a finding with no claim:\n%s", body)
	}

	parsed, ok := FindFinding(body)
	if !ok {
		t.Fatalf("FindFinding: marker not found in\n%s", body)
	}
	if parsed.ClaimTextKey != "" {
		t.Fatalf("ClaimTextKey = %q, want empty", parsed.ClaimTextKey)
	}

	decodedHead, decoded, err := DecodeFindingBody(domain.ReviewComment{
		Path:      historical.Path,
		StartLine: historical.StartLine,
		EndLine:   historical.EndLine,
		Body:      body,
	})
	if err != nil {
		t.Fatalf("DecodeFindingBody: %v", err)
	}
	if decodedHead != head {
		t.Fatalf("head = %q, want %q", decodedHead, head)
	}
	if decoded.Claim != "" {
		t.Fatalf("decoded claim = %q, want empty: the comment never carries the sentence", decoded.Claim)
	}
}

func TestMarkerParserRejectsMalformedValues(t *testing.T) {
	if _, ok := FindReview("<!-- pr-review-agent:review:v1 head=ZZZZ -->"); ok {
		t.Fatal("malformed review marker accepted")
	}
	if _, ok := FindFinding("<!-- pr-review-agent:finding:v1 head=abc id=def -->"); ok {
		t.Fatal("malformed finding marker accepted")
	}
}

func TestFindingBodyRoundTripAndStableIdentityVerification(t *testing.T) {
	head := domain.HeadSHA("a3c4f1cac7f595bc824704b9d2a1f1191630dc32")
	finding := domain.Finding{
		Path:       "internal/app/handler.go",
		StartLine:  12,
		EndLine:    14,
		Title:      "Validate `payload` before enqueue",
		Body:       "An invalid `payload` reaches the queue and breaks processing. Validate `payload` before calling `enqueue`.",
		Suggestion: "if err := payload.Validate(); err != nil {\n\treturn err\n}",
		Importance: 7,
	}
	encoded, err := EncodeFindingBody(head, finding)
	if err != nil {
		t.Fatalf("EncodeFindingBody: %v", err)
	}
	decodedHead, decodedFinding, err := DecodeFindingBody(domain.ReviewComment{
		Path:      finding.Path,
		StartLine: finding.StartLine,
		EndLine:   finding.EndLine,
		Body:      encoded,
	})
	if err != nil {
		t.Fatalf("DecodeFindingBody: %v", err)
	}
	if decodedHead != head {
		t.Fatalf("head = %q, want %q", decodedHead, head)
	}
	if decodedFinding.Title != finding.Title ||
		decodedFinding.Body != finding.Body ||
		decodedFinding.Suggestion != finding.Suggestion {
		t.Fatalf("decoded finding = %+v, want %+v", decodedFinding, finding)
	}
	wantSuggestion := "```suggestion\n" + finding.Suggestion + "\n```"
	if !strings.Contains(encoded, wantSuggestion) {
		t.Fatalf("encoded body missing suggestion: %q", encoded)
	}

	historical := finding
	historical.Suggestion = ""
	historicalBody, err := EncodeFindingBody(head, historical)
	if err != nil {
		t.Fatalf("EncodeFindingBody historical: %v", err)
	}
	_, decodedHistorical, err := DecodeFindingBody(domain.ReviewComment{
		Path:      historical.Path,
		StartLine: historical.StartLine,
		EndLine:   historical.EndLine,
		Body:      historicalBody,
	})
	if err != nil {
		t.Fatalf("DecodeFindingBody historical: %v", err)
	}
	if decodedHistorical.Suggestion != "" {
		t.Fatalf("historical suggestion = %q, want empty", decodedHistorical.Suggestion)
	}

	tampered := strings.Replace(encoded, "Validate `payload` before enqueue", "Different defect", 1)
	if _, _, err := DecodeFindingBody(domain.ReviewComment{
		Path:      finding.Path,
		StartLine: finding.StartLine,
		EndLine:   finding.EndLine,
		Body:      tampered,
	}); err == nil {
		t.Fatal("changed identity: want error")
	}

	moved := finding
	moved.StartLine = 30
	moved.EndLine = 31
	moved.Body = "Updated wording after the code moved."
	moved.Importance = 9
	originalID, err := FindingID(finding)
	if err != nil {
		t.Fatalf("FindingID original: %v", err)
	}
	movedID, err := FindingID(moved)
	if err != nil {
		t.Fatalf("FindingID moved: %v", err)
	}
	if movedID != originalID {
		t.Fatalf("moved id = %q, want stable id %q", movedID, originalID)
	}
}
