package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	e2eHeadSHA        = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	e2eBaseSHA        = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	e2eOwner          = "octocat"
	e2eRepo           = "example"
	e2eBotLogin       = "e2e-review-agent[bot]"
	e2eAppID          = int64(12345)
	e2eInstallationID = int64(99)
	e2ePullNumber     = 7
	e2eWebhookSecret  = "e2e-webhook-secret"
)

// The compiled service reviews one pull request against local GitHub and model
// peers. Usage exhaustion is configured to pass, and the disabled provider is
// never called.
func TestEndToEndUsageExhaustionPassesAndSkipsTheDisabledProvider(t *testing.T) {
	var primaryCalls atomic.Int32
	var disabledCalls atomic.Int32
	primary := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		primaryCalls.Add(1)
		writeUsageExhausted(writer)
	}))
	t.Cleanup(primary.Close)
	disabled := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		disabledCalls.Add(1)
		http.Error(writer, "disabled provider was called", http.StatusInternalServerError)
	}))
	t.Cleanup(disabled.Close)

	github := newGitHubPeer(t)
	process := startAgent(t, primary.URL, disabled.URL, true, github)
	postWebhook(t, process.baseURL, "delivery-usage")

	conclusion := github.waitConclusion(t, 20*time.Second)
	if conclusion != "success" {
		t.Fatalf("check conclusion = %q, want success\n%s", conclusion, process.output.String())
	}
	if primaryCalls.Load() == 0 || disabledCalls.Load() != 0 {
		t.Fatalf("provider calls = primary %d, disabled %d, want the enabled provider only\n%s",
			primaryCalls.Load(), disabledCalls.Load(), process.output.String())
	}
	if !strings.Contains(github.commentBody(), "no remaining usage") {
		t.Fatalf("comment = %q, want the usage notice\n%s", github.commentBody(), process.output.String())
	}
}

// A model that answers is reviewed through the real process, and the check
// passes because the review itself passed.
func TestEndToEndAnsweredReviewPublishesSuccess(t *testing.T) {
	model := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		content := `{"overview":"This change adds one line to the program.","omissions_acceptable":true,"decision_reason":"The added line is complete.","findings":[]}`
		if strings.Contains(string(body), "review_report") {
			content = `{"summary":"The pull request adds one line.","walkthrough":["The file gains one line."]}`
		}
		writeReviewStream(writer, content)
	}))
	t.Cleanup(model.Close)
	unused := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		http.Error(writer, "unused provider was called", http.StatusInternalServerError)
	}))
	t.Cleanup(unused.Close)

	github := newGitHubPeer(t)
	process := startAgent(t, model.URL, unused.URL, true, github)
	postWebhook(t, process.baseURL, "delivery-success")

	conclusion := github.waitConclusion(t, 20*time.Second)
	if conclusion != "success" {
		t.Fatalf("check conclusion = %q, want success\n%s", conclusion, process.output.String())
	}
	if !strings.Contains(github.commentBody(), "no severe defects") && !strings.Contains(github.commentBody(), "adds one line") {
		t.Fatalf("comment = %q, want the published review\n%s", github.commentBody(), process.output.String())
	}
}

type agentProcess struct {
	baseURL string
	output  *bytes.Buffer
	command *exec.Cmd
}

func startAgent(t *testing.T, primaryURL string, secondaryURL string, disableSecondary bool, github *githubPeer) *agentProcess {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "pr-review-agent")
	build := exec.Command("go", "build", "-o", binary, ".")
	build.Dir = "."
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build agent: %v\n%s", err, output)
	}

	port := freePort(t)
	configuration := map[string]any{
		"GITHUB_APP_ID":              fmt.Sprintf("%d", e2eAppID),
		"GITHUB_BOT_LOGIN":           e2eBotLogin,
		"GITHUB_API_BASE_URL":        github.server.URL,
		"GITHUB_GRAPHQL_URL":         github.server.URL + "/graphql",
		"PORT":                       port,
		"REVIEW_MIN_IMPORTANCE":      "8",
		"REVIEW_WORKERS":             "1",
		"REVIEW_CHUNK_TIMEOUT":       "30s",
		"SERVICE_FAILURE_APPEARANCE": map[string]string{"usage_exceeded": "pass"},
		"PROVIDER_PRIORITY":          []string{"primary", "secondary"},
		"PROVIDERS": []map[string]any{
			{"id": "primary", "base_url": strings.TrimRight(primaryURL, "/") + "/v1", "model": "primary-model", "api_key_binding": "PRIMARY_KEY"},
			{"id": "secondary", "base_url": strings.TrimRight(secondaryURL, "/") + "/v1", "model": "secondary-model", "api_key_binding": "SECONDARY_KEY", "disabled": disableSecondary},
		},
	}
	encoded, err := json.Marshal(configuration)
	if err != nil {
		t.Fatalf("encode runtime: %v", err)
	}
	runtimePath := filepath.Join(t.TempDir(), "runtime.json")
	if err := os.WriteFile(runtimePath, encoded, 0o600); err != nil {
		t.Fatalf("write runtime: %v", err)
	}

	output := &bytes.Buffer{}
	command := exec.Command(binary)
	command.Env = append(os.Environ(),
		"RUNTIME_CONFIG_PATH="+runtimePath,
		"GITHUB_PRIVATE_KEY="+generatePrivateKey(t),
		"GITHUB_WEBHOOK_SECRET="+e2eWebhookSecret,
		"PROVIDER_PRIMARY_API_KEY=e2e-primary-key",
		"PROVIDER_SECONDARY_API_KEY=e2e-secondary-key",
	)
	command.Stdout = output
	command.Stderr = output
	if err := command.Start(); err != nil {
		t.Fatalf("start agent: %v", err)
	}
	t.Cleanup(func() {
		_ = command.Process.Kill()
		_, _ = command.Process.Wait()
	})
	baseURL := "http://127.0.0.1:" + port
	waitForHealth(t, baseURL, output)
	return &agentProcess{baseURL: baseURL, output: output, command: command}
}

func postWebhook(t *testing.T, baseURL string, deliveryID string) {
	t.Helper()
	body := []byte(fmt.Sprintf(`{
		"action":"opened",
		"installation":{"id":%d},
		"repository":{"name":%q,"owner":{"login":%q}},
		"pull_request":{"number":%d,"draft":false,"title":"Add a line","body":"The program grows.","head":{"sha":%q},"base":{"sha":%q}}
	}`, e2eInstallationID, e2eRepo, e2eOwner, e2ePullNumber, e2eHeadSHA, e2eBaseSHA))
	mac := hmac.New(sha256.New, []byte(e2eWebhookSecret))
	_, _ = mac.Write(body)
	request, err := http.NewRequest(http.MethodPost, baseURL+"/api/v1/github_webhooks", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("create webhook: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	request.Header.Set("X-Github-Event", "pull_request")
	request.Header.Set("X-Github-Delivery", deliveryID)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("post webhook: %v", err)
	}
	defer response.Body.Close()
	responseBody, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("webhook status = %d, body = %s", response.StatusCode, responseBody)
	}
}

func waitForHealth(t *testing.T, baseURL string, output *bytes.Buffer) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		response, err := http.Get(baseURL + "/health")
		if err == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("agent did not become healthy\n%s", output.String())
}

type githubPeer struct {
	server *httptest.Server
	mu     sync.Mutex
	checks map[int64]map[string]any
	nextID int64
	body   string
	routes []string
}

func newGitHubPeer(t *testing.T) *githubPeer {
	t.Helper()
	peer := &githubPeer{checks: map[int64]map[string]any{}, nextID: 77}
	peer.server = httptest.NewServer(http.HandlerFunc(peer.serve))
	t.Cleanup(peer.server.Close)
	return peer
}

func (peer *githubPeer) serve(writer http.ResponseWriter, request *http.Request) {
	peer.mu.Lock()
	peer.routes = append(peer.routes, request.Method+" "+request.URL.Path)
	peer.mu.Unlock()

	switch {
	case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/access_tokens"):
		writeJSON(writer, map[string]any{
			"token": "ghs_e2e", "expires_at": "2099-01-01T00:00:00Z",
			"repository_selection": "all",
			"permissions":          map[string]string{"contents": "read", "pull_requests": "write"},
		})
	case request.Method == http.MethodPost && request.URL.Path == "/graphql":
		writeJSON(writer, map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": map[string]any{
			"reviewThreads": map[string]any{
				"pageInfo": map[string]any{"hasNextPage": false, "endCursor": nil},
				"nodes":    []any{},
			},
		}}}})
	case request.Method == http.MethodGet && strings.Contains(request.URL.Path, "/check-runs"):
		writeJSON(writer, map[string]any{"check_runs": peer.checkList()})
	case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/check-runs"):
		writeJSON(writer, peer.createCheck(request))
	case request.Method == http.MethodPatch && strings.Contains(request.URL.Path, "/check-runs/"):
		writeJSON(writer, peer.updateCheck(request))
	case request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/reviews"):
		writeJSON(writer, []any{})
	case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/reviews"):
		writeJSON(writer, map[string]any{
			"id": float64(9), "commit_id": e2eHeadSHA, "state": "APPROVED", "body": "Approved",
			"user": map[string]any{"login": e2eBotLogin},
		})
	case request.Method == http.MethodGet && strings.Contains(request.URL.Path, "/issues/") && strings.HasSuffix(request.URL.Path, "/comments"):
		writeJSON(writer, peer.comments())
	case request.Method == http.MethodPost && strings.Contains(request.URL.Path, "/issues/") && strings.HasSuffix(request.URL.Path, "/comments"):
		writeJSON(writer, peer.saveComment(request, 1))
	case request.Method == http.MethodPatch && strings.Contains(request.URL.Path, "/issues/comments/"):
		writeJSON(writer, peer.saveComment(request, 1))
	case request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/files"):
		patch := "@@ -1 +1,2 @@\n package main\n+added\n"
		writeJSON(writer, []map[string]any{{"filename": "main.go", "status": "modified", "patch": patch}})
	case request.Method == http.MethodGet && strings.Contains(request.URL.Path, "/contents/"):
		writeJSON(writer, map[string]any{
			"type": "file", "encoding": "base64",
			"content": base64.StdEncoding.EncodeToString([]byte("package main\nadded\n")),
			"sha":     e2eHeadSHA,
		})
	case request.Method == http.MethodGet && strings.Contains(request.URL.Path, "/pulls/"):
		writeJSON(writer, map[string]any{
			"number": e2ePullNumber, "draft": false, "title": "Add a line", "body": "The program grows.",
			"head": map[string]string{"sha": e2eHeadSHA},
			"base": map[string]string{"sha": e2eBaseSHA},
		})
	default:
		http.Error(writer, "unexpected "+request.Method+" "+request.URL.Path, http.StatusNotFound)
	}
}

func (peer *githubPeer) checkList() []map[string]any {
	peer.mu.Lock()
	defer peer.mu.Unlock()
	list := make([]map[string]any, 0, len(peer.checks))
	for _, check := range peer.checks {
		list = append(list, check)
	}
	return list
}

func (peer *githubPeer) createCheck(request *http.Request) map[string]any {
	var body struct {
		Name       string `json:"name"`
		HeadSHA    string `json:"head_sha"`
		ExternalID string `json:"external_id"`
	}
	_ = json.NewDecoder(request.Body).Decode(&body)
	peer.mu.Lock()
	defer peer.mu.Unlock()
	peer.nextID++
	check := map[string]any{
		"id": peer.nextID, "name": body.Name, "head_sha": body.HeadSHA,
		"status": "queued", "conclusion": "", "external_id": body.ExternalID,
		"app": map[string]any{"id": e2eAppID},
	}
	peer.checks[peer.nextID] = check
	return check
}

func (peer *githubPeer) updateCheck(request *http.Request) map[string]any {
	var body struct {
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
	}
	_ = json.NewDecoder(request.Body).Decode(&body)
	idText := request.URL.Path[strings.LastIndex(request.URL.Path, "/")+1:]
	var id int64
	_, _ = fmt.Sscan(idText, &id)
	peer.mu.Lock()
	defer peer.mu.Unlock()
	check := peer.checks[id]
	if check == nil {
		check = map[string]any{"id": id, "head_sha": e2eHeadSHA, "name": "PR-Agent Review", "app": map[string]any{"id": e2eAppID}}
		peer.checks[id] = check
	}
	if body.Status != "" {
		check["status"] = body.Status
	}
	if body.Conclusion != "" {
		check["conclusion"] = body.Conclusion
		check["status"] = "completed"
	}
	return check
}

func (peer *githubPeer) comments() []map[string]any {
	peer.mu.Lock()
	defer peer.mu.Unlock()
	if peer.body == "" {
		return []map[string]any{}
	}
	return []map[string]any{{
		"id": float64(1), "body": peer.body, "user": map[string]any{"login": e2eBotLogin},
	}}
}

func (peer *githubPeer) saveComment(request *http.Request, id int64) map[string]any {
	var body struct {
		Body string `json:"body"`
	}
	_ = json.NewDecoder(request.Body).Decode(&body)
	peer.mu.Lock()
	peer.body = body.Body
	peer.mu.Unlock()
	return map[string]any{"id": id, "body": body.Body, "user": map[string]any{"login": e2eBotLogin}}
}

func (peer *githubPeer) commentBody() string {
	peer.mu.Lock()
	defer peer.mu.Unlock()
	return peer.body
}

func (peer *githubPeer) waitConclusion(t *testing.T, budget time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		peer.mu.Lock()
		for _, check := range peer.checks {
			if conclusion, _ := check["conclusion"].(string); conclusion != "" {
				peer.mu.Unlock()
				return conclusion
			}
		}
		routes := append([]string(nil), peer.routes...)
		peer.mu.Unlock()
		_ = routes
		time.Sleep(50 * time.Millisecond)
	}
	peer.mu.Lock()
	defer peer.mu.Unlock()
	t.Fatalf("check did not complete, routes = %v", peer.routes)
	return ""
}

func freePort(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	return fmt.Sprintf("%d", port)
}

func generatePrivateKey(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
}

func writeJSON(writer http.ResponseWriter, payload any) {
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(payload)
}

func writeUsageExhausted(writer http.ResponseWriter) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(writer).Encode(map[string]any{
		"error": map[string]any{
			"message": "The usage limit has been reached",
			"type":    "invalid_request_error",
			"code":    "upstream_failed",
		},
	})
}

func writeReviewStream(writer http.ResponseWriter, content string) {
	writer.Header().Set("Content-Type", "text/event-stream")
	writer.WriteHeader(http.StatusOK)
	frames := []map[string]any{
		{"type": "response.output_text.delta", "delta": content},
		{"type": "response.completed", "response": map[string]any{
			"id": "resp-e2e", "object": "response", "model": "primary-model", "status": "completed",
			"output": []map[string]any{{
				"type": "message", "role": "assistant", "status": "completed",
				"content": []map[string]any{{"type": "output_text", "text": content, "annotations": []any{}}},
			}},
		}},
	}
	for _, frame := range frames {
		encoded, _ := json.Marshal(frame)
		_, _ = writer.Write([]byte("event: " + frame["type"].(string) + "\ndata: " + string(encoded) + "\n\n"))
	}
}
