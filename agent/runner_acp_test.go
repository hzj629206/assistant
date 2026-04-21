package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

type fakeACPSession struct {
	sessionID    string
	replyText    string
	caps         acpAgentCapabilities
	promptBlocks [][]acpContentBlock
	closed       bool
}

func (s *fakeACPSession) RunPrompt(_ context.Context, prompt []acpContentBlock) (string, error) {
	copied := make([]acpContentBlock, len(prompt))
	copy(copied, prompt)
	s.promptBlocks = append(s.promptBlocks, copied)
	return s.replyText, nil
}

func (s *fakeACPSession) CurrentSessionID() string {
	return s.sessionID
}

func (s *fakeACPSession) AgentCapabilities() acpAgentCapabilities {
	return s.caps
}

func (s *fakeACPSession) Close() error {
	s.closed = true
	return nil
}

func TestACPRunnerRunTurnRegistersHTTPToolServerForGlobalTools(t *testing.T) {
	runner := NewACPRunner(ACPRunnerOptions{
		Command:      "agent",
		Args:         []string{"acp"},
		SystemPrompt: "System rule",
		Tools:        []Tool{uppercaseTool{}},
	})

	fakeSession := &fakeACPSession{
		sessionID: "session-new",
		replyText: "done",
		caps:      acpAgentCapabilities{MCP: acpMCPCapabilities{HTTP: true}},
	}
	var captured acpSessionOptions
	runner.sessionFactory = func(_ context.Context, options acpSessionOptions) (acpSession, error) {
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
	if len(fakeSession.promptBlocks[0]) < 2 {
		t.Fatalf("expected separate system and user text blocks, got %d", len(fakeSession.promptBlocks[0]))
	}
	if text := textFromACPBlock(t, fakeSession.promptBlocks[0][0]); text != "System rule" {
		t.Fatalf("unexpected system prompt block: %q", text)
	}
	if text := textFromACPBlock(t, fakeSession.promptBlocks[0][1]); text == "System rule" || text == "hello" {
		t.Fatalf("expected second block to be the rendered user prompt, got %q", text)
	}
	if fakeSession.closed {
		t.Fatal("expected session to remain open after release")
	}
}

func TestACPRunnerReusesSessionForSameConversation(t *testing.T) {
	runner := NewACPRunner(ACPRunnerOptions{
		Command: "agent",
		Args:    []string{"acp"},
	})
	runner.sessionIdleTimeout = time.Minute

	createCount := 0
	firstSession := &fakeACPSession{
		sessionID: "session-live",
		replyText: "ok",
		caps:      acpAgentCapabilities{MCP: acpMCPCapabilities{HTTP: true}},
	}
	runner.sessionFactory = func(_ context.Context, options acpSessionOptions) (acpSession, error) {
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

func TestACPRunnerCloseClosesManagedSessions(t *testing.T) {
	runner := NewACPRunner(ACPRunnerOptions{
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

func TestIgnoreExpectedACPExitReturnsNilForSIGTERM(t *testing.T) {
	t.Parallel()

	cmd := exec.CommandContext(context.Background(), "sh", "-c", "kill -TERM $$")
	err := cmd.Run()
	if err == nil {
		t.Fatal("Run returned nil, want exit error")
	}

	if got := ignoreExpectedACPExit(err); got != nil {
		t.Fatalf("ignoreExpectedACPExit returned %v, want nil", got)
	}
}

func TestIgnoreExpectedACPExitPreservesNonSignalExit(t *testing.T) {
	t.Parallel()

	cmd := exec.CommandContext(context.Background(), "sh", "-c", "exit 7")
	err := cmd.Run()
	if err == nil {
		t.Fatal("Run returned nil, want exit error")
	}

	got := ignoreExpectedACPExit(err)
	if got == nil {
		t.Fatal("ignoreExpectedACPExit returned nil, want error")
	}

	var exitErr *exec.ExitError
	if !errors.As(got, &exitErr) {
		t.Fatalf("ignoreExpectedACPExit returned %T, want *exec.ExitError", got)
	}
	if status, ok := exitErr.Sys().(syscall.WaitStatus); !ok || status.ExitStatus() != 7 {
		t.Fatalf("unexpected wait status: %#v", exitErr.Sys())
	}
}

func TestACPRunnerRunTurnDoesNotRepeatSystemPromptOnResume(t *testing.T) {
	runner := NewACPRunner(ACPRunnerOptions{
		Command:      "agent",
		Args:         []string{"acp"},
		SystemPrompt: "System rule",
	})

	fakeSession := &fakeACPSession{
		sessionID: "session-existing",
		replyText: "done",
	}
	runner.sessionFactory = func(_ context.Context, options acpSessionOptions) (acpSession, error) {
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

func TestACPProcessSessionRunPromptSendsSessionCancelOnContextCancel(t *testing.T) {
	var output bytes.Buffer
	session := &acpProcessSession{
		transport: newACPRPCTransport(bytes.NewReader(nil), &output, nil, nil),
		sessionID: "session-cancel",
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := session.RunPrompt(ctx, []acpContentBlock{{"type": "text", "text": "hello"}})
		done <- err
	}()

	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected RunPrompt to fail after cancellation")
		}
	case <-time.After(time.Second):
		t.Fatal("RunPrompt did not return after cancellation")
	}

	deadline := time.Now().Add(time.Second)
	for {
		written := output.String()
		if strings.Contains(written, "\"method\":\"session/prompt\"") && strings.Contains(written, "\"method\":\"session/cancel\"") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected session/prompt and session/cancel writes, got %q", written)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestACPRunnerRunTurnUsesImageBlocksWhenAgentSupportsImages(t *testing.T) {
	dir := t.TempDir()
	imagePath := filepath.Join(dir, "current.png")
	if err := os.WriteFile(imagePath, png1x1Data(t), 0o600); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	runner := NewACPRunner(ACPRunnerOptions{
		Command: "agent",
		Args:    []string{"acp"},
	})
	fakeSession := &fakeACPSession{
		sessionID: "session-image",
		replyText: "done",
		caps: acpAgentCapabilities{
			Prompt: acpPromptCapabilities{Image: true},
		},
	}
	runner.sessionFactory = func(_ context.Context, options acpSessionOptions) (acpSession, error) {
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
	}, acpPromptCapabilities{})
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
	}, acpPromptCapabilities{EmbeddedContext: true})
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
	runner := NewACPRunner(ACPRunnerOptions{
		Command: "agent",
		Args:    []string{"acp"},
		Tools:   []Tool{uppercaseTool{}},
	})

	fakeSession := &fakeACPSession{
		sessionID: "session-no-http",
		replyText: "done",
	}
	runner.sessionFactory = func(_ context.Context, options acpSessionOptions) (acpSession, error) {
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

func TestSupportsACPAuthMethod(t *testing.T) {
	authMethods := []struct {
		ID string `json:"id"`
	}{
		{ID: "agent-login"},
		{ID: "device-code"},
	}

	if !supportsACPAuthMethod(authMethods, "device-code") {
		t.Fatal("expected configured auth method to be supported")
	}
	if supportsACPAuthMethod(authMethods, "missing-method") {
		t.Fatal("expected unknown auth method to be unsupported")
	}
	if !supportsACPAuthMethod(authMethods, "") {
		t.Fatal("expected empty auth method to be accepted")
	}
}

func TestACPToolHTTPServerServesJSONResponse(t *testing.T) {
	token := "token-1"
	server, err := newACPToolHTTPServer(context.Background(), acpToolHTTPServerOptions{
		IsAuthorized: func(candidate string) bool { return candidate == token },
		ResolveTurnRequest: func(candidate string) (TurnRequest, bool) {
			if candidate != token {
				return TurnRequest{}, false
			}
			return TurnRequest{Conversation: ConversationState{Key: "conversation-1"}}, true
		},
		Tools: func() []Tool { return []Tool{uppercaseTool{}} },
	})
	if err != nil {
		t.Fatalf("newACPToolHTTPServer failed: %v", err)
	}
	defer func() {
		if closeErr := server.Close(); closeErr != nil {
			t.Fatalf("Close failed: %v", closeErr)
		}
	}()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, server.url, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"test","version":"1.0.0"}}}`))
	if err != nil {
		t.Fatalf("NewRequest failed: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do failed: %v", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if contentType := resp.Header.Get("Content-Type"); !strings.Contains(contentType, "application/json") {
		t.Fatalf("unexpected content type: %q", contentType)
	}
	var payload struct {
		Result struct {
			ServerInfo struct {
				Name string `json:"name"`
			} `json:"serverInfo"`
		} `json:"result"`
	}
	if err = json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("Decode failed: %v", err)
	}
	if payload.Result.ServerInfo.Name != defaultACPToolServerName {
		t.Fatalf("unexpected server name: %q", payload.Result.ServerInfo.Name)
	}
}

func TestACPToolHTTPServerRejectsMissingAuthorization(t *testing.T) {
	server, err := newACPToolHTTPServer(context.Background(), acpToolHTTPServerOptions{
		IsAuthorized:       func(string) bool { return false },
		ResolveTurnRequest: func(string) (TurnRequest, bool) { return TurnRequest{}, false },
		Tools:              func() []Tool { return []Tool{uppercaseTool{}} },
	})
	if err != nil {
		t.Fatalf("newACPToolHTTPServer failed: %v", err)
	}
	defer func() {
		if closeErr := server.Close(); closeErr != nil {
			t.Fatalf("Close failed: %v", closeErr)
		}
	}()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, server.url, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"test","version":"1.0.0"}}}`))
	if err != nil {
		t.Fatalf("NewRequest failed: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do failed: %v", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unexpected status: %d", resp.StatusCode)
	}
}

func textFromACPBlock(t *testing.T, block acpContentBlock) string {
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
