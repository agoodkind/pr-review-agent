package main_test

import (
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"goodkind.io/pr-review-agent/internal/review"
)

func TestDiagnosticCommandsRejectReviewsWithPendingInspection(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	binary := filepath.Join(directory, "pragent-ops")
	build := exec.CommandContext(t.Context(), "go", "build", "-o", binary, "./cmd/pragent-ops")
	build.Dir = root
	if output, buildErr := build.CombinedOutput(); buildErr != nil {
		t.Fatalf("Build pragent-ops: %v\n%s", buildErr, output)
	}
	server := httptest.NewTLSServer(http.HandlerFunc(writePendingInspectionResponse))
	defer server.Close()
	certificatePath := filepath.Join(directory, "server.pem")
	certificate := pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: server.Certificate().Raw,
	})
	if err := os.WriteFile(certificatePath, certificate, 0o600); err != nil {
		t.Fatal(err)
	}
	runtimePath := diagnosticInspectionRuntime(t, root, directory, server.URL)
	credentialPath := filepath.Join(directory, "synthetic-credential")
	if err := os.WriteFile(
		credentialPath,
		[]byte("local-inspection-test-credential"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	datasetPath := filepath.Join(directory, "dataset.json")
	dataset := `{"cases":[{"id":"pending-inspection","provenance":{"description":"A local command regression requires an unchanged helper before completing review."},"files":[{"path":"main.go","status":"modified","patch":"@@ -1,2 +1,2 @@\n package main\n-const Value = 1\n+const Value = 2\n","current_content":"package main\nconst Value = 2\n"}],"expected":[]}]}`
	if err := os.WriteFile(datasetPath, []byte(dataset), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, commandName := range []string{"probe", "prose-eval"} {
		t.Run(
			commandName,
			func(t *testing.T) {
				outputDirectory := filepath.Join(directory, commandName)
				arguments := []string{
					commandName,
					"--runtime",
					runtimePath,
					"--provider",
					"openai_goodkind_io",
					"--credential-file",
					credentialPath,
					"--output-dir",
					outputDirectory,
				}
				if commandName == "prose-eval" {
					arguments = append(arguments, "--dataset", datasetPath)
				}
				command := exec.CommandContext(t.Context(), binary, arguments...)
				command.Dir = root
				command.Env = append(
					os.Environ(),
					"SSL_CERT_FILE="+certificatePath,
					"SSL_CERT_DIR=",
					"GODEBUG=x509sslcertoverrideplatform=1",
				)
				output, commandErr := command.CombinedOutput()
				if commandErr == nil {
					t.Fatalf(
						"%s accepted a review that still requires inspection:\n%s",
						commandName,
						output,
					)
				}
				content, readErr := os.ReadFile(filepath.Join(outputDirectory, "result.json"))
				if readErr != nil {
					t.Fatalf(
						"%s did not save its diagnostic result: %v\n%s",
						commandName,
						readErr,
						output,
					)
				}
				if commandName == "probe" {
					var result struct {
						Success    bool
						Completion review.Completion
						Usage      review.UsageSummary
					}
					if err := json.Unmarshal(content, &result); err != nil {
						t.Fatal(err)
					}
					if result.Success || len(
						result.Completion.InspectionRequests,
					) != 1 || result.Completion.InspectionRequests[0].Target != "helper.go" {
						t.Fatal(
							"The probe lost the pending request or reported a completed review.",
						)
					}
					assertDiagnosticInspectionUsage(t, result.Usage)
					return
				}
				var result struct {
					FailedCases int `json:"failed_cases"`
					Cases       []struct {
						Completed bool                `json:"completed"`
						Usage     review.UsageSummary `json:"usage"`
					} `json:"cases"`
				}
				if err := json.Unmarshal(content, &result); err != nil {
					t.Fatal(err)
				}
				if result.FailedCases != 1 || len(result.Cases) != 1 || result.Cases[0].Completed {
					t.Fatal(
						"The evaluation reported completion while inspection remained necessary.",
					)
				}
				assertDiagnosticInspectionUsage(t, result.Cases[0].Usage)
			},
		)
	}
}

func assertDiagnosticInspectionUsage(t *testing.T, usage review.UsageSummary) {
	t.Helper()
	if usage.Requests != 1 || usage.ReportedRequests != 1 || usage.InputTokens != 11 || usage.OutputTokens != 5 || usage.TotalTokens != 16 {
		t.Fatalf("The incomplete diagnostic lost reported request usage: %+v", usage)
	}
}

func diagnosticInspectionRuntime(t *testing.T, root, directory, endpoint string) string {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(root, "runtime.json"))
	if err != nil {
		t.Fatal(err)
	}
	var runtime map[string]json.RawMessage
	if err := json.Unmarshal(content, &runtime); err != nil {
		t.Fatal(err)
	}
	runtime["PROVIDERS"], err = json.Marshal([]map[string]string{
		{
			"id":               "openai_goodkind_io",
			"base_url":         endpoint,
			"model":            "inspection-command-model",
			"reasoning_effort": "low",
			"api_key_binding":  "OPENAI_GOODKIND_IO_API_KEY",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for key, path := range map[string]string{
		"REVIEW_RULES_FILE":   "config/review-rules.json",
		"REVIEW_PROMPTS_FILE": "config/review-prompts.json",
	} {
		runtime[key], err = json.Marshal(filepath.Join(root, path))
		if err != nil {
			t.Fatal(err)
		}
	}
	content, err = json.Marshal(runtime)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "runtime.json")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func writePendingInspectionResponse(writer http.ResponseWriter, _ *http.Request) {
	const result = `{"overview":"The changed source requires its existing helper implementation.","omissions_acceptable":true,"decision_reason":"","file_assessments":[],"findings":[],"inspection_requests":[{"operation":"read_file","target":"helper.go","query":"","start_line":0,"end_line":0,"reason":"Check the existing helper before completing the source review."}]}`
	writer.Header().Set("Content-Type", "text/event-stream")
	frames := []map[string]any{
		{
			"type":          "response.output_text.delta",
			"delta":         result,
			"output_index":  0,
			"content_index": 0,
		},
		{
			"type": "response.completed",
			"response": map[string]any{
				"id":     "inspection-command-response",
				"object": "response",
				"status": "completed",
				"model":  "inspection-command-model",
				"output": []map[string]any{
					{
						"type":   "message",
						"role":   "assistant",
						"status": "completed",
						"content": []map[string]any{
							{
								"type":        "output_text",
								"text":        result,
								"annotations": []any{},
							},
						},
					},
				},
				"usage": map[string]int{
					"input_tokens":  11,
					"output_tokens": 5,
					"total_tokens":  16,
				},
			},
		},
	}
	for _, frame := range frames {
		content, err := json.Marshal(frame)
		if err != nil {
			http.Error(
				writer,
				"The local response could not be encoded.",
				http.StatusInternalServerError,
			)
			return
		}
		if _, err := fmt.Fprintf(writer, "data: %s\n\n", content); err != nil {
			return
		}
	}
}
