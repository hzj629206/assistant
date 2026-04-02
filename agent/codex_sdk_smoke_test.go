package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/godeps/codex-sdk-go"
)

// TestCodexSDKSmoke verifies the minimal codex-sdk-go flow from the README:
// NewCodex -> StartThread -> Run.
// Use medium reasoning here because the runner hardcodes gpt-5.4 and normalizes
// unsupported minimal reasoning to medium.
func TestCodexSDKSmoke(t *testing.T) {
	t.Parallel()

	skipCodexSDKSmoke(t, "CODEX_SDK_SMOKE")

	result, err := runCodexSDKSmoke(codex.ThreadOptions{
		Model:                "gpt-5.4",
		SandboxMode:          codex.SandboxWorkspaceWrite,
		ModelReasoningEffort: codex.ReasoningMedium,
		WebSearchMode:        codex.WebSearchLive,
		ApprovalPolicy:       codex.ApprovalNever,
	})
	if err != nil {
		t.Fatalf("run Codex SDK smoke test failed: %v", err)
	}
	if result.ThreadID == "" {
		t.Fatal("expected Codex thread ID to be populated")
	}
	if result.Response.Status != "ok" {
		t.Fatalf("unexpected status: %q", result.Response.Status)
	}
	if result.Response.Echo != "codex-sdk-smoke" {
		t.Fatalf("unexpected echo: %q", result.Response.Echo)
	}
}

func TestCodexSDKResumeThreadSmoke(t *testing.T) {
	t.Parallel()

	skipCodexSDKSmoke(t, "CODEX_SDK_SMOKE")

	result, err := runCodexSDKResumeSmoke(codex.ThreadOptions{
		Model:                "gpt-5.4",
		SandboxMode:          codex.SandboxWorkspaceWrite,
		ModelReasoningEffort: codex.ReasoningMedium,
		WebSearchMode:        codex.WebSearchLive,
		ApprovalPolicy:       codex.ApprovalNever,
	})
	if err != nil {
		t.Fatalf("run Codex SDK resume smoke test failed: %v", err)
	}
	if result.ThreadID == "" {
		t.Fatal("expected Codex thread ID to be populated")
	}
	if result.InitialResponse.Status != "ok" {
		t.Fatalf("unexpected initial status: %q", result.InitialResponse.Status)
	}
	if result.InitialResponse.Echo != "codex-sdk-smoke-initial" {
		t.Fatalf("unexpected initial echo: %q", result.InitialResponse.Echo)
	}
	if result.ResumedResponse.Status != "ok" {
		t.Fatalf("unexpected resumed status: %q", result.ResumedResponse.Status)
	}
	if result.ResumedResponse.Echo != "codex-sdk-smoke-resumed" {
		t.Fatalf("unexpected resumed echo: %q", result.ResumedResponse.Echo)
	}
}

func TestCodexSDKSmokeConfigMatrix(t *testing.T) {
	t.Parallel()

	skipCodexSDKSmoke(t, "CODEX_SDK_SMOKE_MATRIX")

	testCases := []struct {
		name    string
		options codex.ThreadOptions
	}{
		{
			name: "baseline_workspace_write_never",
			options: codex.ThreadOptions{
				Model:                "gpt-5.4",
				SandboxMode:          codex.SandboxWorkspaceWrite,
				ModelReasoningEffort: codex.ReasoningMedium,
				WebSearchMode:        codex.WebSearchLive,
				ApprovalPolicy:       codex.ApprovalNever,
			},
		},
		{
			name: "workspace_write_never",
			options: codex.ThreadOptions{
				Model:                "gpt-5.4",
				SandboxMode:          codex.SandboxWorkspaceWrite,
				ModelReasoningEffort: codex.ReasoningMedium,
				WebSearchMode:        codex.WebSearchLive,
				ApprovalPolicy:       codex.ApprovalNever,
			},
		},
		{
			name: "workspace_write_on_failure",
			options: codex.ThreadOptions{
				Model:                "gpt-5.4",
				SandboxMode:          codex.SandboxWorkspaceWrite,
				ModelReasoningEffort: codex.ReasoningMedium,
				WebSearchMode:        codex.WebSearchLive,
				ApprovalPolicy:       codex.ApprovalOnFailure,
			},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			start := time.Now()
			result, err := runCodexSDKSmoke(testCase.options)
			duration := time.Since(start).Round(time.Millisecond)
			if err != nil {
				t.Fatalf("smoke run failed after %s: %v", duration, err)
			}

			t.Logf(
				"completed in %s with thread_id=%s sandbox=%s approval=%s",
				duration,
				result.ThreadID,
				testCase.options.SandboxMode,
				testCase.options.ApprovalPolicy,
			)
		})
	}
}

func TestCodexSDKSmokeBehaviorMatrix(t *testing.T) {
	t.Parallel()

	skipCodexSDKSmoke(t, "CODEX_SDK_SMOKE_BEHAVIOR")

	testCases := []struct {
		name    string
		options codex.ThreadOptions
	}{
		{
			name: "baseline_live_medium",
			options: codex.ThreadOptions{
				Model:                "gpt-5.4",
				SandboxMode:          codex.SandboxWorkspaceWrite,
				ModelReasoningEffort: codex.ReasoningMedium,
				WebSearchMode:        codex.WebSearchLive,
				ApprovalPolicy:       codex.ApprovalNever,
			},
		},
		{
			name: "disabled_web_medium",
			options: codex.ThreadOptions{
				Model:                "gpt-5.4",
				SandboxMode:          codex.SandboxWorkspaceWrite,
				ModelReasoningEffort: codex.ReasoningMedium,
				WebSearchMode:        codex.WebSearchDisabled,
				ApprovalPolicy:       codex.ApprovalNever,
			},
		},
		{
			name: "live_web_low",
			options: codex.ThreadOptions{
				Model:                "gpt-5.4",
				SandboxMode:          codex.SandboxWorkspaceWrite,
				ModelReasoningEffort: codex.ReasoningLow,
				WebSearchMode:        codex.WebSearchLive,
				ApprovalPolicy:       codex.ApprovalNever,
			},
		},
		{
			name: "disabled_web_low",
			options: codex.ThreadOptions{
				Model:                "gpt-5.4",
				SandboxMode:          codex.SandboxWorkspaceWrite,
				ModelReasoningEffort: codex.ReasoningLow,
				WebSearchMode:        codex.WebSearchDisabled,
				ApprovalPolicy:       codex.ApprovalNever,
			},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			start := time.Now()
			result, err := runCodexSDKSmoke(testCase.options)
			duration := time.Since(start).Round(time.Millisecond)
			if err != nil {
				t.Fatalf(
					"behavior run failed after %s: %v (sandbox=%s approval=%s web=%s reasoning=%s)",
					duration,
					err,
					testCase.options.SandboxMode,
					testCase.options.ApprovalPolicy,
					testCase.options.WebSearchMode,
					testCase.options.ModelReasoningEffort,
				)
			}

			t.Logf(
				"completed in %s with thread_id=%s sandbox=%s approval=%s web=%s reasoning=%s",
				duration,
				result.ThreadID,
				testCase.options.SandboxMode,
				testCase.options.ApprovalPolicy,
				testCase.options.WebSearchMode,
				testCase.options.ModelReasoningEffort,
			)
		})
	}
}

func TestNewCodexRunnerDefaultsReasoningToMedium(t *testing.T) {
	t.Parallel()

	runner := NewCodexRunner(CodexRunnerOptions{})
	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working directory failed: %v", err)
	}
	if runner.threadOptions.Model != defaultModel {
		t.Fatalf("unexpected default model: %q", runner.threadOptions.Model)
	}
	if runner.threadOptions.SandboxMode != codex.SandboxReadOnly {
		t.Fatalf("unexpected default sandbox mode: %q", runner.threadOptions.SandboxMode)
	}
	if runner.threadOptions.WorkingDirectory != workingDirectory {
		t.Fatalf("unexpected default working directory: %q", runner.threadOptions.WorkingDirectory)
	}
	if !runner.threadOptions.SkipGitRepoCheck {
		t.Fatal("expected default skip git repo check to be enabled")
	}
	if runner.threadOptions.ApprovalPolicy != codex.ApprovalNever {
		t.Fatalf("unexpected default approval policy: %q", runner.threadOptions.ApprovalPolicy)
	}
	if runner.threadOptions.ModelReasoningEffort != codex.ReasoningMedium {
		t.Fatalf("unexpected default reasoning effort: %q", runner.threadOptions.ModelReasoningEffort)
	}
}

func TestNewCodexRunnerNormalizesUnsupportedMinimalReasoning(t *testing.T) {
	t.Parallel()

	runner := NewCodexRunner(CodexRunnerOptions{
		ThreadOptions: codex.ThreadOptions{
			Model:                "gpt-5.1",
			SandboxMode:          codex.SandboxWorkspaceWrite,
			WorkingDirectory:     "/tmp/assistant-custom",
			SkipGitRepoCheck:     true,
			ApprovalPolicy:       codex.ApprovalOnRequest,
			ModelReasoningEffort: codex.ReasoningMinimal,
		},
	})
	if runner.threadOptions.Model != defaultModel {
		t.Fatalf("unexpected model override result: %q", runner.threadOptions.Model)
	}
	if runner.threadOptions.SandboxMode != codex.SandboxWorkspaceWrite {
		t.Fatalf("unexpected sandbox mode: %q", runner.threadOptions.SandboxMode)
	}
	if runner.threadOptions.WorkingDirectory != "/tmp/assistant-custom" {
		t.Fatalf("unexpected working directory: %q", runner.threadOptions.WorkingDirectory)
	}
	if !runner.threadOptions.SkipGitRepoCheck {
		t.Fatal("expected explicit skip git repo check to be preserved")
	}
	if runner.threadOptions.ApprovalPolicy != codex.ApprovalOnRequest {
		t.Fatalf("unexpected approval policy: %q", runner.threadOptions.ApprovalPolicy)
	}
	if runner.threadOptions.ModelReasoningEffort != codex.ReasoningMedium {
		t.Fatalf("unexpected normalized reasoning effort: %q", runner.threadOptions.ModelReasoningEffort)
	}
}

func TestNewCodexRunnerPreservesExplicitSupportedReasoning(t *testing.T) {
	t.Parallel()

	runner := NewCodexRunner(CodexRunnerOptions{
		ThreadOptions: codex.ThreadOptions{
			SandboxMode:          codex.SandboxWorkspaceWrite,
			WorkingDirectory:     "/tmp/assistant-supported",
			SkipGitRepoCheck:     true,
			ApprovalPolicy:       codex.ApprovalOnRequest,
			ModelReasoningEffort: codex.ReasoningHigh,
		},
	})
	if runner.threadOptions.Model != defaultModel {
		t.Fatalf("unexpected default model: %q", runner.threadOptions.Model)
	}
	if runner.threadOptions.SandboxMode != codex.SandboxWorkspaceWrite {
		t.Fatalf("unexpected sandbox mode: %q", runner.threadOptions.SandboxMode)
	}
	if runner.threadOptions.WorkingDirectory != "/tmp/assistant-supported" {
		t.Fatalf("unexpected working directory: %q", runner.threadOptions.WorkingDirectory)
	}
	if !runner.threadOptions.SkipGitRepoCheck {
		t.Fatal("expected explicit skip git repo check to be preserved")
	}
	if runner.threadOptions.ApprovalPolicy != codex.ApprovalOnRequest {
		t.Fatalf("unexpected approval policy: %q", runner.threadOptions.ApprovalPolicy)
	}
	if runner.threadOptions.ModelReasoningEffort != codex.ReasoningHigh {
		t.Fatalf("unexpected reasoning effort: %q", runner.threadOptions.ModelReasoningEffort)
	}
}

type codexSDKSmokeResult struct {
	ThreadID string
	Response struct {
		Status string `json:"status"`
		Echo   string `json:"echo"`
	}
}

type codexSDKResumeSmokeResult struct {
	ThreadID        string
	InitialResponse struct {
		Status string `json:"status"`
		Echo   string `json:"echo"`
	}
	ResumedResponse struct {
		Status string `json:"status"`
		Echo   string `json:"echo"`
	}
}

func skipCodexSDKSmoke(t *testing.T, envKey string) {
	t.Helper()

	if testing.Short() {
		t.Skip("skipping Codex SDK smoke test in short mode")
	}
	if os.Getenv(envKey) == "" {
		t.Skipf("set %s=1 to run the Codex SDK smoke test", envKey)
	}
	if _, err := exec.LookPath("codex"); err != nil {
		t.Skipf("skipping Codex SDK smoke test: codex CLI not found: %v", err)
	}
}

func runCodexSDKSmoke(options codex.ThreadOptions) (codexSDKSmokeResult, error) {
	threadOptions, err := buildCodexSDKThreadOptions(options)
	if err != nil {
		return codexSDKSmokeResult{}, err
	}

	client := codex.NewCodex(codex.CodexOptions{})
	thread := client.StartThread(threadOptions)

	response, err := runCodexSDKThreadTurn(thread, "codex-sdk-smoke")
	if err != nil {
		return codexSDKSmokeResult{}, err
	}

	result := codexSDKSmokeResult{
		ThreadID: thread.ID(),
		Response: response,
	}
	if result.ThreadID == "" {
		return codexSDKSmokeResult{}, errors.New("expected Codex thread ID to be populated")
	}
	return result, nil
}

func runCodexSDKResumeSmoke(options codex.ThreadOptions) (codexSDKResumeSmokeResult, error) {
	threadOptions, err := buildCodexSDKThreadOptions(options)
	if err != nil {
		return codexSDKResumeSmokeResult{}, err
	}

	client := codex.NewCodex(codex.CodexOptions{})
	thread := client.StartThread(threadOptions)

	initialResponse, err := runCodexSDKThreadTurn(thread, "codex-sdk-smoke-initial")
	if err != nil {
		return codexSDKResumeSmokeResult{}, err
	}
	threadID := thread.ID()
	if threadID == "" {
		return codexSDKResumeSmokeResult{}, errors.New("expected Codex thread ID to be populated")
	}

	resumedThread := client.ResumeThread(threadID, threadOptions)
	resumedResponse, err := runCodexSDKThreadTurn(resumedThread, "codex-sdk-smoke-resumed")
	if err != nil {
		return codexSDKResumeSmokeResult{}, err
	}
	if resumedThread.ID() != threadID {
		return codexSDKResumeSmokeResult{}, fmt.Errorf(
			"expected resumed thread ID %q, got %q",
			threadID,
			resumedThread.ID(),
		)
	}

	result := codexSDKResumeSmokeResult{
		ThreadID:        threadID,
		InitialResponse: initialResponse,
		ResumedResponse: resumedResponse,
	}
	return result, nil
}

func buildCodexSDKThreadOptions(options codex.ThreadOptions) (codex.ThreadOptions, error) {
	workingDirectory, err := os.Getwd()
	if err != nil {
		return codex.ThreadOptions{}, fmt.Errorf("get working directory failed: %w", err)
	}

	networkAccess := true
	webSearch := true
	return codex.ThreadOptions{
		Model:                options.Model,
		SandboxMode:          options.SandboxMode,
		WorkingDirectory:     workingDirectory,
		SkipGitRepoCheck:     true,
		ModelReasoningEffort: options.ModelReasoningEffort,
		NetworkAccessEnabled: &networkAccess,
		WebSearchEnabled:     &webSearch,
		WebSearchMode:        options.WebSearchMode,
		ApprovalPolicy:       options.ApprovalPolicy,
	}, nil
}

type codexSDKSmokeResponse struct {
	Status string `json:"status"`
	Echo   string `json:"echo"`
}

type codexSDKRunnableThread interface {
	ID() string
	Run(input codex.Input, opts codex.TurnOptions) (*codex.Turn, error)
}

func runCodexSDKThreadTurn(thread codexSDKRunnableThread, expectedEcho string) (codexSDKSmokeResponse, error) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"status": map[string]any{
				"type": "string",
				"enum": []any{"ok"},
			},
			"echo": map[string]any{
				"type": "string",
			},
		},
		"required":             []any{"status", "echo"},
		"additionalProperties": false,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	turn, err := thread.Run(
		codex.TextInput(fmt.Sprintf(
			`Return JSON only. Set "status" to "ok" and "echo" to %q.`,
			expectedEcho,
		)),
		codex.TurnOptions{
			OutputSchema: schema,
			Context:      ctx,
		},
	)
	if err != nil {
		return codexSDKSmokeResponse{}, err
	}
	if turn == nil {
		return codexSDKSmokeResponse{}, errors.New("expected turn to be populated")
	}

	var response codexSDKSmokeResponse
	if err = json.Unmarshal([]byte(turn.FinalResponse), &response); err != nil {
		return codexSDKSmokeResponse{}, fmt.Errorf("decode final response failed: %w; response=%q", err, turn.FinalResponse)
	}
	if response.Status != "ok" {
		return codexSDKSmokeResponse{}, fmt.Errorf("unexpected status: %q", response.Status)
	}
	if response.Echo != expectedEcho {
		return codexSDKSmokeResponse{}, fmt.Errorf("unexpected echo: %q", response.Echo)
	}
	return response, nil
}
