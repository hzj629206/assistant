package seatalk

import (
	"bytes"
	"context"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type testEventProcessor struct {
	called bool
	resp   any
	err    error
}

func (p *testEventProcessor) ProcessEvent(_ context.Context, _ EventRequest, _ Event) (any, error) {
	p.called = true
	return p.resp, p.err
}

func TestCallbackHandlerRespondsToVerificationWithoutProcessor(t *testing.T) {
	body := []byte(`{"event_id":"evt-1","event_type":"event_verification","timestamp":1,"app_id":"app-1","event":{"seatalk_challenge":"challenge-1"}}`)
	handler := NewCallbackHandler(Config{
		AppID:         "app-1",
		SigningSecret: "signing-secret",
	}, nil)

	req := httptest.NewRequest(http.MethodPost, "/callback", bytes.NewReader(body))
	req.Header.Set("Signature", CalculateSignature(body, "signing-secret"))
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("unexpected status code: %d", recorder.Code)
	}
	if got := recorder.Body.String(); got != "{\"seatalk_challenge\":\"challenge-1\"}\n" {
		t.Fatalf("unexpected response body: %q", got)
	}
}

func TestCallbackHandlerDelegatesNonVerificationToProcessor(t *testing.T) {
	body := []byte(`{"event_id":"evt-2","event_type":"user_enter_chatroom_with_bot","timestamp":1,"app_id":"app-1","event":{"employee_code":"e1","seatalk_id":"u1","email":"u1@example.com"}}`)
	processor := &testEventProcessor{
		resp: map[string]string{"status": "ok"},
	}
	handler := NewCallbackHandler(Config{
		AppID:         "app-1",
		SigningSecret: "signing-secret",
	}, processor)

	req := httptest.NewRequest(http.MethodPost, "/callback", bytes.NewReader(body))
	req.Header.Set("Signature", CalculateSignature(body, "signing-secret"))
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, req)

	if !processor.called {
		t.Fatal("expected processor to be called")
	}
	if recorder.Code != http.StatusOK {
		t.Fatalf("unexpected status code: %d", recorder.Code)
	}
	if got := recorder.Body.String(); got != "{\"status\":\"ok\"}\n" {
		t.Fatalf("unexpected response body: %q", got)
	}
}

func TestCallbackHandlerReturnsEmptyJSONObjectWhenProcessorReturnsNil(t *testing.T) {
	body := []byte(`{"event_id":"evt-3","event_type":"user_enter_chatroom_with_bot","timestamp":1,"app_id":"app-1","event":{"employee_code":"e1","seatalk_id":"u1","email":"u1@example.com"}}`)
	processor := &testEventProcessor{}
	handler := NewCallbackHandler(Config{
		AppID:         "app-1",
		SigningSecret: "signing-secret",
	}, processor)

	req := httptest.NewRequest(http.MethodPost, "/callback", bytes.NewReader(body))
	req.Header.Set("Signature", CalculateSignature(body, "signing-secret"))
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, req)

	if !processor.called {
		t.Fatal("expected processor to be called")
	}
	if recorder.Code != http.StatusOK {
		t.Fatalf("unexpected status code: %d", recorder.Code)
	}
	if got := recorder.Body.String(); got != "{}\n" {
		t.Fatalf("unexpected response body: %q", got)
	}
}

func TestCallbackHandlerLogsEventSummary(t *testing.T) {
	body := []byte(`{"event_id":"evt-4","event_type":"message_from_bot_subscriber","timestamp":1,"app_id":"app-1","event":{"seatalk_id":"u1","employee_code":"e1","email":"u1@example.com","message":{"message_id":"msg-1","quoted_message_id":"","thread_id":"","tag":"text","text":{"content":"hello","last_edited_time":0},"image":{"content":""},"file":{"content":"","filename":""},"video":{"content":""}}}}`)
	processor := &testEventProcessor{}
	handler := NewCallbackHandler(Config{
		AppID:         "app-1",
		SigningSecret: "signing-secret",
	}, processor)

	var buffer bytes.Buffer
	originalWriter := log.Writer()
	originalFlags := log.Flags()
	log.SetOutput(&buffer)
	log.SetFlags(0)
	defer log.SetOutput(originalWriter)
	defer log.SetFlags(originalFlags)

	req := httptest.NewRequest(http.MethodPost, "/callback", bytes.NewReader(body))
	req.Header.Set("Signature", CalculateSignature(body, "signing-secret"))
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, req)

	output := buffer.String()
	if !strings.Contains(output, "received event: event_id=evt-4 event_type=message_from_bot_subscriber message_id=msg-1 sender=u1") {
		t.Fatalf("missing event summary log: %q", output)
	}
}
