package seatalkoapisdk

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestClientConnectAndAck(t *testing.T) {
	var logBuffer bytes.Buffer
	var registerRid string

	wsURL, serverErr, closeServer := newTestWSServer(t, func(conn *websocket.Conn) error {
		defer conn.Close()

		register, err := readEnvelope(conn)
		if err != nil {
			return err
		}
		if register.Cmd != CommandRegister {
			return fmt.Errorf("expected register, got %s", register.Cmd)
		}
		if register.Header.AppID != "app-1" || register.Header.AppSecret != "secret-1" {
			return fmt.Errorf("unexpected credential: %+v", register.Header)
		}
		if register.Header.Rid == "" {
			return fmt.Errorf("register rid is empty")
		}
		registerRid = register.Header.Rid

		if err := conn.WriteJSON(Envelope{
			Cmd: CommandRegister,
			Header: Header{
				AppID: "app-1",
				Sid:   "sid-1",
				Token: "token-1",
			},
			Code:    CodeOK,
			Message: "ok",
			Data:    json.RawMessage(`{"heartbeat_interval":7,"heartbeat_timeout":21}`),
		}); err != nil {
			return err
		}

		ack, err := readEnvelope(conn)
		if err != nil {
			return err
		}
		if ack.Cmd != CommandAck {
			return fmt.Errorf("expected ack, got %s", ack.Cmd)
		}
		if ack.Header.Token != "token-1" || ack.Header.CallbackID != "callback-1" {
			return fmt.Errorf("unexpected ack header: %+v", ack.Header)
		}
		if ack.Header.Rid == "" {
			return fmt.Errorf("ack rid is empty")
		}
		if ack.Header.Rid == registerRid {
			return fmt.Errorf("ack rid should differ from register rid: %s", ack.Header.Rid)
		}
		return nil
	})
	defer closeServer()

	client := NewClient(
		"app-1",
		"secret-1",
		WithWebSocketURL(wsURL),
		WithLogger(log.New(&logBuffer, "", 0)),
	)
	result, err := client.Connect(context.Background())
	if err != nil {
		t.Fatalf("Connect() error = %v", err)
	}
	if result.Token != "token-1" {
		t.Fatalf("unexpected token: %s", result.Token)
	}
	if result.Sid != "sid-1" {
		t.Fatalf("unexpected sid: %s", result.Sid)
	}
	if result.HeartbeatInterval != 7*time.Second || result.HeartbeatTimeout != 21*time.Second {
		t.Fatalf("unexpected heartbeat settings: %+v", result)
	}
	if got := client.currentPingInterval(); got != 7*time.Second {
		t.Fatalf("currentPingInterval() = %v, want %v", got, 7*time.Second)
	}
	if got := client.Token(); got != "token-1" {
		t.Fatalf("Token() = %s", got)
	}
	if err := client.Ack(context.Background(), "callback-1"); err != nil {
		t.Fatalf("Ack() error = %v", err)
	}
	if err := waitServerErr(serverErr); err != nil {
		t.Fatal(err)
	}
	logOutput := logBuffer.String()
	if !strings.Contains(logOutput, "sent register command rid=") ||
		!strings.Contains(logOutput, "sent ack command rid=") {
		t.Fatalf("log output does not include request rid: %s", logOutput)
	}
}

func TestClientStartStopsOnContextCancel(t *testing.T) {
	wsURL, serverErr, closeServer := newTestWSServer(t, func(conn *websocket.Conn) error {
		defer conn.Close()

		if _, err := readEnvelope(conn); err != nil {
			return err
		}
		if err := conn.WriteJSON(Envelope{
			Cmd:     CommandRegister,
			Header:  Header{AppID: "app-1", Token: "token-1"},
			Code:    CodeOK,
			Message: "ok",
		}); err != nil {
			return err
		}

		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return nil
			}
		}
	})
	defer closeServer()

	ctx, cancel := context.WithCancel(context.Background())
	client := NewClient("app-1", "secret-1", WithWebSocketURL(wsURL))
	if _, err := client.Connect(ctx); err != nil {
		t.Fatalf("Connect() error = %v", err)
	}

	startErr := make(chan error, 1)
	go func() {
		startErr <- client.Start(ctx)
	}()

	cancel()

	select {
	case err := <-startErr:
		if err != nil {
			t.Fatalf("Start() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for Start()")
	}
	if err := waitServerErr(serverErr); err != nil {
		t.Fatal(err)
	}
}

func TestClientStartSendsPingAndIgnoresPong(t *testing.T) {
	wsURL, serverErr, closeServer := newTestWSServer(t, func(conn *websocket.Conn) error {
		defer conn.Close()

		if _, err := readEnvelope(conn); err != nil {
			return err
		}
		if err := conn.WriteJSON(Envelope{
			Cmd:     CommandRegister,
			Header:  Header{AppID: "app-1", Token: "token-1"},
			Code:    CodeOK,
			Message: "ok",
		}); err != nil {
			return err
		}

		ping, err := readEnvelope(conn)
		if err != nil {
			return err
		}
		if ping.Cmd != CommandPing || ping.Header.Token != "token-1" {
			return fmt.Errorf("unexpected ping: %+v", ping)
		}
		if err := conn.WriteJSON(Envelope{
			Cmd:     CommandPong,
			Header:  Header{Token: "token-1"},
			Code:    CodeOK,
			Message: "ok",
		}); err != nil {
			return err
		}
		return conn.WriteControl(
			websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
			time.Now().Add(time.Second),
		)
	})
	defer closeServer()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	client := NewClient(
		"app-1",
		"secret-1",
		WithWebSocketURL(wsURL),
		WithPingInterval(10*time.Millisecond),
	)
	if _, err := client.Connect(ctx); err != nil {
		t.Fatalf("Connect() error = %v", err)
	}

	startErr := make(chan error, 1)
	go func() {
		startErr <- client.Start(ctx)
	}()

	if err := waitServerErr(serverErr); err != nil {
		t.Fatal(err)
	}
	cancel()
	_ = client.Close()

	select {
	case err := <-startErr:
		if err != nil {
			t.Fatalf("Start() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for Start()")
	}
}

func TestClientRunDispatchesEventAndAutoAck(t *testing.T) {
	eventCh := make(chan *Event, 1)

	wsURL, serverErr, closeServer := newTestWSServer(t, func(conn *websocket.Conn) error {
		defer conn.Close()

		if _, err := readEnvelope(conn); err != nil {
			return err
		}
		if err := conn.WriteJSON(Envelope{
			Cmd:     CommandRegister,
			Header:  Header{AppID: "app-1", Token: "token-1"},
			Code:    CodeOK,
			Message: "ok",
		}); err != nil {
			return err
		}
		if err := conn.WriteJSON(Envelope{
			Cmd: CommandEvent,
			Header: Header{
				AppID:      "app-1",
				CallbackID: "callback-1",
			},
			Data: json.RawMessage(`{"hello":"world"}`),
			Code: CodeOK,
		}); err != nil {
			return err
		}

		ack, err := readEnvelope(conn)
		if err != nil {
			return err
		}
		if ack.Cmd != CommandAck || ack.Header.Token != "token-1" || ack.Header.CallbackID != "callback-1" {
			return fmt.Errorf("unexpected auto ack: %+v", ack)
		}

		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return nil
			}
		}
	})
	defer closeServer()

	dispatcher := NewEventDispatcher().
		OnEvent(func(ctx context.Context, event *Event) error {
			eventCh <- event
			return nil
		})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	client := NewClient("app-1", "secret-1", WithWebSocketURL(wsURL), WithEventDispatcher(dispatcher))
	runErr := make(chan error, 1)
	go func() {
		runErr <- client.Run(ctx)
	}()

	select {
	case event := <-eventCh:
		if event.CallbackID != "callback-1" {
			t.Fatalf("unexpected callback id: %s", event.CallbackID)
		}
		if string(event.Data) != `{"hello":"world"}` {
			t.Fatalf("unexpected event data: %s", event.Data)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for event")
	}

	cancel()
	_ = client.Close()

	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("Run() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for Run()")
	}
	if err := waitServerErr(serverErr); err != nil {
		t.Fatal(err)
	}
}

func TestClientRunDispatchesUserEnterChatroomWithBotAndAutoAck(t *testing.T) {
	eventCh := make(chan *UserEnterChatroomWithBotEvent, 1)

	wsURL, serverErr, closeServer := newTestWSServer(t, func(conn *websocket.Conn) error {
		defer conn.Close()

		if _, err := readEnvelope(conn); err != nil {
			return err
		}
		if err := conn.WriteJSON(Envelope{
			Cmd:     CommandRegister,
			Header:  Header{AppID: "app-1", Token: "token-1"},
			Code:    CodeOK,
			Message: "ok",
		}); err != nil {
			return err
		}
		if err := conn.WriteJSON(Envelope{
			Cmd: CommandEvent,
			Header: Header{
				AppID:      "app-1",
				CallbackID: "callback-1",
			},
			Data: json.RawMessage(`{
				"event_id": "1234567",
				"event_type": "user_enter_chatroom_with_bot",
				"timestamp": 1611220944,
				"app_id": "app-1",
				"event": {
					"seatalk_id": "1239487273",
					"employee_code": "e_12345678",
					"email": "sample@seatalk.biz"
				}
			}`),
			Code: CodeOK,
		}); err != nil {
			return err
		}

		ack, err := readEnvelope(conn)
		if err != nil {
			return err
		}
		if ack.Cmd != CommandAck || ack.Header.Token != "token-1" || ack.Header.CallbackID != "callback-1" {
			return fmt.Errorf("unexpected auto ack: %+v", ack)
		}

		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return nil
			}
		}
	})
	defer closeServer()

	dispatcher := NewEventDispatcher().
		OnUserEnterChatroomWithBot(func(ctx context.Context, event *UserEnterChatroomWithBotEvent) error {
			eventCh <- event
			return nil
		})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	client := NewClient("app-1", "secret-1", WithWebSocketURL(wsURL), WithEventDispatcher(dispatcher))
	runErr := make(chan error, 1)
	go func() {
		runErr <- client.Run(ctx)
	}()

	select {
	case event := <-eventCh:
		if event.CallbackID != "callback-1" {
			t.Fatalf("unexpected callback id: %s", event.CallbackID)
		}
		if event.EventID != "1234567" || event.EventType != EventTypeUserEnterChatroomWithBot {
			t.Fatalf("unexpected event metadata: %+v", event)
		}
		if event.Timestamp != 1611220944 || event.AppID != "app-1" {
			t.Fatalf("unexpected event app/timestamp: %+v", event)
		}
		if event.Event.SeaTalkID != "1239487273" ||
			event.Event.EmployeeCode != "e_12345678" ||
			event.Event.Email != "sample@seatalk.biz" {
			t.Fatalf("unexpected event detail: %+v", event.Event)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for user enter chatroom event")
	}

	cancel()
	_ = client.Close()

	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("Run() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for Run()")
	}
	if err := waitServerErr(serverErr); err != nil {
		t.Fatal(err)
	}
}

func TestClientRunDispatchesMessageFromBotSubscriberAndAutoAck(t *testing.T) {
	eventCh := make(chan *MessageFromBotSubscriberEvent, 1)

	wsURL, serverErr, closeServer := newTestWSServer(t, func(conn *websocket.Conn) error {
		defer conn.Close()

		if _, err := readEnvelope(conn); err != nil {
			return err
		}
		if err := conn.WriteJSON(Envelope{
			Cmd:     CommandRegister,
			Header:  Header{AppID: "app-1", Token: "token-1"},
			Code:    CodeOK,
			Message: "ok",
		}); err != nil {
			return err
		}
		if err := conn.WriteJSON(Envelope{
			Cmd: CommandEvent,
			Header: Header{
				AppID:      "app-1",
				CallbackID: "callback-1",
			},
			Data: json.RawMessage(`{
				"event_id": "message-event-1",
				"event_type": "message_from_bot_subscriber",
				"timestamp": 1611220944,
				"app_id": "app-1",
				"event": {
					"seatalk_id": "1239487273",
					"employee_code": "e_12345678",
					"email": "sample@seatalk.biz",
					"message": {
						"message_id": "msg-1",
						"quoted_message_id": "quoted-msg-1",
						"thread_id": "thread-1",
						"tag": "text",
						"text": {
							"content": "hello bot"
						},
						"combined_forwarded_chat_history": {
							"content": [
								{
									"tag": "text",
									"sender": {
										"seatalk_id": "forwarder-1",
										"employee_code": "e_forwarder_1",
										"email": "forwarder1@seatalk.biz",
										"sender_type": 1
									},
									"message_sent_time": 1611220900,
									"text": {
										"content": "forwarded text"
									}
								}
							]
						}
					}
				}
			}`),
			Code: CodeOK,
		}); err != nil {
			return err
		}

		ack, err := readEnvelope(conn)
		if err != nil {
			return err
		}
		if ack.Cmd != CommandAck || ack.Header.Token != "token-1" || ack.Header.CallbackID != "callback-1" {
			return fmt.Errorf("unexpected auto ack: %+v", ack)
		}

		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return nil
			}
		}
	})
	defer closeServer()

	dispatcher := NewEventDispatcher().
		OnMessageFromBotSubscriber(func(ctx context.Context, event *MessageFromBotSubscriberEvent) error {
			eventCh <- event
			return nil
		})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	client := NewClient("app-1", "secret-1", WithWebSocketURL(wsURL), WithEventDispatcher(dispatcher))
	runErr := make(chan error, 1)
	go func() {
		runErr <- client.Run(ctx)
	}()

	select {
	case event := <-eventCh:
		if event.CallbackID != "callback-1" {
			t.Fatalf("unexpected callback id: %s", event.CallbackID)
		}
		if event.EventID != "message-event-1" || event.EventType != EventTypeMessageFromBotSubscriber {
			t.Fatalf("unexpected event metadata: %+v", event)
		}
		if event.Timestamp != 1611220944 || event.AppID != "app-1" {
			t.Fatalf("unexpected event app/timestamp: %+v", event)
		}
		if event.Event.SeaTalkID != "1239487273" ||
			event.Event.EmployeeCode != "e_12345678" ||
			event.Event.Email != "sample@seatalk.biz" {
			t.Fatalf("unexpected sender detail: %+v", event.Event)
		}
		message := event.Event.Message
		if message.MessageID != "msg-1" ||
			message.QuotedMessageID != "quoted-msg-1" ||
			message.ThreadID != "thread-1" ||
			message.Tag != MessageTagText {
			t.Fatalf("unexpected message metadata: %+v", message)
		}
		if message.Text == nil || message.Text.Content != "hello bot" {
			t.Fatalf("unexpected text message: %+v", message.Text)
		}
		if message.CombinedForwardedChatHistory == nil ||
			len(message.CombinedForwardedChatHistory.Content) != 1 ||
			message.CombinedForwardedChatHistory.Content[0].Sender == nil ||
			message.CombinedForwardedChatHistory.Content[0].Sender.SenderType != 1 ||
			message.CombinedForwardedChatHistory.Content[0].Text == nil ||
			message.CombinedForwardedChatHistory.Content[0].Text.Content != "forwarded text" {
			t.Fatalf("unexpected combined forwarded chat history: %+v", message.CombinedForwardedChatHistory)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for message from bot subscriber event")
	}

	cancel()
	_ = client.Close()

	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("Run() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for Run()")
	}
	if err := waitServerErr(serverErr); err != nil {
		t.Fatal(err)
	}
}

func TestClientRunDispatchesNewMentionedMessageReceivedFromGroupChatAndAutoAck(t *testing.T) {
	eventCh := make(chan *NewMentionedMessageReceivedFromGroupChatEvent, 1)

	wsURL, serverErr, closeServer := newTestWSServer(t, func(conn *websocket.Conn) error {
		defer conn.Close()

		if _, err := readEnvelope(conn); err != nil {
			return err
		}
		if err := conn.WriteJSON(Envelope{
			Cmd:     CommandRegister,
			Header:  Header{AppID: "app-1", Token: "token-1"},
			Code:    CodeOK,
			Message: "ok",
		}); err != nil {
			return err
		}
		if err := conn.WriteJSON(Envelope{
			Cmd: CommandEvent,
			Header: Header{
				AppID:      "app-1",
				CallbackID: "callback-1",
			},
			Data: json.RawMessage(`{
				"event_id": "group-mention-event-1",
				"event_type": "new_mentioned_message_received_from_group_chat",
				"timestamp": 1611220944,
				"app_id": "app-1",
				"event": {
					"group_id": "group-1",
					"message": {
						"message_id": "msg-1",
						"quoted_message_id": "quoted-msg-1",
						"thread_id": "thread-1",
						"sender": {
							"seatalk_id": "1239487273",
							"employee_code": "e_12345678",
							"email": "sample@seatalk.biz",
							"sender_type": 1
						},
						"message_sent_time": 1611220933,
						"tag": "text",
						"text": {
							"plain_text": "@bot hello",
							"mentioned_list": [
								{
									"username": "bot",
									"seatalk_id": "bot-1",
									"employee_code": "",
									"email": "",
									"location": 0,
									"length": 4
								}
							]
						}
					}
				}
			}`),
			Code: CodeOK,
		}); err != nil {
			return err
		}

		ack, err := readEnvelope(conn)
		if err != nil {
			return err
		}
		if ack.Cmd != CommandAck || ack.Header.Token != "token-1" || ack.Header.CallbackID != "callback-1" {
			return fmt.Errorf("unexpected auto ack: %+v", ack)
		}

		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return nil
			}
		}
	})
	defer closeServer()

	dispatcher := NewEventDispatcher().
		OnNewMentionedMessageReceivedFromGroupChat(func(ctx context.Context, event *NewMentionedMessageReceivedFromGroupChatEvent) error {
			eventCh <- event
			return nil
		})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	client := NewClient("app-1", "secret-1", WithWebSocketURL(wsURL), WithEventDispatcher(dispatcher))
	runErr := make(chan error, 1)
	go func() {
		runErr <- client.Run(ctx)
	}()

	select {
	case event := <-eventCh:
		if event.CallbackID != "callback-1" {
			t.Fatalf("unexpected callback id: %s", event.CallbackID)
		}
		if event.EventID != "group-mention-event-1" || event.EventType != EventTypeNewMentionedMessageReceivedFromGroupChat {
			t.Fatalf("unexpected event metadata: %+v", event)
		}
		if event.Timestamp != 1611220944 || event.AppID != "app-1" {
			t.Fatalf("unexpected event app/timestamp: %+v", event)
		}
		if event.Event.GroupID != "group-1" {
			t.Fatalf("unexpected group id: %s", event.Event.GroupID)
		}
		message := event.Event.Message
		if message.MessageID != "msg-1" ||
			message.QuotedMessageID != "quoted-msg-1" ||
			message.ThreadID != "thread-1" ||
			message.MessageSentTime != 1611220933 ||
			message.Tag != MessageTagText {
			t.Fatalf("unexpected message metadata: %+v", message)
		}
		if message.Sender.SeaTalkID != "1239487273" ||
			message.Sender.EmployeeCode != "e_12345678" ||
			message.Sender.Email != "sample@seatalk.biz" ||
			message.Sender.SenderType != 1 {
			t.Fatalf("unexpected sender: %+v", message.Sender)
		}
		if message.Text == nil || message.Text.PlainText != "@bot hello" {
			t.Fatalf("unexpected text: %+v", message.Text)
		}
		if len(message.Text.MentionedList) != 1 ||
			message.Text.MentionedList[0].Username != "bot" ||
			message.Text.MentionedList[0].SeaTalkID != "bot-1" ||
			message.Text.MentionedList[0].Location == nil ||
			*message.Text.MentionedList[0].Location != 0 ||
			message.Text.MentionedList[0].Length == nil ||
			*message.Text.MentionedList[0].Length != 4 {
			t.Fatalf("unexpected mentioned list: %+v", message.Text.MentionedList)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for new mentioned group chat event")
	}

	cancel()
	_ = client.Close()

	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("Run() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for Run()")
	}
	if err := waitServerErr(serverErr); err != nil {
		t.Fatal(err)
	}
}

func TestClientRunDispatchesInteractiveMessageClickAndAutoAck(t *testing.T) {
	eventCh := make(chan *InteractiveMessageClickEvent, 1)

	wsURL, serverErr, closeServer := newTestWSServer(t, func(conn *websocket.Conn) error {
		defer conn.Close()

		if _, err := readEnvelope(conn); err != nil {
			return err
		}
		if err := conn.WriteJSON(Envelope{
			Cmd:     CommandRegister,
			Header:  Header{AppID: "app-1", Token: "token-1"},
			Code:    CodeOK,
			Message: "ok",
		}); err != nil {
			return err
		}
		if err := conn.WriteJSON(Envelope{
			Cmd: CommandEvent,
			Header: Header{
				AppID:      "app-1",
				CallbackID: "callback-1",
			},
			Data: json.RawMessage(`{
				"event_id": "interactive-click-event-1",
				"event_type": "interactive_message_click",
				"timestamp": 1611220944,
				"app_id": "app-1",
				"event": {
					"message_id": "interactive-msg-1",
					"employee_code": "e_12345678",
					"email": "sample@seatalk.biz",
					"value": "approve",
					"seatalk_id": "1239487273",
					"group_id": "group-1",
					"thread_id": "thread-1"
				}
			}`),
			Code: CodeOK,
		}); err != nil {
			return err
		}

		ack, err := readEnvelope(conn)
		if err != nil {
			return err
		}
		if ack.Cmd != CommandAck || ack.Header.Token != "token-1" || ack.Header.CallbackID != "callback-1" {
			return fmt.Errorf("unexpected auto ack: %+v", ack)
		}

		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return nil
			}
		}
	})
	defer closeServer()

	dispatcher := NewEventDispatcher().
		OnInteractiveMessageClick(func(ctx context.Context, event *InteractiveMessageClickEvent) error {
			eventCh <- event
			return nil
		})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	client := NewClient("app-1", "secret-1", WithWebSocketURL(wsURL), WithEventDispatcher(dispatcher))
	runErr := make(chan error, 1)
	go func() {
		runErr <- client.Run(ctx)
	}()

	select {
	case event := <-eventCh:
		if event.CallbackID != "callback-1" {
			t.Fatalf("unexpected callback id: %s", event.CallbackID)
		}
		if event.EventID != "interactive-click-event-1" || event.EventType != EventTypeInteractiveMessageClick {
			t.Fatalf("unexpected event metadata: %+v", event)
		}
		if event.Timestamp != 1611220944 || event.AppID != "app-1" {
			t.Fatalf("unexpected event app/timestamp: %+v", event)
		}
		if event.Event.MessageID != "interactive-msg-1" ||
			event.Event.EmployeeCode != "e_12345678" ||
			event.Event.Email != "sample@seatalk.biz" ||
			event.Event.Value != "approve" ||
			event.Event.SeaTalkID != "1239487273" ||
			event.Event.GroupID != "group-1" ||
			event.Event.ThreadID != "thread-1" {
			t.Fatalf("unexpected interactive click detail: %+v", event.Event)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for interactive message click event")
	}

	cancel()
	_ = client.Close()

	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("Run() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for Run()")
	}
	if err := waitServerErr(serverErr); err != nil {
		t.Fatal(err)
	}
}

func TestClientRunDispatchesNewMessageReceivedFromThreadAndAutoAck(t *testing.T) {
	eventCh := make(chan *NewMessageReceivedFromThreadEvent, 1)

	wsURL, serverErr, closeServer := newTestWSServer(t, func(conn *websocket.Conn) error {
		defer conn.Close()

		if _, err := readEnvelope(conn); err != nil {
			return err
		}
		if err := conn.WriteJSON(Envelope{
			Cmd:     CommandRegister,
			Header:  Header{AppID: "app-1", Token: "token-1"},
			Code:    CodeOK,
			Message: "ok",
		}); err != nil {
			return err
		}
		if err := conn.WriteJSON(Envelope{
			Cmd: CommandEvent,
			Header: Header{
				AppID:      "app-1",
				CallbackID: "callback-1",
			},
			Data: json.RawMessage(`{
				"event_id": "thread-message-event-1",
				"event_type": "new_message_received_from_thread",
				"timestamp": 1611220944,
				"app_id": "app-1",
				"event": {
					"group_id": "group-1",
					"message": {
						"message_id": "msg-1",
						"quoted_message_id": "quoted-msg-1",
						"thread_id": "thread-1",
						"sender": {
							"seatalk_id": "1239487273",
							"employee_code": "e_12345678",
							"email": "sample@seatalk.biz",
							"sender_type": 1
						},
						"message_sent_time": 1611220933,
						"tag": "text",
						"text": {
							"plain_text": "@bot hello from thread",
							"mentioned_list": [
								{
									"username": "bot",
									"seatalk_id": "bot-1",
									"employee_code": "",
									"email": ""
								}
							]
						}
					}
				}
			}`),
			Code: CodeOK,
		}); err != nil {
			return err
		}

		ack, err := readEnvelope(conn)
		if err != nil {
			return err
		}
		if ack.Cmd != CommandAck || ack.Header.Token != "token-1" || ack.Header.CallbackID != "callback-1" {
			return fmt.Errorf("unexpected auto ack: %+v", ack)
		}

		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return nil
			}
		}
	})
	defer closeServer()

	dispatcher := NewEventDispatcher().
		OnNewMessageReceivedFromThread(func(ctx context.Context, event *NewMessageReceivedFromThreadEvent) error {
			eventCh <- event
			return nil
		})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	client := NewClient("app-1", "secret-1", WithWebSocketURL(wsURL), WithEventDispatcher(dispatcher))
	runErr := make(chan error, 1)
	go func() {
		runErr <- client.Run(ctx)
	}()

	select {
	case event := <-eventCh:
		if event.CallbackID != "callback-1" {
			t.Fatalf("unexpected callback id: %s", event.CallbackID)
		}
		if event.EventID != "thread-message-event-1" || event.EventType != EventTypeNewMessageReceivedFromThread {
			t.Fatalf("unexpected event metadata: %+v", event)
		}
		if event.Timestamp != 1611220944 || event.AppID != "app-1" {
			t.Fatalf("unexpected event app/timestamp: %+v", event)
		}
		if event.Event.GroupID != "group-1" {
			t.Fatalf("unexpected group id: %s", event.Event.GroupID)
		}
		message := event.Event.Message
		if message.MessageID != "msg-1" ||
			message.QuotedMessageID != "quoted-msg-1" ||
			message.ThreadID != "thread-1" ||
			message.MessageSentTime != 1611220933 ||
			message.Tag != MessageTagText {
			t.Fatalf("unexpected message metadata: %+v", message)
		}
		if message.Sender.SeaTalkID != "1239487273" ||
			message.Sender.EmployeeCode != "e_12345678" ||
			message.Sender.Email != "sample@seatalk.biz" ||
			message.Sender.SenderType != 1 {
			t.Fatalf("unexpected sender: %+v", message.Sender)
		}
		if message.Text == nil || message.Text.PlainText != "@bot hello from thread" {
			t.Fatalf("unexpected text: %+v", message.Text)
		}
		if len(message.Text.MentionedList) != 1 ||
			message.Text.MentionedList[0].Username != "bot" ||
			message.Text.MentionedList[0].SeaTalkID != "bot-1" {
			t.Fatalf("unexpected mentioned list: %+v", message.Text.MentionedList)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for new message received from thread event")
	}

	cancel()
	_ = client.Close()

	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("Run() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for Run()")
	}
	if err := waitServerErr(serverErr); err != nil {
		t.Fatal(err)
	}
}

func TestClientRunDispatchesNewMessageReceivedFromGroupChatAndAutoAck(t *testing.T) {
	eventCh := make(chan *NewMessageReceivedFromGroupChatEvent, 1)

	wsURL, serverErr, closeServer := newTestWSServer(t, func(conn *websocket.Conn) error {
		defer conn.Close()

		if _, err := readEnvelope(conn); err != nil {
			return err
		}
		if err := conn.WriteJSON(Envelope{
			Cmd:     CommandRegister,
			Header:  Header{AppID: "app-1", Token: "token-1"},
			Code:    CodeOK,
			Message: "ok",
		}); err != nil {
			return err
		}
		if err := conn.WriteJSON(Envelope{
			Cmd: CommandEvent,
			Header: Header{
				AppID:      "app-1",
				CallbackID: "callback-1",
			},
			Data: json.RawMessage(`{
				"event_id": "group-message-event-1",
				"event_type": "new_message_received_from_group_chat",
				"timestamp": 1611220944,
				"app_id": "app-1",
				"event": {
					"group_id": "group-1",
					"message": {
						"message_id": "msg-1",
						"quoted_message_id": "quoted-msg-1",
						"thread_id": "thread-1",
						"sender": {
							"seatalk_id": "1239487273",
							"employee_code": "e_12345678",
							"email": "sample@seatalk.biz",
							"sender_type": 1
						},
						"message_sent_time": 1611220933,
						"tag": "edit",
						"text": {
							"plain_text": "hello group",
							"mentioned_list": [
								{
									"username": "bot",
									"seatalk_id": "bot-1",
									"employee_code": "",
									"email": ""
								}
							]
						},
						"interactive_message": {
							"elements": [
								{
									"element_type": "button",
									"button": {
										"button_type": "callback",
										"text": "Approve",
										"mobile_link": {
											"type": "web",
											"path": "/approve",
											"params": {"id": "card-1"}
										}
									}
								}
							],
							"mentioned_list": []
						},
						"combined_forwarded_chat_history": {
							"content": [
								{
									"tag": "text",
									"message_sent_time": 1611220800,
									"text": {
										"plain_text": "forwarded group text",
										"mentioned_list": []
									}
								}
							]
						},
						"image": {
							"content": "https://example.com/image.png"
						},
						"video": {
							"content": "https://example.com/video.mp4"
						},
						"file": {
							"content": "https://example.com/file.txt",
							"filename": "file.txt"
						},
						"change_members": {
							"added_members": [
								{
									"seatalk_id": "added-1",
									"employee_code": "e_added_1",
									"email": "added1@seatalk.biz"
								}
							],
							"removed_members": [
								{
									"seatalk_id": "removed-1",
									"employee_code": "e_removed_1",
									"email": "removed1@seatalk.biz"
								}
							],
							"is_newly_created": true
						},
						"recall_msgs": {
							"messages": [
								{
									"message_id": "msg-2",
									"thread_id": "thread-2"
								}
							]
						},
						"group_removed": {
							"op_user": {
								"seatalk_id": "op-1",
								"employee_code": "e_op_1",
								"email": "op1@seatalk.biz"
							}
						},
						"edit": {
							"message": {
								"message_id": "msg-0",
								"thread_id": "thread-1",
								"sender": {
									"seatalk_id": "editor-1",
									"employee_code": "e_editor_1",
									"email": "editor1@seatalk.biz"
								},
								"message_sent_time": 1611220900,
								"tag": "text",
								"text": {
									"plain_text": "edited text",
									"mentioned_list": []
								}
							}
						}
					}
				}
			}`),
			Code: CodeOK,
		}); err != nil {
			return err
		}

		ack, err := readEnvelope(conn)
		if err != nil {
			return err
		}
		if ack.Cmd != CommandAck || ack.Header.Token != "token-1" || ack.Header.CallbackID != "callback-1" {
			return fmt.Errorf("unexpected auto ack: %+v", ack)
		}

		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return nil
			}
		}
	})
	defer closeServer()

	dispatcher := NewEventDispatcher().
		OnNewMessageReceivedFromGroupChat(func(ctx context.Context, event *NewMessageReceivedFromGroupChatEvent) error {
			eventCh <- event
			return nil
		})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	client := NewClient("app-1", "secret-1", WithWebSocketURL(wsURL), WithEventDispatcher(dispatcher))
	runErr := make(chan error, 1)
	go func() {
		runErr <- client.Run(ctx)
	}()

	select {
	case event := <-eventCh:
		if event.CallbackID != "callback-1" {
			t.Fatalf("unexpected callback id: %s", event.CallbackID)
		}
		if event.EventID != "group-message-event-1" || event.EventType != EventTypeNewMessageReceivedFromGroupChat {
			t.Fatalf("unexpected event metadata: %+v", event)
		}
		if event.Timestamp != 1611220944 || event.AppID != "app-1" {
			t.Fatalf("unexpected event app/timestamp: %+v", event)
		}
		if event.Event.GroupID != "group-1" {
			t.Fatalf("unexpected group id: %s", event.Event.GroupID)
		}
		message := event.Event.Message
		if message.MessageID != "msg-1" ||
			message.QuotedMessageID != "quoted-msg-1" ||
			message.ThreadID != "thread-1" ||
			message.MessageSentTime != 1611220933 ||
			message.Tag != MessageTagEdit {
			t.Fatalf("unexpected message metadata: %+v", message)
		}
		if message.Sender.SeaTalkID != "1239487273" ||
			message.Sender.EmployeeCode != "e_12345678" ||
			message.Sender.Email != "sample@seatalk.biz" ||
			message.Sender.SenderType != 1 {
			t.Fatalf("unexpected sender: %+v", message.Sender)
		}
		if message.Text == nil || message.Text.PlainText != "hello group" || len(message.Text.MentionedList) != 1 {
			t.Fatalf("unexpected text: %+v", message.Text)
		}
		if message.InteractiveMessage == nil ||
			len(message.InteractiveMessage.Elements) != 1 ||
			message.InteractiveMessage.Elements[0].Button == nil ||
			message.InteractiveMessage.Elements[0].Button.MobileLink == nil ||
			message.InteractiveMessage.Elements[0].Button.MobileLink.Params["id"] != "card-1" {
			t.Fatalf("unexpected interactive message: %+v", message.InteractiveMessage)
		}
		if message.CombinedForwardedChatHistory == nil ||
			len(message.CombinedForwardedChatHistory.Content) != 1 ||
			message.CombinedForwardedChatHistory.Content[0].Text == nil ||
			message.CombinedForwardedChatHistory.Content[0].Text.PlainText != "forwarded group text" {
			t.Fatalf("unexpected combined forwarded chat history: %+v", message.CombinedForwardedChatHistory)
		}
		if message.Image == nil || message.Image.Content != "https://example.com/image.png" {
			t.Fatalf("unexpected image: %+v", message.Image)
		}
		if message.Video == nil || message.Video.Content != "https://example.com/video.mp4" {
			t.Fatalf("unexpected video: %+v", message.Video)
		}
		if message.File == nil || message.File.Content != "https://example.com/file.txt" || message.File.Filename != "file.txt" {
			t.Fatalf("unexpected file: %+v", message.File)
		}
		if message.ChangeMembers == nil ||
			!message.ChangeMembers.IsNewlyCreated ||
			len(message.ChangeMembers.AddedMembers) != 1 ||
			message.ChangeMembers.AddedMembers[0].SeaTalkID != "added-1" ||
			len(message.ChangeMembers.RemovedMembers) != 1 ||
			message.ChangeMembers.RemovedMembers[0].SeaTalkID != "removed-1" {
			t.Fatalf("unexpected change members: %+v", message.ChangeMembers)
		}
		if message.RecallMsgs == nil ||
			len(message.RecallMsgs.Messages) != 1 ||
			message.RecallMsgs.Messages[0].MessageID != "msg-2" ||
			message.RecallMsgs.Messages[0].ThreadID != "thread-2" {
			t.Fatalf("unexpected recall messages: %+v", message.RecallMsgs)
		}
		if message.GroupRemoved == nil || message.GroupRemoved.OpUser.SeaTalkID != "op-1" {
			t.Fatalf("unexpected group removed payload: %+v", message.GroupRemoved)
		}
		if message.Edit == nil ||
			message.Edit.Message.MessageID != "msg-0" ||
			message.Edit.Message.Tag != MessageTagText ||
			message.Edit.Message.Text == nil ||
			message.Edit.Message.Text.PlainText != "edited text" {
			t.Fatalf("unexpected edit payload: %+v", message.Edit)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for new message received from group chat event")
	}

	cancel()
	_ = client.Close()

	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("Run() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for Run()")
	}
	if err := waitServerErr(serverErr); err != nil {
		t.Fatal(err)
	}
}

func TestClientRunDispatchesBotAddedToGroupChatAndAutoAck(t *testing.T) {
	eventCh := make(chan *BotAddedToGroupChatEvent, 1)

	wsURL, serverErr, closeServer := newTestWSServer(t, func(conn *websocket.Conn) error {
		defer conn.Close()

		if _, err := readEnvelope(conn); err != nil {
			return err
		}
		if err := conn.WriteJSON(Envelope{
			Cmd:     CommandRegister,
			Header:  Header{AppID: "app-1", Token: "token-1"},
			Code:    CodeOK,
			Message: "ok",
		}); err != nil {
			return err
		}
		if err := conn.WriteJSON(Envelope{
			Cmd: CommandEvent,
			Header: Header{
				AppID:      "app-1",
				CallbackID: "callback-1",
			},
			Data: json.RawMessage(`{
				"event_id": "bot-added-event-1",
				"event_type": "bot_added_to_group_chat",
				"timestamp": 1611220944,
				"app_id": "app-1",
				"event": {
					"group": {
						"group_id": "group-1",
						"group_name": "Engineering",
						"external": true,
						"group_settings": {
							"chat_history_for_new_members": "7 days",
							"can_notify_with_at_all": true,
							"can_view_member_list": false
						}
					},
					"inviter": {
						"seatalk_id": "1239487273",
						"employee_code": "e_12345678",
						"email": "sample@seatalk.biz"
					}
				}
			}`),
			Code: CodeOK,
		}); err != nil {
			return err
		}

		ack, err := readEnvelope(conn)
		if err != nil {
			return err
		}
		if ack.Cmd != CommandAck || ack.Header.Token != "token-1" || ack.Header.CallbackID != "callback-1" {
			return fmt.Errorf("unexpected auto ack: %+v", ack)
		}

		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return nil
			}
		}
	})
	defer closeServer()

	dispatcher := NewEventDispatcher().
		OnBotAddedToGroupChat(func(ctx context.Context, event *BotAddedToGroupChatEvent) error {
			eventCh <- event
			return nil
		})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	client := NewClient("app-1", "secret-1", WithWebSocketURL(wsURL), WithEventDispatcher(dispatcher))
	runErr := make(chan error, 1)
	go func() {
		runErr <- client.Run(ctx)
	}()

	select {
	case event := <-eventCh:
		if event.CallbackID != "callback-1" {
			t.Fatalf("unexpected callback id: %s", event.CallbackID)
		}
		if event.EventID != "bot-added-event-1" || event.EventType != EventTypeBotAddedToGroupChat {
			t.Fatalf("unexpected event metadata: %+v", event)
		}
		if event.Timestamp != 1611220944 || event.AppID != "app-1" {
			t.Fatalf("unexpected event app/timestamp: %+v", event)
		}
		group := event.Event.Group
		if group.GroupID != "group-1" || group.GroupName != "Engineering" || !group.External {
			t.Fatalf("unexpected group: %+v", group)
		}
		if group.GroupSettings.ChatHistoryForNewMembers != "7 days" ||
			!group.GroupSettings.CanNotifyWithAtAll ||
			group.GroupSettings.CanViewMemberList {
			t.Fatalf("unexpected group settings: %+v", group.GroupSettings)
		}
		if event.Event.Inviter.SeaTalkID != "1239487273" ||
			event.Event.Inviter.EmployeeCode != "e_12345678" ||
			event.Event.Inviter.Email != "sample@seatalk.biz" {
			t.Fatalf("unexpected inviter: %+v", event.Event.Inviter)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for bot added to group chat event")
	}

	cancel()
	_ = client.Close()

	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("Run() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for Run()")
	}
	if err := waitServerErr(serverErr); err != nil {
		t.Fatal(err)
	}
}

func TestClientRunDispatchesBotRemovedFromGroupChatAndAutoAck(t *testing.T) {
	eventCh := make(chan *BotRemovedFromGroupChatEvent, 1)

	wsURL, serverErr, closeServer := newTestWSServer(t, func(conn *websocket.Conn) error {
		defer conn.Close()

		if _, err := readEnvelope(conn); err != nil {
			return err
		}
		if err := conn.WriteJSON(Envelope{
			Cmd:     CommandRegister,
			Header:  Header{AppID: "app-1", Token: "token-1"},
			Code:    CodeOK,
			Message: "ok",
		}); err != nil {
			return err
		}
		if err := conn.WriteJSON(Envelope{
			Cmd: CommandEvent,
			Header: Header{
				AppID:      "app-1",
				CallbackID: "callback-1",
			},
			Data: json.RawMessage(`{
				"event_id": "bot-removed-event-1",
				"event_type": "bot_removed_from_group_chat",
				"timestamp": 1611220944,
				"app_id": "app-1",
				"event": {
					"group_id": "group-1",
					"remover": {
						"seatalk_id": "1239487273",
						"employee_code": "e_12345678",
						"email": "sample@seatalk.biz"
					}
				}
			}`),
			Code: CodeOK,
		}); err != nil {
			return err
		}

		ack, err := readEnvelope(conn)
		if err != nil {
			return err
		}
		if ack.Cmd != CommandAck || ack.Header.Token != "token-1" || ack.Header.CallbackID != "callback-1" {
			return fmt.Errorf("unexpected auto ack: %+v", ack)
		}

		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return nil
			}
		}
	})
	defer closeServer()

	dispatcher := NewEventDispatcher().
		OnBotRemovedFromGroupChat(func(ctx context.Context, event *BotRemovedFromGroupChatEvent) error {
			eventCh <- event
			return nil
		})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	client := NewClient("app-1", "secret-1", WithWebSocketURL(wsURL), WithEventDispatcher(dispatcher))
	runErr := make(chan error, 1)
	go func() {
		runErr <- client.Run(ctx)
	}()

	select {
	case event := <-eventCh:
		if event.CallbackID != "callback-1" {
			t.Fatalf("unexpected callback id: %s", event.CallbackID)
		}
		if event.EventID != "bot-removed-event-1" || event.EventType != EventTypeBotRemovedFromGroupChat {
			t.Fatalf("unexpected event metadata: %+v", event)
		}
		if event.Timestamp != 1611220944 || event.AppID != "app-1" {
			t.Fatalf("unexpected event app/timestamp: %+v", event)
		}
		if event.Event.GroupID != "group-1" {
			t.Fatalf("unexpected group id: %s", event.Event.GroupID)
		}
		if event.Event.Remover.SeaTalkID != "1239487273" ||
			event.Event.Remover.EmployeeCode != "e_12345678" ||
			event.Event.Remover.Email != "sample@seatalk.biz" {
			t.Fatalf("unexpected remover: %+v", event.Event.Remover)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for bot removed from group chat event")
	}

	cancel()
	_ = client.Close()

	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("Run() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for Run()")
	}
	if err := waitServerErr(serverErr); err != nil {
		t.Fatal(err)
	}
}

func TestClientRunDispatchesGroupChatConvertedToExternalGroupAndAutoAck(t *testing.T) {
	eventCh := make(chan *GroupChatConvertedToExternalGroupEvent, 1)

	wsURL, serverErr, closeServer := newTestWSServer(t, func(conn *websocket.Conn) error {
		defer conn.Close()

		if _, err := readEnvelope(conn); err != nil {
			return err
		}
		if err := conn.WriteJSON(Envelope{
			Cmd:     CommandRegister,
			Header:  Header{AppID: "app-1", Token: "token-1"},
			Code:    CodeOK,
			Message: "ok",
		}); err != nil {
			return err
		}
		if err := conn.WriteJSON(Envelope{
			Cmd: CommandEvent,
			Header: Header{
				AppID:      "app-1",
				CallbackID: "callback-1",
			},
			Data: json.RawMessage(`{
				"event_id": "external-group-event-1",
				"event_type": "group_chat_converted_to_external_group",
				"timestamp": 1611220944,
				"app_id": "app-1",
				"event": {
					"group_id": "group-1",
					"operator": {
						"seatalk_id": "1239487273",
						"employee_code": "e_12345678",
						"email": "sample@seatalk.biz"
					}
				}
			}`),
			Code: CodeOK,
		}); err != nil {
			return err
		}

		ack, err := readEnvelope(conn)
		if err != nil {
			return err
		}
		if ack.Cmd != CommandAck || ack.Header.Token != "token-1" || ack.Header.CallbackID != "callback-1" {
			return fmt.Errorf("unexpected auto ack: %+v", ack)
		}

		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return nil
			}
		}
	})
	defer closeServer()

	dispatcher := NewEventDispatcher().
		OnGroupChatConvertedToExternalGroup(func(ctx context.Context, event *GroupChatConvertedToExternalGroupEvent) error {
			eventCh <- event
			return nil
		})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	client := NewClient("app-1", "secret-1", WithWebSocketURL(wsURL), WithEventDispatcher(dispatcher))
	runErr := make(chan error, 1)
	go func() {
		runErr <- client.Run(ctx)
	}()

	select {
	case event := <-eventCh:
		if event.CallbackID != "callback-1" {
			t.Fatalf("unexpected callback id: %s", event.CallbackID)
		}
		if event.EventID != "external-group-event-1" || event.EventType != EventTypeGroupChatConvertedToExternalGroup {
			t.Fatalf("unexpected event metadata: %+v", event)
		}
		if event.Timestamp != 1611220944 || event.AppID != "app-1" {
			t.Fatalf("unexpected event app/timestamp: %+v", event)
		}
		if event.Event.GroupID != "group-1" {
			t.Fatalf("unexpected group id: %s", event.Event.GroupID)
		}
		if event.Event.Operator.SeaTalkID != "1239487273" ||
			event.Event.Operator.EmployeeCode != "e_12345678" ||
			event.Event.Operator.Email != "sample@seatalk.biz" {
			t.Fatalf("unexpected operator: %+v", event.Event.Operator)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for group chat converted to external group event")
	}

	cancel()
	_ = client.Close()

	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("Run() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for Run()")
	}
	if err := waitServerErr(serverErr); err != nil {
		t.Fatal(err)
	}
}

func TestClientConnectReturnsRegisterError(t *testing.T) {
	wsURL, serverErr, closeServer := newTestWSServer(t, func(conn *websocket.Conn) error {
		defer conn.Close()
		if _, err := readEnvelope(conn); err != nil {
			return err
		}
		return conn.WriteJSON(Envelope{
			Cmd:     CommandRegister,
			Code:    CodeError,
			Message: "bad credential",
		})
	})
	defer closeServer()

	client := NewClient("app-1", "secret-1", WithWebSocketURL(wsURL))
	_, err := client.Connect(context.Background())
	if err == nil {
		t.Fatal("Connect() error = nil")
	}
	registerErr, ok := err.(*RegisterError)
	if !ok {
		t.Fatalf("Connect() error type = %T", err)
	}
	if registerErr.Code != CodeError || registerErr.Message != "bad credential" {
		t.Fatalf("unexpected register error: %+v", registerErr)
	}
	if err := waitServerErr(serverErr); err != nil {
		t.Fatal(err)
	}
}

func newTestWSServer(t *testing.T, handler func(conn *websocket.Conn) error) (string, <-chan error, func()) {
	t.Helper()

	errCh := make(chan error, 1)
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			errCh <- err
			return
		}
		errCh <- handler(conn)
	}))

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	return wsURL, errCh, server.Close
}

func readEnvelope(conn *websocket.Conn) (*Envelope, error) {
	var env Envelope
	if err := conn.ReadJSON(&env); err != nil {
		return nil, err
	}
	return &env, nil
}

func waitServerErr(errCh <-chan error) error {
	select {
	case err := <-errCh:
		return err
	case <-time.After(time.Second):
		return fmt.Errorf("timeout waiting for websocket server")
	}
}
