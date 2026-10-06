package opsstats

import (
	"bytes"
	"encoding/json"
	"time"

	"goodkind.io/pr-review-agent/internal/reassessment"
)

type queuePayload struct {
	Records          json.RawMessage `json:"records"`
	Targets          json.RawMessage `json:"targets"`
	Sweep            json.RawMessage `json:"sweep"`
	NextAlarmMS      json.RawMessage `json:"next_alarm_ms"`
	TransitionCounts json.RawMessage `json:"transition_counts"`
}

func jsonArray(value json.RawMessage) bool {
	trimmed := bytes.TrimSpace(value)
	return len(trimmed) > 0 && trimmed[0] == '['
}

func jsonNull(value json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(value), []byte("null"))
}

// ReadQueueSnapshot preserves unavailable legacy fields instead of reporting empty diagnostics.
func ReadQueueSnapshot(data []byte, capturedAt time.Time, status int) QueueSnapshot {
	var snapshot QueueSnapshot
	snapshot.CapturedAt, snapshot.HTTPStatus = capturedAt, status
	var payload queuePayload
	if json.Unmarshal(data, &payload) != nil {
		snapshot.UnavailableReason = "The queue response could not be decoded."
		return snapshot
	}
	snapshot.Known = jsonArray(payload.Records) && json.Unmarshal(payload.Records, &snapshot.Records) == nil
	if !snapshot.Known {
		snapshot.UnavailableReason = "The queue response does not contain a valid records array."
	}
	snapshot.RegistryKnown = jsonArray(payload.Targets) && json.Unmarshal(payload.Targets, &snapshot.Targets) == nil
	if !snapshot.RegistryKnown {
		snapshot.UnknownFields = append(snapshot.UnknownFields, "targets")
	}
	readSweep(&snapshot, payload.Sweep)
	readAlarm(&snapshot, payload.NextAlarmMS)
	readTransitionCounts(&snapshot, payload.TransitionCounts)
	snapshot.UnknownFields = append(snapshot.UnknownFields, "sweep_failure")
	return snapshot
}

func readSweep(snapshot *QueueSnapshot, value json.RawMessage) {
	if jsonNull(value) {
		snapshot.SweepKnown = true
		return
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(value, &fields) != nil || !sweepFieldsPresent(fields) {
		snapshot.UnknownFields = append(snapshot.UnknownFields, "sweep")
		return
	}
	var cursor reassessment.SweepCursor
	if json.Unmarshal(value, &cursor) != nil {
		snapshot.UnknownFields = append(snapshot.UnknownFields, "sweep")
		return
	}
	snapshot.Sweep, snapshot.SweepKnown = &cursor, true
}

func sweepFieldsPresent(fields map[string]json.RawMessage) bool {
	for _, key := range []string{"version", "stage", "not_before_ms", "installation_page", "installation_index", "installation_ids", "installations_next", "repository_page", "repository_index", "repositories", "repositories_next", "pull_request_page", "pull_request_index", "pull_requests", "pull_requests_next"} {
		if len(fields[key]) == 0 {
			return false
		}
	}
	return true
}

func readAlarm(snapshot *QueueSnapshot, value json.RawMessage) {
	if jsonNull(value) {
		snapshot.AlarmKnown = true
		return
	}
	var alarm int64
	if len(value) == 0 || json.Unmarshal(value, &alarm) != nil || alarm < 0 {
		snapshot.UnknownFields = append(snapshot.UnknownFields, "next_alarm_ms")
		return
	}
	snapshot.NextAlarmMS, snapshot.AlarmKnown = &alarm, true
}

func readTransitionCounts(snapshot *QueueSnapshot, value json.RawMessage) {
	if jsonNull(value) {
		snapshot.TransitionMetadataKnown = true
		return
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(value, &fields) != nil || fields == nil || len(fields["counts"]) == 0 || len(fields["since_ms"]) == 0 || len(fields["last_update_ms"]) == 0 {
		snapshot.UnknownFields = append(snapshot.UnknownFields, "transition_counts")
		return
	}
	var counts QueueTransitionCounts
	if json.Unmarshal(value, &counts) != nil || counts.Counts == nil || counts.SinceMS < 0 || counts.LastUpdateMS < counts.SinceMS {
		snapshot.UnknownFields = append(snapshot.UnknownFields, "transition_counts")
		return
	}
	for _, count := range counts.Counts {
		if count < 0 {
			snapshot.UnknownFields = append(snapshot.UnknownFields, "transition_counts")
			return
		}
	}
	snapshot.TransitionCounts, snapshot.TransitionMetadataKnown = &counts, true
}

type queuePhase string

const (
	queueWaiting    queuePhase = "waiting"
	queueRunning    queuePhase = "running"
	queueConfirming queuePhase = "confirming"
	queueTerminal   queuePhase = "terminal"
)

func queueMatches(item Revisit, record reassessment.Record) bool {
	if record.Job.Repository.Owner+"/"+record.Job.Repository.Name != item.Target.Repository || int64(record.Job.Number) != item.Target.Number {
		return false
	}
	for _, trigger := range item.Triggers {
		if trigger.CurrentHead && trigger.Head == string(record.Head) && trigger.RunID == record.OriginDeliveryID {
			return true
		}
	}
	return false
}

func applyQueueRecord(item *Revisit, record reassessment.Record) {
	if !queueMatches(*item, record) {
		return
	}
	item.QueueRecordIDs = append(item.QueueRecordIDs, record.ID)
	switch queuePhase(record.Phase) {
	case queueWaiting:
		item.Status = "reassessment_queued"
		if record.Reason == "quota_wait" {
			item.Status = "reassessment_deferred_by_quota"
		}
	case queueRunning:
		item.Status = "reassessment_running"
	case queueConfirming:
		item.Status = "reassessment_confirming"
	case queueTerminal:
		return
	}
}

// ApplyQueue excludes confirmed waiting retries from missing reassessment counts.
func ApplyQueue(report *Report, snapshot QueueSnapshot) {
	report.CurrentQueue = &snapshot
	if !snapshot.Known {
		return
	}
	for index := range report.Revisits {
		item := &report.Revisits[index]
		if item.Resolved || item.PullRequestState != "OPEN" || item.Draft {
			continue
		}
		for _, record := range snapshot.Records {
			applyQueueRecord(item, record)
		}
	}
	report.Totals.OpenReviewsWithoutRecordedReassessment = 0
	report.Totals.OpenThreadsWithoutRecordedDecision = 0
	report.Totals.OpenItemsWithoutTerminalEvidence = 0
	for _, item := range report.Revisits {
		report.recordMissingItem(item)
	}
}
