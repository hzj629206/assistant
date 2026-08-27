package agent

import (
	"context"
	"strings"
)

const slashCommandStop = "stop"
const slashCommandStopMute = "mute"
const slashCommandReset = "reset"
const slashCommandHelp = "help"
const slashCommandStatus = "status"

// CommandSpec describes one supported slash command.
type CommandSpec struct {
	Name        string
	Usage       string
	Description string
	Interrupts  bool
}

var supportedSlashCommands = []CommandSpec{
	{
		Name:        slashCommandStop,
		Usage:       "/stop [mute]",
		Description: "Stop the current turn and discard unprocessed messages; use mute to ignore future messages in this conversation.",
		Interrupts:  true,
	},
	{
		Name:        slashCommandReset,
		Usage:       "/reset",
		Description: "Stop the current turn, reset the persisted conversation state, and force the next turn to start a new runner thread.",
		Interrupts:  true,
	},
	{
		Name:        slashCommandStatus,
		Usage:       "/status",
		Description: "Show the current runner working directories and execution settings.",
	},
	{
		Name:        slashCommandHelp,
		Usage:       "/help",
		Description: "List supported slash commands.",
	},
}

// SlashCommand describes one dispatcher-level slash command.
type SlashCommand struct {
	Name string
	Args string
	Raw  string
}

// CommandRequest wraps one slash command invocation routed outside the normal turn queue.
type CommandRequest struct {
	ConversationKey string
	EventID         string
	Responder       CommandResponder
	Command         SlashCommand
	resolvedSpec    CommandSpec
}

// ParseSlashCommand parses a slash command from normalized text content.
func ParseSlashCommand(text string) (SlashCommand, bool) {
	trimmed := strings.TrimSpace(text)
	if !strings.HasPrefix(trimmed, "/") {
		return SlashCommand{}, false
	}

	body := strings.TrimSpace(strings.TrimPrefix(trimmed, "/"))
	if body == "" {
		return SlashCommand{}, false
	}

	name := body
	args := ""
	if index := strings.IndexAny(body, " \t\r\n"); index >= 0 {
		name = body[:index]
		args = strings.TrimSpace(body[index+1:])
	}
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return SlashCommand{}, false
	}

	return SlashCommand{
		Name: name,
		Args: args,
		Raw:  trimmed,
	}, true
}

// IsStop reports whether the slash command is /stop.
func (c SlashCommand) IsStop() bool {
	return strings.EqualFold(strings.TrimSpace(c.Name), slashCommandStop)
}

// IsReset reports whether the slash command is /reset.
func (c SlashCommand) IsReset() bool {
	return strings.EqualFold(strings.TrimSpace(c.Name), slashCommandReset)
}

// IsStatus reports whether the slash command is /status.
func (c SlashCommand) IsStatus() bool {
	return strings.EqualFold(strings.TrimSpace(c.Name), slashCommandStatus)
}

// IsHelp reports whether the slash command is /help.
func (c SlashCommand) IsHelp() bool {
	return strings.EqualFold(strings.TrimSpace(c.Name), slashCommandHelp)
}

// Spec returns the supported command spec for the current slash command.
func (c SlashCommand) Spec() (CommandSpec, bool) {
	return DispatcherCommandSpec(c.Name)
}

// SupportedDispatcherSlashCommands returns the supported dispatcher-level slash commands.
func SupportedDispatcherSlashCommands() []CommandSpec {
	return append([]CommandSpec(nil), supportedSlashCommands...)
}

// DispatcherCommandSpec looks up one dispatcher-level slash command specification.
func DispatcherCommandSpec(name string) (CommandSpec, bool) {
	normalized := strings.TrimSpace(strings.ToLower(name))
	for _, spec := range supportedSlashCommands {
		if spec.Name == normalized {
			return spec, true
		}
	}
	return CommandSpec{}, false
}

// Validate reports whether the command has valid slash-command syntax.
func (c SlashCommand) Validate() bool {
	if strings.TrimSpace(c.Name) == "" {
		return false
	}
	args := strings.TrimSpace(c.Args)
	return args == "" || c.IsStop() && strings.EqualFold(args, slashCommandStopMute)
}

// CommandResponder delivers the final slash-command reply.
type CommandResponder interface {
	SendText(ctx context.Context, text string) error
	Cleanup(ctx context.Context) error
}
