package githubapp_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"goodkind.io/pr-review-agent/internal/domain"
)

// TestGitFailedPackStorageDiscardsEntry verifies rollback for a valid pack
// without its wanted commit and a real reference-write failure after storage.
func TestGitFailedPackStorageDiscardsEntry(t *testing.T) {
	for _, omitted := range []bool{true, false} {
		name := "reference write failure"
		if omitted {
			name = "wanted commit omitted"
		}

		t.Run(name, func(t *testing.T) {
			temporary := t.TempDir()
			t.Setenv("TMPDIR", temporary)
			fixture := newGitFixture(t)

			ctx, closeSnapshot, err := fixture.client.OpenRepositorySnapshot(t.Context(), 77, fixture.repo, fixture.root)
			if err != nil {
				t.Fatal(err)
			}

			t.Cleanup(func() { _ = closeSnapshot() })

			original, err := fixture.client.GetFileContent(ctx, 77, fixture.repo, gitFixtureSourcePath, fixture.root)
			if err != nil {
				t.Fatal(err)
			}

			cacheDirectory := gitFixtureCacheDirectory(t, temporary)
			if omitted {
				fixture.responsePack = gitFixtureInput(t, fixture.directory, string(fixture.base)+"\n",
					"pack-objects", "--stdout", "--revs", "--window=0",
				)
			} else {
				entries, readError := os.ReadDir(cacheDirectory)
				if readError != nil || len(entries) != 1 {
					t.Fatalf("primed repository entries=%v: %v", entries, readError)
				}

				refPath := filepath.Join(cacheDirectory, entries[0].Name(), "refs", "pragent", string(fixture.head))

				mkdirError := os.Mkdir(refPath, 0o700)
				if mkdirError != nil {
					t.Fatal(mkdirError)
				}
			}

			_, err = fixture.client.GetFileContent(t.Context(), 77, fixture.repo, gitFixtureSourcePath, fixture.head)
			if err == nil {
				t.Fatal("The invalid pack-storage operation did not fail")
			}

			if omitted && !strings.Contains(err.Error(), "exact requested commit") {
				t.Fatalf("The valid pack did not fail at wanted-commit verification: %v", err)
			}

			assertEmptyGitCache(t, cacheDirectory)

			retained, readError := fixture.client.GetFileContent(ctx, 77, fixture.repo, gitFixtureSourcePath, fixture.root)
			if readError != nil || string(retained.Data) != string(original.Data) {
				t.Fatalf("The failed cache update changed the established review snapshot: %v", readError)
			}

			fixture.mutex.Lock()
			before := fixture.transfers
			fixture.responsePack = nil
			fixture.mutex.Unlock()

			wanted := fixture.head
			if omitted {
				wanted = fixture.base
			}

			_, readError = fixture.client.GetFileContent(t.Context(), 77, fixture.repo, gitFixtureSourcePath, wanted)
			if readError != nil {
				t.Fatal(readError)
			}

			fixture.mutex.Lock()
			defer fixture.mutex.Unlock()

			if fixture.transfers != before+1 || fixture.contentRequests != 0 {
				t.Fatalf("fresh transfers=%d, expected=%d, content API requests=%d",
					fixture.transfers, before+1, fixture.contentRequests,
				)
			}
		})
	}
}

// TestGitRejectsExpandedPackHeaderBeforeInflation verifies that a tiny pack
// cannot require a multi-gigabyte allocation through its declared object size.
func TestGitRejectsExpandedPackHeaderBeforeInflation(t *testing.T) {
	fixture := newGitFixture(t)
	fixture.responsePack = []byte{
		'P', 'A', 'C', 'K', 0, 0, 0, 2, 0, 0, 0, 1,
		0xb1, 0x80, 0x80, 0x80, 0x40,
	}

	_, err := fixture.client.GetFile(t.Context(), 77, fixture.repo, gitFixtureSourcePath, fixture.head)
	if err == nil || !strings.Contains(err.Error(), "extracted byte limit") {
		t.Fatalf("oversized declared object did not stop before zlib inflation: %v", err)
	}

	fixture.mutex.Lock()
	defer fixture.mutex.Unlock()

	if fixture.contentRequests != 0 {
		t.Fatalf("oversized pack content API requests=%d", fixture.contentRequests)
	}
}

func repeatedGitTree(t *testing.T, fixture *gitFixture, levels int, content string) domain.HeadSHA {
	t.Helper()

	writeGitFixtureFile(t, fixture.directory, "tree-leaf.txt", content)
	blob := gitCommand(t, fixture.directory, "hash-object", "-w", "tree-leaf.txt")

	tree := strings.TrimSpace(string(gitFixtureInput(t, fixture.directory,
		"100644 blob "+blob+"\tleaf.txt\n", "mktree",
	)))
	for range levels {
		tree = strings.TrimSpace(string(gitFixtureInput(t, fixture.directory,
			"040000 tree "+tree+"\ta\n040000 tree "+tree+"\tb\n", "mktree",
		)))
	}

	commit := gitCommand(t, fixture.directory, "commit-tree", tree,
		"-p", string(fixture.base), "-m", "Create a repeated-child-tree fixture",
	)
	gitCommand(t, fixture.directory, "update-ref", "refs/heads/repeated-tree", commit)

	return domain.HeadSHA(commit)
}

// TestGitBoundsLogicalTreeBeforeDiff verifies that repeated child hashes count
// separately at every logical path before recursive diff expansion.
func TestGitBoundsLogicalTreeBeforeDiff(t *testing.T) {
	testCases := []struct {
		name    string
		levels  int
		content string
		error   string
	}{
		{name: "logical entries", levels: 18, content: "small\n", error: "entry limit"},
		{name: "logical bytes", levels: 12, content: strings.Repeat("x", 1024*1024), error: "extracted byte limit"},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newGitFixture(t)
			head := repeatedGitTree(t, fixture, testCase.levels, testCase.content)

			_, err := fixture.client.Compare(t.Context(), 77, fixture.repo, fixture.base, head)
			if err == nil || !strings.Contains(err.Error(), testCase.error) {
				t.Fatalf("repeated tree did not enforce %s: %v", testCase.error, err)
			}
		})
	}
}
