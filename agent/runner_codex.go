package agent

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/godeps/codex-sdk-go"
)

const defaultMaxToolIterations = 50

// CodexRunner bridges dispatcher turns to the Codex CLI through codex-sdk-go.
type CodexRunner struct {
	client            *codex.Codex
	startThread       func(codex.ThreadOptions) codexThread
	resumeThread      func(string, codex.ThreadOptions) codexThread
	threadOptions     codex.ThreadOptions
	maxToolIterations int
	//nolint:containedctx // This is the runner lifecycle root context shared by managed Codex sessions.
	lifecycleCtx  context.Context
	cancel        context.CancelFunc
	mu            sync.RWMutex
	systemPrompts []string
	tools         []Tool
	closed        bool
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
	if threadOptions.ModelReasoningEffort == "" {
		threadOptions.ModelReasoningEffort = codex.ReasoningLow
	}

	maxToolIterations := options.MaxToolIterations
	if maxToolIterations <= 0 {
		maxToolIterations = defaultMaxToolIterations
	}

	lifecycleCtx, cancel := context.WithCancel(context.Background())
	runner := &CodexRunner{
		client: client,
		startThread: func(options codex.ThreadOptions) codexThread {
			return client.StartThread(options)
		},
		resumeThread: func(threadID string, options codex.ThreadOptions) codexThread {
			return client.ResumeThread(threadID, options)
		},
		threadOptions:     threadOptions,
		maxToolIterations: maxToolIterations,
		lifecycleCtx:      lifecycleCtx,
		cancel:            cancel,
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

// StartSession creates or resumes one dispatcher-managed Codex thread session.
func (r *CodexRunner) StartSession(_ context.Context, options SessionOptions) (Session, error) {
	if r == nil {
		return nil, errors.New("start codex session failed: runner is nil")
	}
	if r.isClosed() {
		return nil, errors.New("start codex session failed: runner is closed")
	}

	conversationKey := strings.TrimSpace(options.ConversationKey)
	resumeSessionID := strings.TrimSpace(options.ResumeSessionID)
	thread, err := r.createCodexThread(resumeSessionID)
	if err != nil {
		return nil, fmt.Errorf("start codex session failed: %w", err)
	}
	if thread == nil {
		return nil, errors.New("start codex session failed: thread is nil")
	}
	threadID := firstNonEmptyString(thread.ID(), resumeSessionID, conversationKey)

	return &codexRunnerSession{
		runner:                r,
		conversationKey:       conversationKey,
		threadID:              threadID,
		thread:                thread,
		pendingInitialContext: resumeSessionID == "",
	}, nil
}

func (r *CodexRunner) createCodexThread(threadID string) (codexThread, error) {
	if r == nil {
		return nil, errors.New("codex runner is nil")
	}

	threadID = strings.TrimSpace(threadID)
	if threadID != "" {
		if r.resumeThread != nil {
			thread := r.resumeThread(threadID, r.threadOptions)
			if thread == nil {
				return nil, errors.New("thread is nil")
			}
			return thread, nil
		}
		if r.client != nil {
			thread := r.client.ResumeThread(threadID, r.threadOptions)
			if thread == nil {
				return nil, errors.New("thread is nil")
			}
			return thread, nil
		}
		return nil, errors.New("resume thread is unavailable")
	}

	if r.startThread != nil {
		thread := r.startThread(r.threadOptions)
		if thread == nil {
			return nil, errors.New("thread is nil")
		}
		return thread, nil
	}
	if r.client != nil {
		thread := r.client.StartThread(r.threadOptions)
		if thread == nil {
			return nil, errors.New("thread is nil")
		}
		return thread, nil
	}
	return nil, errors.New("start thread is unavailable")
}

func (r *CodexRunner) effectiveMaxToolIterations() int {
	if r == nil || r.maxToolIterations <= 0 {
		return defaultMaxToolIterations
	}
	return r.maxToolIterations
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

type codexThread interface {
	ID() string
	RunStreamed(input codex.Input, turnOptions codex.TurnOptions) (*codex.StreamedTurn, error)
}

// Close stops the runner lifecycle.
func (r *CodexRunner) Close() error {
	if r == nil {
		return nil
	}

	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	cancel := r.cancel
	r.cancel = nil
	r.mu.Unlock()

	if cancel != nil {
		cancel()
	}

	log.Printf("codex runner closed")
	return nil
}

func (r *CodexRunner) isClosed() bool {
	if r == nil {
		return false
	}

	r.mu.Lock()
	closed := r.closed
	r.mu.Unlock()
	return closed
}

func logCodexCompletedItem(req TurnRequest, item codex.ThreadItem) {
	switch current := item.(type) {
	case *codex.AgentMessageItem:
		log.Printf(
			"codex runner item completed: conversation=%s type=%s text_len=%d",
			req.Conversation.Key,
			current.Type,
			len(current.Text),
		)
	case *codex.ReasoningItem:
		log.Printf(
			"codex runner reasoning completed: conversation=%s text=%q",
			req.Conversation.Key,
			abbreviateLogText(current.Text, 200),
		)
	case *codex.CommandExecutionItem:
		exitCode := ""
		if current.ExitCode != nil {
			exitCode = strconv.Itoa(*current.ExitCode)
		}
		log.Printf(
			"codex runner command completed: conversation=%s status=%s exit_code=%s command=%q output=%q",
			req.Conversation.Key,
			current.Status,
			exitCode,
			abbreviateLogText(current.Command, 160),
			abbreviateLogText(current.AggregatedOutput, 200),
		)
	case *codex.FileChangeItem:
		log.Printf(
			"codex runner file change completed: conversation=%s status=%s changes=%s",
			req.Conversation.Key,
			current.Status,
			summarizeCodexFileChanges(current.Changes),
		)
	case *codex.McpToolCallItem:
		log.Printf(
			"codex runner mcp tool completed: conversation=%s server=%s tool=%s status=%s error=%q",
			req.Conversation.Key,
			current.Server,
			current.Tool,
			current.Status,
			summarizeCodexMCPError(current.Error),
		)
	case *codex.WebSearchItem:
		log.Printf(
			"codex runner web search completed: conversation=%s query=%q",
			req.Conversation.Key,
			abbreviateLogText(current.Query, 200),
		)
	case *codex.TodoListItem:
		log.Printf(
			"codex runner todo list completed: conversation=%s items=%d completed=%d",
			req.Conversation.Key,
			len(current.Items),
			countCompletedCodexTodos(current.Items),
		)
	case *codex.ErrorItem:
		log.Printf(
			"codex runner item error: conversation=%s message=%q",
			req.Conversation.Key,
			abbreviateLogText(current.Message, 200),
		)
	default:
		log.Printf(
			"codex runner item completed: conversation=%s type=%s",
			req.Conversation.Key,
			item.ItemType(),
		)
	}
}

func summarizeCodexFileChanges(changes []codex.FileUpdateChange) string {
	if len(changes) == 0 {
		return "none"
	}

	parts := make([]string, 0, len(changes))
	for _, change := range changes {
		part := string(change.Kind)
		if strings.TrimSpace(change.Path) != "" {
			part += ":" + change.Path
		}
		parts = append(parts, part)
	}

	return abbreviateLogText(strings.Join(parts, ", "), 240)
}

func summarizeCodexMCPError(err *codex.McpToolCallError) string {
	if err == nil {
		return ""
	}

	return abbreviateLogText(err.Message, 200)
}

func countCompletedCodexTodos(items []codex.TodoItem) int {
	count := 0
	for _, item := range items {
		if item.Completed {
			count++
		}
	}

	return count
}
