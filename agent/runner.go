package agent

import (
	"context"
	"encoding/json"
	"errors"
)

type turnRequestContextKey struct{}

// Tool describes one server-side capability exposed through the prompt-based tool loop.
type Tool interface {
	Name() string
	Description() string
	InputSchema() any
	OutputSchema() any
	Call(ctx context.Context, input json.RawMessage) (any, error)
}

// SessionMode describes one runner-backed mode exposed for the current conversation.
type SessionMode struct {
	ID    string
	Label string
}

// SessionModes describes the runner-backed mode state for the current conversation.
type SessionModes struct {
	CurrentModeID  string
	AvailableModes []SessionMode
}

// SessionConfigOptionChoice describes one allowed value for a session config option.
type SessionConfigOptionChoice struct {
	Name        string
	Description string
}

// SessionConfigOption describes one runner-backed configuration option.
type SessionConfigOption struct {
	Name         string
	CurrentValue string
	Options      []SessionConfigOptionChoice
}

// SessionStatus describes the effective runner-backed session settings for the current conversation.
type SessionStatus struct {
	Agent              string
	WorkingDirectories []string
	Modes              SessionModes
	ConfigOptions      []SessionConfigOption
}

// ErrSessionInterruptUnavailable indicates the runner currently cannot issue an interrupt for the active turn.
var ErrSessionInterruptUnavailable = errors.New("session interrupt is unavailable")

// ErrSessionBusy indicates the session already has an active turn in progress.
var ErrSessionBusy = errors.New("session already has an active turn")

// Session executes agent turns against one runner-managed agent thread/session.
//
// Contract:
//   - RunTurn calls on the same session must not overlap. Implementations should reject
//     overlapping calls with ErrSessionBusy.
//   - RunTurn should be able to return once Interrupt or Close is called.
//   - Interrupt must be safe to call repeatedly and concurrently. It should return nil when
//     no turn is active, and return ErrSessionInterruptUnavailable when a turn is active but
//     the backing runner cannot issue an interrupt.
//   - If Interrupt is requested while RunTurn is active, that RunTurn call must not return a
//     successful TurnResult. It should return an interruption or cancellation error instead,
//     even if the backing runner produced a partial or final reply.
//   - Close must be safe to call repeatedly and concurrently.
type Session interface {
	ID() string
	RunTurn(ctx context.Context, req TurnRequest) (TurnResult, error)
	Interrupt(ctx context.Context) error
	Close() error
	Status(ctx context.Context) (SessionStatus, error)
	Commands() []CommandSpec
	HandleCommand(ctx context.Context, command SlashCommand) (string, error)
}

// SessionOptions configures one runner-backed session instance.
type SessionOptions struct {
	ConversationKey string
	ResumeSessionID string
}

// Runner creates sessions that execute agent turns.
type Runner interface {
	StartSession(ctx context.Context, options SessionOptions) (Session, error)
	Close() error
	RegisterSystemPrompt(prompt string)
	RegisterTools(tools ...Tool)
}

// ContextWithTurnRequest stores the current turn request in context for tool calls.
func ContextWithTurnRequest(ctx context.Context, req TurnRequest) context.Context {
	return context.WithValue(ctx, turnRequestContextKey{}, req)
}

// TurnRequestFromContext returns the turn request attached to a tool call context.
func TurnRequestFromContext(ctx context.Context) (TurnRequest, bool) {
	if ctx == nil {
		return TurnRequest{}, false
	}

	req, ok := ctx.Value(turnRequestContextKey{}).(TurnRequest)
	return req, ok
}

func joinRunnerContext(ctx context.Context, runnerCtx context.Context) (context.Context, func()) {
	if ctx == nil {
		ctx = context.Background()
	}
	if runnerCtx == nil {
		return context.WithCancel(ctx)
	}

	joinedCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(runnerCtx, cancel)
	return joinedCtx, func() {
		stop()
		cancel()
	}
}
