package main

import (
	"context"

	claudecode "github.com/lancekrogers/claude-code-go/pkg/claude"

	"github.com/hzj629206/assistant/agent"
	"github.com/hzj629206/assistant/config"
	"github.com/hzj629206/assistant/internal/daemon"
)

func main() {
	daemon.Run(newClaudeCodeRunner)
}

func newClaudeCodeRunner(_ context.Context, cfg config.Config) (agent.Runner, error) {
	return agent.NewClaudeCodeRunner(agent.ClaudeCodeRunnerOptions{
		RunOptions: newClaudeCodeRunOptions(cfg.Claude),
	}), nil
}

func newClaudeCodeRunOptions(cfg config.ClaudeConfig) claudecode.RunOptions {
	return claudecode.RunOptions{
		Effort:         claudeReasoningEffort(cfg.Effort),
		Model:          cfg.Model,
		PermissionMode: claudePermissionMode(cfg.Permission),
		AddDirectories: append([]string(nil), cfg.AdditionalDirectories...),
	}
}

func claudeReasoningEffort(value string) claudecode.EffortLevel {
	switch value {
	case "low":
		return claudecode.EffortLow
	case "medium":
		return claudecode.EffortMedium
	case "high":
		return claudecode.EffortHigh
	case "xhigh":
		return claudecode.EffortXHigh
	case "max":
		return claudecode.EffortMax
	default:
		return ""
	}
}

func claudePermissionMode(value string) claudecode.PermissionMode {
	switch value {
	case "default":
		return claudecode.PermissionModeDefault
	case "accept-edits":
		return claudecode.PermissionModeAcceptEdits
	case "auto":
		return claudecode.PermissionModeAuto
	case "bypass-permissions":
		return claudecode.PermissionModeBypassPermissions
	case "dont-ask":
		return claudecode.PermissionModeDontAsk
	case "plan":
		return claudecode.PermissionModePlan
	default:
		return ""
	}
}
