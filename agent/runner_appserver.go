package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"maps"
	"os"
	"runtime/debug"
	"strings"
	"sync"

	appcodex "github.com/pmenglund/codex-sdk-go"
	appproto "github.com/pmenglund/codex-sdk-go/protocol"
	apprpc "github.com/pmenglund/codex-sdk-go/rpc"
)

// AppServerRunner bridges dispatcher turns to the Codex app-server through pmenglund/codex-sdk-go.
type AppServerRunner struct {
	closeFn           func() error
	rpcClient         *apprpc.Client
	startThread       func(context.Context, appcodex.ThreadStartOptions) (appServerThread, error)
	resumeThread      func(context.Context, appcodex.ThreadResumeOptions) (appServerThread, error)
	runThreadTurnFn   func(context.Context, TurnRequest, appServerThread, []appcodex.Input, *appcodex.TurnOptions) (*appcodex.TurnResult, error)
	startOptions      appcodex.ThreadStartOptions
	resumeOptions     appcodex.ThreadResumeOptions
	turnOptions       appcodex.TurnOptions
	nativeToolCalls   bool
	maxToolIterations int
	mu                sync.RWMutex
	systemPrompts     []string
	tools             []Tool
	activeTurns       map[string]TurnRequest
}

// AppServerRunnerOptions configures an AppServerRunner.
type AppServerRunnerOptions struct {
	Client            *appcodex.Codex
	CodexOptions      appcodex.Options
	StartOptions      appcodex.ThreadStartOptions
	ResumeOptions     appcodex.ThreadResumeOptions
	TurnOptions       appcodex.TurnOptions
	SystemPrompt      string
	Tools             []Tool
	MaxToolIterations int
}

type appServerThread interface {
	ID() string
	RunStreamed(ctx context.Context, inputs []appcodex.Input, opts *appcodex.TurnOptions) (appServerTurnStream, error)
}

type appServerTurnStream interface {
	Next(ctx context.Context) (apprpc.Notification, error)
	Close()
}

// SandboxPolicy is the app-server sandbox policy payload used by this project.
type SandboxPolicy map[string]any

var (
	SandboxPolicyReadOnly         = SandboxPolicy{"type": "readOnly"}
	SandboxPolicyWorkspaceWrite   = SandboxPolicy{"type": "workspaceWrite"}
	SandboxPolicyDangerFullAccess = SandboxPolicy{"type": "dangerFullAccess"}
)

const (
	appServerConfigWebSearchKey      = "web_search"
	appServerSandboxNetworkAccessKey = "networkAccess"
)

func (p SandboxPolicy) String() string {
	switch p["type"] {
	case "readOnly":
		return "read-only"
	case "workspaceWrite":
		return "workspace-write"
	case "dangerFullAccess":
		return "danger-full-access"
	default:
		return ""
	}
}

// NewAppServerRunner builds a runner backed by the Codex app-server.
func NewAppServerRunner(ctx context.Context, options AppServerRunnerOptions) (*AppServerRunner, error) {
	startOptions := options.StartOptions
	resumeOptions := options.ResumeOptions
	turnOptions := options.TurnOptions

	if startOptions.Cwd == "" || resumeOptions.Cwd == "" || turnOptions.Cwd == "" {
		workingDirectory, err := os.Getwd()
		if err == nil {
			if startOptions.Cwd == "" {
				startOptions.Cwd = workingDirectory
			}
			if resumeOptions.Cwd == "" {
				resumeOptions.Cwd = workingDirectory
			}
			if turnOptions.Cwd == "" {
				turnOptions.Cwd = workingDirectory
			}
		}
	}

	if startOptions.Model == "" {
		startOptions.Model = defaultModel
	}
	if resumeOptions.Model == "" {
		resumeOptions.Model = startOptions.Model
	}
	if turnOptions.Model == "" {
		turnOptions.Model = startOptions.Model
	}

	if startOptions.ApprovalPolicy == nil {
		startOptions.ApprovalPolicy = appcodex.ApprovalPolicyNever
	}
	if resumeOptions.ApprovalPolicy == nil {
		resumeOptions.ApprovalPolicy = appcodex.ApprovalPolicyNever
	}
	if turnOptions.ApprovalPolicy == nil {
		turnOptions.ApprovalPolicy = appcodex.ApprovalPolicyNever
	}

	startOptions.Config = defaultAppServerConfig(startOptions.Config)
	resumeOptions.Config = defaultAppServerConfig(resumeOptions.Config)

	if startOptions.SandboxPolicy == nil {
		startOptions.SandboxPolicy = defaultAppServerSandboxPolicy()
	}
	if resumeOptions.Sandbox == nil {
		resumeOptions.Sandbox = defaultAppServerSandboxPolicy()
	}
	if turnOptions.SandboxPolicy == nil {
		turnOptions.SandboxPolicy = defaultAppServerSandboxPolicy()
	}
	startOptions.SandboxPolicy = normalizeAppServerSandboxPolicy(startOptions.SandboxPolicy)
	resumeOptions.Sandbox = normalizeAppServerSandboxPolicy(resumeOptions.Sandbox)
	turnOptions.SandboxPolicy = normalizeAppServerSandboxPolicy(turnOptions.SandboxPolicy)
	if sandboxPolicy, ok := startOptions.SandboxPolicy.(SandboxPolicy); ok {
		startOptions.SandboxPolicy = sandboxPolicy.String()
	}
	if sandboxPolicy, ok := resumeOptions.Sandbox.(SandboxPolicy); ok {
		resumeOptions.Sandbox = sandboxPolicy.String()
	}
	turnOptions.SandboxPolicy = applyWorkspaceWriteNetworkAccess(turnOptions.SandboxPolicy)

	if turnOptions.Effort == nil || turnOptions.Effort == appcodex.ReasoningEffortMinimal {
		// `gpt-5.4` doesn't support `minimal`.
		turnOptions.Effort = appcodex.ReasoningEffortMedium
	}

	maxToolIterations := options.MaxToolIterations
	if maxToolIterations <= 0 {
		maxToolIterations = defaultMaxToolIterations
	}

	runner := &AppServerRunner{
		startOptions:      startOptions,
		resumeOptions:     resumeOptions,
		turnOptions:       turnOptions,
		maxToolIterations: maxToolIterations,
		activeTurns:       make(map[string]TurnRequest),
	}

	rpcClient, closeFn, nativeToolCalls, err := newAppServerRPCClient(ctx, options.CodexOptions, options.Client, runner)
	if err != nil {
		return nil, fmt.Errorf("create codex app-server client failed: %w", err)
	}
	runner.rpcClient = rpcClient
	runner.closeFn = closeFn
	runner.nativeToolCalls = nativeToolCalls
	if runner.rpcClient != nil {
		runner.startThread = func(ctx context.Context, options appcodex.ThreadStartOptions) (appServerThread, error) {
			return runner.startRPCThread(ctx, options)
		}
		runner.resumeThread = func(ctx context.Context, options appcodex.ThreadResumeOptions) (appServerThread, error) {
			return runner.resumeRPCThread(ctx, options)
		}
	}

	runner.RegisterSystemPrompt(options.SystemPrompt)
	runner.RegisterTools(options.Tools...)

	return runner, nil
}

func defaultAppServerSandboxPolicy() SandboxPolicy {
	return SandboxPolicyReadOnly
}

func defaultAppServerConfig(config map[string]any) map[string]any {
	if config == nil {
		config = make(map[string]any, 1)
	} else {
		cloned := make(map[string]any, len(config)+1)
		maps.Copy(cloned, config)
		config = cloned
	}

	if _, ok := config[appServerConfigWebSearchKey]; !ok {
		config[appServerConfigWebSearchKey] = "live"
	}

	return config
}

func normalizeAppServerSandboxPolicy(policy any) any {
	switch value := policy.(type) {
	case nil:
		return nil
	case SandboxPolicy:
		return value
	case map[string]any:
		return SandboxPolicy(value)
	case appcodex.SandboxMode:
		return normalizeAppServerSandboxMode(string(value))
	case string:
		return normalizeAppServerSandboxMode(value)
	default:
		return policy
	}
}

func normalizeAppServerSandboxMode(mode string) any {
	switch mode {
	case string(appcodex.SandboxModeReadOnly):
		return SandboxPolicyReadOnly
	case string(appcodex.SandboxModeWorkspaceWrite):
		return SandboxPolicyWorkspaceWrite
	case string(appcodex.SandboxModeDangerFullAccess):
		return SandboxPolicyDangerFullAccess
	default:
		return mode
	}
}

func applyWorkspaceWriteNetworkAccess(policy any) any {
	sandboxPolicy, ok := policy.(SandboxPolicy)
	if !ok {
		return policy
	}
	if sandboxPolicy["type"] != "workspaceWrite" {
		return sandboxPolicy
	}

	cloned := make(SandboxPolicy, len(sandboxPolicy)+1)
	maps.Copy(cloned, sandboxPolicy)
	if _, ok = cloned[appServerSandboxNetworkAccessKey]; ok {
		return cloned
	}
	cloned[appServerSandboxNetworkAccessKey] = true
	return cloned
}

func describeAppServerSandboxPolicy(policy any) string {
	switch value := policy.(type) {
	case nil:
		return ""
	case SandboxPolicy:
		return value.String()
	case fmt.Stringer:
		return value.String()
	case string:
		return value
	default:
		return fmt.Sprintf("%v", value)
	}
}

func describeAppServerApprovalPolicy(policy any) string {
	switch value := policy.(type) {
	case nil:
		return ""
	case fmt.Stringer:
		return value.String()
	case string:
		return value
	default:
		return fmt.Sprintf("%v", value)
	}
}

// Close shuts down the underlying app-server client.
func (r *AppServerRunner) Close() error {
	if r == nil || r.closeFn == nil {
		return nil
	}

	return r.closeFn()
}

// RegisterSystemPrompt appends one global system prompt block for new conversations.
func (r *AppServerRunner) RegisterSystemPrompt(prompt string) {
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
func (r *AppServerRunner) RegisterTools(tools ...Tool) {
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

func (r *AppServerRunner) globalContext() ([]string, []Tool) {
	if r == nil {
		return nil, nil
	}

	r.mu.RLock()
	defer r.mu.RUnlock()

	return append([]string(nil), r.systemPrompts...), append([]Tool(nil), r.tools...)
}

// RunTurn runs one Codex turn and returns the updated thread mapping and final reply text.
func (r *AppServerRunner) RunTurn(ctx context.Context, req TurnRequest) (TurnResult, error) {
	if r == nil {
		return TurnResult{}, errors.New("run app-server turn failed: runner is nil")
	}

	var (
		thread       appServerThread
		err          error
		threadAction = "start"
	)
	if req.Conversation.CodexThreadID != "" {
		threadAction = "resume"
		options := r.resumeOptions
		options.ThreadID = req.Conversation.CodexThreadID
		thread, err = r.resumeThread(ctx, options)
		if err != nil {
			log.Printf(
				"app-server runner resume thread failed: conversation=%s thread_id=%s model=%s cwd=%s sandbox=%s approval=%s err=%v",
				req.Conversation.Key,
				options.ThreadID,
				options.Model,
				options.Cwd,
				describeAppServerSandboxPolicy(options.Sandbox),
				describeAppServerApprovalPolicy(options.ApprovalPolicy),
				err,
			)
		}
	} else {
		thread, err = r.startThread(ctx, r.startOptions)
		if err != nil {
			log.Printf(
				"app-server runner start thread failed: conversation=%s model=%s cwd=%s sandbox=%s approval=%s err=%v",
				req.Conversation.Key,
				r.startOptions.Model,
				r.startOptions.Cwd,
				describeAppServerSandboxPolicy(r.startOptions.SandboxPolicy),
				describeAppServerApprovalPolicy(r.startOptions.ApprovalPolicy),
				err,
			)
		}
	}
	if err != nil {
		return TurnResult{}, fmt.Errorf("run app-server turn failed: %w", err)
	}
	if thread == nil {
		return TurnResult{}, errors.New("run app-server turn failed: thread is nil")
	}
	log.Printf(
		"app-server runner thread ready: conversation=%s action=%s requested_thread=%s actual_thread=%s",
		req.Conversation.Key,
		threadAction,
		req.Conversation.CodexThreadID,
		thread.ID(),
	)

	inputs, err := r.buildTurnInputs(req)
	if err != nil {
		return TurnResult{}, fmt.Errorf("run app-server turn failed: %w", err)
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
	if len(tools) == 0 || r.nativeToolCalls {
		log.Printf("app-server runner executing turn: conversation=%s mode=direct", req.Conversation.Key)
		r.setActiveTurn(thread.ID(), req)
		defer r.clearActiveTurn(thread.ID())
		turn, runErr := r.runThreadTurn(ctx, req, thread, inputs, &r.turnOptions)
		if runErr != nil {
			return TurnResult{}, fmt.Errorf("run app-server turn failed: %w", runErr)
		}
		replyText = turn.FinalResponse
	} else {
		log.Printf("app-server runner executing turn: conversation=%s mode=tool_loop tool_count=%d", req.Conversation.Key, len(tools))
		replyText, err = r.runToolLoop(ctx, req, thread, inputs)
		if err != nil {
			return TurnResult{}, fmt.Errorf("run app-server turn failed: %w", err)
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

func (r *AppServerRunner) runThreadTurn(ctx context.Context, req TurnRequest, thread appServerThread, inputs []appcodex.Input, options *appcodex.TurnOptions) (*appcodex.TurnResult, error) {
	if r.runThreadTurnFn != nil {
		turn, err := r.runThreadTurnFn(ctx, req, thread, inputs, options)
		if err != nil {
			log.Printf(
				"app-server runner turn failed: conversation=%s thread_id=%s model=%s cwd=%s sandbox=%s approval=%s input_count=%d err=%v",
				req.Conversation.Key,
				thread.ID(),
				options.Model,
				options.Cwd,
				describeAppServerSandboxPolicy(options.SandboxPolicy),
				describeAppServerApprovalPolicy(options.ApprovalPolicy),
				len(inputs),
				err,
			)
		}
		return turn, err
	}

	log.Printf("app-server runner started streamed turn: conversation=%s thread_id=%s", req.Conversation.Key, thread.ID())

	stream, err := thread.RunStreamed(ctx, inputs, options)
	if err != nil {
		if options != nil {
			log.Printf(
				"app-server runner start streamed turn failed: conversation=%s thread_id=%s model=%s cwd=%s sandbox=%s approval=%s input_count=%d err=%v",
				req.Conversation.Key,
				thread.ID(),
				options.Model,
				options.Cwd,
				describeAppServerSandboxPolicy(options.SandboxPolicy),
				describeAppServerApprovalPolicy(options.ApprovalPolicy),
				len(inputs),
				err,
			)
		} else {
			log.Printf(
				"app-server runner start streamed turn failed: conversation=%s thread_id=%s input_count=%d err=%v",
				req.Conversation.Key,
				thread.ID(),
				len(inputs),
				err,
			)
		}
		return nil, err
	}
	defer stream.Close()

	return r.collectStreamedTurn(ctx, req, stream)
}

func (r *AppServerRunner) collectStreamedTurn(ctx context.Context, req TurnRequest, stream appServerTurnStream) (*appcodex.TurnResult, error) {
	if stream == nil {
		return nil, errors.New("streamed turn is nil")
	}

	result := &appcodex.TurnResult{}
	for {
		note, err := stream.Next(ctx)
		if err != nil {
			log.Printf(
				"app-server runner streamed turn read failed: conversation=%s err=%v",
				req.Conversation.Key,
				err,
			)
			return nil, err
		}
		result.Notifications = append(result.Notifications, note)

		switch note.Method {
		case "item/completed":
			item, text := parseAppServerItem(note)
			if len(item) != 0 {
				result.Items = append(result.Items, item)
			}
			if text != "" {
				result.FinalResponse = text
			}
		case "turn/started":
			result.TurnID = parseAppServerTurnID(note)
		case "turn/completed":
			result.TurnID = parseAppServerTurnID(note)
			if turnErr := parseAppServerTurnError(note); turnErr != nil {
				log.Printf(
					"app-server runner completed turn with error: conversation=%s turn_id=%s err=%v",
					req.Conversation.Key,
					result.TurnID,
					turnErr,
				)
				return nil, turnErr
			}
			log.Printf(
				"app-server runner completed streamed turn: conversation=%s items=%d final_response_len=%d",
				req.Conversation.Key,
				len(result.Items),
				len(result.FinalResponse),
			)
			return result, nil
		case "turn/failed", "error":
			if turnErr := parseAppServerTurnError(note); turnErr != nil {
				log.Printf(
					"app-server runner turn notification failed: conversation=%s turn_id=%s method=%s err=%v",
					req.Conversation.Key,
					result.TurnID,
					note.Method,
					turnErr,
				)
				return nil, turnErr
			}
			log.Printf(
				"app-server runner turn notification failed without detail: conversation=%s turn_id=%s method=%s",
				req.Conversation.Key,
				result.TurnID,
				note.Method,
			)
			return nil, errors.New("app-server turn failed")
		}
	}
}

func (r *AppServerRunner) runToolLoop(ctx context.Context, req TurnRequest, thread appServerThread, inputs []appcodex.Input) (string, error) {
	currentInputs := append([]appcodex.Input(nil), inputs...)
	toolCtx := ContextWithTurnRequest(ctx, req)

	for iteration := 0; iteration < r.maxToolIterations; iteration++ {
		log.Printf(
			"app-server runner tool loop iteration: conversation=%s thread_id=%s iteration=%d",
			req.Conversation.Key,
			thread.ID(),
			iteration+1,
		)

		turnOptions := r.turnOptions
		turnOptions.OutputSchema = toolLoopResponseSchema()

		turn, runErr := r.runThreadTurn(ctx, req, thread, currentInputs, &turnOptions)
		if runErr != nil {
			return "", runErr
		}

		decision, parseErr := parseToolLoopResponse(turn.FinalResponse)
		if parseErr != nil {
			return "", parseErr
		}
		log.Printf(
			"app-server runner tool loop decision: conversation=%s iteration=%d action=%s tool=%s message_len=%d",
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
				"app-server runner calling tool: conversation=%s iteration=%d tool=%s input_bytes=%d",
				req.Conversation.Key,
				iteration+1,
				tool.Name(),
				len(decision.ToolInput),
			)
			result, callErr := tool.Call(toolCtx, decision.ToolInput)
			if callErr != nil {
				log.Printf(
					"app-server runner tool failed: conversation=%s iteration=%d tool=%s err=%v",
					req.Conversation.Key,
					iteration+1,
					tool.Name(),
					callErr,
				)
			} else {
				log.Printf(
					"app-server runner tool completed: conversation=%s iteration=%d tool=%s",
					req.Conversation.Key,
					iteration+1,
					tool.Name(),
				)
			}

			nextInput, runErr := buildToolResultInputForAppServer(tool.Name(), decision.ToolInput, result, callErr)
			if runErr != nil {
				return "", runErr
			}
			currentInputs = []appcodex.Input{nextInput}
		case "silent":
			return "", nil
		default:
			return "", fmt.Errorf("tool loop returned unsupported action %q", decision.Action)
		}
	}

	return "", fmt.Errorf("tool loop exceeded %d iterations", r.maxToolIterations)
}

func (r *AppServerRunner) findTool(name string) (Tool, bool) {
	_, tools := r.globalContext()
	for _, tool := range tools {
		if tool.Name() == name {
			return tool, true
		}
	}

	return nil, false
}

func (r *AppServerRunner) buildTurnInputs(req TurnRequest) ([]appcodex.Input, error) {
	prompt, imagePaths := buildTurnPrompt(req.Message)
	inputs := make([]appcodex.Input, 0, 1+len(imagePaths))

	if prompt != "" {
		inputs = append(inputs, appcodex.TextInput(prompt))
	}
	for _, imagePath := range imagePaths {
		inputs = append(inputs, appcodex.LocalImageInput(imagePath))
	}
	if len(inputs) == 0 {
		inputs = append(inputs, appcodex.TextInput(""))
	}

	return r.injectInitialTurnContext(req, inputs)
}

func (r *AppServerRunner) injectInitialTurnContext(req TurnRequest, inputs []appcodex.Input) ([]appcodex.Input, error) {
	if req.Conversation.CodexThreadID != "" {
		return inputs, nil
	}

	prompts, tools := r.globalContext()
	if r.nativeToolCalls {
		tools = nil
	}
	return injectInitialPromptIntoInputs(inputs, prompts, tools)
}

func (r *AppServerRunner) setActiveTurn(threadID string, req TurnRequest) {
	if r == nil || threadID == "" {
		return
	}

	r.mu.Lock()
	if r.activeTurns == nil {
		r.activeTurns = make(map[string]TurnRequest)
	}
	r.activeTurns[threadID] = req
	r.mu.Unlock()
}

func (r *AppServerRunner) clearActiveTurn(threadID string) {
	if r == nil || threadID == "" {
		return
	}

	r.mu.Lock()
	delete(r.activeTurns, threadID)
	r.mu.Unlock()
}

func (r *AppServerRunner) activeTurn(threadID string) (TurnRequest, bool) {
	if r == nil || threadID == "" {
		return TurnRequest{}, false
	}

	r.mu.RLock()
	req, ok := r.activeTurns[threadID]
	r.mu.RUnlock()
	return req, ok
}

func injectInitialPromptIntoInputs(inputs []appcodex.Input, prompts []string, tools []Tool) ([]appcodex.Input, error) {
	instruction, err := buildInitialInstruction(prompts, tools)
	if err != nil {
		return nil, err
	}
	if instruction == "" {
		return inputs, nil
	}

	cloned := append([]appcodex.Input(nil), inputs...)
	for index := range cloned {
		if cloned[index].Type == appcodex.InputTypeText {
			cloned[index].Text = joinPromptBlocks(instruction, cloned[index].Text)
			return cloned, nil
		}
	}

	return append([]appcodex.Input{appcodex.TextInput(instruction)}, cloned...), nil
}

func buildToolResultInputForAppServer(toolName string, toolInput json.RawMessage, result any, err error) (appcodex.Input, error) {
	input, buildErr := buildToolResultInput(toolName, toolInput, result, err)
	if buildErr != nil {
		return appcodex.Input{}, buildErr
	}

	return appcodex.TextInput(input.Text), nil
}

type appServerTurnNotification struct {
	ThreadID string              `json:"threadId,omitempty"`
	Turn     *appServerTurnState `json:"turn,omitempty"`
	Item     json.RawMessage     `json:"item,omitempty"`
	Error    *appServerTurnError `json:"error,omitempty"`
}

type appServerTurnState struct {
	ID     string              `json:"id,omitempty"`
	Status string              `json:"status,omitempty"`
	Error  *appServerTurnError `json:"error,omitempty"`
}

type appServerTurnError struct {
	Message string `json:"message,omitempty"`
}

func parseAppServerTurnID(note apprpc.Notification) string {
	payload, err := parseAppServerTurnNotification(note)
	if err != nil || payload.Turn == nil {
		return ""
	}

	return payload.Turn.ID
}

func parseAppServerTurnError(note apprpc.Notification) error {
	payload, err := parseAppServerTurnNotification(note)
	if err != nil {
		return err
	}
	if payload.Turn != nil && payload.Turn.Error != nil && payload.Turn.Error.Message != "" {
		return errors.New(payload.Turn.Error.Message)
	}
	if payload.Error != nil && payload.Error.Message != "" {
		return errors.New(payload.Error.Message)
	}
	if payload.Turn != nil && payload.Turn.Status == "failed" {
		return errors.New("turn failed")
	}

	return nil
}

func parseAppServerItem(note apprpc.Notification) (json.RawMessage, string) {
	payload, err := parseAppServerTurnNotification(note)
	if err != nil || len(payload.Item) == 0 {
		return nil, ""
	}

	text, _ := extractAppServerTextFromItem(payload.Item)
	return payload.Item, text
}

func parseAppServerTurnNotification(note apprpc.Notification) (appServerTurnNotification, error) {
	var payload appServerTurnNotification
	if len(note.Raw) == 0 {
		return payload, nil
	}
	if err := note.UnmarshalParams(&payload); err != nil {
		return payload, err
	}

	return payload, nil
}

func extractAppServerTextFromItem(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 {
		return "", false
	}

	var direct struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &direct); err == nil && direct.Text != "" {
		return direct.Text, true
	}

	var wrapper map[string]json.RawMessage
	if err := json.Unmarshal(raw, &wrapper); err != nil || len(wrapper) != 1 {
		return "", false
	}
	for _, inner := range wrapper {
		var nested struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(inner, &nested); err == nil && nested.Text != "" {
			return nested.Text, true
		}
	}

	return "", false
}

func stringPtr(value string) *string {
	return &value
}

func marshalAppServerJSONValue(field string, value any) (json.RawMessage, error) {
	if value == nil {
		return nil, nil
	}
	raw, ok := value.(json.RawMessage)
	if ok {
		if len(raw) == 0 {
			return nil, nil
		}
		var decoded any
		if err := json.Unmarshal(raw, &decoded); err != nil {
			return nil, fmt.Errorf("%s must be valid JSON: %w", field, err)
		}
		return raw, nil
	}

	data, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode %s failed: %w", field, err)
	}
	return data, nil
}

type appServerThreadStartParams struct {
	Model                 *string                `json:"model,omitempty"`
	Cwd                   *string                `json:"cwd,omitempty"`
	ApprovalPolicy        json.RawMessage        `json:"approvalPolicy,omitempty"`
	Sandbox               json.RawMessage        `json:"sandbox,omitempty"`
	Config                *map[string]any        `json:"config,omitempty"`
	BaseInstructions      *string                `json:"baseInstructions,omitempty"`
	DeveloperInstructions *string                `json:"developerInstructions,omitempty"`
	DynamicTools          []appServerDynamicTool `json:"dynamicTools,omitempty"`
}

type appServerThreadResumeParams struct {
	ThreadID              string          `json:"threadId"`
	Model                 *string         `json:"model,omitempty"`
	ModelProvider         *string         `json:"modelProvider,omitempty"`
	Cwd                   *string         `json:"cwd,omitempty"`
	ApprovalPolicy        json.RawMessage `json:"approvalPolicy,omitempty"`
	Sandbox               json.RawMessage `json:"sandbox,omitempty"`
	Config                *map[string]any `json:"config,omitempty"`
	BaseInstructions      *string         `json:"baseInstructions,omitempty"`
	DeveloperInstructions *string         `json:"developerInstructions,omitempty"`
}

type appServerDynamicTool struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	InputSchema any    `json:"inputSchema"`
}

func buildAppServerThreadStartParams(options appcodex.ThreadStartOptions, tools []Tool, nativeToolCalls bool) (appServerThreadStartParams, error) {
	params := appServerThreadStartParams{}
	if options.Model != "" {
		params.Model = stringPtr(options.Model)
	}
	if options.Cwd != "" {
		params.Cwd = stringPtr(options.Cwd)
	}
	if raw, err := marshalAppServerJSONValue("approvalPolicy", options.ApprovalPolicy); err != nil {
		return params, err
	} else if len(raw) != 0 {
		params.ApprovalPolicy = raw
	}
	if raw, err := marshalAppServerJSONValue("sandbox", options.SandboxPolicy); err != nil {
		return params, err
	} else if len(raw) != 0 {
		params.Sandbox = raw
	}
	if options.Config != nil {
		config := options.Config
		params.Config = &config
	}
	if options.BaseInstructions != "" {
		params.BaseInstructions = stringPtr(options.BaseInstructions)
	}
	if options.DeveloperInstructions != "" {
		params.DeveloperInstructions = stringPtr(options.DeveloperInstructions)
	}
	if nativeToolCalls && len(tools) != 0 {
		params.DynamicTools = make([]appServerDynamicTool, 0, len(tools))
		for _, tool := range tools {
			params.DynamicTools = append(params.DynamicTools, appServerDynamicTool{
				Name:        tool.Name(),
				Description: tool.Description(),
				InputSchema: tool.InputSchema(),
			})
		}
	}
	if options.ExperimentalRawEvents {
		return params, errors.New("experimental raw events are no longer supported by the current app-server protocol")
	}
	return params, nil
}

func buildAppServerThreadResumeParams(options appcodex.ThreadResumeOptions) (appServerThreadResumeParams, error) {
	params := appServerThreadResumeParams{ThreadID: options.ThreadID}
	if len(options.History) > 0 {
		return params, errors.New("thread resume history is no longer supported by the current app-server protocol")
	}
	if options.Path != "" {
		return params, errors.New("thread resume path is no longer supported by the current app-server protocol")
	}
	if options.Model != "" {
		params.Model = stringPtr(options.Model)
	}
	if options.ModelProvider != "" {
		params.ModelProvider = stringPtr(options.ModelProvider)
	}
	if options.Cwd != "" {
		params.Cwd = stringPtr(options.Cwd)
	}
	if raw, err := marshalAppServerJSONValue("approvalPolicy", options.ApprovalPolicy); err != nil {
		return params, err
	} else if len(raw) != 0 {
		params.ApprovalPolicy = raw
	}
	if raw, err := marshalAppServerJSONValue("sandbox", options.Sandbox); err != nil {
		return params, err
	} else if len(raw) != 0 {
		params.Sandbox = raw
	}
	if options.Config != nil {
		config := options.Config
		params.Config = &config
	}
	if options.BaseInstructions != "" {
		params.BaseInstructions = stringPtr(options.BaseInstructions)
	}
	if options.DeveloperInstructions != "" {
		params.DeveloperInstructions = stringPtr(options.DeveloperInstructions)
	}
	return params, nil
}

func buildAppServerTurnStartParams(threadID string, inputs []appcodex.Input, opts *appcodex.TurnOptions) (appproto.TurnStartParams, error) {
	params := appproto.TurnStartParams{
		ThreadID: threadID,
		Input:    make([]appproto.TurnStartParamsInputElem, 0, len(inputs)),
	}
	for _, input := range inputs {
		params.Input = append(params.Input, input)
	}
	if opts == nil {
		return params, nil
	}

	if opts.Cwd != "" {
		params.Cwd = stringPtr(opts.Cwd)
	}
	if raw, err := marshalAppServerJSONValue("approvalPolicy", opts.ApprovalPolicy); err != nil {
		return params, err
	} else if len(raw) != 0 {
		params.ApprovalPolicy = raw
	}
	if raw, err := marshalAppServerJSONValue("sandboxPolicy", opts.SandboxPolicy); err != nil {
		return params, err
	} else if len(raw) != 0 {
		params.SandboxPolicy = raw
	}
	if opts.Model != "" {
		params.Model = stringPtr(opts.Model)
	}
	if raw, err := marshalAppServerJSONValue("effort", opts.Effort); err != nil {
		return params, err
	} else if len(raw) != 0 {
		params.Effort = raw
	}
	if raw, err := marshalAppServerJSONValue("summary", opts.Summary); err != nil {
		return params, err
	} else if len(raw) != 0 {
		params.Summary = raw
	}
	if raw, err := marshalAppServerJSONValue("outputSchema", opts.OutputSchema); err != nil {
		return params, err
	} else if len(raw) != 0 {
		params.OutputSchema = raw
	}
	if opts.CollaborationMode != nil {
		return params, errors.New("collaboration mode is no longer supported by the current app-server protocol")
	}

	return params, nil
}

func newAppServerRPCClient(ctx context.Context, options appcodex.Options, existing *appcodex.Codex, handler apprpc.ServerRequestHandler) (*apprpc.Client, func() error, bool, error) {
	if existing != nil {
		rpcClient := existing.Client()
		if rpcClient != nil {
			rpcClient.SetRequestHandler(handler)
		}
		return rpcClient, existing.Close, false, nil
	}

	logger := resolveAppServerLogger(options.Logger)
	transport := options.Transport
	if transport == nil {
		spawn := options.Spawn
		if spawn.CodexPath == "" {
			spawn.CodexPath = "codex"
		}
		args := []string{"app-server"}
		for _, override := range spawn.ConfigOverrides {
			args = append(args, "--config", override)
		}
		args = append(args, spawn.ExtraArgs...)

		logger.Info("assistant starting app-server", "path", spawn.CodexPath, "args", strings.Join(args, " "))

		if spawn.Stderr == nil {
			spawn.Stderr = apprpc.DefaultStderr()
		}
		var err error
		transport, err = apprpc.SpawnStdio(context.WithoutCancel(ctx), spawn.CodexPath, args, spawn.Stderr)
		if err != nil {
			return nil, nil, false, err
		}
	}

	rpcClient := apprpc.NewClient(transport, apprpc.ClientOptions{
		Logger:         logger,
		RequestHandler: handler,
	})

	initializeParams := appproto.InitializeParams{
		ClientInfo: appproto.ClientInfo{
			Name:    "assistant-appserver-runner",
			Title:   stringPtr("Assistant AppServer Runner"),
			Version: appServerRunnerVersion(),
		},
		Capabilities: appproto.InitializeCapabilities{
			ExperimentalApi: true,
		},
	}
	if options.ClientInfo.Name != "" {
		initializeParams.ClientInfo = options.ClientInfo
	}

	var initResponse any
	if err := rpcClient.Call(ctx, "initialize", initializeParams, &initResponse); err != nil {
		_ = rpcClient.Close()
		return nil, nil, false, err
	}
	if err := rpcClient.Notify(ctx, "initialized", nil); err != nil {
		_ = rpcClient.Close()
		return nil, nil, false, err
	}

	return rpcClient, rpcClient.Close, true, nil
}

func resolveAppServerLogger(logger *slog.Logger) *slog.Logger {
	if logger != nil {
		return logger
	}
	return slog.New(slog.NewTextHandler(os.Stderr, nil))
}

func appServerRunnerVersion() string {
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" {
		return info.Main.Version
	}
	return "dev"
}

func (r *AppServerRunner) startRPCThread(ctx context.Context, options appcodex.ThreadStartOptions) (appServerThread, error) {
	if r == nil || r.rpcClient == nil {
		return nil, errors.New("rpc client is not initialized")
	}

	_, tools := r.globalContext()
	params, err := buildAppServerThreadStartParams(options, tools, r.nativeToolCalls)
	if err != nil {
		return nil, err
	}
	var response appproto.ThreadStartResponse
	if err := r.rpcClient.Call(ctx, "thread/start", params, &response); err != nil {
		return nil, err
	}

	threadID := response.ThreadID
	if threadID == "" && response.Thread != nil {
		threadID = response.Thread.ID
	}
	if threadID == "" {
		return nil, errors.New("thread id not found in thread/start response")
	}

	return &rpcAppServerThread{client: r.rpcClient, id: threadID}, nil
}

func (r *AppServerRunner) resumeRPCThread(ctx context.Context, options appcodex.ThreadResumeOptions) (appServerThread, error) {
	if r == nil || r.rpcClient == nil {
		return nil, errors.New("rpc client is not initialized")
	}

	params, err := buildAppServerThreadResumeParams(options)
	if err != nil {
		return nil, err
	}
	var response appproto.ThreadResumeResponse
	if err := r.rpcClient.Call(ctx, "thread/resume", params, &response); err != nil {
		return nil, err
	}

	threadID := response.ThreadID
	if threadID == "" && response.Thread != nil {
		threadID = response.Thread.ID
	}
	if threadID == "" {
		return nil, errors.New("thread id not found in thread/resume response")
	}

	return &rpcAppServerThread{client: r.rpcClient, id: threadID}, nil
}

type rpcAppServerThread struct {
	client *apprpc.Client
	id     string
}

func (t *rpcAppServerThread) ID() string {
	if t == nil {
		return ""
	}
	return t.id
}

func (t *rpcAppServerThread) RunStreamed(ctx context.Context, inputs []appcodex.Input, opts *appcodex.TurnOptions) (appServerTurnStream, error) {
	if t == nil || t.client == nil {
		return nil, errors.New("thread client is not initialized")
	}

	iter := t.client.SubscribeNotifications(0)
	params, err := buildAppServerTurnStartParams(t.id, inputs, opts)
	if err != nil {
		iter.Close()
		return nil, err
	}
	var response any
	if err := t.client.Call(ctx, "turn/start", params, &response); err != nil {
		iter.Close()
		return nil, err
	}

	return &rpcAppServerTurnStream{iter: iter, threadID: t.id}, nil
}

type rpcAppServerTurnStream struct {
	iter     *apprpc.NotificationIterator
	threadID string
}

func (s *rpcAppServerTurnStream) Next(ctx context.Context) (apprpc.Notification, error) {
	if s == nil || s.iter == nil {
		return apprpc.Notification{}, errors.New("turn stream is not initialized")
	}

	for {
		note, err := s.iter.Next(ctx)
		if err != nil {
			return note, err
		}
		if s.threadID == "" || matchesAppServerThreadID(note, s.threadID) {
			return note, nil
		}
	}
}

func (s *rpcAppServerTurnStream) Close() {
	if s == nil || s.iter == nil {
		return
	}
	s.iter.Close()
}

func matchesAppServerThreadID(note apprpc.Notification, threadID string) bool {
	payload, err := parseAppServerTurnNotification(note)
	if err != nil || payload.ThreadID == "" {
		return true
	}
	return payload.ThreadID == threadID
}

func (*AppServerRunner) AccountChatgptAuthTokensRefresh(context.Context, appproto.ChatgptAuthTokensRefreshParams) (*appproto.ChatgptAuthTokensRefreshResponse, error) {
	return nil, errors.New("chatgpt auth token refresh is not configured")
}

func (*AppServerRunner) ApplyPatchApproval(_ context.Context, params appproto.ApplyPatchApprovalParams) (*appproto.ApplyPatchApprovalResponse, error) {
	logAppServerRequest("auto-approved apply patch request", params)
	response := appproto.SanitizedApplyPatchApprovalResponseJSON{Decision: "approved"}
	return &response, nil
}

func (*AppServerRunner) ExecCommandApproval(_ context.Context, params appproto.ExecCommandApprovalParams) (*appproto.ExecCommandApprovalResponse, error) {
	logAppServerRequest("auto-approved exec command request", params)
	response := appproto.SanitizedExecCommandApprovalResponseJSON{Decision: "approved"}
	return &response, nil
}

func (*AppServerRunner) ItemCommandExecutionRequestApproval(_ context.Context, params appproto.CommandExecutionRequestApprovalParams) (*appproto.CommandExecutionRequestApprovalResponse, error) {
	logAppServerRequest("auto-approved command execution request", params)
	response := appproto.CommandExecutionRequestApprovalResponse{
		Decision: "accept",
	}
	return &response, nil
}

func (*AppServerRunner) ItemFileChangeRequestApproval(_ context.Context, params appproto.FileChangeRequestApprovalParams) (*appproto.FileChangeRequestApprovalResponse, error) {
	logAppServerRequest("auto-approved file change request", params)
	response := appproto.SanitizedFileChangeRequestApprovalResponseJSON{Decision: "accept"}
	return &response, nil
}

func (*AppServerRunner) ItemPermissionsRequestApproval(_ context.Context, params appproto.PermissionsRequestApprovalParams) (*appproto.PermissionsRequestApprovalResponse, error) {
	logAppServerRequest("auto-approved permissions request", params)
	response := appproto.PermissionsRequestApprovalResponse{
		Permissions: params.Permissions,
	}
	return &response, nil
}

func (*AppServerRunner) ItemToolRequestUserInput(context.Context, appproto.ToolRequestUserInputParams) (*appproto.ToolRequestUserInputResponse, error) {
	return nil, errors.New("tool user input is not configured")
}

func (*AppServerRunner) McpServerElicitationRequest(_ context.Context, params appproto.McpServerElicitationRequestParams) (*appproto.McpServerElicitationRequestResponse, error) {
	if shouldAutoAcceptMCPToolApproval(params) {
		logAppServerRequest("auto-accepted MCP tool approval elicitation request", params)
		response := appproto.McpServerElicitationRequestResponse(
			appproto.SanitizedMCPServerElicitationRequestResponseJSON{
				Action:  appproto.MCPServerElicitationActionAccept,
				Content: map[string]any{},
			},
		)
		return &response, nil
	}

	logAppServerRequest("declined MCP elicitation request because no interactive elicitation handler is configured", params)
	response := appproto.McpServerElicitationRequestResponse(
		appproto.SanitizedMCPServerElicitationRequestResponseJSON{
			Action: appproto.MCPServerElicitationActionDecline,
		},
	)
	return &response, nil
}

func logAppServerRequest(message string, params any) {
	payload, err := json.Marshal(params)
	if err != nil {
		log.Printf("app-server runner %s: marshal_params_err=%v", message, err)
		return
	}
	log.Printf("app-server runner %s: params=%s", message, string(payload))
}

func shouldAutoAcceptMCPToolApproval(params any) bool {
	root, ok := params.(map[string]any)
	if !ok {
		return false
	}

	meta, ok := root["_meta"].(map[string]any)
	if !ok {
		return false
	}
	approvalKind, ok := meta["codex_approval_kind"].(string)
	if !ok || approvalKind != "mcp_tool_call" {
		return false
	}

	requestedSchema, ok := root["requestedSchema"].(map[string]any)
	if !ok {
		return false
	}
	properties, ok := requestedSchema["properties"].(map[string]any)
	if !ok {
		return false
	}

	return len(properties) == 0
}

type appServerDynamicToolCallParams struct {
	ThreadID  string `json:"threadId"`
	TurnID    string `json:"turnId"`
	CallID    string `json:"callId"`
	Tool      string `json:"tool"`
	Arguments any    `json:"arguments"`
}

func (r *AppServerRunner) ItemToolCall(ctx context.Context, params appproto.DynamicToolCallParams) (*appproto.DynamicToolCallResponse, error) {
	if r == nil {
		return nil, errors.New("app-server runner is nil")
	}

	decoded, err := decodeDynamicToolCallParams(params)
	if err != nil {
		return nil, err
	}

	req, ok := r.activeTurn(decoded.ThreadID)
	if !ok {
		return nil, fmt.Errorf("tool call for unknown active thread %q", decoded.ThreadID)
	}
	tool, ok := r.findTool(decoded.Tool)
	if !ok {
		return nil, fmt.Errorf("unknown tool %q", decoded.Tool)
	}

	input, err := json.Marshal(decoded.Arguments)
	if err != nil {
		return nil, fmt.Errorf("encode tool arguments failed: %w", err)
	}
	if len(input) == 0 {
		input = []byte("{}")
	}

	toolCtx := ContextWithTurnRequest(ctx, req)
	log.Printf(
		"app-server runner calling dynamic tool: conversation=%s thread_id=%s tool=%s input_bytes=%d",
		req.Conversation.Key,
		decoded.ThreadID,
		tool.Name(),
		len(input),
	)
	result, callErr := tool.Call(toolCtx, input)
	if callErr != nil {
		log.Printf(
			"app-server runner dynamic tool failed: conversation=%s thread_id=%s tool=%s err=%v",
			req.Conversation.Key,
			decoded.ThreadID,
			tool.Name(),
			callErr,
		)
	} else {
		log.Printf(
			"app-server runner dynamic tool completed: conversation=%s thread_id=%s tool=%s",
			req.Conversation.Key,
			decoded.ThreadID,
			tool.Name(),
		)
	}
	contentItems, buildErr := buildDynamicToolContentItems(result, callErr)
	if buildErr != nil {
		log.Printf(
			"app-server runner dynamic tool result encoding failed: conversation=%s thread_id=%s tool=%s err=%v",
			req.Conversation.Key,
			decoded.ThreadID,
			tool.Name(),
			buildErr,
		)
		return nil, buildErr
	}

	response := appproto.DynamicToolCallResponse(appproto.SanitizedDynamicToolCallResponse{
		ContentItems: contentItems,
		Success:      callErr == nil,
	})
	return &response, nil
}

func decodeDynamicToolCallParams(value appproto.DynamicToolCallParams) (appServerDynamicToolCallParams, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return appServerDynamicToolCallParams{}, fmt.Errorf("encode dynamic tool params failed: %w", err)
	}

	var params appServerDynamicToolCallParams
	if err := json.Unmarshal(data, &params); err != nil {
		return appServerDynamicToolCallParams{}, fmt.Errorf("decode dynamic tool params failed: %w", err)
	}
	if params.ThreadID == "" {
		return appServerDynamicToolCallParams{}, errors.New("dynamic tool params missing threadId")
	}
	if params.Tool == "" {
		return appServerDynamicToolCallParams{}, errors.New("dynamic tool params missing tool")
	}
	return params, nil
}

func buildDynamicToolContentItems(result any, callErr error) ([]appproto.SanitizedDynamicToolCallResponseJSONContentItemsElem, error) {
	if callErr != nil {
		return dynamicToolErrorContentItems(callErr), nil
	}

	switch value := result.(type) {
	case nil:
		return []appproto.SanitizedDynamicToolCallResponseJSONContentItemsElem{map[string]any{
			"type": "inputText",
			"text": "OK",
		}}, nil
	case string:
		return []appproto.SanitizedDynamicToolCallResponseJSONContentItemsElem{map[string]any{
			"type": "inputText",
			"text": value,
		}}, nil
	case []byte:
		return []appproto.SanitizedDynamicToolCallResponseJSONContentItemsElem{map[string]any{
			"type": "inputText",
			"text": string(value),
		}}, nil
	case json.RawMessage:
		return []appproto.SanitizedDynamicToolCallResponseJSONContentItemsElem{map[string]any{
			"type": "inputText",
			"text": string(value),
		}}, nil
	default:
		payload, err := json.Marshal(value)
		if err != nil {
			return nil, fmt.Errorf("encode tool result failed: %w", err)
		}
		return []appproto.SanitizedDynamicToolCallResponseJSONContentItemsElem{map[string]any{
			"type": "inputText",
			"text": string(payload),
		}}, nil
	}
}

func dynamicToolErrorContentItems(err error) []appproto.SanitizedDynamicToolCallResponseJSONContentItemsElem {
	return []appproto.SanitizedDynamicToolCallResponseJSONContentItemsElem{map[string]any{
		"type": "inputText",
		"text": err.Error(),
	}}
}
