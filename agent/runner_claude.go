package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	claudecode "github.com/lancekrogers/claude-code-go/pkg/claude"
)

const defaultClaudeCodeBinaryPath = "claude"
const claudeSDKMCPServerName = "assistant"
const claudeSDKEntrypoint = "sdk-py"
const claudeAgentSDKVersion = "go-local"
const claudeCodeTerminateTimeout = 2 * time.Second
const defaultClaudeCodeSessionIdleTimeout = 10 * time.Minute

// ClaudeCodeRunner bridges dispatcher turns to the Claude Code CLI through claude-code-go.
// Claude sessions are reused per conversation while idle and restarted from the stored
// Claude session ID after eviction or process failure.
type ClaudeCodeRunner struct {
	client             *claudecode.ClaudeClient
	sessionFactory     func(context.Context, string, claudecode.RunOptions) (claudePersistentTurnSession, error)
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
		retryPolicy:    cloneClaudeRetryPolicy(retryPolicy),
		runOptions:     runOptions,
		argFiles:       make(map[string]string),
		sessions:       make(map[string]*claudeRunnerSession),
		sessionFactory: nil,
	}
	runner.sessionFactory = func(ctx context.Context, conversationKey string, sessionOptions claudecode.RunOptions) (claudePersistentTurnSession, error) {
		return startClaudePersistentTurnSession(ctx, client, conversationKey, sessionOptions)
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

	typing := newTypingStatusController(
		req.Message.Responder,
		defaultTypingInitialDelay,
		defaultTypingRefreshCooldown,
	)
	typing.Start(ctx)
	defer typing.Stop()

	sessionID := req.Conversation.RunnerThreadID
	_, tools := r.globalContext()
	log.Printf("claude code runner executing turn: conversation=%s session_id=%s tool_count=%d", req.Conversation.Key, sessionID, len(tools))
	var (
		result *claudecode.ClaudeResult
		err    error
	)
	result, err = r.runClaudeTurn(ctx, req, prompt, imagePaths, sessionID)
	if err != nil {
		return TurnResult{}, fmt.Errorf("run claude code turn failed: %w", err)
	}
	sessionID = resolvedClaudeSessionID(sessionID, result)
	return TurnResult{
		RunnerThreadID: sessionID,
		ReplyText:      result.Result,
	}, nil
}

func (r *ClaudeCodeRunner) runClaudeTurn(ctx context.Context, req TurnRequest, prompt string, imagePaths []string, sessionID string) (*claudecode.ClaudeResult, error) {
	_, tools := r.globalContext()
	control := newClaudeControlServer(req, tools)
	inputReader, err := buildClaudeStreamJSONInput(prompt, imagePaths)
	if err != nil {
		return nil, err
	}
	inputBytes, err := io.ReadAll(inputReader)
	if err != nil {
		return nil, fmt.Errorf("read claude stream input failed: %w", err)
	}

	retryPolicy := cloneClaudeRetryPolicy(r.retryPolicy)
	if retryPolicy == nil {
		retryPolicy = &claudecode.RetryPolicy{}
	}

	var lastErr error
	resumeSessionID := sessionID
	for attempt := 0; attempt <= retryPolicy.MaxRetries; attempt++ {
		if attempt > 0 {
			delay := calculateClaudeRetryBackoff(retryPolicy, attempt)
			log.Printf(
				"claude code runner retrying turn: conversation=%s attempt=%d max_retries=%d delay=%s",
				req.Conversation.Key,
				attempt,
				retryPolicy.MaxRetries,
				delay,
			)
			if err := waitClaudeRetryDelay(ctx, delay); err != nil {
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
		result, err := session.session.RunTurn(ctx, bytes.NewReader(inputBytes), control)
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
			if err = waitClaudeRetryDelay(ctx, retryDelay); err != nil {
				return nil, err
			}
		}
	}

	return nil, fmt.Errorf("claude code retries exhausted after %d attempts: %w", retryPolicy.MaxRetries+1, lastErr)
}

type claudePersistentTurnSession interface {
	RunTurn(ctx context.Context, stdin io.Reader, control *claudeControlServer) (*claudecode.ClaudeResult, error)
	CurrentSessionID() string
	Close() error
}

type claudeRunnerSession struct {
	session   claudePersistentTurnSession
	idleTimer *time.Timer
	inUse     bool
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
	runOptions.Format = claudecode.StreamJSONOutput
	runOptions.InputFormat = claudecode.StreamJSONInput
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
	return closeErr
}

func (r *ClaudeCodeRunner) applyArgumentFiles(runOptions *claudecode.RunOptions) error {
	if r == nil || runOptions == nil {
		return nil
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	var err error
	runOptions.SystemPrompt, err = r.argFilePathLocked("system-prompt", runOptions.SystemPrompt)
	if err != nil {
		return err
	}
	runOptions.AppendPrompt, err = r.argFilePathLocked("append-system-prompt", runOptions.AppendPrompt)
	if err != nil {
		return err
	}
	for index, config := range runOptions.MCPConfigs {
		runOptions.MCPConfigs[index], err = r.argFilePathLocked("mcp-config", config)
		if err != nil {
			return err
		}
	}
	return nil
}

func (r *ClaudeCodeRunner) argFilePathLocked(name string, content string) (string, error) {
	if strings.TrimSpace(content) == "" {
		return "", nil
	}
	if r.argFiles == nil {
		r.argFiles = make(map[string]string)
	}

	cacheKey := name + "\x00" + content
	path := r.argFiles[cacheKey]
	if path != "" {
		exists, err := claudeTempFileExists(path)
		if err != nil {
			return "", err
		}
		if exists {
			return path, nil
		}
		delete(r.argFiles, cacheKey)
	}

	var err error
	path, err = writeClaudeArgumentTempFile(name, content)
	if err != nil {
		return "", err
	}
	r.argFiles[cacheKey] = path
	return path, nil
}

func claudeTempFileExists(path string) (bool, error) {
	info, err := os.Stat(path)
	if err == nil {
		return !info.IsDir(), nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}

func (r *ClaudeCodeRunner) clearArgFileCacheLocked() {
	for _, path := range r.argFiles {
		removeClaudeTempFile(path)
	}
	clear(r.argFiles)
}

func cloneClaudeRetryPolicy(policy *claudecode.RetryPolicy) *claudecode.RetryPolicy {
	if policy == nil {
		return nil
	}

	cloned := *policy
	return &cloned
}

func calculateClaudeRetryBackoff(policy *claudecode.RetryPolicy, attempt int) time.Duration {
	if policy == nil || attempt <= 0 {
		return 0
	}

	delay := policy.BaseDelay
	if delay <= 0 {
		return 0
	}

	factor := policy.BackoffFactor
	if factor <= 0 {
		factor = 1
	}

	for step := 1; step < attempt; step++ {
		nextDelay := time.Duration(float64(delay) * factor)
		if nextDelay <= 0 {
			nextDelay = delay
		}
		delay = nextDelay
		if policy.MaxDelay > 0 && delay >= policy.MaxDelay {
			return policy.MaxDelay
		}
	}

	if policy.MaxDelay > 0 && delay > policy.MaxDelay {
		return policy.MaxDelay
	}

	return delay
}

func waitClaudeRetryDelay(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return nil
	}

	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *ClaudeCodeRunner) buildTurnPrompt(req TurnRequest) (string, []string) {
	prompt, imagePaths := buildTurnPrompt(req.Message)
	return prompt, imagePaths
}

func resolvedClaudeSessionID(previous string, result *claudecode.ClaudeResult) string {
	if result != nil && strings.TrimSpace(result.SessionID) != "" {
		return strings.TrimSpace(result.SessionID)
	}

	return previous
}

func buildClaudeStreamJSONInput(prompt string, imagePaths []string) (io.Reader, error) {
	content := make([]any, 0, 1+len(imagePaths))
	if text := strings.TrimSpace(prompt); text != "" {
		content = append(content, map[string]any{
			"type": "text",
			"text": text,
		})
	}

	for _, imagePath := range imagePaths {
		block, err := buildClaudeImageContentBlock(imagePath)
		if err != nil {
			return nil, err
		}
		content = append(content, block)
	}

	if len(content) == 0 {
		content = append(content, map[string]any{
			"type": "text",
			"text": "",
		})
	}

	payload := map[string]any{
		"type": "user",
		"message": map[string]any{
			"role":    "user",
			"content": content,
		},
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode claude stream input failed: %w", err)
	}

	return bytes.NewReader(append(data, '\n')), nil
}

func buildClaudeImageContentBlock(imagePath string) (map[string]any, error) {
	imagePath = strings.TrimSpace(imagePath)
	if imagePath == "" {
		return nil, errors.New("image path is empty")
	}

	//nolint:gosec // Image paths come from normalized local attachments selected by the host application.
	imageBytes, err := os.ReadFile(imagePath)
	if err != nil {
		return nil, fmt.Errorf("read image %q failed: %w", imagePath, err)
	}
	mediaType := http.DetectContentType(imageBytes)
	if !strings.HasPrefix(mediaType, "image/") {
		return nil, fmt.Errorf("unsupported image media type %q for %s", mediaType, imagePath)
	}

	return map[string]any{
		"type": "image",
		"source": map[string]any{
			"type":       "base64",
			"media_type": mediaType,
			"data":       base64.StdEncoding.EncodeToString(imageBytes),
		},
	}, nil
}

type claudeControlServer struct {
	req   TurnRequest
	tools map[string]Tool
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
	return server
}

func (s *claudeControlServer) hasTools() bool {
	return s != nil && len(s.tools) != 0
}

func (s *claudeControlServer) mcpConfigJSON() (string, error) {
	builder := claudecode.NewMCPConfigBuilder()
	configJSON, err := builder.AddServer(claudeSDKMCPServerName, claudecode.MCPServerConfig{
		Type: "sdk",
		URL:  "",
	}).BuildJSON()
	if err != nil {
		return "", fmt.Errorf("build claude mcp config failed: %w", err)
	}

	var config map[string]any
	if err = json.Unmarshal([]byte(configJSON), &config); err != nil {
		return "", fmt.Errorf("decode claude mcp config failed: %w", err)
	}
	mcpServers, ok := config["mcpServers"].(map[string]any)
	if !ok {
		return "", errors.New("claude mcp config missing mcpServers")
	}
	serverConfig, ok := mcpServers[claudeSDKMCPServerName].(map[string]any)
	if !ok {
		return "", fmt.Errorf("claude mcp config missing server %q", claudeSDKMCPServerName)
	}
	serverConfig["name"] = claudeSDKMCPServerName

	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encode claude mcp config failed: %w", err)
	}

	return string(data), nil
}

func (s *claudeControlServer) handleRequest(ctx context.Context, request map[string]any) (map[string]any, error) {
	subtype, _ := request["subtype"].(string)
	switch subtype {
	case "initialize":
		return map[string]any{}, nil
	case "can_use_tool":
		originalInput, _ := request["input"].(map[string]any)
		return map[string]any{
			"behavior":     "allow",
			"updatedInput": originalInput,
		}, nil
	case "mcp_message":
		serverName, _ := request["server_name"].(string)
		if serverName != claudeSDKMCPServerName {
			return nil, fmt.Errorf("unsupported SDK MCP server %q", serverName)
		}
		message, _ := request["message"].(map[string]any)
		if message == nil {
			return nil, errors.New("missing SDK MCP message payload")
		}

		return map[string]any{
			"mcp_response": s.handleMCPMessage(ctx, message),
		}, nil
	default:
		return nil, fmt.Errorf("unsupported control request subtype %q", subtype)
	}
}

func (s *claudeControlServer) handleMCPMessage(ctx context.Context, message map[string]any) map[string]any {
	id := message["id"]
	method, _ := message["method"].(string)
	params, _ := message["params"].(map[string]any)

	switch method {
	case "initialize":
		return map[string]any{
			"jsonrpc": "2.0",
			"id":      id,
			"result": map[string]any{
				"protocolVersion": "2024-11-05",
				"capabilities": map[string]any{
					"tools": map[string]any{},
				},
				"serverInfo": map[string]any{
					"name":    claudeSDKMCPServerName,
					"version": "1.0.0",
				},
			},
		}
	case "tools/list":
		return map[string]any{
			"jsonrpc": "2.0",
			"id":      id,
			"result": map[string]any{
				"tools": s.listMCPTools(),
			},
		}
	case "tools/call":
		toolName, _ := params["name"].(string)
		arguments, _ := params["arguments"].(map[string]any)
		return s.callMCPTool(ctx, id, toolName, arguments)
	case "notifications/initialized":
		return map[string]any{
			"jsonrpc": "2.0",
			"result":  map[string]any{},
		}
	default:
		return map[string]any{
			"jsonrpc": "2.0",
			"id":      id,
			"error": map[string]any{
				"code":    -32601,
				"message": fmt.Sprintf("method %q not found", method),
			},
		}
	}
}

func (s *claudeControlServer) listMCPTools() []map[string]any {
	names := make([]string, 0, len(s.tools))
	for name := range s.tools {
		names = append(names, name)
	}
	slices.Sort(names)

	tools := make([]map[string]any, 0, len(names))
	for _, name := range names {
		tool := s.tools[name]
		inputSchema := tool.InputSchema()
		if inputSchema == nil {
			inputSchema = map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			}
		}
		tools = append(tools, map[string]any{
			"name":        tool.Name(),
			"description": tool.Description(),
			"inputSchema": inputSchema,
		})
	}

	return tools
}

func (s *claudeControlServer) callMCPTool(ctx context.Context, id any, toolName string, arguments map[string]any) map[string]any {
	tool, ok := s.tools[toolName]
	if !ok {
		return map[string]any{
			"jsonrpc": "2.0",
			"id":      id,
			"error": map[string]any{
				"code":    -32601,
				"message": fmt.Sprintf("tool %q not found", toolName),
			},
		}
	}

	inputJSON, err := json.Marshal(arguments)
	if err != nil {
		return map[string]any{
			"jsonrpc": "2.0",
			"id":      id,
			"error": map[string]any{
				"code":    -32602,
				"message": fmt.Sprintf("encode tool arguments failed: %v", err),
			},
		}
	}

	log.Printf(
		"claude code runner handling MCP tool call: conversation=%s tool=%s input_bytes=%d",
		s.req.Conversation.Key,
		toolName,
		len(inputJSON),
	)

	toolCtx := ContextWithTurnRequest(ctx, s.req)
	result, err := tool.Call(toolCtx, inputJSON)
	content := make([]map[string]any, 0, 1)
	response := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
	}
	if err != nil {
		log.Printf(
			"claude code runner MCP tool failed: conversation=%s tool=%s err=%v",
			s.req.Conversation.Key,
			toolName,
			err,
		)
		content = append(content, map[string]any{
			"type": "text",
			"text": err.Error(),
		})
		response["result"] = map[string]any{
			"content": content,
			"isError": true,
		}
		return response
	}

	text, err := formatClaudeMCPToolResult(result)
	if err != nil {
		return map[string]any{
			"jsonrpc": "2.0",
			"id":      id,
			"error": map[string]any{
				"code":    -32603,
				"message": fmt.Sprintf("encode tool result failed: %v", err),
			},
		}
	}
	content = append(content, map[string]any{
		"type": "text",
		"text": text,
	})
	response["result"] = map[string]any{
		"content": content,
	}
	return response
}

func formatClaudeMCPToolResult(result any) (string, error) {
	if result == nil {
		return "null", nil
	}
	if text, ok := result.(string); ok {
		return text, nil
	}

	data, err := json.Marshal(result)
	if err != nil {
		return "", err
	}

	return string(data), nil
}

type claudeTurnState struct {
	handleRequest func(map[string]any) (map[string]any, error)
	control       *claudeControlServer
	initRespCh    chan error
	resultCh      chan *claudecode.ClaudeResult
	errCh         chan error
}

type claudePersistentProcessSession struct {
	conversationKey string
	runOptions      claudecode.RunOptions
	cmd             *exec.Cmd
	cancel          context.CancelFunc
	stdinPipe       io.WriteCloser
	waitCh          chan error
	exitDone        chan struct{}
	stderrDone      chan struct{}
	scanDone        chan struct{}

	writeMu sync.Mutex
	turnMu  sync.Mutex
	stateMu sync.Mutex

	currentTurn *claudeTurnState
	sessionID   string
	initialized bool
	waitErr     error
	closed      bool
}

func startClaudePersistentTurnSession(ctx context.Context, client *claudecode.ClaudeClient, conversationKey string, opts claudecode.RunOptions) (claudePersistentTurnSession, error) {
	if ctx == nil {
		return nil, errors.New("claude code session context is nil")
	}
	if client == nil {
		return nil, errors.New("claude code client is nil")
	}
	if err := claudecode.PreprocessOptions(&opts); err != nil {
		return nil, err
	}

	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	args := buildClaudeProcessArgs(&opts)
	log.Printf(
		"claude code runner spawning persistent process: conversation=%s bin=%s cwd=%s args=%q",
		conversationKey,
		client.BinPath,
		opts.WorkingDirectory,
		args,
	)

	//nolint:gosec // The Claude binary path is configured by the host process and arguments are generated from structured run options.
	cmd := exec.CommandContext(runCtx, client.BinPath, args...)
	if opts.WorkingDirectory != "" {
		cmd.Dir = opts.WorkingDirectory
	}
	cmd.Env = buildClaudeSDKEnv(opts.WorkingDirectory)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, claudecode.NewClaudeError(claudecode.ErrorCommand, fmt.Sprintf("failed to get stdin pipe: %v", err))
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		_ = stdinPipe.Close()
		return nil, claudecode.NewClaudeError(claudecode.ErrorCommand, fmt.Sprintf("failed to get stdout pipe: %v", err))
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		_ = stdinPipe.Close()
		return nil, claudecode.NewClaudeError(claudecode.ErrorCommand, fmt.Sprintf("failed to get stderr pipe: %v", err))
	}
	if err = cmd.Start(); err != nil {
		cancel()
		_ = stdinPipe.Close()
		return nil, claudecode.NewClaudeError(claudecode.ErrorCommand, fmt.Sprintf("failed to start command: %v", err))
	}
	log.Printf("claude code runner persistent process started: conversation=%s pid=%d", conversationKey, cmd.Process.Pid)

	session := &claudePersistentProcessSession{
		conversationKey: conversationKey,
		runOptions:      opts,
		cmd:             cmd,
		cancel:          cancel,
		stdinPipe:       stdinPipe,
		waitCh:          make(chan error, 1),
		exitDone:        make(chan struct{}),
		stderrDone:      make(chan struct{}),
		scanDone:        make(chan struct{}),
		sessionID:       strings.TrimSpace(opts.ResumeID),
	}

	go func() {
		session.waitCh <- cmd.Wait()
	}()

	go session.captureStderr(stderr)
	go session.scanStdout(stdout)
	go session.watchExit()

	return session, nil
}

func (s *claudePersistentProcessSession) CurrentSessionID() string {
	if s == nil {
		return ""
	}
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return s.sessionID
}

func (s *claudePersistentProcessSession) RunTurn(ctx context.Context, stdin io.Reader, control *claudeControlServer) (*claudecode.ClaudeResult, error) {
	if s == nil {
		return nil, errors.New("claude session is nil")
	}

	inputBytes, err := io.ReadAll(stdin)
	if err != nil {
		return nil, fmt.Errorf("read claude stdin failed: %w", err)
	}

	s.turnMu.Lock()
	defer s.turnMu.Unlock()

	return s.runTurn(ctx, inputBytes, control)
}

func (s *claudePersistentProcessSession) runTurn(ctx context.Context, inputBytes []byte, control *claudeControlServer) (*claudecode.ClaudeResult, error) {
	turn := &claudeTurnState{
		control:  control,
		resultCh: make(chan *claudecode.ClaudeResult, 1),
		errCh:    make(chan error, 1),
	}
	if control != nil {
		turn.handleRequest = func(request map[string]any) (map[string]any, error) {
			return control.handleRequest(ctx, request)
		}
	}
	if s.shouldInitialize(control) {
		turn.initRespCh = make(chan error, 1)
	}

	if err := s.setCurrentTurn(turn); err != nil {
		return nil, err
	}
	defer s.clearCurrentTurn(turn)

	if turn.initRespCh != nil {
		if err := s.writeJSONLine(map[string]any{
			"type":       "control_request",
			"request_id": "initialize",
			"request": map[string]any{
				"subtype": "initialize",
			},
		}); err != nil {
			return nil, claudecode.NewClaudeError(claudecode.ErrorCommand, fmt.Sprintf("failed to write initialize request: %v", err))
		}

		select {
		case err := <-turn.initRespCh:
			if err != nil {
				return nil, err
			}
		case err := <-turn.errCh:
			return nil, err
		case <-ctx.Done():
			_ = s.Close()
			return nil, ctx.Err()
		}
	}

	if len(inputBytes) > 0 {
		if err := s.writeRaw(inputBytes); err != nil {
			return nil, claudecode.NewClaudeError(claudecode.ErrorCommand, fmt.Sprintf("failed to write user input: %v", err))
		}
	}

	select {
	case result := <-turn.resultCh:
		return result, nil
	case err := <-turn.errCh:
		return nil, err
	case <-ctx.Done():
		_ = s.Close()
		return nil, ctx.Err()
	}
}

func (s *claudePersistentProcessSession) setCurrentTurn(turn *claudeTurnState) error {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	if s.closed {
		return errors.New("claude session is closed")
	}
	if s.waitErr != nil {
		return s.waitErr
	}
	s.currentTurn = turn
	return nil
}

func (s *claudePersistentProcessSession) shouldInitialize(control *claudeControlServer) bool {
	if s == nil || control == nil || !control.hasTools() {
		return false
	}

	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return !s.initialized
}

func (s *claudePersistentProcessSession) markInitialized() {
	if s == nil {
		return
	}

	s.stateMu.Lock()
	s.initialized = true
	s.stateMu.Unlock()
}

func (s *claudePersistentProcessSession) clearCurrentTurn(turn *claudeTurnState) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	if s.currentTurn == turn {
		s.currentTurn = nil
	}
}

func (s *claudePersistentProcessSession) writeRaw(data []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.stdinPipe.Write(data)
	return err
}

func (s *claudePersistentProcessSession) writeJSONLine(payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return s.writeRaw(append(data, '\n'))
}

func (s *claudePersistentProcessSession) captureStderr(stderr io.Reader) {
	defer close(s.stderrDone)
	stderrBuf := new(bytes.Buffer)
	_, _ = io.Copy(stderrBuf, stderr)
	stderrText := strings.TrimSpace(stderrBuf.String())
	if stderrText == "" {
		log.Printf("claude code runner stderr stream closed: conversation=%s pid=%d bytes=%d", s.conversationKey, s.cmd.Process.Pid, stderrBuf.Len())
		return
	}
	log.Printf(
		"claude code runner stderr stream closed: conversation=%s pid=%d bytes=%d stderr=%q",
		s.conversationKey,
		s.cmd.Process.Pid,
		stderrBuf.Len(),
		stderrText,
	)
}

func (s *claudePersistentProcessSession) scanStdout(stdout io.Reader) {
	defer close(s.scanDone)

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}

		var envelope map[string]any
		if err := json.Unmarshal([]byte(line), &envelope); err != nil {
			s.failCurrentTurn(claudecode.NewClaudeError(claudecode.ErrorValidation, fmt.Sprintf("failed to parse JSON message: %v", err)))
			_ = s.Close()
			return
		}

		switch envelope["type"] {
		case "control_response":
			s.handleControlResponse(envelope)
		case "control_request":
			s.handleControlRequest(envelope)
		default:
			s.handleStreamMessage(envelope, line)
		}
	}

	if err := scanner.Err(); err != nil && !errors.Is(err, context.Canceled) {
		s.failCurrentTurn(claudecode.NewClaudeError(claudecode.ErrorCommand, fmt.Sprintf("failed to scan stream output: %v", err)))
	}
}

func (s *claudePersistentProcessSession) handleControlResponse(envelope map[string]any) {
	turn := s.getCurrentTurn()
	if turn == nil || turn.initRespCh == nil {
		return
	}
	response, _ := envelope["response"].(map[string]any)
	requestID, _ := response["request_id"].(string)
	if requestID != "initialize" {
		return
	}
	subtype, _ := response["subtype"].(string)
	if subtype == "error" {
		errText := fmt.Sprintf("%v", response["error"])
		if strings.Contains(strings.ToLower(errText), "already initialized") {
			s.markInitialized()
			turn.initRespCh <- nil
			return
		}
		turn.initRespCh <- fmt.Errorf("initialize failed: %v", response["error"])
		return
	}
	s.markInitialized()
	turn.initRespCh <- nil
}

func (s *claudePersistentProcessSession) handleControlRequest(envelope map[string]any) {
	turn := s.getCurrentTurn()
	if turn == nil || turn.control == nil || turn.handleRequest == nil {
		s.failCurrentTurn(errors.New("received control request without active control handler"))
		_ = s.Close()
		return
	}

	requestID, _ := envelope["request_id"].(string)
	request, _ := envelope["request"].(map[string]any)
	subtype, _ := request["subtype"].(string)
	if subtype == "can_use_tool" || subtype == "mcp_message" {
		log.Printf(
			"claude code runner received control request: conversation=%s pid=%d request_id=%s subtype=%s",
			s.conversationKey,
			s.cmd.Process.Pid,
			requestID,
			subtype,
		)
	}

	response, err := turn.handleRequest(request)
	responseEnvelope := map[string]any{
		"request_id": requestID,
	}
	if err != nil {
		responseEnvelope["subtype"] = "error"
		responseEnvelope["error"] = err.Error()
	} else {
		responseEnvelope["subtype"] = "success"
		responseEnvelope["response"] = response
	}
	payload := map[string]any{
		"type":     "control_response",
		"response": responseEnvelope,
	}
	if err = s.writeJSONLine(payload); err != nil {
		s.failCurrentTurn(claudecode.NewClaudeError(claudecode.ErrorCommand, fmt.Sprintf("failed to write control response: %v", err)))
		_ = s.Close()
	}
}

func (s *claudePersistentProcessSession) handleStreamMessage(envelope map[string]any, line string) {
	var msg claudecode.Message
	if err := json.Unmarshal([]byte(line), &msg); err != nil {
		s.failCurrentTurn(claudecode.NewClaudeError(claudecode.ErrorValidation, fmt.Sprintf("failed to decode stream message: %v", err)))
		_ = s.Close()
		return
	}

	switch msg.Type {
	case "system":
		s.handleSystemMessage(&msg)
	case "assistant":
		s.handleAssistantMessage(&msg)
	case "user":
		s.handleUserMessage(&msg)
	case "result":
		s.handleResultMessage(envelope, &msg)
	case "control_cancel_request":
		s.handleControlCancelRequest(envelope)
	default:
		conversationKey := sanitizeLogValue(s.conversationKey)
		messageType := sanitizeLogValue(msg.Type)
		log.Printf(
			"claude code runner ignoring unsupported stream message: conversation=%s pid=%d type=%s",
			strconv.Quote(conversationKey),
			s.cmd.Process.Pid,
			strconv.Quote(messageType),
		)
	}
}

func (s *claudePersistentProcessSession) handleSystemMessage(msg *claudecode.Message) {
	if msg == nil {
		return
	}

	sessionID := strings.TrimSpace(msg.SessionID)
	if sessionID == "" {
		return
	}

	s.stateMu.Lock()
	s.sessionID = sessionID
	s.stateMu.Unlock()

	log.Printf(
		"claude code runner received system message: conversation=%s pid=%d session_id=%s",
		s.conversationKey,
		s.cmd.Process.Pid,
		sessionID,
	)
}

func (s *claudePersistentProcessSession) handleAssistantMessage(msg *claudecode.Message) {
	roleMessage, err := decodeClaudeStreamRoleMessage(msg)
	if err != nil {
		log.Printf(
			"claude code runner failed to decode assistant message: conversation=%s pid=%d err=%v",
			s.conversationKey,
			s.cmd.Process.Pid,
			err,
		)
		return
	}

	for _, block := range roleMessage.Content {
		switch block.Type {
		case "text":
			if strings.TrimSpace(block.Text) == "" {
				continue
			}
			log.Printf(
				"claude code runner received assistant text chunk: conversation=%s pid=%d chars=%d",
				s.conversationKey,
				s.cmd.Process.Pid,
				len(block.Text),
			)
		case "thinking":
			if strings.TrimSpace(block.Thinking) == "" {
				continue
			}
			log.Printf(
				"claude code runner received assistant thinking chunk: conversation=%s pid=%d chars=%d",
				s.conversationKey,
				s.cmd.Process.Pid,
				len(block.Thinking),
			)
		case "tool_use":
			log.Printf(
				"claude code runner received assistant tool use: conversation=%s pid=%d tool=%s input=%s",
				s.conversationKey,
				s.cmd.Process.Pid,
				block.Name,
				summarizeClaudeToolInput(block.Name, block.Input),
			)
		}
	}
}

func (s *claudePersistentProcessSession) handleUserMessage(msg *claudecode.Message) {
	roleMessage, err := decodeClaudeStreamRoleMessage(msg)
	if err != nil {
		log.Printf(
			"claude code runner failed to decode user message: conversation=%s pid=%d err=%v",
			s.conversationKey,
			s.cmd.Process.Pid,
			err,
		)
		return
	}

	for _, block := range roleMessage.Content {
		if block.Type != "tool_result" || !block.IsError {
			continue
		}
		log.Printf(
			"claude code runner received tool error result: conversation=%s pid=%d detail=%s",
			s.conversationKey,
			s.cmd.Process.Pid,
			summarizeClaudeToolResult(block.Content),
		)
	}
}

func (s *claudePersistentProcessSession) handleResultMessage(envelope map[string]any, msg *claudecode.Message) {
	if msg == nil {
		return
	}

	inputTokens, outputTokens := parseClaudeResultUsage(envelope)
	conversationKey := sanitizeLogValue(s.conversationKey)
	sessionID := sanitizeLogValue(msg.SessionID)

	log.Printf(
		"claude code runner received result message: conversation=%s pid=%d session_id=%s result_len=%d is_error=%t input_tokens=%d output_tokens=%d",
		strconv.Quote(conversationKey),
		s.cmd.Process.Pid,
		strconv.Quote(sessionID),
		len(msg.Result),
		msg.IsError,
		inputTokens,
		outputTokens,
	)
	result := &claudecode.ClaudeResult{
		Type:          msg.Type,
		Subtype:       msg.Subtype,
		Result:        msg.Result,
		CostUSD:       msg.CostUSD,
		DurationMS:    msg.DurationMS,
		DurationAPIMS: msg.DurationAPIMS,
		IsError:       msg.IsError,
		NumTurns:      msg.NumTurns,
		SessionID:     msg.SessionID,
	}

	s.stateMu.Lock()
	if strings.TrimSpace(result.SessionID) != "" {
		s.sessionID = strings.TrimSpace(result.SessionID)
	}
	turn := s.currentTurn
	s.stateMu.Unlock()
	if turn != nil {
		turn.resultCh <- result
	}
}

func (s *claudePersistentProcessSession) handleControlCancelRequest(envelope map[string]any) {
	conversationKey := sanitizeLogValue(s.conversationKey)
	requestID, _ := envelope["request_id"].(string)
	requestID = sanitizeLogValue(requestID)
	log.Printf(
		"claude code runner received control cancel request: conversation=%s pid=%d request_id=%s",
		strconv.Quote(conversationKey),
		s.cmd.Process.Pid,
		strconv.Quote(requestID),
	)
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

type claudeStreamRoleMessage struct {
	Role    string                     `json:"role"`
	Content []claudeStreamContentBlock `json:"content"`
}

type claudeStreamContentBlock struct {
	Type     string         `json:"type"`
	Name     string         `json:"name,omitempty"`
	Text     string         `json:"text,omitempty"`
	Thinking string         `json:"thinking,omitempty"`
	Input    map[string]any `json:"input,omitempty"`
	IsError  bool           `json:"is_error,omitempty"`
	Content  any            `json:"content,omitempty"`
}

func decodeClaudeStreamRoleMessage(msg *claudecode.Message) (claudeStreamRoleMessage, error) {
	if msg == nil || len(msg.Message) == 0 {
		return claudeStreamRoleMessage{}, nil
	}

	var roleMessage claudeStreamRoleMessage
	err := json.Unmarshal(msg.Message, &roleMessage)
	if err != nil {
		return claudeStreamRoleMessage{}, err
	}

	return roleMessage, nil
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

func parseClaudeResultUsage(envelope map[string]any) (int, int) {
	usage, _ := envelope["usage"].(map[string]any)
	if usage == nil {
		return 0, 0
	}

	return parseClaudeUsageInt(usage["input_tokens"]), parseClaudeUsageInt(usage["output_tokens"])
}

func parseClaudeUsageInt(value any) int {
	switch typed := value.(type) {
	case float64:
		return int(typed)
	case int:
		return typed
	case int64:
		return int(typed)
	case json.Number:
		number, err := typed.Int64()
		if err != nil {
			return 0
		}
		return int(number)
	default:
		return 0
	}
}

func (s *claudePersistentProcessSession) watchExit() {
	err := awaitClaudeProcess(s.waitCh)
	s.stateMu.Lock()
	closed := s.closed
	if s.waitErr == nil {
		if err == nil && !closed {
			err = errors.New("claude code process exited")
		}
		s.waitErr = err
	}
	turn := s.currentTurn
	s.stateMu.Unlock()
	close(s.exitDone)

	log.Printf("claude code runner persistent process wait returned: conversation=%s pid=%d err=%v", s.conversationKey, s.cmd.Process.Pid, err)
	if turn != nil && err != nil {
		turn.errCh <- err
	}
}

func (s *claudePersistentProcessSession) getCurrentTurn() *claudeTurnState {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return s.currentTurn
}

func (s *claudePersistentProcessSession) failCurrentTurn(err error) {
	s.stateMu.Lock()
	turn := s.currentTurn
	s.stateMu.Unlock()
	if turn != nil {
		select {
		case turn.errCh <- err:
		default:
		}
	}
}

func (s *claudePersistentProcessSession) Close() error {
	if s == nil {
		return nil
	}

	s.stateMu.Lock()
	if s.closed {
		s.stateMu.Unlock()
		return nil
	}
	s.closed = true
	s.stateMu.Unlock()

	if s.stdinPipe != nil {
		_ = s.stdinPipe.Close()
	}
	if s.cancel != nil {
		s.cancel()
	}
	if err := signalClaudeProcessGroup(s.cmd, syscall.SIGTERM); err != nil {
		log.Printf("claude code runner terminate persistent process failed: conversation=%s err=%v", s.conversationKey, err)
	}
	if !s.waitForExit(claudeCodeTerminateTimeout) {
		if err := signalClaudeProcessGroup(s.cmd, syscall.SIGKILL); err != nil {
			log.Printf("claude code runner kill persistent process failed: conversation=%s err=%v", s.conversationKey, err)
		}
	}
	<-s.exitDone
	s.stateMu.Lock()
	err := s.waitErr
	s.stateMu.Unlock()
	<-s.stderrDone
	<-s.scanDone
	return err
}

func (s *claudePersistentProcessSession) waitForExit(timeout time.Duration) bool {
	if timeout <= 0 {
		<-s.exitDone
		return true
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-s.exitDone:
		return true
	case <-timer.C:
		return false
	}
}

func writeClaudeArgumentTempFile(name string, content string) (string, error) {
	if strings.TrimSpace(content) == "" {
		return "", nil
	}

	file, err := os.CreateTemp("", "assistant-claude-"+name+"-*.txt")
	if err != nil {
		return "", fmt.Errorf("create claude %s temp file failed: %w", name, err)
	}
	path := file.Name()
	_, err = file.WriteString(content)
	if closeErr := file.Close(); closeErr != nil && err == nil {
		err = closeErr
	}
	if err != nil {
		removeClaudeTempFile(path)
		return "", fmt.Errorf("write claude %s temp file failed: %w", name, err)
	}

	return path, nil
}

func replaceClaudeArgWithFile(args []string, inlineFlag string, fileFlag string, filePath string) []string {
	for index := 0; index < len(args)-1; index++ {
		if args[index] != inlineFlag {
			continue
		}

		replaced := make([]string, 0, len(args))
		replaced = append(replaced, args[:index]...)
		replaced = append(replaced, fileFlag, filePath)
		replaced = append(replaced, args[index+2:]...)
		return replaced
	}

	return args
}

func removeClaudeTempFile(path string) {
	_ = os.Remove(path)
}

func buildClaudeProcessArgs(opts *claudecode.RunOptions) []string {
	args := buildClaudeSDKArgs(opts)
	if opts == nil {
		return args
	}
	if opts.SystemPrompt != "" {
		args = replaceClaudeArgWithFile(args, "--system-prompt", "--system-prompt-file", opts.SystemPrompt)
	}
	if opts.AppendPrompt != "" {
		args = replaceClaudeArgWithFile(args, "--append-system-prompt", "--append-system-prompt-file", opts.AppendPrompt)
	}
	return args
}

func buildClaudeSDKArgs(opts *claudecode.RunOptions) []string {
	args := []string{"--output-format", string(claudecode.StreamJSONOutput), "--verbose"}
	if opts == nil {
		return args
	}

	if opts.SystemPrompt != "" {
		args = append(args, "--system-prompt", opts.SystemPrompt)
	}
	if opts.AppendPrompt != "" {
		args = append(args, "--append-system-prompt", opts.AppendPrompt)
	}
	if len(opts.AllowedTools) > 0 {
		args = append(args, "--allowedTools", strings.Join(opts.AllowedTools, ","))
	}
	if len(opts.DisallowedTools) > 0 {
		args = append(args, "--disallowedTools", strings.Join(opts.DisallowedTools, ","))
	}
	if opts.PermissionMode == claudecode.PermissionModeDefault {
		args = append(args, "--permission-prompt-tool", "stdio")
	}
	if opts.PermissionMode != "" {
		args = append(args, "--permission-mode", string(opts.PermissionMode))
	}
	if opts.ResumeID != "" {
		args = append(args, "--resume", opts.ResumeID)
	} else if opts.Continue {
		args = append(args, "--continue")
	}
	if opts.SessionID != "" {
		args = append(args, "--session-id", opts.SessionID)
	}
	if opts.ForkSession {
		args = append(args, "--fork-session")
	}
	if opts.ModelAlias != "" {
		args = append(args, "--model", opts.ModelAlias)
	} else if opts.Model != "" {
		args = append(args, "--model", opts.Model)
	}
	if opts.Effort != "" {
		args = append(args, "--effort", string(opts.Effort))
	}
	if opts.Settings != "" {
		args = append(args, "--settings", opts.Settings)
	}
	for _, dir := range opts.AddDirectories {
		args = append(args, "--add-dir", dir)
	}
	if len(opts.MCPConfigs) > 0 {
		args = append(args, "--mcp-config")
		args = append(args, opts.MCPConfigs...)
	}
	if opts.StrictMCPConfig {
		args = append(args, "--strict-mcp-config")
	}
	if len(opts.SettingSources) > 0 {
		args = append(args, "--setting-sources", strings.Join(opts.SettingSources, ","))
	}
	if len(opts.Tools) > 0 {
		args = append(args, "--tools", strings.Join(opts.Tools, ","))
	}
	if opts.InputFormat != "" {
		args = append(args, "--input-format", string(opts.InputFormat))
	}
	if opts.IncludeHookEvents {
		args = append(args, "--include-hook-events")
	}
	if opts.IncludePartialMessages {
		args = append(args, "--include-partial-messages")
	}
	if opts.ReplayUserMessages {
		args = append(args, "--replay-user-messages")
	}
	if opts.DebugFile != "" {
		args = append(args, "--debug-file", opts.DebugFile)
	}
	if opts.Bare {
		args = append(args, "--bare")
	}
	if opts.Brief {
		args = append(args, "--brief")
	}
	if len(opts.Betas) > 0 {
		args = append(args, "--betas", strings.Join(opts.Betas, ","))
	}
	if len(opts.Files) > 0 {
		args = append(args, "--file")
		args = append(args, opts.Files...)
	}
	if opts.ExcludeDynamicSystemPromptSections {
		args = append(args, "--exclude-dynamic-system-prompt-sections")
	}
	if opts.MaxBudgetUSD > 0 {
		args = append(args, "--max-budget-usd", fmt.Sprintf("%g", opts.MaxBudgetUSD))
	}
	if opts.NoSessionPersistence {
		args = append(args, "--no-session-persistence")
	}

	return args
}

func buildClaudeSDKEnv(workingDirectory string) []string {
	env := make([]string, 0, len(os.Environ())+2)
	for _, item := range os.Environ() {
		if strings.HasPrefix(item, "CLAUDECODE=") {
			continue
		}
		env = append(env, item)
	}
	env = append(env, "CLAUDE_CODE_ENTRYPOINT="+claudeSDKEntrypoint)
	env = append(env, "CLAUDE_AGENT_SDK_VERSION="+claudeAgentSDKVersion)
	if workingDirectory != "" {
		env = append(env, "PWD="+workingDirectory)
	}
	return env
}

func awaitClaudeProcess(waitCh <-chan error) error {
	if waitCh == nil {
		return nil
	}

	return <-waitCh
}

func signalClaudeProcessGroup(cmd *exec.Cmd, signal syscall.Signal) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}

	err := syscall.Kill(-cmd.Process.Pid, signal)
	if err == nil || errors.Is(err, os.ErrProcessDone) || errors.Is(err, syscall.ESRCH) {
		return nil
	}

	return err
}
