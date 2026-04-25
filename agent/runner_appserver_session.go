package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	appcodex "github.com/pmenglund/codex-sdk-go"
	appproto "github.com/pmenglund/codex-sdk-go/protocol"
	apprpc "github.com/pmenglund/codex-sdk-go/rpc"
)

type appServerSession struct {
	runner          *AppServerRunner
	conversationKey string
	threadID        string
	thread          appServerThread
	mu              sync.Mutex
	activeTurn      *appServerActiveTurn
	closed          atomic.Bool
}

type appServerActiveTurn struct {
	req                TurnRequest
	tools              []Tool
	threadID           string
	turnID             string
	done               chan struct{}
	interruptRequested chan struct{}
	interruptDone      chan struct{}
	interruptErr       error
	interruptSent      bool
	interruptFinished  bool
	requestInterrupt   func()
	interrupted        atomic.Bool
}

func (s *appServerSession) ID() string { return strings.TrimSpace(s.threadID) }

func (s *appServerSession) RunTurn(ctx context.Context, req TurnRequest) (TurnResult, error) {
	if s == nil || s.runner == nil {
		return TurnResult{}, errors.New("run app-server turn failed: session is nil")
	}
	if s.closed.Load() {
		return TurnResult{}, errors.New("run app-server turn failed: session is closed")
	}
	var err error
	req, err = normalizeSessionTurnRequest(req, s.conversationKey, s.threadID)
	if err != nil {
		return TurnResult{}, fmt.Errorf("run app-server turn failed: %w", err)
	}

	thread := s.currentThread()
	if thread == nil {
		return TurnResult{}, errors.New("run app-server turn failed: thread is nil")
	}

	turnContext := appServerTurnContext{
		prompts: s.runner.globalPrompts(),
		tools:   s.runner.globalTools(),
	}

	runCtx, releaseRunCtx := joinRunnerContext(ctx, s.runner.lifecycleCtx)
	defer releaseRunCtx()

	inputs := buildAppServerTurnInputs(req)
	stopTyping := startTyping(ctx, req.Message.Responder)
	defer stopTyping()

	log.Printf("app-server session executing turn: conversation=%s mode=direct", s.conversationKey)
	sessionKey := appServerSessionKey(req.Conversation)
	s.runner.registerSession(sessionKey, s)
	threadID := firstNonEmptyString(thread.ID(), s.ID(), req.Conversation.RunnerThreadID, s.conversationKey)
	active, err := s.beginTurn(threadID, "", req, turnContext.tools)
	if err != nil {
		s.runner.unregisterSession(sessionKey, s)
		return TurnResult{}, fmt.Errorf("run app-server turn failed: %w", err)
	}
	defer s.endTurn(threadID, "")

	turn, runErr := s.runThreadTurn(runCtx, req, thread, inputs, &s.runner.turnOptions)
	if runErr != nil {
		return TurnResult{}, fmt.Errorf("run app-server turn failed: %w", runErr)
	}
	if active != nil && active.interrupted.Load() {
		return TurnResult{}, fmt.Errorf("run app-server turn failed: %w", context.Canceled)
	}

	if threadID := strings.TrimSpace(thread.ID()); threadID != "" {
		s.setThreadID(threadID)
	}
	return TurnResult{
		RunnerThreadID: s.ID(),
		ReplyText:      turn.FinalResponse,
	}, nil
}

func (s *appServerSession) Interrupt(ctx context.Context) error {
	if s == nil || s.runner == nil {
		return nil
	}

	s.mu.Lock()
	active := s.activeTurn
	if active == nil {
		s.mu.Unlock()
		return nil
	}
	interruptDone := active.interruptDone
	requestInterrupt := active.requestInterrupt
	s.mu.Unlock()
	if requestInterrupt == nil || interruptDone == nil {
		return errors.New("app-server session interrupt state is incomplete")
	}
	log.Printf("app-server session interrupt requested: conversation=%s", s.conversationKey)
	active.interrupted.Store(true)
	requestInterrupt()
	if interruptDone != nil {
		select {
		case <-interruptDone:
			s.mu.Lock()
			interruptErr := active.interruptErr
			s.mu.Unlock()
			if interruptErr != nil {
				return interruptErr
			}
		case <-active.done:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	select {
	case <-active.done:
		log.Printf("app-server session interrupt completed: conversation=%s", s.conversationKey)
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *appServerSession) Close() error {
	if s == nil || s.runner == nil {
		return nil
	}
	if s.closed.Swap(true) {
		return nil
	}
	s.mu.Lock()
	active := s.activeTurn
	threadID := strings.TrimSpace(s.threadID)
	s.mu.Unlock()
	if active == nil || active.done == nil {
		err := s.unsubscribeThread(threadID)
		s.runner.unregisterSession(s.conversationKey, s)
		return err
	}
	done := active.done
	interruptCtx, cancel := context.WithTimeout(context.Background(), defaultRunnerCloseInterruptTimeout)
	err := s.Interrupt(interruptCtx)
	cancel()
	if err != nil && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) && !errors.Is(err, ErrSessionInterruptUnavailable) {
		return err
	}
	select {
	case <-done:
	case <-time.After(defaultRunnerCloseInterruptTimeout):
	}
	err = s.unsubscribeThread(threadID)
	s.runner.unregisterSession(s.conversationKey, s)
	return err
}

func (s *appServerSession) Status(context.Context) (SessionStatus, error) {
	if s == nil || s.runner == nil {
		return SessionStatus{}, nil
	}

	cwd := strings.TrimSpace(s.runner.turnOptions.Cwd)
	if cwd == "" {
		cwd = strings.TrimSpace(s.runner.resumeOptions.Cwd)
	}
	if cwd == "" {
		cwd = strings.TrimSpace(s.runner.startOptions.Cwd)
	}
	model := strings.TrimSpace(s.runner.turnOptions.Model)
	if model == "" {
		model = strings.TrimSpace(s.runner.resumeOptions.Model)
	}
	if model == "" {
		model = strings.TrimSpace(s.runner.startOptions.Model)
	}
	directories := make([]string, 0, 1)
	if cwd != "" {
		directories = append(directories, cwd)
	}
	return SessionStatus{
		Agent:              "codex-appserver",
		WorkingDirectories: directories,
		Modes: SessionModes{
			CurrentModeID: formatRunnerMode(
				describeAppServerSandboxPolicy(s.runner.turnOptions.SandboxPolicy),
				describeAppServerApprovalPolicy(s.runner.turnOptions.ApprovalPolicy),
			),
		},
		ConfigOptions: statusConfigOptions(
			SessionConfigOption{
				Name:         "model",
				CurrentValue: model,
			},
			SessionConfigOption{
				Name:         "effort",
				CurrentValue: strings.TrimSpace(fmt.Sprint(s.runner.turnOptions.Effort)),
			},
		),
	}, nil
}

func (*appServerSession) Commands() []CommandSpec { return nil }

func (*appServerSession) HandleCommand(context.Context, SlashCommand) (string, error) {
	return "", errors.New("unsupported slash command")
}

func (s *appServerSession) unsubscribeThread(threadID string) error {
	if s == nil || s.runner == nil || threadID == "" {
		return nil
	}

	s.runner.mu.RLock()
	unsubscribeThreadFn := s.runner.unsubscribeThreadFn
	s.runner.mu.RUnlock()
	if unsubscribeThreadFn == nil {
		return nil
	}

	unsubscribeCtx, cancel := context.WithTimeout(context.Background(), defaultRunnerCloseInterruptTimeout)
	defer cancel()
	return unsubscribeThreadFn(unsubscribeCtx, threadID)
}

func (s *appServerSession) currentThread() appServerThread {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.thread
}

func (s *appServerSession) beginTurn(threadID string, turnID string, req TurnRequest, tools []Tool) (*appServerActiveTurn, error) {
	if s == nil || strings.TrimSpace(threadID) == "" {
		return nil, errors.New("app-server session is nil")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed.Load() {
		return nil, errors.New("session is closed")
	}
	if s.activeTurn != nil {
		return nil, ErrSessionBusy
	}

	interruptRequested := make(chan struct{})
	active := &appServerActiveTurn{
		done:               make(chan struct{}),
		interruptRequested: interruptRequested,
		interruptDone:      make(chan struct{}),
	}
	active.requestInterrupt = func() {
		select {
		case <-interruptRequested:
		default:
			close(interruptRequested)
		}
	}
	active.req = req
	active.tools = append([]Tool(nil), tools...)
	active.threadID = threadID
	active.turnID = strings.TrimSpace(turnID)
	s.threadID = threadID
	s.activeTurn = active
	return active, nil
}

func (s *appServerSession) endTurn(threadID string, turnID string) {
	if s == nil || strings.TrimSpace(threadID) == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	active := s.activeTurn
	if active == nil || active.threadID != threadID {
		return
	}
	if turnID != "" && active.turnID != "" && active.turnID != turnID {
		return
	}

	s.activeTurn = nil
	s.completeInterruptLocked(active, nil)
	close(active.done)
}

func (s *appServerSession) updateTurnID(threadID string, turnID string) {
	if s == nil || strings.TrimSpace(threadID) == "" || strings.TrimSpace(turnID) == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	active := s.activeTurn
	if active == nil || active.threadID != threadID {
		return
	}
	active.turnID = turnID
}

func (s *appServerSession) setThreadID(threadID string) {
	if s == nil || strings.TrimSpace(threadID) == "" {
		return
	}
	s.mu.Lock()
	s.threadID = threadID
	s.mu.Unlock()
}

func (s *appServerSession) completeInterruptLocked(active *appServerActiveTurn, err error) {
	if active == nil || active.interruptFinished {
		return
	}
	active.interruptErr = err
	active.interruptFinished = true
	if active.interruptDone == nil {
		active.interruptDone = make(chan struct{})
	}
	close(active.interruptDone)
}

func (s *appServerSession) activeTurnSnapshot() (*appServerActiveTurn, string, bool) {
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

func (s *appServerSession) interruptActiveTurnIfRequested(ctx context.Context, threadID string, turnID string) {
	if s == nil || strings.TrimSpace(threadID) == "" {
		return
	}

	s.mu.Lock()
	active := s.activeTurn
	if active == nil || strings.TrimSpace(active.threadID) != strings.TrimSpace(threadID) {
		s.mu.Unlock()
		return
	}
	if trimmedTurnID := strings.TrimSpace(turnID); trimmedTurnID != "" {
		active.turnID = trimmedTurnID
	}
	select {
	case <-active.interruptRequested:
	default:
		s.mu.Unlock()
		return
	}
	if active.interruptFinished || active.interruptSent || strings.TrimSpace(active.turnID) == "" {
		s.mu.Unlock()
		return
	}
	active.interruptSent = true
	callThreadID := active.threadID
	callTurnID := active.turnID
	s.mu.Unlock()

	s.runner.mu.RLock()
	interruptTurnFn := s.runner.interruptTurnFn
	s.runner.mu.RUnlock()
	if interruptTurnFn == nil {
		s.mu.Lock()
		if current := s.activeTurn; current == active {
			s.completeInterruptLocked(active, ErrSessionInterruptUnavailable)
		}
		s.mu.Unlock()
		return
	}

	log.Printf("app-server session interrupt requested: conversation=%s thread_id=%s turn_id=%s", s.conversationKey, callThreadID, callTurnID)
	interruptCtx, cancel := context.WithTimeout(ctx, defaultRunnerCloseInterruptTimeout)
	err := interruptTurnFn(interruptCtx, callThreadID, callTurnID)
	cancel()

	s.mu.Lock()
	if current := s.activeTurn; current == active {
		s.completeInterruptLocked(active, err)
	}
	s.mu.Unlock()
}

func (s *appServerSession) matchesTurn(threadID string, turnID string) bool {
	if s == nil {
		return false
	}

	trimmedThreadID := strings.TrimSpace(threadID)
	trimmedTurnID := strings.TrimSpace(turnID)

	s.mu.Lock()
	defer s.mu.Unlock()

	active := s.activeTurn
	if active == nil {
		return false
	}
	if strings.TrimSpace(active.threadID) != trimmedThreadID {
		return false
	}
	if trimmedTurnID == "" {
		return true
	}
	activeTurnID := strings.TrimSpace(active.turnID)
	return activeTurnID == "" || activeTurnID == trimmedTurnID
}

func (s *appServerSession) handleItemToolCall(ctx context.Context, params appServerDynamicToolCallParams) (*appproto.DynamicToolCallResponse, error) {
	if s == nil {
		return nil, errors.New("app-server session is nil")
	}

	s.mu.Lock()
	active := s.activeTurn
	if active == nil {
		s.mu.Unlock()
		if params.TurnID == "" {
			return nil, fmt.Errorf("tool call for unknown active thread %q", params.ThreadID)
		}
		return nil, fmt.Errorf("tool call for unknown active turn %q on thread %q", params.TurnID, params.ThreadID)
	}
	if strings.TrimSpace(active.threadID) != strings.TrimSpace(params.ThreadID) {
		s.mu.Unlock()
		if params.TurnID == "" {
			return nil, fmt.Errorf("tool call for unknown active thread %q", params.ThreadID)
		}
		return nil, fmt.Errorf("tool call for unknown active turn %q on thread %q", params.TurnID, params.ThreadID)
	}
	if trimmedTurnID := strings.TrimSpace(params.TurnID); trimmedTurnID != "" {
		activeTurnID := strings.TrimSpace(active.turnID)
		if activeTurnID != "" && activeTurnID != trimmedTurnID {
			s.mu.Unlock()
			return nil, fmt.Errorf("tool call for unknown active turn %q on thread %q", params.TurnID, params.ThreadID)
		}
	}

	req := active.req
	tools := append([]Tool(nil), active.tools...)
	s.mu.Unlock()

	tool, ok := findToolIn(tools, params.Tool)
	if !ok {
		return nil, fmt.Errorf("unknown tool %q", params.Tool)
	}

	input, err := json.Marshal(params.Arguments)
	if err != nil {
		return nil, fmt.Errorf("encode tool arguments failed: %w", err)
	}
	if len(input) == 0 {
		input = []byte("{}")
	}

	toolCtx := ContextWithTurnRequest(ctx, req)
	log.Printf(
		"app-server runner calling dynamic tool: conversation=%s thread_id=%s tool=%s input_bytes=%d",
		req.Conversation.Key,
		params.ThreadID,
		tool.Name(),
		len(input),
	)
	result, callErr := tool.Call(toolCtx, input)
	if callErr != nil {
		log.Printf(
			"app-server runner dynamic tool failed: conversation=%s thread_id=%s tool=%s err=%v",
			req.Conversation.Key,
			params.ThreadID,
			tool.Name(),
			callErr,
		)
	} else {
		log.Printf(
			"app-server runner dynamic tool completed: conversation=%s thread_id=%s tool=%s",
			req.Conversation.Key,
			params.ThreadID,
			tool.Name(),
		)
	}
	contentItems, buildErr := buildDynamicToolContentItems(result, callErr)
	if buildErr != nil {
		log.Printf(
			"app-server runner dynamic tool result encoding failed: conversation=%s thread_id=%s tool=%s err=%v",
			req.Conversation.Key,
			params.ThreadID,
			tool.Name(),
			buildErr,
		)
		return nil, buildErr
	}

	response := appproto.SanitizedDynamicToolCallResponse{
		ContentItems: contentItems,
		Success:      callErr == nil,
	}
	return &response, nil
}

func (s *appServerSession) runThreadTurn(ctx context.Context, req TurnRequest, thread appServerThread, inputs []appcodex.Input, options *appcodex.TurnOptions) (*appcodex.TurnResult, error) {
	if s == nil || s.runner == nil {
		return nil, errors.New("app-server session is nil")
	}
	if s.runner.runThreadTurnFn != nil {
		turn, err := s.runner.runThreadTurnFn(ctx, req, thread, inputs, options)
		if err != nil {
			log.Printf(
				"app-server runner turn failed: conversation=%s thread_id=%s model=%s cwd=%s sandbox=%s approval=%s input_count=%d err=%v",
				req.Conversation.Key,
				thread.ID(),
				options.Model,
				options.Cwd,
				describeAppServerSandboxPolicy(options.SandboxPolicy),
				describeAppServerApprovalPolicy(options.ApprovalPolicy),
				len(inputs),
				err,
			)
		}
		return turn, err
	}

	log.Printf("app-server runner started streamed turn: conversation=%s thread_id=%s", req.Conversation.Key, thread.ID())
	stream, err := thread.RunStreamed(ctx, inputs, options)
	if err != nil {
		if options != nil {
			log.Printf(
				"app-server runner start streamed turn failed: conversation=%s thread_id=%s model=%s cwd=%s sandbox=%s approval=%s input_count=%d err=%v",
				req.Conversation.Key,
				thread.ID(),
				options.Model,
				options.Cwd,
				describeAppServerSandboxPolicy(options.SandboxPolicy),
				describeAppServerApprovalPolicy(options.ApprovalPolicy),
				len(inputs),
				err,
			)
		} else {
			log.Printf("app-server runner start streamed turn failed: conversation=%s thread_id=%s input_count=%d err=%v", req.Conversation.Key, thread.ID(), len(inputs), err)
		}
		return nil, err
	}
	defer stream.Close()

	if streamTurnID := stream.TurnID(); streamTurnID != "" {
		s.updateTurnID(thread.ID(), streamTurnID)
		s.interruptActiveTurnIfRequested(ctx, thread.ID(), streamTurnID)
		defer s.endTurn(thread.ID(), streamTurnID)
	}

	return s.collectStreamedTurn(ctx, req, thread.ID(), stream)
}

func (s *appServerSession) collectStreamedTurn(ctx context.Context, req TurnRequest, threadID string, stream appServerTurnStream) (*appcodex.TurnResult, error) {
	if stream == nil {
		return nil, errors.New("streamed turn is nil")
	}

	type turnNoteResult struct {
		note apprpc.Notification
		err  error
	}

	noteCh := make(chan turnNoteResult, 1)
	go func() {
		for {
			note, err := stream.Next(ctx)
			noteCh <- turnNoteResult{note: note, err: err}
			if err != nil {
				return
			}
		}
	}()

	var interruptRequested <-chan struct{}
	s.mu.Lock()
	active := s.activeTurn
	if active != nil && strings.TrimSpace(active.threadID) == strings.TrimSpace(threadID) {
		interruptRequested = active.interruptRequested
	}
	s.mu.Unlock()

	result := &appcodex.TurnResult{}
	for {
		select {
		case <-interruptRequested:
			s.interruptActiveTurnIfRequested(ctx, threadID, result.TurnID)
			interruptRequested = nil
		case next := <-noteCh:
			note := next.note
			err := next.err
			if err != nil {
				log.Printf("app-server runner streamed turn read failed: conversation=%s err=%v", req.Conversation.Key, err)
				return nil, err
			}
			result.Notifications = append(result.Notifications, note)

			switch note.Method {
			case "item/completed":
				item, text := parseAppServerItem(note)
				if len(item) != 0 {
					result.Items = append(result.Items, item)
				}
				if text != "" {
					result.FinalResponse = text
				}
			case "turn/started":
				result.TurnID = parseAppServerTurnID(note)
				s.updateTurnID(threadID, result.TurnID)
				s.interruptActiveTurnIfRequested(ctx, threadID, result.TurnID)
			case "turn/completed":
				result.TurnID = parseAppServerTurnID(note)
				turnStatus := parseAppServerTurnStatus(note)
				s.updateTurnID(threadID, result.TurnID)
				s.interruptActiveTurnIfRequested(ctx, threadID, result.TurnID)
				if turnErr := parseAppServerTurnError(note); turnErr != nil {
					log.Printf("app-server runner completed turn with error: conversation=%s turn_id=%s status=%s err=%v", req.Conversation.Key, result.TurnID, turnStatus, turnErr)
					return nil, turnErr
				}
				log.Printf("app-server runner completed streamed turn: conversation=%s turn_id=%s status=%s items=%d final_response_len=%d", req.Conversation.Key, result.TurnID, turnStatus, len(result.Items), len(result.FinalResponse))
				return result, nil
			case "turn/failed":
				if turnErr := parseAppServerTurnError(note); turnErr != nil {
					log.Printf("app-server runner turn notification failed: conversation=%s turn_id=%s method=%s err=%v", req.Conversation.Key, result.TurnID, note.Method, turnErr)
					return nil, turnErr
				}
				log.Printf("app-server runner turn notification failed without detail: conversation=%s turn_id=%s method=%s", req.Conversation.Key, result.TurnID, note.Method)
				return nil, errors.New("app-server turn failed")
			case "error":
				if shouldRetryAppServerTurn(note) {
					log.Printf("app-server runner turn notification requested retry: conversation=%s turn_id=%s", req.Conversation.Key, result.TurnID)
					continue
				}
				if turnErr := parseAppServerTurnError(note); turnErr != nil {
					log.Printf("app-server runner turn notification failed: conversation=%s turn_id=%s method=%s err=%v", req.Conversation.Key, result.TurnID, note.Method, turnErr)
					return nil, turnErr
				}
				log.Printf("app-server runner turn notification failed without detail: conversation=%s turn_id=%s method=%s", req.Conversation.Key, result.TurnID, note.Method)
				return nil, errors.New("app-server turn failed")
			}
		}
	}
}

func (r *AppServerRunner) findSessionForTurn(threadID string, turnID string) (*appServerSession, bool) {
	if r == nil || strings.TrimSpace(threadID) == "" {
		return nil, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, session := range r.sessions {
		if session == nil {
			continue
		}
		if session.matchesTurn(threadID, turnID) {
			return session, true
		}
	}
	return nil, false
}

func (r *AppServerRunner) registerSession(sessionKey string, session *appServerSession) {
	if r == nil || session == nil {
		return
	}
	r.mu.Lock()
	if r.sessions == nil {
		r.sessions = make(map[string]*appServerSession)
	}
	if sessionKey == "" {
		sessionKey = session.conversationKey
	}
	if sessionKey != "" {
		r.sessions[sessionKey] = session
	}
	r.mu.Unlock()
}

func (r *AppServerRunner) unregisterSession(sessionKey string, target *appServerSession) {
	if r == nil || target == nil {
		return
	}
	r.mu.Lock()
	if sessionKey == "" {
		sessionKey = target.conversationKey
	}
	if sessionKey != "" {
		if existing := r.sessions[sessionKey]; existing == target {
			delete(r.sessions, sessionKey)
		}
	}
	r.mu.Unlock()
}

func buildAppServerTurnInputs(req TurnRequest) []appcodex.Input {
	prompt, imagePaths := buildTurnPrompt(req.Message)
	inputs := make([]appcodex.Input, 0, 1+len(imagePaths))
	if prompt != "" {
		inputs = append(inputs, appcodex.TextInput(prompt))
	}
	for _, imagePath := range imagePaths {
		inputs = append(inputs, appcodex.LocalImageInput(imagePath))
	}
	if len(inputs) == 0 {
		inputs = append(inputs, appcodex.TextInput(""))
	}
	return inputs
}

func appServerSessionKey(conversation ConversationState) string {
	key := strings.TrimSpace(conversation.Key)
	if key != "" {
		return key
	}
	return strings.TrimSpace(conversation.RunnerThreadID)
}
