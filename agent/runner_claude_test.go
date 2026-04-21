package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	claudecode "github.com/lancekrogers/claude-code-go/pkg/claude"
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

func TestClaudeCodeRunnerRunTurnStartsConversationAndStoresSessionID(t *testing.T) {
	t.Parallel()

	var prompts []string
	var options []claudecode.RunOptions
	runner := NewClaudeCodeRunner(ClaudeCodeRunnerOptions{
		RunOptions: claudecode.RunOptions{
			Model: "claude-sonnet-4-5",
		},
	})
	runner.sessionFactory = func(_ context.Context, _ string, opts claudecode.RunOptions) (claudePersistentTurnSession, error) {
		options = append(options, opts)
		return &fakeClaudePersistentSession{
			runTurn: func(_ context.Context, stdin io.Reader, _ *claudeControlServer) (*claudecode.ClaudeResult, error) {
				prompt, err := readClaudeStreamInput(stdin)
				if err != nil {
					return nil, err
				}
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

	result, err := runner.RunTurn(context.Background(), TurnRequest{
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
	if options[0].Format != claudecode.StreamJSONOutput {
		t.Fatalf("unexpected output format: %s", options[0].Format)
	}
	if options[0].InputFormat != claudecode.StreamJSONInput {
		t.Fatalf("unexpected input format: %s", options[0].InputFormat)
	}
	if !strings.Contains(options[0].AppendPrompt, "assistant-claude-append-system-prompt-") {
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

func TestClaudeCodeRunnerRunTurnResumesExistingSession(t *testing.T) {
	t.Parallel()

	var prompt string
	var options []claudecode.RunOptions
	runner := NewClaudeCodeRunner(ClaudeCodeRunnerOptions{})
	runner.sessionFactory = func(_ context.Context, _ string, opts claudecode.RunOptions) (claudePersistentTurnSession, error) {
		options = append(options, opts)
		return &fakeClaudePersistentSession{
			currentSessionID: "session-existing",
			runTurn: func(_ context.Context, stdin io.Reader, _ *claudeControlServer) (*claudecode.ClaudeResult, error) {
				var err error
				prompt, err = readClaudeStreamInput(stdin)
				if err != nil {
					return nil, err
				}
				return &claudecode.ClaudeResult{
					Type:      "result",
					Result:    "follow-up reply",
					SessionID: "session-existing",
				}, nil
			},
		}, nil
	}
	runner.RegisterSystemPrompt("Global system prompt.")

	_, err := runner.RunTurn(context.Background(), TurnRequest{
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
	runner.sessionFactory = func(_ context.Context, _ string, opts claudecode.RunOptions) (claudePersistentTurnSession, error) {
		argsSnapshot = buildClaudeSDKArgs(&opts)
		return &fakeClaudePersistentSession{
			runTurn: func(_ context.Context, stdin io.Reader, control *claudeControlServer) (*claudecode.ClaudeResult, error) {
				_, err := readClaudeStreamInput(stdin)
				if err != nil {
					return nil, err
				}
				controlSeen = control != nil
				return &claudecode.ClaudeResult{
					Type:      "result",
					Result:    "ok",
					SessionID: "session-new",
				}, nil
			},
		}, nil
	}

	_, err := runner.RunTurn(context.Background(), TurnRequest{
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
	runner.sessionFactory = func(_ context.Context, _ string, opts claudecode.RunOptions) (claudePersistentTurnSession, error) {
		if len(opts.MCPConfigs) != 1 {
			t.Fatalf("unexpected mcp config count: %d", len(opts.MCPConfigs))
		}
		mcpConfig = opts.MCPConfigs[0]
		return &fakeClaudePersistentSession{
			runTurn: func(ctx context.Context, stdin io.Reader, control *claudeControlServer) (*claudecode.ClaudeResult, error) {
				var err error
				prompt, err = readClaudeStreamInput(stdin)
				if err != nil {
					return nil, err
				}
				if control == nil || !control.hasTools() {
					t.Fatal("expected control server with tools")
				}

				listResponse, err := control.handleRequest(ctx, map[string]any{
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
				callResponse, err := control.handleRequest(ctx, map[string]any{
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

	result, err := runner.RunTurn(context.Background(), TurnRequest{
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
	if !strings.Contains(mcpConfig, "assistant-claude-mcp-config-") {
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

func TestClaudeCodeRunnerRetriesRetryableError(t *testing.T) {
	t.Parallel()

	attempts := 0
	runner := NewClaudeCodeRunner(ClaudeCodeRunnerOptions{
		RetryPolicy: &claudecode.RetryPolicy{
			MaxRetries:    1,
			BaseDelay:     time.Millisecond,
			MaxDelay:      time.Millisecond,
			BackoffFactor: 1,
		},
	})
	runner.sessionFactory = func(_ context.Context, _ string, _ claudecode.RunOptions) (claudePersistentTurnSession, error) {
		return &fakeClaudePersistentSession{
			runTurn: func(_ context.Context, _ io.Reader, _ *claudeControlServer) (*claudecode.ClaudeResult, error) {
				attempts++
				if attempts == 1 {
					return nil, &claudecode.ClaudeError{
						Type:    claudecode.ErrorNetwork,
						Message: "temporary network issue",
					}
				}
				return &claudecode.ClaudeResult{
					Type:      "result",
					Result:    "assistant reply",
					SessionID: "session-new",
				}, nil
			},
		}, nil
	}

	result, err := runner.RunTurn(context.Background(), TurnRequest{
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
	if attempts != 2 {
		t.Fatalf("unexpected attempt count: %d", attempts)
	}
	if result.ReplyText != "assistant reply" {
		t.Fatalf("unexpected reply: %s", result.ReplyText)
	}
}

func TestClaudeCodeRunnerDoesNotRetryNonRetryableError(t *testing.T) {
	t.Parallel()

	attempts := 0
	runner := NewClaudeCodeRunner(ClaudeCodeRunnerOptions{
		RetryPolicy: &claudecode.RetryPolicy{
			MaxRetries:    3,
			BaseDelay:     time.Millisecond,
			MaxDelay:      time.Millisecond,
			BackoffFactor: 1,
		},
	})
	expectedErr := &claudecode.ClaudeError{
		Type:    claudecode.ErrorAuthentication,
		Message: "invalid credentials",
	}
	runner.sessionFactory = func(_ context.Context, _ string, _ claudecode.RunOptions) (claudePersistentTurnSession, error) {
		return &fakeClaudePersistentSession{
			runTurn: func(_ context.Context, _ io.Reader, _ *claudeControlServer) (*claudecode.ClaudeResult, error) {
				attempts++
				return nil, expectedErr
			},
		}, nil
	}

	_, err := runner.RunTurn(context.Background(), TurnRequest{
		Conversation: ConversationState{Key: "conversation-1"},
		Message: InboundMessage{
			Kind:       MessageKindText,
			Sender:     "alice",
			SentAtUnix: 1000,
			Text:       "hello",
		},
	})
	if !errors.Is(err, expectedErr) {
		t.Fatalf("expected wrapped authentication error, got: %v", err)
	}
	if attempts != 1 {
		t.Fatalf("unexpected attempt count: %d", attempts)
	}
}

func TestClaudeCodeRunnerReusesLiveSessionForSameConversation(t *testing.T) {
	t.Parallel()

	createCount := 0
	runCount := 0
	runner := NewClaudeCodeRunner(ClaudeCodeRunnerOptions{})
	runner.sessionFactory = func(_ context.Context, _ string, _ claudecode.RunOptions) (claudePersistentTurnSession, error) {
		createCount++
		return &fakeClaudePersistentSession{
			currentSessionID: "session-live",
			runTurn: func(_ context.Context, _ io.Reader, _ *claudeControlServer) (*claudecode.ClaudeResult, error) {
				runCount++
				return &claudecode.ClaudeResult{
					Type:      "result",
					Result:    "ok",
					SessionID: "session-live",
				}, nil
			},
		}, nil
	}

	first, err := runner.RunTurn(context.Background(), TurnRequest{
		Conversation: ConversationState{Key: "conversation-1"},
		Message:      InboundMessage{Kind: MessageKindText, Text: "hello"},
	})
	if err != nil {
		t.Fatalf("first RunTurn failed: %v", err)
	}
	_, err = runner.RunTurn(context.Background(), TurnRequest{
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

func TestClaudeCodeRunnerExpiresIdleSession(t *testing.T) {
	t.Parallel()

	createCount := 0
	closeCount := 0
	runner := NewClaudeCodeRunner(ClaudeCodeRunnerOptions{
		SessionIdleTimeout: 20 * time.Millisecond,
	})
	runner.sessionFactory = func(_ context.Context, _ string, _ claudecode.RunOptions) (claudePersistentTurnSession, error) {
		createCount++
		return &fakeClaudePersistentSession{
			currentSessionID: "session-live",
			runTurn: func(_ context.Context, _ io.Reader, _ *claudeControlServer) (*claudecode.ClaudeResult, error) {
				return &claudecode.ClaudeResult{
					Type:      "result",
					Result:    "ok",
					SessionID: "session-live",
				}, nil
			},
			closeFunc: func() error {
				closeCount++
				return nil
			},
		}, nil
	}

	first, err := runner.RunTurn(context.Background(), TurnRequest{
		Conversation: ConversationState{Key: "conversation-1"},
		Message:      InboundMessage{Kind: MessageKindText, Text: "hello"},
	})
	if err != nil {
		t.Fatalf("first RunTurn failed: %v", err)
	}
	time.Sleep(80 * time.Millisecond)
	_, err = runner.RunTurn(context.Background(), TurnRequest{
		Conversation: ConversationState{Key: "conversation-1", RunnerThreadID: first.RunnerThreadID},
		Message:      InboundMessage{Kind: MessageKindText, Text: "again"},
	})
	if err != nil {
		t.Fatalf("second RunTurn failed: %v", err)
	}

	if createCount != 2 {
		t.Fatalf("unexpected session create count: %d", createCount)
	}
	if closeCount == 0 {
		t.Fatal("expected idle session to be closed")
	}
}

func TestClaudePersistentProcessSessionShouldInitializeOnlyOnce(t *testing.T) {
	t.Parallel()

	session := &claudePersistentProcessSession{}
	control := newClaudeControlServer(TurnRequest{}, []Tool{uppercaseTool{}})

	if !session.shouldInitialize(control) {
		t.Fatal("expected first turn to require initialize")
	}

	session.markInitialized()

	if session.shouldInitialize(control) {
		t.Fatal("did not expect initialize after session is marked initialized")
	}
}

func TestClaudePersistentProcessSessionHandleSystemMessageUpdatesSessionID(t *testing.T) {
	t.Parallel()

	session := &claudePersistentProcessSession{
		conversationKey: "conversation-1",
		cmd:             fakeClaudeExecCmd(1234),
	}

	envelope := map[string]any{
		"type":       "system",
		"session_id": "session-from-system",
	}
	line, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	session.handleStreamMessage(envelope, string(line))

	if got := session.CurrentSessionID(); got != "session-from-system" {
		t.Fatalf("unexpected session id: %s", got)
	}
}

func TestClaudePersistentProcessSessionHandleResultMessageDeliversResult(t *testing.T) {
	t.Parallel()

	turn := &claudeTurnState{
		resultCh: make(chan *claudecode.ClaudeResult, 1),
	}
	session := &claudePersistentProcessSession{
		conversationKey: "conversation-1",
		cmd:             fakeClaudeExecCmd(1234),
		currentTurn:     turn,
	}

	envelope := map[string]any{
		"type":       "result",
		"session_id": "session-result",
		"result":     "assistant reply",
		"usage": map[string]any{
			"input_tokens":  float64(12),
			"output_tokens": float64(34),
		},
	}
	line, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	session.handleStreamMessage(envelope, string(line))

	select {
	case result := <-turn.resultCh:
		if result.Result != "assistant reply" {
			t.Fatalf("unexpected reply: %s", result.Result)
		}
		if result.SessionID != "session-result" {
			t.Fatalf("unexpected result session id: %s", result.SessionID)
		}
	case <-time.After(time.Second):
		t.Fatal("expected result message")
	}

	if got := session.CurrentSessionID(); got != "session-result" {
		t.Fatalf("unexpected session id: %s", got)
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

	roleMessage, err := decodeClaudeStreamRoleMessage(msg)
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

	inputTokens, outputTokens := parseClaudeResultUsage(map[string]any{
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

	args := buildClaudeProcessArgs(&options)
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
	removeClaudeTempFile(firstPath)

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

type fakeClaudePersistentSession struct {
	runTurn          func(context.Context, io.Reader, *claudeControlServer) (*claudecode.ClaudeResult, error)
	closeFunc        func() error
	currentSessionID string
}

func (s *fakeClaudePersistentSession) RunTurn(ctx context.Context, stdin io.Reader, control *claudeControlServer) (*claudecode.ClaudeResult, error) {
	if s.runTurn == nil {
		return nil, errors.New("runTurn is nil")
	}
	result, err := s.runTurn(ctx, stdin, control)
	if result != nil && strings.TrimSpace(result.SessionID) != "" {
		s.currentSessionID = strings.TrimSpace(result.SessionID)
	}
	return result, err
}

func (s *fakeClaudePersistentSession) CurrentSessionID() string {
	return s.currentSessionID
}

func (s *fakeClaudePersistentSession) Close() error {
	if s.closeFunc == nil {
		return nil
	}
	return s.closeFunc()
}

func readClaudeStreamInput(stdin io.Reader) (string, error) {
	data, err := io.ReadAll(stdin)
	if err != nil {
		return "", err
	}

	var payload struct {
		Type    string `json:"type"`
		Message struct {
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text,omitempty"`
			} `json:"content"`
		} `json:"message"`
	}
	if err = json.Unmarshal(data, &payload); err != nil {
		return "", err
	}

	parts := make([]string, 0, len(payload.Message.Content))
	for _, block := range payload.Message.Content {
		if block.Type == "text" && block.Text != "" {
			parts = append(parts, block.Text)
		}
	}

	return strings.Join(parts, "\n"), nil
}

func readTestFile(path string) ([]byte, error) {
	//nolint:gosec // Test paths are generated by the test subject with os.CreateTemp.
	return os.ReadFile(path)
}

func fakeClaudeExecCmd(pid int) *exec.Cmd {
	cmd := &exec.Cmd{
		Process: &os.Process{
			Pid: pid,
		},
	}
	return cmd
}

func TestIgnoreExpectedClaudeExitReturnsNilForSIGTERM(t *testing.T) {
	t.Parallel()

	cmd := exec.CommandContext(context.Background(), "sh", "-c", "kill -TERM $$")
	err := cmd.Run()
	if err == nil {
		t.Fatal("Run returned nil, want exit error")
	}

	if got := ignoreExpectedClaudeExit(err); got != nil {
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

	got := ignoreExpectedClaudeExit(err)
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

func TestClaudePersistentProcessSessionCloseIgnoresUnexpectedExitSentinel(t *testing.T) {
	t.Parallel()

	exitDone := make(chan struct{})
	close(exitDone)
	stderrDone := make(chan struct{})
	close(stderrDone)
	scanDone := make(chan struct{})
	close(scanDone)

	session := &claudePersistentProcessSession{
		cmd:        fakeClaudeExecCmd(12345),
		waitErr:    errClaudeProcessExited,
		exitDone:   exitDone,
		stderrDone: stderrDone,
		scanDone:   scanDone,
	}

	if err := session.Close(); err != nil {
		t.Fatalf("Close returned %v, want nil", err)
	}
}
