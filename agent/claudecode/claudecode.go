package claudecode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os/exec"
	"strings"
	"time"
)

// ClaudeClient holds the configured Claude CLI binary path.
type ClaudeClient struct {
	BinPath string
}

// NewClient creates a Claude CLI client wrapper.
func NewClient(binPath string) *ClaudeClient {
	return &ClaudeClient{BinPath: binPath}
}

// RunTurn runs one one-shot Claude print-mode turn with text input.
// The output format is text by default, or json when OutputSchema is set.
func (c *ClaudeClient) RunTurn(ctx context.Context, input string, opts RunOptions) (*ClaudeResult, error) {
	if c == nil {
		return nil, errors.New("claude client is nil")
	}
	if err := PreprocessOptions(&opts); err != nil {
		return nil, err
	}

	outputFormat := TextOutput
	if opts.OutputSchema != nil {
		outputFormat = JSONOutput
	}

	args := append([]string{"-p"}, BuildCLIArgsForInputAndOutput(&opts, TextInput, outputFormat)...)
	slog.Info("starting claude command", "path", c.BinPath, "args", args, "work_dir", opts.WorkingDirectory)
	//nolint:gosec // The Claude binary path is configured by the host process and arguments are generated from structured run options.
	cmd := exec.CommandContext(ctx, c.BinPath, args...)
	if opts.WorkingDirectory != "" {
		cmd.Dir = opts.WorkingDirectory
	}
	cmd.Env = BuildCLIEnv(opts.WorkingDirectory)
	cmd.Stdin = strings.NewReader(input)

	output, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			stderrText := strings.TrimSpace(string(exitErr.Stderr))
			if stderrText != "" {
				return nil, NewClaudeError(ErrorCommand, stderrText)
			}
		}
		return nil, NewClaudeError(ErrorCommand, fmt.Sprintf("failed to run claude command: %v", err))
	}

	result := &ClaudeResult{
		Type: "result",
	}
	if outputFormat == TextOutput {
		result.Result = string(output)
		return result, nil
	}

	if err = json.Unmarshal(output, result); err != nil {
		return nil, fmt.Errorf("decode claude json output failed: %w", err)
	}
	return result, nil
}

// InputFormat controls Claude CLI stdin input formatting.
type InputFormat string

const (
	TextInput       InputFormat = "text"
	StreamJSONInput InputFormat = "stream-json"
)

// OutputFormat controls Claude CLI output formatting.
type OutputFormat string

const (
	TextOutput       OutputFormat = "text"
	JSONOutput       OutputFormat = "json"
	StreamJSONOutput OutputFormat = "stream-json"
)

// EffortLevel controls Claude reasoning effort.
type EffortLevel string

const (
	EffortLow    EffortLevel = "low"
	EffortMedium EffortLevel = "medium"
	EffortHigh   EffortLevel = "high"
	EffortXHigh  EffortLevel = "xhigh"
	EffortMax    EffortLevel = "max"
)

// PermissionMode controls how Claude handles tool permissions.
type PermissionMode string

const (
	PermissionModeDefault           PermissionMode = "default"
	PermissionModeAcceptEdits       PermissionMode = "acceptEdits"
	PermissionModeAuto              PermissionMode = "auto"
	PermissionModeBypassPermissions PermissionMode = "bypassPermissions"
	PermissionModeDontAsk           PermissionMode = "dontAsk"
	PermissionModePlan              PermissionMode = "plan"
)

// RunOptions configures Claude CLI execution for this project.
type RunOptions struct {
	SystemPrompt                       string
	AppendPrompt                       string
	AllowedTools                       []string
	DisallowedTools                    []string
	ResumeID                           string
	Continue                           bool
	Verbose                            bool
	Model                              string
	ModelAlias                         string
	Effort                             EffortLevel
	Timeout                            time.Duration
	IncludeHookEvents                  bool
	IncludePartialMessages             bool
	ReplayUserMessages                 bool
	DebugFile                          string
	Bare                               bool
	Brief                              bool
	Betas                              []string
	Files                              []string
	ExcludeDynamicSystemPromptSections bool
	PermissionMode                     PermissionMode
	OutputSchema                       any
	MaxBudgetUSD                       float64
	SessionID                          string
	ForkSession                        bool
	NoSessionPersistence               bool
	MCPConfigs                         []string
	StrictMCPConfig                    bool
	AddDirectories                     []string
	SettingSources                     []string
	Settings                           string
	Tools                              []string
	WorkingDirectory                   string
}

// RetryPolicy defines retry behavior for transient Claude failures.
type RetryPolicy struct {
	MaxRetries    int
	BaseDelay     time.Duration
	MaxDelay      time.Duration
	BackoffFactor float64
}

// DefaultRetryPolicy returns the default retry settings used by the runner.
func DefaultRetryPolicy() *RetryPolicy {
	return &RetryPolicy{
		MaxRetries:    3,
		BaseDelay:     100 * time.Millisecond,
		MaxDelay:      5 * time.Second,
		BackoffFactor: 2,
	}
}

// ErrorType classifies Claude execution failures.
type ErrorType int

const (
	ErrorUnknown ErrorType = iota
	ErrorAuthentication
	ErrorRateLimit
	ErrorPermission
	ErrorCommand
	ErrorNetwork
	ErrorMCP
	ErrorValidation
	ErrorTimeout
	ErrorSession
)

func (e ErrorType) String() string {
	switch e {
	case ErrorAuthentication:
		return "authentication"
	case ErrorRateLimit:
		return "rate_limit"
	case ErrorPermission:
		return "permission"
	case ErrorCommand:
		return "command"
	case ErrorNetwork:
		return "network"
	case ErrorMCP:
		return "mcp"
	case ErrorValidation:
		return "validation"
	case ErrorTimeout:
		return "timeout"
	case ErrorSession:
		return "session"
	case ErrorUnknown:
		return "unknown"
	default:
		return "unknown"
	}
}

// IsRetryable reports whether the error type is generally safe to retry.
func (e ErrorType) IsRetryable() bool {
	switch e {
	case ErrorRateLimit, ErrorNetwork, ErrorTimeout, ErrorMCP:
		return true
	default:
		return false
	}
}

// ClaudeError is the structured error returned by the runner.
type ClaudeError struct {
	Type     ErrorType      `json:"type"`
	Message  string         `json:"message"`
	Code     int            `json:"code,omitempty"`
	Details  map[string]any `json:"details,omitempty"`
	Original error          `json:"-"`
}

func (e *ClaudeError) Error() string {
	if e == nil {
		return ""
	}
	if e.Code != 0 {
		return fmt.Sprintf("claude error (%s, code=%d): %s", e.Type.String(), e.Code, e.Message)
	}
	return fmt.Sprintf("claude error (%s): %s", e.Type.String(), e.Message)
}

// Unwrap returns the underlying error.
func (e *ClaudeError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Original
}

// IsRetryable reports whether this specific error should be retried.
func (e *ClaudeError) IsRetryable() bool {
	if e == nil || !e.Type.IsRetryable() {
		return false
	}
	if e.Type != ErrorMCP {
		return true
	}

	lowerMsg := strings.ToLower(e.Message)
	return !containsAny(lowerMsg, []string{"configuration", "config", "invalid", "not found", "permission", "authentication", "unauthorized", "forbidden"})
}

// RetryDelay returns the recommended retry delay in seconds.
func (e *ClaudeError) RetryDelay() int {
	if e == nil {
		return 0
	}
	switch e.Type {
	case ErrorRateLimit:
		if retryAfter, ok := e.Details["retry_after"].(int); ok {
			return retryAfter
		}
		return 60
	case ErrorNetwork, ErrorTimeout:
		return 5
	case ErrorMCP:
		if e.IsRetryable() {
			return 3
		}
	default:
		return 0
	}
	return 0
}

// NewClaudeError creates a structured Claude error.
func NewClaudeError(errorType ErrorType, message string) *ClaudeError {
	return &ClaudeError{
		Type:    errorType,
		Message: message,
		Details: make(map[string]any),
	}
}

// NewValidationError creates a structured validation error.
func NewValidationError(message string, field string, value any) *ClaudeError {
	return &ClaudeError{
		Type:    ErrorValidation,
		Message: message,
		Details: map[string]any{
			"field": field,
			"value": value,
		},
	}
}

// ClaudeResult represents the final Claude CLI result message.
type ClaudeResult struct {
	Type             string  `json:"type"`
	Subtype          string  `json:"subtype,omitempty"`
	Result           string  `json:"result,omitempty"`
	StructuredOutput any     `json:"structured_output,omitempty"`
	CostUSD          float64 `json:"total_cost_usd"`
	DurationMS       int64   `json:"duration_ms"`
	DurationAPIMS    int64   `json:"duration_api_ms"`
	IsError          bool    `json:"is_error"`
	NumTurns         int     `json:"num_turns"`
	SessionID        string  `json:"session_id"`
}

// Message represents a streamed Claude CLI message.
type Message struct {
	Type             string          `json:"type"`
	Subtype          string          `json:"subtype,omitempty"`
	Message          json.RawMessage `json:"message,omitempty"`
	SessionID        string          `json:"session_id"`
	CostUSD          float64         `json:"total_cost_usd,omitempty"`
	DurationMS       int64           `json:"duration_ms,omitempty"`
	DurationAPIMS    int64           `json:"duration_api_ms,omitempty"`
	IsError          bool            `json:"is_error,omitempty"`
	NumTurns         int             `json:"num_turns,omitempty"`
	Result           string          `json:"result,omitempty"`
	StructuredOutput any             `json:"structured_output,omitempty"`
}

// MCPServerType identifies the server transport used by an MCP server.
type MCPServerType string

// MCPServerConfig represents an MCP server configuration.
type MCPServerConfig struct {
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	URL     string            `json:"url,omitempty"`
	Type    MCPServerType     `json:"type,omitempty"`
}

// MCPConfig contains the full MCP configuration file content.
type MCPConfig struct {
	MCPServers map[string]MCPServerConfig `json:"mcpServers"`
}

// MCPConfigBuilder builds MCP configuration JSON.
type MCPConfigBuilder struct {
	servers map[string]MCPServerConfig
}

// NewMCPConfigBuilder creates a new MCP config builder.
func NewMCPConfigBuilder() *MCPConfigBuilder {
	return &MCPConfigBuilder{servers: make(map[string]MCPServerConfig)}
}

// AddServer adds or replaces one MCP server definition.
func (b *MCPConfigBuilder) AddServer(name string, config MCPServerConfig) *MCPConfigBuilder {
	if b == nil {
		return nil
	}
	b.servers[name] = config
	return b
}

// BuildJSON renders the MCP configuration as indented JSON.
func (b *MCPConfigBuilder) BuildJSON() (string, error) {
	config := MCPConfig{
		MCPServers: make(map[string]MCPServerConfig, len(b.servers)),
	}
	maps.Copy(config.MCPServers, b.servers)

	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return "", fmt.Errorf("failed to marshal MCP config: %w", err)
	}
	return string(data), nil
}

// PreprocessOptions validates the subset of options used by this project.
func PreprocessOptions(opts *RunOptions) error {
	if opts == nil {
		return nil
	}
	if opts.Effort != "" && !isValidEffortLevel(opts.Effort) {
		return NewValidationError("Invalid effort level", "Effort", opts.Effort)
	}
	if opts.PermissionMode != "" && !isValidPermissionMode(opts.PermissionMode) {
		return NewValidationError("Invalid permission mode", "PermissionMode", opts.PermissionMode)
	}
	if opts.Timeout < 0 {
		return NewValidationError("Timeout cannot be negative", "Timeout", opts.Timeout)
	}
	if opts.MaxBudgetUSD < 0 {
		return NewValidationError("MaxBudgetUSD cannot be negative", "MaxBudgetUSD", opts.MaxBudgetUSD)
	}
	if opts.OutputSchema != nil {
		if _, err := json.Marshal(opts.OutputSchema); err != nil {
			return NewValidationError("OutputSchema must be JSON-serializable", "OutputSchema", opts.OutputSchema)
		}
	}
	if strings.TrimSpace(opts.ResumeID) == "" {
		opts.ResumeID = ""
	}
	if opts.SessionID != "" {
		if err := ValidateSessionID(opts.SessionID); err != nil {
			return NewValidationError(err.Error(), "SessionID", opts.SessionID)
		}
	}
	for _, source := range opts.SettingSources {
		switch source {
		case "user", "project", "local":
		default:
			return NewValidationError("Invalid setting source", "SettingSources", opts.SettingSources)
		}
	}
	return nil
}

// ValidateSessionID validates UUID-shaped session IDs.
func ValidateSessionID(id string) error {
	if id == "" {
		return nil
	}
	if len(id) != 36 {
		return errors.New("invalid session ID: must be a valid UUID (36 characters)")
	}
	if id[8] != '-' || id[13] != '-' || id[18] != '-' || id[23] != '-' {
		return errors.New("invalid session ID: must be a valid UUID format (xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx)")
	}
	for i, c := range id {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			continue
		}
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return fmt.Errorf("invalid session ID: contains non-hexadecimal character at position %d", i)
		}
	}
	return nil
}

func isValidEffortLevel(level EffortLevel) bool {
	switch level {
	case EffortLow, EffortMedium, EffortHigh, EffortXHigh, EffortMax:
		return true
	default:
		return false
	}
}

func isValidPermissionMode(mode PermissionMode) bool {
	switch mode {
	case PermissionModeDefault,
		PermissionModeAcceptEdits,
		PermissionModeAuto,
		PermissionModeBypassPermissions,
		PermissionModeDontAsk,
		PermissionModePlan:
		return true
	default:
		return false
	}
}

func containsAny(text string, parts []string) bool {
	for _, part := range parts {
		if strings.Contains(text, part) {
			return true
		}
	}
	return false
}
