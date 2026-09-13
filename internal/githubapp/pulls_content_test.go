package githubapp_test

import (
	"context"
	"testing"
	"time"

	"goodkind.io/pr-review-agent/internal/domain"
)

func TestGetFileReturnsSubmoduleReference(t *testing.T) {
	client, server, state := newStatefulTestClient(t, testPrivateKey(t), time.Unix(1_700_000_000, 0))
	defer server.Close()

	state.fileContent = map[string]any{
		"encoding":          nil,
		"name":              "zinit",
		"path":              "lib/zinit",
		"sha":               "aa243da8c5ba1e1781f35dd98e8515bd82dd82c4",
		"size":              0,
		"submodule_git_url": "https://github.com/zdharma-continuum/zinit.git",
		"type":              "submodule",
	}

	content, err := client.GetFile(context.Background(), 99, testRepo(), "lib/zinit", domain.HeadSHA(testHeadSHA))
	if err != nil {
		t.Fatalf("GetFile: %v", err)
	}
	want := "Submodule repository: https://github.com/zdharma-continuum/zinit.git\nSubproject commit aa243da8c5ba1e1781f35dd98e8515bd82dd82c4\n"
	if string(content) != want {
		t.Fatalf("content = %q, want %q", content, want)
	}
}

func TestGetFileRejectsIncompleteSubmoduleAndUnsupportedEncoding(t *testing.T) {
	cases := []struct {
		name     string
		response map[string]any
	}{
		{
			name: "missing repository",
			response: map[string]any{
				"type": "submodule",
				"sha":  testHeadSHA,
			},
		},
		{
			name: "invalid commit",
			response: map[string]any{
				"type":              "submodule",
				"sha":               "not-a-commit",
				"submodule_git_url": "https://github.com/zdharma-continuum/zinit.git",
			},
		},
		{
			name: "unsupported encoding",
			response: map[string]any{
				"type":     "file",
				"encoding": "none",
				"content":  "",
			},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			client, server, state := newStatefulTestClient(t, testPrivateKey(t), time.Unix(1_700_000_000, 0))
			defer server.Close()
			state.fileContent = testCase.response

			content, err := client.GetFile(context.Background(), 99, testRepo(), "lib/zinit", domain.HeadSHA(testHeadSHA))
			if err == nil || content != nil {
				t.Fatalf("GetFile = (%q, %v), want no content and an error", content, err)
			}
		})
	}
}
