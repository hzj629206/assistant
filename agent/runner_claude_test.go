package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hzj629206/assistant/agent/claudecode"
)

func TestNewClaudeCodeRunnerDefaultsPermissionModeToDontAsk(t *testing.T) {
	t.Parallel()

	runner := NewClaudeCodeRunner(ClaudeCodeRunnerOptions{})
	if runner.runOptions.PermissionMode != claudecode.PermissionModeDontAsk {
		t.Fatalf("unexpected permission mode: %s", runner.runOptions.PermissionMode)
	}
}

func TestNewClaudeCodeRunnerPreservesExplicitPermissionMode(t *testing.T) {
	t.Parallel()

	runner := NewClaudeCodeRunner(ClaudeCodeRunnerOptions{
		RunOptions: claudecode.RunOptions{
			PermissionMode: claudecode.PermissionModePlan,
		},
	})
	if runner.runOptions.PermissionMode != claudecode.PermissionModePlan {
		t.Fatalf("unexpected permission mode: %s", runner.runOptions.PermissionMode)
	}
}

func TestNewClaudeCodeRunnerDefaultsSettingSources(t *testing.T) {
	t.Parallel()

	runner := NewClaudeCodeRunner(ClaudeCodeRunnerOptions{})
	want := []string{"user", "project", "local"}
	if !slices.Equal(runner.runOptions.SettingSources, want) {
		t.Fatalf("unexpected setting sources: %q", runner.runOptions.SettingSources)
	}
}

func TestNewClaudeCodeRunnerDefaultsWorkingDirectoryToCurrentDirectory(t *testing.T) {
	t.Parallel()

	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd failed: %v", err)
	}

	runner := NewClaudeCodeRunner(ClaudeCodeRunnerOptions{})
	if runner.runOptions.WorkingDirectory != workingDirectory {
		t.Fatalf("unexpected working directory: %q", runner.runOptions.WorkingDirectory)
	}
}

func TestNewClaudeCodeRunnerPreservesExplicitWorkingDirectory(t *testing.T) {
	t.Parallel()

	runner := NewClaudeCodeRunner(ClaudeCodeRunnerOptions{
		RunOptions: claudecode.RunOptions{
			WorkingDirectory: "/tmp/assistant-claude",
		},
	})
	if runner.runOptions.WorkingDirectory != "/tmp/assistant-claude" {
		t.Fatalf("unexpected working directory: %q", runner.runOptions.WorkingDirectory)
	}
}

func TestClaudeProcessExitLogDetailsIncludesSignal(t *testing.T) {
	t.Parallel()

	err := exec.CommandContext(context.Background(), "sh", "-c", "kill -TERM $$").Run()
	if err == nil {
		t.Fatal("Run returned nil, want exit error")
	}

	got := claudeProcessExitLogDetails(err)
	if got != "signal=terminated" {
		t.Fatalf("unexpected signal details: %q", got)
	}
}

func TestClaudeProcessExitLogDetailsIncludesExitCode(t *testing.T) {
	t.Parallel()

	err := exec.CommandContext(context.Background(), "sh", "-c", "exit 7").Run()
	if err == nil {
		t.Fatal("Run returned nil, want exit error")
	}

	got := claudeProcessExitLogDetails(err)
	if got != "exit_code=7" {
		t.Fatalf("unexpected exit code details: %q", got)
	}
}

func TestClaudeCodeRunnerRunTurnStartsConversationAndStoresSessionID(t *testing.T) {
	t.Parallel()

	var prompts []string
	var options []claudecode.RunOptions
	runner := NewClaudeCodeRunner(ClaudeCodeRunnerOptions{
		RunOptions: claudecode.RunOptions{
			Model: "claude-sonnet-4-5",
		},
	})
	runner.sessionFactory = func(_ context.Context, _ string, opts claudecode.RunOptions, _ claudecode.SessionHooks) (claudecode.Session, error) {
		options = append(options, opts)
		return &fakeClaudePersistentSession{
			runTurn: func(_ context.Context, blocks []map[string]any) (*claudecode.ClaudeResult, error) {
				prompt := readClaudeStreamInput(blocks)
				prompts = append(prompts, prompt)
				return &claudecode.ClaudeResult{
					Type:      "result",
					Result:    "assistant reply",
					SessionID: "session-new",
				}, nil
			},
		}, nil
	}
	runner.RegisterSystemPrompt("Global system prompt.")

	result, err := runTurnWithRunner(t, runner, TurnRequest{
		Conversation: ConversationState{Key: "conversation-1"},
		Message: InboundMessage{
			Kind:       MessageKindText,
			Sender:     "alice",
			SentAtUnix: 1000,
			Text:       "hello",
		},
	})
	if err != nil {
		t.Fatalf("RunTurn failed: %v", err)
	}

	if result.RunnerThreadID != "session-new" {
		t.Fatalf("unexpected session id: %s", result.RunnerThreadID)
	}
	if result.ReplyText != "assistant reply" {
		t.Fatalf("unexpected reply: %s", result.ReplyText)
	}
	if len(prompts) != 1 {
		t.Fatalf("unexpected prompt count: %d", len(prompts))
	}
	if strings.Contains(prompts[0], "Global system prompt.") {
		t.Fatalf("did not expect system prompt in user prompt, got:\n%s", prompts[0])
	}
	if !strings.Contains(prompts[0], "hello") {
		t.Fatalf("expected user text in prompt, got:\n%s", prompts[0])
	}
	if len(options) != 1 {
		t.Fatalf("unexpected options count: %d", len(options))
	}
	if options[0].ResumeID != "" {
		t.Fatalf("unexpected resume id: %s", options[0].ResumeID)
	}
	if err = claudecode.ValidateSessionID(options[0].SessionID); err != nil {
		t.Fatalf("expected generated session id, got %q (%v)", options[0].SessionID, err)
	}
	if !strings.Contains(options[0].AppendPrompt, "claudecode-append-system-prompt-") {
		t.Fatalf("expected append prompt file path, got:\n%s", options[0].AppendPrompt)
	}
	appendPromptData, err := readTestFile(options[0].AppendPrompt)
	if err != nil {
		t.Fatalf("read append prompt file failed: %v", err)
	}
	if !strings.Contains(string(appendPromptData), "Global system prompt.") {
		t.Fatalf("expected append prompt file to include system prompt, got:\n%s", string(appendPromptData))
	}
	if options[0].PermissionMode != claudecode.PermissionModeDontAsk {
		t.Fatalf("unexpected permission mode: %s", options[0].PermissionMode)
	}
}

func TestClaudeCodeRunnerStartSessionExposesGeneratedSessionID(t *testing.T) {
	t.Parallel()

	var createdOptions claudecode.RunOptions
	runner := NewClaudeCodeRunner(ClaudeCodeRunnerOptions{})
	runner.sessionFactory = func(_ context.Context, _ string, opts claudecode.RunOptions, _ claudecode.SessionHooks) (claudecode.Session, error) {
		createdOptions = opts
		return &fakeClaudePersistentSession{
			currentSessionID: opts.SessionID,
			runTurn: func(_ context.Context, _ []map[string]any) (*claudecode.ClaudeResult, error) {
				return &claudecode.ClaudeResult{
					Type:      "result",
					Result:    "ok",
					SessionID: opts.SessionID,
				}, nil
			},
		}, nil
	}

	session, err := runner.StartSession(context.Background(), SessionOptions{ConversationKey: "conversation-1"})
	if err != nil {
		t.Fatalf("StartSession failed: %v", err)
	}
	if err = claudecode.ValidateSessionID(createdOptions.SessionID); err != nil {
		t.Fatalf("expected generated session id, got %q (%v)", createdOptions.SessionID, err)
	}
	if session.ID() != createdOptions.SessionID {
		t.Fatalf("unexpected session id: got %q want %q", session.ID(), createdOptions.SessionID)
	}
}

func TestClaudeCodeSessionRunTurnReturnsErrorOnConversationMismatch(t *testing.T) {
	t.Parallel()

	session := &claudeRunnerSession{
		runner:          &ClaudeCodeRunner{},
		conversationKey: "conversation-1",
		sessionID:       "session-live",
	}

	_, err := session.RunTurn(context.Background(), TurnRequest{
		Conversation: ConversationState{
			Key:            "conversation-2",
			RunnerThreadID: "session-live",
		},
		Message: InboundMessage{Kind: MessageKindText, Text: "hello"},
	})
	if err == nil || !strings.Contains(err.Error(), `conversation key mismatch: session="conversation-1" request="conversation-2"`) {
		t.Fatalf("unexpected error: %v", err)
	}

	_, err = session.RunTurn(context.Background(), TurnRequest{
		Conversation: ConversationState{
			Key:            "conversation-1",
			RunnerThreadID: "session-other",
		},
		Message: InboundMessage{Kind: MessageKindText, Text: "hello"},
	})
	if err == nil || !strings.Contains(err.Error(), `runner thread id mismatch: session="session-live" request="session-other"`) {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestClaudeCodeRunnerRunTurnWrapperStartsFreshSessionUsingResumeID(t *testing.T) {
	t.Parallel()

	var prompt string
	var options []claudecode.RunOptions
	runner := NewClaudeCodeRunner(ClaudeCodeRunnerOptions{})
	runner.sessionFactory = func(_ context.Context, _ string, opts claudecode.RunOptions, _ claudecode.SessionHooks) (claudecode.Session, error) {
		options = append(options, opts)
		return &fakeClaudePersistentSession{
			currentSessionID: "session-existing",
			runTurn: func(_ context.Context, blocks []map[string]any) (*claudecode.ClaudeResult, error) {
				prompt = readClaudeStreamInput(blocks)
				return &claudecode.ClaudeResult{
					Type:      "result",
					Result:    "follow-up reply",
					SessionID: "session-existing",
				}, nil
			},
		}, nil
	}
	runner.RegisterSystemPrompt("Global system prompt.")

	_, err := runTurnWithRunner(t, runner, TurnRequest{
		Conversation: ConversationState{
			Key:            "conversation-1",
			RunnerThreadID: "session-existing",
		},
		Message: InboundMessage{
			Kind:       MessageKindText,
			Sender:     "alice",
			SentAtUnix: 1000,
			Text:       "follow up",
		},
	})
	if err != nil {
		t.Fatalf("RunTurn failed: %v", err)
	}

	if len(options) != 1 {
		t.Fatalf("unexpected options count: %d", len(options))
	}
	if options[0].ResumeID != "session-existing" {
		t.Fatalf("unexpected resume id: %s", options[0].ResumeID)
	}
	if options[0].AppendPrompt != "" {
		t.Fatalf("expected append prompt to be empty when resuming, got:\n%s", options[0].AppendPrompt)
	}
	if !strings.Contains(prompt, "follow up") {
		t.Fatalf("expected user prompt, got:\n%s", prompt)
	}
}

func TestClaudeCodeRunnerRunTurnUsesStdioPermissionPromptInDefaultMode(t *testing.T) {
	t.Parallel()

	var (
		argsSnapshot []string
		controlSeen  bool
	)
	runner := NewClaudeCodeRunner(ClaudeCodeRunnerOptions{
		RunOptions: claudecode.RunOptions{
			PermissionMode: claudecode.PermissionModeDefault,
		},
	})
	runner.sessionFactory = func(_ context.Context, _ string, opts claudecode.RunOptions, hooks claudecode.SessionHooks) (claudecode.Session, error) {
		argsSnapshot = claudecode.BuildCLIArgs(&opts)
		controlSeen = hooks.HandleControlRequest != nil
		return &fakeClaudePersistentSession{
			runTurn: func(_ context.Context, blocks []map[string]any) (*claudecode.ClaudeResult, error) {
				_ = readClaudeStreamInput(blocks)
				return &claudecode.ClaudeResult{
					Type:      "result",
					Result:    "ok",
					SessionID: "session-new",
				}, nil
			},
		}, nil
	}

	_, err := runTurnWithRunner(t, runner, TurnRequest{
		Conversation: ConversationState{Key: "conversation-1"},
		Message: InboundMessage{
			Kind:       MessageKindText,
			Sender:     "alice",
			SentAtUnix: 1000,
			Text:       "write a file",
		},
	})
	if err != nil {
		t.Fatalf("RunTurn failed: %v", err)
	}

	if !controlSeen {
		t.Fatal("expected control server to be created")
	}
	if !slices.Contains(argsSnapshot, "--permission-prompt-tool") {
		t.Fatalf("expected permission prompt tool flag, got %q", argsSnapshot)
	}
	if !slices.Contains(argsSnapshot, "stdio") {
		t.Fatalf("expected stdio permission prompt tool, got %q", argsSnapshot)
	}
}

func TestClaudeCodeRunnerRunTurnExposesNativeMCPTools(t *testing.T) {
	t.Parallel()

	var (
		prompt    string
		mcpConfig string
	)
	runner := NewClaudeCodeRunner(ClaudeCodeRunnerOptions{})
	runner.RegisterTools(uppercaseTool{})
	runner.sessionFactory = func(_ context.Context, _ string, opts claudecode.RunOptions, hooks claudecode.SessionHooks) (claudecode.Session, error) {
		if len(opts.MCPConfigs) != 1 {
			t.Fatalf("unexpected mcp config count: %d", len(opts.MCPConfigs))
		}
		mcpConfig = opts.MCPConfigs[0]
		if hooks.HandleControlRequest == nil {
			t.Fatal("expected control server with tools")
		}
		return &fakeClaudePersistentSession{
			runTurn: func(_ context.Context, blocks []map[string]any) (*claudecode.ClaudeResult, error) {
				prompt = readClaudeStreamInput(blocks)

				listResponse, err := hooks.HandleControlRequest(map[string]any{
					"subtype":     "mcp_message",
					"server_name": claudeSDKMCPServerName,
					"message": map[string]any{
						"jsonrpc": "2.0",
						"id":      1,
						"method":  "tools/list",
					},
				})
				if err != nil {
					return nil, err
				}
				callResponse, err := hooks.HandleControlRequest(map[string]any{
					"subtype":     "mcp_message",
					"server_name": claudeSDKMCPServerName,
					"message": map[string]any{
						"jsonrpc": "2.0",
						"id":      2,
						"method":  "tools/call",
						"params": map[string]any{
							"name": "uppercase",
							"arguments": map[string]any{
								"text": "hello",
							},
						},
					},
				})
				if err != nil {
					return nil, err
				}

				listData, err := json.Marshal(listResponse)
				if err != nil {
					return nil, err
				}
				callData, err := json.Marshal(callResponse)
				if err != nil {
					return nil, err
				}
				return &claudecode.ClaudeResult{
					Type:      "result",
					Result:    string(listData) + "\n" + string(callData),
					SessionID: "session-new",
				}, nil
			},
		}, nil
	}

	result, err := runTurnWithRunner(t, runner, TurnRequest{
		Conversation: ConversationState{Key: "conversation-1"},
		Message: InboundMessage{
			Kind:       MessageKindText,
			Sender:     "alice",
			SentAtUnix: 1000,
			Text:       "uppercase hello",
		},
	})
	if err != nil {
		t.Fatalf("RunTurn failed: %v", err)
	}

	if !strings.Contains(result.ReplyText, `"name":"uppercase"`) {
		t.Fatalf("expected tool in tools/list response, got: %s", result.ReplyText)
	}
	if !strings.Contains(result.ReplyText, `{\"text\":\"HELLO\"}`) {
		t.Fatalf("expected tool call result in response, got: %s", result.ReplyText)
	}
	if result.RunnerThreadID != "session-new" {
		t.Fatalf("unexpected session id: %s", result.RunnerThreadID)
	}
	if !strings.Contains(mcpConfig, "claudecode-mcp-config-") {
		t.Fatalf("expected mcp config file path, got: %s", mcpConfig)
	}
	mcpConfigData, err := readTestFile(mcpConfig)
	if err != nil {
		t.Fatalf("read mcp config file failed: %v", err)
	}
	if !strings.Contains(string(mcpConfigData), `"type": "sdk"`) && !strings.Contains(string(mcpConfigData), `"type":"sdk"`) {
		t.Fatalf("expected sdk mcp config, got: %s", string(mcpConfigData))
	}
	if !strings.Contains(prompt, "uppercase hello") {
		t.Fatalf("expected user prompt, got: %s", prompt)
	}
}

func TestClaudeCodeSessionReusesPersistentProcessWithinSession(t *testing.T) {
	t.Parallel()

	createCount := 0
	runCount := 0
	runner := NewClaudeCodeRunner(ClaudeCodeRunnerOptions{})
	runner.sessionFactory = func(_ context.Context, _ string, _ claudecode.RunOptions, _ claudecode.SessionHooks) (claudecode.Session, error) {
		createCount++
		return &fakeClaudePersistentSession{
			currentSessionID: "session-live",
			runTurn: func(_ context.Context, _ []map[string]any) (*claudecode.ClaudeResult, error) {
				runCount++
				return &claudecode.ClaudeResult{
					Type:      "result",
					Result:    "ok",
					SessionID: "session-live",
				}, nil
			},
		}, nil
	}
	session, err := runner.StartSession(context.Background(), SessionOptions{ConversationKey: "conversation-1"})
	if err != nil {
		t.Fatalf("StartSession failed: %v", err)
	}

	first, err := session.RunTurn(context.Background(), TurnRequest{
		Conversation: ConversationState{Key: "conversation-1"},
		Message:      InboundMessage{Kind: MessageKindText, Text: "hello"},
	})
	if err != nil {
		t.Fatalf("first RunTurn failed: %v", err)
	}
	_, err = session.RunTurn(context.Background(), TurnRequest{
		Conversation: ConversationState{Key: "conversation-1", RunnerThreadID: first.RunnerThreadID},
		Message:      InboundMessage{Kind: MessageKindText, Text: "again"},
	})
	if err != nil {
		t.Fatalf("second RunTurn failed: %v", err)
	}

	if createCount != 1 {
		t.Fatalf("unexpected session create count: %d", createCount)
	}
	if runCount != 2 {
		t.Fatalf("unexpected run count: %d", runCount)
	}
}

func TestClaudeCodeRunnerRunTurnResumesExistingSession(t *testing.T) {
	t.Parallel()

	createCount := 0
	runner := NewClaudeCodeRunner(ClaudeCodeRunnerOptions{
		RunOptions: claudecode.RunOptions{},
	})
	resumeIDs := make([]string, 0, 2)
	runner.sessionFactory = func(_ context.Context, _ string, options claudecode.RunOptions, _ claudecode.SessionHooks) (claudecode.Session, error) {
		createCount++
		resumeIDs = append(resumeIDs, options.ResumeID)
		return &fakeClaudePersistentSession{
			currentSessionID: "session-live",
			runTurn: func(_ context.Context, _ []map[string]any) (*claudecode.ClaudeResult, error) {
				return &claudecode.ClaudeResult{
					Type:      "result",
					Result:    "ok",
					SessionID: "session-live",
				}, nil
			},
		}, nil
	}

	first, err := runTurnWithRunner(t, runner, TurnRequest{
		Conversation: ConversationState{Key: "conversation-1"},
		Message:      InboundMessage{Kind: MessageKindText, Text: "hello"},
	})
	if err != nil {
		t.Fatalf("first RunTurn failed: %v", err)
	}
	_, err = runTurnWithRunner(t, runner, TurnRequest{
		Conversation: ConversationState{Key: "conversation-1", RunnerThreadID: first.RunnerThreadID},
		Message:      InboundMessage{Kind: MessageKindText, Text: "again"},
	})
	if err != nil {
		t.Fatalf("second RunTurn failed: %v", err)
	}

	if createCount != 2 {
		t.Fatalf("unexpected session create count: %d", createCount)
	}
	if len(resumeIDs) != 2 || resumeIDs[0] != "" || resumeIDs[1] != "session-live" {
		t.Fatalf("unexpected resume ids: %#v", resumeIDs)
	}
}

func TestClaudeCodeSessionInterruptWaitsForActiveTurnCompletion(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	release := make(chan struct{})
	interruptCalled := make(chan struct{}, 1)
	runner := NewClaudeCodeRunner(ClaudeCodeRunnerOptions{})
	runner.sessionFactory = func(_ context.Context, _ string, _ claudecode.RunOptions, _ claudecode.SessionHooks) (claudecode.Session, error) {
		return &fakeClaudePersistentSession{
			currentSessionID: "session-live",
			runTurn: func(_ context.Context, _ []map[string]any) (*claudecode.ClaudeResult, error) {
				close(started)
				<-release
				return nil, errors.New("interrupted")
			},
			interruptCurrentTurn: func(context.Context) error {
				interruptCalled <- struct{}{}
				<-release
				return nil
			},
		}, nil
	}
	session, err := runner.StartSession(context.Background(), SessionOptions{ConversationKey: "conversation-1"})
	if err != nil {
		t.Fatalf("StartSession failed: %v", err)
	}

	runErrCh := make(chan error, 1)
	go func() {
		_, err := session.RunTurn(context.Background(), TurnRequest{
			Conversation: ConversationState{Key: "conversation-1"},
			Message:      InboundMessage{Kind: MessageKindText, Text: "hello"},
		})
		runErrCh <- err
	}()

	<-started

	interruptDone := make(chan error, 1)
	go func() {
		interruptDone <- session.Interrupt(context.Background())
	}()

	select {
	case err := <-interruptDone:
		t.Fatalf("interrupt returned before turn finished: %v", err)
	case <-time.After(20 * time.Millisecond):
	}

	select {
	case <-interruptCalled:
	case <-time.After(time.Second):
		t.Fatal("expected protocol interrupt to be called")
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
		if err == nil || err.Error() != "run claude code turn failed: interrupted" {
			t.Fatalf("unexpected run turn error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("run turn did not finish")
	}
}

func TestClaudeCodeSessionInterruptDoesNotAllowSuccessfulTurnResult(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	release := make(chan struct{})
	interruptCalled := make(chan struct{}, 1)
	runner := NewClaudeCodeRunner(ClaudeCodeRunnerOptions{})
	runner.sessionFactory = func(_ context.Context, _ string, _ claudecode.RunOptions, _ claudecode.SessionHooks) (claudecode.Session, error) {
		return &fakeClaudePersistentSession{
			currentSessionID: "session-live",
			runTurn: func(_ context.Context, _ []map[string]any) (*claudecode.ClaudeResult, error) {
				close(started)
				<-release
				return &claudecode.ClaudeResult{SessionID: "session-live", Result: "partial reply"}, nil
			},
			interruptCurrentTurn: func(context.Context) error {
				interruptCalled <- struct{}{}
				<-release
				return nil
			},
		}, nil
	}
	session, err := runner.StartSession(context.Background(), SessionOptions{ConversationKey: "conversation-1"})
	if err != nil {
		t.Fatalf("StartSession failed: %v", err)
	}

	runErrCh := make(chan error, 1)
	go func() {
		_, err := session.RunTurn(context.Background(), TurnRequest{
			Conversation: ConversationState{Key: "conversation-1"},
			Message:      InboundMessage{Kind: MessageKindText, Text: "hello"},
		})
		runErrCh <- err
	}()

	<-started

	interruptDone := make(chan error, 1)
	go func() {
		interruptDone <- session.Interrupt(context.Background())
	}()

	select {
	case <-interruptCalled:
	case <-time.After(time.Second):
		t.Fatal("expected protocol interrupt to be called")
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
		if err == nil || err.Error() != "run claude code turn failed: context canceled" {
			t.Fatalf("unexpected run turn error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("run turn did not finish")
	}
}

func TestClaudeCodeRunnerInterruptReturnsNilWithoutActiveTurn(t *testing.T) {
	t.Parallel()

	runner := NewClaudeCodeRunner(ClaudeCodeRunnerOptions{})

	err := interruptWithRunner(t, runner, ConversationState{Key: "conversation-idle"})
	if err != nil {
		t.Fatalf("expected nil interrupt on idle session, got %v", err)
	}
}

func TestClaudeCodeRunnerRunTurnMapsClaudeSessionBusyError(t *testing.T) {
	t.Parallel()

	runner := NewClaudeCodeRunner(ClaudeCodeRunnerOptions{})
	runner.sessionFactory = func(_ context.Context, _ string, _ claudecode.RunOptions, _ claudecode.SessionHooks) (claudecode.Session, error) {
		return &fakeClaudePersistentSession{
			currentSessionID: "session-live",
			runTurn: func(context.Context, []map[string]any) (*claudecode.ClaudeResult, error) {
				return nil, claudecode.ErrSessionBusy
			},
		}, nil
	}
	session, err := runner.StartSession(context.Background(), SessionOptions{ConversationKey: "conversation-1"})
	if err != nil {
		t.Fatalf("StartSession failed: %v", err)
	}

	_, err = session.RunTurn(context.Background(), TurnRequest{
		Conversation: ConversationState{Key: "conversation-1"},
		Message:      InboundMessage{Kind: MessageKindText, Text: "hello"},
	})
	if !errors.Is(err, ErrSessionBusy) {
		t.Fatalf("expected ErrSessionBusy, got %v", err)
	}
}

func TestClaudeCodeSessionCloseClosesUnderlyingSession(t *testing.T) {
	t.Parallel()

	runner := NewClaudeCodeRunner(ClaudeCodeRunnerOptions{})
	closed := false
	runner.sessionFactory = func(_ context.Context, _ string, _ claudecode.RunOptions, _ claudecode.SessionHooks) (claudecode.Session, error) {
		return &fakeClaudePersistentSession{
			currentSessionID: "session-live",
			closeFunc: func() error {
				closed = true
				return nil
			},
		}, nil
	}
	session, err := runner.StartSession(context.Background(), SessionOptions{ConversationKey: "conversation-close"})
	if err != nil {
		t.Fatalf("StartSession failed: %v", err)
	}

	if err = session.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}
	if !closed {
		t.Fatal("expected underlying session to be closed")
	}
}

func TestClaudeCodeSessionRejectsConcurrentRunTurn(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	release := make(chan struct{})
	runner := NewClaudeCodeRunner(ClaudeCodeRunnerOptions{})
	runner.sessionFactory = func(_ context.Context, _ string, _ claudecode.RunOptions, _ claudecode.SessionHooks) (claudecode.Session, error) {
		return &fakeClaudePersistentSession{
			currentSessionID: "session-live",
			runTurn: func(_ context.Context, _ []map[string]any) (*claudecode.ClaudeResult, error) {
				select {
				case <-started:
					return nil, claudecode.ErrSessionBusy
				default:
					close(started)
				}
				<-release
				return nil, errors.New("interrupted")
			},
		}, nil
	}
	session, err := runner.StartSession(context.Background(), SessionOptions{ConversationKey: "conversation-busy"})
	if err != nil {
		t.Fatalf("StartSession failed: %v", err)
	}

	runErrCh := make(chan error, 1)
	go func() {
		_, err := session.RunTurn(context.Background(), TurnRequest{
			Conversation: ConversationState{Key: "conversation-busy"},
			Message:      InboundMessage{Kind: MessageKindText, Text: "hello"},
		})
		runErrCh <- err
	}()

	<-started

	_, err = session.RunTurn(context.Background(), TurnRequest{
		Conversation: ConversationState{Key: "conversation-busy"},
		Message:      InboundMessage{Kind: MessageKindText, Text: "again"},
	})
	if !errors.Is(err, ErrSessionBusy) {
		t.Fatalf("expected ErrSessionBusy, got %v", err)
	}

	close(release)

	select {
	case <-runErrCh:
	case <-time.After(time.Second):
		t.Fatal("first run turn did not finish")
	}
}

func TestDecodeClaudeStreamRoleMessageParsesAssistantContent(t *testing.T) {
	t.Parallel()

	msg := &claudecode.Message{
		Type: "assistant",
		Message: json.RawMessage(`{
			"role":"assistant",
			"content":[
				{"type":"text","text":"hello"},
				{"type":"thinking","thinking":"reasoning"},
				{"type":"tool_use","name":"Read","input":{"file_path":"runner_claude.go"}}
			]
		}`),
	}

	roleMessage, err := claudecode.DecodeStreamRoleMessage(msg)
	if err != nil {
		t.Fatalf("decodeClaudeStreamRoleMessage failed: %v", err)
	}
	if roleMessage.Role != "assistant" {
		t.Fatalf("unexpected role: %s", roleMessage.Role)
	}
	if len(roleMessage.Content) != 3 {
		t.Fatalf("unexpected content count: %d", len(roleMessage.Content))
	}
	if roleMessage.Content[0].Text != "hello" {
		t.Fatalf("unexpected text content: %s", roleMessage.Content[0].Text)
	}
	if roleMessage.Content[1].Thinking != "reasoning" {
		t.Fatalf("unexpected thinking content: %s", roleMessage.Content[1].Thinking)
	}
	if roleMessage.Content[2].Name != "Read" {
		t.Fatalf("unexpected tool name: %s", roleMessage.Content[2].Name)
	}
	if roleMessage.Content[2].Input["file_path"] != "runner_claude.go" {
		t.Fatalf("unexpected tool input: %+v", roleMessage.Content[2].Input)
	}
}

func TestParseClaudeResultUsage(t *testing.T) {
	t.Parallel()

	inputTokens, outputTokens := claudecode.ParseResultUsage(map[string]any{
		"usage": map[string]any{
			"input_tokens":  float64(21),
			"output_tokens": json.Number("34"),
		},
	})
	if inputTokens != 21 {
		t.Fatalf("unexpected input tokens: %d", inputTokens)
	}
	if outputTokens != 34 {
		t.Fatalf("unexpected output tokens: %d", outputTokens)
	}
}

func TestClaudeCodeRunnerApplyArgumentFilesCachesPaths(t *testing.T) {
	t.Parallel()

	runner := NewClaudeCodeRunner(ClaudeCodeRunnerOptions{})
	options := claudecode.RunOptions{
		SystemPrompt: "system prompt",
		AppendPrompt: "append prompt",
		MCPConfigs:   []string{`{"mcpServers":{"assistant":{"type":"sdk"}}}`},
	}
	err := runner.applyArgumentFiles(&options)
	if err != nil {
		t.Fatalf("applyArgumentFiles failed: %v", err)
	}
	t.Cleanup(func() {
		_ = runner.Close()
	})

	args := claudecode.BuildCLIArgs(&options)
	if !slices.Contains(args, "--system-prompt-file") || !slices.Contains(args, "--append-system-prompt-file") || !slices.Contains(args, "--mcp-config") {
		t.Fatalf("expected prompt and mcp args, got %q", args)
	}

	firstSystemPath := options.SystemPrompt
	firstAppendPath := options.AppendPrompt
	firstMCPPath := options.MCPConfigs[0]

	otherOptions := claudecode.RunOptions{
		SystemPrompt: "system prompt",
		AppendPrompt: "append prompt",
		MCPConfigs:   []string{`{"mcpServers":{"assistant":{"type":"sdk"}}}`},
	}
	err = runner.applyArgumentFiles(&otherOptions)
	if err != nil {
		t.Fatalf("second applyArgumentFiles failed: %v", err)
	}
	if otherOptions.SystemPrompt != firstSystemPath {
		t.Fatalf("expected cached system prompt path, got %q want %q", otherOptions.SystemPrompt, firstSystemPath)
	}
	if otherOptions.AppendPrompt != firstAppendPath {
		t.Fatalf("expected cached append prompt path, got %q want %q", otherOptions.AppendPrompt, firstAppendPath)
	}
	if otherOptions.MCPConfigs[0] != firstMCPPath {
		t.Fatalf("expected cached mcp config path, got %q want %q", otherOptions.MCPConfigs[0], firstMCPPath)
	}

	systemData, err := readTestFile(firstSystemPath)
	if err != nil {
		t.Fatalf("ReadFile system prompt failed: %v", err)
	}
	if string(systemData) != "system prompt" {
		t.Fatalf("unexpected system prompt content: %q", string(systemData))
	}

	appendData, err := readTestFile(firstAppendPath)
	if err != nil {
		t.Fatalf("ReadFile append prompt failed: %v", err)
	}
	if string(appendData) != "append prompt" {
		t.Fatalf("unexpected append prompt content: %q", string(appendData))
	}

	mcpData, err := readTestFile(firstMCPPath)
	if err != nil {
		t.Fatalf("ReadFile mcp config failed: %v", err)
	}
	if !strings.Contains(string(mcpData), `"type":"sdk"`) {
		t.Fatalf("unexpected mcp config content: %q", string(mcpData))
	}
}

func TestClaudeCodeRunnerApplyArgumentFilesRecreatesMissingCachedFile(t *testing.T) {
	t.Parallel()

	runner := NewClaudeCodeRunner(ClaudeCodeRunnerOptions{})
	options := claudecode.RunOptions{
		AppendPrompt: "append prompt",
	}
	err := runner.applyArgumentFiles(&options)
	if err != nil {
		t.Fatalf("applyArgumentFiles failed: %v", err)
	}
	t.Cleanup(func() {
		_ = runner.Close()
	})

	firstPath := options.AppendPrompt
	claudecode.RemoveTempFile(firstPath)

	nextOptions := claudecode.RunOptions{
		AppendPrompt: "append prompt",
	}
	err = runner.applyArgumentFiles(&nextOptions)
	if err != nil {
		t.Fatalf("second applyArgumentFiles failed: %v", err)
	}
	if nextOptions.AppendPrompt == firstPath {
		t.Fatalf("expected recreated prompt file path to differ after deletion: %q", firstPath)
	}

	appendData, err := readTestFile(nextOptions.AppendPrompt)
	if err != nil {
		t.Fatalf("ReadFile recreated append prompt failed: %v", err)
	}
	if string(appendData) != "append prompt" {
		t.Fatalf("unexpected recreated append prompt content: %q", string(appendData))
	}
}

func TestClaudeCodeRunnerApplyArgumentFilesRollsBackNewFilesOnFailure(t *testing.T) {
	t.Parallel()

	runner := NewClaudeCodeRunner(ClaudeCodeRunnerOptions{})
	var createdPath string
	callCount := 0
	runner.argFileWriter = func(name string, content string) (string, error) {
		callCount++
		if callCount == 1 {
			path, err := claudecode.WriteArgumentTempFile(name, content)
			if err == nil {
				createdPath = path
			}
			return path, err
		}
		return "", errors.New("write failed")
	}

	options := claudecode.RunOptions{
		SystemPrompt: "system prompt",
		AppendPrompt: "append prompt",
	}
	err := runner.applyArgumentFiles(&options)
	if err == nil {
		t.Fatal("applyArgumentFiles returned nil, want error")
	}
	if createdPath == "" {
		t.Fatal("expected first temp file to be created")
	}
	if exists, statErr := claudecode.TempFileExists(createdPath); statErr != nil {
		t.Fatalf("TempFileExists failed: %v", statErr)
	} else if exists {
		t.Fatalf("expected rolled back temp file to be removed: %s", createdPath)
	}
	if len(runner.argFiles) != 0 {
		t.Fatalf("expected temp file cache to be rolled back, got %d entries", len(runner.argFiles))
	}
}

type fakeClaudePersistentSession struct {
	runTurn              func(context.Context, []map[string]any) (*claudecode.ClaudeResult, error)
	interruptCurrentTurn func(context.Context) error
	closeFunc            func() error
	currentSessionID     string
}

func (s *fakeClaudePersistentSession) RunTurn(ctx context.Context, blocks []map[string]any) (*claudecode.ClaudeResult, error) {
	if s.runTurn == nil {
		return nil, errors.New("runTurn is nil")
	}
	result, err := s.runTurn(ctx, blocks)
	if result != nil && strings.TrimSpace(result.SessionID) != "" {
		s.currentSessionID = strings.TrimSpace(result.SessionID)
	}
	return result, err
}

func (s *fakeClaudePersistentSession) SessionID() string {
	return s.currentSessionID
}

func (s *fakeClaudePersistentSession) Interrupt(ctx context.Context) error {
	if s.interruptCurrentTurn == nil {
		return nil
	}
	return s.interruptCurrentTurn(ctx)
}

func (s *fakeClaudePersistentSession) Close() error {
	if s.closeFunc == nil {
		return nil
	}
	return s.closeFunc()
}

func readClaudeStreamInput(blocks []map[string]any) string {
	parts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		blockType, _ := block["type"].(string)
		text, _ := block["text"].(string)
		if blockType == "text" && text != "" {
			parts = append(parts, text)
		}
	}

	return strings.Join(parts, "\n")
}

func readTestFile(path string) ([]byte, error) {
	//nolint:gosec // Test paths are generated by the test subject with os.CreateTemp.
	return os.ReadFile(path)
}

func TestIgnoreExpectedClaudeExitReturnsNilForSIGTERM(t *testing.T) {
	t.Parallel()

	cmd := exec.CommandContext(context.Background(), "sh", "-c", "kill -TERM $$")
	err := cmd.Run()
	if err == nil {
		t.Fatal("Run returned nil, want exit error")
	}

	if got := claudecode.IgnoreExpectedExit(err); got != nil {
		t.Fatalf("ignoreExpectedClaudeExit returned %v, want nil", got)
	}
}

func TestIgnoreExpectedClaudeExitPreservesNonSignalExit(t *testing.T) {
	t.Parallel()

	cmd := exec.CommandContext(context.Background(), "sh", "-c", "exit 7")
	err := cmd.Run()
	if err == nil {
		t.Fatal("Run returned nil, want exit error")
	}

	got := claudecode.IgnoreExpectedExit(err)
	if got == nil {
		t.Fatal("ignoreExpectedClaudeExit returned nil, want error")
	}

	var exitErr *exec.ExitError
	if !errors.As(got, &exitErr) {
		t.Fatalf("ignoreExpectedClaudeExit returned %T, want *exec.ExitError", got)
	}
	if status, ok := exitErr.Sys().(syscall.WaitStatus); !ok || status.ExitStatus() != 7 {
		t.Fatalf("unexpected wait status: %#v", exitErr.Sys())
	}
}
