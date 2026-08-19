package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"runtime/debug"
	"strings"

	appcodex "github.com/pmenglund/codex-sdk-go"
	appproto "github.com/pmenglund/codex-sdk-go/protocol"
	apprpc "github.com/pmenglund/codex-sdk-go/rpc"
)

// This file contains the small raw-RPC layer that remains necessary on top of
// codex-sdk-go. Prefer the SDK's public types and client machinery whenever they
// cover an operation. The raw calls below exist because the high-level thread API
// does not yet declare dynamic tools, while this runner must both register custom
// tools and handle their callbacks; it also needs turn IDs for interruption.
//
// When upgrading the SDK, compare these wire structs and method names with the
// generated protocol first. Delete a raw path once the equivalent high-level API
// supports dynamic-tool registration and preserves the lifecycle requirements in
// runner_appserver_transport.go.

type appServerTurnNotification struct {
	WillRetry *bool               `json:"willRetry,omitempty"`
	ThreadID  string              `json:"threadId,omitempty"`
	TurnID    string              `json:"turnId,omitempty"`
	Turn      *appServerTurnState `json:"turn,omitempty"`
	Item      json.RawMessage     `json:"item,omitempty"`
	Error     *appServerTurnError `json:"error,omitempty"`
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

func parseAppServerTurnStatus(note apprpc.Notification) string {
	payload, err := parseAppServerTurnNotification(note)
	if err != nil || payload.Turn == nil {
		return ""
	}

	return strings.TrimSpace(payload.Turn.Status)
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

func shouldRetryAppServerTurn(note apprpc.Notification) bool {
	payload, err := parseAppServerTurnNotification(note)
	if err != nil || payload.WillRetry == nil {
		return false
	}

	return *payload.WillRetry
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

// appServerThreadStartParams extends the generated thread/start shape with
// dynamicTools. The latter is intentionally manual until ThreadStartOptions
// exposes it in the SDK.
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

func buildAppServerThreadStartParams(options appcodex.ThreadStartOptions, tools []Tool) (appServerThreadStartParams, error) {
	params := appServerThreadStartParams{}
	if options.Model != "" {
		params.Model = new(options.Model)
	}
	if options.Cwd != "" {
		params.Cwd = new(options.Cwd)
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
		params.BaseInstructions = new(options.BaseInstructions)
	}
	if options.DeveloperInstructions != "" {
		params.DeveloperInstructions = new(options.DeveloperInstructions)
	}
	if len(tools) != 0 {
		params.DynamicTools = make([]appServerDynamicTool, 0, len(tools))
		for _, tool := range tools {
			params.DynamicTools = append(params.DynamicTools, appServerDynamicTool{
				Name:        tool.Name(),
				Description: tool.Description(),
				InputSchema: tool.InputSchema(),
			})
		}
	}
	return params, nil
}

func buildAppServerThreadResumeParams(options appcodex.ThreadResumeOptions) (appServerThreadResumeParams, error) {
	params := appServerThreadResumeParams{ThreadID: options.ThreadID}
	if options.Model != "" {
		params.Model = new(options.Model)
	}
	if options.ModelProvider != "" {
		params.ModelProvider = new(options.ModelProvider)
	}
	if options.Cwd != "" {
		params.Cwd = new(options.Cwd)
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
		params.BaseInstructions = new(options.BaseInstructions)
	}
	if options.DeveloperInstructions != "" {
		params.DeveloperInstructions = new(options.DeveloperInstructions)
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
		params.Cwd = new(opts.Cwd)
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
		params.Model = new(opts.Model)
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
	return params, nil
}

// newAppServerRPCClient uses the SDK RPC client but manages spawning itself so
// the daemon can use appServerStdioTransport. Capabilities are intentionally not
// sent during initialize: they are optional protocol opt-ins, and notification
// filtering is performed locally by rpcAppServerTurnStream.
func newAppServerRPCClient(ctx context.Context, options appcodex.Options, existing *appcodex.Codex, callbacks *appcodex.ServerRequestCallbacks) (*apprpc.Client, func() error, bool, error) {
	if existing != nil {
		rpcClient := existing.Client()
		if rpcClient == nil {
			return nil, nil, false, errors.New("app-server client is not initialized")
		}
		rpcClient.SetRequestHandler(callbacks)
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
		transport, err = spawnAppServerStdio(context.WithoutCancel(ctx), spawn.CodexPath, args, spawn.Stderr)
		if err != nil {
			return nil, nil, false, err
		}
	}

	rpcClient, err := apprpc.NewClientChecked(transport, apprpc.ClientOptions{
		Logger:         logger,
		RequestHandler: callbacks,
	})
	if err != nil {
		_ = transport.Close()
		return nil, nil, false, err
	}

	initializeParams := appproto.InitializeParams{
		ClientInfo: appproto.ClientInfo{
			Name:    "assistant-appserver-runner",
			Title:   new("Assistant AppServer Runner"),
			Version: appServerRunnerVersion(),
		},
		Capabilities: &appproto.InitializeCapabilities{
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

func shouldRecoverAppServerRPCClient(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, os.ErrClosed) {
		return true
	}

	message := strings.ToLower(strings.TrimSpace(err.Error()))
	switch {
	case strings.Contains(message, "connection closed"):
		return true
	case strings.Contains(message, "client closed"):
		return true
	case strings.Contains(message, "closed pipe"):
		return true
	case strings.Contains(message, "broken pipe"):
		return true
	case strings.Contains(message, "use of closed network connection"):
		return true
	case strings.Contains(message, "file already closed"):
		return true
	case strings.Contains(message, "eof"):
		return true
	default:
		return false
	}
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
	return r.startRPCThreadWithTools(ctx, options, r.globalTools())
}

func (r *AppServerRunner) interruptRPCThreadTurn(ctx context.Context, threadID string, turnID string) error {
	if r == nil || r.rpcClient == nil {
		return errors.New("rpc client is not initialized")
	}

	_, err := r.rpcClient.TurnInterrupt(ctx, appproto.TurnInterruptParams{
		ThreadID: threadID,
		TurnID:   turnID,
	})
	return err
}

func (r *AppServerRunner) unsubscribeRPCThread(ctx context.Context, threadID string) error {
	if r == nil || r.rpcClient == nil {
		return errors.New("rpc client is not initialized")
	}
	if strings.TrimSpace(threadID) == "" {
		return nil
	}

	_, err := r.rpcClient.ThreadUnsubscribe(ctx, appproto.ThreadUnsubscribeParams{
		ThreadID: threadID,
	})
	return err
}

func (r *AppServerRunner) startRPCThreadWithTools(ctx context.Context, options appcodex.ThreadStartOptions, tools []Tool) (appServerThread, error) {
	if r == nil || r.rpcClient == nil {
		return nil, errors.New("rpc client is not initialized")
	}

	params, err := buildAppServerThreadStartParams(options, tools)
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

// rpcAppServerThread retains the low-level client because turn/start and
// turn/interrupt are coordinated by the runner rather than the SDK TurnHandle.
type rpcAppServerThread struct {
	client *apprpc.Client
	id     string
}

type appServerTurnStartResponse struct {
	TurnID string              `json:"turnId,omitempty"`
	Turn   *appServerTurnState `json:"turn,omitempty"`
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
	var response appServerTurnStartResponse
	if err := t.client.Call(ctx, "turn/start", params, &response); err != nil {
		iter.Close()
		return nil, err
	}
	turnID := response.TurnID
	if turnID == "" && response.Turn != nil {
		turnID = response.Turn.ID
	}

	return &rpcAppServerTurnStream{iter: iter, threadID: t.id, turnID: turnID}, nil
}

type rpcAppServerTurnStream struct {
	iter     *apprpc.NotificationIterator
	threadID string
	turnID   string
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
		// Deltas are intentionally discarded before matching or collecting them.
		// The runner only publishes completed items and terminal turn state.
		if shouldIgnoreAppServerNotification(note.Method) {
			continue
		}
		if matchesAppServerTurn(note, s.threadID, s.turnID) {
			return note, nil
		}
	}
}

// shouldIgnoreAppServerNotification reports whether a notification is a streaming delta
// that the runner does not currently consume.
func shouldIgnoreAppServerNotification(method string) bool {
	switch method {
	case "command/exec/outputDelta",
		"item/agentMessage/delta",
		"item/fileChange/outputDelta",
		"item/plan/delta",
		"item/reasoning/summaryTextDelta",
		"item/reasoning/textDelta":
		return true
	default:
		return false
	}
}

func (s *rpcAppServerTurnStream) TurnID() string {
	if s == nil {
		return ""
	}
	return s.turnID
}

func (s *rpcAppServerTurnStream) Close() {
	if s == nil || s.iter == nil {
		return
	}
	s.iter.Close()
}

func matchesAppServerTurn(note apprpc.Notification, threadID, turnID string) bool {
	payload, err := parseAppServerTurnNotification(note)
	if err != nil {
		return false
	}

	if threadID != "" && payload.ThreadID != "" && payload.ThreadID != threadID {
		return false
	}

	if turnID == "" {
		return threadID == "" || payload.ThreadID == "" || payload.ThreadID == threadID
	}

	noteTurnID := payload.TurnID
	if noteTurnID == "" && payload.Turn != nil {
		noteTurnID = payload.Turn.ID
	}
	if noteTurnID == "" {
		return strings.HasPrefix(note.Method, "item/") && (payload.ThreadID == "" || payload.ThreadID == threadID)
	}

	return noteTurnID == turnID
}
