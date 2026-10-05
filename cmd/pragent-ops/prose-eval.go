package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"

	"goodkind.io/gklog"
	"goodkind.io/pr-review-agent/internal/cloudflareops"
	"goodkind.io/pr-review-agent/internal/config"
	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
	"goodkind.io/pr-review-agent/internal/openai"
	"goodkind.io/pr-review-agent/internal/review"
	"goodkind.io/pr-review-agent/internal/reviewrules"
)

type proseEvaluationOptions struct {
	dataset        string
	provider       string
	credentialFile string
	runtime        string
	signingKeyFile string
	output         string
	caseIDs        []string
}

type proseDataset struct {
	Cases []proseCase `json:"cases"`
}

type proseProvenance struct {
	Repository  string `json:"repository"`
	Revision    string `json:"revision"`
	URL         string `json:"url"`
	Description string `json:"description"`
}

type proseCase struct {
	ID         string             `json:"id"`
	Provenance proseProvenance    `json:"provenance"`
	Files      []proseFile        `json:"files"`
	Metadata   []proseMetadata    `json:"metadata,omitempty"`
	Expected   []proseExpectation `json:"expected"`
}

type proseFile struct {
	Path           string `json:"path"`
	Status         string `json:"status"`
	Patch          string `json:"patch"`
	CurrentContent string `json:"current_content"`
	SourceSHA256   string `json:"source_sha256"`
}

type proseExpectation struct {
	Surface        domain.FindingSurface     `json:"surface,omitempty"`
	CommitSHA      domain.HeadSHA            `json:"commit_sha,omitempty"`
	RuleIDs        []reviewrules.ID          `json:"rule_ids"`
	Path           string                    `json:"path"`
	StartLine      int                       `json:"start_line"`
	EndLine        int                       `json:"end_line"`
	AllowedActions []domain.CorrectionAction `json:"allowed_actions,omitempty"`
}

type proseMetadata struct {
	Surface      domain.FindingSurface `json:"surface"`
	CommitSHA    domain.HeadSHA        `json:"commit_sha"`
	Text         string                `json:"text"`
	StartLine    int                   `json:"start_line"`
	EndLine      int                   `json:"end_line"`
	SourceSHA256 string                `json:"source_sha256"`
}

func (expected proseExpectation) target() domain.FindingTarget {
	var finding domain.Finding
	finding.Surface = expected.Surface
	finding.CommitSHA = expected.CommitSHA
	return finding.Target()
}

type proseCaseResult struct {
	ID                string              `json:"case_id"`
	Provenance        proseProvenance     `json:"provenance"`
	SourceSHA256      map[string]string   `json:"source_sha256"`
	PromptSHA256      []string            `json:"prompt_sha256"`
	PromptBytes       []int               `json:"prompt_bytes"`
	Completed         bool                `json:"completed"`
	RawFindings       []domain.Finding    `json:"raw_findings"`
	Findings          []domain.Finding    `json:"findings"`
	Detected          []proseExpectation  `json:"detected"`
	Missed            []proseExpectation  `json:"missed"`
	NotEvaluated      []proseExpectation  `json:"not_evaluated"`
	FalsePositives    []domain.Finding    `json:"false_positives"`
	UnsafeCorrections []domain.Finding    `json:"unsafe_corrections"`
	Usage             review.UsageSummary `json:"usage"`
	HTTPStatuses      map[int]int         `json:"http_statuses"`
	HTTPErrors        []probeHTTPError    `json:"http_errors"`
	Errors            []string            `json:"errors"`
}

type proseEvaluationResult struct {
	Provider           string                 `json:"provider"`
	ConfiguredModel    string                 `json:"configured_model"`
	ReasoningEffort    config.ReasoningEffort `json:"reasoning_effort"`
	DatasetSHA256      string                 `json:"dataset_sha256"`
	PolicySHA256       string                 `json:"policy_sha256"`
	MaximumPromptBytes int                    `json:"configured_max_prompt_bytes"`
	Cases              []proseCaseResult      `json:"cases"`
	Detected           int                    `json:"detected"`
	Missed             int                    `json:"missed"`
	FalsePositives     int                    `json:"false_positives"`
	FailedCases        int                    `json:"failed_cases"`
	UnsafeCorrections  int                    `json:"unsafe_corrections"`
}

func proseEvaluationFlags(args []string, stderr io.Writer) (proseEvaluationOptions, error) {
	var options proseEvaluationOptions
	set := flag.NewFlagSet("prose-eval", flag.ContinueOnError)
	set.SetOutput(stderr)
	set.StringVar(&options.dataset, "dataset", "", "Source-derived evaluation cases and expected findings.")
	set.StringVar(&options.provider, "provider", "", "Exact configured provider ID.")
	set.StringVar(&options.credentialFile, "credential-file", "", "Private provider API key file.")
	set.StringVar(&options.runtime, "runtime", "runtime.json", "Public runtime configuration.")
	set.StringVar(&options.signingKeyFile, "signing-key-file", "", "Budget signing key file; required for capped providers.")
	set.StringVar(&options.output, "output-dir", "", "New private result directory.")
	set.Func("case", "Evaluate one exact case ID; repeat to select several cases.", func(id string) error {
		options.caseIDs = append(options.caseIDs, id)
		return nil
	})
	if err := set.Parse(args); err != nil {
		return options, err
	}
	if set.NArg() != 0 || options.dataset == "" || options.provider == "" || options.credentialFile == "" {
		return options, errors.New("prose-eval requires --dataset, --provider, and --credential-file with no positional arguments")
	}
	return options, nil
}

func proseEval(ctx context.Context, args []string, stdout io.Writer, stderr io.Writer) error {
	options, err := proseEvaluationFlags(args, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	if err != nil {
		return err
	}
	directory, err := cloudflareops.PrivateDirectory(options.output)
	if err != nil {
		return err
	}
	logFile, err := os.OpenFile(filepath.Join(directory, "logs.ndjson"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer func() { _ = logFile.Close() }()
	logger := slog.New(slog.NewJSONHandler(logFile, nil))
	ctx = gklog.WithLogger(ctx, logger)
	cfg, err := proseEvaluationConfig(options)
	if err != nil {
		return err
	}
	datasetBytes, err := os.ReadFile(options.dataset)
	if err != nil {
		logger.WarnContext(ctx, "Evaluation dataset read failed")
		return fmt.Errorf("read evaluation dataset: %w", err)
	}
	var dataset proseDataset
	if err = json.Unmarshal(datasetBytes, &dataset); err != nil {
		return errors.New("evaluation dataset is invalid JSON")
	}
	if err = validateProseDataset(dataset, cfg.ReviewPolicy.Catalog()); err != nil {
		return err
	}
	dataset.Cases, err = selectProseCases(dataset.Cases, options.caseIDs)
	if err != nil {
		return err
	}
	selected := cfg.Providers[0]
	policyHeader, err := review.PolicyHeader(cfg.ReviewPolicy, cfg.MinimumImportance, cfg.RuleImportance)
	if err != nil {
		return err
	}
	result := proseEvaluationResult{
		Provider: selected.ID, ConfiguredModel: selected.Model, ReasoningEffort: selected.ReasoningEffort,
		DatasetSHA256: proseSHA256(datasetBytes), PolicySHA256: proseSHA256([]byte(policyHeader)),
		MaximumPromptBytes: cfg.PromptBytes(),
		Cases:              nil, Detected: 0, Missed: 0, FalsePositives: 0, FailedCases: 0, UnsafeCorrections: 0,
	}
	for _, scenario := range dataset.Cases {
		caseResult, caseErr := evaluateProseCase(ctx, cfg, scenario)
		if caseErr != nil {
			return caseErr
		}
		result.Cases = append(result.Cases, caseResult)
		result.Detected += len(caseResult.Detected)
		result.Missed += len(caseResult.Missed)
		result.FalsePositives += len(caseResult.FalsePositives)
		result.UnsafeCorrections += len(caseResult.UnsafeCorrections)
		if !caseResult.Completed {
			result.FailedCases++
		}
		if err = writeProseEvaluation(directory, result); err != nil {
			return err
		}
		if ctx.Err() != nil {
			return errors.New("evaluation interrupted; partial results are saved in the private output directory")
		}
	}
	if _, err = fmt.Fprintf(stdout, "Cases: %d. Detected: %d. Missed: %d. Unexpected findings: %d. Unsafe corrections: %d. Failed cases: %d.\nPrivate results: %s\n", len(result.Cases), result.Detected, result.Missed, result.FalsePositives, result.UnsafeCorrections, result.FailedCases, filepath.Join(directory, "result.json")); err != nil {
		return err
	}
	if result.Missed != 0 || result.FalsePositives != 0 || result.UnsafeCorrections != 0 || result.FailedCases != 0 {
		return errors.New("prose evaluation did not pass; private results contain missed findings, unexpected findings, unsafe corrections, or incomplete cases")
	}
	return nil
}

func selectProseCases(cases []proseCase, selected []string) ([]proseCase, error) {
	if len(selected) == 0 {
		return cases, nil
	}
	byID := make(map[string]proseCase)
	for _, scenario := range cases {
		byID[scenario.ID] = scenario
	}
	result := make([]proseCase, 0, len(selected))
	seen := make(map[string]bool)
	for _, id := range selected {
		scenario, found := byID[id]
		if !found || seen[id] {
			return nil, errors.New("selected evaluation case is unknown or duplicated")
		}
		seen[id] = true
		result = append(result, scenario)
	}
	return result, nil
}

func proseEvaluationConfig(options proseEvaluationOptions) (config.Config, error) {
	runtime, err := ReadProbeRuntime(options.runtime)
	if err != nil {
		return config.Config{}, errors.New("public runtime configuration could not be read")
	}
	credential, err := cloudflareops.ReadCredential(options.credentialFile)
	if err != nil {
		return config.Config{}, err
	}
	return probeConfig(runtime, options.provider, string(credential), options.signingKeyFile)
}

func validateProseDataset(dataset proseDataset, catalog reviewrules.Catalog) error {
	if len(dataset.Cases) == 0 {
		return errors.New("evaluation dataset requires at least one case")
	}
	seen := make(map[string]bool)
	for _, scenario := range dataset.Cases {
		if scenario.ID == "" || seen[scenario.ID] || len(scenario.Files)+len(scenario.Metadata) == 0 {
			return errors.New("evaluation cases require unique IDs and source content")
		}
		seen[scenario.ID] = true
		if scenario.Provenance.URL == "" && scenario.Provenance.Description == "" {
			return errors.New("evaluation cases require source provenance")
		}
		for _, expected := range scenario.Expected {
			if expected.target().Surface == domain.FindingFile && expected.Path == "" || expected.StartLine <= 0 || expected.EndLine < expected.StartLine || len(expected.RuleIDs) == 0 {
				return errors.New("expected findings require rule IDs, a valid source target, and a valid line range")
			}
			for _, id := range expected.RuleIDs {
				if !slices.Contains(catalog.IDs(), id) {
					return fmt.Errorf("expected finding has unknown rule ID: %s", id)
				}
			}
			for _, action := range expected.AllowedActions {
				switch action {
				case domain.CorrectionNone, domain.CorrectionReplace, domain.CorrectionDelete:
				default:
					return errors.New("expected finding has an unknown correction action")
				}
			}
		}
	}
	return nil
}

func proseCaseInput(scenario proseCase) (diff.ReviewInput, map[string]string, error) {
	input := diff.ReviewInput{PullRequest: githubapp.PullRequest{}, Files: nil, Metadata: nil, MergeBase: ""}
	hashes := make(map[string]string)
	for _, file := range scenario.Files {
		if file.Path == "" || file.Patch == "" || file.CurrentContent == "" || file.Status == "" {
			return input, hashes, errors.New("evaluation source files require path, status, patch, and current_content")
		}
		hash := proseSHA256([]byte(file.CurrentContent))
		if file.SourceSHA256 != "" && file.SourceSHA256 != hash {
			return input, hashes, errors.New("evaluation source hash does not match current_content")
		}
		if err := diff.ValidatePatchContent(file.Patch, file.CurrentContent); err != nil {
			return input, hashes, err
		}
		lines, hunks, err := diff.ChangedRightLines(file.Patch)
		if err != nil {
			return input, hashes, err
		}
		hashes[file.Path] = hash
		input.Files = append(input.Files, diff.FileContext{
			Path: file.Path, Status: file.Status, Patch: file.Patch, CurrentContent: file.CurrentContent,
			ChangedRightLines: lines, ChangedRightHunks: hunks, CoverageComplete: true, Gap: diff.CoverageGapNone,
		})
	}
	for _, metadata := range scenario.Metadata {
		hash := proseSHA256([]byte(metadata.Text))
		if metadata.SourceSHA256 != "" && metadata.SourceSHA256 != hash {
			return input, hashes, errors.New("evaluation metadata hash does not match text")
		}
		source := diff.MetadataSource{
			Surface: metadata.Surface, CommitSHA: metadata.CommitSHA, Text: metadata.Text,
			StartLine: metadata.StartLine, EndLine: metadata.EndLine,
		}
		hashes[source.Label()] = hash
		input.Metadata = append(input.Metadata, source)
	}
	return input, hashes, nil
}

func evaluateProseCase(ctx context.Context, cfg config.Config, scenario proseCase) (proseCaseResult, error) {
	ctx = gklog.WithLogger(ctx, gklog.L(ctx).With("case_id", scenario.ID))
	input, hashes, err := proseCaseInput(scenario)
	if err != nil {
		return proseCaseResult{}, err
	}
	chunks, err := diff.ChunkInput(input, cfg.PromptBytes())
	if err != nil {
		return proseCaseResult{}, err
	}
	chunks, err = review.BoundReviewChunks(chunks, input.Files, cfg.MinimumImportance, "", cfg.ReviewPolicy, cfg.PromptBytes())
	if err != nil {
		return proseCaseResult{}, err
	}
	if len(chunks) == 0 {
		return proseCaseResult{}, errors.New("evaluation input contains no reviewable source")
	}
	fileIndex := review.BuildFileIndex(input.Files)
	metadataIndex := review.BuildMetadataIndex(input.Metadata)
	ctx, recorder := review.WithUsageRecorder(ctx)
	transport := &probeTransport{base: http.DefaultTransport, statuses: make(map[int]int)}
	client := openai.NewClient(cfg, &http.Client{Transport: transport})
	result := proseCaseResult{
		ID: scenario.ID, Provenance: scenario.Provenance, SourceSHA256: hashes, PromptSHA256: nil, PromptBytes: nil,
		Completed: true, RawFindings: nil, Findings: nil, Detected: nil, Missed: nil, NotEvaluated: nil, FalsePositives: nil, UnsafeCorrections: nil,
		Usage: review.UsageSummary{}, HTTPStatuses: nil, HTTPErrors: nil, Errors: nil,
	}
	for _, chunk := range chunks {
		if !chunk.CoverageComplete {
			return result, errors.New("evaluation input has incomplete hunk coverage")
		}
		prompt, promptErr := review.BuildPrompt(chunk, cfg.MinimumImportance, "", cfg.ReviewPolicy, cfg.PromptBytes())
		if promptErr != nil {
			return result, promptErr
		}
		result.PromptSHA256 = append(result.PromptSHA256, proseSHA256([]byte(prompt)))
		result.PromptBytes = append(result.PromptBytes, len(prompt))
		requestCtx, cancel := context.WithTimeout(ctx, cfg.ReviewChunkTimeout)
		completion, requestErr := client.Review(requestCtx, prompt)
		cancel()
		if requestErr != nil {
			result.Completed = false
			result.Errors = append(result.Errors, requestErr.Error())
			continue
		}
		result.RawFindings = append(result.RawFindings, completion.Result.Findings...)
		grounded := review.GroundedFindings(ctx, completion.Result.Findings, fileIndex, metadataIndex, chunk.Text)
		result.Findings = append(result.Findings, review.EligibleFindings(grounded, fileIndex, metadataIndex, cfg.MinimumImportance)...)
	}
	result.Usage = recorder.Summary()
	if result.Usage.ReportedRequests == 0 || result.Usage.TotalTokens <= 0 {
		result.Completed = false
		result.Errors = append(result.Errors, "the provider did not report positive token usage")
	}
	result.HTTPStatuses = transport.statuses
	result.HTTPErrors = transport.errors
	if transport.captureErr != nil {
		result.Completed = false
		result.Errors = append(result.Errors, transport.captureErr.Error())
	}
	matchProseExpectations(&result, scenario.Expected)
	return result, nil
}

func matchProseExpectations(result *proseCaseResult, expected []proseExpectation) {
	matched := make([]bool, len(expected))
	for _, finding := range result.Findings {
		found := false
		for index, expectation := range expected {
			if !matched[index] && finding.Target() == expectation.target() && finding.Path == expectation.Path && finding.StartLine <= expectation.EndLine && finding.EndLine >= expectation.StartLine && slices.Contains(expectation.RuleIDs, finding.RuleID) {
				matched[index] = true
				result.Detected = append(result.Detected, expectation)
				if len(expectation.AllowedActions) > 0 && !slices.Contains(expectation.AllowedActions, finding.CorrectionAction) {
					result.UnsafeCorrections = append(result.UnsafeCorrections, finding)
				}
				found = true
				break
			}
		}
		if !found {
			result.FalsePositives = append(result.FalsePositives, finding)
		}
	}
	for index, expectation := range expected {
		if matched[index] {
			continue
		}
		if result.Completed {
			result.Missed = append(result.Missed, expectation)
		} else {
			result.NotEvaluated = append(result.NotEvaluated, expectation)
		}
	}
}

func proseSHA256(data []byte) string {
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

func writeProseEvaluation(directory string, result proseEvaluationResult) error {
	data, err := json.Marshal(result)
	if err != nil {
		return errors.New("evaluation results could not be encoded")
	}
	return cloudflareops.WriteJSON(filepath.Join(directory, "result.json"), json.RawMessage(data))
}
