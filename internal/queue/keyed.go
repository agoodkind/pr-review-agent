package queue

import "sync"

// KeyedLocker serializes work by string key.
type KeyedLocker struct {
	mu    sync.Mutex
	locks map[string]*refCountedMutex
}

type refCountedMutex struct {
	mu     sync.Mutex
	refs   int
	holder *KeyedLocker
	key    string
}

// NewKeyedLocker creates a keyed mutex registry.
func NewKeyedLocker() *KeyedLocker {
	return &KeyedLocker{
		mu:    sync.Mutex{},
		locks: make(map[string]*refCountedMutex),
	}
}

// Lock acquires the mutex for key and returns an unlock function.
func (locker *KeyedLocker) Lock(key string) func() {
	locker.mu.Lock()
	entry, ok := locker.locks[key]
	if !ok {
		entry = &refCountedMutex{
			mu:     sync.Mutex{},
			refs:   0,
			holder: locker,
			key:    key,
		}
		locker.locks[key] = entry
	}
	entry.refs++
	locker.mu.Unlock()

	entry.mu.Lock()
	return func() {
		entry.mu.Unlock()
		entry.holder.release(entry)
	}
}

func (locker *KeyedLocker) release(entry *refCountedMutex) {
	locker.mu.Lock()
	defer locker.mu.Unlock()

	entry.refs--
	if entry.refs > 0 {
		return
	}
	delete(locker.locks, entry.key)
}
