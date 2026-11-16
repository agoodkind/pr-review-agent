package review_test

import (
	"testing"

	"goodkind.io/pr-review-agent/internal/review"
)

func rendered(t testing.TB) func(string, error) string {
	t.Helper()

	return func(body string, err error) string {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}

		return body
	}
}

func admitted(t testing.TB) func(review.AdmissionVerdict, error) review.AdmissionVerdict {
	t.Helper()

	return func(verdict review.AdmissionVerdict, err error) review.AdmissionVerdict {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}

		return verdict
	}
}
