package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"testing"
	"time"

	appcodex "github.com/pmenglund/codex-sdk-go"
)

// TestAppServerSDKSmoke verifies the minimal app-server SDK flow:
// New -> StartThread -> RunInputs.
func TestAppServerSDKSmoke(t *testing.T) {
	t.Parallel()

	skipAppServerSDKSmoke(t, "CODEX_APP_SERVER_SDK_SMOKE")

	result, err := runAppServerSDKSmoke(appcodex.ThreadStartOptions{
		Model:          "gpt-5.4",
		SandboxPolicy:  appcodex.SandboxModeWorkspaceWrite,
		ApprovalPolicy: appcodex.ApprovalPolicyNever,
	}, appcodex.TurnOptions{
		Model:          "gpt-5.4",
		SandboxPolicy:  appcodex.SandboxModeWorkspaceWrite,
		ApprovalPolicy: appcodex.ApprovalPolicyNever,
		Effort:         appcodex.ReasoningEffortMedium,
	})
	if err != nil {
		t.Fatalf("run app-server SDK smoke test failed: %v", err)
	}
	if result.ThreadID == "" {
		t.Fatal("expected app-server thread ID to be populated")
	}
	if result.Response.Status != "ok" {
		t.Fatalf("unexpected status: %q", result.Response.Status)
	}
	if result.Response.Echo != "app-server-sdk-smoke" {
		t.Fatalf("unexpected echo: %q", result.Response.Echo)
	}
}

func TestAppServerSDKResumeThreadSmoke(t *testing.T) {
	t.Parallel()

	skipAppServerSDKSmoke(t, "CODEX_APP_SERVER_SDK_SMOKE")

	result, err := runAppServerSDKResumeSmoke(appcodex.ThreadStartOptions{
		Model:          "gpt-5.4",
		SandboxPolicy:  appcodex.SandboxModeWorkspaceWrite,
		ApprovalPolicy: appcodex.ApprovalPolicyNever,
	}, appcodex.ThreadResumeOptions{
		Model:          "gpt-5.4",
		Sandbox:        appcodex.SandboxModeWorkspaceWrite,
		ApprovalPolicy: appcodex.ApprovalPolicyNever,
	}, appcodex.TurnOptions{
		Model:          "gpt-5.4",
		SandboxPolicy:  appcodex.SandboxModeWorkspaceWrite,
		ApprovalPolicy: appcodex.ApprovalPolicyNever,
		Effort:         appcodex.ReasoningEffortMedium,
	})
	if err != nil {
		t.Fatalf("run app-server SDK resume smoke test failed: %v", err)
	}
	if result.ThreadID == "" {
		t.Fatal("expected app-server thread ID to be populated")
	}
	if result.InitialResponse.Status != "ok" {
		t.Fatalf("unexpected initial status: %q", result.InitialResponse.Status)
	}
	if result.InitialResponse.Echo != "app-server-sdk-smoke-initial" {
		t.Fatalf("unexpected initial echo: %q", result.InitialResponse.Echo)
	}
	if result.ResumedResponse.Status != "ok" {
		t.Fatalf("unexpected resumed status: %q", result.ResumedResponse.Status)
	}
	if result.ResumedResponse.Echo != "app-server-sdk-smoke-resumed" {
		t.Fatalf("unexpected resumed echo: %q", result.ResumedResponse.Echo)
	}
}

func TestNewAppServerRunnerDefaultsReasoningToMedium(t *testing.T) {
	t.Parallel()

	runner, err := NewAppServerRunner(context.Background(), AppServerRunnerOptions{
		Client: &appcodex.Codex{},
	})
	if err != nil {
		t.Fatalf("create app-server runner failed: %v", err)
	}

	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working directory failed: %v", err)
	}
	if runner.startOptions.Model != defaultModel {
		t.Fatalf("unexpected default start model: %q", runner.startOptions.Model)
	}
	if runner.resumeOptions.Model != defaultModel {
		t.Fatalf("unexpected default resume model: %q", runner.resumeOptions.Model)
	}
	if runner.turnOptions.Model != defaultModel {
		t.Fatalf("unexpected default turn model: %q", runner.turnOptions.Model)
	}
	if runner.startOptions.Cwd != workingDirectory {
		t.Fatalf("unexpected default start working directory: %q", runner.startOptions.Cwd)
	}
	if runner.resumeOptions.Cwd != workingDirectory {
		t.Fatalf("unexpected default resume working directory: %q", runner.resumeOptions.Cwd)
	}
	if runner.turnOptions.Cwd != workingDirectory {
		t.Fatalf("unexpected default turn working directory: %q", runner.turnOptions.Cwd)
	}
	expectedSandboxPolicy := SandboxPolicyWorkspaceWrite
	if runner.startOptions.SandboxPolicy != expectedSandboxPolicy.String() {
		t.Fatalf("unexpected default start sandbox policy: %v", runner.startOptions.SandboxPolicy)
	}
	if runner.resumeOptions.Sandbox != expectedSandboxPolicy.String() {
		t.Fatalf("unexpected default resume sandbox policy: %v", runner.resumeOptions.Sandbox)
	}
	if !reflect.DeepEqual(runner.turnOptions.SandboxPolicy, expectedSandboxPolicy) {
		t.Fatalf("unexpected default turn sandbox policy: %v", runner.turnOptions.SandboxPolicy)
	}
	if runner.startOptions.ApprovalPolicy != appcodex.ApprovalPolicyNever {
		t.Fatalf("unexpected default start approval policy: %v", runner.startOptions.ApprovalPolicy)
	}
	if runner.resumeOptions.ApprovalPolicy != appcodex.ApprovalPolicyNever {
		t.Fatalf("unexpected default resume approval policy: %v", runner.resumeOptions.ApprovalPolicy)
	}
	if runner.turnOptions.ApprovalPolicy != appcodex.ApprovalPolicyNever {
		t.Fatalf("unexpected default turn approval policy: %v", runner.turnOptions.ApprovalPolicy)
	}
	if runner.turnOptions.Effort != appcodex.ReasoningEffortMedium {
		t.Fatalf("unexpected default reasoning effort: %v", runner.turnOptions.Effort)
	}
}

func TestNewAppServerRunnerNormalizesLegacySandboxModes(t *testing.T) {
	t.Parallel()

	runner, err := NewAppServerRunner(context.Background(), AppServerRunnerOptions{
		Client: &appcodex.Codex{},
		StartOptions: appcodex.ThreadStartOptions{
			SandboxPolicy: appcodex.SandboxModeWorkspaceWrite,
		},
		ResumeOptions: appcodex.ThreadResumeOptions{
			Sandbox: appcodex.SandboxModeReadOnly,
		},
		TurnOptions: appcodex.TurnOptions{
			SandboxPolicy: appcodex.SandboxModeDangerFullAccess,
		},
	})
	if err != nil {
		t.Fatalf("create app-server runner failed: %v", err)
	}

	if runner.startOptions.SandboxPolicy != SandboxPolicyWorkspaceWrite.String() {
		t.Fatalf("unexpected start sandbox policy: %#v", runner.startOptions.SandboxPolicy)
	}
	if runner.resumeOptions.Sandbox != SandboxPolicyReadOnly.String() {
		t.Fatalf("unexpected resume sandbox policy: %#v", runner.resumeOptions.Sandbox)
	}
	if !reflect.DeepEqual(runner.turnOptions.SandboxPolicy, SandboxPolicyDangerFullAccess) {
		t.Fatalf("unexpected turn sandbox policy: %#v", runner.turnOptions.SandboxPolicy)
	}
}

func TestNewAppServerRunnerNormalizesUnsupportedMinimalReasoning(t *testing.T) {
	t.Parallel()

	runner, err := NewAppServerRunner(context.Background(), AppServerRunnerOptions{
		Client: &appcodex.Codex{},
		StartOptions: appcodex.ThreadStartOptions{
			Model:          "gpt-5.4-mini",
			Cwd:            "/tmp/assistant-appserver-custom",
			SandboxPolicy:  appcodex.SandboxModeWorkspaceWrite,
			ApprovalPolicy: appcodex.ApprovalPolicyOnRequest,
		},
		ResumeOptions: appcodex.ThreadResumeOptions{
			Model:          "gpt-5.4-mini",
			Cwd:            "/tmp/assistant-appserver-custom",
			Sandbox:        appcodex.SandboxModeWorkspaceWrite,
			ApprovalPolicy: appcodex.ApprovalPolicyOnRequest,
		},
		TurnOptions: appcodex.TurnOptions{
			Model:          "gpt-5.4-mini",
			Cwd:            "/tmp/assistant-appserver-custom",
			SandboxPolicy:  appcodex.SandboxModeWorkspaceWrite,
			ApprovalPolicy: appcodex.ApprovalPolicyOnRequest,
			Effort:         appcodex.ReasoningEffortMinimal,
		},
	})
	if err != nil {
		t.Fatalf("create app-server runner failed: %v", err)
	}

	if runner.startOptions.Model != "gpt-5.4-mini" {
		t.Fatalf("unexpected start model: %q", runner.startOptions.Model)
	}
	if runner.resumeOptions.Model != "gpt-5.4-mini" {
		t.Fatalf("unexpected resume model: %q", runner.resumeOptions.Model)
	}
	if runner.turnOptions.Model != "gpt-5.4-mini" {
		t.Fatalf("unexpected turn model: %q", runner.turnOptions.Model)
	}
	if runner.startOptions.Cwd != "/tmp/assistant-appserver-custom" {
		t.Fatalf("unexpected start working directory: %q", runner.startOptions.Cwd)
	}
	if runner.resumeOptions.Cwd != "/tmp/assistant-appserver-custom" {
		t.Fatalf("unexpected resume working directory: %q", runner.resumeOptions.Cwd)
	}
	if runner.turnOptions.Cwd != "/tmp/assistant-appserver-custom" {
		t.Fatalf("unexpected turn working directory: %q", runner.turnOptions.Cwd)
	}
	if runner.startOptions.ApprovalPolicy != appcodex.ApprovalPolicyOnRequest {
		t.Fatalf("unexpected start approval policy: %v", runner.startOptions.ApprovalPolicy)
	}
	if runner.resumeOptions.ApprovalPolicy != appcodex.ApprovalPolicyOnRequest {
		t.Fatalf("unexpected resume approval policy: %v", runner.resumeOptions.ApprovalPolicy)
	}
	if runner.turnOptions.ApprovalPolicy != appcodex.ApprovalPolicyOnRequest {
		t.Fatalf("unexpected turn approval policy: %v", runner.turnOptions.ApprovalPolicy)
	}
	if runner.turnOptions.Effort != appcodex.ReasoningEffortMedium {
		t.Fatalf("unexpected normalized reasoning effort: %v", runner.turnOptions.Effort)
	}
}

type appServerSDKSmokeResult struct {
	ThreadID string
	Response struct {
		Status string `json:"status"`
		Echo   string `json:"echo"`
	}
}

type appServerSDKResumeSmokeResult struct {
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

func skipAppServerSDKSmoke(t *testing.T, envKey string) {
	t.Helper()

	if testing.Short() {
		t.Skip("skipping app-server SDK smoke test in short mode")
	}
	if os.Getenv(envKey) == "" {
		t.Skipf("set %s=1 to run the app-server SDK smoke test", envKey)
	}
	if _, err := exec.LookPath("codex"); err != nil {
		t.Skipf("skipping app-server SDK smoke test: codex CLI not found: %v", err)
	}
}

func runAppServerSDKSmoke(startOptions appcodex.ThreadStartOptions, turnOptions appcodex.TurnOptions) (appServerSDKSmokeResult, error) {
	startOptions, _, turnOptions, err := buildAppServerSDKOptions(startOptions, appcodex.ThreadResumeOptions{}, turnOptions)
	if err != nil {
		return appServerSDKSmokeResult{}, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	client, err := appcodex.New(ctx, appcodex.Options{})
	if err != nil {
		return appServerSDKSmokeResult{}, err
	}
	defer func() {
		_ = client.Close()
	}()

	thread, err := client.StartThread(ctx, startOptions)
	if err != nil {
		return appServerSDKSmokeResult{}, err
	}

	response, err := runAppServerSDKThreadTurn(ctx, thread, turnOptions, "app-server-sdk-smoke")
	if err != nil {
		return appServerSDKSmokeResult{}, err
	}

	result := appServerSDKSmokeResult{
		ThreadID: thread.ID(),
		Response: response,
	}
	if result.ThreadID == "" {
		return appServerSDKSmokeResult{}, errors.New("expected app-server thread ID to be populated")
	}
	return result, nil
}

func runAppServerSDKResumeSmoke(startOptions appcodex.ThreadStartOptions, resumeOptions appcodex.ThreadResumeOptions, turnOptions appcodex.TurnOptions) (appServerSDKResumeSmokeResult, error) {
	startOptions, resumeOptions, turnOptions, err := buildAppServerSDKOptions(startOptions, resumeOptions, turnOptions)
	if err != nil {
		return appServerSDKResumeSmokeResult{}, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	client, err := appcodex.New(ctx, appcodex.Options{})
	if err != nil {
		return appServerSDKResumeSmokeResult{}, err
	}
	defer func() {
		_ = client.Close()
	}()

	thread, err := client.StartThread(ctx, startOptions)
	if err != nil {
		return appServerSDKResumeSmokeResult{}, err
	}

	initialResponse, err := runAppServerSDKThreadTurn(ctx, thread, turnOptions, "app-server-sdk-smoke-initial")
	if err != nil {
		return appServerSDKResumeSmokeResult{}, err
	}
	threadID := thread.ID()
	if threadID == "" {
		return appServerSDKResumeSmokeResult{}, errors.New("expected app-server thread ID to be populated")
	}

	resumeOptions.ThreadID = threadID
	resumedThread, err := client.ResumeThread(ctx, resumeOptions)
	if err != nil {
		return appServerSDKResumeSmokeResult{}, err
	}

	resumedResponse, err := runAppServerSDKThreadTurn(ctx, resumedThread, turnOptions, "app-server-sdk-smoke-resumed")
	if err != nil {
		return appServerSDKResumeSmokeResult{}, err
	}
	if resumedThread.ID() != threadID {
		return appServerSDKResumeSmokeResult{}, fmt.Errorf("expected resumed thread ID %q, got %q", threadID, resumedThread.ID())
	}

	return appServerSDKResumeSmokeResult{
		ThreadID:        threadID,
		InitialResponse: initialResponse,
		ResumedResponse: resumedResponse,
	}, nil
}

func buildAppServerSDKOptions(startOptions appcodex.ThreadStartOptions, resumeOptions appcodex.ThreadResumeOptions, turnOptions appcodex.TurnOptions) (appcodex.ThreadStartOptions, appcodex.ThreadResumeOptions, appcodex.TurnOptions, error) {
	workingDirectory, err := os.Getwd()
	if err != nil {
		return appcodex.ThreadStartOptions{}, appcodex.ThreadResumeOptions{}, appcodex.TurnOptions{}, fmt.Errorf("get working directory failed: %w", err)
	}

	if startOptions.Cwd == "" {
		startOptions.Cwd = workingDirectory
	}
	if resumeOptions.Cwd == "" {
		resumeOptions.Cwd = workingDirectory
	}
	if turnOptions.Cwd == "" {
		turnOptions.Cwd = workingDirectory
	}
	if startOptions.Model == "" {
		startOptions.Model = "gpt-5.4"
	}
	if resumeOptions.Model == "" {
		resumeOptions.Model = startOptions.Model
	}
	if turnOptions.Model == "" {
		turnOptions.Model = startOptions.Model
	}
	if startOptions.SandboxPolicy == nil {
		startOptions.SandboxPolicy = defaultAppServerSandboxPolicy()
	}
	if resumeOptions.Sandbox == nil {
		resumeOptions.Sandbox = defaultAppServerSandboxPolicy()
	}
	if turnOptions.SandboxPolicy == nil {
		turnOptions.SandboxPolicy = defaultAppServerSandboxPolicy()
	}
	startOptions.SandboxPolicy = normalizeAppServerSandboxPolicy(startOptions.SandboxPolicy)
	resumeOptions.Sandbox = normalizeAppServerSandboxPolicy(resumeOptions.Sandbox)
	turnOptions.SandboxPolicy = normalizeAppServerSandboxPolicy(turnOptions.SandboxPolicy)
	if startOptions.ApprovalPolicy == nil {
		startOptions.ApprovalPolicy = appcodex.ApprovalPolicyNever
	}
	if resumeOptions.ApprovalPolicy == nil {
		resumeOptions.ApprovalPolicy = appcodex.ApprovalPolicyNever
	}
	if turnOptions.ApprovalPolicy == nil {
		turnOptions.ApprovalPolicy = appcodex.ApprovalPolicyNever
	}
	if turnOptions.Effort == nil {
		turnOptions.Effort = appcodex.ReasoningEffortMedium
	}

	return startOptions, resumeOptions, turnOptions, nil
}

type appServerSDKSmokeResponse struct {
	Status string `json:"status"`
	Echo   string `json:"echo"`
}

type appServerSDKRunnableThread interface {
	ID() string
	RunInputs(ctx context.Context, inputs []appcodex.Input, opts *appcodex.TurnOptions) (*appcodex.TurnResult, error)
}

func runAppServerSDKThreadTurn(ctx context.Context, thread appServerSDKRunnableThread, turnOptions appcodex.TurnOptions, expectedEcho string) (appServerSDKSmokeResponse, error) {
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

	turnOptions.OutputSchema = schema
	turn, err := thread.RunInputs(
		ctx,
		[]appcodex.Input{
			appcodex.TextInput(fmt.Sprintf(`Return JSON only. Set "status" to "ok" and "echo" to %q.`, expectedEcho)),
		},
		&turnOptions,
	)
	if err != nil {
		return appServerSDKSmokeResponse{}, err
	}
	if turn == nil {
		return appServerSDKSmokeResponse{}, errors.New("expected turn to be populated")
	}

	var response appServerSDKSmokeResponse
	if err := json.Unmarshal([]byte(turn.FinalResponse), &response); err != nil {
		return appServerSDKSmokeResponse{}, fmt.Errorf("decode final response failed: %w; response=%q", err, turn.FinalResponse)
	}
	if response.Status != "ok" {
		return appServerSDKSmokeResponse{}, fmt.Errorf("unexpected status: %q", response.Status)
	}
	if response.Echo != expectedEcho {
		return appServerSDKSmokeResponse{}, fmt.Errorf("unexpected echo: %q", response.Echo)
	}

	return response, nil
}
