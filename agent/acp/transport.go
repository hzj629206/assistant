package acp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"strconv"
	"sync"
	"sync/atomic"
)

const defaultReadLoopErrorCode = -32000

type rpcTransport struct {
	input     *bufio.Reader
	output    io.Writer
	encoder   *json.Encoder
	writeMu   sync.Mutex
	nextID    atomic.Int64
	pendingMu sync.Mutex
	pending   map[string]rpcPendingCall
	done      chan struct{}
	closeErr  error
	strict    atomic.Bool
	onNotify  func(string, json.RawMessage)
	onRequest func(string, json.RawMessage, json.RawMessage)
}

type rpcOutcome struct {
	result json.RawMessage
	err    *rpcError
}

type rpcPendingCall struct {
	wait     chan rpcOutcome
	onResult func(json.RawMessage)
}

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *rpcError) Error() string {
	if e == nil {
		return "json-rpc error"
	}
	return fmt.Sprintf("json-rpc %d: %s", e.Code, e.Message)
}

func newRPCTransport(input io.Reader, output io.Writer, onNotify func(string, json.RawMessage), onRequest func(string, json.RawMessage, json.RawMessage)) *rpcTransport {
	return &rpcTransport{
		input:     bufio.NewReader(input),
		output:    output,
		encoder:   json.NewEncoder(output),
		pending:   make(map[string]rpcPendingCall),
		done:      make(chan struct{}),
		onNotify:  onNotify,
		onRequest: onRequest,
	}
}

func (t *rpcTransport) readLoop(ctx context.Context) {
	defer t.cancelAll(nil)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		line, err := t.input.ReadBytes('\n')
		line = bytes.TrimSpace(line)
		if len(line) != 0 {
			t.dispatch(line)
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				log.Printf("acp read loop stopped: err=%v", err)
			}
			t.cancelAll(err)
			return
		}
	}
}

func (t *rpcTransport) dispatch(line []byte) {
	if t.strict.Load() && len(line) > 0 && line[0] == '[' {
		_ = t.respondError(json.RawMessage("null"), -32600, "JSON-RPC batches are not supported")
		return
	}
	var envelope struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
		Result json.RawMessage `json:"result"`
		Error  *rpcError       `json:"error"`
	}
	if err := json.Unmarshal(line, &envelope); err != nil {
		return
	}
	if envelope.Method != "" {
		if t.strict.Load() && bytes.Equal(bytes.TrimSpace(envelope.ID), []byte("null")) {
			_ = t.respondError(json.RawMessage("null"), -32600, "request ID must not be null")
			return
		}
		if len(bytes.TrimSpace(envelope.ID)) == 0 || bytes.Equal(bytes.TrimSpace(envelope.ID), []byte("null")) {
			if t.onNotify != nil {
				t.onNotify(envelope.Method, envelope.Params)
			}
			return
		}
		if t.onRequest != nil {
			t.onRequest(envelope.Method, envelope.ID, envelope.Params)
		}
		return
	}
	if len(bytes.TrimSpace(envelope.ID)) != 0 {
		t.completePending(envelope.ID, envelope.Result, envelope.Error)
	}
}

func (t *rpcTransport) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	return t.callWithCallbacks(ctx, method, params, nil, nil)
}

func (t *rpcTransport) callWithCallbacks(ctx context.Context, method string, params any, onSent func(), onResult func(json.RawMessage)) (json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	id := t.nextID.Add(1)
	key := strconv.FormatInt(id, 10)
	wait := make(chan rpcOutcome, 1)
	t.pendingMu.Lock()
	if t.closeErr != nil {
		err := t.closeErr
		t.pendingMu.Unlock()
		return nil, err
	}
	t.pending[key] = rpcPendingCall{wait: wait, onResult: onResult}
	t.pendingMu.Unlock()

	request := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	}
	if err := t.writeJSON(request); err != nil {
		t.pendingMu.Lock()
		delete(t.pending, key)
		t.pendingMu.Unlock()
		return nil, err
	}
	if onSent != nil {
		onSent()
	}

	select {
	case <-ctx.Done():
		t.pendingMu.Lock()
		delete(t.pending, key)
		t.pendingMu.Unlock()
		_ = t.notify("$/cancel_request", map[string]any{"requestId": id})
		return nil, ctx.Err()
	case outcome := <-wait:
		if outcome.err != nil {
			return nil, outcome.err
		}
		return outcome.result, nil
	}
}

func (t *rpcTransport) notify(method string, params any) error {
	return t.writeJSON(map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
		"params":  params,
	})
}

func (t *rpcTransport) completePending(id json.RawMessage, result json.RawMessage, rpcErr *rpcError) {
	key := string(bytes.TrimSpace(id))
	t.pendingMu.Lock()
	call, ok := t.pending[key]
	delete(t.pending, key)
	t.pendingMu.Unlock()
	if !ok {
		return
	}
	// Apply setup state on the reader goroutine so a later notification
	// cannot be overwritten by a response processed by a slower caller.
	if rpcErr == nil && call.onResult != nil {
		call.onResult(result)
	}
	call.wait <- rpcOutcome{result: result, err: rpcErr}
}

func (t *rpcTransport) cancelAll(err error) {
	message := "transport closed"
	if err != nil {
		message = err.Error()
	}
	t.pendingMu.Lock()
	defer t.pendingMu.Unlock()
	if t.closeErr != nil {
		return
	}
	t.closeErr = &rpcError{Code: defaultReadLoopErrorCode, Message: message}
	for key, call := range t.pending {
		call.wait <- rpcOutcome{err: &rpcError{Code: defaultReadLoopErrorCode, Message: message}}
		delete(t.pending, key)
	}
	close(t.done)
}

func (t *rpcTransport) error() error {
	t.pendingMu.Lock()
	defer t.pendingMu.Unlock()
	return t.closeErr
}

func (t *rpcTransport) respondSuccess(id json.RawMessage, result any) error {
	return t.writeJSON(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"result":  result,
	})
}

func (t *rpcTransport) respondError(id json.RawMessage, code int, message string) error {
	return t.respondErrorData(id, code, message, nil)
}

func (t *rpcTransport) respondErrorData(id json.RawMessage, code int, message string, data any) error {
	rpcErr := map[string]any{"code": code, "message": message}
	if data != nil {
		rpcErr["data"] = data
	}
	return t.writeJSON(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   rpcErr,
	})
}

func (t *rpcTransport) writeJSON(payload any) error {
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	return t.encoder.Encode(payload)
}
