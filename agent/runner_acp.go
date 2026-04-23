package agent

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/hzj629206/assistant/agent/acp"
)

const (
	defaultACPToolServerName     = "assistant"
	defaultACPSessionIdleTimeout = 10 * time.Minute
	defaultACPEmbeddedContextMax = 256 * 1024
)

// ACPRunner bridges dispatcher turns to an ACP-compatible local agent process.
// Global tools are exposed through a runner-scoped streamable HTTP MCP server,
// while individual tool calls are routed with the active turn context.
type ACPRunner struct {
	command            string
	args               []string
	env                []string
	authMethod         string
	workDir            string
	sessionIdleTimeout time.Duration
	sessionFactory     func(context.Context, acp.SessionOptions) (acp.Session, error)
	//nolint:containedctx // This is the runner lifecycle root context shared by managed ACP sessions.
	lifecycleCtx  context.Context
	cancel        context.CancelFunc
	mu            sync.RWMutex
	systemPrompts []string
	tools         []Tool
	sessionsMu    sync.Mutex
	sessions      map[string]*acpRunnerSession
	activeTurns   map[string]TurnRequest
	toolServer    *acp.HTTPToolServer
	closed        bool
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
	session   acp.Session
	token     string
	idleTimer *time.Timer
	inUse     bool
}

// NewACPRunner builds a runner backed by an ACP agent CLI.
//
//nolint:contextcheck // This constructor intentionally accepts a root lifecycle context for the runner.
func NewACPRunner(ctx context.Context, options ACPRunnerOptions) *ACPRunner {
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
	if ctx == nil {
		ctx = context.Background()
	}
	// The runner outlives daemon startup and should only stop on explicit close.
	lifecycleCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	runner.lifecycleCtx = lifecycleCtx
	runner.cancel = cancel
	if runner.sessionIdleTimeout <= 0 {
		runner.sessionIdleTimeout = defaultACPSessionIdleTimeout
	}
	runner.sessionFactory = acp.StartSession

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
	cancel := r.cancel
	r.cancel = nil
	r.sessionsMu.Unlock()

	if cancel != nil {
		cancel()
	}

	var closeErr error
	for _, session := range sessions {
		closeErr = errors.Join(closeErr, session.session.Close())
	}
	if toolServer != nil {
		closeErr = errors.Join(closeErr, toolServer.Close())
	}
	if closeErr != nil {
		log.Printf("acp runner closed: sessions=%d err=%v", len(sessions), closeErr)
	} else {
		log.Printf("acp runner closed: sessions=%d", len(sessions))
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
	promptBlocks, err := buildACPPromptBlocks(initialPromptBlocks, req.Message, session.session.Capabilities().Prompt)
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

	stopTyping := startTyping(ctx, req.Message.Responder)
	defer stopTyping()

	turnResult, err := session.session.RunTurn(ctx, promptBlocks)
	if err != nil {
		r.discardACPSession(req.Conversation.Key, session, err)
		return TurnResult{}, fmt.Errorf("run acp turn failed: %w", err)
	}

	return TurnResult{
		RunnerThreadID: turnResult.SessionID,
		ReplyText:      turnResult.ReplyText,
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

	sessionOptions := acp.SessionOptions{
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
		toolServer, err := r.ensureACPToolServer(context.WithoutCancel(ctx))
		if err != nil {
			return nil, err
		}
		sessionOptions.MCPServers = []acp.MCPServer{toolServer.ServerConfig(token)}
	}

	if r.sessionFactory == nil {
		return nil, errors.New("acp session factory is nil")
	}
	if r.lifecycleCtx == nil {
		return nil, errors.New("acp runner lifecycle context is nil")
	}
	//nolint:contextcheck // ACP sessions intentionally inherit the runner lifecycle context.
	session, err := r.sessionFactory(r.lifecycleCtx, sessionOptions)
	if err != nil {
		return nil, err
	}
	if needsTools && !session.Capabilities().MCP.HTTP {
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

func (r *ACPRunner) ensureACPToolServer(ctx context.Context) (*acp.HTTPToolServer, error) {
	r.sessionsMu.Lock()
	defer r.sessionsMu.Unlock()
	if r.toolServer != nil {
		return r.toolServer, nil
	}

	server, err := acp.NewHTTPToolServer(ctx, acp.HTTPToolServerOptions{
		ServerName: defaultACPToolServerName,
		IsAuthorized: func(token string) bool {
			return r.isAuthorizedACPToken(token)
		},
		Tools: func(token string) []acp.HTTPTool {
			return r.acpHTTPTools(token)
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

func (r *ACPRunner) acpHTTPTools(token string) []acp.HTTPTool {
	_, tools := r.globalContext()
	adapted := make([]acp.HTTPTool, 0, len(tools))
	for _, tool := range tools {
		if tool == nil {
			continue
		}
		adapted = append(adapted, acpHTTPToolAdapter{
			token:  token,
			tool:   tool,
			runner: r,
		})
	}
	return adapted
}

type acpHTTPToolAdapter struct {
	token  string
	tool   Tool
	runner *ACPRunner
}

func (t acpHTTPToolAdapter) Name() string {
	return t.tool.Name()
}

func (t acpHTTPToolAdapter) Description() string {
	return t.tool.Description()
}

func (t acpHTTPToolAdapter) InputSchema() any {
	return t.tool.InputSchema()
}

func (t acpHTTPToolAdapter) OutputSchema() any {
	return t.tool.OutputSchema()
}

func (t acpHTTPToolAdapter) Call(ctx context.Context, arguments json.RawMessage) (any, error) {
	if len(arguments) == 0 {
		arguments = json.RawMessage("{}")
	}
	turnReq, ok := t.runner.activeTurnForToken(t.token)
	if !ok {
		return nil, errors.New("tool call failed: active turn context not found")
	}
	return t.tool.Call(ContextWithTurnRequest(ctx, turnReq), arguments)
}

func buildACPPromptBlocks(prefixTextBlocks []string, message InboundMessage, capabilities acp.PromptCapabilities) ([]acp.ContentBlock, error) {
	blocks := make([]acp.ContentBlock, 0, len(prefixTextBlocks)+1)
	prompt := buildTurnPromptResult(message)
	textParts := make([]string, 0, len(prefixTextBlocks)+1)
	if len(prefixTextBlocks) != 0 {
		systemBlock := acp.SystemBlock(prefixTextBlocks...)
		if text := strings.TrimSpace(stringValueFromACPContentBlock(systemBlock, "text")); text != "" {
			textParts = append(textParts, text)
		}
	}
	if trimmedPrompt := strings.TrimSpace(prompt.Text); trimmedPrompt != "" {
		textParts = append(textParts, trimmedPrompt)
	}
	if len(textParts) != 0 {
		blocks = append(blocks, acp.TextBlock(strings.Join(textParts, "\n\n")))
	}

	for _, attachment := range prompt.Attachments {
		block, err := buildACPAttachmentBlock(attachment, capabilities)
		if err != nil {
			return nil, err
		}
		if block != nil {
			blocks = append(blocks, block)
		}
	}

	if len(blocks) == 0 {
		blocks = append(blocks, acp.TextBlock(""))
	}
	return blocks, nil
}

func stringValueFromACPContentBlock(block acp.ContentBlock, key string) string {
	value, _ := block[key].(string)
	return value
}

func buildACPAttachmentBlock(attachment promptAttachmentRef, capabilities acp.PromptCapabilities) (acp.ContentBlock, error) {
	path := strings.TrimSpace(attachment.Path)
	if path == "" {
		return nil, nil
	}

	if attachment.Kind == promptAttachmentImage && capabilities.Image {
		return buildACPImageBlock(path)
	}
	if capabilities.EmbeddedContext {
		return buildACPEmbeddedResourceBlock(path)
	}
	return buildACPResourceLinkBlock(path)
}

func buildACPImageBlock(path string) (acp.ContentBlock, error) {
	//nolint:gosec // Attachment paths come from normalized local files prepared by the host application for the current turn.
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read image attachment %q failed: %w", path, err)
	}

	mimeType := detectACPMIMEType(path, data)
	if !strings.HasPrefix(mimeType, "image/") {
		return nil, fmt.Errorf("image attachment %q has unsupported MIME type %q", path, mimeType)
	}

	return acp.ImageBlock(mimeType, base64.StdEncoding.EncodeToString(data), acpFileURI(path)), nil
}

func buildACPResourceLinkBlock(path string) (acp.ContentBlock, error) {
	cleanPath, uri := acpPathAndURI(path)
	if uri == "" {
		return nil, fmt.Errorf("build file uri for %q failed", path)
	}

	mimeType := detectACPMIMEType(cleanPath, nil)
	size := int64(0)
	info, err := os.Stat(cleanPath)
	if err == nil && info != nil && !info.IsDir() {
		size = info.Size()
	}
	return acp.ResourceLinkBlock(uri, filepath.Base(cleanPath), mimeType, size), nil
}

func buildACPEmbeddedResourceBlock(path string) (acp.ContentBlock, error) {
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

	return acp.EmbeddedResourceBlock(uri, mimeType, string(content)), nil
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

func randomACPToken() (string, error) {
	var token [24]byte
	_, err := rand.Read(token[:])
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(token[:]), nil
}
