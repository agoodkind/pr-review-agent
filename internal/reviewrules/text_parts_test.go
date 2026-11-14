package reviewrules_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"goodkind.io/pr-review-agent/internal/reviewrules"
)

func TestPolicyAssemblesOrderedTextParts(t *testing.T) {
	catalog, prompts := readTextPartConfiguration(t)
	catalog["preamble"] = encodeTextPartValue(
		t,
		[]string{"First preamble.", "", "Second preamble."},
	)
	catalog["output_preamble"] = encodeTextPartValue(t, []string{"Output first.", "Output second."})

	var rules []map[string]json.RawMessage
	decodeTextPartValue(t, catalog["rules"], &rules)
	rules[0]["instructions"] = encodeTextPartValue(
		t,
		[]string{
			"Before {{.SchemaName}}.",
			"",
			"After {{.MinimumImportance}}.",
		},
	)
	catalog["rules"] = encodeTextPartValue(t, rules)

	var templates map[string]json.RawMessage
	decodeTextPartValue(t, prompts["templates"], &templates)
	templates["review.system"] = encodeTextPartValue(
		t,
		[]string{
			"First {{.Input}}.",
			"",
			"Second {{.Total}}.",
		},
	)
	prompts["templates"] = encodeTextPartValue(t, templates)

	var classification map[string]json.RawMessage
	decodeTextPartValue(t, prompts["file_classification"], &classification)
	classification["instructions"] = encodeTextPartValue(
		t,
		[]string{
			"Classify first.",
			"",
			"Classify second.",
		},
	)
	classification["path_instructions"] = encodeTextPartValue(
		t,
		map[string][]string{
			"*.go": {"Path first.", "Path second."},
		},
	)
	prompts["file_classification"] = encodeTextPartValue(t, classification)

	catalogPath, promptsPath := writeTextPartConfiguration(t, catalog, prompts)
	policy, err := loadTextPartPolicy(catalogPath, promptsPath)
	if err != nil {
		t.Fatal(err)
	}

	var data reviewrules.PromptData
	data.Input, data.Total = "value", 2
	rendered, err := policy.Render("review.system", data)
	if err != nil {
		t.Fatal(err)
	}

	if rendered != "First value.\n\nSecond 2." {
		t.Fatalf("The prompt changed its ordered parts: %q", rendered)
	}

	rendered, err = policy.Catalog().Prompt(reviewrules.Importance{"functional_correctness": 6})
	if err != nil {
		t.Fatal(err)
	}

	if !strings.HasPrefix(rendered, "First preamble.\n\nSecond preamble.") ||
		!strings.Contains(rendered, "Before functional_correctness.\n\nAfter 6.") {
		t.Fatalf("The catalog changed its ordered parts: %s", rendered)
	}

	rendered, err = policy.Catalog().OutputPolicy()
	if err != nil {
		t.Fatal(err)
	}

	if !strings.HasPrefix(rendered, "Output first.\nOutput second.") {
		t.Fatalf("The output policy changed its ordered parts: %s", rendered)
	}

	settings := policy.FileClassification()
	settings.Instructions[0] = "Changed."
	settings.PathInstructions["*.go"][0] = "Changed."
	if policy.FileReviewInstructions() != "Classify first.\n\nClassify second." ||
		policy.FileClassificationPathInstructions("example.go") != "\nPath first.\nPath second." {
		t.Fatal("Changing a policy copy changed the loaded instructions.")
	}
}

func TestPolicyRejectsInvalidTextParts(t *testing.T) {
	for _, field := range []string{
		"preamble",
		"output_preamble",
		"rule",
		"template",
		"classification",
		"path",
	} {
		for _, invalid := range []string{
			`"legacy scalar"`,
			`null`,
			`[]`,
			`[""]`,
			`["valid", null]`,
			`["valid", 3]`,
		} {
			t.Run(
				field+"/"+invalid,
				func(t *testing.T) {
					catalog, prompts := readTextPartConfiguration(t)
					setInvalidTextParts(t, catalog, prompts, field, json.RawMessage(invalid))
					catalogPath, promptsPath := writeTextPartConfiguration(t, catalog, prompts)
					if _, err := loadTextPartPolicy(catalogPath, promptsPath); err == nil {
						t.Fatalf("Policy loading accepted invalid %s: %s", field, invalid)
					}
				},
			)
		}
	}
}

func TestPolicyRejectsMissingPartsAndInvalidAssembledTemplate(t *testing.T) {
	t.Run(
		"missing preamble",
		func(t *testing.T) {
			catalog, prompts := readTextPartConfiguration(t)
			delete(catalog, "preamble")
			catalogPath, promptsPath := writeTextPartConfiguration(t, catalog, prompts)
			if _, err := loadTextPartPolicy(catalogPath, promptsPath); err == nil {
				t.Fatal("Policy loading accepted a missing preamble.")
			}
		},
	)

	t.Run(
		"invalid assembled template",
		func(t *testing.T) {
			catalog, prompts := readTextPartConfiguration(t)
			var templates map[string]json.RawMessage
			decodeTextPartValue(t, prompts["templates"], &templates)
			templates["review.system"] = encodeTextPartValue(
				t,
				[]string{
					"{{if .Input}}",
					"The template does not close its condition.",
				},
			)
			prompts["templates"] = encodeTextPartValue(t, templates)
			catalogPath, promptsPath := writeTextPartConfiguration(t, catalog, prompts)
			if _, err := loadTextPartPolicy(catalogPath, promptsPath); err == nil ||
				!strings.Contains(err.Error(), "review.system") {
				t.Fatalf("Policy loading did not identify the invalid assembled template: %v", err)
			}
		},
	)
}

func setInvalidTextParts(
	t *testing.T,
	catalog map[string]json.RawMessage,
	prompts map[string]json.RawMessage,
	field string,
	value json.RawMessage,
) {
	t.Helper()

	switch field {
	case "preamble", "output_preamble":
		catalog[field] = value
	case "rule":
		var rules []map[string]json.RawMessage
		decodeTextPartValue(t, catalog["rules"], &rules)
		rules[0]["instructions"] = value
		catalog["rules"] = encodeTextPartValue(t, rules)
	case "template":
		var templates map[string]json.RawMessage
		decodeTextPartValue(t, prompts["templates"], &templates)
		templates["review.system"] = value
		prompts["templates"] = encodeTextPartValue(t, templates)
	case "classification", "path":
		var classification map[string]json.RawMessage
		decodeTextPartValue(t, prompts["file_classification"], &classification)
		if field == "classification" {
			classification["instructions"] = value
		} else {
			classification["path_instructions"] = encodeTextPartValue(
				t,
				map[string]json.RawMessage{"*.go": value},
			)
		}

		prompts["file_classification"] = encodeTextPartValue(t, classification)
	}
}

func readTextPartConfiguration(
	t *testing.T,
) (map[string]json.RawMessage, map[string]json.RawMessage) {
	t.Helper()
	catalog := readTextPartFile(t, "review-rules.json")
	prompts := readTextPartFile(t, "review-prompts.json")

	return catalog, prompts
}

func readTextPartFile(t *testing.T, name string) map[string]json.RawMessage {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "config", name))
	if err != nil {
		t.Fatal(err)
	}

	var fields map[string]json.RawMessage
	decodeTextPartValue(t, data, &fields)

	return fields
}

func writeTextPartConfiguration(
	t *testing.T,
	catalog map[string]json.RawMessage,
	prompts map[string]json.RawMessage,
) (string, string) {
	t.Helper()
	directory := t.TempDir()
	catalogPath := filepath.Join(directory, "rules.json")
	promptsPath := filepath.Join(directory, "prompts.json")

	for path, fields := range map[string]map[string]json.RawMessage{
		catalogPath: catalog,
		promptsPath: prompts,
	} {
		if err := os.WriteFile(path, encodeTextPartValue(t, fields), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	return catalogPath, promptsPath
}

func encodeTextPartValue[T any](t *testing.T, value T) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}

	return data
}

func loadTextPartPolicy(catalogPath, promptsPath string) (reviewrules.Policy, error) {
	return reviewrules.LoadPolicy(
		catalogPath,
		promptsPath,
		filepath.Join("..", "..", "config", "review-service.json"),
		filepath.Join("..", "..", "config", "templates"),
	)
}

func decodeTextPartValue[T any](t *testing.T, data []byte, value *T) {
	t.Helper()
	if err := json.Unmarshal(data, value); err != nil {
		t.Fatal(err)
	}
}
