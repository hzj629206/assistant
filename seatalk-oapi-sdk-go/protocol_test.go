package seatalkoapisdk

import (
	"reflect"
	"testing"
)

func TestGroupChatMessageTextDoesNotExposeFormatContent(t *testing.T) {
	if _, ok := reflect.TypeOf(GroupChatMessageText{}).FieldByName("FormatContent"); ok {
		t.Fatal("GroupChatMessageText must not expose FormatContent")
	}
}

func TestGroupChatEventsUseTheSameMessageStructure(t *testing.T) {
	groupMessageType := reflect.TypeOf(GroupChatMessage{})

	mentionedType, ok := reflect.TypeOf(NewMentionedMessageReceivedFromGroupChatEventData{}).FieldByName("Message")
	if !ok || mentionedType.Type != groupMessageType {
		t.Fatalf("mentioned event message type = %v, want %v", mentionedType.Type, groupMessageType)
	}

	threadType, ok := reflect.TypeOf(NewMessageReceivedFromThreadEventData{}).FieldByName("Message")
	if !ok || threadType.Type != groupMessageType {
		t.Fatalf("thread event message type = %v, want %v", threadType.Type, groupMessageType)
	}
}
