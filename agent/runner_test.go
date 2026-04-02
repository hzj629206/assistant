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

func TestNoopRunnerEscapesSeaTalkMarkdown(t *testing.T) {
	t.Parallel()

	runner := &NoopRunner{}
	result, err := runner.RunTurn(context.Background(), TurnRequest{
		Message: InboundMessage{
			Kind: MessageKindText,
			Text: "*bold*\n_item_\n`code`\n- item\n1. first",
		},
	})
	if err != nil {
		t.Fatalf("run turn failed: %v", err)
	}
	if result.ReplyText != "\\*bold\\*\n\\_item\\_\n\\`code\\`\n\\- item\n1\\. first" {
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

func TestNoopRunnerDebugSummaryIncludesCategories(t *testing.T) {
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
	if !strings.HasPrefix(result.ReplyText, "```\n") {
		t.Fatalf("debug summary should start with a code block: %q", result.ReplyText)
	}
	if !strings.HasSuffix(result.ReplyText, "\n```") {
		t.Fatalf("debug summary should end with a code block: %q", result.ReplyText)
	}

	for _, fragment := range []string{
		`Noop debug summary`,
		`runner: noop`,
		`conversation_key: conv-1`,
		`message_kind: forwarded`,
		`history_count: 1`,
		`merged_count: 1`,
		`forwarded_count: 1`,
		`system_prompt_count: 1`,
		`tool_count: 1`,
		`available_categories: conversation, message, history, merged, prompts, tools, full`,
		`use /debug <category> to inspect one category.`,
	} {
		if !strings.Contains(result.ReplyText, fragment) {
			t.Fatalf("debug payload missing %q:\n%s", fragment, result.ReplyText)
		}
	}
}

func TestNoopRunnerDebugCategoryIncludesRegisteredContext(t *testing.T) {
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
			Text:            "/debug tools",
		},
	})
	if err != nil {
		t.Fatalf("run turn failed: %v", err)
	}
	if !strings.HasPrefix(result.ReplyText, "```\n") {
		t.Fatalf("debug payload should start with a code block: %q", result.ReplyText)
	}
	if !strings.HasSuffix(result.ReplyText, "\n```") {
		t.Fatalf("debug payload should end with a code block: %q", result.ReplyText)
	}
	for _, fragment := range []string{
		`"name": "uppercase"`,
		`"description": "Uppercase the provided text."`,
	} {
		if !strings.Contains(result.ReplyText, fragment) {
			t.Fatalf("debug payload missing %q:\n%s", fragment, result.ReplyText)
		}
	}
}

func TestNoopRunnerDebugUnknownCategoryListsChoices(t *testing.T) {
	t.Parallel()

	runner := &NoopRunner{}
	result, err := runner.RunTurn(context.Background(), TurnRequest{
		Message: InboundMessage{
			Kind: MessageKindText,
			Text: "/debug nope",
		},
	})
	if err != nil {
		t.Fatalf("run turn failed: %v", err)
	}
	if !strings.Contains(result.ReplyText, "Unknown debug category: nope") {
		t.Fatalf("unexpected debug reply: %q", result.ReplyText)
	}
	if !strings.Contains(result.ReplyText, "Available categories: conversation, message, history, merged, prompts, tools, full") {
		t.Fatalf("unexpected debug reply: %q", result.ReplyText)
	}
}

func TestFormatSeaTalkMarkdownCodeBlockEscapesFence(t *testing.T) {
	t.Parallel()

	got := formatSeaTalkMarkdownCodeBlock("before ``` after")
	want := "```\nbefore ``\\` after\n```"
	if got != want {
		t.Fatalf("unexpected code block: %q", got)
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
	if result.ReplyText != "<forwarded\\_messages>" {
		t.Fatalf("unexpected reply: %q", result.ReplyText)
	}
}
