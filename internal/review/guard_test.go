package review

import "testing"

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

func admitted(t testing.TB) func(AdmissionVerdict, error) AdmissionVerdict {
	t.Helper()

	return func(verdict AdmissionVerdict, err error) AdmissionVerdict {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}

		return verdict
	}
}
