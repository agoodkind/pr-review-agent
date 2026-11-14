package reviewrules_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"goodkind.io/pr-review-agent/internal/reviewrules"
)

type policySources struct {
	catalog   string
	prompts   string
	settings  string
	templates string
}

func (sources policySources) load() (reviewrules.Policy, error) {
	return reviewrules.LoadPolicy(
		sources.catalog,
		sources.prompts,
		sources.settings,
		sources.templates,
	)
}

func copyPolicySources(t *testing.T) policySources {
	t.Helper()
	directory := t.TempDir()

	sources := policySources{
		catalog:   filepath.Join(directory, "review-rules.json"),
		prompts:   filepath.Join(directory, "review-prompts.json"),
		settings:  filepath.Join(directory, "review-service.json"),
		templates: filepath.Join(directory, "templates"),
	}
	for _, destination := range []string{sources.catalog, sources.prompts, sources.settings} {
		copyPolicyFile(
			t,
			filepath.Join("..", "..", "config", filepath.Base(destination)),
			destination,
		)
	}

	if err := os.Mkdir(sources.templates, 0o700); err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(filepath.Join("..", "..", "config", "templates"))
	if err != nil {
		t.Fatal(err)
	}

	for _, entry := range entries {
		copyPolicyFile(
			t,
			filepath.Join("..", "..", "config", "templates", entry.Name()),
			filepath.Join(sources.templates, entry.Name()),
		)
	}

	return sources
}

func copyPolicyFile(t *testing.T, source, destination string) {
	t.Helper()
	data, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}

	writePolicyFile(t, destination, data)
}

func writePolicyFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestPolicyRequiresEachSource(t *testing.T) {
	for _, source := range []string{"catalog", "prompts", "settings", "templates"} {
		t.Run(
			source,
			func(t *testing.T) {
				sources := copyPolicySources(t)
				missing := filepath.Join(t.TempDir(), "missing")
				switch source {
				case "catalog":
					sources.catalog = missing
				case "prompts":
					sources.prompts = missing
				case "settings":
					sources.settings = missing
				case "templates":
					sources.templates = missing
				}

				if _, err := sources.load(); err == nil || !strings.Contains(err.Error(), missing) {
					t.Fatalf("Loading did not identify the missing source: %v", err)
				}
			},
		)
	}
}

func TestPolicyRejectsInvalidOutputTemplates(t *testing.T) {
	for _, scenario := range []string{"missing", "empty", "syntax", "field", "duplicate"} {
		t.Run(
			scenario,
			func(t *testing.T) {
				sources := copyPolicySources(t)
				path := filepath.Join(sources.templates, "native.summary_link.md.tmpl")
				switch scenario {
				case "missing":
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
				case "empty":
					writePolicyFile(t, path, []byte(" \n"))
				case "syntax":
					writePolicyFile(t, path, []byte("{{if .Input}}"))
				case "field":
					writePolicyFile(t, path, []byte("{{.UnknownField}}"))
				case "duplicate":
					prompts := readTextPartFile(t, "review-prompts.json")
					var templates map[string][]string
					decodeTextPartValue(t, prompts["templates"], &templates)
					templates["native.summary_link"] = []string{"Duplicate."}
					prompts["templates"] = encodeTextPartValue(t, templates)
					writePolicyFile(t, sources.prompts, encodeTextPartValue(t, prompts))
				}

				if _, err := sources.load(); err == nil {
					t.Fatal("Loading accepted an invalid output template.")
				}
			},
		)
	}
}

func TestPolicyRejectsFieldsInWrongSources(t *testing.T) {
	for _, scenario := range []string{
		"prompt setting",
		"prompt classification setting",
		"setting prompt",
		"setting classification prompt",
	} {
		t.Run(
			scenario,
			func(t *testing.T) {
				sources := copyPolicySources(t)
				path := sources.prompts
				fields := readTextPartFile(t, "review-prompts.json")
				if strings.HasPrefix(scenario, "setting") {
					path = sources.settings
					fields = readTextPartFile(t, "review-service.json")
				}

				switch scenario {
				case "prompt setting":
					fields["limits"] = encodeTextPartValue(
						t,
						map[string]int{"summary_sentences": 3},
					)
				case "setting prompt":
					fields["templates"] = encodeTextPartValue(
						t,
						map[string][]string{"unexpected": {"Unexpected."}},
					)
				default:
					var classification map[string]json.RawMessage
					decodeTextPartValue(t, fields["file_classification"], &classification)
					if scenario == "prompt classification setting" {
						classification["maximum_reason_bytes"] = encodeTextPartValue(t, 100)
					} else {
						classification["instructions"] = encodeTextPartValue(
							t,
							[]string{"Unexpected."},
						)
					}

					fields["file_classification"] = encodeTextPartValue(t, classification)
				}

				writePolicyFile(t, path, encodeTextPartValue(t, fields))
				if _, err := sources.load(); err == nil ||
					!strings.Contains(err.Error(), "unknown field") {
					t.Fatalf("Loading did not reject a field in the wrong source: %v", err)
				}
			},
		)
	}
}

func TestPolicyRejectsTemplatesInWrongSources(t *testing.T) {
	for _, scenario := range []string{
		"prompt in output directory",
		"rule in prompt JSON",
		"rule in output directory",
	} {
		t.Run(
			scenario,
			func(t *testing.T) {
				sources := copyPolicySources(t)
				prompts := readTextPartFile(t, "review-prompts.json")
				var templates map[string][]string
				decodeTextPartValue(t, prompts["templates"], &templates)
				var rejected string
				switch scenario {
				case "prompt in output directory":
					rejected = "classification.system"
					body := strings.Join(templates[rejected], "\n")
					delete(templates, rejected)
					writePolicyFile(
						t,
						filepath.Join(sources.templates, rejected+".md.tmpl"),
						[]byte(body),
					)
				case "rule in prompt JSON":
					rejected = "rule.functional_correctness"
					templates[rejected] = []string{"Duplicate rule."}
				case "rule in output directory":
					rejected = "rule.functional_correctness"
					writePolicyFile(
						t,
						filepath.Join(sources.templates, rejected+".md.tmpl"),
						[]byte("Duplicate rule."),
					)
				}

				prompts["templates"] = encodeTextPartValue(t, templates)
				writePolicyFile(t, sources.prompts, encodeTextPartValue(t, prompts))
				if _, err := sources.load(); err == nil || !strings.Contains(
					err.Error(),
					rejected,
				) {
					t.Fatalf("Loading did not reject the template in the wrong source: %v", err)
				}
			},
		)
	}
}

func TestPolicyFingerprintIncludesSettingsAndExactTemplateBytes(t *testing.T) {
	for _, source := range []string{"settings", "template"} {
		t.Run(
			source,
			func(t *testing.T) {
				sources := copyPolicySources(t)
				original, err := sources.load()
				if err != nil {
					t.Fatal(err)
				}

				repeated, err := sources.load()
				if err != nil {
					t.Fatal(err)
				}

				if original.Fingerprint() != repeated.Fingerprint() {
					t.Fatal("Loading the same sources changed the fingerprint.")
				}

				if source == "settings" {
					settings := readTextPartFile(t, "review-service.json")
					var limits map[string]int
					decodeTextPartValue(t, settings["limits"], &limits)
					limits["summary_sentences"]++
					settings["limits"] = encodeTextPartValue(t, limits)
					writePolicyFile(t, sources.settings, encodeTextPartValue(t, settings))
				} else {
					path := filepath.Join(sources.templates, "native.summary_link.md.tmpl")
					body, err := os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}

					writePolicyFile(t, path, append(body, '\n'))
				}

				changed, err := sources.load()
				if err != nil {
					t.Fatal(err)
				}

				if original.Fingerprint() == changed.Fingerprint() {
					t.Fatal("Changing a source did not change the fingerprint.")
				}

				if source == "template" {
					data := reviewrules.PromptData{SummaryURL: "https://example.com/review"}
					baseline, err := original.Render("native.summary_link", data)
					if err != nil {
						t.Fatal(err)
					}

					rendered, err := changed.Render("native.summary_link", data)
					if err != nil {
						t.Fatal(err)
					}

					if rendered != baseline+"\n" {
						t.Fatalf("Rendering changed the exact template bytes: %q", rendered)
					}
				}
			},
		)
	}
}
