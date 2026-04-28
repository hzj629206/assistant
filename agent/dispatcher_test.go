package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hzj629206/assistant/cache"
)

type testRunner struct {
	mu                sync.Mutex
	startSessionCalls int
	startSessionID    string
	startSessionStart chan struct{}
	startSessionWait  chan struct{}
	calls             int
	lastReq           TurnRequest
	activeCancel      context.CancelFunc
	interruptCalls    int
	lastInterrupt     ConversationState
	statusCalls       int
	lastStatus        ConversationState
	status            SessionStatus
	statusErr         error
	statusStarted     chan struct{}
	statusRelease     chan struct{}
	interruptStarted  chan struct{}
	interruptRelease  chan struct{}
	interruptErr      error
	closeCalls        int
	closeStarted      chan struct{}
	closeRelease      chan struct{}
	closeErr          error
	started           chan struct{}
	release           chan struct{}
	waitForCancel     bool
	canceled          chan struct{}
	replyOnCancel     string
	err               error
	panicV            any
}

type testSession struct {
	runner          *testRunner
	conversationKey string
	sessionID       string
}

type compatSession interface {
	Session
	RunTurn(ctx context.Context, req TurnRequest) (TurnResult, error)
	Interrupt(ctx context.Context) error
}

type compatSessionAdapter struct {
	Session
}

type testTurnInterruptor interface {
	interruptCurrentTurn(ctx context.Context) error
}

type testScheduledTurn struct {
	session  *testSession
	req      TurnRequest
	done     chan struct{}
	doneOnce sync.Once
}

func startTestSession(t *testing.T, runner Runner, conversation ConversationState) compatSession {
	t.Helper()

	session, err := runner.StartSession(context.Background(), SessionOptions{
		ConversationKey: conversation.Key,
		ResumeSessionID: conversation.RunnerThreadID,
	})
	if err != nil {
		t.Fatalf("StartSession failed: %v", err)
	}
	return mustCompatSession(t, session)
}

func mustCompatSession(t *testing.T, session Session) compatSession {
	t.Helper()
	compat, ok := session.(compatSession)
	if ok {
		return compat
	}
	return &compatSessionAdapter{Session: session}
}

func runTurnWithRunner(t *testing.T, runner Runner, req TurnRequest) (TurnResult, error) {
	t.Helper()
	return runSessionTurn(context.Background(), startTestSession(t, runner, req.Conversation), req)
}

func interruptWithRunner(t *testing.T, runner Runner, conversation ConversationState) error {
	t.Helper()
	session, err := runner.StartSession(context.Background(), SessionOptions{
		ConversationKey: conversation.Key,
		ResumeSessionID: conversation.RunnerThreadID,
	})
	if err != nil {
		t.Fatalf("StartSession failed: %v", err)
	}
	return interruptSession(context.Background(), session)
}

func interruptSession(ctx context.Context, session Session) error {
	if session == nil {
		return nil
	}
	interruptible, ok := session.(testTurnInterruptor)
	if !ok {
		return errors.New("session interrupt is unavailable")
	}
	return interruptible.interruptCurrentTurn(ctx)
}

func (s *testSession) ID() string { return s.sessionID }

func (s *testSession) ScheduleTurn(_ context.Context, req TurnRequest) (ScheduledTurn, error) {
	req.Conversation.Key = s.conversationKey
	req.Conversation.RunnerThreadID = s.sessionID
	return &testScheduledTurn{
		session: s,
		req:     req,
		done:    make(chan struct{}),
	}, nil
}

func (s *testSession) RunTurn(ctx context.Context, req TurnRequest) (TurnResult, error) {
	turn, err := s.ScheduleTurn(ctx, req)
	if err != nil {
		return TurnResult{}, err
	}
	return turn.Run(ctx)
}

func (s *testSession) Interrupt(ctx context.Context) error {
	return s.runner.Interrupt(ctx, ConversationState{Key: s.conversationKey, RunnerThreadID: s.sessionID})
}

func (s *testSession) interruptCurrentTurn(ctx context.Context) error {
	return s.Interrupt(ctx)
}

func (s *testSession) Status(ctx context.Context) (SessionStatus, error) {
	return s.runner.Status(ctx, ConversationState{Key: s.conversationKey, RunnerThreadID: s.sessionID})
}

func (s *testSession) Close() error {
	return s.runner.Close()
}

func (t *testScheduledTurn) Run(ctx context.Context) (TurnResult, error) {
	defer t.doneOnce.Do(func() {
		close(t.done)
	})
	return t.session.runner.RunTurn(ctx, t.req)
}

func (t *testScheduledTurn) Interrupt(ctx context.Context) error {
	defer t.doneOnce.Do(func() {
		close(t.done)
	})
	return t.session.runner.Interrupt(ctx, ConversationState{Key: t.session.conversationKey, RunnerThreadID: t.session.sessionID})
}

func (t *testScheduledTurn) Done() <-chan struct{} { return t.done }

func (a *compatSessionAdapter) RunTurn(ctx context.Context, req TurnRequest) (TurnResult, error) {
	return runSessionTurn(ctx, a.Session, req)
}

func (a *compatSessionAdapter) Interrupt(ctx context.Context) error {
	return interruptSession(ctx, a.Session)
}

func (a *compatSessionAdapter) interruptCurrentTurn(ctx context.Context) error {
	return interruptSession(ctx, a.Session)
}

func (r *testRunner) StartSession(ctx context.Context, options SessionOptions) (Session, error) {
	if r.startSessionStart != nil {
		select {
		case r.startSessionStart <- struct{}{}:
		default:
		}
	}
	if r.startSessionWait != nil {
		select {
		case <-r.startSessionWait:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	r.mu.Lock()
	r.startSessionCalls++
	sessionID := strings.TrimSpace(options.ResumeSessionID)
	if sessionID == "" {
		sessionID = strings.TrimSpace(r.startSessionID)
	}
	r.mu.Unlock()
	return &testSession{
		runner:          r,
		conversationKey: options.ConversationKey,
		sessionID:       sessionID,
	}, nil
}

func (r *testRunner) RunTurn(ctx context.Context, req TurnRequest) (TurnResult, error) {
	runCtx, cancel := context.WithCancel(ctx)

	r.mu.Lock()
	r.calls++
	r.lastReq = req
	r.activeCancel = cancel
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.activeCancel = nil
		r.mu.Unlock()
		cancel()
	}()

	if r.started != nil {
		select {
		case r.started <- struct{}{}:
		default:
		}
	}
	if r.waitForCancel {
		<-runCtx.Done()
		if r.canceled != nil {
			select {
			case r.canceled <- struct{}{}:
			default:
			}
		}
		if r.release != nil {
			<-r.release
		}
		if r.replyOnCancel != "" {
			return TurnResult{ReplyText: r.replyOnCancel}, nil
		}
		return TurnResult{}, runCtx.Err()
	}
	if r.release != nil {
		select {
		case <-r.release:
		case <-runCtx.Done():
			if r.canceled != nil {
				select {
				case r.canceled <- struct{}{}:
				default:
				}
			}
			return TurnResult{}, runCtx.Err()
		}
	}
	if r.panicV != nil {
		panic(r.panicV)
	}
	if r.err != nil {
		return TurnResult{}, r.err
	}

	return TurnResult{ReplyText: req.Message.Text}, nil
}

func (*testRunner) RegisterSystemPrompt(string) {}

func (*testRunner) RegisterTools(...Tool) {}

func (r *testRunner) Close() error {
	r.mu.Lock()
	r.closeCalls++
	if r.closeStarted != nil {
		select {
		case r.closeStarted <- struct{}{}:
		default:
		}
	}
	release := r.closeRelease
	err := r.closeErr
	cancel := r.activeCancel
	r.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if release != nil {
		<-release
	}
	return err
}

func (r *testRunner) Interrupt(_ context.Context, conversation ConversationState) error {
	r.mu.Lock()
	r.interruptCalls++
	r.lastInterrupt = conversation
	if r.interruptStarted != nil {
		select {
		case r.interruptStarted <- struct{}{}:
		default:
		}
	}
	release := r.interruptRelease
	err := r.interruptErr
	cancel := r.activeCancel
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if release != nil {
		<-release
	}
	return err
}

func (r *testRunner) Status(_ context.Context, conversation ConversationState) (SessionStatus, error) {
	r.mu.Lock()
	r.statusCalls++
	r.lastStatus = conversation
	status := r.status
	err := r.statusErr
	if r.statusStarted != nil {
		select {
		case r.statusStarted <- struct{}{}:
		default:
		}
	}
	release := r.statusRelease
	r.mu.Unlock()

	if release != nil {
		<-release
	}
	return status, err
}

func (r *testRunner) Calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

func (r *testRunner) LastRequest() TurnRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastReq
}

func (r *testRunner) LastInterrupt() ConversationState {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastInterrupt
}

func (r *testRunner) InterruptCalls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.interruptCalls
}

func (r *testRunner) StatusCalls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.statusCalls
}

func (r *testRunner) LastStatus() ConversationState {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastStatus
}

func (r *testRunner) CloseCalls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.closeCalls
}

func (r *testRunner) StartSessionCalls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.startSessionCalls
}

type sentReply struct {
	text string
}

type testResponder struct {
	mu           sync.Mutex
	sendCalls    int
	typingCalls  int
	cleanupCalls int
	reply        sentReply
	err          error
}

func (r *testResponder) SendText(_ context.Context, text string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.sendCalls++
	r.reply = sentReply{text: text}
	return r.err
}

func (r *testResponder) SetTyping(context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.typingCalls++
	return nil
}

func (r *testResponder) Cleanup(context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cleanupCalls++
	return nil
}

func (r *testResponder) SendCalls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sendCalls
}

func (r *testResponder) CleanupCalls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cleanupCalls
}

func (r *testResponder) Reply() sentReply {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reply
}

func waitForResponderSend(responder *testResponder) error {
	deadline := time.After(time.Second)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	for {
		if responder.SendCalls() >= 1 {
			return nil
		}

		select {
		case <-deadline:
			return context.DeadlineExceeded
		case <-ticker.C:
		}
	}
}

func TestDispatcherShutdownDropsQueuedWorkAndWaitsForRunningTurn(t *testing.T) {
	t.Parallel()

	runner := &testRunner{
		started: make(chan struct{}, 1),
		release: make(chan struct{}),
	}
	dispatcher := NewDispatcher(DispatcherOptions{
		Store:       NewConversationStore(cache.NewMemoryStorage()),
		Runner:      runner,
		WorkerCount: 1,
	})
	_ = dispatcher.Start()

	if err := dispatcher.Enqueue(context.Background(), InboundMessage{
		ID:              "evt-1",
		ConversationKey: "private:e_1:msg-1",
		Text:            "hello",
	}); err != nil {
		t.Fatalf("enqueue failed: %v", err)
	}

	if err := dispatcher.Enqueue(context.Background(), InboundMessage{
		ID:              "evt-2",
		ConversationKey: "private:e_1:msg-2",
		Text:            "queued",
	}); err != nil {
		t.Fatalf("enqueue failed: %v", err)
	}

	<-runner.started

	done := make(chan error, 1)
	go func() {
		done <- dispatcher.Shutdown(context.Background())
	}()

	select {
	case err := <-done:
		t.Fatalf("shutdown returned before work finished: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	close(runner.release)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("shutdown failed: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown timed out waiting for queued work")
	}

	if got := runner.Calls(); got != 1 {
		t.Fatalf("unexpected runner call count: %d", got)
	}
}

func TestDispatcherShutdownCancelsRunningTurnAfterGracePeriod(t *testing.T) {
	t.Parallel()

	runner := &testRunner{
		started:       make(chan struct{}, 1),
		canceled:      make(chan struct{}, 1),
		waitForCancel: true,
	}
	dispatcher := NewDispatcher(DispatcherOptions{
		Store:               NewConversationStore(cache.NewMemoryStorage()),
		Runner:              runner,
		WorkerCount:         1,
		ShutdownTurnTimeout: 50 * time.Millisecond,
	})
	_ = dispatcher.Start()

	if err := dispatcher.Enqueue(context.Background(), InboundMessage{
		ID:              "evt-1",
		ConversationKey: "private:e_1:msg-1",
		Text:            "hello",
	}); err != nil {
		t.Fatalf("enqueue failed: %v", err)
	}

	select {
	case <-runner.started:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for runner start")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	if err := dispatcher.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("shutdown failed: %v", err)
	}

	select {
	case <-runner.canceled:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for runner cancellation")
	}
	if runner.CloseCalls() == 0 {
		t.Fatal("expected shutdown to close managed sessions after grace period")
	}

	if got := runner.Calls(); got != 1 {
		t.Fatalf("unexpected runner call count: %d", got)
	}
}

func TestDispatcherShutdownDefersSessionCloseUntilInterruptedTurnFinishes(t *testing.T) {
	t.Parallel()

	runner := &testRunner{
		started:       make(chan struct{}, 1),
		canceled:      make(chan struct{}, 1),
		closeStarted:  make(chan struct{}, 1),
		release:       make(chan struct{}),
		waitForCancel: true,
	}
	dispatcher := NewDispatcher(DispatcherOptions{
		Store:               NewConversationStore(cache.NewMemoryStorage()),
		Runner:              runner,
		WorkerCount:         1,
		ShutdownTurnTimeout: 50 * time.Millisecond,
	})
	_ = dispatcher.Start()

	if err := dispatcher.Enqueue(context.Background(), InboundMessage{
		ID:              "evt-1",
		ConversationKey: "private:e_1:msg-close-after-turn",
		Text:            "hello",
	}); err != nil {
		t.Fatalf("enqueue failed: %v", err)
	}

	select {
	case <-runner.started:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for runner start")
	}

	done := make(chan error, 1)
	go func() {
		done <- dispatcher.Shutdown(context.Background())
	}()

	select {
	case <-runner.closeStarted:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for session close start")
	}

	select {
	case <-runner.canceled:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for runner cancellation")
	}

	select {
	case err := <-done:
		t.Fatalf("shutdown returned before running turn finished: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	close(runner.release)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("shutdown failed: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown timed out waiting for interrupted turn")
	}
}

func TestDispatcherShutdownDropsQueuedCommandsAndWaitsForRunningCommand(t *testing.T) {
	t.Parallel()

	runner := &testRunner{
		started:          make(chan struct{}, 1),
		release:          make(chan struct{}),
		waitForCancel:    true,
		interruptStarted: make(chan struct{}, 2),
		interruptRelease: make(chan struct{}),
	}
	store := NewConversationStore(cache.NewMemoryStorage())
	err := store.PutConversation(context.Background(), ConversationState{
		Key:            "private:e_shutdown:0",
		RunnerThreadID: "thread-shutdown",
		LastEventID:    "evt-prev",
		LastActivityAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("put conversation failed: %v", err)
	}
	dispatcher := NewDispatcher(DispatcherOptions{
		Store:       store,
		Runner:      runner,
		WorkerCount: 1,
	})
	_ = dispatcher.Start()
	managedSession, err := dispatcher.ensureConversationSession(context.Background(), ConversationState{
		Key:            "private:e_shutdown:0",
		RunnerThreadID: "thread-shutdown",
	})
	if err != nil {
		t.Fatalf("ensure managed session failed: %v", err)
	}
	if managedSession == nil {
		t.Fatal("expected managed session")
	}

	firstResponder := &testResponder{}
	if err := dispatcher.Enqueue(context.Background(), InboundMessage{
		ID:              "evt-active-shutdown",
		ConversationKey: "private:e_shutdown:0",
		Kind:            MessageKindText,
		Text:            "running turn",
		Responder:       &testResponder{},
	}); err != nil {
		t.Fatalf("enqueue active message failed: %v", err)
	}

	select {
	case <-runner.started:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for active turn start")
	}

	if err := dispatcher.EnqueueCommand(context.Background(), CommandRequest{
		ConversationKey: "private:e_shutdown:0",
		EventID:         "evt-stop-running",
		Responder:       firstResponder,
		Command:         SlashCommand{Name: "stop", Raw: "/stop"},
	}); err != nil {
		t.Fatalf("enqueue running command failed: %v", err)
	}

	select {
	case <-runner.interruptStarted:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for running command interrupt")
	}

	secondResponder := &testResponder{}
	if err := dispatcher.EnqueueCommand(context.Background(), CommandRequest{
		ConversationKey: "private:e_shutdown:0",
		EventID:         "evt-help-queued",
		Responder:       secondResponder,
		Command:         SlashCommand{Name: "help", Raw: "/help"},
	}); err != nil {
		t.Fatalf("enqueue queued command failed: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		done <- dispatcher.Shutdown(context.Background())
	}()

	select {
	case err := <-done:
		t.Fatalf("shutdown returned before running command finished: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	close(runner.interruptRelease)
	close(runner.release)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("shutdown failed: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown timed out waiting for running command")
	}

	if err := waitForResponderSend(firstResponder); err != nil {
		t.Fatalf("timed out waiting for running command reply: %v", err)
	}
	if got := secondResponder.SendCalls(); got != 0 {
		t.Fatalf("expected queued command reply to be dropped, got %d sends", got)
	}
	if got := secondResponder.CleanupCalls(); got != 1 {
		t.Fatalf("expected queued command cleanup once, got %d", got)
	}
}

func TestDispatcherExpiresIdleSession(t *testing.T) {
	t.Parallel()

	runner := &testRunner{}
	dispatcher := NewDispatcher(DispatcherOptions{
		Store:              NewConversationStore(cache.NewMemoryStorage()),
		Runner:             runner,
		SessionIdleTimeout: 50 * time.Millisecond,
	})

	session, err := dispatcher.ensureConversationSession(context.Background(), ConversationState{Key: "private:e_1:msg-1"})
	if err != nil {
		t.Fatalf("ensure session failed: %v", err)
	}
	dispatcher.beginConversationSessionUse("private:e_1:msg-1", session)
	dispatcher.endConversationSessionUse("private:e_1:msg-1", session)

	deadline := time.After(time.Second)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	for runner.CloseCalls() < 1 || dispatcher.managedConversationSession("private:e_1:msg-1") != nil {
		select {
		case <-deadline:
			t.Fatal("idle session was not expired")
		case <-ticker.C:
		}
	}

	_, err = dispatcher.ensureConversationSession(context.Background(), ConversationState{Key: "private:e_1:msg-1"})
	if err != nil {
		t.Fatalf("ensure session after idle expiry failed: %v", err)
	}
	if got := runner.StartSessionCalls(); got < 2 {
		t.Fatalf("expected session to be recreated after idle expiry, got start_session_calls=%d", got)
	}
}

func TestDispatcherPersistsSessionIDBeforeFirstTurn(t *testing.T) {
	t.Parallel()

	store := NewConversationStore(cache.NewMemoryStorage())
	runner := &testRunner{
		startSessionID: "thread-prestarted",
		started:        make(chan struct{}, 1),
		release:        make(chan struct{}),
	}
	dispatcher := NewDispatcher(DispatcherOptions{
		Store:       store,
		Runner:      runner,
		WorkerCount: 1,
	})
	_ = dispatcher.Start()

	err := dispatcher.Enqueue(context.Background(), InboundMessage{
		ID:              "evt-prestart",
		ConversationKey: "private:e_1:prestart",
		Kind:            MessageKindText,
		Text:            "hello",
		Responder:       &testResponder{},
	})
	if err != nil {
		t.Fatalf("enqueue message failed: %v", err)
	}

	select {
	case <-runner.started:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for first turn start")
	}

	state, err := store.GetConversation(context.Background(), "private:e_1:prestart")
	if err != nil {
		t.Fatalf("get conversation failed: %v", err)
	}
	if state.RunnerThreadID != "thread-prestarted" {
		t.Fatalf("expected persisted runner thread id, got %q", state.RunnerThreadID)
	}

	close(runner.release)
}

func TestDispatcherRejectsEnqueueAfterShutdown(t *testing.T) {
	t.Parallel()

	dispatcher := NewDispatcher(DispatcherOptions{
		Store: NewConversationStore(cache.NewMemoryStorage()),
	})
	_ = dispatcher.Start()

	if err := dispatcher.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown failed: %v", err)
	}

	err := dispatcher.Enqueue(context.Background(), InboundMessage{
		ID:              "evt-1",
		ConversationKey: "private:e_1:msg-1",
	})
	if !errors.Is(err, ErrDispatcherClosed) {
		t.Fatalf("expected ErrDispatcherClosed, got %v", err)
	}
}

func TestDispatcherDiscardsSessionAfterNonTimeoutTurnError(t *testing.T) {
	t.Parallel()

	runner := &testRunner{
		err: errors.New("boom"),
	}
	dispatcher := NewDispatcher(DispatcherOptions{
		Store:  NewConversationStore(cache.NewMemoryStorage()),
		Runner: runner,
	})

	err := dispatcher.handleMessage(context.Background(), InboundMessage{
		ID:              "evt-fail",
		ConversationKey: "private:e_1:fail",
		Text:            "hello",
	})
	if err == nil || err.Error() != "boom" {
		t.Fatalf("unexpected handle error: %v", err)
	}
	if runner.CloseCalls() != 1 {
		t.Fatalf("expected failed turn session to be closed, got %d", runner.CloseCalls())
	}
	if session := dispatcher.managedConversationSession("private:e_1:fail"); session != nil {
		t.Fatal("expected failed turn session to be discarded")
	}

	_, err = dispatcher.ensureConversationSession(context.Background(), ConversationState{Key: "private:e_1:fail"})
	if err != nil {
		t.Fatalf("ensure session after failure failed: %v", err)
	}
	if got := runner.StartSessionCalls(); got < 2 {
		t.Fatalf("expected session to be recreated after failure, got start_session_calls=%d", got)
	}
}

func TestDispatcherResetCommandInterruptsAndResetsConversationState(t *testing.T) {
	t.Parallel()

	store := NewConversationStore(cache.NewMemoryStorage())
	err := store.PutConversation(context.Background(), ConversationState{
		Key:            "private:e_1:reset",
		RunnerThreadID: "thread-reset",
	})
	if err != nil {
		t.Fatalf("put conversation failed: %v", err)
	}

	runner := &testRunner{}
	dispatcher := NewDispatcher(DispatcherOptions{
		Store:  store,
		Runner: runner,
	})
	session, err := dispatcher.ensureConversationSession(context.Background(), ConversationState{
		Key:            "private:e_1:reset",
		RunnerThreadID: "thread-reset",
	})
	if err != nil {
		t.Fatalf("ensure session failed: %v", err)
	}
	if session == nil {
		t.Fatal("expected managed session")
	}

	reply, err := dispatcher.executeBlockingCommand(context.Background(), CommandRequest{
		ConversationKey: "private:e_1:reset",
		Command:         SlashCommand{Name: "reset"},
	})
	if err != nil {
		t.Fatalf("build reset reply failed: %v", err)
	}
	if !strings.Contains(reply, "_Conversation reset._") {
		t.Fatalf("unexpected reset reply: %q", reply)
	}
	if runner.InterruptCalls() != 0 {
		t.Fatalf("expected reset without active turn to skip interrupt, got %d", runner.InterruptCalls())
	}
	if runner.CloseCalls() != 1 {
		t.Fatalf("expected one close during reset, got %d", runner.CloseCalls())
	}
	if _, err = store.GetConversation(context.Background(), "private:e_1:reset"); !errors.Is(err, cache.ErrNotFound) {
		t.Fatalf("expected conversation state to be deleted, got %v", err)
	}
	if session := dispatcher.managedConversationSession("private:e_1:reset"); session != nil {
		t.Fatal("expected dispatcher session to be removed after reset")
	}
}

func TestDispatcherResetCausesNextTurnToReloadInitialMessages(t *testing.T) {
	t.Parallel()

	store := NewConversationStore(cache.NewMemoryStorage())
	err := store.PutConversation(context.Background(), ConversationState{
		Key:            "private:e_1:reset-next",
		RunnerThreadID: "thread-old",
	})
	if err != nil {
		t.Fatalf("put conversation failed: %v", err)
	}

	runner := &testRunner{}
	dispatcher := NewDispatcher(DispatcherOptions{
		Store:       store,
		Runner:      runner,
		WorkerCount: 1,
	})
	_ = dispatcher.Start()

	_, err = dispatcher.executeBlockingCommand(context.Background(), CommandRequest{
		ConversationKey: "private:e_1:reset-next",
		Command:         SlashCommand{Name: "reset"},
	})
	if err != nil {
		t.Fatalf("build reset reply failed: %v", err)
	}

	if err = dispatcher.Enqueue(context.Background(), InboundMessage{
		ID:              "evt-reset-next",
		ConversationKey: "private:e_1:reset-next",
		Text:            "hello after reset",
		LoadInitialMessages: func(context.Context) ([]InboundMessage, error) {
			return []InboundMessage{{
				ID:              "history-1",
				ConversationKey: "private:e_1:reset-next",
				Text:            "older message",
			}}, nil
		},
	}); err != nil {
		t.Fatalf("enqueue failed: %v", err)
	}

	deadline := time.Now().Add(time.Second)
	for runner.Calls() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if runner.Calls() != 1 {
		t.Fatal("timed out waiting for runner call")
	}
	if runner.LastRequest().Conversation.RunnerThreadID != "" {
		t.Fatalf("expected next turn to start without persisted thread, got %q", runner.LastRequest().Conversation.RunnerThreadID)
	}
	if len(runner.LastRequest().Message.historicalMessages) != 1 {
		t.Fatalf("expected initial history to be reloaded after reset, got %d", len(runner.LastRequest().Message.historicalMessages))
	}
}

func TestDispatcherReportsNonFatalMessageErrors(t *testing.T) {
	t.Parallel()

	runner := &testRunner{err: errors.New("runner failed")}
	dispatcher := NewDispatcher(DispatcherOptions{
		Store:       NewConversationStore(cache.NewMemoryStorage()),
		Runner:      runner,
		WorkerCount: 1,
	})
	_ = dispatcher.Start()

	if err := dispatcher.Enqueue(context.Background(), InboundMessage{
		ID:              "evt-1",
		ConversationKey: "private:e_1:msg-1",
		Text:            "hello",
	}); err != nil {
		t.Fatalf("enqueue failed: %v", err)
	}

	if err := dispatcher.Enqueue(context.Background(), InboundMessage{
		ID:              "evt-2",
		ConversationKey: "private:e_1:msg-2",
		Text:            "again",
	}); err != nil {
		t.Fatalf("enqueue failed after non-fatal error: %v", err)
	}

	deadline := time.After(time.Second)
	for {
		if got := runner.Calls(); got >= 2 {
			break
		}

		select {
		case <-time.After(10 * time.Millisecond):
		case <-deadline:
			t.Fatal("timed out waiting for worker to continue after non-fatal error")
		}
	}

	if err := dispatcher.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown failed: %v", err)
	}
}

func TestDispatcherReportsFatalWorkerPanics(t *testing.T) {
	t.Parallel()

	runner := &testRunner{panicV: "boom"}
	fatalErrCh := make(chan error, 1)
	dispatcher := NewDispatcher(DispatcherOptions{
		Store:       NewConversationStore(cache.NewMemoryStorage()),
		Runner:      runner,
		FatalErrCh:  fatalErrCh,
		WorkerCount: 1,
	})
	_ = dispatcher.Start()

	if err := dispatcher.Enqueue(context.Background(), InboundMessage{
		ID:              "evt-1",
		ConversationKey: "private:e_1:msg-1",
		Text:            "hello",
	}); err != nil {
		t.Fatalf("enqueue failed: %v", err)
	}

	select {
	case err := <-fatalErrCh:
		if err == nil {
			t.Fatal("expected fatal error in external channel")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for fatal error in external channel")
	}

	if err := dispatcher.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown failed: %v", err)
	}
}

func TestDispatcherUsesPreHydratedQuotedMessage(t *testing.T) {
	t.Parallel()

	runner := &testRunner{}
	dispatcher := NewDispatcher(DispatcherOptions{
		Store:       NewConversationStore(cache.NewMemoryStorage()),
		Runner:      runner,
		WorkerCount: 1,
	})
	_ = dispatcher.Start()

	if err := dispatcher.Enqueue(context.Background(), InboundMessage{
		ID:              "evt-quoted-1",
		ConversationKey: "private:e_1:msg-quoted-1",
		Kind:            MessageKindText,
		QuotedMessage: &ReferencedMessage{
			Kind: MessageKindText,
			Text: "quoted hello",
		},
		Text: "hello",
	}); err != nil {
		t.Fatalf("enqueue failed: %v", err)
	}

	deadline := time.After(time.Second)
	for runner.Calls() < 1 {
		select {
		case <-time.After(10 * time.Millisecond):
		case <-deadline:
			t.Fatal("timed out waiting for runner call")
		}
	}

	req := runner.LastRequest()
	if req.Message.QuotedMessage == nil {
		t.Fatal("expected quoted message to be hydrated")
	}
	if req.Message.QuotedMessage.Kind != MessageKindText {
		t.Fatalf("unexpected quoted message kind: %s", req.Message.QuotedMessage.Kind)
	}
	if req.Message.QuotedMessage.Text != "quoted hello" {
		t.Fatalf("unexpected quoted message text: %s", req.Message.QuotedMessage.Text)
	}
	if err := dispatcher.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown failed: %v", err)
	}
}

func TestDispatcherLoadsInitialContextOnlyForNewConversation(t *testing.T) {
	t.Parallel()

	runner := &testRunner{}
	dispatcher := NewDispatcher(DispatcherOptions{
		Store:       NewConversationStore(cache.NewMemoryStorage()),
		Runner:      runner,
		WorkerCount: 1,
	})
	_ = dispatcher.Start()

	loadCalls := 0
	if err := dispatcher.Enqueue(context.Background(), InboundMessage{
		ID:              "evt-context-1",
		ConversationKey: "group:g_1:thread-1",
		Text:            "hello",
		LoadInitialContext: func(context.Context) (string, error) {
			loadCalls++
			return "[1] alice@example.com: earlier message", nil
		},
	}); err != nil {
		t.Fatalf("enqueue failed: %v", err)
	}

	deadline := time.After(time.Second)
	for runner.Calls() < 1 {
		select {
		case <-time.After(10 * time.Millisecond):
		case <-deadline:
			t.Fatal("timed out waiting for runner call")
		}
	}

	firstReq := runner.LastRequest()
	if firstReq.Message.InitialContext() != "[1] alice@example.com: earlier message" {
		t.Fatalf("unexpected initial context: %q", firstReq.Message.InitialContext())
	}
	if loadCalls != 1 {
		t.Fatalf("unexpected load call count after first message: %d", loadCalls)
	}

	if err := dispatcher.Enqueue(context.Background(), InboundMessage{
		ID:              "evt-context-2",
		ConversationKey: "group:g_1:thread-1",
		Text:            "again",
		LoadInitialContext: func(context.Context) (string, error) {
			loadCalls++
			return "should not be loaded", nil
		},
	}); err != nil {
		t.Fatalf("enqueue failed: %v", err)
	}

	deadline = time.After(time.Second)
	for runner.Calls() < 2 {
		select {
		case <-time.After(10 * time.Millisecond):
		case <-deadline:
			t.Fatal("timed out waiting for second runner call")
		}
	}

	secondReq := runner.LastRequest()
	if secondReq.Message.InitialContext() != "" {
		t.Fatalf("expected no initial context for existing conversation, got %q", secondReq.Message.InitialContext())
	}
	if loadCalls != 1 {
		t.Fatalf("unexpected load call count after second message: %d", loadCalls)
	}

	if err := dispatcher.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown failed: %v", err)
	}
}

func TestDispatcherPrependsInitialMessagesOnlyForNewConversation(t *testing.T) {
	t.Parallel()

	runner := &testRunner{}
	dispatcher := NewDispatcher(DispatcherOptions{
		Store:       NewConversationStore(cache.NewMemoryStorage()),
		Runner:      runner,
		WorkerCount: 1,
	})
	_ = dispatcher.Start()

	loadCalls := 0
	if err := dispatcher.Enqueue(context.Background(), InboundMessage{
		ID:              "evt-history-1",
		ConversationKey: "group:g_1:thread-1",
		Kind:            MessageKindText,
		Sender:          "bob@example.com",
		Text:            "current",
		LoadInitialMessages: func(context.Context) ([]InboundMessage, error) {
			loadCalls++
			return []InboundMessage{{
				ID:              "history-1",
				ConversationKey: "group:g_1:thread-1",
				Kind:            MessageKindImage,
				Sender:          "alice@example.com",
				ImagePath:       "/tmp/history.png",
			}}, nil
		},
	}); err != nil {
		t.Fatalf("enqueue failed: %v", err)
	}

	deadline := time.After(time.Second)
	for runner.Calls() < 1 {
		select {
		case <-time.After(10 * time.Millisecond):
		case <-deadline:
			t.Fatal("timed out waiting for runner call")
		}
	}

	firstReq := runner.LastRequest()
	if len(firstReq.Message.historicalMessages) != 1 {
		t.Fatalf("unexpected history count: %d", len(firstReq.Message.historicalMessages))
	}
	if len(firstReq.Message.mergedMessages) != 0 {
		t.Fatalf("unexpected merged message count: %d", len(firstReq.Message.mergedMessages))
	}
	if firstReq.Message.historicalMessages[0].ImagePath != "/tmp/history.png" {
		t.Fatalf("unexpected history image path: %q", firstReq.Message.historicalMessages[0].ImagePath)
	}
	if loadCalls != 1 {
		t.Fatalf("unexpected load call count after first message: %d", loadCalls)
	}

	if err := dispatcher.Enqueue(context.Background(), InboundMessage{
		ID:              "evt-history-2",
		ConversationKey: "group:g_1:thread-1",
		Kind:            MessageKindText,
		Text:            "again",
		LoadInitialMessages: func(context.Context) ([]InboundMessage, error) {
			loadCalls++
			return []InboundMessage{{ID: "history-2", ConversationKey: "group:g_1:thread-1", Kind: MessageKindText, Text: "should not load"}}, nil
		},
	}); err != nil {
		t.Fatalf("enqueue failed: %v", err)
	}

	deadline = time.After(time.Second)
	for runner.Calls() < 2 {
		select {
		case <-time.After(10 * time.Millisecond):
		case <-deadline:
			t.Fatal("timed out waiting for second runner call")
		}
	}

	secondReq := runner.LastRequest()
	if len(secondReq.Message.historicalMessages) != 0 {
		t.Fatalf("unexpected history count for existing conversation: %d", len(secondReq.Message.historicalMessages))
	}
	if len(secondReq.Message.mergedMessages) != 0 {
		t.Fatalf("unexpected merged messages for existing conversation: %+v", secondReq.Message.mergedMessages)
	}
	if loadCalls != 1 {
		t.Fatalf("unexpected load call count after second message: %d", loadCalls)
	}

	if err := dispatcher.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown failed: %v", err)
	}
}

func TestDispatcherMergesPendingMessagesForSameConversation(t *testing.T) {
	t.Parallel()

	runner := &testRunner{
		started: make(chan struct{}, 2),
		release: make(chan struct{}),
	}
	dispatcher := NewDispatcher(DispatcherOptions{
		Store:       NewConversationStore(cache.NewMemoryStorage()),
		Runner:      runner,
		WorkerCount: 1,
	})
	_ = dispatcher.Start()

	responder1 := &testResponder{}
	if err := dispatcher.Enqueue(context.Background(), InboundMessage{
		ID:              "evt-merge-1",
		ConversationKey: "group:g_1:thread-1",
		Kind:            MessageKindText,
		Sender:          "alice@example.com",
		Text:            "first",
		Responder:       responder1,
	}); err != nil {
		t.Fatalf("enqueue first message failed: %v", err)
	}

	select {
	case <-runner.started:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for first runner start")
	}

	responder2 := &testResponder{}
	responder3 := &testResponder{}
	if err := dispatcher.Enqueue(context.Background(), InboundMessage{
		ID:              "evt-merge-2",
		ConversationKey: "group:g_1:thread-1",
		Kind:            MessageKindText,
		Sender:          "bob@example.com",
		Text:            "second",
		Responder:       responder2,
	}); err != nil {
		t.Fatalf("enqueue second message failed: %v", err)
	}
	if err := dispatcher.Enqueue(context.Background(), InboundMessage{
		ID:              "evt-merge-3",
		ConversationKey: "group:g_1:thread-1",
		Kind:            MessageKindText,
		Sender:          "carol@example.com",
		Text:            "third",
		Responder:       responder3,
	}); err != nil {
		t.Fatalf("enqueue third message failed: %v", err)
	}

	runner.release <- struct{}{}

	select {
	case <-runner.started:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for merged runner start")
	}

	req := runner.LastRequest()
	if got := len(req.Message.mergedMessages); got != 2 {
		t.Fatalf("unexpected merged message count: %d", got)
	}
	if req.Message.mergedMessages[0].ID != "evt-merge-2" || req.Message.mergedMessages[1].ID != "evt-merge-3" {
		t.Fatalf("unexpected merged message ids: %+v", req.Message.mergedMessages)
	}
	if req.Message.ID != "evt-merge-3" {
		t.Fatalf("unexpected combined message id: %s", req.Message.ID)
	}

	runner.release <- struct{}{}

	deadline := time.After(time.Second)
	for runner.Calls() < 2 || responder3.SendCalls() < 1 || responder2.CleanupCalls() < 1 || responder3.CleanupCalls() < 1 {
		select {
		case <-time.After(10 * time.Millisecond):
		case <-deadline:
			t.Fatal("timed out waiting for merged run completion")
		}
	}

	if responder1.SendCalls() != 1 {
		t.Fatalf("unexpected first responder send calls: %d", responder1.SendCalls())
	}
	if responder2.SendCalls() != 0 {
		t.Fatalf("unexpected second responder send calls: %d", responder2.SendCalls())
	}
	if responder3.SendCalls() != 1 {
		t.Fatalf("unexpected third responder send calls: %d", responder3.SendCalls())
	}
	if responder2.CleanupCalls() != 1 || responder3.CleanupCalls() != 1 {
		t.Fatalf("unexpected merged responder cleanup calls: second=%d third=%d", responder2.CleanupCalls(), responder3.CleanupCalls())
	}

	if err := dispatcher.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown failed: %v", err)
	}
}

func TestDispatcherUsesResponder(t *testing.T) {
	t.Parallel()

	runner := &testRunner{}
	responder := &testResponder{}
	dispatcher := NewDispatcher(DispatcherOptions{
		Store:       NewConversationStore(cache.NewMemoryStorage()),
		Runner:      runner,
		WorkerCount: 1,
	})
	_ = dispatcher.Start()

	if err := dispatcher.Enqueue(context.Background(), InboundMessage{
		ID:              "evt-reply-1",
		ConversationKey: "private:e_1:msg-reply-1",
		Text:            "hello",
		Responder:       responder,
	}); err != nil {
		t.Fatalf("enqueue failed: %v", err)
	}

	deadline := time.After(time.Second)
	for responder.SendCalls() < 1 {
		select {
		case <-time.After(10 * time.Millisecond):
		case <-deadline:
			t.Fatal("timed out waiting for responder send")
		}
	}

	got := responder.Reply()
	if got.text != "hello" {
		t.Fatalf("unexpected reply text: %q", got.text)
	}
	if responder.CleanupCalls() != 1 {
		t.Fatalf("unexpected cleanup call count: %d", responder.CleanupCalls())
	}

	if err := dispatcher.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown failed: %v", err)
	}
}

func TestDispatcherDelaysNonTextMessageUntilFollowUpTextArrives(t *testing.T) {
	t.Parallel()

	runner := &testRunner{}
	dispatcher := NewDispatcher(DispatcherOptions{
		Store:              NewConversationStore(cache.NewMemoryStorage()),
		Runner:             runner,
		WorkerCount:        1,
		NonTextMergeWindow: 200 * time.Millisecond,
	})
	_ = dispatcher.Start()

	if err := dispatcher.Enqueue(context.Background(), InboundMessage{
		ID:              "evt-delay-1",
		ConversationKey: "private:e_1:0",
		Kind:            MessageKindImage,
	}); err != nil {
		t.Fatalf("enqueue non-text message failed: %v", err)
	}

	time.Sleep(50 * time.Millisecond)
	if got := runner.Calls(); got != 0 {
		t.Fatalf("unexpected runner call count before follow-up text: %d", got)
	}

	if err := dispatcher.Enqueue(context.Background(), InboundMessage{
		ID:              "evt-delay-2",
		ConversationKey: "private:e_1:0",
		Kind:            MessageKindText,
		Text:            "this explains the image",
	}); err != nil {
		t.Fatalf("enqueue follow-up text failed: %v", err)
	}

	deadline := time.After(time.Second)
	for runner.Calls() < 1 {
		select {
		case <-time.After(10 * time.Millisecond):
		case <-deadline:
			t.Fatal("timed out waiting for delayed batch to run")
		}
	}

	req := runner.LastRequest()
	if got := len(req.Message.mergedMessages); got != 2 {
		t.Fatalf("unexpected merged message count: %d", got)
	}
	if req.Message.mergedMessages[0].ID != "evt-delay-1" || req.Message.mergedMessages[1].ID != "evt-delay-2" {
		t.Fatalf("unexpected merged message ids: %+v", req.Message.mergedMessages)
	}

	if err := dispatcher.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown failed: %v", err)
	}
}

func TestDispatcherFlushesDelayedNonTextMessageAfterTimeout(t *testing.T) {
	t.Parallel()

	runner := &testRunner{}
	dispatcher := NewDispatcher(DispatcherOptions{
		Store:              NewConversationStore(cache.NewMemoryStorage()),
		Runner:             runner,
		WorkerCount:        1,
		NonTextMergeWindow: 50 * time.Millisecond,
	})
	_ = dispatcher.Start()

	if err := dispatcher.Enqueue(context.Background(), InboundMessage{
		ID:              "evt-delay-timeout-1",
		ConversationKey: "private:e_1:0",
		Kind:            MessageKindImage,
	}); err != nil {
		t.Fatalf("enqueue non-text message failed: %v", err)
	}

	deadline := time.After(time.Second)
	for runner.Calls() < 1 {
		select {
		case <-time.After(10 * time.Millisecond):
		case <-deadline:
			t.Fatal("timed out waiting for delayed non-text message to flush")
		}
	}

	req := runner.LastRequest()
	if req.Message.ID != "evt-delay-timeout-1" {
		t.Fatalf("unexpected message id: %s", req.Message.ID)
	}
	if len(req.Message.mergedMessages) != 0 {
		t.Fatalf("unexpected merged messages: %+v", req.Message.mergedMessages)
	}

	if err := dispatcher.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown failed: %v", err)
	}
}

func TestDispatcherRequeuesPendingBatchWhenInitialEnqueueFails(t *testing.T) {
	t.Parallel()

	dispatcher := NewDispatcher(DispatcherOptions{
		Store:       NewConversationStore(cache.NewMemoryStorage()),
		QueueSize:   1,
		WorkerCount: 1,
	})

	if _, err := dispatcher.queue.Enqueue(context.Background(), dispatcher.stopCh, InboundMessage{
		ID:              "evt-prefill",
		ConversationKey: "private:e_other:0",
		Text:            "prefill",
	}); err != nil {
		t.Fatalf("prefill queue failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- dispatcher.Enqueue(ctx, InboundMessage{
			ID:              "evt-primary",
			ConversationKey: "private:e_1:0",
			Text:            "primary",
		})
	}()

	deadline := time.After(time.Second)
	for {
		dispatcher.pendingMu.Lock()
		state := dispatcher.pending["private:e_1:0"]
		queued := state != nil && state.queued
		dispatcher.pendingMu.Unlock()
		if queued {
			break
		}

		select {
		case <-time.After(10 * time.Millisecond):
		case <-deadline:
			t.Fatal("timed out waiting for conversation to become queued")
		}
	}

	if err := dispatcher.Enqueue(context.Background(), InboundMessage{
		ID:              "evt-merged",
		ConversationKey: "private:e_1:0",
		Text:            "merged",
	}); err != nil {
		t.Fatalf("enqueue merged message failed: %v", err)
	}

	cancel()

	deadline = time.After(time.Second)
	for {
		message, ok := dispatcher.queue.TryDequeue()
		if ok {
			if message.ID != "evt-prefill" {
				t.Fatalf("unexpected prefill message id: %s", message.ID)
			}
			break
		}

		select {
		case <-time.After(10 * time.Millisecond):
		case <-deadline:
			t.Fatal("timed out waiting to drain prefilled queue item")
		}
	}

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected canceled enqueue error, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for initial enqueue to fail")
	}

	deadline = time.After(time.Second)
	for {
		message, ok := dispatcher.queue.TryDequeue()
		if ok {
			if message.ID != "evt-merged" {
				t.Fatalf("unexpected requeued message id: %s", message.ID)
			}
			if message.Text != "merged" {
				t.Fatalf("unexpected requeued message text: %s", message.Text)
			}
			break
		}

		select {
		case <-time.After(10 * time.Millisecond):
		case <-deadline:
			t.Fatal("timed out waiting for pending message to be requeued")
		}
	}

	dispatcher.pendingMu.Lock()
	state := dispatcher.pending["private:e_1:0"]
	dispatcher.pendingMu.Unlock()
	if state == nil || !state.queued || state.active || len(state.batch) != 0 {
		t.Fatalf("unexpected pending state after requeue: %+v", state)
	}
}

func TestDispatcherClaimNextPublishesConversationBeforeWorkerRuns(t *testing.T) {
	t.Parallel()

	dispatcher := NewDispatcher(DispatcherOptions{
		Store: NewConversationStore(cache.NewMemoryStorage()),
	})

	message := InboundMessage{
		ID:              "evt-claim",
		ConversationKey: "private:e_claim:0",
		Text:            "hello",
	}
	if err := dispatcher.enqueueReadyMessage(context.Background(), message); err != nil {
		t.Fatalf("enqueue ready message failed: %v", err)
	}

	claimed, ok := dispatcher.queue.ClaimNext(dispatcher.stopCh, dispatcher.claimConversationWork)
	if !ok {
		t.Fatal("expected queue claim to succeed")
	}
	if claimed.ID != message.ID {
		t.Fatalf("unexpected claimed message id: %s", claimed.ID)
	}

	active := dispatcher.activeConversationWork(message.ConversationKey)
	if active == nil {
		t.Fatal("expected claimed conversation to be visible as active")
	}

	dispatcher.pendingMu.Lock()
	state := dispatcher.pending[message.ConversationKey]
	dispatcher.pendingMu.Unlock()
	if state == nil || !state.active || state.queued {
		t.Fatalf("unexpected pending state after claim: %+v", state)
	}

	dispatcher.releaseClaimedConversationWork(message.ConversationKey)()
}

func TestDispatcherDelaysForwardedMessageUntilFollowUpTextArrives(t *testing.T) {
	t.Parallel()

	runner := &testRunner{}
	dispatcher := NewDispatcher(DispatcherOptions{
		Store:              NewConversationStore(cache.NewMemoryStorage()),
		Runner:             runner,
		WorkerCount:        1,
		NonTextMergeWindow: 200 * time.Millisecond,
	})
	_ = dispatcher.Start()

	if err := dispatcher.Enqueue(context.Background(), InboundMessage{
		ID:              "evt-forwarded-1",
		ConversationKey: "private:e_1:0",
		Kind:            MessageKindForwarded,
		ForwardedMessages: []ReferencedMessage{
			{Kind: MessageKindText, Text: "forwarded hello"},
		},
	}); err != nil {
		t.Fatalf("enqueue forwarded message failed: %v", err)
	}

	time.Sleep(50 * time.Millisecond)
	if got := runner.Calls(); got != 0 {
		t.Fatalf("unexpected runner call count before follow-up text: %d", got)
	}

	if err := dispatcher.Enqueue(context.Background(), InboundMessage{
		ID:              "evt-forwarded-2",
		ConversationKey: "private:e_1:0",
		Kind:            MessageKindText,
		Text:            "this explains the forwarded message",
	}); err != nil {
		t.Fatalf("enqueue follow-up text failed: %v", err)
	}

	deadline := time.After(time.Second)
	for runner.Calls() < 1 {
		select {
		case <-time.After(10 * time.Millisecond):
		case <-deadline:
			t.Fatal("timed out waiting for delayed forwarded batch to run")
		}
	}

	req := runner.LastRequest()
	if got := len(req.Message.mergedMessages); got != 2 {
		t.Fatalf("unexpected merged message count: %d", got)
	}
	if req.Message.mergedMessages[0].ID != "evt-forwarded-1" || req.Message.mergedMessages[1].ID != "evt-forwarded-2" {
		t.Fatalf("unexpected merged message ids: %+v", req.Message.mergedMessages)
	}

	if err := dispatcher.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown failed: %v", err)
	}
}

func TestDispatcherHandleStopCancelsTurnAndRepliesWithDiscardedMessages(t *testing.T) {
	t.Parallel()

	runner := &testRunner{
		started:       make(chan struct{}, 1),
		canceled:      make(chan struct{}, 1),
		waitForCancel: true,
	}
	dispatcher := NewDispatcher(DispatcherOptions{
		Store:       NewConversationStore(cache.NewMemoryStorage()),
		Runner:      runner,
		WorkerCount: 1,
	})
	_ = dispatcher.Start()

	activeResponder := &testResponder{}
	if err := dispatcher.Enqueue(context.Background(), InboundMessage{
		ID:              "evt-active",
		ConversationKey: "private:e_1:0",
		Kind:            MessageKindText,
		Text:            "running turn",
		Responder:       activeResponder,
	}); err != nil {
		t.Fatalf("enqueue active message failed: %v", err)
	}
	select {
	case <-runner.started:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for active turn")
	}

	queuedResponder := &testResponder{}
	if err := dispatcher.Enqueue(context.Background(), InboundMessage{
		ID:              "evt-queued",
		ConversationKey: "private:e_1:0",
		Kind:            MessageKindImage,
		Text:            "queued follow-up should be discarded because it never ran",
		Responder:       queuedResponder,
	}); err != nil {
		t.Fatalf("enqueue queued message failed: %v", err)
	}

	commandResponder := &testResponder{}
	err := dispatcher.EnqueueCommand(context.Background(), CommandRequest{
		ConversationKey: "private:e_1:0",
		EventID:         "evt-stop",
		Responder:       commandResponder,
		Command:         SlashCommand{Name: "stop", Raw: "/stop"},
	})
	if err != nil {
		t.Fatalf("enqueue command failed: %v", err)
	}

	select {
	case <-runner.canceled:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for runner cancellation")
	}
	if err := waitForResponderSend(commandResponder); err != nil {
		t.Fatalf("timed out waiting for command reply: %v", err)
	}

	reply := commandResponder.Reply().text
	if !strings.Contains(reply, "_Conversation interrupted._") {
		t.Fatalf("unexpected command reply: %q", reply)
	}
	if !strings.Contains(reply, "_Quote the messages above if you want to continue from them._") {
		t.Fatalf("unexpected command reply: %q", reply)
	}
	if !strings.Contains(reply, "queued follow-up should be discarded because it never ran") {
		t.Fatalf("expected discarded queued message in reply: %q", reply)
	}
	if runner.LastInterrupt().Key != "private:e_1:0" {
		t.Fatalf("unexpected interrupted conversation: %+v", runner.LastInterrupt())
	}
}

func TestDispatcherHandleStopCancelsTurnWhileSessionStarting(t *testing.T) {
	t.Parallel()

	runner := &testRunner{
		startSessionStart: make(chan struct{}, 1),
		startSessionWait:  make(chan struct{}),
		started:           make(chan struct{}, 1),
		canceled:          make(chan struct{}, 1),
		waitForCancel:     true,
	}
	dispatcher := NewDispatcher(DispatcherOptions{
		Store:       NewConversationStore(cache.NewMemoryStorage()),
		Runner:      runner,
		WorkerCount: 1,
	})
	_ = dispatcher.Start()

	activeResponder := &testResponder{}
	if err := dispatcher.Enqueue(context.Background(), InboundMessage{
		ID:              "evt-active-starting",
		ConversationKey: "private:e_1:stop-starting",
		Kind:            MessageKindText,
		Text:            "turn is still starting",
		Responder:       activeResponder,
	}); err != nil {
		t.Fatalf("enqueue active message failed: %v", err)
	}

	select {
	case <-runner.startSessionStart:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for start session")
	}

	commandResponder := &testResponder{}
	if err := dispatcher.EnqueueCommand(context.Background(), CommandRequest{
		ConversationKey: "private:e_1:stop-starting",
		EventID:         "evt-stop-starting",
		Responder:       commandResponder,
		Command:         SlashCommand{Name: "stop", Raw: "/stop"},
	}); err != nil {
		t.Fatalf("enqueue command failed: %v", err)
	}

	time.Sleep(50 * time.Millisecond)
	if got := commandResponder.SendCalls(); got != 0 {
		t.Fatalf("stop reply sent before session became ready, got %d", got)
	}

	close(runner.startSessionWait)

	time.Sleep(50 * time.Millisecond)
	if got := runner.Calls(); got != 0 {
		t.Fatalf("expected no turn start after session became ready, got %d", got)
	}

	if err := waitForResponderSend(commandResponder); err != nil {
		t.Fatalf("timed out waiting for command reply: %v", err)
	}

	reply := commandResponder.Reply().text
	if !strings.Contains(reply, "_Conversation interrupted._") {
		t.Fatalf("unexpected command reply: %q", reply)
	}
	if got := runner.Calls(); got != 0 {
		t.Fatalf("expected no turn run after session became ready, got %d", got)
	}
	if got := runner.InterruptCalls(); got != 1 {
		t.Fatalf("expected one session interrupt after session became ready, got %d", got)
	}
	if got := activeResponder.SendCalls(); got != 0 {
		t.Fatalf("expected no reply from canceled turn, got %d", got)
	}
}

func TestDispatcherHandleStopSuppressesReplyFromInterruptedTurn(t *testing.T) {
	t.Parallel()

	runner := &testRunner{
		started:       make(chan struct{}, 1),
		canceled:      make(chan struct{}, 1),
		waitForCancel: true,
		replyOnCancel: "partial reply that should be suppressed",
	}
	dispatcher := NewDispatcher(DispatcherOptions{
		Store:       NewConversationStore(cache.NewMemoryStorage()),
		Runner:      runner,
		WorkerCount: 1,
	})
	_ = dispatcher.Start()

	activeResponder := &testResponder{}
	if err := dispatcher.Enqueue(context.Background(), InboundMessage{
		ID:              "evt-active-suppress",
		ConversationKey: "private:e_1:stop-suppress",
		Kind:            MessageKindText,
		Text:            "running turn",
		Responder:       activeResponder,
	}); err != nil {
		t.Fatalf("enqueue active message failed: %v", err)
	}
	select {
	case <-runner.started:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for active turn")
	}

	commandResponder := &testResponder{}
	if err := dispatcher.EnqueueCommand(context.Background(), CommandRequest{
		ConversationKey: "private:e_1:stop-suppress",
		EventID:         "evt-stop-suppress",
		Responder:       commandResponder,
		Command:         SlashCommand{Name: "stop", Raw: "/stop"},
	}); err != nil {
		t.Fatalf("enqueue command failed: %v", err)
	}

	select {
	case <-runner.canceled:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for runner cancellation")
	}
	if err := waitForResponderSend(commandResponder); err != nil {
		t.Fatalf("timed out waiting for command reply: %v", err)
	}
	if got := activeResponder.SendCalls(); got != 0 {
		t.Fatalf("expected interrupted turn reply to be suppressed, got send count %d", got)
	}
}

func TestDispatcherHandleStopPreservesDirtyConversationState(t *testing.T) {
	t.Parallel()

	store := NewConversationStore(cache.NewMemoryStorage())
	runner := &testRunner{
		startSessionID: "thread-stop",
		started:        make(chan struct{}, 1),
		canceled:       make(chan struct{}, 1),
		waitForCancel:  true,
	}
	dispatcher := NewDispatcher(DispatcherOptions{
		Store:       store,
		Runner:      runner,
		WorkerCount: 1,
	})
	_ = dispatcher.Start()

	if err := dispatcher.Enqueue(context.Background(), InboundMessage{
		ID:              "evt-active-dirty",
		ConversationKey: "private:e_1:stop-dirty",
		Kind:            MessageKindText,
		Text:            "running turn",
		Responder:       &testResponder{},
	}); err != nil {
		t.Fatalf("enqueue active message failed: %v", err)
	}
	select {
	case <-runner.started:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for active turn")
	}

	commandResponder := &testResponder{}
	if err := dispatcher.EnqueueCommand(context.Background(), CommandRequest{
		ConversationKey: "private:e_1:stop-dirty",
		EventID:         "evt-stop-dirty",
		Responder:       commandResponder,
		Command:         SlashCommand{Name: "stop", Raw: "/stop"},
	}); err != nil {
		t.Fatalf("enqueue command failed: %v", err)
	}

	select {
	case <-runner.canceled:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for runner cancellation")
	}
	if err := waitForResponderSend(commandResponder); err != nil {
		t.Fatalf("timed out waiting for command reply: %v", err)
	}

	state, err := store.GetConversation(context.Background(), "private:e_1:stop-dirty")
	if err != nil {
		t.Fatalf("get conversation failed: %v", err)
	}
	if state.RunnerThreadID != "thread-stop" {
		t.Fatalf("unexpected runner thread id: %q", state.RunnerThreadID)
	}
}

func TestDispatcherHandleStopWaitsForTurnCompletionBeforeReply(t *testing.T) {
	t.Parallel()

	runner := &testRunner{
		started:       make(chan struct{}, 1),
		canceled:      make(chan struct{}, 1),
		release:       make(chan struct{}),
		waitForCancel: true,
	}
	dispatcher := NewDispatcher(DispatcherOptions{
		Store:       NewConversationStore(cache.NewMemoryStorage()),
		Runner:      runner,
		WorkerCount: 1,
	})
	_ = dispatcher.Start()

	if err := dispatcher.Enqueue(context.Background(), InboundMessage{
		ID:              "evt-active-wait",
		ConversationKey: "private:e_wait:0",
		Kind:            MessageKindText,
		Text:            "running turn",
		Responder:       &testResponder{},
	}); err != nil {
		t.Fatalf("enqueue active message failed: %v", err)
	}
	select {
	case <-runner.started:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for active turn")
	}

	commandResponder := &testResponder{}
	if err := dispatcher.EnqueueCommand(context.Background(), CommandRequest{
		ConversationKey: "private:e_wait:0",
		EventID:         "evt-stop-wait",
		Responder:       commandResponder,
		Command:         SlashCommand{Name: "stop", Raw: "/stop"},
	}); err != nil {
		t.Fatalf("enqueue command failed: %v", err)
	}

	select {
	case <-runner.canceled:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for runner cancellation")
	}

	time.Sleep(50 * time.Millisecond)
	if commandResponder.SendCalls() != 0 {
		t.Fatal("stop reply sent before active turn completed")
	}

	close(runner.release)

	if err := waitForResponderSend(commandResponder); err != nil {
		t.Fatalf("timed out waiting for command reply: %v", err)
	}
}

func TestDispatcherHandleStopDropsQueuedConversationMessage(t *testing.T) {
	t.Parallel()

	runner := &testRunner{
		started: make(chan struct{}, 1),
		release: make(chan struct{}),
	}
	dispatcher := NewDispatcher(DispatcherOptions{
		Store:       NewConversationStore(cache.NewMemoryStorage()),
		Runner:      runner,
		WorkerCount: 1,
	})
	_ = dispatcher.Start()

	if err := dispatcher.Enqueue(context.Background(), InboundMessage{
		ID:              "evt-active-other",
		ConversationKey: "private:e_other:0",
		Kind:            MessageKindText,
		Text:            "keep worker busy",
		Responder:       &testResponder{},
	}); err != nil {
		t.Fatalf("enqueue active message failed: %v", err)
	}
	select {
	case <-runner.started:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for blocking turn")
	}

	queuedResponder := &testResponder{}
	if err := dispatcher.Enqueue(context.Background(), InboundMessage{
		ID:              "evt-queued-target",
		ConversationKey: "private:e_1:0",
		Kind:            MessageKindText,
		Text:            "queued in global queue and should be discarded",
		Responder:       queuedResponder,
	}); err != nil {
		t.Fatalf("enqueue queued target message failed: %v", err)
	}

	commandResponder := &testResponder{}
	if err := dispatcher.EnqueueCommand(context.Background(), CommandRequest{
		ConversationKey: "private:e_1:0",
		EventID:         "evt-stop-target",
		Responder:       commandResponder,
		Command:         SlashCommand{Name: "stop", Raw: "/stop"},
	}); err != nil {
		t.Fatalf("enqueue command failed: %v", err)
	}

	if err := waitForResponderSend(commandResponder); err != nil {
		t.Fatalf("timed out waiting for command reply: %v", err)
	}

	reply := commandResponder.Reply().text
	if !strings.Contains(reply, "queued in global queue and should be discarded") {
		t.Fatalf("expected queued global message in reply: %q", reply)
	}

	close(runner.release)

	deadline := time.After(500 * time.Millisecond)
	for {
		if runner.Calls() == 1 {
			select {
			case <-time.After(10 * time.Millisecond):
			case <-deadline:
				return
			}
			continue
		}
		t.Fatalf("expected dropped queued message to never run, got %d runner calls", runner.Calls())
	}
}

func TestDispatcherHandleStopDropsIncomingMessagesDuringCommand(t *testing.T) {
	t.Parallel()

	runner := &testRunner{
		started:          make(chan struct{}, 1),
		release:          make(chan struct{}),
		waitForCancel:    true,
		interruptStarted: make(chan struct{}, 1),
		interruptRelease: make(chan struct{}),
	}
	store := NewConversationStore(cache.NewMemoryStorage())
	err := store.PutConversation(context.Background(), ConversationState{
		Key:            "private:e_2:0",
		RunnerThreadID: "thread-stop",
		LastEventID:    "evt-prev",
		LastActivityAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("put conversation failed: %v", err)
	}
	dispatcher := NewDispatcher(DispatcherOptions{
		Store:       store,
		Runner:      runner,
		WorkerCount: 1,
	})
	_ = dispatcher.Start()
	managedSession, err := dispatcher.ensureConversationSession(context.Background(), ConversationState{
		Key:            "private:e_2:0",
		RunnerThreadID: "thread-stop",
	})
	if err != nil {
		t.Fatalf("ensure managed session failed: %v", err)
	}
	if managedSession == nil {
		t.Fatal("expected managed session")
	}

	commandResponder := &testResponder{}
	if err = dispatcher.Enqueue(context.Background(), InboundMessage{
		ID:              "evt-active-stop",
		ConversationKey: "private:e_2:0",
		Kind:            MessageKindText,
		Text:            "active turn",
		Responder:       &testResponder{},
	}); err != nil {
		t.Fatalf("enqueue active message failed: %v", err)
	}

	select {
	case <-runner.started:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for active turn")
	}

	if err = dispatcher.EnqueueCommand(context.Background(), CommandRequest{
		ConversationKey: "private:e_2:0",
		EventID:         "evt-stop",
		Responder:       commandResponder,
		Command:         SlashCommand{Name: "stop", Raw: "/stop"},
	}); err != nil {
		t.Fatalf("enqueue command failed: %v", err)
	}

	select {
	case <-runner.interruptStarted:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for interrupt start")
	}

	incomingResponder := &testResponder{}
	if err := dispatcher.Enqueue(context.Background(), InboundMessage{
		ID:              "evt-dropped",
		ConversationKey: "private:e_2:0",
		Kind:            MessageKindText,
		Text:            "incoming message dropped while stop is running",
		Responder:       incomingResponder,
	}); err != nil {
		t.Fatalf("enqueue incoming message failed: %v", err)
	}

	close(runner.interruptRelease)
	close(runner.release)

	if err := waitForResponderSend(commandResponder); err != nil {
		t.Fatalf("timed out waiting for enqueue command reply: %v", err)
	}

	reply := commandResponder.Reply().text
	if !strings.Contains(reply, "incoming message dropped while stop is running") {
		t.Fatalf("expected dropped incoming message in reply: %q", reply)
	}
	if got := incomingResponder.CleanupCalls(); got != 1 {
		t.Fatalf("unexpected incoming cleanup count: %d", got)
	}
}

func TestDispatcherHelpDoesNotInterruptRunningTurn(t *testing.T) {
	t.Parallel()

	runner := &testRunner{
		started: make(chan struct{}, 1),
		release: make(chan struct{}),
	}
	dispatcher := NewDispatcher(DispatcherOptions{
		Store:       NewConversationStore(cache.NewMemoryStorage()),
		Runner:      runner,
		WorkerCount: 1,
	})
	_ = dispatcher.Start()

	if err := dispatcher.Enqueue(context.Background(), InboundMessage{
		ID:              "evt-active-help",
		ConversationKey: "private:e_help:0",
		Kind:            MessageKindText,
		Text:            "keep running",
		Responder:       &testResponder{},
	}); err != nil {
		t.Fatalf("enqueue active message failed: %v", err)
	}
	select {
	case <-runner.started:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for active turn")
	}

	commandResponder := &testResponder{}
	if err := dispatcher.EnqueueCommand(context.Background(), CommandRequest{
		ConversationKey: "private:e_help:0",
		EventID:         "evt-help",
		Responder:       commandResponder,
		Command:         SlashCommand{Name: "help", Raw: "/help"},
	}); err != nil {
		t.Fatalf("enqueue help command failed: %v", err)
	}

	if err := waitForResponderSend(commandResponder); err != nil {
		t.Fatalf("timed out waiting for help reply: %v", err)
	}

	if got := runner.InterruptCalls(); got != 0 {
		t.Fatalf("unexpected interrupt call count: %d", got)
	}

	reply := commandResponder.Reply().text
	if !strings.Contains(reply, "Supported slash commands:") {
		t.Fatalf("unexpected help reply: %q", reply)
	}
	if !strings.Contains(reply, "`/status`") {
		t.Fatalf("expected /status in help reply: %q", reply)
	}

	close(runner.release)
}

func TestDispatcherStopCommandDoesNotWaitForConversationLock(t *testing.T) {
	t.Parallel()

	dispatcher := NewDispatcher(DispatcherOptions{
		Store:  NewConversationStore(cache.NewMemoryStorage()),
		Runner: &testRunner{},
	})

	unlock := dispatcher.locks.Lock("private:e_1:stop-unlocked")
	defer unlock()

	replyCh := make(chan string, 1)
	errCh := make(chan error, 1)
	go func() {
		reply, err := dispatcher.executeBlockingCommand(context.Background(), CommandRequest{
			ConversationKey: "private:e_1:stop-unlocked",
			Command:         SlashCommand{Name: "stop", Raw: "/stop"},
		})
		if err != nil {
			errCh <- err
			return
		}
		replyCh <- reply
	}()

	select {
	case err := <-errCh:
		t.Fatalf("stop command failed: %v", err)
	case reply := <-replyCh:
		if !strings.Contains(reply, "_Conversation is not running._") {
			t.Fatalf("unexpected stop reply: %q", reply)
		}
	case <-time.After(time.Second):
		t.Fatal("stop command waited on conversation lock")
	}
}

func TestDispatcherStatusDoesNotDropInboundMessages(t *testing.T) {
	t.Parallel()

	runner := &testRunner{
		status: SessionStatus{
			Agent:              "codex",
			WorkingDirectories: []string{"/workspace", "/workspace/extra"},
			Modes: SessionModes{
				CurrentModeID: "workspace-write / never",
			},
			ConfigOptions: []SessionConfigOption{
				{Name: "model", CurrentValue: "gpt-5.4", Options: []SessionConfigOptionChoice{{Name: "gpt-5.4", Description: "Current default model"}}},
				{Name: "effort", CurrentValue: "medium", Options: []SessionConfigOptionChoice{{Name: "high", Description: "Deeper reasoning"}}},
			},
		},
		statusStarted: make(chan struct{}, 1),
		statusRelease: make(chan struct{}),
	}
	store := NewConversationStore(cache.NewMemoryStorage())
	err := store.PutConversation(context.Background(), ConversationState{
		Key:            "private:e_status:0",
		RunnerThreadID: "thread-status",
		LastEventID:    "evt-prev",
		LastActivityAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("put conversation failed: %v", err)
	}
	dispatcher := NewDispatcher(DispatcherOptions{
		Store:       store,
		Runner:      runner,
		WorkerCount: 1,
	})
	_ = dispatcher.Start()
	managedSession, err := dispatcher.ensureConversationSession(context.Background(), ConversationState{
		Key:            "private:e_status:0",
		RunnerThreadID: "thread-status",
	})
	if err != nil {
		t.Fatalf("ensure managed session failed: %v", err)
	}
	if managedSession == nil {
		t.Fatal("expected managed session")
	}

	commandResponder := &testResponder{}
	if err = dispatcher.EnqueueCommand(context.Background(), CommandRequest{
		ConversationKey: "private:e_status:0",
		EventID:         "evt-status",
		Responder:       commandResponder,
		Command:         SlashCommand{Name: "status", Raw: "/status"},
	}); err != nil {
		t.Fatalf("enqueue status command failed: %v", err)
	}

	select {
	case <-runner.statusStarted:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for status command start")
	}

	messageResponder := &testResponder{}
	if err := dispatcher.Enqueue(context.Background(), InboundMessage{
		ID:              "evt-normal-during-status",
		ConversationKey: "private:e_status:0",
		Kind:            MessageKindText,
		Text:            "normal message should still run",
		Responder:       messageResponder,
	}); err != nil {
		t.Fatalf("enqueue normal message failed: %v", err)
	}

	close(runner.statusRelease)

	if err := waitForResponderSend(commandResponder); err != nil {
		t.Fatalf("timed out waiting for status reply: %v", err)
	}

	deadline := time.After(time.Second)
	for runner.Calls() < 1 {
		select {
		case <-time.After(10 * time.Millisecond):
		case <-deadline:
			t.Fatal("timed out waiting for normal message turn")
		}
	}

	if got := runner.InterruptCalls(); got != 0 {
		t.Fatalf("unexpected interrupt call count: %d", got)
	}
	if got := runner.StartSessionCalls(); got != 1 {
		t.Fatalf("expected status to reuse existing managed session, got start session call count: %d", got)
	}
	if got := messageResponder.CleanupCalls(); got != 1 {
		t.Fatalf("unexpected message cleanup count: %d", got)
	}

	reply := commandResponder.Reply().text
	if !strings.Contains(reply, "_Current conversation status:_") ||
		!strings.Contains(reply, "- _Agent_: `codex`") ||
		!strings.Contains(reply, "- _Working directories_: `/workspace, /workspace/extra`") ||
		!strings.Contains(reply, "- _Config options:_\n  - _Name_: `model`\n    - _Current value_: `gpt-5.4`") ||
		!strings.Contains(reply, "  - _Name_: `effort`\n    - _Current value_: `medium`") ||
		strings.Contains(reply, "**Options**:") {
		t.Fatalf("unexpected status reply: %q", reply)
	}
}

func TestDispatcherStatusWithoutConversationThreadDoesNotStartSession(t *testing.T) {
	t.Parallel()

	runner := &testRunner{
		statusStarted: make(chan struct{}, 1),
	}
	dispatcher := NewDispatcher(DispatcherOptions{
		Store:       NewConversationStore(cache.NewMemoryStorage()),
		Runner:      runner,
		WorkerCount: 1,
	})
	_ = dispatcher.Start()

	commandResponder := &testResponder{}
	err := dispatcher.EnqueueCommand(context.Background(), CommandRequest{
		ConversationKey: "private:e_status_missing:0",
		EventID:         "evt-status-missing",
		Responder:       commandResponder,
		Command:         SlashCommand{Name: "status", Raw: "/status"},
	})
	if err != nil {
		t.Fatalf("enqueue status command failed: %v", err)
	}

	if err = waitForResponderSend(commandResponder); err != nil {
		t.Fatalf("timed out waiting for status reply: %v", err)
	}

	select {
	case <-runner.statusStarted:
		t.Fatal("unexpected status call")
	default:
	}
	if got := runner.StartSessionCalls(); got != 0 {
		t.Fatalf("unexpected start session call count: %d", got)
	}
	reply := commandResponder.Reply().text
	if !strings.Contains(reply, "_Current conversation status:_ _inactive_") {
		t.Fatalf("unexpected status reply: %q", reply)
	}
}

func TestDispatcherStatusLoadsPersistedSessionWhenConversationIsIdle(t *testing.T) {
	t.Parallel()

	runner := &testRunner{
		status: SessionStatus{
			Agent:              "codex",
			WorkingDirectories: []string{"/workspace"},
		},
		statusStarted: make(chan struct{}, 1),
	}
	store := NewConversationStore(cache.NewMemoryStorage())
	err := store.PutConversation(context.Background(), ConversationState{
		Key:            "private:e_status_load:0",
		RunnerThreadID: "thread-status-load",
		LastEventID:    "evt-prev",
		LastActivityAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("put conversation failed: %v", err)
	}
	dispatcher := NewDispatcher(DispatcherOptions{
		Store:       store,
		Runner:      runner,
		WorkerCount: 1,
	})
	_ = dispatcher.Start()

	commandResponder := &testResponder{}
	err = dispatcher.EnqueueCommand(context.Background(), CommandRequest{
		ConversationKey: "private:e_status_load:0",
		EventID:         "evt-status-load",
		Responder:       commandResponder,
		Command:         SlashCommand{Name: "status", Raw: "/status"},
	})
	if err != nil {
		t.Fatalf("enqueue status command failed: %v", err)
	}

	if err = waitForResponderSend(commandResponder); err != nil {
		t.Fatalf("timed out waiting for status reply: %v", err)
	}

	if got := runner.StartSessionCalls(); got != 1 {
		t.Fatalf("expected status to lazy load persisted session once, got %d", got)
	}
	if got := runner.StatusCalls(); got != 1 {
		t.Fatalf("expected one status call, got %d", got)
	}
	if got := runner.LastStatus().RunnerThreadID; got != "thread-status-load" {
		t.Fatalf("unexpected status thread id: %q", got)
	}
	session := dispatcher.managedConversationSession("private:e_status_load:0")
	if session == nil {
		t.Fatal("expected lazy-loaded session to remain managed")
	}
	if session.ID() != "thread-status-load" {
		t.Fatalf("unexpected managed session id: %q", session.ID())
	}
}

func TestDispatcherStatusDoesNotLoadPersistedSessionWhenConversationLockIsBusy(t *testing.T) {
	t.Parallel()

	runner := &testRunner{
		statusStarted: make(chan struct{}, 1),
	}
	store := NewConversationStore(cache.NewMemoryStorage())
	err := store.PutConversation(context.Background(), ConversationState{
		Key:            "private:e_status_busy:0",
		RunnerThreadID: "thread-status-busy",
		LastEventID:    "evt-prev",
		LastActivityAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("put conversation failed: %v", err)
	}
	dispatcher := NewDispatcher(DispatcherOptions{
		Store:       store,
		Runner:      runner,
		WorkerCount: 1,
	})
	_ = dispatcher.Start()

	unlock := dispatcher.locks.Lock("private:e_status_busy:0")
	defer unlock()

	commandResponder := &testResponder{}
	err = dispatcher.EnqueueCommand(context.Background(), CommandRequest{
		ConversationKey: "private:e_status_busy:0",
		EventID:         "evt-status-busy",
		Responder:       commandResponder,
		Command:         SlashCommand{Name: "status", Raw: "/status"},
	})
	if err != nil {
		t.Fatalf("enqueue status command failed: %v", err)
	}

	if err = waitForResponderSend(commandResponder); err != nil {
		t.Fatalf("timed out waiting for status reply: %v", err)
	}

	select {
	case <-runner.statusStarted:
		t.Fatal("unexpected status call")
	default:
	}
	if got := runner.StartSessionCalls(); got != 0 {
		t.Fatalf("unexpected start session call count: %d", got)
	}
	if session := dispatcher.managedConversationSession("private:e_status_busy:0"); session != nil {
		t.Fatal("expected no managed session when status lock acquisition fails")
	}
	reply := commandResponder.Reply().text
	if !strings.Contains(reply, "_Current conversation status:_ _inactive_") {
		t.Fatalf("unexpected status reply: %q", reply)
	}
}

func TestDispatcherCommandQueueBlocksConversationUntilQueuedCommandsDrain(t *testing.T) {
	t.Parallel()

	runner := &testRunner{
		started:          make(chan struct{}, 1),
		release:          make(chan struct{}),
		waitForCancel:    true,
		interruptStarted: make(chan struct{}, 2),
		interruptRelease: make(chan struct{}),
	}
	store := NewConversationStore(cache.NewMemoryStorage())
	err := store.PutConversation(context.Background(), ConversationState{
		Key:            "private:e_3:0",
		RunnerThreadID: "thread-command-queue",
		LastEventID:    "evt-prev",
		LastActivityAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("put conversation failed: %v", err)
	}
	dispatcher := NewDispatcher(DispatcherOptions{
		Store:       store,
		Runner:      runner,
		WorkerCount: 1,
	})
	_ = dispatcher.Start()
	managedSession, err := dispatcher.ensureConversationSession(context.Background(), ConversationState{
		Key:            "private:e_3:0",
		RunnerThreadID: "thread-command-queue",
	})
	if err != nil {
		t.Fatalf("ensure managed session failed: %v", err)
	}
	if managedSession == nil {
		t.Fatal("expected managed session")
	}

	firstResponder := &testResponder{}
	secondResponder := &testResponder{}
	if err := dispatcher.Enqueue(context.Background(), InboundMessage{
		ID:              "evt-active-command-queue",
		ConversationKey: "private:e_3:0",
		Kind:            MessageKindText,
		Text:            "active turn",
		Responder:       &testResponder{},
	}); err != nil {
		t.Fatalf("enqueue active message failed: %v", err)
	}

	select {
	case <-runner.started:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for active turn")
	}

	if err := dispatcher.EnqueueCommand(context.Background(), CommandRequest{
		ConversationKey: "private:e_3:0",
		EventID:         "evt-stop-1",
		Responder:       firstResponder,
		Command:         SlashCommand{Name: "stop", Raw: "/stop"},
	}); err != nil {
		t.Fatalf("first enqueue command failed: %v", err)
	}

	select {
	case <-runner.interruptStarted:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for first command interrupt")
	}

	if err := dispatcher.EnqueueCommand(context.Background(), CommandRequest{
		ConversationKey: "private:e_3:0",
		EventID:         "evt-stop-2",
		Responder:       secondResponder,
		Command:         SlashCommand{Name: "stop", Raw: "/stop"},
	}); err != nil {
		t.Fatalf("second enqueue command failed: %v", err)
	}

	droppedResponder := &testResponder{}
	if err := dispatcher.Enqueue(context.Background(), InboundMessage{
		ID:              "evt-dropped-between-commands",
		ConversationKey: "private:e_3:0",
		Kind:            MessageKindText,
		Text:            "dropped while another command is queued",
		Responder:       droppedResponder,
	}); err != nil {
		t.Fatalf("enqueue dropped message failed: %v", err)
	}

	close(runner.interruptRelease)
	close(runner.release)

	if err := waitForResponderSend(firstResponder); err != nil {
		t.Fatalf("timed out waiting for first command reply: %v", err)
	}
	if err := waitForResponderSend(secondResponder); err != nil {
		t.Fatalf("timed out waiting for second command reply: %v", err)
	}

	firstReply := firstResponder.Reply().text
	secondReply := secondResponder.Reply().text
	if !strings.Contains(firstReply, "dropped while another command is queued") &&
		!strings.Contains(secondReply, "dropped while another command is queued") {
		t.Fatalf("expected queued command replies to include dropped message: first=%q second=%q", firstReply, secondReply)
	}
	if firstResponder.CleanupCalls() != 1 || secondResponder.CleanupCalls() != 1 {
		t.Fatalf("unexpected command cleanup counts: first=%d second=%d", firstResponder.CleanupCalls(), secondResponder.CleanupCalls())
	}
	if droppedResponder.CleanupCalls() != 1 {
		t.Fatalf("unexpected dropped responder cleanup count: %d", droppedResponder.CleanupCalls())
	}
	if runner.LastInterrupt().Key != "private:e_3:0" {
		t.Fatalf("unexpected interrupted conversation after queued commands: %+v", runner.LastInterrupt())
	}
}

func TestDispatcherResetWaitsForTurnCompletionBeforeDeletingConversationState(t *testing.T) {
	t.Parallel()

	store := NewConversationStore(cache.NewMemoryStorage())
	runner := &testRunner{
		started:       make(chan struct{}, 1),
		canceled:      make(chan struct{}, 1),
		release:       make(chan struct{}),
		waitForCancel: true,
	}
	dispatcher := NewDispatcher(DispatcherOptions{
		Store:       store,
		Runner:      runner,
		WorkerCount: 1,
	})
	_ = dispatcher.Start()

	if err := dispatcher.Enqueue(context.Background(), InboundMessage{
		ID:              "evt-active-reset",
		ConversationKey: "private:e_reset_wait:0",
		Kind:            MessageKindText,
		Text:            "running turn",
		Responder:       &testResponder{},
	}); err != nil {
		t.Fatalf("enqueue active message failed: %v", err)
	}
	select {
	case <-runner.started:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for active turn")
	}

	commandResponder := &testResponder{}
	if err := dispatcher.EnqueueCommand(context.Background(), CommandRequest{
		ConversationKey: "private:e_reset_wait:0",
		EventID:         "evt-reset-wait",
		Responder:       commandResponder,
		Command:         SlashCommand{Name: "reset", Raw: "/reset"},
	}); err != nil {
		t.Fatalf("enqueue reset command failed: %v", err)
	}

	select {
	case <-runner.canceled:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for runner cancellation")
	}

	time.Sleep(50 * time.Millisecond)
	if commandResponder.SendCalls() != 0 {
		t.Fatal("reset reply sent before active turn completed")
	}

	close(runner.release)

	if err := waitForResponderSend(commandResponder); err != nil {
		t.Fatalf("timed out waiting for reset reply: %v", err)
	}

	if _, err := store.GetConversation(context.Background(), "private:e_reset_wait:0"); !errors.Is(err, cache.ErrNotFound) {
		t.Fatalf("expected conversation state to remain deleted after reset, got %v", err)
	}
}
