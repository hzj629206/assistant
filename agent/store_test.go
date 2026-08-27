package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/hzj629206/assistant/cache"
)

func TestConversationStoreRoundTrip(t *testing.T) {
	t.Parallel()

	store := NewConversationStore(cache.NewMemoryStorage())
	ctx := context.Background()
	state := ConversationState{
		Key:            "private:e_1:msg-1",
		RunnerThreadID: "codex-1",
		Muted:          true,
	}

	if err := store.PutConversation(ctx, state); err != nil {
		t.Fatalf("put conversation failed: %v", err)
	}

	got, err := store.GetConversation(ctx, state.Key)
	if err != nil {
		t.Fatalf("get conversation failed: %v", err)
	}
	if got.RunnerThreadID != "codex-1" {
		t.Fatalf("unexpected agent thread id: %s", got.RunnerThreadID)
	}
	if !got.Muted {
		t.Fatal("expected muted state to survive round trip")
	}
}

func TestConversationStoreMarksProcessed(t *testing.T) {
	t.Parallel()

	store := NewConversationStore(cache.NewMemoryStorage())
	ctx := context.Background()

	first, err := store.MarkProcessed(ctx, "evt-1")
	if err != nil {
		t.Fatalf("mark processed failed: %v", err)
	}
	second, err := store.MarkProcessed(ctx, "evt-1")
	if err != nil {
		t.Fatalf("mark processed failed: %v", err)
	}
	if !first {
		t.Fatal("expected first event mark to be new")
	}
	if second {
		t.Fatal("expected second event mark to be duplicate")
	}
}

func TestConversationStoreReturnsNotFound(t *testing.T) {
	t.Parallel()

	store := NewConversationStore(cache.NewMemoryStorage())
	_, err := store.GetConversation(context.Background(), "missing")
	if !errors.Is(err, cache.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}
