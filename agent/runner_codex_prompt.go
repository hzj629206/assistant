package agent

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/hzj629206/assistant/agent/codex"
)

type promptAttachmentKind string

const (
	promptAttachmentImage promptAttachmentKind = "image"
	promptAttachmentFile  promptAttachmentKind = "file"
	promptAttachmentVideo promptAttachmentKind = "video"
)

type promptAttachmentRef struct {
	Kind promptAttachmentKind
	Path string
}

type turnPrompt struct {
	Text        string
	ImagePaths  []string
	Attachments []promptAttachmentRef
}

func buildCodexTurnInput(req TurnRequest, turnContext codexTurnContext) (codex.Input, error) {
	prompt := buildTurnPromptResult(req.Message)
	if len(prompt.ImagePaths) == 0 {
		return injectCodexInitialTurnContext(req, codex.TextInput(prompt.Text), turnContext)
	}

	items := make([]codex.UserInput, 0, 1+len(prompt.ImagePaths))
	if prompt.Text != "" {
		items = append(items, codex.UserInput{
			Type: codex.UserInputText,
			Text: prompt.Text,
		})
	}

	for _, imagePath := range prompt.ImagePaths {
		items = append(items, codex.UserInput{
			Type: codex.UserInputLocalImage,
			Path: imagePath,
		})
	}

	return injectCodexInitialTurnContext(req, codex.ItemsInput(items...), turnContext)
}

func injectCodexInitialTurnContext(req TurnRequest, input codex.Input, turnContext codexTurnContext) (codex.Input, error) {
	if req.Conversation.RunnerThreadID != "" {
		return input, nil
	}

	return injectCodexInitialPrompt(input, turnContext.prompts, turnContext.tools)
}

func injectCodexInitialPrompt(input codex.Input, prompts []string, tools []Tool) (codex.Input, error) {
	instruction, err := buildInitialInstruction(prompts, tools)
	if err != nil {
		return codex.Input{}, err
	}
	if instruction == "" {
		return input, nil
	}

	if len(input.Items) == 0 {
		return codex.TextInput(joinPromptBlocks(instruction, input.Text)), nil
	}

	items := append([]codex.UserInput(nil), input.Items...)
	for index := range items {
		if items[index].Type == codex.UserInputText {
			items[index].Text = joinPromptBlocks(instruction, items[index].Text)
			return codex.ItemsInput(items...), nil
		}
	}

	items = append([]codex.UserInput{{
		Type: codex.UserInputText,
		Text: instruction,
	}}, items...)

	return codex.ItemsInput(items...), nil
}

func buildInitialInstruction(prompts []string, tools []Tool) (string, error) {
	type toolSpec struct {
		Name         string `json:"name"`
		Description  string `json:"description"`
		InputSchema  any    `json:"input_schema"`
		OutputSchema any    `json:"output_schema,omitempty"`
	}

	parts := make([]string, 0, len(prompts)+1)
	parts = append(parts, prompts...)

	if len(tools) == 0 {
		return joinPromptBlocks(parts...), nil
	}

	specs := make([]toolSpec, 0, len(tools))
	for _, tool := range tools {
		specs = append(specs, toolSpec{
			Name:         tool.Name(),
			Description:  tool.Description(),
			InputSchema:  tool.InputSchema(),
			OutputSchema: tool.OutputSchema(),
		})
	}

	body, err := json.MarshalIndent(specs, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encode tool catalog failed: %w", err)
	}

	parts = append(parts, strings.TrimSpace(`
You are operating in a structured tool loop.

Available tools:
`+string(body)+`

You must always return valid JSON that matches the provided schema.

Rules:
- Use {"action":"call_tool","tool_name":"...","tool_input_json":"{...}"} when a tool is required.
- The value of "tool_input_json" must be a JSON object encoded as a string.
- Use {"action":"respond","message":"..."} only when you can fully answer the user.
- Use {"action":"silent"} when the task is complete and no user-facing reply should be sent.
- Always include all schema fields. Use empty strings for fields that do not apply.
- Do not describe tool calls in prose outside the JSON response.
`))

	return joinPromptBlocks(parts...), nil
}

func buildTurnPromptResult(message InboundMessage) turnPrompt {
	var prompt string
	var imagePaths []string
	if len(message.mergedMessages) > 0 {
		prompt, imagePaths = buildCompositeTurnPrompt(message)
	} else {
		prompt, imagePaths = buildSingleTurnPrompt(message, true)
	}

	return turnPrompt{
		Text:        prompt,
		ImagePaths:  imagePaths,
		Attachments: collectPromptAttachments(message),
	}
}

func collectPromptAttachments(message InboundMessage) []promptAttachmentRef {
	seen := make(map[string]struct{})
	attachments := make([]promptAttachmentRef, 0)
	appendUnique := func(kind promptAttachmentKind, paths []string) {
		for _, path := range paths {
			path = strings.TrimSpace(path)
			if path == "" {
				continue
			}
			key := string(kind) + "\x00" + path
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			attachments = append(attachments, promptAttachmentRef{Kind: kind, Path: path})
		}
	}

	var walkReferenced func(ReferencedMessage)
	var walkInbound func(InboundMessage)

	walkReferenced = func(current ReferencedMessage) {
		appendUnique(promptAttachmentImage, allImagePaths(current.ImagePath, current.ImagePaths))
		appendUnique(promptAttachmentFile, allFilePaths(current.FilePath, current.FilePaths))
		appendUnique(promptAttachmentVideo, allVideoPaths(current.VideoPath, current.VideoPaths))
		for _, forwarded := range current.ForwardedMessages {
			walkReferenced(forwarded)
		}
	}

	walkInbound = func(current InboundMessage) {
		appendUnique(promptAttachmentImage, allImagePaths(current.ImagePath, current.ImagePaths))
		appendUnique(promptAttachmentFile, allFilePaths(current.FilePath, current.FilePaths))
		appendUnique(promptAttachmentVideo, allVideoPaths(current.VideoPath, current.VideoPaths))
		if current.QuotedMessage != nil {
			walkReferenced(*current.QuotedMessage)
		}
		for _, historical := range current.HistoricalMessages() {
			walkInbound(historical)
		}
		for _, merged := range current.MergedMessages() {
			walkInbound(merged)
		}
		for _, forwarded := range current.ForwardedMessages {
			walkReferenced(forwarded)
		}
	}

	walkInbound(message)
	return attachments
}

func buildSingleTurnPrompt(message InboundMessage, includeReplyMention bool) (string, []string) {
	var parts []string
	var imagePaths []string

	appendConversationContextParts(&parts, &imagePaths, message)
	if currentMessageContext := formatCurrentMessageContext(message, includeReplyMention); currentMessageContext != "" {
		parts = append(parts, currentMessageContext)
	}

	if message.QuotedMessage != nil {
		part, refs := formatReferencedMessage(*message.QuotedMessage)
		if part != "" {
			parts = append(parts, part)
		}
		imagePaths = append(imagePaths, refs...)
	}

	currentImagePaths := allImagePaths(message.ImagePath, message.ImagePaths)
	currentFilePaths := allFilePaths(message.FilePath, message.FilePaths)
	currentVideoPaths := allVideoPaths(message.VideoPath, message.VideoPaths)
	switch message.Kind {
	case MessageKindForwarded:
		part, refs := formatForwardedMessages("User sent combined forwarded chat history.", message.ForwardedMessages)
		if part != "" {
			parts = append(parts, part)
		} else {
			parts = append(parts, "User sent combined forwarded chat history.")
		}
		imagePaths = append(imagePaths, refs...)
	case MessageKindImage:
		if len(currentImagePaths) > 0 {
			parts = append(parts, "User sent an image.\nAttachment: current image")
			imagePaths = append(imagePaths, currentImagePaths...)
		}
	case MessageKindFile:
		if block := formatCurrentFileBlock(strings.TrimSpace(message.Text), currentFilePaths); block != "" {
			parts = append(parts, block)
		} else if placeholder := formatUnsupportedInboundPlaceholder(message.Kind); placeholder != "" {
			parts = append(parts, placeholder)
		}
	case MessageKindMixed:
		if block := formatCurrentMixedBlock(strings.TrimSpace(message.Text), currentImagePaths, currentFilePaths, currentVideoPaths); block != "" {
			parts = append(parts, block)
			imagePaths = append(imagePaths, currentImagePaths...)
		}
	case MessageKindInteractiveCard:
		block := make([]string, 0, 2)
		if text := strings.TrimSpace(message.Text); text != "" {
			block = append(block, "User sent an interactive message card.\nContent: "+text)
		}
		if len(currentImagePaths) > 0 {
			block = append(block, formatCurrentImageAttachmentBlock(len(currentImagePaths)))
			imagePaths = append(imagePaths, currentImagePaths...)
		}
		if len(block) > 0 {
			parts = append(parts, strings.Join(block, "\n"))
		}
		if len(block) == 0 {
			if placeholder := formatUnsupportedInboundPlaceholder(message.Kind); placeholder != "" {
				parts = append(parts, placeholder)
			}
		}
	case MessageKindVideo:
		if block := formatCurrentVideoBlock(currentVideoPaths); block != "" {
			parts = append(parts, block)
		} else if placeholder := formatUnsupportedInboundPlaceholder(message.Kind); placeholder != "" {
			parts = append(parts, placeholder)
		}
	default:
		if message.Text != "" {
			parts = append(parts, message.Text)
		}
	}

	return strings.TrimSpace(strings.Join(parts, "\n\n")), imagePaths
}

// buildCompositeTurnPrompt renders a batched turn where the real current-message content lives in mergedMessages.
// In this mode, the top-level InboundMessage is a container produced by combineInboundMessages rather than a
// standalone current message:
//   - mergedMessages contains all current inbound messages in arrival order and is the source of per-message
//     content, sender metadata, timestamps, and reply mention hints rendered in the prompt.
//   - initialContext and historicalMessages still belong to the current turn and should be rendered once before
//     the merged current messages.
//   - prompt generation here must not depend on any top-level fields other than mergedMessages, initialContext,
//     and historicalMessages.
//   - Other top-level non-content fields belong to the dispatcher/runner envelope as usual.
func buildCompositeTurnPrompt(message InboundMessage) (string, []string) {
	var parts []string
	imagePaths := make([]string, 0, len(message.historicalMessages)+len(message.mergedMessages))

	appendConversationContextParts(&parts, &imagePaths, message)

	currentMessages := message.mergedMessages
	switch len(currentMessages) {
	case 0:
	case 1:
		// A single merged message is not the normal dispatcher output, but keep this branch so prompt rendering
		// remains robust if another caller wraps one current message in a batch container.
		part, refs := buildSingleTurnPrompt(currentMessages[0], true)
		if part != "" {
			parts = append(parts, part)
		}
		imagePaths = append(imagePaths, refs...)
	default:
		parts = append(parts, "Multiple new messages arrived while the assistant was busy. Process them together in order.\n")
		for index, current := range currentMessages {
			part, refs := buildPromptBlock("Message "+strconv.Itoa(index+1), current, true)
			if part != "" {
				parts = append(parts, part)
			}
			imagePaths = append(imagePaths, refs...)
		}
	}

	return strings.TrimSpace(strings.Join(parts, "\n\n")), imagePaths
}

func appendConversationContextParts(parts *[]string, imagePaths *[]string, message InboundMessage) {
	if message.initialContext != "" {
		*parts = append(*parts, "Conversation context:\n"+message.initialContext)
	}

	if len(message.historicalMessages) == 0 {
		return
	}

	*parts = append(*parts, "Earlier messages from the current conversation are included below for context.")
	for index, current := range message.historicalMessages {
		part, refs := buildPromptBlock("History message "+strconv.Itoa(index+1), current, false)
		if part != "" {
			*parts = append(*parts, part)
		}
		*imagePaths = append(*imagePaths, refs...)
	}
}

// buildPromptBlock renders one ordinary message entry inside a larger prompt and relies only on that message's
// own content fields. Turn-level context such as initialContext, historicalMessages, and mergedMessages must be
// handled by the caller before invoking this helper.
func buildPromptBlock(label string, message InboundMessage, includeReplyMention bool) (string, []string) {
	part, refs := buildSingleTurnPrompt(message, includeReplyMention)
	if part == "" {
		return "", refs
	}

	return label + ":\n" + part, refs
}

func formatCurrentMessageContext(message InboundMessage, includeReplyMention bool) string {
	lines := []string{
		"Current message context:",
		"- time: " + formatCurrentMessageTime(message.SentAtUnix),
		"- sender: `" + formatCurrentMessageSenderName(message.Sender) + "`",
	}
	if includeReplyMention {
		if mentionHint := strings.TrimSpace(message.SenderMentionHint); mentionHint != "" {
			lines = append(lines, "- sender mention hint: `"+mentionHint+"`")
		}
	}
	if messageTags := formatMessageTags(message.MessageTags); messageTags != "" {
		lines = append(lines, "- tags:\n"+messageTags)
	}
	return strings.Join(lines, "\n")
}

func formatMessageTags(tags []string) string {
	if len(tags) == 0 {
		return ""
	}

	lines := make([]string, 0, len(tags))
	for _, tag := range tags {
		tag = strings.TrimSpace(tag)
		if tag == "" {
			continue
		}
		lines = append(lines, "  - "+tag)
	}
	return strings.Join(lines, "\n")
}

func formatCurrentMessageTime(timestamp int64) string {
	if timestamp <= 0 {
		return "unknown"
	}
	return time.Unix(timestamp, 0).In(time.Local).Format(time.RFC3339) //nolint:gosmopolitan // Personal project: timestamps should follow the local machine timezone.
}

func formatCurrentMessageSenderName(sender string) string {
	sender = strings.TrimSpace(sender)
	if sender == "" {
		return "unknown"
	}
	return sender
}

func formatReferencedMessage(message ReferencedMessage) (string, []string) {
	referencedImagePaths := allImagePaths(message.ImagePath, message.ImagePaths)
	referencedFilePaths := allFilePaths(message.FilePath, message.FilePaths)
	referencedVideoPaths := allVideoPaths(message.VideoPath, message.VideoPaths)
	switch message.Kind {
	case MessageKindForwarded:
		part, refs := formatForwardedMessages("Quoted message:\nType: combined_forwarded_chat_history", message.ForwardedMessages)
		if part != "" {
			return part, refs
		}
		return "Quoted message:\nType: combined_forwarded_chat_history", nil
	case MessageKindImage:
		if len(referencedImagePaths) == 0 {
			return "", nil
		}
		return "Quoted message:\nType: image\nAttachment: quoted image", referencedImagePaths
	case MessageKindMixed:
		if block := formatQuotedMixedBlock(strings.TrimSpace(message.Text), referencedImagePaths, referencedFilePaths, referencedVideoPaths); block != "" {
			return block, referencedImagePaths
		}
		return "", nil
	case MessageKindInteractiveCard:
		if text := strings.TrimSpace(message.Text); text != "" {
			parts := []string{"Quoted message:\nType: interactive_card", "Content: " + text}
			if len(referencedImagePaths) > 0 {
				parts = append(parts, formatQuotedImageAttachmentLine(len(referencedImagePaths)))
			}
			return strings.Join(parts, "\n"), referencedImagePaths
		}
		if len(referencedImagePaths) > 0 {
			return "Quoted message:\nType: interactive_card\n" + formatQuotedImageAttachmentLine(len(referencedImagePaths)), referencedImagePaths
		}
		placeholder := formatUnsupportedQuotedPlaceholder(message.Kind)
		if placeholder == "" {
			return "", nil
		}
		return "Quoted message:\n" + placeholder, nil
	case MessageKindFile, MessageKindVideo:
		if message.Kind == MessageKindFile {
			if block := formatQuotedFileBlock(strings.TrimSpace(message.Text), referencedFilePaths); block != "" {
				return block, nil
			}
		} else if block := formatQuotedVideoBlock(referencedVideoPaths); block != "" {
			return block, nil
		}
		placeholder := formatUnsupportedQuotedPlaceholder(message.Kind)
		if placeholder == "" {
			return "", nil
		}
		return "Quoted message:\n" + placeholder, nil
	default:
		if message.Text == "" {
			return "", nil
		}
		return "Quoted message:\nType: text\nContent: " + message.Text, nil
	}
}

func formatForwardedMessages(prefix string, messages []ReferencedMessage) (string, []string) {
	if len(messages) == 0 {
		return "", nil
	}

	parts := make([]string, 0, len(messages)+1)
	if trimmed := strings.TrimSpace(prefix); trimmed != "" {
		parts = append(parts, trimmed)
	}
	imagePaths := make([]string, 0, len(messages))
	for index, message := range messages {
		part, refs := formatForwardedMessageBlock(index+1, message)
		if part != "" {
			parts = append(parts, part)
		}
		imagePaths = append(imagePaths, refs...)
	}
	if len(parts) == 0 {
		return "", imagePaths
	}
	return strings.Join(parts, "\n\n"), imagePaths
}

func formatForwardedMessageBlock(index int, message ReferencedMessage) (string, []string) {
	lines := []string{"Forwarded message " + strconv.Itoa(index) + ":"}
	lines = append(lines, "- time: "+formatCurrentMessageTime(message.SentAtUnix))
	lines = append(lines, "- sender: "+formatCurrentMessageSenderName(message.Sender))

	body, refs := formatForwardedMessageContent(message)
	if body != "" {
		lines = append(lines, body)
	}

	return strings.Join(lines, "\n"), refs
}

func formatForwardedMessageContent(message ReferencedMessage) (string, []string) {
	referencedImagePaths := allImagePaths(message.ImagePath, message.ImagePaths)
	referencedFilePaths := allFilePaths(message.FilePath, message.FilePaths)
	referencedVideoPaths := allVideoPaths(message.VideoPath, message.VideoPaths)
	switch message.Kind {
	case MessageKindForwarded:
		if part, refs := formatForwardedMessages("Content:", message.ForwardedMessages); part != "" {
			return part, refs
		}
		return "Type: combined_forwarded_chat_history", nil
	case MessageKindText:
		if text := strings.TrimSpace(message.Text); text != "" {
			return "Content: " + text, nil
		}
	case MessageKindImage:
		if len(referencedImagePaths) > 0 {
			return "Type: image\nAttachment: forwarded image", referencedImagePaths
		}
		return "Type: image", nil
	case MessageKindMixed:
		if block := formatForwardedMixedBlock(strings.TrimSpace(message.Text), referencedImagePaths, referencedFilePaths, referencedVideoPaths); block != "" {
			return block, referencedImagePaths
		}
		return "", referencedImagePaths
	case MessageKindInteractiveCard:
		parts := []string{"Type: interactive_card"}
		if text := strings.TrimSpace(message.Text); text != "" {
			parts = append(parts, "Content: "+text)
		}
		if len(referencedImagePaths) > 0 {
			parts = append(parts, formatForwardedImageAttachmentBlock(len(referencedImagePaths)))
		}
		return strings.Join(parts, "\n"), referencedImagePaths
	case MessageKindFile:
		if block := formatForwardedFileBlock(strings.TrimSpace(message.Text), referencedFilePaths); block != "" {
			return block, nil
		}
		return "Type: file\nContent: <file>", nil
	case MessageKindVideo:
		if block := formatForwardedVideoBlock(referencedVideoPaths); block != "" {
			return block, nil
		}
		return "Type: video\nContent: <video>", nil
	}

	return "", referencedImagePaths
}

func formatForwardedImageAttachmentBlock(count int) string {
	if count <= 1 {
		return "Attachment: forwarded image"
	}
	return "Attachments: forwarded images (" + strconv.Itoa(count) + ")"
}

func formatCurrentImageAttachmentBlock(count int) string {
	if count <= 1 {
		return "Attachment: current image"
	}
	return "Attachments: current images (" + strconv.Itoa(count) + ")"
}

func formatQuotedImageAttachmentLine(count int) string {
	if count <= 1 {
		return "Attachment: quoted image"
	}
	return "Attachments: quoted images (" + strconv.Itoa(count) + ")"
}

func formatCurrentMixedBlock(text string, imagePaths, filePaths, videoPaths []string) string {
	parts := []string{"User sent a mixed message."}
	if text != "" {
		parts = append(parts, "Content: "+text)
	}
	if len(imagePaths) > 0 {
		parts = append(parts, formatCurrentImageAttachmentBlock(len(imagePaths)))
	}
	if len(filePaths) > 0 {
		parts = append(parts, formatCurrentFileAttachmentLines(len(filePaths), filePaths)...)
	}
	if len(videoPaths) > 0 {
		parts = append(parts, formatCurrentVideoAttachmentLines(len(videoPaths), videoPaths)...)
	}
	if len(parts) == 1 {
		return ""
	}
	return strings.Join(parts, "\n")
}

func formatQuotedMixedBlock(text string, imagePaths, filePaths, videoPaths []string) string {
	parts := []string{"Quoted message:\nType: mixed"}
	if text != "" {
		parts = append(parts, "Content: "+text)
	}
	if len(imagePaths) > 0 {
		parts = append(parts, formatQuotedImageAttachmentLine(len(imagePaths)))
	}
	if len(filePaths) > 0 {
		parts = append(parts, formatQuotedFileAttachmentLines(len(filePaths), filePaths)...)
	}
	if len(videoPaths) > 0 {
		parts = append(parts, formatQuotedVideoAttachmentLines(len(videoPaths), videoPaths)...)
	}
	if len(parts) == 1 {
		return ""
	}
	return strings.Join(parts, "\n")
}

func formatForwardedMixedBlock(text string, imagePaths, filePaths, videoPaths []string) string {
	parts := []string{"Type: mixed"}
	if text != "" {
		parts = append(parts, "Content: "+text)
	}
	if len(imagePaths) > 0 {
		parts = append(parts, formatForwardedImageAttachmentBlock(len(imagePaths)))
	}
	if len(filePaths) > 0 {
		parts = append(parts, formatForwardedFileAttachmentLines(len(filePaths), filePaths)...)
	}
	if len(videoPaths) > 0 {
		parts = append(parts, formatForwardedVideoAttachmentLines(len(videoPaths), videoPaths)...)
	}
	if len(parts) == 1 {
		return ""
	}
	return strings.Join(parts, "\n")
}

func formatCurrentFileBlock(filename string, filePaths []string) string {
	parts := []string{"User sent a file."}
	if filename != "" {
		parts = append(parts, "Filename: "+filename)
	}
	parts = append(parts, formatCurrentFileAttachmentLines(len(filePaths), filePaths)...)
	if len(parts) == 1 && filename == "" {
		return ""
	}
	return strings.Join(parts, "\n")
}

func formatQuotedFileBlock(filename string, filePaths []string) string {
	parts := []string{"Quoted message:\nType: file"}
	if filename != "" {
		parts = append(parts, "Filename: "+filename)
	}
	parts = append(parts, formatQuotedFileAttachmentLines(len(filePaths), filePaths)...)
	if len(parts) == 1 {
		return ""
	}
	return strings.Join(parts, "\n")
}

func formatForwardedFileBlock(filename string, filePaths []string) string {
	parts := []string{"Type: file"}
	if filename != "" {
		parts = append(parts, "Filename: "+filename)
	}
	parts = append(parts, formatForwardedFileAttachmentLines(len(filePaths), filePaths)...)
	if len(parts) == 1 {
		return ""
	}
	return strings.Join(parts, "\n")
}

func formatCurrentVideoBlock(videoPaths []string) string {
	parts := make([]string, 1, 4)
	parts[0] = "User sent a video."
	parts = append(parts, formatCurrentVideoAttachmentLines(len(videoPaths), videoPaths)...)
	if len(parts) == 1 {
		return ""
	}
	return strings.Join(parts, "\n")
}

func formatQuotedVideoBlock(videoPaths []string) string {
	parts := make([]string, 1, 4)
	parts[0] = "Quoted message:\nType: video"
	parts = append(parts, formatQuotedVideoAttachmentLines(len(videoPaths), videoPaths)...)
	if len(parts) == 1 {
		return ""
	}
	return strings.Join(parts, "\n")
}

func formatForwardedVideoBlock(videoPaths []string) string {
	parts := make([]string, 1, 4)
	parts[0] = "Type: video"
	parts = append(parts, formatForwardedVideoAttachmentLines(len(videoPaths), videoPaths)...)
	if len(parts) == 1 {
		return ""
	}
	return strings.Join(parts, "\n")
}

func formatCurrentFileAttachmentLines(count int, filePaths []string) []string {
	if count == 1 {
		return []string{"Attachment: current file", "Local path: " + filePaths[0], temporaryFilePathNotice()}
	}
	if count > 1 {
		return []string{"Attachments: current files (" + strconv.Itoa(count) + ")", "Local paths: " + strings.Join(filePaths, ", "), temporaryFilePathNotice()}
	}
	return nil
}

func formatQuotedFileAttachmentLines(count int, filePaths []string) []string {
	if count == 1 {
		return []string{"Attachment: quoted file", "Local path: " + filePaths[0], temporaryFilePathNotice()}
	}
	if count > 1 {
		return []string{"Attachments: quoted files (" + strconv.Itoa(count) + ")", "Local paths: " + strings.Join(filePaths, ", "), temporaryFilePathNotice()}
	}
	return nil
}

func formatForwardedFileAttachmentLines(count int, filePaths []string) []string {
	if count == 1 {
		return []string{"Attachment: forwarded file", "Local path: " + filePaths[0], temporaryFilePathNotice()}
	}
	if count > 1 {
		return []string{"Attachments: forwarded files (" + strconv.Itoa(count) + ")", "Local paths: " + strings.Join(filePaths, ", "), temporaryFilePathNotice()}
	}
	return nil
}

func formatCurrentVideoAttachmentLines(count int, videoPaths []string) []string {
	if count == 1 {
		return []string{"Attachment: current video", "Local path: " + videoPaths[0], temporaryFilePathNotice()}
	}
	if count > 1 {
		return []string{"Attachments: current videos (" + strconv.Itoa(count) + ")", "Local paths: " + strings.Join(videoPaths, ", "), temporaryFilePathNotice()}
	}
	return nil
}

func formatQuotedVideoAttachmentLines(count int, videoPaths []string) []string {
	if count == 1 {
		return []string{"Attachment: quoted video", "Local path: " + videoPaths[0], temporaryFilePathNotice()}
	}
	if count > 1 {
		return []string{"Attachments: quoted videos (" + strconv.Itoa(count) + ")", "Local paths: " + strings.Join(videoPaths, ", "), temporaryFilePathNotice()}
	}
	return nil
}

func formatForwardedVideoAttachmentLines(count int, videoPaths []string) []string {
	if count == 1 {
		return []string{"Attachment: forwarded video", "Local path: " + videoPaths[0], temporaryFilePathNotice()}
	}
	if count > 1 {
		return []string{"Attachments: forwarded videos (" + strconv.Itoa(count) + ")", "Local paths: " + strings.Join(videoPaths, ", "), temporaryFilePathNotice()}
	}
	return nil
}

func formatUnsupportedInboundPlaceholder(kind MessageKind) string {
	switch kind {
	case MessageKindFile:
		return "User sent a file attachment, but no local file path is available.\nDo not reply solely because of this placeholder."
	case MessageKindVideo:
		return "User sent a video attachment, but no local file path is available.\nDo not reply solely because of this placeholder."
	case MessageKindInteractiveCard:
		return "User sent an interactive message card that is not currently parsed.\nDo not reply solely because of this placeholder."
	default:
		return ""
	}
}

func formatUnsupportedQuotedPlaceholder(kind MessageKind) string {
	switch kind {
	case MessageKindFile:
		return "Type: file\nContent: <file>"
	case MessageKindVideo:
		return "Type: video\nContent: <video>"
	case MessageKindInteractiveCard:
		return "Type: interactive_card\nContent: <interactive card>"
	default:
		return ""
	}
}
