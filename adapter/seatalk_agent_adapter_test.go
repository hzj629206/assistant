package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hzj629206/assistant/agent"
	"github.com/hzj629206/assistant/cache"
	"github.com/hzj629206/assistant/seatalk"
)

func TestSeaTalkAgentAdapterEnqueuesSupportedEvent(t *testing.T) {
	t.Parallel()

	runner := &testRunner{}
	dispatcher := agent.NewDispatcher(agent.DispatcherOptions{
		Store:              agent.NewConversationStore(cache.NewMemoryStorage()),
		Runner:             runner,
		WorkerCount:        1,
		NonTextMergeWindow: 10 * time.Millisecond,
	})
	seaTalkAdapter := newSeaTalkAgentAdapterWithClient(dispatcher, nil)

	if err := dispatcher.Start(); err != nil {
		t.Fatalf("start dispatcher failed: %v", err)
	}
	defer func() {
		if err := dispatcher.Shutdown(context.Background()); err != nil {
			t.Fatalf("shutdown dispatcher failed: %v", err)
		}
	}()

	req := seatalk.EventRequest{EventID: "evt-1", Timestamp: 1_700_000_000_000}
	event := &seatalk.MessageFromBotSubscriberEvent{
		EmployeeCode: "e_1",
		SeatalkID:    "u_1",
	}
	event.Message.MessageID = "msg-1"
	event.Message.Tag = "text"
	event.Message.Text.Content = "hello"

	if _, err := seaTalkAdapter.ProcessEvent(context.Background(), req, event); err != nil {
		t.Fatalf("process event failed: %v", err)
	}

	if err := waitForRunnerCalls(runner); err != nil {
		t.Fatal(err)
	}

	reqCall := runner.LastRequest()
	if reqCall.Message.Responder == nil {
		t.Fatal("expected responder to be attached")
	}
	if reqCall.Message.Text != "hello" {
		t.Fatalf("unexpected routed message text: %q", reqCall.Message.Text)
	}
}

func TestSeaTalkAgentAdapterSystemPromptMentionsUpdatingInteractiveCardsAfterActions(t *testing.T) {
	t.Parallel()

	adapter := newSeaTalkAgentAdapterWithClient(nil, seatalk.NewClient(seatalk.Config{}))
	prompt := adapter.SystemPrompt()
	if !strings.Contains(prompt, "After executing an interactive button action, decide whether the current interactive card should be updated to reflect the new state.") {
		t.Fatalf("system prompt missing interactive update guidance: %q", prompt)
	}
	if !strings.Contains(prompt, "prefer updating the current card instead of only sending a plain text follow-up") {
		t.Fatalf("system prompt missing stale-card guidance: %q", prompt)
	}
}

func TestSeaTalkAgentAdapterSystemPromptIncludesSeaTalkFormattingGuidance(t *testing.T) {
	t.Parallel()

	adapter := NewSeaTalkAgentAdapter(nil, seatalk.Config{})
	prompt := adapter.SystemPrompt()
	if !strings.Contains(prompt, "SeaTalk Markdown:") {
		t.Fatalf("system prompt missing text format guidance: %q", prompt)
	}
	if !strings.Contains(prompt, "SeaTalk Markdown only supports bold, italic, ordered lists, unordered lists, inline code, and code blocks.") {
		t.Fatalf("system prompt missing SeaTalk markdown guidance: %q", prompt)
	}
	if !strings.Contains(prompt, "SeaTalk Markdown lists support nesting, and nested list indentation must use tabs.") {
		t.Fatalf("system prompt missing nested list guidance: %q", prompt)
	}
	if !strings.Contains(prompt, "Prefer SeaTalk Markdown format for text content when replying in SeaTalk unless plain text is clearly more appropriate.") {
		t.Fatalf("system prompt missing Markdown preference guidance: %q", prompt)
	}
	if !strings.Contains(prompt, "SeaTalk Markdown code blocks do not support language types, so do not add language identifiers after the opening triple backticks.") {
		t.Fatalf("system prompt missing code block guidance: %q", prompt)
	}
	if !strings.Contains(prompt, "Working Context:") {
		t.Fatalf("system prompt missing working context section: %q", prompt)
	}
	if !strings.Contains(prompt, "Tasks may not be related to the current working directory, so do not assume file paths are based on it.") {
		t.Fatalf("system prompt missing path assumption guidance: %q", prompt)
	}
	if strings.Contains(prompt, "User mention:") {
		t.Fatalf("system prompt should not include global mention guidance: %q", prompt)
	}
	if strings.Contains(prompt, "When you need to mention a user in SeaTalk, use one of these tags:") {
		t.Fatalf("system prompt should not include global mention syntax guidance: %q", prompt)
	}
	if strings.Contains(prompt, "USER_EMAIL is limited to corporate addresses under @sea.com, @shopee.com, or @monee.com.") {
		t.Fatalf("system prompt should not include global mention email guidance: %q", prompt)
	}
	if strings.Contains(prompt, "Message sender format:") {
		t.Fatalf("system prompt should not inject sender format guidance: %q", prompt)
	}
	if strings.Contains(prompt, "Recognize yourself in conversation using the following form:") {
		t.Fatalf("system prompt should not inject bot identity guidance: %q", prompt)
	}
}

func TestSeaTalkAgentAdapterPreparesInboundVideoMessage(t *testing.T) {
	t.Parallel()

	runner := &testRunner{}
	dispatcher := agent.NewDispatcher(agent.DispatcherOptions{
		Store:              agent.NewConversationStore(cache.NewMemoryStorage()),
		Runner:             runner,
		WorkerCount:        1,
		NonTextMergeWindow: 10 * time.Millisecond,
	})
	client := seatalk.NewClient(
		seatalk.Config{AppID: "app-id", AppSecret: "app-secret"},
		seatalk.WithHTTPClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			switch req.URL.Path {
			case "/messaging/v2/file/demo-video":
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader("video-bytes")),
				}, nil
			default:
				t.Fatalf("unexpected path: %s", req.URL.Path)
				return nil, nil
			}
		})}),
		seatalk.WithTokenProvider(func(context.Context, *http.Client, string, string) (string, error) {
			return "token-123", nil
		}),
	)
	seaTalkAdapter := newSeaTalkAgentAdapterWithClient(dispatcher, client)

	if err := dispatcher.Start(); err != nil {
		t.Fatalf("start dispatcher failed: %v", err)
	}
	defer func() {
		if err := dispatcher.Shutdown(context.Background()); err != nil {
			t.Fatalf("shutdown dispatcher failed: %v", err)
		}
	}()

	req := seatalk.EventRequest{EventID: "evt-video-1", Timestamp: 1_700_000_000_000}
	event := &seatalk.MessageFromBotSubscriberEvent{
		EmployeeCode: "e_1",
		SeatalkID:    "u_1",
	}
	event.Message.MessageID = "msg-video-1"
	event.Message.Tag = "video"
	event.Message.Video.Content = "https://openapi.seatalk.io/messaging/v2/file/demo-video"

	if _, err := seaTalkAdapter.ProcessEvent(context.Background(), req, event); err != nil {
		t.Fatalf("process event failed: %v", err)
	}

	if err := waitForRunnerCalls(runner); err != nil {
		t.Fatal(err)
	}

	reqCall := runner.LastRequest()
	if reqCall.Message.Kind != agent.MessageKindVideo {
		t.Fatalf("unexpected message kind: %s", reqCall.Message.Kind)
	}
	if reqCall.Message.VideoPath == "" {
		t.Fatal("expected video path to be populated")
	}
	if len(reqCall.Message.VideoPaths) != 1 || reqCall.Message.VideoPaths[0] != reqCall.Message.VideoPath {
		t.Fatalf("unexpected video paths: %+v", reqCall.Message.VideoPaths)
	}
}

func TestSeaTalkAgentAdapterMergesPrivateRootThreadMessagesIntoLatestReplyTarget(t *testing.T) {
	t.Parallel()

	runner := &testRunner{}
	dispatcher := agent.NewDispatcher(agent.DispatcherOptions{
		Store:              agent.NewConversationStore(cache.NewMemoryStorage()),
		Runner:             runner,
		WorkerCount:        1,
		NonTextMergeWindow: 200 * time.Millisecond,
	})
	seaTalkAdapter := newSeaTalkAgentAdapterWithClient(dispatcher, nil)

	if err := dispatcher.Start(); err != nil {
		t.Fatalf("start dispatcher failed: %v", err)
	}
	defer func() {
		if err := dispatcher.Shutdown(context.Background()); err != nil {
			t.Fatalf("shutdown dispatcher failed: %v", err)
		}
	}()

	imageReq := seatalk.EventRequest{EventID: "evt-root-image-1", Timestamp: 1_700_000_000_000}
	imageEvent := &seatalk.MessageFromBotSubscriberEvent{
		EmployeeCode: "e_1",
	}
	imageEvent.Message.MessageID = "msg-root-image-1"
	imageEvent.Message.ThreadID = "0"
	imageEvent.Message.Tag = "file"
	imageEvent.Message.File.Content = "file-token"
	imageEvent.Message.File.Filename = "screenshot.png"

	if _, err := seaTalkAdapter.ProcessEvent(context.Background(), imageReq, imageEvent); err != nil {
		t.Fatalf("process file event failed: %v", err)
	}

	time.Sleep(50 * time.Millisecond)
	if calls := runner.Calls(); calls != 0 {
		t.Fatalf("unexpected runner call count before follow-up text: %d", calls)
	}

	textReq := seatalk.EventRequest{EventID: "evt-root-text-1", Timestamp: 1_700_000_000_100}
	textEvent := &seatalk.MessageFromBotSubscriberEvent{
		EmployeeCode: "e_1",
		Email:        "alice@example.com",
	}
	textEvent.Message.MessageID = "msg-root-text-1"
	textEvent.Message.ThreadID = "0"
	textEvent.Message.Tag = "text"
	textEvent.Message.Text.Content = "this explains the image"

	if _, err := seaTalkAdapter.ProcessEvent(context.Background(), textReq, textEvent); err != nil {
		t.Fatalf("process text event failed: %v", err)
	}

	if err := waitForRunnerCalls(runner); err != nil {
		t.Fatal(err)
	}

	reqCall := runner.LastRequest()
	if reqCall.Message.ConversationKey != "seatalk:private:e_1:0" {
		t.Fatalf("unexpected conversation key: %q", reqCall.Message.ConversationKey)
	}
	if len(reqCall.Message.MergedMessages()) != 2 {
		t.Fatalf("unexpected merged message count: %d", len(reqCall.Message.MergedMessages()))
	}

	responder, ok := reqCall.Message.Responder.(*SeaTalkResponder)
	if !ok {
		t.Fatalf("unexpected responder type: %T", reqCall.Message.Responder)
	}
	if responder.target.threadID != "msg-root-text-1" {
		t.Fatalf("unexpected reply thread id: %s", responder.target.threadID)
	}
}

func TestSeaTalkAgentAdapterEnqueuesInteractiveClickEvent(t *testing.T) {
	t.Parallel()

	runner := &testRunner{}
	dispatcher := agent.NewDispatcher(agent.DispatcherOptions{
		Store:       agent.NewConversationStore(cache.NewMemoryStorage()),
		Runner:      runner,
		WorkerCount: 1,
	})
	seaTalkAdapter := newSeaTalkAgentAdapterWithClient(dispatcher, nil)

	if err := dispatcher.Start(); err != nil {
		t.Fatalf("start dispatcher failed: %v", err)
	}
	defer func() {
		if err := dispatcher.Shutdown(context.Background()); err != nil {
			t.Fatalf("shutdown dispatcher failed: %v", err)
		}
	}()

	req := seatalk.EventRequest{EventID: "evt-interactive-1", Timestamp: 1_700_000_000_000}
	event := &seatalk.InteractiveMessageClickEvent{
		MessageID:    "msg-card-1",
		EmployeeCode: "e_1",
		Email:        "alice@example.com",
		ThreadID:     "thread-1",
		Value:        `{"action":"tool_call","tool_name":"seatalk_update_interactive_message","tool_input_json":"{\"elements\":[{\"element_type\":\"title\",\"title\":{\"text\":\"Approved\"}}]}"}`,
	}

	if _, err := seaTalkAdapter.ProcessEvent(context.Background(), req, event); err != nil {
		t.Fatalf("process event failed: %v", err)
	}

	if err := waitForRunnerCalls(runner); err != nil {
		t.Fatal(err)
	}

	reqCall := runner.LastRequest()
	if reqCall.Message.ConversationKey != "seatalk:private:e_1:thread-1" {
		t.Fatalf("unexpected conversation key: %q", reqCall.Message.ConversationKey)
	}
	if reqCall.Message.Responder == nil {
		t.Fatal("expected responder to be attached")
	}
	if !strings.Contains(reqCall.Message.Text, "User clicked a SeaTalk interactive message button.") {
		t.Fatalf("unexpected interactive click message: %q", reqCall.Message.Text)
	}
	if !strings.Contains(reqCall.Message.Text, `"action":"tool_call"`) {
		t.Fatalf("unexpected callback payload: %q", reqCall.Message.Text)
	}
}

func TestSeaTalkAgentAdapterEnqueuesInteractivePromptAction(t *testing.T) {
	t.Parallel()

	runner := &testRunner{}
	dispatcher := agent.NewDispatcher(agent.DispatcherOptions{
		Store:       agent.NewConversationStore(cache.NewMemoryStorage()),
		Runner:      runner,
		WorkerCount: 1,
	})
	seaTalkAdapter := newSeaTalkAgentAdapterWithClient(dispatcher, nil)

	if err := dispatcher.Start(); err != nil {
		t.Fatalf("start dispatcher failed: %v", err)
	}
	defer func() {
		if err := dispatcher.Shutdown(context.Background()); err != nil {
			t.Fatalf("shutdown dispatcher failed: %v", err)
		}
	}()

	req := seatalk.EventRequest{EventID: "evt-interactive-prompt-1", Timestamp: 1_700_000_000_000}
	event := &seatalk.InteractiveMessageClickEvent{
		MessageID:    "msg-card-1",
		EmployeeCode: "e_1",
		Email:        "alice@example.com",
		ThreadID:     "thread-1",
		Value:        `{"action":"prompt","prompt":"Continue with the approval workflow."}`,
	}

	if _, err := seaTalkAdapter.ProcessEvent(context.Background(), req, event); err != nil {
		t.Fatalf("process event failed: %v", err)
	}

	if err := waitForRunnerCalls(runner); err != nil {
		t.Fatal(err)
	}

	reqCall := runner.LastRequest()
	if reqCall.Message.ConversationKey != "seatalk:private:e_1:thread-1" {
		t.Fatalf("unexpected conversation key: %q", reqCall.Message.ConversationKey)
	}
	if reqCall.Message.Kind != agent.MessageKindText {
		t.Fatalf("unexpected message kind: %s", reqCall.Message.Kind)
	}
	if reqCall.Message.Sender != "alice@example.com" {
		t.Fatalf("unexpected sender: %q", reqCall.Message.Sender)
	}
	if reqCall.Message.SentAtUnix != 1_700_000_000 {
		t.Fatalf("unexpected sent time: %d", reqCall.Message.SentAtUnix)
	}
	if reqCall.Message.Text != "Continue with the approval workflow." {
		t.Fatalf("unexpected prompt text: %q", reqCall.Message.Text)
	}
	if strings.Contains(reqCall.Message.Text, "User clicked a SeaTalk interactive message button.") {
		t.Fatalf("prompt action should not be wrapped as tool click text: %q", reqCall.Message.Text)
	}
}

func TestSeaTalkAgentAdapterIgnoresDuplicateInteractiveClicksWhileActionIsRunning(t *testing.T) {
	t.Parallel()

	runner := &blockingTestRunner{
		started: make(chan struct{}, 4),
		release: make(chan struct{}),
	}
	dispatcher := agent.NewDispatcher(agent.DispatcherOptions{
		Store:       agent.NewConversationStore(cache.NewMemoryStorage()),
		Runner:      runner,
		WorkerCount: 1,
	})
	seaTalkAdapter := newSeaTalkAgentAdapterWithClient(dispatcher, nil)

	if err := dispatcher.Start(); err != nil {
		t.Fatalf("start dispatcher failed: %v", err)
	}
	defer func() {
		close(runner.release)
		if err := dispatcher.Shutdown(context.Background()); err != nil {
			t.Fatalf("shutdown dispatcher failed: %v", err)
		}
	}()

	event := &seatalk.InteractiveMessageClickEvent{
		MessageID:    "msg-card-1",
		EmployeeCode: "e_1",
		Email:        "alice@example.com",
		ThreadID:     "thread-1",
		Value:        `{"action":"prompt","prompt":"Continue with the approval workflow."}`,
	}

	if _, err := seaTalkAdapter.ProcessEvent(context.Background(), seatalk.EventRequest{EventID: "evt-interactive-dup-1", Timestamp: 1_700_000_000_000}, event); err != nil {
		t.Fatalf("process first event failed: %v", err)
	}
	if err := waitForBlockingRunnerStarts(runner, 1); err != nil {
		t.Fatal(err)
	}

	if _, err := seaTalkAdapter.ProcessEvent(context.Background(), seatalk.EventRequest{EventID: "evt-interactive-dup-2", Timestamp: 1_700_000_000_001}, event); err != nil {
		t.Fatalf("process duplicate event failed: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if runner.Calls() != 1 {
		t.Fatalf("expected duplicate click to be ignored while running, got %d calls", runner.Calls())
	}

	runner.release <- struct{}{}
	if err := waitForInteractiveActionUnlock(seaTalkAdapter, event); err != nil {
		t.Fatal(err)
	}

	if _, err := seaTalkAdapter.ProcessEvent(context.Background(), seatalk.EventRequest{EventID: "evt-interactive-dup-3", Timestamp: 1_700_000_000_002}, event); err != nil {
		t.Fatalf("process event after release failed: %v", err)
	}
	if err := waitForBlockingRunnerStarts(runner, 2); err != nil {
		t.Fatal(err)
	}
	runner.release <- struct{}{}
}

func TestSeaTalkAgentAdapterLoadsInitialContextForFirstPrivateInteractiveClickEvent(t *testing.T) {
	t.Parallel()

	runner := &testRunner{}
	dispatcher := agent.NewDispatcher(agent.DispatcherOptions{
		Store:       agent.NewConversationStore(cache.NewMemoryStorage()),
		Runner:      runner,
		WorkerCount: 1,
	})
	client := seatalk.NewClient(
		seatalk.Config{AppID: "app-id", AppSecret: "app-secret", EmployeeInfoEnabled: true},
		seatalk.WithHTTPClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.Method != http.MethodGet {
				t.Fatalf("unexpected method: %s", req.Method)
			}
			switch req.URL.Path {
			case "/contacts/v2/profile":
				if req.URL.Query().Get("employee_code") != "e_1" {
					t.Fatalf("unexpected employee code: %s", req.URL.Query().Get("employee_code"))
				}
				return jsonResponse(t, map[string]any{
					"code": 0,
					"employees": []map[string]any{
						{
							"employee_code":                   "e_1",
							"email":                           "alice@example.com",
							"mobile":                          "+6512345678",
							"departments":                     []string{"eng", "assistant"},
							"reporting_manager_employee_code": "e_mgr_1",
						},
					},
				}), nil
			case "/messaging/v2/single_chat/get_thread_by_thread_id":
				if req.URL.Query().Get("employee_code") != "e_1" {
					t.Fatalf("unexpected employee code: %s", req.URL.Query().Get("employee_code"))
				}
				if req.URL.Query().Get("thread_id") != "thread-1" {
					t.Fatalf("unexpected thread id: %s", req.URL.Query().Get("thread_id"))
				}
				return jsonResponse(t, map[string]any{
					"code": 0,
					"thread_messages": []map[string]any{
						{
							"message_id":        "msg-1",
							"thread_id":         "thread-1",
							"message_sent_time": 1000,
							"tag":               "text",
							"sender":            map[string]any{"email": "alice@example.com"},
							"text":              map[string]any{"plain_text": "earlier private message"},
							"quoted_message_id": "",
						},
						{
							"message_id":        "msg-card-1",
							"thread_id":         "thread-1",
							"message_sent_time": 2000,
							"tag":               "interactive_message",
							"sender":            map[string]any{"email": "assistant@example.com"},
							"interactive_message": map[string]any{
								"elements": []map[string]any{
									{"element_type": "title", "title": map[string]any{"text": "Approval Needed"}},
								},
							},
							"quoted_message_id": "",
						},
					},
				}), nil
			default:
				t.Fatalf("unexpected path: %s", req.URL.Path)
				return nil, nil
			}
		})}),
		seatalk.WithTokenProvider(func(context.Context, *http.Client, string, string) (string, error) {
			return "token-123", nil
		}),
	)
	seaTalkAdapter := newSeaTalkAgentAdapterWithClient(dispatcher, client)

	if err := dispatcher.Start(); err != nil {
		t.Fatalf("start dispatcher failed: %v", err)
	}
	defer func() {
		if err := dispatcher.Shutdown(context.Background()); err != nil {
			t.Fatalf("shutdown dispatcher failed: %v", err)
		}
	}()

	req := seatalk.EventRequest{EventID: "evt-interactive-private-ctx-1", Timestamp: 1_700_000_000_000}
	event := &seatalk.InteractiveMessageClickEvent{
		MessageID:    "msg-card-1",
		EmployeeCode: "e_1",
		Email:        "alice@example.com",
		ThreadID:     "thread-1",
		Value:        `{"action":"prompt","prompt":"Continue with the approval workflow."}`,
	}

	if _, err := seaTalkAdapter.ProcessEvent(context.Background(), req, event); err != nil {
		t.Fatalf("process event failed: %v", err)
	}

	if err := waitForRunnerCalls(runner); err != nil {
		t.Fatal(err)
	}

	reqCall := runner.LastRequest()
	expected := "Employee profile:\n- employee_code: e_1\n- email: alice@example.com\n- phone: +6512345678\n- departments: eng, assistant\n- manager_employee_code: e_mgr_1\nPrivate thread guidance:\n- This conversation is a private chat thread."
	if reqCall.Message.InitialContext() != expected {
		t.Fatalf("unexpected initial context: %q", reqCall.Message.InitialContext())
	}
	if len(reqCall.Message.HistoricalMessages()) != 1 {
		t.Fatalf("unexpected historical message count: %d", len(reqCall.Message.HistoricalMessages()))
	}
	if reqCall.Message.HistoricalMessages()[0].Text != "earlier private message" {
		t.Fatalf("unexpected history text: %q", reqCall.Message.HistoricalMessages()[0].Text)
	}
}

func TestSeaTalkAgentAdapterLoadsInitialContextForFirstGroupInteractiveClickEvent(t *testing.T) {
	t.Parallel()

	runner := &testRunner{}
	dispatcher := agent.NewDispatcher(agent.DispatcherOptions{
		Store:       agent.NewConversationStore(cache.NewMemoryStorage()),
		Runner:      runner,
		WorkerCount: 1,
	})
	client := seatalk.NewClient(
		seatalk.Config{AppID: "app-id", AppSecret: "app-secret"},
		seatalk.WithHTTPClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.Method != http.MethodGet {
				t.Fatalf("unexpected method: %s", req.Method)
			}
			switch req.URL.Path {
			case "/messaging/v2/group_chat/info":
				if req.URL.Query().Get("group_id") != "group-1" {
					t.Fatalf("unexpected group id: %s", req.URL.Query().Get("group_id"))
				}
				return jsonResponse(t, map[string]any{
					"code": 0,
					"group": map[string]any{
						"group_name":       "Demo Group",
						"group_user_total": 20,
					},
				}), nil
			case "/messaging/v2/group_chat/get_thread_by_thread_id":
				if req.URL.Query().Get("group_id") != "group-1" {
					t.Fatalf("unexpected group id: %s", req.URL.Query().Get("group_id"))
				}
				if req.URL.Query().Get("thread_id") != "thread-1" {
					t.Fatalf("unexpected thread id: %s", req.URL.Query().Get("thread_id"))
				}
				return jsonResponse(t, map[string]any{
					"code": 0,
					"thread_messages": []map[string]any{
						{
							"message_id":        "msg-1",
							"thread_id":         "thread-1",
							"message_sent_time": 1000,
							"tag":               "text",
							"sender":            map[string]any{"email": "alice@example.com"},
							"text":              map[string]any{"plain_text": "earlier group message"},
							"quoted_message_id": "",
						},
						{
							"message_id":        "msg-card-1",
							"thread_id":         "thread-1",
							"message_sent_time": 2000,
							"tag":               "interactive_message",
							"sender":            map[string]any{"email": "assistant@example.com"},
							"interactive_message": map[string]any{
								"elements": []map[string]any{
									{"element_type": "title", "title": map[string]any{"text": "Approval Needed"}},
								},
							},
							"quoted_message_id": "",
						},
					},
				}), nil
			default:
				t.Fatalf("unexpected path: %s", req.URL.Path)
				return nil, nil
			}
		})}),
		seatalk.WithTokenProvider(func(context.Context, *http.Client, string, string) (string, error) {
			return "token-123", nil
		}),
	)
	seaTalkAdapter := newSeaTalkAgentAdapterWithClient(dispatcher, client)

	if err := dispatcher.Start(); err != nil {
		t.Fatalf("start dispatcher failed: %v", err)
	}
	defer func() {
		if err := dispatcher.Shutdown(context.Background()); err != nil {
			t.Fatalf("shutdown dispatcher failed: %v", err)
		}
	}()

	req := seatalk.EventRequest{EventID: "evt-interactive-group-ctx-1", Timestamp: 1_700_000_000_000}
	event := &seatalk.InteractiveMessageClickEvent{
		MessageID:    "msg-card-1",
		EmployeeCode: "e_group_1",
		Email:        "alice@example.com",
		GroupID:      "group-1",
		ThreadID:     "thread-1",
		Value:        `{"action":"prompt","prompt":"Proceed with the task."}`,
	}

	if _, err := seaTalkAdapter.ProcessEvent(context.Background(), req, event); err != nil {
		t.Fatalf("process event failed: %v", err)
	}

	if err := waitForRunnerCalls(runner); err != nil {
		t.Fatal(err)
	}

	reqCall := runner.LastRequest()
	expected := "Group profile:\n- name: Demo Group\n" +
		"Group thread guidance:\n- The current message may include message tags. The tag `group_mentioned_message` means the bot was explicitly mentioned in that message.\n- When the bot is explicitly mentioned, first decide whether the mention is a real task request, direct addressing, or only a reference to the bot.\n  - For references or introductions, usually do not reply. If the sender is explicitly introducing the bot in the current message and a social acknowledgment is expected, a brief and natural reply is allowed.\n  - For a real task request, a reply is required. If the reply addresses one or more senders, include mentions for the relevant sender or senders by following the sender mention hint in the message context.\n- For messages without the tag `group_mentioned_message`, be conservative and default to not replying. Reply only when a user-facing response is clearly necessary.\n  - If the context is clear enough, you do not need to mention the sender.\n- The sender mention hint in the message context only shows the mention format; it does not mean a mention is required.\n- When you need to mention someone not a sender, use one of these tags:\n  - `<mention-tag target=\"seatalk://user?id=SEATALK_ID\"/>`, SEATALK_ID is identified from:\n    - Message mention format: `@USERNAME [mentioned_user_seatalk_id=SEATALK_ID]`\n  - `<mention-tag target=\"seatalk://user?email=USER_EMAIL\"/>`, USER_EMAIL is limited to corporate addresses under @sea.com/@shopee.com/@monee.com, and identified from:\n    - Message mention format: `@USERNAME [mentioned_user_email=USER_EMAIL]`\n    - Group member format: `<USER_EMAIL>`"
	if reqCall.Message.InitialContext() != expected {
		t.Fatalf("unexpected initial context: %q", reqCall.Message.InitialContext())
	}
	if len(reqCall.Message.HistoricalMessages()) != 1 {
		t.Fatalf("unexpected historical message count: %d", len(reqCall.Message.HistoricalMessages()))
	}
	if reqCall.Message.HistoricalMessages()[0].Text != "earlier group message" {
		t.Fatalf("unexpected history text: %q", reqCall.Message.HistoricalMessages()[0].Text)
	}
}

func TestNormalizeQuotedMessageSupportsInteractiveMessage(t *testing.T) {
	t.Parallel()

	quoted, err := normalizeQuotedMessage(context.Background(), nil, seatalk.GetMessageResult{
		Tag: "interactive_message",
		InteractiveMessage: &seatalk.ThreadInteractiveMessage{
			Elements: []json.RawMessage{
				json.RawMessage(`{"element_type":"title","title":{"text":"Approval Needed"}}`),
				json.RawMessage(`{"element_type":"button_group","button_group":[{"button_type":"callback","text":"Approve"},{"button_type":"callback","text":"Reject"}]}`),
			},
		},
	})
	if err != nil {
		t.Fatalf("normalize quoted message failed: %v", err)
	}
	if quoted == nil {
		t.Fatal("expected interactive quoted message")
	}
	if quoted.Kind != agent.MessageKindInteractiveCard {
		t.Fatalf("unexpected kind: %s", quoted.Kind)
	}
	expected := `interactive card; title="Approval Needed"; buttons=[Approve, Reject]`
	if quoted.Text != expected {
		t.Fatalf("unexpected quoted text: %q", quoted.Text)
	}
}

func TestNormalizeQuotedMessageSupportsInteractiveMessageWithExpandedMentions(t *testing.T) {
	t.Parallel()

	quoted, err := normalizeQuotedMessage(context.Background(), nil, seatalk.GetMessageResult{
		Tag: "interactive_message",
		InteractiveMessage: &seatalk.ThreadInteractiveMessage{
			Elements: []json.RawMessage{
				json.RawMessage(`{"element_type":"title","title":{"text":"Ask @Carol"}}`),
			},
			MentionedList: []seatalk.MentionedEntity{
				{Username: "Carol", SeatalkID: "seatalk-user-3"},
			},
		},
	})
	if err != nil {
		t.Fatalf("normalize quoted message failed: %v", err)
	}
	if quoted == nil {
		t.Fatal("expected interactive quoted message")
	}
	expected := `interactive card; title="Ask @Carol [mentioned_user_seatalk_id=seatalk-user-3]"`
	if quoted.Text != expected {
		t.Fatalf("unexpected quoted text: %q", quoted.Text)
	}
}

func TestNormalizeQuotedMessageIncludesSenderTimeAndExpandedMentions(t *testing.T) {
	t.Parallel()

	quoted, err := normalizeQuotedMessage(context.Background(), nil, seatalk.GetMessageResult{
		MessageSentTime: 1234,
		Sender: seatalk.MessageSender{
			Email: "alice@example.com",
		},
		Tag: "text",
		Text: &seatalk.ThreadTextMessage{
			PlainText: "@bob please review",
			MentionedList: []seatalk.MentionedEntity{
				{
					Username:  "bob",
					SeatalkID: "u_bob",
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("normalize quoted message failed: %v", err)
	}
	if quoted == nil {
		t.Fatal("expected quoted message")
	}
	if quoted.Kind != agent.MessageKindText {
		t.Fatalf("unexpected kind: %s", quoted.Kind)
	}
	if quoted.Sender != "alice@example.com" {
		t.Fatalf("unexpected sender: %q", quoted.Sender)
	}
	if quoted.SentAtUnix != 1234 {
		t.Fatalf("unexpected sent time: %d", quoted.SentAtUnix)
	}
	expected := "@bob [mentioned_user_seatalk_id=u_bob] please review"
	if quoted.Text != expected {
		t.Fatalf("unexpected quoted text: %q", quoted.Text)
	}
}

func TestNormalizeQuotedMessageSupportsCombinedForwardedChatHistory(t *testing.T) {
	t.Parallel()

	quoted, err := normalizeQuotedMessage(context.Background(), nil, seatalk.GetMessageResult{
		Tag: "combined_forwarded_chat_history",
		CombinedForwardedChatHistory: &seatalk.CombinedForwardedChatHistoryMessage{
			Content: []map[string]any{
				{
					"tag":               "text",
					"message_sent_time": 1000,
					"sender": map[string]any{
						"email": "alice@example.com",
					},
					"text": map[string]any{
						"content": "forwarded hello",
					},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("normalize quoted message failed: %v", err)
	}
	if quoted == nil {
		t.Fatal("expected combined forwarded quoted message")
	}
	if quoted.Kind != agent.MessageKindForwarded {
		t.Fatalf("unexpected kind: %s", quoted.Kind)
	}
	if len(quoted.ForwardedMessages) != 1 {
		t.Fatalf("unexpected forwarded message count: %d", len(quoted.ForwardedMessages))
	}
	if quoted.ForwardedMessages[0].Sender != "alice@example.com" {
		t.Fatalf("unexpected forwarded sender: %q", quoted.ForwardedMessages[0].Sender)
	}
	if quoted.ForwardedMessages[0].Text != "forwarded hello" {
		t.Fatalf("unexpected forwarded text: %q", quoted.ForwardedMessages[0].Text)
	}
}

func TestSeaTalkAgentAdapterLoadsInitialContextForFirstMentionedThreadMessage(t *testing.T) {
	t.Parallel()

	runner := &testRunner{}
	dispatcher := agent.NewDispatcher(agent.DispatcherOptions{
		Store:       agent.NewConversationStore(cache.NewMemoryStorage()),
		Runner:      runner,
		WorkerCount: 1,
	})
	client := seatalk.NewClient(
		seatalk.Config{AppID: "app-id", AppSecret: "app-secret", EmployeeInfoEnabled: true},
		seatalk.WithHTTPClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.Method != http.MethodGet {
				t.Fatalf("unexpected method: %s", req.Method)
			}
			switch req.URL.Path {
			case "/messaging/v2/group_chat/info":
				if req.URL.Query().Get("group_id") != "group-1" {
					t.Fatalf("unexpected group id: %s", req.URL.Query().Get("group_id"))
				}
				return jsonResponse(t, map[string]any{
					"code": 0,
					"group": map[string]any{
						"group_name":                 "Demo Group",
						"group_settings":             map[string]any{"chat_history_for_new_members": "7 days", "can_notify_with_at_all": true, "can_view_member_list": true},
						"group_user_total":           12,
						"group_bot_total":            1,
						"group_system_account_total": 0,
						"group_user_list":            []map[string]any{},
						"group_bot_list":             []string{},
						"group_system_account_list":  []string{},
					},
				}), nil
			case "/messaging/v2/group_chat/get_thread_by_thread_id":
				if req.URL.Query().Get("group_id") != "group-1" {
					t.Fatalf("unexpected group id: %s", req.URL.Query().Get("group_id"))
				}
				if req.URL.Query().Get("thread_id") != "thread-1" {
					t.Fatalf("unexpected thread id: %s", req.URL.Query().Get("thread_id"))
				}
				return jsonResponse(t, map[string]any{
					"code": 0,
					"thread_messages": []map[string]any{
						{
							"message_id":        "msg-1",
							"thread_id":         "thread-1",
							"message_sent_time": 1000,
							"tag":               "text",
							"sender":            map[string]any{"email": "alice@example.com"},
							"text":              map[string]any{"plain_text": "earlier message"},
							"quoted_message_id": "",
						},
						{
							"message_id":        "msg-2",
							"thread_id":         "thread-1",
							"message_sent_time": 2000,
							"tag":               "text",
							"sender":            map[string]any{"email": "bob@example.com"},
							"text":              map[string]any{"plain_text": "@bot current message"},
							"quoted_message_id": "",
						},
					},
				}), nil
			default:
				t.Fatalf("unexpected path: %s", req.URL.Path)
				return nil, nil
			}
		})}),
		seatalk.WithTokenProvider(func(context.Context, *http.Client, string, string) (string, error) {
			return "token-123", nil
		}),
	)
	seaTalkAdapter := newSeaTalkAgentAdapterWithClient(dispatcher, client)

	if err := dispatcher.Start(); err != nil {
		t.Fatalf("start dispatcher failed: %v", err)
	}
	defer func() {
		if err := dispatcher.Shutdown(context.Background()); err != nil {
			t.Fatalf("shutdown dispatcher failed: %v", err)
		}
	}()

	req := seatalk.EventRequest{EventID: "evt-mention-1", Timestamp: 1_700_000_000_000}
	event := &seatalk.NewMentionedMessageReceivedFromGroupChatEvent{
		GroupID: "group-1",
	}
	event.Message.MessageID = "msg-2"
	event.Message.ThreadID = "thread-1"
	event.Message.Tag = "text"
	event.Message.Text.PlainText = "@bot current message"
	event.Message.Sender.Email = "bob@example.com"

	if _, err := seaTalkAdapter.ProcessEvent(context.Background(), req, event); err != nil {
		t.Fatalf("process event failed: %v", err)
	}

	if err := waitForRunnerCalls(runner); err != nil {
		t.Fatal(err)
	}

	reqCall := runner.LastRequest()
	expectedInitialContext := "Group profile:\n- name: Demo Group\n" +
		"Group thread guidance:\n- The current message may include message tags. The tag `group_mentioned_message` means the bot was explicitly mentioned in that message.\n- When the bot is explicitly mentioned, first decide whether the mention is a real task request, direct addressing, or only a reference to the bot.\n  - For references or introductions, usually do not reply. If the sender is explicitly introducing the bot in the current message and a social acknowledgment is expected, a brief and natural reply is allowed.\n  - For a real task request, a reply is required. If the reply addresses one or more senders, include mentions for the relevant sender or senders by following the sender mention hint in the message context.\n- For messages without the tag `group_mentioned_message`, be conservative and default to not replying. Reply only when a user-facing response is clearly necessary.\n  - If the context is clear enough, you do not need to mention the sender.\n- The sender mention hint in the message context only shows the mention format; it does not mean a mention is required.\n- When you need to mention someone not a sender, use one of these tags:\n  - `<mention-tag target=\"seatalk://user?id=SEATALK_ID\"/>`, SEATALK_ID is identified from:\n    - Message mention format: `@USERNAME [mentioned_user_seatalk_id=SEATALK_ID]`\n  - `<mention-tag target=\"seatalk://user?email=USER_EMAIL\"/>`, USER_EMAIL is limited to corporate addresses under @sea.com/@shopee.com/@monee.com, and identified from:\n    - Message mention format: `@USERNAME [mentioned_user_email=USER_EMAIL]`\n    - Group member format: `<USER_EMAIL>`"
	if reqCall.Message.InitialContext() != expectedInitialContext {
		t.Fatalf("unexpected initial context: %q", reqCall.Message.InitialContext())
	}
	if len(reqCall.Message.HistoricalMessages()) != 1 {
		t.Fatalf("unexpected historical message count: %d", len(reqCall.Message.HistoricalMessages()))
	}
	if len(reqCall.Message.MergedMessages()) != 0 {
		t.Fatalf("unexpected merged message count: %d", len(reqCall.Message.MergedMessages()))
	}
	if reqCall.Message.HistoricalMessages()[0].Text != "earlier message" {
		t.Fatalf("unexpected history text: %q", reqCall.Message.HistoricalMessages()[0].Text)
	}
}

func TestSeaTalkAgentAdapterLoadsQuotedThreadHistoryMessage(t *testing.T) {
	t.Parallel()

	runner := &testRunner{}
	dispatcher := agent.NewDispatcher(agent.DispatcherOptions{
		Store:       agent.NewConversationStore(cache.NewMemoryStorage()),
		Runner:      runner,
		WorkerCount: 1,
	})
	client := seatalk.NewClient(
		seatalk.Config{AppID: "app-id", AppSecret: "app-secret", EmployeeInfoEnabled: true},
		seatalk.WithHTTPClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.Method != http.MethodGet {
				t.Fatalf("unexpected method: %s", req.Method)
			}
			switch req.URL.Path {
			case "/messaging/v2/group_chat/info":
				return jsonResponse(t, map[string]any{
					"code": 0,
					"group": map[string]any{
						"group_name":                 "Demo Group",
						"group_settings":             map[string]any{"chat_history_for_new_members": "7 days", "can_notify_with_at_all": true, "can_view_member_list": true},
						"group_user_total":           12,
						"group_bot_total":            1,
						"group_system_account_total": 0,
						"group_user_list":            []map[string]any{},
						"group_bot_list":             []string{},
						"group_system_account_list":  []string{},
					},
				}), nil
			case "/messaging/v2/group_chat/get_thread_by_thread_id":
				return jsonResponse(t, map[string]any{
					"code": 0,
					"thread_messages": []map[string]any{
						{
							"message_id":        "msg-1",
							"thread_id":         "thread-1",
							"message_sent_time": 1000,
							"quoted_message_id": "quoted-1",
							"tag":               "text",
							"sender":            map[string]any{"email": "alice@example.com"},
							"text":              map[string]any{"plain_text": "reply with quote"},
						},
						{
							"message_id":        "msg-2",
							"thread_id":         "thread-1",
							"message_sent_time": 2000,
							"tag":               "text",
							"sender":            map[string]any{"email": "bob@example.com"},
							"text":              map[string]any{"plain_text": "@bot current message"},
						},
					},
				}), nil
			case "/messaging/v2/get_message_by_message_id":
				if req.URL.Query().Get("message_id") != "quoted-1" {
					t.Fatalf("unexpected quoted message id: %s", req.URL.Query().Get("message_id"))
				}
				return jsonResponse(t, map[string]any{
					"code":       0,
					"message_id": "quoted-1",
					"sender": map[string]any{
						"email": "carol@example.com",
					},
					"message_sent_time": 900,
					"tag":               "text",
					"text": map[string]any{
						"plain_text": "original quoted message",
					},
				}), nil
			default:
				t.Fatalf("unexpected path: %s", req.URL.Path)
				return nil, nil
			}
		})}),
		seatalk.WithTokenProvider(func(context.Context, *http.Client, string, string) (string, error) {
			return "token-123", nil
		}),
	)
	seaTalkAdapter := newSeaTalkAgentAdapterWithClient(dispatcher, client)

	if err := dispatcher.Start(); err != nil {
		t.Fatalf("start dispatcher failed: %v", err)
	}
	defer func() {
		if err := dispatcher.Shutdown(context.Background()); err != nil {
			t.Fatalf("shutdown dispatcher failed: %v", err)
		}
	}()

	req := seatalk.EventRequest{EventID: "evt-mention-quoted-history-1", Timestamp: 1_700_000_000_000}
	event := &seatalk.NewMentionedMessageReceivedFromGroupChatEvent{
		GroupID: "group-1",
	}
	event.Message.MessageID = "msg-2"
	event.Message.ThreadID = "thread-1"
	event.Message.Tag = "text"
	event.Message.Text.PlainText = "@bot current message"
	event.Message.Sender.Email = "bob@example.com"

	if _, err := seaTalkAdapter.ProcessEvent(context.Background(), req, event); err != nil {
		t.Fatalf("process event failed: %v", err)
	}

	if err := waitForRunnerCalls(runner); err != nil {
		t.Fatal(err)
	}

	reqCall := runner.LastRequest()
	if len(reqCall.Message.HistoricalMessages()) != 1 {
		t.Fatalf("unexpected historical message count: %d", len(reqCall.Message.HistoricalMessages()))
	}
	history := reqCall.Message.HistoricalMessages()[0]
	if history.QuotedMessage == nil {
		t.Fatal("expected quoted message in history")
	}
	if history.QuotedMessage.Sender != "carol@example.com" {
		t.Fatalf("unexpected quoted sender: %q", history.QuotedMessage.Sender)
	}
	if history.QuotedMessage.SentAtUnix != 900 {
		t.Fatalf("unexpected quoted sent time: %d", history.QuotedMessage.SentAtUnix)
	}
	if history.QuotedMessage.Text != "original quoted message" {
		t.Fatalf("unexpected quoted text: %q", history.QuotedMessage.Text)
	}
}

func TestSeaTalkAgentAdapterLoadsGroupContextWithoutThreadHistoryForTopLevelMentionedGroupMessage(t *testing.T) {
	t.Parallel()

	runner := &testRunner{}
	dispatcher := agent.NewDispatcher(agent.DispatcherOptions{
		Store:       agent.NewConversationStore(cache.NewMemoryStorage()),
		Runner:      runner,
		WorkerCount: 1,
	})
	client := seatalk.NewClient(
		seatalk.Config{AppID: "app-id", AppSecret: "app-secret", EmployeeInfoEnabled: true},
		seatalk.WithHTTPClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			switch req.URL.Path {
			case "/messaging/v2/group_chat/info":
				return jsonResponse(t, map[string]any{
					"group": map[string]any{
						"group_name":       "Demo Group",
						"group_user_total": 20,
					},
				}), nil
			default:
				t.Fatalf("unexpected outbound request for top-level mentioned group message: %s %s", req.Method, req.URL.Path)
				return nil, nil
			}
		})}),
		seatalk.WithTokenProvider(func(context.Context, *http.Client, string, string) (string, error) {
			return "token-123", nil
		}),
	)
	seaTalkAdapter := newSeaTalkAgentAdapterWithClient(dispatcher, client)

	if err := dispatcher.Start(); err != nil {
		t.Fatalf("start dispatcher failed: %v", err)
	}
	defer func() {
		if err := dispatcher.Shutdown(context.Background()); err != nil {
			t.Fatalf("shutdown dispatcher failed: %v", err)
		}
	}()

	req := seatalk.EventRequest{EventID: "evt-mention-root-1", Timestamp: 1_700_000_000_000}
	event := &seatalk.NewMentionedMessageReceivedFromGroupChatEvent{
		GroupID: "group-1",
	}
	event.Message.MessageID = "msg-root-1"
	event.Message.Tag = "text"
	event.Message.Text.PlainText = "@bot root message"
	event.Message.Sender.Email = "bob@example.com"

	if _, err := seaTalkAdapter.ProcessEvent(context.Background(), req, event); err != nil {
		t.Fatalf("process event failed: %v", err)
	}

	if err := waitForRunnerCalls(runner); err != nil {
		t.Fatal(err)
	}

	reqCall := runner.LastRequest()
	expectedInitialContext := "Group profile:\n- name: Demo Group\n" +
		"Group thread guidance:\n- The current message may include message tags. The tag `group_mentioned_message` means the bot was explicitly mentioned in that message.\n- When the bot is explicitly mentioned, first decide whether the mention is a real task request, direct addressing, or only a reference to the bot.\n  - For references or introductions, usually do not reply. If the sender is explicitly introducing the bot in the current message and a social acknowledgment is expected, a brief and natural reply is allowed.\n  - For a real task request, a reply is required. If the reply addresses one or more senders, include mentions for the relevant sender or senders by following the sender mention hint in the message context.\n- For messages without the tag `group_mentioned_message`, be conservative and default to not replying. Reply only when a user-facing response is clearly necessary.\n  - If the context is clear enough, you do not need to mention the sender.\n- The sender mention hint in the message context only shows the mention format; it does not mean a mention is required.\n- When you need to mention someone not a sender, use one of these tags:\n  - `<mention-tag target=\"seatalk://user?id=SEATALK_ID\"/>`, SEATALK_ID is identified from:\n    - Message mention format: `@USERNAME [mentioned_user_seatalk_id=SEATALK_ID]`\n  - `<mention-tag target=\"seatalk://user?email=USER_EMAIL\"/>`, USER_EMAIL is limited to corporate addresses under @sea.com/@shopee.com/@monee.com, and identified from:\n    - Message mention format: `@USERNAME [mentioned_user_email=USER_EMAIL]`\n    - Group member format: `<USER_EMAIL>`"
	if reqCall.Message.InitialContext() != expectedInitialContext {
		t.Fatalf("unexpected initial context: %q", reqCall.Message.InitialContext())
	}
	if len(reqCall.Message.HistoricalMessages()) != 0 {
		t.Fatalf("unexpected historical message count: %d", len(reqCall.Message.HistoricalMessages()))
	}
	if reqCall.Message.ConversationKey != "seatalk:group:group-1:msg-root-1" {
		t.Fatalf("unexpected conversation key: %s", reqCall.Message.ConversationKey)
	}
}

func TestSeaTalkAgentAdapterLoadsInitialContextForFirstPrivateThreadMessage(t *testing.T) {
	t.Parallel()

	runner := &testRunner{}
	dispatcher := agent.NewDispatcher(agent.DispatcherOptions{
		Store:       agent.NewConversationStore(cache.NewMemoryStorage()),
		Runner:      runner,
		WorkerCount: 1,
	})
	client := seatalk.NewClient(
		seatalk.Config{AppID: "app-id", AppSecret: "app-secret", EmployeeInfoEnabled: true},
		seatalk.WithHTTPClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.Method != http.MethodGet {
				t.Fatalf("unexpected method: %s", req.Method)
			}
			switch req.URL.Path {
			case "/contacts/v2/profile":
				if req.URL.Query().Get("employee_code") != "e_1" {
					t.Fatalf("unexpected employee code: %s", req.URL.Query().Get("employee_code"))
				}
				return jsonResponse(t, map[string]any{
					"code": 0,
					"employees": []map[string]any{
						{
							"employee_code":                   "e_1",
							"email":                           "alice@example.com",
							"mobile":                          "+6512345678",
							"departments":                     []string{"eng", "assistant"},
							"reporting_manager_employee_code": "e_mgr_1",
						},
					},
				}), nil
			case "/messaging/v2/single_chat/get_thread_by_thread_id":
				if req.URL.Query().Get("employee_code") != "e_1" {
					t.Fatalf("unexpected employee code: %s", req.URL.Query().Get("employee_code"))
				}
				if req.URL.Query().Get("thread_id") != "thread-1" {
					t.Fatalf("unexpected thread id: %s", req.URL.Query().Get("thread_id"))
				}
				return jsonResponse(t, map[string]any{
					"code": 0,
					"thread_messages": []map[string]any{
						{
							"message_id":        "msg-1",
							"thread_id":         "thread-1",
							"message_sent_time": 1000,
							"tag":               "text",
							"sender":            map[string]any{"email": "alice@example.com"},
							"text":              map[string]any{"plain_text": "earlier private message"},
							"quoted_message_id": "",
						},
						{
							"message_id":        "msg-2",
							"thread_id":         "thread-1",
							"message_sent_time": 2000,
							"tag":               "text",
							"sender":            map[string]any{"email": "alice@example.com"},
							"text":              map[string]any{"plain_text": "current private message"},
							"quoted_message_id": "",
						},
					},
				}), nil
			default:
				t.Fatalf("unexpected path: %s", req.URL.Path)
				return nil, nil
			}
		})}),
		seatalk.WithTokenProvider(func(context.Context, *http.Client, string, string) (string, error) {
			return "token-123", nil
		}),
	)
	seaTalkAdapter := newSeaTalkAgentAdapterWithClient(dispatcher, client)

	if err := dispatcher.Start(); err != nil {
		t.Fatalf("start dispatcher failed: %v", err)
	}
	defer func() {
		if err := dispatcher.Shutdown(context.Background()); err != nil {
			t.Fatalf("shutdown dispatcher failed: %v", err)
		}
	}()

	req := seatalk.EventRequest{EventID: "evt-private-thread-1", Timestamp: 1_700_000_000_000}
	event := &seatalk.MessageFromBotSubscriberEvent{
		EmployeeCode: "e_1",
		SeatalkID:    "u_1",
	}
	event.Message.MessageID = "msg-2"
	event.Message.ThreadID = "thread-1"
	event.Message.Tag = "text"
	event.Message.Text.Content = "current private message"

	if _, err := seaTalkAdapter.ProcessEvent(context.Background(), req, event); err != nil {
		t.Fatalf("process event failed: %v", err)
	}

	if err := waitForRunnerCalls(runner); err != nil {
		t.Fatal(err)
	}

	reqCall := runner.LastRequest()
	if reqCall.Message.InitialContext() != "Employee profile:\n- employee_code: e_1\n- email: alice@example.com\n- phone: +6512345678\n- departments: eng, assistant\n- manager_employee_code: e_mgr_1\nPrivate thread guidance:\n- This conversation is a private chat thread." {
		t.Fatalf("unexpected initial context: %q", reqCall.Message.InitialContext())
	}
	if len(reqCall.Message.HistoricalMessages()) != 1 {
		t.Fatalf("unexpected historical message count: %d", len(reqCall.Message.HistoricalMessages()))
	}
	if len(reqCall.Message.MergedMessages()) != 0 {
		t.Fatalf("unexpected merged message count: %d", len(reqCall.Message.MergedMessages()))
	}
	if reqCall.Message.HistoricalMessages()[0].Text != "earlier private message" {
		t.Fatalf("unexpected history text: %q", reqCall.Message.HistoricalMessages()[0].Text)
	}
}

func TestSeaTalkAgentAdapterLoadsPrivateContextWithoutHistoryForTopLevelPrivateMessage(t *testing.T) {
	t.Parallel()

	runner := &testRunner{}
	dispatcher := agent.NewDispatcher(agent.DispatcherOptions{
		Store:       agent.NewConversationStore(cache.NewMemoryStorage()),
		Runner:      runner,
		WorkerCount: 1,
	})
	client := seatalk.NewClient(
		seatalk.Config{AppID: "app-id", AppSecret: "app-secret", EmployeeInfoEnabled: true},
		seatalk.WithHTTPClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.Method != http.MethodGet {
				t.Fatalf("unexpected method: %s", req.Method)
			}
			switch req.URL.Path {
			case "/contacts/v2/profile":
				if req.URL.Query().Get("employee_code") != "e_1" {
					t.Fatalf("unexpected employee code: %s", req.URL.Query().Get("employee_code"))
				}
				return jsonResponse(t, map[string]any{
					"code": 0,
					"employees": []map[string]any{
						{
							"employee_code":                   "e_1",
							"email":                           "alice@example.com",
							"mobile":                          "+6512345678",
							"departments":                     []string{"eng", "assistant"},
							"reporting_manager_employee_code": "e_mgr_1",
						},
					},
				}), nil
			default:
				t.Fatalf("unexpected path: %s", req.URL.Path)
				return nil, nil
			}
		})}),
		seatalk.WithTokenProvider(func(context.Context, *http.Client, string, string) (string, error) {
			return "token-123", nil
		}),
	)
	seaTalkAdapter := newSeaTalkAgentAdapterWithClient(dispatcher, client)

	if err := dispatcher.Start(); err != nil {
		t.Fatalf("start dispatcher failed: %v", err)
	}
	defer func() {
		if err := dispatcher.Shutdown(context.Background()); err != nil {
			t.Fatalf("shutdown dispatcher failed: %v", err)
		}
	}()

	req := seatalk.EventRequest{EventID: "evt-private-root-1", Timestamp: 1_700_000_000_000}
	event := &seatalk.MessageFromBotSubscriberEvent{
		EmployeeCode: "e_1",
		SeatalkID:    "u_1",
	}
	event.Message.MessageID = "msg-root-1"
	event.Message.ThreadID = "0"
	event.Message.Tag = "text"
	event.Message.Text.Content = "current top-level private message"

	if _, err := seaTalkAdapter.ProcessEvent(context.Background(), req, event); err != nil {
		t.Fatalf("process event failed: %v", err)
	}

	if err := waitForRunnerCalls(runner); err != nil {
		t.Fatal(err)
	}

	reqCall := runner.LastRequest()
	if reqCall.Message.InitialContext() != "Employee profile:\n- employee_code: e_1\n- email: alice@example.com\n- phone: +6512345678\n- departments: eng, assistant\n- manager_employee_code: e_mgr_1\nPrivate thread guidance:\n- This conversation is a private chat thread." {
		t.Fatalf("unexpected initial context: %q", reqCall.Message.InitialContext())
	}
	if len(reqCall.Message.HistoricalMessages()) != 0 {
		t.Fatalf("unexpected historical message count: %d", len(reqCall.Message.HistoricalMessages()))
	}
	if reqCall.Message.ConversationKey != "seatalk:private:e_1:0" {
		t.Fatalf("unexpected conversation key: %s", reqCall.Message.ConversationKey)
	}
}

func TestSeaTalkAgentAdapterAddsReplyDecisionGuidanceForFirstGroupThreadEvent(t *testing.T) {
	t.Parallel()

	runner := &testRunner{}
	dispatcher := agent.NewDispatcher(agent.DispatcherOptions{
		Store:       agent.NewConversationStore(cache.NewMemoryStorage()),
		Runner:      runner,
		WorkerCount: 1,
	})
	client := seatalk.NewClient(
		seatalk.Config{AppID: "app-id", AppSecret: "app-secret", EmployeeInfoEnabled: true},
		seatalk.WithHTTPClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.Method != http.MethodGet {
				t.Fatalf("unexpected method: %s", req.Method)
			}
			switch req.URL.Path {
			case "/messaging/v2/group_chat/info":
				return jsonResponse(t, map[string]any{
					"code": 0,
					"group": map[string]any{
						"group_name":                 "Demo Group",
						"group_settings":             map[string]any{"chat_history_for_new_members": "7 days", "can_notify_with_at_all": true, "can_view_member_list": true},
						"group_user_total":           12,
						"group_bot_total":            1,
						"group_system_account_total": 0,
						"group_user_list":            []map[string]any{},
						"group_bot_list":             []string{},
						"group_system_account_list":  []string{},
					},
				}), nil
			case "/messaging/v2/group_chat/get_thread_by_thread_id":
				if req.URL.Query().Get("group_id") != "group-1" {
					t.Fatalf("unexpected group id: %s", req.URL.Query().Get("group_id"))
				}
				if req.URL.Query().Get("thread_id") != "thread-1" {
					t.Fatalf("unexpected thread id: %s", req.URL.Query().Get("thread_id"))
				}
				return jsonResponse(t, map[string]any{
					"code": 0,
					"thread_messages": []map[string]any{
						{
							"message_id":        "msg-1",
							"thread_id":         "thread-1",
							"message_sent_time": 1000,
							"tag":               "text",
							"sender":            map[string]any{"email": "alice@example.com"},
							"text":              map[string]any{"plain_text": "earlier message"},
							"quoted_message_id": "",
						},
						{
							"message_id":        "msg-2",
							"thread_id":         "thread-1",
							"message_sent_time": 2000,
							"tag":               "text",
							"sender":            map[string]any{"email": "alice@example.com"},
							"text":              map[string]any{"plain_text": "@bot maybe sync with bob"},
							"quoted_message_id": "",
						},
					},
				}), nil
			default:
				t.Fatalf("unexpected path: %s", req.URL.Path)
				return nil, nil
			}
		})}),
		seatalk.WithTokenProvider(func(context.Context, *http.Client, string, string) (string, error) {
			return "token-123", nil
		}),
	)
	seaTalkAdapter := newSeaTalkAgentAdapterWithClient(dispatcher, client)

	if err := dispatcher.Start(); err != nil {
		t.Fatalf("start dispatcher failed: %v", err)
	}
	defer func() {
		if err := dispatcher.Shutdown(context.Background()); err != nil {
			t.Fatalf("shutdown dispatcher failed: %v", err)
		}
	}()

	req := seatalk.EventRequest{EventID: "evt-thread-1", Timestamp: 1_700_000_000_000}
	event := &seatalk.NewMessageReceivedFromThreadEvent{
		GroupID: "group-1",
	}
	event.Message.MessageID = "msg-2"
	event.Message.ThreadID = "thread-1"
	event.Message.Tag = "text"
	event.Message.Text.PlainText = "@bot maybe sync with bob"
	event.Message.Sender.EmployeeCode = "e_group_1"
	event.Message.Sender.Email = "alice@example.com"

	if _, err := seaTalkAdapter.ProcessEvent(context.Background(), req, event); err != nil {
		t.Fatalf("process event failed: %v", err)
	}

	if err := waitForRunnerCalls(runner); err != nil {
		t.Fatal(err)
	}

	reqCall := runner.LastRequest()
	expected := "Group profile:\n- name: Demo Group\n" +
		"Group thread guidance:\n- The current message may include message tags. The tag `group_mentioned_message` means the bot was explicitly mentioned in that message.\n- When the bot is explicitly mentioned, first decide whether the mention is a real task request, direct addressing, or only a reference to the bot.\n  - For references or introductions, usually do not reply. If the sender is explicitly introducing the bot in the current message and a social acknowledgment is expected, a brief and natural reply is allowed.\n  - For a real task request, a reply is required. If the reply addresses one or more senders, include mentions for the relevant sender or senders by following the sender mention hint in the message context.\n- For messages without the tag `group_mentioned_message`, be conservative and default to not replying. Reply only when a user-facing response is clearly necessary.\n  - If the context is clear enough, you do not need to mention the sender.\n- The sender mention hint in the message context only shows the mention format; it does not mean a mention is required.\n- When you need to mention someone not a sender, use one of these tags:\n  - `<mention-tag target=\"seatalk://user?id=SEATALK_ID\"/>`, SEATALK_ID is identified from:\n    - Message mention format: `@USERNAME [mentioned_user_seatalk_id=SEATALK_ID]`\n  - `<mention-tag target=\"seatalk://user?email=USER_EMAIL\"/>`, USER_EMAIL is limited to corporate addresses under @sea.com/@shopee.com/@monee.com, and identified from:\n    - Message mention format: `@USERNAME [mentioned_user_email=USER_EMAIL]`\n    - Group member format: `<USER_EMAIL>`"
	if reqCall.Message.InitialContext() != expected {
		t.Fatalf("unexpected initial context: %q", reqCall.Message.InitialContext())
	}
	if len(reqCall.Message.MessageTags) != 0 {
		t.Fatalf("unexpected message tags: %+v", reqCall.Message.MessageTags)
	}
	if len(reqCall.Message.HistoricalMessages()) != 1 {
		t.Fatalf("unexpected historical message count: %d", len(reqCall.Message.HistoricalMessages()))
	}
	if reqCall.Message.HistoricalMessages()[0].Text != "earlier message" {
		t.Fatalf("unexpected history text: %q", reqCall.Message.HistoricalMessages()[0].Text)
	}
}

func TestSeaTalkAgentAdapterLoadsInitialContextForFirstPrivateThreadEvent(t *testing.T) {
	t.Parallel()

	runner := &testRunner{}
	dispatcher := agent.NewDispatcher(agent.DispatcherOptions{
		Store:       agent.NewConversationStore(cache.NewMemoryStorage()),
		Runner:      runner,
		WorkerCount: 1,
	})
	client := seatalk.NewClient(
		seatalk.Config{AppID: "app-id", AppSecret: "app-secret", EmployeeInfoEnabled: true},
		seatalk.WithHTTPClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.Method != http.MethodGet {
				t.Fatalf("unexpected method: %s", req.Method)
			}
			switch req.URL.Path {
			case "/contacts/v2/profile":
				if req.URL.Query().Get("employee_code") != "e_1" {
					t.Fatalf("unexpected employee code: %s", req.URL.Query().Get("employee_code"))
				}
				return jsonResponse(t, map[string]any{
					"code": 0,
					"employees": []map[string]any{
						{
							"employee_code":                   "e_1",
							"email":                           "alice@example.com",
							"mobile":                          "+6512345678",
							"departments":                     []string{"eng", "assistant"},
							"reporting_manager_employee_code": "e_mgr_1",
						},
					},
				}), nil
			case "/messaging/v2/single_chat/get_thread_by_thread_id":
				if req.URL.Query().Get("employee_code") != "e_1" {
					t.Fatalf("unexpected employee code: %s", req.URL.Query().Get("employee_code"))
				}
				if req.URL.Query().Get("thread_id") != "thread-1" {
					t.Fatalf("unexpected thread id: %s", req.URL.Query().Get("thread_id"))
				}
				return jsonResponse(t, map[string]any{
					"code": 0,
					"thread_messages": []map[string]any{
						{
							"message_id":        "msg-1",
							"thread_id":         "thread-1",
							"message_sent_time": 1000,
							"tag":               "text",
							"sender":            map[string]any{"email": "alice@example.com"},
							"text":              map[string]any{"plain_text": "earlier private message"},
							"quoted_message_id": "",
						},
						{
							"message_id":        "msg-2",
							"thread_id":         "thread-1",
							"message_sent_time": 2000,
							"tag":               "text",
							"sender":            map[string]any{"email": "alice@example.com"},
							"text":              map[string]any{"plain_text": "current private thread message"},
							"quoted_message_id": "",
						},
					},
				}), nil
			default:
				t.Fatalf("unexpected path: %s", req.URL.Path)
				return nil, nil
			}
		})}),
		seatalk.WithTokenProvider(func(context.Context, *http.Client, string, string) (string, error) {
			return "token-123", nil
		}),
	)
	seaTalkAdapter := newSeaTalkAgentAdapterWithClient(dispatcher, client)

	if err := dispatcher.Start(); err != nil {
		t.Fatalf("start dispatcher failed: %v", err)
	}
	defer func() {
		if err := dispatcher.Shutdown(context.Background()); err != nil {
			t.Fatalf("shutdown dispatcher failed: %v", err)
		}
	}()

	req := seatalk.EventRequest{EventID: "evt-private-thread-event-1", Timestamp: 1_700_000_000_000}
	event := &seatalk.MessageFromBotSubscriberEvent{
		EmployeeCode: "e_1",
		Email:        "alice@example.com",
	}
	event.Message.MessageID = "msg-2"
	event.Message.ThreadID = "thread-1"
	event.Message.Tag = "text"
	event.Message.Text.Content = "current private thread message"

	if _, err := seaTalkAdapter.ProcessEvent(context.Background(), req, event); err != nil {
		t.Fatalf("process event failed: %v", err)
	}

	if err := waitForRunnerCalls(runner); err != nil {
		t.Fatal(err)
	}

	reqCall := runner.LastRequest()
	expected := "Employee profile:\n- employee_code: e_1\n- email: alice@example.com\n- phone: +6512345678\n- departments: eng, assistant\n- manager_employee_code: e_mgr_1\nPrivate thread guidance:\n- This conversation is a private chat thread."
	if reqCall.Message.InitialContext() != expected {
		t.Fatalf("unexpected initial context: %q", reqCall.Message.InitialContext())
	}
	if len(reqCall.Message.HistoricalMessages()) != 1 {
		t.Fatalf("unexpected historical message count: %d", len(reqCall.Message.HistoricalMessages()))
	}
	if reqCall.Message.HistoricalMessages()[0].Text != "earlier private message" {
		t.Fatalf("unexpected history text: %q", reqCall.Message.HistoricalMessages()[0].Text)
	}
}

func TestSeaTalkResponderCleanupRemovesTempDir(t *testing.T) {
	t.Parallel()

	responder := &SeaTalkResponder{tempDir: t.TempDir()}
	if err := responder.Cleanup(context.Background()); err != nil {
		t.Fatalf("cleanup failed: %v", err)
	}
	if _, err := os.Stat(responder.tempDir); !os.IsNotExist(err) {
		t.Fatalf("expected temp dir to be removed, got err=%v", err)
	}
}

func TestSeaTalkAgentAdapterIncludesSmallGroupUserListInInitialContext(t *testing.T) {
	t.Parallel()

	client := seatalk.NewClient(
		seatalk.Config{AppID: "app-id", AppSecret: "app-secret", EmployeeInfoEnabled: true},
		seatalk.WithHTTPClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.Method != http.MethodGet {
				t.Fatalf("unexpected method: %s", req.Method)
			}
			if req.URL.Path != "/messaging/v2/group_chat/info" {
				t.Fatalf("unexpected path: %s", req.URL.Path)
			}
			return jsonResponse(t, map[string]any{
				"code": 0,
				"group": map[string]any{
					"group_name":                 "Small Group",
					"group_settings":             map[string]any{},
					"group_user_total":           2,
					"group_bot_total":            0,
					"group_system_account_total": 0,
					"group_user_list": []map[string]any{
						{"employee_code": "e_1", "email": "alice@example.com"},
						{"employee_code": "e_2", "email": "bob@example.com"},
					},
					"group_bot_list":            []string{},
					"group_system_account_list": []string{},
				},
			}), nil
		})}),
		seatalk.WithTokenProvider(func(context.Context, *http.Client, string, string) (string, error) {
			return "token-123", nil
		}),
	)

	adapter := newSeaTalkAgentAdapterWithClient(nil, client)
	contextBlock, err := adapter.loadGroupProfile(context.Background(), "group-1")
	if err != nil {
		t.Fatalf("load group profile failed: %v", err)
	}
	expected := "Group profile:\n- name: Small Group\n- users:\n  - e_1 <alice@example.com>\n  - e_2 <bob@example.com>"
	if contextBlock != expected {
		t.Fatalf("unexpected group profile:\n%s", contextBlock)
	}
}

func TestSeaTalkResponderSendTextLeavesPlainGroupReplyUntouched(t *testing.T) {
	t.Parallel()

	client := seatalk.NewClient(
		seatalk.Config{AppID: "app-id", AppSecret: "app-secret", EmployeeInfoEnabled: true},
		seatalk.WithHTTPClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.Method != http.MethodPost {
				t.Fatalf("unexpected method: %s", req.Method)
			}
			if req.URL.Path != "/messaging/v2/group_chat" {
				t.Fatalf("unexpected path: %s", req.URL.Path)
			}
			var body struct {
				GroupID string `json:"group_id"`
				Message struct {
					ThreadID string `json:"thread_id"`
					Text     struct {
						Format  int    `json:"format"`
						Content string `json:"content"`
					} `json:"text"`
				} `json:"message"`
			}
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				t.Fatalf("decode request failed: %v", err)
			}
			if body.Message.Text.Format != seatalk.TextFormatMarkdown {
				t.Fatalf("unexpected group message format: %d", body.Message.Text.Format)
			}
			if body.Message.Text.Content != "hello" {
				t.Fatalf("unexpected group message content: %q", body.Message.Text.Content)
			}
			if body.Message.ThreadID != "thread-1" {
				t.Fatalf("unexpected thread id: %q", body.Message.ThreadID)
			}
			return jsonResponse(t, map[string]any{
				"code":       0,
				"message_id": "reply-1",
			}), nil
		})}),
		seatalk.WithTokenProvider(func(context.Context, *http.Client, string, string) (string, error) {
			return "token-123", nil
		}),
	)

	responder := &SeaTalkResponder{
		client: client,
		target: seaTalkReplyTarget{
			isGroup:       true,
			groupID:       "group-1",
			threadID:      "thread-1",
			mentionTarget: seaTalkMentionTarget{seatalkID: "seatalk-user-1", email: "alice@example.com"},
		},
	}

	if err := responder.SendText(context.Background(), "hello"); err != nil {
		t.Fatalf("send text failed: %v", err)
	}
}

func TestSeaTalkResponderSendTextUsesExplicitMentionTagsWithoutAutoPrefix(t *testing.T) {
	t.Parallel()

	client := seatalk.NewClient(
		seatalk.Config{AppID: "app-id", AppSecret: "app-secret", EmployeeInfoEnabled: true},
		seatalk.WithHTTPClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.Method != http.MethodPost {
				t.Fatalf("unexpected method: %s", req.Method)
			}
			if req.URL.Path != "/messaging/v2/group_chat" {
				t.Fatalf("unexpected path: %s", req.URL.Path)
			}
			var body struct {
				Message struct {
					Text struct {
						Format  int    `json:"format"`
						Content string `json:"content"`
					} `json:"text"`
				} `json:"message"`
			}
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				t.Fatalf("decode request failed: %v", err)
			}
			if body.Message.Text.Format != seatalk.TextFormatMarkdown {
				t.Fatalf("unexpected group message format: %d", body.Message.Text.Format)
			}
			expected := `<mention-tag target="seatalk://user?email=alice@sea.com"/> <mention-tag target="seatalk://user?email=bob@sea.com"/> merged reply`
			if body.Message.Text.Content != expected {
				t.Fatalf("unexpected group message content: %q", body.Message.Text.Content)
			}
			return jsonResponse(t, map[string]any{
				"code":       0,
				"message_id": "reply-1",
			}), nil
		})}),
		seatalk.WithTokenProvider(func(context.Context, *http.Client, string, string) (string, error) {
			return "token-123", nil
		}),
	)

	responder := &SeaTalkResponder{
		client: client,
		target: seaTalkReplyTarget{
			isGroup:       true,
			groupID:       "group-1",
			threadID:      "thread-1",
			mentionTarget: seaTalkMentionTarget{seatalkID: "seatalk-user-1", email: "last@sea.com"},
		},
	}

	text := `<mention-tag target="seatalk://user?email=alice@sea.com"/> <mention-tag target="seatalk://user?email=bob@sea.com"/> merged reply`
	if err := responder.SendText(context.Background(), text); err != nil {
		t.Fatalf("send text failed: %v", err)
	}
}

func TestSeaTalkResponderSetTypingWithinAllowedWindow(t *testing.T) {
	t.Parallel()

	requests := 0
	client := seatalk.NewClient(
		seatalk.Config{AppID: "app-id", AppSecret: "app-secret"},
		seatalk.WithHTTPClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			requests++
			if req.Method != http.MethodPost {
				t.Fatalf("unexpected method: %s", req.Method)
			}
			if req.URL.Path != "/messaging/v2/single_chat_typing" {
				t.Fatalf("unexpected path: %s", req.URL.Path)
			}

			var body struct {
				EmployeeCode string `json:"employee_code"`
				ThreadID     string `json:"thread_id"`
			}
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				t.Fatalf("decode request failed: %v", err)
			}
			if body.EmployeeCode != "e_1" {
				t.Fatalf("unexpected employee code: %q", body.EmployeeCode)
			}
			if body.ThreadID != "thread-1" {
				t.Fatalf("unexpected thread id: %q", body.ThreadID)
			}

			return jsonResponse(t, map[string]any{"code": 0}), nil
		})}),
		seatalk.WithTokenProvider(func(context.Context, *http.Client, string, string) (string, error) {
			return "token-123", nil
		}),
	)

	responder := &SeaTalkResponder{
		client:             client,
		target:             seaTalkReplyTarget{employeeCode: "e_1", threadID: "thread-1"},
		typingEnabled:      true,
		typingAllowedUntil: time.Now().Add(5 * time.Second),
	}

	if err := responder.SetTyping(context.Background()); err != nil {
		t.Fatalf("set typing failed: %v", err)
	}
	if requests != 1 {
		t.Fatalf("unexpected typing request count: %d", requests)
	}
}

func TestSeaTalkResponderSetTypingSkipsExpiredWindow(t *testing.T) {
	t.Parallel()

	requests := 0
	client := seatalk.NewClient(
		seatalk.Config{AppID: "app-id", AppSecret: "app-secret"},
		seatalk.WithHTTPClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			requests++
			return jsonResponse(t, map[string]any{"code": 0}), nil
		})}),
		seatalk.WithTokenProvider(func(context.Context, *http.Client, string, string) (string, error) {
			t.Fatalf("token provider should not be called")
			return "", nil
		}),
	)

	responder := &SeaTalkResponder{
		client:             client,
		target:             seaTalkReplyTarget{employeeCode: "e_1", threadID: "thread-1"},
		typingEnabled:      true,
		typingAllowedUntil: time.Now().Add(-time.Second),
	}

	if err := responder.SetTyping(context.Background()); err != nil {
		t.Fatalf("set typing failed: %v", err)
	}
	if requests != 0 {
		t.Fatalf("unexpected typing request count: %d", requests)
	}
}

func TestSeaTalkResponderSetTypingSkipsTopLevelGroupMessage(t *testing.T) {
	t.Parallel()

	requests := 0
	client := seatalk.NewClient(
		seatalk.Config{AppID: "app-id", AppSecret: "app-secret"},
		seatalk.WithHTTPClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			requests++
			return jsonResponse(t, map[string]any{"code": 0}), nil
		})}),
		seatalk.WithTokenProvider(func(context.Context, *http.Client, string, string) (string, error) {
			t.Fatalf("token provider should not be called")
			return "", nil
		}),
	)

	responder := &SeaTalkResponder{
		client:             client,
		target:             seaTalkReplyTarget{isGroup: true, groupID: "group-1", threadID: "0"},
		typingEnabled:      true,
		typingAllowedUntil: time.Now().Add(5 * time.Second),
	}

	if err := responder.SetTyping(context.Background()); err != nil {
		t.Fatalf("set typing failed: %v", err)
	}
	if requests != 0 {
		t.Fatalf("unexpected typing request count: %d", requests)
	}
}

func TestSeaTalkResponderSetTypingSkipsTopLevelPrivateMessage(t *testing.T) {
	t.Parallel()

	requests := 0
	client := seatalk.NewClient(
		seatalk.Config{AppID: "app-id", AppSecret: "app-secret"},
		seatalk.WithHTTPClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			requests++
			return jsonResponse(t, map[string]any{"code": 0}), nil
		})}),
		seatalk.WithTokenProvider(func(context.Context, *http.Client, string, string) (string, error) {
			t.Fatalf("token provider should not be called")
			return "", nil
		}),
	)

	responder := &SeaTalkResponder{
		client:             client,
		target:             seaTalkReplyTarget{employeeCode: "e_1", threadID: "0"},
		typingEnabled:      true,
		typingAllowedUntil: time.Now().Add(5 * time.Second),
	}

	if err := responder.SetTyping(context.Background()); err != nil {
		t.Fatalf("set typing failed: %v", err)
	}
	if requests != 0 {
		t.Fatalf("unexpected typing request count: %d", requests)
	}
}

func TestSeaTalkResponderSetTypingSkipsRootMessageBackedPrivateThread(t *testing.T) {
	t.Parallel()

	requests := 0
	client := seatalk.NewClient(
		seatalk.Config{AppID: "app-id", AppSecret: "app-secret"},
		seatalk.WithHTTPClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			requests++
			return jsonResponse(t, map[string]any{"code": 0}), nil
		})}),
		seatalk.WithTokenProvider(func(context.Context, *http.Client, string, string) (string, error) {
			t.Fatalf("token provider should not be called")
			return "", nil
		}),
	)

	responder := &SeaTalkResponder{
		client: client,
		target: seaTalkReplyTarget{
			employeeCode: "e_1",
			messageID:    "msg-root-1",
			threadID:     "msg-root-1",
		},
		typingEnabled:      true,
		typingAllowedUntil: time.Now().Add(5 * time.Second),
	}

	if err := responder.SetTyping(context.Background()); err != nil {
		t.Fatalf("set typing failed: %v", err)
	}
	if requests != 0 {
		t.Fatalf("unexpected typing request count: %d", requests)
	}
}

func TestSeaTalkResponderSetTypingSkipsEmptyThreadID(t *testing.T) {
	t.Parallel()

	requests := 0
	client := seatalk.NewClient(
		seatalk.Config{AppID: "app-id", AppSecret: "app-secret"},
		seatalk.WithHTTPClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			requests++
			return jsonResponse(t, map[string]any{"code": 0}), nil
		})}),
		seatalk.WithTokenProvider(func(context.Context, *http.Client, string, string) (string, error) {
			t.Fatalf("token provider should not be called")
			return "", nil
		}),
	)

	responder := &SeaTalkResponder{
		client:             client,
		target:             seaTalkReplyTarget{employeeCode: "e_1", threadID: ""},
		typingEnabled:      true,
		typingAllowedUntil: time.Now().Add(5 * time.Second),
	}

	if err := responder.SetTyping(context.Background()); err != nil {
		t.Fatalf("set typing failed: %v", err)
	}
	if requests != 0 {
		t.Fatalf("unexpected typing request count: %d", requests)
	}
}

func TestSeaTalkResponderSetTypingStopsAfterMaxCount(t *testing.T) {
	t.Parallel()

	requests := 0
	client := seatalk.NewClient(
		seatalk.Config{AppID: "app-id", AppSecret: "app-secret"},
		seatalk.WithHTTPClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			requests++
			return jsonResponse(t, map[string]any{"code": 0}), nil
		})}),
		seatalk.WithTokenProvider(func(context.Context, *http.Client, string, string) (string, error) {
			return "token-123", nil
		}),
	)

	responder := &SeaTalkResponder{
		client:             client,
		target:             seaTalkReplyTarget{employeeCode: "e_1", threadID: "thread-1"},
		typingEnabled:      true,
		typingAllowedUntil: time.Now().Add(30 * time.Second),
	}

	for range seatalkTypingStatusMaxCount + 1 {
		if err := responder.SetTyping(context.Background()); err != nil {
			t.Fatalf("set typing failed: %v", err)
		}
	}

	if requests != seatalkTypingStatusMaxCount {
		t.Fatalf("unexpected typing request count: got %d want %d", requests, seatalkTypingStatusMaxCount)
	}
	if responder.typingStatusCount != seatalkTypingStatusMaxCount {
		t.Fatalf("unexpected typing status count: got %d want %d", responder.typingStatusCount, seatalkTypingStatusMaxCount)
	}
}

func TestProcessEventPrivateMessageTypingUsesSecondTimestamp(t *testing.T) {
	t.Parallel()

	requests := 0
	client := seatalk.NewClient(
		seatalk.Config{AppID: "app-id", AppSecret: "app-secret"},
		seatalk.WithHTTPClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			switch req.URL.Path {
			case "/messaging/v2/single_chat/get_thread_by_thread_id":
				if req.URL.Query().Get("employee_code") != "e_1" {
					t.Fatalf("unexpected employee code: %s", req.URL.Query().Get("employee_code"))
				}
				if req.URL.Query().Get("thread_id") != "thread-1" {
					t.Fatalf("unexpected thread id: %s", req.URL.Query().Get("thread_id"))
				}
				return jsonResponse(t, map[string]any{
					"code":            0,
					"thread_messages": []map[string]any{},
				}), nil
			case "/messaging/v2/single_chat_typing":
				requests++
				return jsonResponse(t, map[string]any{"code": 0}), nil
			default:
				t.Fatalf("unexpected path: %s", req.URL.Path)
				return nil, nil
			}
		})}),
		seatalk.WithTokenProvider(func(context.Context, *http.Client, string, string) (string, error) {
			return "token-123", nil
		}),
	)

	runner := &typingRunner{}
	dispatcher := agent.NewDispatcher(agent.DispatcherOptions{
		Store:              agent.NewConversationStore(cache.NewMemoryStorage()),
		Runner:             runner,
		WorkerCount:        1,
		NonTextMergeWindow: 10 * time.Millisecond,
	})
	seaTalkAdapter := newSeaTalkAgentAdapterWithClient(dispatcher, client)
	if err := dispatcher.Start(); err != nil {
		t.Fatalf("start dispatcher failed: %v", err)
	}
	defer func() {
		if err := dispatcher.Shutdown(context.Background()); err != nil {
			t.Fatalf("shutdown dispatcher failed: %v", err)
		}
	}()

	req := seatalk.EventRequest{EventID: "evt-private-seconds-1", Timestamp: time.Now().Unix()}
	event := &seatalk.MessageFromBotSubscriberEvent{
		EmployeeCode: "e_1",
		Message: struct {
			MessageID       string "json:\"message_id\""
			QuotedMessageID string "json:\"quoted_message_id\""
			ThreadID        string "json:\"thread_id\""
			Tag             string "json:\"tag\""
			Text            struct {
				Content        string "json:\"content\""
				LastEditedTime int64  "json:\"last_edited_time\""
			} "json:\"text\""
			Image struct {
				Content string "json:\"content\""
			} "json:\"image\""
			File struct {
				Content  string "json:\"content\""
				Filename string "json:\"filename\""
			} "json:\"file\""
			Video struct {
				Content string "json:\"content\""
			} "json:\"video\""
			CombinedForwardedChatHistory *seatalk.CombinedForwardedChatHistoryMessage "json:\"combined_forwarded_chat_history\""
		}{
			MessageID: "msg-1",
			ThreadID:  "thread-1",
			Tag:       "text",
			Text: struct {
				Content        string "json:\"content\""
				LastEditedTime int64  "json:\"last_edited_time\""
			}{
				Content: "hello",
			},
		},
	}

	if _, err := seaTalkAdapter.ProcessEvent(context.Background(), req, event); err != nil {
		t.Fatalf("process event failed: %v", err)
	}

	if err := waitForTypingRunnerCall(runner); err != nil {
		t.Fatal(err)
	}

	reqCall := runner.LastRequest()
	if reqCall.Message.SentAtUnix != req.Timestamp {
		t.Fatalf("unexpected sent_at_unix: got %d want %d", reqCall.Message.SentAtUnix, req.Timestamp)
	}
	if requests != 1 {
		t.Fatalf("unexpected request count: %d", requests)
	}
}

func TestSeaTalkResponderSetTypingSkipsWhenDisabled(t *testing.T) {
	t.Parallel()

	requests := 0
	client := seatalk.NewClient(
		seatalk.Config{AppID: "app-id", AppSecret: "app-secret"},
		seatalk.WithHTTPClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			requests++
			return jsonResponse(t, map[string]any{"code": 0}), nil
		})}),
		seatalk.WithTokenProvider(func(context.Context, *http.Client, string, string) (string, error) {
			t.Fatalf("token provider should not be called")
			return "", nil
		}),
	)

	responder := &SeaTalkResponder{
		client:             client,
		target:             seaTalkReplyTarget{isGroup: true, groupID: "group-1", threadID: "thread-1"},
		typingEnabled:      false,
		typingAllowedUntil: time.Now().Add(5 * time.Second),
	}

	if err := responder.SetTyping(context.Background()); err != nil {
		t.Fatalf("set typing failed: %v", err)
	}
	if requests != 0 {
		t.Fatalf("unexpected typing request count: %d", requests)
	}
}

func TestSeaTalkInteractiveSendToolUsesCurrentConversationTarget(t *testing.T) {
	t.Parallel()

	client := seatalk.NewClient(
		seatalk.Config{AppID: "app-id", AppSecret: "app-secret"},
		seatalk.WithHTTPClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.Method != http.MethodPost {
				t.Fatalf("unexpected method: %s", req.Method)
			}
			if req.URL.Path != "/messaging/v2/group_chat" {
				t.Fatalf("unexpected path: %s", req.URL.Path)
			}

			var body map[string]any
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				t.Fatalf("decode request failed: %v", err)
			}
			if body["group_id"] != "group-1" {
				t.Fatalf("unexpected group id: %#v", body["group_id"])
			}
			message, ok := body["message"].(map[string]any)
			if !ok {
				t.Fatalf("unexpected message payload: %#v", body["message"])
			}
			if message["thread_id"] != "thread-1" {
				t.Fatalf("unexpected thread id: %#v", message["thread_id"])
			}
			interactiveMessage, ok := message["interactive_message"].(map[string]any)
			if !ok {
				t.Fatalf("unexpected interactive message payload: %#v", message["interactive_message"])
			}
			elements, ok := interactiveMessage["elements"].([]any)
			if !ok || len(elements) != 2 {
				t.Fatalf("unexpected elements payload: %#v", interactiveMessage["elements"])
			}
			buttonElement, ok := elements[1].(map[string]any)
			if !ok {
				t.Fatalf("unexpected button element payload: %#v", elements[1])
			}
			button, ok := buttonElement["button"].(map[string]any)
			if !ok {
				t.Fatalf("unexpected button payload: %#v", buttonElement["button"])
			}
			value, ok := button["value"].(string)
			if !ok {
				t.Fatalf("unexpected button value payload: %#v", button["value"])
			}
			resolvedValue, err := resolveInteractiveCallbackValue(context.Background(), value)
			if err != nil {
				t.Fatalf("resolve callback value failed: %v", err)
			}
			if resolvedValue != `{"action":"tool_call","tool_name":"seatalk_update_interactive_message","tool_input_json":"{\"message_id\":\"interactive-msg-1\",\"elements\":[{\"element_type\":\"title\",\"title\":{\"text\":\"Approved\"}}]}"}` {
				t.Fatalf("unexpected resolved button value: %#v", resolvedValue)
			}

			return jsonResponse(t, map[string]any{
				"code":       0,
				"message_id": "interactive-msg-1",
			}), nil
		})}),
		seatalk.WithTokenProvider(func(context.Context, *http.Client, string, string) (string, error) {
			return "token-123", nil
		}),
	)

	tool := seaTalkSendInteractiveMessageTool{}
	ctx := agent.ContextWithTurnRequest(context.Background(), agent.TurnRequest{
		Conversation: agent.ConversationState{Key: "seatalk:group:group-1:thread-1"},
		Message: agent.InboundMessage{
			Responder: &SeaTalkResponder{
				client: client,
				target: seaTalkReplyTarget{
					isGroup:  true,
					groupID:  "group-1",
					threadID: "thread-1",
				},
			},
		},
	})

	result, err := tool.Call(ctx, json.RawMessage(`{
		"elements": [
			{"element_type":"title","title":{"text":"Choose action"}},
			{"element_type":"button","button":{"button_type":"callback","text":"Approve","value":"{\"action\":\"tool_call\",\"tool_name\":\"seatalk_update_interactive_message\",\"tool_input_json\":\"{\\\"message_id\\\":\\\"interactive-msg-1\\\",\\\"elements\\\":[{\\\"element_type\\\":\\\"title\\\",\\\"title\\\":{\\\"text\\\":\\\"Approved\\\"}}]}\"}"}}
		]
	}`))
	if err != nil {
		t.Fatalf("tool call failed: %v", err)
	}

	body, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("unexpected tool result type: %T", result)
	}
	if body["message_id"] != "interactive-msg-1" {
		t.Fatalf("unexpected message id: %#v", body["message_id"])
	}
}

func TestSeaTalkSendInteractiveMessageToolDefaultsDescriptionFormatToMarkdown(t *testing.T) {
	t.Parallel()

	client := seatalk.NewClient(
		seatalk.Config{AppID: "app-id", AppSecret: "app-secret"},
		seatalk.WithHTTPClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.Method != http.MethodPost {
				t.Fatalf("unexpected method: %s", req.Method)
			}
			if req.URL.Path != "/messaging/v2/group_chat" {
				t.Fatalf("unexpected path: %s", req.URL.Path)
			}

			var body map[string]any
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				t.Fatalf("decode request failed: %v", err)
			}
			message, ok := body["message"].(map[string]any)
			if !ok {
				t.Fatalf("unexpected message payload: %#v", body["message"])
			}
			interactiveMessage, ok := message["interactive_message"].(map[string]any)
			if !ok {
				t.Fatalf("unexpected interactive message payload: %#v", message["interactive_message"])
			}
			elements, ok := interactiveMessage["elements"].([]any)
			if !ok || len(elements) != 1 {
				t.Fatalf("unexpected elements payload: %#v", interactiveMessage["elements"])
			}
			descriptionElement, ok := elements[0].(map[string]any)
			if !ok {
				t.Fatalf("unexpected description element payload: %#v", elements[0])
			}
			description, ok := descriptionElement["description"].(map[string]any)
			if !ok {
				t.Fatalf("unexpected description payload: %#v", descriptionElement["description"])
			}
			if got := description["format"]; got != float64(seatalk.TextFormatMarkdown) {
				t.Fatalf("unexpected description format: %#v", got)
			}

			return jsonResponse(t, map[string]any{
				"code":       0,
				"message_id": "interactive-msg-markdown",
			}), nil
		})}),
		seatalk.WithTokenProvider(func(context.Context, *http.Client, string, string) (string, error) {
			return "token-123", nil
		}),
	)

	tool := seaTalkSendInteractiveMessageTool{}
	ctx := agent.ContextWithTurnRequest(context.Background(), agent.TurnRequest{
		Conversation: agent.ConversationState{Key: "seatalk:group:group-1:thread-1"},
		Message: agent.InboundMessage{
			Responder: &SeaTalkResponder{
				client: client,
				target: seaTalkReplyTarget{
					isGroup:  true,
					groupID:  "group-1",
					threadID: "thread-1",
				},
			},
		},
	})

	result, err := tool.Call(ctx, json.RawMessage(`{
		"elements": [
			{"element_type":"description","description":{"text":"**Build failed**. [Open run](https://example.com/run/42)"}}
		]
	}`))
	if err != nil {
		t.Fatalf("tool call failed: %v", err)
	}

	responseBody, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("unexpected tool result type: %T", result)
	}
	if responseBody["message_id"] != "interactive-msg-markdown" {
		t.Fatalf("unexpected message id: %#v", responseBody["message_id"])
	}
}

func TestSeaTalkSendInteractiveMessageToolDescriptionMentionsMarkdown(t *testing.T) {
	t.Parallel()

	description := seaTalkSendInteractiveMessageTool{}.Description()
	if !strings.Contains(description, "Description elements support SeaTalk Markdown.") {
		t.Fatalf("expected markdown support in tool description, got %q", description)
	}
}

func TestSeaTalkSendFileToolUsesCurrentGroupConversationTarget(t *testing.T) {
	t.Parallel()

	filePath := filepath.Join(t.TempDir(), "report.csv")
	if err := os.WriteFile(filePath, []byte("name,value\nfoo,1\n"), 0o600); err != nil {
		t.Fatalf("write file failed: %v", err)
	}

	client := seatalk.NewClient(
		seatalk.Config{AppID: "app-id", AppSecret: "app-secret"},
		seatalk.WithHTTPClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.Method != http.MethodPost {
				t.Fatalf("unexpected method: %s", req.Method)
			}
			if req.URL.Path != "/messaging/v2/group_chat" {
				t.Fatalf("unexpected path: %s", req.URL.Path)
			}

			var body struct {
				GroupID string `json:"group_id"`
				Message struct {
					ThreadID string `json:"thread_id"`
					Tag      string `json:"tag"`
					File     struct {
						Filename string `json:"filename"`
						Content  string `json:"content"`
					} `json:"file"`
				} `json:"message"`
			}
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				t.Fatalf("decode request failed: %v", err)
			}
			if body.GroupID != "group-1" {
				t.Fatalf("unexpected group id: %q", body.GroupID)
			}
			if body.Message.ThreadID != "thread-1" {
				t.Fatalf("unexpected thread id: %q", body.Message.ThreadID)
			}
			if body.Message.Tag != "file" {
				t.Fatalf("unexpected tag: %q", body.Message.Tag)
			}
			if body.Message.File.Filename != "custom-report.csv" {
				t.Fatalf("unexpected filename: %q", body.Message.File.Filename)
			}
			if body.Message.File.Content == "" {
				t.Fatal("expected base64 content")
			}

			return jsonResponse(t, map[string]any{
				"code":       0,
				"message_id": "file-msg-1",
			}), nil
		})}),
		seatalk.WithTokenProvider(func(context.Context, *http.Client, string, string) (string, error) {
			return "token-123", nil
		}),
	)

	tool := seaTalkSendFileTool{}
	ctx := agent.ContextWithTurnRequest(context.Background(), agent.TurnRequest{
		Conversation: agent.ConversationState{Key: "seatalk:group:group-1:thread-1"},
		Message: agent.InboundMessage{
			Responder: &SeaTalkResponder{
				client: client,
				target: seaTalkReplyTarget{
					isGroup:  true,
					groupID:  "group-1",
					threadID: "thread-1",
				},
			},
		},
	})

	result, err := tool.Call(ctx, json.RawMessage(`{
		"local_file_path": "`+filePath+`",
		"filename": "custom-report.csv"
	}`))
	if err != nil {
		t.Fatalf("tool call failed: %v", err)
	}

	body, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("unexpected tool result type: %T", result)
	}
	if body["message_id"] != "file-msg-1" {
		t.Fatalf("unexpected message id: %#v", body["message_id"])
	}
	if body["filename"] != "custom-report.csv" {
		t.Fatalf("unexpected filename: %#v", body["filename"])
	}
}

func TestSeaTalkSendFileToolUsesCurrentPrivateConversationTarget(t *testing.T) {
	t.Parallel()

	filePath := filepath.Join(t.TempDir(), "artifact.json")
	if err := os.WriteFile(filePath, []byte(`{"ok":true}`), 0o600); err != nil {
		t.Fatalf("write file failed: %v", err)
	}

	client := seatalk.NewClient(
		seatalk.Config{AppID: "app-id", AppSecret: "app-secret"},
		seatalk.WithHTTPClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.Method != http.MethodPost {
				t.Fatalf("unexpected method: %s", req.Method)
			}
			if req.URL.Path != "/messaging/v2/single_chat" {
				t.Fatalf("unexpected path: %s", req.URL.Path)
			}

			var body struct {
				EmployeeCode   string `json:"employee_code"`
				UsablePlatform string `json:"usable_platform"`
				Message        struct {
					ThreadID string `json:"thread_id"`
					Tag      string `json:"tag"`
					File     struct {
						Filename string `json:"filename"`
						Content  string `json:"content"`
					} `json:"file"`
				} `json:"message"`
			}
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				t.Fatalf("decode request failed: %v", err)
			}
			if body.EmployeeCode != "e_1" {
				t.Fatalf("unexpected employee code: %q", body.EmployeeCode)
			}
			if body.UsablePlatform != seatalk.UsablePlatformAll {
				t.Fatalf("unexpected usable_platform: %q", body.UsablePlatform)
			}
			if body.Message.ThreadID != "thread-1" {
				t.Fatalf("unexpected thread id: %q", body.Message.ThreadID)
			}
			if body.Message.File.Filename != "artifact.json" {
				t.Fatalf("unexpected filename: %q", body.Message.File.Filename)
			}

			return jsonResponse(t, map[string]any{
				"code":       0,
				"message_id": "file-msg-2",
			}), nil
		})}),
		seatalk.WithTokenProvider(func(context.Context, *http.Client, string, string) (string, error) {
			return "token-123", nil
		}),
	)

	tool := seaTalkSendFileTool{}
	ctx := agent.ContextWithTurnRequest(context.Background(), agent.TurnRequest{
		Conversation: agent.ConversationState{Key: "seatalk:private:e_1:thread-1"},
		Message: agent.InboundMessage{
			Responder: &SeaTalkResponder{
				client: client,
				target: seaTalkReplyTarget{
					employeeCode: "e_1",
					threadID:     "thread-1",
				},
			},
		},
	})

	result, err := tool.Call(ctx, json.RawMessage(`{
		"local_file_path": "`+filePath+`"
	}`))
	if err != nil {
		t.Fatalf("tool call failed: %v", err)
	}

	body, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("unexpected tool result type: %T", result)
	}
	if body["message_id"] != "file-msg-2" {
		t.Fatalf("unexpected message id: %#v", body["message_id"])
	}
	if body["filename"] != "artifact.json" {
		t.Fatalf("unexpected filename: %#v", body["filename"])
	}
}

func TestSeaTalkInteractiveUpdateToolDefaultsToClickedMessageID(t *testing.T) {
	t.Parallel()

	client := seatalk.NewClient(
		seatalk.Config{AppID: "app-id", AppSecret: "app-secret"},
		seatalk.WithHTTPClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.Method != http.MethodPost {
				t.Fatalf("unexpected method: %s", req.Method)
			}
			if req.URL.Path != "/messaging/v2/update" {
				t.Fatalf("unexpected path: %s", req.URL.Path)
			}

			var body map[string]any
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				t.Fatalf("decode request failed: %v", err)
			}
			if body["message_id"] != "interactive-msg-1" {
				t.Fatalf("unexpected message id: %#v", body["message_id"])
			}

			return jsonResponse(t, map[string]any{
				"code": 0,
			}), nil
		})}),
		seatalk.WithTokenProvider(func(context.Context, *http.Client, string, string) (string, error) {
			return "token-123", nil
		}),
	)

	tool := seaTalkUpdateInteractiveMessageTool{}
	ctx := agent.ContextWithTurnRequest(context.Background(), agent.TurnRequest{
		Conversation: agent.ConversationState{Key: "seatalk:private:e_1:thread-1"},
		Message: agent.InboundMessage{
			Responder: &SeaTalkResponder{
				client:             client,
				interactiveMessage: "interactive-msg-1",
				target: seaTalkReplyTarget{
					employeeCode: "e_1",
					threadID:     "thread-1",
				},
			},
		},
	})

	result, err := tool.Call(ctx, json.RawMessage(`{
		"elements": [
			{"element_type":"title","title":{"text":"Approved"}}
		]
	}`))
	if err != nil {
		t.Fatalf("tool call failed: %v", err)
	}

	body, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("unexpected tool result type: %T", result)
	}
	if body["message_id"] != "interactive-msg-1" {
		t.Fatalf("unexpected message id: %#v", body["message_id"])
	}
	if body["updated"] != true {
		t.Fatalf("unexpected updated flag: %#v", body["updated"])
	}
}

func TestSeaTalkAgentAdapterResolvesTokenizedInteractiveClickEvent(t *testing.T) {
	t.Parallel()

	runner := &testRunner{}
	dispatcher := agent.NewDispatcher(agent.DispatcherOptions{
		Store:       agent.NewConversationStore(cache.NewMemoryStorage()),
		Runner:      runner,
		WorkerCount: 1,
	})
	seaTalkAdapter := newSeaTalkAgentAdapterWithClient(dispatcher, nil)

	if err := dispatcher.Start(); err != nil {
		t.Fatalf("start dispatcher failed: %v", err)
	}
	defer func() {
		if err := dispatcher.Shutdown(context.Background()); err != nil {
			t.Fatalf("shutdown dispatcher failed: %v", err)
		}
	}()

	rawValue := `{"action":"tool_call","tool_name":"seatalk_update_interactive_message","tool_input_json":"{\"message_id\":\"interactive-msg-1\",\"elements\":[{\"element_type\":\"title\",\"title\":{\"text\":\"Approved with a long tokenized callback payload to exceed SeaTalk limits\"}},{\"element_type\":\"description\",\"description\":{\"text\":\"This payload should be stored in cache instead of being sent inline.\",\"format\":2}}]}"}`
	tokenizedValue, err := normalizeInteractiveCallbackValue(context.Background(), rawValue)
	if err != nil {
		t.Fatalf("normalize callback value failed: %v", err)
	}
	if !strings.HasPrefix(tokenizedValue, interactiveCallbackValueRefPrefix) {
		t.Fatalf("expected tokenized callback value, got %q", tokenizedValue)
	}

	req := seatalk.EventRequest{EventID: "evt-interactive-2", Timestamp: 1_700_000_000_000}
	event := &seatalk.InteractiveMessageClickEvent{
		MessageID:    "msg-card-1",
		EmployeeCode: "e_1",
		Email:        "alice@example.com",
		ThreadID:     "thread-1",
		Value:        tokenizedValue,
	}

	if _, err := seaTalkAdapter.ProcessEvent(context.Background(), req, event); err != nil {
		t.Fatalf("process event failed: %v", err)
	}

	if err := waitForRunnerCalls(runner); err != nil {
		t.Fatal(err)
	}

	reqCall := runner.LastRequest()
	if !strings.Contains(reqCall.Message.Text, rawValue) {
		t.Fatalf("unexpected resolved callback payload: %q", reqCall.Message.Text)
	}
}

func TestSeaTalkAgentAdapterResolvesTokenizedInteractivePromptAction(t *testing.T) {
	t.Parallel()

	runner := &testRunner{}
	dispatcher := agent.NewDispatcher(agent.DispatcherOptions{
		Store:       agent.NewConversationStore(cache.NewMemoryStorage()),
		Runner:      runner,
		WorkerCount: 1,
	})
	seaTalkAdapter := newSeaTalkAgentAdapterWithClient(dispatcher, nil)

	if err := dispatcher.Start(); err != nil {
		t.Fatalf("start dispatcher failed: %v", err)
	}
	defer func() {
		if err := dispatcher.Shutdown(context.Background()); err != nil {
			t.Fatalf("shutdown dispatcher failed: %v", err)
		}
	}()

	rawValue := `{"action":"prompt","prompt":"Continue with the approval workflow, summarize the current deployment blockers for the team, list the failing checks, explain the likely root cause, and ask the reviewer whether to retry the deployment or pause it for manual investigation before taking the next step."}`
	tokenizedValue, err := normalizeInteractiveCallbackValue(context.Background(), rawValue)
	if err != nil {
		t.Fatalf("normalize callback value failed: %v", err)
	}
	if !strings.HasPrefix(tokenizedValue, interactiveCallbackValueRefPrefix) {
		t.Fatalf("expected tokenized callback value, got %q", tokenizedValue)
	}

	req := seatalk.EventRequest{EventID: "evt-interactive-prompt-2", Timestamp: 1_700_000_000_000}
	event := &seatalk.InteractiveMessageClickEvent{
		MessageID:    "msg-card-1",
		EmployeeCode: "e_1",
		Email:        "alice@example.com",
		ThreadID:     "thread-1",
		Value:        tokenizedValue,
	}

	if _, err := seaTalkAdapter.ProcessEvent(context.Background(), req, event); err != nil {
		t.Fatalf("process event failed: %v", err)
	}

	if err := waitForRunnerCalls(runner); err != nil {
		t.Fatal(err)
	}

	reqCall := runner.LastRequest()
	if got := reqCall.Message.Text; got != "Continue with the approval workflow, summarize the current deployment blockers for the team, list the failing checks, explain the likely root cause, and ask the reviewer whether to retry the deployment or pause it for manual investigation before taking the next step." {
		t.Fatalf("unexpected resolved prompt payload: %q", got)
	}
}

func TestSeaTalkInteractiveSendToolSkipsMissingLocalImage(t *testing.T) {
	t.Parallel()

	client := seatalk.NewClient(
		seatalk.Config{AppID: "app-id", AppSecret: "app-secret"},
		seatalk.WithHTTPClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			var body map[string]any
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				t.Fatalf("decode request failed: %v", err)
			}
			message, ok := body["message"].(map[string]any)
			if !ok {
				t.Fatalf("unexpected message payload: %#v", body["message"])
			}
			interactiveMessage, ok := message["interactive_message"].(map[string]any)
			if !ok {
				t.Fatalf("unexpected interactive message payload: %#v", message["interactive_message"])
			}
			elements, ok := interactiveMessage["elements"].([]any)
			if !ok || len(elements) != 1 {
				t.Fatalf("unexpected elements payload: %#v", interactiveMessage["elements"])
			}
			element, ok := elements[0].(map[string]any)
			if !ok {
				t.Fatalf("unexpected element payload: %#v", elements[0])
			}
			if element["element_type"] != "title" {
				t.Fatalf("unexpected element type: %#v", element["element_type"])
			}

			return jsonResponse(t, map[string]any{
				"code":       0,
				"message_id": "interactive-msg-2",
			}), nil
		})}),
		seatalk.WithTokenProvider(func(context.Context, *http.Client, string, string) (string, error) {
			return "token-123", nil
		}),
	)

	tool := seaTalkSendInteractiveMessageTool{}
	ctx := agent.ContextWithTurnRequest(context.Background(), agent.TurnRequest{
		Conversation: agent.ConversationState{Key: "seatalk:group:group-1:thread-1"},
		Message: agent.InboundMessage{
			Responder: &SeaTalkResponder{
				client: client,
				target: seaTalkReplyTarget{
					isGroup:  true,
					groupID:  "group-1",
					threadID: "thread-1",
				},
			},
		},
	})

	result, err := tool.Call(ctx, json.RawMessage(`{
		"elements": [
			{"element_type":"title","title":{"text":"Card title"}},
			{"element_type":"image","image":{"local_file_path":"/tmp/does-not-exist.png"}}
		]
	}`))
	if err != nil {
		t.Fatalf("tool call failed: %v", err)
	}

	body, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("unexpected tool result type: %T", result)
	}
	if body["message_id"] != "interactive-msg-2" {
		t.Fatalf("unexpected message id: %#v", body["message_id"])
	}
}

func TestSeaTalkInteractiveSendToolSkipsInvalidLocalImageContent(t *testing.T) {
	t.Parallel()

	tempFile, err := os.CreateTemp(t.TempDir(), "not-image-*.txt")
	if err != nil {
		t.Fatalf("create temp file failed: %v", err)
	}
	if _, err = tempFile.WriteString("plain text is not an image"); err != nil {
		t.Fatalf("write temp file failed: %v", err)
	}
	if err = tempFile.Close(); err != nil {
		t.Fatalf("close temp file failed: %v", err)
	}

	client := seatalk.NewClient(
		seatalk.Config{AppID: "app-id", AppSecret: "app-secret"},
		seatalk.WithHTTPClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			var body map[string]any
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				t.Fatalf("decode request failed: %v", err)
			}
			message, ok := body["message"].(map[string]any)
			if !ok {
				t.Fatalf("unexpected message payload: %#v", body["message"])
			}
			interactiveMessage, ok := message["interactive_message"].(map[string]any)
			if !ok {
				t.Fatalf("unexpected interactive message payload: %#v", message["interactive_message"])
			}
			elements, ok := interactiveMessage["elements"].([]any)
			if !ok || len(elements) != 1 {
				t.Fatalf("unexpected elements payload: %#v", interactiveMessage["elements"])
			}

			return jsonResponse(t, map[string]any{
				"code":       0,
				"message_id": "interactive-msg-2b",
			}), nil
		})}),
		seatalk.WithTokenProvider(func(context.Context, *http.Client, string, string) (string, error) {
			return "token-123", nil
		}),
	)

	tool := seaTalkSendInteractiveMessageTool{}
	ctx := agent.ContextWithTurnRequest(context.Background(), agent.TurnRequest{
		Conversation: agent.ConversationState{Key: "seatalk:group:group-1:thread-1"},
		Message: agent.InboundMessage{
			Responder: &SeaTalkResponder{
				client: client,
				target: seaTalkReplyTarget{
					isGroup:  true,
					groupID:  "group-1",
					threadID: "thread-1",
				},
			},
		},
	})

	result, err := tool.Call(ctx, json.RawMessage(`{
		"elements": [
			{"element_type":"title","title":{"text":"Card title"}},
			{"element_type":"image","image":{"local_file_path":"`+tempFile.Name()+`"}}
		]
	}`))
	if err != nil {
		t.Fatalf("tool call failed: %v", err)
	}

	body, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("unexpected tool result type: %T", result)
	}
	if body["message_id"] != "interactive-msg-2b" {
		t.Fatalf("unexpected message id: %#v", body["message_id"])
	}
}

func TestSeaTalkAgentAdapterPopulatesGroupMentionFromEmployeeInfoFallback(t *testing.T) {
	t.Parallel()

	client := seatalk.NewClient(
		seatalk.Config{AppID: "app-id", AppSecret: "app-secret", EmployeeInfoEnabled: true},
		seatalk.WithHTTPClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.Method != http.MethodGet {
				t.Fatalf("unexpected method: %s", req.Method)
			}
			if req.URL.Path != "/contacts/v2/profile" {
				t.Fatalf("unexpected path: %s", req.URL.Path)
			}
			if req.URL.Query().Get("employee_code") != "e_group_1" {
				t.Fatalf("unexpected employee code: %s", req.URL.Query().Get("employee_code"))
			}
			return jsonResponse(t, map[string]any{
				"code": 0,
				"employees": []map[string]any{
					{
						"employee_code": "e_group_1",
						"seatalk_id":    "seatalk-user-1",
						"email":         "alice@example.com",
					},
				},
			}), nil
		})}),
		seatalk.WithTokenProvider(func(context.Context, *http.Client, string, string) (string, error) {
			return "token-123", nil
		}),
	)

	adapter := newSeaTalkAgentAdapterWithClient(nil, client)
	responder := &SeaTalkResponder{
		client: client,
		target: seaTalkReplyTarget{
			isGroup:         true,
			mentionEmployee: "e_group_1",
		},
	}

	if err := adapter.populateReplyMention(context.Background(), responder); err != nil {
		t.Fatalf("populate reply mention failed: %v", err)
	}
	if responder.target.mentionTarget.seatalkID != "seatalk-user-1" {
		t.Fatalf("unexpected mention seatalk id: %q", responder.target.mentionTarget.seatalkID)
	}
	if responder.target.mentionTarget.email != "alice@example.com" {
		t.Fatalf("unexpected mention email: %q", responder.target.mentionTarget.email)
	}
}

func TestSeaTalkAgentAdapterSkipsMentionFallbackWhenEmailExists(t *testing.T) {
	t.Parallel()

	client := seatalk.NewClient(
		seatalk.Config{AppID: "app-id", AppSecret: "app-secret"},
		seatalk.WithHTTPClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			t.Fatalf("unexpected request: %s %s", req.Method, req.URL.String())
			return nil, nil
		})}),
		seatalk.WithTokenProvider(func(context.Context, *http.Client, string, string) (string, error) {
			t.Fatalf("token provider should not be called")
			return "", nil
		}),
	)

	adapter := newSeaTalkAgentAdapterWithClient(nil, client)
	responder := &SeaTalkResponder{
		client: client,
		target: seaTalkReplyTarget{
			isGroup:         true,
			mentionEmployee: "e_group_1",
			mentionTarget:   seaTalkMentionTarget{email: "alice@example.com"},
		},
	}

	if err := adapter.populateReplyMention(context.Background(), responder); err != nil {
		t.Fatalf("populate reply mention failed: %v", err)
	}
	if responder.target.mentionTarget.seatalkID != "" {
		t.Fatalf("unexpected mention seatalk id: %q", responder.target.mentionTarget.seatalkID)
	}
	if responder.target.mentionTarget.email != "alice@example.com" {
		t.Fatalf("unexpected mention email: %q", responder.target.mentionTarget.email)
	}
}

func TestSeaTalkAgentAdapterTools(t *testing.T) {
	t.Parallel()

	adapter := newSeaTalkAgentAdapterWithClient(nil, seatalk.NewClient(seatalk.Config{AppID: "app-id", AppSecret: "app-secret"}))
	tools := adapter.Tools()
	if len(tools) != 3 {
		t.Fatalf("unexpected tool count: %d", len(tools))
	}
	if tools[0].Name() != "seatalk_send_file" {
		t.Fatalf("unexpected first tool name: %s", tools[0].Name())
	}
	if tools[1].Name() != "seatalk_send_interactive_message" {
		t.Fatalf("unexpected second tool name: %s", tools[1].Name())
	}
	if tools[2].Name() != "seatalk_update_interactive_message" {
		t.Fatalf("unexpected third tool name: %s", tools[2].Name())
	}
}

func TestSeaTalkAgentAdapterSkipsMentionFallbackWhenEmployeeInfoDisabled(t *testing.T) {
	t.Parallel()

	client := seatalk.NewClient(
		seatalk.Config{AppID: "app-id", AppSecret: "app-secret"},
		seatalk.WithHTTPClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			t.Fatalf("unexpected request: %s %s", req.Method, req.URL.String())
			return nil, nil
		})}),
	)

	adapter := newSeaTalkAgentAdapterWithClient(nil, client)
	responder := &SeaTalkResponder{
		client: client,
		target: seaTalkReplyTarget{
			isGroup:         true,
			mentionEmployee: "e_group_1",
		},
	}

	if err := adapter.populateReplyMention(context.Background(), responder); err != nil {
		t.Fatalf("populate reply mention failed: %v", err)
	}
	if !responder.target.mentionTarget.IsZero() {
		t.Fatalf("unexpected mention target: %+v", responder.target.mentionTarget)
	}
}

func TestSeaTalkAgentAdapterSkipsEmployeeProfileWhenEmployeeInfoDisabled(t *testing.T) {
	t.Parallel()

	client := seatalk.NewClient(
		seatalk.Config{AppID: "app-id", AppSecret: "app-secret"},
		seatalk.WithHTTPClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			t.Fatalf("unexpected request: %s %s", req.Method, req.URL.String())
			return nil, nil
		})}),
	)

	adapter := newSeaTalkAgentAdapterWithClient(nil, client)
	got, err := adapter.buildPrivateThreadInitialContext(context.Background(), "e_1")
	if err != nil {
		t.Fatalf("build private thread initial context failed: %v", err)
	}
	if got != "Private thread guidance:\n- This conversation is a private chat thread." {
		t.Fatalf("unexpected initial context: %q", got)
	}
}

func TestSeaTalkAgentAdapterSkipsPrivateThreadEmployeeProfileHistoryWhenEmployeeInfoDisabled(t *testing.T) {
	t.Parallel()

	client := seatalk.NewClient(
		seatalk.Config{AppID: "app-id", AppSecret: "app-secret"},
		seatalk.WithHTTPClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.URL.Path == "/contacts/v2/profile" {
				t.Fatalf("unexpected employee profile request")
			}
			if req.Method != http.MethodGet {
				t.Fatalf("unexpected method: %s", req.Method)
			}
			if req.URL.Path != "/messaging/v2/single_chat/get_thread_by_thread_id" {
				t.Fatalf("unexpected path: %s", req.URL.Path)
			}
			return jsonResponse(t, map[string]any{
				"code": 0,
				"thread_messages": []map[string]any{
					{
						"message_id":        "msg-1",
						"thread_id":         "thread-1",
						"message_sent_time": 1000,
						"tag":               "text",
						"sender":            map[string]any{"email": "alice@example.com"},
						"text":              map[string]any{"plain_text": "earlier private message"},
						"quoted_message_id": "",
					},
					{
						"message_id":        "msg-2",
						"thread_id":         "thread-1",
						"message_sent_time": 2000,
						"tag":               "text",
						"sender":            map[string]any{"email": "alice@example.com"},
						"text":              map[string]any{"plain_text": "current private message"},
						"quoted_message_id": "",
					},
				},
			}), nil
		})}),
		seatalk.WithTokenProvider(func(context.Context, *http.Client, string, string) (string, error) {
			return "token-123", nil
		}),
	)

	adapter := newSeaTalkAgentAdapterWithClient(nil, client)
	got, err := adapter.buildPrivateThreadInitialMessages(context.Background(), nil, "seatalk:private:e_1:thread-1", "e_1", "thread-1", "msg-2")
	if err != nil {
		t.Fatalf("build private thread initial messages failed: %v", err)
	}

	if len(got) != 1 {
		t.Fatalf("unexpected initial message count: %d", len(got))
	}
	if got[0].Text != "earlier private message" {
		t.Fatalf("unexpected initial message text: %q", got[0].Text)
	}
}

func TestSeaTalkAgentAdapterBuildsThreadHistoryMessagesWithImages(t *testing.T) {
	t.Parallel()

	client := seatalk.NewClient(
		seatalk.Config{AppID: "app-id", AppSecret: "app-secret"},
		seatalk.WithHTTPClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.Method != http.MethodGet {
				t.Fatalf("unexpected method: %s", req.Method)
			}
			switch req.URL.Path {
			case "/messaging/v2/single_chat/get_thread_by_thread_id":
				return jsonResponse(t, map[string]any{
					"code": 0,
					"thread_messages": []map[string]any{
						{
							"message_id":        "msg-1",
							"thread_id":         "thread-1",
							"message_sent_time": 1000,
							"tag":               "image",
							"sender":            map[string]any{"email": "alice@example.com"},
							"image":             map[string]any{"content": "https://openapi.seatalk.io/messaging/v2/file/history-image"},
							"quoted_message_id": "",
						},
						{
							"message_id":        "msg-2",
							"thread_id":         "thread-1",
							"message_sent_time": 2000,
							"tag":               "text",
							"sender":            map[string]any{"email": "alice@example.com"},
							"text":              map[string]any{"plain_text": "current private message"},
							"quoted_message_id": "",
						},
					},
				}), nil
			case "/messaging/v2/file/history-image":
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader("image-bytes")),
				}, nil
			default:
				t.Fatalf("unexpected path: %s", req.URL.Path)
				return nil, nil
			}
		})}),
		seatalk.WithTokenProvider(func(context.Context, *http.Client, string, string) (string, error) {
			return "token-123", nil
		}),
	)

	adapter := newSeaTalkAgentAdapterWithClient(nil, client)
	responder := &SeaTalkResponder{client: client}
	got, err := adapter.buildPrivateThreadInitialMessages(context.Background(), responder, "seatalk:private:e_1:thread-1", "e_1", "thread-1", "msg-2")
	if err != nil {
		t.Fatalf("build private thread initial messages failed: %v", err)
	}

	if len(got) != 1 {
		t.Fatalf("unexpected initial message count: %d", len(got))
	}
	if got[0].Kind != agent.MessageKindImage {
		t.Fatalf("unexpected initial message kind: %s", got[0].Kind)
	}
	if got[0].ImagePath == "" {
		t.Fatal("expected image path to be populated")
	}
	if len(got[0].ImagePaths) != 1 || got[0].ImagePaths[0] != got[0].ImagePath {
		t.Fatalf("unexpected image paths: %+v", got[0].ImagePaths)
	}
	if err := responder.Cleanup(context.Background()); err != nil {
		t.Fatalf("cleanup failed: %v", err)
	}
}

func TestSeaTalkAgentAdapterBuildsThreadHistoryMessagesWithFiles(t *testing.T) {
	t.Parallel()

	client := seatalk.NewClient(
		seatalk.Config{AppID: "app-id", AppSecret: "app-secret"},
		seatalk.WithHTTPClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.Method != http.MethodGet {
				t.Fatalf("unexpected method: %s", req.Method)
			}
			switch req.URL.Path {
			case "/messaging/v2/single_chat/get_thread_by_thread_id":
				return jsonResponse(t, map[string]any{
					"code": 0,
					"thread_messages": []map[string]any{
						{
							"message_id":        "msg-1",
							"thread_id":         "thread-1",
							"message_sent_time": 1000,
							"tag":               "file",
							"sender":            map[string]any{"email": "alice@example.com"},
							"file": map[string]any{
								"content":  "https://openapi.seatalk.io/messaging/v2/file/history-file",
								"filename": "report.pdf",
							},
							"quoted_message_id": "",
						},
						{
							"message_id":        "msg-2",
							"thread_id":         "thread-1",
							"message_sent_time": 2000,
							"tag":               "text",
							"sender":            map[string]any{"email": "alice@example.com"},
							"text":              map[string]any{"plain_text": "current private message"},
							"quoted_message_id": "",
						},
					},
				}), nil
			case "/messaging/v2/file/history-file":
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader("%PDF-1.7 demo")),
				}, nil
			default:
				t.Fatalf("unexpected path: %s", req.URL.Path)
				return nil, nil
			}
		})}),
		seatalk.WithTokenProvider(func(context.Context, *http.Client, string, string) (string, error) {
			return "token-123", nil
		}),
	)

	adapter := newSeaTalkAgentAdapterWithClient(nil, client)
	responder := &SeaTalkResponder{client: client}
	got, err := adapter.buildPrivateThreadInitialMessages(context.Background(), responder, "seatalk:private:e_1:thread-1", "e_1", "thread-1", "msg-2")
	if err != nil {
		t.Fatalf("build private thread initial messages failed: %v", err)
	}

	if len(got) != 1 {
		t.Fatalf("unexpected initial message count: %d", len(got))
	}
	if got[0].Kind != agent.MessageKindFile {
		t.Fatalf("unexpected initial message kind: %s", got[0].Kind)
	}
	if got[0].Text != "report.pdf" {
		t.Fatalf("unexpected initial message text: %q", got[0].Text)
	}
	if got[0].FilePath == "" {
		t.Fatal("expected file path to be populated")
	}
	if len(got[0].FilePaths) != 1 || got[0].FilePaths[0] != got[0].FilePath {
		t.Fatalf("unexpected file paths: %+v", got[0].FilePaths)
	}
	if !strings.HasSuffix(got[0].FilePath, ".pdf") {
		t.Fatalf("unexpected file extension: %q", got[0].FilePath)
	}
	if err := responder.Cleanup(context.Background()); err != nil {
		t.Fatalf("cleanup failed: %v", err)
	}
}

func TestSeaTalkAgentAdapterBuildsThreadHistoryMessagesWithVideos(t *testing.T) {
	t.Parallel()

	client := seatalk.NewClient(
		seatalk.Config{AppID: "app-id", AppSecret: "app-secret"},
		seatalk.WithHTTPClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.Method != http.MethodGet {
				t.Fatalf("unexpected method: %s", req.Method)
			}
			switch req.URL.Path {
			case "/messaging/v2/single_chat/get_thread_by_thread_id":
				return jsonResponse(t, map[string]any{
					"code": 0,
					"thread_messages": []map[string]any{
						{
							"message_id":        "msg-1",
							"thread_id":         "thread-1",
							"message_sent_time": 1000,
							"tag":               "video",
							"sender":            map[string]any{"email": "alice@example.com"},
							"video":             map[string]any{"content": "https://openapi.seatalk.io/messaging/v2/file/history-video"},
							"quoted_message_id": "",
						},
						{
							"message_id":        "msg-2",
							"thread_id":         "thread-1",
							"message_sent_time": 2000,
							"tag":               "text",
							"sender":            map[string]any{"email": "alice@example.com"},
							"text":              map[string]any{"plain_text": "current private message"},
							"quoted_message_id": "",
						},
					},
				}), nil
			case "/messaging/v2/file/history-video":
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader("video-bytes")),
				}, nil
			default:
				t.Fatalf("unexpected path: %s", req.URL.Path)
				return nil, nil
			}
		})}),
		seatalk.WithTokenProvider(func(context.Context, *http.Client, string, string) (string, error) {
			return "token-123", nil
		}),
	)

	adapter := newSeaTalkAgentAdapterWithClient(nil, client)
	responder := &SeaTalkResponder{client: client}
	got, err := adapter.buildPrivateThreadInitialMessages(context.Background(), responder, "seatalk:private:e_1:thread-1", "e_1", "thread-1", "msg-2")
	if err != nil {
		t.Fatalf("build private thread initial messages failed: %v", err)
	}

	if len(got) != 1 {
		t.Fatalf("unexpected initial message count: %d", len(got))
	}
	if got[0].Kind != agent.MessageKindVideo {
		t.Fatalf("unexpected initial message kind: %s", got[0].Kind)
	}
	if got[0].VideoPath == "" {
		t.Fatal("expected video path to be populated")
	}
	if len(got[0].VideoPaths) != 1 || got[0].VideoPaths[0] != got[0].VideoPath {
		t.Fatalf("unexpected video paths: %+v", got[0].VideoPaths)
	}
	if err := responder.Cleanup(context.Background()); err != nil {
		t.Fatalf("cleanup failed: %v", err)
	}
}

type testRunner struct {
	mu      sync.Mutex
	calls   int
	lastReq agent.TurnRequest
}

type typingRunner struct {
	mu      sync.Mutex
	calls   int
	lastReq agent.TurnRequest
}

func (r *testRunner) RunTurn(_ context.Context, req agent.TurnRequest) (agent.TurnResult, error) {
	r.mu.Lock()
	r.calls++
	r.lastReq = req
	r.mu.Unlock()
	return agent.TurnResult{}, nil
}

func (*testRunner) RegisterSystemPrompt(string) {}

func (*testRunner) RegisterTools(...agent.Tool) {}

func (r *typingRunner) RunTurn(ctx context.Context, req agent.TurnRequest) (agent.TurnResult, error) {
	if err := req.Message.Responder.SetTyping(ctx); err != nil {
		return agent.TurnResult{}, err
	}

	r.mu.Lock()
	r.calls++
	r.lastReq = req
	r.mu.Unlock()
	return agent.TurnResult{}, nil
}

func (*typingRunner) RegisterSystemPrompt(string) {}

func (*typingRunner) RegisterTools(...agent.Tool) {}

func (r *testRunner) LastRequest() agent.TurnRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastReq
}

func (r *testRunner) Calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

func (r *typingRunner) LastRequest() agent.TurnRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastReq
}

func waitForRunnerCalls(runner *testRunner) error {
	deadline := time.After(time.Second)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	for {
		runner.mu.Lock()
		calls := runner.calls
		runner.mu.Unlock()
		if calls >= 1 {
			return nil
		}

		select {
		case <-deadline:
			return context.DeadlineExceeded
		case <-ticker.C:
		}
	}
}

func waitForTypingRunnerCall(runner *typingRunner) error {
	deadline := time.After(time.Second)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	for {
		runner.mu.Lock()
		calls := runner.calls
		runner.mu.Unlock()
		if calls >= 1 {
			return nil
		}

		select {
		case <-deadline:
			return context.DeadlineExceeded
		case <-ticker.C:
		}
	}
}

type blockingTestRunner struct {
	testRunner

	started chan struct{}
	release chan struct{}
}

func (r *blockingTestRunner) RunTurn(_ context.Context, req agent.TurnRequest) (agent.TurnResult, error) {
	r.mu.Lock()
	r.calls++
	r.lastReq = req
	r.mu.Unlock()

	r.started <- struct{}{}
	<-r.release
	return agent.TurnResult{}, nil
}

func waitForBlockingRunnerStarts(runner *blockingTestRunner, calls int) error {
	deadline := time.After(time.Second)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	for {
		if runner.Calls() >= calls {
			return nil
		}

		select {
		case <-deadline:
			return context.DeadlineExceeded
		case <-ticker.C:
		}
	}
}

func waitForInteractiveActionUnlock(adapter *SeaTalkAgentAdapter, event *seatalk.InteractiveMessageClickEvent) error {
	deadline := time.After(time.Second)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	key := interactiveActionLockKey(event)
	for {
		_, err := adapter.interactiveActionStore.Get(context.Background(), key)
		if errors.Is(err, cache.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}

		select {
		case <-deadline:
			return context.DeadlineExceeded
		case <-ticker.C:
		}
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func jsonResponse(t *testing.T, body any) *http.Response {
	t.Helper()

	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal response failed: %v", err)
	}

	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(payload)),
	}
}
