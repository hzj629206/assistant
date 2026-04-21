package main

import (
	"context"
	"testing"

	"github.com/hzj629206/assistant/agent"
	"github.com/hzj629206/assistant/internal/config"
)

func TestNewACPRunnerReturnsACPRunner(t *testing.T) {
	t.Parallel()

	runner, err := newACPRunner(context.Background(), config.Config{})
	if err != nil {
		t.Fatalf("newACPRunner failed: %v", err)
	}
	if _, ok := runner.(*agent.ACPRunner); !ok {
		t.Fatalf("unexpected runner type: %T", runner)
	}
}
