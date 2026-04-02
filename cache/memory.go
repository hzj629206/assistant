package cache

import (
	"context"
	"runtime"
	"sync"
	"time"
)

const defaultCleanupInterval = time.Minute

type memoryItem struct {
	Object     []byte
	Expiration int64
}

func (item memoryItem) Expired() bool {
	if item.Expiration == 0 {
		return false
	}

	return time.Now().UnixNano() > item.Expiration
}

type janitor struct {
	interval time.Duration
	stopCh   chan struct{}
	doneCh   chan struct{}
	stopOnce sync.Once
}

type memoryStore struct {
	items   map[string]memoryItem
	mu      sync.RWMutex
	janitor *janitor
}

// MemoryStorage is an in-memory implementation of Storage.
type MemoryStorage struct {
	store      *memoryStore
	cleanup    runtime.Cleanup
	hasCleanup bool
}

// NewMemoryStorage creates a new in-memory cache storage.
func NewMemoryStorage() *MemoryStorage {
	return NewMemoryStorageWithCleanupInterval(defaultCleanupInterval)
}

// NewMemoryStorageWithCleanupInterval creates a storage with a custom cleanup interval.
// A non-positive interval disables background cleanup.
func NewMemoryStorageWithCleanupInterval(cleanupInterval time.Duration) *MemoryStorage {
	store := &memoryStore{
		items: make(map[string]memoryItem),
	}
	storage := &MemoryStorage{
		store: store,
	}

	if cleanupInterval > 0 {
		runJanitor(store, cleanupInterval)
		storage.cleanup = runtime.AddCleanup(storage, stopJanitor, store.janitor)
		storage.hasCleanup = true
	}

	return storage
}

// Get returns a copy of the stored value for the given key.
func (s *MemoryStorage) Get(_ context.Context, key string) ([]byte, error) {
	s.store.mu.RLock()
	item, found := s.store.items[key]
	if !found {
		s.store.mu.RUnlock()
		return nil, ErrNotFound
	}
	if item.Expired() {
		s.store.mu.RUnlock()
		return nil, ErrNotFound
	}
	s.store.mu.RUnlock()

	return cloneBytes(item.Object), nil
}

// Add stores a value only when the key does not exist or has already expired.
func (s *MemoryStorage) Add(_ context.Context, key string, value []byte, ttl time.Duration) error {
	item := newMemoryItem(value, ttl)

	s.store.mu.Lock()
	existing, found := s.store.items[key]
	if found && !existing.Expired() {
		s.store.mu.Unlock()
		return ErrAlreadyExists
	}
	s.store.items[key] = item
	s.store.mu.Unlock()
	return nil
}

// Set stores a copy of the value for the given key.
func (s *MemoryStorage) Set(_ context.Context, key string, value []byte, ttl time.Duration) error {
	s.store.mu.Lock()
	s.store.items[key] = newMemoryItem(value, ttl)
	s.store.mu.Unlock()
	return nil
}

// Del removes the key from storage.
func (s *MemoryStorage) Del(_ context.Context, key string) error {
	s.store.mu.Lock()
	delete(s.store.items, key)
	s.store.mu.Unlock()
	return nil
}

// Close stops the background cleanup worker.
func (s *MemoryStorage) Close() error {
	if s.hasCleanup {
		s.cleanup.Stop()
	}
	stopJanitor(s.store.janitor)
	return nil
}

func (s *memoryStore) deleteExpired() {
	s.mu.Lock()
	for key, item := range s.items {
		if item.Expired() {
			delete(s.items, key)
		}
	}
	s.mu.Unlock()
}

func runJanitor(store *memoryStore, cleanupInterval time.Duration) {
	j := &janitor{
		interval: cleanupInterval,
		stopCh:   make(chan struct{}),
		doneCh:   make(chan struct{}),
	}
	store.janitor = j

	go j.run(store)
}

func stopJanitor(j *janitor) {
	if j == nil {
		return
	}

	j.stopOnce.Do(func() {
		close(j.stopCh)
		<-j.doneCh
	})
}

func (j *janitor) run(store *memoryStore) {
	ticker := time.NewTicker(j.interval)
	defer func() {
		ticker.Stop()
		close(j.doneCh)
	}()

	for {
		select {
		case <-ticker.C:
			store.deleteExpired()
		case <-j.stopCh:
			return
		}
	}
}

func cloneBytes(value []byte) []byte {
	if value == nil {
		return nil
	}

	cloned := make([]byte, len(value))
	copy(cloned, value)
	return cloned
}

func newMemoryItem(value []byte, ttl time.Duration) memoryItem {
	var expiration int64
	if ttl > 0 {
		expiration = time.Now().Add(ttl).UnixNano()
	}

	return memoryItem{
		Object:     cloneBytes(value),
		Expiration: expiration,
	}
}
