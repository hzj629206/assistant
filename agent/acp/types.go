package acp

import (
	"context"
	"encoding/json"
	"io"
)

// Session provides one reusable ACP subprocess session.
//
// ScheduleTurn should preempt any previous active turn before publishing the new turn.
type Session interface {
	ScheduleTurn(ctx context.Context, blocks []ContentBlock) (ScheduledTurn, error)
	Interrupt(ctx context.Context) error
	SessionID() string
	Status() SessionMetadata
	Capabilities() AgentCapabilities
	Close() error
}

// ScheduledTurn represents one ACP session turn that can be run or interrupted in either order.
type ScheduledTurn interface {
	Run(ctx context.Context) (TurnResult, error)
	Interrupt(ctx context.Context) error
	Done() <-chan struct{}
}

// TurnResult contains one ACP turn result.
type TurnResult struct {
	SessionID string
	ReplyText string
	RawResult json.RawMessage
}

// SessionOptions configures one ACP subprocess session.
type SessionOptions struct {
	Command         string
	Args            []string
	Env             []string
	WorkingDir      string
	ResumeSessionID string
	AuthMethod      string
	MCPServers      []MCPServer
	Stderr          io.Writer
	Observer        Observer
	Permission      PermissionHandler
}

// SessionMetadata contains ACP session state exposed by the protocol.
type SessionMetadata struct {
	SessionID     string
	Modes         SessionModes
	ConfigOptions json.RawMessage
}

// SessionMode describes one ACP mode returned by the agent.
type SessionMode struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// SessionModes describes the ACP mode state returned by the agent.
type SessionModes struct {
	CurrentModeID  string        `json:"currentModeId"`
	AvailableModes []SessionMode `json:"availableModes"`
}

// MCPServer describes one MCP server passed to the ACP agent.
type MCPServer struct {
	Name    string         `json:"name"`
	Type    string         `json:"type,omitempty"`
	Command string         `json:"command,omitempty"`
	Args    []string       `json:"args,omitempty"`
	Env     []MCPEnvVar    `json:"env,omitempty"`
	URL     string         `json:"url,omitempty"`
	Headers []MCPHeader    `json:"headers,omitempty"`
	Meta    map[string]any `json:"-"`
}

// MCPEnvVar describes one MCP environment variable.
type MCPEnvVar struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// MCPHeader describes one MCP request header.
type MCPHeader struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// ContentBlock is one ACP prompt content block.
type ContentBlock map[string]any

// AgentCapabilities contains ACP agent capability flags.
type AgentCapabilities struct {
	Prompt PromptCapabilities
	MCP    MCPCapabilities
}

// PromptCapabilities contains ACP prompt capability flags.
type PromptCapabilities struct {
	Image           bool
	EmbeddedContext bool
}

// MCPCapabilities contains ACP MCP capability flags.
type MCPCapabilities struct {
	HTTP bool
}

// Observer receives optional runtime callbacks from one ACP session.
type Observer struct {
	OnNotification      func(method string, params json.RawMessage)
	OnSessionUpdate     func(update SessionUpdate)
	OnPermissionRequest func(request PermissionRequest)
	OnPermissionResult  func(request PermissionRequest, decision PermissionDecision)
	OnProcessExit       func(err error)
}

// PermissionHandler decides how to answer ACP permission requests.
type PermissionHandler func(context.Context, PermissionRequest) (PermissionDecision, error)

// PermissionRequest describes one ACP permission request.
type PermissionRequest struct {
	Method    string
	SessionID string
	Options   []PermissionOption
	Raw       json.RawMessage
}

// PermissionOption describes one ACP permission option.
type PermissionOption struct {
	OptionID string `json:"optionId"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
}

// PermissionDecision describes the selected ACP permission outcome.
type PermissionDecision struct {
	Allow    bool
	OptionID string
}

// SessionUpdate describes one ACP session/update notification.
type SessionUpdate struct {
	SessionID     string
	SessionUpdate string
	ContentType   string
	Text          string
	Raw           json.RawMessage
}
