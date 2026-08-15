package agent

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"

	"github.com/hzj629206/assistant/agent/codex"
)

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
	CodexOptions      codex.CodexOptions
	ThreadOptions     codex.ThreadOptions
	SystemPrompt      string
	Tools             []Tool
	MaxToolIterations int
}

// NewCodexRunner builds a runner backed by the Codex CLI.
func NewCodexRunner(options CodexRunnerOptions) *CodexRunner {
	client := options.Client
	if client == nil {
		client = codex.NewCodex(options.CodexOptions)
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
	threadID := firstNonEmptyString(thread.ID(), resumeSessionID)
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
