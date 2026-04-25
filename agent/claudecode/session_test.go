package claudecode

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestPersistentProcessSessionMarkInitialized(t *testing.T) {
	t.Parallel()

	session := &persistentProcessSession{}
	if session.isInitialized() {
		t.Fatal("did not expect fresh session to be initialized")
	}

	session.markInitialized()

	if !session.isInitialized() {
		t.Fatal("expected session to be initialized after markInitialized")
	}
}

func TestPersistentProcessSessionHandleSystemMessageUpdatesSessionID(t *testing.T) {
	t.Parallel()

	session := &persistentProcessSession{
		cmd: fakeExecCmd(1234),
	}

	envelope := map[string]any{
		"type":       "system",
		"session_id": "session-from-system",
	}
	line, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	session.handleStreamMessage(envelope, string(line))

	if got := session.SessionID(); got != "session-from-system" {
		t.Fatalf("unexpected session id: %s", got)
	}
}

func TestPersistentProcessSessionHandleResultMessageDeliversResult(t *testing.T) {
	t.Parallel()

	turn := &turnState{
		resultCh: make(chan *ClaudeResult, 1),
	}
	session := &persistentProcessSession{
		cmd:         fakeExecCmd(1234),
		currentTurn: turn,
	}

	envelope := map[string]any{
		"type":       "result",
		"session_id": "session-result",
		"result":     "assistant reply",
		"usage": map[string]any{
			"input_tokens":  float64(12),
			"output_tokens": float64(34),
		},
	}
	line, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	session.handleStreamMessage(envelope, string(line))

	select {
	case result := <-turn.resultCh:
		if result.Result != "assistant reply" {
			t.Fatalf("unexpected reply: %s", result.Result)
		}
		if result.SessionID != "session-result" {
			t.Fatalf("unexpected result session id: %s", result.SessionID)
		}
	case <-time.After(time.Second):
		t.Fatal("expected result message")
	}

	if got := session.SessionID(); got != "session-result" {
		t.Fatalf("unexpected session id: %s", got)
	}
}

func TestPersistentProcessSessionHandleResultMessageDeliversStructuredOutput(t *testing.T) {
	t.Parallel()

	turn := &turnState{
		resultCh: make(chan *ClaudeResult, 1),
	}
	session := &persistentProcessSession{
		cmd:         fakeExecCmd(1234),
		currentTurn: turn,
	}

	envelope := map[string]any{
		"type":              "result",
		"session_id":        "session-structured",
		"structured_output": map[string]any{"answer": "ok", "count": float64(2)},
	}
	line, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	session.handleStreamMessage(envelope, string(line))

	select {
	case result := <-turn.resultCh:
		output, ok := result.StructuredOutput.(map[string]any)
		if !ok {
			t.Fatalf("unexpected structured output type: %T", result.StructuredOutput)
		}
		if output["answer"] != "ok" {
			t.Fatalf("unexpected structured output answer: %v", output["answer"])
		}
		if output["count"] != float64(2) {
			t.Fatalf("unexpected structured output count: %v", output["count"])
		}
		if result.SessionID != "session-structured" {
			t.Fatalf("unexpected result session id: %s", result.SessionID)
		}
	case <-time.After(time.Second):
		t.Fatal("expected structured result message")
	}
}

func TestPersistentProcessSessionHandleResultMessageFallsBackToAssistantText(t *testing.T) {
	t.Parallel()

	turn := &turnState{
		resultCh: make(chan *ClaudeResult, 1),
	}
	session := &persistentProcessSession{
		cmd:         fakeExecCmd(1234),
		currentTurn: turn,
	}

	assistantEnvelope := map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"role": "assistant",
			"content": []map[string]any{
				{"type": "text", "text": "part one"},
				{"type": "thinking", "thinking": "hidden"},
				{"type": "text", "text": " and part two"},
			},
		},
	}
	assistantLine, err := json.Marshal(assistantEnvelope)
	if err != nil {
		t.Fatalf("Marshal assistant failed: %v", err)
	}
	session.handleStreamMessage(assistantEnvelope, string(assistantLine))

	resultEnvelope := map[string]any{
		"type":       "result",
		"session_id": "session-fallback",
		"result":     "",
	}
	resultLine, err := json.Marshal(resultEnvelope)
	if err != nil {
		t.Fatalf("Marshal result failed: %v", err)
	}

	session.handleStreamMessage(resultEnvelope, string(resultLine))

	select {
	case result := <-turn.resultCh:
		if result.Result != "part one and part two" {
			t.Fatalf("unexpected fallback reply: %q", result.Result)
		}
		if result.SessionID != "session-fallback" {
			t.Fatalf("unexpected result session id: %s", result.SessionID)
		}
	case <-time.After(time.Second):
		t.Fatal("expected fallback result message")
	}
}

func TestPersistentProcessSessionHandleResultMessagePreservesWhitespaceOnlyResult(t *testing.T) {
	t.Parallel()

	turn := &turnState{
		resultCh: make(chan *ClaudeResult, 1),
	}
	session := &persistentProcessSession{
		cmd:         fakeExecCmd(1234),
		currentTurn: turn,
	}

	assistantEnvelope := map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"role": "assistant",
			"content": []map[string]any{
				{"type": "text", "text": "fallback text"},
			},
		},
	}
	assistantLine, err := json.Marshal(assistantEnvelope)
	if err != nil {
		t.Fatalf("Marshal assistant failed: %v", err)
	}
	session.handleStreamMessage(assistantEnvelope, string(assistantLine))

	resultEnvelope := map[string]any{
		"type":       "result",
		"session_id": "session-whitespace",
		"result":     "\n  \t",
	}
	resultLine, err := json.Marshal(resultEnvelope)
	if err != nil {
		t.Fatalf("Marshal result failed: %v", err)
	}

	session.handleStreamMessage(resultEnvelope, string(resultLine))

	select {
	case result := <-turn.resultCh:
		if result.Result != "\n  \t" {
			t.Fatalf("unexpected result: %q", result.Result)
		}
		if result.SessionID != "session-whitespace" {
			t.Fatalf("unexpected result session id: %s", result.SessionID)
		}
	case <-time.After(time.Second):
		t.Fatal("expected result message")
	}
}

func newTestTurnState() *turnState {
	return &turnState{
		done:               make(chan struct{}),
		interruptRequested: make(chan struct{}),
		errCh:              make(chan error, 1),
	}
}

func TestPersistentProcessSessionInterruptWritesInterruptRequest(t *testing.T) {
	t.Parallel()

	reader, writer := io.Pipe()
	t.Cleanup(func() {
		_ = reader.Close()
		_ = writer.Close()
	})

	turn := newTestTurnState()
	session := &persistentProcessSession{
		cmd:         fakeExecCmd(1234),
		stdinPipe:   writer,
		currentTurn: turn,
	}

	requestWritten := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(reader).ReadString('\n')
		requestWritten <- line
	}()

	done := make(chan error, 1)
	go session.watchTurnInterrupt(context.Background(), turn)
	go func() {
		done <- session.Interrupt(context.Background())
	}()

	select {
	case written := <-requestWritten:
		if !strings.Contains(written, `"type":"control_request"`) {
			t.Fatalf("expected control request payload, got %q", written)
		}
		if !strings.Contains(written, `"request_id":"interrupt"`) {
			t.Fatalf("expected interrupt request id, got %q", written)
		}
		if !strings.Contains(written, `"subtype":"interrupt"`) {
			t.Fatalf("expected interrupt subtype, got %q", written)
		}
	case <-time.After(time.Second):
		t.Fatal("expected interrupt request to be written")
	}

	session.handleControlResponse(map[string]any{
		"response": map[string]any{
			"request_id": "interrupt",
			"subtype":    "success",
		},
	})

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Interrupt failed: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Interrupt did not return after control response")
	}
}

func TestPersistentProcessSessionInterruptIsIdempotentForConcurrentCallers(t *testing.T) {
	t.Parallel()

	reader, writer := io.Pipe()
	t.Cleanup(func() {
		_ = reader.Close()
		_ = writer.Close()
	})

	turn := newTestTurnState()
	session := &persistentProcessSession{
		cmd:         fakeExecCmd(1234),
		stdinPipe:   writer,
		currentTurn: turn,
	}

	requestWritten := make(chan string, 2)
	go func() {
		line, _ := bufio.NewReader(reader).ReadString('\n')
		requestWritten <- line
	}()

	doneA := make(chan error, 1)
	doneB := make(chan error, 1)
	go session.watchTurnInterrupt(context.Background(), turn)
	go func() {
		doneA <- session.Interrupt(context.Background())
	}()
	go func() {
		doneB <- session.Interrupt(context.Background())
	}()

	select {
	case written := <-requestWritten:
		if !strings.Contains(written, `"request_id":"interrupt"`) {
			t.Fatalf("expected interrupt request id, got %q", written)
		}
	case <-time.After(time.Second):
		t.Fatal("expected interrupt request to be written")
	}

	session.handleControlResponse(map[string]any{
		"response": map[string]any{
			"request_id": "interrupt",
			"subtype":    "success",
		},
	})

	select {
	case err := <-doneA:
		if err != nil {
			t.Fatalf("first InterruptCurrentTurn failed: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("first InterruptCurrentTurn did not return")
	}

	select {
	case err := <-doneB:
		if err != nil {
			t.Fatalf("second InterruptCurrentTurn failed: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("second InterruptCurrentTurn did not return")
	}
}

func TestPersistentProcessSessionRunTurnRejectsConcurrentTurn(t *testing.T) {
	t.Parallel()

	reader, writer := io.Pipe()
	t.Cleanup(func() {
		_ = reader.Close()
		_ = writer.Close()
	})

	session := &persistentProcessSession{
		cmd:       fakeExecCmd(1234),
		stdinPipe: writer,
	}

	firstStarted := make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		_, err := session.runTurnWithInput(context.Background(), []byte("{\"type\":\"user\"}\n"))
		firstDone <- err
	}()

	go func() {
		line, _ := bufio.NewReader(reader).ReadString('\n')
		if line != "" {
			close(firstStarted)
		}
	}()

	<-firstStarted

	_, err := session.runTurnWithInput(context.Background(), []byte("{\"type\":\"user\"}\n"))
	if !errors.Is(err, ErrSessionBusy) {
		t.Fatalf("expected ErrSessionBusy, got %v", err)
	}

	session.handleResultMessage(map[string]any{}, &Message{
		Type:   "result",
		Result: "done",
	})

	select {
	case err = <-firstDone:
		if err != nil {
			t.Fatalf("first RunTurn failed: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("first RunTurn did not finish")
	}
}

func TestPersistentProcessSessionCloseIgnoresUnexpectedExitSentinel(t *testing.T) {
	t.Parallel()

	exitDone := make(chan struct{})
	close(exitDone)
	scanDone := make(chan struct{})
	close(scanDone)

	session := &persistentProcessSession{
		cmd:      fakeExecCmd(12345),
		waitErr:  ErrProcessExited,
		exitDone: exitDone,
		scanDone: scanDone,
	}

	if err := session.Close(); err != nil {
		t.Fatalf("Close returned %v, want nil", err)
	}
}

func TestPersistentProcessSessionCloseIgnoresCanceledWaitOnShutdown(t *testing.T) {
	t.Parallel()

	exitDone := make(chan struct{})
	close(exitDone)
	scanDone := make(chan struct{})
	close(scanDone)

	session := &persistentProcessSession{
		cmd:      fakeExecCmd(12345),
		waitErr:  context.Canceled,
		closed:   true,
		exitDone: exitDone,
		scanDone: scanDone,
	}

	if err := session.Close(); err != nil {
		t.Fatalf("Close returned %v, want nil", err)
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

func TestPersistentProcessSessionCloseBeforeStartReturnsImmediately(t *testing.T) {
	t.Parallel()

	session := &persistentProcessSession{}

	done := make(chan error, 1)
	go func() {
		done <- session.Close()
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Close returned %v, want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close blocked for an unstarted session")
	}
}

func TestPersistentProcessSessionCloseFailsActiveTurn(t *testing.T) {
	t.Parallel()

	exitDone := make(chan struct{})
	close(exitDone)
	scanDone := make(chan struct{})
	close(scanDone)

	turn := newTestTurnState()
	session := &persistentProcessSession{
		cmd:         fakeExecCmd(12345),
		currentTurn: turn,
		exitDone:    exitDone,
		scanDone:    scanDone,
	}
	go func() {
		<-turn.interruptRequested
		close(turn.done)
	}()

	done := make(chan error, 1)
	go func() {
		done <- session.Close()
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Close returned %v, want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not return")
	}
}

func fakeExecCmd(pid int) *exec.Cmd {
	return &exec.Cmd{
		Process: &os.Process{
			Pid: pid,
		},
	}
}
