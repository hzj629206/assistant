package adapter

import (
	"cmp"
	"encoding/json"
	"errors"
	"log"
	"slices"
	"strconv"
	"strings"

	"github.com/hzj629206/assistant/agent"
	"github.com/hzj629206/assistant/seatalk"
)

// seaTalkRouter converts SeaTalk callback events into normalized inbound messages.
type seaTalkRouter struct{}

type seaTalkRoute struct {
	message          agent.InboundMessage
	imageURLs        []string
	fileAttachments  []seatalkFileAttachment
	videoAttachments []seatalkFileAttachment
	forwardedHistory *seatalk.CombinedForwardedChatHistoryMessage
	quotedMessageID  string
	replyTarget      seaTalkReplyTarget
}

type seatalkFileAttachment struct {
	URL      string
	Filename string
}

type seaTalkReplyTarget struct {
	isGroup         bool
	groupID         string
	employeeCode    string
	messageID       string
	threadID        string
	mentionEmployee string
	mentionTarget   seaTalkMentionTarget
}

type seaTalkMentionTarget struct {
	seatalkID string
	email     string
}

func newSeaTalkMentionTarget(seatalkID, email string) seaTalkMentionTarget {
	return seaTalkMentionTarget{
		seatalkID: strings.TrimSpace(seatalkID),
		email:     strings.TrimSpace(email),
	}
}

func (t seaTalkMentionTarget) IsZero() bool {
	return t.seatalkID == "" && t.email == ""
}

func (t seaTalkMentionTarget) MarkdownTag() string {
	if t.seatalkID != "" {
		return `<mention-tag target="seatalk://user?id=` + t.seatalkID + `"/>`
	}
	if t.email != "" {
		return `<mention-tag target="seatalk://user?email=` + t.email + `"/>`
	}
	return ""
}

// Route maps a decoded SeaTalk event into an inbound message.
// The boolean return value reports whether the event should be processed by the agent layer.
func (seaTalkRouter) Route(req seatalk.EventRequest, event seatalk.Event) (seaTalkRoute, bool, error) {
	switch e := event.(type) {
	case *seatalk.MessageFromBotSubscriberEvent:
		return routePrivateMessage(req, e)
	case *seatalk.NewMentionedMessageReceivedFromGroupChatEvent:
		return routeMentionedGroupMessage(req, e)
	case *seatalk.NewMessageReceivedFromThreadEvent:
		return routeThreadMessage(req, e)
	default:
		return seaTalkRoute{}, false, nil
	}
}

func routePrivateMessage(req seatalk.EventRequest, event *seatalk.MessageFromBotSubscriberEvent) (seaTalkRoute, bool, error) {
	if event.EmployeeCode == "" {
		return seaTalkRoute{}, false, errors.New("route private message failed: employee code is empty")
	}

	kind, text, imageURLs, ok := normalizeMessageContent(
		fileAttachmentFromURL(event.Message.File.Content, event.Message.File.Filename),
		event.Message.Tag,
		event.Message.Text.Content,
		event.Message.Image.Content,
		nil,
	)
	if !ok {
		logDroppedMessageTag("private_message", req.EventID, event.Message.MessageID, event.Message.Tag)
		return seaTalkRoute{}, false, nil
	}

	threadRef := privateConversationThreadKey(event.Message.ThreadID, event.Message.MessageID)
	replyThreadID := privateReplyThreadID(event.Message.ThreadID, event.Message.MessageID)

	return seaTalkRoute{
		message: agent.InboundMessage{
			ID:              req.EventID,
			ConversationKey: conversationKey(false, event.EmployeeCode, "", threadRef),
			Kind:            kind,
			Sender:          formatCurrentMessageSender(event.Email, event.SeatalkID, 1),
			SentAtUnix:      currentMessageUnixTime(0, req.Timestamp),
			Text:            text,
			MessageTags:     updatedMessageTags(nil, messageWasUpdated(event.Message.Tag, event.Message.Text.LastEditedTime, nil)),
		},
		imageURLs:        imageURLs,
		fileAttachments:  fileAttachmentsFromURL(event.Message.File.Content, event.Message.File.Filename),
		videoAttachments: fileAttachmentsFromURL(event.Message.Video.Content, ""),
		forwardedHistory: event.Message.CombinedForwardedChatHistory,
		quotedMessageID:  event.Message.QuotedMessageID,
		replyTarget: seaTalkReplyTarget{
			employeeCode: event.EmployeeCode,
			messageID:    strings.TrimSpace(event.Message.MessageID),
			threadID:     replyThreadID,
		},
	}, true, nil
}

func routeMentionedGroupMessage(req seatalk.EventRequest, event *seatalk.NewMentionedMessageReceivedFromGroupChatEvent) (seaTalkRoute, bool, error) {
	if strings.TrimSpace(event.GroupID) == "" {
		return seaTalkRoute{}, false, errors.New("route mentioned group message failed: group id is empty")
	}
	var (
		kind      agent.MessageKind
		text      string
		imageURLs []string
		ok        bool
	)

	switch event.Message.Tag {
	case "text":
		mentioned := make([]seatalk.MentionedEntity, 0, len(event.Message.Text.MentionedList))
		for _, entry := range event.Message.Text.MentionedList {
			mentioned = append(mentioned, seatalk.MentionedEntity{
				Username:     entry.Username,
				SeatalkID:    entry.SeatalkID,
				EmployeeCode: entry.EmployeeCode,
				Email:        entry.Email,
			})
		}
		text = expandMentionedText(strings.TrimSpace(event.Message.Text.PlainText), mentioned)
		if text == "" {
			logDroppedMessageTag("mentioned_group_message", req.EventID, event.Message.MessageID, event.Message.Tag)
			return seaTalkRoute{}, false, nil
		}
		kind = agent.MessageKindText
		ok = true
	case "interactive_message":
		kind, text, imageURLs = normalizeInteractiveMessageContent(event.Message.InteractiveMessage)
		ok = text != "" || len(imageURLs) > 0
	default:
		logDroppedMessageTag("mentioned_group_message", req.EventID, event.Message.MessageID, event.Message.Tag)
		return seaTalkRoute{}, false, nil
	}
	if !ok {
		logDroppedMessageTag("mentioned_group_message", req.EventID, event.Message.MessageID, event.Message.Tag)
		return seaTalkRoute{}, false, nil
	}

	threadRef := groupConversationThreadKey(event.Message.ThreadID, event.Message.MessageID)
	messageTags := updatedMessageTags(
		[]string{"group_mentioned_message"},
		messageWasUpdated(event.Message.Tag, event.Message.Text.LastEditedTime, event.Message.InteractiveMessage),
	)

	return seaTalkRoute{
		message: agent.InboundMessage{
			ID:              req.EventID,
			ConversationKey: conversationKey(true, "", event.GroupID, threadRef),
			Kind:            kind,
			Sender:          formatCurrentMessageSender(event.Message.Sender.Email, event.Message.Sender.SeatalkID, event.Message.Sender.SenderType),
			SentAtUnix:      currentMessageUnixTime(event.Message.MessageSentTime, req.Timestamp),
			Text:            text,
			MessageTags:     messageTags,
		},
		imageURLs:       imageURLs,
		quotedMessageID: event.Message.QuotedMessageID,
		replyTarget: seaTalkReplyTarget{
			isGroup:         true,
			groupID:         event.GroupID,
			messageID:       strings.TrimSpace(event.Message.MessageID),
			threadID:        threadRef,
			mentionEmployee: strings.TrimSpace(event.Message.Sender.EmployeeCode),
			mentionTarget: newSeaTalkMentionTarget(
				event.Message.Sender.SeatalkID,
				event.Message.Sender.Email,
			),
		},
	}, true, nil
}

func routeThreadMessage(req seatalk.EventRequest, event *seatalk.NewMessageReceivedFromThreadEvent) (seaTalkRoute, bool, error) {
	if strings.TrimSpace(event.GroupID) == "" {
		return seaTalkRoute{}, false, errors.New("route group thread message failed: group id is empty")
	}
	if event.Message.ThreadID == "" {
		logDroppedMessageTag("thread_message", req.EventID, event.Message.MessageID, event.Message.Tag)
		return seaTalkRoute{}, false, nil
	}

	textContent := event.Message.Text.PlainText
	if event.Message.Tag == string(agent.MessageKindText) {
		mentioned := make([]seatalk.MentionedEntity, 0, len(event.Message.Text.MentionedList))
		for _, entry := range event.Message.Text.MentionedList {
			mentioned = append(mentioned, seatalk.MentionedEntity{
				Username:     entry.Username,
				SeatalkID:    entry.SeatalkID,
				EmployeeCode: entry.EmployeeCode,
				Email:        entry.Email,
			})
		}
		textContent = expandMentionedText(textContent, mentioned)
	}

	kind, text, imageURLs, ok := normalizeMessageContent(
		fileAttachmentFromURL(event.Message.File.Content, event.Message.File.Filename),
		event.Message.Tag,
		textContent,
		event.Message.Image.Content,
		event.Message.InteractiveMessage,
	)
	if !ok {
		logDroppedMessageTag("thread_message", req.EventID, event.Message.MessageID, event.Message.Tag)
		return seaTalkRoute{}, false, nil
	}

	target := seaTalkReplyTarget{
		isGroup:         true,
		groupID:         event.GroupID,
		messageID:       strings.TrimSpace(event.Message.MessageID),
		threadID:        event.Message.ThreadID,
		mentionEmployee: strings.TrimSpace(event.Message.Sender.EmployeeCode),
		mentionTarget: newSeaTalkMentionTarget(
			event.Message.Sender.SeatalkID,
			event.Message.Sender.Email,
		),
	}
	messageTags := updatedMessageTags(
		nil,
		messageWasUpdated(event.Message.Tag, event.Message.Text.LastEditedTime, event.Message.InteractiveMessage),
	)

	return seaTalkRoute{
		message: agent.InboundMessage{
			ID:              req.EventID,
			ConversationKey: conversationKey(true, "", event.GroupID, event.Message.ThreadID),
			Kind:            kind,
			Sender:          formatCurrentMessageSender(event.Message.Sender.Email, event.Message.Sender.SeatalkID, event.Message.Sender.SenderType),
			SentAtUnix:      currentMessageUnixTime(event.Message.MessageSentTime, req.Timestamp),
			Text:            text,
			MessageTags:     messageTags,
		},
		imageURLs:        imageURLs,
		fileAttachments:  fileAttachmentsFromURL(event.Message.File.Content, event.Message.File.Filename),
		videoAttachments: fileAttachmentsFromURL(event.Message.Video.Content, ""),
		forwardedHistory: event.Message.CombinedForwardedChatHistory,
		quotedMessageID:  event.Message.QuotedMessageID,
		replyTarget:      target,
	}, true, nil
}

func normalizeMessageContent(
	fileAttachment seatalkFileAttachment,
	tag string,
	textContent string,
	imageURL string,
	interactiveMessage *seatalk.ThreadInteractiveMessage,
) (agent.MessageKind, string, []string, bool) {
	switch tag {
	case string(agent.MessageKindText):
		text := strings.TrimSpace(textContent)
		if text == "" {
			return "", "", nil, false
		}
		return agent.MessageKindText, text, nil, true
	case string(agent.MessageKindImage):
		imageURL = strings.TrimSpace(imageURL)
		if imageURL == "" {
			return "", "", nil, false
		}
		return agent.MessageKindImage, "", []string{imageURL}, true
	case "combined_forwarded_chat_history":
		return agent.MessageKindForwarded, "", nil, true
	case "file":
		return agent.MessageKindFile, strings.TrimSpace(fileAttachment.Filename), nil, true
	case "video":
		return agent.MessageKindVideo, "", nil, true
	case "interactive_message":
		kind, text, imageURLs := normalizeInteractiveMessageContent(interactiveMessage)
		return kind, text, imageURLs, true
	default:
		return "", "", nil, false
	}
}

func normalizeInteractiveMessageContent(message *seatalk.ThreadInteractiveMessage) (agent.MessageKind, string, []string) {
	text := strings.TrimSpace(summarizeInteractiveMessage(message))
	imageURLs := compactURLs(seatalk.ExtractInteractiveImageURLs(message))
	if message != nil && message.LastEditedTime == 0 && !interactiveMessageHasActionButton(message) {
		return agent.MessageKindMixed, text, imageURLs
	}
	return agent.MessageKindInteractiveCard, text, imageURLs
}

func messageWasUpdated(tag string, textLastEditedTime int64, interactiveMessage *seatalk.ThreadInteractiveMessage) bool {
	switch tag {
	case "text":
		return textLastEditedTime > 0
	case "interactive_message":
		return interactiveMessage != nil && interactiveMessage.LastEditedTime > 0
	default:
		return false
	}
}

func updatedMessageTags(base []string, updated bool) []string {
	if !updated {
		if len(base) == 0 {
			return nil
		}
		cloned := make([]string, len(base))
		copy(cloned, base)
		return cloned
	}

	result := make([]string, 0, len(base)+1)
	result = append(result, base...)
	result = append(result, "updated")
	return result
}

func interactiveMessageHasActionButton(message *seatalk.ThreadInteractiveMessage) bool {
	if message == nil {
		return false
	}

	for _, raw := range message.Elements {
		if len(raw) == 0 {
			continue
		}

		var element struct {
			ElementType string `json:"element_type"`
			Button      any    `json:"button"`
			ButtonGroup []any  `json:"button_group"`
		}
		if err := json.Unmarshal(raw, &element); err != nil {
			continue
		}

		switch strings.TrimSpace(element.ElementType) {
		case "button":
			if element.Button != nil {
				return true
			}
		case "button_group":
			if len(element.ButtonGroup) > 0 {
				return true
			}
		}
	}

	return false
}

func fileAttachmentFromURL(url, filename string) seatalkFileAttachment {
	return seatalkFileAttachment{
		URL:      strings.TrimSpace(url),
		Filename: strings.TrimSpace(filename),
	}
}

func fileAttachmentsFromURL(url, filename string) []seatalkFileAttachment {
	attachment := fileAttachmentFromURL(url, filename)
	if attachment.URL == "" && attachment.Filename == "" {
		return nil
	}
	return []seatalkFileAttachment{attachment}
}

func compactURLs(values []string) []string {
	filtered := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		filtered = append(filtered, value)
	}
	return filtered
}

func conversationKey(isGroup bool, employeeCode, groupID, threadRef string) string {
	if isGroup {
		return "seatalk:group:" + groupID + ":" + threadRef
	}

	return "seatalk:private:" + employeeCode + ":" + threadRef
}

func groupConversationThreadKey(threadID, messageID string) string {
	threadID = strings.TrimSpace(threadID)
	if threadID != "" {
		return threadID
	}

	return strings.TrimSpace(messageID)
}

func privateConversationThreadKey(threadID, messageID string) string {
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return strings.TrimSpace(messageID)
	}
	if threadID == "0" {
		return "0"
	}
	return threadID
}

func privateReplyThreadID(threadID, messageID string) string {
	threadID = strings.TrimSpace(threadID)
	if threadID == "" || threadID == "0" {
		return strings.TrimSpace(messageID)
	}
	return threadID
}

func logDroppedMessageTag(source, eventID, messageID, tag string) {
	log.Printf(
		"seatalk router dropped message: source=%s event_id=%s message_id=%s tag=%s",
		source,
		strings.TrimSpace(eventID),
		strings.TrimSpace(messageID),
		strings.TrimSpace(tag),
	)
}

func formatCurrentMessageSender(email, seatalkID string, senderType int) string {
	seatalkID = strings.TrimSpace(seatalkID)
	switch senderType {
	case 2:
		if seatalkID == "" || seatalkID == "0" {
			return "bot:unknown"
		}
		return "bot:" + seatalkID
	case 3:
		if seatalkID == "" || seatalkID == "0" {
			return "system:unknown"
		}
		return "system:" + seatalkID
	}

	email = strings.TrimSpace(email)
	if email != "" {
		return email
	}
	if seatalkID == "" || seatalkID == "0" {
		return "unknown"
	}
	return "seatalk:" + seatalkID
}

func formatMentionIdentity(email, seatalkID string) string {
	email = strings.TrimSpace(email)
	if email != "" {
		return "mentioned_user_email=" + email
	}

	seatalkID = strings.TrimSpace(seatalkID)
	if seatalkID != "" && seatalkID != "0" {
		return "mentioned_user_seatalk_id=" + seatalkID
	}

	return "mentioned_user=unknown"
}

func expandMentionedText(text string, mentioned []seatalk.MentionedEntity) string {
	text = strings.TrimSpace(text)
	if text == "" || len(mentioned) == 0 {
		return text
	}

	type replacement struct {
		token    string
		expanded string
	}

	replacements := make([]replacement, 0, len(mentioned))
	seen := make(map[string]struct{}, len(mentioned))
	for _, entity := range mentioned {
		username := strings.TrimSpace(entity.Username)
		if username == "" {
			continue
		}

		token := "@" + username
		if _, ok := seen[token]; ok {
			continue
		}
		seen[token] = struct{}{}

		replacements = append(replacements, replacement{
			token:    token,
			expanded: token + " [" + formatMentionIdentity(entity.Email, entity.SeatalkID) + "]",
		})
	}

	slices.SortFunc(replacements, func(left, right replacement) int {
		return cmp.Compare(len(right.token), len(left.token))
	})

	for _, item := range replacements {
		text = strings.ReplaceAll(text, item.token, item.expanded)
	}

	return text
}

func currentMessageUnixTime(messageSentTime int64, requestTimestamp int64) int64 {
	if messageSentTime > 0 {
		return messageSentTime
	}
	if requestTimestamp <= 0 {
		return 0
	}
	if requestTimestamp >= 1_000_000_000_000 {
		return requestTimestamp / 1000
	}
	return requestTimestamp
}

type forwardedMessageDraft struct {
	message          agent.ReferencedMessage
	imageURLs        []string
	fileAttachments  []seatalkFileAttachment
	videoAttachments []seatalkFileAttachment
}

func buildForwardedMessageDrafts(message *seatalk.CombinedForwardedChatHistoryMessage) []forwardedMessageDraft {
	if message == nil {
		return nil
	}

	return collectForwardedMessageDrafts(message.Content)
}

func collectForwardedMessageDrafts(messages []map[string]any) []forwardedMessageDraft {
	lines := make([]forwardedMessageDraft, 0, len(messages))
	for _, typed := range messages {
		if strings.TrimSpace(stringValue(typed["tag"])) == "combined_forwarded_chat_history" {
			lines = append(lines, collectForwardedMessageDrafts(extractNestedForwardedMessages(typed))...)
			continue
		}

		message, ok := normalizeForwardedMessageMap(typed)
		if !ok {
			continue
		}
		lines = append(lines, message)
	}

	return lines
}

func normalizeForwardedMessageMap(message map[string]any) (forwardedMessageDraft, bool) {
	referenced := forwardedMessageDraft{
		message: agent.ReferencedMessage{
			Sender:     extractForwardedSender(message["sender"]),
			SentAtUnix: extractUnixTimestamp(message["message_sent_time"]),
		},
	}

	switch strings.TrimSpace(stringValue(message["tag"])) {
	case "text":
		text := extractForwardedTextContent(message["text"])
		if text == "" {
			return forwardedMessageDraft{}, false
		}
		referenced.message.Kind = agent.MessageKindText
		referenced.message.Text = text
		return referenced, true
	case "image":
		imageURL := extractForwardedImageURL(message["image"])
		referenced.message.Kind = agent.MessageKindImage
		if imageURL != "" {
			referenced.imageURLs = []string{imageURL}
		}
		return referenced, true
	case "file":
		referenced.message.Kind = agent.MessageKindFile
		fileAttachment := extractForwardedFileAttachment(message["file"])
		referenced.message.Text = fileAttachment.Filename
		if fileAttachment.URL != "" || fileAttachment.Filename != "" {
			referenced.fileAttachments = []seatalkFileAttachment{fileAttachment}
		}
		return referenced, true
	case "video":
		referenced.message.Kind = agent.MessageKindVideo
		videoAttachment := extractForwardedFileAttachment(message["video"])
		if videoAttachment.URL != "" || videoAttachment.Filename != "" {
			referenced.videoAttachments = []seatalkFileAttachment{videoAttachment}
		}
		return referenced, true
	case "interactive_message":
		summary, imageURLs := extractForwardedInteractiveContent(message["interactive_message"])
		referenced.message.Kind = agent.MessageKindInteractiveCard
		referenced.message.Text = summary
		referenced.imageURLs = compactURLs(imageURLs)
		if referenced.message.Text == "" && len(referenced.imageURLs) == 0 {
			return forwardedMessageDraft{}, false
		}
		return referenced, true
	default:
		return forwardedMessageDraft{}, false
	}
}

func extractForwardedImageURL(value any) string {
	fields, ok := value.(map[string]any)
	if !ok {
		return ""
	}
	return strings.TrimSpace(stringValue(fields["content"]))
}

func extractForwardedFileAttachment(value any) seatalkFileAttachment {
	fields, ok := value.(map[string]any)
	if !ok {
		return seatalkFileAttachment{}
	}

	return seatalkFileAttachment{
		URL:      strings.TrimSpace(stringValue(fields["content"])),
		Filename: strings.TrimSpace(stringValue(fields["filename"])),
	}
}

func extractNestedForwardedMessages(message map[string]any) []map[string]any {
	if nested, ok := message["combined_forwarded_chat_history"]; ok {
		if messages := decodeForwardedMessageList(nested); len(messages) > 0 {
			return messages
		}
	}
	if content, ok := message["content"]; ok {
		return decodeForwardedMessageList(content)
	}
	return nil
}

func decodeForwardedMessageList(value any) []map[string]any {
	switch typed := value.(type) {
	case []map[string]any:
		return typed
	case map[string]any:
		if content, ok := typed["content"]; ok {
			return decodeForwardedMessageList(content)
		}
	case []any:
		messages := make([]map[string]any, 0, len(typed))
		for _, item := range typed {
			fields, ok := item.(map[string]any)
			if !ok {
				continue
			}
			messages = append(messages, fields)
		}
		return messages
	}
	return nil
}

func extractForwardedTextContent(value any) string {
	fields, ok := value.(map[string]any)
	if !ok {
		return ""
	}

	text := strings.TrimSpace(stringValue(fields["content"]))
	if text != "" {
		return text
	}

	return strings.TrimSpace(stringValue(fields["plain_text"]))
}

func extractForwardedSender(value any) string {
	fields, ok := value.(map[string]any)
	if !ok {
		return ""
	}

	return formatCurrentMessageSender(
		stringValue(fields["email"]),
		stringValue(fields["seatalk_id"]),
		intValue(fields["sender_type"]),
	)
}

func extractForwardedInteractiveContent(value any) (string, []string) {
	fields, ok := value.(map[string]any)
	if !ok {
		return "", nil
	}

	rawElements, ok := fields["elements"].([]any)
	if !ok || len(rawElements) == 0 {
		return "", nil
	}

	elements := make([]json.RawMessage, 0, len(rawElements))
	for _, element := range rawElements {
		body, err := json.Marshal(element)
		if err != nil {
			continue
		}
		elements = append(elements, json.RawMessage(body))
	}
	if len(elements) == 0 {
		return "", nil
	}

	message := &seatalk.ThreadInteractiveMessage{Elements: elements}
	if mentioned := decodeForwardedMentionedEntities(fields["mentioned_list"]); len(mentioned) > 0 {
		message.MentionedList = mentioned
	}
	return summarizeInteractiveMessage(message), seatalk.ExtractInteractiveImageURLs(message)
}

func decodeForwardedMentionedEntities(value any) []seatalk.MentionedEntity {
	items, ok := value.([]any)
	if !ok {
		return nil
	}

	mentioned := make([]seatalk.MentionedEntity, 0, len(items))
	for _, item := range items {
		fields, ok := item.(map[string]any)
		if !ok {
			continue
		}
		mentioned = append(mentioned, seatalk.MentionedEntity{
			Username:     strings.TrimSpace(stringValue(fields["username"])),
			SeatalkID:    strings.TrimSpace(stringValue(fields["seatalk_id"])),
			EmployeeCode: strings.TrimSpace(stringValue(fields["employee_code"])),
			Email:        strings.TrimSpace(stringValue(fields["email"])),
		})
	}
	return mentioned
}

func summarizeInteractiveMessage(message *seatalk.ThreadInteractiveMessage) string {
	if message == nil {
		return ""
	}

	return expandMentionedText(strings.TrimSpace(seatalk.SummarizeInteractiveMessage(message)), message.MentionedList)
}

func extractUnixTimestamp(value any) int64 {
	switch typed := value.(type) {
	case int64:
		return typed
	case int:
		return int64(typed)
	case int32:
		return int64(typed)
	case float64:
		return int64(typed)
	case seatalk.UnixTimestamp:
		return int64(typed)
	case string:
		seconds, err := strconv.ParseInt(strings.TrimSpace(typed), 10, 64)
		if err != nil {
			return 0
		}
		return seconds
	}

	text := strings.TrimSpace(stringValue(value))
	if text == "" {
		return 0
	}
	seconds, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return 0
	}
	return seconds
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}

func intValue(value any) int {
	switch typed := value.(type) {
	case int:
		return typed
	case int32:
		return int(typed)
	case int64:
		return int(typed)
	case float64:
		return int(typed)
	default:
		return 0
	}
}
