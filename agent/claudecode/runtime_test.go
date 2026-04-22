package claudecode

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestBuildProcessArgsUsesFileFlagsOnlyForExistingFiles(t *testing.T) {
	t.Parallel()

	inlineArgs := BuildProcessArgs(&RunOptions{
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

	fileArgs := BuildProcessArgs(&RunOptions{
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
