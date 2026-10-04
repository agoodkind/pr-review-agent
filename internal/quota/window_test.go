package quota_test

import (
	"testing"
	"time"

	"goodkind.io/pr-review-agent/internal/quota"
)

func TestRollingAdmissionWaitsForEnoughReportedUsageToExpire(t *testing.T) {
	now := time.Date(2026, time.October, 4, 12, 0, 0, 0, time.UTC)
	window := quota.Window{Mode: quota.Rolling, Duration: "24h"}
	events := []quota.Event{
		{AdmittedAtMS: now.Add(-24 * time.Hour).UnixMilli(), Usage: quota.Usage{InputTokens: 900}},
		{AdmittedAtMS: now.Add(-23 * time.Hour).UnixMilli(), Usage: quota.Usage{InputTokens: 1, OutputTokens: 9}},
		{AdmittedAtMS: now.Add(-22 * time.Hour).UnixMilli(), Usage: quota.Usage{InputTokens: 50, OutputTokens: 100}},
		{AdmittedAtMS: now.Add(-time.Hour).UnixMilli(), Usage: quota.Usage{InputTokens: 49, OutputTokens: 100}},
	}
	snapshot, err := quota.Evaluate(window, now, 50, []quota.TokenType{quota.InputTokens}, events, now.Add(-12*time.Hour).UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Allowed || snapshot.Used != 100 || snapshot.Remaining != 0 || snapshot.HistoryComplete {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	wantAvailable := now.Add(2 * time.Hour)
	if snapshot.AvailableAtMS != wantAvailable.UnixMilli() {
		t.Fatalf("available = %s, want %s", time.UnixMilli(snapshot.AvailableAtMS), wantAvailable)
	}
	snapshot, err = quota.Evaluate(window, wantAvailable, 50, []quota.TokenType{quota.InputTokens}, events, now.Add(-12*time.Hour).UnixMilli())
	if err != nil || !snapshot.Allowed || snapshot.Used != 49 {
		t.Fatalf("after expiration = %+v, error = %v", snapshot, err)
	}
}

func TestFixedWindowChargesUsageToAdmissionInterval(t *testing.T) {
	window := quota.Window{Mode: quota.Fixed, Duration: "24h", Anchor: "1970-01-01T00:00:00Z"}
	midnight := time.Date(2026, time.October, 5, 0, 0, 0, 0, time.UTC)
	events := []quota.Event{
		{AdmittedAtMS: midnight.Add(-time.Millisecond).UnixMilli(), Usage: quota.Usage{TotalTokens: 100}},
		{AdmittedAtMS: midnight.UnixMilli(), Usage: quota.Usage{TotalTokens: 7}},
	}
	before, err := quota.Evaluate(window, midnight.Add(-time.Millisecond), 100, nil, events, midnight.Add(-48*time.Hour).UnixMilli())
	if err != nil || before.Allowed || before.Used != 100 || before.AvailableAtMS != midnight.UnixMilli() {
		t.Fatalf("before reset = %+v, error = %v", before, err)
	}
	after, err := quota.Evaluate(window, midnight, 100, nil, events, midnight.Add(-48*time.Hour).UnixMilli())
	if err != nil || !after.Allowed || after.Used != 7 || !after.HistoryComplete {
		t.Fatalf("after reset = %+v, error = %v", after, err)
	}
}

func TestCronCalendarWindowsUseTimezoneAndDaylightSavingBoundaries(t *testing.T) {
	window := quota.Window{Mode: quota.Cron, Schedule: "0 0 * * *", Timezone: "America/Los_Angeles"}
	for _, example := range []struct{ name, now, start, end string }{
		{"spring", "2026-03-08T20:00:00Z", "2026-03-08T08:00:00Z", "2026-03-09T07:00:00Z"},
		{"fall", "2026-11-01T20:00:00Z", "2026-11-01T07:00:00Z", "2026-11-02T08:00:00Z"},
		{"exact reset", "2026-10-05T07:00:00Z", "2026-10-05T07:00:00Z", "2026-10-06T07:00:00Z"},
	} {
		t.Run(example.name, func(t *testing.T) {
			now, err := time.Parse(time.RFC3339, example.now)
			if err != nil {
				t.Fatal(err)
			}
			bounds, err := window.Bounds(now)
			if err != nil {
				t.Fatal(err)
			}
			if time.UnixMilli(bounds.StartMS).UTC().Format(time.RFC3339) != example.start || time.UnixMilli(bounds.EndMS).UTC().Format(time.RFC3339) != example.end || !bounds.IncludeStart || bounds.IncludeEnd {
				t.Fatalf("bounds = %+v", bounds)
			}
		})
	}
}

func TestCronWindowRejectsMachineDependentTimezone(t *testing.T) {
	window := quota.Window{Mode: quota.Cron, Schedule: "0 0 * * *", Timezone: "Local"}
	if _, err := window.Bounds(time.Date(2026, time.October, 4, 12, 0, 0, 0, time.UTC)); err == nil {
		t.Fatal("cron window accepted the machine's local timezone")
	}
}

func TestCronWindowRejectsImpossibleCalendarReset(t *testing.T) {
	window := quota.Window{Mode: quota.Cron, Schedule: "0 0 30 2 *", Timezone: "UTC"}
	if err := window.Validate(); err == nil {
		t.Fatal("cron window accepted February 30")
	}
	window.Schedule = "0 0 29 2 *"
	if err := window.Validate(); err != nil {
		t.Fatalf("cron window rejected February 29: %v", err)
	}
}
