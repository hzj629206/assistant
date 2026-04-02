package agent

import (
	"context"
	"time"
)

// MessageKind identifies the normalized inbound message type.
type MessageKind string

const (
	MessageKindMixed           MessageKind = "mixed"
	MessageKindText            MessageKind = "text"
	MessageKindImage           MessageKind = "image"
	MessageKindFile            MessageKind = "file"
	MessageKindVideo           MessageKind = "video"
	MessageKindForwarded       MessageKind = "forwarded"
	MessageKindInteractiveCard MessageKind = "interactive_card"
)

// ReferencedMessage stores the normalized content of a quoted message.
type ReferencedMessage struct {
	// Kind identifies the normalized content type of the referenced message.
	Kind MessageKind
	// Sender identifies the original sender of the referenced message.
	Sender string
	// SentAtUnix stores the referenced message creation time as a Unix timestamp in seconds.
	SentAtUnix        int64
	Text              string
	ImagePath         string
	ImagePaths        []string
	FilePath          string
	FilePaths         []string
	VideoPath         string
	VideoPaths        []string
	ForwardedMessages []ReferencedMessage
}

// InboundMessage is a normalized user message routed from an adapter.
type InboundMessage struct {
	// ConversationKey identifies the source conversation used to load and persist conversation state.
	ConversationKey string
	// ID is the unique inbound event identifier used for deduplication and tracing.
	ID string
	// SentAtUnix stores the inbound message creation time as a Unix timestamp in seconds.
	SentAtUnix int64
	// Sender identifies the original sender of the inbound message.
	Sender string
	// SenderMentionHint stores the platform-specific mention token or display text to address the sender when the agent has already decided to reply.
	SenderMentionHint string
	// Responder delivers side effects such as replies, typing updates, and per-turn cleanup.
	Responder Responder
	// MessageTags stores structured per-message markers injected only for the current turn prompt.
	MessageTags []string
	// QuotedMessage stores the normalized quoted or replied-to message when available.
	QuotedMessage *ReferencedMessage
	// Kind identifies the normalized content type of the inbound message.
	Kind MessageKind
	// Text stores the normalized text body for text-like inbound content.
	Text string
	// ImagePath holds the local file system path to an image attachment in the inbound message.
	// Which is the requirement of `codex-sdk-go`.
	ImagePath string
	// ImagePaths holds additional local file system paths to image attachments in the inbound message.
	ImagePaths []string
	// FilePath holds the local file system path to a file attachment in the inbound message.
	FilePath string
	// FilePaths holds additional local file system paths to file attachments in the inbound message.
	FilePaths []string
	// VideoPath holds the local file system path to a video attachment in the inbound message.
	VideoPath string
	// VideoPaths holds additional local file system paths to video attachments in the inbound message.
	VideoPaths []string
	// ForwardedMessages stores the normalized message list extracted from combined forwarded chat history.
	ForwardedMessages []ReferencedMessage
	// LoadInitialContext lazily resolves supplemental context when the dispatcher sees a new conversation.
	LoadInitialContext func(context.Context) (string, error)
	// LoadInitialMessages lazily resolves earlier messages that should be prepended only for the first turn of a conversation.
	LoadInitialMessages func(context.Context) ([]InboundMessage, error)
	// initialContext stores supplemental context injected only for the first turn of a conversation.
	initialContext string
	// historicalMessages stores earlier conversation messages that are included only as context for the current turn.
	// When this field is non-empty, prompt construction reads the historical message content from these entries
	// instead of the top-level content fields on the container message.
	historicalMessages []InboundMessage
	// mergedMessages stores multiple inbound messages that should be processed in one turn, in arrival order.
	// When this field is non-empty, the top-level message acts as a batch container: prompt construction reads
	// message content, sender metadata, and reply mention hints from the nested messages, while top-level routing
	// metadata such as ConversationKey, ID, Responder, and InitialContext still applies to the combined turn.
	mergedMessages []InboundMessage
}

// InitialContext returns supplemental context injected only for the first turn of a conversation.
func (m InboundMessage) InitialContext() string {
	return m.initialContext
}

// HistoricalMessages returns earlier conversation messages included only as context for the current turn.
func (m InboundMessage) HistoricalMessages() []InboundMessage {
	if len(m.historicalMessages) == 0 {
		return nil
	}

	result := make([]InboundMessage, len(m.historicalMessages))
	copy(result, m.historicalMessages)
	return result
}

// MergedMessages returns inbound messages that should be processed in one combined turn.
func (m InboundMessage) MergedMessages() []InboundMessage {
	if len(m.mergedMessages) == 0 {
		return nil
	}

	result := make([]InboundMessage, len(m.mergedMessages))
	copy(result, m.mergedMessages)
	return result
}

// ConversationState stores the mapping between a source conversation and a Codex thread.
type ConversationState struct {
	Key            string    `json:"key"`
	CodexThreadID  string    `json:"codex_thread_id,omitempty"`
	LastEventID    string    `json:"last_event_id,omitempty"`
	LastActivityAt time.Time `json:"last_activity_at"`
}

// TurnRequest contains the state and new user input for one agent turn.
type TurnRequest struct {
	Conversation ConversationState
	Message      InboundMessage
}

// TurnResult contains the Codex thread mapping updates and final reply payload.
type TurnResult struct {
	CodexThreadID string
	ReplyText     string
}
