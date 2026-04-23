package claudecode

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestBuildCLIArgsHonorsVerboseAndClaudeFlags(t *testing.T) {
	t.Parallel()

	args := BuildCLIArgs(&RunOptions{
		Verbose:                true,
		PermissionMode:         PermissionModeAcceptEdits,
		SettingSources:         []string{"user", "project", "local"},
		IncludePartialMessages: true,
		ReplayUserMessages:     true,
		Debug:                  true,
		DebugToStderr:          true,
		EnableAuthStatus:       true,
		MaxThinkingTokens:      31999,
		NoChrome:               true,
	})

	for _, expected := range []string{
		"--verbose",
		"--permission-prompt-tool",
		"stdio",
		"--permission-mode",
		string(PermissionModeAcceptEdits),
		"--setting-sources",
		"user,project,local",
		"--include-partial-messages",
		"--replay-user-messages",
		"--debug",
		"--debug-to-stderr",
		"--enable-auth-status",
		"--max-thinking-tokens",
		"31999",
		"--no-chrome",
	} {
		if !slices.Contains(args, expected) {
			t.Fatalf("expected %q in args, got %q", expected, args)
		}
	}
}

func TestPreprocessOptionsRejectsNegativeMaxThinkingTokens(t *testing.T) {
	t.Parallel()

	err := PreprocessOptions(&RunOptions{MaxThinkingTokens: -1})
	if err == nil {
		t.Fatal("expected validation error")
	}
}

func TestBuildCLIArgsOmitsVerboseByDefault(t *testing.T) {
	t.Parallel()

	args := BuildCLIArgs(&RunOptions{})
	if slices.Contains(args, "--verbose") {
		t.Fatalf("did not expect verbose flag by default, got %q", args)
	}
	if !slices.Contains(args, "--permission-prompt-tool") || !slices.Contains(args, "stdio") {
		t.Fatalf("expected stdio permission prompt tool by default, got %q", args)
	}
}

func TestBuildProcessArgsUsesFileFlagsOnlyForExistingFiles(t *testing.T) {
	t.Parallel()

	inlineArgs := BuildCLIArgs(&RunOptions{
		SystemPrompt: "inline system prompt",
		AppendPrompt: "inline append prompt",
	})
	if !slices.Contains(inlineArgs, "--system-prompt") || slices.Contains(inlineArgs, "--system-prompt-file") {
		t.Fatalf("expected inline system prompt arg, got %q", inlineArgs)
	}
	if !slices.Contains(inlineArgs, "--append-system-prompt") || slices.Contains(inlineArgs, "--append-system-prompt-file") {
		t.Fatalf("expected inline append prompt arg, got %q", inlineArgs)
	}

	dir := t.TempDir()
	systemPath := filepath.Join(dir, "system.txt")
	appendPath := filepath.Join(dir, "append.txt")
	err := os.WriteFile(systemPath, []byte("system prompt"), 0o600)
	if err != nil {
		t.Fatalf("WriteFile system prompt failed: %v", err)
	}
	err = os.WriteFile(appendPath, []byte("append prompt"), 0o600)
	if err != nil {
		t.Fatalf("WriteFile append prompt failed: %v", err)
	}

	fileArgs := BuildCLIArgs(&RunOptions{
		SystemPrompt: systemPath,
		AppendPrompt: appendPath,
	})
	if !slices.Contains(fileArgs, "--system-prompt-file") || slices.Contains(fileArgs, "--system-prompt") {
		t.Fatalf("expected file-based system prompt arg, got %q", fileArgs)
	}
	if !slices.Contains(fileArgs, "--append-system-prompt-file") || slices.Contains(fileArgs, "--append-system-prompt") {
		t.Fatalf("expected file-based append prompt arg, got %q", fileArgs)
	}
}

func TestBuildStreamProcessArgsIgnoresNoSessionPersistence(t *testing.T) {
	t.Parallel()

	args := BuildStreamProcessArgs(&RunOptions{
		NoSessionPersistence: true,
	})
	if slices.Contains(args, "--no-session-persistence") {
		t.Fatalf("did not expect no-session-persistence in session args: %q", args)
	}
}
