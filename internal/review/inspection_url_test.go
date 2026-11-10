package review_test

import (
	"strings"
	"testing"

	"goodkind.io/pr-review-agent/internal/policytest"
	"goodkind.io/pr-review-agent/internal/review"
)

func TestPublicInspectionRejectsPrivateURLsAndDNSResults(t *testing.T) {
	fixture := newInspectionGitFixture(t)
	inspector, err := review.NewInspector(
		fixture.source,
		fixture.ref,
		fixture.files,
		policytest.Load(t),
		2048,
		6,
	)
	if err != nil {
		t.Fatal(err)
	}

	result, err := inspector.Inspect(
		t.Context(),
		review.InspectionRequest{
			Operation: review.InspectionReadURL,
			Target:    "https://localhost./",
			Reason:    "Read the linked public documentation.",
		},
	)
	if err == nil || result != "" || !strings.Contains(err.Error(), "private network address") {
		t.Fatalf("Public source inspection did not reject a private DNS result: %v", err)
	}

	for _, address := range []string{
		"http://example.com/docs",
		"https://user:password@example.com/docs",
		"https://127.0.0.1/docs",
		"https://[::1]/docs",
		"https://100.100.100.200/docs",
		"https://[::ffff:127.0.0.1]/",
		"https://10.0.0.1/",
		"https://169.254.169.254/",
		"https://100.64.0.1/",
		"https://[fe80::1]/",
		"https://[fd00::1]/",
	} {
		result, err := inspector.Inspect(
			t.Context(),
			review.InspectionRequest{
				Operation: review.InspectionReadURL,
				Target:    address,
				Reason:    "Read the linked public documentation.",
			},
		)
		if err == nil || result != "" {
			t.Fatal("Source inspection accepted a private or credential-bearing URL.")
		}
	}
}
