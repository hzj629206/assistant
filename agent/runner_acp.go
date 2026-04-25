package agent

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
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
	defaultACPEmbeddedContextMax = 256 * 1024
)

// ACPRunner bridges dispatcher turns to an ACP-compatible local agent process.
// Global tools are exposed through a runner-scoped streamable HTTP MCP server,
// while individual tool calls are routed with the active turn context.
type ACPRunner struct {
	command        string
	args           []string
	env            []string
	authMethod     string
	workDir        string
	sessionFactory func(context.Context, acp.SessionOptions) (acp.Session, error)
	//nolint:containedctx // This is the runner lifecycle root context shared by managed ACP sessions.
	lifecycleCtx   context.Context
	cancel         context.CancelFunc
	mu             sync.RWMutex
	systemPrompts  []string
	tools          []Tool
	runtimeMu      sync.Mutex
	toolCallTokens map[string]toolCallTokenState
	toolServer     *acp.HTTPToolServer
	closed         bool
}

type toolCallTokenState struct {
	req *TurnRequest
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
		command:        command,
		args:           append([]string(nil), options.Args...),
		env:            append([]string(nil), options.Env...),
		authMethod:     strings.TrimSpace(options.AuthMethod),
		workDir:        workDir,
		toolCallTokens: make(map[string]toolCallTokenState),
	}
	if ctx == nil {
		ctx = context.Background()
	}
	// The runner outlives daemon startup and should only stop on explicit close.
	lifecycleCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	runner.lifecycleCtx = lifecycleCtx
	runner.cancel = cancel
	runner.sessionFactory = acp.StartSession

	runner.RegisterSystemPrompt(options.SystemPrompt)
	runner.RegisterTools(options.Tools...)

	return runner
}

// StartSession creates or resumes one dispatcher-managed ACP session.
// The dispatcher owns session reuse and lifecycle management.
func (r *ACPRunner) StartSession(ctx context.Context, options SessionOptions) (Session, error) {
	if r == nil {
		return nil, errors.New("start acp session failed: runner is nil")
	}
	r.runtimeMu.Lock()
	if r.closed {
		r.runtimeMu.Unlock()
		return nil, errors.New("start acp session failed: runner is closed")
	}
	r.runtimeMu.Unlock()

	session, token, err := r.createACPSession(ctx, strings.TrimSpace(options.ResumeSessionID))
	if err != nil {
		return nil, fmt.Errorf("start acp session failed: %w", err)
	}
	return &acpRunnerSession{
		runner:                      r,
		conversationKey:             strings.TrimSpace(options.ConversationKey),
		sessionID:                   strings.TrimSpace(session.SessionID()),
		session:                     session,
		token:                       token,
		pendingInitialSystemPrompts: strings.TrimSpace(options.ResumeSessionID) == "",
	}, nil
}

// Close shuts down shared ACP runner resources and signals active sessions to exit.
func (r *ACPRunner) Close() error {
	if r == nil {
		return nil
	}

	r.runtimeMu.Lock()
	if r.closed {
		r.runtimeMu.Unlock()
		return nil
	}
	r.closed = true
	clear(r.toolCallTokens)
	toolServer := r.toolServer
	r.toolServer = nil
	cancel := r.cancel
	r.cancel = nil
	r.runtimeMu.Unlock()

	if cancel != nil {
		cancel()
	}

	var closeErr error
	if toolServer != nil {
		closeErr = errors.Join(closeErr, toolServer.Close())
	}
	if closeErr != nil {
		log.Printf("acp runner closed: err=%v", closeErr)
	} else {
		log.Printf("acp runner closed")
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

func (r *ACPRunner) ensureACPToolServer(ctx context.Context) (*acp.HTTPToolServer, error) {
	r.runtimeMu.Lock()
	defer r.runtimeMu.Unlock()
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

func (r *ACPRunner) createACPSession(ctx context.Context, resumeSessionID string) (acp.Session, string, error) {
	if r == nil {
		return nil, "", errors.New("acp runner is nil")
	}

	sessionOptions := acp.SessionOptions{
		Command:         r.command,
		Args:            append([]string(nil), r.args...),
		Env:             append([]string(nil), r.env...),
		WorkingDir:      r.workDir,
		ResumeSessionID: strings.TrimSpace(resumeSessionID),
		AuthMethod:      r.authMethod,
	}

	_, tools := r.globalContext()
	needsTools := len(tools) != 0

	token, err := randomACPToken()
	if err != nil {
		return nil, "", err
	}
	r.runtimeMu.Lock()
	if r.closed {
		r.runtimeMu.Unlock()
		return nil, "", errors.New("acp runner is closed")
	}
	r.reserveToolCallToken(token)
	r.runtimeMu.Unlock()
	defer r.releaseReservedToolCallToken(token)

	if needsTools {
		toolServer, serverErr := r.ensureACPToolServer(context.WithoutCancel(ctx))
		if serverErr != nil {
			return nil, "", serverErr
		}
		sessionOptions.MCPServers = []acp.MCPServer{toolServer.ServerConfig(token)}
	}

	if r.sessionFactory == nil {
		return nil, "", errors.New("acp session factory is nil")
	}
	if r.lifecycleCtx == nil {
		return nil, "", errors.New("acp runner lifecycle context is nil")
	}

	//nolint:contextcheck // ACP sessions are intentionally rooted in the runner lifecycle context.
	session, err := r.sessionFactory(r.lifecycleCtx, sessionOptions)
	if err != nil {
		return nil, "", err
	}
	if needsTools && !session.Capabilities().MCP.HTTP {
		_ = session.Close()
		return nil, "", errors.New("acp agent does not advertise HTTP MCP support")
	}
	return session, token, nil
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
