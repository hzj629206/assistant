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

func (unmarshalableTool) Name() string {
	return "bad"
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
			CodexThreadID: "thread-1",
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
	_, err := runner.RunTurn(context.Background(), TurnRequest{})
	if err == nil || err.Error() != "run codex turn failed: runner is nil" {
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
	runner := &CodexRunner{
		startThread: func(codex.ThreadOptions) codexThread {
			return thread
		},
	}

	result, err := runner.RunTurn(context.Background(), TurnRequest{
		Message: InboundMessage{
			Kind: MessageKindText,
			Text: "hello",
		},
	})
	if err != nil {
		t.Fatalf("run turn failed: %v", err)
	}
	if result.CodexThreadID != "thread-new" {
		t.Fatalf("unexpected thread id: %s", result.CodexThreadID)
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

	result, err := runner.RunTurn(context.Background(), TurnRequest{
		Conversation: ConversationState{
			CodexThreadID: "thread-existing",
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
	if result.CodexThreadID != "thread-existing" {
		t.Fatalf("unexpected result thread id: %s", result.CodexThreadID)
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

	result, err := runner.RunTurn(context.Background(), TurnRequest{
		Message: InboundMessage{
			Kind: MessageKindText,
			Text: "uppercase hello",
		},
	})
	if err != nil {
		t.Fatalf("run turn failed: %v", err)
	}
	if result.CodexThreadID != "thread-tools" {
		t.Fatalf("unexpected thread id: %s", result.CodexThreadID)
	}
	if result.ReplyText != "HELLO" {
		t.Fatalf("unexpected reply: %s", result.ReplyText)
	}
	if len(thread.inputs) != 2 {
		t.Fatalf("unexpected input count: %d", len(thread.inputs))
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

	_, err := runner.RunTurn(context.Background(), TurnRequest{
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

	_, err := runner.RunTurn(context.Background(), TurnRequest{
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

	turn, err := runner.collectStreamedTurn(TurnRequest{
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

	_, err := runner.collectStreamedTurn(TurnRequest{}, streamed)
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

	_, err := runner.collectStreamedTurn(TurnRequest{}, streamed)
	if err == nil || err.Error() != "stream failed" {
		t.Fatalf("unexpected error: %v", err)
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
