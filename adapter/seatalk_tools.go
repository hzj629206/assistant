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
	"unicode/utf8"

	"github.com/hzj629206/assistant/agent"
	"github.com/hzj629206/assistant/cache"
	"github.com/hzj629206/assistant/seatalk"
)

type seaTalkSendFileTool struct{}

type seaTalkPushInteractiveMessageTool struct{}

type interactiveToolPayload struct {
	Mode      string                    `json:"mode,omitempty"`
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
}

const (
	interactiveCallbackValueMaxLength = 200
	interactiveCallbackValueRefPrefix = "stcb:"
	interactiveCallbackValueTTL       = 30 * 24 * time.Hour
	interactivePushModeSend           = "send"
	interactivePushModeUpdate         = "update"
	interactiveTitleMaxCount          = 3
	interactiveDescriptionMaxCount    = 5
	interactiveButtonMaxCount         = 5
	interactiveButtonGroupMaxCount    = 3
	interactiveImageMaxCount          = 3
	interactiveTitleMaxLength         = 120
	interactiveDescriptionSchemaMax   = 800
	interactiveDescriptionMaxLength   = 1000
	interactiveButtonTextMaxLength    = 50
	seaTalkFileBase64MaxBytes         = 5 * 1024 * 1024
)

func (seaTalkSendFileTool) Name() string {
	return "seatalk_send_file"
}

func (seaTalkSendFileTool) Description() string {
	return strings.TrimSpace(`
Send a local file from the current machine into the current SeaTalk conversation.
`)
}

func (seaTalkSendFileTool) InputSchema() any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"local_file_path": map[string]any{
				"type": "string",
			},
			"filename": map[string]any{
				"type": "string",
			},
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
	base64Size := base64.StdEncoding.EncodedLen(len(content))
	if base64Size > seaTalkFileBase64MaxBytes {
		return nil, fmt.Errorf(
			"seatalk send file tool failed: base64-encoded file content exceeds 5M limit: got %d bytes, max %d bytes",
			base64Size,
			seaTalkFileBase64MaxBytes,
		)
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

func (seaTalkPushInteractiveMessageTool) Name() string {
	return "seatalk_push_interactive_message"
}

func (seaTalkPushInteractiveMessageTool) Description() string {
	return strings.TrimSpace(`
Send or update a SeaTalk interactive message card in the current conversation.
Set "mode" to control whether this call sends a new card or updates an existing card.
- mode="send": always send a new interactive card and ignore "message_id".
- mode="update": update "message_id" if provided, otherwise update the current interactive card in context. Fail if neither target is available.
Elements are rendered top-to-bottom in array order. Mix title, description, button, button_group, and image elements freely to build the card.
Limits per card: title <= 3, description <= 5, standalone button <= 5, button_group <= 3, image <= 3.
Before sending or updating a card, you must self-check element counts and ensure every per-card limit is satisfied.
Description elements use SeaTalk Markdown format. Each description element supports up to 800 characters, so use separate description elements only when they add meaningful structure.
For callback buttons, set "value" to a JSON-encoded callback action payload serialized into a string.
Supported callback action payloads:
- Tool call: {"action":"tool_call","tool_name":"...","tool_input_json":"{...}"}
- Prompt submission: {"action":"prompt","prompt":"..."}
Keep callback values compact when possible. Oversized valid callback payloads are automatically replaced with a short internal reference before sending to SeaTalk.
`)
}

func (seaTalkPushInteractiveMessageTool) InputSchema() any {
	return interactiveToolInputSchema()
}

func (seaTalkPushInteractiveMessageTool) OutputSchema() any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"message_id": map[string]any{
				"type":        "string",
				"description": "The SeaTalk message ID that was sent or updated.",
			},
		},
		"required":             []any{"message_id"},
		"additionalProperties": false,
	}
}

func (seaTalkPushInteractiveMessageTool) Call(ctx context.Context, input json.RawMessage) (any, error) {
	responder, turnReq, err := seaTalkResponderFromToolContext(ctx)
	if err != nil {
		return nil, err
	}

	payload, err := decodeInteractiveToolPayload(input)
	if err != nil {
		return nil, fmt.Errorf("seatalk push interactive message tool failed: %w", err)
	}
	if err = validateInteractiveInputElementCounts(payload.Elements); err != nil {
		return nil, fmt.Errorf("seatalk push interactive message tool failed: %w", err)
	}

	mode := strings.TrimSpace(payload.Mode)
	if mode == "" {
		return nil, errors.New(`seatalk push interactive message tool failed: mode is required and must be "send" or "update"`)
	}
	messageID := strings.TrimSpace(payload.MessageID)
	if messageID == "" {
		messageID = responder.CurrentInteractiveMessageID()
	}
	switch mode {
	case interactivePushModeSend, interactivePushModeUpdate:
	default:
		return nil, fmt.Errorf("seatalk push interactive message tool failed: invalid mode %q", mode)
	}
	if mode == interactivePushModeSend {
		messageID = ""
	}
	if mode == interactivePushModeUpdate && messageID == "" {
		return nil, errors.New("seatalk push interactive message tool failed: update mode requires message_id or current interactive message context")
	}

	message, err := payload.buildMessage(ctx)
	if err != nil {
		return nil, fmt.Errorf("seatalk push interactive message tool failed: %w", err)
	}

	if messageID == "" {
		log.Printf(
			"seatalk tool push_interactive_message: conversation=%q target=%q mode=send element_count=%d",
			turnReq.Conversation.Key,
			responder.target.logValue(),
			len(message.Elements),
		)
		result, err := responder.SendInteractive(ctx, message)
		if err != nil {
			return nil, fmt.Errorf("seatalk push interactive message tool failed: %w", err)
		}

		return map[string]any{
			"message_id": result.MessageID,
		}, nil
	}

	log.Printf(
		"seatalk tool push_interactive_message: conversation=%q target=%q mode=update message_id=%q element_count=%d",
		turnReq.Conversation.Key,
		responder.target.logValue(),
		messageID,
		len(message.Elements),
	)
	if err := responder.UpdateInteractive(ctx, messageID, message); err != nil {
		return nil, fmt.Errorf("seatalk push interactive message tool failed: %w", err)
	}

	return map[string]any{
		"message_id": messageID,
	}, nil
}

func validateInteractiveInputElementCounts(elements []interactiveElementInput) error {
	titleCount := 0
	descriptionCount := 0
	buttonCount := 0
	buttonGroupCount := 0
	imageCount := 0

	var element interactiveElementInput
	for _, element = range elements {
		switch strings.TrimSpace(element.ElementType) {
		case "title":
			titleCount++
		case "description":
			descriptionCount++
		case "button":
			buttonCount++
		case "button_group":
			buttonGroupCount++
		case "image":
			imageCount++
		}
	}

	violations := make([]string, 0, 5)
	if titleCount > interactiveTitleMaxCount {
		violations = append(violations, fmt.Sprintf("title=%d (max %d)", titleCount, interactiveTitleMaxCount))
	}
	if descriptionCount > interactiveDescriptionMaxCount {
		violations = append(violations, fmt.Sprintf("description=%d (max %d)", descriptionCount, interactiveDescriptionMaxCount))
	}
	if buttonCount > interactiveButtonMaxCount {
		violations = append(violations, fmt.Sprintf("standalone_button=%d (max %d)", buttonCount, interactiveButtonMaxCount))
	}
	if buttonGroupCount > interactiveButtonGroupMaxCount {
		violations = append(violations, fmt.Sprintf("button_group=%d (max %d)", buttonGroupCount, interactiveButtonGroupMaxCount))
	}
	if imageCount > interactiveImageMaxCount {
		violations = append(violations, fmt.Sprintf("image=%d (max %d)", imageCount, interactiveImageMaxCount))
	}
	if len(violations) > 0 {
		return fmt.Errorf("interactive card element count exceeds per-card limits: %s", strings.Join(violations, ", "))
	}

	return nil
}

func interactiveToolInputSchema() map[string]any {
	properties := map[string]any{
		"mode": map[string]any{
			"type": "string",
			"enum": []any{interactivePushModeSend, interactivePushModeUpdate},
		},
		"message_id": map[string]any{
			"type":        "string",
			"description": `Optional target interactive message ID. Ignored when mode="send".`,
		},
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
							"text": map[string]any{
								"type":      "string",
								"maxLength": interactiveTitleMaxLength,
							},
						},
						"required":             []any{"text"},
						"additionalProperties": false,
					},
					"description": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"text": map[string]any{
								"type":      "string",
								"maxLength": interactiveDescriptionSchemaMax,
							},
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
							"base64_content": map[string]any{
								"type": "string",
							},
						},
						"required":             []any{"base64_content"},
						"additionalProperties": false,
					},
				},
				"required":             []any{"element_type"},
				"additionalProperties": false,
			},
		},
	}

	return map[string]any{
		"type":                 "object",
		"properties":           properties,
		"required":             []any{"mode", "elements"},
		"additionalProperties": false,
	}
}

func interactiveButtonSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"button_type": map[string]any{
				"type":        "string",
				"enum":        []any{seatalk.InteractiveButtonTypeCallback, seatalk.InteractiveButtonTypeRedirect},
				"description": `Button behavior: "redirect" opens an external link, "callback" executes the action payload.`,
			},
			"text": map[string]any{
				"type":      "string",
				"maxLength": interactiveButtonTextMaxLength,
			},
			"value": map[string]any{
				"type":        "string",
				"description": `Callback action payload when button_type="callback".`,
			},
			"mobile_link": map[string]any{
				"type":        "object",
				"description": `Redirect destination used on SeaTalk mobile clients when button_type="redirect".`,
				"properties": map[string]any{
					"type": map[string]any{
						"type":        "string",
						"enum":        []any{seatalk.InteractiveLinkTypeRN, seatalk.InteractiveLinkTypeWeb},
						"description": `"rn" opens an in-app RN page; "web" opens a web URL.`,
					},
					"path": map[string]any{
						"type":        "string",
						"description": `RN path starting with "/" or a full web/external URL for mobile redirect.`,
					},
					"params": map[string]any{
						"type":                 "object",
						"description":          `Optional query parameters passed with an "rn" mobile link.`,
						"additionalProperties": map[string]any{"type": "string"},
					},
				},
				"required":             []any{"type", "path"},
				"additionalProperties": false,
			},
			"desktop_link": map[string]any{
				"type":        "object",
				"description": `Redirect destination used on SeaTalk desktop clients when button_type="redirect".`,
				"properties": map[string]any{
					"type": map[string]any{
						"type":        "string",
						"enum":        []any{seatalk.InteractiveLinkTypeWeb},
						"description": `Desktop redirect type. SeaTalk currently supports only "web".`,
					},
					"path": map[string]any{
						"type":        "string",
						"description": `Full web app or external website URL opened on desktop.`,
					},
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
	if err := validateInteractiveCardElementCounts(elements); err != nil {
		return seatalk.InteractiveMessage{}, err
	}

	return seatalk.InteractiveMessage{Elements: elements}, nil
}

func validateInteractiveCardElementCounts(elements []seatalk.InteractiveElement) error {
	titleCount := 0
	descriptionCount := 0
	buttonCount := 0
	buttonGroupCount := 0
	imageCount := 0

	for _, element := range elements {
		switch element.(type) {
		case seatalk.InteractiveTitleElement:
			titleCount++
		case seatalk.InteractiveDescriptionElement:
			descriptionCount++
		case seatalk.InteractiveButtonElement:
			buttonCount++
		case seatalk.InteractiveButtonGroupElement:
			buttonGroupCount++
		case seatalk.InteractiveImageElement:
			imageCount++
		}
	}

	if titleCount > interactiveTitleMaxCount {
		return fmt.Errorf("title element count exceeds limit: got %d, max %d", titleCount, interactiveTitleMaxCount)
	}
	if descriptionCount > interactiveDescriptionMaxCount {
		return fmt.Errorf("description element count exceeds limit: got %d, max %d", descriptionCount, interactiveDescriptionMaxCount)
	}
	if buttonCount > interactiveButtonMaxCount {
		return fmt.Errorf("standalone button element count exceeds limit: got %d, max %d", buttonCount, interactiveButtonMaxCount)
	}
	if buttonGroupCount > interactiveButtonGroupMaxCount {
		return fmt.Errorf("button_group element count exceeds limit: got %d, max %d", buttonGroupCount, interactiveButtonGroupMaxCount)
	}
	if imageCount > interactiveImageMaxCount {
		return fmt.Errorf("image element count exceeds limit: got %d, max %d", imageCount, interactiveImageMaxCount)
	}

	return nil
}

func (e interactiveElementInput) build(ctx context.Context) ([]seatalk.InteractiveElement, error) {
	switch strings.TrimSpace(e.ElementType) {
	case "title":
		if e.Title == nil || strings.TrimSpace(e.Title.Text) == "" {
			return nil, errors.New("title.text is empty")
		}
		return []seatalk.InteractiveElement{
			seatalk.InteractiveTitleElement{Text: truncateRunes(strings.TrimSpace(e.Title.Text), interactiveTitleMaxLength)},
		}, nil
	case "description":
		if e.Description == nil || strings.TrimSpace(e.Description.Text) == "" {
			return nil, errors.New("description.text is empty")
		}
		descriptionText, err := normalizeInteractiveDescriptionText(strings.TrimSpace(e.Description.Text))
		if err != nil {
			return nil, err
		}
		return []seatalk.InteractiveElement{seatalk.InteractiveDescriptionElement{
			Text:   descriptionText,
			Format: seatalk.TextFormatMarkdown,
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
		base64Size := base64.StdEncoding.EncodedLen(len(decoded))
		if base64Size > seaTalkFileBase64MaxBytes {
			return nil, fmt.Errorf(
				"base64-encoded image content exceeds 5M limit: got %d bytes, max %d bytes",
				base64Size,
				seaTalkFileBase64MaxBytes,
			)
		}
		if err := validateInteractiveImageContent(decoded); err != nil {
			return nil, fmt.Errorf("validate image.base64_content: %w", err)
		}
		return seatalk.InteractiveImageElement{Content: decoded}, nil
	}

	return nil, errors.New("image.base64_content is empty")
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
		Text:        truncateRunes(strings.TrimSpace(b.Text), interactiveButtonTextMaxLength),
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

func truncateRunes(value string, maxLength int) string {
	if maxLength <= 0 {
		return ""
	}

	runes := []rune(value)
	if len(runes) <= maxLength {
		return value
	}

	return string(runes[:maxLength])
}

func normalizeInteractiveDescriptionText(value string) (string, error) {
	normalized := normalizeSeaTalkMarkdown(value)
	if utf8.RuneCountInString(normalized) > interactiveDescriptionMaxLength {
		return "", fmt.Errorf(
			"description.text exceeds SeaTalk hard limit: got %d characters; keep description.text within %d characters",
			utf8.RuneCountInString(value),
			interactiveDescriptionSchemaMax,
		)
	}

	return normalized, nil
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
