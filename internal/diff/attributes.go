package diff

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"path"
	"strings"

	"github.com/go-git/go-git/v5/plumbing/format/gitattributes"

	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
)

type attributesLocation struct {
	revision  domain.HeadSHA
	directory string
}

type repositoryAttributes struct {
	source   Source
	ref      domain.PullRequestRef
	stacks   map[attributesLocation][]gitattributes.MatchAttribute
	contents map[attributesLocation][]byte
}

func newRepositoryAttributes(source Source, ref domain.PullRequestRef) *repositoryAttributes {
	return &repositoryAttributes{
		source:   source,
		ref:      ref,
		stacks:   make(map[attributesLocation][]gitattributes.MatchAttribute),
		contents: make(map[attributesLocation][]byte),
	}
}

func (attributes *repositoryAttributes) remember(file FileContext, revision domain.HeadSHA) {
	if file.Status == "removed" || !file.CoverageComplete || path.Base(file.Path) != ".gitattributes" {
		return
	}
	location := attributesLocation{revision: revision, directory: path.Dir(file.Path)}
	attributes.contents[location] = []byte(file.CurrentContent)
}

func (attributes *repositoryAttributes) forChange(ctx context.Context, changed githubapp.ChangedFile, recovery *patchRecovery) (map[string]string, error) {
	revision := recovery.pr.Head
	filePath := changed.Path
	if changed.Status == "removed" {
		if err := recovery.resolveBase(ctx); err != nil {
			return nil, fmt.Errorf("resolve attributes base for %s: %w", changed.Path, err)
		}
		revision = recovery.base
		if changed.PreviousPath != "" {
			filePath = changed.PreviousPath
		}
	}
	if revision == "" {
		return nil, fmt.Errorf("load attributes for %s: immutable file revision is required", filePath)
	}
	stack, err := attributes.stack(ctx, path.Dir(filePath), revision)
	if err != nil {
		return nil, err
	}
	return effectiveAttributes(stack, strings.Split(filePath, "/")), nil
}

func (attributes *repositoryAttributes) stack(ctx context.Context, directory string, revision domain.HeadSHA) ([]gitattributes.MatchAttribute, error) {
	location := attributesLocation{revision: revision, directory: directory}
	if stack, exists := attributes.stacks[location]; exists {
		return stack, nil
	}
	var stack []gitattributes.MatchAttribute
	var domainPath []string
	if directory != "." {
		parent, err := attributes.stack(ctx, path.Dir(directory), revision)
		if err != nil {
			return nil, err
		}
		stack = append(stack, parent...)
		domainPath = strings.Split(directory, "/")
	}
	attributePath := path.Join(directory, ".gitattributes")
	content, exists := attributes.contents[location]
	if !exists {
		var err error
		content, err = attributes.source.GetFile(ctx, attributes.ref.InstallationID, attributes.ref.Repository, attributePath, revision)
		if err != nil {
			var apiError githubapp.APIError
			if errors.As(err, &apiError) && apiError.StatusCode == http.StatusNotFound {
				attributes.stacks[location] = stack
				return stack, nil
			}
			return nil, fmt.Errorf("load attributes %s at %s: %w", attributePath, revision, err)
		}
	}
	patterns, err := gitattributes.ReadAttributes(bytes.NewReader(content), domainPath, directory == ".")
	if err != nil {
		return nil, fmt.Errorf("parse attributes %s at %s: %w", attributePath, revision, err)
	}
	stack = append(stack, patterns...)
	attributes.stacks[location] = stack
	return stack, nil
}

func effectiveAttributes(stack []gitattributes.MatchAttribute, filePath []string) map[string]string {
	macros := make([]gitattributes.MatchAttribute, 0)
	for _, pattern := range stack {
		if pattern.Pattern == nil {
			macros = append(macros, pattern)
		}
	}
	result := make(map[string]string)
	for _, pattern := range stack {
		if pattern.Pattern == nil {
			continue
		}
		// Merge each rule in increasing priority because the library matcher overwrites earlier results.
		matcherStack := append(append([]gitattributes.MatchAttribute(nil), macros...), pattern)
		matched, _ := gitattributes.NewMatcher(matcherStack).Match(filePath, nil)
		for name, attribute := range matched {
			switch {
			case attribute.IsSet():
				result[name] = "true"
			case attribute.IsUnset():
				result[name] = "false"
			case attribute.IsUnspecified():
				result[name] = "unspecified"
			default:
				result[name] = attribute.Value()
			}
		}
	}
	return result
}
