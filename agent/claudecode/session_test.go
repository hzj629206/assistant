package claudecode

import (
	"encoding/json"
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestPersistentProcessSessionShouldInitializeOnlyOnce(t *testing.T) {
	t.Parallel()

	session := &persistentProcessSession{}
	hooks := TurnHooks{
		ShouldInitialize: func() bool { return true },
		HandleControlRequest: func(map[string]any) (map[string]any, error) {
			return nil, nil
		},
	}

	if !session.shouldInitialize(hooks) {
		t.Fatal("expected first turn to require initialize")
	}

	session.markInitialized()

	if session.shouldInitialize(hooks) {
		t.Fatal("did not expect initialize after session is marked initialized")
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

	if got := session.CurrentSessionID(); got != "session-from-system" {
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

	if got := session.CurrentSessionID(); got != "session-result" {
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

func TestPersistentProcessSessionCloseIgnoresUnexpectedExitSentinel(t *testing.T) {
	t.Parallel()

	exitDone := make(chan struct{})
	close(exitDone)
	stderrDone := make(chan struct{})
	close(stderrDone)
	scanDone := make(chan struct{})
	close(scanDone)

	session := &persistentProcessSession{
		cmd:        fakeExecCmd(12345),
		waitErr:    ErrProcessExited,
		exitDone:   exitDone,
		stderrDone: stderrDone,
		scanDone:   scanDone,
	}

	if err := session.Close(); err != nil {
		t.Fatalf("Close returned %v, want nil", err)
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

func fakeExecCmd(pid int) *exec.Cmd {
	return &exec.Cmd{
		Process: &os.Process{
			Pid: pid,
		},
	}
}
