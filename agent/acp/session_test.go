package acp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestProcessExitLogDetailsIncludesSignal(t *testing.T) {
	t.Parallel()

	err := exec.CommandContext(context.Background(), "sh", "-c", "kill -TERM $$").Run()
	if err == nil {
		t.Fatal("Run returned nil, want exit error")
	}

	got := processExitLogDetails(err)
	if got != "signal=terminated" {
		t.Fatalf("unexpected signal details: %q", got)
	}
}

func TestProcessExitLogDetailsIncludesExitCode(t *testing.T) {
	t.Parallel()

	err := exec.CommandContext(context.Background(), "sh", "-c", "exit 7").Run()
	if err == nil {
		t.Fatal("Run returned nil, want exit error")
	}

	got := processExitLogDetails(err)
	if got != "exit_code=7" {
		t.Fatalf("unexpected exit code details: %q", got)
	}
}

func TestProcessSessionRunTurnSendsSessionCancelOnContextCancel(t *testing.T) {
	var output bytes.Buffer
	session := &processSession{
		transport: newRPCTransport(bytes.NewReader(nil), &output, nil, nil),
		sessionID: "session-cancel",
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := session.RunTurn(ctx, []ContentBlock{{"type": "text", "text": "hello"}})
		done <- err
	}()

	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected RunTurn to fail after cancellation")
		}
	case <-time.After(time.Second):
		t.Fatal("RunTurn did not return after cancellation")
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

func TestProcessSessionInterruptReturnsNilWithoutActiveTurn(t *testing.T) {
	session := &processSession{}

	err := session.Interrupt(context.Background())
	if err != nil {
		t.Fatalf("expected nil interrupt without active turn, got %v", err)
	}
}

func TestProcessSessionInterruptWaitsForTurnCompletion(t *testing.T) {
	var output bytes.Buffer
	session := &processSession{
		transport: newRPCTransport(bytes.NewReader(nil), &output, nil, nil),
		sessionID: "session-interrupt",
	}

	activeTurn, err := session.startActiveTurn("session-interrupt")
	if err != nil {
		t.Fatalf("startActiveTurn failed: %v", err)
	}

	interruptDone := make(chan error, 1)
	go func() {
		interruptDone <- session.Interrupt(context.Background())
	}()

	select {
	case err = <-interruptDone:
		t.Fatalf("interrupt returned before turn completion: %v", err)
	case <-time.After(20 * time.Millisecond):
	}

	if strings.Contains(output.String(), "\"method\":\"session/cancel\"") {
		t.Fatalf("did not expect session/cancel write from Interrupt, got %q", output.String())
	}

	session.finishActiveTurn(activeTurn)

	select {
	case err = <-interruptDone:
		if err != nil {
			t.Fatalf("interrupt failed: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("interrupt did not wait for turn completion")
	}
}

func TestProcessSessionRunTurnReturnsCanceledWhenInterruptedPromptStillCompletes(t *testing.T) {
	var output bytes.Buffer
	reader, writer := io.Pipe()
	session := &processSession{
		transport: newRPCTransport(reader, &output, nil, nil),
		sessionID: "session-interrupted-complete",
	}
	session.transport.onNotify = session.handleNotification

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go session.transport.readLoop(ctx)

	runErrCh := make(chan error, 1)
	go func() {
		_, err := session.RunTurn(context.Background(), []ContentBlock{{"type": "text", "text": "hello"}})
		runErrCh <- err
	}()

	deadline := time.Now().Add(time.Second)
	for {
		if strings.Contains(output.String(), "\"method\":\"session/prompt\"") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected session/prompt write, got %q", output.String())
		}
		time.Sleep(10 * time.Millisecond)
	}

	interruptDone := make(chan error, 1)
	go func() {
		interruptDone <- session.Interrupt(context.Background())
	}()

	for {
		if strings.Contains(output.String(), "\"method\":\"session/cancel\"") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected session/cancel write, got %q", output.String())
		}
		time.Sleep(10 * time.Millisecond)
	}

	_, _ = writer.Write([]byte("{\"id\":1,\"result\":{\"stopReason\":\"end_turn\"}}\n"))
	_ = writer.Close()

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
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("RunTurn did not return")
	}
}

func TestIgnoreExpectedExitReturnsNilForSIGTERM(t *testing.T) {
	t.Parallel()

	cmd := exec.CommandContext(context.Background(), "sh", "-c", "kill -TERM $$")
	err := cmd.Run()
	if err == nil {
		t.Fatal("Run returned nil, want exit error")
	}

	if got := ignoreExpectedExit(err); got != nil {
		t.Fatalf("ignoreExpectedExit returned %v, want nil", got)
	}
}

func TestIgnoreExpectedExitPreservesNonSignalExit(t *testing.T) {
	t.Parallel()

	cmd := exec.CommandContext(context.Background(), "sh", "-c", "exit 7")
	err := cmd.Run()
	if err == nil {
		t.Fatal("Run returned nil, want exit error")
	}

	got := ignoreExpectedExit(err)
	if got == nil {
		t.Fatal("ignoreExpectedExit returned nil, want error")
	}

	var exitErr *exec.ExitError
	if !errors.As(got, &exitErr) {
		t.Fatalf("ignoreExpectedExit returned %T, want *exec.ExitError", got)
	}
	if status, ok := exitErr.Sys().(syscall.WaitStatus); !ok || status.ExitStatus() != 7 {
		t.Fatalf("unexpected wait status: %#v", exitErr.Sys())
	}
}

func TestIgnoreExpectedSignalProcessGroupError(t *testing.T) {
	t.Parallel()

	if !ignoreExpectedSignalProcessGroupError(nil) {
		t.Fatal("nil error should be ignored")
	}
	if !ignoreExpectedSignalProcessGroupError(syscall.EPERM) {
		t.Fatal("EPERM should be ignored during shutdown")
	}
	if !ignoreExpectedSignalProcessGroupError(syscall.ESRCH) {
		t.Fatal("ESRCH should be ignored during shutdown")
	}
	if ignoreExpectedSignalProcessGroupError(syscall.EINVAL) {
		t.Fatal("EINVAL should not be ignored")
	}
}

func TestSupportsAuthMethod(t *testing.T) {
	authMethods := []struct {
		ID string `json:"id"`
	}{
		{ID: "agent-login"},
		{ID: "device-code"},
	}

	if !supportsAuthMethod(authMethods, "device-code") {
		t.Fatal("expected configured auth method to be supported")
	}
	if supportsAuthMethod(authMethods, "missing-method") {
		t.Fatal("expected unknown auth method to be unsupported")
	}
	if !supportsAuthMethod(authMethods, "") {
		t.Fatal("expected empty auth method to be accepted")
	}
}

func TestStartSessionTimesOutWhenAgentDoesNotRespond(t *testing.T) {
	originalTimeout := processHandshakeTimeout
	processHandshakeTimeout = 100 * time.Millisecond
	t.Cleanup(func() {
		processHandshakeTimeout = originalTimeout
	})

	session, err := StartSession(t.Context(), SessionOptions{
		Command: "sh",
		Args:    []string{"-c", "cat >/dev/null"},
	})
	if err == nil {
		if session != nil {
			_ = session.Close()
		}
		t.Fatal("StartSession returned nil error, want timeout")
	}
	if !strings.Contains(err.Error(), "timed out after") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestProcessSessionHandleRequestUsesCustomPermissionHandler(t *testing.T) {
	var output bytes.Buffer
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	session := &processSession{
		lifecycleCtx: ctx,
		transport:    newRPCTransport(bytes.NewReader(nil), &output, nil, nil),
		permission: func(handlerCtx context.Context, request PermissionRequest) (PermissionDecision, error) {
			if handlerCtx != ctx {
				t.Fatal("expected permission handler to receive session lifecycle context")
			}
			if request.Method != "session/request_permission" {
				t.Fatalf("unexpected method: %q", request.Method)
			}
			if len(request.Options) != 2 {
				t.Fatalf("unexpected option count: %d", len(request.Options))
			}
			return PermissionDecision{Allow: true, OptionID: "allow-id"}, nil
		},
	}

	params := json.RawMessage(`{"options":[{"optionId":"allow-id","name":"Allow","kind":"allow_once"},{"optionId":"deny-id","name":"Deny","kind":"deny"}]}`)
	session.handleRequest("session/request_permission", json.RawMessage(`1`), params)

	written := output.String()
	if !strings.Contains(written, `"optionId":"allow-id"`) {
		t.Fatalf("expected selected option in response, got %q", written)
	}
}

func TestProcessSessionInterruptCancelsPendingPermissionRequests(t *testing.T) {
	var output bytes.Buffer
	handlerCtxDone := make(chan struct{})
	session := &processSession{
		lifecycleCtx: context.Background(),
		transport:    newRPCTransport(bytes.NewReader(nil), &output, nil, nil),
		permission: func(handlerCtx context.Context, request PermissionRequest) (PermissionDecision, error) {
			<-handlerCtx.Done()
			close(handlerCtxDone)
			return PermissionDecision{}, handlerCtx.Err()
		},
	}

	activeTurn, err := session.startActiveTurn("session-permission-cancel")
	if err != nil {
		t.Fatalf("startActiveTurn failed: %v", err)
	}
	defer session.finishActiveTurn(activeTurn)

	requestDone := make(chan struct{})
	go func() {
		defer close(requestDone)
		params := json.RawMessage(`{"options":[{"optionId":"allow-id","name":"Allow","kind":"allow_once"},{"optionId":"deny-id","name":"Deny","kind":"deny"}]}`)
		session.handleRequest("session/request_permission", json.RawMessage(`7`), params)
	}()

	deadline := time.Now().Add(time.Second)
	for {
		session.permissionMu.Lock()
		pendingCount := len(session.pendingPermission)
		session.permissionMu.Unlock()
		if pendingCount == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected pending permission request, got output %q", output.String())
		}
		time.Sleep(10 * time.Millisecond)
	}

	session.signalTurnInterrupt(activeTurn)
	session.watchTurnInterrupt(context.Background(), activeTurn)

	select {
	case <-handlerCtxDone:
	case <-time.After(time.Second):
		t.Fatal("permission handler context was not canceled")
	}

	select {
	case <-requestDone:
	case <-time.After(time.Second):
		t.Fatal("permission request did not complete after interrupt")
	}

	written := output.String()
	if !strings.Contains(written, `"method":"session/cancel"`) {
		t.Fatalf("expected session/cancel write, got %q", written)
	}
	if !strings.Contains(written, `"id":7`) || !strings.Contains(written, `"outcome":"cancelled"`) {
		t.Fatalf("expected cancelled permission response, got %q", written)
	}
}

func TestProcessSessionHandleNotificationCallsObserver(t *testing.T) {
	var got SessionUpdate
	session := &processSession{
		observer: Observer{
			OnSessionUpdate: func(update SessionUpdate) {
				got = update
			},
		},
	}

	session.handleNotification("session/update", json.RawMessage(`{
		"sessionId":"session-1",
		"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"hello"}}
	}`))

	if got.SessionID != "session-1" {
		t.Fatalf("unexpected session id: %q", got.SessionID)
	}
	if got.SessionUpdate != "agent_message_chunk" {
		t.Fatalf("unexpected update type: %q", got.SessionUpdate)
	}
	if got.Text != "hello" {
		t.Fatalf("unexpected update text: %q", got.Text)
	}
}

func TestProcessSessionHandleNotificationPreservesWhitespaceChunks(t *testing.T) {
	session := &processSession{
		promptBuilder: &strings.Builder{},
	}

	updates := []string{
		`{"sessionId":"session-1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"*   first item"}}}`,
		`{"sessionId":"session-1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"\n"}}}`,
		`{"sessionId":"session-1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"*   second item"}}}`,
		`{"sessionId":"session-1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"\n  "}}}`,
		`{"sessionId":"session-1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"*   nested item"}}}`,
	}

	for _, update := range updates {
		session.handleNotification("session/update", json.RawMessage(update))
	}

	got := session.promptBuilder.String()
	want := "*   first item\n*   second item\n  *   nested item"
	if got != want {
		t.Fatalf("unexpected accumulated prompt text: %q", got)
	}
}

func TestProcessSessionRunTurnPreservesLeadingAndTrailingWhitespace(t *testing.T) {
	var output bytes.Buffer
	reader, writer := io.Pipe()
	session := &processSession{
		transport: newRPCTransport(reader, &output, nil, nil),
		sessionID: "session-whitespace",
	}
	session.transport.onNotify = session.handleNotification

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go session.transport.readLoop(ctx)
	go func() {
		time.Sleep(10 * time.Millisecond)
		_, _ = writer.Write([]byte("{\"method\":\"session/update\",\"params\":{\"sessionId\":\"session-whitespace\",\"update\":{\"sessionUpdate\":\"agent_message_chunk\",\"content\":{\"type\":\"text\",\"text\":\"\\n  reply with preserved whitespace\"}}}}\n"))
		_, _ = writer.Write([]byte("{\"method\":\"session/update\",\"params\":{\"sessionId\":\"session-whitespace\",\"update\":{\"sessionUpdate\":\"agent_message_chunk\",\"content\":{\"type\":\"text\",\"text\":\"  \\n\"}}}}\n"))
		_, _ = writer.Write([]byte("{\"id\":1,\"result\":{\"status\":\"ok\"}}\n"))
		_ = writer.Close()
	}()

	result, err := session.RunTurn(context.Background(), []ContentBlock{{"type": "text", "text": "hello"}})
	if err != nil {
		t.Fatalf("RunTurn failed: %v", err)
	}
	if result.ReplyText != "\n  reply with preserved whitespace  \n" {
		t.Fatalf("unexpected reply text: %q", result.ReplyText)
	}
}

func TestProcessSessionRunTurnReturnsStructuredResult(t *testing.T) {
	var output bytes.Buffer
	reader, writer := io.Pipe()
	session := &processSession{
		transport: newRPCTransport(reader, &output, nil, nil),
		sessionID: "session-result",
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go session.transport.readLoop(ctx)
	go func() {
		time.Sleep(10 * time.Millisecond)
		_, _ = writer.Write([]byte("{\"id\":1,\"result\":{\"status\":\"ok\"}}\n"))
		_ = writer.Close()
	}()

	result, err := session.RunTurn(context.Background(), []ContentBlock{{"type": "text", "text": "hello"}})
	if err != nil {
		t.Fatalf("RunTurn failed: %v", err)
	}
	if result.SessionID != "session-result" {
		t.Fatalf("unexpected session id: %q", result.SessionID)
	}
	if result.ReplyText != "" {
		t.Fatalf("unexpected reply text: %q", result.ReplyText)
	}
	if string(result.RawResult) != `{"status":"ok"}` {
		t.Fatalf("unexpected raw result: %s", string(result.RawResult))
	}
}
