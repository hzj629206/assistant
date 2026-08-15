package codex

import "context"

// ApprovalMode controls the approval policy for tool executions.
type ApprovalMode string

const (
	ApprovalNever     ApprovalMode = "never"
	ApprovalOnRequest ApprovalMode = "on-request"
	ApprovalOnFailure ApprovalMode = "on-failure"
	ApprovalUntrusted ApprovalMode = "untrusted"
)

// SandboxMode controls filesystem access for the Codex CLI.
type SandboxMode string

const (
	SandboxReadOnly         SandboxMode = "read-only"
	SandboxWorkspaceWrite   SandboxMode = "workspace-write"
	SandboxDangerFullAccess SandboxMode = "danger-full-access"
)

// ModelReasoningEffort configures the model reasoning effort.
type ModelReasoningEffort string

const (
	ReasoningMinimal ModelReasoningEffort = "minimal"
	ReasoningLow     ModelReasoningEffort = "low"
	ReasoningMedium  ModelReasoningEffort = "medium"
	ReasoningHigh    ModelReasoningEffort = "high"
	ReasoningXHigh   ModelReasoningEffort = "xhigh"
)

// WebSearchMode controls web search behavior.
type WebSearchMode string

const (
	WebSearchDisabled WebSearchMode = "disabled"
	WebSearchCached   WebSearchMode = "cached"
	WebSearchLive     WebSearchMode = "live"
)

// CodexOptions configures the Codex client.
type CodexOptions struct {
	CodexPathOverride string
	BaseURL           string
	APIKey            string
	// Config provides additional Codex CLI configuration overrides.
	// The SDK flattens nested objects into repeated --config dotted.path=TOML-value flags.
	Config map[string]any
	// Env overrides the environment passed to the Codex CLI process.
	// When provided, the SDK will not inherit variables from the parent process.
	Env map[string]string
}

// ThreadOptions configures a Codex thread.
type ThreadOptions struct {
	Model                 string
	SandboxMode           SandboxMode
	WorkingDirectory      string
	SkipGitRepoCheck      bool
	ModelReasoningEffort  ModelReasoningEffort
	NetworkAccessEnabled  *bool
	WebSearchMode         WebSearchMode
	WebSearchEnabled      *bool
	ApprovalPolicy        ApprovalMode
	AdditionalDirectories []string
}

// TurnOptions configures a single turn.
type TurnOptions struct {
	// OutputSchema is a JSON schema describing the expected agent output.
	OutputSchema any
	// Context controls cancellation for the turn.
	//nolint:containedctx // The caller owns turn cancellation and the process inherits it.
	Context context.Context
}

// UserInputType represents the type of an input entry.
type UserInputType string

const (
	UserInputText       UserInputType = "text"
	UserInputLocalImage UserInputType = "local_image"
)

// UserInput represents a structured input entry.
type UserInput struct {
	Type UserInputType `json:"type"`
	Text string        `json:"text,omitempty"`
	Path string        `json:"path,omitempty"`
}

// Input describes a turn input. Provide Text or Items.
type Input struct {
	Text  string
	Items []UserInput
}

// TextInput creates an Input with a text prompt.
func TextInput(text string) Input {
	return Input{Text: text}
}

// ItemsInput creates an Input with structured entries.
func ItemsInput(items ...UserInput) Input {
	return Input{Items: items}
}
