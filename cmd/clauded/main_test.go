package main

import (
	"reflect"
	"testing"

	claudecode "github.com/lancekrogers/claude-code-go/pkg/claude"

	"github.com/hzj629206/assistant/config"
)

func TestNewClaudeCodeRunOptionsMapsConfig(t *testing.T) {
	t.Parallel()

	claudeCfg := config.ClaudeConfig{
		Model:                 "claude-sonnet-4-5",
		Permission:            "accept-edits",
		ReasoningEffort:       "high",
		AdditionalDirectories: []string{"/tmp/a", "/tmp/b"},
	}

	got := newClaudeCodeRunOptions(claudeCfg)

	want := claudecode.RunOptions{
		Effort:         claudecode.EffortHigh,
		Model:          "claude-sonnet-4-5",
		PermissionMode: claudecode.PermissionModeAcceptEdits,
		AddDirectories: []string{"/tmp/a", "/tmp/b"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected claude run options: %#v", got)
	}
}
