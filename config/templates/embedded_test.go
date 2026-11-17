package templates_test

import (
	"reflect"
	"strings"
	"testing"

	"goodkind.io/pr-review-agent/config/templates"
)

func TestRenderRejectsMissingTemplate(t *testing.T) {
	text, err := templates.Render("output.does_not_exist", reflect.Value{})
	if err == nil || text != "" || !strings.Contains(err.Error(), "output.does_not_exist") {
		t.Fatalf(
			"Render must identify the missing template and reject its output: %q, %v",
			text,
			err,
		)
	}
}

func TestRenderRejectsInvalidTemplateData(t *testing.T) {
	data := reflect.ValueOf(struct{ Key string }{Key: "result"})
	text, err := templates.Render("output.detail_row", data)
	if err == nil || text != "" || !strings.Contains(err.Error(), "Value") {
		t.Fatalf(
			"Render must reject incomplete data without publishing partial output: %q, %v",
			text,
			err,
		)
	}
}
