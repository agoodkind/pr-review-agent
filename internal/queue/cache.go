// Package queue deduplicates webhook deliveries and serializes review jobs.
package queue

import (
	"sync"
	"time"
)

// Clock returns the current time.
type Clock func() time.Time

type cacheEntry struct {
	expiresAt time.Time
}

// DeliveryCache tracks claimed webhook delivery identifiers.
type DeliveryCache struct {
	capacity int
	ttl      time.Duration
	clock    Clock
	mu       sync.Mutex
	entries  map[string]cacheEntry
	order    []string
}

// NewDeliveryCache creates a bounded delivery claim cache.
func NewDeliveryCache(capacity int, ttl time.Duration, clock Clock) *DeliveryCache {
	return &DeliveryCache{
		capacity: capacity,
		ttl:      ttl,
		clock:    clock,
		entries:  make(map[string]cacheEntry, capacity),
		order:    make([]string, 0, capacity),
		mu:       sync.Mutex{},
	}
}

// Claim reserves a delivery identifier until expiry.
func (cache *DeliveryCache) Claim(deliveryID string) bool {
	cache.mu.Lock()
	defer cache.mu.Unlock()

	cache.evictExpiredLocked()
	if entry, ok := cache.entries[deliveryID]; ok {
		if cache.clock().Before(entry.expiresAt) {
			return false
		}
		delete(cache.entries, deliveryID)
		cache.removeOrderLocked(deliveryID)
	}

	if len(cache.entries) >= cache.capacity {
		cache.evictOldestLocked()
	}

	now := cache.clock()
	cache.entries[deliveryID] = cacheEntry{expiresAt: now.Add(cache.ttl)}
	cache.order = append(cache.order, deliveryID)
	return true
}

// Release drops a delivery claim without waiting for expiry.
func (cache *DeliveryCache) Release(deliveryID string) {
	cache.mu.Lock()
	defer cache.mu.Unlock()

	delete(cache.entries, deliveryID)
	cache.removeOrderLocked(deliveryID)
}

func (cache *DeliveryCache) evictExpiredLocked() {
	now := cache.clock()
	remaining := cache.order[:0]
	for _, deliveryID := range cache.order {
		entry, ok := cache.entries[deliveryID]
		if !ok {
			continue
		}
		if now.Before(entry.expiresAt) {
			remaining = append(remaining, deliveryID)
			continue
		}
		delete(cache.entries, deliveryID)
	}
	cache.order = remaining
}

func (cache *DeliveryCache) evictOldestLocked() {
	if len(cache.order) == 0 {
		return
	}
	oldest := cache.order[0]
	delete(cache.entries, oldest)
	cache.order = cache.order[1:]
}

func (cache *DeliveryCache) removeOrderLocked(deliveryID string) {
	for index, existing := range cache.order {
		if existing == deliveryID {
			cache.order = append(cache.order[:index], cache.order[index+1:]...)
			return
		}
	}
}
