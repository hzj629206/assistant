package agent

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"

	"github.com/hzj629206/assistant/agent/claudecode"
)

type claudeRunnerSession struct {
	runner          *ClaudeCodeRunner
	conversationKey string
	sessionID       string

	mu      sync.Mutex
	session claudecode.Session
	control *claudeControlServer
	active  *interruptibleRunnerTurn
	closed  bool
}

func (s *claudeRunnerSession) ID() string { return strings.TrimSpace(s.sessionID) }

func (s *claudeRunnerSession) RunTurn(ctx context.Context, req TurnRequest) (TurnResult, error) {
	if s == nil || s.runner == nil {
		return TurnResult{}, errors.New("run claude code turn failed: session is nil")
	}

	req, err := normalizeSessionTurnRequest(req, s.conversationKey, s.sessionID)
	if err != nil {
		return TurnResult{}, fmt.Errorf("run claude code turn failed: %w", err)
	}

	prompt, imagePaths := s.runner.buildTurnPrompt(req)
	_, tools := s.runner.globalContext()
	log.Printf("claude code session executing turn: conversation=%s session_id=%s tool_count=%d", s.conversationKey, s.sessionID, len(tools))
	active, err := s.beginActiveTurn()
	if err != nil {
		return TurnResult{}, fmt.Errorf("run claude code turn failed: %w", err)
	}
	defer s.endActiveTurn(active)
	stopTyping := startTyping(ctx, req.Message.Responder)
	defer stopTyping()

	result, err := s.runClaudeTurn(ctx, req, prompt, imagePaths)
	if err != nil {
		return TurnResult{}, fmt.Errorf("run claude code turn failed: %w", err)
	}
	if active.interrupted.Load() {
		return TurnResult{}, fmt.Errorf("run claude code turn failed: %w", context.Canceled)
	}
	s.sessionID = claudecode.ResolveSessionID(s.sessionID, result)
	return TurnResult{
		RunnerThreadID: s.sessionID,
		ReplyText:      result.Result,
	}, nil
}

func (s *claudeRunnerSession) Interrupt(ctx context.Context) error {
	if s == nil || s.runner == nil {
		return nil
	}

	session, sessionID, err := s.currentSessionForInterrupt()
	if err != nil {
		return err
	}
	if session == nil {
		return nil
	}
	s.markActiveTurnInterrupted()
	log.Printf("claude code session interrupt requested: conversation=%s session_id=%s", s.conversationKey, sessionID)
	err = session.Interrupt(ctx)
	if err != nil {
		return mapClaudeSessionError(err)
	}
	log.Printf("claude code session interrupt completed: conversation=%s session_id=%s", s.conversationKey, sessionID)
	return nil
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

func (s *claudeRunnerSession) runClaudeTurn(ctx context.Context, req TurnRequest, prompt string, imagePaths []string) (*claudecode.ClaudeResult, error) {
	if s == nil || s.runner == nil {
		return nil, errors.New("claude code session is nil")
	}

	blocks, err := claudecode.BuildUserContentBlocks(prompt, imagePaths)
	if err != nil {
		return nil, err
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
		len(prompt),
		len(imagePaths),
	)
	result, err := session.RunTurn(turnCtx, blocks)
	if err != nil {
		return nil, mapClaudeSessionError(err)
	}
	s.mu.Lock()
	s.sessionID = claudecode.ResolveSessionID(s.sessionID, result)
	s.mu.Unlock()
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

func (s *claudeRunnerSession) currentSessionForInterrupt() (claudecode.Session, string, error) {
	if s == nil || s.runner == nil {
		return nil, "", nil
	}

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

func (s *claudeRunnerSession) beginActiveTurn() (*interruptibleRunnerTurn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errors.New("claude code session is closed")
	}
	if s.active != nil {
		return nil, ErrSessionBusy
	}
	active := &interruptibleRunnerTurn{done: make(chan struct{})}
	s.active = active
	return active, nil
}

func (s *claudeRunnerSession) endActiveTurn(active *interruptibleRunnerTurn) {
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

func (s *claudeRunnerSession) markActiveTurnInterrupted() {
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

func (s *claudeRunnerSession) currentSessionIDLocked() string {
	if s.session != nil {
		if current := strings.TrimSpace(s.session.SessionID()); current != "" {
			return current
		}
	}
	return strings.TrimSpace(s.sessionID)
}

func mapClaudeSessionError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, claudecode.ErrSessionBusy) {
		return ErrSessionBusy
	}
	return err
}
