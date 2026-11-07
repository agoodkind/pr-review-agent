package githubapp_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"goodkind.io/pr-review-agent/internal/config"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
)

func TestSearchRepositoryUsesScopedLiteralDiscovery(t *testing.T) {
	query := `bridge " OR repo:foreign/repository`
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/app/installations/77/access_tokens" {
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(writer).Encode(map[string]string{"token": "inspection-test-token", "expires_at": time.Now().Add(time.Hour).Format(time.RFC3339)})
			return
		}
		if request.Method != http.MethodGet || request.URL.Path != "/search/code" ||
			request.URL.Query().Get("q") != strconv.Quote(query)+" repo:owner/repository in:file" ||
			request.URL.Query().Get("per_page") != "30" || request.Header.Get("Authorization") != "Bearer inspection-test-token" {
			t.Error("Repository discovery changed its literal scope or authentication.")
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"incomplete_results":true,"items":[{"path":"source.go","repository":{"full_name":"owner/repository"}},{"path":"source.go","repository":{"full_name":"owner/repository"}}]}`)
	}))
	defer server.Close()
	address, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	var cfg config.Config
	cfg.GitHubAPIBaseURL = address
	cfg.GitHubAppID = 12345
	examplePrivateKey := testPrivateKey(t)
	cfg.GitHubPrivateKey = examplePrivateKey
	client := githubapp.NewClient(cfg, server.Client(), time.Now, slog.New(slog.NewTextHandler(io.Discard, nil)))
	paths, err := client.SearchRepository(context.Background(), 77, domain.Repository{Owner: "owner", Name: "repository"}, query)
	if err != nil || len(paths) != 1 || paths[0] != "source.go" {
		encoded, _ := json.Marshal(paths)
		t.Fatalf("Discovery did not return only distinct candidate paths: %s, %v", encoded, err)
	}
}
