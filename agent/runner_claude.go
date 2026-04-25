package agent

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hzj629206/assistant/agent/claudecode"
)

const defaultClaudeCodeBinaryPath = "claude"
const claudeSDKMCPServerName = "assistant"
const defaultRunnerCloseInterruptTimeout = 5 * time.Second

// ClaudeCodeRunner bridges dispatcher turns to the Claude Code CLI through the local Claude wrapper.
// Dispatcher-owned sessions keep the reusable Claude session handle and active turn state.
type ClaudeCodeRunner struct {
	client         *claudecode.ClaudeClient
	sessionFactory func(context.Context, string, claudecode.RunOptions, claudecode.SessionHooks) (claudecode.Session, error)
	argFileWriter  func(string, string) (string, error)
	runOptions     claudecode.RunOptions
	//nolint:containedctx // This is the runner lifecycle root context shared by managed Claude sessions.
	lifecycleCtx  context.Context
	cancel        context.CancelFunc
	mu            sync.RWMutex
	systemPrompts []string
	tools         []Tool
	argFiles      map[string]string
	runtimeMu     sync.Mutex
	closed        bool
}

// ClaudeCodeRunnerOptions configures a ClaudeCodeRunner.
type ClaudeCodeRunnerOptions struct {
	Client             *claudecode.ClaudeClient
	BinaryPath         string
	RunOptions         claudecode.RunOptions
	SessionIdleTimeout time.Duration
	SystemPrompt       string
	Tools              []Tool
}

// NewClaudeCodeRunner builds a runner backed by the Claude Code CLI.
func NewClaudeCodeRunner(options ClaudeCodeRunnerOptions) *ClaudeCodeRunner {
	client := options.Client
	if client == nil {
		binaryPath := strings.TrimSpace(options.BinaryPath)
		if binaryPath == "" {
			binaryPath = defaultClaudeCodeBinaryPath
		}
		client = claudecode.NewClient(binaryPath)
	}

	runOptions := options.RunOptions
	if runOptions.PermissionMode == "" {
		runOptions.PermissionMode = claudecode.PermissionModeDontAsk
	}
	if len(runOptions.SettingSources) == 0 {
		runOptions.SettingSources = []string{"user", "project", "local"}
	}
	if runOptions.Effort == "" {
		runOptions.Effort = claudecode.EffortLow
	}
	if runOptions.WorkingDirectory == "" {
		workingDirectory, err := os.Getwd()
		if err == nil {
			runOptions.WorkingDirectory = workingDirectory
		}
	}

	runner := &ClaudeCodeRunner{
		client:         client,
		argFileWriter:  claudecode.WriteArgumentTempFile,
		runOptions:     runOptions,
		argFiles:       make(map[string]string),
		sessionFactory: nil,
	}
	runner.lifecycleCtx, runner.cancel = context.WithCancel(context.Background())
	runner.sessionFactory = func(ctx context.Context, conversationKey string, sessionOptions claudecode.RunOptions, hooks claudecode.SessionHooks) (claudecode.Session, error) {
		return claudecode.StartSession(ctx, claudecode.SessionOptions{
			Client:   client,
			Run:      sessionOptions,
			Hooks:    hooks,
			Observer: newClaudeSessionObserver(conversationKey),
		})
	}

	runner.RegisterSystemPrompt(options.SystemPrompt)
	runner.RegisterTools(options.Tools...)

	return runner
}

// RegisterSystemPrompt appends one global system prompt block for new conversations.
func (r *ClaudeCodeRunner) RegisterSystemPrompt(prompt string) {
	if r == nil {
		return
	}

	trimmed := strings.TrimSpace(prompt)
	if trimmed == "" {
		return
	}

	r.mu.Lock()
	r.systemPrompts = append(r.systemPrompts, trimmed)
	r.clearArgFileCacheLocked()
	r.mu.Unlock()
}

// RegisterTools appends tools that are exposed to new conversations.
func (r *ClaudeCodeRunner) RegisterTools(tools ...Tool) {
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
	r.clearArgFileCacheLocked()
	r.mu.Unlock()
}

func (r *ClaudeCodeRunner) globalContext() ([]string, []Tool) {
	if r == nil {
		return nil, nil
	}

	r.mu.RLock()
	defer r.mu.RUnlock()

	return append([]string(nil), r.systemPrompts...), append([]Tool(nil), r.tools...)
}

// StartSession creates or resumes one dispatcher-managed Claude session.
// The dispatcher owns session reuse and lifecycle management.
func (r *ClaudeCodeRunner) StartSession(ctx context.Context, options SessionOptions) (Session, error) {
	if r == nil {
		return nil, errors.New("start claude code session failed: runner is nil")
	}
	r.runtimeMu.Lock()
	if r.closed {
		r.runtimeMu.Unlock()
		return nil, errors.New("start claude code session failed: runner is closed")
	}
	r.runtimeMu.Unlock()

	conversationKey := strings.TrimSpace(options.ConversationKey)
	resumeSessionID := strings.TrimSpace(options.ResumeSessionID)
	_, tools := r.globalContext()
	control := newClaudeControlServer(tools)
	session, err := r.createClaudeSession(ctx, conversationKey, resumeSessionID, control)
	if err != nil {
		return nil, fmt.Errorf("start claude code session failed: %w", err)
	}
	return &claudeRunnerSession{
		runner:          r,
		conversationKey: conversationKey,
		sessionID:       strings.TrimSpace(session.SessionID()),
		session:         session,
		control:         control,
	}, nil
}

func (r *ClaudeCodeRunner) createClaudeSession(ctx context.Context, conversationKey string, sessionID string, control *claudeControlServer) (claudecode.Session, error) {
	if r == nil {
		return nil, errors.New("claude code runner is nil")
	}

	runOptions, err := r.buildClaudeSessionRunOptions(sessionID, control)
	if err != nil {
		return nil, err
	}
	if r.sessionFactory == nil {
		return nil, errors.New("claude code session factory is nil")
	}
	return r.sessionFactory(ctx, conversationKey, runOptions, claudecode.SessionHooks{
		HandleControlRequest: control.handleRequest,
	})
}

func (r *ClaudeCodeRunner) Close() error {
	if r == nil {
		return nil
	}

	r.runtimeMu.Lock()
	if r.closed {
		r.runtimeMu.Unlock()
		return nil
	}
	r.closed = true
	cancel := r.cancel
	r.cancel = nil
	r.runtimeMu.Unlock()

	if cancel != nil {
		cancel()
	}

	r.mu.Lock()
	r.clearArgFileCacheLocked()
	r.mu.Unlock()
	log.Printf("claude code runner closed")
	return nil
}

type claudeControlServer struct {
	mu  sync.RWMutex
	req TurnRequest
	//nolint:containedctx // This stores the active turn context for MCP tool calls during a single turn.
	turnCtx context.Context
	tools   map[string]Tool
	server  *claudecode.ControlServer
}

func newClaudeControlServer(tools []Tool) *claudeControlServer {
	server := &claudeControlServer{
		tools: make(map[string]Tool, len(tools)),
	}
	for _, tool := range tools {
		if tool != nil {
			server.tools[tool.Name()] = tool
		}
	}
	server.server = &claudecode.ControlServer{
		ServerName:   claudeSDKMCPServerName,
		ToolProvider: server,
	}
	return server
}

func (s *claudeControlServer) mcpConfigJSON() (string, error) {
	if s == nil || s.server == nil {
		return "", errors.New("claude control server is nil")
	}
	return s.server.BuildMCPConfigJSON()
}

func (s *claudeControlServer) handleRequest(request map[string]any) (map[string]any, error) {
	if s == nil || s.server == nil {
		return nil, errors.New("claude control server is nil")
	}

	ctx := context.Background()
	s.mu.RLock()
	if s.turnCtx != nil {
		ctx = s.turnCtx
	}
	s.mu.RUnlock()
	return s.server.HandleRequest(ctx, request)
}

func (s *claudeControlServer) bindTurn(ctx context.Context, req TurnRequest) {
	if s == nil {
		return
	}

	s.mu.Lock()
	s.turnCtx = ctx
	s.req = req
	s.mu.Unlock()
}

func (s *claudeControlServer) clearTurnContext() {
	if s == nil {
		return
	}

	s.mu.Lock()
	s.turnCtx = nil
	s.req = TurnRequest{}
	s.mu.Unlock()
}

func (s *claudeControlServer) ListTools() []claudecode.MCPToolDefinition {
	names := make([]string, 0, len(s.tools))
	for name := range s.tools {
		names = append(names, name)
	}
	slices.Sort(names)

	tools := make([]claudecode.MCPToolDefinition, 0, len(names))
	for _, name := range names {
		tool := s.tools[name]
		tools = append(tools, claudecode.MCPToolDefinition{
			Name:        tool.Name(),
			Description: tool.Description(),
			InputSchema: tool.InputSchema(),
		})
	}

	return tools
}

func (s *claudeControlServer) CallTool(ctx context.Context, toolName string, arguments map[string]any) (any, error) {
	s.mu.RLock()
	req := s.req
	turnCtx := s.turnCtx
	s.mu.RUnlock()

	tool, ok := s.tools[toolName]
	if !ok {
		return nil, fmt.Errorf("tool %q not found", toolName)
	}

	inputJSON, err := json.Marshal(arguments)
	if err != nil {
		return nil, fmt.Errorf("encode tool arguments failed: %w", err)
	}

	log.Printf(
		"claude code runner handling MCP tool call: conversation=%s tool=%s input_bytes=%d",
		req.Conversation.Key,
		toolName,
		len(inputJSON),
	)

	//nolint:contextcheck // Tool calls use the active turn context when available, otherwise the request context.
	toolCtx := ctx
	if turnCtx != nil {
		toolCtx = turnCtx
	}
	toolCtx = ContextWithTurnRequest(toolCtx, req)
	result, err := tool.Call(toolCtx, inputJSON)
	if err != nil {
		log.Printf(
			"claude code runner MCP tool failed: conversation=%s tool=%s err=%v",
			req.Conversation.Key,
			toolName,
			err,
		)
		return nil, err
	}
	return result, nil
}

func (r *ClaudeCodeRunner) applyArgumentFiles(runOptions *claudecode.RunOptions) error {
	if r == nil || runOptions == nil {
		return nil
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	createdPaths := make(map[string]string)
	defer func() {
		if createdPaths == nil {
			return
		}
		for cacheKey, path := range createdPaths {
			delete(r.argFiles, cacheKey)
			claudecode.RemoveTempFile(path)
		}
	}()

	var err error
	runOptions.SystemPrompt, err = r.argFilePathLocked("system-prompt", runOptions.SystemPrompt, createdPaths)
	if err != nil {
		return err
	}
	runOptions.AppendPrompt, err = r.argFilePathLocked("append-system-prompt", runOptions.AppendPrompt, createdPaths)
	if err != nil {
		return err
	}
	for index, config := range runOptions.MCPConfigs {
		runOptions.MCPConfigs[index], err = r.argFilePathLocked("mcp-config", config, createdPaths)
		if err != nil {
			return err
		}
	}
	createdPaths = nil
	return nil
}

func (r *ClaudeCodeRunner) argFilePathLocked(name string, content string, createdPaths map[string]string) (string, error) {
	if strings.TrimSpace(content) == "" {
		return "", nil
	}
	if r.argFiles == nil {
		r.argFiles = make(map[string]string)
	}

	cacheKey := name + "\x00" + content
	path := r.argFiles[cacheKey]
	if path != "" {
		exists, err := claudecode.TempFileExists(path)
		if err != nil {
			return "", err
		}
		if exists {
			return path, nil
		}
		delete(r.argFiles, cacheKey)
	}

	var err error
	if r.argFileWriter == nil {
		r.argFileWriter = claudecode.WriteArgumentTempFile
	}
	path, err = r.argFileWriter(name, content)
	if err != nil {
		return "", err
	}
	r.argFiles[cacheKey] = path
	if createdPaths != nil {
		createdPaths[cacheKey] = path
	}
	return path, nil
}

func (r *ClaudeCodeRunner) clearArgFileCacheLocked() {
	for _, path := range r.argFiles {
		claudecode.RemoveTempFile(path)
	}
	clear(r.argFiles)
}

func (r *ClaudeCodeRunner) buildTurnPrompt(req TurnRequest) (string, []string) {
	return buildTurnPrompt(req.Message)
}

func (r *ClaudeCodeRunner) buildClaudeSessionRunOptions(sessionID string, control *claudeControlServer) (claudecode.RunOptions, error) {
	systemPrompts, tools := r.globalContext()
	runOptions := r.runOptions
	if sessionID == "" {
		runOptions.AppendPrompt = joinPromptBlocks(runOptions.AppendPrompt, joinPromptBlocks(systemPrompts...))
		generatedSessionID, err := newClaudeSessionID()
		if err != nil {
			return claudecode.RunOptions{}, err
		}
		runOptions.SessionID = generatedSessionID
	} else {
		runOptions.AppendPrompt = ""
		runOptions.SessionID = ""
	}
	runOptions.Verbose = true
	runOptions.ResumeID = sessionID
	runOptions.Continue = false
	runOptions.ForkSession = false
	if len(tools) != 0 {
		if control == nil {
			control = newClaudeControlServer(tools)
		}
		configJSON, err := control.mcpConfigJSON()
		if err != nil {
			return claudecode.RunOptions{}, err
		}
		runOptions.MCPConfigs = append(append([]string(nil), runOptions.MCPConfigs...), configJSON)
	}
	err := r.applyArgumentFiles(&runOptions)
	if err != nil {
		return claudecode.RunOptions{}, err
	}
	return runOptions, nil
}

func newClaudeSessionID() (string, error) {
	var uuid [16]byte
	_, err := rand.Read(uuid[:])
	if err != nil {
		return "", fmt.Errorf("generate claude session id failed: %w", err)
	}
	uuid[6] = (uuid[6] & 0x0f) | 0x40
	uuid[8] = (uuid[8] & 0x3f) | 0x80
	return fmt.Sprintf(
		"%08x-%04x-%04x-%04x-%012x",
		uuid[0:4],
		uuid[4:6],
		uuid[6:8],
		uuid[8:10],
		uuid[10:16],
	), nil
}

func newClaudeSessionObserver(conversationKey string) claudecode.SessionObserver {
	return claudecode.SessionObserver{
		OnProcessSpawn: func(client *claudecode.ClaudeClient, opts claudecode.RunOptions, args []string) {
			binPath := ""
			if client != nil {
				binPath = client.BinPath
			}
			log.Printf("claude code runner spawning persistent process: conversation=%s bin=%s cwd=%s args=%q", conversationKey, binPath, opts.WorkingDirectory, args)
		},
		OnProcessStarted: func(pid int) {
			log.Printf("claude code runner persistent process started: conversation=%s pid=%d", conversationKey, pid)
		},
		OnControlRequest: func(pid int, requestID string, subtype string) {
			if subtype != "can_use_tool" && subtype != "mcp_message" {
				return
			}
			log.Printf("claude code runner received control request: conversation=%s pid=%d request_id=%s subtype=%s", conversationKey, pid, requestID, subtype)
		},
		OnSystemMessage: func(pid int, msg *claudecode.Message) {
			if msg == nil || strings.TrimSpace(msg.SessionID) == "" {
				return
			}
			log.Printf("claude code runner received system message: conversation=%s pid=%d session_id=%s", conversationKey, pid, strings.TrimSpace(msg.SessionID))
		},
		OnAssistantMessage: func(pid int, roleMessage claudecode.StreamRoleMessage) {
			for _, block := range roleMessage.Content {
				switch block.Type {
				case "text":
					if strings.TrimSpace(block.Text) == "" {
						continue
					}
					log.Printf("claude code runner received assistant text chunk: conversation=%s pid=%d chars=%d", conversationKey, pid, len(block.Text))
				case "thinking":
					if strings.TrimSpace(block.Thinking) == "" {
						continue
					}
					log.Printf("claude code runner received assistant thinking chunk: conversation=%s pid=%d chars=%d", conversationKey, pid, len(block.Thinking))
				case "tool_use":
					log.Printf("claude code runner received assistant tool use: conversation=%s pid=%d tool=%s input=%s", conversationKey, pid, block.Name, summarizeClaudeToolInput(block.Name, block.Input))
				}
			}
		},
		OnAssistantDecodeError: func(pid int, err error) {
			log.Printf("claude code runner failed to decode assistant message: conversation=%s pid=%d err=%v", conversationKey, pid, err)
		},
		OnUserMessage: func(pid int, roleMessage claudecode.StreamRoleMessage) {
			for _, block := range roleMessage.Content {
				if block.Type != "tool_result" || !block.IsError {
					continue
				}
				log.Printf("claude code runner received tool error result: conversation=%s pid=%d detail=%s", conversationKey, pid, summarizeClaudeToolResult(block.Content))
			}
		},
		OnUserDecodeError: func(pid int, err error) {
			log.Printf("claude code runner failed to decode user message: conversation=%s pid=%d err=%v", conversationKey, pid, err)
		},
		OnResultMessage: func(pid int, msg *claudecode.Message, inputTokens int, outputTokens int) {
			if msg == nil {
				return
			}
			log.Printf(
				"claude code runner received result message: conversation=%s pid=%d session_id=%s result_len=%d is_error=%t input_tokens=%d output_tokens=%d",
				strconv.Quote(sanitizeLogValue(conversationKey)),
				pid,
				strconv.Quote(sanitizeLogValue(msg.SessionID)),
				len(msg.Result),
				msg.IsError,
				inputTokens,
				outputTokens,
			)
		},
		OnControlCancelRequest: func(pid int, requestID string) {
			log.Printf("claude code runner received control cancel request: conversation=%s pid=%d request_id=%s", strconv.Quote(sanitizeLogValue(conversationKey)), pid, strconv.Quote(sanitizeLogValue(requestID)))
		},
		OnUnsupportedMessage: func(pid int, messageType string) {
			log.Printf("claude code runner ignoring unsupported stream message: conversation=%s pid=%d type=%s", strconv.Quote(sanitizeLogValue(conversationKey)), pid, strconv.Quote(sanitizeLogValue(messageType)))
		},
		OnProcessExit: func(pid int, closed bool, waitErr error) {
			if claudecode.IgnoreExpectedExit(waitErr) == nil {
				details := claudeProcessExitLogDetails(waitErr)
				if details == "" {
					log.Printf("claude code runner persistent process exited: conversation=%s pid=%d closed=%t", conversationKey, pid, closed)
					return
				}
				log.Printf("claude code runner persistent process exited: conversation=%s pid=%d closed=%t %s", conversationKey, pid, closed, details)
				return
			}
			log.Printf("claude code runner persistent process wait returned: conversation=%s pid=%d err=%v", conversationKey, pid, waitErr)
		},
	}
}

func claudeProcessExitLogDetails(waitErr error) string {
	if waitErr == nil {
		return "exit_code=0"
	}

	var exitErr *exec.ExitError
	if !errors.As(waitErr, &exitErr) || exitErr.ProcessState == nil {
		return ""
	}

	waitStatus, ok := exitErr.Sys().(syscall.WaitStatus)
	if !ok {
		return ""
	}
	if waitStatus.Signaled() {
		return fmt.Sprintf("signal=%s", waitStatus.Signal())
	}
	if waitStatus.Exited() {
		return fmt.Sprintf("exit_code=%d", waitStatus.ExitStatus())
	}
	return ""
}

func sanitizeLogValue(value string) string {
	if value == "" {
		return ""
	}

	replacer := strings.NewReplacer(
		"\n", "\\n",
		"\r", "\\r",
		"\t", "\\t",
	)
	return replacer.Replace(value)
}

func summarizeClaudeToolInput(toolName string, input map[string]any) string {
	_ = toolName
	if len(input) == 0 {
		return "{}"
	}

	data, err := json.Marshal(input)
	if err != nil {
		return fmt.Sprintf("%v", input)
	}

	return string(data)
}

func summarizeClaudeToolResult(content any) string {
	switch value := content.(type) {
	case nil:
		return "null"
	case string:
		if strings.TrimSpace(value) == "" {
			return `""`
		}
		return value
	default:
		data, err := json.Marshal(value)
		if err != nil {
			return fmt.Sprintf("%v", value)
		}
		return string(data)
	}
}
