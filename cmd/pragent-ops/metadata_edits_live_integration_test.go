//go:build integration

package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"goodkind.io/pr-review-agent/internal/cloudflareops"
	"goodkind.io/pr-review-agent/internal/config"
	"goodkind.io/pr-review-agent/internal/diff"
	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/githubapp"
	"goodkind.io/pr-review-agent/internal/marker"
	"goodkind.io/pr-review-agent/internal/reassessment"
)

const metadataLivePollInterval = 2 * time.Second

func TestLiveMetadataEditsCoalesceWithoutRepeatingCompletedReview(t *testing.T) {
	if os.Getenv("PR_AGENT_LIVE_METADATA_SETTLE_TEST") != "1" {
		t.Skip("The deployed metadata settling regression is not enabled.")
	}
	if _, err := exec.LookPath("gh"); err != nil {
		t.Fatal("The GitHub CLI is required for actual fixture title edits.")
	}
	opts, reference, cfg, settings := metadataLiveConfiguration(t)
	interval, err := time.ParseDuration(settings.MetadataSettleInterval)
	if err != nil || interval < 30*time.Second {
		t.Fatal("The deployed regression requires a metadata settle interval of at least thirty seconds.")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*interval+2*time.Minute)
	defer cancel()
	client := githubapp.NewClient(cfg, &http.Client{Timeout: 30 * time.Second}, time.Now, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	before := metadataLiveSnapshot(t, ctx, client, reference)
	if !liveReviewBodyFixture(t, opts, before.PullRequest.Title) {
		t.Fatal("The explicit target must match the recovery configuration or identify a test fixture.")
	}
	reference.Head = before.PullRequest.Head
	summary := liveApprovalSummary(t, ctx, client, reference, cfg.GitHubBotLogin)
	state, valid := marker.DecodeState(summary.Body)
	if !valid || state.Status != marker.StateDone || state.LastReviewed != reference.Head || len(state.Pending) != 0 || len(state.Unread) != 0 {
		t.Fatal("The fixture requires completed file coverage before title edits.")
	}
	metadataLiveCache(t, summary.Body, before.PullRequest)
	if githubapp.LatestDecisionReview(before.Reviews, cfg.GitHubBotLogin).State != "CHANGES_REQUESTED" {
		t.Fatal("The selected fixture requires its original requested-changes decision.")
	}
	baseline := metadataLiveCheck(t, ctx, client, reference)
	if baseline.Status != "completed" {
		t.Fatal("The fixture must have no unfinished review check before the regression.")
	}
	evidence, err := cloudflareops.PrivateDirectory(os.Getenv("PR_AGENT_LIVE_METADATA_EVIDENCE_DIR"))
	if err != nil {
		t.Fatal("The private metadata regression evidence directory could not be created.")
	}
	t.Logf("Private metadata regression evidence is available in %s.", evidence)
	if metadataLiveSave(filepath.Join(evidence, "baseline.json"), before) != nil || metadataLiveSave(filepath.Join(evidence, "baseline-check.json"), baseline) != nil {
		t.Fatal("The original fixture and review check could not be captured privately.")
	}
	t.Cleanup(func() { metadataLiveRestore(t, client, reference, before.PullRequest, evidence) })
	started := time.Now().UTC()
	changedTitle := before.PullRequest.Title + " (metadata settling verification " + rand.Text() + ")"
	if err := metadataLiveEdit(ctx, reference, changedTitle, evidence); err != nil {
		t.Fatal("The first real GitHub title edit failed.")
	}
	first := metadataLiveAwaitCheck(t, ctx, client, reference, baseline.ID, "in_progress")
	firstRecord := metadataLiveRecord(t, ctx, settings.QueueURL, reference, first)
	if firstRecord != nil && metadataLiveSave(filepath.Join(evidence, "first-record.json"), firstRecord) != nil {
		t.Fatal("The first durable metadata record could not be captured privately.")
	}
	if time.Since(started) >= interval {
		t.Fatal("The first admission exceeded the configured settle interval.")
	}
	if err := metadataLiveEdit(ctx, reference, before.PullRequest.Title, evidence); err != nil {
		t.Fatal("The original GitHub title could not be restored for the second edit.")
	}
	second := metadataLiveAwaitCheck(t, ctx, client, reference, first.ID, "in_progress")
	secondRecord := metadataLiveRecord(t, ctx, settings.QueueURL, reference, second)
	if secondRecord != nil && metadataLiveSave(filepath.Join(evidence, "second-record.json"), secondRecord) != nil {
		t.Fatal("The coalesced durable metadata record could not be captured privately.")
	}
	if firstRecord != nil && secondRecord != nil {
		if secondRecord.Generation == firstRecord.Generation || !slices.Contains(secondRecord.SourceDeliveries, first.ExternalID) || secondRecord.NotBeforeMS <= firstRecord.NotBeforeMS {
			t.Fatal("The durable record did not replace the earlier edit and retain its delivery identifier.")
		}
	}
	completed := metadataLiveAwaitCompletion(t, ctx, client, reference, second)
	if completed.Conclusion != "success" || completed.Outcome.Disposition != domain.AssessmentCompleted || completed.Outcome.MetadataRevision != diff.PullRequestRevision(before.PullRequest) {
		t.Fatal("The restored metadata did not produce a completed assessment of the original revision.")
	}
	superseded, found, err := client.FindCheckRunByExternalID(ctx, reference.InstallationID, reference.Repository, reference.Head, config.ReviewCheckName, first.ExternalID)
	if err != nil || !found || superseded.Status != "completed" || superseded.Conclusion != "skipped" {
		t.Fatal("The earlier metadata check did not conclude as superseded.")
	}
	after := metadataLiveSnapshot(t, ctx, client, reference)
	if metadataLiveSave(filepath.Join(evidence, "final.json"), after) != nil {
		t.Fatal("The final fixture state could not be captured privately.")
	}
	if after.PullRequest != before.PullRequest || !reflect.DeepEqual(after.Reviews, before.Reviews) || !reflect.DeepEqual(after.Threads, before.Threads) {
		t.Fatal("The regression changed fixture metadata, native reviews, or inline threads.")
	}
	currentSummary := liveApprovalSummary(t, ctx, client, reference, cfg.GitHubBotLogin)
	currentState, valid := marker.DecodeState(currentSummary.Body)
	if !valid || currentSummary.ID != summary.ID || !reflect.DeepEqual(currentState, state) {
		t.Fatal("The editable summary or completed file checkpoint changed during metadata settling.")
	}
	metadataLiveLogs(t, ctx, started, reference, second.ExternalID, evidence)
	t.Logf("Metadata edits retained summary %d and completed check %d after superseding check %d.", summary.ID, second.ID, first.ID)
}

func metadataLiveConfiguration(t *testing.T) (reviewBodyOptions, domain.PullRequestRef, config.Config, config.Reassessment) {
	t.Helper()
	opts := liveReviewBodyOptions(t)
	repository, err := reviewBodyRepository(opts.repository)
	if err != nil {
		t.Fatal("The explicit metadata test repository is invalid.")
	}
	cfg, err := reviewBodyIdentity(opts)
	if err != nil {
		t.Fatal("The live GitHub app identity could not be loaded from its private-key file.")
	}
	data, err := os.ReadFile(opts.runtime)
	if err != nil {
		t.Fatal("The public runtime configuration could not be read.")
	}
	var runtime struct {
		Reassessment config.Reassessment `json:"REASSESSMENT"`
	}
	if json.Unmarshal(data, &runtime) != nil || !runtime.Reassessment.Enabled {
		t.Fatal("The runtime must enable durable reassessment for the live metadata regression.")
	}
	return opts, domain.PullRequestRef{Repository: repository, InstallationID: opts.installationID, Number: opts.pullRequest, Head: ""}, cfg, runtime.Reassessment
}

func metadataLiveSnapshot(t *testing.T, ctx context.Context, client *githubapp.Client, reference domain.PullRequestRef) reviewBodySnapshot {
	t.Helper()
	snapshot, err := captureReviewBodies(ctx, client, reference.InstallationID, reference.Repository, reference.Number)
	if err != nil || snapshot.PullRequest.State != "open" || snapshot.PullRequest.Draft || !snapshot.PullRequest.EligibilityKnown || !snapshot.PullRequest.MetadataKnown {
		t.Fatal("The real GitHub fixture could not be read as an eligible pull request.")
	}
	return snapshot
}

func metadataLiveCache(t *testing.T, body string, pullRequest githubapp.PullRequest) {
	t.Helper()
	const prefix = "<!-- pr-review-agent:metadata:v1 "
	start := strings.LastIndex(body, prefix)
	if start < 0 {
		t.Fatal("The fixture requires its existing metadata assessment record.")
	}
	encoded, _, found := strings.Cut(body[start+len(prefix):], " -->")
	decoded, err := base64.RawURLEncoding.DecodeString(encoded)
	var record struct {
		Head     domain.HeadSHA `json:"head"`
		Revision string         `json:"revision"`
	}
	if !found || err != nil || json.Unmarshal(decoded, &record) != nil || record.Head != pullRequest.Head || record.Revision != diff.PullRequestRevision(pullRequest) {
		t.Fatal("The fixture metadata cache does not match its original title and commit range.")
	}
}

func metadataLiveEdit(ctx context.Context, reference domain.PullRequestRef, title, evidence string) error {
	command := exec.CommandContext(ctx, "gh", "pr", "edit", strconv.Itoa(reference.Number), "--repo", reference.Repository.Owner+"/"+reference.Repository.Name, "--title", title)
	output, err := command.CombinedOutput()
	if err != nil {
		if writeErr := os.WriteFile(filepath.Join(evidence, "title-edit-error.txt"), output, 0o600); writeErr != nil {
			return errors.Join(err, writeErr)
		}
	}
	return err
}

func metadataLiveCheck(t *testing.T, ctx context.Context, client *githubapp.Client, reference domain.PullRequestRef) githubapp.CheckRun {
	t.Helper()
	check, found, err := client.FindCheckRun(ctx, reference.InstallationID, reference.Repository, reference.Head, config.ReviewCheckName)
	if err != nil || !found {
		t.Fatal("The fixture review check could not be read.")
	}
	return check
}

func metadataLiveAwaitCheck(t *testing.T, ctx context.Context, client *githubapp.Client, reference domain.PullRequestRef, previousID int64, status string) githubapp.CheckRun {
	t.Helper()
	for ctx.Err() == nil {
		check := metadataLiveCheck(t, ctx, client, reference)
		if check.ID != previousID {
			if check.Status != status {
				t.Fatal("The edited metadata check did not remain in progress immediately after admission.")
			}
			return check
		}
		metadataLiveWait(ctx)
	}
	t.Fatal("The deployed service did not admit the edited metadata before the deadline.")
	var empty githubapp.CheckRun
	return empty
}

func metadataLiveAwaitCompletion(t *testing.T, ctx context.Context, client *githubapp.Client, reference domain.PullRequestRef, admitted githubapp.CheckRun) githubapp.CheckRun {
	t.Helper()
	for ctx.Err() == nil {
		check := metadataLiveCheck(t, ctx, client, reference)
		if check.ID != admitted.ID {
			t.Fatal("The settled metadata review replaced its originally admitted check.")
		}
		if check.Status == "completed" {
			return check
		}
		metadataLiveWait(ctx)
	}
	t.Fatal("The deployed metadata assessment did not complete before the deadline.")
	var empty githubapp.CheckRun
	return empty
}

func metadataLiveWait(ctx context.Context) {
	select {
	case <-ctx.Done():
	case <-time.After(metadataLivePollInterval):
	}
}

func metadataLiveRestore(t *testing.T, client *githubapp.Client, reference domain.PullRequestRef, original githubapp.PullRequest, evidence string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	current, err := client.GetPullRequest(ctx, reference.InstallationID, reference.Repository, reference.Number)
	if err != nil || current.Title != original.Title {
		if err := metadataLiveEdit(ctx, reference, original.Title, evidence); err != nil {
			t.Error("The original fixture title could not be restored after the regression.")
		}
	}
	current, err = client.GetPullRequest(ctx, reference.InstallationID, reference.Repository, reference.Number)
	if err != nil || current.Title != original.Title || current.Body != original.Body || current.Head != original.Head {
		t.Error("The final fixture title, description, and head do not match the original snapshot.")
	}
}

func metadataLiveRecord(t *testing.T, ctx context.Context, address string, reference domain.PullRequestRef, admitted githubapp.CheckRun) *reassessment.Record {
	t.Helper()
	tokenFile := os.Getenv("PR_AGENT_LIVE_OPERATOR_TOKEN_FILE")
	if tokenFile == "" {
		t.Log("The operator token file is absent, so durable record readback is unavailable.")
		return nil
	}
	token, err := cloudflareops.ReadCredential(tokenFile)
	if err != nil {
		t.Fatal("The private operator token file could not be read.")
	}
	body, err := json.Marshal(map[string]string{"action": "query", "key": fmt.Sprintf("%d:%s", reference.InstallationID, reference.Key())})
	if err != nil {
		t.Fatal("The operator read request could not be encoded.")
	}
	for ctx.Err() == nil {
		request, requestErr := http.NewRequestWithContext(ctx, http.MethodPost, address, strings.NewReader(string(body)))
		if requestErr != nil {
			t.Fatal("The operator read request could not be created.")
		}
		request.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
		request.Header.Set("Content-Type", "application/json")
		response, readErr := (&http.Client{Timeout: 15 * time.Second}).Do(request)
		if readErr != nil {
			t.Fatal("The deployed durable record could not be queried.")
		}
		var payload struct {
			Record *reassessment.Record `json:"record"`
		}
		decodeErr := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&payload)
		closeErr := response.Body.Close()
		if response.StatusCode != http.StatusOK || decodeErr != nil || closeErr != nil {
			t.Fatal("The operator endpoint did not return a valid durable record.")
		}
		if payload.Record != nil && payload.Record.Job.CheckRunID == admitted.ID {
			if !payload.Record.Job.MetadataOnly || payload.Record.Phase != "waiting" || payload.Record.NotBeforeMS <= time.Now().UnixMilli() || !slices.Contains(payload.Record.SourceDeliveries, admitted.ExternalID) {
				t.Fatal("The durable record does not retain its pending metadata edit and future deadline.")
			}
			return payload.Record
		}
		metadataLiveWait(ctx)
	}
	t.Fatal("The admitted metadata check has no matching durable queued record.")
	return nil
}

func metadataLiveLogs(t *testing.T, ctx context.Context, started time.Time, reference domain.PullRequestRef, deliveryID, evidence string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer cancel()
	credential := os.Getenv("PR_AGENT_LIVE_CLOUDFLARE_TOKEN_FILE")
	accountCredential := os.Getenv("PR_AGENT_LIVE_CLOUDFLARE_ACCOUNT_TOKEN_FILE")
	if credential == "" && accountCredential == "" {
		t.Log("The observability token file is absent, so provider request telemetry is unavailable.")
		return
	}
	args := []string{"--from", started.Format(time.RFC3339), "--end", time.Now().UTC().Format(time.RFC3339), "--wrangler", "../../deploy/cloudflare/wrangler.jsonc", "--output-dir", filepath.Join(evidence, "cloudflare")}
	if credential != "" {
		args = append(args, "--token-file", credential)
	} else {
		args = append(args, "--account-token-file", accountCredential)
	}
	opts, err := flags("logs", args, io.Discard)
	if err != nil {
		t.Fatal("The private telemetry capture could not be configured.")
	}
	manifest, err := captureManifest(ctx, opts)
	if err != nil || !manifest.Complete {
		t.Fatal("The private telemetry capture did not exhaust available records.")
	}
	events, err := cloudflareops.ReadEvents(filepath.Join(manifest.Directory, "events.ndjson"))
	if err != nil {
		t.Fatal("The private metadata regression telemetry could not be read.")
	}
	seen := false
	for _, event := range events {
		source := event.Source
		if metadataLiveString(source["repository"]) != reference.Repository.Owner+"/"+reference.Repository.Name || metadataLiveString(source["pull_request"]) != strconv.Itoa(reference.Number) || metadataLiveString(source["delivery_id"]) != deliveryID {
			continue
		}
		seen = true
		message := metadataLiveString(source["message"])
		if metadataLiveString(source["provider_id"]) != "" || message == "review chunk completed" || message == "review model analysis started" || message == "model provider attempt failed" {
			t.Fatal("The restored metadata triggered an additional model or file-chunk request.")
		}
	}
	if !seen {
		t.Fatal("The telemetry capture contains no matching metadata assessment records.")
	}
	t.Logf("Private metadata regression telemetry is available in %s.", manifest.Directory)
}

func metadataLiveString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var value string
	if json.Unmarshal(raw, &value) == nil {
		return value
	}
	return strings.TrimSpace(string(raw))
}

func metadataLiveSave[T reviewBodySnapshot | githubapp.CheckRun | *reassessment.Record](path string, value T) error {
	data, err := json.Marshal(value)
	if err != nil {
		return errors.New("The private metadata evidence could not be encoded.")
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return errors.New("The private metadata evidence file could not be written.")
	}
	return nil
}
