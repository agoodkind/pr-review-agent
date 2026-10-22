// Package reviewrules loads editorial policy and prompt templates at runtime.
package reviewrules

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"
	"strings"
	"text/template"
)

type (
	// ID identifies a configured rule across source revisions.
	ID string
	// Importance selects publication scores for categories without a fixed score.
	Importance map[ID]int
	// FailureCode identifies an API error code allowed in public diagnostics.
	FailureCode string
)

const (
	// MinimumImportance is the lower bound of the public finding contract.
	MinimumImportance = 1
	// MaximumImportance is the upper bound of the public finding contract.
	MaximumImportance = 10
)

type categorySettings struct {
	FixedImportance int  `json:"fixed_importance"`
	OutputPolicy    bool `json:"output_policy"`
}

type rule struct {
	ID            ID            `json:"id"`
	Deduplication Deduplication `json:"deduplication,omitempty"`
	Category      string        `json:"category"`
	Title         string        `json:"title"`
	Instructions  string        `json:"instructions"`
}

type catalogFile struct {
	Preamble         string                      `json:"preamble"`
	OutputPreamble   string                      `json:"output_preamble"`
	CategorySettings map[string]categorySettings `json:"category_settings"`
	Rules            []rule                      `json:"rules"`
}

type untrustedMarkers struct {
	Begin        string `json:"begin"`
	End          string `json:"end"`
	EscapedBegin string `json:"escaped_begin"`
	EscapedEnd   string `json:"escaped_end"`
}

type promptsFile struct {
	RelationshipReview      RelationshipReviewPolicy `json:"relationship_review"`
	FileAssessmentsDisplay  FindingDisplay           `json:"file_assessments_display"`
	FileClassification      FileClassificationPolicy `json:"file_classification"`
	MetadataFindingsDisplay FindingDisplay           `json:"metadata_findings_display"`
	UntrustedMarkers        untrustedMarkers         `json:"untrusted_markers"`
	Limits                  FormattingLimits         `json:"limits"`
	Templates               map[string]string        `json:"templates"`
	ProviderDetails         []ProviderDetailColumn   `json:"provider_details"`
	FailureCodes            []FailureCode            `json:"failure_codes"`
}

// FormattingLimits bounds generated reports using the loaded configuration.
type FormattingLimits struct {
	SummarySentences int `json:"summary_sentences"`
	WalkthroughItems int `json:"walkthrough_items"`
}

// PromptData is the concrete interpolation contract for configured templates.
type PromptData struct {
	Path              string
	Relationship      string
	SummaryURL        string
	Provider          string
	Model             string
	FailureClass      string
	FailureStatus     string
	FailureCode       string
	MinimumImportance int
	Rules             string
	WritingPolicy     string
	UntrustedPolicy   string
	Input             string
	Index             int
	Total             int
	Disputes          string
	SchemaName        string
	Schema            string
	Anchors           string
	SummarySentences  int
	WalkthroughItems  int
}

// Catalog uses immutable configuration shared by concurrent reviews.
type Catalog struct {
	configuration catalogFile
	templates     map[string]*template.Template
	limits        FormattingLimits
}

// Policy stores one loaded catalog and its configured prompt templates.
type Policy struct {
	relationshipReview      RelationshipReviewPolicy
	fingerprint             string
	fileAssessmentsDisplay  FindingDisplay
	fileClassification      FileClassificationPolicy
	metadataFindingsDisplay FindingDisplay
	catalog                 Catalog
	templates               map[string]*template.Template
	untrustedMarkers        untrustedMarkers
	limits                  FormattingLimits
	providerDetails         []ProviderDetailColumn
	failureCodes            []FailureCode
}

// LoadPolicy requires both configuration files and rejects malformed templates.
func LoadPolicy(catalogPath, promptsPath string) (Policy, error) {
	rules, err := decodeFile[catalogFile](catalogPath)
	if err != nil {
		return Policy{}, err
	}
	if err := validateCatalog(rules); err != nil {
		return Policy{}, err
	}
	prompts, err := decodeFile[promptsFile](promptsPath)
	if err != nil {
		return Policy{}, err
	}
	if err := validatePromptSettings(prompts); err != nil {
		return Policy{}, err
	}
	compiled := make(map[string]*template.Template, len(prompts.Templates))
	for name, body := range prompts.Templates {
		if strings.TrimSpace(name) == "" || strings.TrimSpace(body) == "" {
			return Policy{}, fmt.Errorf("prompt template requires a name and nonempty body")
		}
		parsed, err := compileTemplate(name, body)
		if err != nil {
			return Policy{}, err
		}
		compiled[name] = parsed
	}
	for _, entry := range rules.Rules {
		name := "rule." + string(entry.ID)
		parsed, err := compileTemplate(name, entry.Instructions)
		if err != nil {
			return Policy{}, err
		}
		compiled[name] = parsed
	}
	if _, exists := compiled["native.summary_link"]; !exists {
		return Policy{}, fmt.Errorf("required prompt template %q is missing", "native.summary_link")
	}
	if err := validateRelationshipTemplates(prompts.RelationshipReview, compiled); err != nil {
		return Policy{}, err
	}
	for _, name := range []string{"review.context_omitted", "review.path_instructions", "omissions.deferred", "coverage.pending", "coverage.check_title", "coverage.verdict_pending", "coverage.persistence_limit", "inline.footer", "failure.class.other", "failure.class.deadline", "failure.class.provider_unavailable", "failure.class.panic", "failure.class.daily_budget", "failure.class.usage_exceeded", "failure.class.rate_limited", "failure.dismiss", "failure.provider", "failure.service", "failure.details", "failure.row", "catalog.review", "catalog.output", "catalog.rule", "review.system", "review.input", "report.system", "report.input", "reconcile.system", "reconcile.input", "consolidate.system", "consolidate.input", "consolidate.across.input", "disputes.input", "omissions.none", "omissions.input", "omissions.decision", "pull_request.input", "structured.output", "untrusted.policy", "probe.user", "schema.finding.surface", "schema.finding.commit_sha", "schema.finding.correction_action", "schema.finding.path", "schema.finding.start_line", "schema.finding.end_line"} {
		if _, exists := compiled[name]; !exists {
			return Policy{}, fmt.Errorf("required prompt template %q is missing", name)
		}
	}
	encoded, err := json.Marshal(struct {
		Rules   catalogFile `json:"rules"`
		Prompts promptsFile `json:"prompts"`
	}{Rules: rules, Prompts: prompts})
	if err != nil {
		slog.Warn("Review policy identity encoding failed", "err", err)
		return Policy{}, fmt.Errorf("encode review policy identity: %w", err)
	}
	return Policy{
		relationshipReview:      prompts.RelationshipReview,
		fingerprint:             fmt.Sprintf("%x", sha256.Sum256(encoded)),
		fileAssessmentsDisplay:  prompts.FileAssessmentsDisplay,
		fileClassification:      prompts.FileClassification,
		metadataFindingsDisplay: prompts.MetadataFindingsDisplay,
		catalog:                 Catalog{configuration: rules, templates: compiled, limits: prompts.Limits}, templates: compiled,
		untrustedMarkers: prompts.UntrustedMarkers, limits: prompts.Limits,
		providerDetails: prompts.ProviderDetails,
		failureCodes:    prompts.FailureCodes,
	}, nil
}

// Fingerprint identifies the loaded rules and prompt configuration.
func (policy Policy) Fingerprint() string { return policy.fingerprint }

// FileAssessmentsDisplay controls file assessment details in the review summary.
func (policy Policy) FileAssessmentsDisplay() FindingDisplay { return policy.fileAssessmentsDisplay }

func validatePromptSettings(prompts promptsFile) error {
	if err := validateFindingDisplay(prompts.MetadataFindingsDisplay); err != nil {
		return err
	}
	if prompts.FileAssessmentsDisplay != FindingsExpanded && prompts.FileAssessmentsDisplay != FindingsCollapsed && prompts.FileAssessmentsDisplay != FindingsHidden {
		return fmt.Errorf("file_assessments_display must be expanded, collapsed, or hidden")
	}
	if err := prompts.FileClassification.Validate(); err != nil {
		return err
	}
	if err := prompts.RelationshipReview.Validate(); err != nil {
		return err
	}
	if prompts.UntrustedMarkers.Begin == "" || prompts.UntrustedMarkers.End == "" || prompts.UntrustedMarkers.EscapedBegin == "" || prompts.UntrustedMarkers.EscapedEnd == "" {
		return fmt.Errorf("prompt configuration requires all input markers")
	}
	if prompts.Limits.SummarySentences <= 0 || prompts.Limits.WalkthroughItems <= 0 {
		return fmt.Errorf("prompt formatting limits must be positive")
	}
	if err := validateProviderColumns(prompts.ProviderDetails); err != nil {
		return err
	}
	if len(prompts.Templates) == 0 {
		return fmt.Errorf("prompt configuration requires templates")
	}
	return nil
}

func compileTemplate(name, body string) (*template.Template, error) {
	parsed, err := template.New(name).Option("missingkey=error").Parse(body)
	if err != nil {
		slog.Warn("Prompt template parsing failed", "template", name, "err", err)
		return nil, fmt.Errorf("parse prompt template %q: %w", name, err)
	}
	var data PromptData
	if err := parsed.Execute(io.Discard, data); err != nil {
		slog.Warn("Prompt template field validation failed", "template", name, "err", err)
		return nil, fmt.Errorf("validate prompt template %q: %w", name, err)
	}
	return parsed, nil
}

func decodeFile[T catalogFile | promptsFile](path string) (T, error) {
	var target T
	data, err := os.ReadFile(path)
	if err != nil {
		slog.Warn("Policy configuration reading failed", "path", path, "err", err)
		return target, fmt.Errorf("read policy configuration %q: %w", path, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&target); err != nil {
		slog.Warn("Policy configuration decoding failed", "path", path, "err", err)
		return target, fmt.Errorf("decode policy configuration %q: %w", path, err)
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		return target, fmt.Errorf("policy configuration %q must contain one JSON value", path)
	}
	return target, nil
}

func validateCatalog(catalog catalogFile) error {
	if strings.TrimSpace(catalog.Preamble) == "" || strings.TrimSpace(catalog.OutputPreamble) == "" || len(catalog.Rules) == 0 || len(catalog.CategorySettings) == 0 {
		return fmt.Errorf("rule catalog requires preambles, categories, and rules")
	}
	for category, settings := range catalog.CategorySettings {
		if category == "" || settings.FixedImportance < 0 || settings.FixedImportance > MaximumImportance {
			return fmt.Errorf("rule category %q has invalid importance", category)
		}
	}
	seen := make(map[ID]struct{}, len(catalog.Rules))
	for _, entry := range catalog.Rules {
		if entry.Deduplication != "" && entry.Deduplication != DeduplicateDefect && entry.Deduplication != DeduplicateLocation {
			return fmt.Errorf("review rule %q deduplication must be defect or location", entry.ID)
		}
		if entry.ID == "" || entry.Title == "" || entry.Instructions == "" {
			return fmt.Errorf("review rule requires an ID, title, and instructions")
		}
		if _, exists := catalog.CategorySettings[entry.Category]; !exists {
			return fmt.Errorf("review rule %q has unknown category %q", entry.ID, entry.Category)
		}
		if _, duplicate := seen[entry.ID]; duplicate {
			return fmt.Errorf("duplicate review rule ID %q", entry.ID)
		}
		seen[entry.ID] = struct{}{}
	}
	return nil
}

// Catalog exposes rule queries without exposing mutable rule storage.
func (policy Policy) Catalog() Catalog { return policy.catalog }

// Render applies configured formatting limits before evaluating a loaded template.
func (policy Policy) Render(name string, data PromptData) (string, error) {
	return render(policy.templates, name, data, policy.limits)
}

func render(templates map[string]*template.Template, name string, data PromptData, limits FormattingLimits) (string, error) {
	data.SummarySentences = limits.SummarySentences
	data.WalkthroughItems = limits.WalkthroughItems
	selected, exists := templates[name]
	if !exists {
		return "", fmt.Errorf("prompt template %q is not configured", name)
	}
	var output strings.Builder
	if err := selected.Execute(&output, data); err != nil {
		slog.Warn("Prompt template rendering failed", "template", name, "err", err)
		return "", fmt.Errorf("render prompt template %q: %w", name, err)
	}
	return output.String(), nil
}

// Limits supplies the same constraints used by prompt text and report processing.
func (policy Policy) Limits() FormattingLimits { return policy.limits }

// ProviderDetails returns the configured collapsed table columns.
func (policy Policy) ProviderDetails() []ProviderDetailColumn {
	return slices.Clone(policy.providerDetails)
}

// RecognizesFailureCode uses failure_codes from the prompt configuration.
func (policy Policy) RecognizesFailureCode(code string) bool {
	return slices.Contains(policy.failureCodes, FailureCode(code))
}

// WrapUntrusted delimits source material using the configured protocol markers.
func (policy Policy) WrapUntrusted(input string) string {
	return policy.untrustedMarkers.Begin + "\n" + input + "\n" + policy.untrustedMarkers.End
}

// EscapeUntrusted prevents source text from closing a configured input boundary.
func (policy Policy) EscapeUntrusted(input string) string {
	input = strings.ReplaceAll(input, policy.untrustedMarkers.Begin, policy.untrustedMarkers.EscapedBegin)
	return strings.ReplaceAll(input, policy.untrustedMarkers.End, policy.untrustedMarkers.EscapedEnd)
}

// IDs returns a separate slice in configuration order for response schemas.
func (catalog Catalog) IDs() []ID {
	ids := make([]ID, 0, len(catalog.configuration.Rules))
	for _, entry := range catalog.configuration.Rules {
		ids = append(ids, entry.ID)
	}
	return ids
}

// ValidateImportance rejects overrides that disagree with category constraints.
func (catalog Catalog) ValidateImportance(overrides Importance) error {
	ids := make([]ID, 0, len(overrides))
	for id := range overrides {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		entry, err := catalog.findRule(id)
		if err != nil {
			return err
		}
		importance := overrides[id]
		if importance < MinimumImportance || importance > MaximumImportance {
			return fmt.Errorf("review rule %q importance must be between %d and %d", id, MinimumImportance, MaximumImportance)
		}
		fixed := catalog.configuration.CategorySettings[entry.Category].FixedImportance
		if fixed != 0 && importance != fixed {
			return fmt.Errorf("review rule %q category requires importance %d", id, fixed)
		}
	}
	return nil
}

// FindingImportance applies category constraints before model or rule scores.
func (catalog Catalog) FindingImportance(id ID, reported int, overrides Importance) (int, error) {
	entry, err := catalog.findRule(id)
	if err != nil {
		return 0, err
	}
	if fixed := catalog.configuration.CategorySettings[entry.Category].FixedImportance; fixed != 0 {
		return fixed, nil
	}
	if fixed, configured := overrides[id]; configured {
		return fixed, nil
	}
	if reported < MinimumImportance || reported > MaximumImportance {
		return 0, fmt.Errorf("review rule %q reported importance must be between %d and %d", id, MinimumImportance, MaximumImportance)
	}
	return reported, nil
}

// Prompt evaluates configured instructions and score annotations for review input.
func (catalog Catalog) Prompt(overrides Importance) (string, error) {
	rules, err := catalog.renderRules(overrides, false)
	if err != nil {
		return "", err
	}
	var data PromptData
	data.Input = catalog.configuration.Preamble
	data.Rules = rules
	return render(catalog.templates, "catalog.review", data, catalog.limits)
}

// OutputPolicy includes only categories configured for generated text.
func (catalog Catalog) OutputPolicy() (string, error) {
	rules, err := catalog.renderRules(nil, true)
	if err != nil {
		return "", err
	}
	var data PromptData
	data.Input = catalog.configuration.OutputPreamble
	data.Rules = rules
	return render(catalog.templates, "catalog.output", data, catalog.limits)
}

func (catalog Catalog) renderRules(overrides Importance, outputOnly bool) (string, error) {
	var output strings.Builder
	for _, entry := range catalog.configuration.Rules {
		settings := catalog.configuration.CategorySettings[entry.Category]
		if outputOnly && !settings.OutputPolicy {
			continue
		}
		importance := settings.FixedImportance
		if importance == 0 {
			importance = overrides[entry.ID]
		}
		var data PromptData
		data.MinimumImportance = importance
		data.Rules = entry.Title
		data.SchemaName = string(entry.ID)
		instructions, err := render(catalog.templates, "rule."+string(entry.ID), data, catalog.limits)
		if err != nil {
			return "", err
		}
		data.Input = instructions
		text, err := render(catalog.templates, "catalog.rule", data, catalog.limits)
		if err != nil {
			return "", err
		}
		output.WriteString(text)
	}
	return output.String(), nil
}

func (catalog Catalog) findRule(id ID) (rule, error) {
	for _, entry := range catalog.configuration.Rules {
		if entry.ID == id {
			return entry, nil
		}
	}
	return rule{}, fmt.Errorf("unknown review rule ID %q", id)
}
