package codex

import (
	"encoding/json"
)

// Usage describes token usage for a turn.
type Usage struct {
	InputTokens       int `json:"input_tokens"`
	CachedInputTokens int `json:"cached_input_tokens"`
	OutputTokens      int `json:"output_tokens"`
}

// ThreadError indicates a failure in the stream.
type ThreadError struct {
	Message string `json:"message"`
}

// ThreadEvent represents a top-level JSONL event.
type ThreadEvent struct {
	Type     string       `json:"type"`
	ThreadID string       `json:"thread_id,omitempty"`
	Usage    *Usage       `json:"usage,omitempty"`
	Error    *ThreadError `json:"error,omitempty"`
	Item     ThreadItem   `json:"item,omitempty"`
	Message  string       `json:"message,omitempty"`
}

func (e *ThreadEvent) UnmarshalJSON(data []byte) error {
	var aux struct {
		Type     string          `json:"type"`
		ThreadID string          `json:"thread_id,omitempty"`
		Usage    *Usage          `json:"usage,omitempty"`
		Error    *ThreadError    `json:"error,omitempty"`
		Item     json.RawMessage `json:"item,omitempty"`
		Message  string          `json:"message,omitempty"`
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	e.Type = aux.Type
	e.ThreadID = aux.ThreadID
	e.Usage = aux.Usage
	e.Error = aux.Error
	e.Message = aux.Message
	if len(aux.Item) != 0 {
		item, err := parseThreadItem(aux.Item)
		if err != nil {
			return err
		}
		e.Item = item
	}
	return nil
}

// ThreadItem is a union of item payloads emitted by Codex.
type ThreadItem interface {
	ItemType() string
}

// UnknownItem preserves forward-compatible item payloads emitted by newer Codex CLIs.
type UnknownItem struct {
	Type string          `json:"type"`
	Raw  json.RawMessage `json:"raw"`
}

func (i *UnknownItem) ItemType() string { return i.Type }

// AgentMessageItem is a response from the agent.
type AgentMessageItem struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	Text string `json:"text"`
}

func (i *AgentMessageItem) ItemType() string { return i.Type }

// ReasoningItem captures the agent's reasoning summary.
type ReasoningItem struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	Text string `json:"text"`
}

func (i *ReasoningItem) ItemType() string { return i.Type }

// CommandExecutionStatus is the status of a command execution.
type CommandExecutionStatus string

const (
	CommandExecutionInProgress CommandExecutionStatus = "in_progress"
	CommandExecutionCompleted  CommandExecutionStatus = "completed"
	CommandExecutionFailed     CommandExecutionStatus = "failed"
)

// CommandExecutionItem is a command executed by the agent.
type CommandExecutionItem struct {
	ID               string                 `json:"id"`
	Type             string                 `json:"type"`
	Command          string                 `json:"command"`
	AggregatedOutput string                 `json:"aggregated_output"`
	ExitCode         *int                   `json:"exit_code,omitempty"`
	Status           CommandExecutionStatus `json:"status"`
}

func (i *CommandExecutionItem) ItemType() string { return i.Type }

// PatchChangeKind indicates a file change type.
type PatchChangeKind string

const (
	PatchChangeAdd    PatchChangeKind = "add"
	PatchChangeDelete PatchChangeKind = "delete"
	PatchChangeUpdate PatchChangeKind = "update"
)

// FileUpdateChange describes a file change.
type FileUpdateChange struct {
	Path string          `json:"path"`
	Kind PatchChangeKind `json:"kind"`
}

// PatchApplyStatus is the status of a file change patch.
type PatchApplyStatus string

const (
	PatchApplyCompleted PatchApplyStatus = "completed"
	PatchApplyFailed    PatchApplyStatus = "failed"
)

// FileChangeItem describes a set of file changes.
type FileChangeItem struct {
	ID      string             `json:"id"`
	Type    string             `json:"type"`
	Changes []FileUpdateChange `json:"changes"`
	Status  PatchApplyStatus   `json:"status"`
}

func (i *FileChangeItem) ItemType() string { return i.Type }

// McpToolCallStatus is the status of an MCP tool call.
type McpToolCallStatus string

const (
	McpToolCallInProgress McpToolCallStatus = "in_progress"
	McpToolCallCompleted  McpToolCallStatus = "completed"
	McpToolCallFailed     McpToolCallStatus = "failed"
)

// McpToolCallResult holds a successful MCP tool response.
type McpToolCallResult struct {
	Content           []map[string]any `json:"content,omitempty"`
	StructuredContent any              `json:"structured_content,omitempty"`
}

// McpToolCallError is an MCP tool error payload.
type McpToolCallError struct {
	Message string `json:"message"`
}

// McpToolCallItem represents a call to an MCP tool.
type McpToolCallItem struct {
	ID        string             `json:"id"`
	Type      string             `json:"type"`
	Server    string             `json:"server"`
	Tool      string             `json:"tool"`
	Arguments any                `json:"arguments"`
	Result    *McpToolCallResult `json:"result,omitempty"`
	Error     *McpToolCallError  `json:"error,omitempty"`
	Status    McpToolCallStatus  `json:"status"`
}

func (i *McpToolCallItem) ItemType() string { return i.Type }

// WebSearchItem captures a web search request.
type WebSearchItem struct {
	ID    string `json:"id"`
	Type  string `json:"type"`
	Query string `json:"query"`
}

func (i *WebSearchItem) ItemType() string { return i.Type }

// ErrorItem is a non-fatal error surfaced as an item.
type ErrorItem struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Message string `json:"message"`
}

func (i *ErrorItem) ItemType() string { return i.Type }

// TodoItem is a single entry in the agent's todo list.
type TodoItem struct {
	Text      string `json:"text"`
	Completed bool   `json:"completed"`
}

// TodoListItem tracks the agent's todo list.
type TodoListItem struct {
	ID    string     `json:"id"`
	Type  string     `json:"type"`
	Items []TodoItem `json:"items"`
}

func (i *TodoListItem) ItemType() string { return i.Type }

// WebSearchItem or other types parsed by item type string.
func parseThreadItem(raw json.RawMessage) (ThreadItem, error) {
	var base struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &base); err != nil {
		return nil, err
	}
	switch base.Type {
	case "agent_message":
		var item AgentMessageItem
		if err := json.Unmarshal(raw, &item); err != nil {
			return nil, err
		}
		return &item, nil
	case "reasoning":
		var item ReasoningItem
		if err := json.Unmarshal(raw, &item); err != nil {
			return nil, err
		}
		return &item, nil
	case "command_execution":
		var item CommandExecutionItem
		if err := json.Unmarshal(raw, &item); err != nil {
			return nil, err
		}
		return &item, nil
	case "file_change":
		var item FileChangeItem
		if err := json.Unmarshal(raw, &item); err != nil {
			return nil, err
		}
		return &item, nil
	case "mcp_tool_call":
		var item McpToolCallItem
		if err := json.Unmarshal(raw, &item); err != nil {
			return nil, err
		}
		return &item, nil
	case "web_search":
		var item WebSearchItem
		if err := json.Unmarshal(raw, &item); err != nil {
			return nil, err
		}
		return &item, nil
	case "todo_list":
		var item TodoListItem
		if err := json.Unmarshal(raw, &item); err != nil {
			return nil, err
		}
		return &item, nil
	case "error":
		var item ErrorItem
		if err := json.Unmarshal(raw, &item); err != nil {
			return nil, err
		}
		return &item, nil
	default:
		return &UnknownItem{
			Type: base.Type,
			Raw:  append(json.RawMessage(nil), raw...),
		}, nil
	}
}
