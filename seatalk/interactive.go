package seatalk

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

const (
	// InteractiveButtonTypeCallback triggers an event callback with the configured value.
	InteractiveButtonTypeCallback = "callback"
	// InteractiveButtonTypeRedirect redirects the user to an RN or web link.
	InteractiveButtonTypeRedirect = "redirect"
	// InteractiveLinkTypeRN opens an RN app page.
	InteractiveLinkTypeRN = "rn"
	// InteractiveLinkTypeWeb opens a web page.
	InteractiveLinkTypeWeb = "web"
)

// InteractiveElement represents one SeaTalk card element in the top-to-bottom stack.
type InteractiveElement interface {
	// Marshaler encodes (MarshalJSON) the element into the SeaTalk interactive card JSON shape.
	json.Marshaler
	// interactiveElement marks concrete implementations of InteractiveElement.
	interactiveElement()
}

// InteractiveTitleElement renders a first-level title in the card.
// SeaTalk allows up to 3 title elements in one card.
type InteractiveTitleElement struct {
	// Text is the title content. SeaTalk allows 1 to 120 characters.
	Text string
}

// InteractiveDescriptionElement renders a description block in the card.
// SeaTalk allows up to 5 description elements in one card.
type InteractiveDescriptionElement struct {
	// Text is the description content. SeaTalk allows 1 to 1000 characters.
	Text string
	// Format is 1 for Markdown and 2 for plain text. The default is Markdown.
	Format int
}

// InteractiveButtonElement renders a single callback or redirect button.
// SeaTalk allows up to 5 standalone button elements in one card.
type InteractiveButtonElement struct {
	// Button is the button definition shown in the card.
	Button InteractiveButton
}

// InteractiveButtonGroupElement renders up to 3 buttons in one horizontal group.
// SeaTalk allows up to 3 button group elements in one card.
type InteractiveButtonGroupElement struct {
	// Buttons is the button list rendered in the same row.
	// Each button group can contain 1 to 3 buttons.
	Buttons []InteractiveButton
}

// InteractiveImageElement renders an image inside the card.
// SeaTalk allows up to 3 image elements in one card.
type InteractiveImageElement struct {
	// Content is the raw image bytes. The client base64-encodes it before sending.
	// SeaTalk supports PNG, JPG and GIF images, up to 5 MB after encoding.
	Content []byte
}

// InteractiveButton describes a callback or redirect button in a card.
type InteractiveButton struct {
	// Type is "callback" or "redirect".
	Type string `json:"button_type"`
	// Text is the button label. SeaTalk allows 1 to 50 characters.
	Text string `json:"text"`
	// Value is the callback payload. It is required for callback buttons.
	Value string `json:"value,omitempty"`
	// MobileLink configures the mobile destination for redirect buttons.
	MobileLink *InteractiveMobileLink `json:"mobile_link,omitempty"`
	// DesktopLink configures the desktop destination for redirect buttons.
	DesktopLink *InteractiveDesktopLink `json:"desktop_link,omitempty"`
}

// InteractiveMobileLink defines the destination opened on SeaTalk mobile clients.
type InteractiveMobileLink struct {
	// Type is "rn" or "web".
	Type string `json:"type"`
	// Path is the RN path starting with "/" or a full web URL.
	Path string `json:"path"`
	// Params contains RN route parameters appended to the RN path.
	Params map[string]string `json:"params,omitempty"`
}

// InteractiveDesktopLink defines the destination opened on SeaTalk desktop clients.
type InteractiveDesktopLink struct {
	// Type is currently only "web".
	Type string `json:"type"`
	// Path is the full web URL.
	Path string `json:"path"`
}

type interactiveSummaryElement struct {
	ElementType string `json:"element_type"`
	Title       *struct {
		Text string `json:"text"`
	} `json:"title"`
	Description *struct {
		Text string `json:"text"`
	} `json:"description"`
	Button      *InteractiveButton  `json:"button"`
	ButtonGroup []InteractiveButton `json:"button_group"`
	Image       *struct {
		Content string `json:"content"`
	} `json:"image"`
}

func (InteractiveTitleElement) interactiveElement() {}

// MarshalJSON encodes the title element into the SeaTalk interactive card schema.
func (e InteractiveTitleElement) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		ElementType string `json:"element_type"`
		Title       struct {
			Text string `json:"text"`
		} `json:"title"`
	}{
		ElementType: "title",
		Title: struct {
			Text string `json:"text"`
		}{
			Text: e.Text,
		},
	})
}

func (InteractiveDescriptionElement) interactiveElement() {}

// MarshalJSON encodes the description element into the SeaTalk interactive card schema.
func (e InteractiveDescriptionElement) MarshalJSON() ([]byte, error) {
	format := e.Format
	if format == 0 {
		format = TextFormatMarkdown
	}

	return json.Marshal(struct {
		ElementType string `json:"element_type"`
		Description struct {
			Format int    `json:"format,omitempty"`
			Text   string `json:"text"`
		} `json:"description"`
	}{
		ElementType: "description",
		Description: struct {
			Format int    `json:"format,omitempty"`
			Text   string `json:"text"`
		}{
			Format: format,
			Text:   e.Text,
		},
	})
}

func (InteractiveButtonElement) interactiveElement() {}

// MarshalJSON encodes the single button element into the SeaTalk interactive card schema.
func (e InteractiveButtonElement) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		ElementType string            `json:"element_type"`
		Button      InteractiveButton `json:"button"`
	}{
		ElementType: "button",
		Button:      e.Button,
	})
}

func (InteractiveButtonGroupElement) interactiveElement() {}

// MarshalJSON encodes the button group element into the SeaTalk interactive card schema.
func (e InteractiveButtonGroupElement) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		ElementType string              `json:"element_type"`
		ButtonGroup []InteractiveButton `json:"button_group"`
	}{
		ElementType: "button_group",
		ButtonGroup: e.Buttons,
	})
}

func (InteractiveImageElement) interactiveElement() {}

// MarshalJSON encodes the image element into the SeaTalk interactive card schema.
func (e InteractiveImageElement) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		ElementType string `json:"element_type"`
		Image       struct {
			Content string `json:"content"`
		} `json:"image"`
	}{
		ElementType: "image",
		Image: struct {
			Content string `json:"content"`
		}{
			Content: base64.StdEncoding.EncodeToString(e.Content),
		},
	})
}

type sendInteractiveBody struct {
	// Elements is the interactive message card body defined by SeaTalk.
	Elements []InteractiveElement `json:"elements"`
}

type updateInteractiveMessageRequest struct {
	// MessageID is the ID of the interactive message to update.
	MessageID string `json:"message_id"`
	// Message contains the updated interactive message payload.
	Message struct {
		// InteractiveMessage is the updated interactive card.
		InteractiveMessage sendInteractiveBody `json:"interactive_message"`
	} `json:"message"`
}

type updateInteractiveMessageResponse struct {
	// Code is the SeaTalk API status code, where 0 means success.
	Code int `json:"code"`
	// Message contains the error message when Code is non-zero.
	Message string `json:"message"`
}

// InteractiveMessage is the payload for an interactive message card.
type InteractiveMessage struct {
	// Elements is the card element list defined by the SeaTalk interactive message schema.
	// SeaTalk supports title, description, button, button_group and image elements.
	Elements []InteractiveElement
}

// SummarizeInteractiveMessage renders a compact text summary for an inbound interactive card.
func SummarizeInteractiveMessage(message *ThreadInteractiveMessage) string {
	if message == nil {
		return ""
	}

	return summarizeInteractiveElements(message.Elements)
}

func summarizeInteractiveElements(elements []json.RawMessage) string {
	if len(elements) == 0 {
		return ""
	}

	titles := make([]string, 0, len(elements))
	descriptions := make([]string, 0, len(elements))
	buttons := make([]string, 0, len(elements))
	imageURLs := make([]string, 0, len(elements))
	imageCount := 0

	for _, raw := range elements {
		if len(raw) == 0 {
			continue
		}

		var element interactiveSummaryElement
		if err := json.Unmarshal(raw, &element); err != nil {
			continue
		}

		switch strings.TrimSpace(element.ElementType) {
		case "title":
			if element.Title != nil {
				if text := normalizeInteractiveSummaryText(element.Title.Text); text != "" {
					titles = append(titles, text)
				}
			}
		case "description":
			if element.Description != nil {
				if text := normalizeInteractiveSummaryText(element.Description.Text); text != "" {
					descriptions = append(descriptions, text)
				}
			}
		case "button":
			if element.Button != nil {
				if label := summarizeInteractiveButton(*element.Button); label != "" {
					buttons = append(buttons, label)
				}
			}
		case "button_group":
			for _, button := range element.ButtonGroup {
				if label := summarizeInteractiveButton(button); label != "" {
					buttons = append(buttons, label)
				}
			}
		case "image":
			imageCount++
			if element.Image != nil {
				if imageURL := normalizeInteractiveImageURL(element.Image.Content); imageURL != "" {
					imageURLs = append(imageURLs, imageURL)
				}
			}
		}
	}

	parts := []string{"interactive card"}
	if len(titles) > 0 {
		parts = append(parts, `title="`+strings.Join(titles, " | ")+`"`)
	}
	if len(descriptions) > 0 {
		parts = append(parts, `description="`+strings.Join(descriptions, " | ")+`"`)
	}
	if len(buttons) > 0 {
		parts = append(parts, "buttons=["+strings.Join(buttons, ", ")+"]")
	}
	if len(imageURLs) > 0 {
		parts = append(parts, "image_urls=["+strings.Join(imageURLs, ", ")+"]")
	} else if imageCount > 0 {
		parts = append(parts, "images="+strconv.Itoa(imageCount))
	}
	if len(parts) == 1 {
		return ""
	}

	return strings.Join(parts, "; ")
}

func summarizeInteractiveButton(button InteractiveButton) string {
	label := normalizeInteractiveSummaryText(button.Text)
	if label == "" {
		return ""
	}
	if strings.TrimSpace(button.Type) != InteractiveButtonTypeRedirect {
		return label
	}

	destination := summarizeInteractiveButtonDestination(button)
	if destination == "" {
		return label
	}

	return label + " (" + destination + ")"
}

func summarizeInteractiveButtonDestination(button InteractiveButton) string {
	if button.DesktopLink != nil {
		if destination := normalizeInteractiveSummaryText(button.DesktopLink.Path); destination != "" {
			return destination
		}
	}

	if button.MobileLink == nil {
		return ""
	}

	path := normalizeInteractiveSummaryText(button.MobileLink.Path)
	if path == "" {
		return ""
	}
	if strings.TrimSpace(button.MobileLink.Type) == InteractiveLinkTypeRN {
		return "rn:" + path
	}

	return path
}

// ExtractInteractiveImageURLs returns http(s) image URLs found in an inbound interactive card.
func ExtractInteractiveImageURLs(message *ThreadInteractiveMessage) []string {
	if message == nil {
		return nil
	}

	return extractInteractiveImageURLs(message.Elements)
}

func normalizeInteractiveSummaryText(value string) string {
	return strings.Join(strings.Fields(strings.TrimSpace(value)), " ")
}

func extractInteractiveImageURLs(elements []json.RawMessage) []string {
	imageURLs := make([]string, 0, len(elements))
	seen := make(map[string]struct{}, len(elements))
	for _, raw := range elements {
		if len(raw) == 0 {
			continue
		}

		var element interactiveSummaryElement
		if err := json.Unmarshal(raw, &element); err != nil {
			continue
		}
		if strings.TrimSpace(element.ElementType) != "image" || element.Image == nil {
			continue
		}

		imageURL := normalizeInteractiveImageURL(element.Image.Content)
		if imageURL == "" {
			continue
		}
		if _, ok := seen[imageURL]; ok {
			continue
		}
		seen[imageURL] = struct{}{}
		imageURLs = append(imageURLs, imageURL)
	}

	return imageURLs
}

func normalizeInteractiveImageURL(value string) string {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "http://") || strings.HasPrefix(value, "https://") {
		return value
	}
	return ""
}

// SendPrivateInteractive sends an interactive message card to a bot user in a 1-on-1 chat.
func (c *Client) SendPrivateInteractive(ctx context.Context, employeeCode string, message InteractiveMessage, opts PrivateSendOptions) (SendMessageResult, error) {
	if len(message.Elements) == 0 {
		return SendMessageResult{}, errors.New("send private interactive message failed: elements is empty")
	}

	return c.sendPrivateChatMessage(ctx, employeeCode, sendGroupChatBodyObject{
		Tag: "interactive_message",
		InteractiveMessage: &sendInteractiveBody{
			Elements: message.Elements,
		},
		ThreadID: opts.ThreadID,
	}, opts)
}

// SendGroupInteractive sends an interactive message card to a group chat.
func (c *Client) SendGroupInteractive(ctx context.Context, groupID string, message InteractiveMessage, opts SendOptions) (SendMessageResult, error) {
	if len(message.Elements) == 0 {
		return SendMessageResult{}, errors.New("send interactive message failed: elements is empty")
	}

	return c.sendGroupChatMessage(ctx, groupID, sendGroupChatBodyObject{
		Tag: "interactive_message",
		InteractiveMessage: &sendInteractiveBody{
			Elements: message.Elements,
		},
		ThreadID: opts.ThreadID,
	})
}

// UpdateInteractiveMessage updates an interactive message card that was previously sent by the bot.
func (c *Client) UpdateInteractiveMessage(ctx context.Context, messageID string, message InteractiveMessage) error {
	if c == nil {
		return errors.New("update interactive message failed: client is nil")
	}
	if messageID == "" {
		return errors.New("update interactive message failed: message id is empty")
	}
	if len(message.Elements) == 0 {
		return errors.New("update interactive message failed: elements is empty")
	}

	reqPayload := updateInteractiveMessageRequest{
		MessageID: messageID,
	}
	reqPayload.Message.InteractiveMessage = sendInteractiveBody(message)

	reqBody, err := json.Marshal(reqPayload)
	if err != nil {
		return fmt.Errorf("update interactive message failed: encode request body: %w", err)
	}

	client := c.httpClient
	if client == nil {
		client = http.DefaultClient
	}

	tokenProvider := c.tokenProvider
	if tokenProvider == nil {
		tokenProvider = GetAccessToken
	}

	token, err := tokenProvider(ctx, client, c.appID, c.appSecret)
	if err != nil {
		return fmt.Errorf("update interactive message failed: get access token: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, updateMessageEndpoint, bytes.NewReader(reqBody))
	if err != nil {
		return fmt.Errorf("update interactive message failed: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req) //nolint:gosec // SeaTalk API endpoint is controlled by trusted configuration.
	if err != nil {
		return fmt.Errorf("update interactive message failed: send request: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("update interactive message failed: read response body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("update interactive message failed: unexpected status %d: %s", resp.StatusCode, string(respBody))
	}

	var apiResp updateInteractiveMessageResponse
	if err := json.Unmarshal(respBody, &apiResp); err != nil {
		return fmt.Errorf("update interactive message failed: decode response body: %w", err)
	}
	if apiResp.Code != 0 {
		if apiResp.Message != "" {
			return fmt.Errorf("update interactive message failed: api returned code %d: %s", apiResp.Code, apiResp.Message)
		}
		return fmt.Errorf("update interactive message failed: api returned code %d", apiResp.Code)
	}

	return nil
}
