package review

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/go-enry/go-enry/v2"

	"goodkind.io/gklog"

	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
	"goodkind.io/pr-review-agent/internal/reviewrules"
)

// Inspector performs read-only evidence operations for one reviewed repository head.
type Inspector interface {
	Inspect(context.Context, InspectionRequest) (string, error)
}

// InspectionSource supplies immutable repository reads and indexed path discovery.
type InspectionSource interface {
	diff.Source
	GetFileContent(context.Context, int64, domain.Repository, string, domain.HeadSHA) (githubapp.FileContent, error)
	SearchRepository(context.Context, int64, domain.Repository, string) ([]string, error)
}

type repositoryInspector struct {
	source         InspectionSource
	collector      *diff.Collector
	ref            domain.PullRequestRef
	files          []diff.FileContext
	policy         reviewrules.Policy
	maximumBytes   int
	maximumResults int
	publicClient   *http.Client
}

var errInspectionExcluded = errors.New("content policy excludes this inspection target")

var errInspectionBytes = errors.New("inspection source exceeds the configured byte limit")

// NewInspector binds inspection to the original repository, head, and content policy.
func NewInspector(source InspectionSource, ref domain.PullRequestRef, files []diff.FileContext, policy reviewrules.Policy, maximumBytes, maximumResults int) (Inspector, error) {
	if source == nil || maximumBytes <= 0 || maximumResults <= 0 {
		return nil, errors.New("inspection requires a source and positive byte and result limits")
	}
	if _, err := domain.ParseHeadSHA(string(ref.Head)); err != nil {
		slog.Warn("Inspection head validation failed", "err", err)
		return nil, fmt.Errorf("inspection requires an immutable reviewed head: %w", err)
	}
	return &repositoryInspector{
		source: source, collector: diff.NewCollector(source), ref: ref,
		files: slices.Clone(files), policy: policy, maximumBytes: maximumBytes, maximumResults: maximumResults,
		publicClient: newPublicInspectionClient(),
	}, nil
}

func (inspector *repositoryInspector) Inspect(ctx context.Context, request InspectionRequest) (string, error) {
	if err := request.Validate(); err != nil {
		return "", err
	}
	switch request.Operation {
	case InspectionReadFile:
		return inspector.readFile(ctx, request)
	case InspectionSearchRepository:
		return inspector.search(ctx, request)
	case InspectionReadURL:
		return inspector.readURL(ctx, request)
	default:
		return "", errors.New("unsupported inspection operation")
	}
}

func inspectionPath(filePath string, allowRoot bool) error {
	if allowRoot && (filePath == "" || filePath == ".") {
		return nil
	}
	if filePath == "" || filePath == "." || path.IsAbs(filePath) || path.Clean(filePath) != filePath ||
		strings.HasPrefix(filePath, "../") || strings.ContainsAny(filePath, "\\\r\n\x00") {
		return errors.New("inspection requires an exact repository-relative path")
	}
	return nil
}

func (inspector *repositoryInspector) loadFile(ctx context.Context, filePath string) (diff.FileContext, error) {
	logger := gklog.L(ctx)
	if err := inspectionPath(filePath, false); err != nil {
		return diff.FileContext{}, err
	}
	for _, file := range inspector.files {
		if file.IsSubmodule() && strings.HasPrefix(filePath, file.Path+"/") {
			return diff.FileContext{}, errors.New("nested submodule source is outside the parent review scope")
		}
		if file.Path == filePath && file.Status != "removed" && file.CoverageComplete {
			if err := inspector.checkContent(file); err != nil {
				return diff.FileContext{}, err
			}
			return file, nil
		}
	}
	file, err := inspector.collector.CollectInspectionFile(ctx, inspector.ref, filePath)
	if err != nil {
		logger.WarnContext(ctx, "Load supporting repository source failed", "path", filePath, "err", err)
		return diff.FileContext{}, fmt.Errorf("collect pinned inspection source %s: %w", filePath, err)
	}
	if err := inspector.checkContent(file); err != nil {
		return diff.FileContext{}, err
	}
	return file, nil
}

func (inspector *repositoryInspector) checkContent(file diff.FileContext) error {
	if !utf8.ValidString(file.CurrentContent) || enry.IsBinary([]byte(file.CurrentContent)) {
		slog.Warn("Binary inspection content requires another inspection method", "path", file.Path, "err", errInspectionExcluded)
		return fmt.Errorf("%w: binary content requires another inspection method", errInspectionExcluded)
	}
	if kind, _, excluded := deterministicExclusion(file, inspector.policy); excluded && inspector.policy.AllowsContentExclusion(kind) {
		return errInspectionExcluded
	}
	if !inspector.policy.RequiresFileContentReview(file.Path) {
		for _, prefix := range inspector.policy.FileClassification().UncertainContentPrefixes {
			if strings.HasPrefix(strings.TrimSpace(file.CurrentContent), prefix) {
				slog.Warn("Opaque inspection content does not establish source behavior", "path", file.Path, "err", errInspectionExcluded)
				return fmt.Errorf("%w: opaque content does not establish source behavior", errInspectionExcluded)
			}
		}
	}
	return nil
}

func (inspector *repositoryInspector) readFile(ctx context.Context, request InspectionRequest) (string, error) {
	file, err := inspector.loadFile(ctx, request.Target)
	if err != nil {
		return "", err
	}
	identity := "Repository file: " + file.Path + "\nHead: " + string(inspector.ref.Head)
	start, end, err := inspectionQueryRange(file.CurrentContent, request)
	if err != nil {
		return "", err
	}
	return inspectionExcerpt(identity, file.CurrentContent, start, end, inspector.maximumBytes, false)
}

func (inspector *repositoryInspector) search(ctx context.Context, request InspectionRequest) (string, error) {
	logger := gklog.L(ctx)
	if err := inspectionPath(request.Target, true); err != nil {
		return "", err
	}
	if strings.TrimSpace(request.Query) == "" || strings.ContainsAny(request.Query, "\r\n\x00") {
		return "", errors.New("repository search requires a nonempty literal query")
	}
	paths, err := inspector.source.SearchRepository(ctx, inspector.ref.InstallationID, inspector.ref.Repository, request.Query)
	if err != nil {
		logger.WarnContext(ctx, "Discover repository inspection candidates failed", "err", err)
		return "", fmt.Errorf("discover repository inspection candidates: %w", err)
	}
	candidates := inspectionCandidatePaths(inspector.files, paths, request, inspector.maximumResults)
	var output strings.Builder
	output.WriteString("Repository search uses indexed discovery and original changed files. Results do not establish exhaustive absence.\n")
	fmt.Fprintf(&output, "Literal query: %s\nVerified head: %s\n", request.Query, inspector.ref.Head)
	if output.Len() > inspector.maximumBytes {
		return "", errors.New("inspection search metadata exceeds the configured byte limit")
	}
	matched := 0
	for _, candidate := range candidates {
		excerpt, err := inspector.searchMatch(ctx, candidate, request.Query, inspector.maximumBytes-output.Len()-1)
		if err != nil {
			if matched > 0 && errors.Is(err, errInspectionBytes) {
				return output.String(), nil
			}
			return "", err
		}
		if excerpt == "" {
			continue
		}
		output.WriteString(excerpt + "\n")
		matched++
	}
	if matched == 0 {
		output.WriteString("The bounded candidate checks did not establish a match. Unindexed or unchecked paths may still match.\n")
	}
	if output.Len() > inspector.maximumBytes {
		return "", errors.New("inspection search metadata exceeds the configured byte limit")
	}
	return output.String(), nil
}

func inspectionCandidatePaths(files []diff.FileContext, discovered []string, request InspectionRequest, maximum int) []string {
	var candidates []string
	for _, file := range files {
		if file.Status != "removed" && (strings.Contains(file.CurrentContent, request.Query) || strings.Contains(file.Patch, request.Query)) && inspectionWithinPath(request.Target, file.Path) {
			candidates = append(candidates, file.Path)
		}
	}
	for _, filePath := range discovered {
		if !slices.Contains(candidates, filePath) && inspectionWithinPath(request.Target, filePath) {
			candidates = append(candidates, filePath)
		}
	}
	return candidates[:min(len(candidates), maximum)]
}

func inspectionWithinPath(prefix, filePath string) bool {
	return prefix == "" || prefix == "." || filePath == prefix || strings.HasPrefix(filePath, prefix+"/")
}

func (inspector *repositoryInspector) searchMatch(ctx context.Context, filePath, query string, maximumBytes int) (string, error) {
	logger := gklog.L(ctx)
	file, err := inspector.loadFile(ctx, filePath)
	if err != nil {
		var apiError githubapp.APIError
		if errors.Is(err, errInspectionExcluded) || (errors.As(err, &apiError) && (apiError.StatusCode == http.StatusNotFound || apiError.StatusCode == http.StatusGone)) {
			return "", nil
		}
		logger.WarnContext(ctx, "Verify repository inspection candidate failed", "path", filePath, "err", err)
		return "", fmt.Errorf("verify repository search candidate %s: %w", filePath, err)
	}
	for index, line := range strings.Split(file.CurrentContent, "\n") {
		if strings.Contains(line, query) {
			identity := "Verified repository match: " + filePath + "\nHead: " + string(inspector.ref.Head)
			return inspectionExcerpt(identity, file.CurrentContent, index+1, index+1, maximumBytes, false)
		}
	}
	return "", nil
}

func inspectionExcerpt(identity, content string, startLine, endLine, maximumBytes int, bodyTruncated bool) (string, error) {
	lines, startLine, endLine, err := inspectionLineRange(content, startLine, endLine)
	if err != nil {
		return "", err
	}
	provenance := fmt.Sprintf("%s\nSHA-256 of inspected text: %x\nInspected text bytes: %d\n", identity, sha256.Sum256([]byte(content)), len(content))
	var excerpt strings.Builder
	returnedEnd := max(0, startLine-1)
	for index := startLine; index > 0 && index <= endLine; index++ {
		line := fmt.Sprintf("%d: %s\n", index, lines[index-1])
		metadata := inspectionMetadata(provenance, startLine, index, len(lines), bodyTruncated || index < endLine)
		if len(metadata)+excerpt.Len()+len(line) > maximumBytes {
			break
		}
		excerpt.WriteString(line)
		returnedEnd = index
	}
	if returnedEnd < startLine && endLine >= startLine {
		return "", errInspectionBytes
	}
	metadata := inspectionMetadata(provenance, startLine, returnedEnd, len(lines), bodyTruncated || returnedEnd < endLine)
	if len(metadata)+excerpt.Len() > maximumBytes {
		return "", errInspectionBytes
	}
	return metadata + excerpt.String(), nil
}

func inspectionLineRange(content string, startLine, endLine int) ([]string, int, int, error) {
	lines := strings.Split(strings.TrimSuffix(content, "\n"), "\n")
	if content == "" {
		lines = nil
	}
	if startLine == 0 {
		startLine = 1
	}
	if endLine == 0 || endLine > len(lines) {
		endLine = len(lines)
	}
	if len(lines) == 0 && startLine == 1 && endLine == 0 {
		return lines, 0, 0, nil
	}
	if startLine < 1 || startLine > len(lines) || endLine < startLine {
		return nil, 0, 0, errors.New("the requested source range does not exist in the retrieved content")
	}
	return lines, startLine, endLine, nil
}

func inspectionMetadata(provenance string, start, end, total int, truncated bool) string {
	return fmt.Sprintf("%sLines: %d-%d of %d\nTruncated: %t\nContent:\n", provenance, start, end, total, truncated)
}

func inspectionQueryRange(content string, request InspectionRequest) (int, int, error) {
	if request.Query == "" || request.StartLine != 0 || request.EndLine != 0 {
		return request.StartLine, request.EndLine, nil
	}
	const contextLines = 8
	lines := strings.Split(content, "\n")
	for index, line := range lines {
		if strings.Contains(line, request.Query) {
			return max(1, index+1-contextLines), min(len(lines), index+1+contextLines), nil
		}
	}
	return 0, 0, errors.New("the requested literal query did not match the retrieved source")
}

func (inspector *repositoryInspector) readURL(ctx context.Context, request InspectionRequest) (string, error) {
	address, err := url.Parse(request.Target)
	if err != nil {
		return "", errors.New("inspection requires a valid public HTTPS URL")
	}
	if err := validatePublicInspectionURL(address); err != nil {
		return "", err
	}
	return inspector.readPublicURL(ctx, address, request)
}
