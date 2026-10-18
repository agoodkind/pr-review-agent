package queue_test

import (
	"log/slog"
	"testing"

	"goodkind.io/pr-review-agent/internal/domain"
	"goodkind.io/pr-review-agent/internal/queue"
)

func TestStoppedAdmissionPreservesAcceptedQueueStatus(t *testing.T) {
	dispatcher := queue.NewDispatcher(2, 1, nil, slog.Default())
	for _, delivery := range []string{"first", "second"} {
		if !dispatcher.Enqueue(domain.ReviewJob{DeliveryID: delivery}) {
			t.Fatalf("enqueue %s failed", delivery)
		}
	}
	before := dispatcher.Status()
	if before.AdmissionClosed || before.Queued != 2 || before.Running != 0 || before.Outstanding != 2 {
		t.Fatalf("accepted status = %+v", before)
	}
	if status := dispatcher.StopAdmissionIfIdle(); status != before {
		t.Fatalf("idle check changed accepted work or closed admission: %+v", status)
	}
	for range 2 {
		status := dispatcher.StopAdmission()
		if !status.AdmissionClosed || status.Queued != 2 || status.Running != 0 || status.Outstanding != 2 {
			t.Fatalf("drained status = %+v", status)
		}
		if dispatcher.Enqueue(domain.ReviewJob{DeliveryID: "late"}) {
			t.Fatal("closed admission accepted another job")
		}
	}
}

func TestIdleClosureRejectsConcurrentAdmission(t *testing.T) {
	for range 100 {
		dispatcher := queue.NewDispatcher(1, 1, nil, slog.Default())
		accepted := make(chan bool, 1)
		go func() {
			accepted <- dispatcher.Enqueue(domain.ReviewJob{DeliveryID: "concurrent"})
		}()
		dispatcher.StopAdmissionIfIdle()
		wasAccepted := <-accepted
		status := dispatcher.Status()
		if wasAccepted {
			if status.AdmissionClosed || status.Outstanding != 1 {
				t.Fatalf("idle closure discarded accepted work: %+v", status)
			}
		} else if !status.AdmissionClosed || status.Outstanding != 0 {
			t.Fatalf("idle closure did not reject admission: %+v", status)
		}
	}
}
