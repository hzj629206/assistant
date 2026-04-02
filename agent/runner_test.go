package agent

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestNoopRunnerEchoesText(t *testing.T) {
	t.Parallel()

	runner := &NoopRunner{}
	result, err := runner.RunTurn(context.Background(), TurnRequest{
		Message: InboundMessage{
			Kind: MessageKindText,
			Text: "hello",
		},
	})
	if err != nil {
		t.Fatalf("run turn failed: %v", err)
	}
	if result.ReplyText != "hello" {
		t.Fatalf("unexpected reply: %q", result.ReplyText)
	}
}

func TestNoopRunnerEchoesTextAfterSpecifiedDelay(t *testing.T) {
	t.Parallel()

	runner := &NoopRunner{}
	startedAt := time.Now()
	result, err := runner.RunTurn(context.Background(), TurnRequest{
		Message: InboundMessage{
			Kind: MessageKindText,
			Text: "/delay 20ms hello",
		},
	})
	if err != nil {
		t.Fatalf("run turn failed: %v", err)
	}
	if result.ReplyText != "hello" {
		t.Fatalf("unexpected reply: %q", result.ReplyText)
	}
	if elapsed := time.Since(startedAt); elapsed < 20*time.Millisecond {
		t.Fatalf("delay was not applied: elapsed=%s", elapsed)
	}
}

func TestNoopRunnerDebugIncludesRegisteredContext(t *testing.T) {
	t.Parallel()

	runner := &NoopRunner{}
	runner.RegisterSystemPrompt("Global system prompt.")
	runner.RegisterTools(uppercaseTool{})

	result, err := runner.RunTurn(context.Background(), TurnRequest{
		Conversation: ConversationState{
			Key:           "conv-1",
			CodexThreadID: "thread-1",
			LastEventID:   "evt-1",
		},
		Message: InboundMessage{
			ID:              "msg-1",
			ConversationKey: "conv-1",
			Kind:            MessageKindForwarded,
			Text:            "/debug",
			ForwardedMessages: []ReferencedMessage{
				{
					Kind:   MessageKindText,
					Sender: "alice@example.com",
					Text:   "forwarded text",
				},
			},
			QuotedMessage: &ReferencedMessage{
				Kind: MessageKindText,
				Text: "quoted text",
			},
			initialContext: "[1] demo context",
			historicalMessages: []InboundMessage{
				{
					ID:   "history-1",
					Kind: MessageKindText,
					Text: "earlier message",
				},
			},
			mergedMessages: []InboundMessage{
				{
					ID:   "msg-0",
					Kind: MessageKindText,
					Text: "/debug",
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("run turn failed: %v", err)
	}

	for _, fragment := range []string{
		`"runner": "noop"`,
		`"system_prompts": [`,
		`"Global system prompt."`,
		`"tools": [`,
		`"name": "uppercase"`,
		`"description": "Uppercase the provided text."`,
		`"conversation": {`,
		`"key": "conv-1"`,
		`"codex_thread_id": "thread-1"`,
		`"message": {`,
		`"id": "msg-1"`,
		`"conversation_key": "conv-1"`,
		`"text": "/debug"`,
		`"initial_context": "[1] demo context"`,
		`"quoted_message": {`,
		`"forwarded_messages": [`,
		`"historical_messages": [`,
		`"merged_messages": [`,
		`"earlier message"`,
		`"forwarded text"`,
		`"quoted text"`,
	} {
		if !strings.Contains(result.ReplyText, fragment) {
			t.Fatalf("debug payload missing %q:\n%s", fragment, result.ReplyText)
		}
	}
}

func TestNoopRunnerEchoesLastMergedText(t *testing.T) {
	t.Parallel()

	runner := &NoopRunner{}
	result, err := runner.RunTurn(context.Background(), TurnRequest{
		Message: InboundMessage{
			Kind: MessageKindText,
			mergedMessages: []InboundMessage{
				{
					ID:   "msg-1",
					Kind: MessageKindImage,
				},
				{
					ID:   "msg-2",
					Kind: MessageKindText,
					Text: "latest text",
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("run turn failed: %v", err)
	}
	if result.ReplyText != "latest text" {
		t.Fatalf("unexpected reply: %q", result.ReplyText)
	}
}

func TestNoopRunnerEchoesLastMergedTextAfterSpecifiedDelay(t *testing.T) {
	t.Parallel()

	runner := &NoopRunner{}
	startedAt := time.Now()
	result, err := runner.RunTurn(context.Background(), TurnRequest{
		Message: InboundMessage{
			Kind: MessageKindText,
			mergedMessages: []InboundMessage{
				{
					ID:   "msg-1",
					Kind: MessageKindText,
					Text: "first text",
				},
				{
					ID:   "msg-2",
					Kind: MessageKindText,
					Text: "/delay 20ms latest text",
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("run turn failed: %v", err)
	}
	if result.ReplyText != "latest text" {
		t.Fatalf("unexpected reply: %q", result.ReplyText)
	}
	if elapsed := time.Since(startedAt); elapsed < 20*time.Millisecond {
		t.Fatalf("delay was not applied: elapsed=%s", elapsed)
	}
}

func TestNoopRunnerReturnsForwardedPlaceholder(t *testing.T) {
	t.Parallel()

	runner := &NoopRunner{}
	result, err := runner.RunTurn(context.Background(), TurnRequest{
		Message: InboundMessage{
			Kind: MessageKindForwarded,
			ForwardedMessages: []ReferencedMessage{
				{
					Kind: MessageKindText,
					Text: "forwarded hello",
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("run turn failed: %v", err)
	}
	if result.ReplyText != "<forwarded_messages>" {
		t.Fatalf("unexpected reply: %q", result.ReplyText)
	}
}
