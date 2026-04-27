package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	appcodex "github.com/pmenglund/codex-sdk-go"
	"github.com/pmenglund/codex-sdk-go/protocol"
	apprpc "github.com/pmenglund/codex-sdk-go/rpc"
)

const testDefaultModel = "gpt-5.4"

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

func newTestAppServerActiveTurn(req TurnRequest, tools []Tool, threadID string, turnID string) *appServerActiveTurn {
	interruptRequested := make(chan struct{})
	active := &appServerActiveTurn{
		req:                req,
		tools:              append([]Tool(nil), tools...),
		threadID:           threadID,
		turnID:             turnID,
		done:               make(chan struct{}),
		interruptRequested: interruptRequested,
		interruptDone:      make(chan struct{}),
	}
	active.requestInterrupt = func() {
		select {
		case <-interruptRequested:
		default:
			close(interruptRequested)
		}
	}
	return active
}

func driveTestAppServerInterrupt(session *appServerSession, active *appServerActiveTurn) {
	go func() {
		<-active.interruptRequested
		session.interruptActiveTurnIfRequested(context.Background(), active.threadID, active.turnID)
	}()
}

func TestLogAppServerProcessExitIncludesSignal(t *testing.T) {
	var output bytes.Buffer
	originalWriter := log.Writer()
	originalFlags := log.Flags()
	log.SetOutput(&output)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(originalWriter)
		log.SetFlags(originalFlags)
	})

	err := exec.CommandContext(context.Background(), "sh", "-c", "kill -TERM $$").Run()
	if err == nil {
		t.Fatal("Run returned nil, want exit error")
	}

	logAppServerProcessExit(fakeExecCmd(43210), err)

	got := output.String()
	if !strings.Contains(got, "pid=43210") {
		t.Fatalf("log output missing pid: %q", got)
	}
	if !strings.Contains(got, "signal=terminated") {
		t.Fatalf("log output missing signal: %q", got)
	}
}

func TestLogAppServerProcessExitIncludesExitCode(t *testing.T) {
	var output bytes.Buffer
	originalWriter := log.Writer()
	originalFlags := log.Flags()
	log.SetOutput(&output)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(originalWriter)
		log.SetFlags(originalFlags)
	})

	err := exec.CommandContext(context.Background(), "sh", "-c", "exit 7").Run()
	if err == nil {
		t.Fatal("Run returned nil, want exit error")
	}

	logAppServerProcessExit(fakeExecCmd(43210), err)

	got := output.String()
	if !strings.Contains(got, "pid=43210") {
		t.Fatalf("log output missing pid: %q", got)
	}
	if !strings.Contains(got, "exit_code=7") {
		t.Fatalf("log output missing exit code: %q", got)
	}
}

func TestIgnoreExpectedAppServerSignalError(t *testing.T) {
	t.Parallel()

	if !ignoreExpectedAppServerSignalError(nil) {
		t.Fatal("nil error should be ignored")
	}
	if !ignoreExpectedAppServerSignalError(syscall.EPERM) {
		t.Fatal("EPERM should be ignored during shutdown")
	}
	if !ignoreExpectedAppServerSignalError(syscall.ESRCH) {
		t.Fatal("ESRCH should be ignored during shutdown")
	}
	if ignoreExpectedAppServerSignalError(syscall.EINVAL) {
		t.Fatal("EINVAL should not be ignored")
	}
}

func fakeExecCmd(pid int) *exec.Cmd {
	return &exec.Cmd{
		Process: &os.Process{
			Pid: pid,
		},
	}
}

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

	inputs := buildAppServerTurnInputs(TurnRequest{
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

	inputs := buildAppServerTurnInputs(TurnRequest{
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

	inputs := buildAppServerTurnInputs(TurnRequest{
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

	inputs := buildAppServerTurnInputs(TurnRequest{
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
	_, err := runner.StartSession(context.Background(), SessionOptions{})
	if err == nil || err.Error() != "start app-server session failed: runner is nil" {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestAppServerSessionRunTurnReturnsErrorOnConversationMismatch(t *testing.T) {
	t.Parallel()

	session := &appServerSession{
		runner:          &AppServerRunner{},
		conversationKey: "conversation-1",
		threadID:        "thread-live",
	}

	_, err := session.RunTurn(context.Background(), TurnRequest{
		Conversation: ConversationState{
			Key:            "conversation-2",
			RunnerThreadID: "thread-live",
		},
		Message: InboundMessage{Kind: MessageKindText, Text: "hello"},
	})
	if err == nil || !strings.Contains(err.Error(), `conversation key mismatch: session="conversation-1" request="conversation-2"`) {
		t.Fatalf("unexpected error: %v", err)
	}

	_, err = session.RunTurn(context.Background(), TurnRequest{
		Conversation: ConversationState{
			Key:            "conversation-1",
			RunnerThreadID: "thread-other",
		},
		Message: InboundMessage{Kind: MessageKindText, Text: "hello"},
	})
	if err == nil || !strings.Contains(err.Error(), `runner thread id mismatch: session="thread-live" request="thread-other"`) {
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

	result, err := runTurnWithRunner(t, runner, TurnRequest{
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

	_, err := runTurnWithRunner(t, runner, TurnRequest{
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

	result, err := runTurnWithRunner(t, runner, TurnRequest{
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

	_, err := runTurnWithRunner(t, runner, TurnRequest{
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
		sessions: make(map[string]*appServerSession),
	}

	_, err := runner.StartSession(context.Background(), SessionOptions{
		ResumeSessionID: "thread-existing",
	})
	if err == nil {
		t.Fatal("StartSession returned nil error")
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
		sessions: make(map[string]*appServerSession),
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

	_, err := runTurnWithRunner(t, runner, TurnRequest{
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

	session := &appServerSession{runner: runner}
	result, err := session.collectStreamedTurn(context.Background(), TurnRequest{
		Conversation: ConversationState{
			Key: "conv-1",
		},
	}, "thread-1", stream)
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
				"model":                 testDefaultModel,
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
				"model":  testDefaultModel,
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
			Model: testDefaultModel,
			Cwd:   cwd,
		},
		TurnOptions: appcodex.TurnOptions{
			Model:  testDefaultModel,
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

	result, err := runTurnWithRunner(t, runner, TurnRequest{
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
		sessions: map[string]*appServerSession{
			"exact": {
				conversationKey: "exact",
				threadID:        "thread-1",
				activeTurn: newTestAppServerActiveTurn(
					TurnRequest{Conversation: ConversationState{Key: "exact"}},
					[]Tool{uppercaseTool{}},
					"thread-1",
					"turn-1",
				),
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
		sessions: map[string]*appServerSession{
			"snapshot": {
				conversationKey: "snapshot",
				threadID:        "thread-1",
				activeTurn: newTestAppServerActiveTurn(
					TurnRequest{Conversation: ConversationState{Key: "snapshot"}},
					[]Tool{uppercaseTool{}},
					"thread-1",
					"",
				),
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

func TestAppServerInterruptUsesTurnInterruptAndWaitsForCompletion(t *testing.T) {
	t.Parallel()

	active := newTestAppServerActiveTurn(TurnRequest{
		Conversation: ConversationState{Key: "conv-1"},
	}, nil, "thread-1", "turn-1")
	var interruptedThreadID string
	var interruptedTurnID string
	interruptCalled := make(chan struct{})
	runner := &AppServerRunner{
		interruptTurnFn: func(_ context.Context, threadID string, turnID string) error {
			interruptedThreadID = threadID
			interruptedTurnID = turnID
			close(interruptCalled)
			return nil
		},
	}
	session := &appServerSession{
		runner:          runner,
		conversationKey: "conv-1",
		threadID:        "thread-1",
		activeTurn:      active,
	}
	driveTestAppServerInterrupt(session, active)

	done := make(chan error, 1)
	go func() {
		done <- session.Interrupt(context.Background())
	}()

	select {
	case err := <-done:
		t.Fatalf("interrupt returned before active turn completed: %v", err)
	default:
	}

	select {
	case <-interruptCalled:
	case <-time.After(time.Second):
		t.Fatal("interrupt handler was not called")
	}
	if interruptedThreadID != "thread-1" || interruptedTurnID != "turn-1" {
		t.Fatalf("unexpected interrupted ids: thread=%s turn=%s", interruptedThreadID, interruptedTurnID)
	}

	close(active.done)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("interrupt failed: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("interrupt did not wait for active turn completion")
	}
}

func TestInterruptedAppServerTurnDoesNotReturnSuccessfulResult(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	release := make(chan struct{})
	runner := &AppServerRunner{}
	session := &appServerSession{
		runner:          runner,
		conversationKey: "conv-interrupt-success",
		threadID:        "thread-1",
		thread:          &fakeAppServerThread{id: "thread-1"},
	}
	runner.runThreadTurnFn = func(_ context.Context, _ TurnRequest, _ appServerThread, _ []appcodex.Input, _ *appcodex.TurnOptions) (*appcodex.TurnResult, error) {
		close(started)
		active, ok := session.activeTurnSnapshot()
		if !ok {
			t.Fatal("expected active turn")
		}
		<-active.interruptRequested
		<-release
		return &appcodex.TurnResult{FinalResponse: "partial reply"}, nil
	}
	runner.interruptTurnFn = func(_ context.Context, threadID string, turnID string) error {
		return nil
	}

	runErrCh := make(chan error, 1)
	go func() {
		_, err := session.RunTurn(context.Background(), TurnRequest{
			Conversation: ConversationState{Key: "conv-interrupt-success"},
			Message:      InboundMessage{Kind: MessageKindText, Text: "hello"},
		})
		runErrCh <- err
	}()

	<-started

	interruptDone := make(chan error, 1)
	go func() {
		interruptDone <- session.Interrupt(context.Background())
	}()

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
		if err == nil || err.Error() != "run app-server turn failed: context canceled" {
			t.Fatalf("unexpected run turn error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("run turn did not finish")
	}
}

func TestInterruptedAppServerTurnReturnsCanceledAfterActiveTurnClears(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	release := make(chan struct{})
	runner := &AppServerRunner{}
	session := &appServerSession{
		runner:          runner,
		conversationKey: "conv-interrupt-cleared",
		threadID:        "thread-1",
		thread:          &fakeAppServerThread{id: "thread-1"},
	}
	runner.runThreadTurnFn = func(_ context.Context, _ TurnRequest, _ appServerThread, _ []appcodex.Input, _ *appcodex.TurnOptions) (*appcodex.TurnResult, error) {
		close(started)
		active, ok := session.activeTurnSnapshot()
		if !ok {
			t.Fatal("expected active turn")
		}
		<-active.interruptRequested
		session.endTurn("thread-1", "")
		<-release
		return &appcodex.TurnResult{FinalResponse: "partial reply"}, nil
	}

	runErrCh := make(chan error, 1)
	go func() {
		_, err := session.RunTurn(context.Background(), TurnRequest{
			Conversation: ConversationState{Key: "conv-interrupt-cleared"},
			Message:      InboundMessage{Kind: MessageKindText, Text: "hello"},
		})
		runErrCh <- err
	}()

	<-started

	interruptDone := make(chan error, 1)
	go func() {
		interruptDone <- session.Interrupt(context.Background())
	}()

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
		if err == nil || err.Error() != "run app-server turn failed: context canceled" {
			t.Fatalf("unexpected run turn error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("run turn did not finish")
	}
}

func TestAppServerInterruptIsIdempotentForConcurrentCallers(t *testing.T) {
	t.Parallel()

	active := newTestAppServerActiveTurn(TurnRequest{
		Conversation: ConversationState{Key: "conv-1"},
	}, nil, "thread-1", "turn-1")
	interruptCalls := 0
	interruptCalled := make(chan struct{}, 1)
	runner := &AppServerRunner{
		interruptTurnFn: func(_ context.Context, threadID string, turnID string) error {
			interruptCalls++
			if threadID != "thread-1" || turnID != "turn-1" {
				t.Fatalf("unexpected interrupted ids: thread=%s turn=%s", threadID, turnID)
			}
			select {
			case interruptCalled <- struct{}{}:
			default:
			}
			return nil
		},
	}
	session := &appServerSession{
		runner:          runner,
		conversationKey: "conv-1",
		threadID:        "thread-1",
		activeTurn:      active,
	}
	driveTestAppServerInterrupt(session, active)

	doneA := make(chan error, 1)
	doneB := make(chan error, 1)
	go func() {
		doneA <- session.Interrupt(context.Background())
	}()
	go func() {
		doneB <- session.Interrupt(context.Background())
	}()

	select {
	case <-interruptCalled:
	case <-time.After(time.Second):
		t.Fatal("interrupt handler was not called")
	}

	if interruptCalls != 1 {
		t.Fatalf("expected exactly one interrupt call, got %d", interruptCalls)
	}

	close(active.done)

	select {
	case err := <-doneA:
		if err != nil {
			t.Fatalf("first interrupt failed: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("first interrupt did not return")
	}

	select {
	case err := <-doneB:
		if err != nil {
			t.Fatalf("second interrupt failed: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("second interrupt did not return")
	}
}

func TestAppServerInterruptReturnsNilWithoutActiveTurn(t *testing.T) {
	t.Parallel()

	session := &appServerSession{
		runner:          &AppServerRunner{},
		conversationKey: "conv-idle",
	}

	err := session.Interrupt(context.Background())
	if err != nil {
		t.Fatalf("expected nil interrupt on idle session, got %v", err)
	}
}

func TestAppServerInterruptReturnsUnavailableWhenInterruptFunctionMissing(t *testing.T) {
	t.Parallel()

	runner := &AppServerRunner{}
	session := &appServerSession{
		runner:          runner,
		conversationKey: "conv-1",
		threadID:        "thread-1",
		activeTurn: newTestAppServerActiveTurn(TurnRequest{
			Conversation: ConversationState{Key: "conv-1"},
		}, nil, "thread-1", "turn-1"),
	}
	driveTestAppServerInterrupt(session, session.activeTurn)

	err := session.Interrupt(context.Background())
	if !errors.Is(err, ErrSessionInterruptUnavailable) {
		t.Fatalf("expected ErrSessionInterruptUnavailable, got %v", err)
	}
}

func TestAppServerSessionCloseInterruptsActiveTurnBeforeReturning(t *testing.T) {
	t.Parallel()

	active := newTestAppServerActiveTurn(TurnRequest{
		Conversation: ConversationState{Key: "conv-close"},
	}, nil, "thread-close", "turn-close")

	interruptCalled := make(chan struct{})
	unsubscribeCalled := make(chan struct{})
	var unsubscribedThreadID string
	closeCalled := make(chan struct{})
	runner := &AppServerRunner{
		interruptTurnFn: func(_ context.Context, threadID string, turnID string) error {
			if threadID != "thread-close" || turnID != "turn-close" {
				t.Fatalf("unexpected interrupt ids: thread=%s turn=%s", threadID, turnID)
			}
			close(interruptCalled)
			return nil
		},
		unsubscribeThreadFn: func(_ context.Context, threadID string) error {
			unsubscribedThreadID = threadID
			close(unsubscribeCalled)
			return nil
		},
		closeFn: func() error {
			close(closeCalled)
			return nil
		},
	}
	session := &appServerSession{
		runner:          runner,
		conversationKey: "conv-close",
		threadID:        "thread-close",
		activeTurn:      active,
	}
	driveTestAppServerInterrupt(session, active)

	done := make(chan error, 1)
	go func() {
		done <- session.Close()
	}()

	select {
	case <-interruptCalled:
	case <-time.After(time.Second):
		t.Fatal("close did not interrupt active turn")
	}

	select {
	case err := <-done:
		t.Fatalf("close returned before active turn completed: %v", err)
	case <-time.After(20 * time.Millisecond):
	}

	select {
	case <-unsubscribeCalled:
		t.Fatal("close unsubscribed thread before active turn completed")
	default:
	}

	select {
	case <-closeCalled:
		t.Fatal("runner close handler should not be called by session close")
	default:
	}

	close(active.done)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("close failed: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("close did not wait for active turn completion")
	}

	select {
	case <-unsubscribeCalled:
	default:
		t.Fatal("close did not unsubscribe thread")
	}
	if unsubscribedThreadID != "thread-close" {
		t.Fatalf("unexpected unsubscribed thread id: %s", unsubscribedThreadID)
	}

	select {
	case <-closeCalled:
		t.Fatal("runner close handler should not be called by session close")
	default:
	}
}

func TestAppServerSessionCloseUnsubscribesIdleThread(t *testing.T) {
	t.Parallel()

	unsubscribeCalled := make(chan struct{}, 1)
	var unsubscribedThreadID string
	runner := &AppServerRunner{
		unsubscribeThreadFn: func(_ context.Context, threadID string) error {
			unsubscribedThreadID = threadID
			unsubscribeCalled <- struct{}{}
			return nil
		},
	}
	session := &appServerSession{
		runner:          runner,
		conversationKey: "conv-idle-close",
		threadID:        "thread-idle",
	}

	if err := session.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	select {
	case <-unsubscribeCalled:
	default:
		t.Fatal("close did not unsubscribe idle thread")
	}
	if unsubscribedThreadID != "thread-idle" {
		t.Fatalf("unexpected unsubscribed thread id: %s", unsubscribedThreadID)
	}
}

func TestAppServerSessionEndTurnIgnoresMismatchedTurnID(t *testing.T) {
	t.Parallel()

	active := newTestAppServerActiveTurn(TurnRequest{
		Conversation: ConversationState{Key: "conv-mismatch"},
	}, nil, "thread-mismatch", "turn-live")
	runner := &AppServerRunner{
		sessions: map[string]*appServerSession{
			"conv-mismatch": {
				conversationKey: "conv-mismatch",
				threadID:        "thread-mismatch",
				activeTurn:      active,
			},
		},
	}
	session := runner.sessions["conv-mismatch"]

	session.endTurn("thread-mismatch", "turn-other")

	select {
	case <-active.done:
		t.Fatal("expected mismatched turn clear to keep done open")
	default:
	}

	if session == nil || session.activeTurn != active {
		t.Fatal("expected mismatched turn clear to preserve the active turn")
	}

	session.endTurn("thread-mismatch", "turn-live")

	select {
	case <-active.done:
	case <-time.After(time.Second):
		t.Fatal("expected matched turn clear to close done")
	}

	if session == nil || session.activeTurn != nil {
		t.Fatal("expected matched turn clear to remove the active turn")
	}
}

func TestClosedAppServerSessionRejectsRunTurn(t *testing.T) {
	t.Parallel()

	session := &appServerSession{
		runner: &AppServerRunner{},
	}

	if err := session.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	_, err := session.RunTurn(context.Background(), TurnRequest{})
	if err == nil || err.Error() != "run app-server turn failed: session is closed" {
		t.Fatalf("unexpected run turn error: %v", err)
	}
}

func TestAppServerSessionRejectsConcurrentRunTurn(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	release := make(chan struct{})
	session := &appServerSession{
		runner:          &AppServerRunner{},
		conversationKey: "conv-busy",
		threadID:        "thread-busy",
		thread:          &fakeAppServerThread{id: "thread-busy"},
	}
	session.runner.runThreadTurnFn = func(context.Context, TurnRequest, appServerThread, []appcodex.Input, *appcodex.TurnOptions) (*appcodex.TurnResult, error) {
		close(started)
		<-release
		return &appcodex.TurnResult{FinalResponse: "done"}, nil
	}

	runErrCh := make(chan error, 1)
	go func() {
		_, err := session.RunTurn(context.Background(), TurnRequest{
			Conversation: ConversationState{Key: "conv-busy"},
			Message:      InboundMessage{Kind: MessageKindText, Text: "hello"},
		})
		runErrCh <- err
	}()

	<-started

	_, err := session.RunTurn(context.Background(), TurnRequest{
		Conversation: ConversationState{Key: "conv-busy"},
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
