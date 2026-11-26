package acp

import (
	"encoding/json"
	"fmt"
	"os"
	"testing"
)

func TestACPSubprocessDrainsReplyBeforeExit(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"v1", "v2"} {
		t.Run(mode, func(t *testing.T) {
			session, startErr := StartSession(t.Context(), SessionOptions{
				Command: executable, Args: []string{"-test.run=^TestACPProtocolProcess$"},
				Env: []string{"ASSISTANT_ACP_PROTOCOL_TEST=" + mode},
			})
			if startErr != nil {
				t.Fatal(startErr)
			}
			t.Cleanup(func() { _ = session.Close() })
			turn, scheduleErr := session.ScheduleTurn(t.Context(), []ContentBlock{TextBlock("hello")})
			if scheduleErr != nil {
				t.Fatal(scheduleErr)
			}
			result, runErr := turn.Run(t.Context())
			if runErr != nil || result.ReplyText != "complete reply" {
				t.Fatalf("lost reply before process exit: result=%+v err=%v", result, runErr)
			}
		})
	}
}

// TestACPProtocolProcess replays ACP envelopes in a separate test process.
func TestACPProtocolProcess(t *testing.T) {
	mode := os.Getenv("ASSISTANT_ACP_PROTOCOL_TEST")
	if mode == "" {
		return
	}
	if err := replayACPProtocol(mode); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

func replayACPProtocol(mode string) error {
	input := json.NewDecoder(os.Stdin)
	output := json.NewEncoder(os.Stdout)
	for _, method := range []string{"initialize", "session/new", "session/prompt"} {
		var request protocolEnvelope
		if err := input.Decode(&request); err != nil {
			return err
		}
		if request.Method != method {
			return fmt.Errorf("expected %s, got %s", method, request.Method)
		}
		var result any
		switch method {
		case "initialize":
			result = json.RawMessage(`{"protocolVersion":2,"info":{"name":"fixture","version":"test"},"capabilities":{"session":{}}}`)
			if mode == "v1" {
				result = json.RawMessage(`{"protocolVersion":1,"agentCapabilities":{}}`)
			}
		case "session/new":
			result = map[string]any{"sessionId": "root"}
		case "session/prompt":
			if mode == "v1" {
				if err := output.Encode(json.RawMessage(`{"method":"session/update","params":{"sessionId":"root","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"complete reply"}}}}`)); err != nil {
					return err
				}
				// Exercise a final JSON-RPC line without a trailing newline.
				_, err := fmt.Fprintf(os.Stdout, `{"id":%s,"result":{"stopReason":"end_turn"}}`, request.ID)
				return err
			}
			result = map[string]any{"messageId": "user-1"}
		}
		if err := output.Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result}); err != nil {
			return err
		}
	}
	for _, update := range []json.RawMessage{
		json.RawMessage(`{"sessionUpdate":"user_message","messageId":"user-1","content":[{"type":"text","text":"hello"}]}`),
		json.RawMessage(`{"sessionUpdate":"state_update","state":"running"}`),
		json.RawMessage(`{"sessionUpdate":"agent_message","messageId":"reply","content":[{"type":"text","text":"complete reply"}]}`),
	} {
		if err := output.Encode(map[string]any{"method": "session/update", "params": map[string]any{"sessionId": "root", "update": update}}); err != nil {
			return err
		}
	}
	_, err := fmt.Fprint(os.Stdout, `{"method":"session/update","params":{"sessionId":"root","update":{"sessionUpdate":"state_update","state":"idle","stopReason":"end_turn"}}}`)
	return err
}
