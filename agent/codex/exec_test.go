package codex

import (
	"reflect"
	"testing"
)

func TestBuildCommandArgsIncludesConfigBeforeThreadOverrides(t *testing.T) {
	networkAccess := true

	args, err := buildCommandArgs(CodexExecArgs{
		ThreadID: "thread-123",
		Images:   []string{"one.png", "two.jpg"},
		Config: map[string]any{
			"approval_policy":        "never",
			"feature_enabled":        true,
			"model_reasoning_effort": "low",
			"nested": map[string]any{
				"answer": 42,
				"array":  []any{"x", true, 3.5},
			},
			"sandbox_workspace_write": map[string]any{
				"network_access": false,
			},
			"web_search": "cached",
		},
		Model:                 "gpt-5",
		SandboxMode:           SandboxDangerFullAccess,
		WorkingDirectory:      "/tmp/project",
		AdditionalDirectories: []string{"/extra/one", "/extra/two"},
		SkipGitRepoCheck:      true,
		OutputSchemaFile:      "/tmp/schema.json",
		ModelReasoningEffort:  ReasoningHigh,
		NetworkAccessEnabled:  &networkAccess,
		WebSearchMode:         WebSearchLive,
		ApprovalPolicy:        ApprovalOnRequest,
	})
	if err != nil {
		t.Fatalf("buildCommandArgs: %v", err)
	}

	want := []string{
		"exec", "--experimental-json",
		"--config", "approval_policy=\"never\"",
		"--config", "feature_enabled=true",
		"--config", "model_reasoning_effort=\"low\"",
		"--config", "nested.answer=42",
		"--config", "nested.array=[\"x\", true, 3.5]",
		"--config", "sandbox_workspace_write.network_access=false",
		"--config", "web_search=\"cached\"",
		"--model", "gpt-5",
		"--sandbox", "danger-full-access",
		"--cd", "/tmp/project",
		"--add-dir", "/extra/one",
		"--add-dir", "/extra/two",
		"--skip-git-repo-check",
		"--output-schema", "/tmp/schema.json",
		"--config", "model_reasoning_effort=\"high\"",
		"--config", "sandbox_workspace_write.network_access=true",
		"--config", "web_search=\"live\"",
		"--config", "approval_policy=\"on-request\"",
		"--image", "one.png",
		"--image", "two.jpg",
		"resume", "thread-123",
	}

	if !reflect.DeepEqual(args, want) {
		t.Fatalf("unexpected args:\n got: %#v\nwant: %#v", args, want)
	}
}

func TestBuildCommandArgsUsesWebSearchEnabledFallback(t *testing.T) {
	webSearchEnabled := false

	args, err := buildCommandArgs(CodexExecArgs{
		WebSearchEnabled: &webSearchEnabled,
	})
	if err != nil {
		t.Fatalf("buildCommandArgs: %v", err)
	}

	want := []string{
		"exec", "--experimental-json",
		"--config", "web_search=\"disabled\"",
	}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("unexpected args:\n got: %#v\nwant: %#v", args, want)
	}
}

func TestBuildCommandArgsRejectsNilConfigValues(t *testing.T) {
	_, err := buildCommandArgs(CodexExecArgs{
		Config: map[string]any{
			"bad": nil,
		},
	})
	if err == nil {
		t.Fatal("expected error for nil config value")
	}
}
