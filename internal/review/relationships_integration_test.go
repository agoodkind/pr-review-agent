package review_test

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"goodkind.io/pr-review-agent/internal/config"
	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/openai"
	"goodkind.io/pr-review-agent/internal/policytest"
	"goodkind.io/pr-review-agent/internal/review"
	"goodkind.io/pr-review-agent/internal/reviewrules"
)

func TestRelationshipReviewJoinsRealGitSourceWithSharedState(t *testing.T) {
	file, chunks := relationshipGitInput(t)
	policy := policytest.Load(t)
	model := openai.NewClient(config.Config{ReviewPolicy: policy, MinimumImportance: 7, ReviewModel: "configured-model"}, nil)
	maximumBytes := config.MaximumPromptBytes
	combined, err := review.PrepareRelationshipChunks(model, chunks, []diff.FileContext{file}, 7, "", policy, maximumBytes)
	if err != nil {
		t.Fatal(err)
	}
	if len(combined) <= len(chunks) {
		t.Fatal("Separated changes did not receive joint review work.")
	}
	sharedStateReviewed := false
	for _, chunk := range combined[len(chunks):] {
		if !chunk.CoverageComplete || len(chunk.Pieces) != 1 {
			t.Fatal("The related source must fit one indivisible joint assessment.")
		}
		if _, _, split := chunk.Split(); split {
			t.Fatal("Dividing a relationship assessment separated its required source endpoints.")
		}
		rows, err := diff.ChangedSourceRows(chunk)
		if err != nil {
			t.Fatal(err)
		}
		writer, removal := false, false
		for _, row := range rows {
			if row.Path != file.Path || !diff.ValidRange(file.ChangedRightLines, file.ChangedRightHunks, row.RightLine, row.RightLine) {
				t.Fatal("Joint review invented a source coordinate.")
			}
			writer = writer || strings.Contains(row.Text, `store["x"] = 2`)
			removal = removal || strings.Contains(row.Text, `delete(store, "y")`)
		}
		sharedStateReviewed = sharedStateReviewed || (writer && removal)
		prompt, err := review.BuildPrompt(chunk, 7, "", policy, maximumBytes)
		if err != nil {
			t.Fatal(err)
		}
		payloads, err := model.ReviewRequestPayloads(prompt)
		if err != nil {
			t.Fatal(err)
		}
		for _, payload := range payloads {
			if len(payload.Body) > maximumBytes {
				t.Fatal("The actual provider request exceeds the joint review budget.")
			}
		}
	}
	if !sharedStateReviewed {
		t.Fatal("The separated writer and deletion changes did not appear together in a request.")
	}
	limited, err := review.PrepareRelationshipChunks(model, chunks, []diff.FileContext{file}, 7, "", policy, 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, chunk := range limited {
		if chunk.RelationshipReview && chunk.CoverageComplete {
			t.Fatal("An over-budget joint assessment falsely established complete coverage.")
		}
	}
}

func TestRelationshipReviewHonorsConfiguredWorkLimit(t *testing.T) {
	file, chunks := relationshipGitInput(t)
	for _, enabled := range []bool{true, false} {
		policy := relationshipPolicy(t, enabled, 1)
		model := openai.NewClient(config.Config{ReviewPolicy: policy, MinimumImportance: 7, ReviewModel: "configured-model"}, nil)
		combined, err := review.PrepareRelationshipChunks(model, chunks, []diff.FileContext{file}, 7, "", policy, config.MaximumPromptBytes)
		if err != nil {
			t.Fatal(err)
		}
		if !enabled {
			if len(combined) != len(chunks) {
				t.Fatal("Disabled joint review added work.")
			}
			continue
		}
		if len(combined) != len(chunks)+2 || !combined[len(chunks)].CoverageComplete || combined[len(chunks)+1].CoverageComplete {
			t.Fatal("The configured pair limit must retain one joint assessment and an explicit gap for remaining work.")
		}
	}
}

func TestRelationshipReviewSubdividesLargeGitSourceForJointRequests(t *testing.T) {
	var before, after strings.Builder
	for _, destination := range []*strings.Builder{&before, &after} {
		destination.WriteString("package sample\n\nvar store = map[int]string{}\n\nfunc write() {\n")
	}
	for index := range 1000 {
		fmt.Fprintf(&before, "store[%d] = %q\n", index, strings.Repeat("x", 48))
		fmt.Fprintf(&after, "store[%d] = %q\n", index, strings.Repeat("y", 48))
	}
	before.WriteString("}\n")
	after.WriteString("}\n")
	file := relationshipGitChangedFile(t, before.String(), after.String())
	policy := policytest.Load(t)
	model := openai.NewClient(config.Config{ReviewPolicy: policy, MinimumImportance: 7, ReviewModel: "configured-model"}, nil)
	maximumBytes := config.MaximumPromptBytes
	chunks, err := diff.ChunkInput(diff.ReviewInput{Files: []diff.FileContext{file}}, maximumBytes)
	if err != nil {
		t.Fatal(err)
	}
	chunks, err = review.BoundModelReviewChunks(model, chunks, []diff.FileContext{file}, 7, "", policy, maximumBytes)
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) < 2 {
		t.Fatal("The real Git source must require several ordinary review requests.")
	}
	baseline, err := model.ReviewRequestBytes("")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("The provider requires %d bytes before source; the ordinary review uses %d chunks.", baseline, len(chunks))
	combined, err := review.PrepareRelationshipChunks(model, chunks, []diff.FileContext{file}, 7, "", policy, maximumBytes)
	if err != nil {
		t.Fatal(err)
	}
	ordinaryRows := make(map[int]string)
	joint := 0
	for _, chunk := range combined {
		if !chunk.CoverageComplete {
			t.Fatalf("Divisible handwritten source must support complete joint assessments within the configured budget: %d/%d %s", chunk.Index, chunk.Total, chunk.Text)
		}
		rows, err := diff.ChangedSourceRows(chunk)
		if err != nil {
			t.Fatal(err)
		}
		if chunk.RelationshipReview {
			joint++
		} else {
			for _, row := range rows {
				if _, duplicate := ordinaryRows[row.RightLine]; duplicate {
					t.Fatal("Relationship preparation duplicated ordinary source coverage.")
				}
				ordinaryRows[row.RightLine] = row.Text
			}
		}
		prompt, err := review.BuildPrompt(chunk, 7, "", policy, maximumBytes)
		if err != nil {
			t.Fatal(err)
		}
		actual, err := model.ReviewRequestBytes(prompt)
		if err != nil || actual > maximumBytes {
			t.Fatalf("A prepared request exceeds the actual serialized budget: %d bytes, %v.", actual, err)
		}
	}
	if joint == 0 || len(ordinaryRows) != len(file.ChangedRightLines) {
		t.Fatal("Joint review preparation lost necessary source or omitted the joint assessment.")
	}
	const restrictedBytes = 80000
	restricted, err := diff.ChunkInput(diff.ReviewInput{Files: []diff.FileContext{file}}, restrictedBytes)
	if err != nil {
		t.Fatal(err)
	}
	restricted, err = review.BoundModelReviewChunks(model, restricted, []diff.FileContext{file}, 7, "", policy, restrictedBytes)
	if err != nil {
		t.Fatal(err)
	}
	limited, err := review.PrepareRelationshipChunks(model, restricted, []diff.FileContext{file}, 7, "", policy, restrictedBytes)
	if err != nil {
		t.Fatal(err)
	}
	if len(limited) > len(restricted)+policy.RelationshipReview().MaximumPairs+1 {
		t.Fatal("An already exhausted relationship work limit multiplied ordinary review work.")
	}
	gap := false
	for _, chunk := range limited {
		gap = gap || !chunk.CoverageComplete
	}
	if !gap {
		t.Fatal("The restricted request budget falsely established complete joint coverage.")
	}
}

func relationshipPolicy(t *testing.T, enabled bool, maximumPairs int) reviewrules.Policy {
	t.Helper()
	configuration := filepath.Join("..", "..", "config")
	data, err := os.ReadFile(filepath.Join(configuration, "review-prompts.json"))
	if err != nil {
		t.Fatal(err)
	}
	var prompts map[string]json.RawMessage
	if err := json.Unmarshal(data, &prompts); err != nil {
		t.Fatal(err)
	}
	prompts["relationship_review"], err = json.Marshal(reviewrules.RelationshipReviewPolicy{Enabled: enabled, MaximumPairs: maximumPairs})
	if err != nil {
		t.Fatal(err)
	}
	data, err = json.Marshal(prompts)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "review-prompts.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	policy, err := reviewrules.LoadPolicy(filepath.Join(configuration, "review-rules.json"), path)
	if err != nil {
		t.Fatal(err)
	}
	return policy
}

func relationshipGitInput(t *testing.T) (diff.FileContext, []diff.Chunk) {
	t.Helper()
	before := "package sample\n\nvar store = map[string]int{}\n\nfunc write() {\n\tstore[\"x\"] = 1\n}\n\nfunc unrelated() int {\n\treturn 1\n}\n\nfunc remove() {\n\tdelete(store, \"x\")\n}\n"
	after := strings.ReplaceAll(before, "= 1", "= 2")
	after = strings.ReplaceAll(after, "return 1", "return 2")
	after = strings.ReplaceAll(after, `delete(store, "x")`, `delete(store, "y")`)
	file := relationshipGitChangedFile(t, before, after)
	chunks, err := diff.ChunkInput(diff.ReviewInput{Files: []diff.FileContext{file}}, 240)
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) < 3 {
		t.Fatal("The real Git fixture must separate writer, unrelated, and deletion changes.")
	}
	return file, chunks
}

func relationshipGitChangedFile(t *testing.T, before, after string) diff.FileContext {
	t.Helper()
	directory := t.TempDir()
	relationshipGitCommand(t, directory, "init", "--quiet")
	path := filepath.Join(directory, "state.go")
	if err := os.WriteFile(path, []byte(before), 0o600); err != nil {
		t.Fatal(err)
	}
	relationshipGitCommand(t, directory, "add", "state.go")
	relationshipGitCommand(t, directory, "-c", "user.name=Integration Test", "-c", "user.email=integration@example.invalid", "-c", "commit.gpgsign=false", "-c", "core.hooksPath=/dev/null", "commit", "--quiet", "-m", "Create source state\n\nCo-authored-by: Codex <noreply@openai.com>")
	if err := os.WriteFile(path, []byte(after), 0o600); err != nil {
		t.Fatal(err)
	}
	patch := relationshipGitCommand(t, directory, "diff", "--unified=0", "--", "state.go")
	changed, hunks, err := diff.ChangedRightLines(patch)
	if err != nil {
		t.Fatal(err)
	}
	return diff.FileContext{Path: "state.go", Status: "modified", Patch: patch, CurrentContent: after, ChangedRightLines: changed, ChangedRightHunks: hunks, CoverageComplete: true}
}

func relationshipGitCommand(t *testing.T, directory string, arguments ...string) string {
	t.Helper()
	command := exec.CommandContext(t.Context(), "git", arguments...)
	command.Dir = directory
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("Git %s failed: %v\n%s", arguments[0], err, output)
	}
	return string(output)
}
