package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/godeps/codex-sdk-go"
)

const defaultMaxToolIterations = 4
const defaultModel = "gpt-5.4"

// CodexRunner bridges dispatcher turns to the Codex CLI through codex-sdk-go.
type CodexRunner struct {
	client            *codex.Codex
	startThread       func(codex.ThreadOptions) codexThread
	resumeThread      func(string, codex.ThreadOptions) codexThread
	threadOptions     codex.ThreadOptions
	maxToolIterations int
	mu                sync.RWMutex
	systemPrompts     []string
	tools             []Tool
}

// CodexRunnerOptions configures a CodexRunner.
type CodexRunnerOptions struct {
	Client            *codex.Codex
	ThreadOptions     codex.ThreadOptions
	SystemPrompt      string
	Tools             []Tool
	MaxToolIterations int
}

// NewCodexRunner builds a runner backed by the Codex CLI.
func NewCodexRunner(options CodexRunnerOptions) *CodexRunner {
	client := options.Client
	if client == nil {
		client = codex.NewCodex(codex.CodexOptions{})
	}

	threadOptions := options.ThreadOptions
	if threadOptions.SandboxMode == "" {
		threadOptions.SandboxMode = codex.SandboxReadOnly
	}
	if threadOptions.WorkingDirectory == "" {
		workingDirectory, err := os.Getwd()
		if err == nil {
			threadOptions.WorkingDirectory = workingDirectory
		}
	}
	threadOptions.SkipGitRepoCheck = true
	if threadOptions.ApprovalPolicy == "" {
		threadOptions.ApprovalPolicy = codex.ApprovalNever
	}
	if threadOptions.NetworkAccessEnabled == nil {
		networkAccessEnabled := true
		threadOptions.NetworkAccessEnabled = &networkAccessEnabled
	}
	if threadOptions.WebSearchEnabled == nil {
		webSearchEnabled := true
		threadOptions.WebSearchEnabled = &webSearchEnabled
		threadOptions.WebSearchMode = codex.WebSearchLive
	}

	threadOptions.Model = defaultModel
	if threadOptions.ModelReasoningEffort == "" || threadOptions.ModelReasoningEffort == codex.ReasoningMinimal {
		// `gpt-5.4` doesn't support `minimal`.
		threadOptions.ModelReasoningEffort = codex.ReasoningMedium
	}

	maxToolIterations := options.MaxToolIterations
	if maxToolIterations <= 0 {
		maxToolIterations = defaultMaxToolIterations
	}

	runner := &CodexRunner{
		client:      client,
		startThread: func(options codex.ThreadOptions) codexThread { return client.StartThread(options) },
		resumeThread: func(threadID string, options codex.ThreadOptions) codexThread {
			return client.ResumeThread(threadID, options)
		},
		threadOptions:     threadOptions,
		maxToolIterations: maxToolIterations,
	}

	runner.RegisterSystemPrompt(options.SystemPrompt)
	runner.RegisterTools(options.Tools...)

	return runner
}

// RegisterSystemPrompt appends one global system prompt block for new conversations.
func (r *CodexRunner) RegisterSystemPrompt(prompt string) {
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

// RegisterTools appends tools that are exposed to new conversations.
func (r *CodexRunner) RegisterTools(tools ...Tool) {
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

func (r *CodexRunner) globalContext() ([]string, []Tool) {
	if r == nil {
		return nil, nil
	}

	r.mu.RLock()
	defer r.mu.RUnlock()

	return append([]string(nil), r.systemPrompts...), append([]Tool(nil), r.tools...)
}

type codexThread interface {
	ID() string
	RunStreamed(input codex.Input, turnOptions codex.TurnOptions) (*codex.StreamedTurn, error)
}

// RunTurn runs one Codex turn and returns the updated thread mapping and final reply text.
func (r *CodexRunner) RunTurn(ctx context.Context, req TurnRequest) (TurnResult, error) {
	if r == nil {
		return TurnResult{}, errors.New("run codex turn failed: runner is nil")
	}

	var thread codexThread
	threadAction := "start"
	if req.Conversation.CodexThreadID != "" {
		threadAction = "resume"
		if r.resumeThread != nil {
			thread = r.resumeThread(req.Conversation.CodexThreadID, r.threadOptions)
		} else if r.client != nil {
			thread = r.client.ResumeThread(req.Conversation.CodexThreadID, r.threadOptions)
		}
	} else {
		if r.startThread != nil {
			thread = r.startThread(r.threadOptions)
		} else if r.client != nil {
			thread = r.client.StartThread(r.threadOptions)
		}
	}
	if thread == nil {
		return TurnResult{}, errors.New("run codex turn failed: runner is nil")
	}
	log.Printf(
		"codex runner thread ready: conversation=%s action=%s requested_thread=%s actual_thread=%s",
		req.Conversation.Key,
		threadAction,
		req.Conversation.CodexThreadID,
		thread.ID(),
	)

	input, err := r.buildTurnInput(req)
	if err != nil {
		return TurnResult{}, fmt.Errorf("run codex turn failed: %w", err)
	}

	typing := newTypingStatusController(
		req.Message.Responder,
		defaultTypingInitialDelay,
		defaultTypingRefreshCooldown,
	)
	typing.Start(ctx)
	defer typing.Stop()

	var replyText string
	_, tools := r.globalContext()
	if len(tools) == 0 {
		log.Printf("codex runner executing turn: conversation=%s mode=direct", req.Conversation.Key)
		turn, runErr := r.runThreadTurn(req, thread, input, codex.TurnOptions{
			Context: ctx,
		})
		if runErr != nil {
			return TurnResult{}, fmt.Errorf("run codex turn failed: %w", runErr)
		}
		replyText = turn.FinalResponse
	} else {
		log.Printf("codex runner executing turn: conversation=%s mode=tool_loop tool_count=%d", req.Conversation.Key, len(tools))
		replyText, err = r.runToolLoop(ctx, req, thread, input)
		if err != nil {
			return TurnResult{}, fmt.Errorf("run codex turn failed: %w", err)
		}
	}

	threadID := thread.ID()
	if threadID == "" {
		threadID = req.Conversation.CodexThreadID
	}

	return TurnResult{
		CodexThreadID: threadID,
		ReplyText:     replyText,
	}, nil
}

func (r *CodexRunner) runThreadTurn(req TurnRequest, thread codexThread, input codex.Input, options codex.TurnOptions) (codex.Turn, error) {
	log.Printf("codex runner started streamed turn: conversation=%s thread_id=%s", req.Conversation.Key, thread.ID())
	streamed, err := thread.RunStreamed(input, options)
	if err != nil {
		return codex.Turn{}, err
	}

	return r.collectStreamedTurn(req, streamed)
}

func (r *CodexRunner) collectStreamedTurn(req TurnRequest, streamed *codex.StreamedTurn) (codex.Turn, error) {
	if streamed == nil {
		return codex.Turn{}, errors.New("streamed turn is nil")
	}

	var items []codex.ThreadItem
	var finalResponse string
	var usage *codex.Usage
	var turnFailure *codex.ThreadError

	for event := range streamed.Events {
		switch event.Type {
		case "item.completed":
			if event.Item != nil {
				items = append(items, event.Item)
				if msg, ok := event.Item.(*codex.AgentMessageItem); ok {
					finalResponse = msg.Text
				}
			}
		case "turn.completed":
			usage = event.Usage
		case "turn.failed":
			turnFailure = event.Error
		}
	}

	if err := <-streamed.Done; err != nil {
		return codex.Turn{}, err
	}
	if turnFailure != nil {
		return codex.Turn{}, errors.New(turnFailure.Message)
	}
	log.Printf(
		"codex runner completed streamed turn: conversation=%s items=%d final_response_len=%d",
		req.Conversation.Key,
		len(items),
		len(finalResponse),
	)

	return codex.Turn{
		Items:         items,
		FinalResponse: finalResponse,
		Usage:         usage,
	}, nil
}

type toolLoopResponse struct {
	Action        string          `json:"action"`
	Message       string          `json:"message,omitempty"`
	ToolName      string          `json:"tool_name,omitempty"`
	ToolInput     json.RawMessage `json:"tool_input,omitempty"`
	ToolInputJSON string          `json:"tool_input_json,omitempty"`
}

func (r *CodexRunner) runToolLoop(ctx context.Context, req TurnRequest, thread codexThread, input codex.Input) (string, error) {
	currentInput := input
	toolCtx := ContextWithTurnRequest(ctx, req)
	for iteration := 0; iteration < r.maxToolIterations; iteration++ {
		log.Printf(
			"codex runner tool loop iteration: conversation=%s thread_id=%s iteration=%d",
			req.Conversation.Key,
			thread.ID(),
			iteration+1,
		)
		turn, runErr := r.runThreadTurn(req, thread, currentInput, codex.TurnOptions{
			OutputSchema: toolLoopResponseSchema(),
			Context:      ctx,
		})
		if runErr != nil {
			return "", runErr
		}

		decision, parseErr := parseToolLoopResponse(turn.FinalResponse)
		if parseErr != nil {
			return "", parseErr
		}
		log.Printf(
			"codex runner tool loop decision: conversation=%s iteration=%d action=%s tool=%s message_len=%d",
			req.Conversation.Key,
			iteration+1,
			decision.Action,
			decision.ToolName,
			len(decision.Message),
		)

		switch decision.Action {
		case "respond":
			if strings.TrimSpace(decision.Message) == "" {
				return "", errors.New("tool loop returned an empty assistant message")
			}
			return decision.Message, nil
		case "call_tool":
			tool, ok := r.findTool(decision.ToolName)
			if !ok {
				return "", fmt.Errorf("tool loop requested unknown tool %q", decision.ToolName)
			}

			log.Printf(
				"codex runner calling tool: conversation=%s iteration=%d tool=%s input_bytes=%d",
				req.Conversation.Key,
				iteration+1,
				tool.Name(),
				len(decision.ToolInput),
			)
			result, callErr := tool.Call(toolCtx, decision.ToolInput)
			if callErr != nil {
				log.Printf(
					"codex runner tool failed: conversation=%s iteration=%d tool=%s err=%v",
					req.Conversation.Key,
					iteration+1,
					tool.Name(),
					callErr,
				)
			} else {
				log.Printf(
					"codex runner tool completed: conversation=%s iteration=%d tool=%s",
					req.Conversation.Key,
					iteration+1,
					tool.Name(),
				)
			}
			currentInput, runErr = buildToolResultInput(tool.Name(), decision.ToolInput, result, callErr)
			if runErr != nil {
				return "", runErr
			}
		case "silent":
			return "", nil
		default:
			return "", fmt.Errorf("tool loop returned unsupported action %q", decision.Action)
		}
	}

	return "", fmt.Errorf("tool loop exceeded %d iterations", r.maxToolIterations)
}

func (r *CodexRunner) findTool(name string) (Tool, bool) {
	_, tools := r.globalContext()
	for _, tool := range tools {
		if tool.Name() == name {
			return tool, true
		}
	}

	return nil, false
}

func parseToolLoopResponse(raw string) (toolLoopResponse, error) {
	var response toolLoopResponse
	if err := json.Unmarshal([]byte(raw), &response); err != nil {
		return toolLoopResponse{}, fmt.Errorf("decode tool loop response failed: %w", err)
	}
	if len(response.ToolInput) == 0 && strings.TrimSpace(response.ToolInputJSON) != "" {
		response.ToolInput = json.RawMessage(response.ToolInputJSON)
	}
	if response.Action == "call_tool" && len(response.ToolInput) == 0 {
		response.ToolInput = json.RawMessage("{}")
	}

	return response, nil
}

func (r *CodexRunner) injectInitialPrompt(input codex.Input, prompts []string, tools []Tool) (codex.Input, error) {
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

func buildToolResultInput(toolName string, toolInput json.RawMessage, result any, err error) (codex.Input, error) {
	inputJSON := "{}"
	if len(toolInput) != 0 {
		inputJSON = string(toolInput)
	}

	var status string
	var payload string
	if err != nil {
		status = "error"
		payload = err.Error()
	} else {
		status = "ok"
		data, marshalErr := json.MarshalIndent(result, "", "  ")
		if marshalErr != nil {
			return codex.Input{}, fmt.Errorf("encode tool result failed: %w", marshalErr)
		}
		payload = string(data)
	}

	return codex.TextInput(strings.TrimSpace(fmt.Sprintf(`
Tool execution finished.

Tool: %s
Status: %s
Input:
%s

Result:
%s

Continue the task. Return valid JSON matching the schema for either another tool call or the final user-facing response.
`, toolName, status, inputJSON, payload))), nil
}

func toolLoopResponseSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action": map[string]any{
				"type": "string",
				"enum": []any{"respond", "call_tool", "silent"},
			},
			"message": map[string]any{
				"type": "string",
			},
			"tool_name": map[string]any{
				"type": "string",
			},
			"tool_input_json": map[string]any{
				"type": "string",
			},
		},
		"required":             []any{"action", "message", "tool_name", "tool_input_json"},
		"additionalProperties": false,
	}
}

func joinPromptBlocks(parts ...string) string {
	nonEmpty := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			nonEmpty = append(nonEmpty, trimmed)
		}
	}

	return strings.Join(nonEmpty, "\n\n")
}

func (r *CodexRunner) buildTurnInput(req TurnRequest) (codex.Input, error) {
	prompt, imagePaths := buildTurnPrompt(req.Message)
	if len(imagePaths) == 0 {
		return r.injectInitialTurnContext(req, codex.TextInput(prompt))
	}

	items := make([]codex.UserInput, 0, 1+len(imagePaths))
	if prompt != "" {
		items = append(items, codex.UserInput{
			Type: codex.UserInputText,
			Text: prompt,
		})
	}

	for _, imagePath := range imagePaths {
		items = append(items, codex.UserInput{
			Type: codex.UserInputLocalImage,
			Path: imagePath,
		})
	}

	return r.injectInitialTurnContext(req, codex.ItemsInput(items...))
}

func allImagePaths(primary string, extra []string) []string {
	return allPaths(primary, extra)
}

func allFilePaths(primary string, extra []string) []string {
	return allPaths(primary, extra)
}

func allVideoPaths(primary string, extra []string) []string {
	return allPaths(primary, extra)
}

func allPaths(primary string, extra []string) []string {
	paths := make([]string, 0, 1+len(extra))
	seen := make(map[string]struct{}, 1+len(extra))
	appendPath := func(path string) {
		path = strings.TrimSpace(path)
		if path == "" {
			return
		}
		if _, ok := seen[path]; ok {
			return
		}
		seen[path] = struct{}{}
		paths = append(paths, path)
	}

	appendPath(primary)
	for _, path := range extra {
		appendPath(path)
	}

	return paths
}

func (r *CodexRunner) injectInitialTurnContext(req TurnRequest, input codex.Input) (codex.Input, error) {
	if req.Conversation.CodexThreadID != "" {
		return input, nil
	}

	prompts, tools := r.globalContext()
	return r.injectInitialPrompt(input, prompts, tools)
}

func buildTurnPrompt(message InboundMessage) (string, []string) {
	if len(message.mergedMessages) > 0 {
		return buildCompositeTurnPrompt(message)
	}

	return buildSingleTurnPrompt(message, true)
}

func buildSingleTurnPrompt(message InboundMessage, includeReplyMention bool) (string, []string) {
	var parts []string
	var imagePaths []string

	appendConversationContextParts(&parts, &imagePaths, message)
	if currentMessageContext := formatCurrentMessageContext(message); currentMessageContext != "" {
		parts = append(parts, currentMessageContext)
	}
	if messageTags := formatMessageTags(message.MessageTags); messageTags != "" {
		parts = append(parts, "Message tags:\n"+messageTags)
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
	if includeReplyMention {
		if mentionHint := strings.TrimSpace(message.SenderMentionHint); mentionHint != "" {
			parts = append(parts, "Sender mention hint: "+mentionHint)
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
		parts = append(parts, "Multiple new messages arrived while the assistant was busy. Process them together in order.\nIf you reply in a threaded conversation, explicitly mention the relevant sender or senders in your reply body using the provided reply mention hints.")
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

func formatCurrentMessageContext(message InboundMessage) string {
	lines := []string{
		"Current message context:",
		"- time: " + formatCurrentMessageTime(message.SentAtUnix),
		"- sender: " + formatCurrentMessageSenderName(message.Sender),
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
		lines = append(lines, "- "+tag)
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

func temporaryFilePathNotice() string {
	return "Path validity: local file paths are temporary and only valid for this turn."
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
