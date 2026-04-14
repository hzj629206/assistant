package agent

import (
	"context"
	"encoding/json"
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

// Runner executes one agent turn against a conversation state.
type Runner interface {
	RunTurn(ctx context.Context, req TurnRequest) (TurnResult, error)
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
