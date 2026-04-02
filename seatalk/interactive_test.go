package seatalk

import (
	"encoding/json"
	"testing"
)

func TestSummarizeInteractiveMessage(t *testing.T) {
	t.Parallel()

	summary := SummarizeInteractiveMessage(&ThreadInteractiveMessage{
		Elements: []json.RawMessage{
			json.RawMessage(`{"element_type":"title","title":{"text":" Build Failed "}}`),
			json.RawMessage(`{"element_type":"description","description":{"text":"Job #42 needs attention"}}`),
			json.RawMessage(`{"element_type":"button_group","button_group":[{"button_type":"callback","text":"Retry"},{"button_type":"redirect","text":"Open Run","desktop_link":{"type":"web","path":"https://example.com/run/42"}}]}`),
			json.RawMessage(`{"element_type":"image","image":{"content":"ignored"}}`),
		},
	})

	expected := `interactive card; title="Build Failed"; description="Job #42 needs attention"; buttons=[Retry, Open Run (https://example.com/run/42)]; images=1`
	if summary != expected {
		t.Fatalf("unexpected summary: %q", summary)
	}
}

func TestSummarizeInteractiveMessageIncludesImageURLs(t *testing.T) {
	t.Parallel()

	summary := SummarizeInteractiveMessage(&ThreadInteractiveMessage{
		Elements: []json.RawMessage{
			json.RawMessage(`{"element_type":"title","title":{"text":"Build Failed"}}`),
			json.RawMessage(`{"element_type":"image","image":{"content":"https://example.com/image-1.png"}}`),
			json.RawMessage(`{"element_type":"image","image":{"content":"https://example.com/image-2.png"}}`),
		},
	})

	expected := `interactive card; title="Build Failed"; image_urls=[https://example.com/image-1.png, https://example.com/image-2.png]`
	if summary != expected {
		t.Fatalf("unexpected summary: %q", summary)
	}
}
