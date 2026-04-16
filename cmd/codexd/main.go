package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	codexcli "github.com/godeps/codex-sdk-go"
	codexapp "github.com/pmenglund/codex-sdk-go"

	"github.com/hzj629206/assistant/agent"
	"github.com/hzj629206/assistant/config"
	"github.com/hzj629206/assistant/internal/daemon"
)

func main() {
	daemon.Run(newCodexRunner)
}

func newCodexRunner(ctx context.Context, cfg config.Config) (agent.Runner, error) {
	switch cfg.Codex.Backend {
	case "appserver":
		return agent.NewAppServerRunner(ctx, agent.AppServerRunnerOptions{
			StartOptions: codexapp.ThreadStartOptions{
				Model:         cfg.Codex.Model,
				SandboxPolicy: appServerSandboxPolicy(cfg.Codex.Sandbox),
			},
			ResumeOptions: codexapp.ThreadResumeOptions{
				Model:   cfg.Codex.Model,
				Sandbox: appServerSandboxPolicy(cfg.Codex.Sandbox),
			},
			TurnOptions: codexapp.TurnOptions{
				Model:         cfg.Codex.Model,
				SandboxPolicy: appServerTurnSandboxPolicy(cfg.Codex.Sandbox, cfg.Codex.AdditionalWritableRoots),
				Effort:        appServerReasoningEffort(cfg.Codex.ReasoningEffort),
			},
		})
	case "exec":
		return agent.NewCodexRunner(agent.CodexRunnerOptions{
			ThreadOptions: codexcli.ThreadOptions{
				Model:                 cfg.Codex.Model,
				SandboxMode:           codexSandboxMode(cfg.Codex.Sandbox),
				ModelReasoningEffort:  codexReasoningEffort(cfg.Codex.ReasoningEffort),
				AdditionalDirectories: codexAdditionalDirectories(cfg.Codex.AdditionalWritableRoots),
			},
		}), nil
	default:
		return nil, fmt.Errorf("unsupported codex backend %q", cfg.Codex.Backend)
	}
}

func appServerReasoningEffort(value string) any {
	switch value {
	case "none":
		return codexapp.ReasoningEffortNone
	case "minimal":
		return codexapp.ReasoningEffortMinimal
	case "medium":
		return codexapp.ReasoningEffortMedium
	case "high":
		return codexapp.ReasoningEffortHigh
	case "xhigh":
		return codexapp.ReasoningEffortXHigh
	default:
		return codexapp.ReasoningEffortLow
	}
}

func appServerSandboxPolicy(value string) any {
	switch value {
	case "workspace-write":
		return codexapp.SandboxModeWorkspaceWrite
	case "danger-full-access":
		return codexapp.SandboxModeDangerFullAccess
	default:
		return codexapp.SandboxModeReadOnly
	}
}

func appServerTurnSandboxPolicy(value string, writableRoots []string) any {
	if value != "workspace-write" {
		return appServerSandboxPolicy(value)
	}

	policy := agent.SandboxPolicy{"type": "workspaceWrite"}
	if len(writableRoots) != 0 {
		policy["writableRoots"] = append([]string(nil), writableRoots...)
	}
	return policy
}

func codexReasoningEffort(value string) codexcli.ModelReasoningEffort {
	switch value {
	case "minimal":
		return codexcli.ReasoningMinimal
	case "medium":
		return codexcli.ReasoningMedium
	case "high":
		return codexcli.ReasoningHigh
	case "xhigh":
		return codexcli.ReasoningXHigh
	default:
		return codexcli.ReasoningLow
	}
}

func codexSandboxMode(value string) codexcli.SandboxMode {
	switch value {
	case "workspace-write":
		return codexcli.SandboxWorkspaceWrite
	case "danger-full-access":
		return codexcli.SandboxDangerFullAccess
	default:
		return codexcli.SandboxReadOnly
	}
}

func codexAdditionalDirectories(paths []string) []string {
	if len(paths) == 0 {
		return nil
	}

	directories := make([]string, 0, len(paths))
	seen := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		directory := path
		info, err := os.Stat(path)
		if err == nil && !info.IsDir() {
			directory = filepath.Dir(path)
		}
		directory = filepath.Clean(directory)
		if _, ok := seen[directory]; ok {
			continue
		}
		seen[directory] = struct{}{}
		directories = append(directories, directory)
	}
	if len(directories) == 0 {
		return nil
	}
	return directories
}
