package main

import (
	"context"
	"io"
	"os"
	"os/exec"
	"testing"
)

func TestVersionFlag(t *testing.T) {
	command := exec.Command("go", "run", ".", "--version")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("go run . --version: %v\n%s", err, output)
	}

	want := "pr-review-agent dev (unknown, built unknown)\n"
	if string(output) != want {
		t.Fatalf("output = %q, want %q", output, want)
	}
}

func TestRejectsUnknownArguments(t *testing.T) {
	if code := run([]string{"pr-review-agent", "--help"}, lookupEnv, io.Discard, stubNotifyContext); code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
}

func TestRunPrintsVersion(t *testing.T) {
	if code := run([]string{"pr-review-agent", "--version"}, lookupEnv, io.Discard, stubNotifyContext); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
}

func stubNotifyContext(ctx context.Context, _ ...os.Signal) (context.Context, context.CancelFunc) {
	return context.WithCancel(ctx)
}
