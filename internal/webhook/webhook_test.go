package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"
)

func TestEndToEndPullRequestWebhookAcceptance(t *testing.T) {
	payload := []byte(`{
		"action":"opened",
		"installation":{"id":42},
		"repository":{"name":"repo","owner":{"login":"owner"}},
		"pull_request":{"number":7,"draft":false,"head":{"sha":"a3c4f1cac7f595bc824704b9d2a1f1191630dc32"}}
	}`)
	secret := []byte("secret")
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(payload)
	signature := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	if err := VerifySHA256(signature, secret, payload); err != nil {
		t.Fatalf("VerifySHA256: %v", err)
	}

	event, supported, err := ParsePullRequest("pull_request", "delivery-e2e", payload)
	if err != nil {
		t.Fatalf("ParsePullRequest: %v", err)
	}
	if !supported {
		t.Fatal("supported = false, want true")
	}
	if event.Head != "a3c4f1cac7f595bc824704b9d2a1f1191630dc32" {
		t.Fatalf("head = %q, want opened head sha", event.Head)
	}
}

func TestVerifySHA256AcceptsValidSignature(t *testing.T) {
	secret := []byte("secret")
	body := []byte(`{"action":"opened"}`)
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(body)
	signature := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	if err := VerifySHA256(signature, secret, body); err != nil {
		t.Fatalf("VerifySHA256: %v", err)
	}
}

func TestVerifySHA256RejectsMissingMalformedAndWrongSignatures(t *testing.T) {
	secret := []byte("secret")
	body := []byte(`{"action":"opened"}`)
	cases := []string{
		"",
		"sha1=deadbeef",
		"sha256=not-hex",
		"sha256=00",
	}
	for _, signature := range cases {
		if err := VerifySHA256(signature, secret, body); err != ErrInvalidSignature {
			t.Fatalf("signature %q: got %v, want ErrInvalidSignature", signature, err)
		}
	}
}

func TestParsePullRequestAcceptsFourSupportedActions(t *testing.T) {
	for _, action := range []string{"opened", "reopened", "ready_for_review", "synchronize"} {
		payload := map[string]any{
			"action": action,
			"installation": map[string]any{
				"id": float64(42),
			},
			"repository": map[string]any{
				"name": "repo",
				"owner": map[string]any{
					"login": "owner",
				},
			},
			"pull_request": map[string]any{
				"number": float64(7),
				"draft":  false,
				"head": map[string]any{
					"sha": "a3c4f1cac7f595bc824704b9d2a1f1191630dc32",
				},
			},
		}
		body, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		event, supported, err := ParsePullRequest("pull_request", "delivery-1", body)
		if err != nil {
			t.Fatalf("action %s: %v", action, err)
		}
		if !supported {
			t.Fatalf("action %s: not supported", action)
		}
		if event.Action != action {
			t.Fatalf("action = %q, want %q", event.Action, action)
		}
	}
}

func TestParsePullRequestIgnoresDraftAndUnsupportedEvents(t *testing.T) {
	draftPayload := []byte(`{
		"action":"opened",
		"installation":{"id":1},
		"repository":{"name":"repo","owner":{"login":"owner"}},
		"pull_request":{"number":1,"draft":true,"head":{"sha":"a3c4f1cac7f595bc824704b9d2a1f1191630dc32"}}
	}`)
	_, supported, err := ParsePullRequest("pull_request", "delivery-1", draftPayload)
	if err != nil {
		t.Fatalf("draft opened: %v", err)
	}
	if supported {
		t.Fatal("draft opened: want unsupported")
	}

	_, supported, err = ParsePullRequest("push", "delivery-2", []byte(`{}`))
	if err != nil {
		t.Fatalf("push event: %v", err)
	}
	if supported {
		t.Fatal("push event: want unsupported")
	}

	readyPayload := []byte(`{
		"action":"ready_for_review",
		"installation":{"id":1},
		"repository":{"name":"repo","owner":{"login":"owner"}},
		"pull_request":{"number":1,"draft":true,"head":{"sha":"a3c4f1cac7f595bc824704b9d2a1f1191630dc32"}}
	}`)
	_, supported, err = ParsePullRequest("pull_request", "delivery-3", readyPayload)
	if err != nil {
		t.Fatalf("ready_for_review draft: %v", err)
	}
	if !supported {
		t.Fatal("ready_for_review draft: want supported")
	}
}

func TestParsePullRequestRejectsMissingRequiredFields(t *testing.T) {
	body := []byte(`{"action":"opened"}`)
	_, _, err := ParsePullRequest("pull_request", "delivery-1", body)
	if err == nil {
		t.Fatal("missing fields: want error")
	}
}
