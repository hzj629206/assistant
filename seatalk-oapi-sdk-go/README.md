# seatalk-oapi-sdk-go

Small Go SDK for the seatalk open platform developer bot WebSocket protocol.

For a Chinese integration guide aimed at SOP open platform developers, see [OPEN_PLATFORM_DEVELOPER_GUIDE.md](OPEN_PLATFORM_DEVELOPER_GUIDE.md).

## Basic usage

```go
dispatcher := seatalkoapisdk.NewEventDispatcher().
	OnUserEnterChatroomWithBot(func(ctx context.Context, event *seatalkoapisdk.UserEnterChatroomWithBotEvent) error {
		fmt.Printf("seatalk_id=%s email=%s\n", event.Event.SeaTalkID, event.Event.Email)
		return nil
	}).
	OnMessageFromBotSubscriber(func(ctx context.Context, event *seatalkoapisdk.MessageFromBotSubscriberEvent) error {
		if event.Event.Message.Tag == seatalkoapisdk.MessageTagText && event.Event.Message.Text != nil {
			fmt.Printf("message=%s\n", event.Event.Message.Text.Content)
		}
		return nil
	}).
	OnNewMentionedMessageReceivedFromGroupChat(func(ctx context.Context, event *seatalkoapisdk.NewMentionedMessageReceivedFromGroupChatEvent) error {
		if event.Event.Message.Text != nil {
			fmt.Printf("group_id=%s message=%s\n", event.Event.GroupID, event.Event.Message.Text.PlainText)
		}
		return nil
	}).
	OnNewMessageReceivedFromGroupChat(func(ctx context.Context, event *seatalkoapisdk.NewMessageReceivedFromGroupChatEvent) error {
		if event.Event.Message.Text != nil {
			fmt.Printf("group_id=%s message=%s\n", event.Event.GroupID, event.Event.Message.Text.PlainText)
		}
		return nil
	}).
	OnInteractiveMessageClick(func(ctx context.Context, event *seatalkoapisdk.InteractiveMessageClickEvent) error {
		fmt.Printf("message_id=%s value=%s\n", event.Event.MessageID, event.Event.Value)
		return nil
	}).
	OnNewMessageReceivedFromThread(func(ctx context.Context, event *seatalkoapisdk.NewMessageReceivedFromThreadEvent) error {
		if event.Event.Message.Text != nil {
			fmt.Printf("thread_id=%s message=%s\n", event.Event.Message.ThreadID, event.Event.Message.Text.PlainText)
		}
		return nil
	}).
	OnBotAddedToGroupChat(func(ctx context.Context, event *seatalkoapisdk.BotAddedToGroupChatEvent) error {
		fmt.Printf("group_id=%s group_name=%s\n", event.Event.Group.GroupID, event.Event.Group.GroupName)
		return nil
	}).
	OnBotRemovedFromGroupChat(func(ctx context.Context, event *seatalkoapisdk.BotRemovedFromGroupChatEvent) error {
		fmt.Printf("group_id=%s remover=%s\n", event.Event.GroupID, event.Event.Remover.SeaTalkID)
		return nil
	}).
	OnGroupChatConvertedToExternalGroup(func(ctx context.Context, event *seatalkoapisdk.GroupChatConvertedToExternalGroupEvent) error {
		fmt.Printf("group_id=%s operator=%s\n", event.Event.GroupID, event.Event.Operator.SeaTalkID)
		return nil
	})

client := seatalkoapisdk.NewClient(
	appID,
	appSecret,
	seatalkoapisdk.WithWebSocketURL("wss://ws-openapi.haiserve.com/ws/bot"),
	seatalkoapisdk.WithEventDispatcher(dispatcher),
)

if err := client.Run(context.Background()); err != nil {
	log.Fatal(err)
}
```

When `Run` or `Start` is active, the SDK sends a `ping` command using the
heartbeat interval returned by the server during `register`. If the server does
not return one, the SDK falls back to the local ping interval, which is 15
seconds by default. `pong` responses are consumed silently. When a registered
event handler returns `nil`, the SDK sends `ack` automatically if the event
carries a `callback_id`.

For manual flows, call `Connect`, `Ack`, `Start`, and `Close` directly.

## Examples

Run examples from the `seatalk-oapi-sdk-go` directory:

```bash
go run ./examples/basic_client \
  --url "wss://ws-openapi.haiserve.com/ws/bot" \
  --app-id "$APP_ID" \
  --app-secret "$APP_SECRET"
```

- `examples/basic_client`: connect and print envelopes with the default dispatcher.
- `examples/typed_handlers`: register handlers for all typed events supported by the SDK.
- `examples/custom_logging`: customize envelope and invalid-frame logging.
