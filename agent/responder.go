package agent

import (
	"context"
	"log"
	"sync"
	"time"
)

const (
	defaultTypingInitialDelay    = time.Second
	defaultTypingRefreshCooldown = 3 * time.Second
)

// Responder handles side effects for one inbound conversation turn.
type Responder interface {
	SendText(ctx context.Context, text string) error
	SetTyping(ctx context.Context) error
	Cleanup(ctx context.Context) error
}

// LoggingResponder logs reply actions instead of delivering them.
type LoggingResponder struct{}

// SendText logs the outbound reply body.
func (LoggingResponder) SendText(_ context.Context, text string) error {
	log.Printf("agent reply suppressed: text=%q", text)
	return nil
}

// SetTyping suppresses typing notifications.
func (LoggingResponder) SetTyping(context.Context) error {
	return nil
}

// Cleanup is a no-op for the logging responder.
func (LoggingResponder) Cleanup(context.Context) error {
	return nil
}

// typingStatusController sends typing updates only for turns that last long enough,
// then refreshes the indicator at a controlled cadence while the turn is still running.
type typingStatusController struct {
	responder       Responder
	initialDelay    time.Duration
	refreshCooldown time.Duration
	stopOnce        sync.Once
	done            chan struct{}
	wg              sync.WaitGroup
}

func newTypingStatusController(responder Responder, initialDelay, refreshCooldown time.Duration) *typingStatusController {
	if responder == nil || initialDelay <= 0 || refreshCooldown <= 0 {
		return nil
	}

	return &typingStatusController{
		responder:       responder,
		initialDelay:    initialDelay,
		refreshCooldown: refreshCooldown,
		done:            make(chan struct{}),
	}
}

func (c *typingStatusController) Start(ctx context.Context) {
	if c == nil {
		return
	}

	c.wg.Go(func() {
		initialTimer := time.NewTimer(c.initialDelay)
		defer initialTimer.Stop()

		select {
		case <-ctx.Done():
			return
		case <-c.done:
			return
		case <-initialTimer.C:
		}

		_ = c.responder.SetTyping(ctx)

		refreshTicker := time.NewTicker(c.refreshCooldown)
		defer refreshTicker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-c.done:
				return
			case <-refreshTicker.C:
				_ = c.responder.SetTyping(ctx)
			}
		}
	})
}

func (c *typingStatusController) Stop() {
	if c == nil {
		return
	}

	c.stopOnce.Do(func() {
		close(c.done)
	})
	c.wg.Wait()
}
