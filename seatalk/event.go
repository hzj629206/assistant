package seatalk

import "fmt"

// Event is a decoded SeaTalk event payload.
type Event interface {
	EventType() string
	String() string
}

// https://open.seatalk.io/docs/list-of-events
// Note: Updating messages won't trigger any event.
const (
	EventTypeVerification                     = "event_verification"
	EventTypeUserEnterChatroomWithBot         = "user_enter_chatroom_with_bot"
	EventTypeMessageFromBotSubscriber         = "message_from_bot_subscriber"
	EventTypeNewMentionedMessageFromGroupChat = "new_mentioned_message_received_from_group_chat"
	EventTypeInteractiveMessageClick          = "interactive_message_click"
	EventTypeNewMessageReceivedFromThread     = "new_message_received_from_thread"
	EventTypeBotAddedToGroupChat              = "bot_added_to_group_chat"
	EventTypeBotRemovedFromGroupChat          = "bot_removed_from_group_chat"
)

var eventFactories = map[string]func() Event{
	EventTypeVerification:                     func() Event { return &VerificationEvent{} },
	EventTypeUserEnterChatroomWithBot:         func() Event { return &UserEnterChatroomWithBotEvent{} },
	EventTypeMessageFromBotSubscriber:         func() Event { return &MessageFromBotSubscriberEvent{} },
	EventTypeNewMentionedMessageFromGroupChat: func() Event { return &NewMentionedMessageReceivedFromGroupChatEvent{} },
	EventTypeInteractiveMessageClick:          func() Event { return &InteractiveMessageClickEvent{} },
	EventTypeNewMessageReceivedFromThread:     func() Event { return &NewMessageReceivedFromThreadEvent{} },
	EventTypeBotAddedToGroupChat:              func() Event { return &BotAddedToGroupChatEvent{} },
	EventTypeBotRemovedFromGroupChat:          func() Event { return &BotRemovedFromGroupChatEvent{} },
}

type VerificationEvent struct {
	SeatalkChallenge string `json:"seatalk_challenge"`
}

// EventType returns the SeaTalk event type.
func (*VerificationEvent) EventType() string {
	return EventTypeVerification
}

// String returns the log summary for the verification event.
func (e *VerificationEvent) String() string {
	return fmt.Sprintf("seatalk_challenge=%q", e.SeatalkChallenge)
}

// UserEnterChatroomWithBotEvent is sent when a user enters a 1-on-1 chatroom with the bot from any entry point.
// API doc: https://open.seatalk.io/docs/user_enter_chatroom_with_bot
// Note:
//   - No event callback will be sent if the user enters the chatroom while bot is not online.
//   - No event callback will be sent when user returns to SeaTalk from another app/window unless the user actively re-enters the chatroom.
type UserEnterChatroomWithBotEvent struct {
	// The employee_code of the user
	EmployeeCode string `json:"employee_code"`
	// The SeaTalk ID of the user enters the chatroom
	SeatalkID string `json:"seatalk_id"`
	// The email of the user enters the chatroom
	Email string `json:"email"`
}

// EventType returns the SeaTalk event type.
func (*UserEnterChatroomWithBotEvent) EventType() string {
	return EventTypeUserEnterChatroomWithBot
}

// String returns the log summary for the user-enter-chatroom event.
func (e *UserEnterChatroomWithBotEvent) String() string {
	return fmt.Sprintf("seatalk_id=%s employee_code=%s email=%s", e.SeatalkID, e.EmployeeCode, e.Email)
}

// MessageFromBotSubscriberEvent is sent when a bot user sends a 1-on-1 message (include thread messages).
// API doc: https://open.seatalk.io/docs/event_message_received_from_bot_subscriber
// Plain text, image, file, video, and combined forwarded message types are supported in this event.
//
// Images, files and videos sent to bots can be downloaded with the URL starting with https://openapi.seatalk.io/messaging/v2/file/.
// For more information, please refer to [this document](https://open.seatalk.io/docs/Introduction-to-Received-Message-Types).
// The rate limit for this endpoint is 100 requests/min.
type MessageFromBotSubscriberEvent struct {
	// The SeaTalk ID of the message sender
	SeatalkID string `json:"seatalk_id"`
	// The employee_code of the message sender
	EmployeeCode string `json:"employee_code"`
	// The email of the message sender
	// Return empty if the message sender does not belong to the same org as bot.
	Email string `json:"email"`
	// The message received
	Message struct {
		// The ID of the message
		// Note: For security reasons, a single message will have different message_ids when accessed by different apps.
		MessageID string `json:"message_id"`
		// The ID of the quoted message (If the message has quoted any message)
		// Notes:
		//   - Only quoting messages sent within the last 7 days is supported.
		//   - For security reasons, a single message will have different message_ids when accessed by different apps.
		QuotedMessageID string `json:"quoted_message_id"`
		// The ID of the thread this message belongs to.
		ThreadID string `json:"thread_id"`
		// The message type.
		// Allowed tags: "text", "image", "file", "video", "combined_forwarded_chat_history"
		// For more details on supported message types for bots, refer to [this document](https://open.seatalk.io/docs/introduction-to-received-message-types).
		Tag string `json:"tag"`
		// The text message object
		Text struct {
			// The text message content
			Content string `json:"content"`
			// Return the message's most recent edit time
			// Return 0 if the message is not edited
			LastEditedTime int64 `json:"last_edited_time"`
		} `json:"text"`
		// The image object
		Image struct {
			// The URL of the image. Requires a valid API token to access.
			// The image message expires in 7 days and cannot be downloaded using the URL subsequently.
			// Max: 250 MB
			Content string `json:"content"`
		} `json:"image"`
		// The file object
		File struct {
			// The URL of the file. Requires a valid API token to access.
			// The file message expires in 7 days and cannot be downloaded using the URL subsequently
			// Max: 250 MB
			Content string `json:"content"`
			// The file name with extension; files with no extension specified will be sent as unidentified files
			// Max: 100 characters
			Filename string `json:"filename"`
		} `json:"file"`
		// The video object
		Video struct {
			// The URL of the video. Requires a valid API token to access.
			// The video message expires in 7 days and cannot be downloaded using the URL subsequently.
			// Max: 250 MB
			Content string `json:"content"`
		} `json:"video"`
		// The combined forwarded chat history object
		CombinedForwardedChatHistory *CombinedForwardedChatHistoryMessage `json:"combined_forwarded_chat_history"`
	} `json:"message"`
}

// EventType returns the SeaTalk event type.
func (*MessageFromBotSubscriberEvent) EventType() string {
	return EventTypeMessageFromBotSubscriber
}

// String returns the log summary for the private message event.
func (e *MessageFromBotSubscriberEvent) String() string {
	return fmt.Sprintf("message_id=%s sender=%s", e.Message.MessageID, e.SeatalkID)
}

// NewMentionedMessageReceivedFromGroupChatEvent is sent when the bot is mentioned in a group chat.
// https://open.seatalk.io/docs/event_new_mentioned_message_from_group_chat
type NewMentionedMessageReceivedFromGroupChatEvent struct {
	// The ID of the group chat which the bot receives the mentioned message from
	GroupID string `json:"group_id"`
	// The message received
	Message struct {
		// The ID of the message
		// Note: For security reasons, a single message will have different message_ids when accessed by different apps.
		MessageID string `json:"message_id"`
		// The ID of the quoted message (If the message has quoted any message)
		// Note: For security reasons, a single message will have different message_ids when accessed by different apps.
		QuotedMessageID string `json:"quoted_message_id"`
		// The ID of the thread. Provided if the message is part of a thread.
		ThreadID string `json:"thread_id"`
		// Information of the message sender
		Sender struct {
			// The SeaTalk ID of the message sender
			SeatalkID string `json:"seatalk_id"`
			// The employee_code of the message sender
			// It will return empty if the message sender and the bot do not belong to the same organization.
			EmployeeCode string `json:"employee_code"`
			// The email of the message sender
			// It will return empty if the message sender and the bot do not belong to the same organization.
			Email string `json:"email"`
			// 1: User, 2: Bot, 3: System Account
			SenderType int `json:"sender_type"`
		} `json:"sender"`
		// The time when the message was sent
		MessageSentTime int64 `json:"message_sent_time"`
		// The type of the message.
		Tag string `json:"tag"`
		// The text object
		Text struct {
			// Message content in the plain text format
			// Refer to [this document](https://open.seatalk.io/docs/introduction-to-received-message-types) to
			// understand the details on how plain text format is converted if the original message is formatted.
			PlainText string `json:"plain_text"`
			// Return the message's most recent edit time
			// Return 0 if the message is not edited
			LastEditedTime int64 `json:"last_edited_time"`
			// List of mappings between usernames and SeaTalk ID of users or bots
			MentionedList []struct {
				// Mention specific user or bot: User's current username inserted into the plain_text message content
				// Mention all: Empty
				Username string `json:"username"`
				// Mention specific user or bot: SeaTalk ID
				// Mention all: 0
				SeatalkID string `json:"seatalk_id"`
				// Mention specific user: The employee_code of the mentioned user
				// Mention specific bot:  Empty
				// Mention all: Empty
				EmployeeCode string `json:"employee_code"`
				// Mention specific user: The email of the mentioned user
				// Mention specific bot:  Empty
				// Mention all: Empty
				Email string `json:"email"`
			} `json:"mentioned_list"`
		} `json:"text"`
		// The interactive card payload when Tag is "interactive_message".
		InteractiveMessage *ThreadInteractiveMessage `json:"interactive_message"`
	} `json:"message"`
}

// EventType returns the SeaTalk event type.
func (*NewMentionedMessageReceivedFromGroupChatEvent) EventType() string {
	return EventTypeNewMentionedMessageFromGroupChat
}

// String returns the log summary for the mentioned group message event.
func (e *NewMentionedMessageReceivedFromGroupChatEvent) String() string {
	return fmt.Sprintf(
		"message_id=%s sender=%s thread_id=%s group_id=%s",
		e.Message.MessageID,
		e.Message.Sender.SeatalkID,
		e.Message.ThreadID,
		e.GroupID,
	)
}

// NewMessageReceivedFromThreadEvent is sent when a new message is posted in a subscribed group chat thread.
// https://open.seatalk.io/docs/new_message_received_from_thread
type NewMessageReceivedFromThreadEvent struct {
	// The ID of the group chat which bot has received the mentioned message from.
	GroupID string `json:"group_id"`
	// The message received
	Message struct {
		// The message ID
		// Note: For security reasons, a single message will have different message_ids when accessed by different apps.
		MessageID string `json:"message_id"`
		// Return only when the message has quoted another message. The message ID of the quoted message.
		QuotedMessageID string `json:"quoted_message_id"`
		// The ID of the thread. Provided if the message is part of a thread.
		ThreadID string `json:"thread_id"`
		Sender   struct {
			// The SeaTalk ID of the message sender (if the sender is a bot, it will be the seatalk_id of the bot)
			SeatalkID string `json:"seatalk_id"`
			// The employee code of the message sender. If the sender is a bot, employee_code will be empty.
			// Return only when the message sender belongs to the same org as bot.
			EmployeeCode string `json:"employee_code"`
			// The email of the message sender
			Email string `json:"email"`
			// 1: User, 2: Bot, 3: System Account
			SenderType int `json:"sender_type"`
		} `json:"sender"`
		// The time when the message is sent.
		MessageSentTime int64 `json:"message_sent_time"`
		// The message type.
		// Allowed tags: "text", "combined_forwarded_chat_history", "image", "file", "video"
		// For more details on supported message types for bots, refer to [this document](https://open.seatalk.io/docs/introduction-to-received-message-types).
		Tag string `json:"tag"`
		// The text message object
		Text struct {
			// The text message content
			PlainText string `json:"plain_text"`
			// Return the message's most recent edit time
			// Return 0 if the message is not edited
			LastEditedTime int64 `json:"last_edited_time"`
			// The list of user info of users being mentioned in the message.
			MentionedList []struct {
				// User's current username inserted into the plain text content
				Username string `json:"username"`
				// Mention specific user: User's SeaTalk ID.
				// Mention all: 0
				SeatalkID string `json:"seatalk_id"`
				// Mention specific user: The employee_code of the mentioned user
				// Mention specific bot:  Empty
				// Mention all: Empty
				EmployeeCode string `json:"employee_code"`
				// Mention specific user: The email of the mentioned user
				// Mention specific bot:  Empty
				// Mention all: Empty
				Email string `json:"email"`
			} `json:"mentioned_list"`
		} `json:"text"`
		CombinedForwardedChatHistory *CombinedForwardedChatHistoryMessage `json:"combined_forwarded_chat_history"`
		// The image object
		Image struct {
			// The URL of the image. Requires a valid API token to access.
			// The image message expires in 7 days and cannot be downloaded using the URL subsequently.
			Content string `json:"content"`
		} `json:"image"`
		// The file object
		File struct {
			// The URL of the file. Requires a valid API token to access.
			// The file message expires in 7 days and cannot be downloaded using the URL subsequently
			Content string `json:"content"`
			// The file name with extension; files with no extension specified will be sent as unidentified files
			Filename string `json:"filename"`
		} `json:"file"`
		// The video object
		Video struct {
			// The URL of the video. Requires a valid API token to access.
			// The video message expires in 7 days and cannot be downloaded using the URL subsequently.
			Content string `json:"content"`
		} `json:"video"`
		InteractiveMessage *ThreadInteractiveMessage `json:"interactive_message"`
	} `json:"message"`
}

// EventType returns the SeaTalk event type.
func (*NewMessageReceivedFromThreadEvent) EventType() string {
	return EventTypeNewMessageReceivedFromThread
}

// String returns the log summary for the thread message event.
func (e *NewMessageReceivedFromThreadEvent) String() string {
	return fmt.Sprintf(
		"message_id=%s sender=%s thread_id=%s group_id=%s",
		e.Message.MessageID,
		e.Message.Sender.SeatalkID,
		e.Message.ThreadID,
		e.GroupID,
	)
}

// InteractiveMessageClickEvent is sent when a user clicks a callback button in an interactive card.
// https://open.seatalk.io/docs/event_interactive_message_click
type InteractiveMessageClickEvent struct {
	MessageID    string `json:"message_id"`
	EmployeeCode string `json:"employee_code"`
	Email        string `json:"email"`
	// The callback value of the button clicked
	Value     string `json:"value"`
	SeatalkID string `json:"seatalk_id"`
	GroupID   string `json:"group_id"`
	ThreadID  string `json:"thread_id"`
}

// EventType returns the SeaTalk event type.
func (*InteractiveMessageClickEvent) EventType() string {
	return EventTypeInteractiveMessageClick
}

// String returns the log summary for the interactive callback event.
func (e *InteractiveMessageClickEvent) String() string {
	return fmt.Sprintf(
		"message_id=%s seatalk_id=%s employee_code=%s group_id=%s thread_id=%s value=%q",
		e.MessageID,
		e.SeatalkID,
		e.EmployeeCode,
		e.GroupID,
		e.ThreadID,
		e.Value,
	)
}

// BotAddedToGroupChatEvent is sent when the bot is added to a group chat.
// https://open.seatalk.io/docs/event-bot-added-to-group-chat
type BotAddedToGroupChatEvent struct {
	// Information of the group chat which the bot is added to
	Group struct {
		// The ID of the group chat
		GroupID string `json:"group_id"`
		// Group name when the bot is added
		GroupName string `json:"group_name"`
		// Current group settings when the bot is added
		GroupSettings struct {
			// The extent to which the bot can access the chat histories sent prior to joining.
			// Possible values are "disabled", "1 day" and "7 days".
			ChatHistoryForNewMembers string `json:"chat_history_for_new_members"`
			// Whether group members are allowed to notify all group members with '@All'.
			CanNotifyWithAtAll bool `json:"can_notify_with_at_all"`
			// Whether group members are allowed to view the group member list
			CanViewMemberList bool `json:"can_view_member_list"`
		} `json:"group_settings"`
	} `json:"group"`
	Inviter struct {
		// The SeaTalk ID of the user who has added the bot to the group chat
		SeatalkID string `json:"seatalk_id"`
		// The employee_code of the user who has added the bot to the group chat
		EmployeeCode string `json:"employee_code"`
		// The email of the user who added the bot to the group chat
		Email string `json:"email"`
	} `json:"inviter"`
}

// EventType returns the SeaTalk event type.
func (*BotAddedToGroupChatEvent) EventType() string {
	return EventTypeBotAddedToGroupChat
}

// String returns the log summary for the bot-added event.
func (e *BotAddedToGroupChatEvent) String() string {
	return fmt.Sprintf(
		"group_id=%s group_name=%q inviter_seatalk_id=%s inviter_email=%s",
		e.Group.GroupID,
		e.Group.GroupName,
		e.Inviter.SeatalkID,
		e.Inviter.Email,
	)
}

// BotRemovedFromGroupChatEvent is sent when the bot is removed from a group chat.
// https://open.seatalk.io/docs/Event-Bot-Removed-From-Group-Chat
type BotRemovedFromGroupChatEvent struct {
	// The ID of the group chat
	GroupID string `json:"group_id"`
	Remover struct {
		// The SeaTalk ID of the remover
		SeatalkID string `json:"seatalk_id"`
		// The employee_code of the remover
		// Return empty if the remover and the bot do not belong to the same organization
		EmployeeCode string `json:"employee_code"`
		// The email of the remover
		// Return empty when the user and the bot do not belong to the same organization.
		Email string `json:"email"`
	} `json:"remover"`
}

// EventType returns the SeaTalk event type.
func (*BotRemovedFromGroupChatEvent) EventType() string {
	return EventTypeBotRemovedFromGroupChat
}

// String returns the log summary for the bot-removed event.
func (e *BotRemovedFromGroupChatEvent) String() string {
	return fmt.Sprintf(
		"group_id=%s remover_seatalk_id=%s remover_email=%s",
		e.GroupID,
		e.Remover.SeatalkID,
		e.Remover.Email,
	)
}

// VerificationResponse builds the callback response required by SeaTalk verification requests.
func VerificationResponse(e *VerificationEvent) map[string]string {
	return map[string]string{
		"seatalk_challenge": e.SeatalkChallenge,
	}
}
