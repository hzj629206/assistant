package agent

import (
	"context"
	"encoding/json"
	"log"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

type turnRequestContextKey struct{}

// Tool describes one server-side capability exposed through the prompt-based tool loop.
type Tool interface {
	Name() string
	Description() string
	InputSchema() any
	OutputSchema() any
	Call(ctx context.Context, input json.RawMessage) (any, error)
}

// Runner executes one agent turn against a conversation state.
type Runner interface {
	RunTurn(ctx context.Context, req TurnRequest) (TurnResult, error)
	RegisterSystemPrompt(prompt string)
	RegisterTools(tools ...Tool)
}

// ContextWithTurnRequest stores the current turn request in context for tool calls.
func ContextWithTurnRequest(ctx context.Context, req TurnRequest) context.Context {
	return context.WithValue(ctx, turnRequestContextKey{}, req)
}

// TurnRequestFromContext returns the turn request attached to a tool call context.
func TurnRequestFromContext(ctx context.Context) (TurnRequest, bool) {
	if ctx == nil {
		return TurnRequest{}, false
	}

	req, ok := ctx.Value(turnRequestContextKey{}).(TurnRequest)
	return req, ok
}

// NoopRunner is a lightweight runner that echoes messages and can dump registered context.
type NoopRunner struct {
	mu            sync.RWMutex
	systemPrompts []string
	tools         []Tool
}

var orderedListPrefixPattern = regexp.MustCompile(`^(\s*)(\d+)\. `)
var noopRunnerDebugCategories = []string{"conversation", "message", "history", "merged", "prompts", "tools", "full"}

// RunTurn records the turn and returns a simple placeholder reply.
func (r *NoopRunner) RunTurn(ctx context.Context, req TurnRequest) (TurnResult, error) {
	historicalMessages := req.Message.HistoricalMessages()
	mergedMessages := req.Message.MergedMessages()
	log.Printf(
		"agent noop runner: conversation=%s kind=%s text=%q image_path=%q image_count=%d forwarded_count=%d history_count=%d merged_count=%d",
		req.Conversation.Key,
		req.Message.Kind,
		req.Message.Text,
		req.Message.ImagePath,
		len(allImagePaths(req.Message.ImagePath, req.Message.ImagePaths)),
		len(req.Message.ForwardedMessages),
		len(historicalMessages),
		len(mergedMessages),
	)

	result := TurnResult{}
	debugCategory, isDebug := parseNoopDebugCommand(firstDebugCommand(req.Message))
	if isDebug {
		payload, err := r.debugPayload(req, debugCategory)
		if err != nil {
			return result, err
		}
		result.ReplyText = formatSeaTalkMarkdownCodeBlock(payload)
		return result, nil
	}

	if len(mergedMessages) > 0 {
		result.ReplyText = escapeSeaTalkMarkdownText(latestTextReply(mergedMessages))
		if result.ReplyText != "" {
			delay, replyText, ok := parseNoopDelayedReply(result.ReplyText)
			if ok {
				if err := waitNoopReplyDelay(ctx, delay); err != nil {
					return TurnResult{}, err
				}
				result.ReplyText = replyText
			}
			return result, nil
		}
	}
	if req.Message.Kind == MessageKindText {
		result.ReplyText = escapeSeaTalkMarkdownText(req.Message.Text)
	}
	if req.Message.Kind == MessageKindForwarded {
		result.ReplyText = escapeSeaTalkMarkdownText("<forwarded_messages>")
	}
	if req.Message.Kind == MessageKindImage {
		result.ReplyText = escapeSeaTalkMarkdownText("<image>")
	}
	if req.Message.Kind == MessageKindMixed {
		result.ReplyText = escapeSeaTalkMarkdownText(req.Message.Text)
	}
	if result.ReplyText == "" && len(req.Message.ForwardedMessages) > 0 {
		result.ReplyText = escapeSeaTalkMarkdownText("<forwarded_messages>")
	}

	return applyNoopReplyDelay(ctx, result)
}

type noopRunnerToolSpec struct {
	Name         string `json:"name"`
	Description  string `json:"description"`
	InputSchema  any    `json:"input_schema"`
	OutputSchema any    `json:"output_schema,omitempty"`
}

type noopRunnerDebugMessage struct {
	ID                 string                   `json:"id,omitempty"`
	ConversationKey    string                   `json:"conversation_key,omitempty"`
	Kind               MessageKind              `json:"kind"`
	Text               string                   `json:"text,omitempty"`
	InitialContext     string                   `json:"initial_context,omitempty"`
	ImagePath          string                   `json:"image_path,omitempty"`
	ImagePaths         []string                 `json:"image_paths,omitempty"`
	QuotedMessage      *ReferencedMessage       `json:"quoted_message,omitempty"`
	ForwardedMessages  []ReferencedMessage      `json:"forwarded_messages,omitempty"`
	HistoricalMessages []noopRunnerDebugMessage `json:"historical_messages,omitempty"`
	MergedMessages     []noopRunnerDebugMessage `json:"merged_messages,omitempty"`
}

type noopRunnerDebugPayload struct {
	Runner        string                 `json:"runner"`
	Conversation  ConversationState      `json:"conversation"`
	Message       noopRunnerDebugMessage `json:"message"`
	SystemPrompts []string               `json:"system_prompts,omitempty"`
	Tools         []noopRunnerToolSpec   `json:"tools,omitempty"`
}

func (r *NoopRunner) debugPayload(req TurnRequest, category string) (string, error) {
	prompts, tools := r.globalContext()

	toolSpecs := make([]noopRunnerToolSpec, 0, len(tools))
	for _, tool := range tools {
		toolSpecs = append(toolSpecs, noopRunnerToolSpec{
			Name:         tool.Name(),
			Description:  tool.Description(),
			InputSchema:  tool.InputSchema(),
			OutputSchema: tool.OutputSchema(),
		})
	}

	payload := noopRunnerDebugPayload{
		Runner:        "noop",
		Conversation:  req.Conversation,
		Message:       buildNoopRunnerDebugMessage(req.Message),
		SystemPrompts: prompts,
		Tools:         toolSpecs,
	}

	if category == "" {
		return renderNoopDebugSummary(payload), nil
	}

	var value any
	switch category {
	case "conversation":
		value = payload.Conversation
	case "message":
		value = buildNoopRunnerDebugMessageWithoutContext(req.Message)
	case "history":
		value = payload.Message.HistoricalMessages
	case "merged":
		value = payload.Message.MergedMessages
	case "prompts":
		value = payload.SystemPrompts
	case "tools":
		value = payload.Tools
	case "full":
		value = payload
	default:
		return renderNoopDebugUnknownCategory(category), nil
	}

	body, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return "", err
	}

	return string(body), nil
}

func buildNoopRunnerDebugMessage(message InboundMessage) noopRunnerDebugMessage {
	debugMessage := noopRunnerDebugMessage{
		ID:              message.ID,
		ConversationKey: message.ConversationKey,
		Kind:            message.Kind,
		Text:            message.Text,
		InitialContext:  message.InitialContext(),
		ImagePath:       message.ImagePath,
		ImagePaths:      append([]string(nil), message.ImagePaths...),
		QuotedMessage:   message.QuotedMessage,
	}
	if len(message.ForwardedMessages) > 0 {
		debugMessage.ForwardedMessages = append([]ReferencedMessage(nil), message.ForwardedMessages...)
	}
	if historicalMessages := message.HistoricalMessages(); len(historicalMessages) > 0 {
		debugMessage.HistoricalMessages = make([]noopRunnerDebugMessage, 0, len(historicalMessages))
		for _, current := range historicalMessages {
			debugMessage.HistoricalMessages = append(debugMessage.HistoricalMessages, buildNoopRunnerDebugMessage(current))
		}
	}
	if mergedMessages := message.MergedMessages(); len(mergedMessages) > 0 {
		debugMessage.MergedMessages = make([]noopRunnerDebugMessage, 0, len(mergedMessages))
		for _, current := range mergedMessages {
			debugMessage.MergedMessages = append(debugMessage.MergedMessages, buildNoopRunnerDebugMessage(current))
		}
	}
	return debugMessage
}

func buildNoopRunnerDebugMessageWithoutContext(message InboundMessage) noopRunnerDebugMessage {
	debugMessage := buildNoopRunnerDebugMessage(message)
	debugMessage.HistoricalMessages = nil
	debugMessage.MergedMessages = nil
	return debugMessage
}

func firstDebugCommand(message InboundMessage) string {
	if len(message.MergedMessages()) > 0 {
		for _, current := range message.MergedMessages() {
			if candidate := firstDebugCommand(current); candidate != "" {
				return candidate
			}
		}
	}
	return strings.TrimSpace(message.Text)
}

func parseNoopDebugCommand(text string) (string, bool) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "/debug" {
		return "", true
	}
	if !strings.HasPrefix(trimmed, "/debug ") {
		return "", false
	}

	category := strings.TrimSpace(strings.TrimPrefix(trimmed, "/debug"))
	if category == "" {
		return "", true
	}

	return strings.ToLower(category), true
}

func parseNoopDelayedReply(text string) (time.Duration, string, bool) {
	trimmed := strings.TrimSpace(text)
	if !strings.HasPrefix(trimmed, "/delay ") {
		return 0, text, false
	}

	remainder := strings.TrimSpace(strings.TrimPrefix(trimmed, "/delay"))
	delayField, replyText, ok := strings.Cut(remainder, " ")
	if !ok {
		return 0, text, false
	}

	delay, err := time.ParseDuration(delayField)
	if err != nil || delay < 0 {
		return 0, text, false
	}

	replyText = strings.TrimSpace(replyText)
	if replyText == "" {
		return 0, text, false
	}

	return delay, replyText, true
}

func waitNoopReplyDelay(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return nil
	}

	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func applyNoopReplyDelay(ctx context.Context, result TurnResult) (TurnResult, error) {
	delay, replyText, ok := parseNoopDelayedReply(result.ReplyText)
	if !ok {
		return result, nil
	}

	if err := waitNoopReplyDelay(ctx, delay); err != nil {
		return TurnResult{}, err
	}

	result.ReplyText = replyText
	return result, nil
}

func latestTextReply(messages []InboundMessage) string {
	for index := len(messages) - 1; index >= 0; index-- {
		current := messages[index]
		if current.Kind != MessageKindText && current.Kind != MessageKindMixed {
			continue
		}
		if text := strings.TrimSpace(current.Text); text != "" {
			return current.Text
		}
	}
	return ""
}

func escapeSeaTalkMarkdownText(text string) string {
	if text == "" {
		return ""
	}

	lines := strings.Split(text, "\n")
	for index, line := range lines {
		lines[index] = escapeSeaTalkMarkdownLine(line)
	}
	return strings.Join(lines, "\n")
}

func escapeSeaTalkMarkdownLine(line string) string {
	escaped := strings.NewReplacer(
		"\\", "\\\\",
		"`", "\\`",
		"*", "\\*",
		"_", "\\_",
	).Replace(line)

	if matches := orderedListPrefixPattern.FindStringSubmatch(escaped); matches != nil {
		prefix := matches[1] + matches[2] + `\. `
		return prefix + escaped[len(matches[0]):]
	}

	trimmed := strings.TrimLeft(escaped, " \t")
	indent := escaped[:len(escaped)-len(trimmed)]
	if strings.HasPrefix(trimmed, "- ") || strings.HasPrefix(trimmed, "* ") {
		return indent + `\` + trimmed
	}

	return escaped
}

func formatSeaTalkMarkdownCodeBlock(text string) string {
	safe := strings.ReplaceAll(text, "```", "``\\`")
	return "```\n" + safe + "\n```"
}

func renderNoopDebugSummary(payload noopRunnerDebugPayload) string {
	lines := []string{
		"Noop debug summary",
		"runner: " + payload.Runner,
		"conversation_key: " + payload.Conversation.Key,
		"message_kind: " + string(payload.Message.Kind),
		"history_count: " + strconv.Itoa(len(payload.Message.HistoricalMessages)),
		"merged_count: " + strconv.Itoa(len(payload.Message.MergedMessages)),
		"forwarded_count: " + strconv.Itoa(len(payload.Message.ForwardedMessages)),
		"system_prompt_count: " + strconv.Itoa(len(payload.SystemPrompts)),
		"tool_count: " + strconv.Itoa(len(payload.Tools)),
		"available_categories: " + strings.Join(noopRunnerDebugCategories, ", "),
		"use /debug <category> to inspect one category.",
	}
	return strings.Join(lines, "\n")
}

func renderNoopDebugUnknownCategory(category string) string {
	return "Unknown debug category: " + category + "\nAvailable categories: " + strings.Join(noopRunnerDebugCategories, ", ")
}

func (r *NoopRunner) globalContext() ([]string, []Tool) {
	if r == nil {
		return nil, nil
	}

	r.mu.RLock()
	defer r.mu.RUnlock()

	return append([]string(nil), r.systemPrompts...), append([]Tool(nil), r.tools...)
}

// RegisterSystemPrompt records one global prompt block for later debug inspection.
func (r *NoopRunner) RegisterSystemPrompt(prompt string) {
	if r == nil {
		return
	}

	trimmed := strings.TrimSpace(prompt)
	if trimmed == "" {
		return
	}

	r.mu.Lock()
	r.systemPrompts = append(r.systemPrompts, trimmed)
	r.mu.Unlock()
}

// RegisterTools records tools for later debug inspection.
func (r *NoopRunner) RegisterTools(tools ...Tool) {
	if r == nil || len(tools) == 0 {
		return
	}

	filtered := make([]Tool, 0, len(tools))
	for _, tool := range tools {
		if tool != nil {
			filtered = append(filtered, tool)
		}
	}
	if len(filtered) == 0 {
		return
	}

	r.mu.Lock()
	r.tools = append(r.tools, filtered...)
	r.mu.Unlock()
}
