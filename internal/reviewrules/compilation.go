package reviewrules

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"text/template"
)

var (
	errPromptTemplateIncomplete  = errors.New("prompt template requires a name and nonempty body")
	errDuplicatePolicyTemplate   = errors.New("duplicate policy template")
	errOutputTemplateMissing     = errors.New("required output template is missing")
	errOutputTemplateEmpty       = errors.New("output template requires a name and nonempty body")
	errOutputTemplateUnsupported = errors.New("unsupported output template")
)

type missingTemplateError struct{ name string }

func (err *missingTemplateError) Error() string {
	return "required prompt template " + strconv.Quote(err.name) + " is missing"
}

func compilePolicyTemplates(
	rules catalogFile,
	prompts promptsFile,
	outputTemplates map[string]string,
) (map[string]*template.Template, error) {
	compiled := make(map[string]*template.Template, len(prompts.Templates))

	for name, body := range outputTemplates {
		if _, exists := prompts.Templates[name]; exists {
			slog.Warn("Policy template key duplicates a prompt template", "template", name)

			return nil, fmt.Errorf("%w %q", errDuplicatePolicyTemplate, name)
		}

		parsed, err := compileTemplate(name, body)
		if err != nil {
			return nil, err
		}

		compiled[name] = parsed
	}

	for name, parts := range prompts.Templates {
		body := parts.text()
		if strings.TrimSpace(name) == "" || strings.TrimSpace(body) == "" {
			return nil, errPromptTemplateIncomplete
		}

		parsed, err := compileTemplate(name, body)
		if err != nil {
			return nil, err
		}

		compiled[name] = parsed
	}

	for _, entry := range rules.Rules {
		name := "rule." + string(entry.ID)
		if _, exists := compiled[name]; exists {
			slog.Warn("Policy template key duplicates a rule template", "template", name)

			return nil, fmt.Errorf("%w %q", errDuplicatePolicyTemplate, name)
		}

		parsed, err := compileTemplate(name, entry.Instructions.text())
		if err != nil {
			return nil, err
		}

		compiled[name] = parsed
	}

	return compiled, nil
}

func readOutputTemplates(directory string) (map[string]string, error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		slog.Warn("Output template directory reading failed", "path", directory, "err", err)

		return nil, fmt.Errorf("read output template directory %q: %w", directory, err)
	}

	templates := make(map[string]string)

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md.tmpl") {
			continue
		}

		name := strings.TrimSuffix(entry.Name(), ".md.tmpl")
		if !slices.Contains(requiredOutputTemplates, name) {
			slog.Warn("Output template key is unsupported", "template", name)

			return nil, fmt.Errorf("%w %q", errOutputTemplateUnsupported, name)
		}

		body, err := os.ReadFile(filepath.Join(directory, entry.Name()))
		if err != nil {
			slog.Warn("Output template reading failed", "template", name, "err", err)

			return nil, fmt.Errorf("read output template %q: %w", name, err)
		}

		if strings.TrimSpace(name) == "" || strings.TrimSpace(string(body)) == "" {
			slog.Warn("Output template requires a name and nonempty body", "template", name)

			return nil, fmt.Errorf("%w: %q", errOutputTemplateEmpty, entry.Name())
		}

		templates[name] = string(body)
	}

	for _, name := range requiredOutputTemplates {
		if _, exists := templates[name]; !exists {
			slog.Warn("Required output template is missing", "template", name)

			return nil, fmt.Errorf("%w: %q", errOutputTemplateMissing, name)
		}
	}

	return templates, nil
}

var requiredOutputTemplates = []string{
	"native.summary_link",
	"native.request_changes",
	"inline.footer",
	"failure.saved",
	"failure.class.other",
	"failure.class.deadline",
	"failure.class.provider_unavailable",
	"failure.class.panic",
	"failure.class.daily_budget",
	"failure.class.usage_exceeded",
	"failure.class.rate_limited",
	"failure.dismiss",
	"failure.provider",
	"failure.service",
	"failure.details",
	"failure.row",
	"coverage.classification_incomplete",
	"coverage.requirement_conflict",
	"coverage.accepted",
	"coverage.pending",
	"coverage.check_title",
	"coverage.verdict_pending",
	"coverage.persistence_limit",
	"classification.required",
}

func requirePolicyTemplates(compiled map[string]*template.Template) error {
	for _, name := range []string{
		"report.context_omitted",
		"consolidate.context_omitted",
		"review.context_omitted",
		"review.path_instructions",
		"omissions.deferred",
		"catalog.review",
		"catalog.output",
		"catalog.rule",
		"review.system",
		"review.input",
		"report.system",
		"report.input",
		"reconcile.system",
		"reconcile.input",
		"consolidate.system",
		"consolidate.input",
		"consolidate.across.input",
		"disputes.input",
		"omissions.none",
		"omissions.input",
		"omissions.decision",
		"pull_request.input",
		"structured.output",
		"untrusted.policy",
		"probe.user",
		"schema.finding.surface",
		"schema.finding.commit_sha",
		"schema.finding.correction_action",
		"schema.finding.path",
		"schema.finding.start_line",
		"schema.finding.end_line",
	} {
		if _, exists := compiled[name]; !exists {
			return &missingTemplateError{name: name}
		}
	}

	return nil
}
