package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	appcodex "github.com/pmenglund/codex-sdk-go"
	"github.com/pmenglund/codex-sdk-go/protocol"
	apprpc "github.com/pmenglund/codex-sdk-go/rpc"
)

type fakeAppServerThread struct {
	id string
}

type fakeAppServerTurnStream struct {
	notifications []apprpc.Notification
	index         int
	turnID        string
}

func (t *fakeAppServerThread) ID() string {
	return t.id
}

func (t *fakeAppServerThread) RunStreamed(context.Context, []appcodex.Input, *appcodex.TurnOptions) (appServerTurnStream, error) {
	return nil, errors.New("unexpected streamed turn")
}

func (s *fakeAppServerTurnStream) Next(context.Context) (apprpc.Notification, error) {
	if s.index >= len(s.notifications) {
		return apprpc.Notification{}, errors.New("stream exhausted")
	}

	note := s.notifications[s.index]
	s.index++
	return note, nil
}

func (s *fakeAppServerTurnStream) TurnID() string {
	return s.turnID
}

func (s *fakeAppServerTurnStream) Close() {}

func TestBuildTurnInputsUsesLocalImagePath(t *testing.T) {
	t.Parallel()

	file, err := os.CreateTemp(t.TempDir(), "assistant-appserver-runner-image-*.png")
	if err != nil {
		t.Fatalf("create temp file failed: %v", err)
	}
	if _, err := file.WriteString("image"); err != nil {
		t.Fatalf("write temp file failed: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close temp file failed: %v", err)
	}

	runner := &AppServerRunner{}
	inputs := runner.buildTurnInputs(TurnRequest{
		Message: InboundMessage{
			Kind:      MessageKindImage,
			ImagePath: file.Name(),
		},
	})

	if len(inputs) != 2 {
		t.Fatalf("unexpected input count: %d", len(inputs))
	}
	if inputs[0].Type != appcodex.InputTypeText {
		t.Fatalf("unexpected first input type: %s", inputs[0].Type)
	}
	if inputs[1].Type != appcodex.InputTypeLocalImage {
		t.Fatalf("unexpected second input type: %s", inputs[1].Type)
	}
	if inputs[1].Path != file.Name() {
		t.Fatalf("unexpected image path: %s", inputs[1].Path)
	}
}

func TestBuildTurnInputsUsesMixedMessageImagePaths(t *testing.T) {
	t.Parallel()

	fileOne, err := os.CreateTemp(t.TempDir(), "assistant-appserver-runner-mixed-1-*.png")
	if err != nil {
		t.Fatalf("create temp file failed: %v", err)
	}
	if err := fileOne.Close(); err != nil {
		t.Fatalf("close temp file failed: %v", err)
	}

	fileTwo, err := os.CreateTemp(t.TempDir(), "assistant-appserver-runner-mixed-2-*.png")
	if err != nil {
		t.Fatalf("create temp file failed: %v", err)
	}
	if err := fileTwo.Close(); err != nil {
		t.Fatalf("close temp file failed: %v", err)
	}

	runner := &AppServerRunner{}
	inputs := runner.buildTurnInputs(TurnRequest{
		Message: InboundMessage{
			Kind:       MessageKindMixed,
			Text:       "mixed content",
			ImagePaths: []string{fileOne.Name(), fileTwo.Name()},
		},
	})

	if len(inputs) != 3 {
		t.Fatalf("unexpected input count: %d", len(inputs))
	}
	if inputs[0].Type != appcodex.InputTypeText {
		t.Fatalf("unexpected first input type: %s", inputs[0].Type)
	}
	if inputs[1].Type != appcodex.InputTypeLocalImage || inputs[2].Type != appcodex.InputTypeLocalImage {
		t.Fatalf("unexpected image input types: %+v", inputs)
	}
}

func TestBuildTurnInputsDoesNotInjectToolInstructionsForNewConversation(t *testing.T) {
	t.Parallel()

	runner := &AppServerRunner{}
	runner.RegisterSystemPrompt("Global system prompt.")
	runner.RegisterTools(uppercaseTool{})

	inputs := runner.buildTurnInputs(TurnRequest{
		Message: InboundMessage{
			Kind: MessageKindText,
			Text: "hello",
		},
	})

	if len(inputs) != 1 {
		t.Fatalf("unexpected input count: %d", len(inputs))
	}
	if strings.Contains(inputs[0].Text, "Global system prompt.") {
		t.Fatalf("did not expect system prompt in input: %s", inputs[0].Text)
	}
	if strings.Contains(inputs[0].Text, "structured tool loop") {
		t.Fatalf("did not expect tool instruction in input: %s", inputs[0].Text)
	}
	if !strings.Contains(inputs[0].Text, "hello") {
		t.Fatalf("user message not preserved: %s", inputs[0].Text)
	}
}

func TestBuildTurnInputsSkipsInitialContextForExistingConversation(t *testing.T) {
	t.Parallel()

	runner := &AppServerRunner{}
	runner.RegisterSystemPrompt("Global system prompt.")
	runner.RegisterTools(uppercaseTool{})

	inputs := runner.buildTurnInputs(TurnRequest{
		Conversation: ConversationState{
			RunnerThreadID: "thread-1",
		},
		Message: InboundMessage{
			Kind: MessageKindText,
			Text: "hello",
		},
	})

	if len(inputs) != 1 {
		t.Fatalf("unexpected input count: %d", len(inputs))
	}
	if inputs[0].Text != "Current message context:\n- time: unknown\n- sender: `unknown`\n\nhello" {
		t.Fatalf("unexpected input text: %s", inputs[0].Text)
	}
}

func TestAppServerRunTurnReturnsErrorForNilRunner(t *testing.T) {
	t.Parallel()

	var runner *AppServerRunner
	_, err := runner.RunTurn(context.Background(), TurnRequest{})
	if err == nil || err.Error() != "run app-server turn failed: runner is nil" {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestAppServerRunTurnStartsThreadAndReturnsReply(t *testing.T) {
	t.Parallel()

	thread := &fakeAppServerThread{id: "thread-new"}
	var startedOptions appcodex.ThreadStartOptions
	var receivedInputs []appcodex.Input
	runner := &AppServerRunner{
		startThread: func(_ context.Context, options appcodex.ThreadStartOptions) (appServerThread, error) {
			startedOptions = options
			return thread, nil
		},
		runThreadTurnFn: func(_ context.Context, _ TurnRequest, _ appServerThread, inputs []appcodex.Input, _ *appcodex.TurnOptions) (*appcodex.TurnResult, error) {
			receivedInputs = append(receivedInputs, inputs...)
			return &appcodex.TurnResult{
				FinalResponse: "hello back",
			}, nil
		},
	}

	result, err := runner.RunTurn(context.Background(), TurnRequest{
		Message: InboundMessage{
			Kind: MessageKindText,
			Text: "hello",
		},
	})
	if err != nil {
		t.Fatalf("run turn failed: %v", err)
	}
	if result.RunnerThreadID != "thread-new" {
		t.Fatalf("unexpected thread id: %s", result.RunnerThreadID)
	}
	if result.ReplyText != "hello back" {
		t.Fatalf("unexpected reply: %s", result.ReplyText)
	}
	if startedOptions.DeveloperInstructions != "" {
		t.Fatalf("unexpected developer instructions: %q", startedOptions.DeveloperInstructions)
	}
	if len(receivedInputs) != 1 || receivedInputs[0].Text != "Current message context:\n- time: unknown\n- sender: `unknown`\n\nhello" {
		t.Fatalf("unexpected thread inputs: %+v", receivedInputs)
	}
}

func TestAppServerRunTurnPassesSystemPromptAsDeveloperInstructions(t *testing.T) {
	t.Parallel()

	thread := &fakeAppServerThread{id: "thread-new"}
	var startedOptions appcodex.ThreadStartOptions
	var receivedInputs []appcodex.Input
	runner := &AppServerRunner{
		startOptions: appcodex.ThreadStartOptions{
			DeveloperInstructions: "Base developer instruction.",
		},
		startThread: func(_ context.Context, options appcodex.ThreadStartOptions) (appServerThread, error) {
			startedOptions = options
			return thread, nil
		},
		runThreadTurnFn: func(_ context.Context, _ TurnRequest, _ appServerThread, inputs []appcodex.Input, _ *appcodex.TurnOptions) (*appcodex.TurnResult, error) {
			receivedInputs = append(receivedInputs, inputs...)
			return &appcodex.TurnResult{FinalResponse: "hello back"}, nil
		},
	}
	runner.RegisterSystemPrompt("Global system prompt.")

	_, err := runner.RunTurn(context.Background(), TurnRequest{
		Message: InboundMessage{
			Kind: MessageKindText,
			Text: "hello",
		},
	})
	if err != nil {
		t.Fatalf("run turn failed: %v", err)
	}
	if startedOptions.DeveloperInstructions != "Base developer instruction.\n\nGlobal system prompt." {
		t.Fatalf("unexpected developer instructions: %q", startedOptions.DeveloperInstructions)
	}
	if len(receivedInputs) != 1 {
		t.Fatalf("unexpected thread inputs: %+v", receivedInputs)
	}
	if strings.Contains(receivedInputs[0].Text, "Global system prompt.") {
		t.Fatalf("did not expect system prompt in turn input: %s", receivedInputs[0].Text)
	}
}

func TestAppServerRunTurnResumesExistingThreadAndFallsBackToConversationID(t *testing.T) {
	t.Parallel()

	var resumedThreadID string
	thread := &fakeAppServerThread{}
	runner := &AppServerRunner{
		resumeThread: func(_ context.Context, options appcodex.ThreadResumeOptions) (appServerThread, error) {
			resumedThreadID = options.ThreadID
			return thread, nil
		},
		runThreadTurnFn: func(context.Context, TurnRequest, appServerThread, []appcodex.Input, *appcodex.TurnOptions) (*appcodex.TurnResult, error) {
			return &appcodex.TurnResult{
				FinalResponse: "welcome back",
			}, nil
		},
	}

	result, err := runner.RunTurn(context.Background(), TurnRequest{
		Conversation: ConversationState{
			RunnerThreadID: "thread-existing",
		},
		Message: InboundMessage{
			Kind: MessageKindText,
			Text: "hello again",
		},
	})
	if err != nil {
		t.Fatalf("run turn failed: %v", err)
	}
	if resumedThreadID != "thread-existing" {
		t.Fatalf("unexpected resumed thread id: %s", resumedThreadID)
	}
	if result.RunnerThreadID != "thread-existing" {
		t.Fatalf("unexpected result thread id: %s", result.RunnerThreadID)
	}
	if result.ReplyText != "welcome back" {
		t.Fatalf("unexpected reply: %s", result.ReplyText)
	}
}

func TestAppServerRunTurnDoesNotPassSystemPromptAsDeveloperInstructionsOnResume(t *testing.T) {
	t.Parallel()

	thread := &fakeAppServerThread{id: "thread-existing"}
	var resumedOptions appcodex.ThreadResumeOptions
	runner := &AppServerRunner{
		resumeOptions: appcodex.ThreadResumeOptions{
			DeveloperInstructions: "Base resume instruction.",
		},
		resumeThread: func(_ context.Context, options appcodex.ThreadResumeOptions) (appServerThread, error) {
			resumedOptions = options
			return thread, nil
		},
		runThreadTurnFn: func(context.Context, TurnRequest, appServerThread, []appcodex.Input, *appcodex.TurnOptions) (*appcodex.TurnResult, error) {
			return &appcodex.TurnResult{FinalResponse: "welcome back"}, nil
		},
	}
	runner.RegisterSystemPrompt("Global system prompt.")

	_, err := runner.RunTurn(context.Background(), TurnRequest{
		Conversation: ConversationState{
			RunnerThreadID: "thread-existing",
		},
		Message: InboundMessage{
			Kind: MessageKindText,
			Text: "hello again",
		},
	})
	if err != nil {
		t.Fatalf("run turn failed: %v", err)
	}
	if resumedOptions.ThreadID != "thread-existing" {
		t.Fatalf("unexpected resumed thread id: %s", resumedOptions.ThreadID)
	}
	if resumedOptions.DeveloperInstructions != "Base resume instruction." {
		t.Fatalf("unexpected developer instructions: %q", resumedOptions.DeveloperInstructions)
	}
}

func TestAppServerRunTurnInvalidatesRecoverableRPCClientError(t *testing.T) {
	t.Parallel()

	clientClosed := false
	runner := &AppServerRunner{
		rpcClient: &apprpc.Client{},
		closeFn:   func() error { clientClosed = true; return nil },
		resumeThread: func(_ context.Context, _ appcodex.ThreadResumeOptions) (appServerThread, error) {
			return nil, errors.New("connection closed")
		},
		activeTurns: make(map[string]appServerActiveTurn),
	}

	_, err := runner.RunTurn(context.Background(), TurnRequest{
		Conversation: ConversationState{RunnerThreadID: "thread-existing"},
		Message:      InboundMessage{Kind: MessageKindText, Text: "hello"},
	})
	if err == nil {
		t.Fatal("RunTurn returned nil error")
	}
	if runner.rpcClient != nil {
		t.Fatal("expected rpcClient to be invalidated")
	}
	if runner.startThread != nil || runner.resumeThread != nil {
		t.Fatal("expected rpc thread hooks to be cleared")
	}
	if !clientClosed {
		t.Fatal("expected invalidation to close the old client")
	}
}

func TestEnsureRPCClientRecreatesMissingClient(t *testing.T) {
	t.Parallel()

	createCount := 0
	runner := &AppServerRunner{
		canRecoverRPCClient: true,
		rpcClientFactory: func(context.Context, apprpc.ServerRequestHandler) (*apprpc.Client, func() error, bool, error) {
			createCount++
			return &apprpc.Client{}, func() error { return nil }, true, nil
		},
		activeTurns: make(map[string]appServerActiveTurn),
	}

	if err := runner.ensureRPCClient(context.Background()); err != nil {
		t.Fatalf("ensureRPCClient failed: %v", err)
	}
	if createCount != 1 {
		t.Fatalf("unexpected create count after first ensure: %d", createCount)
	}
	if runner.rpcClient == nil {
		t.Fatal("expected rpcClient to be recreated")
	}
	if runner.startThread == nil || runner.resumeThread == nil {
		t.Fatal("expected rpc thread hooks to be rebound")
	}

	runner.bindRPCClient(nil, nil)

	if err := runner.ensureRPCClient(context.Background()); err != nil {
		t.Fatalf("second ensureRPCClient failed: %v", err)
	}
	if createCount != 2 {
		t.Fatalf("unexpected create count after second ensure: %d", createCount)
	}
}

func TestAppServerRunTurnUsesFrozenPromptSnapshotForNewThread(t *testing.T) {
	t.Parallel()

	thread := &fakeAppServerThread{id: "thread-new"}
	var runner *AppServerRunner
	runner = &AppServerRunner{
		startThread: func(_ context.Context, options appcodex.ThreadStartOptions) (appServerThread, error) {
			if options.DeveloperInstructions != "Prompt A" {
				t.Fatalf("unexpected developer instructions: %q", options.DeveloperInstructions)
			}
			runner.RegisterSystemPrompt("Prompt B")
			return thread, nil
		},
		runThreadTurnFn: func(_ context.Context, _ TurnRequest, _ appServerThread, _ []appcodex.Input, _ *appcodex.TurnOptions) (*appcodex.TurnResult, error) {
			return &appcodex.TurnResult{FinalResponse: "ok"}, nil
		},
	}
	runner.RegisterSystemPrompt("Prompt A")

	_, err := runner.RunTurn(context.Background(), TurnRequest{
		Message: InboundMessage{
			Kind: MessageKindText,
			Text: "hello",
		},
	})
	if err != nil {
		t.Fatalf("run turn failed: %v", err)
	}
}

func TestMcpServerElicitationRequestDeclinesByDefault(t *testing.T) {
	t.Parallel()

	runner := &AppServerRunner{}
	response, err := runner.McpServerElicitationRequest(context.Background(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if response == nil {
		t.Fatal("expected response")
	}

	typed, ok := any(*response).(protocol.SanitizedMCPServerElicitationRequestResponseJSON)
	if !ok {
		t.Fatalf("unexpected response type: %T", *response)
	}
	if typed.Action != protocol.MCPServerElicitationActionDecline {
		t.Fatalf("unexpected action: %s", typed.Action)
	}
	if typed.Content != nil {
		t.Fatalf("expected nil content, got: %#v", typed.Content)
	}
}

func TestMcpServerElicitationRequestAutoAcceptsEmptySchemaMCPToolApproval(t *testing.T) {
	t.Parallel()

	runner := &AppServerRunner{}
	response, err := runner.McpServerElicitationRequest(context.Background(), map[string]any{
		"_meta": map[string]any{
			"codex_approval_kind": "mcp_tool_call",
		},
		"serverName": "demo",
		"message":    "Allow tool run?",
		"requestedSchema": map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if response == nil {
		t.Fatal("expected response")
	}

	typed, ok := any(*response).(protocol.SanitizedMCPServerElicitationRequestResponseJSON)
	if !ok {
		t.Fatalf("unexpected response type: %T", *response)
	}
	if typed.Action != protocol.MCPServerElicitationActionAccept {
		t.Fatalf("unexpected action: %s", typed.Action)
	}
	content, ok := typed.Content.(map[string]any)
	if !ok {
		t.Fatalf("unexpected content type: %T", typed.Content)
	}
	if len(content) != 0 {
		t.Fatalf("expected empty content, got: %#v", content)
	}
}

func TestMcpServerElicitationRequestDeclinesStructuredPayload(t *testing.T) {
	t.Parallel()

	runner := &AppServerRunner{}
	response, err := runner.McpServerElicitationRequest(context.Background(), map[string]any{
		"_meta": map[string]any{
			"codex_approval_kind": "mcp_tool_call",
		},
		"serverName": "demo",
		"message":    "Need more input",
		"requestedSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"target": map[string]any{
					"type":  "string",
					"title": "Target",
				},
			},
			"required": []string{"target"},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if response == nil {
		t.Fatal("expected response")
	}

	typed, ok := any(*response).(protocol.SanitizedMCPServerElicitationRequestResponseJSON)
	if !ok {
		t.Fatalf("unexpected response type: %T", *response)
	}
	if typed.Action != protocol.MCPServerElicitationActionDecline {
		t.Fatalf("unexpected action: %s", typed.Action)
	}
}

func TestApplyPatchApprovalApprovesByDefault(t *testing.T) {
	t.Parallel()

	runner := &AppServerRunner{}
	response, err := runner.ApplyPatchApproval(context.Background(), protocol.SanitizedApplyPatchApprovalParamsJSON{
		ConversationID: "conv-1",
		CallID:         "call-1",
		FileChanges:    map[string]any{},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if response == nil {
		t.Fatal("expected response")
	}

	typed, ok := any(*response).(protocol.SanitizedApplyPatchApprovalResponseJSON)
	if !ok {
		t.Fatalf("unexpected response type: %T", *response)
	}
	if typed.Decision != "approved" {
		t.Fatalf("unexpected decision: %v", typed.Decision)
	}
}

func TestExecCommandApprovalApprovesByDefault(t *testing.T) {
	t.Parallel()

	runner := &AppServerRunner{}
	response, err := runner.ExecCommandApproval(context.Background(), protocol.SanitizedExecCommandApprovalParamsJSON{
		ConversationID: "conv-1",
		CallID:         "call-1",
		Command:        []string{"pwd"},
		Cwd:            "/tmp",
		ParsedCmd:      []protocol.SanitizedExecCommandApprovalParamsJSONParsedCmdElem{},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if response == nil {
		t.Fatal("expected response")
	}

	typed, ok := any(*response).(protocol.SanitizedExecCommandApprovalResponseJSON)
	if !ok {
		t.Fatalf("unexpected response type: %T", *response)
	}
	if typed.Decision != "approved" {
		t.Fatalf("unexpected decision: %v", typed.Decision)
	}
}

func TestItemCommandExecutionRequestApprovalAcceptsByDefault(t *testing.T) {
	t.Parallel()

	runner := &AppServerRunner{}
	response, err := runner.ItemCommandExecutionRequestApproval(context.Background(), protocol.CommandExecutionRequestApprovalParams{
		ThreadID: "thread-1",
		TurnID:   "turn-1",
		ItemID:   "item-1",
		Command:  stringPtr("pwd"),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if response == nil {
		t.Fatal("expected response")
	}
	if response.Decision != "accept" {
		t.Fatalf("unexpected decision: %v", response.Decision)
	}
}

func TestItemFileChangeRequestApprovalAcceptsByDefault(t *testing.T) {
	t.Parallel()

	runner := &AppServerRunner{}
	response, err := runner.ItemFileChangeRequestApproval(context.Background(), protocol.SanitizedFileChangeRequestApprovalParamsJSON{
		ThreadID: "thread-1",
		TurnID:   "turn-1",
		ItemID:   "item-1",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if response == nil {
		t.Fatal("expected response")
	}

	typed, ok := any(*response).(protocol.SanitizedFileChangeRequestApprovalResponseJSON)
	if !ok {
		t.Fatalf("unexpected response type: %T", *response)
	}
	if typed.Decision != "accept" {
		t.Fatalf("unexpected decision: %v", typed.Decision)
	}
}

func TestItemPermissionsRequestApprovalMirrorsRequestedPermissions(t *testing.T) {
	t.Parallel()

	runner := &AppServerRunner{}
	permissions := map[string]any{
		"shell": "allow",
	}
	response, err := runner.ItemPermissionsRequestApproval(context.Background(), protocol.PermissionsRequestApprovalParams{
		ThreadID:    "thread-1",
		TurnID:      "turn-1",
		ItemID:      "item-1",
		Permissions: permissions,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if response == nil {
		t.Fatal("expected response")
	}
	if !reflect.DeepEqual(response.Permissions, permissions) {
		t.Fatalf("unexpected permissions: %#v", response.Permissions)
	}
}

func TestSandboxPolicyString(t *testing.T) {
	t.Parallel()

	if got := SandboxPolicyWorkspaceWrite.String(); got != "workspace-write" {
		t.Fatalf("unexpected sandbox policy string: %s", got)
	}
}

func TestParseAppServerItemExtractsNestedText(t *testing.T) {
	t.Parallel()

	note := appcodexNotification(t, map[string]any{
		"item": map[string]any{
			"assistant_message": map[string]any{
				"text": "nested text",
			},
		},
	})

	item, text := parseAppServerItem(note)
	if len(item) == 0 {
		t.Fatal("expected item payload")
	}
	if text != "nested text" {
		t.Fatalf("unexpected text: %q", text)
	}
}

func TestParseAppServerTurnErrorIgnoresRetryableErrorNotification(t *testing.T) {
	t.Parallel()

	note := appcodexNotification(t, map[string]any{
		"willRetry": true,
		"error": map[string]any{
			"message": "Reconnecting... 2/5",
		},
	})

	if !shouldRetryAppServerTurn(note) {
		t.Fatal("expected retryable app-server turn notification")
	}
	if err := parseAppServerTurnError(note); err == nil || err.Error() != "Reconnecting... 2/5" {
		t.Fatalf("unexpected parsed error: %v", err)
	}
}

func TestCollectStreamedTurnContinuesAfterRetryableErrorNotification(t *testing.T) {
	t.Parallel()

	runner := &AppServerRunner{}
	stream := &fakeAppServerTurnStream{
		notifications: []apprpc.Notification{
			{
				Method: "turn/started",
				Raw: appServerMustRaw(t, map[string]any{
					"turn": map[string]any{
						"id":     "turn-1",
						"status": "inProgress",
					},
				}),
			},
			{
				Method: "error",
				Raw: appServerMustRaw(t, map[string]any{
					"willRetry": true,
					"error": map[string]any{
						"message": "Reconnecting... 2/5",
					},
				}),
			},
			{
				Method: "item/completed",
				Raw: appServerMustRaw(t, map[string]any{
					"item": map[string]any{
						"assistant_message": map[string]any{
							"text": "Recovered response",
						},
					},
				}),
			},
			{
				Method: "turn/completed",
				Raw: appServerMustRaw(t, map[string]any{
					"turn": map[string]any{
						"id":     "turn-1",
						"status": "completed",
					},
				}),
			},
		},
	}

	result, err := runner.collectStreamedTurn(context.Background(), TurnRequest{
		Conversation: ConversationState{
			Key: "conv-1",
		},
	}, stream)
	if err != nil {
		t.Fatalf("collect streamed turn failed: %v", err)
	}
	if result.TurnID != "turn-1" {
		t.Fatalf("unexpected turn id: %s", result.TurnID)
	}
	if result.FinalResponse != "Recovered response" {
		t.Fatalf("unexpected final response: %q", result.FinalResponse)
	}
	if len(result.Items) != 1 {
		t.Fatalf("unexpected items count: %d", len(result.Items))
	}
}

func appcodexNotification(t *testing.T, payload map[string]any) apprpc.Notification {
	t.Helper()

	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload failed: %v", err)
	}

	return apprpc.Notification{Raw: raw}
}

func TestNewAppServerRunnerUsesExperimentalDynamicToolCalls(t *testing.T) {
	t.Parallel()

	cwd := filepath.Join(t.TempDir(), "workspace")
	if err := os.MkdirAll(cwd, 0o750); err != nil {
		t.Fatalf("create cwd failed: %v", err)
	}

	transcript := []apprpc.TranscriptEntry{
		appServerWriteLine(t, apprpc.JSONRPCRequest{
			ID:     apprpc.NewIntRequestID(1),
			Method: "initialize",
			Params: appServerMustRaw(t, map[string]any{
				"clientInfo": map[string]any{
					"name":    "assistant-test",
					"title":   "Assistant Test",
					"version": "test",
				},
				"capabilities": map[string]any{
					"experimentalApi": true,
					"optOutNotificationMethods": []string{
						"command/exec/outputDelta",
						"item/agentMessage/delta",
						"item/fileChange/outputDelta",
						"item/plan/delta",
						"item/reasoning/summaryTextDelta",
						"item/reasoning/textDelta",
					},
				},
			}),
		}),
		appServerReadLine(t, apprpc.JSONRPCResponse{
			ID:     apprpc.NewIntRequestID(1),
			Result: appServerMustRaw(t, map[string]any{}),
		}),
		appServerWriteLine(t, apprpc.JSONRPCNotification{Method: "initialized"}),
		appServerWriteLine(t, apprpc.JSONRPCRequest{
			ID:     apprpc.NewIntRequestID(2),
			Method: "thread/start",
			Params: appServerMustRaw(t, map[string]any{
				"model":                 defaultModel,
				"cwd":                   cwd,
				"approvalPolicy":        "never",
				"developerInstructions": "Global system prompt.",
				"config": map[string]any{
					"web_search": "live",
				},
				"sandbox": "read-only",
				"dynamicTools": []map[string]any{
					{
						"name":        "uppercase",
						"description": "Uppercase the provided text.",
						"inputSchema": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"text": map[string]any{"type": "string"},
							},
							"required":             []any{"text"},
							"additionalProperties": false,
						},
					},
				},
			}),
		}),
		appServerReadLine(t, apprpc.JSONRPCResponse{
			ID:     apprpc.NewIntRequestID(2),
			Result: appServerMustRaw(t, map[string]any{"threadId": "thr-native"}),
		}),
		appServerWriteLine(t, apprpc.JSONRPCRequest{
			ID:     apprpc.NewIntRequestID(3),
			Method: "turn/start",
			Params: appServerMustRaw(t, map[string]any{
				"threadId": "thr-native",
				"input": []map[string]any{
					{
						"type": "text",
						"text": "Current message context:\n- time: unknown\n- sender: `unknown`\n\nhello",
					},
				},
				"cwd":            cwd,
				"approvalPolicy": "never",
				"sandboxPolicy": map[string]any{
					"type": "readOnly",
				},
				"model":  defaultModel,
				"effort": "medium",
			}),
		}),
		appServerReadLine(t, apprpc.JSONRPCResponse{
			ID:     apprpc.NewIntRequestID(3),
			Result: appServerMustRaw(t, map[string]any{"turn": map[string]any{"id": "turn-native"}}),
		}),
		appServerReadLine(t, apprpc.JSONRPCNotification{
			Method: "turn/started",
			Params: appServerMustRaw(t, map[string]any{
				"threadId": "thr-native",
				"turn": map[string]any{
					"id":     "turn-native",
					"status": "inProgress",
				},
			}),
		}),
		appServerReadLine(t, apprpc.JSONRPCRequest{
			ID:     apprpc.NewIntRequestID(99),
			Method: "item/tool/call",
			Params: appServerMustRaw(t, map[string]any{
				"threadId": "thr-native",
				"turnId":   "turn-native",
				"callId":   "call-1",
				"tool":     "uppercase",
				"arguments": map[string]any{
					"text": "hello",
				},
			}),
		}),
		appServerWriteLine(t, apprpc.JSONRPCResponse{
			ID: apprpc.NewIntRequestID(99),
			Result: appServerMustRaw(t, map[string]any{
				"contentItems": []map[string]any{
					{
						"type": "inputText",
						"text": `{"text":"HELLO"}`,
					},
				},
				"success": true,
			}),
		}),
		appServerReadLine(t, apprpc.JSONRPCNotification{
			Method: "item/completed",
			Params: appServerMustRaw(t, map[string]any{
				"threadId": "thr-native",
				"item": map[string]any{
					"assistant_message": map[string]any{
						"text": "HELLO",
					},
				},
			}),
		}),
		appServerReadLine(t, apprpc.JSONRPCNotification{
			Method: "turn/completed",
			Params: appServerMustRaw(t, map[string]any{
				"threadId": "thr-native",
				"turn": map[string]any{
					"id":     "turn-native",
					"status": "completed",
				},
			}),
		}),
	}

	runner, err := NewAppServerRunner(context.Background(), AppServerRunnerOptions{
		CodexOptions: appcodex.Options{
			Transport: apprpc.NewReplayTransport(transcript),
			ClientInfo: protocol.ClientInfo{
				Name:    "assistant-test",
				Title:   stringPtr("Assistant Test"),
				Version: "test",
			},
		},
		StartOptions: appcodex.ThreadStartOptions{
			Model: defaultModel,
			Cwd:   cwd,
		},
		TurnOptions: appcodex.TurnOptions{
			Model:  defaultModel,
			Cwd:    cwd,
			Effort: appcodex.ReasoningEffortMedium,
		},
		SystemPrompt: "Global system prompt.",
		Tools:        []Tool{uppercaseTool{}},
	})
	if err != nil {
		t.Fatalf("create runner failed: %v", err)
	}
	defer func() {
		if closeErr := runner.Close(); closeErr != nil {
			t.Fatalf("close runner failed: %v", closeErr)
		}
	}()

	result, err := runner.RunTurn(context.Background(), TurnRequest{
		Message: InboundMessage{
			Kind: MessageKindText,
			Text: "hello",
		},
	})
	if err != nil {
		t.Fatalf("run turn failed: %v", err)
	}
	if result.RunnerThreadID != "thr-native" {
		t.Fatalf("unexpected thread id: %s", result.RunnerThreadID)
	}
	if result.ReplyText != "HELLO" {
		t.Fatalf("unexpected reply text: %q", result.ReplyText)
	}
}

func TestMatchesAppServerTurnFiltersByThreadAndTurn(t *testing.T) {
	t.Parallel()

	matching := apprpc.Notification{
		Method: "item/completed",
		Raw: appServerMustRaw(t, map[string]any{
			"threadId": "thread-1",
			"turnId":   "turn-1",
			"item": map[string]any{
				"assistant_message": map[string]any{"text": "ok"},
			},
		}),
	}
	if !matchesAppServerTurn(matching, "thread-1", "turn-1") {
		t.Fatal("expected matching turn notification")
	}

	wrongTurn := apprpc.Notification{
		Method: "error",
		Raw: appServerMustRaw(t, map[string]any{
			"threadId": "thread-1",
			"turnId":   "turn-2",
			"error":    map[string]any{"message": "wrong turn"},
		}),
	}
	if matchesAppServerTurn(wrongTurn, "thread-1", "turn-1") {
		t.Fatal("did not expect notification from another turn")
	}

	global := apprpc.Notification{
		Method: "account/rateLimits/updated",
		Raw: appServerMustRaw(t, map[string]any{
			"rateLimits": map[string]any{},
		}),
	}
	if matchesAppServerTurn(global, "thread-1", "turn-1") {
		t.Fatal("did not expect global notification to match a specific turn")
	}
}

func TestItemToolCallPrefersExactTurnMatch(t *testing.T) {
	t.Parallel()

	runner := &AppServerRunner{
		activeTurns: map[string]appServerActiveTurn{
			appServerActiveTurnKey("thread-1", ""): {
				req: TurnRequest{
					Conversation: ConversationState{Key: "fallback"},
				},
				tools: []Tool{snapshotTool{name: "fallback"}},
			},
			appServerActiveTurnKey("thread-1", "turn-1"): {
				req: TurnRequest{
					Conversation: ConversationState{Key: "exact"},
				},
				tools: []Tool{uppercaseTool{}},
			},
		},
	}

	response, err := runner.ItemToolCall(context.Background(), protocol.DynamicToolCallParams{
		ThreadID: "thread-1",
		TurnID:   "turn-1",
		Tool:     "uppercase",
		Arguments: map[string]any{
			"text": "hello",
		},
	})
	if err != nil {
		t.Fatalf("item tool call failed: %v", err)
	}
	if response == nil || !response.Success {
		t.Fatalf("unexpected tool response: %#v", response)
	}
	content := response.ContentItems
	if len(content) != 1 {
		t.Fatalf("unexpected content items: %#v", content)
	}
	item, ok := content[0].(map[string]any)
	if !ok {
		t.Fatalf("unexpected content item type: %#v", content[0])
	}
	if item["text"] != `{"text":"HELLO"}` {
		t.Fatalf("unexpected content text: %#v", item["text"])
	}
}

func TestItemToolCallUsesFrozenToolSnapshot(t *testing.T) {
	t.Parallel()

	runner := &AppServerRunner{
		activeTurns: map[string]appServerActiveTurn{
			appServerActiveTurnKey("thread-1", ""): {
				req: TurnRequest{
					Conversation: ConversationState{Key: "snapshot"},
				},
				tools: []Tool{uppercaseTool{}},
			},
		},
		tools: []Tool{snapshotTool{name: "late"}},
	}

	response, err := runner.ItemToolCall(context.Background(), protocol.DynamicToolCallParams{
		ThreadID: "thread-1",
		Tool:     "uppercase",
		Arguments: map[string]any{
			"text": "hello",
		},
	})
	if err != nil {
		t.Fatalf("item tool call failed: %v", err)
	}
	if response == nil || !response.Success {
		t.Fatalf("unexpected tool response: %#v", response)
	}
}

func appServerMustRaw(t *testing.T, value any) json.RawMessage {
	t.Helper()

	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal raw json failed: %v", err)
	}
	return data
}

func appServerWriteLine(t *testing.T, value any) apprpc.TranscriptEntry {
	t.Helper()

	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal write line failed: %v", err)
	}
	return apprpc.TranscriptEntry{Direction: apprpc.TranscriptWrite, Line: string(data)}
}

func appServerReadLine(t *testing.T, value any) apprpc.TranscriptEntry {
	t.Helper()

	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal read line failed: %v", err)
	}
	return apprpc.TranscriptEntry{Direction: apprpc.TranscriptRead, Line: string(data)}
}
