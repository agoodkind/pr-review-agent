package opsstats

import "goodkind.io/pr-review-agent/internal/reassessment"

type queuePhase string

const (
	queueWaiting  queuePhase = "waiting"
	queueRunning  queuePhase = "running"
	queueTerminal queuePhase = "terminal"
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
