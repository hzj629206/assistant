package seatalkoapisdk

import (
	"context"
	"encoding/json"
	"fmt"
)

type EventHandler func(ctx context.Context, event *Event) error
type UserEnterChatroomWithBotHandler func(ctx context.Context, event *UserEnterChatroomWithBotEvent) error
type MessageFromBotSubscriberHandler func(ctx context.Context, event *MessageFromBotSubscriberEvent) error
type NewMentionedMessageReceivedFromGroupChatHandler func(ctx context.Context, event *NewMentionedMessageReceivedFromGroupChatEvent) error
type NewMessageReceivedFromGroupChatHandler func(ctx context.Context, event *NewMessageReceivedFromGroupChatEvent) error
type InteractiveMessageClickHandler func(ctx context.Context, event *InteractiveMessageClickEvent) error
type NewMessageReceivedFromThreadHandler func(ctx context.Context, event *NewMessageReceivedFromThreadEvent) error
type BotAddedToGroupChatHandler func(ctx context.Context, event *BotAddedToGroupChatEvent) error
type BotRemovedFromGroupChatHandler func(ctx context.Context, event *BotRemovedFromGroupChatEvent) error
type GroupChatConvertedToExternalGroupHandler func(ctx context.Context, event *GroupChatConvertedToExternalGroupEvent) error
type EnvelopeHandler func(ctx context.Context, envelope *Envelope) error
type InvalidFrameHandler func(ctx context.Context, payload []byte, err error) error

type EventDispatcher struct {
	onEvent                                    EventHandler
	onUserEnterChatroomWithBot                 UserEnterChatroomWithBotHandler
	onMessageFromBotSubscriber                 MessageFromBotSubscriberHandler
	onNewMentionedMessageReceivedFromGroupChat NewMentionedMessageReceivedFromGroupChatHandler
	onNewMessageReceivedFromGroupChat          NewMessageReceivedFromGroupChatHandler
	onInteractiveMessageClick                  InteractiveMessageClickHandler
	onNewMessageReceivedFromThread             NewMessageReceivedFromThreadHandler
	onBotAddedToGroupChat                      BotAddedToGroupChatHandler
	onBotRemovedFromGroupChat                  BotRemovedFromGroupChatHandler
	onGroupChatConvertedToExternalGroup        GroupChatConvertedToExternalGroupHandler
	onKick                                     EnvelopeHandler
	onEnvelope                                 EnvelopeHandler
	onInvalidFrame                             InvalidFrameHandler
}

func NewEventDispatcher() *EventDispatcher {
	return &EventDispatcher{
		onEnvelope:     defaultEnvelopeHandler,
		onInvalidFrame: defaultInvalidFrameHandler,
	}
}

func (d *EventDispatcher) OnEvent(handler EventHandler) *EventDispatcher {
	d.onEvent = handler
	return d
}

func (d *EventDispatcher) OnUserEnterChatroomWithBot(handler UserEnterChatroomWithBotHandler) *EventDispatcher {
	d.onUserEnterChatroomWithBot = handler
	return d
}

func (d *EventDispatcher) OnMessageFromBotSubscriber(handler MessageFromBotSubscriberHandler) *EventDispatcher {
	d.onMessageFromBotSubscriber = handler
	return d
}

func (d *EventDispatcher) OnNewMentionedMessageReceivedFromGroupChat(handler NewMentionedMessageReceivedFromGroupChatHandler) *EventDispatcher {
	d.onNewMentionedMessageReceivedFromGroupChat = handler
	return d
}

func (d *EventDispatcher) OnNewMessageReceivedFromGroupChat(handler NewMessageReceivedFromGroupChatHandler) *EventDispatcher {
	d.onNewMessageReceivedFromGroupChat = handler
	return d
}

func (d *EventDispatcher) OnInteractiveMessageClick(handler InteractiveMessageClickHandler) *EventDispatcher {
	d.onInteractiveMessageClick = handler
	return d
}

func (d *EventDispatcher) OnNewMessageReceivedFromThread(handler NewMessageReceivedFromThreadHandler) *EventDispatcher {
	d.onNewMessageReceivedFromThread = handler
	return d
}

func (d *EventDispatcher) OnBotAddedToGroupChat(handler BotAddedToGroupChatHandler) *EventDispatcher {
	d.onBotAddedToGroupChat = handler
	return d
}

func (d *EventDispatcher) OnBotRemovedFromGroupChat(handler BotRemovedFromGroupChatHandler) *EventDispatcher {
	d.onBotRemovedFromGroupChat = handler
	return d
}

func (d *EventDispatcher) OnGroupChatConvertedToExternalGroup(handler GroupChatConvertedToExternalGroupHandler) *EventDispatcher {
	d.onGroupChatConvertedToExternalGroup = handler
	return d
}

func (d *EventDispatcher) OnKick(handler EnvelopeHandler) *EventDispatcher {
	d.onKick = handler
	return d
}

func (d *EventDispatcher) OnEnvelope(handler EnvelopeHandler) *EventDispatcher {
	d.onEnvelope = handler
	return d
}

func (d *EventDispatcher) OnInvalidFrame(handler InvalidFrameHandler) *EventDispatcher {
	d.onInvalidFrame = handler
	return d
}

func (d *EventDispatcher) dispatch(ctx context.Context, c *Client, env *Envelope) error {
	if d == nil {
		if env.Cmd == CommandKick {
			return &KickError{Message: env.Message, Envelope: *env}
		}
		return nil
	}
	if env.Cmd == CommandPong {
		return nil
	}
	if d.onEnvelope != nil {
		if err := d.onEnvelope(ctx, env); err != nil {
			return err
		}
	}

	switch env.Cmd {
	case CommandEvent:
		event := &Event{
			AppID:      env.Header.AppID,
			CallbackID: env.Header.CallbackID,
			Rid:        env.Header.Rid,
			Sid:        env.Header.Sid,
			Data:       env.Data,
		}
		handled := false
		if d.onEvent != nil {
			if err := d.onEvent(ctx, event); err != nil {
				return err
			}
			handled = true
		}
		typedHandled, err := d.dispatchTypedEvent(ctx, event)
		if err != nil {
			return err
		}
		handled = handled || typedHandled
		if handled && event.CallbackID != "" {
			return c.Ack(ctx, event.CallbackID)
		}
	case CommandKick:
		if d.onKick != nil {
			if err := d.onKick(ctx, env); err != nil {
				return err
			}
		}
		return &KickError{Message: env.Message, Envelope: *env}
	}
	return nil
}

func (d *EventDispatcher) dispatchTypedEvent(ctx context.Context, event *Event) (bool, error) {
	if !d.hasTypedEventHandler() {
		return false, nil
	}

	var meta struct {
		EventType string `json:"event_type"`
	}
	if err := json.Unmarshal(event.Data, &meta); err != nil {
		return false, err
	}

	switch meta.EventType {
	case EventTypeUserEnterChatroomWithBot:
		if d.onUserEnterChatroomWithBot == nil {
			return false, nil
		}
		var typedEvent UserEnterChatroomWithBotEvent
		if err := json.Unmarshal(event.Data, &typedEvent); err != nil {
			return false, err
		}
		typedEvent.CallbackID = event.CallbackID
		return true, d.onUserEnterChatroomWithBot(ctx, &typedEvent)
	case EventTypeMessageFromBotSubscriber:
		if d.onMessageFromBotSubscriber == nil {
			return false, nil
		}
		var typedEvent MessageFromBotSubscriberEvent
		if err := json.Unmarshal(event.Data, &typedEvent); err != nil {
			return false, err
		}
		typedEvent.CallbackID = event.CallbackID
		return true, d.onMessageFromBotSubscriber(ctx, &typedEvent)
	case EventTypeNewMentionedMessageReceivedFromGroupChat:
		if d.onNewMentionedMessageReceivedFromGroupChat == nil {
			return false, nil
		}
		var typedEvent NewMentionedMessageReceivedFromGroupChatEvent
		if err := json.Unmarshal(event.Data, &typedEvent); err != nil {
			return false, err
		}
		typedEvent.CallbackID = event.CallbackID
		return true, d.onNewMentionedMessageReceivedFromGroupChat(ctx, &typedEvent)
	case EventTypeNewMessageReceivedFromGroupChat:
		if d.onNewMessageReceivedFromGroupChat == nil {
			return false, nil
		}
		var typedEvent NewMessageReceivedFromGroupChatEvent
		if err := json.Unmarshal(event.Data, &typedEvent); err != nil {
			return false, err
		}
		typedEvent.CallbackID = event.CallbackID
		return true, d.onNewMessageReceivedFromGroupChat(ctx, &typedEvent)
	case EventTypeInteractiveMessageClick:
		if d.onInteractiveMessageClick == nil {
			return false, nil
		}
		var typedEvent InteractiveMessageClickEvent
		if err := json.Unmarshal(event.Data, &typedEvent); err != nil {
			return false, err
		}
		typedEvent.CallbackID = event.CallbackID
		return true, d.onInteractiveMessageClick(ctx, &typedEvent)
	case EventTypeNewMessageReceivedFromThread:
		if d.onNewMessageReceivedFromThread == nil {
			return false, nil
		}
		var typedEvent NewMessageReceivedFromThreadEvent
		if err := json.Unmarshal(event.Data, &typedEvent); err != nil {
			return false, err
		}
		typedEvent.CallbackID = event.CallbackID
		return true, d.onNewMessageReceivedFromThread(ctx, &typedEvent)
	case EventTypeBotAddedToGroupChat:
		if d.onBotAddedToGroupChat == nil {
			return false, nil
		}
		var typedEvent BotAddedToGroupChatEvent
		if err := json.Unmarshal(event.Data, &typedEvent); err != nil {
			return false, err
		}
		typedEvent.CallbackID = event.CallbackID
		return true, d.onBotAddedToGroupChat(ctx, &typedEvent)
	case EventTypeBotRemovedFromGroupChat:
		if d.onBotRemovedFromGroupChat == nil {
			return false, nil
		}
		var typedEvent BotRemovedFromGroupChatEvent
		if err := json.Unmarshal(event.Data, &typedEvent); err != nil {
			return false, err
		}
		typedEvent.CallbackID = event.CallbackID
		return true, d.onBotRemovedFromGroupChat(ctx, &typedEvent)
	case EventTypeGroupChatConvertedToExternalGroup:
		if d.onGroupChatConvertedToExternalGroup == nil {
			return false, nil
		}
		var typedEvent GroupChatConvertedToExternalGroupEvent
		if err := json.Unmarshal(event.Data, &typedEvent); err != nil {
			return false, err
		}
		typedEvent.CallbackID = event.CallbackID
		return true, d.onGroupChatConvertedToExternalGroup(ctx, &typedEvent)
	default:
		return false, nil
	}
}

func (d *EventDispatcher) hasTypedEventHandler() bool {
	return d.onUserEnterChatroomWithBot != nil ||
		d.onMessageFromBotSubscriber != nil ||
		d.onNewMentionedMessageReceivedFromGroupChat != nil ||
		d.onNewMessageReceivedFromGroupChat != nil ||
		d.onInteractiveMessageClick != nil ||
		d.onNewMessageReceivedFromThread != nil ||
		d.onBotAddedToGroupChat != nil ||
		d.onBotRemovedFromGroupChat != nil ||
		d.onGroupChatConvertedToExternalGroup != nil
}

func defaultEnvelopeHandler(ctx context.Context, envelope *Envelope) error {
	pretty, err := json.MarshalIndent(envelope, "", "  ")
	if err != nil {
		return err
	}
	fmt.Printf("%s\n", pretty)
	return nil
}

func defaultInvalidFrameHandler(ctx context.Context, payload []byte, err error) error {
	fmt.Printf("non-json frame: %s err: %v\n", string(payload), err)
	return nil
}

func (d *EventDispatcher) handleInvalidFrame(ctx context.Context, payload []byte, err error) error {
	if d != nil && d.onInvalidFrame != nil {
		return d.onInvalidFrame(ctx, payload, err)
	}
	return err
}
