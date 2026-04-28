package agent

import (
	"context"
	"encoding/json"
)

// Turn represents one session-managed turn that can be run or interrupted in either order.
//
// Contract:
//   - Run and Interrupt must be safe to call in either order and concurrently.
//   - Run should be able to return once Interrupt is called.
//   - Run must not return until the turn has fully completed and all cleanup is done.
//   - Interrupt must be safe to call repeatedly and concurrently.
//   - Interrupt must not return until the active turn has fully completed and all cleanup is done.
//   - If Interrupt is requested for the turn, Run must not return a
//     successful TurnResult. It should return an interruption or cancellation error instead,
//     even if the backing runner produced a partial or final reply.
//   - Done must close at the same completion point as Run returns and Interrupt returns,
//     and is provided for asynchronous waiting.
type Turn interface {
	Run(ctx context.Context) (TurnResult, error)
	Interrupt(ctx context.Context) error
	Done() <-chan struct{}
}

// Session executes agent turns against one runner-managed agent thread/session.
//
// Contract:
//   - ScheduleTurn publishes one new active turn and returns its handle.
//   - If a previous turn is still active, ScheduleTurn should interrupt it, wait for it
//     to finish, and then publish the new turn.
//   - Close must be safe to call repeatedly and concurrently.
//   - Status must be safe to call concurrently with ScheduleTurn, Turn.Run,
//     Turn.Interrupt, and Close.
type Session interface {
	ID() string
	ScheduleTurn(ctx context.Context, req TurnRequest) (Turn, error)
	Close() error
	Status(ctx context.Context) (SessionStatus, error)
}

// Runner creates sessions that execute agent turns.
type Runner interface {
	StartSession(ctx context.Context, options SessionOptions) (Session, error)
	Close() error
	RegisterSystemPrompt(prompt string)
	RegisterTools(tools ...Tool)
}

// Tool describes one server-side capability exposed through the prompt-based tool loop.
type Tool interface {
	Name() string
	Description() string
	InputSchema() any
	OutputSchema() any
	Call(ctx context.Context, input json.RawMessage) (any, error)
}

type turnRequestContextKey struct{}

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
