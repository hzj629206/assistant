// typed_handlers registers handlers for all typed seatalk open platform events supported by the SDK.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	seatalkoapisdk "git.garena.com/seatalk/seatalk-oapi-sdk-go"
)

func main() {
	wsURL := flag.String("url", seatalkoapisdk.DefaultWebSocketURL, "full WebSocket URL")
	appID := flag.String("app-id", "", "developer bot app_id (required)")
	appSecret := flag.String("app-secret", "", "developer bot app_secret (required)")
	flag.Parse()

	if *appID == "" || *appSecret == "" {
		flag.Usage()
		fmt.Fprintln(os.Stderr, "app-id and app-secret are required")
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	dispatcher := seatalkoapisdk.NewEventDispatcher().
		OnUserEnterChatroomWithBot(func(ctx context.Context, event *seatalkoapisdk.UserEnterChatroomWithBotEvent) error {
			fmt.Printf("user enter chatroom: event_id=%s seatalk_id=%s employee_code=%s email=%s\n",
				event.EventID, event.Event.SeaTalkID, event.Event.EmployeeCode, event.Event.Email)
			return nil
		}).
		OnMessageFromBotSubscriber(func(ctx context.Context, event *seatalkoapisdk.MessageFromBotSubscriberEvent) error {
			fmt.Printf("subscriber message: event_id=%s sender=%s %s\n",
				event.EventID, event.Event.SeaTalkID, describeSubscriberMessage(event.Event.Message))
			return nil
		}).
		OnNewMentionedMessageReceivedFromGroupChat(func(ctx context.Context, event *seatalkoapisdk.NewMentionedMessageReceivedFromGroupChatEvent) error {
			message := event.Event.Message
			text := ""
			mentions := 0
			if message.Text != nil {
				text = message.Text.PlainText
				mentions = len(message.Text.MentionedList)
			}
			fmt.Printf("group mention: event_id=%s group_id=%s sender=%s text=%q mentions=%d\n",
				event.EventID, event.Event.GroupID, message.Sender.SeaTalkID, text, mentions)
			return nil
		}).
		OnNewMessageReceivedFromGroupChat(func(ctx context.Context, event *seatalkoapisdk.NewMessageReceivedFromGroupChatEvent) error {
			fmt.Printf("group message: event_id=%s group_id=%s sender=%s %s\n",
				event.EventID, event.Event.GroupID, event.Event.Message.Sender.SeaTalkID, describeGroupChatMessage(event.Event.Message))
			return nil
		}).
		OnInteractiveMessageClick(func(ctx context.Context, event *seatalkoapisdk.InteractiveMessageClickEvent) error {
			fmt.Printf("interactive click: event_id=%s message_id=%s user=%s value=%s group_id=%s thread_id=%s\n",
				event.EventID, event.Event.MessageID, event.Event.SeaTalkID, event.Event.Value, event.Event.GroupID, event.Event.ThreadID)
			return nil
		}).
		OnNewMessageReceivedFromThread(func(ctx context.Context, event *seatalkoapisdk.NewMessageReceivedFromThreadEvent) error {
			fmt.Printf("thread message: event_id=%s group_id=%s sender=%s %s\n",
				event.EventID, event.Event.GroupID, event.Event.Message.Sender.SeaTalkID, describeThreadMessage(event.Event.Message))
			return nil
		}).
		OnBotAddedToGroupChat(func(ctx context.Context, event *seatalkoapisdk.BotAddedToGroupChatEvent) error {
			fmt.Printf("bot added: event_id=%s group_id=%s group_name=%s inviter=%s\n",
				event.EventID, event.Event.Group.GroupID, event.Event.Group.GroupName, event.Event.Inviter.SeaTalkID)
			return nil
		}).
		OnBotRemovedFromGroupChat(func(ctx context.Context, event *seatalkoapisdk.BotRemovedFromGroupChatEvent) error {
			fmt.Printf("bot removed: event_id=%s group_id=%s remover=%s\n",
				event.EventID, event.Event.GroupID, event.Event.Remover.SeaTalkID)
			return nil
		}).
		OnGroupChatConvertedToExternalGroup(func(ctx context.Context, event *seatalkoapisdk.GroupChatConvertedToExternalGroupEvent) error {
			fmt.Printf("group converted to external: event_id=%s group_id=%s operator=%s\n",
				event.EventID, event.Event.GroupID, event.Event.Operator.SeaTalkID)
			return nil
		})

	client := seatalkoapisdk.NewClient(
		*appID,
		*appSecret,
		seatalkoapisdk.WithWebSocketURL(*wsURL),
		seatalkoapisdk.WithEventDispatcher(dispatcher),
		seatalkoapisdk.WithLogger(log.New(os.Stdout, "seatalk_oapi_sdk_go ", log.LstdFlags)),
	)
	defer client.Close()

	result, err := client.Connect(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "register: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("registered ok, session token: %s\n", result.Token)

	if err := client.Start(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "read: %v\n", err)
		os.Exit(1)
	}
	if ctx.Err() != nil {
		fmt.Println("shutting down")
		return
	}
	fmt.Println("connection closed")
}

func describeSubscriberMessage(message seatalkoapisdk.BotSubscriberMessage) string {
	switch message.Tag {
	case seatalkoapisdk.MessageTagText:
		if message.Text != nil {
			return fmt.Sprintf("message_id=%s text=%q", message.MessageID, message.Text.Content)
		}
	case seatalkoapisdk.MessageTagImage:
		if message.Image != nil {
			return fmt.Sprintf("message_id=%s image=%s", message.MessageID, message.Image.Content)
		}
	case seatalkoapisdk.MessageTagFile:
		if message.File != nil {
			return fmt.Sprintf("message_id=%s file=%s url=%s", message.MessageID, message.File.Filename, message.File.Content)
		}
	case seatalkoapisdk.MessageTagVideo:
		if message.Video != nil {
			return fmt.Sprintf("message_id=%s video=%s", message.MessageID, message.Video.Content)
		}
	}
	return fmt.Sprintf("message_id=%s tag=%s", message.MessageID, message.Tag)
}

func describeThreadMessage(message seatalkoapisdk.ThreadMessage) string {
	switch message.Tag {
	case seatalkoapisdk.MessageTagText:
		if message.Text != nil {
			return fmt.Sprintf("message_id=%s thread_id=%s text=%q mentions=%d",
				message.MessageID, message.ThreadID, message.Text.PlainText, len(message.Text.MentionedList))
		}
	case seatalkoapisdk.MessageTagImage:
		if message.Image != nil {
			return fmt.Sprintf("message_id=%s thread_id=%s image=%s", message.MessageID, message.ThreadID, message.Image.Content)
		}
	case seatalkoapisdk.MessageTagFile:
		if message.File != nil {
			return fmt.Sprintf("message_id=%s thread_id=%s file=%s url=%s",
				message.MessageID, message.ThreadID, message.File.Filename, message.File.Content)
		}
	case seatalkoapisdk.MessageTagVideo:
		if message.Video != nil {
			return fmt.Sprintf("message_id=%s thread_id=%s video=%s", message.MessageID, message.ThreadID, message.Video.Content)
		}
	}
	return fmt.Sprintf("message_id=%s thread_id=%s tag=%s", message.MessageID, message.ThreadID, message.Tag)
}

func describeGroupChatMessage(message seatalkoapisdk.GroupChatMessage) string {
	switch message.Tag {
	case seatalkoapisdk.MessageTagText:
		if message.Text != nil {
			return fmt.Sprintf("message_id=%s thread_id=%s text=%q mentions=%d",
				message.MessageID, message.ThreadID, message.Text.PlainText, len(message.Text.MentionedList))
		}
	case seatalkoapisdk.MessageTagInteractiveMessage:
		if message.InteractiveMessage != nil {
			return fmt.Sprintf("message_id=%s thread_id=%s interactive_elements=%d",
				message.MessageID, message.ThreadID, len(message.InteractiveMessage.Elements))
		}
	case seatalkoapisdk.MessageTagImage:
		if message.Image != nil {
			return fmt.Sprintf("message_id=%s thread_id=%s image=%s", message.MessageID, message.ThreadID, message.Image.Content)
		}
	case seatalkoapisdk.MessageTagFile:
		if message.File != nil {
			return fmt.Sprintf("message_id=%s thread_id=%s file=%s url=%s",
				message.MessageID, message.ThreadID, message.File.Filename, message.File.Content)
		}
	case seatalkoapisdk.MessageTagVideo:
		if message.Video != nil {
			return fmt.Sprintf("message_id=%s thread_id=%s video=%s", message.MessageID, message.ThreadID, message.Video.Content)
		}
	case seatalkoapisdk.MessageTagChangeMembers:
		if message.ChangeMembers != nil {
			return fmt.Sprintf("message_id=%s thread_id=%s added=%d removed=%d newly_created=%t",
				message.MessageID, message.ThreadID, len(message.ChangeMembers.AddedMembers), len(message.ChangeMembers.RemovedMembers), message.ChangeMembers.IsNewlyCreated)
		}
	case seatalkoapisdk.MessageTagRecallMsgs:
		if message.RecallMsgs != nil {
			return fmt.Sprintf("message_id=%s thread_id=%s recalled=%d",
				message.MessageID, message.ThreadID, len(message.RecallMsgs.Messages))
		}
	case seatalkoapisdk.MessageTagGroupRemoved:
		if message.GroupRemoved != nil {
			return fmt.Sprintf("message_id=%s thread_id=%s op_user=%s",
				message.MessageID, message.ThreadID, message.GroupRemoved.OpUser.SeaTalkID)
		}
	case seatalkoapisdk.MessageTagEdit:
		if message.Edit != nil {
			return fmt.Sprintf("message_id=%s thread_id=%s edited_message_id=%s edited_tag=%s",
				message.MessageID, message.ThreadID, message.Edit.Message.MessageID, message.Edit.Message.Tag)
		}
	}
	return fmt.Sprintf("message_id=%s thread_id=%s tag=%s", message.MessageID, message.ThreadID, message.Tag)
}
