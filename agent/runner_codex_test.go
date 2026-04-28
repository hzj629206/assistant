package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/godeps/codex-sdk-go"
)

type testRunnerResponder struct {
	typingCalls int
}

type unmarshalableTool struct{}

type fakeCodexThread struct {
	id            string
	turns         []codex.Turn
	inputs        []codex.Input
	options       []codex.TurnOptions
	runStreamedFn func(codex.Input, codex.TurnOptions) (*codex.StreamedTurn, error)
}

type uppercaseTool struct{}

type snapshotTool struct {
	name string
}

func (unmarshalableTool) Name() string {
	return "bad"
}

func (t *fakeCodexThread) ID() string {
	return t.id
}

func (t *fakeCodexThread) RunStreamed(input codex.Input, options codex.TurnOptions) (*codex.StreamedTurn, error) {
	t.inputs = append(t.inputs, input)
	t.options = append(t.options, options)
	if t.runStreamedFn != nil {
		return t.runStreamedFn(input, options)
	}
	if len(t.turns) == 0 {
		return nil, errors.New("unexpected turn")
	}

	turn := t.turns[0]
	t.turns = t.turns[1:]

	events := make([]codex.ThreadEvent, 0, len(turn.Items)+2)
	if len(turn.Items) == 0 && turn.FinalResponse != "" {
		turn.Items = append(turn.Items, &codex.AgentMessageItem{
			ID:   "agent-message",
			Type: "agent_message",
			Text: turn.FinalResponse,
		})
	}
	for _, item := range turn.Items {
		events = append(events, codex.ThreadEvent{
			Type: "item.completed",
			Item: item,
		})
	}
	events = append(events, codex.ThreadEvent{
		Type:  "turn.completed",
		Usage: turn.Usage,
	})

	return &codex.StreamedTurn{
		Events: closedEvents(events...),
		Done:   closedDone(nil),
	}, nil
}

func (uppercaseTool) Name() string {
	return "uppercase"
}

func (t snapshotTool) Name() string {
	return t.name
}

func (snapshotTool) Description() string {
	return "A test tool."
}

func (snapshotTool) InputSchema() any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
	}
}

func (snapshotTool) OutputSchema() any {
	return nil
}

func (snapshotTool) Call(context.Context, json.RawMessage) (any, error) {
	return map[string]string{"ok": "ok"}, nil
}

func (uppercaseTool) Description() string {
	return "Uppercase the provided text."
}

func (uppercaseTool) InputSchema() any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"text": map[string]any{"type": "string"},
		},
		"required":             []any{"text"},
		"additionalProperties": false,
	}
}

func (uppercaseTool) OutputSchema() any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"text": map[string]any{"type": "string"},
		},
		"required":             []any{"text"},
		"additionalProperties": false,
	}
}

func (uppercaseTool) Call(_ context.Context, input json.RawMessage) (any, error) {
	var payload struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(input, &payload); err != nil {
		return nil, err
	}

	return map[string]string{"text": strings.ToUpper(payload.Text)}, nil
}

func (unmarshalableTool) Description() string {
	return "Returns an unsupported schema payload."
}

func (unmarshalableTool) InputSchema() any {
	return func() {}
}

func (unmarshalableTool) OutputSchema() any {
	return nil
}

func (unmarshalableTool) Call(context.Context, json.RawMessage) (any, error) {
	return nil, nil
}

func (r *testRunnerResponder) SendText(context.Context, string) error {
	return nil
}

func (r *testRunnerResponder) SetTyping(context.Context) error {
	r.typingCalls++
	return nil
}

func (r *testRunnerResponder) Cleanup(context.Context) error {
	return nil
}

func testLocalRFC3339() string {
	return time.Unix(1000, 0).In(time.Local).Format(time.RFC3339) //nolint:gosmopolitan // Tests intentionally follow the local machine timezone behavior.
}

func testLocalRFC3339At(timestamp int64) string {
	return time.Unix(timestamp, 0).In(time.Local).Format(time.RFC3339) //nolint:gosmopolitan // Tests intentionally follow the local machine timezone behavior.
}

func TestBuildTurnPromptIncludesQuotedTextContext(t *testing.T) {
	t.Parallel()

	prompt, imageRefs := buildTurnPrompt(InboundMessage{
		Kind:       MessageKindText,
		Sender:     "unknown",
		SentAtUnix: 1000,
		Text:       "current message",
		QuotedMessage: &ReferencedMessage{
			Kind: MessageKindText,
			Text: "quoted message",
		},
	})

	expected := "Current message context:\n- time: " + testLocalRFC3339() + "\n- sender: `unknown`\n\nQuoted message:\nType: text\nContent: quoted message\n\ncurrent message"
	if prompt != expected {
		t.Fatalf("unexpected prompt:\n%s", prompt)
	}
	if len(imageRefs) != 0 {
		t.Fatalf("unexpected image refs: %+v", imageRefs)
	}
}

func TestBuildTurnPromptIncludesInitialContext(t *testing.T) {
	t.Parallel()

	prompt, imageRefs := buildTurnPrompt(InboundMessage{
		initialContext: "[1] alice@example.com: earlier message",
		Kind:           MessageKindText,
		Sender:         "alice@example.com",
		SentAtUnix:     1000,
		Text:           "current message",
	})

	expected := "Conversation context:\n[1] alice@example.com: earlier message\n\nCurrent message context:\n- time: " + testLocalRFC3339() + "\n- sender: `alice@example.com`\n\ncurrent message"
	if prompt != expected {
		t.Fatalf("unexpected prompt:\n%s", prompt)
	}
	if len(imageRefs) != 0 {
		t.Fatalf("unexpected image refs: %+v", imageRefs)
	}
}

func TestBuildTurnPromptIncludesSingleMessageSenderMentionHint(t *testing.T) {
	t.Parallel()

	prompt, imageRefs := buildTurnPrompt(InboundMessage{
		Kind:              MessageKindText,
		Sender:            "alice@sea.com",
		SenderMentionHint: `<mention-tag target="seatalk://user?id=seatalk-user-1"/>`,
		SentAtUnix:        1000,
		Text:              "current message",
	})

	expected := "Current message context:\n- time: " + testLocalRFC3339() + "\n- sender: `alice@sea.com`\n- sender mention hint: `<mention-tag target=\"seatalk://user?id=seatalk-user-1\"/>`\n\ncurrent message"
	if prompt != expected {
		t.Fatalf("unexpected prompt:\n%s", prompt)
	}
	if len(imageRefs) != 0 {
		t.Fatalf("unexpected image refs: %+v", imageRefs)
	}
}

func TestBuildTurnPromptIncludesMessageTags(t *testing.T) {
	t.Parallel()

	prompt, imageRefs := buildTurnPrompt(InboundMessage{
		Kind:        MessageKindText,
		Sender:      "alice@sea.com",
		SentAtUnix:  1000,
		MessageTags: []string{"group_mentioned_message"},
		Text:        "current message",
	})

	expected := "Current message context:\n- time: " + testLocalRFC3339() + "\n- sender: `alice@sea.com`\n- tags:\n  - group_mentioned_message\n\ncurrent message"
	if prompt != expected {
		t.Fatalf("unexpected prompt:\n%s", prompt)
	}
	if len(imageRefs) != 0 {
		t.Fatalf("unexpected image refs: %+v", imageRefs)
	}
}

func TestBuildTurnPromptIncludesImageAttachments(t *testing.T) {
	t.Parallel()

	prompt, imageRefs := buildTurnPrompt(InboundMessage{
		Kind:       MessageKindImage,
		Sender:     "unknown",
		SentAtUnix: 1000,
		ImagePath:  "/tmp/current.png",
		QuotedMessage: &ReferencedMessage{
			Kind:      MessageKindImage,
			ImagePath: "/tmp/quoted.png",
		},
	})

	expected := "Current message context:\n- time: " + testLocalRFC3339() + "\n- sender: `unknown`\n\nQuoted message:\nType: image\nAttachment: quoted image\n\nUser sent an image.\nAttachment: current image"
	if prompt != expected {
		t.Fatalf("unexpected prompt:\n%s", prompt)
	}
	if len(imageRefs) != 2 {
		t.Fatalf("unexpected image refs: %+v", imageRefs)
	}
}

func TestBuildTurnPromptIncludesUnsupportedAttachmentGuidance(t *testing.T) {
	t.Parallel()

	prompt, imageRefs := buildTurnPrompt(InboundMessage{
		Kind:       MessageKindInteractiveCard,
		Sender:     "unknown",
		SentAtUnix: 1000,
	})

	expected := "Current message context:\n- time: " + testLocalRFC3339() + "\n- sender: `unknown`\n\nUser sent an interactive message card that is not currently parsed.\nDo not reply solely because of this placeholder."
	if prompt != expected {
		t.Fatalf("unexpected prompt:\n%s", prompt)
	}
	if len(imageRefs) != 0 {
		t.Fatalf("unexpected image refs: %+v", imageRefs)
	}
}

func TestBuildTurnPromptIncludesInteractiveMessageSummary(t *testing.T) {
	t.Parallel()

	prompt, imageRefs := buildTurnPrompt(InboundMessage{
		Kind:       MessageKindInteractiveCard,
		Sender:     "unknown",
		SentAtUnix: 1000,
		Text:       `interactive card; title="Deploy Result"; buttons=[Retry, Logs (https://example.com/logs)]`,
	})

	expected := "Current message context:\n- time: " + testLocalRFC3339() + "\n- sender: `unknown`\n\nUser sent an interactive message card.\nContent: interactive card; title=\"Deploy Result\"; buttons=[Retry, Logs (https://example.com/logs)]"
	if prompt != expected {
		t.Fatalf("unexpected prompt:\n%s", prompt)
	}
	if len(imageRefs) != 0 {
		t.Fatalf("unexpected image refs: %+v", imageRefs)
	}
}

func TestBuildTurnPromptIncludesInteractiveMessageTextAndImages(t *testing.T) {
	t.Parallel()

	prompt, imageRefs := buildTurnPrompt(InboundMessage{
		Kind:       MessageKindInteractiveCard,
		Sender:     "unknown",
		SentAtUnix: 1000,
		Text:       `interactive card; title="Deploy Result"; image_urls=[https://example.com/image-1.png, https://example.com/image-2.png]`,
		ImagePaths: []string{"/tmp/current-1.png", "/tmp/current-2.png"},
		QuotedMessage: &ReferencedMessage{
			Kind:       MessageKindInteractiveCard,
			Text:       `interactive card; title="Earlier Result"; image_urls=[https://example.com/quoted.png]`,
			ImagePaths: []string{"/tmp/quoted.png"},
		},
	})

	expected := "Current message context:\n- time: " + testLocalRFC3339() + "\n- sender: `unknown`\n\nQuoted message:\nType: interactive_card\nContent: interactive card; title=\"Earlier Result\"; image_urls=[https://example.com/quoted.png]\nAttachment: quoted image\n\nUser sent an interactive message card.\nContent: interactive card; title=\"Deploy Result\"; image_urls=[https://example.com/image-1.png, https://example.com/image-2.png]\nAttachments: current images (2)"
	if prompt != expected {
		t.Fatalf("unexpected prompt:\n%s", prompt)
	}
	if len(imageRefs) != 3 {
		t.Fatalf("unexpected image refs: %+v", imageRefs)
	}
}

func TestBuildTurnPromptIncludesMixedMessageTextImageAndFileContext(t *testing.T) {
	t.Parallel()

	prompt, imageRefs := buildTurnPrompt(InboundMessage{
		Kind:       MessageKindMixed,
		Sender:     "unknown",
		SentAtUnix: 1000,
		Text:       "mixed body",
		ImagePaths: []string{"/tmp/current.png"},
		FilePaths:  []string{"/tmp/current.pdf"},
		VideoPaths: []string{"/tmp/current.mp4"},
	})

	expected := "Current message context:\n- time: " + testLocalRFC3339() + "\n- sender: `unknown`\n\nUser sent a mixed message.\nContent: mixed body\nAttachment: current image\nAttachment: current file\nLocal path: /tmp/current.pdf\nPath validity: local file paths are temporary and only valid for this turn.\nAttachment: current video\nLocal path: /tmp/current.mp4\nPath validity: local file paths are temporary and only valid for this turn."
	if prompt != expected {
		t.Fatalf("unexpected prompt:\n%s", prompt)
	}
	if len(imageRefs) != 1 || imageRefs[0] != "/tmp/current.png" {
		t.Fatalf("unexpected image refs: %+v", imageRefs)
	}
}

func TestBuildTurnPromptIncludesStructuredForwardedMessages(t *testing.T) {
	t.Parallel()

	prompt, imageRefs := buildTurnPrompt(InboundMessage{
		Kind:       MessageKindForwarded,
		Sender:     "unknown",
		SentAtUnix: 1000,
		ForwardedMessages: []ReferencedMessage{
			{
				Kind:       MessageKindText,
				Sender:     "alice@example.com",
				SentAtUnix: 1001,
				Text:       "forwarded hello",
			},
			{
				Kind:       MessageKindImage,
				Sender:     "bob@example.com",
				SentAtUnix: 1002,
				ImagePath:  "/tmp/forwarded.png",
			},
		},
	})

	expected := "Current message context:\n- time: " + testLocalRFC3339() + "\n- sender: `unknown`\n\nUser sent combined forwarded chat history.\n\nForwarded message 1:\n- time: " + testLocalRFC3339At(1001) + "\n- sender: alice@example.com\nContent: forwarded hello\n\nForwarded message 2:\n- time: " + testLocalRFC3339At(1002) + "\n- sender: bob@example.com\nType: image\nAttachment: forwarded image"
	if prompt != expected {
		t.Fatalf("unexpected prompt:\n%s", prompt)
	}
	if len(imageRefs) != 1 || imageRefs[0] != "/tmp/forwarded.png" {
		t.Fatalf("unexpected image refs: %+v", imageRefs)
	}
}

func TestFormatReferencedMessageIncludesStructuredForwardedMessages(t *testing.T) {
	t.Parallel()

	prompt, imageRefs := formatReferencedMessage(ReferencedMessage{
		Kind: MessageKindForwarded,
		ForwardedMessages: []ReferencedMessage{
			{
				Kind:       MessageKindText,
				Sender:     "alice@example.com",
				SentAtUnix: 1000,
				Text:       "forwarded hello",
			},
		},
	})

	expected := "Quoted message:\nType: combined_forwarded_chat_history\n\nForwarded message 1:\n- time: " + testLocalRFC3339() + "\n- sender: alice@example.com\nContent: forwarded hello"
	if prompt != expected {
		t.Fatalf("unexpected prompt:\n%s", prompt)
	}
	if len(imageRefs) != 0 {
		t.Fatalf("unexpected image refs: %+v", imageRefs)
	}
}

func TestBuildTurnPromptIncludesEmptyForwardedMessagePlaceholder(t *testing.T) {
	t.Parallel()

	prompt, imageRefs := buildTurnPrompt(InboundMessage{
		Kind:       MessageKindForwarded,
		Sender:     "unknown",
		SentAtUnix: 1000,
	})

	expected := "Current message context:\n- time: " + testLocalRFC3339() + "\n- sender: `unknown`\n\nUser sent combined forwarded chat history."
	if prompt != expected {
		t.Fatalf("unexpected prompt:\n%s", prompt)
	}
	if len(imageRefs) != 0 {
		t.Fatalf("unexpected image refs: %+v", imageRefs)
	}
}

func TestFormatReferencedMessageIncludesEmptyForwardedMessagePlaceholder(t *testing.T) {
	t.Parallel()

	prompt, imageRefs := formatReferencedMessage(ReferencedMessage{
		Kind: MessageKindForwarded,
	})

	expected := "Quoted message:\nType: combined_forwarded_chat_history"
	if prompt != expected {
		t.Fatalf("unexpected prompt:\n%s", prompt)
	}
	if len(imageRefs) != 0 {
		t.Fatalf("unexpected image refs: %+v", imageRefs)
	}
}

func TestBuildTurnPromptFallsBackToUnknownCurrentMessageContext(t *testing.T) {
	t.Parallel()

	prompt, imageRefs := buildTurnPrompt(InboundMessage{
		Kind: MessageKindText,
		Text: "current message",
	})

	expected := "Current message context:\n- time: unknown\n- sender: `unknown`\n\ncurrent message"
	if prompt != expected {
		t.Fatalf("unexpected prompt:\n%s", prompt)
	}
	if len(imageRefs) != 0 {
		t.Fatalf("unexpected image refs: %+v", imageRefs)
	}
}

func TestBuildTurnPromptIncludesMergedMessagesInOrder(t *testing.T) {
	t.Parallel()

	prompt, imageRefs := buildTurnPrompt(InboundMessage{
		initialContext: "Earlier thread context",
		mergedMessages: []InboundMessage{
			{
				ID:         "evt-1",
				Kind:       MessageKindText,
				Sender:     "alice@sea.com",
				SentAtUnix: 1000,
				Text:       "first message",
			},
			{
				ID:         "evt-2",
				Kind:       MessageKindInteractiveCard,
				Sender:     "bob@sea.com",
				SentAtUnix: 1001,
				Text:       `interactive card; title="Approval Needed"; buttons=[Approve, Reject]`,
			},
		},
	})

	expected := "Conversation context:\nEarlier thread context\n\nMultiple new messages arrived while the assistant was busy. Process them together in order.\n\n\nMessage 1:\nCurrent message context:\n- time: " + testLocalRFC3339() + "\n- sender: `alice@sea.com`\n\nfirst message\n\nMessage 2:\nCurrent message context:\n- time: " + testLocalRFC3339At(1001) + "\n- sender: `bob@sea.com`\n\nUser sent an interactive message card.\nContent: interactive card; title=\"Approval Needed\"; buttons=[Approve, Reject]"
	if prompt != expected {
		t.Fatalf("unexpected prompt:\n%s", prompt)
	}
	if len(imageRefs) != 0 {
		t.Fatalf("unexpected image refs: %+v", imageRefs)
	}
}

func TestBuildTurnPromptIncludesHistoricalMessagesBeforeCurrentMessage(t *testing.T) {
	t.Parallel()

	prompt, imageRefs := buildTurnPrompt(InboundMessage{
		initialContext: "Group profile:\n- name: Demo Group",
		historicalMessages: []InboundMessage{
			{
				ID:         "msg-history-1",
				Kind:       MessageKindText,
				Sender:     "alice@example.com",
				SentAtUnix: 1000,
				Text:       "earlier text",
			},
			{
				ID:         "msg-history-2",
				Kind:       MessageKindImage,
				Sender:     "bob@example.com",
				SentAtUnix: 1001,
				ImagePath:  "/tmp/history.png",
			},
		},
		mergedMessages: []InboundMessage{
			{
				ID:                "evt-current-1",
				Kind:              MessageKindText,
				Sender:            "carol@sea.com",
				SentAtUnix:        1002,
				Text:              "current message",
				SenderMentionHint: `<mention-tag target="seatalk://user?email=carol@sea.com"/>`,
			},
		},
	})

	expected := "Conversation context:\nGroup profile:\n- name: Demo Group\n\nEarlier messages from the current conversation are included below for context.\n\nHistory message 1:\nCurrent message context:\n- time: " + testLocalRFC3339() + "\n- sender: `alice@example.com`\n\nearlier text\n\nHistory message 2:\nCurrent message context:\n- time: " + testLocalRFC3339At(1001) + "\n- sender: `bob@example.com`\n\nUser sent an image.\nAttachment: current image\n\nCurrent message context:\n- time: " + testLocalRFC3339At(1002) + "\n- sender: `carol@sea.com`\n- sender mention hint: `<mention-tag target=\"seatalk://user?email=carol@sea.com\"/>`\n\ncurrent message"
	if prompt != expected {
		t.Fatalf("unexpected prompt:\n%s", prompt)
	}
	if len(imageRefs) != 1 || imageRefs[0] != "/tmp/history.png" {
		t.Fatalf("unexpected image refs: %+v", imageRefs)
	}
}

func TestBuildTurnPromptIncludesTopLevelCurrentMessageWhenHistoryExists(t *testing.T) {
	t.Parallel()

	prompt, imageRefs := buildTurnPrompt(InboundMessage{
		initialContext: "Group profile:\n- name: Demo Group",
		historicalMessages: []InboundMessage{
			{
				ID:         "msg-history-1",
				Kind:       MessageKindText,
				Sender:     "alice@example.com",
				SentAtUnix: 1000,
				Text:       "earlier text",
			},
		},
		Kind:              MessageKindText,
		Sender:            "carol@sea.com",
		SentAtUnix:        1002,
		Text:              "current message",
		SenderMentionHint: `<mention-tag target="seatalk://user?email=carol@sea.com"/>`,
	})

	expected := "Conversation context:\nGroup profile:\n- name: Demo Group\n\nEarlier messages from the current conversation are included below for context.\n\nHistory message 1:\nCurrent message context:\n- time: " + testLocalRFC3339() + "\n- sender: `alice@example.com`\n\nearlier text\n\nCurrent message context:\n- time: " + testLocalRFC3339At(1002) + "\n- sender: `carol@sea.com`\n- sender mention hint: `<mention-tag target=\"seatalk://user?email=carol@sea.com\"/>`\n\ncurrent message"
	if prompt != expected {
		t.Fatalf("unexpected prompt:\n%s", prompt)
	}
	if len(imageRefs) != 0 {
		t.Fatalf("unexpected image refs: %+v", imageRefs)
	}
}

func TestBuildTurnPromptIncludesHistoricalMessagesWithoutMergedMessages(t *testing.T) {
	t.Parallel()

	prompt, imageRefs := buildTurnPrompt(InboundMessage{
		initialContext: "Group profile:\n- name: Demo Group",
		historicalMessages: []InboundMessage{
			{
				ID:         "msg-history-1",
				Kind:       MessageKindText,
				Sender:     "alice@example.com",
				SentAtUnix: 1000,
				Text:       "earlier text",
			},
		},
		Kind:              MessageKindText,
		Sender:            "carol@sea.com",
		SentAtUnix:        1002,
		Text:              "current message",
		SenderMentionHint: `<mention-tag target="seatalk://user?email=carol@sea.com"/>`,
	})

	expected := "Conversation context:\nGroup profile:\n- name: Demo Group\n\nEarlier messages from the current conversation are included below for context.\n\nHistory message 1:\nCurrent message context:\n- time: " + testLocalRFC3339() + "\n- sender: `alice@example.com`\n\nearlier text\n\nCurrent message context:\n- time: " + testLocalRFC3339At(1002) + "\n- sender: `carol@sea.com`\n- sender mention hint: `<mention-tag target=\"seatalk://user?email=carol@sea.com\"/>`\n\ncurrent message"
	if prompt != expected {
		t.Fatalf("unexpected prompt:\n%s", prompt)
	}
	if len(imageRefs) != 0 {
		t.Fatalf("unexpected image refs: %+v", imageRefs)
	}
}

func TestFormatReferencedMessageIncludesUnsupportedAttachmentPlaceholder(t *testing.T) {
	t.Parallel()

	prompt, imageRefs := formatReferencedMessage(ReferencedMessage{
		Kind: MessageKindFile,
	})

	expected := "Quoted message:\nType: file\nContent: <file>"
	if prompt != expected {
		t.Fatalf("unexpected prompt:\n%s", prompt)
	}
	if len(imageRefs) != 0 {
		t.Fatalf("unexpected image refs: %+v", imageRefs)
	}
}

func TestBuildTurnPromptIncludesFileAttachmentContext(t *testing.T) {
	t.Parallel()

	prompt, imageRefs := buildTurnPrompt(InboundMessage{
		Kind:       MessageKindFile,
		Sender:     "unknown",
		SentAtUnix: 1000,
		Text:       "report.pdf",
		FilePath:   "/tmp/report.pdf",
	})

	expected := "Current message context:\n- time: " + testLocalRFC3339() + "\n- sender: `unknown`\n\nUser sent a file.\nFilename: report.pdf\nAttachment: current file\nLocal path: /tmp/report.pdf\nPath validity: local file paths are temporary and only valid for this turn."
	if prompt != expected {
		t.Fatalf("unexpected prompt:\n%s", prompt)
	}
	if len(imageRefs) != 0 {
		t.Fatalf("unexpected image refs: %+v", imageRefs)
	}
}

func TestFormatReferencedMessageIncludesQuotedFileAttachmentContext(t *testing.T) {
	t.Parallel()

	prompt, imageRefs := formatReferencedMessage(ReferencedMessage{
		Kind:     MessageKindFile,
		Text:     "quoted.pdf",
		FilePath: "/tmp/quoted.pdf",
	})

	expected := "Quoted message:\nType: file\nFilename: quoted.pdf\nAttachment: quoted file\nLocal path: /tmp/quoted.pdf\nPath validity: local file paths are temporary and only valid for this turn."
	if prompt != expected {
		t.Fatalf("unexpected prompt:\n%s", prompt)
	}
	if len(imageRefs) != 0 {
		t.Fatalf("unexpected image refs: %+v", imageRefs)
	}
}

func TestBuildTurnPromptIncludesVideoAttachmentContext(t *testing.T) {
	t.Parallel()

	prompt, imageRefs := buildTurnPrompt(InboundMessage{
		Kind:       MessageKindVideo,
		Sender:     "unknown",
		SentAtUnix: 1000,
		VideoPath:  "/tmp/demo.mp4",
	})

	expected := "Current message context:\n- time: " + testLocalRFC3339() + "\n- sender: `unknown`\n\nUser sent a video.\nAttachment: current video\nLocal path: /tmp/demo.mp4\nPath validity: local file paths are temporary and only valid for this turn."
	if prompt != expected {
		t.Fatalf("unexpected prompt:\n%s", prompt)
	}
	if len(imageRefs) != 0 {
		t.Fatalf("unexpected image refs: %+v", imageRefs)
	}
}

func TestFormatReferencedMessageIncludesQuotedVideoAttachmentContext(t *testing.T) {
	t.Parallel()

	prompt, imageRefs := formatReferencedMessage(ReferencedMessage{
		Kind:      MessageKindVideo,
		VideoPath: "/tmp/quoted.mp4",
	})

	expected := "Quoted message:\nType: video\nAttachment: quoted video\nLocal path: /tmp/quoted.mp4\nPath validity: local file paths are temporary and only valid for this turn."
	if prompt != expected {
		t.Fatalf("unexpected prompt:\n%s", prompt)
	}
	if len(imageRefs) != 0 {
		t.Fatalf("unexpected image refs: %+v", imageRefs)
	}
}

func TestFormatReferencedMessageIncludesQuotedMixedFileAttachmentContext(t *testing.T) {
	t.Parallel()

	prompt, imageRefs := formatReferencedMessage(ReferencedMessage{
		Kind:      MessageKindMixed,
		Text:      "quoted mixed",
		ImagePath: "/tmp/quoted.png",
		FilePath:  "/tmp/quoted.pdf",
		VideoPath: "/tmp/quoted.mp4",
	})

	expected := "Quoted message:\nType: mixed\nContent: quoted mixed\nAttachment: quoted image\nAttachment: quoted file\nLocal path: /tmp/quoted.pdf\nPath validity: local file paths are temporary and only valid for this turn.\nAttachment: quoted video\nLocal path: /tmp/quoted.mp4\nPath validity: local file paths are temporary and only valid for this turn."
	if prompt != expected {
		t.Fatalf("unexpected prompt:\n%s", prompt)
	}
	if len(imageRefs) != 1 || imageRefs[0] != "/tmp/quoted.png" {
		t.Fatalf("unexpected image refs: %+v", imageRefs)
	}
}

func TestFormatReferencedMessageIncludesInteractiveMessageSummary(t *testing.T) {
	t.Parallel()

	prompt, imageRefs := formatReferencedMessage(ReferencedMessage{
		Kind: MessageKindInteractiveCard,
		Text: `interactive card; title="Approval Needed"; buttons=[Approve, Reject]`,
	})

	expected := "Quoted message:\nType: interactive_card\nContent: interactive card; title=\"Approval Needed\"; buttons=[Approve, Reject]"
	if prompt != expected {
		t.Fatalf("unexpected prompt:\n%s", prompt)
	}
	if len(imageRefs) != 0 {
		t.Fatalf("unexpected image refs: %+v", imageRefs)
	}
}

func TestBuildTurnInputUsesLocalImagePath(t *testing.T) {
	t.Parallel()

	file, err := os.CreateTemp(t.TempDir(), "assistant-runner-image-*.png")
	if err != nil {
		t.Fatalf("create temp file failed: %v", err)
	}
	if _, err := file.WriteString("image"); err != nil {
		t.Fatalf("write temp file failed: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close temp file failed: %v", err)
	}

	runner := &CodexRunner{}

	input, err := runner.buildTurnInput(TurnRequest{
		Message: InboundMessage{
			Kind:      MessageKindImage,
			ImagePath: file.Name(),
		},
	})
	if err != nil {
		t.Fatalf("build turn input failed: %v", err)
	}

	if len(input.Items) != 2 {
		t.Fatalf("unexpected item count: %d", len(input.Items))
	}
	if input.Items[0].Type != codex.UserInputText {
		t.Fatalf("unexpected first item type: %s", input.Items[0].Type)
	}
	if input.Items[1].Type != codex.UserInputLocalImage {
		t.Fatalf("unexpected second item type: %s", input.Items[1].Type)
	}
	if input.Items[1].Path != file.Name() {
		t.Fatalf("unexpected image path: %s", input.Items[1].Path)
	}
}

func TestFormatForwardedMessageContentIncludesMixedFileAttachmentContext(t *testing.T) {
	t.Parallel()

	prompt, imageRefs := formatForwardedMessageContent(ReferencedMessage{
		Kind:      MessageKindMixed,
		Text:      "forwarded mixed",
		ImagePath: "/tmp/forwarded.png",
		FilePath:  "/tmp/forwarded.pdf",
		VideoPath: "/tmp/forwarded.mp4",
	})

	expected := "Type: mixed\nContent: forwarded mixed\nAttachment: forwarded image\nAttachment: forwarded file\nLocal path: /tmp/forwarded.pdf\nPath validity: local file paths are temporary and only valid for this turn.\nAttachment: forwarded video\nLocal path: /tmp/forwarded.mp4\nPath validity: local file paths are temporary and only valid for this turn."
	if prompt != expected {
		t.Fatalf("unexpected prompt:\n%s", prompt)
	}
	if len(imageRefs) != 1 || imageRefs[0] != "/tmp/forwarded.png" {
		t.Fatalf("unexpected image refs: %+v", imageRefs)
	}
}

func TestFormatForwardedMessageContentIncludesVideoAttachmentContext(t *testing.T) {
	t.Parallel()

	prompt, imageRefs := formatForwardedMessageContent(ReferencedMessage{
		Kind:      MessageKindVideo,
		VideoPath: "/tmp/forwarded.mp4",
	})

	expected := "Type: video\nAttachment: forwarded video\nLocal path: /tmp/forwarded.mp4\nPath validity: local file paths are temporary and only valid for this turn."
	if prompt != expected {
		t.Fatalf("unexpected prompt:\n%s", prompt)
	}
	if len(imageRefs) != 0 {
		t.Fatalf("unexpected image refs: %+v", imageRefs)
	}
}

func TestBuildTurnInputUsesMixedMessageImagePaths(t *testing.T) {
	t.Parallel()

	fileOne, err := os.CreateTemp(t.TempDir(), "assistant-runner-mixed-1-*.png")
	if err != nil {
		t.Fatalf("create temp file failed: %v", err)
	}
	if err := fileOne.Close(); err != nil {
		t.Fatalf("close temp file failed: %v", err)
	}

	fileTwo, err := os.CreateTemp(t.TempDir(), "assistant-runner-mixed-2-*.png")
	if err != nil {
		t.Fatalf("create temp file failed: %v", err)
	}
	if err := fileTwo.Close(); err != nil {
		t.Fatalf("close temp file failed: %v", err)
	}

	runner := &CodexRunner{}

	input, err := runner.buildTurnInput(TurnRequest{
		Message: InboundMessage{
			Kind:       MessageKindMixed,
			Text:       "mixed content",
			ImagePaths: []string{fileOne.Name(), fileTwo.Name()},
		},
	})
	if err != nil {
		t.Fatalf("build turn input failed: %v", err)
	}

	if len(input.Items) != 3 {
		t.Fatalf("unexpected item count: %d", len(input.Items))
	}
	if input.Items[0].Type != codex.UserInputText {
		t.Fatalf("unexpected first item type: %s", input.Items[0].Type)
	}
	if input.Items[1].Type != codex.UserInputLocalImage || input.Items[2].Type != codex.UserInputLocalImage {
		t.Fatalf("unexpected image item types: %+v", input.Items)
	}
}

func TestBuildTurnInputInjectsInitialSystemPromptAndToolsForNewConversation(t *testing.T) {
	t.Parallel()

	runner := &CodexRunner{}
	runner.RegisterSystemPrompt("Global system prompt.")
	runner.RegisterTools(uppercaseTool{})

	input, err := runner.buildTurnInput(TurnRequest{
		Message: InboundMessage{
			Kind: MessageKindText,
			Text: "hello",
		},
	})
	if err != nil {
		t.Fatalf("build turn input failed: %v", err)
	}

	if !strings.Contains(input.Text, "Global system prompt.") {
		t.Fatalf("system prompt not injected: %s", input.Text)
	}
	if !strings.Contains(input.Text, "structured tool loop") {
		t.Fatalf("tool instruction not injected: %s", input.Text)
	}
	if !strings.Contains(input.Text, "hello") {
		t.Fatalf("user message not preserved: %s", input.Text)
	}
}

func TestBuildTurnInputSkipsInitialContextForExistingConversation(t *testing.T) {
	t.Parallel()

	runner := &CodexRunner{}
	runner.RegisterSystemPrompt("Global system prompt.")
	runner.RegisterTools(uppercaseTool{})

	input, err := runner.buildTurnInput(TurnRequest{
		Conversation: ConversationState{
			RunnerThreadID: "thread-1",
		},
		Message: InboundMessage{
			Kind: MessageKindText,
			Text: "hello",
		},
	})
	if err != nil {
		t.Fatalf("build turn input failed: %v", err)
	}

	if input.Text != "Current message context:\n- time: unknown\n- sender: `unknown`\n\nhello" {
		t.Fatalf("unexpected input text: %s", input.Text)
	}
}

func TestRunTurnReturnsErrorForNilRunner(t *testing.T) {
	t.Parallel()

	var runner *CodexRunner
	_, err := runner.StartSession(context.Background(), SessionOptions{})
	if err == nil || err.Error() != "start codex session failed: runner is nil" {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCodexStartSessionStartsThreadImmediately(t *testing.T) {
	t.Parallel()

	thread := &fakeCodexThread{id: "thread-new"}
	startCalls := 0
	runner := &CodexRunner{
		startThread: func(codex.ThreadOptions) codexThread {
			startCalls++
			return thread
		},
	}

	rawSession, err := runner.StartSession(context.Background(), SessionOptions{ConversationKey: "conversation-1"})
	if err != nil {
		t.Fatalf("start session failed: %v", err)
	}
	session := mustCompatSession(t, rawSession)
	if got := startCalls; got != 1 {
		t.Fatalf("unexpected start call count: %d", got)
	}
	if session.ID() != "thread-new" {
		t.Fatalf("unexpected session id: %s", session.ID())
	}
}

func TestCodexStartSessionResumesThreadImmediately(t *testing.T) {
	t.Parallel()

	thread := &fakeCodexThread{id: "thread-existing"}
	resumeCalls := 0
	var resumedThreadID string
	runner := &CodexRunner{
		resumeThread: func(threadID string, _ codex.ThreadOptions) codexThread {
			resumeCalls++
			resumedThreadID = threadID
			return thread
		},
	}

	rawSession, err := runner.StartSession(context.Background(), SessionOptions{
		ConversationKey: "conversation-1",
		ResumeSessionID: "thread-existing",
	})
	if err != nil {
		t.Fatalf("start session failed: %v", err)
	}
	session := mustCompatSession(t, rawSession)
	if got := resumeCalls; got != 1 {
		t.Fatalf("unexpected resume call count: %d", got)
	}
	if resumedThreadID != "thread-existing" {
		t.Fatalf("unexpected resumed thread id: %s", resumedThreadID)
	}
	if session.ID() != "thread-existing" {
		t.Fatalf("unexpected session id: %s", session.ID())
	}
}

func TestCodexSessionRunTurnReturnsErrorOnConversationMismatch(t *testing.T) {
	t.Parallel()

	session := &codexRunnerSession{
		runner:          &CodexRunner{},
		conversationKey: "conversation-1",
		threadID:        "thread-live",
	}

	_, err := runSessionTurn(context.Background(), session, TurnRequest{
		Conversation: ConversationState{
			Key:            "conversation-2",
			RunnerThreadID: "thread-live",
		},
		Message: InboundMessage{Kind: MessageKindText, Text: "hello"},
	})
	if err == nil || !strings.Contains(err.Error(), `conversation key mismatch: session="conversation-1" request="conversation-2"`) {
		t.Fatalf("unexpected error: %v", err)
	}

	_, err = runSessionTurn(context.Background(), session, TurnRequest{
		Conversation: ConversationState{
			Key:            "conversation-1",
			RunnerThreadID: "thread-other",
		},
		Message: InboundMessage{Kind: MessageKindText, Text: "hello"},
	})
	if err == nil || !strings.Contains(err.Error(), `runner thread id mismatch: session="thread-live" request="thread-other"`) {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRunTurnStartsThreadAndReturnsReply(t *testing.T) {
	t.Parallel()

	thread := &fakeCodexThread{
		id: "thread-new",
		turns: []codex.Turn{{
			FinalResponse: "hello back",
		}},
	}
	lifecycleCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runner := &CodexRunner{
		startThread: func(codex.ThreadOptions) codexThread {
			return thread
		},
		lifecycleCtx: lifecycleCtx,
		cancel:       cancel,
	}

	result, err := runTurnWithRunner(t, runner, TurnRequest{
		Message: InboundMessage{
			Kind: MessageKindText,
			Text: "hello",
		},
	})
	if err != nil {
		t.Fatalf("run turn failed: %v", err)
	}
	if result.RunnerThreadID != "thread-new" {
		t.Fatalf("unexpected thread id: %s", result.RunnerThreadID)
	}
	if result.ReplyText != "hello back" {
		t.Fatalf("unexpected reply: %s", result.ReplyText)
	}
	if len(thread.inputs) != 1 || thread.inputs[0].Text != "Current message context:\n- time: unknown\n- sender: `unknown`\n\nhello" {
		t.Fatalf("unexpected thread inputs: %+v", thread.inputs)
	}
}

func TestRunTurnResumesExistingThreadAndFallsBackToConversationID(t *testing.T) {
	t.Parallel()

	var resumedThreadID string
	thread := &fakeCodexThread{
		turns: []codex.Turn{{
			FinalResponse: "welcome back",
		}},
	}
	runner := &CodexRunner{
		resumeThread: func(threadID string, _ codex.ThreadOptions) codexThread {
			resumedThreadID = threadID
			return thread
		},
	}

	result, err := runTurnWithRunner(t, runner, TurnRequest{
		Conversation: ConversationState{
			RunnerThreadID: "thread-existing",
		},
		Message: InboundMessage{
			Kind: MessageKindText,
			Text: "hello again",
		},
	})
	if err != nil {
		t.Fatalf("run turn failed: %v", err)
	}
	if resumedThreadID != "thread-existing" {
		t.Fatalf("unexpected resumed thread id: %s", resumedThreadID)
	}
	if result.RunnerThreadID != "thread-existing" {
		t.Fatalf("unexpected result thread id: %s", result.RunnerThreadID)
	}
	if result.ReplyText != "welcome back" {
		t.Fatalf("unexpected reply: %s", result.ReplyText)
	}
}

func TestRunTurnUsesToolLoopWhenToolsRegistered(t *testing.T) {
	t.Parallel()

	thread := &fakeCodexThread{
		id: "thread-tools",
		turns: []codex.Turn{
			{
				FinalResponse: `{"action":"call_tool","tool_name":"uppercase","tool_input":{"text":"hello"}}`,
			},
			{
				FinalResponse: `{"action":"respond","message":"HELLO"}`,
			},
		},
	}
	runner := &CodexRunner{
		startThread: func(codex.ThreadOptions) codexThread {
			return thread
		},
		maxToolIterations: 3,
	}
	runner.RegisterTools(uppercaseTool{})

	result, err := runTurnWithRunner(t, runner, TurnRequest{
		Message: InboundMessage{
			Kind: MessageKindText,
			Text: "uppercase hello",
		},
	})
	if err != nil {
		t.Fatalf("run turn failed: %v", err)
	}
	if result.RunnerThreadID != "thread-tools" {
		t.Fatalf("unexpected thread id: %s", result.RunnerThreadID)
	}
	if result.ReplyText != "HELLO" {
		t.Fatalf("unexpected reply: %s", result.ReplyText)
	}
	if len(thread.inputs) != 2 {
		t.Fatalf("unexpected input count: %d", len(thread.inputs))
	}
}

func TestRunToolLoopExecutesToolAndReturnsFinalResponse(t *testing.T) {
	t.Parallel()

	runner := &CodexRunner{
		maxToolIterations: 3,
	}
	runner.RegisterTools(uppercaseTool{})
	thread := &fakeCodexThread{
		id: "thread-1",
		turns: []codex.Turn{
			{
				FinalResponse: `{"action":"call_tool","tool_name":"uppercase","tool_input_json":"{\"text\":\"hello\"}"}`,
			},
			{
				FinalResponse: `{"action":"respond","message":"HELLO"}`,
			},
		},
	}

	req := TurnRequest{
		Message: InboundMessage{
			Kind: MessageKindText,
			Text: "Please uppercase hello",
		},
	}
	input, err := runner.buildTurnInput(req)
	if err != nil {
		t.Fatalf("build turn input failed: %v", err)
	}

	session := &codexRunnerSession{runner: runner}
	reply, err := session.runToolLoop(context.Background(), req, thread, input)
	if err != nil {
		t.Fatalf("run tool loop failed: %v", err)
	}
	if reply != "HELLO" {
		t.Fatalf("unexpected reply: %s", reply)
	}
	if len(thread.inputs) != 2 {
		t.Fatalf("unexpected input count: %d", len(thread.inputs))
	}
	if got := thread.inputs[0].Text; !strings.Contains(got, "structured tool loop") || !strings.Contains(got, "Please uppercase hello") {
		t.Fatalf("unexpected first prompt: %s", got)
	}
	if got := thread.inputs[0].Text; !strings.Contains(got, `"output_schema"`) {
		t.Fatalf("tool output schema missing from prompt: %s", got)
	}
	if got := thread.inputs[1].Text; !strings.Contains(got, `{"text":"HELLO"}`) {
		t.Fatalf("unexpected tool result prompt: %s", got)
	}
	if thread.options[0].OutputSchema == nil || thread.options[1].OutputSchema == nil {
		t.Fatalf("expected tool loop schema on every turn")
	}
}

func TestRunToolLoopRecoversFromUnknownTool(t *testing.T) {
	t.Parallel()

	runner := &CodexRunner{
		maxToolIterations: 2,
	}
	runner.RegisterTools(uppercaseTool{})
	thread := &fakeCodexThread{
		turns: []codex.Turn{
			{
				FinalResponse: `{"action":"call_tool","tool_name":"missing","tool_input_json":"{}"}`,
			},
			{
				FinalResponse: `{"action":"respond","message":"fallback"}`,
			},
		},
	}

	req := TurnRequest{
		Message: InboundMessage{
			Kind: MessageKindText,
			Text: "test",
		},
	}
	input, err := runner.buildTurnInput(req)
	if err != nil {
		t.Fatalf("build turn input failed: %v", err)
	}

	session := &codexRunnerSession{runner: runner}
	reply, err := session.runToolLoop(context.Background(), req, thread, input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if reply != "fallback" {
		t.Fatalf("unexpected reply: %q", reply)
	}
	if len(thread.inputs) != 2 {
		t.Fatalf("unexpected input count: %d", len(thread.inputs))
	}
	if got := thread.inputs[1].Text; !strings.Contains(got, `unknown tool "missing"`) {
		t.Fatalf("unexpected recovery prompt: %s", got)
	}
}

func TestRunToolLoopSupportsSilentCompletion(t *testing.T) {
	t.Parallel()

	runner := &CodexRunner{
		maxToolIterations: 1,
	}
	runner.RegisterTools(uppercaseTool{})
	thread := &fakeCodexThread{
		id: "thread-silent",
		turns: []codex.Turn{
			{
				FinalResponse: `{"action":"silent","message":"","tool_name":"","tool_input_json":""}`,
			},
		},
	}

	req := TurnRequest{
		Message: InboundMessage{
			Kind: MessageKindText,
			Text: "No reply needed",
		},
	}
	input, err := runner.buildTurnInput(req)
	if err != nil {
		t.Fatalf("build turn input failed: %v", err)
	}

	session := &codexRunnerSession{runner: runner}
	reply, err := session.runToolLoop(context.Background(), req, thread, input)
	if err != nil {
		t.Fatalf("run tool loop failed: %v", err)
	}
	if reply != "" {
		t.Fatalf("unexpected reply: %q", reply)
	}
}

func TestRunToolLoopRecoversFromInvalidJSON(t *testing.T) {
	t.Parallel()

	runner := &CodexRunner{
		maxToolIterations: 2,
	}
	runner.RegisterTools(uppercaseTool{})
	thread := &fakeCodexThread{
		turns: []codex.Turn{
			{
				FinalResponse: `not json`,
			},
			{
				FinalResponse: `{"action":"respond","message":"fixed"}`,
			},
		},
	}

	req := TurnRequest{
		Message: InboundMessage{
			Kind: MessageKindText,
			Text: "test",
		},
	}
	input, err := runner.buildTurnInput(req)
	if err != nil {
		t.Fatalf("build turn input failed: %v", err)
	}

	session := &codexRunnerSession{runner: runner}
	reply, err := session.runToolLoop(context.Background(), req, thread, input)
	if err != nil {
		t.Fatalf("run tool loop failed: %v", err)
	}
	if reply != "fixed" {
		t.Fatalf("unexpected reply: %q", reply)
	}
	if len(thread.inputs) != 2 {
		t.Fatalf("unexpected input count: %d", len(thread.inputs))
	}
	if got := thread.inputs[1].Text; !strings.Contains(got, "previous JSON response was invalid") || !strings.Contains(got, "not json") {
		t.Fatalf("unexpected recovery prompt: %s", got)
	}
}

func TestBuildTurnInputWithContextUsesFrozenToolSnapshot(t *testing.T) {
	t.Parallel()

	runner := &CodexRunner{}
	frozenTools := []Tool{snapshotTool{name: "frozen"}}

	input, err := runner.buildTurnInputWithContext(TurnRequest{
		Message: InboundMessage{
			Kind: MessageKindText,
			Text: "test",
		},
	}, codexTurnContext{
		tools: frozenTools,
	})
	if err != nil {
		t.Fatalf("build turn input failed: %v", err)
	}

	runner.RegisterTools(snapshotTool{name: "late"})

	if !strings.Contains(input.Text, `"name": "frozen"`) {
		t.Fatalf("frozen tool missing from prompt: %s", input.Text)
	}
	if strings.Contains(input.Text, `"name": "late"`) {
		t.Fatalf("late tool unexpectedly appeared in prompt: %s", input.Text)
	}

	tool, ok := findToolIn(frozenTools, "frozen")
	if !ok || tool.Name() != "frozen" {
		t.Fatalf("frozen tool lookup failed")
	}
	if _, ok := findToolIn(frozenTools, "late"); ok {
		t.Fatalf("late tool unexpectedly present in frozen snapshot")
	}
}

func TestRunTurnWrapsBuildInputError(t *testing.T) {
	t.Parallel()

	runner := &CodexRunner{
		startThread: func(codex.ThreadOptions) codexThread {
			return &fakeCodexThread{id: "thread-build-error"}
		},
	}
	runner.RegisterTools(unmarshalableTool{})

	_, err := runTurnWithRunner(t, runner, TurnRequest{
		Message: InboundMessage{
			Kind: MessageKindText,
			Text: "hello",
		},
	})
	if err == nil || !strings.Contains(err.Error(), "run codex turn failed: encode tool catalog failed") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRunTurnWrapsThreadExecutionError(t *testing.T) {
	t.Parallel()

	runner := &CodexRunner{
		startThread: func(codex.ThreadOptions) codexThread {
			return &fakeCodexThread{id: "thread-run-error"}
		},
	}

	_, err := runTurnWithRunner(t, runner, TurnRequest{
		Message: InboundMessage{
			Kind: MessageKindText,
			Text: "hello",
		},
	})
	if err == nil || err.Error() != "run codex turn failed: unexpected turn" {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCollectStreamedTurnSetsTypingStatusOnCompletedItems(t *testing.T) {
	t.Parallel()

	responder := &testRunnerResponder{}
	runner := &CodexRunner{}
	streamed := &codex.StreamedTurn{
		Events: closedEvents(
			codex.ThreadEvent{
				Type: "item.completed",
				Item: &codex.ReasoningItem{ID: "item-1", Type: "reasoning", Text: "thinking"},
			},
			codex.ThreadEvent{
				Type: "item.completed",
				Item: &codex.AgentMessageItem{ID: "item-2", Type: "agent_message", Text: "final"},
			},
			codex.ThreadEvent{
				Type:  "turn.completed",
				Usage: &codex.Usage{OutputTokens: 12},
			},
		),
		Done: closedDone(nil),
	}

	session := &codexRunnerSession{runner: runner}
	turn, err := session.collectStreamedTurn(TurnRequest{
		Conversation: ConversationState{Key: "private:e_1:msg-1"},
		Message: InboundMessage{
			Responder: responder,
		},
	}, streamed)
	if err != nil {
		t.Fatalf("collect streamed turn failed: %v", err)
	}

	if turn.FinalResponse != "final" {
		t.Fatalf("unexpected final response: %s", turn.FinalResponse)
	}
	if responder.typingCalls != 0 {
		t.Fatalf("unexpected typing call count: %d", responder.typingCalls)
	}
}

func TestCollectStreamedTurnReturnsTurnFailure(t *testing.T) {
	t.Parallel()

	runner := &CodexRunner{}
	streamed := &codex.StreamedTurn{
		Events: closedEvents(codex.ThreadEvent{
			Type:  "turn.failed",
			Error: &codex.ThreadError{Message: "boom"},
		}),
		Done: closedDone(nil),
	}

	session := &codexRunnerSession{runner: runner}
	_, err := session.collectStreamedTurn(TurnRequest{}, streamed)
	if err == nil || err.Error() != "boom" {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCollectStreamedTurnReturnsStreamError(t *testing.T) {
	t.Parallel()

	runner := &CodexRunner{}
	streamed := &codex.StreamedTurn{
		Events: closedEvents(),
		Done:   closedDone(errors.New("stream failed")),
	}

	session := &codexRunnerSession{runner: runner}
	_, err := session.collectStreamedTurn(TurnRequest{}, streamed)
	if err == nil || err.Error() != "stream failed" {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestInterruptWaitsForActiveCodexSessionTurn(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	release := make(chan struct{})
	thread := &fakeCodexThread{
		id: "thread-interrupt",
		runStreamedFn: func(_ codex.Input, options codex.TurnOptions) (*codex.StreamedTurn, error) {
			close(started)

			events := make(chan codex.ThreadEvent)
			done := make(chan error, 1)
			go func() {
				<-options.Context.Done()
				<-release
				close(events)
				done <- options.Context.Err()
				close(done)
			}()

			return &codex.StreamedTurn{
				Events: events,
				Done:   done,
			}, nil
		},
	}
	runner := &CodexRunner{
		startThread: func(codex.ThreadOptions) codexThread {
			return thread
		},
	}
	session := &codexRunnerSession{
		runner:          runner,
		conversationKey: "conv-interrupt",
		threadID:        "thread-interrupt",
		thread:          thread,
	}

	runErrCh := make(chan error, 1)
	go func() {
		_, err := runSessionTurn(context.Background(), session, TurnRequest{
			Conversation: ConversationState{Key: "conv-interrupt"},
			Message: InboundMessage{
				Kind: MessageKindText,
				Text: "hello",
			},
		})
		runErrCh <- err
	}()

	<-started

	interruptDone := make(chan error, 1)
	go func() {
		interruptDone <- interruptSession(context.Background(), session)
	}()

	select {
	case err := <-interruptDone:
		t.Fatalf("interrupt returned before turn finished: %v", err)
	case <-time.After(20 * time.Millisecond):
	}

	close(release)

	select {
	case err := <-interruptDone:
		if err != nil {
			t.Fatalf("interrupt failed: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("interrupt did not wait for turn completion")
	}

	select {
	case err := <-runErrCh:
		if err == nil || err.Error() != "run codex turn failed: context canceled" {
			t.Fatalf("unexpected run turn error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("run turn did not finish")
	}
}

func TestCodexScheduledTurnInterruptBeforeRunCompletesImmediately(t *testing.T) {
	t.Parallel()

	thread := &fakeCodexThread{id: "thread-pre-run"}
	session := &codexRunnerSession{
		runner:          &CodexRunner{},
		conversationKey: "conv-pre-run",
		threadID:        "thread-pre-run",
		thread:          thread,
	}

	turn, err := session.ScheduleTurn(context.Background(), TurnRequest{
		Conversation: ConversationState{Key: "conv-pre-run"},
		Message: InboundMessage{
			Kind: MessageKindText,
			Text: "hello",
		},
	})
	if err != nil {
		t.Fatalf("ScheduleTurn failed: %v", err)
	}

	if err = turn.Interrupt(context.Background()); err != nil {
		t.Fatalf("Interrupt failed: %v", err)
	}

	select {
	case <-turn.Done():
	case <-time.After(time.Second):
		t.Fatal("turn did not complete after interrupt-before-run")
	}

	result, err := turn.Run(context.Background())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got result=%+v err=%v", result, err)
	}

	if active, ok := session.activeSessionTurn(); ok || active != nil {
		t.Fatal("expected active turn to be released")
	}
}

func TestCodexScheduledTurnRunReturnsErrorWhenStartedConcurrently(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	release := make(chan struct{})
	thread := &fakeCodexThread{
		id: "thread-concurrent-run",
		runStreamedFn: func(_ codex.Input, options codex.TurnOptions) (*codex.StreamedTurn, error) {
			close(started)
			events := make(chan codex.ThreadEvent)
			done := make(chan error, 1)
			go func() {
				<-release
				close(events)
				done <- nil
				close(done)
			}()
			return &codex.StreamedTurn{
				Events: events,
				Done:   done,
			}, nil
		},
	}
	session := &codexRunnerSession{
		runner:          &CodexRunner{},
		conversationKey: "conv-concurrent-run",
		threadID:        "thread-concurrent-run",
		thread:          thread,
	}

	turn, err := session.ScheduleTurn(context.Background(), TurnRequest{
		Conversation: ConversationState{Key: "conv-concurrent-run"},
		Message: InboundMessage{Kind: MessageKindText, Text: "hello"},
	})
	if err != nil {
		t.Fatalf("ScheduleTurn failed: %v", err)
	}

	firstRunDone := make(chan error, 1)
	go func() {
		_, runErr := turn.Run(context.Background())
		firstRunDone <- runErr
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first Run did not start")
	}

	_, err = turn.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "turn run already started") {
		t.Fatalf("expected concurrent Run error, got %v", err)
	}

	close(release)

	select {
	case err = <-firstRunDone:
		if err != nil {
			t.Fatalf("first Run failed: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("first Run did not complete")
	}
}

func TestInterruptedCodexSessionTurnDoesNotReturnSuccessfulResult(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	release := make(chan struct{})
	thread := &fakeCodexThread{
		id: "thread-interrupt-success",
		runStreamedFn: func(_ codex.Input, options codex.TurnOptions) (*codex.StreamedTurn, error) {
			close(started)

			events := make(chan codex.ThreadEvent)
			done := make(chan error, 1)
			go func() {
				<-options.Context.Done()
				<-release
				events <- codex.ThreadEvent{
					Type: "item.completed",
					Item: &codex.AgentMessageItem{
						ID:   "agent-message",
						Type: "agent_message",
						Text: "partial reply",
					},
				}
				events <- codex.ThreadEvent{Type: "turn.completed"}
				close(events)
				done <- nil
				close(done)
			}()

			return &codex.StreamedTurn{
				Events: events,
				Done:   done,
			}, nil
		},
	}
	session := &codexRunnerSession{
		runner:          &CodexRunner{},
		conversationKey: "conv-interrupt-success",
		threadID:        "thread-interrupt-success",
		thread:          thread,
	}

	runErrCh := make(chan error, 1)
	go func() {
		_, err := runSessionTurn(context.Background(), session, TurnRequest{
			Conversation: ConversationState{Key: "conv-interrupt-success"},
			Message:      InboundMessage{Kind: MessageKindText, Text: "hello"},
		})
		runErrCh <- err
	}()

	<-started

	interruptDone := make(chan error, 1)
	go func() {
		interruptDone <- interruptSession(context.Background(), session)
	}()

	close(release)

	select {
	case err := <-interruptDone:
		if err != nil {
			t.Fatalf("interrupt failed: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("interrupt did not wait for turn completion")
	}

	select {
	case err := <-runErrCh:
		if err == nil || err.Error() != "run codex turn failed: context canceled" {
			t.Fatalf("unexpected run turn error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("run turn did not finish")
	}
}

func TestInterruptReturnsNilWithoutActiveCodexSessionTurn(t *testing.T) {
	t.Parallel()

	runner := &CodexRunner{
		startThread: func(codex.ThreadOptions) codexThread {
			return &fakeCodexThread{id: "thread-idle"}
		},
	}

	err := interruptWithRunner(t, runner, ConversationState{Key: "conv-idle"})
	if err != nil {
		t.Fatalf("expected nil interrupt on idle session, got %v", err)
	}
}

func TestSessionCloseCancelsActiveCodexSessionTurn(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	release := make(chan struct{})
	thread := &fakeCodexThread{
		id: "thread-close",
		runStreamedFn: func(_ codex.Input, options codex.TurnOptions) (*codex.StreamedTurn, error) {
			close(started)

			events := make(chan codex.ThreadEvent)
			done := make(chan error, 1)
			go func() {
				<-options.Context.Done()
				<-release
				close(events)
				done <- options.Context.Err()
				close(done)
			}()

			return &codex.StreamedTurn{
				Events: events,
				Done:   done,
			}, nil
		},
	}
	runner := &CodexRunner{
		startThread: func(codex.ThreadOptions) codexThread {
			return thread
		},
	}
	session := &codexRunnerSession{
		runner:          runner,
		conversationKey: "conv-close",
		threadID:        "thread-close",
		thread:          thread,
	}

	runErrCh := make(chan error, 1)
	go func() {
		_, err := runSessionTurn(context.Background(), session, TurnRequest{
			Conversation: ConversationState{Key: "conv-close"},
			Message: InboundMessage{
				Kind: MessageKindText,
				Text: "hello",
			},
		})
		runErrCh <- err
	}()

	<-started

	closeDone := make(chan error, 1)
	go func() {
		closeDone <- session.Close()
	}()

	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("close failed: %v", err)
		}
	case <-time.After(20 * time.Millisecond):
	}

	close(release)

	select {
	case err := <-runErrCh:
		if err == nil || err.Error() != "run codex turn failed: context canceled" {
			t.Fatalf("unexpected run turn error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("run turn did not finish")
	}
}

func TestClosedCodexSessionRejectsRunTurn(t *testing.T) {
	t.Parallel()

	session := &codexRunnerSession{
		runner: &CodexRunner{},
	}

	if err := session.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	_, err := runSessionTurn(context.Background(), session, TurnRequest{})
	if err == nil || err.Error() != "run codex turn failed: session is closed" {
		t.Fatalf("unexpected run turn error: %v", err)
	}
}

func TestCodexSessionScheduleTurnInterruptsPreviousTurn(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	runCount := 0
	thread := &fakeCodexThread{
		id: "thread-busy",
		runStreamedFn: func(_ codex.Input, options codex.TurnOptions) (*codex.StreamedTurn, error) {
			runCount++
			events := make(chan codex.ThreadEvent)
			done := make(chan error, 1)
			go func() {
				if runCount == 1 {
					close(started)
					<-options.Context.Done()
					close(events)
					done <- options.Context.Err()
					close(done)
					return
				}
				close(events)
				done <- nil
				close(done)
			}()
			return &codex.StreamedTurn{Events: events, Done: done}, nil
		},
	}
	session := &codexRunnerSession{
		runner:          &CodexRunner{},
		conversationKey: "conv-busy",
		threadID:        "thread-busy",
		thread:          thread,
	}

	runErrCh := make(chan error, 1)
	go func() {
		_, err := runSessionTurn(context.Background(), session, TurnRequest{
			Conversation: ConversationState{Key: "conv-busy"},
			Message:      InboundMessage{Kind: MessageKindText, Text: "hello"},
		})
		runErrCh <- err
	}()

	<-started

	result, err := runSessionTurn(context.Background(), session, TurnRequest{
		Conversation: ConversationState{Key: "conv-busy"},
		Message:      InboundMessage{Kind: MessageKindText, Text: "again"},
	})
	if err != nil {
		t.Fatalf("second RunTurn failed: %v", err)
	}
	if result.RunnerThreadID != "thread-busy" {
		t.Fatalf("unexpected second thread id: %q", result.RunnerThreadID)
	}
	if err = session.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	select {
	case err = <-runErrCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("unexpected first run error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("first run turn did not finish")
	}
}

func TestParseToolLoopResponseAcceptsEncodedToolInputJSON(t *testing.T) {
	t.Parallel()

	response, err := parseToolLoopResponse(`{"action":"call_tool","tool_name":"uppercase","tool_input_json":"{\"text\":\"hello\"}"}`)
	if err != nil {
		t.Fatalf("parse tool loop response failed: %v", err)
	}
	if string(response.ToolInput) != `{"text":"hello"}` {
		t.Fatalf("unexpected tool input: %s", string(response.ToolInput))
	}
}

func TestParseToolLoopResponseKeepsLegacyToolInputObject(t *testing.T) {
	t.Parallel()

	response, err := parseToolLoopResponse(`{"action":"call_tool","tool_name":"uppercase","tool_input":{"text":"hello"}}`)
	if err != nil {
		t.Fatalf("parse tool loop response failed: %v", err)
	}
	if string(response.ToolInput) != `{"text":"hello"}` {
		t.Fatalf("unexpected tool input: %s", string(response.ToolInput))
	}
}

func closedEvents(events ...codex.ThreadEvent) <-chan codex.ThreadEvent {
	ch := make(chan codex.ThreadEvent, len(events))
	for _, event := range events {
		ch <- event
	}
	close(ch)
	return ch
}

func closedDone(err error) <-chan error {
	ch := make(chan error, 1)
	ch <- err
	close(ch)
	return ch
}
