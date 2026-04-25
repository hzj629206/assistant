package agent

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/godeps/codex-sdk-go"
)

type codexRunnerSession struct {
	runner                *CodexRunner
	conversationKey       string
	threadID              string
	thread                codexThread
	mu                    sync.Mutex
	pendingInitialContext bool
	activeTurn            *codexRunnerActiveTurn
	closed                atomic.Bool
}

func (s *codexRunnerSession) ID() string { return strings.TrimSpace(s.threadID) }

func (s *codexRunnerSession) RunTurn(ctx context.Context, req TurnRequest) (TurnResult, error) {
	if s == nil || s.runner == nil {
		return TurnResult{}, errors.New("run codex turn failed: session is nil")
	}
	if s.closed.Load() {
		return TurnResult{}, errors.New("run codex turn failed: session is closed")
	}
	if s.runner.isClosed() {
		return TurnResult{}, errors.New("run codex turn failed: runner is closed")
	}

	req, err := normalizeSessionTurnRequest(req, s.conversationKey, s.threadID)
	if err != nil {
		return TurnResult{}, fmt.Errorf("run codex turn failed: %w", err)
	}

	prompts, tools := s.runner.globalContext()
	turnContext := codexTurnContext{
		prompts: prompts,
		tools:   tools,
	}

	s.mu.Lock()
	thread := s.thread
	pendingInitialContext := s.pendingInitialContext
	s.mu.Unlock()
	if thread == nil {
		return TurnResult{}, errors.New("run codex turn failed: thread is nil")
	}
	log.Printf(
		"codex session thread ready: conversation=%s thread_id=%s",
		s.conversationKey,
		thread.ID(),
	)

	inputReq := req
	if pendingInitialContext {
		inputReq.Conversation.RunnerThreadID = ""
	}

	input, err := s.runner.buildTurnInputWithContext(inputReq, turnContext)
	if err != nil {
		return TurnResult{}, fmt.Errorf("run codex turn failed: %w", err)
	}
	s.mu.Lock()
	s.pendingInitialContext = false
	s.mu.Unlock()

	turnCtx, releaseTurnCtx := joinRunnerContext(ctx, s.runner.lifecycleCtx)
	defer releaseTurnCtx()
	activeTurn, err := s.startTurn(thread.ID(), releaseTurnCtx)
	if err != nil {
		return TurnResult{}, fmt.Errorf("run codex turn failed: %w", err)
	}
	defer s.finishTurn(activeTurn, thread.ID())

	stopTyping := startTyping(ctx, req.Message.Responder)
	defer stopTyping()

	var replyText string
	if len(tools) == 0 {
		log.Printf("codex session executing turn: conversation=%s mode=direct", s.conversationKey)
		turn, runErr := s.runThreadTurn(req, thread, input, codex.TurnOptions{Context: turnCtx})
		if runErr != nil {
			return TurnResult{}, fmt.Errorf("run codex turn failed: %w", runErr)
		}
		replyText = turn.FinalResponse
	} else {
		log.Printf("codex session executing turn: conversation=%s mode=tool_loop tool_count=%d", s.conversationKey, len(tools))
		replyText, err = s.runToolLoopWithContext(turnCtx, req, thread, input, turnContext)
		if err != nil {
			return TurnResult{}, fmt.Errorf("run codex turn failed: %w", err)
		}
	}
	if activeTurn.interrupted.Load() {
		return TurnResult{}, fmt.Errorf("run codex turn failed: %w", context.Canceled)
	}

	if threadID := strings.TrimSpace(thread.ID()); threadID != "" {
		s.threadID = threadID
	}
	return TurnResult{
		RunnerThreadID: s.threadID,
		ReplyText:      replyText,
	}, nil
}

//nolint:contextcheck // Interrupt accepts a caller-owned context for cancellation while waiting on the active turn.
func (s *codexRunnerSession) Interrupt(ctx context.Context) error {
	if s == nil || s.runner == nil {
		return nil
	}

	activeTurn, threadID, ok := s.activeSessionTurn()
	if !ok {
		return nil
	}

	log.Printf("codex session interrupt requested: conversation=%s thread_id=%s", s.conversationKey, threadID)
	activeTurn.interrupted.Store(true)
	if activeTurn.requestInterrupt != nil {
		activeTurn.requestInterrupt()
	}
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-activeTurn.done:
		log.Printf("codex session interrupt completed: conversation=%s thread_id=%s", s.conversationKey, threadID)
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *codexRunnerSession) Close() error {
	if s == nil || s.runner == nil {
		return nil
	}
	if s.closed.Swap(true) {
		return nil
	}
	activeTurn, _, ok := s.activeSessionTurn()
	if !ok {
		return nil
	}
	if activeTurn.requestInterrupt != nil {
		activeTurn.requestInterrupt()
	}
	<-activeTurn.done
	return nil
}

func (s *codexRunnerSession) Status(context.Context) (SessionStatus, error) {
	if s == nil || s.runner == nil {
		return SessionStatus{}, nil
	}

	directories := make([]string, 0, 1+len(s.runner.threadOptions.AdditionalDirectories))
	if s.runner.threadOptions.WorkingDirectory != "" {
		directories = append(directories, s.runner.threadOptions.WorkingDirectory)
	}
	directories = append(directories, s.runner.threadOptions.AdditionalDirectories...)

	return SessionStatus{
		Agent:              "codex",
		WorkingDirectories: directories,
		Modes: SessionModes{
			CurrentModeID: formatRunnerMode(string(s.runner.threadOptions.SandboxMode), string(s.runner.threadOptions.ApprovalPolicy)),
		},
		ConfigOptions: statusConfigOptions(
			SessionConfigOption{
				Name:         "model",
				CurrentValue: strings.TrimSpace(s.runner.threadOptions.Model),
			},
			SessionConfigOption{
				Name:         "effort",
				CurrentValue: strings.TrimSpace(string(s.runner.threadOptions.ModelReasoningEffort)),
			},
		),
	}, nil
}

func (*codexRunnerSession) Commands() []CommandSpec { return nil }

func (*codexRunnerSession) HandleCommand(context.Context, SlashCommand) (string, error) {
	return "", errors.New("unsupported slash command")
}

type codexRunnerActiveTurn struct {
	cancel           func()
	requestInterrupt func()
	done             chan struct{}
	interrupted      atomic.Bool
}

func (s *codexRunnerSession) activeSessionTurn() (*codexRunnerActiveTurn, string, bool) {
	if s == nil {
		return nil, "", false
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.activeTurn == nil {
		return nil, "", false
	}
	return s.activeTurn, s.threadID, true
}

func (s *codexRunnerSession) startTurn(threadID string, cancel func()) (*codexRunnerActiveTurn, error) {
	if s == nil {
		return nil, errors.New("session is nil")
	}

	activeTurn := &codexRunnerActiveTurn{
		cancel:           cancel,
		requestInterrupt: cancel,
		done:             make(chan struct{}),
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed.Load() {
		return nil, errors.New("session is closed")
	}
	if s.activeTurn != nil {
		return nil, ErrSessionBusy
	}
	if trimmedThreadID := strings.TrimSpace(threadID); trimmedThreadID != "" {
		s.threadID = trimmedThreadID
	}
	s.activeTurn = activeTurn

	return activeTurn, nil
}

func (s *codexRunnerSession) finishTurn(activeTurn *codexRunnerActiveTurn, threadID string) {
	if activeTurn == nil {
		return
	}

	if s != nil {
		s.mu.Lock()
		if trimmedThreadID := strings.TrimSpace(threadID); trimmedThreadID != "" {
			s.threadID = trimmedThreadID
		}
		if s.activeTurn == activeTurn {
			s.activeTurn = nil
		}
		s.mu.Unlock()
	}

	close(activeTurn.done)
}

func (s *codexRunnerSession) runThreadTurn(req TurnRequest, thread codexThread, input codex.Input, options codex.TurnOptions) (codex.Turn, error) {
	log.Printf("codex runner started streamed turn: conversation=%s thread_id=%s", req.Conversation.Key, thread.ID())
	streamed, err := thread.RunStreamed(input, options)
	if err != nil {
		return codex.Turn{}, err
	}

	return s.collectStreamedTurn(req, streamed)
}

func (s *codexRunnerSession) collectStreamedTurn(req TurnRequest, streamed *codex.StreamedTurn) (codex.Turn, error) {
	if streamed == nil {
		return codex.Turn{}, errors.New("streamed turn is nil")
	}

	var items []codex.ThreadItem
	var finalResponse string
	var usage *codex.Usage
	var turnFailure *codex.ThreadError

	for event := range streamed.Events {
		switch event.Type {
		case "thread.started":
			log.Printf(
				"codex runner thread started: conversation=%s thread_id=%s",
				req.Conversation.Key,
				event.ThreadID,
			)
		case "item.completed":
			if event.Item != nil {
				logCodexCompletedItem(req, event.Item)
				items = append(items, event.Item)
				if msg, ok := event.Item.(*codex.AgentMessageItem); ok {
					finalResponse = msg.Text
				}
			}
		case "turn.completed":
			usage = event.Usage
			if usage != nil {
				log.Printf(
					"codex runner turn usage: conversation=%s input_tokens=%d cached_input_tokens=%d output_tokens=%d",
					req.Conversation.Key,
					usage.InputTokens,
					usage.CachedInputTokens,
					usage.OutputTokens,
				)
			}
		case "turn.failed":
			turnFailure = event.Error
			if turnFailure != nil {
				log.Printf(
					"codex runner turn failed event: conversation=%s err=%s",
					req.Conversation.Key,
					turnFailure.Message,
				)
			}
		case "error":
			log.Printf(
				"codex runner stream error event: conversation=%s message=%s",
				req.Conversation.Key,
				strings.TrimSpace(event.Message),
			)
		}
	}

	if err := <-streamed.Done; err != nil {
		return codex.Turn{}, err
	}
	if turnFailure != nil {
		return codex.Turn{}, errors.New(turnFailure.Message)
	}
	log.Printf(
		"codex runner completed streamed turn: conversation=%s items=%d final_response_len=%d",
		req.Conversation.Key,
		len(items),
		len(finalResponse),
	)

	return codex.Turn{
		Items:         items,
		FinalResponse: finalResponse,
		Usage:         usage,
	}, nil
}
