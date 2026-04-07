package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/hzj629206/assistant/agent"
)

func TestAppServerTurnSandboxPolicyAddsWritableRootsForWorkspaceWrite(t *testing.T) {
	t.Parallel()

	roots := []string{"/tmp/state.json", "/var/tmp/assistant-state"}
	got := appServerTurnSandboxPolicy("workspace-write", roots)

	want := agent.SandboxPolicy{
		"type":          "workspaceWrite",
		"writableRoots": roots,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected sandbox policy: %#v", got)
	}
}

func TestAppServerTurnSandboxPolicyLeavesNonWorkspaceWriteUntouched(t *testing.T) {
	t.Parallel()

	got := appServerTurnSandboxPolicy("read-only", []string{"/tmp/state.json"})
	if got != appServerSandboxPolicy("read-only") {
		t.Fatalf("unexpected sandbox policy: %#v", got)
	}
}

func TestCodexAdditionalDirectoriesUsesParentDirectoryForFiles(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	filePath := filepath.Join(root, "state.json")
	otherDir := filepath.Join(root, "nested")
	if err := os.WriteFile(filePath, []byte("state"), 0o600); err != nil {
		t.Fatalf("write state file failed: %v", err)
	}

	got := codexAdditionalDirectories([]string{filePath, otherDir, filePath})
	want := []string{root, otherDir}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected additional directories: %#v", got)
	}
}
