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
	pendingInitialSystemPrompts bool
	currentTurn                 *acpScheduledTurn
	closed                      bool
}

type acpScheduledTurn struct {
	session     *acpRunnerSession
	req         TurnRequest
	turn        acp.ScheduledTurn
	interrupted atomic.Bool
}

func (s *acpRunnerSession) ID() string {
	if s == nil {
		return ""
	}
	return s.currentSessionID()
}

func (s *acpRunnerSession) interruptCurrentTurn(ctx context.Context) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	currentTurn := s.currentTurn
	s.mu.Unlock()
	if currentTurn != nil {
		return currentTurn.Interrupt(ctx)
	}
	session, _, err := s.currentSessionForInterrupt()
	if err != nil || session == nil {
		return err
	}
	return mapACPSessionError(session.Interrupt(ctx))
}

func (s *acpRunnerSession) ScheduleTurn(ctx context.Context, req TurnRequest) (ScheduledTurn, error) {
	if s == nil || s.runner == nil {
		return nil, errors.New("run acp turn failed: session is nil")
	}
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return nil, errors.New("run acp turn failed: session is closed")
	}

	req, err := s.normalizeTurnRequest(req)
	if err != nil {
		return nil, fmt.Errorf("run acp turn failed: %w", err)
	}
	underlyingTurn, err := s.scheduleACPturn(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("run acp turn failed: %w", err)
	}
	scheduledTurn := &acpScheduledTurn{session: s, req: req, turn: underlyingTurn}
	s.mu.Lock()
	s.currentTurn = scheduledTurn
	s.mu.Unlock()
	return scheduledTurn, nil
}

func (t *acpScheduledTurn) Run(ctx context.Context) (TurnResult, error) {
	s := t.session
	req := t.req
	if s == nil || s.runner == nil || t.turn == nil {
		return TurnResult{}, errors.New("run acp turn failed: session is nil")
	}
	defer t.session.clearCurrentTurn(t)
	_, tools := s.runner.globalContext()
	session, token, err := s.currentSessionState()
	if err != nil {
		return TurnResult{}, fmt.Errorf("run acp turn failed: %w", err)
	}

	s.runner.registerToolCallContext(token, req)
	defer s.runner.unregisterToolCallContext(token)

	log.Printf(
		"acp session ready: conversation=%s requested_session=%s actual_session=%s tool_count=%d",
		s.conversationKey,
		s.currentSessionID(),
		session.SessionID(),
		len(tools),
	)

	stopTyping := startTyping(ctx, req.Message.Responder)
	defer stopTyping()

	turnCtx, cancelTurn := joinRunnerContext(ctx, s.runner.lifecycleCtx)
	defer cancelTurn()

	if t.interrupted.Load() {
		return TurnResult{}, fmt.Errorf("run acp turn failed: %w", context.Canceled)
	}
	turnResult, err := t.turn.Run(turnCtx)
	if err != nil {
		return TurnResult{}, fmt.Errorf("run acp turn failed: %w", mapACPSessionError(err))
	}
	if t.interrupted.Load() {
		return TurnResult{}, fmt.Errorf("run acp turn failed: %w", context.Canceled)
	}
	s.setSessionID(turnResult.SessionID)
	return TurnResult{
		RunnerThreadID: s.currentSessionID(),
		ReplyText:      turnResult.ReplyText,
	}, nil
}

//nolint:contextcheck // Interrupt accepts a caller-owned context for cancellation while waiting on the active turn.
func (t *acpScheduledTurn) Interrupt(ctx context.Context) error {
	s := t.session
	if s == nil || t.turn == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	t.interrupted.Store(true)
	err := t.turn.Interrupt(ctx)
	if err != nil {
		return mapACPSessionError(err)
	}
	return nil
}

func (t *acpScheduledTurn) Done() <-chan struct{} {
	if t == nil || t.turn == nil {
		return nil
	}
	return t.turn.Done()
}

func (s *acpRunnerSession) normalizeTurnRequest(req TurnRequest) (TurnRequest, error) {
	if s == nil {
		return req, errors.New("acp session is nil")
	}
	return normalizeSessionTurnRequest(req, s.conversationKey, s.currentSessionID())
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

func (s *acpRunnerSession) scheduleACPturn(ctx context.Context, req TurnRequest) (acp.ScheduledTurn, error) {
	if s == nil {
		return nil, errors.New("acp session is nil")
	}
	prompts, _ := s.runner.globalContext()
	session, _, err := s.currentSessionState()
	if err != nil {
		return nil, err
	}
	initialPromptBlocks := []string(nil)
	if s.hasPendingInitialSystemPrompts() {
		initialPromptBlocks = prompts
	}
	promptBlocks, err := buildACPPromptBlocks(initialPromptBlocks, req.Message, session.Capabilities().Prompt)
	if err != nil {
		return nil, fmt.Errorf("build prompt blocks: %w", err)
	}
	turn, err := session.ScheduleTurn(ctx, promptBlocks)
	if err != nil {
		return nil, err
	}
	if len(initialPromptBlocks) != 0 {
		s.consumeInitialSystemPrompts()
	}
	return turn, nil
}

func (s *acpRunnerSession) clearCurrentTurn(turn *acpScheduledTurn) {
	if s == nil || turn == nil {
		return
	}
	s.mu.Lock()
	if s.currentTurn == turn {
		s.currentTurn = nil
	}
	s.mu.Unlock()
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

func (s *acpRunnerSession) currentSessionID() string {
	if s == nil {
		return ""
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	return s.currentSessionIDLocked()
}

func (s *acpRunnerSession) setSessionID(sessionID string) {
	if s == nil {
		return
	}

	s.mu.Lock()
	s.sessionID = strings.TrimSpace(sessionID)
	s.mu.Unlock()
}

func (s *acpRunnerSession) hasPendingInitialSystemPrompts() bool {
	if s == nil {
		return false
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pendingInitialSystemPrompts
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
