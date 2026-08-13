package marker

import (
	"testing"

	"goodkind.io/pr-review-agent/internal/domain"
)

func TestEndToEndFreshAppInstanceMarkerDedup(t *testing.T) {
	head := domain.HeadSHA("a3c4f1cac7f595bc824704b9d2a1f1191630dc32")
	firstBody := "Summary\n\n" + Review(head)
	secondBody := "Updated summary\n\n" + Review(head)

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
	body := "Summary text\n\n" + Review(head)
	parsed, ok := FindReview(body)
	if !ok {
		t.Fatal("FindReview: not found")
	}
	if parsed != head {
		t.Fatalf("head = %q, want %q", parsed, head)
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

func TestMarkerParserRejectsMalformedValues(t *testing.T) {
	if _, ok := FindReview("<!-- pr-review-agent:review:v1 head=ZZZZ -->"); ok {
		t.Fatal("malformed review marker accepted")
	}
	if _, ok := FindFinding("<!-- pr-review-agent:finding:v1 head=abc id=def -->"); ok {
		t.Fatal("malformed finding marker accepted")
	}
}

func TestFindingBodyRoundTripAndHashVerification(t *testing.T) {
	head := domain.HeadSHA("a3c4f1cac7f595bc824704b9d2a1f1191630dc32")
	finding := domain.Finding{
		Path:       "internal/app/handler.go",
		StartLine:  12,
		EndLine:    14,
		Title:      "Missing validation",
		Body:       "Validate the webhook payload before enqueue.",
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
	if decodedFinding.Title != finding.Title || decodedFinding.Body != finding.Body {
		t.Fatalf("decoded finding = %+v, want %+v", decodedFinding, finding)
	}

	tampered := encoded + "x"
	if _, _, err := DecodeFindingBody(domain.ReviewComment{
		Path:      finding.Path,
		StartLine: finding.StartLine,
		EndLine:   finding.EndLine,
		Body:      tampered,
	}); err == nil {
		t.Fatal("tampered body: want error")
	}
}
