package githubapp_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func gitFixtureInput(t *testing.T, directory, input string, arguments ...string) []byte {
	t.Helper()

	command := exec.CommandContext(t.Context(), "git", arguments...)
	command.Dir = directory
	command.Stdin = strings.NewReader(input)

	command.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")

	output, err := command.Output()
	if err != nil {
		t.Fatalf("native Git fixture command %v failed: %v", arguments, err)
	}

	return output
}

func gitFixtureCacheDirectory(t *testing.T, temporary string) string {
	t.Helper()

	directories, err := filepath.Glob(filepath.Join(temporary, "pragent-git-cache-*"))
	if err != nil || len(directories) != 1 {
		t.Fatalf("private Git cache directories=%v: %v", directories, err)
	}

	return directories[0]
}

func assertEmptyGitCache(t *testing.T, directory string) {
	t.Helper()

	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}

	if len(entries) != 0 {
		t.Fatalf("The failed Git operation retained cache artifacts: %v", entries)
	}
}
