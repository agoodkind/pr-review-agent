package diff

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"unicode/utf8"

	udiff "github.com/aymanbagabas/go-udiff"

	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
)

// ErrNonTextPatch prevents binary content from becoming a reconstructed text patch.
var ErrNonTextPatch = errors.New("file content is not UTF-8 text without NUL bytes")

type patchRecovery struct {
	source Source
	ref    domain.PullRequestRef
	pr     githubapp.PullRequest
	base   domain.HeadSHA
}

func newPatchRecovery(source Source, ref domain.PullRequestRef, pr githubapp.PullRequest, base domain.HeadSHA) *patchRecovery {
	return &patchRecovery{source: source, ref: ref, pr: pr, base: base}
}

func patchNeedsRecovery(changed githubapp.ChangedFile) bool {
	if !changed.PatchPresent {
		return true
	}
	parsed, err := parsePatch(changed.Patch)
	if err != nil || !parsed.complete {
		return true
	}
	if len(parsed.hunks) == 0 {
		return strings.TrimSpace(changed.Patch) != "" || !changed.ChangeCountsKnown || changed.Additions != 0 || changed.Deletions != 0
	}
	return patchChangeCountsError(changed, parsed) != nil
}

func (recovery *patchRecovery) recover(ctx context.Context, changed githubapp.ChangedFile, current string) (string, error) {
	if !isPatchText(current) {
		return "", ErrNonTextPatch
	}
	if recovery.pr.Head == "" {
		return "", errors.New("recover patch: pull request head commit is required")
	}
	if err := recovery.resolveBase(ctx); err != nil {
		return "", err
	}
	previous, err := recovery.previousContent(ctx, changed)
	if err != nil {
		return "", err
	}
	if !isPatchText(previous) {
		return "", ErrNonTextPatch
	}
	if changed.Status == "removed" {
		current = ""
	}
	// Constant labels prevent repository paths from adding diff header lines.
	patch, err := udiff.ToUnified("base", "head", previous, udiff.Lines(previous, current), udiff.DefaultContextLines)
	if err != nil {
		slog.ErrorContext(ctx, "reconstruct text patch", slog.String("path", changed.Path), slog.String("err", err.Error()))
		return "", fmt.Errorf("reconstruct text patch for %s: %w", changed.Path, err)
	}
	if err := validateRecoveredPatch(changed, patch, current); err != nil {
		slog.ErrorContext(ctx, "validate recovered patch", slog.String("path", changed.Path), slog.String("err", err.Error()))
		return "", fmt.Errorf("validate recovered patch for %s: %w", changed.Path, err)
	}
	return patch, nil
}

func (recovery *patchRecovery) resolveBase(ctx context.Context) error {
	if recovery.base != "" {
		return nil
	}
	if recovery.pr.Base == "" {
		return errors.New("recover patch: pull request base commit is required")
	}
	comparison, err := recovery.source.Compare(ctx, recovery.ref.InstallationID, recovery.ref.Repository, recovery.pr.Base, recovery.pr.Head)
	if err != nil {
		slog.ErrorContext(ctx, "resolve patch comparison base", slog.String("err", err.Error()))
		return fmt.Errorf("resolve patch comparison base: %w", err)
	}
	if comparison.MergeBase == "" {
		return errors.New("recover patch: GitHub did not return a comparison merge base")
	}
	recovery.base = comparison.MergeBase
	return nil
}

func (recovery *patchRecovery) previousContent(ctx context.Context, changed githubapp.ChangedFile) (string, error) {
	if changed.Status == "added" {
		return "", nil
	}
	path := changed.Path
	if changed.Status == "renamed" {
		if changed.PreviousPath == "" {
			return "", fmt.Errorf("recover patch for %s: renamed file requires its previous path", changed.Path)
		}
		path = changed.PreviousPath
	}
	content, err := recovery.readFile(ctx, path, recovery.base)
	if err != nil {
		slog.ErrorContext(ctx, "load patch base content", slog.String("path", path), slog.String("base", string(recovery.base)), slog.String("err", err.Error()))
		return "", fmt.Errorf("load patch base content for %s at %s: %w", path, recovery.base, err)
	}
	return string(content.Data), nil
}

func (recovery *patchRecovery) readFile(ctx context.Context, path string, commit domain.HeadSHA) (githubapp.FileContent, error) {
	if source, ok := recovery.source.(interface {
		GetFileContent(context.Context, int64, domain.Repository, string, domain.HeadSHA) (githubapp.FileContent, error)
	}); ok {
		content, err := source.GetFileContent(ctx, recovery.ref.InstallationID, recovery.ref.Repository, path, commit)
		if err != nil {
			slog.ErrorContext(ctx, "load typed file content", slog.String("path", path), slog.String("commit", string(commit)), slog.String("err", err.Error()))
			return githubapp.FileContent{}, fmt.Errorf("load typed file content for %s at %s: %w", path, commit, err)
		}
		return content, nil
	}
	content, err := recovery.source.GetFile(ctx, recovery.ref.InstallationID, recovery.ref.Repository, path, commit)
	if err != nil {
		slog.ErrorContext(ctx, "load file content bytes", slog.String("path", path), slog.String("commit", string(commit)), slog.String("err", err.Error()))
		return githubapp.FileContent{}, fmt.Errorf("load file content bytes for %s at %s: %w", path, commit, err)
	}
	return githubapp.FileContent{Data: content, Metadata: ""}, nil
}

func isPatchText(content string) bool {
	return utf8.ValidString(content) && !strings.ContainsRune(content, '\x00')
}

func validateRecoveredPatch(changed githubapp.ChangedFile, patch, current string) error {
	parsed, err := parsePatch(patch)
	if err != nil {
		return err
	}
	if err := patchChangeCountsError(changed, parsed); err != nil {
		return err
	}
	if patch == "" {
		return nil
	}
	return ValidatePatchContent(patch, current)
}

func patchChangeCountsError(changed githubapp.ChangedFile, parsed parsedPatch) error {
	if !changed.ChangeCountsKnown {
		return nil
	}
	additions := 0
	deletions := 0
	for _, hunk := range parsed.hunks {
		for _, row := range strings.Split(hunk.text, "\n")[1:] {
			switch {
			case strings.HasPrefix(row, "+"):
				additions++
			case strings.HasPrefix(row, "-"):
				deletions++
			}
		}
	}
	if additions != changed.Additions || deletions != changed.Deletions {
		return fmt.Errorf("patch has %d additions and %d deletions; GitHub reported %d additions and %d deletions", additions, deletions, changed.Additions, changed.Deletions)
	}
	return nil
}
