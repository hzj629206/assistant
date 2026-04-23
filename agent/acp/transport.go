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
	"strings"
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
	pending   map[string]chan rpcOutcome
	onNotify  func(string, json.RawMessage)
	onRequest func(string, json.RawMessage, json.RawMessage)
}

type rpcOutcome struct {
	result json.RawMessage
	err    *rpcError
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
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
		pending:   make(map[string]chan rpcOutcome),
		onNotify:  onNotify,
		onRequest: onRequest,
	}
}

func (t *rpcTransport) readLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		line, err := t.input.ReadBytes('\n')
		if err != nil {
			if !errors.Is(err, io.EOF) {
				log.Printf("acp read loop stopped: err=%v", err)
			}
			t.cancelAll(err)
			return
		}
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		t.dispatch(line)
	}
}

func (t *rpcTransport) dispatch(line []byte) {
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
	id := t.nextID.Add(1)
	key := strconv.FormatInt(id, 10)
	wait := make(chan rpcOutcome, 1)
	t.pendingMu.Lock()
	t.pending[key] = wait
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

	select {
	case <-ctx.Done():
		t.pendingMu.Lock()
		delete(t.pending, key)
		t.pendingMu.Unlock()
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
	key := strings.Trim(string(bytes.TrimSpace(id)), "\"")
	t.pendingMu.Lock()
	wait, ok := t.pending[key]
	delete(t.pending, key)
	t.pendingMu.Unlock()
	if !ok {
		return
	}
	wait <- rpcOutcome{result: result, err: rpcErr}
}

func (t *rpcTransport) cancelAll(err error) {
	message := "transport closed"
	if err != nil {
		message = err.Error()
	}
	t.pendingMu.Lock()
	defer t.pendingMu.Unlock()
	for key, wait := range t.pending {
		wait <- rpcOutcome{err: &rpcError{Code: defaultReadLoopErrorCode, Message: message}}
		delete(t.pending, key)
	}
}

func (t *rpcTransport) respondSuccess(id json.RawMessage, result any) error {
	return t.writeJSON(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"result":  result,
	})
}

func (t *rpcTransport) respondError(id json.RawMessage, code int, message string) error {
	return t.writeJSON(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error": map[string]any{
			"code":    code,
			"message": message,
		},
	})
}

func (t *rpcTransport) writeJSON(payload any) error {
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	return t.encoder.Encode(payload)
}
