package reviewrules

import (
	"errors"
	"strconv"
	"strings"
	"text/template"
)

var errPromptTemplateIncomplete = errors.New("prompt template requires a name and nonempty body")

type missingTemplateError struct{ name string }

func (err *missingTemplateError) Error() string {
	return "required prompt template " + strconv.Quote(err.name) + " is missing"
}

func compilePolicyTemplates(
	rules catalogFile,
	prompts promptsFile,
) (map[string]*template.Template, error) {
	compiled := make(map[string]*template.Template, len(prompts.Templates))
	for name, body := range prompts.Templates {
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
		parsed, err := compileTemplate(name, entry.Instructions)
		if err != nil {
			return nil, err
		}

		compiled[name] = parsed
	}

	return compiled, nil
}

func requirePolicyTemplates(compiled map[string]*template.Template) error {
	for _, name := range []string{
		"report.context_omitted",
		"consolidate.context_omitted",
		"review.context_omitted",
		"review.path_instructions",
		"omissions.deferred",
		"coverage.requirement_conflict",
		"coverage.accepted",
		"coverage.pending",
		"coverage.check_title",
		"coverage.verdict_pending",
		"coverage.persistence_limit",
		"inline.footer",
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
