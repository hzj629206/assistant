package tunnel

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

const cloudflareTerminateTimeout = 5 * time.Second

// CloudflaredConfig contains the cloudflared process parameters.
type CloudflaredConfig struct {
	Token           string
	LocalTargetAddr string
	RetryDelay      time.Duration
	MaxRetryDelay   time.Duration
}

// NewCloudflared creates a cloudflared-backed public tunnel.
func NewCloudflared(cfg CloudflaredConfig) (Tunnel, error) {
	if strings.TrimSpace(cfg.Token) == "" {
		return nil, nil
	}

	localTargetAddr, err := validateLocalTargetAddr(cfg.LocalTargetAddr)
	if err != nil {
		return nil, err
	}

	executable, err := exec.LookPath("cloudflared")
	if err != nil {
		return nil, fmt.Errorf("locate cloudflared failed: %w", err)
	}

	localTargetURL, err := deriveLocalTargetURL(localTargetAddr)
	if err != nil {
		return nil, err
	}

	return &cloudflaredTunnel{
		executable:     executable,
		token:          cfg.Token,
		localTargetURL: localTargetURL,
		retryDelay:     cfg.RetryDelay,
		maxRetryDelay:  cfg.MaxRetryDelay,
	}, nil
}

type cloudflaredTunnel struct {
	executable     string
	token          string
	localTargetURL string
	retryDelay     time.Duration
	maxRetryDelay  time.Duration

	cancel context.CancelFunc
	done   chan struct{}

	mu  sync.Mutex
	cmd *exec.Cmd
}

//nolint:contextcheck // Tunnel lifecycles must outlive the startup timeout context passed by the caller.
func (r *cloudflaredTunnel) Start(_ context.Context) {
	if r == nil {
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	r.done = make(chan struct{})

	go func() {
		defer close(r.done)
		r.run(ctx)
	}()
}

func (r *cloudflaredTunnel) Shutdown(ctx context.Context) error {
	if r == nil {
		return nil
	}

	if r.cancel != nil {
		r.cancel()
	}

	if err := r.stopCurrentCommand(ctx); err != nil {
		return err
	}

	if r.done == nil {
		return nil
	}

	select {
	case <-r.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *cloudflaredTunnel) run(ctx context.Context) {
	baseDelay := retryDelay(r.retryDelay)
	delay := baseDelay
	maxDelay := max(r.maxRetryDelay, delay)

	for {
		if ctx.Err() != nil {
			return
		}

		//nolint:gosec // Executable is resolved from PATH once and arguments come from explicit local config.
		cmd := exec.CommandContext(ctx, r.executable, cloudflaredArgs(r.token)...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

		log.Printf("cloudflared tunnel starting: %s, expect route: %s", r.executable, r.localTargetURL)
		if err := cmd.Start(); err != nil {
			log.Printf("cloudflared tunnel unavailable: %v", err)
			if !waitWithContext(ctx, delay) {
				return
			}
			delay = nextRetryDelay(delay, maxDelay)
			continue
		}

		delay = baseDelay
		r.mu.Lock()
		r.cmd = cmd
		r.mu.Unlock()

		err := cmd.Wait()
		r.clearCurrentCommand(cmd)

		if ctx.Err() != nil {
			return
		}
		if err != nil {
			log.Printf("cloudflared tunnel stopped: %v", err)
		} else {
			log.Printf("cloudflared tunnel stopped, reconnecting")
		}
		if !waitWithContext(ctx, delay) {
			return
		}
	}
}

func (r *cloudflaredTunnel) stopCurrentCommand(ctx context.Context) error {
	r.mu.Lock()
	cmd := r.cmd
	r.mu.Unlock()

	if cmd == nil || cmd.Process == nil {
		return nil
	}

	err := signalCloudflaredProcessGroup(cmd, syscall.SIGTERM)
	if err != nil {
		return err
	}

	if waitForCloudflaredCommandExit(ctx, r, cmd, cloudflareTerminateTimeout) {
		return nil
	}

	return signalCloudflaredProcessGroup(cmd, syscall.SIGKILL)
}

func (r *cloudflaredTunnel) clearCurrentCommand(cmd *exec.Cmd) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.cmd == cmd {
		r.cmd = nil
	}
}

func (r *cloudflaredTunnel) currentCommand() *exec.Cmd {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.cmd
}

func cloudflaredArgs(token string) []string {
	return []string{"tunnel", "run", "--token", token, "--protocol", "http2"}
}

func deriveLocalTargetURL(localTargetAddr string) (string, error) {
	if _, err := validateLocalTargetAddr(localTargetAddr); err != nil {
		return "", err
	}

	return "http://" + localTargetAddr, nil
}

func waitForCloudflaredCommandExit(ctx context.Context, runner *cloudflaredTunnel, cmd *exec.Cmd, timeout time.Duration) bool {
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	for {
		if runner.currentCommand() != cmd {
			return true
		}

		select {
		case <-ctx.Done():
			return false
		case <-timer.C:
			return false
		case <-ticker.C:
		}
	}
}

func signalCloudflaredProcessGroup(cmd *exec.Cmd, signal syscall.Signal) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}

	err := syscall.Kill(-cmd.Process.Pid, signal)
	if err == nil || errors.Is(err, os.ErrProcessDone) || errors.Is(err, syscall.ESRCH) {
		return nil
	}

	return err
}
