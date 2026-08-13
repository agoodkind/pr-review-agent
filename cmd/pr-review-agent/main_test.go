package main

import (
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
