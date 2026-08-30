package runlog_test

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"goodkind.io/pr-review-agent/internal/runlog"
)

// captured runs one logger against a recorder and hands back the recorder.
func captured(t *testing.T, write func(logger *slog.Logger)) *runlog.Recorder {
	t.Helper()
	recorder := runlog.NewRecorder()
	write(slog.New(recorder))
	return recorder
}

// The rendered log goes onto a check run, which is as public and as permanent
// as a pull request comment. A model provider error can carry the request it
// failed on, an internal endpoint, or a credential, so the value never reaches
// the render while everything else on the line does.
func TestRenderWithholdsTheValueOfEveryCauseField(t *testing.T) {
	recorder := captured(t, func(logger *slog.Logger) {
		logger.Error(
			"review chunk request failed",
			slog.Int("chunk", 3),
			slog.Int("chunks", 13),
			slog.String("err", "model provider returned HTTP 400: upstream call failed: secret-token-abc123"),
		)
		logger.Error("review chunks unread", slog.Any("causes", []string{
			"chunk 3: model provider returned HTTP 400: secret-token-abc123",
		}))
	})

	rendered := recorder.Render()

	if strings.Contains(rendered, "secret-token-abc123") {
		t.Fatalf("render published text this service never wrote:\n%s", rendered)
	}
	for _, want := range []string{
		"review chunk request failed",
		"review chunks unread",
		"chunk=3",
		"chunks=13",
		"err=[redacted: see this service's log for this run]",
		"causes=[redacted: see this service's log for this run]",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("render missing %q:\n%s", want, rendered)
		}
	}
}

// Withholding runs off an allowlist, so a field nobody vouched for is withheld
// rather than published. last_err is such a field today: it carries a provider
// sentence, and no denylist ever named it. provider_detail stands for the field
// a later log line adds and forgets to list. Both keep their key and their line,
// so the reader loses a lookup rather than the diagnosis.
func TestRenderWithholdsTheValueOfEveryFieldNotListed(t *testing.T) {
	recorder := captured(t, func(logger *slog.Logger) {
		logger.Error(
			"model provider stream retried",
			slog.Int("chunk", 3),
			slog.String("last_err", "upstream call failed: secret-token-abc123"),
		)
		logger.Error(
			"review chunk request failed",
			slog.String("provider_detail", "POST https://internal.example/v1: secret-token-abc123"),
		)
	})

	rendered := recorder.Render()

	if strings.Contains(rendered, "secret-token-abc123") {
		t.Fatalf("render published a field nobody vouched for:\n%s", rendered)
	}
	for _, want := range []string{
		"model provider stream retried",
		"review chunk request failed",
		"chunk=3",
		"last_err=[redacted: see this service's log for this run]",
		"provider_detail=[redacted: see this service's log for this run]",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("render missing %q:\n%s", want, rendered)
		}
	}
}

// Withholding is a property of the published render, not of the capture. The
// entries themselves keep the cause, because the service log the check's run
// identifier points at is where a reader recovers it.
func TestEntriesKeepTheCauseTheRenderWithholds(t *testing.T) {
	recorder := captured(t, func(logger *slog.Logger) {
		logger.Error("review job failed", slog.String("err", "upstream call failed: secret-token-abc123"))
	})

	entries := recorder.Entries()
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	if !strings.Contains(entries[0].Fields["err"], "secret-token-abc123") {
		t.Fatalf("entry err = %q, want the cause kept for the private log", entries[0].Fields["err"])
	}
}

// A field that carries this service's own wording is published as it is, so
// redaction never costs the reader the diagnosis they came for.
func TestRenderKeepsFieldsThisServiceWrote(t *testing.T) {
	recorder := captured(t, func(logger *slog.Logger) {
		logger.Info(
			"review job skipped",
			slog.String("reason", "173 diff chunks, over the 60 chunk review budget"),
			slog.String("model", "fixture-review-model"),
		)
	})

	rendered := recorder.Render()
	for _, want := range []string{
		"reason=173 diff chunks, over the 60 chunk review budget",
		"model=fixture-review-model",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("render missing %q:\n%s", want, rendered)
		}
	}
}

func TestRenderReturnsNothingForARunThatLoggedNothing(t *testing.T) {
	if rendered := runlog.NewRecorder().Render(); rendered != "" {
		t.Fatalf("render = %q, want the empty string so the caller omits the section", rendered)
	}
}

// Tee keeps the service log writing while the recorder captures, so nothing the
// check run withholds is lost from the sink an operator actually reads.
func TestTeeWritesToEverySink(t *testing.T) {
	first := runlog.NewRecorder()
	second := runlog.NewRecorder()

	slog.New(runlog.Tee(first, second)).ErrorContext(
		context.Background(),
		"review job failed",
		slog.String("err", "upstream call failed"),
	)

	for name, recorder := range map[string]*runlog.Recorder{"first": first, "second": second} {
		entries := recorder.Entries()
		if len(entries) != 1 || entries[0].Fields["err"] != "upstream call failed" {
			t.Fatalf("%s sink entries = %+v, want the record with its cause", name, entries)
		}
	}
}
