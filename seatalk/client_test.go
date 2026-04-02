package seatalk

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClientSendText(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("unexpected method: %s", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer token-123" {
			t.Fatalf("unexpected authorization header: %s", got)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Fatalf("unexpected content type: %s", got)
		}

		var req sendGroupChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request failed: %v", err)
		}
		if req.GroupID != "group-1" {
			t.Fatalf("unexpected group id: %s", req.GroupID)
		}
		if req.Message.Tag != "text" {
			t.Fatalf("unexpected message tag: %s", req.Message.Tag)
		}
		if req.Message.Text == nil {
			t.Fatal("expected text body")
		}
		if req.Message.Text.Format != TextFormatPlain {
			t.Fatalf("unexpected text format: %d", req.Message.Text.Format)
		}
		if req.Message.Text.Content != "hello" {
			t.Fatalf("unexpected text content: %s", req.Message.Text.Content)
		}
		if req.Message.QuotedMessageID != "quoted-1" {
			t.Fatalf("unexpected quoted message id: %s", req.Message.QuotedMessageID)
		}
		if req.Message.ThreadID != "thread-1" {
			t.Fatalf("unexpected thread id: %s", req.Message.ThreadID)
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(sendGroupChatResponse{
			Code:      0,
			MessageID: "message-1",
		}); err != nil {
			t.Fatalf("encode response failed: %v", err)
		}
	}))
	defer server.Close()

	originalEndpoint := sendGroupChatEndpoint
	sendGroupChatEndpoint = server.URL
	defer func() {
		sendGroupChatEndpoint = originalEndpoint
	}()

	client := NewClient(Config{AppID: "app-id", AppSecret: "app-secret", EmployeeInfoEnabled: true})
	client.httpClient = server.Client()
	client.tokenProvider = func(ctx context.Context, httpClient *http.Client, appID, appSecret string) (string, error) {
		if httpClient != server.Client() {
			t.Fatal("unexpected token provider client")
		}
		if appID != "app-id" || appSecret != "app-secret" {
			t.Fatalf("unexpected app credentials: %s %s", appID, appSecret)
		}
		return "token-123", nil
	}

	result, err := client.SendGroupText(context.Background(), "group-1", TextMessage{
		Content: "hello",
		Format:  TextFormatPlain,
	}, SendOptions{
		QuotedMessageID: "quoted-1",
		ThreadID:        "thread-1",
	})
	if err != nil {
		t.Fatalf("send text failed: %v", err)
	}
	if result.MessageID != "message-1" {
		t.Fatalf("unexpected message id: %s", result.MessageID)
	}
}

func TestClientSendImageEncodesBase64(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req sendGroupChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request failed: %v", err)
		}
		if req.Message.Image == nil {
			t.Fatal("expected image body")
		}
		if got := req.Message.Image.Content; got != base64.StdEncoding.EncodeToString([]byte("image")) {
			t.Fatalf("unexpected image content: %s", got)
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(sendGroupChatResponse{
			Code:      0,
			MessageID: "message-2",
		}); err != nil {
			t.Fatalf("encode response failed: %v", err)
		}
	}))
	defer server.Close()

	originalEndpoint := sendGroupChatEndpoint
	sendGroupChatEndpoint = server.URL
	defer func() {
		sendGroupChatEndpoint = originalEndpoint
	}()

	client := NewClient(Config{AppID: "app-id", AppSecret: "app-secret", EmployeeInfoEnabled: true})
	client.httpClient = server.Client()
	client.tokenProvider = func(context.Context, *http.Client, string, string) (string, error) {
		return "token-123", nil
	}

	result, err := client.SendGroupImage(context.Background(), "group-1", ImageMessage{
		Content: []byte("image"),
	}, SendOptions{})
	if err != nil {
		t.Fatalf("send image failed: %v", err)
	}
	if result.MessageID != "message-2" {
		t.Fatalf("unexpected message id: %s", result.MessageID)
	}
}

func TestClientSendFile(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req sendGroupChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request failed: %v", err)
		}
		if req.Message.File == nil {
			t.Fatal("expected file body")
		}
		if req.Message.File.Filename != "demo.txt" {
			t.Fatalf("unexpected filename: %s", req.Message.File.Filename)
		}
		if got := req.Message.File.Content; got != base64.StdEncoding.EncodeToString([]byte("demo file")) {
			t.Fatalf("unexpected file content: %s", got)
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(sendGroupChatResponse{
			Code:      0,
			MessageID: "message-3",
		}); err != nil {
			t.Fatalf("encode response failed: %v", err)
		}
	}))
	defer server.Close()

	originalEndpoint := sendGroupChatEndpoint
	sendGroupChatEndpoint = server.URL
	defer func() {
		sendGroupChatEndpoint = originalEndpoint
	}()

	client := NewClient(Config{AppID: "app-id", AppSecret: "app-secret", EmployeeInfoEnabled: true})
	client.httpClient = server.Client()
	client.tokenProvider = func(context.Context, *http.Client, string, string) (string, error) {
		return "token-123", nil
	}

	result, err := client.SendGroupFile(context.Background(), "group-1", FileMessage{
		Filename: "demo.txt",
		Content:  []byte("demo file"),
	}, SendOptions{
		ThreadID: "thread-1",
	})
	if err != nil {
		t.Fatalf("send file failed: %v", err)
	}
	if result.MessageID != "message-3" {
		t.Fatalf("unexpected message id: %s", result.MessageID)
	}
}

func TestClientSendInteractive(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request failed: %v", err)
		}
		message, ok := req["message"].(map[string]any)
		if !ok {
			t.Fatalf("unexpected message payload type: %T", req["message"])
		}
		if message["tag"] != "interactive_message" {
			t.Fatalf("unexpected message tag: %v", message["tag"])
		}
		interactiveMessage, ok := message["interactive_message"].(map[string]any)
		if !ok {
			t.Fatal("expected interactive message body")
		}
		elements, ok := interactiveMessage["elements"].([]any)
		if !ok {
			t.Fatalf("unexpected elements type: %T", interactiveMessage["elements"])
		}
		if len(elements) != 4 {
			t.Fatalf("unexpected element count: %d", len(elements))
		}

		first, ok := elements[0].(map[string]any)
		if !ok {
			t.Fatalf("unexpected first element type: %T", elements[0])
		}
		if first["element_type"] != "title" {
			t.Fatalf("unexpected first element type value: %v", first["element_type"])
		}

		third, ok := elements[2].(map[string]any)
		if !ok {
			t.Fatalf("unexpected third element type: %T", elements[2])
		}
		if third["element_type"] != "button_group" {
			t.Fatalf("unexpected third element type value: %v", third["element_type"])
		}

		fourth, ok := elements[3].(map[string]any)
		if !ok {
			t.Fatalf("unexpected fourth element type: %T", elements[3])
		}
		image, ok := fourth["image"].(map[string]any)
		if !ok {
			t.Fatalf("unexpected image payload type: %T", fourth["image"])
		}
		if image["content"] != base64.StdEncoding.EncodeToString([]byte("img")) {
			t.Fatalf("unexpected image content: %v", image["content"])
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(sendGroupChatResponse{
			Code:      0,
			MessageID: "message-4",
		}); err != nil {
			t.Fatalf("encode response failed: %v", err)
		}
	}))
	defer server.Close()

	originalEndpoint := sendGroupChatEndpoint
	sendGroupChatEndpoint = server.URL
	defer func() {
		sendGroupChatEndpoint = originalEndpoint
	}()

	client := NewClient(Config{AppID: "app-id", AppSecret: "app-secret", EmployeeInfoEnabled: true})
	client.httpClient = server.Client()
	client.tokenProvider = func(context.Context, *http.Client, string, string) (string, error) {
		return "token-123", nil
	}

	result, err := client.SendGroupInteractive(context.Background(), "group-1", InteractiveMessage{
		Elements: []InteractiveElement{
			InteractiveTitleElement{
				Text: "Interactive Message Title",
			},
			InteractiveDescriptionElement{
				Text:   "Interactive Message Description",
				Format: TextFormatMarkdown,
			},
			InteractiveButtonGroupElement{
				Buttons: []InteractiveButton{
					{
						Type:  InteractiveButtonTypeCallback,
						Text:  "Approve",
						Value: "approve",
					},
					{
						Type: InteractiveButtonTypeRedirect,
						Text: "View details",
						MobileLink: &InteractiveMobileLink{
							Type: InteractiveLinkTypeRN,
							Path: "/webview",
							Params: map[string]string{
								"id": "123",
							},
						},
						DesktopLink: &InteractiveDesktopLink{
							Type: InteractiveLinkTypeWeb,
							Path: "https://example.com/detail/123",
						},
					},
				},
			},
			InteractiveImageElement{
				Content: []byte("img"),
			},
		},
	}, SendOptions{
		ThreadID: "thread-2",
	})
	if err != nil {
		t.Fatalf("send interactive failed: %v", err)
	}
	if result.MessageID != "message-4" {
		t.Fatalf("unexpected message id: %s", result.MessageID)
	}
}

func TestClientSendInteractiveRejectsEmptyElements(t *testing.T) {
	t.Parallel()

	client := NewClient(Config{AppID: "app-id", AppSecret: "app-secret", EmployeeInfoEnabled: true})
	client.tokenProvider = func(context.Context, *http.Client, string, string) (string, error) {
		t.Fatal("token provider should not be called")
		return "", nil
	}

	_, err := client.SendGroupInteractive(context.Background(), "group-1", InteractiveMessage{}, SendOptions{})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestClientSendPrivateText(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("unexpected method: %s", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer token-123" {
			t.Fatalf("unexpected authorization header: %s", got)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Fatalf("unexpected content type: %s", got)
		}

		var req sendSingleChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request failed: %v", err)
		}
		if req.EmployeeCode != "e_12345678" {
			t.Fatalf("unexpected employee code: %s", req.EmployeeCode)
		}
		if req.UsablePlatform != UsablePlatformMobile {
			t.Fatalf("unexpected usable platform: %s", req.UsablePlatform)
		}
		if req.Message.Tag != "text" {
			t.Fatalf("unexpected message tag: %s", req.Message.Tag)
		}
		if req.Message.Text == nil || req.Message.Text.Content != "hello private" {
			t.Fatalf("unexpected text payload: %+v", req.Message.Text)
		}
		if req.Message.ThreadID != "thread-private-1" {
			t.Fatalf("unexpected thread id: %s", req.Message.ThreadID)
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(sendGroupChatResponse{
			Code: 0,
		}); err != nil {
			t.Fatalf("encode response failed: %v", err)
		}
	}))
	defer server.Close()

	originalEndpoint := sendSingleChatEndpoint
	sendSingleChatEndpoint = server.URL
	defer func() {
		sendSingleChatEndpoint = originalEndpoint
	}()

	client := NewClient(Config{AppID: "app-id", AppSecret: "app-secret", EmployeeInfoEnabled: true})
	client.httpClient = server.Client()
	client.tokenProvider = func(context.Context, *http.Client, string, string) (string, error) {
		return "token-123", nil
	}

	result, err := client.SendPrivateText(context.Background(), "e_12345678", TextMessage{
		Content: "hello private",
		Format:  TextFormatPlain,
	}, PrivateSendOptions{
		ThreadID:       "thread-private-1",
		UsablePlatform: UsablePlatformMobile,
	})
	if err != nil {
		t.Fatalf("send private text failed: %v", err)
	}
	if result.MessageID != "" {
		t.Fatalf("unexpected private message id: %s", result.MessageID)
	}
}

func TestClientSendPrivateInteractive(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request failed: %v", err)
		}
		if req["employee_code"] != "e_12345678" {
			t.Fatalf("unexpected employee code: %v", req["employee_code"])
		}
		if req["usable_platform"] != UsablePlatformDesktop {
			t.Fatalf("unexpected usable platform: %v", req["usable_platform"])
		}

		message, ok := req["message"].(map[string]any)
		if !ok {
			t.Fatalf("unexpected message payload type: %T", req["message"])
		}
		if message["tag"] != "interactive_message" {
			t.Fatalf("unexpected message tag: %v", message["tag"])
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(sendGroupChatResponse{
			Code:      0,
			MessageID: "private-message-1",
		}); err != nil {
			t.Fatalf("encode response failed: %v", err)
		}
	}))
	defer server.Close()

	originalEndpoint := sendSingleChatEndpoint
	sendSingleChatEndpoint = server.URL
	defer func() {
		sendSingleChatEndpoint = originalEndpoint
	}()

	client := NewClient(Config{AppID: "app-id", AppSecret: "app-secret"})
	client.httpClient = server.Client()
	client.tokenProvider = func(context.Context, *http.Client, string, string) (string, error) {
		return "token-123", nil
	}

	result, err := client.SendPrivateInteractive(context.Background(), "e_12345678", InteractiveMessage{
		Elements: []InteractiveElement{
			InteractiveTitleElement{Text: "Private Card"},
		},
	}, PrivateSendOptions{
		UsablePlatform: UsablePlatformDesktop,
	})
	if err != nil {
		t.Fatalf("send private interactive failed: %v", err)
	}
	if result.MessageID != "private-message-1" {
		t.Fatalf("unexpected private message id: %s", result.MessageID)
	}
}

func TestClientSendPrivateRejectsInvalidUsablePlatform(t *testing.T) {
	t.Parallel()

	client := NewClient(Config{AppID: "app-id", AppSecret: "app-secret"})
	client.tokenProvider = func(context.Context, *http.Client, string, string) (string, error) {
		t.Fatal("token provider should not be called")
		return "", nil
	}

	_, err := client.SendPrivateText(context.Background(), "e_12345678", TextMessage{
		Content: "hello",
	}, PrivateSendOptions{
		UsablePlatform: "tablet",
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestClientGetEmployeeInfo(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("unexpected method: %s", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer token-123" {
			t.Fatalf("unexpected authorization header: %s", got)
		}

		employeeCodes := r.URL.Query()["employee_code"]
		if len(employeeCodes) != 2 {
			t.Fatalf("unexpected employee code count: %d", len(employeeCodes))
		}
		if employeeCodes[0] != "e_12345678" || employeeCodes[1] != "e_87654321" {
			t.Fatalf("unexpected employee codes: %+v", employeeCodes)
		}

		w.Header().Set("Content-Type", "application/json")
		_, err := w.Write([]byte(`{
			"code": 0,
			"employees": [
				{
					"employee_code": "e_12345678",
					"seatalk_id": "9339874886",
					"seatalk_nickname": "Zhan Peng",
					"avatar": "https://openapi.seatalk.io/file/employee/icon/demo",
					"name": "Zhan Peng",
					"email": "peng.zhan@shopee.com",
					"departments": ["12345"],
					"gender": 1,
					"mobile": "",
					"reporting_manager_employee_code": "0",
					"offboarding_time": "1718074364",
					"custom_fields": [
						{
							"name": "github",
							"type": 1,
							"value": "",
							"link_entry_icons": [],
							"link_entry_text": ""
						}
					]
				},
				{
					"employee_code": "e_87654321",
					"seatalk_id": "8222333444",
					"seatalk_nickname": "Alice",
					"name": "Alice Tan",
					"email": "alice@example.com",
					"departments": ["67890"],
					"gender": 2,
					"offboarding_time": 0
				}
			]
		}`))
		if err != nil {
			t.Fatalf("write response failed: %v", err)
		}
	}))
	defer server.Close()

	originalEndpoint := getEmployeeInfoEndpoint
	getEmployeeInfoEndpoint = server.URL
	t.Cleanup(func() {
		getEmployeeInfoEndpoint = originalEndpoint
	})

	client := NewClient(Config{AppID: "app-id", AppSecret: "app-secret", EmployeeInfoEnabled: true})
	client.httpClient = server.Client()
	client.tokenProvider = func(context.Context, *http.Client, string, string) (string, error) {
		return "token-123", nil
	}

	result, err := client.GetEmployeeInfo(context.Background(), "e_12345678", "e_87654321")
	if err != nil {
		t.Fatalf("get employee info failed: %v", err)
	}
	if len(result.Employees) != 2 {
		t.Fatalf("unexpected employee count: %d", len(result.Employees))
	}
	if result.Employees[0].EmployeeCode != "e_12345678" {
		t.Fatalf("unexpected first employee: %+v", result.Employees[0])
	}
	if result.Employees[0].OffboardingTime != UnixTimestamp(1718074364) {
		t.Fatalf("unexpected offboarding time: %d", result.Employees[0].OffboardingTime)
	}
	if len(result.Employees[0].CustomFields) != 1 {
		t.Fatalf("unexpected custom fields: %+v", result.Employees[0].CustomFields)
	}
}

func TestClientGetEmployeeInfoRejectsInvalidEmployeeCodes(t *testing.T) {
	t.Parallel()

	client := NewClient(Config{AppID: "app-id", AppSecret: "app-secret", EmployeeInfoEnabled: true})
	client.tokenProvider = func(context.Context, *http.Client, string, string) (string, error) {
		t.Fatal("token provider should not be called")
		return "", nil
	}

	if _, err := client.GetEmployeeInfo(context.Background()); err == nil {
		t.Fatal("expected empty employee codes error, got nil")
	}

	if _, err := client.GetEmployeeInfo(context.Background(), "e_12345678", ""); err == nil {
		t.Fatal("expected empty employee code error, got nil")
	}
}

func TestClientGetEmployeeInfoReturnsDisabledErrorWhenFeatureDisabled(t *testing.T) {
	t.Parallel()

	client := NewClient(Config{AppID: "app-id", AppSecret: "app-secret"})
	client.tokenProvider = func(context.Context, *http.Client, string, string) (string, error) {
		t.Fatal("token provider should not be called")
		return "", nil
	}

	if _, err := client.GetEmployeeInfo(context.Background(), "e_12345678"); !errors.Is(err, ErrEmployeeInfoDisabled) {
		t.Fatalf("expected disabled error, got %v", err)
	}
}

func TestClientGetGroupInfo(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("unexpected method: %s", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer token-123" {
			t.Fatalf("unexpected authorization header: %s", got)
		}

		query := r.URL.Query()
		if query.Get("group_id") != "group-1" {
			t.Fatalf("unexpected group id: %s", query.Get("group_id"))
		}
		if query.Get("page_size") != "10" {
			t.Fatalf("unexpected page size: %s", query.Get("page_size"))
		}
		if query.Get("cursor") != "cursor-1" {
			t.Fatalf("unexpected cursor: %s", query.Get("cursor"))
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(getGroupInfoResponse{
			Code:       0,
			NextCursor: "cursor-2",
			Group: GroupInfo{
				GroupName: "Test Group",
				GroupSettings: GroupSettings{
					ChatHistoryForNewMembers: "7 days",
					CanNotifyWithAtAll:       true,
					CanViewMemberList:        true,
				},
				GroupUserTotal:          1,
				GroupBotTotal:           2,
				GroupSystemAccountTotal: 0,
				GroupUserList: []GroupUser{
					{
						SeatalkID:    "12345678",
						EmployeeCode: "e_293847124",
						Email:        "sample@seatalk.biz",
					},
				},
				GroupBotList:           []string{"23456789", "34567890"},
				GroupSystemAccountList: []string{},
			},
		}); err != nil {
			t.Fatalf("encode response failed: %v", err)
		}
	}))
	defer server.Close()

	originalEndpoint := getGroupInfoEndpoint
	getGroupInfoEndpoint = server.URL
	t.Cleanup(func() {
		getGroupInfoEndpoint = originalEndpoint
	})

	client := NewClient(Config{AppID: "app-id", AppSecret: "app-secret"})
	client.httpClient = server.Client()
	client.tokenProvider = func(context.Context, *http.Client, string, string) (string, error) {
		return "token-123", nil
	}

	result, err := client.GetGroupInfo(context.Background(), "group-1", GetGroupInfoOptions{
		PageSize: 10,
		Cursor:   "cursor-1",
	})
	if err != nil {
		t.Fatalf("get group info failed: %v", err)
	}
	if result.NextCursor != "cursor-2" {
		t.Fatalf("unexpected next cursor: %s", result.NextCursor)
	}
	if result.Group.GroupName != "Test Group" {
		t.Fatalf("unexpected group name: %s", result.Group.GroupName)
	}
	if len(result.Group.GroupUserList) != 1 || result.Group.GroupUserList[0].SeatalkID != "12345678" {
		t.Fatalf("unexpected group user list: %+v", result.Group.GroupUserList)
	}
	if len(result.Group.GroupBotList) != 2 {
		t.Fatalf("unexpected group bot list: %+v", result.Group.GroupBotList)
	}
}

func TestClientGetGroupInfoRejectsInvalidPageSize(t *testing.T) {
	t.Parallel()

	client := NewClient(Config{AppID: "app-id", AppSecret: "app-secret"})
	client.tokenProvider = func(context.Context, *http.Client, string, string) (string, error) {
		t.Fatal("token provider should not be called")
		return "", nil
	}

	_, err := client.GetGroupInfo(context.Background(), "group-1", GetGroupInfoOptions{
		PageSize: 101,
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestClientGetGroupInfoAPIError(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(getGroupInfoResponse{
			Code:    10001,
			Message: "invalid group",
		}); err != nil {
			t.Fatalf("encode response failed: %v", err)
		}
	}))
	defer server.Close()

	originalEndpoint := getGroupInfoEndpoint
	getGroupInfoEndpoint = server.URL
	t.Cleanup(func() {
		getGroupInfoEndpoint = originalEndpoint
	})

	client := NewClient(Config{AppID: "app-id", AppSecret: "app-secret"})
	client.httpClient = server.Client()
	client.tokenProvider = func(context.Context, *http.Client, string, string) (string, error) {
		return "token-123", nil
	}

	_, err := client.GetGroupInfo(context.Background(), "group-1", GetGroupInfoOptions{})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "invalid group") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestClientDownload(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("unexpected method: %s", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer token-123" {
			t.Fatalf("unexpected authorization header: %s", got)
		}
		if r.URL.Path != "/messaging/v2/file/demo-file-id" {
			t.Fatalf("unexpected request path: %s", r.URL.Path)
		}

		if _, err := w.Write([]byte("file-bytes")); err != nil {
			t.Fatalf("write response failed: %v", err)
		}
	}))
	defer server.Close()

	originalPrefix := downloadFileURLPrefix
	downloadFileURLPrefix = server.URL + "/messaging/v2/file/"
	t.Cleanup(func() {
		downloadFileURLPrefix = originalPrefix
	})

	client := NewClient(Config{AppID: "app-id", AppSecret: "app-secret"})
	client.httpClient = server.Client()
	client.tokenProvider = func(context.Context, *http.Client, string, string) (string, error) {
		return "token-123", nil
	}

	content, err := client.Download(context.Background(), server.URL+"/messaging/v2/file/demo-file-id")
	if err != nil {
		t.Fatalf("download failed: %v", err)
	}
	if string(content) != "file-bytes" {
		t.Fatalf("unexpected content: %s", string(content))
	}
}

func TestClientDownloadRejectsInvalidURL(t *testing.T) {
	t.Parallel()

	client := NewClient(Config{AppID: "app-id", AppSecret: "app-secret"})
	client.tokenProvider = func(context.Context, *http.Client, string, string) (string, error) {
		t.Fatal("token provider should not be called")
		return "", nil
	}

	if _, err := client.Download(context.Background(), ""); err == nil {
		t.Fatal("expected empty url error, got nil")
	}
	if _, err := client.Download(context.Background(), "https://example.com/file/demo"); err == nil {
		t.Fatal("expected unsupported url error, got nil")
	}
}

func TestClientDownloadUnexpectedStatus(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "expired", http.StatusForbidden)
	}))
	defer server.Close()

	originalPrefix := downloadFileURLPrefix
	downloadFileURLPrefix = server.URL + "/messaging/v2/file/"
	t.Cleanup(func() {
		downloadFileURLPrefix = originalPrefix
	})

	client := NewClient(Config{AppID: "app-id", AppSecret: "app-secret"})
	client.httpClient = server.Client()
	client.tokenProvider = func(context.Context, *http.Client, string, string) (string, error) {
		return "token-123", nil
	}

	_, err := client.Download(context.Background(), server.URL+"/messaging/v2/file/demo-file-id")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "unexpected status 403") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestClientGetPrivateThread(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("unexpected method: %s", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer token-123" {
			t.Fatalf("unexpected authorization header: %s", got)
		}

		query := r.URL.Query()
		if query.Get("employee_code") != "e_12345678" {
			t.Fatalf("unexpected employee code: %s", query.Get("employee_code"))
		}
		if query.Get("thread_id") != "thread-1" {
			t.Fatalf("unexpected thread id: %s", query.Get("thread_id"))
		}
		if query.Get("page_size") != "10" {
			t.Fatalf("unexpected page size: %s", query.Get("page_size"))
		}
		if query.Get("cursor") != "cursor-1" {
			t.Fatalf("unexpected cursor: %s", query.Get("cursor"))
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(getPrivateThreadResponse{
			Code:       0,
			NextCursor: "cursor-2",
			ThreadMessages: []PrivateThreadMessage{
				{
					MessageID:       "message-1",
					QuotedMessageID: "",
					ThreadID:        "thread-1",
					Sender: MessageSender{
						SeatalkID:    "123456789",
						EmployeeCode: "abcdefg",
						Email:        "sample1@seatalk.biz",
						SenderType:   1,
					},
					MessageSentTime: 1687944533,
					Tag:             "text",
					Text: &ThreadTextMessage{
						PlainText: "hello",
					},
				},
			},
		}); err != nil {
			t.Fatalf("encode response failed: %v", err)
		}
	}))
	defer server.Close()

	originalEndpoint := getPrivateThreadEndpoint
	getPrivateThreadEndpoint = server.URL
	t.Cleanup(func() {
		getPrivateThreadEndpoint = originalEndpoint
	})

	client := NewClient(Config{AppID: "app-id", AppSecret: "app-secret"})
	client.httpClient = server.Client()
	client.tokenProvider = func(context.Context, *http.Client, string, string) (string, error) {
		return "token-123", nil
	}

	result, err := client.GetPrivateThread(context.Background(), "e_12345678", "thread-1", GetPrivateThreadOptions{
		PageSize: 10,
		Cursor:   "cursor-1",
	})
	if err != nil {
		t.Fatalf("get private thread failed: %v", err)
	}
	if result.NextCursor != "cursor-2" {
		t.Fatalf("unexpected next cursor: %s", result.NextCursor)
	}
	if len(result.ThreadMessages) != 1 {
		t.Fatalf("unexpected thread message count: %d", len(result.ThreadMessages))
	}
	if result.ThreadMessages[0].Text == nil || result.ThreadMessages[0].Text.PlainText != "hello" {
		t.Fatalf("unexpected text payload: %+v", result.ThreadMessages[0].Text)
	}
}

func TestClientGetPrivateThreadRejectsInvalidPageSize(t *testing.T) {
	t.Parallel()

	client := NewClient(Config{AppID: "app-id", AppSecret: "app-secret"})
	client.tokenProvider = func(context.Context, *http.Client, string, string) (string, error) {
		t.Fatal("token provider should not be called")
		return "", nil
	}

	_, err := client.GetPrivateThread(context.Background(), "e_12345678", "thread-1", GetPrivateThreadOptions{
		PageSize: 101,
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestClientGetMessage(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("unexpected method: %s", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer token-123" {
			t.Fatalf("unexpected authorization header: %s", got)
		}

		query := r.URL.Query()
		if query.Get("message_id") != "message-1" {
			t.Fatalf("unexpected message id: %s", query.Get("message_id"))
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(getMessageResponse{
			Code:            0,
			MessageID:       "message-1",
			QuotedMessageID: "quoted-1",
			ThreadID:        "thread-1",
			Sender: MessageSender{
				SeatalkID:    "123456789",
				EmployeeCode: "abcdefg",
				Email:        "sample1@seatalk.biz",
				SenderType:   1,
			},
			MessageSentTime: 1687944533,
			Tag:             "text",
			Text: &ThreadTextMessage{
				PlainText:      "@User1 hello",
				LastEditedTime: 1710919702,
				MentionedList: []MentionedEntity{
					{
						Username:     "User1",
						SeatalkID:    "seatalk-user-1",
						EmployeeCode: "emp-1",
						Email:        "user1@example.com",
					},
				},
			},
		}); err != nil {
			t.Fatalf("encode response failed: %v", err)
		}
	}))
	defer server.Close()

	originalEndpoint := getMessageEndpoint
	getMessageEndpoint = server.URL
	t.Cleanup(func() {
		getMessageEndpoint = originalEndpoint
	})

	client := NewClient(Config{AppID: "app-id", AppSecret: "app-secret"})
	client.httpClient = server.Client()
	client.tokenProvider = func(context.Context, *http.Client, string, string) (string, error) {
		return "token-123", nil
	}

	result, err := client.GetMessage(context.Background(), "message-1")
	if err != nil {
		t.Fatalf("get message failed: %v", err)
	}
	if result.MessageID != "message-1" {
		t.Fatalf("unexpected message id: %s", result.MessageID)
	}
	if result.QuotedMessageID != "quoted-1" {
		t.Fatalf("unexpected quoted message id: %s", result.QuotedMessageID)
	}
	if result.Text == nil || result.Text.LastEditedTime != 1710919702 {
		t.Fatalf("unexpected text payload: %+v", result.Text)
	}
	if len(result.Text.MentionedList) != 1 {
		t.Fatalf("unexpected mentioned list: %+v", result.Text.MentionedList)
	}
}

func TestClientGetMessageRejectsEmptyMessageID(t *testing.T) {
	t.Parallel()

	client := NewClient(Config{AppID: "app-id", AppSecret: "app-secret"})
	client.tokenProvider = func(context.Context, *http.Client, string, string) (string, error) {
		t.Fatal("token provider should not be called")
		return "", nil
	}

	_, err := client.GetMessage(context.Background(), "")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestClientGetGroupThread(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("unexpected method: %s", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer token-123" {
			t.Fatalf("unexpected authorization header: %s", got)
		}

		query := r.URL.Query()
		if query.Get("group_id") != "group-1" {
			t.Fatalf("unexpected group id: %s", query.Get("group_id"))
		}
		if query.Get("thread_id") != "thread-1" {
			t.Fatalf("unexpected thread id: %s", query.Get("thread_id"))
		}
		if query.Get("page_size") != "10" {
			t.Fatalf("unexpected page size: %s", query.Get("page_size"))
		}
		if query.Get("cursor") != "cursor-1" {
			t.Fatalf("unexpected cursor: %s", query.Get("cursor"))
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(getGroupThreadResponse{
			Code:       0,
			NextCursor: "cursor-2",
			ThreadMessages: []GroupThreadMessage{
				{
					MessageID:       "message-1",
					QuotedMessageID: "",
					ThreadID:        "thread-1",
					Sender: MessageSender{
						SeatalkID:    "123456789",
						EmployeeCode: "abcdefg",
						Email:        "sample1@seatalk.biz",
						SenderType:   1,
					},
					MessageSentTime: 1687944533,
					Tag:             "text",
					Text: &ThreadTextMessage{
						PlainText:      "@User1 Today is Monday",
						LastEditedTime: 1710919702,
						MentionedList: []MentionedEntity{
							{
								Username:     "User1",
								SeatalkID:    "seatalk-user-1",
								EmployeeCode: "emp-1",
								Email:        "user1@example.com",
							},
						},
					},
				},
			},
		}); err != nil {
			t.Fatalf("encode response failed: %v", err)
		}
	}))
	defer server.Close()

	originalEndpoint := getGroupThreadEndpoint
	getGroupThreadEndpoint = server.URL
	t.Cleanup(func() {
		getGroupThreadEndpoint = originalEndpoint
	})

	client := NewClient(Config{AppID: "app-id", AppSecret: "app-secret"})
	client.httpClient = server.Client()
	client.tokenProvider = func(context.Context, *http.Client, string, string) (string, error) {
		return "token-123", nil
	}

	result, err := client.GetGroupThread(context.Background(), "group-1", "thread-1", GetGroupThreadOptions{
		PageSize: 10,
		Cursor:   "cursor-1",
	})
	if err != nil {
		t.Fatalf("get group thread failed: %v", err)
	}
	if result.NextCursor != "cursor-2" {
		t.Fatalf("unexpected next cursor: %s", result.NextCursor)
	}
	if len(result.ThreadMessages) != 1 {
		t.Fatalf("unexpected thread message count: %d", len(result.ThreadMessages))
	}
	if result.ThreadMessages[0].Text == nil || result.ThreadMessages[0].Text.LastEditedTime != 1710919702 {
		t.Fatalf("unexpected text payload: %+v", result.ThreadMessages[0].Text)
	}
	if len(result.ThreadMessages[0].Text.MentionedList) != 1 {
		t.Fatalf("unexpected mentioned list: %+v", result.ThreadMessages[0].Text.MentionedList)
	}
}

func TestClientGetGroupThreadRejectsInvalidPageSize(t *testing.T) {
	t.Parallel()

	client := NewClient(Config{AppID: "app-id", AppSecret: "app-secret"})
	client.tokenProvider = func(context.Context, *http.Client, string, string) (string, error) {
		t.Fatal("token provider should not be called")
		return "", nil
	}

	_, err := client.GetGroupThread(context.Background(), "group-1", "thread-1", GetGroupThreadOptions{
		PageSize: 101,
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestClientUpdateInteractiveMessage(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("unexpected method: %s", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer token-123" {
			t.Fatalf("unexpected authorization header: %s", got)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Fatalf("unexpected content type: %s", got)
		}

		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request failed: %v", err)
		}
		if req["message_id"] != "message-1" {
			t.Fatalf("unexpected message id: %v", req["message_id"])
		}
		message, ok := req["message"].(map[string]any)
		if !ok {
			t.Fatalf("unexpected message payload type: %T", req["message"])
		}
		interactiveMessage, ok := message["interactive_message"].(map[string]any)
		if !ok {
			t.Fatalf("unexpected interactive message payload type: %T", message["interactive_message"])
		}
		elements, ok := interactiveMessage["elements"].([]any)
		if !ok || len(elements) != 1 {
			t.Fatalf("unexpected elements payload: %#v", interactiveMessage["elements"])
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(updateInteractiveMessageResponse{
			Code: 0,
		}); err != nil {
			t.Fatalf("encode response failed: %v", err)
		}
	}))
	defer server.Close()

	originalEndpoint := updateMessageEndpoint
	updateMessageEndpoint = server.URL
	t.Cleanup(func() {
		updateMessageEndpoint = originalEndpoint
	})

	client := NewClient(Config{AppID: "app-id", AppSecret: "app-secret"})
	client.httpClient = server.Client()
	client.tokenProvider = func(context.Context, *http.Client, string, string) (string, error) {
		return "token-123", nil
	}

	err := client.UpdateInteractiveMessage(context.Background(), "message-1", InteractiveMessage{
		Elements: []InteractiveElement{
			InteractiveTitleElement{Text: "Updated Title"},
		},
	})
	if err != nil {
		t.Fatalf("update interactive message failed: %v", err)
	}
}

func TestClientUpdateInteractiveMessageRejectsEmptyFields(t *testing.T) {
	t.Parallel()

	client := NewClient(Config{AppID: "app-id", AppSecret: "app-secret"})
	client.tokenProvider = func(context.Context, *http.Client, string, string) (string, error) {
		t.Fatal("token provider should not be called")
		return "", nil
	}

	if err := client.UpdateInteractiveMessage(context.Background(), "", InteractiveMessage{
		Elements: []InteractiveElement{InteractiveTitleElement{Text: "Updated Title"}},
	}); err == nil {
		t.Fatal("expected empty message id error, got nil")
	}

	if err := client.UpdateInteractiveMessage(context.Background(), "message-1", InteractiveMessage{}); err == nil {
		t.Fatal("expected empty elements error, got nil")
	}
}

func TestClientSetGroupTypingStatus(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("unexpected method: %s", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer token-123" {
			t.Fatalf("unexpected authorization header: %s", got)
		}

		var req setGroupTypingStatusRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request failed: %v", err)
		}
		if req.GroupID != "group-1" {
			t.Fatalf("unexpected group id: %s", req.GroupID)
		}
		if req.ThreadID != "thread-1" {
			t.Fatalf("unexpected thread id: %s", req.ThreadID)
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(setGroupTypingStatusResponse{
			Code: 0,
		}); err != nil {
			t.Fatalf("encode response failed: %v", err)
		}
	}))
	defer server.Close()

	originalEndpoint := groupTypingEndpoint
	groupTypingEndpoint = server.URL
	t.Cleanup(func() {
		groupTypingEndpoint = originalEndpoint
	})

	client := NewClient(Config{AppID: "app-id", AppSecret: "app-secret"})
	client.httpClient = server.Client()
	client.tokenProvider = func(context.Context, *http.Client, string, string) (string, error) {
		return "token-123", nil
	}

	if err := client.SetGroupTypingStatus(context.Background(), "group-1", "thread-1"); err != nil {
		t.Fatalf("set group typing status failed: %v", err)
	}
}

func TestClientSetGroupTypingStatusRejectsEmptyGroupID(t *testing.T) {
	t.Parallel()

	client := NewClient(Config{AppID: "app-id", AppSecret: "app-secret"})
	client.tokenProvider = func(context.Context, *http.Client, string, string) (string, error) {
		t.Fatal("token provider should not be called")
		return "", nil
	}

	if err := client.SetGroupTypingStatus(context.Background(), "", "thread-1"); err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestClientSetPrivateTypingStatus(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("unexpected method: %s", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer token-123" {
			t.Fatalf("unexpected authorization header: %s", got)
		}

		var req setPrivateTypingStatusRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request failed: %v", err)
		}
		if req.EmployeeCode != "e_12345678" {
			t.Fatalf("unexpected employee code: %s", req.EmployeeCode)
		}
		if req.ThreadID != "thread-1" {
			t.Fatalf("unexpected thread id: %s", req.ThreadID)
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(setPrivateTypingStatusResponse{
			Code: 0,
		}); err != nil {
			t.Fatalf("encode response failed: %v", err)
		}
	}))
	defer server.Close()

	originalEndpoint := privateTypingEndpoint
	privateTypingEndpoint = server.URL
	t.Cleanup(func() {
		privateTypingEndpoint = originalEndpoint
	})

	client := NewClient(Config{AppID: "app-id", AppSecret: "app-secret"})
	client.httpClient = server.Client()
	client.tokenProvider = func(context.Context, *http.Client, string, string) (string, error) {
		return "token-123", nil
	}

	if err := client.SetPrivateTypingStatus(context.Background(), "e_12345678", "thread-1"); err != nil {
		t.Fatalf("set private typing status failed: %v", err)
	}
}

func TestClientSetPrivateTypingStatusRejectsEmptyEmployeeCode(t *testing.T) {
	t.Parallel()

	client := NewClient(Config{AppID: "app-id", AppSecret: "app-secret"})
	client.tokenProvider = func(context.Context, *http.Client, string, string) (string, error) {
		t.Fatal("token provider should not be called")
		return "", nil
	}

	if err := client.SetPrivateTypingStatus(context.Background(), "", "thread-1"); err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestClientSendTextRejectsInvalidFormat(t *testing.T) {
	t.Parallel()

	client := NewClient(Config{AppID: "app-id", AppSecret: "app-secret"})
	client.tokenProvider = func(context.Context, *http.Client, string, string) (string, error) {
		t.Fatal("token provider should not be called")
		return "", nil
	}

	_, err := client.SendGroupText(context.Background(), "group-1", TextMessage{
		Content: "hello",
		Format:  99,
	}, SendOptions{})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}
