package adapter

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/hzj629206/assistant/agent"
	"github.com/hzj629206/assistant/seatalk"
)

func TestSeaTalkInteractiveSendToolUsesCurrentConversationTarget(t *testing.T) {
	t.Parallel()

	client := seatalk.NewClient(
		seatalk.Config{AppID: "app-id", AppSecret: "app-secret"},
		seatalk.WithHTTPClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.Method != http.MethodPost {
				t.Fatalf("unexpected method: %s", req.Method)
			}
			if req.URL.Path != "/messaging/v2/group_chat" {
				t.Fatalf("unexpected path: %s", req.URL.Path)
			}

			var body map[string]any
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				t.Fatalf("decode request failed: %v", err)
			}
			if body["group_id"] != "group-1" {
				t.Fatalf("unexpected group id: %#v", body["group_id"])
			}
			message, ok := body["message"].(map[string]any)
			if !ok {
				t.Fatalf("unexpected message payload: %#v", body["message"])
			}
			if message["thread_id"] != "thread-1" {
				t.Fatalf("unexpected thread id: %#v", message["thread_id"])
			}
			interactiveMessage, ok := message["interactive_message"].(map[string]any)
			if !ok {
				t.Fatalf("unexpected interactive message payload: %#v", message["interactive_message"])
			}
			elements, ok := interactiveMessage["elements"].([]any)
			if !ok || len(elements) != 2 {
				t.Fatalf("unexpected elements payload: %#v", interactiveMessage["elements"])
			}
			buttonElement, ok := elements[1].(map[string]any)
			if !ok {
				t.Fatalf("unexpected button element payload: %#v", elements[1])
			}
			button, ok := buttonElement["button"].(map[string]any)
			if !ok {
				t.Fatalf("unexpected button payload: %#v", buttonElement["button"])
			}
			value, ok := button["value"].(string)
			if !ok {
				t.Fatalf("unexpected button value payload: %#v", button["value"])
			}
			resolvedValue, err := resolveInteractiveCallbackValue(context.Background(), value)
			if err != nil {
				t.Fatalf("resolve callback value failed: %v", err)
			}
			if resolvedValue != `{"action":"tool_call","tool_name":"seatalk_push_interactive_message","tool_input_json":"{\"mode\":\"update\",\"message_id\":\"interactive-msg-1\",\"elements\":[{\"element_type\":\"title\",\"title\":{\"text\":\"Approved\"}}]}"}` {
				t.Fatalf("unexpected resolved button value: %#v", resolvedValue)
			}

			return jsonResponse(t, map[string]any{
				"code":       0,
				"message_id": "interactive-msg-1",
			}), nil
		})}),
		seatalk.WithTokenProvider(func(context.Context, *http.Client, string, string) (string, error) {
			return "token-123", nil
		}),
	)

	tool := seaTalkPushInteractiveMessageTool{}
	ctx := agent.ContextWithTurnRequest(context.Background(), agent.TurnRequest{
		Conversation: agent.ConversationState{Key: "seatalk:group:group-1:thread-1"},
		Message: agent.InboundMessage{
			Responder: &SeaTalkResponder{
				client: client,
				target: seaTalkReplyTarget{
					isGroup:  true,
					groupID:  "group-1",
					threadID: "thread-1",
				},
			},
		},
	})

	result, err := tool.Call(ctx, json.RawMessage(`{
		"mode": "send",
		"elements": [
			{"element_type":"title","title":{"text":"Choose action"}},
			{"element_type":"button","button":{"button_type":"callback","text":"Approve","value":"{\"action\":\"tool_call\",\"tool_name\":\"seatalk_push_interactive_message\",\"tool_input_json\":\"{\\\"mode\\\":\\\"update\\\",\\\"message_id\\\":\\\"interactive-msg-1\\\",\\\"elements\\\":[{\\\"element_type\\\":\\\"title\\\",\\\"title\\\":{\\\"text\\\":\\\"Approved\\\"}}]}\"}"}}
		]
	}`))
	if err != nil {
		t.Fatalf("tool call failed: %v", err)
	}

	body, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("unexpected tool result type: %T", result)
	}
	if body["message_id"] != "interactive-msg-1" {
		t.Fatalf("unexpected message id: %#v", body["message_id"])
	}
}

func TestSeaTalkPushInteractiveMessageToolDefaultsDescriptionFormatToMarkdown(t *testing.T) {
	t.Parallel()

	client := seatalk.NewClient(
		seatalk.Config{AppID: "app-id", AppSecret: "app-secret"},
		seatalk.WithHTTPClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.Method != http.MethodPost {
				t.Fatalf("unexpected method: %s", req.Method)
			}
			if req.URL.Path != "/messaging/v2/group_chat" {
				t.Fatalf("unexpected path: %s", req.URL.Path)
			}

			var body map[string]any
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				t.Fatalf("decode request failed: %v", err)
			}
			message, ok := body["message"].(map[string]any)
			if !ok {
				t.Fatalf("unexpected message payload: %#v", body["message"])
			}
			interactiveMessage, ok := message["interactive_message"].(map[string]any)
			if !ok {
				t.Fatalf("unexpected interactive message payload: %#v", message["interactive_message"])
			}
			elements, ok := interactiveMessage["elements"].([]any)
			if !ok || len(elements) != 1 {
				t.Fatalf("unexpected elements payload: %#v", interactiveMessage["elements"])
			}
			descriptionElement, ok := elements[0].(map[string]any)
			if !ok {
				t.Fatalf("unexpected description element payload: %#v", elements[0])
			}
			description, ok := descriptionElement["description"].(map[string]any)
			if !ok {
				t.Fatalf("unexpected description payload: %#v", descriptionElement["description"])
			}
			if got := description["format"]; got != float64(seatalk.TextFormatMarkdown) {
				t.Fatalf("unexpected description format: %#v", got)
			}

			return jsonResponse(t, map[string]any{
				"code":       0,
				"message_id": "interactive-msg-markdown",
			}), nil
		})}),
		seatalk.WithTokenProvider(func(context.Context, *http.Client, string, string) (string, error) {
			return "token-123", nil
		}),
	)

	tool := seaTalkPushInteractiveMessageTool{}
	ctx := agent.ContextWithTurnRequest(context.Background(), agent.TurnRequest{
		Conversation: agent.ConversationState{Key: "seatalk:group:group-1:thread-1"},
		Message: agent.InboundMessage{
			Responder: &SeaTalkResponder{
				client: client,
				target: seaTalkReplyTarget{
					isGroup:  true,
					groupID:  "group-1",
					threadID: "thread-1",
				},
			},
		},
	})

	result, err := tool.Call(ctx, json.RawMessage(`{
		"mode": "send",
		"elements": [
			{"element_type":"description","description":{"text":"**Build failed**. [Open run](https://example.com/run/42)"}}
		]
	}`))
	if err != nil {
		t.Fatalf("tool call failed: %v", err)
	}

	responseBody, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("unexpected tool result type: %T", result)
	}
	if responseBody["message_id"] != "interactive-msg-markdown" {
		t.Fatalf("unexpected message id: %#v", responseBody["message_id"])
	}
}

func TestSeaTalkPushInteractiveMessageToolTruncatesLongTitleAndButtonTextFields(t *testing.T) {
	t.Parallel()

	longTitle := strings.Repeat("T", interactiveTitleMaxLength+10)
	longButtonText := strings.Repeat("B", interactiveButtonTextMaxLength+10)

	client := seatalk.NewClient(
		seatalk.Config{AppID: "app-id", AppSecret: "app-secret"},
		seatalk.WithHTTPClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.Method != http.MethodPost {
				t.Fatalf("unexpected method: %s", req.Method)
			}
			if req.URL.Path != "/messaging/v2/group_chat" {
				t.Fatalf("unexpected path: %s", req.URL.Path)
			}

			var body map[string]any
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				t.Fatalf("decode request failed: %v", err)
			}
			message, ok := body["message"].(map[string]any)
			if !ok {
				t.Fatalf("unexpected message payload: %#v", body["message"])
			}
			interactiveMessage, ok := message["interactive_message"].(map[string]any)
			if !ok {
				t.Fatalf("unexpected interactive message payload: %#v", message["interactive_message"])
			}
			elements, ok := interactiveMessage["elements"].([]any)
			if !ok || len(elements) != 2 {
				t.Fatalf("unexpected elements payload: %#v", interactiveMessage["elements"])
			}

			titleElement, ok := elements[0].(map[string]any)
			if !ok {
				t.Fatalf("unexpected title element payload: %#v", elements[0])
			}
			title, ok := titleElement["title"].(map[string]any)
			if !ok {
				t.Fatalf("unexpected title payload: %#v", titleElement["title"])
			}
			titleText, ok := title["text"].(string)
			if !ok {
				t.Fatalf("unexpected title text payload: %#v", title["text"])
			}
			if utf8.RuneCountInString(titleText) != interactiveTitleMaxLength {
				t.Fatalf("unexpected title length: %d", utf8.RuneCountInString(titleText))
			}

			buttonElement, ok := elements[1].(map[string]any)
			if !ok {
				t.Fatalf("unexpected button element payload: %#v", elements[1])
			}
			button, ok := buttonElement["button"].(map[string]any)
			if !ok {
				t.Fatalf("unexpected button payload: %#v", buttonElement["button"])
			}
			buttonText, ok := button["text"].(string)
			if !ok {
				t.Fatalf("unexpected button text payload: %#v", button["text"])
			}
			if utf8.RuneCountInString(buttonText) != interactiveButtonTextMaxLength {
				t.Fatalf("unexpected button text length: %d", utf8.RuneCountInString(buttonText))
			}

			return jsonResponse(t, map[string]any{
				"code":       0,
				"message_id": "interactive-msg-truncated",
			}), nil
		})}),
		seatalk.WithTokenProvider(func(context.Context, *http.Client, string, string) (string, error) {
			return "token-123", nil
		}),
	)

	tool := seaTalkPushInteractiveMessageTool{}
	ctx := agent.ContextWithTurnRequest(context.Background(), agent.TurnRequest{
		Conversation: agent.ConversationState{Key: "seatalk:group:group-1:thread-1"},
		Message: agent.InboundMessage{
			Responder: &SeaTalkResponder{
				client: client,
				target: seaTalkReplyTarget{
					isGroup:  true,
					groupID:  "group-1",
					threadID: "thread-1",
				},
			},
		},
	})

	input, err := json.Marshal(map[string]any{
		"mode": "send",
		"elements": []map[string]any{
			{"element_type": "title", "title": map[string]any{"text": longTitle}},
			{
				"element_type": "button",
				"button": map[string]any{
					"button_type": "callback",
					"text":        longButtonText,
					"value":       `{"action":"prompt","prompt":"continue"}`,
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("marshal input failed: %v", err)
	}

	result, err := tool.Call(ctx, input)
	if err != nil {
		t.Fatalf("tool call failed: %v", err)
	}

	responseBody, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("unexpected tool result type: %T", result)
	}
	if responseBody["message_id"] != "interactive-msg-truncated" {
		t.Fatalf("unexpected message id: %#v", responseBody["message_id"])
	}
}

func TestSeaTalkPushInteractiveMessageToolNormalizesDescriptionMarkdown(t *testing.T) {
	t.Parallel()

	descriptionText := "- item 1\n\n- item 2\n\n```go\nfmt.Println(\"hello\")\n```"

	client := seatalk.NewClient(
		seatalk.Config{AppID: "app-id", AppSecret: "app-secret"},
		seatalk.WithHTTPClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.Method != http.MethodPost {
				t.Fatalf("unexpected method: %s", req.Method)
			}
			if req.URL.Path != "/messaging/v2/group_chat" {
				t.Fatalf("unexpected path: %s", req.URL.Path)
			}

			var body map[string]any
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				t.Fatalf("decode request failed: %v", err)
			}
			message, ok := body["message"].(map[string]any)
			if !ok {
				t.Fatalf("unexpected message payload: %#v", body["message"])
			}
			interactiveMessage, ok := message["interactive_message"].(map[string]any)
			if !ok {
				t.Fatalf("unexpected interactive message payload: %#v", message["interactive_message"])
			}
			elements, ok := interactiveMessage["elements"].([]any)
			if !ok || len(elements) != 1 {
				t.Fatalf("unexpected elements payload: %#v", interactiveMessage["elements"])
			}
			descriptionElement, ok := elements[0].(map[string]any)
			if !ok {
				t.Fatalf("unexpected description element payload: %#v", elements[0])
			}
			description, ok := descriptionElement["description"].(map[string]any)
			if !ok {
				t.Fatalf("unexpected description payload: %#v", descriptionElement["description"])
			}
			descriptionText, ok := description["text"].(string)
			if !ok {
				t.Fatalf("unexpected description text payload: %#v", description["text"])
			}
			if strings.Contains(descriptionText, "```go") {
				t.Fatalf("description should strip code fence language identifiers: %q", descriptionText)
			}
			if !strings.Contains(descriptionText, "- item 1\n\n- item 2\n\n```") {
				t.Fatalf("description should preserve top-level unordered list spacing and code fence structure: %q", descriptionText)
			}
			if strings.Count(descriptionText, "```")%2 != 0 {
				t.Fatalf("description should keep code fences balanced: %q", descriptionText)
			}

			return jsonResponse(t, map[string]any{
				"code":       0,
				"message_id": "interactive-msg-description-markdown",
			}), nil
		})}),
		seatalk.WithTokenProvider(func(context.Context, *http.Client, string, string) (string, error) {
			return "token-123", nil
		}),
	)

	tool := seaTalkPushInteractiveMessageTool{}
	ctx := agent.ContextWithTurnRequest(context.Background(), agent.TurnRequest{
		Conversation: agent.ConversationState{Key: "seatalk:group:group-1:thread-1"},
		Message: agent.InboundMessage{
			Responder: &SeaTalkResponder{
				client: client,
				target: seaTalkReplyTarget{
					isGroup:  true,
					groupID:  "group-1",
					threadID: "thread-1",
				},
			},
		},
	})

	input, err := json.Marshal(map[string]any{
		"mode": "send",
		"elements": []map[string]any{
			{"element_type": "description", "description": map[string]any{"text": descriptionText}},
		},
	})
	if err != nil {
		t.Fatalf("marshal input failed: %v", err)
	}

	result, err := tool.Call(ctx, input)
	if err != nil {
		t.Fatalf("tool call failed: %v", err)
	}

	responseBody, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("unexpected tool result type: %T", result)
	}
	if responseBody["message_id"] != "interactive-msg-description-markdown" {
		t.Fatalf("unexpected message id: %#v", responseBody["message_id"])
	}
}

func TestSeaTalkPushInteractiveMessageToolRejectsDescriptionOverHardLimit(t *testing.T) {
	t.Parallel()

	tool := seaTalkPushInteractiveMessageTool{}
	ctx := agent.ContextWithTurnRequest(context.Background(), agent.TurnRequest{
		Conversation: agent.ConversationState{Key: "seatalk:group:group-1:thread-1"},
		Message: agent.InboundMessage{
			Responder: &SeaTalkResponder{
				client: seatalk.NewClient(seatalk.Config{AppID: "app-id", AppSecret: "app-secret"}),
				target: seaTalkReplyTarget{
					isGroup:  true,
					groupID:  "group-1",
					threadID: "thread-1",
				},
			},
		},
	})

	input, err := json.Marshal(map[string]any{
		"mode": "send",
		"elements": []map[string]any{
			{"element_type": "description", "description": map[string]any{"text": strings.Repeat("D", interactiveDescriptionMaxLength+10)}},
		},
	})
	if err != nil {
		t.Fatalf("marshal input failed: %v", err)
	}

	_, err = tool.Call(ctx, input)
	if err == nil {
		t.Fatal("expected description length error")
	}
	if !strings.Contains(err.Error(), "description.text exceeds SeaTalk hard limit") {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(err.Error(), "keep description.text within 1000 characters") {
		t.Fatalf("unexpected error guidance: %v", err)
	}
}

func TestSeaTalkPushInteractiveMessageToolDescriptionMentionsMarkdown(t *testing.T) {
	t.Parallel()

	description := seaTalkPushInteractiveMessageTool{}.Description()
	if !strings.Contains(description, `mode="send": always send a new interactive card and ignore "message_id".`) {
		t.Fatalf("expected send mode rule in tool description, got %q", description)
	}
	if !strings.Contains(description, `mode="update": update "message_id" if provided, otherwise update the current interactive card in context. Fail if neither target is available.`) {
		t.Fatalf("expected update mode rule in tool description, got %q", description)
	}
	if !strings.Contains(description, "Elements are rendered top-to-bottom in array order. Mix title, description, button, button_group, and image elements freely to build the card.") {
		t.Fatalf("expected element stacking guidance in tool description, got %q", description)
	}
	if !strings.Contains(description, "Limits per card: title <= 3, description <= 5, standalone button <= 5, button_group <= 3, image <= 3. Limits per button group: button <= 3.") {
		t.Fatalf("expected card element limit guidance in tool description, got %q", description)
	}
	if !strings.Contains(description, "Description elements must use SeaTalk Markdown and satisfy the restrictions. Each supports up to 1000 characters.") {
		t.Fatalf("expected description markdown and length guidance in tool description, got %q", description)
	}
	if !strings.Contains(description, `{"action":"tool_call","tool_name":"...","tool_input_json":"{...}"}`) {
		t.Fatalf("expected callback payload example in tool description, got %q", description)
	}
	if strings.Contains(description, "Use this when the user needs explicit choices, confirmation, approval, or clear status updates for important events and progress.") {
		t.Fatalf("tool description should focus on interface contract, got %q", description)
	}
}

func TestSeaTalkPushInteractiveMessageToolRejectsElementCountLimitExceeded(t *testing.T) {
	t.Parallel()

	tool := seaTalkPushInteractiveMessageTool{}
	ctx := agent.ContextWithTurnRequest(context.Background(), agent.TurnRequest{
		Conversation: agent.ConversationState{Key: "seatalk:group:group-1:thread-1"},
		Message: agent.InboundMessage{
			Responder: &SeaTalkResponder{
				target: seaTalkReplyTarget{
					isGroup:  true,
					groupID:  "group-1",
					threadID: "thread-1",
				},
			},
		},
	})

	input, err := json.Marshal(map[string]any{
		"mode": "send",
		"elements": []map[string]any{
			{"element_type": "title", "title": map[string]any{"text": "Title 1"}},
			{"element_type": "title", "title": map[string]any{"text": "Title 2"}},
			{"element_type": "title", "title": map[string]any{"text": "Title 3"}},
			{"element_type": "title", "title": map[string]any{"text": "Title 4"}},
		},
	})
	if err != nil {
		t.Fatalf("marshal input failed: %v", err)
	}

	_, err = tool.Call(ctx, input)
	if err == nil {
		t.Fatal("expected tool call to fail when title element count exceeds limit")
	}
	if !strings.Contains(err.Error(), "interactive card element count exceeds per-card limits") {
		t.Fatalf("unexpected tool error: %v", err)
	}
	if !strings.Contains(err.Error(), "title=4 (max 3)") {
		t.Fatalf("unexpected tool error: %v", err)
	}
}

func TestSeaTalkPushInteractiveMessageToolInputSchemaDescribesModeBehavior(t *testing.T) {
	t.Parallel()

	schema, ok := seaTalkPushInteractiveMessageTool{}.InputSchema().(map[string]any)
	if !ok {
		t.Fatalf("unexpected schema type: %T", seaTalkPushInteractiveMessageTool{}.InputSchema())
	}
	properties, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("unexpected properties payload: %#v", schema["properties"])
	}
	modeSchema, ok := properties["mode"].(map[string]any)
	if !ok {
		t.Fatalf("unexpected mode schema: %#v", properties["mode"])
	}
	if _, exists := modeSchema["description"]; exists {
		t.Fatalf("mode description should be omitted when enum and required already define the contract: %#v", modeSchema["description"])
	}
	required, ok := schema["required"].([]any)
	if !ok {
		t.Fatalf("unexpected required payload: %#v", schema["required"])
	}
	requiredFields := make(map[string]struct{}, len(required))
	for _, field := range required {
		name, ok := field.(string)
		if !ok {
			t.Fatalf("unexpected required field entry: %#v", field)
		}
		requiredFields[name] = struct{}{}
	}
	if len(requiredFields) != 2 {
		t.Fatalf("unexpected required fields: %#v", required)
	}
	if _, ok := requiredFields["elements"]; !ok {
		t.Fatalf("required fields missing elements: %#v", required)
	}
	if _, ok := requiredFields["mode"]; !ok {
		t.Fatalf("unexpected required fields: %#v", required)
	}
	messageID, ok := properties["message_id"].(map[string]any)
	if !ok {
		t.Fatalf("unexpected message_id schema: %#v", properties["message_id"])
	}
	description, ok := messageID["description"].(string)
	if !ok {
		t.Fatalf("unexpected message_id description: %#v", messageID["description"])
	}
	if description != `Optional target interactive message ID. Ignored when mode="send".` {
		t.Fatalf("expected mode-aware message_id description, got %q", description)
	}
	elements, ok := properties["elements"].(map[string]any)
	if !ok {
		t.Fatalf("unexpected elements schema: %#v", properties["elements"])
	}
	items, ok := elements["items"].(map[string]any)
	if !ok {
		t.Fatalf("unexpected elements item schema: %#v", elements["items"])
	}
	itemProperties, ok := items["properties"].(map[string]any)
	if !ok {
		t.Fatalf("unexpected elements item properties: %#v", items["properties"])
	}
	descriptionSchema, ok := itemProperties["description"].(map[string]any)
	if !ok {
		t.Fatalf("unexpected description schema: %#v", itemProperties["description"])
	}
	titleSchema, ok := itemProperties["title"].(map[string]any)
	if !ok {
		t.Fatalf("unexpected title schema: %#v", itemProperties["title"])
	}
	titleProperties, ok := titleSchema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("unexpected title properties: %#v", titleSchema["properties"])
	}
	titleTextSchema, ok := titleProperties["text"].(map[string]any)
	if !ok {
		t.Fatalf("unexpected title.text schema: %#v", titleProperties["text"])
	}
	if maxLength, ok := titleTextSchema["maxLength"].(int); !ok || maxLength != 120 {
		t.Fatalf("unexpected title.text maxLength: %#v", titleTextSchema["maxLength"])
	}
	descriptionProperties, ok := descriptionSchema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("unexpected description properties: %#v", descriptionSchema["properties"])
	}
	descriptionTextSchema, ok := descriptionProperties["text"].(map[string]any)
	if !ok {
		t.Fatalf("unexpected description.text schema: %#v", descriptionProperties["text"])
	}
	if maxLength, ok := descriptionTextSchema["maxLength"].(int); !ok || maxLength != interactiveDescriptionMaxLength {
		t.Fatalf("unexpected description.text maxLength: %#v", descriptionTextSchema["maxLength"])
	}
	if _, exists := descriptionTextSchema["description"]; exists {
		t.Fatalf("description.text schema should not include description: %#v", descriptionTextSchema["description"])
	}
	if _, exists := descriptionProperties["format"]; exists {
		t.Fatalf("description.format should not be exposed in schema: %#v", descriptionProperties["format"])
	}
	buttonSchema, ok := itemProperties["button"].(map[string]any)
	if !ok {
		t.Fatalf("unexpected button schema: %#v", itemProperties["button"])
	}
	buttonProperties, ok := buttonSchema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("unexpected button properties: %#v", buttonSchema["properties"])
	}
	buttonTextSchema, ok := buttonProperties["text"].(map[string]any)
	if !ok {
		t.Fatalf("unexpected button.text schema: %#v", buttonProperties["text"])
	}
	if maxLength, ok := buttonTextSchema["maxLength"].(int); !ok || maxLength != 50 {
		t.Fatalf("unexpected button.text maxLength: %#v", buttonTextSchema["maxLength"])
	}
	buttonTypeSchema, ok := buttonProperties["button_type"].(map[string]any)
	if !ok {
		t.Fatalf("unexpected button_type schema: %#v", buttonProperties["button_type"])
	}
	buttonTypeDescription, ok := buttonTypeSchema["description"].(string)
	if !ok {
		t.Fatalf("unexpected button_type description: %#v", buttonTypeSchema["description"])
	}
	if buttonTypeDescription != `Button behavior: "redirect" opens an external link, "callback" executes the action payload.` {
		t.Fatalf("unexpected button_type description: %q", buttonTypeDescription)
	}
	valueSchema, ok := buttonProperties["value"].(map[string]any)
	if !ok {
		t.Fatalf("unexpected value schema: %#v", buttonProperties["value"])
	}
	valueDescription, ok := valueSchema["description"].(string)
	if !ok {
		t.Fatalf("unexpected value description: %#v", valueSchema["description"])
	}
	if valueDescription != `Callback action payload when button_type="callback".` {
		t.Fatalf("unexpected value description: %q", valueDescription)
	}
	mobileLinkSchema, ok := buttonProperties["mobile_link"].(map[string]any)
	if !ok {
		t.Fatalf("unexpected mobile_link schema: %#v", buttonProperties["mobile_link"])
	}
	mobileLinkDescription, ok := mobileLinkSchema["description"].(string)
	if !ok {
		t.Fatalf("unexpected mobile_link description: %#v", mobileLinkSchema["description"])
	}
	if mobileLinkDescription != `Redirect destination used on SeaTalk mobile clients when button_type="redirect".` {
		t.Fatalf("unexpected mobile_link description: %q", mobileLinkDescription)
	}
	mobileLinkProperties, ok := mobileLinkSchema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("unexpected mobile_link properties: %#v", mobileLinkSchema["properties"])
	}
	mobileLinkTypeSchema, ok := mobileLinkProperties["type"].(map[string]any)
	if !ok {
		t.Fatalf("unexpected mobile_link.type schema: %#v", mobileLinkProperties["type"])
	}
	mobileLinkTypeDescription, ok := mobileLinkTypeSchema["description"].(string)
	if !ok {
		t.Fatalf("unexpected mobile_link.type description: %#v", mobileLinkTypeSchema["description"])
	}
	if mobileLinkTypeDescription != `"rn" opens an in-app RN page; "web" opens a web URL.` {
		t.Fatalf("unexpected mobile_link.type description: %q", mobileLinkTypeDescription)
	}
	desktopLinkSchema, ok := buttonProperties["desktop_link"].(map[string]any)
	if !ok {
		t.Fatalf("unexpected desktop_link schema: %#v", buttonProperties["desktop_link"])
	}
	desktopLinkDescription, ok := desktopLinkSchema["description"].(string)
	if !ok {
		t.Fatalf("unexpected desktop_link description: %#v", desktopLinkSchema["description"])
	}
	if desktopLinkDescription != `Redirect destination used on SeaTalk desktop clients when button_type="redirect".` {
		t.Fatalf("unexpected desktop_link description: %q", desktopLinkDescription)
	}
	desktopLinkProperties, ok := desktopLinkSchema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("unexpected desktop_link properties: %#v", desktopLinkSchema["properties"])
	}
	desktopLinkTypeSchema, ok := desktopLinkProperties["type"].(map[string]any)
	if !ok {
		t.Fatalf("unexpected desktop_link.type schema: %#v", desktopLinkProperties["type"])
	}
	desktopLinkTypeDescription, ok := desktopLinkTypeSchema["description"].(string)
	if !ok {
		t.Fatalf("unexpected desktop_link.type description: %#v", desktopLinkTypeSchema["description"])
	}
	if desktopLinkTypeDescription != `Desktop redirect type. SeaTalk currently supports only "web".` {
		t.Fatalf("unexpected desktop_link.type description: %q", desktopLinkTypeDescription)
	}
	imageSchema, ok := itemProperties["image"].(map[string]any)
	if !ok {
		t.Fatalf("unexpected image schema: %#v", itemProperties["image"])
	}
	if _, ok = imageSchema["description"]; ok {
		t.Fatalf("image schema should not include description: %#v", imageSchema["description"])
	}
	imageProperties, ok := imageSchema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("unexpected image properties: %#v", imageSchema["properties"])
	}
	if _, ok = imageProperties["base64_content"]; !ok {
		t.Fatalf("image schema missing base64_content: %#v", imageProperties)
	}
	base64Schema, ok := imageProperties["base64_content"].(map[string]any)
	if !ok {
		t.Fatalf("unexpected image.base64_content schema: %#v", imageProperties["base64_content"])
	}
	if _, ok = base64Schema["description"]; ok {
		t.Fatalf("image.base64_content schema should not include description: %#v", base64Schema["description"])
	}
	if _, ok = imageProperties["local_file_path"]; ok {
		t.Fatalf("image schema should not include local_file_path: %#v", imageProperties)
	}
}

func TestSeaTalkSendFileToolInputSchemaOmitsParameterDescriptions(t *testing.T) {
	t.Parallel()

	schema, ok := seaTalkSendFileTool{}.InputSchema().(map[string]any)
	if !ok {
		t.Fatalf("unexpected schema type: %T", seaTalkSendFileTool{}.InputSchema())
	}
	properties, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("unexpected schema properties: %#v", schema["properties"])
	}

	localFilePath, ok := properties["local_file_path"].(map[string]any)
	if !ok {
		t.Fatalf("unexpected local_file_path schema: %#v", properties["local_file_path"])
	}
	if _, ok = localFilePath["description"]; ok {
		t.Fatalf("local_file_path schema should not include description: %#v", localFilePath["description"])
	}

	filename, ok := properties["filename"].(map[string]any)
	if !ok {
		t.Fatalf("unexpected filename schema: %#v", properties["filename"])
	}
	if _, ok = filename["description"]; ok {
		t.Fatalf("filename schema should not include description: %#v", filename["description"])
	}
}

func TestSeaTalkPushInteractiveMessageToolRequiresMode(t *testing.T) {
	t.Parallel()

	tool := seaTalkPushInteractiveMessageTool{}
	ctx := agent.ContextWithTurnRequest(context.Background(), agent.TurnRequest{
		Conversation: agent.ConversationState{Key: "seatalk:private:e_1:thread-1"},
		Message: agent.InboundMessage{
			Responder: &SeaTalkResponder{
				target: seaTalkReplyTarget{
					employeeCode: "e_1",
					threadID:     "thread-1",
				},
			},
		},
	})

	_, err := tool.Call(ctx, json.RawMessage(`{
		"elements": [
			{"element_type":"title","title":{"text":"Approved"}}
		]
	}`))
	if err == nil {
		t.Fatal("expected tool call to fail")
	}
	if !strings.Contains(err.Error(), `mode is required and must be "send" or "update"`) {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestSeaTalkPushInteractiveMessageToolUpdateModeDefaultsToCurrentInteractiveMessage(t *testing.T) {
	t.Parallel()

	client := seatalk.NewClient(
		seatalk.Config{AppID: "app-id", AppSecret: "app-secret"},
		seatalk.WithHTTPClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.Method != http.MethodPost {
				t.Fatalf("unexpected method: %s", req.Method)
			}
			if req.URL.Path != "/messaging/v2/update" {
				t.Fatalf("unexpected path: %s", req.URL.Path)
			}

			var body map[string]any
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				t.Fatalf("decode request failed: %v", err)
			}
			if body["message_id"] != "interactive-msg-clicked" {
				t.Fatalf("unexpected message id: %#v", body["message_id"])
			}

			return jsonResponse(t, map[string]any{
				"code": 0,
			}), nil
		})}),
		seatalk.WithTokenProvider(func(context.Context, *http.Client, string, string) (string, error) {
			return "token-123", nil
		}),
	)

	tool := seaTalkPushInteractiveMessageTool{}
	ctx := agent.ContextWithTurnRequest(context.Background(), agent.TurnRequest{
		Conversation: agent.ConversationState{Key: "seatalk:private:e_1:thread-1"},
		Message: agent.InboundMessage{
			Responder: &SeaTalkResponder{
				client:             client,
				interactiveMessage: "interactive-msg-clicked",
				target: seaTalkReplyTarget{
					employeeCode: "e_1",
					threadID:     "thread-1",
				},
			},
		},
	})

	result, err := tool.Call(ctx, json.RawMessage(`{
		"mode": "update",
		"elements": [
			{"element_type":"title","title":{"text":"Approved"}}
		]
	}`))
	if err != nil {
		t.Fatalf("tool call failed: %v", err)
	}

	body, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("unexpected tool result type: %T", result)
	}
	if body["message_id"] != "interactive-msg-clicked" {
		t.Fatalf("unexpected message id: %#v", body["message_id"])
	}
}

func TestSeaTalkPushInteractiveMessageToolSendModeIgnoresCurrentInteractiveMessage(t *testing.T) {
	t.Parallel()

	client := seatalk.NewClient(
		seatalk.Config{AppID: "app-id", AppSecret: "app-secret"},
		seatalk.WithHTTPClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.Method != http.MethodPost {
				t.Fatalf("unexpected method: %s", req.Method)
			}
			if req.URL.Path != "/messaging/v2/single_chat" {
				t.Fatalf("unexpected path: %s", req.URL.Path)
			}

			var body map[string]any
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				t.Fatalf("decode request failed: %v", err)
			}
			if _, ok := body["message_id"]; ok {
				t.Fatalf("did not expect update payload: %#v", body)
			}

			return jsonResponse(t, map[string]any{
				"code":       0,
				"message_id": "interactive-msg-new-send-mode",
			}), nil
		})}),
		seatalk.WithTokenProvider(func(context.Context, *http.Client, string, string) (string, error) {
			return "token-123", nil
		}),
	)

	tool := seaTalkPushInteractiveMessageTool{}
	ctx := agent.ContextWithTurnRequest(context.Background(), agent.TurnRequest{
		Conversation: agent.ConversationState{Key: "seatalk:private:e_1:thread-1"},
		Message: agent.InboundMessage{
			Responder: &SeaTalkResponder{
				client:             client,
				interactiveMessage: "interactive-msg-clicked",
				target: seaTalkReplyTarget{
					employeeCode: "e_1",
					threadID:     "thread-1",
				},
			},
		},
	})

	result, err := tool.Call(ctx, json.RawMessage(`{
		"mode": "send",
		"elements": [
			{"element_type":"title","title":{"text":"Create another card"}}
		]
	}`))
	if err != nil {
		t.Fatalf("tool call failed: %v", err)
	}

	body, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("unexpected tool result type: %T", result)
	}
	if body["message_id"] != "interactive-msg-new-send-mode" {
		t.Fatalf("unexpected message id: %#v", body["message_id"])
	}
}

func TestSeaTalkPushInteractiveMessageToolSendsWhenNoMessageIDOrCurrentInteractiveMessage(t *testing.T) {
	t.Parallel()

	client := seatalk.NewClient(
		seatalk.Config{AppID: "app-id", AppSecret: "app-secret"},
		seatalk.WithHTTPClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.Method != http.MethodPost {
				t.Fatalf("unexpected method: %s", req.Method)
			}
			if req.URL.Path != "/messaging/v2/single_chat" {
				t.Fatalf("unexpected path: %s", req.URL.Path)
			}

			var body map[string]any
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				t.Fatalf("decode request failed: %v", err)
			}
			if _, ok := body["message_id"]; ok {
				t.Fatalf("did not expect update payload: %#v", body)
			}

			return jsonResponse(t, map[string]any{
				"code":       0,
				"message_id": "interactive-msg-new",
			}), nil
		})}),
		seatalk.WithTokenProvider(func(context.Context, *http.Client, string, string) (string, error) {
			return "token-123", nil
		}),
	)

	tool := seaTalkPushInteractiveMessageTool{}
	ctx := agent.ContextWithTurnRequest(context.Background(), agent.TurnRequest{
		Conversation: agent.ConversationState{Key: "seatalk:private:e_1:thread-1"},
		Message: agent.InboundMessage{
			Responder: &SeaTalkResponder{
				client: client,
				target: seaTalkReplyTarget{
					employeeCode: "e_1",
					threadID:     "thread-1",
				},
			},
		},
	})

	result, err := tool.Call(ctx, json.RawMessage(`{
		"mode": "send",
		"elements": [
			{"element_type":"title","title":{"text":"Approved"}}
		]
	}`))
	if err != nil {
		t.Fatalf("tool call failed: %v", err)
	}

	body, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("unexpected tool result type: %T", result)
	}
	if body["message_id"] != "interactive-msg-new" {
		t.Fatalf("unexpected message id: %#v", body["message_id"])
	}
}

func TestSeaTalkSendFileToolUsesCurrentGroupConversationTarget(t *testing.T) {
	t.Parallel()

	filePath := filepath.Join(t.TempDir(), "report.csv")
	if err := os.WriteFile(filePath, []byte("name,value\nfoo,1\n"), 0o600); err != nil {
		t.Fatalf("write file failed: %v", err)
	}

	client := seatalk.NewClient(
		seatalk.Config{AppID: "app-id", AppSecret: "app-secret"},
		seatalk.WithHTTPClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.Method != http.MethodPost {
				t.Fatalf("unexpected method: %s", req.Method)
			}
			if req.URL.Path != "/messaging/v2/group_chat" {
				t.Fatalf("unexpected path: %s", req.URL.Path)
			}

			var body struct {
				GroupID string `json:"group_id"`
				Message struct {
					ThreadID string `json:"thread_id"`
					Tag      string `json:"tag"`
					File     struct {
						Filename string `json:"filename"`
						Content  string `json:"content"`
					} `json:"file"`
				} `json:"message"`
			}
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				t.Fatalf("decode request failed: %v", err)
			}
			if body.GroupID != "group-1" {
				t.Fatalf("unexpected group id: %q", body.GroupID)
			}
			if body.Message.ThreadID != "thread-1" {
				t.Fatalf("unexpected thread id: %q", body.Message.ThreadID)
			}
			if body.Message.Tag != "file" {
				t.Fatalf("unexpected tag: %q", body.Message.Tag)
			}
			if body.Message.File.Filename != "custom-report.csv" {
				t.Fatalf("unexpected filename: %q", body.Message.File.Filename)
			}
			if body.Message.File.Content == "" {
				t.Fatal("expected base64 content")
			}

			return jsonResponse(t, map[string]any{
				"code":       0,
				"message_id": "file-msg-1",
			}), nil
		})}),
		seatalk.WithTokenProvider(func(context.Context, *http.Client, string, string) (string, error) {
			return "token-123", nil
		}),
	)

	tool := seaTalkSendFileTool{}
	ctx := agent.ContextWithTurnRequest(context.Background(), agent.TurnRequest{
		Conversation: agent.ConversationState{Key: "seatalk:group:group-1:thread-1"},
		Message: agent.InboundMessage{
			Responder: &SeaTalkResponder{
				client: client,
				target: seaTalkReplyTarget{
					isGroup:  true,
					groupID:  "group-1",
					threadID: "thread-1",
				},
			},
		},
	})

	result, err := tool.Call(ctx, json.RawMessage(`{
		"local_file_path": "`+filePath+`",
		"filename": "custom-report.csv"
	}`))
	if err != nil {
		t.Fatalf("tool call failed: %v", err)
	}

	body, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("unexpected tool result type: %T", result)
	}
	if body["message_id"] != "file-msg-1" {
		t.Fatalf("unexpected message id: %#v", body["message_id"])
	}
	if body["filename"] != "custom-report.csv" {
		t.Fatalf("unexpected filename: %#v", body["filename"])
	}
}

func TestSeaTalkGetMessageToolRetrievesMessageByID(t *testing.T) {
	t.Parallel()

	client := seatalk.NewClient(
		seatalk.Config{AppID: "app-id", AppSecret: "app-secret"},
		seatalk.WithHTTPClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.Method != http.MethodGet {
				t.Fatalf("unexpected method: %s", req.Method)
			}
			if req.URL.Path != "/messaging/v2/get_message_by_message_id" {
				t.Fatalf("unexpected path: %s", req.URL.Path)
			}
			if req.URL.Query().Get("message_id") != "message-1" {
				t.Fatalf("unexpected message id: %q", req.URL.Query().Get("message_id"))
			}
			if got := req.Header.Get("Authorization"); got != "Bearer token-123" {
				t.Fatalf("unexpected authorization header: %s", got)
			}

			return jsonResponse(t, map[string]any{
				"code":              0,
				"message_id":        "message-1",
				"quoted_message_id": "quoted-1",
				"thread_id":         "thread-1",
				"sender": map[string]any{
					"seatalk_id":    "seatalk-user-1",
					"employee_code": "emp-1",
					"email":         "alice@example.com",
					"sender_type":   1,
				},
				"message_sent_time": 1700000000,
				"tag":               "text",
				"text": map[string]any{
					"plain_text":       "hello",
					"last_edited_time": 1700000010,
				},
			}), nil
		})}),
		seatalk.WithTokenProvider(func(context.Context, *http.Client, string, string) (string, error) {
			return "token-123", nil
		}),
	)

	tool := seaTalkGetMessageTool{}
	ctx := agent.ContextWithTurnRequest(context.Background(), agent.TurnRequest{
		Conversation: agent.ConversationState{Key: "seatalk:group:group-1:thread-1"},
		Message: agent.InboundMessage{
			Responder: &SeaTalkResponder{
				client: client,
				target: seaTalkReplyTarget{
					isGroup:  true,
					groupID:  "group-1",
					threadID: "thread-1",
				},
			},
		},
	})

	result, err := tool.Call(ctx, json.RawMessage(`{"message_id":"message-1"}`))
	if err != nil {
		t.Fatalf("tool call failed: %v", err)
	}

	message, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("unexpected tool result type: %T", result)
	}
	if message["message_id"] != "message-1" {
		t.Fatalf("unexpected message id: %q", message["message_id"])
	}
	if message["quoted_message_id"] != "quoted-1" {
		t.Fatalf("unexpected quoted message id: %q", message["quoted_message_id"])
	}
	text, ok := message["text"].(map[string]any)
	if !ok || text["plain_text"] != "hello" {
		t.Fatalf("unexpected text payload: %+v", message["text"])
	}
	if _, ok = message["image"]; ok {
		t.Fatalf("unexpected nil image field: %+v", message["image"])
	}
}

func TestSeaTalkGetThreadToolRetrievesGroupThreadByID(t *testing.T) {
	t.Parallel()

	client := seatalk.NewClient(
		seatalk.Config{AppID: "app-id", AppSecret: "app-secret"},
		seatalk.WithHTTPClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.Method != http.MethodGet {
				t.Fatalf("unexpected method: %s", req.Method)
			}
			if req.URL.Path != "/messaging/v2/group_chat/get_thread_by_thread_id" {
				t.Fatalf("unexpected path: %s", req.URL.Path)
			}
			query := req.URL.Query()
			if query.Get("group_id") != "group-1" {
				t.Fatalf("unexpected group id: %q", query.Get("group_id"))
			}
			if query.Get("thread_id") != "thread-1" {
				t.Fatalf("unexpected thread id: %q", query.Get("thread_id"))
			}
			if query.Get("page_size") != "10" {
				t.Fatalf("unexpected page size: %q", query.Get("page_size"))
			}
			if query.Get("cursor") != "cursor-1" {
				t.Fatalf("unexpected cursor: %q", query.Get("cursor"))
			}
			if got := req.Header.Get("Authorization"); got != "Bearer token-123" {
				t.Fatalf("unexpected authorization header: %s", got)
			}

			return jsonResponse(t, map[string]any{
				"code":        0,
				"next_cursor": "cursor-2",
				"thread_messages": []map[string]any{
					{
						"message_id":        "message-1",
						"quoted_message_id": "",
						"thread_id":         "thread-1",
						"sender": map[string]any{
							"seatalk_id":    "seatalk-user-1",
							"employee_code": "emp-1",
							"email":         "alice@example.com",
							"sender_type":   1,
						},
						"message_sent_time": 1700000000,
						"tag":               "text",
						"text": map[string]any{
							"plain_text": "hello",
						},
					},
				},
			}), nil
		})}),
		seatalk.WithTokenProvider(func(context.Context, *http.Client, string, string) (string, error) {
			return "token-123", nil
		}),
	)

	tool := seaTalkGetThreadTool{}
	ctx := agent.ContextWithTurnRequest(context.Background(), agent.TurnRequest{
		Conversation: agent.ConversationState{Key: "seatalk:group:group-1:thread-1"},
		Message: agent.InboundMessage{
			Responder: &SeaTalkResponder{
				client: client,
				target: seaTalkReplyTarget{
					employeeCode: "other-employee",
					threadID:     "other-thread",
				},
			},
		},
	})

	result, err := tool.Call(ctx, json.RawMessage(`{"target_type":"group","group_id":"group-1","thread_id":"thread-1","page_size":10,"cursor":"cursor-1"}`))
	if err != nil {
		t.Fatalf("tool call failed: %v", err)
	}

	thread, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("unexpected tool result type: %T", result)
	}
	if thread["next_cursor"] != "cursor-2" {
		t.Fatalf("unexpected next cursor: %q", thread["next_cursor"])
	}
	messages, ok := thread["thread_messages"].([]any)
	if !ok || len(messages) != 1 {
		t.Fatalf("unexpected thread messages: %+v", thread["thread_messages"])
	}
	message, ok := messages[0].(map[string]any)
	if !ok || message["message_id"] != "message-1" {
		t.Fatalf("unexpected message payload: %+v", messages[0])
	}
}

func TestSeaTalkGetThreadToolRetrievesPrivateThreadByTarget(t *testing.T) {
	t.Parallel()

	client := seatalk.NewClient(
		seatalk.Config{AppID: "app-id", AppSecret: "app-secret"},
		seatalk.WithHTTPClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.Method != http.MethodGet {
				t.Fatalf("unexpected method: %s", req.Method)
			}
			if req.URL.Path != "/messaging/v2/single_chat/get_thread_by_thread_id" {
				t.Fatalf("unexpected path: %s", req.URL.Path)
			}
			query := req.URL.Query()
			if query.Get("employee_code") != "e_1" {
				t.Fatalf("unexpected employee code: %q", query.Get("employee_code"))
			}
			if query.Get("thread_id") != "thread-override" {
				t.Fatalf("unexpected thread id: %q", query.Get("thread_id"))
			}

			return jsonResponse(t, map[string]any{
				"code":            0,
				"thread_messages": []map[string]any{},
			}), nil
		})}),
		seatalk.WithTokenProvider(func(context.Context, *http.Client, string, string) (string, error) {
			return "token-123", nil
		}),
	)

	tool := seaTalkGetThreadTool{}
	ctx := agent.ContextWithTurnRequest(context.Background(), agent.TurnRequest{
		Conversation: agent.ConversationState{Key: "seatalk:private:e_1:thread-1"},
		Message: agent.InboundMessage{
			Responder: &SeaTalkResponder{
				client: client,
				target: seaTalkReplyTarget{
					isGroup:  true,
					groupID:  "other-group",
					threadID: "other-thread",
				},
			},
		},
	})

	result, err := tool.Call(ctx, json.RawMessage(`{"target_type":"private","employee_code":"e_1","thread_id":"thread-override"}`))
	if err != nil {
		t.Fatalf("tool call failed: %v", err)
	}

	thread, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("unexpected tool result type: %T", result)
	}
	messages, ok := thread["thread_messages"].([]any)
	if !ok || len(messages) != 0 {
		t.Fatalf("unexpected thread messages: %+v", thread["thread_messages"])
	}
}

func TestSeaTalkGetThreadToolRequiresThreadID(t *testing.T) {
	t.Parallel()

	client := seatalk.NewClient(
		seatalk.Config{AppID: "app-id", AppSecret: "app-secret"},
		seatalk.WithHTTPClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			t.Fatalf("unexpected request: %s %s", req.Method, req.URL.String())
			return nil, nil
		})}),
		seatalk.WithTokenProvider(func(context.Context, *http.Client, string, string) (string, error) {
			t.Fatal("token provider should not be called")
			return "", nil
		}),
	)

	tool := seaTalkGetThreadTool{}
	ctx := agent.ContextWithTurnRequest(context.Background(), agent.TurnRequest{
		Conversation: agent.ConversationState{Key: "seatalk:group:group-1:thread-1"},
		Message: agent.InboundMessage{
			Responder: &SeaTalkResponder{
				client: client,
				target: seaTalkReplyTarget{
					isGroup:  true,
					groupID:  "group-1",
					threadID: "thread-1",
				},
			},
		},
	})

	_, err := tool.Call(ctx, json.RawMessage(`{"target_type":"group","group_id":"group-1"}`))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "thread_id is empty") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestSeaTalkSendFileToolUsesCurrentPrivateConversationTarget(t *testing.T) {
	t.Parallel()

	filePath := filepath.Join(t.TempDir(), "artifact.json")
	if err := os.WriteFile(filePath, []byte(`{"ok":true}`), 0o600); err != nil {
		t.Fatalf("write file failed: %v", err)
	}

	client := seatalk.NewClient(
		seatalk.Config{AppID: "app-id", AppSecret: "app-secret"},
		seatalk.WithHTTPClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.Method != http.MethodPost {
				t.Fatalf("unexpected method: %s", req.Method)
			}
			if req.URL.Path != "/messaging/v2/single_chat" {
				t.Fatalf("unexpected path: %s", req.URL.Path)
			}

			var body struct {
				EmployeeCode   string `json:"employee_code"`
				UsablePlatform string `json:"usable_platform"`
				Message        struct {
					ThreadID string `json:"thread_id"`
					Tag      string `json:"tag"`
					File     struct {
						Filename string `json:"filename"`
						Content  string `json:"content"`
					} `json:"file"`
				} `json:"message"`
			}
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				t.Fatalf("decode request failed: %v", err)
			}
			if body.EmployeeCode != "e_1" {
				t.Fatalf("unexpected employee code: %q", body.EmployeeCode)
			}
			if body.UsablePlatform != seatalk.UsablePlatformAll {
				t.Fatalf("unexpected usable_platform: %q", body.UsablePlatform)
			}
			if body.Message.ThreadID != "thread-1" {
				t.Fatalf("unexpected thread id: %q", body.Message.ThreadID)
			}
			if body.Message.File.Filename != "artifact.json" {
				t.Fatalf("unexpected filename: %q", body.Message.File.Filename)
			}

			return jsonResponse(t, map[string]any{
				"code":       0,
				"message_id": "file-msg-2",
			}), nil
		})}),
		seatalk.WithTokenProvider(func(context.Context, *http.Client, string, string) (string, error) {
			return "token-123", nil
		}),
	)

	tool := seaTalkSendFileTool{}
	ctx := agent.ContextWithTurnRequest(context.Background(), agent.TurnRequest{
		Conversation: agent.ConversationState{Key: "seatalk:private:e_1:thread-1"},
		Message: agent.InboundMessage{
			Responder: &SeaTalkResponder{
				client: client,
				target: seaTalkReplyTarget{
					employeeCode: "e_1",
					threadID:     "thread-1",
				},
			},
		},
	})

	result, err := tool.Call(ctx, json.RawMessage(`{
		"local_file_path": "`+filePath+`"
	}`))
	if err != nil {
		t.Fatalf("tool call failed: %v", err)
	}

	body, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("unexpected tool result type: %T", result)
	}
	if body["message_id"] != "file-msg-2" {
		t.Fatalf("unexpected message id: %#v", body["message_id"])
	}
	if body["filename"] != "artifact.json" {
		t.Fatalf("unexpected filename: %#v", body["filename"])
	}
}

func TestSeaTalkSendFileToolAllowsFileAtBase64SizeLimit(t *testing.T) {
	t.Parallel()

	rawSize := seaTalkFileBase64MaxBytes / 4 * 3
	filePath := filepath.Join(t.TempDir(), "limit.bin")
	if err := os.WriteFile(filePath, bytes.Repeat([]byte{'a'}, rawSize), 0o600); err != nil {
		t.Fatalf("write file failed: %v", err)
	}

	client := seatalk.NewClient(
		seatalk.Config{AppID: "app-id", AppSecret: "app-secret"},
		seatalk.WithHTTPClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.URL.Path != "/messaging/v2/single_chat" {
				t.Fatalf("unexpected path: %s", req.URL.Path)
			}
			return jsonResponse(t, map[string]any{
				"code":       0,
				"message_id": "file-msg-limit",
			}), nil
		})}),
		seatalk.WithTokenProvider(func(context.Context, *http.Client, string, string) (string, error) {
			return "token-123", nil
		}),
	)

	tool := seaTalkSendFileTool{}
	ctx := agent.ContextWithTurnRequest(context.Background(), agent.TurnRequest{
		Conversation: agent.ConversationState{Key: "seatalk:private:e_1:thread-1"},
		Message: agent.InboundMessage{
			Responder: &SeaTalkResponder{
				client: client,
				target: seaTalkReplyTarget{
					employeeCode: "e_1",
					threadID:     "thread-1",
				},
			},
		},
	})

	_, err := tool.Call(ctx, json.RawMessage(`{
		"local_file_path": "`+filePath+`"
	}`))
	if err != nil {
		t.Fatalf("tool call failed: %v", err)
	}
}

func TestSeaTalkSendFileToolRejectsFileExceedingBase64SizeLimit(t *testing.T) {
	t.Parallel()

	rawSize := seaTalkFileBase64MaxBytes/4*3 + 1
	filePath := filepath.Join(t.TempDir(), "too-large.bin")
	if err := os.WriteFile(filePath, bytes.Repeat([]byte{'a'}, rawSize), 0o600); err != nil {
		t.Fatalf("write file failed: %v", err)
	}

	client := seatalk.NewClient(
		seatalk.Config{AppID: "app-id", AppSecret: "app-secret"},
		seatalk.WithHTTPClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			t.Fatalf("unexpected request: %s %s", req.Method, req.URL.Path)
			return nil, nil
		})}),
		seatalk.WithTokenProvider(func(context.Context, *http.Client, string, string) (string, error) {
			t.Fatalf("token provider should not be called")
			return "", nil
		}),
	)

	tool := seaTalkSendFileTool{}
	ctx := agent.ContextWithTurnRequest(context.Background(), agent.TurnRequest{
		Conversation: agent.ConversationState{Key: "seatalk:private:e_1:thread-1"},
		Message: agent.InboundMessage{
			Responder: &SeaTalkResponder{
				client: client,
				target: seaTalkReplyTarget{
					employeeCode: "e_1",
					threadID:     "thread-1",
				},
			},
		},
	})

	_, err := tool.Call(ctx, json.RawMessage(`{
		"local_file_path": "`+filePath+`"
	}`))
	if err == nil {
		t.Fatal("expected file size validation error")
	}

	encodedSize := base64.StdEncoding.EncodedLen(rawSize)
	if !strings.Contains(err.Error(), "base64-encoded file content exceeds 5M limit") {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("got %d bytes", encodedSize)) {
		t.Fatalf("unexpected error size: %v", err)
	}
}

func TestSeaTalkPushInteractiveMessageToolUpdatesWhenMessageIDProvided(t *testing.T) {
	t.Parallel()

	client := seatalk.NewClient(
		seatalk.Config{AppID: "app-id", AppSecret: "app-secret"},
		seatalk.WithHTTPClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.Method != http.MethodPost {
				t.Fatalf("unexpected method: %s", req.Method)
			}
			if req.URL.Path != "/messaging/v2/update" {
				t.Fatalf("unexpected path: %s", req.URL.Path)
			}

			var body map[string]any
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				t.Fatalf("decode request failed: %v", err)
			}
			if body["message_id"] != "interactive-msg-1" {
				t.Fatalf("unexpected message id: %#v", body["message_id"])
			}

			return jsonResponse(t, map[string]any{
				"code": 0,
			}), nil
		})}),
		seatalk.WithTokenProvider(func(context.Context, *http.Client, string, string) (string, error) {
			return "token-123", nil
		}),
	)

	tool := seaTalkPushInteractiveMessageTool{}
	ctx := agent.ContextWithTurnRequest(context.Background(), agent.TurnRequest{
		Conversation: agent.ConversationState{Key: "seatalk:private:e_1:thread-1"},
		Message: agent.InboundMessage{
			Responder: &SeaTalkResponder{
				client:             client,
				interactiveMessage: "interactive-msg-1",
				target: seaTalkReplyTarget{
					employeeCode: "e_1",
					threadID:     "thread-1",
				},
			},
		},
	})

	result, err := tool.Call(ctx, json.RawMessage(`{
		"mode": "update",
		"message_id": "interactive-msg-1",
		"elements": [
			{"element_type":"title","title":{"text":"Approved"}}
		]
	}`))
	if err != nil {
		t.Fatalf("tool call failed: %v", err)
	}

	body, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("unexpected tool result type: %T", result)
	}
	if body["message_id"] != "interactive-msg-1" {
		t.Fatalf("unexpected message id: %#v", body["message_id"])
	}
}

func TestSeaTalkPushInteractiveMessageToolUpdateModeFailsWithoutTarget(t *testing.T) {
	t.Parallel()

	tool := seaTalkPushInteractiveMessageTool{}
	ctx := agent.ContextWithTurnRequest(context.Background(), agent.TurnRequest{
		Conversation: agent.ConversationState{Key: "seatalk:private:e_1:thread-1"},
		Message: agent.InboundMessage{
			Responder: &SeaTalkResponder{
				target: seaTalkReplyTarget{
					employeeCode: "e_1",
					threadID:     "thread-1",
				},
			},
		},
	})

	_, err := tool.Call(ctx, json.RawMessage(`{
		"mode": "update",
		"elements": [
			{"element_type":"title","title":{"text":"Approved"}}
		]
	}`))
	if err == nil {
		t.Fatal("expected tool call to fail")
	}
	if !strings.Contains(err.Error(), "update mode requires message_id or current interactive message context") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestSeaTalkInteractiveSendToolRequiresImageBase64Content(t *testing.T) {
	t.Parallel()

	tool := seaTalkPushInteractiveMessageTool{}
	ctx := agent.ContextWithTurnRequest(context.Background(), agent.TurnRequest{
		Conversation: agent.ConversationState{Key: "seatalk:group:group-1:thread-1"},
		Message: agent.InboundMessage{
			Responder: &SeaTalkResponder{
				target: seaTalkReplyTarget{
					isGroup:  true,
					groupID:  "group-1",
					threadID: "thread-1",
				},
			},
		},
	})

	_, err := tool.Call(ctx, json.RawMessage(`{
		"mode": "send",
		"elements": [
			{"element_type":"title","title":{"text":"Card title"}},
			{"element_type":"image","image":{}}
		]
	}`))
	if err == nil {
		t.Fatal("expected missing image base64 content to fail")
	}
	if !strings.Contains(err.Error(), "image.base64_content is empty") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestSeaTalkInteractiveSendToolRejectsOversizedBase64ImageContent(t *testing.T) {
	t.Parallel()

	rawSize := seaTalkFileBase64MaxBytes/4*3 + 1
	encoded := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{'a'}, rawSize))

	tool := seaTalkPushInteractiveMessageTool{}
	ctx := agent.ContextWithTurnRequest(context.Background(), agent.TurnRequest{
		Conversation: agent.ConversationState{Key: "seatalk:group:group-1:thread-1"},
		Message: agent.InboundMessage{
			Responder: &SeaTalkResponder{
				target: seaTalkReplyTarget{
					isGroup:  true,
					groupID:  "group-1",
					threadID: "thread-1",
				},
			},
		},
	})

	_, err := tool.Call(ctx, json.RawMessage(`{
		"mode": "send",
		"elements": [
			{"element_type":"title","title":{"text":"Card title"}},
			{"element_type":"image","image":{"base64_content":"`+encoded+`"}}
		]
	}`))
	if err == nil {
		t.Fatal("expected oversized image base64 content to fail")
	}
	encodedSize := base64.StdEncoding.EncodedLen(rawSize)
	if !strings.Contains(err.Error(), "base64-encoded image content exceeds 5M limit") {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("got %d bytes", encodedSize)) {
		t.Fatalf("unexpected error size: %v", err)
	}
}
