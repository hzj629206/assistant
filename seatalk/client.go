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
	"net/url"
	"strconv"
	"strings"
)

const (
	// TextFormatMarkdown sends the text content with SeaTalk Markdown formatting.
	TextFormatMarkdown = 1
	// TextFormatPlain sends the text content as plain text.
	TextFormatPlain = 2
)

var (
	sendGroupChatEndpoint    = "https://openapi.seatalk.io/messaging/v2/group_chat"
	sendSingleChatEndpoint   = "https://openapi.seatalk.io/messaging/v2/single_chat"
	getEmployeeInfoEndpoint  = "https://openapi.seatalk.io/contacts/v2/profile"
	getGroupInfoEndpoint     = "https://openapi.seatalk.io/messaging/v2/group_chat/info"
	getMessageEndpoint       = "https://openapi.seatalk.io/messaging/v2/get_message_by_message_id"
	getGroupThreadEndpoint   = "https://openapi.seatalk.io/messaging/v2/group_chat/get_thread_by_thread_id"
	getPrivateThreadEndpoint = "https://openapi.seatalk.io/messaging/v2/single_chat/get_thread_by_thread_id"
	updateMessageEndpoint    = "https://openapi.seatalk.io/messaging/v2/update"
	groupTypingEndpoint      = "https://openapi.seatalk.io/messaging/v2/group_chat_typing"
	privateTypingEndpoint    = "https://openapi.seatalk.io/messaging/v2/single_chat_typing"
	downloadFileURLPrefix    = "https://openapi.seatalk.io/messaging/v2/file/"
)

// ErrEmployeeInfoDisabled indicates that employee profile lookups are disabled by configuration.
var ErrEmployeeInfoDisabled = errors.New("employee info is disabled")

// TokenProvider returns an app access token for the client.
type TokenProvider func(ctx context.Context, client *http.Client, appID, appSecret string) (string, error)

// ClientOption configures a Client during construction.
type ClientOption func(*Client)

// Client sends messages to SeaTalk group chats.
type Client struct {
	appID               string
	appSecret           string
	employeeInfoEnabled bool
	httpClient          *http.Client
	tokenProvider       TokenProvider
}

// WithHTTPClient configures the HTTP client used to call the SeaTalk OpenAPI endpoint.
func WithHTTPClient(client *http.Client) ClientOption {
	return func(c *Client) {
		c.httpClient = client
	}
}

// WithTokenProvider configures the token provider used to build Authorization headers.
func WithTokenProvider(provider TokenProvider) ClientOption {
	return func(c *Client) {
		c.tokenProvider = provider
	}
}

// SendOptions controls optional reply metadata on a message that has been sent.
type SendOptions struct {
	// QuotedMessageID quotes an existing message sent within the last 7 days.
	QuotedMessageID string
	// ThreadID sends the message into an existing thread, or starts a thread from a recent root message.
	ThreadID string
}

const (
	// UsablePlatformAll makes the private message fully available on mobile and desktop.
	UsablePlatformAll = "all"
	// UsablePlatformMobile makes the private message fully available only on mobile clients.
	UsablePlatformMobile = "mobile"
	// UsablePlatformDesktop makes the private message fully available only on desktop clients.
	UsablePlatformDesktop = "desktop"
)

// PrivateSendOptions controls the optional thread and platform settings for a private message.
type PrivateSendOptions struct {
	// ThreadID sends the message into an existing thread, or starts a thread from a recent root message.
	ThreadID string
	// UsablePlatform defines where the message can be fully viewed and acted on.
	// SeaTalk accepts "all", "mobile" and "desktop".
	UsablePlatform string
}

// TextMessage is the payload for a text message.
type TextMessage struct {
	// Content is the text body. SeaTalk allows 1 to 4096 characters.
	Content string
	// Format controls whether the content is interpreted as Markdown or plain text.
	Format int
}

// ImageMessage is the payload for an image message.
type ImageMessage struct {
	// Content is the raw image bytes. The client base64-encodes it before sending.
	// SeaTalk supports PNG, JPG and GIF images, up to 5 MB after encoding.
	Content []byte
}

// FileMessage is the payload for a file message.
type FileMessage struct {
	// Filename is the file name with extension, limited to 100 characters by SeaTalk.
	Filename string
	// Content is the raw file bytes. The client base64-encodes it before sending.
	// SeaTalk requires 10 bytes to 5 MB after encoding.
	Content []byte
}

// SendMessageResult contains the API response after a successful send.
type SendMessageResult struct {
	// MessageID is the SeaTalk message ID returned by the send message API.
	MessageID string
}

// EmployeeInfo contains one employee profile returned by the SeaTalk contacts API.
type EmployeeInfo struct {
	// EmployeeCode is the employee_code of the employee.
	EmployeeCode string `json:"employee_code"`
	// SeatalkID is the SeaTalk ID of the employee.
	SeatalkID string `json:"seatalk_id"`
	// SeatalkNickname is the SeaTalk nickname of the employee.
	SeatalkNickname string `json:"seatalk_nickname"`
	// Avatar is the employee avatar URL on SeaTalk.
	Avatar string `json:"avatar"`
	// Name is the employee name in the organization directory.
	Name string `json:"name"`
	// Email is the employee email address.
	Email string `json:"email"`
	// Departments contains the department codes the employee belongs to.
	Departments []string `json:"departments"`
	// Gender is 0 for blank, 1 for male, 2 for female and 3 for unknown.
	Gender int `json:"gender"`
	// Mobile is the employee mobile phone number.
	Mobile string `json:"mobile"`
	// ReportingManagerEmployeeCode is the employee_code of the reporting manager.
	// SeaTalk returns "0" when it is not applicable.
	ReportingManagerEmployeeCode string `json:"reporting_manager_employee_code"`
	// OffboardingTime is the Unix timestamp when termination is triggered.
	OffboardingTime UnixTimestamp `json:"offboarding_time"`
	// CustomFields contains additional profile fields configured in the organization.
	CustomFields []EmployeeCustomField `json:"custom_fields"`
}

// EmployeeCustomField contains one custom employee profile field.
type EmployeeCustomField struct {
	// Name is the display name of the custom field.
	Name string `json:"name"`
	// Type is the SeaTalk custom field type.
	Type int `json:"type"`
	// Value is the field value serialized by SeaTalk.
	Value string `json:"value"`
	// LinkEntryIcons contains the configured link-entry icons when the field is a link entry.
	LinkEntryIcons []json.RawMessage `json:"link_entry_icons"`
	// LinkEntryText is the configured link-entry display text.
	LinkEntryText string `json:"link_entry_text"`
}

// GetEmployeeInfoResult contains the employee profiles returned by GetEmployeeInfo.
type GetEmployeeInfoResult struct {
	// Employees is the employee profile list that matches the requested employee codes.
	Employees []EmployeeInfo
}

// GetGroupInfoOptions controls the optional pagination arguments for GetGroupInfo.
type GetGroupInfoOptions struct {
	// PageSize defines the number of items returned for each member list.
	// SeaTalk accepts values from 1 to 100 and defaults to 50.
	PageSize int
	// Cursor continues traversal from a previous GetGroupInfo response.
	Cursor string
}

// GroupInfo contains the group chat information returned by the SeaTalk API.
type GroupInfo struct {
	// GroupName is the current group chat name.
	GroupName string `json:"group_name"`
	// GroupSettings contains the current group configuration.
	GroupSettings GroupSettings `json:"group_settings"`
	// GroupUserTotal is the number of normal users in the group.
	GroupUserTotal int `json:"group_user_total"`
	// GroupBotTotal is the number of bots in the group.
	GroupBotTotal int `json:"group_bot_total"`
	// GroupSystemAccountTotal is the number of system accounts in the group.
	GroupSystemAccountTotal int `json:"group_system_account_total"`
	// GroupUserList is the paginated list of normal users in the group.
	GroupUserList []GroupUser `json:"group_user_list"`
	// GroupBotList is the paginated list of bot SeaTalk IDs in the group.
	GroupBotList []string `json:"group_bot_list"`
	// GroupSystemAccountList is the paginated list of system account SeaTalk IDs in the group.
	GroupSystemAccountList []string `json:"group_system_account_list"`
}

// GroupSettings contains the current settings of a group chat.
type GroupSettings struct {
	// ChatHistoryForNewMembers defines how much prior history a new member can access.
	// SeaTalk returns "disabled", "1 day" or "7 days".
	ChatHistoryForNewMembers string `json:"chat_history_for_new_members"`
	// CanNotifyWithAtAll indicates whether members can notify the whole group with "@All".
	CanNotifyWithAtAll bool `json:"can_notify_with_at_all"`
	// CanViewMemberList indicates whether members can view the group member list.
	CanViewMemberList bool `json:"can_view_member_list"`
}

// GroupUser contains one normal user returned in the group user list.
type GroupUser struct {
	// SeatalkID is the SeaTalk ID of the user.
	SeatalkID string `json:"seatalk_id"`
	// EmployeeCode is the employee code of the user when visible to the bot.
	EmployeeCode string `json:"employee_code"`
	// Email is the email of the user when visible to the bot.
	Email string `json:"email"`
}

// GetGroupInfoResult contains the paginated response from GetGroupInfo.
type GetGroupInfoResult struct {
	// NextCursor is the cursor for the next request. Empty means there is no next page.
	NextCursor string
	// Group is the current group chat info payload.
	Group GroupInfo
}

// GetPrivateThreadOptions controls the optional pagination arguments for GetPrivateThread.
type GetPrivateThreadOptions struct {
	// PageSize defines the number of messages returned in one response.
	// SeaTalk accepts values from 1 to 100 and defaults to 50.
	PageSize int
	// Cursor continues traversal from a previous GetPrivateThread response.
	Cursor string
}

// GetGroupThreadOptions controls the optional pagination arguments for GetGroupThread.
type GetGroupThreadOptions struct {
	// PageSize defines the number of messages returned in one response.
	// SeaTalk accepts values from 1 to 100 and defaults to 50.
	PageSize int
	// Cursor continues traversal from a previous GetGroupThread response.
	Cursor string
}

// MentionedEntity contains one mention mapping returned in a thread text message.
type MentionedEntity struct {
	// Username is the current username inserted into the plain text message content.
	Username string `json:"username"`
	// SeatalkID is the SeaTalk ID of the mentioned user or bot.
	SeatalkID string `json:"seatalk_id"`
	// EmployeeCode is the employee code of the mentioned user when available.
	EmployeeCode string `json:"employee_code"`
	// Email is the email of the mentioned user when available.
	Email string `json:"email"`
}

// MessageSender contains the basic sender information returned with a message.
type MessageSender struct {
	// SeatalkID is the SeaTalk ID of the message sender.
	SeatalkID string `json:"seatalk_id"`
	// EmployeeCode is the employee code of the sender when visible to the bot.
	EmployeeCode string `json:"employee_code"`
	// Email is the email of the sender when visible to the bot.
	Email string `json:"email"`
	// SenderType is 1 for user, 2 for bot and 3 for system account.
	SenderType int `json:"sender_type"`
}

// ThreadTextMessage contains the text payload returned in a thread message.
type ThreadTextMessage struct {
	// PlainText is the text content returned by SeaTalk.
	PlainText string `json:"plain_text"`
	// LastEditedTime is the most recent edit time of the message, or 0 if the message was not edited.
	LastEditedTime int64 `json:"last_edited_time"`
	// MentionedList maps mentioned usernames to SeaTalk identities.
	MentionedList []MentionedEntity `json:"mentioned_list"`
}

// ThreadBinaryMessage contains a file-like payload returned in a thread message.
type ThreadBinaryMessage struct {
	// Content is the SeaTalk download URL. A valid API token is required to access it.
	Content string `json:"content"`
}

// ThreadFileMessage contains the file payload returned in a thread message.
type ThreadFileMessage struct {
	// Content is the SeaTalk download URL. A valid API token is required to access it.
	Content string `json:"content"`
	// Filename is the file name with extension.
	Filename string `json:"filename"`
}

// CombinedForwardedChatHistoryMessage contains the combined forwarded chat history payload.
type CombinedForwardedChatHistoryMessage struct {
	// Content contains the forwarded message objects returned by SeaTalk.
	Content []map[string]any `json:"content"`
}

// ThreadInteractiveMessage contains the interactive card payload returned in a thread message.
type ThreadInteractiveMessage struct {
	// Elements is the interactive message card element list.
	Elements []json.RawMessage `json:"elements"`
	// MentionedList maps mentioned usernames to SeaTalk identities in the interactive card.
	MentionedList []MentionedEntity `json:"mentioned_list"`
}

// PrivateThreadMessage contains one message returned by GetPrivateThread.
type PrivateThreadMessage struct {
	// MessageID is the message ID.
	MessageID string `json:"message_id"`
	// QuotedMessageID is the quoted message ID, if any.
	QuotedMessageID string `json:"quoted_message_id"`
	// ThreadID is the thread ID when the message belongs to a thread.
	ThreadID string `json:"thread_id"`
	// Sender contains the message sender information.
	Sender MessageSender `json:"sender"`
	// MessageSentTime is the Unix timestamp when the message was sent.
	MessageSentTime int64 `json:"message_sent_time"`
	// Tag is the message type.
	Tag string `json:"tag"`
	// Text is the text payload when Tag is "text".
	Text *ThreadTextMessage `json:"text"`
	// CombinedForwardedChatHistory is the combined forwarded message payload when present.
	CombinedForwardedChatHistory *CombinedForwardedChatHistoryMessage `json:"combined_forwarded_chat_history"`
	// Image is the image payload when Tag is "image".
	Image *ThreadBinaryMessage `json:"image"`
	// File is the file payload when Tag is "file".
	File *ThreadFileMessage `json:"file"`
	// Video is the video payload when Tag is "video".
	Video *ThreadBinaryMessage `json:"video"`
	// InteractiveMessage is the interactive card payload when Tag is "interactive_message".
	InteractiveMessage *ThreadInteractiveMessage `json:"interactive_message"`
}

// GetPrivateThreadResult contains the paginated response from GetPrivateThread.
type GetPrivateThreadResult struct {
	// NextCursor is the cursor for the next request. Empty means there is no next page.
	NextCursor string
	// ThreadMessages is the current page of messages returned from the thread.
	ThreadMessages []PrivateThreadMessage
}

// GroupThreadMessage contains one message returned by GetGroupThread.
type GroupThreadMessage = PrivateThreadMessage

// GetMessageResult contains the message payload returned by GetMessage.
type GetMessageResult = PrivateThreadMessage

// GetGroupThreadResult contains the paginated response from GetGroupThread.
type GetGroupThreadResult struct {
	// NextCursor is the cursor for the next request. Empty means there is no next page.
	NextCursor string
	// ThreadMessages is the current page of messages returned from the thread.
	ThreadMessages []GroupThreadMessage
}

// UnixTimestamp stores a Unix timestamp returned by SeaTalk.
// SeaTalk documents this field as an integer, but some examples return it as a quoted string.
type UnixTimestamp int64

// UnmarshalJSON accepts both JSON numbers and quoted decimal strings.
func (t *UnixTimestamp) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		*t = 0
		return nil
	}

	var number int64
	if err := json.Unmarshal(data, &number); err == nil {
		*t = UnixTimestamp(number)
		return nil
	}

	var text string
	if err := json.Unmarshal(data, &text); err != nil {
		return fmt.Errorf("decode unix timestamp: %w", err)
	}
	if text == "" {
		*t = 0
		return nil
	}

	number, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return fmt.Errorf("decode unix timestamp: %w", err)
	}
	*t = UnixTimestamp(number)
	return nil
}

type sendGroupChatRequest struct {
	// GroupID is the target group chat ID.
	GroupID string `json:"group_id"`
	// Message is the message payload to send to the group chat.
	Message sendGroupChatBodyObject `json:"message"`
}

type sendSingleChatRequest struct {
	// EmployeeCode is the employee_code of the bot user in the private chat.
	EmployeeCode string `json:"employee_code"`
	// Message is the message payload to send to the private chat.
	Message sendGroupChatBodyObject `json:"message"`
	// UsablePlatform controls where the private message can be fully viewed and acted on.
	UsablePlatform string `json:"usable_platform,omitempty"`
}

type sendGroupChatBodyObject struct {
	// Tag identifies the message type, such as text, image, file or interactive_message.
	Tag string `json:"tag"`
	// Text contains the text message payload when Tag is text.
	Text *sendTextBody `json:"text,omitempty"`
	// Image contains the image message payload when Tag is image.
	Image *sendBinaryBody `json:"image,omitempty"`
	// File contains the file message payload when Tag is file.
	File *sendFileBody `json:"file,omitempty"`
	// InteractiveMessage contains the interactive card payload when Tag is interactive_message.
	InteractiveMessage *sendInteractiveBody `json:"interactive_message,omitempty"`
	// QuotedMessageID quotes an existing message as part of the reply.
	QuotedMessageID string `json:"quoted_message_id,omitempty"`
	// ThreadID sends the message into a specific thread.
	// To start a thread in an unthreaded root message, define thread_id as the message_id of the root message.
	// The root message has to be sent within the past 7 days.
	ThreadID string `json:"thread_id,omitempty"`
}

type sendTextBody struct {
	// Format is 1 for Markdown and 2 for plain text.
	Format int `json:"format,omitempty"`
	// Content is the text content to send.
	Content string `json:"content"`
}

type sendBinaryBody struct {
	// Content is the base64-encoded binary payload.
	Content string `json:"content"`
}

type sendFileBody struct {
	// Filename is the file name with extension shown in SeaTalk.
	Filename string `json:"filename"`
	// Content is the base64-encoded file payload.
	Content string `json:"content"`
}

type sendGroupChatResponse struct {
	// Code is the SeaTalk API status code, where 0 means success.
	Code int `json:"code"`
	// Message contains the error message when Code is non-zero.
	Message string `json:"message"`
	// MessageID is returned on success and identifies the messages have been sent.
	MessageID string `json:"message_id"`
}

type getGroupInfoResponse struct {
	// Code is the SeaTalk API status code, where 0 means success.
	Code int `json:"code"`
	// Message contains the error message when Code is non-zero.
	Message string `json:"message"`
	// NextCursor is the cursor for the next request. Empty means the current page is the last page.
	NextCursor string `json:"next_cursor"`
	// Group is the group chat payload returned by SeaTalk.
	Group GroupInfo `json:"group"`
}

type getEmployeeInfoResponse struct {
	// Code is the SeaTalk API status code, where 0 means success.
	Code int `json:"code"`
	// Message contains the error message when Code is non-zero.
	Message string `json:"message"`
	// Employees is the employee profile list returned by SeaTalk.
	Employees []EmployeeInfo `json:"employees"`
}

type getMessageResponse struct {
	// Code is the SeaTalk API status code, where 0 means success.
	Code int `json:"code"`
	// Message contains the error message when Code is non-zero.
	Message string `json:"message"`
	// MessageID is the message ID.
	MessageID string `json:"message_id"`
	// QuotedMessageID is the quoted message ID, if any.
	QuotedMessageID string `json:"quoted_message_id"`
	// ThreadID is the thread ID when the message belongs to a thread.
	ThreadID string `json:"thread_id"`
	// Sender contains the message sender information.
	Sender MessageSender `json:"sender"`
	// MessageSentTime is the Unix timestamp when the message was sent.
	MessageSentTime int64 `json:"message_sent_time"`
	// Tag is the message type.
	Tag string `json:"tag"`
	// Text is the text payload when Tag is "text".
	Text *ThreadTextMessage `json:"text"`
	// CombinedForwardedChatHistory is the combined forwarded message payload when present.
	CombinedForwardedChatHistory *CombinedForwardedChatHistoryMessage `json:"combined_forwarded_chat_history"`
	// Image is the image payload when Tag is "image".
	Image *ThreadBinaryMessage `json:"image"`
	// File is the file payload when Tag is "file".
	File *ThreadFileMessage `json:"file"`
	// Video is the video payload when Tag is "video".
	Video *ThreadBinaryMessage `json:"video"`
	// InteractiveMessage is the interactive card payload when Tag is "interactive_message".
	InteractiveMessage *ThreadInteractiveMessage `json:"interactive_message"`
}

type getPrivateThreadResponse struct {
	// Code is the SeaTalk API status code, where 0 means success.
	Code int `json:"code"`
	// Message contains the error message when Code is non-zero.
	Message string `json:"message"`
	// NextCursor is the cursor for the next request. Empty means the current page is the last page.
	NextCursor string `json:"next_cursor"`
	// ThreadMessages is the current page of thread messages.
	ThreadMessages []PrivateThreadMessage `json:"thread_messages"`
}

type getGroupThreadResponse struct {
	// Code is the SeaTalk API status code, where 0 means success.
	Code int `json:"code"`
	// Message contains the error message when Code is non-zero.
	Message string `json:"message"`
	// NextCursor is the cursor for the next request. Empty means the current page is the last page.
	NextCursor string `json:"next_cursor"`
	// ThreadMessages is the current page of thread messages.
	ThreadMessages []GroupThreadMessage `json:"thread_messages"`
}

type setGroupTypingStatusRequest struct {
	// GroupID is the target group chat ID.
	GroupID string `json:"group_id"`
	// ThreadID optionally targets a thread within the group chat.
	ThreadID string `json:"thread_id,omitempty"`
}

type setGroupTypingStatusResponse struct {
	// Code is the SeaTalk API status code, where 0 means success.
	Code int `json:"code"`
	// Message contains the error message when Code is non-zero.
	Message string `json:"message"`
}

type setPrivateTypingStatusRequest struct {
	// EmployeeCode is the employee_code of the user in the private chat.
	EmployeeCode string `json:"employee_code"`
	// ThreadID optionally targets a thread within the private chat.
	// To trigger typing on an unthreaded root message, SeaTalk expects the root message_id here.
	ThreadID string `json:"thread_id,omitempty"`
}

type setPrivateTypingStatusResponse struct {
	// Code is the SeaTalk API status code, where 0 means success.
	Code int `json:"code"`
	// Message contains the error message when Code is non-zero.
	Message string `json:"message"`
}

// NewClient builds a SeaTalk client with the provided app credentials.
func NewClient(cfg Config, options ...ClientOption) *Client {
	client := &Client{
		appID:               strings.TrimSpace(cfg.AppID),
		appSecret:           strings.TrimSpace(cfg.AppSecret),
		employeeInfoEnabled: cfg.EmployeeInfoEnabled,
		httpClient:          http.DefaultClient,
		tokenProvider:       GetAccessToken,
	}
	for _, option := range options {
		if option != nil {
			option(client)
		}
	}
	return client
}

// SendGroupText sends a text message to a group chat.
func (c *Client) SendGroupText(ctx context.Context, groupID string, message TextMessage, opts SendOptions) (SendMessageResult, error) {
	if message.Content == "" {
		return SendMessageResult{}, errors.New("send text message failed: content is empty")
	}

	format := message.Format
	if format == 0 {
		format = TextFormatMarkdown
	}
	if format != TextFormatMarkdown && format != TextFormatPlain {
		return SendMessageResult{}, fmt.Errorf("send text message failed: unsupported format %d", format)
	}

	return c.sendGroupChatMessage(ctx, groupID, sendGroupChatBodyObject{
		Tag: "text",
		Text: &sendTextBody{
			Format:  format,
			Content: message.Content,
		},
		QuotedMessageID: opts.QuotedMessageID,
		ThreadID:        opts.ThreadID,
	})
}

// SendGroupImage sends an image message to a group chat.
func (c *Client) SendGroupImage(ctx context.Context, groupID string, message ImageMessage, opts SendOptions) (SendMessageResult, error) {
	if len(message.Content) == 0 {
		return SendMessageResult{}, errors.New("send image message failed: content is empty")
	}

	return c.sendGroupChatMessage(ctx, groupID, sendGroupChatBodyObject{
		Tag: "image",
		Image: &sendBinaryBody{
			Content: base64.StdEncoding.EncodeToString(message.Content),
		},
		QuotedMessageID: opts.QuotedMessageID,
		ThreadID:        opts.ThreadID,
	})
}

// SendGroupFile sends a file message to a group chat.
func (c *Client) SendGroupFile(ctx context.Context, groupID string, message FileMessage, opts SendOptions) (SendMessageResult, error) {
	if message.Filename == "" {
		return SendMessageResult{}, errors.New("send file message failed: filename is empty")
	}
	if len(message.Content) == 0 {
		return SendMessageResult{}, errors.New("send file message failed: content is empty")
	}

	return c.sendGroupChatMessage(ctx, groupID, sendGroupChatBodyObject{
		Tag: "file",
		File: &sendFileBody{
			Filename: message.Filename,
			Content:  base64.StdEncoding.EncodeToString(message.Content),
		},
		QuotedMessageID: opts.QuotedMessageID,
		ThreadID:        opts.ThreadID,
	})
}

// SendPrivateText sends a text message to a bot user in a 1-on-1 chat.
func (c *Client) SendPrivateText(ctx context.Context, employeeCode string, message TextMessage, opts PrivateSendOptions) (SendMessageResult, error) {
	if message.Content == "" {
		return SendMessageResult{}, errors.New("send private text message failed: content is empty")
	}

	format := message.Format
	if format == 0 {
		format = TextFormatMarkdown
	}
	if format != TextFormatMarkdown && format != TextFormatPlain {
		return SendMessageResult{}, fmt.Errorf("send private text message failed: unsupported format %d", format)
	}

	return c.sendPrivateChatMessage(ctx, employeeCode, sendGroupChatBodyObject{
		Tag: "text",
		Text: &sendTextBody{
			Format:  format,
			Content: message.Content,
		},
		ThreadID: opts.ThreadID,
	}, opts)
}

// SendPrivateImage sends an image message to a bot user in a 1-on-1 chat.
func (c *Client) SendPrivateImage(ctx context.Context, employeeCode string, message ImageMessage, opts PrivateSendOptions) (SendMessageResult, error) {
	if len(message.Content) == 0 {
		return SendMessageResult{}, errors.New("send private image message failed: content is empty")
	}

	return c.sendPrivateChatMessage(ctx, employeeCode, sendGroupChatBodyObject{
		Tag: "image",
		Image: &sendBinaryBody{
			Content: base64.StdEncoding.EncodeToString(message.Content),
		},
		ThreadID: opts.ThreadID,
	}, opts)
}

// SendPrivateFile sends a file message to a bot user in a 1-on-1 chat.
func (c *Client) SendPrivateFile(ctx context.Context, employeeCode string, message FileMessage, opts PrivateSendOptions) (SendMessageResult, error) {
	if message.Filename == "" {
		return SendMessageResult{}, errors.New("send private file message failed: filename is empty")
	}
	if len(message.Content) == 0 {
		return SendMessageResult{}, errors.New("send private file message failed: content is empty")
	}

	return c.sendPrivateChatMessage(ctx, employeeCode, sendGroupChatBodyObject{
		Tag: "file",
		File: &sendFileBody{
			Filename: message.Filename,
			Content:  base64.StdEncoding.EncodeToString(message.Content),
		},
		ThreadID: opts.ThreadID,
	}, opts)
}

// GetEmployeeInfo retrieves one or more employee profiles by employee_code.
// SeaTalk accepts at most 500 employee codes in a single request.
func (c *Client) GetEmployeeInfo(ctx context.Context, employeeCodes ...string) (GetEmployeeInfoResult, error) {
	if c == nil {
		return GetEmployeeInfoResult{}, errors.New("get employee info failed: client is nil")
	}
	if !c.employeeInfoEnabled {
		return GetEmployeeInfoResult{}, fmt.Errorf("get employee info failed: %w", ErrEmployeeInfoDisabled)
	}
	if len(employeeCodes) == 0 {
		return GetEmployeeInfoResult{}, errors.New("get employee info failed: employee codes are empty")
	}
	if len(employeeCodes) > 500 {
		return GetEmployeeInfoResult{}, errors.New("get employee info failed: employee codes exceed 500")
	}

	values := url.Values{}
	for _, employeeCode := range employeeCodes {
		if employeeCode == "" {
			return GetEmployeeInfoResult{}, errors.New("get employee info failed: employee code is empty")
		}
		values.Add("employee_code", employeeCode)
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
		return GetEmployeeInfoResult{}, fmt.Errorf("get employee info failed: get access token: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, getEmployeeInfoEndpoint+"?"+values.Encode(), nil)
	if err != nil {
		return GetEmployeeInfoResult{}, fmt.Errorf("get employee info failed: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := client.Do(req) //nolint:gosec // SeaTalk API endpoint is controlled by trusted configuration.
	if err != nil {
		return GetEmployeeInfoResult{}, fmt.Errorf("get employee info failed: send request: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return GetEmployeeInfoResult{}, fmt.Errorf("get employee info failed: read response body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return GetEmployeeInfoResult{}, fmt.Errorf("get employee info failed: unexpected status %d: %s", resp.StatusCode, string(respBody))
	}

	var apiResp getEmployeeInfoResponse
	if err := json.Unmarshal(respBody, &apiResp); err != nil {
		return GetEmployeeInfoResult{}, fmt.Errorf("get employee info failed: decode response body: %w", err)
	}
	if apiResp.Code != 0 {
		if apiResp.Message != "" {
			return GetEmployeeInfoResult{}, fmt.Errorf("get employee info failed: api returned code %d: %s", apiResp.Code, apiResp.Message)
		}
		return GetEmployeeInfoResult{}, fmt.Errorf("get employee info failed: api returned code %d", apiResp.Code)
	}

	return GetEmployeeInfoResult{
		Employees: apiResp.Employees,
	}, nil
}

// GetGroupInfo retrieves the current info of a group chat that the bot has joined.
func (c *Client) GetGroupInfo(ctx context.Context, groupID string, opts GetGroupInfoOptions) (GetGroupInfoResult, error) {
	if c == nil {
		return GetGroupInfoResult{}, errors.New("get group info failed: client is nil")
	}
	if groupID == "" {
		return GetGroupInfoResult{}, errors.New("get group info failed: group id is empty")
	}
	if opts.PageSize < 0 || opts.PageSize > 100 {
		return GetGroupInfoResult{}, errors.New("get group info failed: page size must be between 1 and 100")
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
		return GetGroupInfoResult{}, fmt.Errorf("get group info failed: get access token: %w", err)
	}

	endpoint := getGroupInfoEndpoint
	values := url.Values{}
	values.Set("group_id", groupID)
	if opts.PageSize > 0 {
		values.Set("page_size", strconv.Itoa(opts.PageSize))
	}
	if opts.Cursor != "" {
		values.Set("cursor", opts.Cursor)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"?"+values.Encode(), nil)
	if err != nil {
		return GetGroupInfoResult{}, fmt.Errorf("get group info failed: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := client.Do(req) //nolint:gosec // SeaTalk API endpoint is controlled by trusted configuration.
	if err != nil {
		return GetGroupInfoResult{}, fmt.Errorf("get group info failed: send request: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return GetGroupInfoResult{}, fmt.Errorf("get group info failed: read response body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return GetGroupInfoResult{}, fmt.Errorf("get group info failed: unexpected status %d: %s", resp.StatusCode, string(respBody))
	}

	var apiResp getGroupInfoResponse
	if err := json.Unmarshal(respBody, &apiResp); err != nil {
		return GetGroupInfoResult{}, fmt.Errorf("get group info failed: decode response body: %w", err)
	}
	if apiResp.Code != 0 {
		if apiResp.Message != "" {
			return GetGroupInfoResult{}, fmt.Errorf("get group info failed: api returned code %d: %s", apiResp.Code, apiResp.Message)
		}
		return GetGroupInfoResult{}, fmt.Errorf("get group info failed: api returned code %d", apiResp.Code)
	}

	return GetGroupInfoResult{
		NextCursor: apiResp.NextCursor,
		Group:      apiResp.Group,
	}, nil
}

// Download retrieves the raw bytes from a SeaTalk file download URL.
func (c *Client) Download(ctx context.Context, downloadURL string) ([]byte, error) {
	if c == nil {
		return nil, errors.New("download file failed: client is nil")
	}
	if downloadURL == "" {
		return nil, errors.New("download file failed: url is empty")
	}
	downloadURI, err := url.Parse(downloadURL)
	if err != nil {
		return nil, fmt.Errorf("download file failed: parse url: %w", err)
	}
	if downloadURI.Scheme != "https" || downloadURI.Host != "openapi.seatalk.io" {
		return nil, fmt.Errorf("download file failed: unsupported url %q", downloadURL)
	}
	if !strings.HasPrefix(downloadURI.Path, "/messaging/v2/file/") {
		return nil, fmt.Errorf("download file failed: unsupported url %q", downloadURL)
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
		return nil, fmt.Errorf("download file failed: get access token: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURI.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("download file failed: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := client.Do(req) //nolint:gosec // URL is validated above to require https and the SeaTalk API host.
	if err != nil {
		return nil, fmt.Errorf("download file failed: send request: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("download file failed: read response body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download file failed: unexpected status %d: %s", resp.StatusCode, string(respBody))
	}

	return respBody, nil
}

// GetPrivateThread retrieves messages within a private-chat thread by thread ID.
func (c *Client) GetPrivateThread(ctx context.Context, employeeCode, threadID string, opts GetPrivateThreadOptions) (GetPrivateThreadResult, error) {
	if c == nil {
		return GetPrivateThreadResult{}, errors.New("get private thread failed: client is nil")
	}
	if employeeCode == "" {
		return GetPrivateThreadResult{}, errors.New("get private thread failed: employee code is empty")
	}
	if threadID == "" {
		return GetPrivateThreadResult{}, errors.New("get private thread failed: thread id is empty")
	}
	if opts.PageSize < 0 || opts.PageSize > 100 {
		return GetPrivateThreadResult{}, errors.New("get private thread failed: page size must be between 1 and 100")
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
		return GetPrivateThreadResult{}, fmt.Errorf("get private thread failed: get access token: %w", err)
	}

	values := url.Values{}
	values.Set("employee_code", employeeCode)
	values.Set("thread_id", threadID)
	if opts.PageSize > 0 {
		values.Set("page_size", strconv.Itoa(opts.PageSize))
	}
	if opts.Cursor != "" {
		values.Set("cursor", opts.Cursor)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, getPrivateThreadEndpoint+"?"+values.Encode(), nil)
	if err != nil {
		return GetPrivateThreadResult{}, fmt.Errorf("get private thread failed: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req) //nolint:gosec // SeaTalk API endpoint is controlled by trusted configuration.
	if err != nil {
		return GetPrivateThreadResult{}, fmt.Errorf("get private thread failed: send request: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return GetPrivateThreadResult{}, fmt.Errorf("get private thread failed: read response body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return GetPrivateThreadResult{}, fmt.Errorf("get private thread failed: unexpected status %d: %s", resp.StatusCode, string(respBody))
	}

	var apiResp getPrivateThreadResponse
	if err := json.Unmarshal(respBody, &apiResp); err != nil {
		return GetPrivateThreadResult{}, fmt.Errorf("get private thread failed: decode response body: %w", err)
	}
	if apiResp.Code != 0 {
		if apiResp.Message != "" {
			return GetPrivateThreadResult{}, fmt.Errorf("get private thread failed: api returned code %d: %s", apiResp.Code, apiResp.Message)
		}
		return GetPrivateThreadResult{}, fmt.Errorf("get private thread failed: api returned code %d", apiResp.Code)
	}

	return GetPrivateThreadResult{
		NextCursor:     apiResp.NextCursor,
		ThreadMessages: apiResp.ThreadMessages,
	}, nil
}

// GetMessage retrieves a message by message ID.
func (c *Client) GetMessage(ctx context.Context, messageID string) (GetMessageResult, error) {
	if c == nil {
		return GetMessageResult{}, errors.New("get message failed: client is nil")
	}
	if messageID == "" {
		return GetMessageResult{}, errors.New("get message failed: message id is empty")
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
		return GetMessageResult{}, fmt.Errorf("get message failed: get access token: %w", err)
	}

	values := url.Values{}
	values.Set("message_id", messageID)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, getMessageEndpoint+"?"+values.Encode(), nil)
	if err != nil {
		return GetMessageResult{}, fmt.Errorf("get message failed: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req) //nolint:gosec // SeaTalk API endpoint is controlled by trusted configuration.
	if err != nil {
		return GetMessageResult{}, fmt.Errorf("get message failed: send request: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return GetMessageResult{}, fmt.Errorf("get message failed: read response body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return GetMessageResult{}, fmt.Errorf("get message failed: unexpected status %d: %s", resp.StatusCode, string(respBody))
	}

	var apiResp getMessageResponse
	if err := json.Unmarshal(respBody, &apiResp); err != nil {
		return GetMessageResult{}, fmt.Errorf("get message failed: decode response body: %w", err)
	}
	if apiResp.Code != 0 {
		if apiResp.Message != "" {
			return GetMessageResult{}, fmt.Errorf("get message failed: api returned code %d: %s", apiResp.Code, apiResp.Message)
		}
		return GetMessageResult{}, fmt.Errorf("get message failed: api returned code %d", apiResp.Code)
	}

	return GetMessageResult{
		MessageID:                    apiResp.MessageID,
		QuotedMessageID:              apiResp.QuotedMessageID,
		ThreadID:                     apiResp.ThreadID,
		Sender:                       apiResp.Sender,
		MessageSentTime:              apiResp.MessageSentTime,
		Tag:                          apiResp.Tag,
		Text:                         apiResp.Text,
		CombinedForwardedChatHistory: apiResp.CombinedForwardedChatHistory,
		Image:                        apiResp.Image,
		File:                         apiResp.File,
		Video:                        apiResp.Video,
		InteractiveMessage:           apiResp.InteractiveMessage,
	}, nil
}

// GetGroupThread retrieves messages within a group-chat thread by thread ID.
func (c *Client) GetGroupThread(ctx context.Context, groupID, threadID string, opts GetGroupThreadOptions) (GetGroupThreadResult, error) {
	if c == nil {
		return GetGroupThreadResult{}, errors.New("get group thread failed: client is nil")
	}
	if groupID == "" {
		return GetGroupThreadResult{}, errors.New("get group thread failed: group id is empty")
	}
	if threadID == "" {
		return GetGroupThreadResult{}, errors.New("get group thread failed: thread id is empty")
	}
	if opts.PageSize < 0 || opts.PageSize > 100 {
		return GetGroupThreadResult{}, errors.New("get group thread failed: page size must be between 1 and 100")
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
		return GetGroupThreadResult{}, fmt.Errorf("get group thread failed: get access token: %w", err)
	}

	values := url.Values{}
	values.Set("group_id", groupID)
	values.Set("thread_id", threadID)
	if opts.PageSize > 0 {
		values.Set("page_size", strconv.Itoa(opts.PageSize))
	}
	if opts.Cursor != "" {
		values.Set("cursor", opts.Cursor)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, getGroupThreadEndpoint+"?"+values.Encode(), nil)
	if err != nil {
		return GetGroupThreadResult{}, fmt.Errorf("get group thread failed: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req) //nolint:gosec // SeaTalk API endpoint is controlled by trusted configuration.
	if err != nil {
		return GetGroupThreadResult{}, fmt.Errorf("get group thread failed: send request: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return GetGroupThreadResult{}, fmt.Errorf("get group thread failed: read response body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return GetGroupThreadResult{}, fmt.Errorf("get group thread failed: unexpected status %d: %s", resp.StatusCode, string(respBody))
	}

	var apiResp getGroupThreadResponse
	if err := json.Unmarshal(respBody, &apiResp); err != nil {
		return GetGroupThreadResult{}, fmt.Errorf("get group thread failed: decode response body: %w", err)
	}
	if apiResp.Code != 0 {
		if apiResp.Message != "" {
			return GetGroupThreadResult{}, fmt.Errorf("get group thread failed: api returned code %d: %s", apiResp.Code, apiResp.Message)
		}
		return GetGroupThreadResult{}, fmt.Errorf("get group thread failed: api returned code %d", apiResp.Code)
	}

	return GetGroupThreadResult{
		NextCursor:     apiResp.NextCursor,
		ThreadMessages: apiResp.ThreadMessages,
	}, nil
}

// SetGroupTypingStatus triggers the "Typing..." indicator in a group chat or thread.
func (c *Client) SetGroupTypingStatus(ctx context.Context, groupID, threadID string) error {
	if c == nil {
		return errors.New("set group typing status failed: client is nil")
	}
	if groupID == "" {
		return errors.New("set group typing status failed: group id is empty")
	}

	reqBody, err := json.Marshal(setGroupTypingStatusRequest{
		GroupID:  groupID,
		ThreadID: threadID,
	})
	if err != nil {
		return fmt.Errorf("set group typing status failed: encode request body: %w", err)
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
		return fmt.Errorf("set group typing status failed: get access token: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, groupTypingEndpoint, bytes.NewReader(reqBody))
	if err != nil {
		return fmt.Errorf("set group typing status failed: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req) //nolint:gosec // SeaTalk API endpoint is controlled by trusted configuration.
	if err != nil {
		return fmt.Errorf("set group typing status failed: send request: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("set group typing status failed: read response body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("set group typing status failed: unexpected status %d: %s", resp.StatusCode, string(respBody))
	}

	var apiResp setGroupTypingStatusResponse
	if err := json.Unmarshal(respBody, &apiResp); err != nil {
		return fmt.Errorf("set group typing status failed: decode response body: %w", err)
	}
	if apiResp.Code != 0 {
		if apiResp.Message != "" {
			return fmt.Errorf("set group typing status failed: api returned code %d: %s", apiResp.Code, apiResp.Message)
		}
		return fmt.Errorf("set group typing status failed: api returned code %d", apiResp.Code)
	}

	return nil
}

// SetPrivateTypingStatus triggers the "Typing..." indicator in a private chat or thread.
func (c *Client) SetPrivateTypingStatus(ctx context.Context, employeeCode, threadID string) error {
	if c == nil {
		return errors.New("set private typing status failed: client is nil")
	}
	if employeeCode == "" {
		return errors.New("set private typing status failed: employee code is empty")
	}

	reqBody, err := json.Marshal(setPrivateTypingStatusRequest{
		EmployeeCode: employeeCode,
		ThreadID:     threadID,
	})
	if err != nil {
		return fmt.Errorf("set private typing status failed: encode request body: %w", err)
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
		return fmt.Errorf("set private typing status failed: get access token: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, privateTypingEndpoint, bytes.NewReader(reqBody))
	if err != nil {
		return fmt.Errorf("set private typing status failed: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req) //nolint:gosec // SeaTalk API endpoint is controlled by trusted configuration.
	if err != nil {
		return fmt.Errorf("set private typing status failed: send request: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("set private typing status failed: read response body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("set private typing status failed: unexpected status %d: %s", resp.StatusCode, string(respBody))
	}

	var apiResp setPrivateTypingStatusResponse
	if err := json.Unmarshal(respBody, &apiResp); err != nil {
		return fmt.Errorf("set private typing status failed: decode response body: %w", err)
	}
	if apiResp.Code != 0 {
		if apiResp.Message != "" {
			return fmt.Errorf("set private typing status failed: api returned code %d: %s", apiResp.Code, apiResp.Message)
		}
		return fmt.Errorf("set private typing status failed: api returned code %d", apiResp.Code)
	}

	return nil
}

func (c *Client) sendPrivateChatMessage(ctx context.Context, employeeCode string, message sendGroupChatBodyObject, opts PrivateSendOptions) (SendMessageResult, error) {
	if c == nil {
		return SendMessageResult{}, errors.New("send private message failed: client is nil")
	}
	if employeeCode == "" {
		return SendMessageResult{}, errors.New("send private message failed: employee code is empty")
	}
	if opts.UsablePlatform != "" &&
		opts.UsablePlatform != UsablePlatformAll &&
		opts.UsablePlatform != UsablePlatformMobile &&
		opts.UsablePlatform != UsablePlatformDesktop {
		return SendMessageResult{}, fmt.Errorf("send private message failed: unsupported usable platform %q", opts.UsablePlatform)
	}

	reqBody, err := json.Marshal(sendSingleChatRequest{
		EmployeeCode:   employeeCode,
		Message:        message,
		UsablePlatform: opts.UsablePlatform,
	})
	if err != nil {
		return SendMessageResult{}, fmt.Errorf("send private message failed: encode request body: %w", err)
	}

	return c.sendMessageRequest(ctx, sendSingleChatEndpoint, reqBody, false, "send private message failed")
}

func (c *Client) sendGroupChatMessage(ctx context.Context, groupID string, message sendGroupChatBodyObject) (SendMessageResult, error) {
	if c == nil {
		return SendMessageResult{}, errors.New("send group chat message failed: client is nil")
	}
	if groupID == "" {
		return SendMessageResult{}, errors.New("send group chat message failed: group id is empty")
	}

	reqBody, err := json.Marshal(sendGroupChatRequest{
		GroupID: groupID,
		Message: message,
	})
	if err != nil {
		return SendMessageResult{}, fmt.Errorf("send group chat message failed: encode request body: %w", err)
	}

	return c.sendMessageRequest(ctx, sendGroupChatEndpoint, reqBody, true, "send group chat message failed")
}

func (c *Client) sendMessageRequest(ctx context.Context, endpoint string, reqBody []byte, requireMessageID bool, failurePrefix string) (SendMessageResult, error) {
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
		return SendMessageResult{}, fmt.Errorf("%s: get access token: %w", failurePrefix, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(reqBody))
	if err != nil {
		return SendMessageResult{}, fmt.Errorf("%s: build request: %w", failurePrefix, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req) //nolint:gosec // SeaTalk API endpoint is controlled by trusted configuration.
	if err != nil {
		return SendMessageResult{}, fmt.Errorf("%s: send request: %w", failurePrefix, err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return SendMessageResult{}, fmt.Errorf("%s: read response body: %w", failurePrefix, err)
	}
	if resp.StatusCode != http.StatusOK {
		return SendMessageResult{}, fmt.Errorf("%s: unexpected status %d: %s", failurePrefix, resp.StatusCode, string(respBody))
	}

	var apiResp sendGroupChatResponse
	if err := json.Unmarshal(respBody, &apiResp); err != nil {
		return SendMessageResult{}, fmt.Errorf("%s: decode response body: %w", failurePrefix, err)
	}
	// https://open.seatalk.io/docs/reference_server-api-error-code
	if apiResp.Code != 0 {
		if apiResp.Message != "" {
			return SendMessageResult{}, fmt.Errorf("%s: api returned code %d: %s", failurePrefix, apiResp.Code, apiResp.Message)
		}
		return SendMessageResult{}, fmt.Errorf("%s: api returned code %d", failurePrefix, apiResp.Code)
	}
	if requireMessageID && apiResp.MessageID == "" {
		return SendMessageResult{}, fmt.Errorf("%s: empty message_id in response", failurePrefix)
	}

	return SendMessageResult{
		MessageID: apiResp.MessageID,
	}, nil
}
