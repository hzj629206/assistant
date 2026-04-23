package acp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	defaultProtocolVersion  = 1
	defaultClientName       = "assistant"
	defaultClientVersion    = "1.0.0"
	processTerminateTimeout = 5 * time.Second
)

var processHandshakeTimeout = 30 * time.Second

type processSession struct {
	//nolint:containedctx // This is the session lifecycle root context, not a per-request context.
	lifecycleCtx context.Context
	cancel       context.CancelFunc
	cmd          *exec.Cmd
	stdin        io.Closer
	transport    *rpcTransport
	observer     Observer
	permission   PermissionHandler

	sessionMu sync.RWMutex
	sessionID string
	caps      AgentCapabilities

	promptMu      sync.Mutex
	promptBuilder *strings.Builder
	closed        atomic.Bool

	wg sync.WaitGroup
}

type sessionMode struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

type sessionModes struct {
	CurrentModeID  string        `json:"currentModeId"`
	AvailableModes []sessionMode `json:"availableModes"`
}

// StartSession starts one ACP subprocess session.
func StartSession(ctx context.Context, options SessionOptions) (Session, error) {
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
	log.Printf("starting acp session process: path=%s args=%q work_dir=%s", command, options.Args, workDir)
	//nolint:gosec // The ACP command path and args come from explicit local runner configuration.
	cmd := exec.CommandContext(sessionCtx, command, options.Args...)
	cmd.Dir = workDir
	cmd.Env = mergeEnv(os.Environ(), options.Env)
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

	session := &processSession{
		lifecycleCtx: sessionCtx,
		cancel:       cancel,
		cmd:          cmd,
		stdin:        stdin,
		observer:     options.Observer,
		permission:   options.Permission,
	}
	if session.permission == nil {
		session.permission = defaultPermissionHandler
	}
	if options.Stderr == nil {
		options.Stderr = os.Stderr
	}
	cmd.Stderr = options.Stderr
	session.transport = newRPCTransport(stdout, stdin, session.handleNotification, session.handleRequest)

	if err = cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("start acp command %q failed: %w", command, err)
	}

	session.wg.Go(func() {
		session.transport.readLoop(sessionCtx)
	})

	session.wg.Go(func() {
		rawWaitErr := cmd.Wait()
		waitErr := ignoreExpectedExit(rawWaitErr)
		session.closed.Store(true)
		if rawWaitErr == nil {
			log.Printf("acp process exited")
		} else {
			logProcessExit(rawWaitErr)
		}
		if session.observer.OnProcessExit != nil {
			session.observer.OnProcessExit(waitErr)
		}
		session.transport.cancelAll(waitErr)
	})

	handshakeCtx, handshakeCancel := context.WithTimeout(sessionCtx, processHandshakeTimeout)
	defer handshakeCancel()
	if err = session.handshake(handshakeCtx, options.ResumeSessionID, options.AuthMethod, options.MCPServers, workDir); err != nil {
		_ = session.Close()
		return nil, err
	}

	return session, nil
}

func (s *processSession) handshake(ctx context.Context, resumeSessionID string, authMethod string, mcpServers []MCPServer, workDir string) error {
	initializeParams := map[string]any{
		"protocolVersion": defaultProtocolVersion,
		"clientCapabilities": map[string]any{
			"fs": map[string]any{
				"readTextFile":  false,
				"writeTextFile": false,
			},
			"terminal": false,
		},
		"clientInfo": map[string]any{
			"name":    defaultClientName,
			"version": defaultClientVersion,
		},
	}
	log.Printf("acp initialize started")
	result, err := s.transport.call(ctx, "initialize", initializeParams)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("acp initialize failed: timed out after %s waiting for agent response", processHandshakeTimeout)
		}
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
	s.caps = AgentCapabilities{
		Prompt: PromptCapabilities{
			Image:           initResponse.AgentCapabilities.PromptCapabilities.Image,
			EmbeddedContext: initResponse.AgentCapabilities.PromptCapabilities.EmbeddedContext,
		},
		MCP: MCPCapabilities{
			HTTP: initResponse.AgentCapabilities.MCPCapabilities.HTTP,
		},
	}
	log.Printf(
		"acp initialize completed: protocol_version=%d agent_name=%s agent_title=%s agent_version=%s load_session=%t prompt_image=%t prompt_embedded_context=%t mcp_http=%t auth_methods=%d",
		initResponse.ProtocolVersion,
		sanitizeLogValue(initResponse.AgentInfo.Name),
		sanitizeLogValue(initResponse.AgentInfo.Title),
		sanitizeLogValue(initResponse.AgentInfo.Version),
		initResponse.AgentCapabilities.LoadSession,
		initResponse.AgentCapabilities.PromptCapabilities.Image,
		initResponse.AgentCapabilities.PromptCapabilities.EmbeddedContext,
		initResponse.AgentCapabilities.MCPCapabilities.HTTP,
		len(initResponse.AuthMethods),
	)

	if strings.TrimSpace(authMethod) != "" {
		if !supportsAuthMethod(initResponse.AuthMethods, authMethod) {
			return fmt.Errorf("acp agent does not advertise auth method %q", authMethod)
		}
		log.Printf("acp authenticate started: method=%s", sanitizeLogValue(authMethod))
		if _, err = s.transport.call(ctx, "authenticate", map[string]any{"methodId": authMethod}); err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				return fmt.Errorf("acp authenticate failed: timed out after %s waiting for agent response", processHandshakeTimeout)
			}
			return fmt.Errorf("acp authenticate failed: %w", err)
		}
		log.Printf("acp authenticate completed: method=%s", sanitizeLogValue(authMethod))
	}

	if strings.TrimSpace(resumeSessionID) != "" && initResponse.AgentCapabilities.LoadSession {
		log.Printf("acp session/load started: requested_session=%s", sanitizeLogValue(resumeSessionID))
		if err = s.loadSession(ctx, resumeSessionID, workDir, mcpServers); err == nil {
			return nil
		}
		log.Printf("acp session/load failed, starting new session instead: requested_session=%s err=%v", resumeSessionID, err)
	}

	return s.newSession(ctx, workDir, mcpServers)
}

func (s *processSession) loadSession(ctx context.Context, sessionID string, workDir string, mcpServers []MCPServer) error {
	log.Printf("acp session/load request sent: requested_session=%s cwd=%s mcp_servers=%d", sanitizeLogValue(sessionID), sanitizeLogValue(workDir), len(mcpServers))
	result, err := s.transport.call(ctx, "session/load", map[string]any{
		"sessionId":  sessionID,
		"cwd":        workDir,
		"mcpServers": mcpServers,
	})
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("session/load timed out after %s waiting for agent response", processHandshakeTimeout)
		}
		return err
	}
	var payload struct {
		SessionID     string          `json:"sessionId"`
		Modes         sessionModes    `json:"modes"`
		ConfigOptions json.RawMessage `json:"configOptions"`
	}
	if len(result) != 0 && !bytes.Equal(bytes.TrimSpace(result), []byte("null")) {
		if err = json.Unmarshal(result, &payload); err != nil {
			return fmt.Errorf("decode session/load response failed: %w", err)
		}
	}
	if strings.TrimSpace(payload.SessionID) != "" {
		s.setSessionID(payload.SessionID)
		logModes("session/load", payload.SessionID, payload.Modes)
		logConfigOptions("session/load", payload.SessionID, payload.ConfigOptions)
		return nil
	}
	s.setSessionID(sessionID)
	logModes("session/load", sessionID, payload.Modes)
	logConfigOptions("session/load", sessionID, payload.ConfigOptions)
	return nil
}

func (s *processSession) newSession(ctx context.Context, workDir string, mcpServers []MCPServer) error {
	log.Printf("acp session/new started: cwd=%s mcp_servers=%d", sanitizeLogValue(workDir), len(mcpServers))
	result, err := s.transport.call(ctx, "session/new", map[string]any{
		"cwd":        workDir,
		"mcpServers": mcpServers,
	})
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("session/new timed out after %s waiting for agent response", processHandshakeTimeout)
		}
		return err
	}
	var payload struct {
		SessionID     string          `json:"sessionId"`
		Modes         sessionModes    `json:"modes"`
		ConfigOptions json.RawMessage `json:"configOptions"`
	}
	if err = json.Unmarshal(result, &payload); err != nil {
		return fmt.Errorf("decode session/new response failed: %w", err)
	}
	if strings.TrimSpace(payload.SessionID) == "" {
		return errors.New("session/new returned empty sessionId")
	}
	s.setSessionID(payload.SessionID)
	logModes("session/new", payload.SessionID, payload.Modes)
	logConfigOptions("session/new", payload.SessionID, payload.ConfigOptions)
	return nil
}

func (s *processSession) RunTurn(ctx context.Context, blocks []ContentBlock) (TurnResult, error) {
	sessionID := s.CurrentSessionID()
	if sessionID == "" {
		return TurnResult{}, errors.New("acp session id is empty")
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
			log.Printf("acp session/cancel failed: session_id=%s err=%v", sanitizeLogValue(sessionID), err)
		} else {
			log.Printf("acp session/cancel sent: session_id=%s", sanitizeLogValue(sessionID))
		}
	})
	defer func() {
		if ctx.Err() == nil {
			stopCancelHook()
		}
	}()

	result, err := s.transport.call(ctx, "session/prompt", map[string]any{
		"sessionId": sessionID,
		"prompt":    blocks,
	})
	if err != nil {
		return TurnResult{}, fmt.Errorf("acp session/prompt failed: %w", err)
	}

	s.promptMu.Lock()
	reply := ""
	if s.promptBuilder != nil {
		reply = s.promptBuilder.String()
	}
	s.promptMu.Unlock()
	return TurnResult{
		SessionID: s.CurrentSessionID(),
		ReplyText: reply,
		RawResult: append(json.RawMessage(nil), result...),
	}, nil
}

func (s *processSession) Capabilities() AgentCapabilities {
	s.sessionMu.RLock()
	defer s.sessionMu.RUnlock()
	return s.caps
}

func (s *processSession) CurrentSessionID() string {
	s.sessionMu.RLock()
	defer s.sessionMu.RUnlock()
	return s.sessionID
}

func (s *processSession) setSessionID(sessionID string) {
	s.sessionMu.Lock()
	s.sessionID = strings.TrimSpace(sessionID)
	s.sessionMu.Unlock()
}

func (s *processSession) Close() error {
	if s == nil || s.closed.Swap(true) {
		return nil
	}

	s.cancel()
	if s.stdin != nil {
		_ = s.stdin.Close()
	}

	var shutdownErr error
	if terminateErr := signalProcessGroup(s.cmd, syscall.SIGTERM); terminateErr != nil {
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
	case <-time.After(processTerminateTimeout):
	}

	if killErr := signalProcessGroup(s.cmd, syscall.SIGKILL); killErr != nil {
		shutdownErr = errors.Join(shutdownErr, fmt.Errorf("kill process group: %w", killErr))
	}

	<-done
	return shutdownErr
}

func (s *processSession) handleNotification(method string, params json.RawMessage) {
	if s.observer.OnNotification != nil {
		s.observer.OnNotification(method, params)
	}
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
	if s.observer.OnSessionUpdate != nil {
		s.observer.OnSessionUpdate(SessionUpdate{
			SessionID:     envelope.SessionID,
			SessionUpdate: header.SessionUpdate,
			ContentType:   header.Content.Type,
			Text:          header.Content.Text,
			Raw:           append(json.RawMessage(nil), envelope.Update...),
		})
	}
	if header.SessionUpdate != "agent_thought_chunk" && header.SessionUpdate != "agent_message_chunk" {
		log.Printf(
			"acp session/update received: session_id=%s session_update=%s content_type=%s text_len=%d",
			strconv.Quote(sanitizeLogValue(envelope.SessionID)),
			strconv.Quote(sanitizeLogValue(header.SessionUpdate)),
			strconv.Quote(sanitizeLogValue(header.Content.Type)),
			len(header.Content.Text),
		)
	}
	if header.SessionUpdate != "agent_message_chunk" || header.Content.Text == "" {
		return
	}

	s.promptMu.Lock()
	if s.promptBuilder != nil {
		s.promptBuilder.WriteString(header.Content.Text)
	}
	s.promptMu.Unlock()
}

func (s *processSession) handleRequest(method string, id json.RawMessage, params json.RawMessage) {
	if method != "session/request_permission" {
		_ = s.transport.respondError(id, -32601, "method not implemented")
		return
	}

	var request struct {
		Options []PermissionOption `json:"options"`
	}
	if err := json.Unmarshal(params, &request); err != nil {
		_ = s.transport.respondError(id, -32602, "invalid params")
		return
	}

	permissionRequest := PermissionRequest{
		Method:    method,
		SessionID: s.CurrentSessionID(),
		Options:   request.Options,
		Raw:       append(json.RawMessage(nil), params...),
	}
	if s.observer.OnPermissionRequest != nil {
		s.observer.OnPermissionRequest(permissionRequest)
	}

	decision, err := s.permission(s.lifecycleCtx, permissionRequest)
	if err != nil {
		_ = s.transport.respondError(id, -32603, err.Error())
		return
	}
	if s.observer.OnPermissionResult != nil {
		s.observer.OnPermissionResult(permissionRequest, decision)
	}
	_ = s.transport.respondSuccess(id, buildPermissionResult(decision))
}

func supportsAuthMethod(authMethods []struct {
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

func sanitizeLogValue(value string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, value)
}

func logModes(method string, sessionID string, modes sessionModes) {
	currentModeID := sanitizeLogValue(modes.CurrentModeID)
	if len(modes.AvailableModes) == 0 {
		log.Printf("acp %s completed without available modes: session_id=%s current_mode_id=%s", method, sanitizeLogValue(sessionID), currentModeID)
		return
	}

	parts := make([]string, 0, len(modes.AvailableModes))
	for _, mode := range modes.AvailableModes {
		modeID := sanitizeLogValue(mode.ID)
		modeLabel := sanitizeLogValue(mode.Label)
		if modeLabel == "" {
			parts = append(parts, modeID)
			continue
		}
		parts = append(parts, modeID+"("+modeLabel+")")
	}
	log.Printf(
		"acp %s modes: session_id=%s current_mode_id=%s available_modes=%s",
		method,
		sanitizeLogValue(sessionID),
		currentModeID,
		strings.Join(parts, ", "),
	)
}

func logConfigOptions(method string, sessionID string, configOptions json.RawMessage) {
	trimmed := strings.TrimSpace(string(configOptions))
	if trimmed == "" || trimmed == "null" {
		log.Printf("acp %s completed without config options: session_id=%s", method, sanitizeLogValue(sessionID))
		return
	}

	log.Printf(
		"acp %s config options: session_id=%s config_options=%s",
		method,
		sanitizeLogValue(sessionID),
		sanitizeLogValue(trimmed),
	)
}

func mergeEnv(base []string, extra []string) []string {
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

func signalProcessGroup(cmd *exec.Cmd, signal syscall.Signal) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}

	err := syscall.Kill(-cmd.Process.Pid, signal)
	if ignoreExpectedSignalProcessGroupError(err) {
		return nil
	}
	return err
}

func ignoreExpectedSignalProcessGroupError(err error) bool {
	if err == nil {
		return true
	}
	return errors.Is(err, os.ErrProcessDone) || errors.Is(err, syscall.ESRCH) || errors.Is(err, syscall.EPERM)
}

func ignoreExpectedExit(err error) error {
	if err == nil {
		return nil
	}

	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ProcessState == nil {
		return err
	}

	waitStatus, ok := exitErr.Sys().(syscall.WaitStatus)
	if !ok || !waitStatus.Signaled() {
		return err
	}

	switch waitStatus.Signal() {
	case syscall.SIGINT, syscall.SIGTERM, syscall.SIGKILL:
		return nil
	default:
		return err
	}
}

func logProcessExit(rawWaitErr error) {
	details := processExitLogDetails(rawWaitErr)
	if details != "" {
		log.Printf("acp process exited: %s", details)
		return
	}

	var exitErr *exec.ExitError
	if !errors.As(rawWaitErr, &exitErr) || exitErr.ProcessState == nil {
		log.Printf("acp process exited: err=%v", rawWaitErr)
		return
	}

	waitStatus, ok := exitErr.Sys().(syscall.WaitStatus)
	if ok && waitStatus.Signal() != 0 {
		log.Printf("acp process exited")
		return
	}
	log.Printf("acp process exited: err=%v", rawWaitErr)
}

func processExitLogDetails(rawWaitErr error) string {
	if rawWaitErr == nil {
		return "exit_code=0"
	}

	var exitErr *exec.ExitError
	if !errors.As(rawWaitErr, &exitErr) || exitErr.ProcessState == nil {
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

func defaultPermissionHandler(_ context.Context, request PermissionRequest) (PermissionDecision, error) {
	return PermissionDecision{
		Allow:    true,
		OptionID: pickPermissionOptionID(true, request.Options),
	}, nil
}

func pickPermissionOptionID(allow bool, options []PermissionOption) string {
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

func buildPermissionResult(decision PermissionDecision) map[string]any {
	if !decision.Allow && decision.OptionID == "" {
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
			"optionId": decision.OptionID,
		},
	}
}
