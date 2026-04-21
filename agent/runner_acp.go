package agent

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	defaultACPToolServerName     = "assistant"
	defaultACPProtocolVersion    = 1
	defaultACPReadLoopErrorCode  = -32000
	defaultACPClientName         = "assistant"
	defaultACPClientVersion      = "1.0.0"
	defaultACPSessionIdleTimeout = 10 * time.Minute
	defaultHTTPReadHeaderTimeout = 5 * time.Second
	defaultACPEmbeddedContextMax = 256 * 1024
	acpProcessTerminateTimeout   = 5 * time.Second
)

// ACPRunner bridges dispatcher turns to an ACP-compatible local agent process.
// Global tools are exposed by creating a temporary local streamable HTTP MCP server for each turn.
type ACPRunner struct {
	command            string
	args               []string
	env                []string
	authMethod         string
	workDir            string
	sessionIdleTimeout time.Duration
	sessionFactory     func(context.Context, acpSessionOptions) (acpSession, error)
	mu                 sync.RWMutex
	systemPrompts      []string
	tools              []Tool
	sessionsMu         sync.Mutex
	sessions           map[string]*acpRunnerSession
	activeTurns        map[string]TurnRequest
	toolServer         *acpToolHTTPServer
	closed             bool
}

// ACPRunnerOptions configures an ACPRunner.
type ACPRunnerOptions struct {
	Command            string
	Args               []string
	Env                []string
	AuthMethod         string
	WorkingDir         string
	SessionIdleTimeout time.Duration
	SystemPrompt       string
	Tools              []Tool
}

type acpRunnerSession struct {
	session   acpSession
	token     string
	idleTimer *time.Timer
	inUse     bool
}

type acpSession interface {
	RunPrompt(ctx context.Context, prompt []acpContentBlock) (string, error)
	CurrentSessionID() string
	AgentCapabilities() acpAgentCapabilities
	Close() error
}

type acpSessionOptions struct {
	Command         string
	Args            []string
	Env             []string
	WorkingDir      string
	ResumeSessionID string
	AuthMethod      string
	MCPServers      []acpMCPServer
}

type acpMCPServer struct {
	Name    string         `json:"name"`
	Type    string         `json:"type,omitempty"`
	Command string         `json:"command,omitempty"`
	Args    []string       `json:"args,omitempty"`
	Env     []acpMCPEnvVar `json:"env,omitempty"`
	URL     string         `json:"url,omitempty"`
	Headers []acpMCPHeader `json:"headers,omitempty"`
	Meta    map[string]any `json:"-"`
}

type acpMCPEnvVar struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type acpMCPHeader struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type acpContentBlock map[string]any

type acpAgentCapabilities struct {
	Prompt acpPromptCapabilities
	MCP    acpMCPCapabilities
}

type acpPromptCapabilities struct {
	Image           bool
	EmbeddedContext bool
}

type acpMCPCapabilities struct {
	HTTP bool
}

type acpAttachmentKind string

const (
	acpAttachmentImage acpAttachmentKind = "image"
	acpAttachmentFile  acpAttachmentKind = "file"
	acpAttachmentVideo acpAttachmentKind = "video"
)

type acpAttachmentRef struct {
	Kind acpAttachmentKind
	Path string
}

type acpSessionMode struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

type acpSessionModes struct {
	CurrentModeID  string           `json:"currentModeId"`
	AvailableModes []acpSessionMode `json:"availableModes"`
}

// NewACPRunner builds a runner backed by an ACP agent CLI.
func NewACPRunner(options ACPRunnerOptions) *ACPRunner {
	command := strings.TrimSpace(options.Command)

	workDir := strings.TrimSpace(options.WorkingDir)
	if workDir == "" {
		workingDirectory, err := os.Getwd()
		if err == nil {
			workDir = workingDirectory
		}
	}

	runner := &ACPRunner{
		command:            command,
		args:               append([]string(nil), options.Args...),
		env:                append([]string(nil), options.Env...),
		authMethod:         strings.TrimSpace(options.AuthMethod),
		workDir:            workDir,
		sessionIdleTimeout: options.SessionIdleTimeout,
		sessions:           make(map[string]*acpRunnerSession),
		activeTurns:        make(map[string]TurnRequest),
	}
	if runner.sessionIdleTimeout <= 0 {
		runner.sessionIdleTimeout = defaultACPSessionIdleTimeout
	}
	runner.sessionFactory = func(ctx context.Context, sessionOptions acpSessionOptions) (acpSession, error) {
		return newACPProcessSession(ctx, sessionOptions)
	}

	runner.RegisterSystemPrompt(options.SystemPrompt)
	runner.RegisterTools(options.Tools...)

	return runner
}

// Close shuts down managed ACP sessions and the shared MCP tool server.
func (r *ACPRunner) Close() error {
	if r == nil {
		return nil
	}

	r.sessionsMu.Lock()
	if r.closed {
		r.sessionsMu.Unlock()
		return nil
	}
	r.closed = true
	sessions := make([]*acpRunnerSession, 0, len(r.sessions))
	for key, session := range r.sessions {
		if session.idleTimer != nil {
			session.idleTimer.Stop()
			session.idleTimer = nil
		}
		sessions = append(sessions, session)
		delete(r.sessions, key)
	}
	clear(r.activeTurns)
	toolServer := r.toolServer
	r.toolServer = nil
	r.sessionsMu.Unlock()

	var closeErr error
	for _, session := range sessions {
		closeErr = errors.Join(closeErr, session.session.Close())
	}
	if toolServer != nil {
		closeErr = errors.Join(closeErr, toolServer.Close())
	}
	return closeErr
}

// RegisterSystemPrompt appends one global system prompt block for new conversations.
func (r *ACPRunner) RegisterSystemPrompt(prompt string) {
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
func (r *ACPRunner) RegisterTools(tools ...Tool) {
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

func (r *ACPRunner) globalContext() ([]string, []Tool) {
	if r == nil {
		return nil, nil
	}

	r.mu.RLock()
	defer r.mu.RUnlock()

	return append([]string(nil), r.systemPrompts...), append([]Tool(nil), r.tools...)
}

// RunTurn runs one ACP turn and returns the updated ACP session ID and final reply text.
func (r *ACPRunner) RunTurn(ctx context.Context, req TurnRequest) (TurnResult, error) {
	if r == nil {
		return TurnResult{}, errors.New("run acp turn failed: runner is nil")
	}

	prompts, tools := r.globalContext()

	session, err := r.acquireACPSession(ctx, req.Conversation.Key, req.Conversation.RunnerThreadID, len(tools) != 0)
	if err != nil {
		return TurnResult{}, fmt.Errorf("run acp turn failed: acquire session: %w", err)
	}
	defer r.releaseACPSession(req.Conversation.Key, session)
	initialPromptBlocks := []string(nil)
	if req.Conversation.RunnerThreadID == "" {
		initialPromptBlocks = prompts
	}
	promptBlocks, err := buildACPPromptBlocks(initialPromptBlocks, req.Message, session.session.AgentCapabilities().Prompt)
	if err != nil {
		r.discardACPSession(req.Conversation.Key, session, err)
		return TurnResult{}, fmt.Errorf("run acp turn failed: build prompt blocks: %w", err)
	}
	r.setActiveTurn(session.token, req)
	defer r.clearActiveTurn(session.token)

	log.Printf(
		"acp runner session ready: conversation=%s requested_session=%s actual_session=%s tool_count=%d",
		req.Conversation.Key,
		req.Conversation.RunnerThreadID,
		session.session.CurrentSessionID(),
		len(tools),
	)

	typing := newTypingStatusController(
		req.Message.Responder,
		defaultTypingInitialDelay,
		defaultTypingRefreshCooldown,
	)
	typing.Start(ctx)
	defer typing.Stop()

	replyText, err := session.session.RunPrompt(ctx, promptBlocks)
	if err != nil {
		r.discardACPSession(req.Conversation.Key, session, err)
		return TurnResult{}, fmt.Errorf("run acp turn failed: %w", err)
	}

	return TurnResult{
		RunnerThreadID: session.session.CurrentSessionID(),
		ReplyText:      replyText,
	}, nil
}

func (r *ACPRunner) acquireACPSession(ctx context.Context, conversationKey, sessionID string, needsTools bool) (*acpRunnerSession, error) {
	if r == nil {
		return nil, errors.New("acp runner is nil")
	}

	r.sessionsMu.Lock()
	if r.closed {
		r.sessionsMu.Unlock()
		return nil, errors.New("acp runner is closed")
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

	sessionOptions := acpSessionOptions{
		Command:         r.command,
		Args:            append([]string(nil), r.args...),
		Env:             append([]string(nil), r.env...),
		WorkingDir:      r.workDir,
		ResumeSessionID: sessionID,
		AuthMethod:      r.authMethod,
	}

	token, err := randomACPToken()
	if err != nil {
		return nil, err
	}
	if needsTools {
		toolServer, err := r.ensureACPToolServer(ctx)
		if err != nil {
			return nil, err
		}
		sessionOptions.MCPServers = []acpMCPServer{toolServer.ServerConfig(token)}
	}

	if r.sessionFactory == nil {
		return nil, errors.New("acp session factory is nil")
	}
	session, err := r.sessionFactory(ctx, sessionOptions)
	if err != nil {
		return nil, err
	}
	if needsTools && !session.AgentCapabilities().MCP.HTTP {
		_ = session.Close()
		return nil, errors.New("acp agent does not advertise HTTP MCP support")
	}
	managed := &acpRunnerSession{
		session: session,
		token:   token,
		inUse:   true,
	}

	r.sessionsMu.Lock()
	defer r.sessionsMu.Unlock()
	if r.closed {
		_ = session.Close()
		return nil, errors.New("acp runner is closed")
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

func (r *ACPRunner) releaseACPSession(conversationKey string, managed *acpRunnerSession) {
	if r == nil || managed == nil {
		return
	}
	if r.sessionIdleTimeout <= 0 {
		r.discardACPSession(conversationKey, managed, nil)
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
		log.Printf("acp runner expiring idle session: conversation=%s idle_timeout=%s", conversationKey, r.sessionIdleTimeout)
		r.discardACPSession(conversationKey, managed, nil)
	})
}

func (r *ACPRunner) discardACPSession(conversationKey string, managed *acpRunnerSession, reason error) {
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
	delete(r.activeTurns, managed.token)
	r.sessionsMu.Unlock()

	if reason != nil {
		log.Printf("acp runner closing session process: conversation=%s err=%v", conversationKey, reason)
	}
	_ = managed.session.Close()
}

func (r *ACPRunner) ensureACPToolServer(ctx context.Context) (*acpToolHTTPServer, error) {
	r.sessionsMu.Lock()
	defer r.sessionsMu.Unlock()
	if r.toolServer != nil {
		return r.toolServer, nil
	}

	server, err := newACPToolHTTPServer(ctx, acpToolHTTPServerOptions{
		IsAuthorized: func(token string) bool {
			return r.isAuthorizedACPToken(token)
		},
		ResolveTurnRequest: func(token string) (TurnRequest, bool) {
			return r.activeTurnForToken(token)
		},
		Tools: func() []Tool {
			_, tools := r.globalContext()
			return tools
		},
	})
	if err != nil {
		return nil, err
	}
	r.toolServer = server
	return server, nil
}

func (r *ACPRunner) isAuthorizedACPToken(token string) bool {
	r.sessionsMu.Lock()
	defer r.sessionsMu.Unlock()
	for _, managed := range r.sessions {
		if managed.token == token {
			return true
		}
	}
	return false
}

func (r *ACPRunner) activeTurnForToken(token string) (TurnRequest, bool) {
	r.sessionsMu.Lock()
	defer r.sessionsMu.Unlock()
	req, ok := r.activeTurns[token]
	return req, ok
}

func (r *ACPRunner) setActiveTurn(token string, req TurnRequest) {
	r.sessionsMu.Lock()
	r.activeTurns[token] = req
	r.sessionsMu.Unlock()
}

func (r *ACPRunner) clearActiveTurn(token string) {
	r.sessionsMu.Lock()
	delete(r.activeTurns, token)
	r.sessionsMu.Unlock()
}

type acpProcessSession struct {
	cancel    context.CancelFunc
	cmd       *exec.Cmd
	stdin     io.Closer
	transport *acpRPCTransport

	sessionMu sync.RWMutex
	sessionID string
	caps      acpAgentCapabilities

	promptMu      sync.Mutex
	promptBuilder *strings.Builder
	closed        atomic.Bool

	wg sync.WaitGroup
}

func newACPProcessSession(ctx context.Context, options acpSessionOptions) (*acpProcessSession, error) {
	command := strings.TrimSpace(options.Command)
	if command == "" {
		return nil, errors.New("acp command is empty")
	}

	workDir := strings.TrimSpace(options.WorkingDir)
	if workDir == "" {
		workDir = "."
	}
	workDir, err := filepath.Abs(workDir)
	if err != nil {
		workDir = options.WorkingDir
	}

	sessionCtx, cancel := context.WithCancel(ctx)
	//nolint:gosec // The ACP command path and args come from explicit local runner configuration.
	cmd := exec.CommandContext(sessionCtx, command, options.Args...)
	cmd.Dir = workDir
	cmd.Env = mergeACPEnv(os.Environ(), options.Env)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("acp stdin pipe failed: %w", err)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("acp stdout pipe failed: %w", err)
	}

	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	session := &acpProcessSession{
		cancel: cancel,
		cmd:    cmd,
		stdin:  stdin,
	}
	session.transport = newACPRPCTransport(stdout, stdin, session.handleNotification, session.handleRequest)

	if err = cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("start acp command %q failed: %w", command, err)
	}

	session.wg.Go(func() {
		session.transport.readLoop(sessionCtx)
	})

	session.wg.Go(func() {
		waitErr := cmd.Wait()
		session.closed.Store(true)
		if waitErr != nil {
			var exitErr *exec.ExitError
			if !errors.As(waitErr, &exitErr) || exitErr.ProcessState == nil {
				message := strings.TrimSpace(stderrBuf.String())
				if message != "" {
					log.Printf("acp process exited: err=%v stderr=%s", waitErr, message)
				} else {
					log.Printf("acp process exited: err=%v", waitErr)
				}
			} else {
				waitStatus, ok := exitErr.Sys().(syscall.WaitStatus)
				if !ok || waitStatus.Signal() == 0 {
					message := strings.TrimSpace(stderrBuf.String())
					if message != "" {
						log.Printf("acp process exited: err=%v stderr=%s", waitErr, message)
					} else {
						log.Printf("acp process exited: err=%v", waitErr)
					}
				} else {
					log.Printf("acp process exited")
				}
			}
		}
		session.transport.cancelAll(waitErr)
	})

	if err = session.handshake(sessionCtx, options.ResumeSessionID, options.AuthMethod, options.MCPServers, workDir); err != nil {
		_ = session.Close()
		return nil, err
	}

	return session, nil
}

func (s *acpProcessSession) handshake(ctx context.Context, resumeSessionID, authMethod string, mcpServers []acpMCPServer, workDir string) error {
	initializeParams := map[string]any{
		"protocolVersion": defaultACPProtocolVersion,
		"clientCapabilities": map[string]any{
			"fs": map[string]any{
				"readTextFile":  false,
				"writeTextFile": false,
			},
			"terminal": false,
		},
		"clientInfo": map[string]any{
			"name":    defaultACPClientName,
			"version": defaultACPClientVersion,
		},
	}
	result, err := s.transport.call(ctx, "initialize", initializeParams)
	if err != nil {
		return fmt.Errorf("acp initialize failed: %w", err)
	}

	var initResponse struct {
		ProtocolVersion int `json:"protocolVersion"`
		AgentInfo       struct {
			Name    string `json:"name"`
			Title   string `json:"title"`
			Version string `json:"version"`
		} `json:"agentInfo"`
		AuthMethods []struct {
			ID string `json:"id"`
		} `json:"authMethods"`
		AgentCapabilities struct {
			LoadSession        bool `json:"loadSession"`
			PromptCapabilities struct {
				Image           bool `json:"image"`
				EmbeddedContext bool `json:"embeddedContext"`
			} `json:"promptCapabilities"`
			MCPCapabilities struct {
				HTTP bool `json:"http"`
			} `json:"mcpCapabilities"`
		} `json:"agentCapabilities"`
	}
	if err = json.Unmarshal(result, &initResponse); err != nil {
		return fmt.Errorf("decode acp initialize response failed: %w", err)
	}
	s.caps = acpAgentCapabilities{
		Prompt: acpPromptCapabilities{
			Image:           initResponse.AgentCapabilities.PromptCapabilities.Image,
			EmbeddedContext: initResponse.AgentCapabilities.PromptCapabilities.EmbeddedContext,
		},
		MCP: acpMCPCapabilities{
			HTTP: initResponse.AgentCapabilities.MCPCapabilities.HTTP,
		},
	}
	log.Printf(
		"acp initialize completed: protocol_version=%d agent_name=%s agent_title=%s agent_version=%s load_session=%t prompt_image=%t prompt_embedded_context=%t mcp_http=%t auth_methods=%d",
		initResponse.ProtocolVersion,
		sanitizeACPLogValue(initResponse.AgentInfo.Name),
		sanitizeACPLogValue(initResponse.AgentInfo.Title),
		sanitizeACPLogValue(initResponse.AgentInfo.Version),
		initResponse.AgentCapabilities.LoadSession,
		initResponse.AgentCapabilities.PromptCapabilities.Image,
		initResponse.AgentCapabilities.PromptCapabilities.EmbeddedContext,
		initResponse.AgentCapabilities.MCPCapabilities.HTTP,
		len(initResponse.AuthMethods),
	)

	if strings.TrimSpace(authMethod) != "" {
		if !supportsACPAuthMethod(initResponse.AuthMethods, authMethod) {
			return fmt.Errorf("acp agent does not advertise auth method %q", authMethod)
		}
		if _, err = s.transport.call(ctx, "authenticate", map[string]any{"methodId": authMethod}); err != nil {
			return fmt.Errorf("acp authenticate failed: %w", err)
		}
	}

	if strings.TrimSpace(resumeSessionID) != "" && initResponse.AgentCapabilities.LoadSession {
		if err = s.loadSession(ctx, resumeSessionID, workDir, mcpServers); err == nil {
			return nil
		}
		log.Printf("acp session/load failed, starting new session instead: requested_session=%s err=%v", resumeSessionID, err)
	}

	return s.newSession(ctx, workDir, mcpServers)
}

func (s *acpProcessSession) loadSession(ctx context.Context, sessionID, workDir string, mcpServers []acpMCPServer) error {
	result, err := s.transport.call(ctx, "session/load", map[string]any{
		"sessionId":  sessionID,
		"cwd":        workDir,
		"mcpServers": mcpServers,
	})
	if err != nil {
		return err
	}
	var payload struct {
		SessionID     string          `json:"sessionId"`
		Modes         acpSessionModes `json:"modes"`
		ConfigOptions json.RawMessage `json:"configOptions"`
	}
	if len(result) != 0 && !bytes.Equal(bytes.TrimSpace(result), []byte("null")) {
		if err = json.Unmarshal(result, &payload); err != nil {
			return fmt.Errorf("decode session/load response failed: %w", err)
		}
	}
	if strings.TrimSpace(payload.SessionID) != "" {
		s.setSessionID(payload.SessionID)
		logACPModes("session/load", payload.SessionID, payload.Modes)
		logACPConfigOptions("session/load", payload.SessionID, payload.ConfigOptions)
		return nil
	}
	s.setSessionID(sessionID)
	logACPModes("session/load", sessionID, payload.Modes)
	logACPConfigOptions("session/load", sessionID, payload.ConfigOptions)
	return nil
}

func (s *acpProcessSession) newSession(ctx context.Context, workDir string, mcpServers []acpMCPServer) error {
	result, err := s.transport.call(ctx, "session/new", map[string]any{
		"cwd":        workDir,
		"mcpServers": mcpServers,
	})
	if err != nil {
		return err
	}
	var payload struct {
		SessionID     string          `json:"sessionId"`
		Modes         acpSessionModes `json:"modes"`
		ConfigOptions json.RawMessage `json:"configOptions"`
	}
	if err = json.Unmarshal(result, &payload); err != nil {
		return fmt.Errorf("decode session/new response failed: %w", err)
	}
	if strings.TrimSpace(payload.SessionID) == "" {
		return errors.New("session/new returned empty sessionId")
	}
	s.setSessionID(payload.SessionID)
	logACPModes("session/new", payload.SessionID, payload.Modes)
	logACPConfigOptions("session/new", payload.SessionID, payload.ConfigOptions)
	return nil
}

func (s *acpProcessSession) RunPrompt(ctx context.Context, prompt []acpContentBlock) (string, error) {
	sessionID := s.CurrentSessionID()
	if sessionID == "" {
		return "", errors.New("acp session id is empty")
	}

	s.promptMu.Lock()
	s.promptBuilder = &strings.Builder{}
	s.promptMu.Unlock()
	defer func() {
		s.promptMu.Lock()
		s.promptBuilder = nil
		s.promptMu.Unlock()
	}()

	stopCancelHook := context.AfterFunc(ctx, func() {
		if err := s.transport.notify("session/cancel", map[string]any{"sessionId": sessionID}); err != nil {
			log.Printf("acp session/cancel failed: session_id=%s err=%v", sanitizeACPLogValue(sessionID), err)
		} else {
			log.Printf("acp session/cancel sent: session_id=%s", sanitizeACPLogValue(sessionID))
		}
	})
	defer func() {
		if ctx.Err() == nil {
			stopCancelHook()
		}
	}()

	_, err := s.transport.call(ctx, "session/prompt", map[string]any{
		"sessionId": sessionID,
		"prompt":    prompt,
	})
	if err != nil {
		return "", fmt.Errorf("acp session/prompt failed: %w", err)
	}

	s.promptMu.Lock()
	reply := ""
	if s.promptBuilder != nil {
		reply = strings.TrimSpace(s.promptBuilder.String())
	}
	s.promptMu.Unlock()
	return reply, nil
}

func (s *acpProcessSession) AgentCapabilities() acpAgentCapabilities {
	s.sessionMu.RLock()
	defer s.sessionMu.RUnlock()
	return s.caps
}

func buildACPPromptBlocks(prefixTextBlocks []string, message InboundMessage, capabilities acpPromptCapabilities) ([]acpContentBlock, error) {
	blocks := make([]acpContentBlock, 0, len(prefixTextBlocks)+1)
	for _, text := range prefixTextBlocks {
		if trimmed := strings.TrimSpace(text); trimmed != "" {
			blocks = append(blocks, acpContentBlock{
				"type": "text",
				"text": trimmed,
			})
		}
	}
	prompt, _ := buildTurnPrompt(message)
	if trimmedPrompt := strings.TrimSpace(prompt); trimmedPrompt != "" {
		blocks = append(blocks, acpContentBlock{
			"type": "text",
			"text": trimmedPrompt,
		})
	}

	attachments := collectACPPromptAttachments(message)
	for _, attachment := range attachments {
		block, err := buildACPAttachmentBlock(attachment, capabilities)
		if err != nil {
			return nil, err
		}
		if block != nil {
			blocks = append(blocks, block)
		}
	}

	if len(blocks) == 0 {
		blocks = append(blocks, acpContentBlock{
			"type": "text",
			"text": "",
		})
	}
	return blocks, nil
}

func buildACPAttachmentBlock(attachment acpAttachmentRef, capabilities acpPromptCapabilities) (acpContentBlock, error) {
	path := strings.TrimSpace(attachment.Path)
	if path == "" {
		return nil, nil
	}

	if attachment.Kind == acpAttachmentImage && capabilities.Image {
		return buildACPImageBlock(path)
	}
	if capabilities.EmbeddedContext {
		return buildACPEmbeddedResourceBlock(path)
	}
	return buildACPResourceLinkBlock(path)
}

func buildACPImageBlock(path string) (acpContentBlock, error) {
	//nolint:gosec // Attachment paths come from normalized local files prepared by the host application for the current turn.
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read image attachment %q failed: %w", path, err)
	}

	mimeType := detectACPMIMEType(path, data)
	if !strings.HasPrefix(mimeType, "image/") {
		return nil, fmt.Errorf("image attachment %q has unsupported MIME type %q", path, mimeType)
	}

	block := acpContentBlock{
		"type":     "image",
		"mimeType": mimeType,
		"data":     base64.StdEncoding.EncodeToString(data),
	}
	if uri := acpFileURI(path); uri != "" {
		block["uri"] = uri
	}
	return block, nil
}

func buildACPResourceLinkBlock(path string) (acpContentBlock, error) {
	cleanPath, uri := acpPathAndURI(path)
	if uri == "" {
		return nil, fmt.Errorf("build file uri for %q failed", path)
	}

	block := acpContentBlock{
		"type": "resource_link",
		"uri":  uri,
		"name": filepath.Base(cleanPath),
	}
	if mimeType := detectACPMIMEType(cleanPath, nil); mimeType != "" {
		block["mimeType"] = mimeType
	}

	info, err := os.Stat(cleanPath)
	if err == nil && info != nil && !info.IsDir() {
		block["size"] = info.Size()
	}
	return block, nil
}

func buildACPEmbeddedResourceBlock(path string) (acpContentBlock, error) {
	cleanPath, uri := acpPathAndURI(path)
	if uri == "" {
		return nil, fmt.Errorf("build file uri for %q failed", path)
	}

	info, err := os.Stat(cleanPath)
	if err != nil || info == nil || info.IsDir() || info.Size() > defaultACPEmbeddedContextMax {
		return buildACPResourceLinkBlock(path)
	}

	//nolint:gosec // Attachment paths come from normalized local files prepared by the host application for the current turn.
	content, err := os.ReadFile(cleanPath)
	if err != nil {
		return nil, fmt.Errorf("read embedded context %q failed: %w", cleanPath, err)
	}
	if !utf8.Valid(content) {
		return buildACPResourceLinkBlock(path)
	}

	mimeType := detectACPMIMEType(cleanPath, content)
	if !supportsACPEmbeddedText(mimeType) {
		return buildACPResourceLinkBlock(path)
	}

	return acpContentBlock{
		"type": "resource",
		"resource": map[string]any{
			"uri":      uri,
			"mimeType": mimeType,
			"text":     string(content),
		},
	}, nil
}

func supportsACPAuthMethod(authMethods []struct {
	ID string `json:"id"`
}, authMethod string) bool {
	authMethod = strings.TrimSpace(authMethod)
	if authMethod == "" {
		return true
	}
	for _, candidate := range authMethods {
		if strings.TrimSpace(candidate.ID) == authMethod {
			return true
		}
	}
	return false
}

func supportsACPEmbeddedText(mimeType string) bool {
	mimeType = strings.TrimSpace(mimeType)
	if mimeType == "" {
		return false
	}
	if mediaType, _, err := mime.ParseMediaType(mimeType); err == nil {
		mimeType = mediaType
	}
	if strings.HasPrefix(mimeType, "text/") {
		return true
	}
	switch mimeType {
	case "application/json", "application/xml", "application/yaml", "application/x-yaml", "application/javascript", "application/x-sh":
		return true
	default:
		return false
	}
}

func sanitizeACPLogValue(value string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, value)
}

func logACPModes(method string, sessionID string, modes acpSessionModes) {
	currentModeID := sanitizeACPLogValue(modes.CurrentModeID)
	if len(modes.AvailableModes) == 0 {
		log.Printf(
			"acp %s completed without available modes: session_id=%s current_mode_id=%s",
			method,
			sanitizeACPLogValue(sessionID),
			currentModeID,
		)
		return
	}

	parts := make([]string, 0, len(modes.AvailableModes))
	for _, mode := range modes.AvailableModes {
		modeID := sanitizeACPLogValue(mode.ID)
		modeLabel := sanitizeACPLogValue(mode.Label)
		if modeLabel == "" {
			parts = append(parts, modeID)
			continue
		}
		parts = append(parts, modeID+"("+modeLabel+")")
	}
	log.Printf(
		"acp %s modes: session_id=%s current_mode_id=%s available_modes=%s",
		method,
		sanitizeACPLogValue(sessionID),
		currentModeID,
		strings.Join(parts, ", "),
	)
}

func logACPConfigOptions(method string, sessionID string, configOptions json.RawMessage) {
	trimmed := strings.TrimSpace(string(configOptions))
	if trimmed == "" || trimmed == "null" {
		log.Printf(
			"acp %s completed without config options: session_id=%s",
			method,
			sanitizeACPLogValue(sessionID),
		)
		return
	}

	log.Printf(
		"acp %s config options: session_id=%s config_options=%s",
		method,
		sanitizeACPLogValue(sessionID),
		sanitizeACPLogValue(trimmed),
	)
}

func collectACPPromptAttachments(message InboundMessage) []acpAttachmentRef {
	seen := make(map[string]struct{})
	attachments := make([]acpAttachmentRef, 0)
	appendUnique := func(kind acpAttachmentKind, paths []string) {
		for _, path := range paths {
			path = strings.TrimSpace(path)
			if path == "" {
				continue
			}
			key := string(kind) + "\x00" + path
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			attachments = append(attachments, acpAttachmentRef{Kind: kind, Path: path})
		}
	}

	var walkReferenced func(ReferencedMessage)
	var walkInbound func(InboundMessage)

	walkReferenced = func(current ReferencedMessage) {
		appendUnique(acpAttachmentImage, allImagePaths(current.ImagePath, current.ImagePaths))
		appendUnique(acpAttachmentFile, allFilePaths(current.FilePath, current.FilePaths))
		appendUnique(acpAttachmentVideo, allVideoPaths(current.VideoPath, current.VideoPaths))
		for _, forwarded := range current.ForwardedMessages {
			walkReferenced(forwarded)
		}
	}

	walkInbound = func(current InboundMessage) {
		appendUnique(acpAttachmentImage, allImagePaths(current.ImagePath, current.ImagePaths))
		appendUnique(acpAttachmentFile, allFilePaths(current.FilePath, current.FilePaths))
		appendUnique(acpAttachmentVideo, allVideoPaths(current.VideoPath, current.VideoPaths))
		if current.QuotedMessage != nil {
			walkReferenced(*current.QuotedMessage)
		}
		for _, historical := range current.HistoricalMessages() {
			walkInbound(historical)
		}
		for _, merged := range current.MergedMessages() {
			walkInbound(merged)
		}
		for _, forwarded := range current.ForwardedMessages {
			walkReferenced(forwarded)
		}
	}

	walkInbound(message)
	return attachments
}

func detectACPMIMEType(path string, content []byte) string {
	extension := strings.ToLower(filepath.Ext(strings.TrimSpace(path)))
	if extension != "" {
		if mimeType := mime.TypeByExtension(extension); mimeType != "" {
			return mimeType
		}
	}
	if len(content) != 0 {
		return http.DetectContentType(content)
	}
	return ""
}

func acpPathAndURI(path string) (string, string) {
	cleanPath := strings.TrimSpace(path)
	if cleanPath == "" {
		return "", ""
	}

	absolutePath, err := filepath.Abs(cleanPath)
	if err == nil {
		cleanPath = absolutePath
	}
	fileURL := &url.URL{Scheme: "file", Path: filepath.ToSlash(cleanPath)}
	return cleanPath, fileURL.String()
}

func acpFileURI(path string) string {
	_, uri := acpPathAndURI(path)
	return uri
}

func (s *acpProcessSession) CurrentSessionID() string {
	s.sessionMu.RLock()
	defer s.sessionMu.RUnlock()
	return s.sessionID
}

func (s *acpProcessSession) setSessionID(sessionID string) {
	s.sessionMu.Lock()
	s.sessionID = strings.TrimSpace(sessionID)
	s.sessionMu.Unlock()
}

func (s *acpProcessSession) Close() error {
	if s == nil || s.closed.Swap(true) {
		return nil
	}

	s.cancel()
	if s.stdin != nil {
		_ = s.stdin.Close()
	}

	var shutdownErr error
	if terminateErr := signalACPProcessGroup(s.cmd, syscall.SIGTERM); terminateErr != nil {
		shutdownErr = errors.Join(shutdownErr, fmt.Errorf("terminate process group: %w", terminateErr))
	}

	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return shutdownErr
	case <-time.After(acpProcessTerminateTimeout):
	}

	if killErr := signalACPProcessGroup(s.cmd, syscall.SIGKILL); killErr != nil {
		shutdownErr = errors.Join(shutdownErr, fmt.Errorf("kill process group: %w", killErr))
	}

	<-done
	return shutdownErr
}

func (s *acpProcessSession) handleNotification(method string, params json.RawMessage) {
	if method != "session/update" {
		return
	}

	var envelope struct {
		SessionID string          `json:"sessionId"`
		Update    json.RawMessage `json:"update"`
	}
	if err := json.Unmarshal(params, &envelope); err != nil || len(envelope.Update) == 0 {
		return
	}
	if strings.TrimSpace(envelope.SessionID) != "" {
		s.setSessionID(envelope.SessionID)
	}

	var header struct {
		SessionUpdate string `json:"sessionUpdate"`
		Content       struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(envelope.Update, &header); err != nil {
		return
	}
	if header.SessionUpdate != "agent_thought_chunk" && header.SessionUpdate != "agent_message_chunk" {
		log.Printf(
			"acp session/update received: session_id=%s session_update=%s content_type=%s text_len=%d",
			strconv.Quote(sanitizeACPLogValue(envelope.SessionID)),
			strconv.Quote(sanitizeACPLogValue(header.SessionUpdate)),
			strconv.Quote(sanitizeACPLogValue(header.Content.Type)),
			len(header.Content.Text),
		)
	}
	if header.SessionUpdate != "agent_message_chunk" || strings.TrimSpace(header.Content.Text) == "" {
		return
	}

	s.promptMu.Lock()
	if s.promptBuilder != nil {
		s.promptBuilder.WriteString(header.Content.Text)
	}
	s.promptMu.Unlock()
}

func (s *acpProcessSession) handleRequest(method string, id json.RawMessage, params json.RawMessage) {
	if method != "session/request_permission" {
		_ = s.transport.respondError(id, -32601, "method not implemented")
		return
	}

	var request struct {
		Options []permissionOption `json:"options"`
	}
	if err := json.Unmarshal(params, &request); err != nil {
		_ = s.transport.respondError(id, -32602, "invalid params")
		return
	}

	optionID := pickPermissionOptionID(true, request.Options)
	_ = s.transport.respondSuccess(id, buildPermissionResult(true, optionID))
}

func mergeACPEnv(base []string, extra []string) []string {
	if len(extra) == 0 {
		return append([]string(nil), base...)
	}

	values := make(map[string]string, len(base)+len(extra))
	order := make([]string, 0, len(base)+len(extra))
	for _, item := range append(append([]string(nil), base...), extra...) {
		name, value, ok := strings.Cut(item, "=")
		if !ok {
			continue
		}
		if _, exists := values[name]; !exists {
			order = append(order, name)
		}
		values[name] = value
	}

	merged := make([]string, 0, len(order))
	for _, name := range order {
		merged = append(merged, name+"="+values[name])
	}
	return merged
}

func signalACPProcessGroup(cmd *exec.Cmd, signal syscall.Signal) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}

	err := syscall.Kill(-cmd.Process.Pid, signal)
	if err == nil || errors.Is(err, os.ErrProcessDone) || errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

type acpRPCTransport struct {
	input     *bufio.Reader
	output    io.Writer
	encoder   *json.Encoder
	writeMu   sync.Mutex
	nextID    atomic.Int64
	pendingMu sync.Mutex
	pending   map[string]chan acpRPCOutcome
	onNotify  func(string, json.RawMessage)
	onRequest func(string, json.RawMessage, json.RawMessage)
}

type acpRPCOutcome struct {
	result json.RawMessage
	err    *acpRPCError
}

type acpRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *acpRPCError) Error() string {
	if e == nil {
		return "json-rpc error"
	}
	return fmt.Sprintf("json-rpc %d: %s", e.Code, e.Message)
}

func newACPRPCTransport(input io.Reader, output io.Writer, onNotify func(string, json.RawMessage), onRequest func(string, json.RawMessage, json.RawMessage)) *acpRPCTransport {
	return &acpRPCTransport{
		input:     bufio.NewReader(input),
		output:    output,
		encoder:   json.NewEncoder(output),
		pending:   make(map[string]chan acpRPCOutcome),
		onNotify:  onNotify,
		onRequest: onRequest,
	}
}

func (t *acpRPCTransport) readLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		line, err := t.input.ReadBytes('\n')
		if err != nil {
			if !errors.Is(err, io.EOF) {
				log.Printf("acp read loop stopped: err=%v", err)
			}
			t.cancelAll(err)
			return
		}
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		t.dispatch(line)
	}
}

func (t *acpRPCTransport) dispatch(line []byte) {
	var envelope struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
		Result json.RawMessage `json:"result"`
		Error  *acpRPCError    `json:"error"`
	}
	if err := json.Unmarshal(line, &envelope); err != nil {
		return
	}
	if envelope.Method != "" {
		if len(bytes.TrimSpace(envelope.ID)) == 0 || bytes.Equal(bytes.TrimSpace(envelope.ID), []byte("null")) {
			if t.onNotify != nil {
				t.onNotify(envelope.Method, envelope.Params)
			}
			return
		}
		if t.onRequest != nil {
			t.onRequest(envelope.Method, envelope.ID, envelope.Params)
		}
		return
	}
	if len(bytes.TrimSpace(envelope.ID)) != 0 {
		t.completePending(envelope.ID, envelope.Result, envelope.Error)
	}
}

func (t *acpRPCTransport) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := t.nextID.Add(1)
	key := strconv.FormatInt(id, 10)
	wait := make(chan acpRPCOutcome, 1)
	t.pendingMu.Lock()
	t.pending[key] = wait
	t.pendingMu.Unlock()

	request := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	}
	if err := t.writeJSON(request); err != nil {
		t.pendingMu.Lock()
		delete(t.pending, key)
		t.pendingMu.Unlock()
		return nil, err
	}

	select {
	case <-ctx.Done():
		t.pendingMu.Lock()
		delete(t.pending, key)
		t.pendingMu.Unlock()
		return nil, ctx.Err()
	case outcome := <-wait:
		if outcome.err != nil {
			return nil, outcome.err
		}
		return outcome.result, nil
	}
}

func (t *acpRPCTransport) notify(method string, params any) error {
	return t.writeJSON(map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
		"params":  params,
	})
}

func (t *acpRPCTransport) completePending(id json.RawMessage, result json.RawMessage, rpcErr *acpRPCError) {
	key := strings.Trim(string(bytes.TrimSpace(id)), "\"")
	t.pendingMu.Lock()
	wait, ok := t.pending[key]
	delete(t.pending, key)
	t.pendingMu.Unlock()
	if !ok {
		return
	}
	wait <- acpRPCOutcome{result: result, err: rpcErr}
}

func (t *acpRPCTransport) cancelAll(err error) {
	message := "transport closed"
	if err != nil {
		message = err.Error()
	}
	t.pendingMu.Lock()
	defer t.pendingMu.Unlock()
	for key, wait := range t.pending {
		wait <- acpRPCOutcome{err: &acpRPCError{Code: defaultACPReadLoopErrorCode, Message: message}}
		delete(t.pending, key)
	}
}

func (t *acpRPCTransport) respondSuccess(id json.RawMessage, result any) error {
	return t.writeJSON(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"result":  result,
	})
}

func (t *acpRPCTransport) respondError(id json.RawMessage, code int, message string) error {
	return t.writeJSON(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error": map[string]any{
			"code":    code,
			"message": message,
		},
	})
}

func (t *acpRPCTransport) writeJSON(payload any) error {
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	return t.encoder.Encode(payload)
}

type acpToolHTTPServer struct {
	listener           net.Listener
	server             *http.Server
	url                string
	closeOnce          sync.Once
	isAuthorized       func(string) bool
	resolveTurnRequest func(string) (TurnRequest, bool)
	tools              func() []Tool
}

type acpToolHTTPServerOptions struct {
	IsAuthorized       func(string) bool
	ResolveTurnRequest func(string) (TurnRequest, bool)
	Tools              func() []Tool
}

type acpToolTokenContextKey struct{}

func newACPToolHTTPServer(ctx context.Context, options acpToolHTTPServerOptions) (*acpToolHTTPServer, error) {
	if options.IsAuthorized == nil {
		return nil, errors.New("acp tool server authorization callback is nil")
	}
	if options.ResolveTurnRequest == nil {
		return nil, errors.New("acp tool server turn resolver is nil")
	}
	if options.Tools == nil {
		return nil, errors.New("acp tool server tool provider is nil")
	}

	var listenConfig net.ListenConfig
	listener, err := listenConfig.Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen on tool server failed: %w", err)
	}

	server := &acpToolHTTPServer{
		listener:           listener,
		isAuthorized:       options.IsAuthorized,
		resolveTurnRequest: options.ResolveTurnRequest,
		tools:              options.Tools,
	}

	handler := mcp.NewStreamableHTTPHandler(func(request *http.Request) *mcp.Server {
		return server.newRequestServer(request)
	}, &mcp.StreamableHTTPOptions{
		Stateless:    true,
		JSONResponse: true,
	})
	server.server = &http.Server{
		Handler: server.wrapAuth(handler),
		BaseContext: func(net.Listener) context.Context {
			return ctx
		},
		ReadHeaderTimeout: defaultHTTPReadHeaderTimeout,
	}
	server.url = "http://" + listener.Addr().String() + "/mcp"

	go func() {
		if serveErr := server.server.Serve(listener); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			log.Printf("acp tool server stopped: err=%v", serveErr)
		}
	}()
	return server, nil
}

func (s *acpToolHTTPServer) ServerConfig(token string) acpMCPServer {
	return acpMCPServer{
		Name: defaultACPToolServerName,
		Type: "http",
		URL:  s.url,
		Headers: []acpMCPHeader{
			{Name: "Authorization", Value: "Bearer " + token},
			{Name: "Accept", Value: "application/json, text/event-stream"},
		},
	}
}

func (s *acpToolHTTPServer) wrapAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mcp" {
			http.NotFound(w, r)
			return
		}
		token, ok := bearerACPToken(r.Header.Get("Authorization"))
		if !ok || !s.isAuthorized(token) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if accept := r.Header.Get("Accept"); accept != "" && !strings.Contains(accept, "application/json") {
			http.Error(w, "accept must contain application/json", http.StatusBadRequest)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), acpToolTokenContextKey{}, token)))
	})
}

func bearerACPToken(authorization string) (string, bool) {
	authorization = strings.TrimSpace(authorization)
	if !strings.HasPrefix(authorization, "Bearer ") {
		return "", false
	}
	token := strings.TrimSpace(strings.TrimPrefix(authorization, "Bearer "))
	if token == "" {
		return "", false
	}
	return token, true
}

func (s *acpToolHTTPServer) newRequestServer(_ *http.Request) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{
		Name:    defaultACPToolServerName,
		Version: "1.0.0",
	}, nil)
	for _, tool := range s.tools() {
		if tool == nil {
			continue
		}
		server.AddTool(&mcp.Tool{
			Name:         tool.Name(),
			Description:  tool.Description(),
			InputSchema:  normalizeACPToolSchema(tool.InputSchema()),
			OutputSchema: normalizeACPToolSchema(tool.OutputSchema()),
		}, s.handleToolCall(tool))
	}
	return server
}

func (s *acpToolHTTPServer) handleToolCall(tool Tool) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		token, _ := ctx.Value(acpToolTokenContextKey{}).(string)
		turnReq, ok := s.resolveTurnRequest(token)
		if !ok {
			return &mcp.CallToolResult{
				Content: []mcp.Content{
					&mcp.TextContent{Text: "tool call failed: active turn context not found"},
				},
				IsError: true,
			}, nil
		}

		arguments := req.Params.Arguments
		if len(arguments) == 0 {
			arguments = []byte("{}")
		}

		result, toolErr := tool.Call(ContextWithTurnRequest(ctx, turnReq), arguments)
		if toolErr != nil {
			//nolint:nilerr // MCP tool execution errors are returned in-band via CallToolResult.
			return &mcp.CallToolResult{
				Content: []mcp.Content{
					&mcp.TextContent{Text: toolErr.Error()},
				},
				IsError: true,
			}, nil
		}

		text, err := formatClaudeMCPToolResult(result)
		if err != nil {
			return nil, err
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{Text: text},
			},
		}, nil
	}
}

func normalizeACPToolSchema(schema any) any {
	if schema == nil {
		return json.RawMessage(`{"type":"object","properties":{}}`)
	}
	return schema
}

func randomACPToken() (string, error) {
	var token [24]byte
	_, err := rand.Read(token[:])
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(token[:]), nil
}

func (s *acpToolHTTPServer) Close() error {
	if s == nil {
		return nil
	}

	var err error
	s.closeOnce.Do(func() {
		if s.server != nil {
			err = s.server.Close()
		}
	})
	return err
}

type permissionOption struct {
	OptionID string `json:"optionId"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
}

func pickPermissionOptionID(allow bool, options []permissionOption) string {
	if len(options) == 0 {
		return ""
	}
	if allow {
		for _, option := range options {
			if strings.Contains(strings.ToLower(option.Kind), "allow") {
				return option.OptionID
			}
		}
		for _, option := range options {
			if strings.Contains(strings.ToLower(option.Name), "allow") {
				return option.OptionID
			}
		}
		return options[0].OptionID
	}
	for _, option := range options {
		if strings.Contains(strings.ToLower(option.Kind), "reject") || strings.Contains(strings.ToLower(option.Kind), "deny") {
			return option.OptionID
		}
	}
	for _, option := range options {
		if strings.Contains(strings.ToLower(option.Name), "reject") || strings.Contains(strings.ToLower(option.Name), "deny") {
			return option.OptionID
		}
	}
	return options[len(options)-1].OptionID
}

func buildPermissionResult(allow bool, optionID string) map[string]any {
	if !allow && optionID == "" {
		return map[string]any{
			"outcome": map[string]any{
				//nolint:misspell // ACP permission result uses the protocol spelling "cancelled".
				"outcome": "cancelled",
			},
		}
	}
	return map[string]any{
		"outcome": map[string]any{
			"outcome":  "selected",
			"optionId": optionID,
		},
	}
}
