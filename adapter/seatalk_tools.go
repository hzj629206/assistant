package adapter

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hzj629206/assistant/agent"
	"github.com/hzj629206/assistant/cache"
	"github.com/hzj629206/assistant/seatalk"
)

type seaTalkGetEmployeeInfoTool struct {
	client *seatalk.Client
}

type seaTalkSendFileTool struct{}

type seaTalkSendInteractiveMessageTool struct{}

type seaTalkUpdateInteractiveMessageTool struct{}

type interactiveToolPayload struct {
	MessageID string                    `json:"message_id,omitempty"`
	Elements  []interactiveElementInput `json:"elements"`
}

type interactiveElementInput struct {
	ElementType string                   `json:"element_type"`
	Title       *interactiveTitleInput   `json:"title,omitempty"`
	Description *interactiveTextInput    `json:"description,omitempty"`
	Button      *interactiveButtonInput  `json:"button,omitempty"`
	ButtonGroup []interactiveButtonInput `json:"button_group,omitempty"`
	Image       *interactiveImageInput   `json:"image,omitempty"`
}

type interactiveTitleInput struct {
	Text string `json:"text"`
}

type interactiveTextInput struct {
	Text   string `json:"text"`
	Format int    `json:"format,omitempty"`
}

type interactiveButtonInput struct {
	Type        string                          `json:"button_type"`
	Text        string                          `json:"text"`
	Value       string                          `json:"value,omitempty"`
	MobileLink  *seatalk.InteractiveMobileLink  `json:"mobile_link,omitempty"`
	DesktopLink *seatalk.InteractiveDesktopLink `json:"desktop_link,omitempty"`
}

type interactiveImageInput struct {
	Base64Content string `json:"base64_content"`
	LocalFilePath string `json:"local_file_path,omitempty"`
}

const (
	interactiveCallbackValueMaxLength = 200
	interactiveCallbackValueRefPrefix = "stcb:"
	interactiveCallbackValueTTL       = 30 * 24 * time.Hour
)

func (t seaTalkGetEmployeeInfoTool) Name() string {
	return "seatalk_get_employee_info"
}

func (t seaTalkGetEmployeeInfoTool) Description() string {
	return "Look up SeaTalk employee profiles by employee_code."
}

func (t seaTalkGetEmployeeInfoTool) InputSchema() any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"employee_codes": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "string",
				},
				"minItems": 1,
				"maxItems": 20,
			},
		},
		"required":             []any{"employee_codes"},
		"additionalProperties": false,
	}
}

func (t seaTalkGetEmployeeInfoTool) OutputSchema() any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"employees": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"employee_code":         map[string]any{"type": "string"},
						"email":                 map[string]any{"type": "string"},
						"phone":                 map[string]any{"type": "string"},
						"departments":           map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
						"manager_employee_code": map[string]any{"type": "string"},
					},
					"required": []any{
						"employee_code",
						"email",
						"phone",
						"departments",
						"manager_employee_code",
					},
					"additionalProperties": false,
				},
			},
		},
		"required":             []any{"employees"},
		"additionalProperties": false,
	}
}

func (t seaTalkGetEmployeeInfoTool) Call(ctx context.Context, input json.RawMessage) (any, error) {
	if t.client == nil {
		return nil, errors.New("seatalk get employee info tool failed: client is nil")
	}

	var payload struct {
		EmployeeCodes []string `json:"employee_codes"`
	}
	if err := json.Unmarshal(input, &payload); err != nil {
		return nil, fmt.Errorf("seatalk get employee info tool failed: decode input: %w", err)
	}
	if len(payload.EmployeeCodes) == 0 {
		return nil, errors.New("seatalk get employee info tool failed: employee codes are empty")
	}

	log.Printf("seatalk tool get_employee_info: employee_codes=%v", payload.EmployeeCodes)
	result, err := t.client.GetEmployeeInfo(ctx, payload.EmployeeCodes...)
	if err != nil {
		return nil, fmt.Errorf("seatalk get employee info tool failed: %w", err)
	}

	employees := make([]map[string]any, 0, len(result.Employees))
	for _, employee := range result.Employees {
		employees = append(employees, map[string]any{
			"employee_code":         strings.TrimSpace(employee.EmployeeCode),
			"email":                 fallbackValue(strings.TrimSpace(employee.Email)),
			"phone":                 fallbackValue(strings.TrimSpace(employee.Mobile)),
			"departments":           sanitizedStrings(employee.Departments),
			"manager_employee_code": normalizeManagerCode(employee.ReportingManagerEmployeeCode),
		})
	}

	return map[string]any{"employees": employees}, nil
}

func (seaTalkSendFileTool) Name() string {
	return "seatalk_send_file"
}

func (seaTalkSendFileTool) Description() string {
	return strings.TrimSpace(`
Send a local file from the current machine into the current SeaTalk conversation.
Use this when you generated or updated a data artifact such as a CSV, JSON, log bundle, or report file and the user should receive the file itself.
`)
}

func (seaTalkSendFileTool) InputSchema() any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"local_file_path": map[string]any{"type": "string"},
			"filename":        map[string]any{"type": "string"},
		},
		"required":             []any{"local_file_path"},
		"additionalProperties": false,
	}
}

func (seaTalkSendFileTool) OutputSchema() any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"message_id": map[string]any{"type": "string"},
			"filename":   map[string]any{"type": "string"},
		},
		"required":             []any{"message_id", "filename"},
		"additionalProperties": false,
	}
}

func (seaTalkSendFileTool) Call(ctx context.Context, input json.RawMessage) (any, error) {
	responder, turnReq, err := seaTalkResponderFromToolContext(ctx)
	if err != nil {
		return nil, err
	}

	var payload struct {
		LocalFilePath string `json:"local_file_path"`
		Filename      string `json:"filename"`
	}
	if err := json.Unmarshal(input, &payload); err != nil {
		return nil, fmt.Errorf("seatalk send file tool failed: decode input: %w", err)
	}

	path := filepath.Clean(strings.TrimSpace(payload.LocalFilePath))
	if path == "" || path == "." {
		return nil, errors.New("seatalk send file tool failed: local_file_path is empty")
	}

	content, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("seatalk send file tool failed: read local file: %w", err)
	}
	if len(content) == 0 {
		return nil, errors.New("seatalk send file tool failed: local file is empty")
	}

	filename := strings.TrimSpace(payload.Filename)
	if filename == "" {
		filename = strings.TrimSpace(filepath.Base(path))
	}
	if filename == "" || filename == "." || filename == string(filepath.Separator) {
		return nil, errors.New("seatalk send file tool failed: filename is empty")
	}

	log.Printf(
		"seatalk tool send_file: conversation=%s target=%s local_file_path=%s filename=%s bytes=%d",
		turnReq.Conversation.Key,
		responder.target.logValue(),
		path,
		filename,
		len(content),
	)

	var result seatalk.SendMessageResult
	if responder.target.isGroup {
		result, err = responder.client.SendGroupFile(ctx, responder.target.groupID, seatalk.FileMessage{
			Filename: filename,
			Content:  content,
		}, seatalk.SendOptions{
			ThreadID: responder.target.threadID,
		})
	} else {
		result, err = responder.client.SendPrivateFile(ctx, responder.target.employeeCode, seatalk.FileMessage{
			Filename: filename,
			Content:  content,
		}, seatalk.PrivateSendOptions{
			ThreadID:       responder.target.threadID,
			UsablePlatform: seatalk.UsablePlatformAll,
		})
	}
	if err != nil {
		return nil, fmt.Errorf("seatalk send file tool failed: %w", err)
	}

	return map[string]any{
		"message_id": result.MessageID,
		"filename":   filename,
	}, nil
}

func (seaTalkSendInteractiveMessageTool) Name() string {
	return "seatalk_send_interactive_message"
}

func (seaTalkSendInteractiveMessageTool) Description() string {
	return strings.TrimSpace(`
Send a SeaTalk interactive message card into the current conversation, including rich content cards without any action buttons.
Use this when the user needs explicit choices, confirmation, approval, or clear status updates for important events and progress.
For callback buttons, set "value" to a JSON-encoded callback action payload serialized into a string.
Supported callback action payloads:
- Tool call: {"action":"tool_call","tool_name":"...","tool_input_json":"{...}"}
- Prompt submission: {"action":"prompt","prompt":"..."}
Keep callback values compact when possible. Oversized valid callback payloads are automatically replaced with a short internal reference before sending to SeaTalk.
`)
}

func (seaTalkSendInteractiveMessageTool) InputSchema() any {
	return interactiveToolInputSchema(false)
}

func (seaTalkSendInteractiveMessageTool) OutputSchema() any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"message_id": map[string]any{"type": "string"},
		},
		"required":             []any{"message_id"},
		"additionalProperties": false,
	}
}

func (seaTalkSendInteractiveMessageTool) Call(ctx context.Context, input json.RawMessage) (any, error) {
	responder, _, err := seaTalkResponderFromToolContext(ctx)
	if err != nil {
		return nil, err
	}

	payload, err := decodeInteractiveToolPayload(input)
	if err != nil {
		return nil, fmt.Errorf("seatalk send interactive message tool failed: %w", err)
	}

	message, err := payload.buildMessage(ctx)
	if err != nil {
		return nil, fmt.Errorf("seatalk send interactive message tool failed: %w", err)
	}

	result, err := responder.SendInteractive(ctx, message)
	if err != nil {
		return nil, fmt.Errorf("seatalk send interactive message tool failed: %w", err)
	}

	return map[string]any{"message_id": result.MessageID}, nil
}

func (seaTalkUpdateInteractiveMessageTool) Name() string {
	return "seatalk_update_interactive_message"
}

func (seaTalkUpdateInteractiveMessageTool) Description() string {
	return strings.TrimSpace(`
Update a SeaTalk interactive message card that was previously sent by the bot in the current conversation.
Use this after an action with side effects has already been executed and the card should be updated to reflect the result, status, or next state without repeating that action.
If "message_id" is omitted during an interactive button callback turn, the clicked interactive message is updated by default.
`)
}

func (seaTalkUpdateInteractiveMessageTool) InputSchema() any {
	return interactiveToolInputSchema(true)
}

func (seaTalkUpdateInteractiveMessageTool) OutputSchema() any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"message_id": map[string]any{"type": "string"},
			"updated":    map[string]any{"type": "boolean"},
		},
		"required":             []any{"message_id", "updated"},
		"additionalProperties": false,
	}
}

func (seaTalkUpdateInteractiveMessageTool) Call(ctx context.Context, input json.RawMessage) (any, error) {
	responder, turnReq, err := seaTalkResponderFromToolContext(ctx)
	if err != nil {
		return nil, err
	}

	payload, err := decodeInteractiveToolPayload(input)
	if err != nil {
		return nil, fmt.Errorf("seatalk update interactive message tool failed: %w", err)
	}

	messageID := strings.TrimSpace(payload.MessageID)
	if messageID == "" {
		messageID = responder.CurrentInteractiveMessageID()
	}
	if messageID == "" {
		return nil, errors.New("seatalk update interactive message tool failed: message_id is empty")
	}

	message, err := payload.buildMessage(ctx)
	if err != nil {
		return nil, fmt.Errorf("seatalk update interactive message tool failed: %w", err)
	}

	log.Printf(
		"seatalk tool update_interactive_message: conversation=%s target=%s message_id=%s element_count=%d",
		turnReq.Conversation.Key,
		responder.target.logValue(),
		messageID,
		len(message.Elements),
	)
	if err := responder.UpdateInteractive(ctx, messageID, message); err != nil {
		return nil, fmt.Errorf("seatalk update interactive message tool failed: %w", err)
	}

	return map[string]any{
		"message_id": messageID,
		"updated":    true,
	}, nil
}

func interactiveToolInputSchema(includeMessageID bool) map[string]any {
	properties := map[string]any{
		"elements": map[string]any{
			"type":     "array",
			"minItems": 1,
			"items": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"element_type": map[string]any{
						"type": "string",
						"enum": []any{"title", "description", "button", "button_group", "image"},
					},
					"title": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"text": map[string]any{"type": "string"},
						},
						"required":             []any{"text"},
						"additionalProperties": false,
					},
					"description": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"text":   map[string]any{"type": "string"},
							"format": map[string]any{"type": "integer"},
						},
						"required":             []any{"text"},
						"additionalProperties": false,
					},
					"button": interactiveButtonSchema(),
					"button_group": map[string]any{
						"type":     "array",
						"minItems": 1,
						"maxItems": 3,
						"items":    interactiveButtonSchema(),
					},
					"image": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"base64_content":  map[string]any{"type": "string"},
							"local_file_path": map[string]any{"type": "string"},
						},
						"additionalProperties": false,
					},
				},
				"required":             []any{"element_type"},
				"additionalProperties": false,
			},
		},
	}

	required := []any{"elements"}
	if includeMessageID {
		properties["message_id"] = map[string]any{"type": "string"}
	}

	return map[string]any{
		"type":                 "object",
		"properties":           properties,
		"required":             required,
		"additionalProperties": false,
	}
}

func interactiveButtonSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"button_type": map[string]any{
				"type": "string",
				"enum": []any{seatalk.InteractiveButtonTypeCallback, seatalk.InteractiveButtonTypeRedirect},
			},
			"text":  map[string]any{"type": "string"},
			"value": map[string]any{"type": "string"},
			"mobile_link": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"type": map[string]any{
						"type": "string",
						"enum": []any{seatalk.InteractiveLinkTypeRN, seatalk.InteractiveLinkTypeWeb},
					},
					"path":   map[string]any{"type": "string"},
					"params": map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}},
				},
				"required":             []any{"type", "path"},
				"additionalProperties": false,
			},
			"desktop_link": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"type": map[string]any{
						"type": "string",
						"enum": []any{seatalk.InteractiveLinkTypeWeb},
					},
					"path": map[string]any{"type": "string"},
				},
				"required":             []any{"type", "path"},
				"additionalProperties": false,
			},
		},
		"required":             []any{"button_type", "text"},
		"additionalProperties": false,
	}
}

func decodeInteractiveToolPayload(input json.RawMessage) (interactiveToolPayload, error) {
	var payload interactiveToolPayload
	if err := json.Unmarshal(input, &payload); err != nil {
		return interactiveToolPayload{}, fmt.Errorf("decode input: %w", err)
	}
	if len(payload.Elements) == 0 {
		return interactiveToolPayload{}, errors.New("elements is empty")
	}

	return payload, nil
}

func sanitizedStrings(values []string) []string {
	filtered := make([]string, 0, len(values))
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			filtered = append(filtered, trimmed)
		}
	}
	if len(filtered) == 0 {
		return []string{}
	}
	return filtered
}

func (p interactiveToolPayload) buildMessage(ctx context.Context) (seatalk.InteractiveMessage, error) {
	elements := make([]seatalk.InteractiveElement, 0, len(p.Elements))
	for index, element := range p.Elements {
		converted, err := element.build(ctx)
		if err != nil {
			return seatalk.InteractiveMessage{}, fmt.Errorf("element %d: %w", index, err)
		}
		elements = append(elements, converted...)
	}
	if len(elements) == 0 {
		return seatalk.InteractiveMessage{}, errors.New("elements are empty after processing")
	}

	return seatalk.InteractiveMessage{Elements: elements}, nil
}

func (e interactiveElementInput) build(ctx context.Context) ([]seatalk.InteractiveElement, error) {
	switch strings.TrimSpace(e.ElementType) {
	case "title":
		if e.Title == nil || strings.TrimSpace(e.Title.Text) == "" {
			return nil, errors.New("title.text is empty")
		}
		return []seatalk.InteractiveElement{
			seatalk.InteractiveTitleElement{Text: strings.TrimSpace(e.Title.Text)},
		}, nil
	case "description":
		if e.Description == nil || strings.TrimSpace(e.Description.Text) == "" {
			return nil, errors.New("description.text is empty")
		}
		return []seatalk.InteractiveElement{seatalk.InteractiveDescriptionElement{
			Text:   strings.TrimSpace(e.Description.Text),
			Format: e.Description.Format,
		}}, nil
	case "button":
		if e.Button == nil {
			return nil, errors.New("button is empty")
		}
		button, err := e.Button.build(ctx)
		if err != nil {
			return nil, err
		}
		return []seatalk.InteractiveElement{
			seatalk.InteractiveButtonElement{Button: button},
		}, nil
	case "button_group":
		if len(e.ButtonGroup) == 0 {
			return nil, errors.New("button_group is empty")
		}
		buttons := make([]seatalk.InteractiveButton, 0, len(e.ButtonGroup))
		for index, buttonInput := range e.ButtonGroup {
			button, err := buttonInput.build(ctx)
			if err != nil {
				return nil, fmt.Errorf("button_group[%d]: %w", index, err)
			}
			buttons = append(buttons, button)
		}
		return []seatalk.InteractiveElement{
			seatalk.InteractiveButtonGroupElement{Buttons: buttons},
		}, nil
	case "image":
		if e.Image == nil {
			return nil, errors.New("image is empty")
		}
		imageElement, err := e.Image.build()
		if err != nil {
			return nil, err
		}
		if imageElement != nil {
			return []seatalk.InteractiveElement{imageElement}, nil
		}
		return []seatalk.InteractiveElement{}, nil
	default:
		return nil, fmt.Errorf("unsupported element_type %q", e.ElementType)
	}
}

func (i interactiveImageInput) build() (seatalk.InteractiveElement, error) {
	if content := strings.TrimSpace(i.Base64Content); content != "" {
		decoded, err := base64.StdEncoding.DecodeString(content)
		if err != nil {
			return nil, fmt.Errorf("decode image.base64_content: %w", err)
		}
		if err := validateInteractiveImageContent(decoded); err != nil {
			return nil, fmt.Errorf("validate image.base64_content: %w", err)
		}
		return seatalk.InteractiveImageElement{Content: decoded}, nil
	}

	if path := filepath.Clean(strings.TrimSpace(i.LocalFilePath)); path != "" && path != "." {
		content, err := os.ReadFile(path)
		if err != nil {
			log.Printf("seatalk interactive image local file load failed: path=%s err=%v", path, err)
			return nil, nil
		}
		if err := validateInteractiveImageContent(content); err != nil {
			log.Printf("seatalk interactive image local file validation failed: path=%s err=%v", path, err)
			return nil, nil
		}
		return seatalk.InteractiveImageElement{Content: content}, nil
	}

	return nil, errors.New("image source is empty")
}

func validateInteractiveImageContent(content []byte) error {
	if len(content) == 0 {
		return errors.New("image content is empty")
	}

	contentType := http.DetectContentType(content)
	switch contentType {
	case "image/png", "image/jpeg", "image/gif":
		return nil
	default:
		return fmt.Errorf("unsupported image content type %q", contentType)
	}
}

func (b interactiveButtonInput) build(ctx context.Context) (seatalk.InteractiveButton, error) {
	buttonType := strings.TrimSpace(b.Type)
	if buttonType == "" {
		buttonType = seatalk.InteractiveButtonTypeCallback
	}
	button := seatalk.InteractiveButton{
		Type:        buttonType,
		Text:        strings.TrimSpace(b.Text),
		Value:       b.Value,
		MobileLink:  b.MobileLink,
		DesktopLink: b.DesktopLink,
	}
	if button.Text == "" {
		return seatalk.InteractiveButton{}, errors.New("text is empty")
	}

	switch button.Type {
	case seatalk.InteractiveButtonTypeCallback:
		button.Value = strings.TrimSpace(button.Value)
		if button.Value == "" {
			return seatalk.InteractiveButton{}, errors.New("value is empty for callback button")
		}
		normalizedValue, err := normalizeInteractiveCallbackValue(ctx, button.Value)
		if err != nil {
			return seatalk.InteractiveButton{}, err
		}
		button.Value = normalizedValue
		if err := validateInteractiveCallbackValueRef(button.Value); err != nil {
			return seatalk.InteractiveButton{}, fmt.Errorf("invalid callback button value: %w", err)
		}
	case seatalk.InteractiveButtonTypeRedirect:
		if button.MobileLink == nil && button.DesktopLink == nil {
			return seatalk.InteractiveButton{}, errors.New("redirect button requires mobile_link or desktop_link")
		}
	default:
		return seatalk.InteractiveButton{}, fmt.Errorf("unsupported button_type %q", button.Type)
	}

	return button, nil
}

func normalizeInteractiveCallbackValue(ctx context.Context, raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if err := validateInteractiveCallbackPayload(trimmed); err != nil {
		return "", err
	}
	if len(trimmed) <= interactiveCallbackValueMaxLength {
		return trimmed, nil
	}

	refValue, err := storeInteractiveCallbackValue(ctx, trimmed)
	if err != nil {
		return "", fmt.Errorf("store callback tool payload: %w", err)
	}

	return refValue, nil
}

func resolveInteractiveCallbackValue(ctx context.Context, raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if !strings.HasPrefix(trimmed, interactiveCallbackValueRefPrefix) {
		if err := validateInteractiveCallbackPayload(trimmed); err != nil {
			return "", err
		}
		return trimmed, nil
	}

	token := strings.TrimSpace(strings.TrimPrefix(trimmed, interactiveCallbackValueRefPrefix))
	if token == "" {
		return "", errors.New("callback value reference token is empty")
	}

	data, err := cache.Global().Get(ctx, interactiveCallbackValueCacheKey(token))
	if err != nil {
		if errors.Is(err, cache.ErrNotFound) {
			return "", errors.New("callback value reference was not found or has expired")
		}
		return "", fmt.Errorf("load callback value reference: %w", err)
	}

	resolved := strings.TrimSpace(string(data))
	if err := validateInteractiveCallbackPayload(resolved); err != nil {
		return "", fmt.Errorf("resolve callback value reference: %w", err)
	}

	return resolved, nil
}

func validateInteractiveCallbackValueRef(value string) error {
	if token, ok := strings.CutPrefix(value, interactiveCallbackValueRefPrefix); ok {
		if strings.TrimSpace(token) == "" {
			return errors.New("callback value reference token is empty")
		}
		return nil
	}

	return validateInteractiveCallbackPayload(value)
}

func storeInteractiveCallbackValue(ctx context.Context, raw string) (string, error) {
	token, err := newInteractiveCallbackValueToken()
	if err != nil {
		return "", err
	}
	if err := cache.Global().Add(ctx, interactiveCallbackValueCacheKey(token), []byte(raw), interactiveCallbackValueTTL); err != nil {
		return "", err
	}

	return interactiveCallbackValueRefPrefix + token, nil
}

func newInteractiveCallbackValueToken() (string, error) {
	var tokenBytes [12]byte
	if _, err := rand.Read(tokenBytes[:]); err != nil {
		return "", fmt.Errorf("generate callback value token: %w", err)
	}

	return base64.RawURLEncoding.EncodeToString(tokenBytes[:]), nil
}

func interactiveCallbackValueCacheKey(token string) string {
	return "seatalk:interactive_callback_value:" + token
}

func validateInteractiveCallbackPayload(raw string) error {
	_, err := decodeInteractiveCallbackAction(raw)
	return err
}

type interactiveCallbackAction struct {
	Action        string          `json:"action"`
	ToolName      string          `json:"tool_name"`
	ToolInput     json.RawMessage `json:"tool_input"`
	ToolInputJSON string          `json:"tool_input_json"`
	Prompt        string          `json:"prompt"`
}

func decodeInteractiveCallbackAction(raw string) (interactiveCallbackAction, error) {
	var payload interactiveCallbackAction
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return interactiveCallbackAction{}, fmt.Errorf("decode callback action payload: %w", err)
	}

	action := strings.TrimSpace(payload.Action)
	if action == "" {
		return interactiveCallbackAction{}, errors.New("action is empty")
	}

	switch action {
	case "tool_call":
		payload.Action = action
		if err := validateInteractiveToolCallPayload(&payload); err != nil {
			return interactiveCallbackAction{}, err
		}
		return payload, nil
	case "prompt":
		payload.Action = action
		payload.Prompt = strings.TrimSpace(payload.Prompt)
		if payload.Prompt == "" {
			return interactiveCallbackAction{}, errors.New("prompt is empty")
		}
		return payload, nil
	default:
		return interactiveCallbackAction{}, fmt.Errorf("unsupported callback action %q", action)
	}
}

func validateInteractiveToolCallPayload(payload *interactiveCallbackAction) error {
	if payload == nil {
		return errors.New("tool call payload is nil")
	}
	if strings.TrimSpace(payload.ToolName) == "" {
		return errors.New("tool_name is empty")
	}
	if len(payload.ToolInput) == 0 && strings.TrimSpace(payload.ToolInputJSON) != "" {
		payload.ToolInput = json.RawMessage(payload.ToolInputJSON)
	}
	if len(payload.ToolInput) == 0 {
		payload.ToolInput = json.RawMessage("{}")
	}
	if !json.Valid(payload.ToolInput) {
		return errors.New("tool_input is not valid JSON")
	}

	var body map[string]any
	if err := json.Unmarshal(payload.ToolInput, &body); err != nil {
		return errors.New("tool_input must be a JSON object")
	}

	return nil
}

func seaTalkResponderFromToolContext(ctx context.Context) (*SeaTalkResponder, agent.TurnRequest, error) {
	req, ok := agent.TurnRequestFromContext(ctx)
	if !ok {
		return nil, agent.TurnRequest{}, errors.New("seatalk interactive tool failed: turn context is unavailable")
	}

	responder, ok := req.Message.Responder.(*SeaTalkResponder)
	if !ok || responder == nil {
		return nil, agent.TurnRequest{}, errors.New("seatalk interactive tool failed: SeaTalk responder is unavailable")
	}

	return responder, req, nil
}

func buildInteractiveClickMessage(event *seatalk.InteractiveMessageClickEvent, callbackValue string) string {
	if event == nil {
		return ""
	}

	value := strings.TrimSpace(callbackValue)
	if value == "" {
		value = `{}`
	}

	var parts []string
	parts = append(parts, "User clicked a SeaTalk interactive message button.")
	parts = append(parts, "The button callback value below is the user's selected tool call. If it is a valid tool-call payload, execute that tool directly.")
	parts = append(parts, "Clicked interactive message id: "+strings.TrimSpace(event.MessageID))
	if groupID := strings.TrimSpace(event.GroupID); groupID != "" {
		parts = append(parts, "Group id: "+groupID)
	}
	if threadID := strings.TrimSpace(event.ThreadID); threadID != "" {
		parts = append(parts, "Thread id: "+threadID)
	}
	parts = append(parts, "Button callback value:")
	parts = append(parts, value)

	return strings.Join(parts, "\n")
}
