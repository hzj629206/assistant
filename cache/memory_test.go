package cache

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestMemoryStorageGetSetDel(t *testing.T) {
	t.Parallel()

	store := NewMemoryStorage()
	t.Cleanup(func() {
		_ = store.Close()
	})
	ctx := context.Background()

	if err := store.Set(ctx, "token", []byte("value"), 0); err != nil {
		t.Fatalf("set value failed: %v", err)
	}

	value, err := store.Get(ctx, "token")
	if err != nil {
		t.Fatalf("get value failed: %v", err)
	}
	if string(value) != "value" {
		t.Fatalf("unexpected value: %q", value)
	}

	if err := store.Del(ctx, "token"); err != nil {
		t.Fatalf("delete value failed: %v", err)
	}

	_, err = store.Get(ctx, "token")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestMemoryStorageTTL(t *testing.T) {
	t.Parallel()

	store := NewMemoryStorage()
	t.Cleanup(func() {
		_ = store.Close()
	})
	ctx := context.Background()

	if err := store.Set(ctx, "token", []byte("value"), 20*time.Millisecond); err != nil {
		t.Fatalf("set value failed: %v", err)
	}

	time.Sleep(40 * time.Millisecond)

	_, err := store.Get(ctx, "token")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestMemoryStorageAdd(t *testing.T) {
	t.Parallel()

	store := NewMemoryStorage()
	t.Cleanup(func() {
		_ = store.Close()
	})
	ctx := context.Background()

	if err := store.Add(ctx, "token", []byte("value"), 0); err != nil {
		t.Fatalf("add value failed: %v", err)
	}

	value, err := store.Get(ctx, "token")
	if err != nil {
		t.Fatalf("get value failed: %v", err)
	}
	if string(value) != "value" {
		t.Fatalf("unexpected value: %q", value)
	}

	err = store.Add(ctx, "token", []byte("other"), 0)
	if !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("expected ErrAlreadyExists, got %v", err)
	}
}

func TestMemoryStorageAddReplacesExpiredItem(t *testing.T) {
	t.Parallel()

	store := NewMemoryStorage()
	t.Cleanup(func() {
		_ = store.Close()
	})
	ctx := context.Background()

	if err := store.Set(ctx, "token", []byte("old"), 20*time.Millisecond); err != nil {
		t.Fatalf("set value failed: %v", err)
	}

	time.Sleep(40 * time.Millisecond)

	if err := store.Add(ctx, "token", []byte("new"), 0); err != nil {
		t.Fatalf("add value after expiration failed: %v", err)
	}

	value, err := store.Get(ctx, "token")
	if err != nil {
		t.Fatalf("get value failed: %v", err)
	}
	if string(value) != "new" {
		t.Fatalf("unexpected value: %q", value)
	}
}

func TestMemoryStorageClonesValues(t *testing.T) {
	t.Parallel()

	store := NewMemoryStorage()
	t.Cleanup(func() {
		_ = store.Close()
	})
	ctx := context.Background()

	value := []byte("value")
	if err := store.Set(ctx, "token", value, 0); err != nil {
		t.Fatalf("set value failed: %v", err)
	}
	value[0] = 'V'

	stored, err := store.Get(ctx, "token")
	if err != nil {
		t.Fatalf("get value failed: %v", err)
	}
	if string(stored) != "value" {
		t.Fatalf("unexpected value after external mutation: %q", stored)
	}

	stored[1] = 'A'

	again, err := store.Get(ctx, "token")
	if err != nil {
		t.Fatalf("get value failed: %v", err)
	}
	if string(again) != "value" {
		t.Fatalf("unexpected value after returned slice mutation: %q", again)
	}
}

func TestMemoryStorageCleanupRemovesExpiredEntries(t *testing.T) {
	t.Parallel()

	store := NewMemoryStorageWithCleanupInterval(10 * time.Millisecond)
	t.Cleanup(func() {
		_ = store.Close()
	})

	ctx := context.Background()
	if err := store.Set(ctx, "token", []byte("value"), 15*time.Millisecond); err != nil {
		t.Fatalf("set value failed: %v", err)
	}

	time.Sleep(50 * time.Millisecond)

	store.store.mu.RLock()
	_, ok := store.store.items["token"]
	store.store.mu.RUnlock()
	if ok {
		t.Fatal("expected expired key to be removed by cleanup worker")
	}
}
