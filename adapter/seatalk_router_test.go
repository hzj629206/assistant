package adapter

import (
	"bytes"
	"encoding/json"
	"log"
	"reflect"
	"strings"
	"testing"

	"github.com/hzj629206/assistant/agent"
	"github.com/hzj629206/assistant/seatalk"
)

func TestSeaTalkRouterRoutesPrivateMessage(t *testing.T) {
	t.Parallel()

	req := seatalk.EventRequest{EventID: "evt-1", Timestamp: 1_700_000_000_000}
	event := &seatalk.MessageFromBotSubscriberEvent{
		EmployeeCode: "e_1",
		SeatalkID:    "u_1",
		Email:        "private@example.com",
	}
	event.Message.MessageID = "msg-1"
	event.Message.Tag = "text"
	event.Message.Text.Content = "hello"

	message, ok, err := (seaTalkRouter{}).Route(req, event)
	if err != nil {
		t.Fatalf("route failed: %v", err)
	}
	if !ok {
		t.Fatal("expected private message to be routable")
	}
	if message.message.ConversationKey != "seatalk:private:e_1:msg-1" {
		t.Fatalf("unexpected conversation key: %s", message.message.ConversationKey)
	}
	if message.replyTarget.threadID != "msg-1" {
		t.Fatalf("unexpected reply thread id: %s", message.replyTarget.threadID)
	}
	if message.message.Kind != agent.MessageKindText {
		t.Fatalf("unexpected message kind: %s", message.message.Kind)
	}
	if message.message.Sender != "private@example.com" {
		t.Fatalf("unexpected sender: %s", message.message.Sender)
	}
	if message.message.SentAtUnix != 1_700_000_000 {
		t.Fatalf("unexpected sent time: %d", message.message.SentAtUnix)
	}
	if len(message.message.MessageTags) != 0 {
		t.Fatalf("unexpected message tags: %+v", message.message.MessageTags)
	}
}

func TestSeaTalkRouterTagsUpdatedPrivateTextMessage(t *testing.T) {
	t.Parallel()

	req := seatalk.EventRequest{EventID: "evt-1-updated", Timestamp: 1_700_000_000_000}
	event := &seatalk.MessageFromBotSubscriberEvent{
		EmployeeCode: "e_1",
		SeatalkID:    "u_1",
		Email:        "private@example.com",
	}
	event.Message.MessageID = "msg-1-updated"
	event.Message.Tag = "text"
	event.Message.Text.Content = "hello again"
	event.Message.Text.LastEditedTime = 1_700_000_111

	message, ok, err := (seaTalkRouter{}).Route(req, event)
	if err != nil {
		t.Fatalf("route failed: %v", err)
	}
	if !ok {
		t.Fatal("expected updated private text message to be routable")
	}
	expectedMessageTags := []string{"updated"}
	if !reflect.DeepEqual(message.message.MessageTags, expectedMessageTags) {
		t.Fatalf("unexpected message tags: %+v", message.message.MessageTags)
	}
}

func TestSeaTalkRouterRoutesGroupThreadMessage(t *testing.T) {
	t.Parallel()

	req := seatalk.EventRequest{EventID: "evt-2", Timestamp: 1_700_000_000_000}
	event := &seatalk.NewMessageReceivedFromThreadEvent{
		GroupID: "group-1",
	}
	event.Message.MessageID = "msg-2"
	event.Message.ThreadID = "thread-1"
	event.Message.Tag = "text"
	event.Message.Text.PlainText = "ask @Alice about this"
	event.Message.Text.MentionedList = []struct {
		Username     string `json:"username"`
		SeatalkID    string `json:"seatalk_id"`
		EmployeeCode string `json:"employee_code"`
		Email        string `json:"email"`
	}{
		{
			Username:  "Alice",
			SeatalkID: "seatalk-user-9",
			Email:     "alice@example.com",
		},
	}
	event.Message.MessageSentTime = 1_700_000_100
	event.Message.Sender.EmployeeCode = "e_group_1"
	event.Message.Sender.SeatalkID = "seatalk-user-1"
	event.Message.Sender.Email = "alice@example.com"

	message, ok, err := (seaTalkRouter{}).Route(req, event)
	if err != nil {
		t.Fatalf("route failed: %v", err)
	}
	if !ok {
		t.Fatal("expected thread message to be routable")
	}
	if message.message.ConversationKey != "seatalk:group:group-1:thread-1" {
		t.Fatalf("unexpected conversation key: %s", message.message.ConversationKey)
	}
	if message.replyTarget.threadID != "thread-1" {
		t.Fatalf("unexpected reply thread id: %s", message.replyTarget.threadID)
	}
	if message.replyTarget.mentionTarget.seatalkID != "seatalk-user-1" {
		t.Fatalf("unexpected mention seatalk id: %s", message.replyTarget.mentionTarget.seatalkID)
	}
	if message.replyTarget.mentionTarget.email != "alice@example.com" {
		t.Fatalf("unexpected mention email: %s", message.replyTarget.mentionTarget.email)
	}
	if message.replyTarget.mentionEmployee != "e_group_1" {
		t.Fatalf("unexpected mention employee: %s", message.replyTarget.mentionEmployee)
	}
	if message.message.Kind != agent.MessageKindText {
		t.Fatalf("unexpected message kind: %s", message.message.Kind)
	}
	if message.message.Text != "ask @Alice [mentioned_user_email=alice@example.com] about this" {
		t.Fatalf("unexpected message text: %q", message.message.Text)
	}
	if len(message.message.MessageTags) != 0 {
		t.Fatalf("unexpected message tags: %+v", message.message.MessageTags)
	}
	if message.message.Sender != "alice@example.com" {
		t.Fatalf("unexpected sender: %s", message.message.Sender)
	}
	if message.message.SentAtUnix != 1_700_000_100 {
		t.Fatalf("unexpected sent time: %d", message.message.SentAtUnix)
	}
}

func TestSeaTalkRouterFormatsBotSenderAsSeatalkBotIdentity(t *testing.T) {
	t.Parallel()

	req := seatalk.EventRequest{EventID: "evt-bot-sender", Timestamp: 1_700_000_000_000}
	event := &seatalk.NewMessageReceivedFromThreadEvent{
		GroupID: "group-1",
	}
	event.Message.MessageID = "msg-bot-sender"
	event.Message.ThreadID = "thread-1"
	event.Message.Tag = "text"
	event.Message.Text.PlainText = "bot message"
	event.Message.MessageSentTime = 1_700_000_100
	event.Message.Sender.SeatalkID = "seatalk-bot-1"
	event.Message.Sender.SenderType = 2

	message, ok, err := (seaTalkRouter{}).Route(req, event)
	if err != nil {
		t.Fatalf("route failed: %v", err)
	}
	if !ok {
		t.Fatal("expected thread message to be routable")
	}
	if message.message.Sender != "bot:seatalk-bot-1" {
		t.Fatalf("unexpected sender: %s", message.message.Sender)
	}
}

func TestExtractForwardedSenderFormatsBotSenderAsSeatalkBotIdentity(t *testing.T) {
	t.Parallel()

	got := extractForwardedSender(map[string]any{
		"seatalk_id":  "seatalk-bot-2",
		"sender_type": float64(2),
	})
	if got != "bot:seatalk-bot-2" {
		t.Fatalf("unexpected forwarded sender: %s", got)
	}
}

func TestExtractForwardedSenderReturnsUnknownWhenHumanIdentityIsMissing(t *testing.T) {
	t.Parallel()

	got := extractForwardedSender(map[string]any{
		"seatalk_id":  "",
		"sender_type": float64(1),
	})
	if got != "unknown" {
		t.Fatalf("unexpected forwarded sender: %s", got)
	}
}

func TestSeaTalkRouterRoutesMentionedGroupMessage(t *testing.T) {
	t.Parallel()

	req := seatalk.EventRequest{EventID: "evt-4", Timestamp: 1_700_000_000_000}
	event := &seatalk.NewMentionedMessageReceivedFromGroupChatEvent{
		GroupID: "group-2",
	}
	event.Message.MessageID = "msg-4"
	event.Message.Tag = "text"
	event.Message.Text.PlainText = "@bot ask @Carol hi"
	event.Message.Text.MentionedList = []struct {
		Username     string `json:"username"`
		SeatalkID    string `json:"seatalk_id"`
		EmployeeCode string `json:"employee_code"`
		Email        string `json:"email"`
	}{
		{
			Username:  "bot",
			SeatalkID: "seatalk-bot-1",
		},
		{
			Username:  "Carol",
			SeatalkID: "seatalk-user-3",
		},
	}
	event.Message.MessageSentTime = 1_700_000_200
	event.Message.Sender.EmployeeCode = "e_group_2"
	event.Message.Sender.SeatalkID = "seatalk-user-2"
	event.Message.Sender.Email = "bob@example.com"

	message, ok, err := (seaTalkRouter{}).Route(req, event)
	if err != nil {
		t.Fatalf("route failed: %v", err)
	}
	if !ok {
		t.Fatal("expected mentioned group message to be routable")
	}
	if message.message.ConversationKey != "seatalk:group:group-2:msg-4" {
		t.Fatalf("unexpected conversation key: %s", message.message.ConversationKey)
	}
	if message.replyTarget.threadID != "msg-4" {
		t.Fatalf("unexpected reply thread id: %s", message.replyTarget.threadID)
	}
	if message.replyTarget.mentionTarget.seatalkID != "seatalk-user-2" {
		t.Fatalf("unexpected mention seatalk id: %s", message.replyTarget.mentionTarget.seatalkID)
	}
	if message.replyTarget.mentionTarget.email != "bob@example.com" {
		t.Fatalf("unexpected mention email: %s", message.replyTarget.mentionTarget.email)
	}
	if message.replyTarget.mentionEmployee != "e_group_2" {
		t.Fatalf("unexpected mention employee: %s", message.replyTarget.mentionEmployee)
	}
	if message.message.Text != "@bot [mentioned_user_seatalk_id=seatalk-bot-1] ask @Carol [mentioned_user_seatalk_id=seatalk-user-3] hi" {
		t.Fatalf("unexpected message text: %q", message.message.Text)
	}
	expectedMessageTags := []string{"group_mentioned_message"}
	if !reflect.DeepEqual(message.message.MessageTags, expectedMessageTags) {
		t.Fatalf("unexpected message tags: %+v", message.message.MessageTags)
	}
	if message.message.Sender != "bob@example.com" {
		t.Fatalf("unexpected sender: %s", message.message.Sender)
	}
	if message.message.SentAtUnix != 1_700_000_200 {
		t.Fatalf("unexpected sent time: %d", message.message.SentAtUnix)
	}
}

func TestSeaTalkRouterRoutesMentionedGroupInteractiveMessage(t *testing.T) {
	t.Parallel()

	req := seatalk.EventRequest{EventID: "evt-4-interactive", Timestamp: 1_700_000_000_000}
	event := &seatalk.NewMentionedMessageReceivedFromGroupChatEvent{
		GroupID: "group-2",
	}
	event.Message.MessageID = "msg-4-interactive"
	event.Message.Tag = "interactive_message"
	event.Message.MessageSentTime = 1_700_000_200
	event.Message.Sender.EmployeeCode = "e_group_2"
	event.Message.Sender.SeatalkID = "seatalk-user-2"
	event.Message.Sender.Email = "bob@example.com"
	event.Message.InteractiveMessage = &seatalk.ThreadInteractiveMessage{
		Elements: []json.RawMessage{
			json.RawMessage(`{"element_type":"title","title":{"text":"@Carol please review"}}`),
		},
		MentionedList: []seatalk.MentionedEntity{
			{Username: "Carol", SeatalkID: "seatalk-user-3"},
		},
	}

	message, ok, err := (seaTalkRouter{}).Route(req, event)
	if err != nil {
		t.Fatalf("route failed: %v", err)
	}
	if !ok {
		t.Fatal("expected mentioned group interactive message to be routable")
	}
	if message.message.Kind != agent.MessageKindMixed {
		t.Fatalf("unexpected message kind: %s", message.message.Kind)
	}
	expected := `interactive card; title="@Carol [mentioned_user_seatalk_id=seatalk-user-3] please review"`
	if message.message.Text != expected {
		t.Fatalf("unexpected message text: %q", message.message.Text)
	}
	expectedMessageTags := []string{"group_mentioned_message"}
	if !reflect.DeepEqual(message.message.MessageTags, expectedMessageTags) {
		t.Fatalf("unexpected message tags: %+v", message.message.MessageTags)
	}
}

func TestSeaTalkRouterTagsUpdatedMentionedGroupTextMessage(t *testing.T) {
	t.Parallel()

	req := seatalk.EventRequest{EventID: "evt-4-updated", Timestamp: 1_700_000_000_000}
	event := &seatalk.NewMentionedMessageReceivedFromGroupChatEvent{
		GroupID: "group-2",
	}
	event.Message.MessageID = "msg-4-updated"
	event.Message.Tag = "text"
	event.Message.Text.PlainText = "@bot patched"
	event.Message.Text.LastEditedTime = 1_700_000_250
	event.Message.Text.MentionedList = []struct {
		Username     string `json:"username"`
		SeatalkID    string `json:"seatalk_id"`
		EmployeeCode string `json:"employee_code"`
		Email        string `json:"email"`
	}{
		{
			Username:  "bot",
			SeatalkID: "seatalk-bot-1",
		},
	}

	message, ok, err := (seaTalkRouter{}).Route(req, event)
	if err != nil {
		t.Fatalf("route failed: %v", err)
	}
	if !ok {
		t.Fatal("expected updated mentioned group text message to be routable")
	}
	expectedMessageTags := []string{"group_mentioned_message", "updated"}
	if !reflect.DeepEqual(message.message.MessageTags, expectedMessageTags) {
		t.Fatalf("unexpected message tags: %+v", message.message.MessageTags)
	}
}

func TestSeaTalkRouterRejectsMentionedGroupMessageWithoutGroupID(t *testing.T) {
	t.Parallel()

	req := seatalk.EventRequest{EventID: "evt-4-missing-group", Timestamp: 1_700_000_000_000}
	event := &seatalk.NewMentionedMessageReceivedFromGroupChatEvent{}
	event.Message.MessageID = "msg-4-missing-group"
	event.Message.Tag = "text"
	event.Message.Text.PlainText = "@bot hi"

	_, ok, err := (seaTalkRouter{}).Route(req, event)
	if err == nil || err.Error() != "route mentioned group message failed: group id is empty" {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Fatal("expected mentioned group message without group id to be rejected")
	}
}

func TestSeaTalkRouterRoutesMentionedGroupThreadMessage(t *testing.T) {
	t.Parallel()

	req := seatalk.EventRequest{EventID: "evt-4-thread", Timestamp: 1_700_000_000_000}
	event := &seatalk.NewMentionedMessageReceivedFromGroupChatEvent{
		GroupID: "group-2",
	}
	event.Message.MessageID = "msg-4-thread"
	event.Message.ThreadID = "thread-4"
	event.Message.Tag = "text"
	event.Message.Text.PlainText = "@bot please help"
	event.Message.Text.MentionedList = []struct {
		Username     string `json:"username"`
		SeatalkID    string `json:"seatalk_id"`
		EmployeeCode string `json:"employee_code"`
		Email        string `json:"email"`
	}{
		{
			Username:  "bot",
			SeatalkID: "seatalk-bot-1",
		},
	}
	event.Message.MessageSentTime = 1_700_000_300
	event.Message.Sender.EmployeeCode = "e_group_2"
	event.Message.Sender.SeatalkID = "seatalk-user-2"
	event.Message.Sender.Email = "bob@example.com"

	message, ok, err := (seaTalkRouter{}).Route(req, event)
	if err != nil {
		t.Fatalf("route failed: %v", err)
	}
	if !ok {
		t.Fatal("expected mentioned group thread message to be routable")
	}
	if message.message.ConversationKey != "seatalk:group:group-2:thread-4" {
		t.Fatalf("unexpected conversation key: %s", message.message.ConversationKey)
	}
	if message.replyTarget.threadID != "thread-4" {
		t.Fatalf("unexpected reply thread id: %s", message.replyTarget.threadID)
	}
	if message.message.Text != "@bot [mentioned_user_seatalk_id=seatalk-bot-1] please help" {
		t.Fatalf("unexpected message text: %q", message.message.Text)
	}
	expectedMessageTags := []string{"group_mentioned_message"}
	if !reflect.DeepEqual(message.message.MessageTags, expectedMessageTags) {
		t.Fatalf("unexpected message tags: %+v", message.message.MessageTags)
	}
}

func TestSeaTalkRouterRoutesPrivateImageMessage(t *testing.T) {
	t.Parallel()

	req := seatalk.EventRequest{EventID: "evt-3", Timestamp: 1_700_000_000_000}
	event := &seatalk.MessageFromBotSubscriberEvent{
		EmployeeCode: "e_1",
		SeatalkID:    "u_1",
	}
	event.Message.MessageID = "msg-3"
	event.Message.Tag = "image"
	event.Message.Image.Content = "https://example.com/image.png"

	message, ok, err := (seaTalkRouter{}).Route(req, event)
	if err != nil {
		t.Fatalf("route failed: %v", err)
	}
	if !ok {
		t.Fatal("expected private image message to be routable")
	}
	if message.message.Kind != agent.MessageKindImage {
		t.Fatalf("unexpected message kind: %s", message.message.Kind)
	}
	if len(message.imageURLs) != 1 || message.imageURLs[0] != "https://example.com/image.png" {
		t.Fatalf("unexpected image urls: %+v", message.imageURLs)
	}
}

func TestSeaTalkRouterRoutesPrivateFileMessage(t *testing.T) {
	t.Parallel()

	req := seatalk.EventRequest{EventID: "evt-file-1", Timestamp: 1_700_000_000_000}
	event := &seatalk.MessageFromBotSubscriberEvent{
		EmployeeCode: "e_1",
		SeatalkID:    "u_1",
	}
	event.Message.MessageID = "msg-file-1"
	event.Message.Tag = "file"
	event.Message.File.Content = "https://example.com/report.pdf"
	event.Message.File.Filename = "report.pdf"

	message, ok, err := (seaTalkRouter{}).Route(req, event)
	if err != nil {
		t.Fatalf("route failed: %v", err)
	}
	if !ok {
		t.Fatal("expected private file message to be routable")
	}
	if message.message.Kind != agent.MessageKindFile {
		t.Fatalf("unexpected message kind: %s", message.message.Kind)
	}
	if message.message.Text != "report.pdf" {
		t.Fatalf("unexpected message text: %q", message.message.Text)
	}
	if len(message.fileAttachments) != 1 {
		t.Fatalf("unexpected file attachments: %+v", message.fileAttachments)
	}
	if message.fileAttachments[0].URL != "https://example.com/report.pdf" {
		t.Fatalf("unexpected file url: %q", message.fileAttachments[0].URL)
	}
	if message.fileAttachments[0].Filename != "report.pdf" {
		t.Fatalf("unexpected filename: %q", message.fileAttachments[0].Filename)
	}
}

func TestSeaTalkRouterRoutesPrivateVideoMessage(t *testing.T) {
	t.Parallel()

	req := seatalk.EventRequest{EventID: "evt-video-1", Timestamp: 1_700_000_000_000}
	event := &seatalk.MessageFromBotSubscriberEvent{
		EmployeeCode: "e_1",
		SeatalkID:    "u_1",
	}
	event.Message.MessageID = "msg-video-1"
	event.Message.Tag = "video"
	event.Message.Video.Content = "https://example.com/demo.mp4"

	message, ok, err := (seaTalkRouter{}).Route(req, event)
	if err != nil {
		t.Fatalf("route failed: %v", err)
	}
	if !ok {
		t.Fatal("expected private video message to be routable")
	}
	if message.message.Kind != agent.MessageKindVideo {
		t.Fatalf("unexpected message kind: %s", message.message.Kind)
	}
	if len(message.videoAttachments) != 1 {
		t.Fatalf("unexpected video attachments: %+v", message.videoAttachments)
	}
	if message.videoAttachments[0].URL != "https://example.com/demo.mp4" {
		t.Fatalf("unexpected video url: %q", message.videoAttachments[0].URL)
	}
}

func TestSeaTalkRouterUsesSharedConversationForPrivateRootThread(t *testing.T) {
	t.Parallel()

	req := seatalk.EventRequest{EventID: "evt-root-1", Timestamp: 1_700_000_000_000}
	event := &seatalk.MessageFromBotSubscriberEvent{
		EmployeeCode: "e_1",
		SeatalkID:    "u_1",
	}
	event.Message.MessageID = "msg-root-1"
	event.Message.ThreadID = "0"
	event.Message.Tag = "image"
	event.Message.Image.Content = "https://example.com/image.png"

	message, ok, err := (seaTalkRouter{}).Route(req, event)
	if err != nil {
		t.Fatalf("route failed: %v", err)
	}
	if !ok {
		t.Fatal("expected private root-thread message to be routable")
	}
	if message.message.ConversationKey != "seatalk:private:e_1:0" {
		t.Fatalf("unexpected conversation key: %s", message.message.ConversationKey)
	}
	if message.replyTarget.threadID != "msg-root-1" {
		t.Fatalf("unexpected reply thread id: %s", message.replyTarget.threadID)
	}
}

func TestSeaTalkRouterRoutesPrivateCombinedForwardedChatHistory(t *testing.T) {
	t.Parallel()

	req := seatalk.EventRequest{EventID: "evt-5", Timestamp: 1_700_000_000_000}
	event := &seatalk.MessageFromBotSubscriberEvent{
		EmployeeCode: "e_1",
		SeatalkID:    "u_1",
	}
	event.Message.MessageID = "msg-5"
	event.Message.Tag = "combined_forwarded_chat_history"
	event.Message.CombinedForwardedChatHistory = &seatalk.CombinedForwardedChatHistoryMessage{
		Content: []map[string]any{
			map[string]any{
				"tag": "text",
				"sender": map[string]any{
					"email": "zhangyifei@shopee.com",
				},
				"text": map[string]any{
					"content": "hello from forwarded message",
				},
			},
		},
	}

	message, ok, err := (seaTalkRouter{}).Route(req, event)
	if err != nil {
		t.Fatalf("route failed: %v", err)
	}
	if !ok {
		t.Fatal("expected private forwarded message to be routable")
	}
	if message.message.Kind != agent.MessageKindForwarded {
		t.Fatalf("unexpected message kind: %s", message.message.Kind)
	}
	if message.message.Text != "" {
		t.Fatalf("unexpected message text: %q", message.message.Text)
	}
	if len(message.message.ForwardedMessages) != 0 {
		t.Fatalf("unexpected forwarded messages: %+v", message.message.ForwardedMessages)
	}
}

func TestSeaTalkRouterFallsBackToSeatalkIDWhenEmailMissing(t *testing.T) {
	t.Parallel()

	req := seatalk.EventRequest{EventID: "evt-8", Timestamp: 1_700_000_000_000}
	event := &seatalk.NewMessageReceivedFromThreadEvent{
		GroupID: "group-1",
	}
	event.Message.MessageID = "msg-8"
	event.Message.ThreadID = "thread-8"
	event.Message.Tag = "text"
	event.Message.Text.PlainText = "follow up"
	event.Message.MessageSentTime = 1_700_000_300
	event.Message.Sender.EmployeeCode = "e_group_8"
	event.Message.Sender.SeatalkID = "seatalk-user-8"

	message, ok, err := (seaTalkRouter{}).Route(req, event)
	if err != nil {
		t.Fatalf("route failed: %v", err)
	}
	if !ok {
		t.Fatal("expected thread message to be routable")
	}
	if message.message.Sender != "seatalk:seatalk-user-8" {
		t.Fatalf("unexpected sender: %s", message.message.Sender)
	}
}

func TestSeaTalkRouterRoutesPrivateInteractiveMessageAsPlaceholder(t *testing.T) {
	t.Parallel()

	req := seatalk.EventRequest{EventID: "evt-6", Timestamp: 1_700_000_000_000}
	event := &seatalk.MessageFromBotSubscriberEvent{
		EmployeeCode: "e_1",
		SeatalkID:    "u_1",
	}
	event.Message.MessageID = "msg-6"
	event.Message.Tag = "interactive_message"

	message, ok, err := (seaTalkRouter{}).Route(req, event)
	if err != nil {
		t.Fatalf("route failed: %v", err)
	}
	if !ok {
		t.Fatal("expected private interactive message to be routable")
	}
	if message.message.Kind != agent.MessageKindInteractiveCard {
		t.Fatalf("unexpected message kind: %s", message.message.Kind)
	}
	if message.message.Text != "" {
		t.Fatalf("unexpected message text: %q", message.message.Text)
	}
}

func TestSeaTalkRouterRoutesThreadInteractiveMessageWithSummary(t *testing.T) {
	t.Parallel()

	req := seatalk.EventRequest{EventID: "evt-9", Timestamp: 1_700_000_000_000}
	event := &seatalk.NewMessageReceivedFromThreadEvent{
		GroupID: "group-1",
	}
	event.Message.MessageID = "msg-9"
	event.Message.ThreadID = "thread-9"
	event.Message.Tag = "interactive_message"
	event.Message.InteractiveMessage = &seatalk.ThreadInteractiveMessage{
		Elements: []json.RawMessage{
			json.RawMessage(`{"element_type":"title","title":{"text":"Deploy Result"}}`),
			json.RawMessage(`{"element_type":"description","description":{"text":"Production failed"}}`),
			json.RawMessage(`{"element_type":"button_group","button_group":[{"button_type":"callback","text":"Retry"},{"button_type":"redirect","text":"Logs","desktop_link":{"type":"web","path":"https://example.com/logs"}}]}`),
		},
	}

	message, ok, err := (seaTalkRouter{}).Route(req, event)
	if err != nil {
		t.Fatalf("route failed: %v", err)
	}
	if !ok {
		t.Fatal("expected thread interactive message to be routable")
	}
	if message.message.Kind != agent.MessageKindInteractiveCard {
		t.Fatalf("unexpected message kind: %s", message.message.Kind)
	}
	expected := `interactive card; title="Deploy Result"; description="Production failed"; buttons=[Retry, Logs (https://example.com/logs)]`
	if message.message.Text != expected {
		t.Fatalf("unexpected message text: %q", message.message.Text)
	}
}

func TestSeaTalkRouterRoutesThreadInteractiveMessageWithExpandedMentions(t *testing.T) {
	t.Parallel()

	req := seatalk.EventRequest{EventID: "evt-9-mentions", Timestamp: 1_700_000_000_000}
	event := &seatalk.NewMessageReceivedFromThreadEvent{
		GroupID: "group-1",
	}
	event.Message.MessageID = "msg-9-mentions"
	event.Message.ThreadID = "thread-9"
	event.Message.Tag = "interactive_message"
	event.Message.InteractiveMessage = &seatalk.ThreadInteractiveMessage{
		Elements: []json.RawMessage{
			json.RawMessage(`{"element_type":"title","title":{"text":"Ask @Carol"}}`),
		},
		MentionedList: []seatalk.MentionedEntity{
			{Username: "Carol", SeatalkID: "seatalk-user-3"},
		},
	}

	message, ok, err := (seaTalkRouter{}).Route(req, event)
	if err != nil {
		t.Fatalf("route failed: %v", err)
	}
	if !ok {
		t.Fatal("expected thread interactive message to be routable")
	}
	if message.message.Kind != agent.MessageKindMixed {
		t.Fatalf("unexpected message kind: %s", message.message.Kind)
	}
	expected := `interactive card; title="Ask @Carol [mentioned_user_seatalk_id=seatalk-user-3]"`
	if message.message.Text != expected {
		t.Fatalf("unexpected message text: %q", message.message.Text)
	}
}

func TestSeaTalkRouterRoutesThreadInteractiveMessageWithImagesAsInteractiveCard(t *testing.T) {
	t.Parallel()

	req := seatalk.EventRequest{EventID: "evt-10", Timestamp: 1_700_000_000_000}
	event := &seatalk.NewMessageReceivedFromThreadEvent{
		GroupID: "group-1",
	}
	event.Message.MessageID = "msg-10"
	event.Message.ThreadID = "thread-10"
	event.Message.Tag = "interactive_message"
	event.Message.InteractiveMessage = &seatalk.ThreadInteractiveMessage{
		Elements: []json.RawMessage{
			json.RawMessage(`{"element_type":"title","title":{"text":"Deploy Result"}}`),
			json.RawMessage(`{"element_type":"image","image":{"content":"https://example.com/image-1.png"}}`),
			json.RawMessage(`{"element_type":"image","image":{"content":"https://example.com/image-2.png"}}`),
		},
	}

	message, ok, err := (seaTalkRouter{}).Route(req, event)
	if err != nil {
		t.Fatalf("route failed: %v", err)
	}
	if !ok {
		t.Fatal("expected thread interactive message to be routable")
	}
	if message.message.Kind != agent.MessageKindMixed {
		t.Fatalf("unexpected message kind: %s", message.message.Kind)
	}
	expected := `interactive card; title="Deploy Result"; image_urls=[https://example.com/image-1.png, https://example.com/image-2.png]`
	if message.message.Text != expected {
		t.Fatalf("unexpected message text: %q", message.message.Text)
	}
	if len(message.imageURLs) != 2 {
		t.Fatalf("unexpected image urls: %+v", message.imageURLs)
	}
}

func TestSeaTalkRouterRoutesEditedThreadInteractiveMessageAsInteractiveCard(t *testing.T) {
	t.Parallel()

	req := seatalk.EventRequest{EventID: "evt-10-edited", Timestamp: 1_700_000_000_000}
	event := &seatalk.NewMessageReceivedFromThreadEvent{
		GroupID: "group-1",
	}
	event.Message.MessageID = "msg-10-edited"
	event.Message.ThreadID = "thread-10"
	event.Message.Tag = "interactive_message"
	event.Message.InteractiveMessage = &seatalk.ThreadInteractiveMessage{
		Elements: []json.RawMessage{
			json.RawMessage(`{"element_type":"title","title":{"text":"Deploy Result"}}`),
			json.RawMessage(`{"element_type":"description","description":{"text":"Updated after review"}}`),
		},
		LastEditedTime: 1_700_000_123,
	}

	message, ok, err := (seaTalkRouter{}).Route(req, event)
	if err != nil {
		t.Fatalf("route failed: %v", err)
	}
	if !ok {
		t.Fatal("expected edited thread interactive message to be routable")
	}
	if message.message.Kind != agent.MessageKindInteractiveCard {
		t.Fatalf("unexpected message kind: %s", message.message.Kind)
	}
	expectedMessageTags := []string{"updated"}
	if !reflect.DeepEqual(message.message.MessageTags, expectedMessageTags) {
		t.Fatalf("unexpected message tags: %+v", message.message.MessageTags)
	}
	expected := `interactive card; title="Deploy Result"; description="Updated after review"`
	if message.message.Text != expected {
		t.Fatalf("unexpected message text: %q", message.message.Text)
	}
}

func TestSeaTalkRouterLogsDroppedUnsupportedTag(t *testing.T) {
	t.Parallel()

	req := seatalk.EventRequest{EventID: "evt-7", Timestamp: 1_700_000_000_000}
	event := &seatalk.MessageFromBotSubscriberEvent{
		EmployeeCode: "e_1",
		SeatalkID:    "u_1",
	}
	event.Message.MessageID = "msg-7"
	event.Message.Tag = "sticker"

	var buffer bytes.Buffer
	originalWriter := log.Writer()
	originalFlags := log.Flags()
	log.SetOutput(&buffer)
	log.SetFlags(0)
	defer log.SetOutput(originalWriter)
	defer log.SetFlags(originalFlags)

	_, ok, err := (seaTalkRouter{}).Route(req, event)
	if err != nil {
		t.Fatalf("route failed: %v", err)
	}
	if ok {
		t.Fatal("expected unsupported tag to be dropped")
	}
	if !strings.Contains(buffer.String(), "seatalk router dropped message: source=private_message event_id=evt-7 message_id=msg-7 tag=sticker") {
		t.Fatalf("missing dropped-tag log: %q", buffer.String())
	}
}
