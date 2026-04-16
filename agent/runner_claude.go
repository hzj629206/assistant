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

// ClaudeCodeRunner bridges dispatcher turns to the Claude Code CLI through claude-code-go.
// Each turn starts a new Claude Code process. Conversation continuity is preserved by resuming
// the stored Claude session ID when one exists.
type ClaudeCodeRunner struct {
	client        *claudecode.ClaudeClient
	executeTurn   func(context.Context, io.Reader, *claudecode.RunOptions, *claudeControlServer) (*claudecode.ClaudeResult, error)
	retryPolicy   *claudecode.RetryPolicy
	runOptions    claudecode.RunOptions
	mu            sync.RWMutex
	systemPrompts []string
	tools         []Tool
}

// ClaudeCodeRunnerOptions configures a ClaudeCodeRunner.
type ClaudeCodeRunnerOptions struct {
	Client       *claudecode.ClaudeClient
	BinaryPath   string
	RunOptions   claudecode.RunOptions
	RetryPolicy  *claudecode.RetryPolicy
	SystemPrompt string
	Tools        []Tool
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
		runOptions.PermissionMode = claudecode.PermissionModeDefault
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
		client:      client,
		executeTurn: newClaudeCodeTurnExecutor(client),
		retryPolicy: cloneClaudeRetryPolicy(retryPolicy),
		runOptions:  runOptions,
	}

	runner.RegisterSystemPrompt(options.SystemPrompt)
	runner.RegisterTools(options.Tools...)

	return runner
}

func newClaudeCodeTurnExecutor(client *claudecode.ClaudeClient) func(context.Context, io.Reader, *claudecode.RunOptions, *claudeControlServer) (*claudecode.ClaudeResult, error) {
	return func(ctx context.Context, stdin io.Reader, opts *claudecode.RunOptions, control *claudeControlServer) (*claudecode.ClaudeResult, error) {
		return executeClaudeCodeProcess(ctx, client, stdin, opts, control)
	}
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
	if r.executeTurn == nil {
		return nil, errors.New("claude code client is nil")
	}

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
	control := newClaudeControlServer(req, tools)
	if control.hasTools() {
		configJSON, err := control.mcpConfigJSON()
		if err != nil {
			return nil, err
		}
		runOptions.MCPConfigs = append(append([]string(nil), runOptions.MCPConfigs...), configJSON)
	}

	inputReader, err := buildClaudeStreamJSONInput(prompt, imagePaths)
	if err != nil {
		return nil, err
	}

	log.Printf(
		"claude code runner starting process: conversation=%s resume_session=%s prompt_len=%d image_count=%d",
		req.Conversation.Key,
		sessionID,
		len(prompt),
		len(imagePaths),
	)
	result, err := r.streamClaudeTurnWithRetry(ctx, req, inputReader, &runOptions, control)
	if err != nil {
		return nil, err
	}

	log.Printf(
		"claude code runner completed process: conversation=%s resume_session=%s session_id=%s result_len=%d",
		req.Conversation.Key,
		sessionID,
		result.SessionID,
		len(result.Result),
	)

	return result, nil
}

func (r *ClaudeCodeRunner) streamClaudeTurnWithRetry(ctx context.Context, req TurnRequest, inputReader io.Reader, runOptions *claudecode.RunOptions, control *claudeControlServer) (*claudecode.ClaudeResult, error) {
	retryPolicy := cloneClaudeRetryPolicy(r.retryPolicy)
	if retryPolicy == nil {
		return r.executeTurn(ctx, inputReader, runOptions, control)
	}

	inputBytes, err := io.ReadAll(inputReader)
	if err != nil {
		return nil, fmt.Errorf("read claude stream input failed: %w", err)
	}

	var lastErr error
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

		result, err := r.executeTurn(ctx, bytes.NewReader(inputBytes), runOptions, control)
		if err == nil {
			return result, nil
		}

		lastErr = err
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

func executeClaudeCodeProcess(ctx context.Context, client *claudecode.ClaudeClient, stdin io.Reader, opts *claudecode.RunOptions, control *claudeControlServer) (*claudecode.ClaudeResult, error) {
	if client == nil {
		return nil, errors.New("claude code client is nil")
	}

	runOptions := claudecode.RunOptions{}
	if opts != nil {
		runOptions = *opts
	}
	if err := claudecode.PreprocessOptions(&runOptions); err != nil {
		return nil, err
	}

	runCtx := ctx
	var cancel context.CancelFunc
	if runOptions.Timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, runOptions.Timeout)
	} else {
		runCtx, cancel = context.WithCancel(ctx)
	}
	defer cancel()

	args := buildClaudeSDKArgs(&runOptions)
	log.Printf(
		"claude code runner spawning process: bin=%s cwd=%s args=%q has_control=%t",
		client.BinPath,
		runOptions.WorkingDirectory,
		args,
		control != nil && control.hasTools(),
	)
	//nolint:gosec // The Claude binary path is configured by the host process and arguments are generated from structured run options.
	cmd := exec.CommandContext(runCtx, client.BinPath, args...)
	if runOptions.WorkingDirectory != "" {
		cmd.Dir = runOptions.WorkingDirectory
	}
	cmd.Env = buildClaudeSDKEnv(runOptions.WorkingDirectory)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		return nil, claudecode.NewClaudeError(claudecode.ErrorCommand, fmt.Sprintf("failed to get stdin pipe: %v", err))
	}
	defer func() {
		_ = stdinPipe.Close()
	}()

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, claudecode.NewClaudeError(claudecode.ErrorCommand, fmt.Sprintf("failed to get stdout pipe: %v", err))
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, claudecode.NewClaudeError(claudecode.ErrorCommand, fmt.Sprintf("failed to get stderr pipe: %v", err))
	}

	stderrBuf := new(bytes.Buffer)
	if err = cmd.Start(); err != nil {
		return nil, claudecode.NewClaudeError(claudecode.ErrorCommand, fmt.Sprintf("failed to start command: %v", err))
	}
	log.Printf("claude code runner process started: pid=%d", cmd.Process.Pid)
	waitCh := make(chan error, 1)
	go func() {
		waitCh <- cmd.Wait()
	}()

	stderrDone := make(chan struct{})
	go func() {
		defer close(stderrDone)
		_, _ = io.Copy(stderrBuf, stderr)
		stderrText := strings.TrimSpace(stderrBuf.String())
		if stderrText == "" {
			log.Printf("claude code runner stderr stream closed: pid=%d bytes=%d", cmd.Process.Pid, stderrBuf.Len())
			return
		}
		log.Printf("claude code runner stderr stream closed: pid=%d bytes=%d stderr=%q", cmd.Process.Pid, stderrBuf.Len(), stderrText)
	}()

	var (
		result      *claudecode.ClaudeResult
		scanErr     error
		writeMu     sync.Mutex
		initRespCh  chan error
		initRespMu  sync.Mutex
		initDone    bool
		stdinMu     sync.Mutex
		stdinClosed bool
	)
	if control != nil {
		initRespCh = make(chan error, 1)
	}

	completeInitialize := func(err error) {
		if initRespCh == nil {
			return
		}

		initRespMu.Lock()
		defer initRespMu.Unlock()
		if initDone {
			return
		}
		initDone = true
		initRespCh <- err
	}

	closeStdin := func(reason string) {
		stdinMu.Lock()
		defer stdinMu.Unlock()
		if stdinClosed {
			return
		}
		err := stdinPipe.Close()
		if err != nil {
			log.Printf("claude code runner stdin close failed: pid=%d reason=%s err=%v", cmd.Process.Pid, reason, err)
			return
		}
		stdinClosed = true
		log.Printf("claude code runner stdin closed: pid=%d reason=%s", cmd.Process.Pid, reason)
	}

	writeJSONLine := func(payload any) error {
		data, err := json.Marshal(payload)
		if err != nil {
			return err
		}

		writeMu.Lock()
		defer writeMu.Unlock()
		if _, err = stdinPipe.Write(append(data, '\n')); err != nil {
			return err
		}
		return nil
	}

	scanDone := make(chan struct{})
	go func() {
		defer close(scanDone)

		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.TrimSpace(line) == "" {
				continue
			}

			var envelope map[string]any
			if err := json.Unmarshal([]byte(line), &envelope); err != nil {
				scanErr = claudecode.NewClaudeError(claudecode.ErrorValidation, fmt.Sprintf("failed to parse JSON message: %v", err))
				cancel()
				return
			}

			switch envelope["type"] {
			case "control_response":
				if initRespCh == nil {
					continue
				}
				response, _ := envelope["response"].(map[string]any)
				requestID, _ := response["request_id"].(string)
				if requestID != "initialize" {
					continue
				}
				subtype, _ := response["subtype"].(string)
				if subtype == "error" {
					completeInitialize(fmt.Errorf("initialize failed: %v", response["error"]))
				} else {
					completeInitialize(nil)
				}
			case "control_request":
				if control == nil {
					scanErr = errors.New("received control request without control handler")
					cancel()
					return
				}
				requestID, _ := envelope["request_id"].(string)
				request, _ := envelope["request"].(map[string]any)
				subtype, _ := request["subtype"].(string)
				if subtype == "can_use_tool" || subtype == "mcp_message" {
					log.Printf(
						"claude code runner received control request: pid=%d request_id=%s subtype=%s",
						cmd.Process.Pid,
						requestID,
						subtype,
					)
				}
				response, err := control.handleRequest(runCtx, request)
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
				if err = writeJSONLine(payload); err != nil {
					scanErr = claudecode.NewClaudeError(claudecode.ErrorCommand, fmt.Sprintf("failed to write control response: %v", err))
					cancel()
					return
				}
			default:
				var msg claudecode.Message
				if err := json.Unmarshal([]byte(line), &msg); err != nil {
					scanErr = claudecode.NewClaudeError(claudecode.ErrorValidation, fmt.Sprintf("failed to decode stream message: %v", err))
					cancel()
					return
				}
				if msg.Type != "result" {
					continue
				}
				log.Printf(
					"claude code runner received result message: pid=%d session_id=%s result_len=%d is_error=%t",
					cmd.Process.Pid,
					msg.SessionID,
					len(msg.Result),
					msg.IsError,
				)
				result = &claudecode.ClaudeResult{
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
				closeStdin("result-received")
			}
		}

		completeInitialize(errors.New("claude process exited before initialize response"))
		if err := scanner.Err(); err != nil && !errors.Is(err, context.Canceled) {
			scanErr = claudecode.NewClaudeError(claudecode.ErrorCommand, fmt.Sprintf("failed to scan stream output: %v", err))
			cancel()
		}
	}()

	if initRespCh != nil {
		if err = writeJSONLine(map[string]any{
			"type":       "control_request",
			"request_id": "initialize",
			"request": map[string]any{
				"subtype": "initialize",
			},
		}); err != nil {
			cancel()
			_ = awaitClaudeProcess(waitCh)
			<-stderrDone
			<-scanDone
			return nil, claudecode.NewClaudeError(claudecode.ErrorCommand, fmt.Sprintf("failed to write initialize request: %v", err))
		}

		select {
		case err = <-initRespCh:
			if err != nil {
				cancel()
				_ = awaitClaudeProcess(waitCh)
				<-stderrDone
				<-scanDone
				return nil, err
			}
		case <-runCtx.Done():
			cancel()
			_ = awaitClaudeProcess(waitCh)
			<-stderrDone
			<-scanDone
			return nil, runCtx.Err()
		}
	}

	if stdin != nil {
		inputBytes, err := io.ReadAll(stdin)
		if err != nil {
			cancel()
			_ = awaitClaudeProcess(waitCh)
			<-stderrDone
			<-scanDone
			return nil, fmt.Errorf("read claude stdin failed: %w", err)
		}
		if len(inputBytes) > 0 {
			writeMu.Lock()
			_, err = stdinPipe.Write(inputBytes)
			writeMu.Unlock()
			if err != nil {
				cancel()
				_ = awaitClaudeProcess(waitCh)
				<-stderrDone
				<-scanDone
				return nil, claudecode.NewClaudeError(claudecode.ErrorCommand, fmt.Sprintf("failed to write user input: %v", err))
			}
		}
	}
	if control == nil || !control.hasTools() {
		closeStdin("input-finished-no-control")
	}

	go func() {
		<-runCtx.Done()
		closeStdin("context-done")
		if err := signalClaudeProcessGroup(cmd, syscall.SIGTERM); err != nil {
			log.Printf("claude code runner terminate process group failed: pid=%d err=%v", cmd.Process.Pid, err)
			return
		}
		if waitClaudeProcess(waitCh, claudeCodeTerminateTimeout) {
			return
		}
		if err := signalClaudeProcessGroup(cmd, syscall.SIGKILL); err != nil {
			log.Printf("claude code runner kill process group failed: pid=%d err=%v", cmd.Process.Pid, err)
		}
	}()

	err = awaitClaudeProcess(waitCh)
	<-stderrDone
	<-scanDone
	log.Printf("claude code runner process wait returned: pid=%d err=%v", cmd.Process.Pid, err)
	if scanErr != nil {
		return nil, scanErr
	}
	if err != nil {
		exitCode := 1
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		}
		claudeErr := claudecode.ParseError(stderrBuf.String(), exitCode)
		claudeErr.Original = err
		return nil, claudeErr
	}
	if result == nil {
		return nil, errors.New("claude code stream finished without a result message")
	}

	return result, nil
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

func waitClaudeProcess(waitCh <-chan error, timeout time.Duration) bool {
	if waitCh == nil {
		return true
	}

	select {
	case <-waitCh:
		return true
	case <-time.After(timeout):
		return false
	}
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
