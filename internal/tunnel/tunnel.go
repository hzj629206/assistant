package tunnel

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"
)

// Tunnel defines the lifecycle required by daemon-managed public tunnel implementations.
type Tunnel interface {
	Start(ctx context.Context)
	Shutdown(ctx context.Context) error
}

func validateLocalTargetAddr(localTargetAddr string) (string, error) {
	localTargetAddr = strings.TrimSpace(localTargetAddr)
	host, port, err := net.SplitHostPort(localTargetAddr)
	if err != nil {
		return "", fmt.Errorf("split local target addr %q failed: %w", localTargetAddr, err)
	}
	if host == "" {
		return "", errors.New("local target addr host is required")
	}
	if port == "" {
		return "", errors.New("local target addr port is required")
	}

	return localTargetAddr, nil
}

func retryDelay(delay time.Duration) time.Duration {
	if delay <= 0 {
		return 3 * time.Second
	}
	return delay
}

func nextRetryDelay(delay time.Duration, maxDelay time.Duration) time.Duration {
	delay *= 2
	if delay > maxDelay {
		return maxDelay
	}
	return delay
}

func waitWithContext(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
