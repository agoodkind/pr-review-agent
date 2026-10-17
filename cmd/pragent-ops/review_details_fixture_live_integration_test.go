//go:build integration

package main

import (
	"context"
	"strings"
	"testing"

	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/marker"
)

func (fixture *liveDetailsFixture) write(t *testing.T, ctx context.Context, body string) {
	t.Helper()
	expected := fixture.expected
	current := fixture.capture(t, ctx)
	if current != expected {
		t.Fatal("The fixture summary changed concurrently and will not be overwritten.")
	}
	if current == body {
		return
	}
	updated, err := fixture.client.UpdateIssueComment(ctx, fixture.opts.installationID, fixture.before.Repository, fixture.original.ID, body)
	if err != nil || updated.ID != fixture.original.ID || updated.Author != fixture.original.Author || updated.Body != body {
		t.Fatal("The fixture summary update did not retain its expected identity and body.")
	}
	fixture.expected = body
}

func liveDetailsSection(t *testing.T, body, prefix, suffix string) string {
	t.Helper()
	start := strings.Index(body, prefix)
	if start < 0 || strings.Count(body, prefix) != 1 {
		t.Fatal("The fixture requires exactly one selected summary section.")
	}
	end := strings.Index(body[start+len(prefix):], suffix)
	if end < 0 {
		t.Fatal("The selected summary section is incomplete.")
	}
	return body[start : start+len(prefix)+end+len(suffix)]
}

func liveDetailsAssertRecovered(t *testing.T, body, details, report string, head domain.HeadSHA) {
	t.Helper()
	state, valid := marker.DecodeState(body)
	if !valid || state.Status != marker.StateDone || state.LastReviewed != head || len(state.Pending) != 0 || len(state.Unread) != 0 {
		t.Fatal("The successful refresh did not restore the completed current-head state.")
	}
	if !strings.Contains(body, details) || !strings.Contains(body, report) || strings.Contains(body, "Review refresh failed") {
		t.Fatal("The recovered summary lost measured details or retained the operational failure.")
	}
}
