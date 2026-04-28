package agent

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/hzj629206/assistant/agent/claudecode"
)

type claudeRunnerSession struct {
	runner          *ClaudeCodeRunner
	conversationKey string
	sessionID       string

	mu          sync.Mutex
	session     claudecode.Session
	control     *claudeControlServer
	currentTurn *claudeScheduledTurn
	closed      bool
}

type claudeScheduledTurn struct {
	session     *claudeRunnerSession
	req         TurnRequest
	turn        claudecode.Turn
	promptLen   int
	imageCount  int
	interrupted atomic.Bool
	runCalled   atomic.Bool
}

func (s *claudeRunnerSession) ID() string {
	if s == nil {
		return ""
	}
	return s.currentSessionID()
}

func (s *claudeRunnerSession) interruptCurrentTurn(ctx context.Context) error {
	if s == nil || s.runner == nil {
		return nil
	}
	s.mu.Lock()
	currentTurn := s.currentTurn
	s.mu.Unlock()
	if currentTurn != nil {
		return currentTurn.Interrupt(ctx)
	}
	return nil
}

func (s *claudeRunnerSession) ScheduleTurn(ctx context.Context, req TurnRequest) (Turn, error) {
	if s == nil || s.runner == nil {
		return nil, errors.New("run claude code turn failed: session is nil")
	}
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return nil, errors.New("run claude code turn failed: session is closed")
	}

	req, err := normalizeSessionTurnRequest(req, s.conversationKey, s.currentSessionID())
	if err != nil {
		return nil, fmt.Errorf("run claude code turn failed: %w", err)
	}
	turn, promptLen, imageCount, err := s.scheduleClaudeTurn(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("run claude code turn failed: %w", err)
	}
	scheduledTurn := &claudeScheduledTurn{session: s, req: req, turn: turn, promptLen: promptLen, imageCount: imageCount}
	s.mu.Lock()
	s.currentTurn = scheduledTurn
	s.mu.Unlock()
	return scheduledTurn, nil
}

func (t *claudeScheduledTurn) Run(ctx context.Context) (TurnResult, error) {
	s := t.session
	req := t.req
	if s == nil || s.runner == nil || t.turn == nil {
		return TurnResult{}, errors.New("run claude code turn failed: session is nil")
	}
	if !t.runCalled.CompareAndSwap(false, true) {
		return TurnResult{}, errors.New("run claude code turn failed: turn run already started")
	}
	defer t.session.clearCurrentTurn(t)
	_, tools := s.runner.globalContext()
	log.Printf("claude code session executing turn: conversation=%s session_id=%s tool_count=%d", s.conversationKey, s.currentSessionID(), len(tools))
	stopTyping := startTyping(ctx, req.Message.Responder)
	defer stopTyping()

	if t.interrupted.Load() {
		return TurnResult{}, fmt.Errorf("run claude code turn failed: %w", context.Canceled)
	}
	result, err := s.runClaudeTurn(ctx, req, t.turn, t.promptLen, t.imageCount)
	if err != nil {
		return TurnResult{}, fmt.Errorf("run claude code turn failed: %w", err)
	}
	if t.interrupted.Load() {
		return TurnResult{}, fmt.Errorf("run claude code turn failed: %w", context.Canceled)
	}
	s.setSessionID(claudecode.ResolveSessionID(s.currentSessionID(), result))
	return TurnResult{
		RunnerThreadID: s.currentSessionID(),
		ReplyText:      result.Result,
	}, nil
}

//nolint:contextcheck // Interrupt accepts a caller-owned context for cancellation while waiting on the active turn.
func (t *claudeScheduledTurn) Interrupt(ctx context.Context) error {
	if t == nil || t.turn == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	t.interrupted.Store(true)
	err := t.turn.Interrupt(ctx)
	if err != nil {
		return mapClaudeSessionError(err)
	}
	done := t.turn.Done()
	if done == nil {
		return nil
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (t *claudeScheduledTurn) Done() <-chan struct{} {
	if t == nil || t.turn == nil {
		return nil
	}
	return t.turn.Done()
}

func (s *claudeRunnerSession) Close() error {
	if s == nil {
		return nil
	}

	session := s.closeState()
	if session == nil {
		return nil
	}
	return session.Close()
}

func (s *claudeRunnerSession) Status(context.Context) (SessionStatus, error) {
	if s == nil || s.runner == nil {
		return SessionStatus{}, nil
	}

	directories := make([]string, 0, 1+len(s.runner.runOptions.AddDirectories))
	if s.runner.runOptions.WorkingDirectory != "" {
		directories = append(directories, s.runner.runOptions.WorkingDirectory)
	}
	directories = append(directories, s.runner.runOptions.AddDirectories...)
	model := strings.TrimSpace(s.runner.runOptions.ModelAlias)
	if model == "" {
		model = strings.TrimSpace(s.runner.runOptions.Model)
	}
	return SessionStatus{
		Agent:              "claude",
		WorkingDirectories: directories,
		Modes: SessionModes{
			CurrentModeID: string(s.runner.runOptions.PermissionMode),
		},
		ConfigOptions: statusConfigOptions(
			SessionConfigOption{
				Name:         "model",
				CurrentValue: model,
			},
			SessionConfigOption{
				Name:         "effort",
				CurrentValue: strings.TrimSpace(string(s.runner.runOptions.Effort)),
			},
		),
	}, nil
}

func (s *claudeRunnerSession) runClaudeTurn(ctx context.Context, req TurnRequest, turn claudecode.Turn, promptLen int, imageCount int) (*claudecode.ClaudeResult, error) {
	if s == nil || s.runner == nil {
		return nil, errors.New("claude code session is nil")
	}

	session, err := s.currentSession()
	if err != nil {
		return nil, err
	}
	turnCtx, cancelTurn := joinRunnerContext(ctx, s.runner.lifecycleCtx)
	defer cancelTurn()
	if s.control != nil {
		s.control.bindTurn(turnCtx, req)
		defer s.control.clearTurnContext()
	}
	log.Printf(
		"claude code runner using session process: conversation=%s session_id=%s prompt_len=%d image_count=%d",
		req.Conversation.Key,
		session.SessionID(),
		promptLen,
		imageCount,
	)
	result, err := turn.Run(turnCtx)
	if err != nil {
		return nil, mapClaudeSessionError(err)
	}
	s.setSessionID(claudecode.ResolveSessionID(s.currentSessionID(), result))
	log.Printf(
		"claude code runner completed turn: conversation=%s session_id=%s result_len=%d",
		req.Conversation.Key,
		result.SessionID,
		len(result.Result),
	)
	return result, nil
}

func (s *claudeRunnerSession) currentSession() (claudecode.Session, error) {
	if s == nil || s.runner == nil {
		return nil, errors.New("claude code session is nil")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errors.New("claude code session is closed")
	}
	if s.session == nil {
		return nil, errors.New("claude code session is nil")
	}
	return s.session, nil
}

func (s *claudeRunnerSession) closeState() claudecode.Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	session := s.session
	s.session = nil
	return session
}

func (s *claudeRunnerSession) scheduleClaudeTurn(ctx context.Context, req TurnRequest) (claudecode.Turn, int, int, error) {
	if s == nil || s.runner == nil {
		return nil, 0, 0, errors.New("claude code session is nil")
	}
	prompt, imagePaths := s.runner.buildTurnPrompt(req)
	blocks, err := claudecode.BuildUserContentBlocks(prompt, imagePaths)
	if err != nil {
		return nil, 0, 0, err
	}
	session, err := s.currentSession()
	if err != nil {
		return nil, 0, 0, err
	}
	turn, err := session.ScheduleTurn(ctx, blocks)
	if err != nil {
		return nil, 0, 0, err
	}
	return turn, len(prompt), len(imagePaths), nil
}

func (s *claudeRunnerSession) clearCurrentTurn(turn *claudeScheduledTurn) {
	if s == nil || turn == nil {
		return
	}
	s.mu.Lock()
	if s.currentTurn == turn {
		s.currentTurn = nil
	}
	s.mu.Unlock()
}

func (s *claudeRunnerSession) currentSessionIDLocked() string {
	if s.session != nil {
		if current := strings.TrimSpace(s.session.SessionID()); current != "" {
			return current
		}
	}
	return strings.TrimSpace(s.sessionID)
}

func (s *claudeRunnerSession) currentSessionID() string {
	if s == nil {
		return ""
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	return s.currentSessionIDLocked()
}

func (s *claudeRunnerSession) setSessionID(sessionID string) {
	if s == nil {
		return
	}

	s.mu.Lock()
	s.sessionID = strings.TrimSpace(sessionID)
	s.mu.Unlock()
}

func mapClaudeSessionError(err error) error {
	if err == nil {
		return nil
	}
	return err
}
