package review

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"goodkind.io/gklog"
	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
	"goodkind.io/pr-review-agent/internal/reviewrules"
)

// MetadataReassessor evaluates saved metadata findings independently of discovery.
type MetadataReassessor interface {
	ReassessMetadata(
		ctx context.Context,
		request MetadataReassessmentRequest,
	) ([]domain.MetadataFindingAssessment, error)
}

// MetadataReassessmentRequest supplies a saved finding's prompt and current field.
//
// FindingID identifies the single assessment that the response must contain.
// Source provides the complete current text for coordinate and evidence checks.
// For example, a response cannot quote text from an earlier description.
type MetadataReassessmentRequest struct {
	Prompt    string
	FindingID string
	Source    diff.MetadataSource
}

// MetadataReassessmentRequestSizer measures the complete provider request.
type MetadataReassessmentRequestSizer interface {
	MetadataReassessmentRequestBytes(string) (int, error)
}

// MetadataReassessmentPolicy applies current rules to explicit finding decisions.
func MetadataReassessmentPolicy(policy reviewrules.Policy) (string, error) {
	rules, err := policy.Catalog().Prompt(nil)
	if err != nil {
		slog.Warn("Metadata reassessment rules rendering failed", "err", err)
		return "", fmt.Errorf("render metadata reassessment rules: %w", err)
	}

	writing, err := policy.Catalog().OutputPolicy()
	if err != nil {
		slog.Warn("Metadata reassessment writing policy rendering failed", "err", err)
		return "", fmt.Errorf("render metadata reassessment writing policy: %w", err)
	}

	var data reviewrules.PromptData
	untrusted, err := policy.Render("untrusted.policy", data)
	if err != nil {
		slog.Warn("Metadata reassessment input policy rendering failed", "err", err)
		return "", fmt.Errorf("render metadata reassessment input policy: %w", err)
	}

	data.Rules, data.WritingPolicy, data.UntrustedPolicy = rules, writing, untrusted
	text, err := policy.Render("metadata.reassess.system", data)
	if err != nil {
		slog.Warn("Metadata reassessment system policy rendering failed", "err", err)
		return "", fmt.Errorf("render metadata reassessment system policy: %w", err)
	}

	return text, nil
}

func (service *Service) reassessMetadataFindings(
	ctx context.Context,
	job domain.ReviewJob,
	prior []domain.Finding,
	sources []diff.MetadataSource,
	pullRequest githubapp.PullRequest,
	settings reviewSettings,
) ([]domain.Finding, error) {
	logger := gklog.L(ctx)
	if len(prior) == 0 {
		return nil, nil
	}

	model, supported := service.model.(MetadataReassessor)
	if !supported {
		return prior, errors.New("model does not support explicit metadata reassessment")
	}

	index := currentMetadataIndex(sources, pullRequest)
	removed, err := service.restoreMissingCommitMetadata(ctx, job, prior, index, pullRequest)
	if err != nil {
		return prior, err
	}

	retained := make([]domain.Finding, 0, len(prior))
	for number, finding := range prior {
		if removed[finding.CommitSHA] && finding.EffectiveSurface() == domain.FindingCommitMessage {
			continue
		}

		source, found := index[finding.Target()]
		if !found {
			return prior, errors.New("current metadata field is unavailable for reassessment")
		}

		if source.Text == "" && !pullRequest.MetadataKnown {
			return prior, errors.New(
				"empty current metadata requires a complete pull request response",
			)
		}

		identifier := strconv.Itoa(number + 1)
		prompt, err := service.metadataReassessmentPrompt(
			ctx,
			finding,
			source,
			pullRequest,
			identifier,
		)
		if err != nil {
			return prior, err
		}

		if err := service.checkMetadataReassessmentSize(
			ctx,
			prompt,
			settings.maximumPromptBytes,
		); err != nil {
			return prior, err
		}

		request := MetadataReassessmentRequest{
			Prompt:    prompt,
			FindingID: identifier,
			Source:    source,
		}
		callContext, cancel := modelRequestContext(ctx, service.model, settings.chunkTimeout)
		assessments, err := model.ReassessMetadata(callContext, request)
		cancel()
		if err != nil {
			logger.WarnContext(
				ctx,
				"Metadata finding reassessment failed",
				"finding_id",
				identifier,
				"err",
				err,
			)

			return prior, fmt.Errorf("reassess metadata finding %s: %w", identifier, err)
		}

		assessment, err := request.ValidateAssessments(assessments)
		if err != nil {
			return prior, err
		}

		if assessment.Resolution != domain.ResolutionResolved {
			current, err := currentMetadataFinding(finding, assessment)
			if err != nil {
				return prior, err
			}

			retained = append(retained, current)
		}
	}

	return retained, nil
}

func currentMetadataIndex(
	sources []diff.MetadataSource,
	pullRequest githubapp.PullRequest,
) map[domain.FindingTarget]diff.MetadataSource {
	index := BuildMetadataIndex(sources)
	// Empty mutable fields still supply complete current evidence.
	index[domain.FindingTarget{Surface: domain.FindingPullRequestTitle, CommitSHA: ""}] = diff.MetadataSource{
		Surface:   domain.FindingPullRequestTitle,
		CommitSHA: "",
		Text:      pullRequest.Title,
		StartLine: 1,
		EndLine:   1,
	}
	index[domain.FindingTarget{Surface: domain.FindingPullRequestDescription, CommitSHA: ""}] = diff.MetadataSource{
		Surface:   domain.FindingPullRequestDescription,
		CommitSHA: "",
		Text:      pullRequest.Body,
		StartLine: 1,
		EndLine:   len(strings.Split(pullRequest.Body, "\n")),
	}

	return index
}

func currentMetadataFinding(
	finding domain.Finding,
	assessment domain.MetadataFindingAssessment,
) (domain.Finding, error) {
	finding.StartLine = assessment.StartLine
	finding.EndLine = assessment.EndLine
	finding.Evidence = assessment.Evidence
	finding.Body = assessment.Reason
	// Historical replacements can edit unrelated lines after a prose revision.
	finding.CorrectionAction = domain.CorrectionNone
	finding.Suggestion = ""
	if err := finding.Validate(); err != nil {
		slog.Warn("Current metadata finding validation failed", "err", err)
		return finding, fmt.Errorf("validate current metadata finding: %w", err)
	}

	return finding, nil
}

func (service *Service) metadataReassessmentPrompt(
	ctx context.Context,
	finding domain.Finding,
	source diff.MetadataSource,
	pullRequest githubapp.PullRequest,
	identifier string,
) (string, error) {
	logger := gklog.L(ctx)
	encoded, err := json.Marshal(finding)
	if err != nil {
		logger.WarnContext(ctx, "Saved metadata finding encoding failed", "err", err)
		return "", fmt.Errorf("encode saved metadata finding: %w", err)
	}

	mode := finding.Deduplication
	if mode == "" {
		mode = reviewrules.DeduplicateDefect
	}

	body := fmt.Sprintf(
		"Finding ID: %s\nCurrent head: %s\nCurrent revision: %s\nDeduplication mode: %s\n"+
			"Saved finding (historical assessment):\n%s\nComplete current field %s, lines 1-%d:\n%s",
		identifier,
		pullRequest.Head,
		diff.PullRequestRevision(pullRequest),
		mode,
		encoded,
		source.Label(),
		len(strings.Split(source.Text, "\n")),
		source.Text,
	)
	var data reviewrules.PromptData
	data.Input = service.reviewPolicy.WrapUntrusted(service.reviewPolicy.EscapeUntrusted(body))
	prompt, err := service.reviewPolicy.Render("metadata.reassess.input", data)
	if err != nil {
		logger.WarnContext(ctx, "Metadata reassessment prompt rendering failed", "err", err)
		return "", fmt.Errorf("render metadata reassessment input: %w", err)
	}

	return prompt, nil
}

func (service *Service) checkMetadataReassessmentSize(
	ctx context.Context,
	prompt string,
	maximumBytes int,
) error {
	logger := gklog.L(ctx)
	size := len(prompt)
	if sizer, supported := service.model.(MetadataReassessmentRequestSizer); supported {
		measured, err := sizer.MetadataReassessmentRequestBytes(prompt)
		if err != nil {
			logger.WarnContext(ctx, "Metadata reassessment request measurement failed", "err", err)
			return fmt.Errorf("measure metadata reassessment request: %w", err)
		}

		size = measured
	}

	if size > maximumBytes {
		return &diff.PromptSizeError{Actual: size, Limit: maximumBytes}
	}

	return nil
}

// ValidateAssessments checks a response against the requested finding.
//
// The SDK checks each provider response before accepting its result. A rejected
// response can trigger the configured fallback policy.
//
// A valid response meets these conditions:
//   - The response contains one assessment for the requested finding ID.
//   - Its coordinates identify lines within the complete current field.
//   - Its evidence matches the selected passage.
//   - Its evidence stays empty for an empty field.
//
// For example, a response cannot quote a deleted sentence.
func (request MetadataReassessmentRequest) ValidateAssessments(
	assessments []domain.MetadataFindingAssessment,
) (domain.MetadataFindingAssessment, error) {
	var empty domain.MetadataFindingAssessment
	if err := domain.ValidateMetadataAssessments(assessments); err != nil {
		slog.Warn("Metadata reassessment validation failed", "err", err)
		return empty, fmt.Errorf("validate metadata reassessment: %w", err)
	}

	if len(assessments) != 1 || assessments[0].FindingID != request.FindingID {
		return empty, errors.New("metadata reassessment did not decide the requested finding")
	}

	assessment := assessments[0]
	lines := strings.Split(request.Source.Text, "\n")
	if assessment.EndLine > len(lines) {
		return empty, errors.New("metadata assessment coordinates exceed the current field")
	}

	passage := strings.Join(lines[assessment.StartLine-1:assessment.EndLine], "\n")
	if request.Source.Text != "" &&
		(strings.TrimSpace(
			assessment.Evidence,
		) == "" || !strings.Contains(passage, assessment.Evidence)) {
		return empty, errors.New("metadata reassessment evidence does not match the current field")
	}

	if request.Source.Text == "" && assessment.Evidence != "" {
		return empty, errors.New("metadata reassessment quoted evidence from an empty field")
	}

	return assessment, nil
}
