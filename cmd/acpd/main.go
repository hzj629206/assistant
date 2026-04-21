package main

import (
	"context"

	"github.com/hzj629206/assistant/agent"
	"github.com/hzj629206/assistant/internal/config"
	"github.com/hzj629206/assistant/internal/daemon"
)

func main() {
	daemon.Run(newACPRunner)
}

func newACPRunner(_ context.Context, cfg config.Config) (agent.Runner, error) {
	return agent.NewACPRunner(agent.ACPRunnerOptions{
		Command:    cfg.ACP.Command,
		Args:       append([]string(nil), cfg.ACP.Args...),
		Env:        append([]string(nil), cfg.ACP.Env...),
		AuthMethod: cfg.ACP.AuthMethod,
	}), nil
}
