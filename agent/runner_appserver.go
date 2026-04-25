package agent

import (
	"context"
	"errors"
	"fmt"
	"log"
	"maps"
	"os"
	"strings"
	"sync"
	"time"

	appcodex "github.com/pmenglund/codex-sdk-go"
	appproto "github.com/pmenglund/codex-sdk-go/protocol"
	apprpc "github.com/pmenglund/codex-sdk-go/rpc"
)

// AppServerRunner bridges dispatcher turns to the Codex app-server through pmenglund/codex-sdk-go.
type AppServerRunner struct {
	closeFn             func() error
	rpcClient           *apprpc.Client
	rpcClientFactory    func(context.Context, apprpc.ServerRequestHandler) (*apprpc.Client, func() error, bool, error)
	interruptTurnFn     func(context.Context, string, string) error
	unsubscribeThreadFn func(context.Context, string) error
	canRecoverRPCClient bool
	closed              bool
	//nolint:containedctx // This is the runner lifecycle root context shared by managed app-server sessions.
	lifecycleCtx    context.Context
	cancel          context.CancelFunc
	startThread     func(context.Context, appcodex.ThreadStartOptions) (appServerThread, error)
	resumeThread    func(context.Context, appcodex.ThreadResumeOptions) (appServerThread, error)
	runThreadTurnFn func(context.Context, TurnRequest, appServerThread, []appcodex.Input, *appcodex.TurnOptions) (*appcodex.TurnResult, error)
	startOptions    appcodex.ThreadStartOptions
	resumeOptions   appcodex.ThreadResumeOptions
	turnOptions     appcodex.TurnOptions
	mu              sync.RWMutex
	systemPrompts   []string
	tools           []Tool
	sessions        map[string]*appServerSession
}

// AppServerRunnerOptions configures an AppServerRunner.
type AppServerRunnerOptions struct {
	Client        *appcodex.Codex
	CodexOptions  appcodex.Options
	StartOptions  appcodex.ThreadStartOptions
	ResumeOptions appcodex.ThreadResumeOptions
	TurnOptions   appcodex.TurnOptions
	SystemPrompt  string
	Tools         []Tool
}

type appServerThread interface {
	ID() string
	RunStreamed(ctx context.Context, inputs []appcodex.Input, opts *appcodex.TurnOptions) (appServerTurnStream, error)
}

type appServerTurnStream interface {
	Next(ctx context.Context) (apprpc.Notification, error)
	TurnID() string
	Close()
}

type appServerTurnContext struct {
	prompts []string
	tools   []Tool
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
	appServerStdioCloseTimeout       = 5 * time.Second
	appServerStdioTerminateTimeout   = 5 * time.Second
)

func (p SandboxPolicy) String() string {
	switch p["type"] {
	case "readOnly":
		return string(appproto.SandboxModeReadOnly)
	case "workspaceWrite":
		return string(appproto.SandboxModeWorkspaceWrite)
	case "dangerFullAccess":
		return string(appproto.SandboxModeDangerFullAccess)
	default:
		return ""
	}
}

var appServerOptOutNotificationMethods = []string{
	"command/exec/outputDelta",
	"item/agentMessage/delta",
	"item/fileChange/outputDelta",
	"item/plan/delta",
	"item/reasoning/summaryTextDelta",
	"item/reasoning/textDelta",
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

	if startOptions.ApprovalPolicy == nil {
		startOptions.ApprovalPolicy = appcodex.ApprovalPolicyNever
	}
	if resumeOptions.ApprovalPolicy == nil {
		resumeOptions.ApprovalPolicy = appcodex.ApprovalPolicyNever
	}
	if turnOptions.ApprovalPolicy == nil {
		turnOptions.ApprovalPolicy = appcodex.ApprovalPolicyNever
	}

	if turnOptions.Effort == nil {
		turnOptions.Effort = appcodex.ReasoningEffortLow
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
	startOptions.SandboxPolicy = applyWorkspaceWriteNetworkAccess(startOptions.SandboxPolicy)
	resumeOptions.Sandbox = applyWorkspaceWriteNetworkAccess(resumeOptions.Sandbox)
	turnOptions.SandboxPolicy = applyWorkspaceWriteNetworkAccess(turnOptions.SandboxPolicy)
	// threads use string, turns use map.
	if sandboxPolicy, ok := startOptions.SandboxPolicy.(SandboxPolicy); ok {
		startOptions.SandboxPolicy = sandboxPolicy.String()
	}
	if sandboxPolicy, ok := resumeOptions.Sandbox.(SandboxPolicy); ok {
		resumeOptions.Sandbox = sandboxPolicy.String()
	}

	runner := &AppServerRunner{
		startOptions:        startOptions,
		resumeOptions:       resumeOptions,
		turnOptions:         turnOptions,
		sessions:            make(map[string]*appServerSession),
		canRecoverRPCClient: options.Client == nil && options.CodexOptions.Transport == nil,
	}
	runner.lifecycleCtx, runner.cancel = context.WithCancel(context.Background())
	runner.rpcClientFactory = func(ctx context.Context, handler apprpc.ServerRequestHandler) (*apprpc.Client, func() error, bool, error) {
		return newAppServerRPCClient(ctx, options.CodexOptions, options.Client, handler)
	}

	rpcClient, closeFn, _, err := runner.rpcClientFactory(ctx, runner)
	if err != nil {
		return nil, fmt.Errorf("create codex app-server client failed: %w", err)
	}
	runner.bindRPCClient(rpcClient, closeFn)

	runner.RegisterSystemPrompt(options.SystemPrompt)
	runner.RegisterTools(options.Tools...)

	return runner, nil
}

func defaultAppServerSandboxPolicy() SandboxPolicy {
	return SandboxPolicyReadOnly
}

// StartSession creates or resumes one dispatcher-managed app-server session.
func (r *AppServerRunner) StartSession(ctx context.Context, options SessionOptions) (Session, error) {
	if r == nil {
		return nil, errors.New("start app-server session failed: runner is nil")
	}
	r.mu.RLock()
	closed := r.closed
	r.mu.RUnlock()
	if closed {
		return nil, errors.New("start app-server session failed: runner is closed")
	}
	err := r.ensureRPCClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("start app-server session failed: %w", err)
	}

	conversationKey := strings.TrimSpace(options.ConversationKey)
	sessionID := strings.TrimSpace(options.ResumeSessionID)
	thread, err := r.createAppServerThread(ctx, sessionID)
	if err != nil {
		r.invalidateRPCClientIfRecoverable(err)
		return nil, fmt.Errorf("start app-server session failed: %w", err)
	}
	if thread == nil {
		return nil, errors.New("start app-server session failed: thread is nil")
	}

	threadID := strings.TrimSpace(thread.ID())
	if threadID == "" {
		threadID = sessionID
	}

	session := &appServerSession{
		runner:          r,
		conversationKey: conversationKey,
		threadID:        threadID,
		thread:          thread,
	}
	r.registerSession(conversationKey, session)
	log.Printf(
		"app-server session thread ready: conversation=%s requested_thread=%s actual_thread=%s",
		conversationKey,
		sessionID,
		threadID,
	)
	return session, nil
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
		config[appServerConfigWebSearchKey] = string(appproto.WebSearchModeLive)
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
	r.mu.Lock()
	closeFn := r.closeFn
	r.closeFn = nil
	r.rpcClient = nil
	r.startThread = nil
	r.resumeThread = nil
	r.interruptTurnFn = nil
	r.unsubscribeThreadFn = nil
	r.mu.Unlock()

	var err error
	if closeFn != nil {
		err = errors.Join(err, closeFn())
	}

	if err != nil {
		log.Printf("app-server runner closed: err=%v", err)
	} else {
		log.Printf("app-server runner closed")
	}
	return err
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

func (r *AppServerRunner) globalTools() []Tool {
	if r == nil {
		return nil
	}

	r.mu.RLock()
	defer r.mu.RUnlock()

	return append([]Tool(nil), r.tools...)
}

func (r *AppServerRunner) bindRPCClient(client *apprpc.Client, closeFn func() error) {
	r.rpcClient = client
	r.closeFn = closeFn
	if client == nil {
		r.startThread = nil
		r.resumeThread = nil
		r.interruptTurnFn = nil
		r.unsubscribeThreadFn = nil
		return
	}
	r.startThread = func(ctx context.Context, options appcodex.ThreadStartOptions) (appServerThread, error) {
		return r.startRPCThread(ctx, options)
	}
	r.resumeThread = func(ctx context.Context, options appcodex.ThreadResumeOptions) (appServerThread, error) {
		return r.resumeRPCThread(ctx, options)
	}
	r.interruptTurnFn = func(ctx context.Context, threadID string, turnID string) error {
		return r.interruptRPCThreadTurn(ctx, threadID, turnID)
	}
	r.unsubscribeThreadFn = func(ctx context.Context, threadID string) error {
		return r.unsubscribeRPCThread(ctx, threadID)
	}
}

func (r *AppServerRunner) ensureRPCClient(ctx context.Context) error {
	if r == nil {
		return errors.New("app-server runner is nil")
	}

	r.mu.RLock()
	client := r.rpcClient
	closed := r.closed
	factory := r.rpcClientFactory
	canRecover := r.canRecoverRPCClient
	r.mu.RUnlock()
	if closed {
		return errors.New("app-server runner is closed")
	}
	if client != nil {
		return nil
	}
	if !canRecover || factory == nil {
		return nil
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return errors.New("app-server runner is closed")
	}
	if r.rpcClient != nil {
		return nil
	}

	client, closeFn, _, err := factory(ctx, r)
	if err != nil {
		return fmt.Errorf("recreate codex app-server client failed: %w", err)
	}
	r.bindRPCClient(client, closeFn)
	log.Printf("app-server runner rpc client recreated")
	return nil
}

func (r *AppServerRunner) invalidateRPCClientIfRecoverable(err error) {
	if r == nil || !shouldRecoverAppServerRPCClient(err) {
		return
	}

	r.mu.Lock()
	if r.closed || r.rpcClient == nil {
		r.mu.Unlock()
		return
	}
	closeFn := r.closeFn
	r.bindRPCClient(nil, nil)
	r.mu.Unlock()

	log.Printf("app-server runner invalidating rpc client after transport failure: err=%v", err)
	if closeFn != nil {
		if closeErr := closeFn(); closeErr != nil {
			log.Printf("app-server runner rpc client close failed during invalidation: err=%v", closeErr)
		}
	}
}

func joinAppServerDeveloperInstructions(base string, prompts []string) string {
	return strings.TrimSpace(joinPromptBlocks(base, joinPromptBlocks(prompts...)))
}

func (r *AppServerRunner) createAppServerThread(ctx context.Context, threadID string) (appServerThread, error) {
	if r == nil {
		return nil, errors.New("app-server runner is nil")
	}

	runCtx, releaseRunCtx := joinRunnerContext(ctx, r.lifecycleCtx)
	defer releaseRunCtx()

	trimmedThreadID := strings.TrimSpace(threadID)
	if trimmedThreadID != "" {
		if r.resumeThread == nil {
			return nil, errors.New("resume thread is unavailable")
		}
		options := r.resumeOptions
		options.ThreadID = trimmedThreadID
		return r.resumeThread(runCtx, options)
	}

	if r.startThread == nil {
		return nil, errors.New("start thread is unavailable")
	}
	options := r.startOptions
	options.DeveloperInstructions = joinAppServerDeveloperInstructions(options.DeveloperInstructions, r.globalPrompts())
	if r.rpcClient != nil {
		return r.startRPCThreadWithTools(runCtx, options, r.globalTools())
	}
	return r.startThread(runCtx, options)
}

func (r *AppServerRunner) globalPrompts() []string {
	if r == nil {
		return nil
	}

	r.mu.RLock()
	defer r.mu.RUnlock()

	return append([]string(nil), r.systemPrompts...)
}
