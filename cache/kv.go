package cache

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ErrNotFound indicates the key does not exist in storage.
var ErrNotFound = errors.New("cache: key not found")

// ErrAlreadyExists indicates the key already exists in storage.
var ErrAlreadyExists = errors.New("cache: key already exists")

// Storage defines the key-value storage contract used by stateful components.
// Implementations may store data in memory, Redis, or any other backend.
type Storage interface {
	Get(ctx context.Context, key string) ([]byte, error)
	Add(ctx context.Context, key string, value []byte, ttl time.Duration) error
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
	Del(ctx context.Context, key string) error
}

var (
	globalMu      sync.RWMutex
	globalStorage Storage = NewMemoryStorage()
)

// Global returns the shared key-value storage instance used by stateful components.
func Global() Storage {
	globalMu.RLock()
	defer globalMu.RUnlock()

	return globalStorage
}

// SetGlobal replaces the shared key-value storage instance.
// Passing nil resets the storage backend to the default in-memory implementation.
func SetGlobal(store Storage) {
	globalMu.Lock()
	defer globalMu.Unlock()

	if store == nil {
		store = NewMemoryStorage()
	}
	if store == globalStorage {
		return
	}

	closeStorage(globalStorage)
	globalStorage = store
}

func closeStorage(store Storage) {
	type closer interface {
		Close() error
	}

	if store == nil {
		return
	}

	if c, ok := store.(closer); ok {
		_ = c.Close()
	}
}
