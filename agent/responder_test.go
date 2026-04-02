package agent

import (
	"testing"
	"time"
)

func TestTypingStatusControllerSkipsFastTurns(t *testing.T) {
	t.Parallel()

	responder := &testRunnerResponder{}
	controller := newTypingStatusController(responder, 40*time.Millisecond, 40*time.Millisecond)
	controller.Start(t.Context())
	time.Sleep(15 * time.Millisecond)
	controller.Stop()

	if responder.typingCalls != 0 {
		t.Fatalf("unexpected typing call count: %d", responder.typingCalls)
	}
}

func TestTypingStatusControllerRefreshesSlowTurns(t *testing.T) {
	t.Parallel()

	responder := &testRunnerResponder{}
	controller := newTypingStatusController(responder, 10*time.Millisecond, 20*time.Millisecond)
	controller.Start(t.Context())
	time.Sleep(55 * time.Millisecond)
	controller.Stop()

	if responder.typingCalls < 2 {
		t.Fatalf("expected at least 2 typing calls, got %d", responder.typingCalls)
	}
}
