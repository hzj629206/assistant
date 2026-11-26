package acp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"
)

type protocolEnvelope struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

type protocolPeer struct {
	connection net.Conn
	encoder    *json.Encoder
	incoming   chan protocolEnvelope
}

type protocolTurnOutcome struct {
	result TurnResult
	err    error
}

func newProtocolPeer(t *testing.T) (*processSession, *protocolPeer) {
	t.Helper()
	client, remote := net.Pipe()
	if err := remote.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	session := &processSession{
		lifecycleCtx: ctx, cancel: cancel, sessionID: "root", protocolVersion: 2,
		permission: defaultPermissionHandler,
	}
	session.transport = newRPCTransport(client, client, session.handleNotification, session.handleRequest)
	session.transport.strict.Store(true)
	peer := &protocolPeer{connection: remote, encoder: json.NewEncoder(remote), incoming: make(chan protocolEnvelope, 16)}
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		session.transport.readLoop(ctx)
	}()
	go func() {
		defer close(peer.incoming)
		decoder := json.NewDecoder(remote)
		for {
			var envelope protocolEnvelope
			if err := decoder.Decode(&envelope); err != nil {
				return
			}
			select {
			case peer.incoming <- envelope:
			case <-ctx.Done():
				return
			}
		}
	}()
	t.Cleanup(func() {
		cancel()
		_ = client.Close()
		_ = remote.Close()
		<-readDone
	})
	return session, peer
}

func (p *protocolPeer) next(t *testing.T, method string) protocolEnvelope {
	t.Helper()
	select {
	case envelope, ok := <-p.incoming:
		if !ok || envelope.Method != method {
			t.Fatalf("expected method %q, got %+v (open=%t)", method, envelope, ok)
		}
		return envelope
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for method %q", method)
		return protocolEnvelope{}
	}
}

func (p *protocolPeer) send(t *testing.T, value any) {
	t.Helper()
	if err := p.encoder.Encode(value); err != nil {
		t.Fatal(err)
	}
}

func (p *protocolPeer) respond(t *testing.T, request protocolEnvelope, result any) {
	t.Helper()
	p.send(t, map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result})
}

func (p *protocolPeer) update(t *testing.T, sessionID string, update any) {
	t.Helper()
	p.send(t, map[string]any{
		"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": sessionID, "update": update},
	})
}

func (p *protocolPeer) state(t *testing.T, state, reason string) {
	t.Helper()
	p.update(t, "root", map[string]any{"sessionUpdate": "state_update", "state": state, "stopReason": reason})
}

func startProtocolTurn(t *testing.T, ctx context.Context, session *processSession) (Turn, <-chan protocolTurnOutcome) {
	t.Helper()
	turn, err := session.ScheduleTurn(ctx, []ContentBlock{{"type": "text", "text": "hello"}})
	if err != nil {
		t.Fatal(err)
	}
	outcomes := make(chan protocolTurnOutcome, 1)
	go func() {
		result, runErr := turn.Run(ctx)
		outcomes <- protocolTurnOutcome{result: result, err: runErr}
	}()
	return turn, outcomes
}

func awaitProtocolTurn(t *testing.T, outcomes <-chan protocolTurnOutcome) protocolTurnOutcome {
	t.Helper()
	select {
	case outcome := <-outcomes:
		return outcome
	case <-time.After(3 * time.Second):
		t.Fatal("turn did not finish")
		return protocolTurnOutcome{}
	}
}

func assertProtocolTurnPending(t *testing.T, outcomes <-chan protocolTurnOutcome) {
	t.Helper()
	select {
	case outcome := <-outcomes:
		t.Fatalf("turn completed before terminal state: %+v", outcome)
	case <-time.After(20 * time.Millisecond):
	}
}

func TestACPProtocolVersionNegotiation(t *testing.T) {
	for _, version := range []int{1, 2} {
		t.Run(strconv.Itoa(version), func(t *testing.T) {
			session, peer := newProtocolPeer(t)
			done := make(chan error, 1)
			go func() {
				done <- session.handshake(session.lifecycleCtx, "", "login", nil, "/tmp")
			}()
			request := peer.next(t, "initialize")
			var params map[string]any
			if err := json.Unmarshal(request.Params, &params); err != nil {
				t.Fatal(err)
			}
			if params["protocolVersion"] != float64(2) || params["info"] == nil || params["capabilities"] == nil || params["clientCapabilities"] != nil {
				t.Fatalf("unexpected v2 initialize parameters: %s", request.Params)
			}
			if version == 2 {
				peer.respond(t, request, json.RawMessage(`{"protocolVersion":2,"info":{"name":"agent","version":"test"},"capabilities":{"session":{"prompt":{"image":{},"embeddedContext":{}},"mcp":{"http":{}}}},"authMethods":[{"methodId":"login","name":"Login","type":"agent"}]}`))
				request = peer.next(t, "auth/login")
			} else {
				peer.respond(t, request, json.RawMessage(`{"protocolVersion":1,"agentCapabilities":{"promptCapabilities":{"image":true,"embeddedContext":true},"mcpCapabilities":{"http":true}},"authMethods":[{"id":"login"}]}`))
				request = peer.next(t, "authenticate")
			}
			peer.respond(t, request, map[string]any{})
			request = peer.next(t, "session/new")
			if !strings.Contains(string(request.Params), `"mcpServers":[]`) {
				t.Fatalf("empty MCP servers must be an array: %s", request.Params)
			}
			peer.respond(t, request, map[string]any{"sessionId": "root"})
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if session.Status().ProtocolVersion != version || !session.Capabilities().MCP.HTTP || !session.Capabilities().Prompt.Image || !session.Capabilities().Prompt.EmbeddedContext {
				t.Fatalf("incorrect negotiated state: %+v %+v", session.Status(), session.Capabilities())
			}
		})
	}
}

func TestACPProtocolLegacyInitializeRetry(t *testing.T) {
	session, peer := newProtocolPeer(t)
	done := make(chan error, 1)
	go func() {
		_, err := session.initialize(session.lifecycleCtx)
		done <- err
	}()
	request := peer.next(t, "initialize")
	peer.send(t, map[string]any{"id": request.ID, "error": map[string]any{"code": -32602, "message": "expected protocol 1"}})
	request = peer.next(t, "initialize")
	var params struct {
		Version      int             `json:"protocolVersion"`
		Capabilities json.RawMessage `json:"clientCapabilities"`
		Info         json.RawMessage `json:"clientInfo"`
	}
	if err := json.Unmarshal(request.Params, &params); err != nil {
		t.Fatal(err)
	}
	if params.Version != 1 || len(params.Capabilities) == 0 || len(params.Info) == 0 {
		t.Fatalf("legacy retry used wrong shape: %s", request.Params)
	}
	peer.respond(t, request, json.RawMessage(`{"protocolVersion":1}`))
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestACPProtocolRejectsUnsupportedNegotiation(t *testing.T) {
	for _, raw := range []string{
		`{"protocolVersion":3}`, `{"protocolVersion":0}`,
		`{"protocolVersion":2,"info":{"name":"test","version":"test"},"capabilities":{}}`,
		`{"protocolVersion":2,"capabilities":{"session":{}}}`,
	} {
		if _, err := decodeInitialization(json.RawMessage(raw)); err == nil {
			t.Fatalf("accepted unusable initialization: %s", raw)
		}
	}
	initialized, err := decodeInitialization(json.RawMessage(`{"protocolVersion":2,"info":{"name":"test","version":"test"},"capabilities":{"session":{"prompt":{"image":null,"embeddedContext":{}},"mcp":{"http":null}}}}`))
	if err != nil || initialized.caps.Prompt.Image || initialized.caps.MCP.HTTP || !initialized.caps.Prompt.EmbeddedContext {
		t.Fatalf("incorrect object capability detection: %+v %v", initialized, err)
	}
}

func TestACPV2MCPAndAuthCapabilityGates(t *testing.T) {
	initialized := initialization{version: 2}
	servers := []MCPServer{{Name: "tools", Type: "http", URL: "http://localhost/mcp"}}
	if _, err := initialized.prepareMCPServers(servers); err == nil {
		t.Fatal("accepted HTTP MCP without an advertised capability")
	}
	initialized.caps.MCP.HTTP = true
	prepared, err := initialized.prepareMCPServers(servers)
	if err != nil || len(prepared) != 1 || prepared[0].Type != "http" {
		t.Fatalf("advertised HTTP MCP was rejected: %+v %v", prepared, err)
	}
	initialized.mcpStdio = true
	prepared, err = initialized.prepareMCPServers([]MCPServer{{Name: "tools", Command: "sh"}})
	if err != nil || len(prepared) != 1 || prepared[0].Type != "stdio" || !strings.HasPrefix(prepared[0].Command, "/") {
		t.Fatalf("stdio MCP did not get a discriminator and absolute command: %+v %v", prepared, err)
	}
	initialized.authMethods = []authMethod{{ID: "terminal-login", Type: "terminal"}}
	if err = initialized.validateAuthMethod("terminal-login"); err == nil {
		t.Fatal("terminal authentication must not use auth/login")
	}
}

func TestACPV2TurnWaitsForIdleAndAppliesMessageUpserts(t *testing.T) {
	for _, responseFirst := range []bool{true, false} {
		session, peer := newProtocolPeer(t)
		_, outcomes := startProtocolTurn(t, session.lifecycleCtx, session)
		prompt := peer.next(t, "session/prompt")
		if responseFirst {
			peer.respond(t, prompt, map[string]any{"messageId": "user-1"})
		}
		peer.state(t, "idle", "")
		assertProtocolTurnPending(t, outcomes)
		peer.state(t, "running", "")
		peer.update(t, "root", json.RawMessage(`{"sessionUpdate":"user_message","messageId":"user-1","content":[{"type":"text","text":"hello"}]}`))
		peer.update(t, "root", json.RawMessage(`{"sessionUpdate":"agent_message_chunk","messageId":"a","content":{"type":"text","text":"stale"}}`))
		peer.update(t, "root", json.RawMessage(`{"sessionUpdate":"agent_message","messageId":"b","content":[{"type":"text","text":" tail"}]}`))
		peer.update(t, "root", json.RawMessage(`{"sessionUpdate":"agent_thought","messageId":"a","content":[{"type":"text","text":"private reasoning"}]}`))
		peer.update(t, "root", json.RawMessage(`{"sessionUpdate":"agent_message","messageId":"a","content":[]}`))
		peer.update(t, "root", json.RawMessage(`{"sessionUpdate":"agent_message_chunk","messageId":"a","content":{"type":"text","text":"first"}}`))
		peer.update(t, "root", json.RawMessage(`{"sessionUpdate":"agent_message","messageId":"a","content":null}`))
		peer.update(t, "root", json.RawMessage(`{"sessionUpdate":"agent_message","messageId":"a","content":[{"type":"text","text":" final"},{"type":"text","text":" answer"}]}`))
		peer.update(t, "root", json.RawMessage(`{"sessionUpdate":"agent_message","messageId":"a","_meta":{"title":"unchanged content"}}`))
		peer.update(t, "root", json.RawMessage(`{"sessionUpdate":"subagent_update","sessionId":"child"}`))
		peer.update(t, "child", json.RawMessage(`{"sessionUpdate":"state_update","state":"running"}`))
		peer.update(t, "child", json.RawMessage(`{"sessionUpdate":"agent_message_chunk","messageId":"a","content":{"type":"text","text":"child output"}}`))
		peer.update(t, "child", json.RawMessage(`{"sessionUpdate":"state_update","state":"idle","stopReason":"end_turn"}`))
		peer.update(t, "root", json.RawMessage(`{"sessionUpdate":"_future_update","value":true}`))
		assertProtocolTurnPending(t, outcomes)
		peer.state(t, "idle", "end_turn")
		if !responseFirst {
			assertProtocolTurnPending(t, outcomes)
			// The foreground work can finish before the insertion response.
			peer.update(t, "root", json.RawMessage(`{"sessionUpdate":"agent_message_chunk","messageId":"a","content":{"type":"text","text":"background output"}}`))
			peer.respond(t, prompt, map[string]any{"messageId": "user-1"})
		}
		outcome := awaitProtocolTurn(t, outcomes)
		if outcome.err != nil || outcome.result.ReplyText != " final answer tail" || outcome.result.SessionID != "root" || session.SessionID() != "root" {
			t.Fatalf("incorrect foreground result: %+v", outcome)
		}
		if !strings.Contains(string(outcome.result.RawResult), `"stopReason":"end_turn"`) {
			t.Fatalf("lost terminal state: %s", outcome.result.RawResult)
		}
	}
}

func TestACPV2FailureAndUnknownStopReasons(t *testing.T) {
	for _, reason := range []string{"error", "cancelled", "_future_reason", ""} { //nolint:misspell // ACP protocol spelling.
		t.Run(reason, func(t *testing.T) {
			session, peer := newProtocolPeer(t)
			_, outcomes := startProtocolTurn(t, session.lifecycleCtx, session)
			prompt := peer.next(t, "session/prompt")
			peer.respond(t, prompt, map[string]any{"messageId": "user-1"})
			peer.state(t, "requires_action", "")
			peer.update(t, "root", json.RawMessage(`{"sessionUpdate":"agent_message_chunk","messageId":"a","content":{"type":"text","text":"partial"}}`))
			peer.update(t, "root", map[string]any{
				"sessionUpdate": "state_update", "state": "idle", "stopReason": reason,
				"error": map[string]any{"code": -32000, "message": "authentication expired", "data": map[string]any{"retry": false}},
			})
			outcome := awaitProtocolTurn(t, outcomes)
			switch reason {
			case "error":
				var rpcErr *rpcError
				if !errors.As(outcome.err, &rpcErr) || rpcErr.Code != -32000 || !strings.Contains(string(rpcErr.Data), "retry") || outcome.result.ReplyText != "" {
					t.Fatalf("lost foreground error: %+v", outcome)
				}
			case "cancelled": //nolint:misspell // ACP protocol spelling.
				if !errors.Is(outcome.err, context.Canceled) {
					t.Fatalf("expected cancellation, got %+v", outcome)
				}
			default:
				if outcome.err != nil || outcome.result.ReplyText != "partial" {
					t.Fatalf("unknown/absent stop reason did not finish: %+v", outcome)
				}
			}
		})
	}
}

func TestACPV2CancellationDrainsBeforeSessionReuse(t *testing.T) {
	for _, cancelContext := range []bool{false, true} {
		session, peer := newProtocolPeer(t)
		turnCtx, cancel := context.WithCancel(session.lifecycleCtx)
		defer cancel()
		turn, outcomes := startProtocolTurn(t, turnCtx, session)
		prompt := peer.next(t, "session/prompt")
		peer.respond(t, prompt, map[string]any{"messageId": "user-1"})
		peer.state(t, "running", "")
		interrupted := make(chan error, 1)
		if cancelContext {
			cancel()
		} else {
			go func() { interrupted <- turn.Interrupt(session.lifecycleCtx) }()
		}
		peer.next(t, "session/cancel")
		peer.update(t, "root", json.RawMessage(`{"sessionUpdate":"agent_message_chunk","messageId":"a","content":{"type":"text","text":"old turn cleanup"}}`))
		assertProtocolTurnPending(t, outcomes)
		peer.state(t, "idle", "cancelled") //nolint:misspell // ACP protocol spelling.
		if outcome := awaitProtocolTurn(t, outcomes); !errors.Is(outcome.err, context.Canceled) {
			t.Fatalf("expected cancellation: %+v", outcome)
		}
		if !cancelContext {
			if err := <-interrupted; err != nil {
				t.Fatal(err)
			}
		}
		_, next := startProtocolTurn(t, session.lifecycleCtx, session)
		prompt = peer.next(t, "session/prompt")
		peer.respond(t, prompt, map[string]any{"messageId": "user-2"})
		peer.state(t, "running", "")
		peer.update(t, "root", json.RawMessage(`{"sessionUpdate":"agent_message_chunk","messageId":"b","content":{"type":"text","text":"new turn"}}`))
		peer.state(t, "idle", "end_turn")
		if outcome := awaitProtocolTurn(t, next); outcome.err != nil || outcome.result.ReplyText != "new turn" {
			t.Fatalf("session reuse mixed turns: %+v", outcome)
		}
	}
}

func TestACPV2PermissionCancellationAndChildRouting(t *testing.T) {
	session, peer := newProtocolPeer(t)
	started := make(chan PermissionRequest, 1)
	stopped := make(chan struct{})
	session.permission = func(ctx context.Context, request PermissionRequest) (PermissionDecision, error) {
		if request.Title == "slow" {
			started <- request
			<-ctx.Done()
			close(stopped)
			return PermissionDecision{}, ctx.Err()
		}
		return PermissionDecision{Allow: true, OptionID: "allow"}, nil
	}
	peer.update(t, "root", json.RawMessage(`{"sessionUpdate":"subagent_update","sessionId":"child"}`))
	permission := json.RawMessage(`{"sessionId":"child","title":"slow","description":"approve command","subject":{"type":"command","command":"pwd","cwd":"/tmp"},"options":[{"optionId":"allow","kind":"allow_once","name":"Allow"}]}`)
	peer.send(t, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "session/request_permission", "params": permission})
	select {
	case request := <-started:
		if request.SessionID != "child" || request.Description != "approve command" || !strings.Contains(string(request.Subject), "command") {
			t.Fatalf("permission lost child context: %+v", request)
		}
	case <-time.After(time.Second):
		t.Fatal("permission handler did not run")
	}
	// A string ID "1" is independent from the numeric ID 1, and must be
	// serviced while the slow handler is still pending.
	peer.send(t, map[string]any{"jsonrpc": "2.0", "id": "1", "method": "session/request_permission", "params": json.RawMessage(strings.Replace(string(permission), `"slow"`, `"fast"`, 1))})
	response := peer.next(t, "")
	if string(response.ID) != `"1"` || !strings.Contains(string(response.Result), `"selected"`) {
		t.Fatalf("concurrent permission request blocked: %+v", response)
	}
	peer.send(t, map[string]any{"jsonrpc": "2.0", "method": "$/cancel_request", "params": map[string]any{"requestId": 1}})
	response = peer.next(t, "")
	if string(response.ID) != "1" || !strings.Contains(string(response.Result), `"cancelled"`) { //nolint:misspell // ACP protocol spelling.
		t.Fatalf("missing canceled response: %+v", response)
	}
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("agent cancellation did not reach handler")
	}
	peer.send(t, map[string]any{"method": "$/cancel_request", "params": map[string]any{"requestId": 1}})
	peer.send(t, map[string]any{"id": "last", "method": "session/request_permission", "params": json.RawMessage(strings.Replace(string(permission), `"slow"`, `"fast"`, 1))})
	if response = peer.next(t, ""); string(response.ID) != `"last"` {
		t.Fatalf("duplicate response after cancellation: %+v", response)
	}
	if session.SessionID() != "root" {
		t.Fatalf("child changed root ID: %s", session.SessionID())
	}
}

func TestACPV2ResumeReplayAndConfigOptions(t *testing.T) {
	session, peer := newProtocolPeer(t)
	done := make(chan error, 1)
	go func() { done <- session.resumeV2Session(session.lifecycleCtx, "root", "/tmp", []MCPServer{}) }()
	request := peer.next(t, "session/resume")
	if !strings.Contains(string(request.Params), `"replayFrom":{"type":"start"}`) {
		t.Fatalf("resume did not request replay: %s", request.Params)
	}
	peer.update(t, "root", json.RawMessage(`{"sessionUpdate":"agent_message","messageId":"historical","content":[{"type":"text","text":"history"}]}`))
	peer.update(t, "root", json.RawMessage(`{"sessionUpdate":"config_option_update","configOptions":[{"configId":"obsolete","name":"Obsolete"}]}`))
	peer.update(t, "root", json.RawMessage(`{"sessionUpdate":"replay_complete"}`))
	peer.state(t, "idle", "")
	peer.respond(t, request, json.RawMessage(`{"configOptions":[{"configId":"model","name":"Model","type":"select","currentValue":"new","options":[]},{"configId":"fast","name":"Fast","type":"boolean","currentValue":false}]}`))
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(session.Status().ConfigOptions), "obsolete") || !strings.Contains(string(session.Status().ConfigOptions), "model") {
		t.Fatalf("setup did not replace config list: %+v", session.Status())
	}
	peer.update(t, "root", json.RawMessage(`{"sessionUpdate":"config_option_update","configOptions":[]}`))
	// Sending another request is a barrier ensuring preceding notifications
	// have been applied by the reader before inspecting status.
	peer.send(t, map[string]any{"id": "barrier", "method": "_test/barrier", "params": map[string]any{}})
	peer.next(t, "")
	if string(session.Status().ConfigOptions) != "[]" {
		t.Fatalf("config update did not clear the list: %+v", session.Status())
	}
}

func TestACPV2TransportClosureAfterAcceptance(t *testing.T) {
	session, peer := newProtocolPeer(t)
	_, outcomes := startProtocolTurn(t, session.lifecycleCtx, session)
	prompt := peer.next(t, "session/prompt")
	peer.respond(t, prompt, map[string]any{"messageId": "user-1"})
	peer.state(t, "running", "")
	_ = peer.connection.Close()
	outcome := awaitProtocolTurn(t, outcomes)
	if outcome.err == nil || !strings.Contains(outcome.err.Error(), "transport closed") {
		t.Fatalf("expected transport failure: %+v", outcome)
	}
	_, err := session.transport.call(session.lifecycleCtx, "session/new", map[string]any{})
	if err == nil || errors.Is(err, io.EOF) {
		t.Fatalf("expected diagnostic for closed transport, got %v", err)
	}
}

func TestACPV2CancellationBeforeInsertion(t *testing.T) {
	session, peer := newProtocolPeer(t)
	turnCtx, cancel := context.WithCancel(session.lifecycleCtx)
	defer cancel()
	_, outcomes := startProtocolTurn(t, turnCtx, session)
	prompt := peer.next(t, "session/prompt")
	cancel()
	peer.next(t, "session/cancel")
	peer.send(t, map[string]any{"id": prompt.ID, "error": map[string]any{"code": -32800, "message": "Request cancelled"}}) //nolint:misspell // ACP protocol spelling.
	if outcome := awaitProtocolTurn(t, outcomes); !errors.Is(outcome.err, context.Canceled) {
		t.Fatalf("pre-insertion cancellation was not reported: %+v", outcome)
	}
}

func TestACPV2MalformedInsertionResponseStopsConnection(t *testing.T) {
	session, peer := newProtocolPeer(t)
	_, outcomes := startProtocolTurn(t, session.lifecycleCtx, session)
	prompt := peer.next(t, "session/prompt")
	peer.respond(t, prompt, map[string]any{})
	if outcome := awaitProtocolTurn(t, outcomes); outcome.err == nil || !strings.Contains(outcome.err.Error(), "messageId") {
		t.Fatalf("accepted malformed insertion response: %+v", outcome)
	}
	if session.lifecycleCtx.Err() == nil {
		t.Fatal("malformed peer remained available for another turn")
	}
}

func TestACPV2SetupResponseAndNotificationsKeepWireOrder(t *testing.T) {
	session, peer := newProtocolPeer(t)
	done := make(chan error, 1)
	go func() { done <- session.newSession(session.lifecycleCtx, "/tmp", []MCPServer{}) }()
	request := peer.next(t, "session/new")
	peer.update(t, "root", json.RawMessage(`{"sessionUpdate":"config_option_update","configOptions":[{"configId":"before","name":"before"}]}`))
	peer.respond(t, request, json.RawMessage(`{"sessionId":"root","configOptions":[{"configId":"initial","name":"initial"}]}`))
	peer.update(t, "root", json.RawMessage(`{"sessionUpdate":"config_option_update","configOptions":[{"configId":"after","name":"after"}]}`))
	peer.send(t, map[string]any{"id": "barrier", "method": "_test/barrier"})
	peer.next(t, "")
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(session.Status().ConfigOptions), "after") {
		t.Fatalf("setup response overwrote a newer config update: %+v", session.Status())
	}
}

func TestACPV2TransportRejectsNullIDsAndBatches(t *testing.T) {
	session, peer := newProtocolPeer(t)
	for _, raw := range []string{`{"jsonrpc":"2.0","id":null,"method":"session/request_permission","params":{}}`, `[]`} {
		peer.send(t, json.RawMessage(raw))
		response := peer.next(t, "")
		if response.Error == nil || response.Error.Code != -32600 {
			t.Fatalf("invalid JSON-RPC request was accepted: %+v", response)
		}
	}
	if session.SessionID() != "root" {
		t.Fatal("invalid request changed session")
	}
}
