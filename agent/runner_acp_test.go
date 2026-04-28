package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hzj629206/assistant/agent/acp"
)

type fakeACPSession struct {
	sessionID    string
	replyText    string
	rawResult    json.RawMessage
	caps         acp.AgentCapabilities
	status       acp.SessionMetadata
	promptBlocks [][]acp.ContentBlock
	closed       bool
	runTurn      func(context.Context, []acp.ContentBlock) (acp.TurnResult, error)
	interrupt    func(context.Context) error
	mu           sync.Mutex
	currentTurn  *fakeACPScheduledTurn
}

type fakeACPScheduledTurn struct {
	session *fakeACPSession
	blocks  []acp.ContentBlock
	done    chan struct{}
}

func (t *fakeACPScheduledTurn) Run(ctx context.Context) (acp.TurnResult, error) {
	defer func() {
		t.session.clearCurrentTurn(t)
		close(t.done)
	}()
	return t.session.RunTurn(ctx, t.blocks)
}

func (t *fakeACPScheduledTurn) Interrupt(ctx context.Context) error {
	return t.session.interruptTurn(ctx)
}

func (t *fakeACPScheduledTurn) Done() <-chan struct{} { return t.done }

//nolint:contextcheck // Test fake uses the caller context only to wait for preemption of the previous turn.
func (s *fakeACPSession) ScheduleTurn(ctx context.Context, blocks []acp.ContentBlock) (acp.Turn, error) {
	s.mu.Lock()
	currentTurn := s.currentTurn
	s.mu.Unlock()
	if currentTurn != nil {
		if ctx == nil {
			ctx = context.Background()
		}
		if err := currentTurn.Interrupt(ctx); err != nil {
			return nil, err
		}
	}
	copied := make([]acp.ContentBlock, len(blocks))
	copy(copied, blocks)
	turn := &fakeACPScheduledTurn{
		session: s,
		blocks:  copied,
		done:    make(chan struct{}),
	}
	s.mu.Lock()
	s.currentTurn = turn
	s.mu.Unlock()
	return turn, nil
}

func (s *fakeACPSession) RunTurn(ctx context.Context, blocks []acp.ContentBlock) (acp.TurnResult, error) {
	if s.runTurn != nil {
		return s.runTurn(ctx, blocks)
	}
	copied := make([]acp.ContentBlock, len(blocks))
	copy(copied, blocks)
	s.promptBlocks = append(s.promptBlocks, copied)
	return acp.TurnResult{
		SessionID: s.sessionID,
		ReplyText: s.replyText,
		RawResult: append(json.RawMessage(nil), s.rawResult...),
	}, nil
}

func (s *fakeACPSession) SessionID() string {
	return s.sessionID
}

func (s *fakeACPSession) Status() acp.SessionMetadata {
	status := s.status
	status.ConfigOptions = append(json.RawMessage(nil), s.status.ConfigOptions...)
	status.Modes.AvailableModes = append([]acp.SessionMode(nil), s.status.Modes.AvailableModes...)
	if status.SessionID == "" {
		status.SessionID = s.sessionID
	}
	return status
}

func (s *fakeACPSession) Capabilities() acp.AgentCapabilities {
	return s.caps
}

func (s *fakeACPSession) interruptTurn(ctx context.Context) error {
	if s.interrupt != nil {
		return s.interrupt(ctx)
	}
	return nil
}

func (s *fakeACPSession) Close() error {
	s.closed = true
	return nil
}

func (s *fakeACPSession) clearCurrentTurn(turn *fakeACPScheduledTurn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.currentTurn == turn {
		s.currentTurn = nil
	}
}

func TestACPRunnerRunTurnRegistersHTTPToolServerForGlobalTools(t *testing.T) {
	runner := NewACPRunner(context.Background(), ACPRunnerOptions{
		Command:      "agent",
		Args:         []string{"acp"},
		SystemPrompt: "System rule",
		Tools:        []Tool{uppercaseTool{}},
	})

	fakeSession := &fakeACPSession{
		sessionID: "session-new",
		replyText: "done",
		caps:      acp.AgentCapabilities{MCP: acp.MCPCapabilities{HTTP: true}},
	}
	var captured acp.SessionOptions
	runner.sessionFactory = func(_ context.Context, options acp.SessionOptions) (acp.Session, error) {
		captured = options
		return fakeSession, nil
	}

	result, err := runTurnWithRunner(t, runner, TurnRequest{
		Conversation: ConversationState{Key: "conversation-1"},
		Message: InboundMessage{
			Text:   "hello",
			Kind:   MessageKindText,
			Sender: "user",
		},
	})
	if err != nil {
		t.Fatalf("RunTurn failed: %v", err)
	}

	if result.RunnerThreadID != "session-new" {
		t.Fatalf("unexpected session id: %s", result.RunnerThreadID)
	}
	if result.ReplyText != "done" {
		t.Fatalf("unexpected reply: %q", result.ReplyText)
	}
	if len(captured.MCPServers) != 1 {
		t.Fatalf("expected one MCP server, got %d", len(captured.MCPServers))
	}
	if captured.MCPServers[0].Type != "http" {
		t.Fatalf("unexpected MCP server type: %q", captured.MCPServers[0].Type)
	}
	if !strings.HasPrefix(captured.MCPServers[0].URL, "http://127.0.0.1:") {
		t.Fatalf("unexpected MCP server URL: %q", captured.MCPServers[0].URL)
	}
	if len(captured.MCPServers[0].Headers) != 2 {
		t.Fatalf("unexpected MCP headers: %#v", captured.MCPServers[0].Headers)
	}
	if captured.MCPServers[0].Headers[0].Name != "Authorization" || !strings.HasPrefix(captured.MCPServers[0].Headers[0].Value, "Bearer ") {
		t.Fatalf("unexpected auth header: %#v", captured.MCPServers[0].Headers[0])
	}
	if captured.MCPServers[0].Headers[1].Name != "Accept" || captured.MCPServers[0].Headers[1].Value != "application/json, text/event-stream" {
		t.Fatalf("unexpected accept header: %#v", captured.MCPServers[0].Headers[1])
	}
	if len(fakeSession.promptBlocks) != 1 {
		t.Fatalf("expected one prompt, got %d", len(fakeSession.promptBlocks))
	}
	if len(fakeSession.promptBlocks[0]) != 1 {
		t.Fatalf("expected one merged text block, got %d", len(fakeSession.promptBlocks[0]))
	}
	if text := textFromACPBlock(t, fakeSession.promptBlocks[0][0]); !strings.HasPrefix(text, "System rule\n\n") || strings.Contains(text, "\n\nSystem rule\n\n") {
		t.Fatalf("unexpected merged prompt block: %q", text)
	}
	if fakeSession.closed {
		t.Fatal("expected session to remain open after release")
	}
}

func TestACPRunnerRunTurnAuthorizesMCPTokenDuringSessionStartup(t *testing.T) {
	runner := NewACPRunner(context.Background(), ACPRunnerOptions{
		Command: "agent",
		Args:    []string{"acp"},
		Tools:   []Tool{uppercaseTool{}},
	})

	fakeSession := &fakeACPSession{
		sessionID: "session-new",
		replyText: "done",
		caps:      acp.AgentCapabilities{MCP: acp.MCPCapabilities{HTTP: true}},
	}
	runner.sessionFactory = func(_ context.Context, options acp.SessionOptions) (acp.Session, error) {
		if len(options.MCPServers) != 1 {
			t.Fatalf("expected one MCP server, got %d", len(options.MCPServers))
		}
		if len(options.MCPServers[0].Headers) == 0 {
			t.Fatalf("expected MCP auth header, got %#v", options.MCPServers[0].Headers)
		}
		token, ok := strings.CutPrefix(options.MCPServers[0].Headers[0].Value, "Bearer ")
		if !ok || token == "" {
			t.Fatalf("unexpected auth header: %#v", options.MCPServers[0].Headers[0])
		}
		if !runner.isAuthorizedACPToken(token) {
			t.Fatal("expected MCP token to be authorized during session startup")
		}
		return fakeSession, nil
	}

	_, err := runTurnWithRunner(t, runner, TurnRequest{
		Conversation: ConversationState{Key: "conversation-1"},
		Message: InboundMessage{
			Text:   "hello",
			Kind:   MessageKindText,
			Sender: "user",
		},
	})
	if err != nil {
		t.Fatalf("RunTurn failed: %v", err)
	}
}

func TestACPRunnerRunTurnResumesExistingSession(t *testing.T) {
	runner := NewACPRunner(context.Background(), ACPRunnerOptions{
		Command: "agent",
		Args:    []string{"acp"},
	})

	createCount := 0
	resumeIDs := make([]string, 0, 2)
	runner.sessionFactory = func(_ context.Context, options acp.SessionOptions) (acp.Session, error) {
		createCount++
		resumeIDs = append(resumeIDs, options.ResumeSessionID)
		return &fakeACPSession{
			sessionID: "session-live",
			replyText: "ok",
			caps:      acp.AgentCapabilities{MCP: acp.MCPCapabilities{HTTP: true}},
		}, nil
	}

	first, err := runTurnWithRunner(t, runner, TurnRequest{
		Conversation: ConversationState{Key: "conversation-1"},
		Message:      InboundMessage{Text: "hello", Kind: MessageKindText, Sender: "user"},
	})
	if err != nil {
		t.Fatalf("first RunTurn failed: %v", err)
	}
	second, err := runTurnWithRunner(t, runner, TurnRequest{
		Conversation: ConversationState{Key: "conversation-1", RunnerThreadID: first.RunnerThreadID},
		Message:      InboundMessage{Text: "again", Kind: MessageKindText, Sender: "user"},
	})
	if err != nil {
		t.Fatalf("second RunTurn failed: %v", err)
	}

	if createCount != 2 {
		t.Fatalf("unexpected session create count: %d", createCount)
	}
	if len(resumeIDs) != 2 || resumeIDs[0] != "" || resumeIDs[1] != "session-live" {
		t.Fatalf("unexpected resume ids: %#v", resumeIDs)
	}
	if second.RunnerThreadID != "session-live" {
		t.Fatalf("unexpected reused session id: %q", second.RunnerThreadID)
	}
}

func TestACPRunnerSessionRunTurnReturnsErrorOnConversationMismatch(t *testing.T) {
	runner := NewACPRunner(context.Background(), ACPRunnerOptions{
		Command: "agent",
		Args:    []string{"acp"},
	})
	runner.sessionFactory = func(_ context.Context, _ acp.SessionOptions) (acp.Session, error) {
		return &fakeACPSession{
			sessionID: "session-live",
			replyText: "ok",
		}, nil
	}
	session := startTestSession(t, runner, ConversationState{
		Key:            "conversation-1",
		RunnerThreadID: "session-live",
	})

	_, err := runSessionTurn(context.Background(), session, TurnRequest{
		Conversation: ConversationState{
			Key:            "conversation-2",
			RunnerThreadID: "session-live",
		},
		Message: InboundMessage{Text: "hello", Kind: MessageKindText, Sender: "user"},
	})
	if err == nil || !strings.Contains(err.Error(), `conversation key mismatch: session="conversation-1" request="conversation-2"`) {
		t.Fatalf("unexpected error: %v", err)
	}

	_, err = runSessionTurn(context.Background(), session, TurnRequest{
		Conversation: ConversationState{
			Key:            "conversation-1",
			RunnerThreadID: "session-other",
		},
		Message: InboundMessage{Text: "hello", Kind: MessageKindText, Sender: "user"},
	})
	if err == nil || !strings.Contains(err.Error(), `runner thread id mismatch: session="session-live" request="session-other"`) {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestACPRunnerSessionFactoryContextOutlivesOneTurn(t *testing.T) {
	runner := NewACPRunner(context.Background(), ACPRunnerOptions{
		Command: "agent",
		Args:    []string{"acp"},
	})

	factoryCalled := false
	sessionCtxCh := make(chan context.Context, 1)
	runner.sessionFactory = func(ctx context.Context, _ acp.SessionOptions) (acp.Session, error) {
		factoryCalled = true
		sessionCtxCh <- ctx
		return &fakeACPSession{
			sessionID: "session-live",
			replyText: "ok",
		}, nil
	}

	_, err := runTurnWithRunner(t, runner, TurnRequest{
		Conversation: ConversationState{Key: "conversation-ctx"},
		Message:      InboundMessage{Text: "hello", Kind: MessageKindText, Sender: "user"},
	})
	if err != nil {
		t.Fatalf("RunTurn failed: %v", err)
	}
	if !factoryCalled {
		t.Fatal("expected session factory to be called")
	}
	var sessionCtx context.Context
	select {
	case sessionCtx = <-sessionCtxCh:
	default:
		t.Fatal("expected session context to be captured")
	}

	select {
	case <-sessionCtx.Done():
		t.Fatal("expected session context to remain active after turn completion")
	default:
	}

	if err = runner.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	select {
	case <-sessionCtx.Done():
	default:
		t.Fatal("expected session context to be canceled on runner close")
	}
}

func TestACPRunnerIgnoresParentContextCancellationForSessionLifecycle(t *testing.T) {
	parentCtx, cancelParent := context.WithCancel(context.Background())
	runner := NewACPRunner(parentCtx, ACPRunnerOptions{
		Command: "agent",
		Args:    []string{"acp"},
	})

	sessionCtxCh := make(chan context.Context, 1)
	runner.sessionFactory = func(ctx context.Context, _ acp.SessionOptions) (acp.Session, error) {
		sessionCtxCh <- ctx
		return &fakeACPSession{
			sessionID: "session-live",
			replyText: "ok",
		}, nil
	}

	cancelParent()

	_, err := runTurnWithRunner(t, runner, TurnRequest{
		Conversation: ConversationState{Key: "conversation-parent-cancel"},
		Message:      InboundMessage{Text: "hello", Kind: MessageKindText, Sender: "user"},
	})
	if err != nil {
		t.Fatalf("RunTurn failed after parent context cancellation: %v", err)
	}

	var sessionCtx context.Context
	select {
	case sessionCtx = <-sessionCtxCh:
	default:
		t.Fatal("expected session context to be captured")
	}

	select {
	case <-sessionCtx.Done():
		t.Fatal("expected session context to remain active after parent context cancellation")
	default:
	}

	if err = runner.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	select {
	case <-sessionCtx.Done():
	default:
		t.Fatal("expected session context to be canceled on runner close")
	}
}

func TestACPRunnerSessionCloseClosesUnderlyingSession(t *testing.T) {
	runner := NewACPRunner(context.Background(), ACPRunnerOptions{
		Command: "agent",
		Args:    []string{"acp"},
	})

	fakeSession := &fakeACPSession{sessionID: "session-1"}
	session := &acpRunnerSession{
		runner:          runner,
		conversationKey: "conversation-1",
		sessionID:       "session-1",
		session:         fakeSession,
	}

	if err := session.Close(); err != nil {
		t.Fatalf("session Close failed: %v", err)
	}
	if !fakeSession.closed {
		t.Fatal("expected session Close to close the underlying session")
	}
}

func TestACPRunnerSessionStatusIncludesACPMetadata(t *testing.T) {
	t.Parallel()

	runner := NewACPRunner(context.Background(), ACPRunnerOptions{
		Command:    "agent",
		Args:       []string{"acp"},
		WorkingDir: "/workspace",
	})

	session := &acpRunnerSession{
		runner:          runner,
		conversationKey: "conversation-1",
		sessionID:       "session-1",
		session: &fakeACPSession{
			sessionID: "session-1",
			status: acp.SessionMetadata{
				SessionID: "session-1",
				Modes: acp.SessionModes{
					CurrentModeID: "workspace-write",
					AvailableModes: []acp.SessionMode{
						{ID: "workspace-write", Label: "Workspace Write"},
						{ID: "read-only", Label: "Read Only"},
					},
				},
				ConfigOptions: json.RawMessage(`{"model":"gpt-5.4","effort":"medium"}`),
			},
		},
	}

	status, err := session.Status(context.Background())
	if err != nil {
		t.Fatalf("Status failed: %v", err)
	}
	if status.Agent != "acp" {
		t.Fatalf("unexpected agent: %q", status.Agent)
	}
	if len(status.WorkingDirectories) != 1 || status.WorkingDirectories[0] != "/workspace" {
		t.Fatalf("unexpected working directories: %#v", status.WorkingDirectories)
	}
	if status.Modes.CurrentModeID != "workspace-write" {
		t.Fatalf("unexpected current mode: %q", status.Modes.CurrentModeID)
	}
	if len(status.Modes.AvailableModes) != 2 {
		t.Fatalf("unexpected available modes: %#v", status.Modes.AvailableModes)
	}
	if len(status.ConfigOptions) != 2 {
		t.Fatalf("unexpected config options: %#v", status.ConfigOptions)
	}
	seen := make(map[string]string, len(status.ConfigOptions))
	for _, option := range status.ConfigOptions {
		seen[option.Name] = option.CurrentValue
	}
	if seen["model"] != "gpt-5.4" {
		t.Fatalf("unexpected model config option: %#v", status.ConfigOptions)
	}
	if seen["effort"] != "medium" {
		t.Fatalf("unexpected effort config option: %#v", status.ConfigOptions)
	}
}

func TestACPRunnerSessionInterruptWaitsForActiveTurnCompletion(t *testing.T) {
	t.Parallel()

	runner := NewACPRunner(context.Background(), ACPRunnerOptions{
		Command: "agent",
		Args:    []string{"acp"},
	})

	started := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan struct{})
	interruptCalled := make(chan struct{}, 1)
	runner.sessionFactory = func(_ context.Context, _ acp.SessionOptions) (acp.Session, error) {
		return &fakeACPSession{
			sessionID: "session-live",
			runTurn: func(context.Context, []acp.ContentBlock) (acp.TurnResult, error) {
				close(started)
				<-release
				close(finished)
				return acp.TurnResult{}, errors.New("interrupted")
			},
			interrupt: func(context.Context) error {
				interruptCalled <- struct{}{}
				<-finished
				return nil
			},
		}, nil
	}
	rawSession, err := runner.StartSession(context.Background(), SessionOptions{ConversationKey: "conversation-1"})
	if err != nil {
		t.Fatalf("StartSession failed: %v", err)
	}
	session := mustCompatSession(t, rawSession)

	runErrCh := make(chan error, 1)
	go func() {
		_, err := runSessionTurn(context.Background(), session, TurnRequest{
			Conversation: ConversationState{Key: "conversation-1"},
			Message:      InboundMessage{Text: "hello", Kind: MessageKindText, Sender: "user"},
		})
		runErrCh <- err
	}()

	<-started

	interruptDone := make(chan error, 1)
	go func() {
		interruptDone <- interruptSession(context.Background(), session)
	}()

	select {
	case err := <-interruptDone:
		t.Fatalf("interrupt returned before turn finished: %v", err)
	case <-time.After(20 * time.Millisecond):
	}

	close(release)

	select {
	case err := <-interruptDone:
		if err != nil {
			t.Fatalf("interrupt failed: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("interrupt did not wait for turn completion")
	}

	select {
	case err := <-runErrCh:
		if err == nil || err.Error() != "run acp turn failed: interrupted" {
			t.Fatalf("unexpected run turn error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("run turn did not finish")
	}
}

func TestACPRunnerSessionInterruptDoesNotAllowSuccessfulTurnResult(t *testing.T) {
	t.Parallel()

	runner := NewACPRunner(context.Background(), ACPRunnerOptions{
		Command: "agent",
		Args:    []string{"acp"},
	})

	started := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan struct{})
	interruptCalled := make(chan struct{}, 1)
	runner.sessionFactory = func(_ context.Context, _ acp.SessionOptions) (acp.Session, error) {
		return &fakeACPSession{
			sessionID: "session-live",
			runTurn: func(context.Context, []acp.ContentBlock) (acp.TurnResult, error) {
				close(started)
				<-release
				close(finished)
				return acp.TurnResult{SessionID: "session-live", ReplyText: "partial reply"}, nil
			},
			interrupt: func(context.Context) error {
				interruptCalled <- struct{}{}
				<-finished
				return nil
			},
		}, nil
	}
	rawSession, err := runner.StartSession(context.Background(), SessionOptions{ConversationKey: "conversation-1"})
	if err != nil {
		t.Fatalf("StartSession failed: %v", err)
	}
	session := mustCompatSession(t, rawSession)

	runErrCh := make(chan error, 1)
	go func() {
		_, err := runSessionTurn(context.Background(), session, TurnRequest{
			Conversation: ConversationState{Key: "conversation-1"},
			Message:      InboundMessage{Text: "hello", Kind: MessageKindText, Sender: "user"},
		})
		runErrCh <- err
	}()

	<-started

	interruptDone := make(chan error, 1)
	go func() {
		interruptDone <- interruptSession(context.Background(), session)
	}()

	select {
	case <-interruptCalled:
	case <-time.After(time.Second):
		t.Fatal("expected interrupt to be called")
	}

	close(release)

	select {
	case err := <-interruptDone:
		if err != nil {
			t.Fatalf("interrupt failed: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("interrupt did not wait for turn completion")
	}

	select {
	case err := <-runErrCh:
		if err == nil || err.Error() != "run acp turn failed: context canceled" {
			t.Fatalf("unexpected run turn error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("run turn did not finish")
	}
}

func TestACPRunnerInterruptReturnsNilWithoutActiveTurn(t *testing.T) {
	t.Parallel()

	runner := NewACPRunner(context.Background(), ACPRunnerOptions{
		Command: "agent",
		Args:    []string{"acp"},
	})
	runner.sessionFactory = func(_ context.Context, _ acp.SessionOptions) (acp.Session, error) {
		return &fakeACPSession{
			sessionID: "session-idle",
			replyText: "ok",
		}, nil
	}

	err := interruptWithRunner(t, runner, ConversationState{Key: "conversation-idle"})
	if err != nil {
		t.Fatalf("expected nil interrupt on idle session, got %v", err)
	}
}

func TestACPRunnerSessionCloseWaitsForActiveTurnCompletion(t *testing.T) {
	t.Skip("runner close is a thin delegation layer; wait semantics are covered by agent/acp session tests")
}

func TestACPRunnerSessionScheduleTurnInterruptsPreviousTurn(t *testing.T) {
	t.Parallel()

	runner := NewACPRunner(context.Background(), ACPRunnerOptions{
		Command: "agent",
		Args:    []string{"acp"},
	})

	started := make(chan struct{})
	interruptRequested := make(chan struct{})
	runCount := 0
	runner.sessionFactory = func(_ context.Context, _ acp.SessionOptions) (acp.Session, error) {
		return &fakeACPSession{
			sessionID: "session-live",
			runTurn: func(_ context.Context, _ []acp.ContentBlock) (acp.TurnResult, error) {
				runCount++
				if runCount == 1 {
					close(started)
					<-interruptRequested
					return acp.TurnResult{}, context.Canceled
				}
				return acp.TurnResult{SessionID: "session-live", ReplyText: "second reply"}, nil
			},
			interrupt: func(context.Context) error {
				select {
				case <-interruptRequested:
				default:
					close(interruptRequested)
				}
				return nil
			},
		}, nil
	}
	rawSession, err := runner.StartSession(context.Background(), SessionOptions{ConversationKey: "conversation-busy"})
	if err != nil {
		t.Fatalf("StartSession failed: %v", err)
	}
	session := mustCompatSession(t, rawSession)

	runErrCh := make(chan error, 1)
	go func() {
		_, err := runSessionTurn(context.Background(), session, TurnRequest{
			Conversation: ConversationState{Key: "conversation-busy"},
			Message:      InboundMessage{Text: "hello", Kind: MessageKindText, Sender: "user"},
		})
		runErrCh <- err
	}()

	<-started

	result, err := runSessionTurn(context.Background(), session, TurnRequest{
		Conversation: ConversationState{Key: "conversation-busy"},
		Message:      InboundMessage{Text: "again", Kind: MessageKindText, Sender: "user"},
	})
	if err != nil {
		t.Fatalf("second RunTurn failed: %v", err)
	}
	if result.ReplyText != "second reply" {
		t.Fatalf("unexpected second reply: %q", result.ReplyText)
	}

	select {
	case err = <-runErrCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("unexpected first run error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("first run turn did not finish")
	}
}

func TestACPRunnerRunTurnDoesNotRepeatSystemPromptOnResume(t *testing.T) {
	runner := NewACPRunner(context.Background(), ACPRunnerOptions{
		Command:      "agent",
		Args:         []string{"acp"},
		SystemPrompt: "System rule",
	})

	fakeSession := &fakeACPSession{
		sessionID: "session-existing",
		replyText: "done",
	}
	runner.sessionFactory = func(_ context.Context, options acp.SessionOptions) (acp.Session, error) {
		if options.ResumeSessionID != "session-existing" {
			t.Fatalf("unexpected resume session: %q", options.ResumeSessionID)
		}
		return fakeSession, nil
	}

	_, err := runTurnWithRunner(t, runner, TurnRequest{
		Conversation: ConversationState{Key: "conversation-1", RunnerThreadID: "session-existing"},
		Message: InboundMessage{
			Text:   "hello",
			Kind:   MessageKindText,
			Sender: "user",
		},
	})
	if err != nil {
		t.Fatalf("RunTurn failed: %v", err)
	}

	if len(fakeSession.promptBlocks) != 1 {
		t.Fatalf("expected one prompt, got %d", len(fakeSession.promptBlocks))
	}
	text := textFromACPBlock(t, fakeSession.promptBlocks[0][0])
	if text != "Current message context:\n- time: unknown\n- sender: `user`\n\nhello" {
		t.Fatalf("unexpected resumed prompt: %q", text)
	}
}

func TestACPRunnerRunTurnDoesNotRepeatSystemPromptWithinSameSession(t *testing.T) {
	runner := NewACPRunner(context.Background(), ACPRunnerOptions{
		Command:      "agent",
		Args:         []string{"acp"},
		SystemPrompt: "System rule",
	})

	fakeSession := &fakeACPSession{
		sessionID: "session-new",
		replyText: "done",
	}
	runner.sessionFactory = func(_ context.Context, options acp.SessionOptions) (acp.Session, error) {
		if options.ResumeSessionID != "" {
			t.Fatalf("unexpected resume session: %q", options.ResumeSessionID)
		}
		return fakeSession, nil
	}

	rawSession, err := runner.StartSession(context.Background(), SessionOptions{ConversationKey: "conversation-1"})
	if err != nil {
		t.Fatalf("StartSession failed: %v", err)
	}
	session := mustCompatSession(t, rawSession)

	_, err = runSessionTurn(context.Background(), session, TurnRequest{
		Conversation: ConversationState{Key: "conversation-1"},
		Message: InboundMessage{
			Text:   "hello",
			Kind:   MessageKindText,
			Sender: "user",
		},
	})
	if err != nil {
		t.Fatalf("first RunTurn failed: %v", err)
	}

	_, err = runSessionTurn(context.Background(), session, TurnRequest{
		Conversation: ConversationState{Key: "conversation-1", RunnerThreadID: "session-new"},
		Message: InboundMessage{
			Text:   "continue",
			Kind:   MessageKindText,
			Sender: "user",
		},
	})
	if err != nil {
		t.Fatalf("second RunTurn failed: %v", err)
	}

	if len(fakeSession.promptBlocks) != 2 {
		t.Fatalf("expected two prompts, got %d", len(fakeSession.promptBlocks))
	}
	firstText := textFromACPBlock(t, fakeSession.promptBlocks[0][0])
	if !strings.Contains(firstText, "System rule") {
		t.Fatalf("expected first prompt to include system prompt, got %q", firstText)
	}
	secondText := textFromACPBlock(t, fakeSession.promptBlocks[1][0])
	if strings.Contains(secondText, "System rule") {
		t.Fatalf("expected second prompt to omit system prompt, got %q", secondText)
	}
}

func TestACPRunnerRunTurnUsesImageBlocksWhenAgentSupportsImages(t *testing.T) {
	dir := t.TempDir()
	imagePath := filepath.Join(dir, "current.png")
	if err := os.WriteFile(imagePath, png1x1Data(t), 0o600); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	runner := NewACPRunner(context.Background(), ACPRunnerOptions{
		Command: "agent",
		Args:    []string{"acp"},
	})
	fakeSession := &fakeACPSession{
		sessionID: "session-image",
		replyText: "done",
		caps: acp.AgentCapabilities{
			Prompt: acp.PromptCapabilities{Image: true},
		},
	}
	runner.sessionFactory = func(_ context.Context, options acp.SessionOptions) (acp.Session, error) {
		return fakeSession, nil
	}

	_, err := runTurnWithRunner(t, runner, TurnRequest{
		Conversation: ConversationState{Key: "conversation-image"},
		Message: InboundMessage{
			Kind:      MessageKindImage,
			Sender:    "user",
			ImagePath: imagePath,
		},
	})
	if err != nil {
		t.Fatalf("RunTurn failed: %v", err)
	}

	if len(fakeSession.promptBlocks) != 1 {
		t.Fatalf("expected one prompt, got %d", len(fakeSession.promptBlocks))
	}
	blocks := fakeSession.promptBlocks[0]
	if len(blocks) != 2 {
		t.Fatalf("expected text and image blocks, got %d", len(blocks))
	}
	if got := stringValueACPBlock(blocks[1]["type"]); got != "image" {
		t.Fatalf("unexpected image block type: %q", got)
	}
	if got := stringValueACPBlock(blocks[1]["mimeType"]); got != "image/png" {
		t.Fatalf("unexpected image mime type: %q", got)
	}
	if got := stringValueACPBlock(blocks[1]["uri"]); got != "file://"+filepath.ToSlash(imagePath) {
		t.Fatalf("unexpected image uri: %q", got)
	}
	if stringValueACPBlock(blocks[1]["data"]) == "" {
		t.Fatal("expected image block data")
	}
}

func TestBuildACPPromptBlocksUsesResourceLinksForFiles(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "report.pdf")
	content := []byte("%PDF-1.4 test")
	if err := os.WriteFile(filePath, content, 0o600); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	blocks, err := buildACPPromptBlocks(nil, InboundMessage{
		Kind:     MessageKindFile,
		Text:     "report.pdf",
		FilePath: filePath,
	}, acp.PromptCapabilities{})
	if err != nil {
		t.Fatalf("buildACPPromptBlocks failed: %v", err)
	}

	if len(blocks) != 2 {
		t.Fatalf("expected text and resource link blocks, got %d", len(blocks))
	}
	resource := blocks[1]
	if got := stringValueACPBlock(resource["type"]); got != "resource_link" {
		t.Fatalf("unexpected block type: %q", got)
	}
	if got := stringValueACPBlock(resource["uri"]); got != "file://"+filepath.ToSlash(filePath) {
		t.Fatalf("unexpected resource uri: %q", got)
	}
	if got := stringValueACPBlock(resource["name"]); got != "report.pdf" {
		t.Fatalf("unexpected resource name: %q", got)
	}
	if got := stringValueACPBlock(resource["mimeType"]); got != "application/pdf" {
		t.Fatalf("unexpected resource mime type: %q", got)
	}
	if got := int64ValueACPBlock(resource["size"]); got != int64(len(content)) {
		t.Fatalf("unexpected resource size: %d", got)
	}
}

func TestBuildACPPromptBlocksPlacesMessageBeforeSystemPrompt(t *testing.T) {
	blocks, err := buildACPPromptBlocks([]string{"System rule", "Second rule"}, InboundMessage{
		Kind:   MessageKindText,
		Text:   "hello",
		Sender: "user",
	}, acp.PromptCapabilities{})
	if err != nil {
		t.Fatalf("buildACPPromptBlocks failed: %v", err)
	}

	if len(blocks) != 1 {
		t.Fatalf("expected one merged text block, got %d", len(blocks))
	}
	if text := textFromACPBlock(t, blocks[0]); !strings.HasPrefix(text, "System rule\n\nSecond rule\n\n") || strings.Contains(text, "\n\nSystem rule\n\nSecond rule\n\nSystem rule") {
		t.Fatalf("unexpected merged prompt block: %q", text)
	}
}

func TestBuildACPPromptBlocksUsesEmbeddedResourceForTextFiles(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "notes.md")
	content := []byte("# Notes\nhello\n")
	if err := os.WriteFile(filePath, content, 0o600); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	blocks, err := buildACPPromptBlocks(nil, InboundMessage{
		Kind:     MessageKindFile,
		Text:     "notes.md",
		FilePath: filePath,
	}, acp.PromptCapabilities{EmbeddedContext: true})
	if err != nil {
		t.Fatalf("buildACPPromptBlocks failed: %v", err)
	}

	if len(blocks) != 2 {
		t.Fatalf("expected text and resource blocks, got %d", len(blocks))
	}
	resource := blocks[1]
	if got := stringValueACPBlock(resource["type"]); got != "resource" {
		t.Fatalf("unexpected block type: %q", got)
	}
	payload, ok := resource["resource"].(map[string]any)
	if !ok {
		t.Fatalf("unexpected resource payload type: %T", resource["resource"])
	}
	if got := stringValueACPBlock(payload["uri"]); got != "file://"+filepath.ToSlash(filePath) {
		t.Fatalf("unexpected resource uri: %q", got)
	}
	if got := stringValueACPBlock(payload["mimeType"]); !strings.HasPrefix(got, "text/") {
		t.Fatalf("unexpected resource mime type: %q", got)
	}
	if got := stringValueACPBlock(payload["text"]); got != string(content) {
		t.Fatalf("unexpected embedded text: %q", got)
	}
}

func TestACPRunnerStartSessionFailsWhenAgentDoesNotAdvertiseHTTPMCP(t *testing.T) {
	runner := NewACPRunner(context.Background(), ACPRunnerOptions{
		Command: "agent",
		Args:    []string{"acp"},
		Tools:   []Tool{uppercaseTool{}},
	})

	fakeSession := &fakeACPSession{
		sessionID: "session-no-http",
		replyText: "done",
	}
	runner.sessionFactory = func(_ context.Context, options acp.SessionOptions) (acp.Session, error) {
		if len(options.MCPServers) != 1 {
			t.Fatalf("expected MCP server to be requested, got %d", len(options.MCPServers))
		}
		return fakeSession, nil
	}

	_, err := runner.StartSession(context.Background(), SessionOptions{
		ConversationKey: "conversation-tools",
	})
	if err == nil {
		t.Fatal("expected StartSession to fail")
	}
	if !strings.Contains(err.Error(), "HTTP MCP support") {
		t.Fatalf("unexpected error: %v", err)
	}
	if !fakeSession.closed {
		t.Fatal("expected unsupported session to be closed")
	}
}

func textFromACPBlock(t *testing.T, block acp.ContentBlock) string {
	t.Helper()
	if got := stringValueACPBlock(block["type"]); got != "text" {
		t.Fatalf("unexpected block type: %q", got)
	}
	return stringValueACPBlock(block["text"])
}

func stringValueACPBlock(value any) string {
	text, _ := value.(string)
	return text
}

func int64ValueACPBlock(value any) int64 {
	switch typed := value.(type) {
	case int64:
		return typed
	case int:
		return int64(typed)
	case float64:
		return int64(typed)
	default:
		return 0
	}
}

func png1x1Data(t *testing.T) []byte {
	t.Helper()
	data, err := json.Marshal(nil)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}
	_ = data
	return []byte{
		0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a,
		0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
		0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
		0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4,
		0x89, 0x00, 0x00, 0x00, 0x0d, 0x49, 0x44, 0x41,
		0x54, 0x78, 0x9c, 0x63, 0xf8, 0xcf, 0xc0, 0x00,
		0x00, 0x03, 0x01, 0x01, 0x00, 0xc9, 0xfe, 0x92,
		0xef, 0x00, 0x00, 0x00, 0x00, 0x49, 0x45, 0x4e,
		0x44, 0xae, 0x42, 0x60, 0x82,
	}
}
