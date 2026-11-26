package acp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os/exec"
	"path/filepath"
)

type implementationInfo struct {
	Name    string `json:"name"`
	Title   string `json:"title,omitempty"`
	Version string `json:"version"`
}

type authMethod struct {
	ID       string `json:"id"`
	MethodID string `json:"methodId"`
	Type     string `json:"type"`
}

type initialization struct {
	version     int
	info        implementationInfo
	authMethods []authMethod
	caps        AgentCapabilities
	loadSession bool
	mcpStdio    bool
}

func (s *processSession) negotiatedVersion() int {
	s.sessionMu.RLock()
	defer s.sessionMu.RUnlock()
	return s.protocolVersion
}

func (s *processSession) initialize(ctx context.Context) (initialization, error) {
	info := implementationInfo{Name: defaultClientName, Version: defaultClientVersion}
	requestedVersion := defaultProtocolVersion
	log.Printf("acp initialize started: requested_protocol_version=%d", defaultProtocolVersion)
	result, err := s.transport.call(ctx, "initialize", map[string]any{
		"protocolVersion": defaultProtocolVersion,
		"info":            info,
		"capabilities":    map[string]any{},
	})
	// Some legacy v1 agents reject a newer version instead of selecting v1.
	// A failed initialize leaves the connection uninitialized, so retry only
	// explicit parameter/version errors, never authentication or transport errors.
	var rpcErr *rpcError
	if errors.As(err, &rpcErr) && (rpcErr.Code == -32602 || rpcErr.Code == -32022) {
		requestedVersion = 1
		log.Printf("acp initialize retrying legacy protocol: err=%v", err)
		result, err = s.transport.call(ctx, "initialize", map[string]any{
			"protocolVersion":    1,
			"clientInfo":         info,
			"clientCapabilities": map[string]any{},
		})
	}
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return initialization{}, fmt.Errorf("acp initialize failed: timed out after %s waiting for agent response", processHandshakeTimeout)
		}
		return initialization{}, fmt.Errorf("acp initialize failed: %w", err)
	}
	initialized, err := decodeInitialization(result)
	if err != nil {
		return initialization{}, err
	}
	if initialized.version > requestedVersion {
		return initialization{}, fmt.Errorf("acp agent selected protocol version %d above requested version %d", initialized.version, requestedVersion)
	}
	s.sessionMu.Lock()
	s.protocolVersion = initialized.version
	s.caps = initialized.caps
	s.status.ProtocolVersion = initialized.version
	s.sessionMu.Unlock()
	s.transport.strict.Store(initialized.version == 2)
	log.Printf(
		"acp initialize completed: protocol_version=%d agent_name=%s agent_title=%s agent_version=%s load_session=%t prompt_image=%t prompt_embedded_context=%t mcp_http=%t auth_methods=%d",
		initialized.version, sanitizeLogValue(initialized.info.Name), sanitizeLogValue(initialized.info.Title),
		sanitizeLogValue(initialized.info.Version), initialized.loadSession, initialized.caps.Prompt.Image,
		initialized.caps.Prompt.EmbeddedContext, initialized.caps.MCP.HTTP, len(initialized.authMethods),
	)
	return initialized, nil
}

func decodeInitialization(result json.RawMessage) (initialization, error) {
	var response struct {
		ProtocolVersion   int                `json:"protocolVersion"`
		Info              implementationInfo `json:"info"`
		AgentInfo         implementationInfo `json:"agentInfo"`
		AuthMethods       []authMethod       `json:"authMethods"`
		AgentCapabilities struct {
			LoadSession bool `json:"loadSession"`
			Prompt      struct {
				Image           bool `json:"image"`
				EmbeddedContext bool `json:"embeddedContext"`
			} `json:"promptCapabilities"`
			MCP struct {
				HTTP bool `json:"http"`
			} `json:"mcpCapabilities"`
		} `json:"agentCapabilities"`
		Capabilities struct {
			Session *struct {
				Prompt struct {
					Image           json.RawMessage `json:"image"`
					EmbeddedContext json.RawMessage `json:"embeddedContext"`
				} `json:"prompt"`
				MCP struct {
					HTTP  json.RawMessage `json:"http"`
					Stdio json.RawMessage `json:"stdio"`
				} `json:"mcp"`
			} `json:"session"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(result, &response); err != nil {
		return initialization{}, fmt.Errorf("decode acp initialize response failed: %w", err)
	}
	initialized := initialization{version: response.ProtocolVersion, authMethods: response.AuthMethods}
	switch response.ProtocolVersion {
	case 1:
		initialized.info = response.AgentInfo
		initialized.loadSession = response.AgentCapabilities.LoadSession
		initialized.caps.Prompt.Image = response.AgentCapabilities.Prompt.Image
		initialized.caps.Prompt.EmbeddedContext = response.AgentCapabilities.Prompt.EmbeddedContext
		initialized.caps.MCP.HTTP = response.AgentCapabilities.MCP.HTTP
	case 2:
		initialized.info = response.Info
		if response.Info.Name == "" || response.Info.Version == "" {
			return initialization{}, errors.New("acp v2 initialize response is missing required info.name or info.version")
		}
		capabilities := response.Capabilities.Session
		if capabilities == nil {
			return initialization{}, errors.New("acp v2 agent does not advertise session support")
		}
		initialized.loadSession = true
		initialized.caps.Prompt.Image = objectCapability(capabilities.Prompt.Image)
		initialized.caps.Prompt.EmbeddedContext = objectCapability(capabilities.Prompt.EmbeddedContext)
		initialized.caps.MCP.HTTP = objectCapability(capabilities.MCP.HTTP)
		initialized.mcpStdio = objectCapability(capabilities.MCP.Stdio)
		for i := range initialized.authMethods {
			initialized.authMethods[i].ID = initialized.authMethods[i].MethodID
		}
	default:
		return initialization{}, fmt.Errorf("acp agent selected unsupported protocol version %d (supported: 1, 2)", response.ProtocolVersion)
	}
	return initialized, nil
}

func objectCapability(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && trimmed[0] == '{'
}

func (i initialization) validateAuthMethod(id string) error {
	for _, method := range i.authMethods {
		if method.ID == id {
			if method.Type != "agent" {
				return fmt.Errorf("acp auth method %q has unsupported type %q; use an agent-handled login or authenticate the configured CLI separately", id, method.Type)
			}
			return nil
		}
	}
	return fmt.Errorf("acp agent does not advertise auth method %q", id)
}

func (i initialization) prepareMCPServers(servers []MCPServer) ([]MCPServer, error) {
	prepared := make([]MCPServer, len(servers))
	copy(prepared, servers)
	if i.version != 2 {
		return prepared, nil
	}
	for index := range prepared {
		server := &prepared[index]
		switch server.Type {
		case "http":
			if !i.caps.MCP.HTTP {
				return nil, errors.New("acp v2 agent does not advertise HTTP MCP support")
			}
		case "", "stdio":
			if !i.mcpStdio {
				return nil, errors.New("acp v2 agent does not advertise stdio MCP support")
			}
			server.Type = "stdio"
			if !filepath.IsAbs(server.Command) {
				command, err := exec.LookPath(server.Command)
				if err != nil {
					return nil, fmt.Errorf("resolve ACP MCP command %q: %w", server.Command, err)
				}
				server.Command, err = filepath.Abs(command)
				if err != nil {
					return nil, fmt.Errorf("resolve absolute ACP MCP command: %w", err)
				}
			}
		default:
			return nil, fmt.Errorf("unsupported ACP v2 MCP transport %q", server.Type)
		}
	}
	return prepared, nil
}
