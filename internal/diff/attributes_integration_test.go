package diff_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/domain"
)

func TestCollectorCollectsEffectiveGitAttributesAtPinnedRevisions(t *testing.T) {
	for _, incremental := range []bool{false, true} {
		name := "pull_request"
		if incremental {
			name = "incremental"
		}
		t.Run(name, func(t *testing.T) {
			source := newAttributesRepository(t)
			pullRequest := recoveryPullRequest(source)
			collector := diff.NewCollector(source)
			var input diff.ReviewInput
			var err error
			if incremental {
				input, err = collector.CollectRange(t.Context(), recoveryRef(source.head), pullRequest, source.base)
			} else {
				input, err = collector.Collect(t.Context(), recoveryRef(source.head), pullRequest)
			}
			if err != nil {
				t.Fatal(err)
			}
			expected := map[string]map[string]string{
				"pkg/changed.txt":            {"bundle": "true", "linguist-generated": "false", "linguist-vendored": "true", "root-state": "head", "nested-state": "head"},
				"pkg/named space.txt":        {"bundle": "true", "linguist-generated": "false", "linguist-vendored": "true", "root-state": "head", "nested-state": "head"},
				"pkg/unspecified.txt":        {"bundle": "true", "linguist-generated": "unspecified", "linguist-vendored": "true", "root-state": "head", "nested-state": "head"},
				"pkg/deep/changed.txt":       {"bundle": "true", "linguist-generated": "true", "linguist-vendored": "true", "root-state": "head", "nested-state": "head"},
				"pkg/missing/changed.txt":    {"bundle": "true", "linguist-generated": "false", "linguist-vendored": "true", "root-state": "head", "nested-state": "head"},
				"pkg/moved.txt":              {"bundle": "true", "linguist-generated": "false", "linguist-vendored": "true", "root-state": "head", "nested-state": "head"},
				"pkg/deleted.txt":            {"linguist-generated": "false", "root-state": "base", "nested-state": "base"},
				"pkg/removed/.gitattributes": {},
				"pkg/removed/deleted.txt":    {"linguist-generated": "true", "root-state": "base", "nested-state": "base", "removed-directory": "true"},
			}
			for _, file := range input.Files {
				attributes, exists := expected[file.Path]
				if !exists {
					continue
				}
				if !reflect.DeepEqual(file.Attributes, attributes) {
					t.Errorf("Attributes for %s = %v; expected %v", file.Path, file.Attributes, attributes)
				}
				delete(expected, file.Path)
			}
			if len(expected) != 0 {
				t.Fatalf("Collection omitted expected paths: %v", expected)
			}
		})
	}
}

func newAttributesRepository(t *testing.T) *repositorySource {
	t.Helper()
	directory := t.TempDir()
	largeGitCommand(t, directory, "init", "--quiet")
	writeAttributesFile(t, directory, ".gitattributes", "*.txt linguist-generated root-state=base\n")
	writeAttributesFile(t, directory, "pkg/.gitattributes", "*.txt linguist-generated=false nested-state=base\n")
	writeAttributesFile(t, directory, "pkg/removed/.gitattributes", "*.txt linguist-generated removed-directory\n")
	paths := []string{"pkg/changed.txt", "pkg/named space.txt", "pkg/unspecified.txt", "pkg/deep/changed.txt", "pkg/missing/changed.txt", "pkg/deleted.txt", "pkg/removed/deleted.txt"}
	for _, path := range paths {
		writeAttributesFile(t, directory, path, "old source\n")
	}
	renameContent := strings.Repeat("unchanged rename source\n", 20)
	writeAttributesFile(t, directory, "old/moved.txt", renameContent)
	largeGitCommand(t, directory, "add", "--all")
	largeGitCommit(t, directory, "Create attribute evidence")
	base := domain.HeadSHA(strings.TrimSpace(largeGitCommand(t, directory, "rev-parse", "HEAD")))
	writeAttributesFile(t, directory, ".gitattributes", "[attr]bundle linguist-generated linguist-vendored\n*.txt bundle root-state=head\n")
	writeAttributesFile(t, directory, "pkg/.gitattributes", "*.txt -linguist-generated nested-state=head\nunspecified.txt !linguist-generated\n\"named space.txt\" linguist-generated=false\n")
	writeAttributesFile(t, directory, "pkg/deep/.gitattributes", "*.txt linguist-generated=true\n")
	for _, path := range paths[:5] {
		writeAttributesFile(t, directory, path, "new source\n")
	}
	for _, path := range []string{"pkg/deleted.txt", "pkg/removed/deleted.txt", "pkg/removed/.gitattributes"} {
		if err := os.Remove(filepath.Join(directory, path)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Rename(filepath.Join(directory, "old/moved.txt"), filepath.Join(directory, "pkg/moved.txt")); err != nil {
		t.Fatal(err)
	}
	largeGitCommand(t, directory, "add", "--all")
	largeGitCommit(t, directory, "Change source and repository attributes")
	head := domain.HeadSHA(strings.TrimSpace(largeGitCommand(t, directory, "rev-parse", "HEAD")))
	writeAttributesFile(t, directory, ".gitattributes", "*.txt root-state=uncommitted\n")
	return &repositorySource{directory: directory, base: base, head: head, patchMode: "complete"}
}

func TestCollectorRejectsUnreadableRepositoryAttributes(t *testing.T) {
	for _, failure := range []string{"missing_git_object", "invalid_attribute"} {
		t.Run(failure, func(t *testing.T) {
			directory := t.TempDir()
			largeGitCommand(t, directory, "init", "--quiet")
			attributes := "*.txt linguist-generated\n"
			if failure == "invalid_attribute" {
				attributes = "*.txt invalid@attribute\n"
			}
			writeAttributesFile(t, directory, ".gitattributes", attributes)
			writeAttributesFile(t, directory, "changed.txt", "before\n")
			largeGitCommand(t, directory, "add", "--all")
			largeGitCommit(t, directory, "Create source and attributes")
			base := domain.HeadSHA(strings.TrimSpace(largeGitCommand(t, directory, "rev-parse", "HEAD")))
			writeAttributesFile(t, directory, "changed.txt", "after\n")
			largeGitCommand(t, directory, "add", "--all")
			largeGitCommit(t, directory, "Change source")
			head := domain.HeadSHA(strings.TrimSpace(largeGitCommand(t, directory, "rev-parse", "HEAD")))
			if failure == "missing_git_object" {
				objectID := strings.TrimSpace(largeGitCommand(t, directory, "rev-parse", string(head)+":.gitattributes"))
				objectPath := filepath.Join(directory, ".git", "objects", objectID[:2], objectID[2:])
				if err := os.Remove(objectPath); err != nil {
					t.Fatal(err)
				}
			}
			source := &repositorySource{directory: directory, base: base, head: head, patchMode: "complete"}
			input, err := diff.NewCollector(source).Collect(t.Context(), recoveryRef(head), recoveryPullRequest(source))
			if err == nil || !strings.Contains(err.Error(), "attributes .gitattributes at "+string(head)) {
				t.Fatalf("Collection error = %v; expected the pinned attribute failure", err)
			}
			if len(input.Files) != 0 {
				t.Fatal("Collection returned partial file evidence after an attribute failure.")
			}
		})
	}
}

func writeAttributesFile(t *testing.T, directory, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(directory, path)), 0o700); err != nil {
		t.Fatal(err)
	}
	writeRecoveryFile(t, directory, path, content)
}
