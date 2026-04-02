package agent

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/hzj629206/assistant/cache"
)

type testRunner struct {
	mu            sync.Mutex
	calls         int
	lastReq       TurnRequest
	started       chan struct{}
	release       chan struct{}
	waitForCancel bool
	canceled      chan struct{}
	err           error
	panicV        any
}

func (r *testRunner) RunTurn(ctx context.Context, req TurnRequest) (TurnResult, error) {
	r.mu.Lock()
	r.calls++
	r.lastReq = req
	r.mu.Unlock()

	if r.started != nil {
		select {
		case r.started <- struct{}{}:
		default:
		}
	}
	if r.waitForCancel {
		<-ctx.Done()
		if r.canceled != nil {
			select {
			case r.canceled <- struct{}{}:
			default:
			}
		}
		return TurnResult{}, ctx.Err()
	}
	if r.release != nil {
		select {
		case <-r.release:
		case <-ctx.Done():
			if r.canceled != nil {
				select {
				case r.canceled <- struct{}{}:
				default:
				}
			}
			return TurnResult{}, ctx.Err()
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

	if got := runner.Calls(); got != 1 {
		t.Fatalf("unexpected runner call count: %d", got)
	}
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
