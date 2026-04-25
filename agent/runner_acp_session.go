package agent

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/hzj629206/assistant/agent/acp"
)

type acpRunnerSession struct {
	runner          *ACPRunner
	conversationKey string
	sessionID       string

	mu                          sync.Mutex
	session                     acp.Session
	token                       string
	active                      *interruptibleRunnerTurn
	pendingInitialSystemPrompts bool
	closed                      bool
}

type interruptibleRunnerTurn struct {
	done        chan struct{}
	interrupted atomic.Bool
}

func (s *acpRunnerSession) ID() string { return strings.TrimSpace(s.sessionID) }

func (s *acpRunnerSession) RunTurn(ctx context.Context, req TurnRequest) (TurnResult, error) {
	if s == nil || s.runner == nil {
		return TurnResult{}, errors.New("run acp turn failed: session is nil")
	}

	req, err := s.normalizeTurnRequest(req)
	if err != nil {
		return TurnResult{}, fmt.Errorf("run acp turn failed: %w", err)
	}

	prompts, tools := s.runner.globalContext()
	session, token, err := s.currentSessionState()
	if err != nil {
		return TurnResult{}, fmt.Errorf("run acp turn failed: %w", err)
	}
	active, err := s.beginActiveTurn()
	if err != nil {
		return TurnResult{}, fmt.Errorf("run acp turn failed: %w", err)
	}
	defer s.endActiveTurn(active)

	initialPromptBlocks := []string(nil)
	if s.consumeInitialSystemPrompts() {
		initialPromptBlocks = prompts
	}
	promptBlocks, err := buildACPPromptBlocks(initialPromptBlocks, req.Message, session.Capabilities().Prompt)
	if err != nil {
		return TurnResult{}, fmt.Errorf("run acp turn failed: build prompt blocks: %w", err)
	}
	s.runner.registerToolCallContext(token, req)
	defer s.runner.unregisterToolCallContext(token)

	log.Printf(
		"acp session ready: conversation=%s requested_session=%s actual_session=%s tool_count=%d",
		s.conversationKey,
		s.sessionID,
		session.SessionID(),
		len(tools),
	)

	stopTyping := startTyping(ctx, req.Message.Responder)
	defer stopTyping()

	turnCtx, cancelTurn := joinRunnerContext(ctx, s.runner.lifecycleCtx)
	defer cancelTurn()

	turnResult, err := session.RunTurn(turnCtx, promptBlocks)
	if err != nil {
		return TurnResult{}, fmt.Errorf("run acp turn failed: %w", mapACPSessionError(err))
	}
	if active.interrupted.Load() {
		return TurnResult{}, fmt.Errorf("run acp turn failed: %w", context.Canceled)
	}
	s.sessionID = strings.TrimSpace(turnResult.SessionID)
	return TurnResult{
		RunnerThreadID: s.sessionID,
		ReplyText:      turnResult.ReplyText,
	}, nil
}

func (s *acpRunnerSession) normalizeTurnRequest(req TurnRequest) (TurnRequest, error) {
	if s == nil {
		return req, errors.New("acp session is nil")
	}
	return normalizeSessionTurnRequest(req, s.conversationKey, s.sessionID)
}

//nolint:contextcheck // Interrupt accepts a caller-owned context for cancellation while waiting on the active turn.
func (s *acpRunnerSession) Interrupt(ctx context.Context) error {
	if s == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}

	session, sessionID, err := s.currentSessionForInterrupt()
	if err != nil {
		return err
	}
	if session == nil {
		return nil
	}
	s.markActiveTurnInterrupted()
	log.Printf("acp session interrupt requested: conversation=%s session_id=%s", s.conversationKey, sessionID)
	err = session.Interrupt(ctx)
	if err != nil {
		return mapACPSessionError(err)
	}
	log.Printf("acp session interrupt completed: conversation=%s session_id=%s", s.conversationKey, sessionID)
	return nil
}

func (s *acpRunnerSession) beginActiveTurn() (*interruptibleRunnerTurn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errors.New("session is closed")
	}
	if s.active != nil {
		return nil, ErrSessionBusy
	}
	active := &interruptibleRunnerTurn{done: make(chan struct{})}
	s.active = active
	return active, nil
}

func (s *acpRunnerSession) endActiveTurn(active *interruptibleRunnerTurn) {
	if s == nil || active == nil {
		return
	}
	s.mu.Lock()
	if s.active == active {
		s.active = nil
	}
	s.mu.Unlock()
	close(active.done)
}

func (s *acpRunnerSession) markActiveTurnInterrupted() {
	if s == nil {
		return
	}
	s.mu.Lock()
	active := s.active
	s.mu.Unlock()
	if active != nil {
		active.interrupted.Store(true)
	}
}

func (s *acpRunnerSession) Close() error {
	if s == nil {
		return nil
	}

	session, token := s.closeState()
	if s.runner != nil && token != "" {
		s.runner.unregisterToolCallContext(token)
	}
	if session == nil {
		return nil
	}
	return session.Close()
}

func (s *acpRunnerSession) Status(context.Context) (SessionStatus, error) {
	if s == nil || s.runner == nil {
		return SessionStatus{}, nil
	}

	directories := make([]string, 0, 1)
	if s.runner.workDir != "" {
		directories = append(directories, s.runner.workDir)
	}
	status := SessionStatus{
		Agent:              "acp",
		WorkingDirectories: directories,
	}

	session, _, err := s.currentSessionState()
	if err != nil {
		return status, err
	}
	if session == nil {
		return status, nil
	}

	metadata := session.Status()
	status.Modes = SessionModes{
		CurrentModeID: strings.TrimSpace(metadata.Modes.CurrentModeID),
	}
	if len(metadata.Modes.AvailableModes) != 0 {
		status.Modes.AvailableModes = make([]SessionMode, 0, len(metadata.Modes.AvailableModes))
		for _, mode := range metadata.Modes.AvailableModes {
			status.Modes.AvailableModes = append(status.Modes.AvailableModes, SessionMode{
				ID:    strings.TrimSpace(mode.ID),
				Label: strings.TrimSpace(mode.Label),
			})
		}
	}
	status.ConfigOptions = parseStatusConfigOptions(metadata.ConfigOptions)
	return status, nil
}

func (*acpRunnerSession) Commands() []CommandSpec { return nil }

func (*acpRunnerSession) HandleCommand(context.Context, SlashCommand) (string, error) {
	return "", errors.New("unsupported slash command")
}

func (s *acpRunnerSession) currentSessionState() (acp.Session, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, "", errors.New("session is closed")
	}
	if s.session == nil {
		return nil, "", errors.New("session is nil")
	}
	return s.session, s.token, nil
}

func (s *acpRunnerSession) currentSessionForInterrupt() (acp.Session, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, "", nil
	}
	if s.session == nil {
		return nil, "", nil
	}
	return s.session, s.currentSessionIDLocked(), nil
}

func (s *acpRunnerSession) closeState() (acp.Session, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ""
	}
	s.closed = true
	session := s.session
	token := s.token
	s.session = nil
	s.token = ""
	return session, token
}

func (s *acpRunnerSession) currentSessionIDLocked() string {
	if s.session != nil {
		if current := strings.TrimSpace(s.session.SessionID()); current != "" {
			return current
		}
	}
	return strings.TrimSpace(s.sessionID)
}

func (s *acpRunnerSession) consumeInitialSystemPrompts() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.pendingInitialSystemPrompts {
		return false
	}
	s.pendingInitialSystemPrompts = false
	return true
}

func mapACPSessionError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, acp.ErrSessionBusy) {
		return ErrSessionBusy
	}
	return err
}

func (r *ACPRunner) isAuthorizedACPToken(token string) bool {
	r.runtimeMu.Lock()
	defer r.runtimeMu.Unlock()
	_, ok := r.toolCallTokens[token]
	return ok
}

func (r *ACPRunner) reserveToolCallToken(token string) {
	r.toolCallTokens[token] = toolCallTokenState{}
}

func (r *ACPRunner) releaseReservedToolCallToken(token string) {
	if r == nil || strings.TrimSpace(token) == "" {
		return
	}

	r.runtimeMu.Lock()
	state, ok := r.toolCallTokens[token]
	if ok && state.req == nil {
		delete(r.toolCallTokens, token)
	}
	r.runtimeMu.Unlock()
}

func (r *ACPRunner) registerToolCallContext(token string, req TurnRequest) {
	if r == nil || strings.TrimSpace(token) == "" {
		return
	}

	copied := req
	r.runtimeMu.Lock()
	r.toolCallTokens[token] = toolCallTokenState{req: &copied}
	r.runtimeMu.Unlock()
}

func (r *ACPRunner) unregisterToolCallContext(token string) {
	if r == nil || strings.TrimSpace(token) == "" {
		return
	}

	r.runtimeMu.Lock()
	delete(r.toolCallTokens, token)
	r.runtimeMu.Unlock()
}

func (r *ACPRunner) acpHTTPTools(token string) []acp.HTTPTool {
	if r == nil || strings.TrimSpace(token) == "" || !r.isAuthorizedACPToken(token) {
		return nil
	}

	_, tools := r.globalContext()
	httpTools := make([]acp.HTTPTool, 0, len(tools))
	for _, tool := range tools {
		if tool == nil {
			continue
		}
		httpTools = append(httpTools, tool)
	}
	return httpTools
}
