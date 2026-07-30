package seatalkoapisdk

import (
	"encoding/json"
	"fmt"
	"time"
)

const (
	DefaultWebSocketURL = "wss://ws-openapi.haiserve.com/ws/bot"
)

type Command string

const (
	CommandRegister Command = "register"
	CommandAck      Command = "ack"
	CommandPing     Command = "ping"
	CommandPong     Command = "pong"
	CommandKick     Command = "kick"
	CommandEvent    Command = "event"
)

const (
	EventTypeUserEnterChatroomWithBot                 = "user_enter_chatroom_with_bot"
	EventTypeMessageFromBotSubscriber                 = "message_from_bot_subscriber"
	EventTypeNewMentionedMessageReceivedFromGroupChat = "new_mentioned_message_received_from_group_chat"
	EventTypeNewMessageReceivedFromGroupChat          = "new_message_received_from_group_chat"
	EventTypeInteractiveMessageClick                  = "interactive_message_click"
	EventTypeNewMessageReceivedFromThread             = "new_message_received_from_thread"
	EventTypeBotAddedToGroupChat                      = "bot_added_to_group_chat"
	EventTypeBotRemovedFromGroupChat                  = "bot_removed_from_group_chat"
	EventTypeGroupChatConvertedToExternalGroup        = "group_chat_converted_to_external_group"
)

const (
	MessageTagText                         = "text"
	MessageTagCombinedForwardedChatHistory = "combined_forwarded_chat_history"
	MessageTagInteractiveMessage           = "interactive_message"
	MessageTagImage                        = "image"
	MessageTagFile                         = "file"
	MessageTagVideo                        = "video"
	MessageTagChangeMembers                = "change_members"
	MessageTagRecallMsgs                   = "recall_msgs"
	MessageTagGroupRemoved                 = "group_removed"
	MessageTagEdit                         = "edit"
)

const (
	CodeOK    = 0
	CodeError = 1
)

type Header struct {
	AppID      string `json:"app_id,omitempty"`
	AppSecret  string `json:"app_secret,omitempty"`
	Token      string `json:"token,omitempty"`
	Sid        string `json:"sid,omitempty"`
	CallbackID string `json:"callback_id,omitempty"`
	Rid        string `json:"rid,omitempty"`
}

type Envelope struct {
	Cmd     Command         `json:"cmd"`
	Header  Header          `json:"header,omitempty"`
	Data    json.RawMessage `json:"data,omitempty"`
	Code    int             `json:"code,omitempty"`
	Message string          `json:"message,omitempty"`
}

type RegisterResult struct {
	AppID             string
	Token             string
	Sid               string
	HeartbeatInterval time.Duration
	HeartbeatTimeout  time.Duration
}

type RegisterSettings struct {
	HeartbeatInterval int `json:"heartbeat_interval"`
	HeartbeatTimeout  int `json:"heartbeat_timeout"`
}

type Event struct {
	AppID      string
	CallbackID string
	Rid        string
	Sid        string
	Data       json.RawMessage
}

type UserEnterChatroomWithBotEvent struct {
	CallbackID string                              `json:"-"`
	EventID    string                              `json:"event_id"`
	EventType  string                              `json:"event_type"`
	Timestamp  int64                               `json:"timestamp"`
	AppID      string                              `json:"app_id"`
	Event      UserEnterChatroomWithBotEventDetail `json:"event"`
}

type UserEnterChatroomWithBotEventDetail struct {
	EmployeeCode string `json:"employee_code"`
	SeaTalkID    string `json:"seatalk_id"`
	Email        string `json:"email"`
}

type MessageFromBotSubscriberEvent struct {
	CallbackID string                              `json:"-"`
	EventID    string                              `json:"event_id"`
	EventType  string                              `json:"event_type"`
	Timestamp  int64                               `json:"timestamp"`
	AppID      string                              `json:"app_id"`
	Event      MessageFromBotSubscriberEventDetail `json:"event"`
}

type MessageFromBotSubscriberEventDetail struct {
	SeaTalkID    string               `json:"seatalk_id"`
	EmployeeCode string               `json:"employee_code"`
	Email        string               `json:"email"`
	Message      BotSubscriberMessage `json:"message"`
}

type BotSubscriberMessage struct {
	MessageID                    string                                  `json:"message_id"`
	QuotedMessageID              string                                  `json:"quoted_message_id"`
	Tag                          string                                  `json:"tag"`
	Text                         *TextMessage                            `json:"text,omitempty"`
	ThreadID                     string                                  `json:"thread_id"`
	Image                        *MediaMessage                           `json:"image,omitempty"`
	Video                        *MediaMessage                           `json:"video,omitempty"`
	File                         *FileMessage                            `json:"file,omitempty"`
	CombinedForwardedChatHistory *SingleChatCombinedForwardedChatHistory `json:"combined_forwarded_chat_history,omitempty"`
}

type TextMessage struct {
	Content string `json:"content"`
}

type MediaMessage struct {
	Content string `json:"content"`
}

type FileMessage struct {
	Content  string `json:"content"`
	Filename string `json:"filename"`
}

type SingleChatCombinedForwardedChatHistory struct {
	Content []*SingleChatForwardedMessage `json:"content,omitempty"`
}

type SingleChatForwardedMessage struct {
	Tag                          string                                  `json:"tag"`
	Sender                       *SenderBrief                            `json:"sender,omitempty"`
	MessageSentTime              int64                                   `json:"message_sent_time"`
	Text                         *TextMessage                            `json:"text,omitempty"`
	Image                        *MediaMessage                           `json:"image,omitempty"`
	Video                        *MediaMessage                           `json:"video,omitempty"`
	File                         *FileMessage                            `json:"file,omitempty"`
	CombinedForwardedChatHistory *SingleChatCombinedForwardedChatHistory `json:"combined_forwarded_chat_history,omitempty"`
}

type NewMentionedMessageReceivedFromGroupChatEvent struct {
	CallbackID string                                            `json:"-"`
	EventID    string                                            `json:"event_id"`
	EventType  string                                            `json:"event_type"`
	Timestamp  int64                                             `json:"timestamp"`
	AppID      string                                            `json:"app_id"`
	Event      NewMentionedMessageReceivedFromGroupChatEventData `json:"event"`
}

type NewMentionedMessageReceivedFromGroupChatEventData struct {
	GroupID string           `json:"group_id"`
	Message GroupChatMessage `json:"message"`
}

type MentionedGroupChatMessage = GroupChatMessage

type MentionedGroupChatSender = SenderBrief

type MentionedGroupChatText = GroupChatMessageText

type MentionedGroupChatActor = GroupChatMessageMentionUser

type NewMessageReceivedFromGroupChatEvent struct {
	CallbackID string                                   `json:"-"`
	EventID    string                                   `json:"event_id"`
	EventType  string                                   `json:"event_type"`
	Timestamp  int64                                    `json:"timestamp"`
	AppID      string                                   `json:"app_id"`
	Event      NewMessageReceivedFromGroupChatEventData `json:"event"`
}

type NewMessageReceivedFromGroupChatEventData struct {
	GroupID string           `json:"group_id"`
	Message GroupChatMessage `json:"message"`
}

type GroupChatMessage struct {
	MessageID                    string                                 `json:"message_id"`
	QuotedMessageID              string                                 `json:"quoted_message_id"`
	ThreadID                     string                                 `json:"thread_id"`
	Sender                       SenderBrief                            `json:"sender"`
	MessageSentTime              int64                                  `json:"message_sent_time"`
	Tag                          string                                 `json:"tag"`
	Text                         *GroupChatMessageText                  `json:"text,omitempty"`
	InteractiveMessage           *GroupChatInteractiveMessage           `json:"interactive_message,omitempty"`
	Image                        *MessageImage                          `json:"image,omitempty"`
	Video                        *MessageVideo                          `json:"video,omitempty"`
	File                         *MessageFile                           `json:"file,omitempty"`
	CombinedForwardedChatHistory *GroupChatCombinedForwardedChatHistory `json:"combined_forwarded_chat_history,omitempty"`
	ChangeMembers                *MessageChangeMembers                  `json:"change_members,omitempty"`
	RecallMsgs                   *MessageRecallMsgs                     `json:"recall_msgs,omitempty"`
	GroupRemoved                 *MessageGroupRemoved                   `json:"group_removed,omitempty"`
	Edit                         *MessageEdit                           `json:"edit,omitempty"`
}

type SenderBrief struct {
	SeaTalkID    string `json:"seatalk_id"`
	EmployeeCode string `json:"employee_code"`
	Email        string `json:"email"`
	SenderType   int    `json:"sender_type"`
}

type GroupChatMessageText struct {
	PlainText     string                         `json:"plain_text"`
	MentionedList []*GroupChatMessageMentionUser `json:"mentioned_list"`
}

type GroupChatMessageMentionUser struct {
	Username     string  `json:"username"`
	SeaTalkID    string  `json:"seatalk_id"`
	EmployeeCode string  `json:"employee_code"`
	Email        string  `json:"email"`
	Location     *uint32 `json:"location,omitempty"`
	Length       *uint32 `json:"length,omitempty"`
}

type GroupChatInteractiveMessage struct {
	Elements      []*GroupChatInteractiveElement `json:"elements"`
	MentionedList []*GroupChatMessageMentionUser `json:"mentioned_list,omitempty"`
}

type GroupChatInteractiveElement struct {
	ElementType string                           `json:"element_type"`
	Title       *GroupChatInteractiveTitle       `json:"title,omitempty"`
	Description *GroupChatInteractiveDescription `json:"description,omitempty"`
	Button      *GroupChatInteractiveButton      `json:"button,omitempty"`
	ButtonGroup []*GroupChatInteractiveButton    `json:"button_group,omitempty"`
	Image       *MessageImage                    `json:"image,omitempty"`
}

type GroupChatInteractiveTitle struct {
	Text string `json:"text"`
}

type GroupChatInteractiveDescription struct {
	Text   string `json:"text"`
	Format int32  `json:"format,omitempty"`
}

type GroupChatInteractiveButton struct {
	ButtonType  string                          `json:"button_type"`
	Text        string                          `json:"text"`
	MobileLink  *GroupChatInteractiveButtonLink `json:"mobile_link,omitempty"`
	DesktopLink *GroupChatInteractiveButtonLink `json:"desktop_link,omitempty"`
}

type GroupChatInteractiveButtonLink struct {
	Type   string            `json:"type,omitempty"`
	Path   string            `json:"path,omitempty"`
	Params map[string]string `json:"params,omitempty"`
}

type MessageImage = MediaMessage

type MessageVideo = MediaMessage

type MessageFile = FileMessage

type GroupChatCombinedForwardedChatHistory struct {
	Content []*GroupChatForwardedMessage `json:"content,omitempty"`
}

type GroupChatForwardedMessage struct {
	Tag                          string                                 `json:"tag"`
	Sender                       *SenderBrief                           `json:"sender,omitempty"`
	MessageSentTime              int64                                  `json:"message_sent_time"`
	Text                         *GroupChatMessageText                  `json:"text,omitempty"`
	Image                        *MessageImage                          `json:"image,omitempty"`
	Video                        *MessageVideo                          `json:"video,omitempty"`
	File                         *MessageFile                           `json:"file,omitempty"`
	CombinedForwardedChatHistory *GroupChatCombinedForwardedChatHistory `json:"combined_forwarded_chat_history,omitempty"`
}

type MessageChangeMembers struct {
	AddedMembers   []UserBriefV2 `json:"added_members"`
	RemovedMembers []UserBriefV2 `json:"removed_members"`
	IsNewlyCreated bool          `json:"is_newly_created"`
}

type UserBriefV2 struct {
	SeaTalkID    string `json:"seatalk_id"`
	EmployeeCode string `json:"employee_code"`
	Email        string `json:"email"`
}

type MessageBrief struct {
	MessageID string `json:"message_id"`
	ThreadID  string `json:"thread_id"`
}

type MessageRecallMsgs struct {
	Messages []*MessageBrief `json:"messages"`
}

type MessageGroupRemoved struct {
	OpUser UserBriefV2 `json:"op_user"`
}

type MessageEdit struct {
	Message GroupChatMessage `json:"message"`
}

type InteractiveMessageClickEvent struct {
	CallbackID string                             `json:"-"`
	EventID    string                             `json:"event_id"`
	EventType  string                             `json:"event_type"`
	Timestamp  int64                              `json:"timestamp"`
	AppID      string                             `json:"app_id"`
	Event      InteractiveMessageClickEventDetail `json:"event"`
}

type InteractiveMessageClickEventDetail struct {
	MessageID    string `json:"message_id"`
	EmployeeCode string `json:"employee_code"`
	Email        string `json:"email"`
	Value        string `json:"value"`
	SeaTalkID    string `json:"seatalk_id"`
	GroupID      string `json:"group_id"`
	ThreadID     string `json:"thread_id"`
}

type NewMessageReceivedFromThreadEvent struct {
	CallbackID string                                `json:"-"`
	EventID    string                                `json:"event_id"`
	EventType  string                                `json:"event_type"`
	Timestamp  int64                                 `json:"timestamp"`
	AppID      string                                `json:"app_id"`
	Event      NewMessageReceivedFromThreadEventData `json:"event"`
}

type NewMessageReceivedFromThreadEventData struct {
	GroupID string           `json:"group_id"`
	Message GroupChatMessage `json:"message"`
}

type ThreadMessage = GroupChatMessage

type ThreadMessageSender = SenderBrief

type ThreadTextMessage = GroupChatMessageText

type ThreadMentionedActor = GroupChatMessageMentionUser

type BotAddedToGroupChatEvent struct {
	CallbackID string                       `json:"-"`
	EventID    string                       `json:"event_id"`
	EventType  string                       `json:"event_type"`
	Timestamp  int64                        `json:"timestamp"`
	AppID      string                       `json:"app_id"`
	Event      BotAddedToGroupChatEventData `json:"event"`
}

type BotAddedToGroupChatEventData struct {
	Group   BotAddedGroupChat `json:"group"`
	Inviter BotAddedInviter   `json:"inviter"`
}

type BotAddedGroupChat struct {
	GroupID       string                `json:"group_id"`
	GroupName     string                `json:"group_name"`
	GroupSettings BotAddedGroupSettings `json:"group_settings"`
	External      bool                  `json:"external"`
}

type BotAddedGroupSettings struct {
	ChatHistoryForNewMembers string `json:"chat_history_for_new_members"`
	CanNotifyWithAtAll       bool   `json:"can_notify_with_at_all"`
	CanViewMemberList        bool   `json:"can_view_member_list"`
}

type BotAddedInviter struct {
	SeaTalkID    string `json:"seatalk_id"`
	EmployeeCode string `json:"employee_code"`
	Email        string `json:"email"`
}

type BotRemovedFromGroupChatEvent struct {
	CallbackID string                         `json:"-"`
	EventID    string                         `json:"event_id"`
	EventType  string                         `json:"event_type"`
	Timestamp  int64                          `json:"timestamp"`
	AppID      string                         `json:"app_id"`
	Event      BotRemovedFromGroupChatDetails `json:"event"`
}

type BotRemovedFromGroupChatDetails struct {
	GroupID string            `json:"group_id"`
	Remover BotRemovedRemover `json:"remover"`
}

type BotRemovedRemover struct {
	SeaTalkID    string `json:"seatalk_id"`
	EmployeeCode string `json:"employee_code"`
	Email        string `json:"email"`
}

type GroupChatConvertedToExternalGroupEvent struct {
	CallbackID string                                     `json:"-"`
	EventID    string                                     `json:"event_id"`
	EventType  string                                     `json:"event_type"`
	Timestamp  int64                                      `json:"timestamp"`
	AppID      string                                     `json:"app_id"`
	Event      GroupChatConvertedToExternalGroupEventData `json:"event"`
}

type GroupChatConvertedToExternalGroupEventData struct {
	GroupID  string                                    `json:"group_id"`
	Operator GroupChatConvertedToExternalGroupOperator `json:"operator"`
}

type GroupChatConvertedToExternalGroupOperator struct {
	SeaTalkID    string `json:"seatalk_id"`
	EmployeeCode string `json:"employee_code"`
	Email        string `json:"email"`
}

type RegisterError struct {
	Code    int
	Message string
}

func (e *RegisterError) Error() string {
	return fmt.Sprintf("register rejected: code=%d msg=%s", e.Code, e.Message)
}

type KickError struct {
	Message  string
	Envelope Envelope
}

func (e *KickError) Error() string {
	if e.Message == "" {
		return "kicked"
	}
	return "kicked: " + e.Message
}
