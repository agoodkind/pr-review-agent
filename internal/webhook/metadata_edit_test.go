package webhook_test

import (
	"fmt"
	"testing"

	"goodkind.io/pr-review-agent/internal/webhook"
)

func TestEditedWebhookSelectsMetadataScopeFromChangedFields(t *testing.T) {
	for _, testCase := range []struct {
		name         string
		changes      string
		metadataOnly bool
	}{
		{name: "title", changes: `{"title":{"from":"Earlier title"}}`, metadataOnly: true},
		{name: "description", changes: `{"body":{"from":"Earlier description"}}`, metadataOnly: true},
		{name: "title and description", changes: `{"title":{"from":"Earlier title"},"body":{"from":"Earlier description"}}`, metadataOnly: true},
		{name: "base", changes: `{"base":{"ref":{"from":"main"}}}`, metadataOnly: false},
		{name: "description and base", changes: `{"body":{"from":"Earlier description"},"base":{"ref":{"from":"main"}}}`, metadataOnly: false},
		{name: "unknown change", changes: `{"unknown":{"from":"Earlier value"}}`, metadataOnly: false},
		{name: "missing change details", changes: `null`, metadataOnly: false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			body := []byte(fmt.Sprintf(`{
				"action":"edited",
				"changes":%s,
				"installation":{"id":42},
				"repository":{"name":"repo","owner":{"login":"owner"}},
				"pull_request":{"number":7,"draft":false,"head":{"sha":"a3c4f1cac7f595bc824704b9d2a1f1191630dc32"}}
			}`, testCase.changes))
			event, supported, err := webhook.ParseEvent("pull_request", "metadata-edit", body)
			if err != nil || !supported {
				t.Fatalf("ParseEvent: supported=%v err=%v", supported, err)
			}
			job := event.Job()
			if job.MetadataOnly != testCase.metadataOnly || job.RefreshVerdict != testCase.metadataOnly || job.Forced == testCase.metadataOnly {
				t.Fatalf("job scope: metadata=%v refresh=%v forced=%v", job.MetadataOnly, job.RefreshVerdict, job.Forced)
			}
		})
	}
}
