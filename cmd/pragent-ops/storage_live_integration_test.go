//go:build integration

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"goodkind.io/pr-review-agent/internal/quota"
)

func TestLiveStorageCommandReadsSignedStatistics(t *testing.T) {
	keyPath := os.Getenv("PR_AGENT_LIVE_STORAGE_SIGNING_KEY_FILE")
	if keyPath == "" {
		t.Skip("The live storage command requires an explicit signing-key file.")
	}
	var stdout, stderr bytes.Buffer
	args := []string{"storage", "--runtime", filepath.Join("..", "..", "runtime.json"), "--signing-key-file", keyPath}
	if err := run(t.Context(), args, &stdout, &stderr); err != nil {
		t.Fatal("The storage command could not read production statistics:", err)
	}
	var statistics quota.StorageStats
	if err := json.Unmarshal(stdout.Bytes(), &statistics); err != nil {
		t.Fatal("The storage command did not return structured statistics:", err)
	}
	if statistics.DatabaseBytes <= 0 || statistics.Events <= 0 || statistics.Retention.Enabled {
		t.Fatal("The retained production history must contain measured records with cleanup disabled.")
	}
	directory, err := os.MkdirTemp("", "pragent-signed-storage-live-")
	if err != nil {
		t.Fatal("The test could not create a private evidence directory.")
	}
	path := filepath.Join(directory, "storage.json")
	if err := os.WriteFile(path, stdout.Bytes(), 0o600); err != nil {
		t.Fatal("The test could not save private storage evidence.")
	}
	t.Logf("Read %d records and %d allocated database bytes. Private evidence: %s", statistics.Events, statistics.DatabaseBytes, path)
}
