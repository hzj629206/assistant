package claudecode

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestClaudeResultError(t *testing.T) {
	t.Parallel()

	status := 529
	for _, tt := range []struct {
		name   string
		result ClaudeResult
		want   string
	}{
		{"execution", ClaudeResult{IsError: true, Errors: []string{" first failure ", "", "second failure"}, Result: "partial text"}, "first failure; second failure"},
		{"api", ClaudeResult{Subtype: "success", IsError: true, Result: "API Error: overloaded"}, "API Error: overloaded"},
		{"legacy", ClaudeResult{Subtype: "error_max_turns"}, "error_max_turns"},
		{"startup", ClaudeResult{IsError: true, StartupFailureReason: "config_file_invalid"}, "config_file_invalid"},
		{"status", ClaudeResult{Subtype: "success", IsError: true, APIErrorStatus: &status}, "API error (HTTP 529)"},
		{"unknown", ClaudeResult{IsError: true, Subtype: "success"}, "unknown error"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.result.Err()
			var resultErr *ResultError
			if !errors.As(err, &resultErr) || resultErr.Result != &tt.result {
				t.Fatalf("expected error to preserve result, got %v", err)
			}
			if !strings.HasSuffix(err.Error(), tt.want) {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
	if err := (&ClaudeResult{Subtype: "success"}).Err(); err != nil {
		t.Fatalf("successful result: %v", err)
	}
}

func TestClaudeProtocolErrorAndSessionReuse(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	session, err := StartSession(ctx, SessionOptions{Client: protocolTestClient(t, "errors")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })

	turn, err := session.ScheduleTurn(ctx, []map[string]any{{"type": "text", "text": "first"}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := turn.Run(ctx)
	var resultErr *ResultError
	if !errors.As(err, &resultErr) || !strings.Contains(err.Error(), "service unavailable") {
		t.Fatalf("expected CLI diagnostic, got result=%+v err=%v", result, err)
	}
	if result.Result != "" || result.APIErrorStatus == nil || *result.APIErrorStatus != 503 || result.TerminalReason != "api_error" {
		t.Fatalf("lost error details or used partial text as result: %+v", result)
	}
	if session.SessionID() != "session-error" {
		t.Fatalf("lost session ID after error: %s", session.SessionID())
	}

	turn, err = session.ScheduleTurn(ctx, []map[string]any{{"type": "text", "text": "second"}})
	if err != nil {
		t.Fatal(err)
	}
	result, err = turn.Run(ctx)
	if err != nil || result.Result != "recovered" {
		t.Fatalf("second turn failed: result=%+v err=%v", result, err)
	}
}

func TestClaudeProtocolConcurrentControlCancellation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	started := make(chan struct{})
	canceled := make(chan struct{})
	session, err := StartSession(ctx, SessionOptions{
		Client: protocolTestClient(t, "controls"),
		Hooks: SessionHooks{HandleControlRequest: func(requestCtx context.Context, request map[string]any) (map[string]any, error) {
			if request["tool_name"] == "slow" {
				close(started)
				<-requestCtx.Done()
				close(canceled)
				return nil, requestCtx.Err()
			}
			select {
			case <-started:
			case <-requestCtx.Done():
				return nil, requestCtx.Err()
			}
			return map[string]any{"behavior": "allow", "updatedInput": request["input"]}, nil
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	for range 2 {
		turn, scheduleErr := session.ScheduleTurn(ctx, []map[string]any{{"type": "text", "text": "run"}})
		if scheduleErr != nil {
			t.Fatal(scheduleErr)
		}
		result, runErr := turn.Run(ctx)
		if runErr != nil || result.Result != "ok" {
			t.Fatalf("control requests blocked turn: result=%+v err=%v", result, runErr)
		}
	}
	select {
	case <-canceled:
	case <-ctx.Done():
		t.Fatal("cancellation did not reach the handler")
	}
}

func TestClaudeProtocolStartupFailure(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := StartSession(ctx, SessionOptions{Client: protocolTestClient(t, "startup-error")})
	var resultErr *ResultError
	if !errors.As(err, &resultErr) || resultErr.Result.StartupFailureReason != "config_file_invalid" {
		t.Fatalf("expected startup result instead of exit status: %v", err)
	}
}

func TestClaudeProtocolInterruptAndReuse(t *testing.T) {
	for _, mode := range []string{"interrupt", "context-cancel"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			started := make(chan struct{})
			receipt := make(chan struct{})
			release := make(chan struct{})
			session, err := StartSession(ctx, SessionOptions{
				Client: protocolTestClient(t, mode),
				Hooks: SessionHooks{HandleControlRequest: func(requestCtx context.Context, request map[string]any) (map[string]any, error) {
					if request["tool_name"] == "slow" {
						close(started)
						<-requestCtx.Done()
						return nil, requestCtx.Err()
					}
					close(receipt)
					select {
					case <-release:
						return map[string]any{}, nil
					case <-requestCtx.Done():
						return nil, requestCtx.Err()
					}
				}},
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = session.Close() })
			first, err := session.ScheduleTurn(ctx, []map[string]any{{"type": "text", "text": "first"}})
			if err != nil {
				t.Fatal(err)
			}
			turnCtx, cancelTurn := context.WithCancel(ctx)
			defer cancelTurn()
			finished := make(chan error, 1)
			go func() {
				_, runErr := first.Run(turnCtx)
				finished <- runErr
			}()
			select {
			case <-started:
			case <-ctx.Done():
				t.Fatal("first turn did not start")
			}
			scheduled := make(chan Turn, 1)
			scheduleErr := make(chan error, 1)
			if mode == "context-cancel" {
				cancelTurn()
			} else {
				go func() {
					next, nextErr := session.ScheduleTurn(ctx, []map[string]any{{"type": "text", "text": "second"}})
					scheduled <- next
					scheduleErr <- nextErr
				}()
			}
			select {
			case <-receipt:
			case <-ctx.Done():
				t.Fatal("interrupt did not pass the running tool callback")
			}
			select {
			case <-first.Done():
				t.Fatal("turn completed on interrupt receipt before its terminal result")
			default:
			}
			close(release)
			select {
			case runErr := <-finished:
				if !errors.Is(runErr, context.Canceled) {
					t.Fatalf("interrupted turn returned %v", runErr)
				}
			case <-ctx.Done():
				t.Fatal("interrupted turn did not finish")
			}
			var second Turn
			if mode == "context-cancel" {
				second, err = session.ScheduleTurn(ctx, []map[string]any{{"type": "text", "text": "second"}})
			} else {
				select {
				case second = <-scheduled:
					err = <-scheduleErr
				case <-ctx.Done():
					t.Fatal("replacement turn was not scheduled")
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			result, err := second.Run(ctx)
			if err != nil || result.Result != "ok" {
				t.Fatalf("replacement turn failed: result=%+v err=%v", result, err)
			}
		})
	}
}

func TestClaudeProtocolPreemptBeforeRun(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	session, err := StartSession(ctx, SessionOptions{Client: protocolTestClient(t, "controls")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	first, err := session.ScheduleTurn(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = session.ScheduleTurn(ctx, nil); err != nil {
		t.Fatalf("could not preempt an unstarted turn: %v", err)
	}
	if _, err = first.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("preempted turn returned %v", err)
	}
}

func protocolTestClient(t *testing.T, mode string) *ClaudeClient {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("ASSISTANT_CLAUDE_PROTOCOL_TEST", mode)
	t.Setenv("ASSISTANT_CLAUDE_TEST_EXECUTABLE", executable)
	path := filepath.Join(t.TempDir(), "claude")
	script := "#!/bin/sh\nexec \"$ASSISTANT_CLAUDE_TEST_EXECUTABLE\" -test.run='^TestClaudeProtocolProcess$'\n"
	//nolint:gosec // The subprocess transport requires an executable test fixture.
	if err = os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return NewClient(path)
}

// TestClaudeProtocolProcess replays CLI envelopes in a separate test process.
func TestClaudeProtocolProcess(t *testing.T) {
	mode := os.Getenv("ASSISTANT_CLAUDE_PROTOCOL_TEST")
	if mode == "" {
		return
	}
	if err := replayClaudeProtocol(mode); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

func replayClaudeProtocol(mode string) error {
	input := bufio.NewScanner(os.Stdin)
	output := json.NewEncoder(os.Stdout)
	read := func(wantType string) (map[string]any, error) {
		if !input.Scan() {
			return nil, fmt.Errorf("expected %s, input ended", wantType)
		}
		var message map[string]any
		if err := json.Unmarshal(input.Bytes(), &message); err != nil {
			return nil, err
		}
		if message["type"] != wantType {
			return nil, fmt.Errorf("expected %s, got %v", wantType, message)
		}
		return message, nil
	}
	init, err := read("control_request")
	if err != nil {
		return err
	}
	if mode == "startup-error" {
		if err = output.Encode(map[string]any{"type": "result", "subtype": "error_during_execution", "is_error": true, "errors": []string{"invalid configuration"}, "startup_failure_reason": "config_file_invalid"}); err != nil {
			return err
		}
		return errors.New("startup failed")
	}
	if err = output.Encode(map[string]any{"type": "control_response", "response": map[string]any{"request_id": init["request_id"], "subtype": "success", "response": map[string]any{}}}); err != nil {
		return err
	}
	if err = output.Encode(map[string]any{"type": "system", "subtype": "init", "capabilities": []string{"interrupt_cancel_queued_v1"}}); err != nil {
		return err
	}
	var previousID string
	for index := range 2 {
		user, readErr := read("user")
		if readErr != nil {
			return readErr
		}
		id, _ := user["uuid"].(string)
		if id == "" {
			return errors.New("user input missing UUID")
		}
		if (mode == "interrupt" || mode == "context-cancel") && index == 0 {
			previousID = id
			if err = output.Encode(map[string]any{"type": "control_request", "request_id": "slow", "request": map[string]any{"subtype": "can_use_tool", "tool_name": "slow"}}); err != nil {
				return err
			}
			interrupt, interruptErr := read("control_request")
			if interruptErr != nil {
				return interruptErr
			}
			request, _ := interrupt["request"].(map[string]any)
			if interrupt["request_id"] != "interrupt-"+id || request["cancel_queued"] != true {
				return fmt.Errorf("interrupt missing turn ID or advertised capability: %v", interrupt)
			}
			for _, message := range []map[string]any{
				{"type": "control_cancel_request", "request_id": "slow"},
				{"type": "control_response", "response": map[string]any{"request_id": interrupt["request_id"], "subtype": "success", "response": map[string]any{}}},
				{"type": "control_request", "request_id": "release", "request": map[string]any{"subtype": "can_use_tool", "tool_name": "release"}},
			} {
				if err = output.Encode(message); err != nil {
					return err
				}
			}
			if _, err = read("control_response"); err != nil {
				return err
			}
			if err = output.Encode(map[string]any{"type": "result", "subtype": "success", "user_message_uuid": id}); err != nil {
				return err
			}
			continue
		}
		if previousID != "" {
			if err = output.Encode(map[string]any{"type": "result", "subtype": "success", "result": "stale", "user_message_uuid": previousID}); err != nil {
				return err
			}
		}
		if mode == "errors" && index == 0 {
			if err = output.Encode(map[string]any{"type": "assistant", "user_message_uuid": id, "message": map[string]any{"role": "assistant", "content": []map[string]any{{"type": "text", "text": "incomplete output"}}}}); err != nil {
				return err
			}
			if err = output.Encode(map[string]any{"type": "result", "subtype": "error_during_execution", "is_error": true, "errors": []string{"service unavailable"}, "api_error_status": 503, "terminal_reason": "api_error", "session_id": "session-error", "user_message_uuid": id}); err != nil {
				return err
			}
			continue
		}
		if mode == "controls" && index == 0 {
			for _, name := range []string{"slow", "fast"} {
				if err = output.Encode(map[string]any{"type": "control_request", "request_id": name, "request": map[string]any{"subtype": "can_use_tool", "tool_name": name, "input": map[string]any{}}}); err != nil {
					return err
				}
			}
			response, responseErr := read("control_response")
			if responseErr != nil {
				return responseErr
			}
			body, _ := response["response"].(map[string]any)
			if body["request_id"] != "fast" || body["subtype"] != "success" {
				return fmt.Errorf("unexpected control response: %v", response)
			}
			if err = output.Encode(map[string]any{"type": "control_cancel_request", "request_id": "slow"}); err != nil {
				return err
			}
		}
		text := "ok"
		if mode == "errors" {
			text = "recovered"
		}
		if err = output.Encode(map[string]any{"type": "result", "subtype": "success", "result": text, "user_message_uuid": id}); err != nil {
			return err
		}
	}
	// Keep the persistent subprocess alive until its owner closes stdin.
	for input.Scan() {
		return fmt.Errorf("unexpected late response: %s", input.Text())
	}
	return input.Err()
}
