package acp

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

type testHTTPTool struct{}

func (testHTTPTool) Name() string        { return "uppercase" }
func (testHTTPTool) Description() string { return "Uppercase text" }
func (testHTTPTool) InputSchema() any    { return nil }
func (testHTTPTool) OutputSchema() any   { return nil }
func (testHTTPTool) Call(_ context.Context, arguments json.RawMessage) (any, error) {
	return map[string]any{"ok": string(arguments)}, nil
}

func TestHTTPToolServerServesJSONResponse(t *testing.T) {
	token := "token-1"
	server, err := NewHTTPToolServer(context.Background(), HTTPToolServerOptions{
		ServerName:   "assistant",
		IsAuthorized: func(candidate string) bool { return candidate == token },
		Tools:        func(string) []HTTPTool { return []HTTPTool{testHTTPTool{}} },
	})
	if err != nil {
		t.Fatalf("NewHTTPToolServer failed: %v", err)
	}
	defer func() {
		if closeErr := server.Close(); closeErr != nil {
			t.Fatalf("Close failed: %v", closeErr)
		}
	}()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, server.URL(), strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"test","version":"1.0.0"}}}`))
	if err != nil {
		t.Fatalf("NewRequest failed: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do failed: %v", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if contentType := resp.Header.Get("Content-Type"); !strings.Contains(contentType, "application/json") {
		t.Fatalf("unexpected content type: %q", contentType)
	}
	var payload struct {
		Result struct {
			ServerInfo struct {
				Name string `json:"name"`
			} `json:"serverInfo"`
		} `json:"result"`
	}
	if err = json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("Decode failed: %v", err)
	}
	if payload.Result.ServerInfo.Name != "assistant" {
		t.Fatalf("unexpected server name: %q", payload.Result.ServerInfo.Name)
	}
}

func TestHTTPToolServerRejectsMissingAuthorization(t *testing.T) {
	server, err := NewHTTPToolServer(context.Background(), HTTPToolServerOptions{
		IsAuthorized: func(string) bool { return false },
		Tools:        func(string) []HTTPTool { return []HTTPTool{testHTTPTool{}} },
	})
	if err != nil {
		t.Fatalf("NewHTTPToolServer failed: %v", err)
	}
	defer func() {
		if closeErr := server.Close(); closeErr != nil {
			t.Fatalf("Close failed: %v", closeErr)
		}
	}()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, server.URL(), strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"test","version":"1.0.0"}}}`))
	if err != nil {
		t.Fatalf("NewRequest failed: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do failed: %v", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unexpected status: %d", resp.StatusCode)
	}
}
