package claudecode

import (
	"context"
	"encoding/json"
	"io"
	"testing"
	"time"
)

func TestClaudeProtocolCorrelatesMessagesAndSupersedesText(t *testing.T) {
	t.Parallel()
	turn := &turnState{userMessageID: "current", resultCh: make(chan *ClaudeResult, 1)}
	session := &persistentProcessSession{currentTurn: turn}
	for _, line := range []string{
		`{"type":"assistant","uuid":"retry","user_message_uuid":"current","message":{"role":"assistant","content":[{"type":"text","text":"discarded"}]}}`,
		`{"type":"assistant","parent_tool_use_id":"agent-tool","message":{"role":"assistant","content":[{"type":"text","text":"subagent"}]}}`,
		`{"type":"assistant","user_message_uuid":"previous","message":{"role":"assistant","content":[{"type":"text","text":"stale"}]}}`,
		`{"type":"assistant","origin":{"kind":"task-notification"},"message":{"role":"assistant","content":[{"type":"text","text":"background"}]}}`,
		`{"type":"assistant","uuid":"final","supersedes":["retry"],"user_message_uuids":["current","other"],"user_message_uuid":"other","message":{"role":"assistant","content":[{"type":"text","text":"final"}]}}`,
		`{"type":"result","subtype":"success","result":"previous reply","user_message_uuid":"previous"}`,
		`{"type":"result","subtype":"success","result":"","num_turns":0,"origin":{"kind":"task-notification"}}`,
	} {
		replayStreamMessage(t, session, line)
	}
	select {
	case result := <-turn.resultCh:
		t.Fatalf("unrelated result completed the turn: %+v", result)
	default:
	}
	replayStreamMessage(t, session, `{"type":"result","subtype":"success","user_message_uuids":["current","other"],"user_message_uuid":"other"}`)
	if result := <-turn.resultCh; result.Result != "final" {
		t.Fatalf("fallback text included unrelated or superseded content: %+v", result)
	}
}

func TestClaudeProtocolDuplicateResultsDoNotBlockReader(t *testing.T) {
	t.Parallel()
	turn := &turnState{resultCh: make(chan *ClaudeResult, 1)}
	session := &persistentProcessSession{currentTurn: turn}
	done := make(chan struct{})
	go func() {
		session.handleResultMessage(nil, &Message{Type: "result", Result: "first"})
		session.handleResultMessage(nil, &Message{Type: "result", Result: "duplicate"})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("duplicate result blocked stdout processing")
	}
	if result := <-turn.resultCh; result.Result != "first" {
		t.Fatalf("duplicate result replaced the first: %+v", result)
	}
}

func TestClaudeProtocolCloseCancelsControlRequests(t *testing.T) {
	t.Parallel()
	started := make(chan struct{})
	canceled := make(chan struct{})
	reader, writer := io.Pipe()
	t.Cleanup(func() {
		_ = reader.Close()
		_ = writer.Close()
	})
	session := &persistentProcessSession{
		stdinPipe: writer,
		hooks: SessionHooks{HandleControlRequest: func(ctx context.Context, _ map[string]any) (map[string]any, error) {
			close(started)
			<-ctx.Done()
			close(canceled)
			return nil, ctx.Err()
		}},
	}
	session.handleControlRequest(t.Context(), map[string]any{"request_id": "pending", "request": map[string]any{"subtype": "mcp_message"}})
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("control handler did not start")
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("session close did not cancel its control handler")
	}
}

func replayStreamMessage(t *testing.T, session *persistentProcessSession, line string) {
	t.Helper()
	var envelope map[string]any
	if err := json.Unmarshal([]byte(line), &envelope); err != nil {
		t.Fatal(err)
	}
	session.handleStreamMessage(envelope, line)
}
