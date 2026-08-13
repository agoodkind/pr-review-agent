package queue

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"goodkind.io/pr-review-agent/internal/domain"
)

func TestEndToEndConcurrentDuplicateDeliveryOneReview(t *testing.T) {
	cache := NewDeliveryCache(100, time.Hour, time.Now)
	runner := &recordingRunner{}
	dispatcher := NewDispatcher(1, runner, slog.Default())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dispatcher.Start(ctx)

	job := domain.ReviewJob{DeliveryID: "delivery-concurrent"}
	if !cache.Claim(job.DeliveryID) {
		t.Fatal("first claim: want true")
	}

	var winners int32
	var waitGroup sync.WaitGroup
	for range 12 {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			if cache.Claim(job.DeliveryID) {
				atomic.AddInt32(&winners, 1)
			}
		}()
	}
	waitGroup.Wait()
	if winners != 0 {
		t.Fatalf("extra claims = %d, want 0", winners)
	}

	if !dispatcher.Enqueue(job) {
		t.Fatal("enqueue: want true")
	}
	deadline := time.Now().Add(2 * time.Second)
	for len(runner.snapshot()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := runner.snapshot(); len(got) != 1 || got[0] != job.DeliveryID {
		t.Fatalf("processed = %v, want one delivery-concurrent job", got)
	}
}

func TestDeliveryCacheClaimsOnceUntilExpiry(t *testing.T) {
	now := time.Unix(100, 0)
	clock := func() time.Time { return now }
	cache := NewDeliveryCache(10, time.Minute, clock)

	if !cache.Claim("delivery-1") {
		t.Fatal("first claim: want true")
	}
	if cache.Claim("delivery-1") {
		t.Fatal("second claim: want false")
	}

	now = now.Add(2 * time.Minute)
	if !cache.Claim("delivery-1") {
		t.Fatal("claim after expiry: want true")
	}
}

func TestDeliveryCacheEvictsOldestAtCapacity(t *testing.T) {
	now := time.Unix(100, 0)
	clock := func() time.Time { return now }
	cache := NewDeliveryCache(2, time.Hour, clock)

	if !cache.Claim("delivery-1") {
		t.Fatal("claim delivery-1")
	}
	if !cache.Claim("delivery-2") {
		t.Fatal("claim delivery-2")
	}
	if !cache.Claim("delivery-3") {
		t.Fatal("claim delivery-3")
	}
	if cache.Claim("delivery-2") {
		t.Fatal("delivery-2 should remain claimed")
	}
	if !cache.Claim("delivery-1") {
		t.Fatal("delivery-1 should be reclaimable after eviction")
	}
	if !cache.Claim("delivery-2") {
		t.Fatal("delivery-2 should be reclaimable after delivery-1 fills the cache again")
	}
}

func TestDeliveryCacheConcurrentClaimHasOneWinner(t *testing.T) {
	cache := NewDeliveryCache(100, time.Hour, time.Now)
	var winners int
	var mu sync.Mutex
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if cache.Claim("delivery-1") {
				mu.Lock()
				winners++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if winners != 1 {
		t.Fatalf("winners = %d, want 1", winners)
	}
}

func TestKeyedLockerSerializesSameKeyAndAllowsDifferentKeys(t *testing.T) {
	locker := NewKeyedLocker()
	firstRunning := make(chan struct{})
	secondStarted := make(chan struct{})
	secondDone := make(chan struct{})

	unlockFirst := locker.Lock("repo#1")
	go func() {
		close(firstRunning)
		unlockSecond := locker.Lock("repo#1")
		close(secondStarted)
		<-secondDone
		unlockSecond()
	}()
	<-firstRunning

	unlockOther := locker.Lock("repo#2")
	unlockOther()

	select {
	case <-secondStarted:
		t.Fatal("second same-key lock acquired while first held")
	default:
	}

	unlockFirst()
	select {
	case <-secondStarted:
	case <-time.After(time.Second):
		t.Fatal("second same-key lock did not acquire after unlock")
	}
	close(secondDone)
}

type recordingRunner struct {
	mu    sync.Mutex
	order []string
	err   error
}

func (runner *recordingRunner) Run(_ context.Context, job domain.ReviewJob) error {
	runner.mu.Lock()
	defer runner.mu.Unlock()
	runner.order = append(runner.order, job.DeliveryID)
	return runner.err
}

func (runner *recordingRunner) snapshot() []string {
	runner.mu.Lock()
	defer runner.mu.Unlock()
	copied := make([]string, len(runner.order))
	copy(copied, runner.order)
	return copied
}

func TestDispatcherRejectsWhenFull(t *testing.T) {
	runner := &recordingRunner{}
	dispatcher := NewDispatcher(1, runner, slog.Default())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dispatcher.Start(ctx)

	if !dispatcher.Enqueue(domain.ReviewJob{DeliveryID: "delivery-1"}) {
		t.Fatal("first enqueue: want true")
	}
	if dispatcher.Enqueue(domain.ReviewJob{DeliveryID: "delivery-2"}) {
		t.Fatal("second enqueue: want false")
	}
}

func TestDispatcherRunsJobsInOrder(t *testing.T) {
	runner := &recordingRunner{}
	dispatcher := NewDispatcher(3, runner, slog.Default())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dispatcher.Start(ctx)

	for _, deliveryID := range []string{"delivery-1", "delivery-2", "delivery-3"} {
		if !dispatcher.Enqueue(domain.ReviewJob{DeliveryID: deliveryID}) {
			t.Fatalf("enqueue %s failed", deliveryID)
		}
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		if len(runner.snapshot()) == 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("order = %v, want 3 jobs", runner.snapshot())
		}
		time.Sleep(10 * time.Millisecond)
	}

	want := []string{"delivery-1", "delivery-2", "delivery-3"}
	if got := runner.snapshot(); !equalStringSlices(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

func TestDispatcherShutdownDrainsAcceptedJobs(t *testing.T) {
	runner := &recordingRunner{}
	dispatcher := NewDispatcher(2, runner, slog.Default())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dispatcher.Start(ctx)

	if !dispatcher.Enqueue(domain.ReviewJob{DeliveryID: "delivery-1"}) {
		t.Fatal("enqueue delivery-1")
	}
	if !dispatcher.Enqueue(domain.ReviewJob{DeliveryID: "delivery-2"}) {
		t.Fatal("enqueue delivery-2")
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer shutdownCancel()
	if err := dispatcher.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if got := runner.snapshot(); len(got) != 2 {
		t.Fatalf("processed = %v, want 2 jobs", got)
	}
}

func TestDispatcherKeepsClaimAfterRunnerFailure(t *testing.T) {
	cache := NewDeliveryCache(10, time.Hour, time.Now)
	if !cache.Claim("delivery-1") {
		t.Fatal("claim delivery-1")
	}
	runner := &recordingRunner{err: errors.New("boom")}
	dispatcher := NewDispatcher(1, runner, slog.Default())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dispatcher.Start(ctx)

	if !dispatcher.Enqueue(domain.ReviewJob{DeliveryID: "delivery-1"}) {
		t.Fatal("enqueue")
	}

	deadline := time.Now().Add(time.Second)
	for len(runner.snapshot()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if cache.Claim("delivery-1") {
		t.Fatal("claim should remain reserved after runner failure")
	}
}

func equalStringSlices(left []string, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
