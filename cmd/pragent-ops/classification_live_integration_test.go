//go:build integration

package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"goodkind.io/pr-review-agent/internal/cloudflareops"
	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
	"goodkind.io/pr-review-agent/internal/openai"
	"goodkind.io/pr-review-agent/internal/review"
	"goodkind.io/pr-review-agent/internal/reviewrules"
)

func TestLiveContextualFileAssessments(t *testing.T) {
	if os.Getenv("PR_AGENT_LIVE_CLASSIFICATION_TEST") != "1" {
		t.Skip("Set PR_AGENT_LIVE_CLASSIFICATION_TEST=1 to assess fixture content with a configured provider.")
	}
	providerID := os.Getenv("PR_AGENT_LIVE_PROVIDER_ID")
	credentialPath := os.Getenv("PR_AGENT_LIVE_API_KEY_FILE")
	if providerID == "" || credentialPath == "" {
		t.Fatal("A provider ID and credential file are required for live classification.")
	}
	runtime, err := ReadProbeRuntime(filepath.Join("..", "..", "runtime.json"))
	if err != nil {
		t.Fatal(err)
	}
	credential, err := cloudflareops.ReadCredential(credentialPath)
	if err != nil {
		t.Fatal("The provider credential file could not be read.")
	}
	cfg, err := probeConfig(runtime, providerID, string(credential), os.Getenv("PR_AGENT_LIVE_SIGNING_KEY_FILE"))
	if err != nil {
		t.Fatal("The selected runtime provider could not be configured.")
	}
	model := openai.NewClient(cfg, nil)
	const encryptedPath = "credentials.vault"
	const ciphertext = "$ANSIBLE_VAULT;1.1;AES256\n6439656266616532393062646262633330323265323061336534306662636663\n"
	file := diff.FileContext{Path: encryptedPath, Status: "modified", Patch: "", CurrentContent: ciphertext, CoverageComplete: false, Gap: diff.CoverageGapPatchAbsent}
	parent := diff.FileContext{Path: "authorization.go", Status: "modified", CurrentContent: "package authorization\nconst role = \"reader\"\nconst credentialReference = \"service_token\"\n", CoverageComplete: true, Gap: diff.CoverageGapNone}
	for _, scenario := range []struct {
		name        string
		description string
		exclude     bool
	}{
		{name: "credential_rotation", description: "Rotate the authentication token stored as service_token. Authorization remains fixed by authorization.go. The encrypted value is a credential rather than a role or permission definition. This fixture does not supply external validation results.", exclude: true},
		{name: "hidden_authorization", description: "Change the role and allowed resources inside credentials.vault. The encrypted values now define which records each user may delete. The old authorization.go constants do not establish the new hidden permissions. This fixture does not supply decrypted values or permission validation results.", exclude: false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
			defer cancel()
			ctx, recorder := review.WithUsageRecorder(ctx)
			pr := githubapp.PullRequest{Head: domain.HeadSHA("1111111111111111111111111111111111111111"), Base: domain.HeadSHA("2222222222222222222222222222222222222222"), Title: scenario.name, Body: scenario.description}
			completion, err := review.AssessFileContent(ctx, model, pr, []diff.FileContext{file, parent}, encryptedPath, cfg.ReviewPolicy, cfg.PromptBytes())
			if err != nil {
				t.Fatal("The actual provider did not complete a valid file assessment.")
			}
			if len(completion.Result.FileAssessments) != 1 || completion.Result.OmissionsAcceptable != scenario.exclude {
				t.Fatalf("The contextual assessment did not distinguish credential rotation from hidden authorization: %+v", completion.Result.FileAssessments)
			}
			usage := recorder.Summary()
			if usage.Requests < 1 || usage.ReportedRequests < 1 {
				t.Fatal("The provider did not report actual request usage.")
			}
			t.Logf("provider=%s model=%s requests=%d input=%d output=%d action=%s", providerID, completion.Model, usage.Requests, usage.InputTokens, usage.OutputTokens, completion.Result.FileAssessments[0].Action)
		})
	}
	t.Run("parent_gitlink_scope", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
		defer cancel()
		pointer := diff.FileContext{Path: "lib/sdk", Status: "modified", Patch: "@@ -1 +1 @@\n-Subproject commit 2222222222222222222222222222222222222222\n+Subproject commit 3333333333333333333333333333333333333333\n", CurrentContent: "Subproject commit 3333333333333333333333333333333333333333\n", CoverageComplete: true, Gap: diff.CoverageGapNone}
		modules := diff.FileContext{Path: ".gitmodules", Status: "modified", CurrentContent: "[submodule \"sdk\"]\n\tpath = lib/sdk\n\turl = https://github.com/example/sdk.git\n", CoverageComplete: true, Gap: diff.CoverageGapNone}
		pr := githubapp.PullRequest{Head: domain.HeadSHA("1111111111111111111111111111111111111111"), Base: domain.HeadSHA("2222222222222222222222222222222222222222"), Title: "Update the SDK gitlink", Body: "The parent repository updates its pinned SDK commit. Assess the parent pointer and integration context. The parent change does not include nested upstream file diffs."}
		completion, err := review.AssessFileContent(ctx, model, pr, []diff.FileContext{pointer, modules}, pointer.Path, cfg.ReviewPolicy, cfg.PromptBytes())
		if err != nil {
			t.Fatal("The actual provider did not complete a valid parent gitlink assessment.")
		}
		assessment := completion.Result.FileAssessments[0]
		kindMatches := assessment.ContentKind == reviewrules.ContentExternal || assessment.ContentKind == reviewrules.ContentDependency
		if assessment.Path != pointer.Path || !kindMatches || assessment.Action == domain.FileActionGap || assessment.Action == domain.FileActionInspectRelated {
			t.Fatalf("The gitlink assessment did not limit its scope to the supplied parent change: %+v", assessment)
		}
	})
	t.Run("generated_executable_migration", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
		defer cancel()
		migration := diff.FileContext{Path: "migrations/0042.sql", Status: "added", Patch: "@@ -0,0 +1,2 @@\n+-- Code generated by the migration tool.\n+ALTER TABLE customers DROP COLUMN email;\n", CurrentContent: "-- Code generated by the migration tool.\nALTER TABLE customers DROP COLUMN email;\n", CoverageComplete: true, Gap: diff.CoverageGapNone}
		pr := githubapp.PullRequest{Head: domain.HeadSHA("1111111111111111111111111111111111111111"), Base: domain.HeadSHA("2222222222222222222222222222222222222222"), Title: "Generate the customer schema migration", Body: "The schema tool generated a migration that removes customers.email. Generation does not establish that losing existing values is safe or that deployed callers stopped using the column."}
		completion, err := review.AssessFileContent(ctx, model, pr, []diff.FileContext{migration}, migration.Path, cfg.ReviewPolicy, cfg.PromptBytes())
		if err != nil {
			t.Fatal("The actual provider did not complete a valid executable migration assessment.")
		}
		if completion.Result.FileAssessments[0].Action == domain.FileActionExclude || completion.Result.OmissionsAcceptable {
			t.Fatal("The assessment excluded executable SQL because a tool generated it.")
		}
	})
}
