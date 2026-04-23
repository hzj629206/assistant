package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hzj629206/assistant/agent/acp"
)

type fakeACPSession struct {
	sessionID    string
	replyText    string
	rawResult    json.RawMessage
	caps         acp.AgentCapabilities
	promptBlocks [][]acp.ContentBlock
	closed       bool
}

func (s *fakeACPSession) RunTurn(_ context.Context, blocks []acp.ContentBlock) (acp.TurnResult, error) {
	copied := make([]acp.ContentBlock, len(blocks))
	copy(copied, blocks)
	s.promptBlocks = append(s.promptBlocks, copied)
	return acp.TurnResult{
		SessionID: s.sessionID,
		ReplyText: s.replyText,
		RawResult: append(json.RawMessage(nil), s.rawResult...),
	}, nil
}

func (s *fakeACPSession) CurrentSessionID() string {
	return s.sessionID
}

func (s *fakeACPSession) Capabilities() acp.AgentCapabilities {
	return s.caps
}

func (s *fakeACPSession) Close() error {
	s.closed = true
	return nil
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

	result, err := runner.RunTurn(context.Background(), TurnRequest{
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

func TestACPRunnerReusesSessionForSameConversation(t *testing.T) {
	runner := NewACPRunner(context.Background(), ACPRunnerOptions{
		Command: "agent",
		Args:    []string{"acp"},
	})
	runner.sessionIdleTimeout = time.Minute

	createCount := 0
	firstSession := &fakeACPSession{
		sessionID: "session-live",
		replyText: "ok",
		caps:      acp.AgentCapabilities{MCP: acp.MCPCapabilities{HTTP: true}},
	}
	runner.sessionFactory = func(_ context.Context, options acp.SessionOptions) (acp.Session, error) {
		createCount++
		if createCount > 1 {
			t.Fatalf("unexpected session recreation with options: %#v", options)
		}
		return firstSession, nil
	}

	first, err := runner.RunTurn(context.Background(), TurnRequest{
		Conversation: ConversationState{Key: "conversation-1"},
		Message:      InboundMessage{Text: "hello", Kind: MessageKindText, Sender: "user"},
	})
	if err != nil {
		t.Fatalf("first RunTurn failed: %v", err)
	}
	second, err := runner.RunTurn(context.Background(), TurnRequest{
		Conversation: ConversationState{Key: "conversation-1", RunnerThreadID: first.RunnerThreadID},
		Message:      InboundMessage{Text: "again", Kind: MessageKindText, Sender: "user"},
	})
	if err != nil {
		t.Fatalf("second RunTurn failed: %v", err)
	}

	if createCount != 1 {
		t.Fatalf("unexpected session create count: %d", createCount)
	}
	if second.RunnerThreadID != "session-live" {
		t.Fatalf("unexpected reused session id: %q", second.RunnerThreadID)
	}
	if firstSession.closed {
		t.Fatal("expected reused session to remain open")
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

	_, err := runner.RunTurn(context.Background(), TurnRequest{
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

	_, err := runner.RunTurn(context.Background(), TurnRequest{
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

func TestACPRunnerCloseClosesManagedSessions(t *testing.T) {
	runner := NewACPRunner(context.Background(), ACPRunnerOptions{
		Command: "agent",
		Args:    []string{"acp"},
	})

	firstSession := &fakeACPSession{sessionID: "session-1"}
	secondSession := &fakeACPSession{sessionID: "session-2"}
	runner.sessions["conversation-1"] = &acpRunnerSession{session: firstSession}
	runner.sessions["conversation-2"] = &acpRunnerSession{session: secondSession}

	if err := runner.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}
	if !firstSession.closed || !secondSession.closed {
		t.Fatal("expected Close to close all managed sessions")
	}
	if len(runner.sessions) != 0 {
		t.Fatalf("expected sessions to be cleared, got %d", len(runner.sessions))
	}
	if err := runner.Close(); err != nil {
		t.Fatalf("second Close failed: %v", err)
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

	_, err := runner.RunTurn(context.Background(), TurnRequest{
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

	_, err := runner.RunTurn(context.Background(), TurnRequest{
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

func TestACPRunnerRunTurnFailsWhenAgentDoesNotAdvertiseHTTPMCP(t *testing.T) {
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

	_, err := runner.RunTurn(context.Background(), TurnRequest{
		Conversation: ConversationState{Key: "conversation-tools"},
		Message: InboundMessage{
			Text:   "hello",
			Kind:   MessageKindText,
			Sender: "user",
		},
	})
	if err == nil {
		t.Fatal("expected RunTurn to fail")
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
