package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/godeps/codex-sdk-go"
)

type fakeCodexThread struct {
	id      string
	turns   []codex.Turn
	inputs  []codex.Input
	options []codex.TurnOptions
}

func (t *fakeCodexThread) ID() string {
	return t.id
}

func (t *fakeCodexThread) RunStreamed(input codex.Input, options codex.TurnOptions) (*codex.StreamedTurn, error) {
	t.inputs = append(t.inputs, input)
	t.options = append(t.options, options)
	if len(t.turns) == 0 {
		return nil, errors.New("unexpected turn")
	}

	turn := t.turns[0]
	t.turns = t.turns[1:]

	events := make([]codex.ThreadEvent, 0, len(turn.Items)+2)
	if len(turn.Items) == 0 && turn.FinalResponse != "" {
		turn.Items = append(turn.Items, &codex.AgentMessageItem{
			ID:   "agent-message",
			Type: "agent_message",
			Text: turn.FinalResponse,
		})
	}
	for _, item := range turn.Items {
		events = append(events, codex.ThreadEvent{
			Type: "item.completed",
			Item: item,
		})
	}
	events = append(events, codex.ThreadEvent{
		Type:  "turn.completed",
		Usage: turn.Usage,
	})

	return &codex.StreamedTurn{
		Events: closedEvents(events...),
		Done:   closedDone(nil),
	}, nil
}

type uppercaseTool struct{}

func (uppercaseTool) Name() string {
	return "uppercase"
}

func (uppercaseTool) Description() string {
	return "Uppercase the provided text."
}

func (uppercaseTool) InputSchema() any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"text": map[string]any{"type": "string"},
		},
		"required":             []any{"text"},
		"additionalProperties": false,
	}
}

func (uppercaseTool) OutputSchema() any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"text": map[string]any{"type": "string"},
		},
		"required":             []any{"text"},
		"additionalProperties": false,
	}
}

func (uppercaseTool) Call(_ context.Context, input json.RawMessage) (any, error) {
	var payload struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(input, &payload); err != nil {
		return nil, err
	}

	return map[string]string{"text": strings.ToUpper(payload.Text)}, nil
}

func TestRunToolLoopExecutesToolAndReturnsFinalResponse(t *testing.T) {
	t.Parallel()

	runner := &CodexRunner{
		maxToolIterations: 3,
	}
	runner.RegisterTools(uppercaseTool{})
	thread := &fakeCodexThread{
		id: "thread-1",
		turns: []codex.Turn{
			{
				FinalResponse: `{"action":"call_tool","tool_name":"uppercase","tool_input_json":"{\"text\":\"hello\"}"}`,
			},
			{
				FinalResponse: `{"action":"respond","message":"HELLO"}`,
			},
		},
	}

	req := TurnRequest{
		Message: InboundMessage{
			Kind: MessageKindText,
			Text: "Please uppercase hello",
		},
	}
	input, err := runner.buildTurnInput(req)
	if err != nil {
		t.Fatalf("build turn input failed: %v", err)
	}

	reply, err := runner.runToolLoop(context.Background(), req, thread, input)
	if err != nil {
		t.Fatalf("run tool loop failed: %v", err)
	}
	if reply != "HELLO" {
		t.Fatalf("unexpected reply: %s", reply)
	}
	if len(thread.inputs) != 2 {
		t.Fatalf("unexpected input count: %d", len(thread.inputs))
	}
	if got := thread.inputs[0].Text; !strings.Contains(got, "structured tool loop") || !strings.Contains(got, "Please uppercase hello") {
		t.Fatalf("unexpected first prompt: %s", got)
	}
	if got := thread.inputs[0].Text; !strings.Contains(got, `"output_schema"`) {
		t.Fatalf("tool output schema missing from prompt: %s", got)
	}
	if got := thread.inputs[1].Text; !strings.Contains(got, `{"text":"HELLO"}`) {
		t.Fatalf("unexpected tool result prompt: %s", got)
	}
	if thread.options[0].OutputSchema == nil || thread.options[1].OutputSchema == nil {
		t.Fatalf("expected tool loop schema on every turn")
	}
}

func TestRunToolLoopReturnsUnknownToolError(t *testing.T) {
	t.Parallel()

	runner := &CodexRunner{
		maxToolIterations: 1,
	}
	runner.RegisterTools(uppercaseTool{})
	thread := &fakeCodexThread{
		turns: []codex.Turn{
			{
				FinalResponse: `{"action":"call_tool","tool_name":"missing","tool_input_json":"{}"}`,
			},
		},
	}

	req := TurnRequest{
		Message: InboundMessage{
			Kind: MessageKindText,
			Text: "test",
		},
	}
	input, err := runner.buildTurnInput(req)
	if err != nil {
		t.Fatalf("build turn input failed: %v", err)
	}

	_, err = runner.runToolLoop(context.Background(), req, thread, input)
	if err == nil || !strings.Contains(err.Error(), `unknown tool "missing"`) {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRunToolLoopSupportsSilentCompletion(t *testing.T) {
	t.Parallel()

	runner := &CodexRunner{
		maxToolIterations: 1,
	}
	runner.RegisterTools(uppercaseTool{})
	thread := &fakeCodexThread{
		id: "thread-silent",
		turns: []codex.Turn{
			{
				FinalResponse: `{"action":"silent","message":"","tool_name":"","tool_input_json":""}`,
			},
		},
	}

	req := TurnRequest{
		Message: InboundMessage{
			Kind: MessageKindText,
			Text: "No reply needed",
		},
	}
	input, err := runner.buildTurnInput(req)
	if err != nil {
		t.Fatalf("build turn input failed: %v", err)
	}

	reply, err := runner.runToolLoop(context.Background(), req, thread, input)
	if err != nil {
		t.Fatalf("run tool loop failed: %v", err)
	}
	if reply != "" {
		t.Fatalf("unexpected reply: %q", reply)
	}
}
