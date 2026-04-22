package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hzj629206/assistant/agent/claudecode"
)

const defaultClaudeCodeBinaryPath = "claude"
const claudeSDKMCPServerName = "assistant"
const defaultClaudeCodeSessionIdleTimeout = 10 * time.Minute

// ClaudeCodeRunner bridges dispatcher turns to the Claude Code CLI through the local Claude wrapper.
// Claude sessions are reused per conversation while idle and restarted from the stored
// Claude session ID after eviction or process failure.
type ClaudeCodeRunner struct {
	client             *claudecode.ClaudeClient
	sessionFactory     func(context.Context, string, claudecode.RunOptions) (claudecode.Session, error)
	argFileWriter      func(string, string) (string, error)
	retryPolicy        *claudecode.RetryPolicy
	runOptions         claudecode.RunOptions
	sessionIdleTimeout time.Duration
	mu                 sync.RWMutex
	systemPrompts      []string
	tools              []Tool
	argFiles           map[string]string
	sessionsMu         sync.Mutex
	sessions           map[string]*claudeRunnerSession
	closed             bool
}

// ClaudeCodeRunnerOptions configures a ClaudeCodeRunner.
type ClaudeCodeRunnerOptions struct {
	Client             *claudecode.ClaudeClient
	BinaryPath         string
	RunOptions         claudecode.RunOptions
	RetryPolicy        *claudecode.RetryPolicy
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
	if runOptions.Effort == "" {
		runOptions.Effort = claudecode.EffortLow
	}
	if runOptions.WorkingDirectory == "" {
		workingDirectory, err := os.Getwd()
		if err == nil {
			runOptions.WorkingDirectory = workingDirectory
		}
	}

	retryPolicy := options.RetryPolicy
	if retryPolicy == nil {
		retryPolicy = claudecode.DefaultRetryPolicy()
	}

	runner := &ClaudeCodeRunner{
		client:         client,
		argFileWriter:  claudecode.WriteArgumentTempFile,
		retryPolicy:    claudecode.CloneRetryPolicy(retryPolicy),
		runOptions:     runOptions,
		argFiles:       make(map[string]string),
		sessions:       make(map[string]*claudeRunnerSession),
		sessionFactory: nil,
	}
	runner.sessionFactory = func(ctx context.Context, conversationKey string, sessionOptions claudecode.RunOptions) (claudecode.Session, error) {
		return claudecode.StartSession(ctx, client, sessionOptions, newClaudeSessionObserver(conversationKey))
	}
	runner.sessionIdleTimeout = options.SessionIdleTimeout
	if runner.sessionIdleTimeout <= 0 {
		runner.sessionIdleTimeout = defaultClaudeCodeSessionIdleTimeout
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

// RunTurn runs one Claude Code turn and returns the updated session mapping and final reply text.
func (r *ClaudeCodeRunner) RunTurn(ctx context.Context, req TurnRequest) (TurnResult, error) {
	if r == nil {
		return TurnResult{}, errors.New("run claude code turn failed: runner is nil")
	}

	prompt, imagePaths := r.buildTurnPrompt(req)

	sessionID := req.Conversation.RunnerThreadID
	_, tools := r.globalContext()
	log.Printf("claude code runner executing turn: conversation=%s session_id=%s tool_count=%d", req.Conversation.Key, sessionID, len(tools))
	stopTyping := startTyping(ctx, req.Message.Responder)
	defer stopTyping()

	result, err := r.runClaudeTurn(ctx, req, prompt, imagePaths, sessionID)
	if err != nil {
		return TurnResult{}, fmt.Errorf("run claude code turn failed: %w", err)
	}
	sessionID = claudecode.ResolveSessionID(sessionID, result)
	return TurnResult{
		RunnerThreadID: sessionID,
		ReplyText:      result.Result,
	}, nil
}

func (r *ClaudeCodeRunner) runClaudeTurn(ctx context.Context, req TurnRequest, prompt string, imagePaths []string, sessionID string) (*claudecode.ClaudeResult, error) {
	_, tools := r.globalContext()
	control := newClaudeControlServer(req, tools)
	blocks, err := claudecode.BuildUserContentBlocks(prompt, imagePaths)
	if err != nil {
		return nil, err
	}

	retryPolicy := claudecode.CloneRetryPolicy(r.retryPolicy)
	if retryPolicy == nil {
		retryPolicy = &claudecode.RetryPolicy{}
	}

	var lastErr error
	resumeSessionID := sessionID
	for attempt := 0; attempt <= retryPolicy.MaxRetries; attempt++ {
		if attempt > 0 {
			delay := claudecode.CalculateRetryBackoff(retryPolicy, attempt)
			log.Printf(
				"claude code runner retrying turn: conversation=%s attempt=%d max_retries=%d delay=%s",
				req.Conversation.Key,
				attempt,
				retryPolicy.MaxRetries,
				delay,
			)
			if err := claudecode.WaitRetryDelay(ctx, delay); err != nil {
				return nil, err
			}
		}

		session, err := r.acquireClaudeSession(ctx, req.Conversation.Key, resumeSessionID)
		if err != nil {
			return nil, err
		}
		log.Printf(
			"claude code runner using session process: conversation=%s resume_session=%s prompt_len=%d image_count=%d",
			req.Conversation.Key,
			session.session.CurrentSessionID(),
			len(prompt),
			len(imagePaths),
		)
		result, err := session.session.RunTurn(ctx, blocks, buildClaudeTurnHooks(ctx, control))
		if err == nil {
			r.releaseClaudeSession(req.Conversation.Key, session)
			log.Printf(
				"claude code runner completed turn: conversation=%s session_id=%s result_len=%d",
				req.Conversation.Key,
				result.SessionID,
				len(result.Result),
			)
			return result, nil
		}

		lastErr = err
		resumeSessionID = session.session.CurrentSessionID()
		r.discardClaudeSession(req.Conversation.Key, session, err)
		var claudeErr *claudecode.ClaudeError
		if !errors.As(err, &claudeErr) || !claudeErr.IsRetryable() {
			return nil, err
		}

		if attempt == retryPolicy.MaxRetries {
			break
		}

		retryDelay := time.Duration(claudeErr.RetryDelay()) * time.Second
		if retryDelay > 0 {
			log.Printf(
				"claude code runner received retryable error: conversation=%s attempt=%d error_type=%s retry_delay=%s err=%v",
				req.Conversation.Key,
				attempt+1,
				claudeErr.Type,
				retryDelay,
				err,
			)
			if err = claudecode.WaitRetryDelay(ctx, retryDelay); err != nil {
				return nil, err
			}
		}
	}

	return nil, fmt.Errorf("claude code retries exhausted after %d attempts: %w", retryPolicy.MaxRetries+1, lastErr)
}

type claudeRunnerSession struct {
	session   claudecode.Session
	idleTimer *time.Timer
	inUse     bool
}

func buildClaudeTurnHooks(ctx context.Context, control *claudeControlServer) claudecode.TurnHooks {
	if control == nil {
		return claudecode.TurnHooks{}
	}

	return claudecode.TurnHooks{
		ShouldInitialize: func() bool {
			return control.hasTools()
		},
		HandleControlRequest: func(request map[string]any) (map[string]any, error) {
			return control.handleRequest(ctx, request)
		},
	}
}

func newClaudeSessionObserver(conversationKey string) claudecode.SessionObserver {
	return claudecode.SessionObserver{
		OnProcessSpawn: func(client *claudecode.ClaudeClient, opts claudecode.RunOptions, args []string) {
			binPath := ""
			if client != nil {
				binPath = client.BinPath
			}
			log.Printf(
				"claude code runner spawning persistent process: conversation=%s bin=%s cwd=%s args=%q",
				conversationKey,
				binPath,
				opts.WorkingDirectory,
				args,
			)
		},
		OnProcessStarted: func(pid int) {
			log.Printf("claude code runner persistent process started: conversation=%s pid=%d", conversationKey, pid)
		},
		OnStderrClosed: func(pid int, byteCount int, stderrText string) {
			if stderrText == "" {
				log.Printf("claude code runner stderr stream closed: conversation=%s pid=%d bytes=%d", conversationKey, pid, byteCount)
				return
			}
			log.Printf(
				"claude code runner stderr stream closed: conversation=%s pid=%d bytes=%d stderr=%q",
				conversationKey,
				pid,
				byteCount,
				stderrText,
			)
		},
		OnControlRequest: func(pid int, requestID string, subtype string) {
			if subtype != "can_use_tool" && subtype != "mcp_message" {
				return
			}
			log.Printf(
				"claude code runner received control request: conversation=%s pid=%d request_id=%s subtype=%s",
				conversationKey,
				pid,
				requestID,
				subtype,
			)
		},
		OnSystemMessage: func(pid int, msg *claudecode.Message) {
			if msg == nil || strings.TrimSpace(msg.SessionID) == "" {
				return
			}
			log.Printf(
				"claude code runner received system message: conversation=%s pid=%d session_id=%s",
				conversationKey,
				pid,
				strings.TrimSpace(msg.SessionID),
			)
		},
		OnAssistantMessage: func(pid int, roleMessage claudecode.StreamRoleMessage) {
			for _, block := range roleMessage.Content {
				switch block.Type {
				case "text":
					if strings.TrimSpace(block.Text) == "" {
						continue
					}
					log.Printf(
						"claude code runner received assistant text chunk: conversation=%s pid=%d chars=%d",
						conversationKey,
						pid,
						len(block.Text),
					)
				case "thinking":
					if strings.TrimSpace(block.Thinking) == "" {
						continue
					}
					log.Printf(
						"claude code runner received assistant thinking chunk: conversation=%s pid=%d chars=%d",
						conversationKey,
						pid,
						len(block.Thinking),
					)
				case "tool_use":
					log.Printf(
						"claude code runner received assistant tool use: conversation=%s pid=%d tool=%s input=%s",
						conversationKey,
						pid,
						block.Name,
						summarizeClaudeToolInput(block.Name, block.Input),
					)
				}
			}
		},
		OnAssistantDecodeError: func(pid int, err error) {
			log.Printf(
				"claude code runner failed to decode assistant message: conversation=%s pid=%d err=%v",
				conversationKey,
				pid,
				err,
			)
		},
		OnUserMessage: func(pid int, roleMessage claudecode.StreamRoleMessage) {
			for _, block := range roleMessage.Content {
				if block.Type != "tool_result" || !block.IsError {
					continue
				}
				log.Printf(
					"claude code runner received tool error result: conversation=%s pid=%d detail=%s",
					conversationKey,
					pid,
					summarizeClaudeToolResult(block.Content),
				)
			}
		},
		OnUserDecodeError: func(pid int, err error) {
			log.Printf(
				"claude code runner failed to decode user message: conversation=%s pid=%d err=%v",
				conversationKey,
				pid,
				err,
			)
		},
		OnResultMessage: func(pid int, msg *claudecode.Message, inputTokens int, outputTokens int) {
			if msg == nil {
				return
			}
			safeConversationKey := sanitizeLogValue(conversationKey)
			sessionID := sanitizeLogValue(msg.SessionID)
			log.Printf(
				"claude code runner received result message: conversation=%s pid=%d session_id=%s result_len=%d is_error=%t input_tokens=%d output_tokens=%d",
				strconv.Quote(safeConversationKey),
				pid,
				strconv.Quote(sessionID),
				len(msg.Result),
				msg.IsError,
				inputTokens,
				outputTokens,
			)
		},
		OnControlCancelRequest: func(pid int, requestID string) {
			safeConversationKey := sanitizeLogValue(conversationKey)
			safeRequestID := sanitizeLogValue(requestID)
			log.Printf(
				"claude code runner received control cancel request: conversation=%s pid=%d request_id=%s",
				strconv.Quote(safeConversationKey),
				pid,
				strconv.Quote(safeRequestID),
			)
		},
		OnUnsupportedMessage: func(pid int, messageType string) {
			safeConversationKey := sanitizeLogValue(conversationKey)
			safeMessageType := sanitizeLogValue(messageType)
			log.Printf(
				"claude code runner ignoring unsupported stream message: conversation=%s pid=%d type=%s",
				strconv.Quote(safeConversationKey),
				pid,
				strconv.Quote(safeMessageType),
			)
		},
		OnProcessExit: func(pid int, closed bool, waitErr error) {
			if claudecode.IgnoreExpectedExit(waitErr) == nil {
				log.Printf("claude code runner persistent process exited: conversation=%s pid=%d closed=%t", conversationKey, pid, closed)
				return
			}
			log.Printf("claude code runner persistent process wait returned: conversation=%s pid=%d err=%v", conversationKey, pid, waitErr)
		},
	}
}

func (r *ClaudeCodeRunner) acquireClaudeSession(ctx context.Context, conversationKey string, sessionID string) (*claudeRunnerSession, error) {
	if r == nil {
		return nil, errors.New("claude code runner is nil")
	}

	r.sessionsMu.Lock()
	if r.closed {
		r.sessionsMu.Unlock()
		return nil, errors.New("claude code runner is closed")
	}
	existing := r.sessions[conversationKey]
	if existing != nil {
		if existing.idleTimer != nil {
			existing.idleTimer.Stop()
			existing.idleTimer = nil
		}
		existing.inUse = true
		r.sessionsMu.Unlock()
		return existing, nil
	}
	r.sessionsMu.Unlock()

	runOptions, err := r.buildClaudeSessionRunOptions(sessionID)
	if err != nil {
		return nil, err
	}
	if r.sessionFactory == nil {
		return nil, errors.New("claude code session factory is nil")
	}
	session, err := r.sessionFactory(ctx, conversationKey, runOptions)
	if err != nil {
		return nil, err
	}
	managed := &claudeRunnerSession{
		session: session,
		inUse:   true,
	}

	r.sessionsMu.Lock()
	defer r.sessionsMu.Unlock()
	if r.closed {
		_ = session.Close()
		return nil, errors.New("claude code runner is closed")
	}
	existing = r.sessions[conversationKey]
	if existing != nil {
		if existing.idleTimer != nil {
			existing.idleTimer.Stop()
			existing.idleTimer = nil
		}
		existing.inUse = true
		_ = session.Close()
		return existing, nil
	}
	r.sessions[conversationKey] = managed
	return managed, nil
}

func (r *ClaudeCodeRunner) releaseClaudeSession(conversationKey string, managed *claudeRunnerSession) {
	if r == nil || managed == nil {
		return
	}
	if r.sessionIdleTimeout <= 0 {
		r.discardClaudeSession(conversationKey, managed, nil)
		return
	}

	r.sessionsMu.Lock()
	defer r.sessionsMu.Unlock()
	if r.closed || r.sessions[conversationKey] != managed {
		return
	}
	managed.inUse = false
	if managed.idleTimer != nil {
		managed.idleTimer.Stop()
	}
	managed.idleTimer = time.AfterFunc(r.sessionIdleTimeout, func() {
		r.sessionsMu.Lock()
		if r.sessions[conversationKey] != managed || managed.inUse {
			r.sessionsMu.Unlock()
			return
		}
		r.sessionsMu.Unlock()
		log.Printf("claude code runner expiring idle session: conversation=%s idle_timeout=%s", conversationKey, r.sessionIdleTimeout)
		r.discardClaudeSession(conversationKey, managed, nil)
	})
}

func (r *ClaudeCodeRunner) discardClaudeSession(conversationKey string, managed *claudeRunnerSession, reason error) {
	if r == nil || managed == nil {
		return
	}

	r.sessionsMu.Lock()
	if r.sessions[conversationKey] == managed {
		delete(r.sessions, conversationKey)
	}
	managed.inUse = false
	if managed.idleTimer != nil {
		managed.idleTimer.Stop()
		managed.idleTimer = nil
	}
	r.sessionsMu.Unlock()

	if reason != nil {
		log.Printf("claude code runner closing session process: conversation=%s err=%v", conversationKey, reason)
	}
	_ = managed.session.Close()
}

func (r *ClaudeCodeRunner) buildClaudeSessionRunOptions(sessionID string) (claudecode.RunOptions, error) {
	systemPrompts, tools := r.globalContext()
	runOptions := r.runOptions
	if sessionID == "" {
		runOptions.AppendPrompt = joinPromptBlocks(runOptions.AppendPrompt, joinPromptBlocks(systemPrompts...))
	} else {
		runOptions.AppendPrompt = ""
	}
	runOptions.Verbose = true
	runOptions.ResumeID = sessionID
	runOptions.Continue = false
	runOptions.SessionID = ""
	runOptions.ForkSession = false
	if len(tools) != 0 {
		control := newClaudeControlServer(TurnRequest{}, tools)
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

func (r *ClaudeCodeRunner) Close() error {
	if r == nil {
		return nil
	}

	r.sessionsMu.Lock()
	if r.closed {
		r.sessionsMu.Unlock()
		return nil
	}
	r.closed = true
	sessions := make([]*claudeRunnerSession, 0, len(r.sessions))
	for key, session := range r.sessions {
		if session.idleTimer != nil {
			session.idleTimer.Stop()
			session.idleTimer = nil
		}
		sessions = append(sessions, session)
		delete(r.sessions, key)
	}
	r.sessionsMu.Unlock()

	var closeErr error
	for _, session := range sessions {
		closeErr = errors.Join(closeErr, session.session.Close())
	}
	r.mu.Lock()
	r.clearArgFileCacheLocked()
	r.mu.Unlock()
	if closeErr != nil {
		log.Printf("claude code runner closed: sessions=%d err=%v", len(sessions), closeErr)
	} else {
		log.Printf("claude code runner closed: sessions=%d", len(sessions))
	}
	return closeErr
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
	prompt, imagePaths := buildTurnPrompt(req.Message)
	return prompt, imagePaths
}

type claudeControlServer struct {
	req    TurnRequest
	tools  map[string]Tool
	server *claudecode.ControlServer
}

func newClaudeControlServer(req TurnRequest, tools []Tool) *claudeControlServer {
	server := &claudeControlServer{
		req:   req,
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

func (s *claudeControlServer) hasTools() bool {
	return s != nil && s.server != nil && s.server.HasTools()
}

func (s *claudeControlServer) mcpConfigJSON() (string, error) {
	if s == nil || s.server == nil {
		return "", errors.New("claude control server is nil")
	}
	return s.server.BuildMCPConfigJSON()
}

func (s *claudeControlServer) handleRequest(ctx context.Context, request map[string]any) (map[string]any, error) {
	if s == nil || s.server == nil {
		return nil, errors.New("claude control server is nil")
	}
	return s.server.HandleRequest(ctx, request)
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
		s.req.Conversation.Key,
		toolName,
		len(inputJSON),
	)

	toolCtx := ContextWithTurnRequest(ctx, s.req)
	result, err := tool.Call(toolCtx, inputJSON)
	if err != nil {
		log.Printf(
			"claude code runner MCP tool failed: conversation=%s tool=%s err=%v",
			s.req.Conversation.Key,
			toolName,
			err,
		)
		return nil, err
	}
	return result, nil
}

func formatClaudeMCPToolResult(result any) (string, error) {
	return claudecode.FormatMCPToolResult(result)
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
