package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/hzj629206/assistant/cache"
)

const (
	defaultConversationTTL = 30 * 24 * time.Hour
	defaultProcessedTTL    = 7 * 24 * time.Hour
)

// ConversationStore persists dispatcher state in the shared cache backend.
type ConversationStore struct {
	storage         cache.Storage
	conversationTTL time.Duration
	processedTTL    time.Duration
}

// NewConversationStore builds a conversation store backed by the provided cache storage.
func NewConversationStore(storage cache.Storage) *ConversationStore {
	if storage == nil {
		storage = cache.Global()
	}

	return &ConversationStore{
		storage:         storage,
		conversationTTL: defaultConversationTTL,
		processedTTL:    defaultProcessedTTL,
	}
}

// GetConversation loads one conversation state by key.
func (s *ConversationStore) GetConversation(ctx context.Context, key string) (ConversationState, error) {
	data, err := s.storage.Get(ctx, conversationCacheKey(key))
	if err != nil {
		return ConversationState{}, err
	}

	var state ConversationState
	if err := json.Unmarshal(data, &state); err != nil {
		return ConversationState{}, fmt.Errorf("get conversation failed: decode state: %w", err)
	}

	return state, nil
}

// PutConversation stores the current conversation state.
func (s *ConversationStore) PutConversation(ctx context.Context, state ConversationState) error {
	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("put conversation failed: encode state: %w", err)
	}

	return s.storage.Set(ctx, conversationCacheKey(state.Key), data, s.conversationTTL)
}

// MarkProcessed records one source event id and reports whether it was new.
func (s *ConversationStore) MarkProcessed(ctx context.Context, eventID string) (bool, error) {
	if eventID == "" {
		return true, nil
	}

	err := s.storage.Add(ctx, processedEventCacheKey(eventID), []byte("1"), s.processedTTL)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, cache.ErrAlreadyExists) {
		return false, nil
	}

	return false, fmt.Errorf("mark processed event failed: %w", err)
}

func conversationCacheKey(key string) string {
	return "agent:conversation:" + key
}

func processedEventCacheKey(eventID string) string {
	return "agent:event:" + eventID
}
