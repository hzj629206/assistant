package agent

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	appcodex "github.com/pmenglund/codex-sdk-go"
)

func TestNewAppServerRPCClientPreservesSpawnEnvironmentAndLifetime(t *testing.T) {
	t.Setenv("ASSISTANT_TEST_SPAWN_OVERRIDE", "inherited")
	t.Setenv("ASSISTANT_TEST_SPAWN_INHERITED", "retained")

	// The fake server checks environment forwarding before completing initialization.
	script := `#!/bin/sh
set -eu
[ "$1" = "app-server" ]
[ "$ASSISTANT_TEST_SPAWN_OVERRIDE" = "overridden" ]
[ "$ASSISTANT_TEST_SPAWN_INHERITED" = "retained" ]
IFS= read -r initialize
printf '%s\n' '{"id":1,"result":{}}'
while IFS= read -r message; do :; done
`
	binary := filepath.Join(t.TempDir(), "app-server")
	//nolint:gosec // The private temporary script must be executable to act as an app-server.
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, closeFn, initialized, err := newAppServerRPCClient(ctx, appcodex.Options{
		Spawn: appcodex.SpawnOptions{
			CodexPath: binary,
			Stderr:    io.Discard,
			Env:       []string{"ASSISTANT_TEST_SPAWN_OVERRIDE=overridden"},
		},
	}, nil, newAppServerRequestCallbacks(&AppServerRunner{}))
	if err != nil {
		t.Fatalf("initialize spawned server: %v", err)
	}
	t.Cleanup(func() {
		if err := closeFn(); err != nil {
			t.Errorf("close spawned server: %v", err)
		}
	})
	if !initialized {
		t.Fatal("spawned server was not initialized")
	}

	cancel()
	// Startup cancellation must not terminate the daemon-owned app-server.
	if err := client.Notify(context.Background(), "test/lifetime", nil); err != nil {
		t.Fatalf("use server after startup context cancellation: %v", err)
	}
}

func TestAppServerStdioTransportOwnsProcessGroup(t *testing.T) {
	t.Parallel()

	transport, err := spawnAppServerStdio(context.Background(), "sh", []string{"-c", "cat"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := transport.Close(); err != nil {
			t.Errorf("close transport: %v", err)
		}
	})
	pid := transport.cmd.Process.Pid
	groupID, err := syscall.Getpgid(pid)
	if err != nil {
		t.Fatal(err)
	}
	if groupID != pid {
		t.Fatalf("process group = %d, want dedicated group %d", groupID, pid)
	}
	if err := transport.WriteLine("hello"); err != nil {
		t.Fatal(err)
	}
	line, err := transport.ReadLine()
	if err != nil {
		t.Fatal(err)
	}
	if line != "hello" {
		t.Fatalf("read line = %q, want hello", line)
	}
}
