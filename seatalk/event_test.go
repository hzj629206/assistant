package seatalk

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"
)

func TestCalculateSignature(t *testing.T) {
	t.Parallel()

	body := []byte(`{"event_type":"event_verification"}`)
	got := CalculateSignature(body, "secret")
	want := "3e9595c32357f0705bd369d72524b28386ff5ca29122b27ffe29bc76c456a32f"
	if got != want {
		t.Fatalf("CalculateSignature() = %q, want %q", got, want)
	}
}

func TestSignatureFromHeader(t *testing.T) {
	t.Parallel()

	header := http.Header{}
	header.Set("Signature", "ABCDEF")
	if got := SignatureFromHeader(header); got != "abcdef" {
		t.Fatalf("SignatureFromHeader() = %q, want %q", got, "abcdef")
	}
}

func TestVerifySignature(t *testing.T) {
	t.Parallel()

	body := []byte(`{"hello":"world"}`)
	secret := "demo-secret"
	signature := CalculateSignature(body, secret)

	tests := []struct {
		name    string
		header  http.Header
		wantErr error
	}{
		{
			name: "valid signature",
			header: http.Header{
				"Signature": []string{signature},
			},
		},
		{
			name:    "missing signature",
			header:  http.Header{},
			wantErr: ErrInvalidSignature,
		},
		{
			name: "invalid signature",
			header: http.Header{
				"Signature": []string{"deadbeef"},
			},
			wantErr: ErrInvalidSignature,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := VerifySignature(tc.header, body, secret)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("VerifySignature() error = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestDecodeEventNewMessageReceivedFromThreadSupportsCombinedForwardedChatHistory(t *testing.T) {
	t.Parallel()

	req := EventRequest{
		EventType: EventTypeNewMessageReceivedFromThread,
		Event: json.RawMessage(`{
            "group_id": "group-1",
            "message": {
                "message_id": "message-1",
                "quoted_message_id": "",
                "thread_id": "thread-1",
                "sender": {
                    "seatalk_id": "123",
                    "employee_code": "e_123",
                    "email": "sample@seatalk.biz",
                    "sender_type": 1
                },
                "message_sent_time": 1687764109,
                "tag": "combined_forwarded_chat_history",
                "combined_forwarded_chat_history": {
                    "content": [
                        {
                            "tag": "text",
                            "text": {
                                "plain_text": "hello",
                                "mentioned_list": []
                            }
                        }
                    ]
                }
            }
        }`),
	}

	event, err := req.decodeEvent()
	if err != nil {
		t.Fatalf("DecodeEvent() error = %v", err)
	}

	got, ok := event.(*NewMessageReceivedFromThreadEvent)
	if !ok {
		t.Fatalf("DecodeEvent() type = %T, want *NewMessageReceivedFromThreadEvent", event)
	}
	if got.Message.CombinedForwardedChatHistory == nil {
		t.Fatal("combined forwarded chat history payload was not decoded")
	}
	if len(got.Message.CombinedForwardedChatHistory.Content) != 1 {
		t.Fatalf("combined forwarded content = %#v, want one top-level entry", got.Message.CombinedForwardedChatHistory.Content)
	}
}

func TestDecodeEventMessageFromBotSubscriberSupportsCombinedForwardedChatHistory(t *testing.T) {
	t.Parallel()

	req := EventRequest{
		EventType: EventTypeMessageFromBotSubscriber,
		Event: json.RawMessage(`{
            "seatalk_id": "123",
            "employee_code": "e_123",
            "email": "sample@seatalk.biz",
            "message": {
                "message_id": "message-1",
                "quoted_message_id": "",
                "thread_id": "thread-1",
                "tag": "combined_forwarded_chat_history",
                "combined_forwarded_chat_history": {
                    "content": [
                        {
                            "tag": "text",
                            "text": {
                                "plain_text": "hello",
                                "mentioned_list": []
                            }
                        }
                    ]
                }
            }
        }`),
	}

	event, err := req.decodeEvent()
	if err != nil {
		t.Fatalf("DecodeEvent() error = %v", err)
	}

	got, ok := event.(*MessageFromBotSubscriberEvent)
	if !ok {
		t.Fatalf("DecodeEvent() type = %T, want *MessageFromBotSubscriberEvent", event)
	}
	if got.Message.CombinedForwardedChatHistory == nil {
		t.Fatal("combined forwarded chat history payload was not decoded")
	}
	if len(got.Message.CombinedForwardedChatHistory.Content) != 1 {
		t.Fatalf("combined forwarded content = %#v, want one top-level entry", got.Message.CombinedForwardedChatHistory.Content)
	}
}

func TestDecodeEventNewMessageReceivedFromThreadSupportsInteractiveMessage(t *testing.T) {
	t.Parallel()

	req := EventRequest{
		EventType: EventTypeNewMessageReceivedFromThread,
		Event: json.RawMessage(`{
            "group_id": "group-1",
            "message": {
                "message_id": "message-1",
                "quoted_message_id": "",
                "thread_id": "thread-1",
                "sender": {
                    "seatalk_id": "123",
                    "employee_code": "e_123",
                    "email": "sample@seatalk.biz",
                    "sender_type": 1
                },
                "message_sent_time": 1687764109,
                "tag": "interactive_message",
                "interactive_message": {
                    "elements": [
                        {"element_type": "title", "title": {"text": "Title"}}
                    ],
                    "mentioned_list": [
                        {"username": "Good Bot", "seatalk_id": "1234567"}
                    ]
                }
            }
        }`),
	}

	event, err := req.decodeEvent()
	if err != nil {
		t.Fatalf("DecodeEvent() error = %v", err)
	}

	got, ok := event.(*NewMessageReceivedFromThreadEvent)
	if !ok {
		t.Fatalf("DecodeEvent() type = %T, want *NewMessageReceivedFromThreadEvent", event)
	}
	if got.Message.InteractiveMessage == nil {
		t.Fatal("interactive message payload was not decoded")
	}
	if len(got.Message.InteractiveMessage.Elements) != 1 {
		t.Fatalf("interactive element count = %d, want 1", len(got.Message.InteractiveMessage.Elements))
	}
	if len(got.Message.InteractiveMessage.MentionedList) != 1 {
		t.Fatalf("interactive mentioned_list count = %d, want 1", len(got.Message.InteractiveMessage.MentionedList))
	}
}

func TestDecodeEventNewMentionedMessageReceivedFromGroupChatSupportsInteractiveMessage(t *testing.T) {
	t.Parallel()

	req := EventRequest{
		EventType: EventTypeNewMentionedMessageFromGroupChat,
		Event: json.RawMessage(`{
            "group_id": "group-1",
            "message": {
                "message_id": "message-1",
                "quoted_message_id": "",
                "thread_id": "thread-1",
                "sender": {
                    "seatalk_id": "123",
                    "employee_code": "e_123",
                    "email": "sample@seatalk.biz",
                    "sender_type": 1
                },
                "message_sent_time": 1687764109,
                "tag": "interactive_message",
                "interactive_message": {
                    "elements": [
                        {"element_type": "title", "title": {"text": "Title"}}
                    ],
                    "mentioned_list": [
                        {"username": "Good Bot", "seatalk_id": "1234567"}
                    ]
                }
            }
        }`),
	}

	event, err := req.decodeEvent()
	if err != nil {
		t.Fatalf("DecodeEvent() error = %v", err)
	}

	got, ok := event.(*NewMentionedMessageReceivedFromGroupChatEvent)
	if !ok {
		t.Fatalf("DecodeEvent() type = %T, want *NewMentionedMessageReceivedFromGroupChatEvent", event)
	}
	if got.Message.InteractiveMessage == nil {
		t.Fatal("interactive message payload was not decoded")
	}
	if len(got.Message.InteractiveMessage.Elements) != 1 {
		t.Fatalf("interactive element count = %d, want 1", len(got.Message.InteractiveMessage.Elements))
	}
	if len(got.Message.InteractiveMessage.MentionedList) != 1 {
		t.Fatalf("interactive mentioned_list count = %d, want 1", len(got.Message.InteractiveMessage.MentionedList))
	}
}

func TestMessageFromBotSubscriberEventString(t *testing.T) {
	event := &MessageFromBotSubscriberEvent{
		SeatalkID: "u_1",
	}
	event.Message.MessageID = "msg-1"

	got := event.String()
	if got != "message_id=msg-1 sender=u_1" {
		t.Fatalf("unexpected event string: %q", got)
	}
}
