package codex

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestThreadEventUnmarshalAgentMessage(t *testing.T) {
	raw := `{"type":"item.completed","item":{"id":"1","type":"agent_message","text":"hello"}}`
	var event ThreadEvent
	if err := json.Unmarshal([]byte(raw), &event); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if event.Type != "item.completed" {
		t.Fatalf("unexpected type: %s", event.Type)
	}
	msg, ok := event.Item.(*AgentMessageItem)
	if !ok {
		t.Fatalf("expected agent_message item, got %T", event.Item)
	}
	if msg.Text != "hello" {
		t.Fatalf("unexpected message: %s", msg.Text)
	}
}

func TestThreadEventUnmarshalUnknownItem(t *testing.T) {
	raw := `{"type":"item.completed","item":{"id":"1","type":"unknown","payload":{"ok":true}}}`
	var event ThreadEvent
	if err := json.Unmarshal([]byte(raw), &event); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	item, ok := event.Item.(*UnknownItem)
	if !ok {
		t.Fatalf("expected unknown item, got %T", event.Item)
	}
	if item.Type != "unknown" {
		t.Fatalf("unexpected item type: %s", item.Type)
	}

	var decoded map[string]any
	if err := json.Unmarshal(item.Raw, &decoded); err != nil {
		t.Fatalf("decode raw: %v", err)
	}

	want := map[string]any{
		"id":   "1",
		"type": "unknown",
		"payload": map[string]any{
			"ok": true,
		},
	}
	if !reflect.DeepEqual(decoded, want) {
		t.Fatalf("unexpected raw payload: %#v", decoded)
	}
}
