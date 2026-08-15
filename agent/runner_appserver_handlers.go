package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"

	appcodex "github.com/pmenglund/codex-sdk-go"
	appproto "github.com/pmenglund/codex-sdk-go/protocol"
)

func newAppServerRequestCallbacks(runner *AppServerRunner) *appcodex.ServerRequestCallbacks {
	if runner == nil {
		return &appcodex.ServerRequestCallbacks{}
	}

	return &appcodex.ServerRequestCallbacks{
		RefreshAuthTokens:           runner.AccountChatgptAuthTokensRefresh,
		ApprovePatch:                runner.ApplyPatchApproval,
		GenerateAttestation:         runner.AttestationGenerate,
		ApproveCommand:              runner.ExecCommandApproval,
		ApproveCommandExecution:     runner.ItemCommandExecutionRequestApproval,
		ApproveFileChange:           runner.ItemFileChangeRequestApproval,
		ApprovePermissions:          runner.ItemPermissionsRequestApproval,
		CallTool:                    runner.ItemToolCall,
		RequestUserInput:            runner.ItemToolRequestUserInput,
		RequestMCPServerElicitation: runner.MCPServerElicitationRequest,
	}
}

func (*AppServerRunner) AccountChatgptAuthTokensRefresh(context.Context, appproto.ChatgptAuthTokensRefreshParams) (*appproto.ChatgptAuthTokensRefreshResponse, error) {
	return nil, errors.New("chatgpt auth token refresh is not configured")
}

func (*AppServerRunner) AttestationGenerate(context.Context, appproto.AttestationGenerateParams) (*appproto.AttestationGenerateResponse, error) {
	return nil, errors.New("attestation generation is not configured")
}

func (*AppServerRunner) ApplyPatchApproval(_ context.Context, params appproto.ApplyPatchApprovalParams) (*appproto.ApplyPatchApprovalResponse, error) {
	logAppServerRequest("auto-approved apply patch request", params)
	response := appproto.SanitizedApplyPatchApprovalResponseJSON{Decision: appproto.MustReviewDecision("approved")}
	return &response, nil
}

func (*AppServerRunner) ExecCommandApproval(_ context.Context, params appproto.ExecCommandApprovalParams) (*appproto.ExecCommandApprovalResponse, error) {
	logAppServerRequest("auto-approved exec command request", params)
	response := appproto.SanitizedExecCommandApprovalResponseJSON{Decision: appproto.MustReviewDecision("approved")}
	return &response, nil
}

func (*AppServerRunner) ItemCommandExecutionRequestApproval(_ context.Context, params appproto.CommandExecutionRequestApprovalParams) (*appproto.CommandExecutionRequestApprovalResponse, error) {
	logAppServerRequest("auto-approved command execution request", params)
	response := appproto.CommandExecutionRequestApprovalResponse{Decision: appproto.MustCommandExecutionApprovalDecision("accept")}
	return &response, nil
}

func (*AppServerRunner) ItemFileChangeRequestApproval(_ context.Context, params appproto.FileChangeRequestApprovalParams) (*appproto.FileChangeRequestApprovalResponse, error) {
	logAppServerRequest("auto-approved file change request", params)
	response := appproto.SanitizedFileChangeRequestApprovalResponseJSON{Decision: "accept"}
	return &response, nil
}

func (*AppServerRunner) ItemPermissionsRequestApproval(_ context.Context, params appproto.PermissionsRequestApprovalParams) (*appproto.PermissionsRequestApprovalResponse, error) {
	logAppServerRequest("auto-approved permissions request", params)
	response := appproto.PermissionsRequestApprovalResponse{Permissions: params.Permissions}
	return &response, nil
}

func (*AppServerRunner) ItemToolRequestUserInput(context.Context, appproto.ToolRequestUserInputParams) (*appproto.ToolRequestUserInputResponse, error) {
	return nil, errors.New("tool user input is not configured")
}

func (*AppServerRunner) MCPServerElicitationRequest(_ context.Context, params appproto.MCPServerElicitationRequestParams) (*appproto.MCPServerElicitationRequestResponse, error) {
	if shouldAutoAcceptMCPToolApproval(params) {
		logAppServerRequest("auto-accepted MCP tool approval elicitation request", params)
		response := appproto.MCPServerElicitationRequestResponse{
			Action:  appproto.MCPServerElicitationActionAccept,
			Content: map[string]any{},
		}
		return &response, nil
	}

	logAppServerRequest("declined MCP elicitation request because no interactive elicitation handler is configured", params)
	response := appproto.MCPServerElicitationRequestResponse{
		Action: appproto.MCPServerElicitationActionDecline,
	}
	return &response, nil
}

func logAppServerRequest(message string, params any) {
	payload, err := json.Marshal(params)
	if err != nil {
		log.Printf("app-server runner %s: marshal_params_err=%v", message, err)
		return
	}
	log.Printf("app-server runner %s: params=%s", message, string(payload))
}

func shouldAutoAcceptMCPToolApproval(params appproto.MCPServerElicitationRequestParams) bool {
	var root map[string]any
	if err := json.Unmarshal(params, &root); err != nil {
		return false
	}

	meta, ok := root["_meta"].(map[string]any)
	if !ok {
		return false
	}
	approvalKind, ok := meta["codex_approval_kind"].(string)
	if !ok || approvalKind != "mcp_tool_call" {
		return false
	}

	requestedSchema, ok := root["requestedSchema"].(map[string]any)
	if !ok {
		return false
	}
	properties, ok := requestedSchema["properties"].(map[string]any)
	if !ok {
		return false
	}

	return len(properties) == 0
}

type appServerDynamicToolCallParams struct {
	ThreadID  string `json:"threadId"`
	TurnID    string `json:"turnId"`
	CallID    string `json:"callId"`
	Tool      string `json:"tool"`
	Arguments any    `json:"arguments"`
}

func (r *AppServerRunner) ItemToolCall(ctx context.Context, params appproto.DynamicToolCallParams) (*appproto.DynamicToolCallResponse, error) {
	if r == nil {
		return nil, errors.New("app-server runner is nil")
	}

	decoded, err := decodeDynamicToolCallParams(params)
	if err != nil {
		return nil, err
	}

	session, ok := r.findSessionForTurn(decoded.ThreadID, decoded.TurnID)
	if !ok {
		if decoded.TurnID == "" {
			return nil, fmt.Errorf("tool call for unknown active thread %q", decoded.ThreadID)
		}
		return nil, fmt.Errorf("tool call for unknown active turn %q on thread %q", decoded.TurnID, decoded.ThreadID)
	}
	return session.handleItemToolCall(ctx, decoded)
}

func decodeDynamicToolCallParams(value appproto.DynamicToolCallParams) (appServerDynamicToolCallParams, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return appServerDynamicToolCallParams{}, fmt.Errorf("encode dynamic tool params failed: %w", err)
	}

	var params appServerDynamicToolCallParams
	if err := json.Unmarshal(data, &params); err != nil {
		return appServerDynamicToolCallParams{}, fmt.Errorf("decode dynamic tool params failed: %w", err)
	}
	if params.ThreadID == "" {
		return appServerDynamicToolCallParams{}, errors.New("dynamic tool params missing threadId")
	}
	if params.Tool == "" {
		return appServerDynamicToolCallParams{}, errors.New("dynamic tool params missing tool")
	}
	return params, nil
}

func buildDynamicToolContentItems(result any, callErr error) ([]appproto.DynamicToolCallOutputContentItem, error) {
	if callErr != nil {
		return dynamicToolErrorContentItems(callErr), nil
	}

	switch value := result.(type) {
	case nil:
		return dynamicToolTextContentItems("OK")
	case string:
		return dynamicToolTextContentItems(value)
	case []byte:
		return dynamicToolTextContentItems(string(value))
	case json.RawMessage:
		return dynamicToolTextContentItems(string(value))
	default:
		payload, err := json.Marshal(value)
		if err != nil {
			return nil, fmt.Errorf("encode tool result failed: %w", err)
		}
		return dynamicToolTextContentItems(string(payload))
	}
}

func dynamicToolErrorContentItems(err error) []appproto.DynamicToolCallOutputContentItem {
	items, _ := dynamicToolTextContentItems(err.Error())
	return items
}

func dynamicToolTextContentItems(text string) ([]appproto.DynamicToolCallOutputContentItem, error) {
	var item appproto.DynamicToolCallOutputContentItem
	if err := json.Unmarshal([]byte(`{"type":"inputText","text":`+strconv.Quote(text)+`}`), &item); err != nil {
		return nil, fmt.Errorf("encode dynamic tool text result: %w", err)
	}
	return []appproto.DynamicToolCallOutputContentItem{item}, nil
}
