package agent

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

type appServerStdioTransport struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	mu     sync.Mutex
}

func spawnAppServerStdio(ctx context.Context, binary string, args []string, stderr io.Writer) (*appServerStdioTransport, error) {
	if binary == "" {
		return nil, errors.New("codex binary path is empty")
	}

	//nolint:gosec // The app-server binary path and args come from explicit local runner configuration.
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Stderr = stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}

	if err := cmd.Start(); err != nil {
		return nil, err
	}

	return &appServerStdioTransport{
		cmd:    cmd,
		stdin:  stdin,
		stdout: bufio.NewReader(stdout),
	}, nil
}

func (t *appServerStdioTransport) ReadLine() (string, error) {
	line, err := t.stdout.ReadString('\n')
	if err != nil {
		if errors.Is(err, io.EOF) && line != "" {
			return strings.TrimRight(line, "\n"), nil
		}
		return "", err
	}
	return strings.TrimRight(line, "\n"), nil
}

func (t *appServerStdioTransport) WriteLine(line string) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if !strings.HasSuffix(line, "\n") {
		line += "\n"
	}

	_, err := io.WriteString(t.stdin, line)
	return err
}

func (t *appServerStdioTransport) Close() error {
	var shutdownErr error

	if t.stdin != nil {
		if err := t.stdin.Close(); err != nil {
			shutdownErr = errors.Join(shutdownErr, fmt.Errorf("close stdin: %w", err))
		}
	}
	if t.cmd == nil {
		return shutdownErr
	}

	waitCh := make(chan error, 1)
	go func() {
		waitCh <- t.cmd.Wait()
	}()

	err, exited := waitProcess(waitCh, appServerStdioCloseTimeout)
	if exited {
		logAppServerProcessExit(t.cmd, err)
		return errors.Join(shutdownErr, ignoreExpectedAppServerExit(err))
	}

	if terminateErr := signalAppServerProcessGroup(t.cmd, syscall.SIGTERM); terminateErr != nil {
		shutdownErr = errors.Join(shutdownErr, fmt.Errorf("terminate process group: %w", terminateErr))
	}

	err, exited = waitProcess(waitCh, appServerStdioTerminateTimeout)
	if exited {
		logAppServerProcessExit(t.cmd, err)
		return errors.Join(shutdownErr, ignoreExpectedAppServerExit(err))
	}

	if killErr := signalAppServerProcessGroup(t.cmd, syscall.SIGKILL); killErr != nil {
		shutdownErr = errors.Join(shutdownErr, fmt.Errorf("kill process group: %w", killErr))
	}

	err = <-waitCh
	logAppServerProcessExit(t.cmd, err)
	return errors.Join(shutdownErr, ignoreExpectedAppServerExit(err))
}

func waitProcess(waitCh <-chan error, timeout time.Duration) (error, bool) {
	select {
	case err := <-waitCh:
		return err, true
	case <-time.After(timeout):
		return nil, false
	}
}

func logAppServerProcessExit(cmd *exec.Cmd, err error) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	if err == nil {
		log.Printf("app-server runner process exited: pid=%d exit_code=0", cmd.Process.Pid)
		return
	}

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ProcessState != nil {
		waitStatus, ok := exitErr.Sys().(syscall.WaitStatus)
		if ok && waitStatus.Signaled() {
			log.Printf("app-server runner process exited: pid=%d signal=%s", cmd.Process.Pid, waitStatus.Signal())
			return
		}
		if ok && waitStatus.Exited() {
			log.Printf("app-server runner process exited: pid=%d exit_code=%d", cmd.Process.Pid, waitStatus.ExitStatus())
			return
		}
	}

	log.Printf("app-server runner process wait returned: pid=%d err=%v", cmd.Process.Pid, err)
}

func signalAppServerProcessGroup(cmd *exec.Cmd, signal syscall.Signal) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}

	err := syscall.Kill(-cmd.Process.Pid, signal)
	if ignoreExpectedAppServerSignalError(err) {
		return nil
	}
	return err
}

func ignoreExpectedAppServerSignalError(err error) bool {
	if err == nil {
		return true
	}
	return errors.Is(err, os.ErrProcessDone) || errors.Is(err, syscall.ESRCH) || errors.Is(err, syscall.EPERM)
}

func ignoreExpectedAppServerExit(err error) error {
	if err == nil {
		return nil
	}

	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return fmt.Errorf("wait for process: %w", err)
	}

	waitStatus, ok := exitErr.Sys().(syscall.WaitStatus)
	if !ok {
		return fmt.Errorf("wait for process: %w", err)
	}
	if !waitStatus.Signaled() {
		return fmt.Errorf("wait for process: %w", err)
	}

	switch waitStatus.Signal() {
	case syscall.SIGINT, syscall.SIGTERM, syscall.SIGKILL:
		return nil
	default:
		return fmt.Errorf("wait for process: %w", err)
	}
}
