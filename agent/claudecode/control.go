package claudecode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
)

const defaultProtocolVersion = "2024-11-05"
const defaultServerVersion = "1.0.0"

// MCPToolDefinition describes one MCP tool exposed to Claude.
type MCPToolDefinition struct {
	Name        string
	Description string
	InputSchema any
}

// MCPToolProvider lists tools and executes one tool call.
type MCPToolProvider interface {
	ListTools() []MCPToolDefinition
	CallTool(ctx context.Context, name string, arguments map[string]any) (any, error)
}

// ControlServer implements Claude control requests and MCP tool RPC.
type ControlServer struct {
	ServerName      string
	ServerVersion   string
	ProtocolVersion string
	ToolProvider    MCPToolProvider
}

// HasTools reports whether any MCP tools are exposed.
func (s *ControlServer) HasTools() bool {
	if s == nil || s.ToolProvider == nil {
		return false
	}
	return len(s.ToolProvider.ListTools()) != 0
}

// BuildMCPConfigJSON builds one SDK MCP config document for Claude CLI.
func (s *ControlServer) BuildMCPConfigJSON() (string, error) {
	serverName := s.serverName()

	builder := NewMCPConfigBuilder()
	configJSON, err := builder.AddServer(serverName, MCPServerConfig{
		Type: "sdk",
		URL:  "",
	}).BuildJSON()
	if err != nil {
		return "", fmt.Errorf("build claude mcp config failed: %w", err)
	}

	var config map[string]any
	if err = json.Unmarshal([]byte(configJSON), &config); err != nil {
		return "", fmt.Errorf("decode claude mcp config failed: %w", err)
	}
	mcpServers, ok := config["mcpServers"].(map[string]any)
	if !ok {
		return "", errors.New("claude mcp config missing mcpServers")
	}
	serverConfig, ok := mcpServers[serverName].(map[string]any)
	if !ok {
		return "", fmt.Errorf("claude mcp config missing server %q", serverName)
	}
	serverConfig["name"] = serverName

	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encode claude mcp config failed: %w", err)
	}

	return string(data), nil
}

// HandleRequest handles one Claude control request.
func (s *ControlServer) HandleRequest(ctx context.Context, request map[string]any) (map[string]any, error) {
	subtype, _ := request["subtype"].(string)
	switch subtype {
	case "initialize":
		return map[string]any{}, nil
	case "can_use_tool":
		originalInput, _ := request["input"].(map[string]any)
		return map[string]any{
			"behavior":     "allow",
			"updatedInput": originalInput,
		}, nil
	case "mcp_message":
		serverName, _ := request["server_name"].(string)
		if serverName != s.serverName() {
			return nil, fmt.Errorf("unsupported SDK MCP server %q", serverName)
		}
		message, _ := request["message"].(map[string]any)
		if message == nil {
			return nil, errors.New("missing SDK MCP message payload")
		}

		return map[string]any{
			"mcp_response": s.handleMCPMessage(ctx, message),
		}, nil
	default:
		return nil, fmt.Errorf("unsupported control request subtype %q", subtype)
	}
}

func (s *ControlServer) handleMCPMessage(ctx context.Context, message map[string]any) map[string]any {
	id := message["id"]
	method, _ := message["method"].(string)
	params, _ := message["params"].(map[string]any)

	switch method {
	case "initialize":
		return map[string]any{
			"jsonrpc": "2.0",
			"id":      id,
			"result": map[string]any{
				"protocolVersion": s.protocolVersion(),
				"capabilities": map[string]any{
					"tools": map[string]any{},
				},
				"serverInfo": map[string]any{
					"name":    s.serverName(),
					"version": s.serverVersion(),
				},
			},
		}
	case "tools/list":
		return map[string]any{
			"jsonrpc": "2.0",
			"id":      id,
			"result": map[string]any{
				"tools": s.listMCPTools(),
			},
		}
	case "tools/call":
		toolName, _ := params["name"].(string)
		arguments, _ := params["arguments"].(map[string]any)
		return s.callMCPTool(ctx, id, toolName, arguments)
	case "notifications/initialized":
		return map[string]any{
			"jsonrpc": "2.0",
			"result":  map[string]any{},
		}
	default:
		return map[string]any{
			"jsonrpc": "2.0",
			"id":      id,
			"error": map[string]any{
				"code":    -32601,
				"message": fmt.Sprintf("method %q not found", method),
			},
		}
	}
}

func (s *ControlServer) listMCPTools() []map[string]any {
	if s == nil || s.ToolProvider == nil {
		return nil
	}

	toolDefs := append([]MCPToolDefinition(nil), s.ToolProvider.ListTools()...)
	slices.SortFunc(toolDefs, func(a, b MCPToolDefinition) int {
		if a.Name < b.Name {
			return -1
		}
		if a.Name > b.Name {
			return 1
		}
		return 0
	})

	tools := make([]map[string]any, 0, len(toolDefs))
	for _, tool := range toolDefs {
		inputSchema := tool.InputSchema
		if inputSchema == nil {
			inputSchema = map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			}
		}
		tools = append(tools, map[string]any{
			"name":        tool.Name,
			"description": tool.Description,
			"inputSchema": inputSchema,
		})
	}

	return tools
}

func (s *ControlServer) callMCPTool(ctx context.Context, id any, toolName string, arguments map[string]any) map[string]any {
	if s == nil || s.ToolProvider == nil {
		return map[string]any{
			"jsonrpc": "2.0",
			"id":      id,
			"error": map[string]any{
				"code":    -32601,
				"message": fmt.Sprintf("tool %q not found", toolName),
			},
		}
	}

	result, err := s.ToolProvider.CallTool(ctx, toolName, arguments)
	content := make([]map[string]any, 0, 1)
	response := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
	}
	if err != nil {
		content = append(content, map[string]any{
			"type": "text",
			"text": err.Error(),
		})
		response["result"] = map[string]any{
			"content": content,
			"isError": true,
		}
		return response
	}

	text, err := FormatMCPToolResult(result)
	if err != nil {
		return map[string]any{
			"jsonrpc": "2.0",
			"id":      id,
			"error": map[string]any{
				"code":    -32603,
				"message": fmt.Sprintf("encode tool result failed: %v", err),
			},
		}
	}
	content = append(content, map[string]any{
		"type": "text",
		"text": text,
	})
	response["result"] = map[string]any{
		"content": content,
	}
	return response
}

// FormatMCPToolResult converts one tool result payload to the Claude MCP text format.
func FormatMCPToolResult(result any) (string, error) {
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

func (s *ControlServer) serverName() string {
	if s == nil || s.ServerName == "" {
		return "claude-sdk"
	}
	return s.ServerName
}

func (s *ControlServer) serverVersion() string {
	if s == nil || s.ServerVersion == "" {
		return defaultServerVersion
	}
	return s.ServerVersion
}

func (s *ControlServer) protocolVersion() string {
	if s == nil || s.ProtocolVersion == "" {
		return defaultProtocolVersion
	}
	return s.ProtocolVersion
}
