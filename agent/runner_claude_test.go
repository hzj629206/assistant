package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	claudecode "github.com/lancekrogers/claude-code-go/pkg/claude"
)

func TestNewClaudeCodeRunnerDefaultsPermissionModeToDefault(t *testing.T) {
	t.Parallel()

	runner := NewClaudeCodeRunner(ClaudeCodeRunnerOptions{})
	if runner.runOptions.PermissionMode != claudecode.PermissionModeDefault {
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
	var options []*claudecode.RunOptions
	runner := NewClaudeCodeRunner(ClaudeCodeRunnerOptions{
		RunOptions: claudecode.RunOptions{
			Model: "claude-sonnet-4-5",
		},
	})
	runner.executeTurn = func(_ context.Context, stdin io.Reader, opts *claudecode.RunOptions, _ *claudeControlServer) (*claudecode.ClaudeResult, error) {
		prompt, err := readClaudeStreamInput(stdin)
		if err != nil {
			return nil, err
		}
		prompts = append(prompts, prompt)
		options = append(options, opts)
		return &claudecode.ClaudeResult{
			Type:      "result",
			Result:    "assistant reply",
			SessionID: "session-new",
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
	if !strings.Contains(options[0].AppendPrompt, "Global system prompt.") {
		t.Fatalf("expected append prompt to include system prompt, got:\n%s", options[0].AppendPrompt)
	}
	if options[0].PermissionMode != claudecode.PermissionModeDefault {
		t.Fatalf("unexpected permission mode: %s", options[0].PermissionMode)
	}
}

func TestClaudeCodeRunnerRunTurnResumesExistingSession(t *testing.T) {
	t.Parallel()

	var prompt string
	var resumeID string
	var appendPrompt string
	runner := NewClaudeCodeRunner(ClaudeCodeRunnerOptions{})
	runner.executeTurn = func(_ context.Context, stdin io.Reader, opts *claudecode.RunOptions, _ *claudeControlServer) (*claudecode.ClaudeResult, error) {
		var err error
		prompt, err = readClaudeStreamInput(stdin)
		if err != nil {
			return nil, err
		}
		resumeID = opts.ResumeID
		appendPrompt = opts.AppendPrompt
		return &claudecode.ClaudeResult{
			Type:      "result",
			Result:    "follow-up reply",
			SessionID: "session-existing",
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

	if resumeID != "session-existing" {
		t.Fatalf("unexpected resume id: %s", resumeID)
	}
	if appendPrompt != "" {
		t.Fatalf("expected append prompt to be empty when resuming, got:\n%s", appendPrompt)
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
	runner.executeTurn = func(_ context.Context, stdin io.Reader, opts *claudecode.RunOptions, control *claudeControlServer) (*claudecode.ClaudeResult, error) {
		_, err := readClaudeStreamInput(stdin)
		if err != nil {
			return nil, err
		}
		argsSnapshot = buildClaudeSDKArgs(opts)
		controlSeen = control != nil
		return &claudecode.ClaudeResult{
			Type:      "result",
			Result:    "ok",
			SessionID: "session-new",
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
		resumeID  string
		prompt    string
		mcpConfig string
	)
	runner := NewClaudeCodeRunner(ClaudeCodeRunnerOptions{})
	runner.RegisterTools(uppercaseTool{})
	runner.executeTurn = func(ctx context.Context, stdin io.Reader, opts *claudecode.RunOptions, control *claudeControlServer) (*claudecode.ClaudeResult, error) {
		var err error
		prompt, err = readClaudeStreamInput(stdin)
		if err != nil {
			return nil, err
		}
		resumeID = opts.ResumeID
		if len(opts.MCPConfigs) != 1 {
			t.Fatalf("unexpected mcp config count: %d", len(opts.MCPConfigs))
		}
		mcpConfig = opts.MCPConfigs[0]
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
	if resumeID != "" {
		t.Fatalf("unexpected resume id: %s", resumeID)
	}
	if !strings.Contains(mcpConfig, `"type": "sdk"`) && !strings.Contains(mcpConfig, `"type":"sdk"`) {
		t.Fatalf("expected sdk mcp config, got: %s", mcpConfig)
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
	runner.executeTurn = func(_ context.Context, _ io.Reader, _ *claudecode.RunOptions, _ *claudeControlServer) (*claudecode.ClaudeResult, error) {
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
	runner.executeTurn = func(_ context.Context, _ io.Reader, _ *claudecode.RunOptions, _ *claudeControlServer) (*claudecode.ClaudeResult, error) {
		attempts++
		return nil, expectedErr
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
