package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const defaultHTTPReadHeaderTimeout = 5 * time.Second

// HTTPTool describes one tool exposed through the ACP HTTP MCP bridge.
type HTTPTool interface {
	Name() string
	Description() string
	InputSchema() any
	OutputSchema() any
	Call(ctx context.Context, arguments json.RawMessage) (any, error)
}

// HTTPToolServerOptions configures one ACP HTTP MCP bridge server.
type HTTPToolServerOptions struct {
	ServerName    string
	ServerVersion string
	IsAuthorized  func(token string) bool
	Tools         func(token string) []HTTPTool
}

// HTTPToolServer serves tools over streamable HTTP MCP for ACP agents.
type HTTPToolServer struct {
	listener  net.Listener
	server    *http.Server
	url       string
	name      string
	version   string
	closeOnce sync.Once

	isAuthorized func(string) bool
	tools        func(string) []HTTPTool
}

type httpToolTokenContextKey struct{}

// NewHTTPToolServer starts one ACP HTTP MCP bridge server.
func NewHTTPToolServer(ctx context.Context, options HTTPToolServerOptions) (*HTTPToolServer, error) {
	if options.IsAuthorized == nil {
		return nil, errors.New("acp http tool server authorization callback is nil")
	}
	if options.Tools == nil {
		return nil, errors.New("acp http tool server tool provider is nil")
	}

	serverName := strings.TrimSpace(options.ServerName)
	if serverName == "" {
		serverName = "assistant"
	}
	serverVersion := strings.TrimSpace(options.ServerVersion)
	if serverVersion == "" {
		serverVersion = "1.0.0"
	}

	var listenConfig net.ListenConfig
	listener, err := listenConfig.Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen on tool server failed: %w", err)
	}

	server := &HTTPToolServer{
		listener:     listener,
		name:         serverName,
		version:      serverVersion,
		isAuthorized: options.IsAuthorized,
		tools:        options.Tools,
	}

	handler := mcp.NewStreamableHTTPHandler(func(request *http.Request) *mcp.Server {
		return server.newRequestServer(request)
	}, &mcp.StreamableHTTPOptions{
		Stateless:    true,
		JSONResponse: true,
	})
	server.server = &http.Server{
		Handler: server.wrapAuth(handler),
		BaseContext: func(net.Listener) context.Context {
			return ctx
		},
		ReadHeaderTimeout: defaultHTTPReadHeaderTimeout,
	}
	server.url = "http://" + listener.Addr().String() + "/mcp"

	go func() {
		if serveErr := server.server.Serve(listener); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			log.Printf("acp tool server stopped: err=%v", serveErr)
		}
	}()
	return server, nil
}

// URL returns the MCP endpoint URL.
func (s *HTTPToolServer) URL() string {
	if s == nil {
		return ""
	}
	return s.url
}

// ServerConfig builds one ACP MCP server definition for the given bearer token.
func (s *HTTPToolServer) ServerConfig(token string) MCPServer {
	return MCPServer{
		Name: s.name,
		Type: "http",
		URL:  s.url,
		Headers: []MCPHeader{
			{Name: "Authorization", Value: "Bearer " + token},
			{Name: "Accept", Value: "application/json, text/event-stream"},
		},
	}
}

func (s *HTTPToolServer) wrapAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mcp" {
			http.NotFound(w, r)
			return
		}
		token, ok := bearerToken(r.Header.Get("Authorization"))
		if !ok || !s.isAuthorized(token) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if accept := r.Header.Get("Accept"); accept != "" && !strings.Contains(accept, "application/json") {
			http.Error(w, "accept must contain application/json", http.StatusBadRequest)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), httpToolTokenContextKey{}, token)))
	})
}

func bearerToken(authorization string) (string, bool) {
	authorization = strings.TrimSpace(authorization)
	if !strings.HasPrefix(authorization, "Bearer ") {
		return "", false
	}
	token := strings.TrimSpace(strings.TrimPrefix(authorization, "Bearer "))
	if token == "" {
		return "", false
	}
	return token, true
}

func (s *HTTPToolServer) newRequestServer(request *http.Request) *mcp.Server {
	token, _ := request.Context().Value(httpToolTokenContextKey{}).(string)
	server := mcp.NewServer(&mcp.Implementation{
		Name:    s.name,
		Version: s.version,
	}, nil)
	for _, tool := range s.currentTools(token) {
		if tool == nil {
			continue
		}
		server.AddTool(&mcp.Tool{
			Name:         tool.Name(),
			Description:  tool.Description(),
			InputSchema:  normalizeToolSchema(tool.InputSchema()),
			OutputSchema: normalizeToolSchema(tool.OutputSchema()),
		}, s.handleToolCall(tool))
	}
	return server
}

func (s *HTTPToolServer) currentTools(token string) []HTTPTool {
	if s == nil || s.tools == nil {
		return nil
	}
	return s.tools(token)
}

func (s *HTTPToolServer) handleToolCall(tool HTTPTool) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		arguments := req.Params.Arguments
		if len(arguments) == 0 {
			arguments = []byte("{}")
		}

		result, toolErr := tool.Call(ctx, arguments)
		if toolErr != nil {
			return toolCallErrorResult(toolErr.Error()), nil
		}

		text, err := formatToolResult(result)
		if err != nil {
			return nil, err
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{Text: text},
			},
		}, nil
	}
}

func toolCallErrorResult(message string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: message},
		},
		IsError: true,
	}
}

func normalizeToolSchema(schema any) any {
	if schema == nil {
		return json.RawMessage(`{"type":"object","properties":{}}`)
	}
	return schema
}

func formatToolResult(result any) (string, error) {
	if result == nil {
		return "null", nil
	}
	if text, ok := result.(string); ok {
		return text, nil
	}
	data, err := json.Marshal(result)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// Close shuts down the HTTP MCP bridge server.
func (s *HTTPToolServer) Close() error {
	if s == nil {
		return nil
	}
	var err error
	s.closeOnce.Do(func() {
		if s.server != nil {
			err = s.server.Close()
		}
	})
	return err
}
