package claudecode

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

const defaultProcessTerminateTimeout = 5 * time.Second

// ErrProcessExited reports that the Claude subprocess exited unexpectedly.
var ErrProcessExited = errors.New("claude code process exited")

// ErrSessionClosed reports that the Claude session was closed while a turn was active.
var ErrSessionClosed = errors.New("claude code session closed")

// SessionHooks provides session-scoped control callbacks for one persistent Claude session.
type SessionHooks struct {
	HandleControlRequest func(map[string]any) (map[string]any, error)
}

// SessionObserver receives optional runtime callbacks from one persistent Claude session.
type SessionObserver struct {
	OnProcessSpawn         func(client *ClaudeClient, opts RunOptions, args []string)
	OnProcessStarted       func(pid int)
	OnControlRequest       func(pid int, requestID string, subtype string)
	OnSystemMessage        func(pid int, msg *Message)
	OnAssistantMessage     func(pid int, roleMessage StreamRoleMessage)
	OnAssistantDecodeError func(pid int, err error)
	OnUserMessage          func(pid int, roleMessage StreamRoleMessage)
	OnUserDecodeError      func(pid int, err error)
	OnResultMessage        func(pid int, msg *Message, inputTokens int, outputTokens int)
	OnControlCancelRequest func(pid int, requestID string)
	OnUnsupportedMessage   func(pid int, messageType string)
	OnProcessExit          func(pid int, closed bool, waitErr error)
}

// SessionOptions configures one reusable Claude CLI session.
type SessionOptions struct {
	Client   *ClaudeClient
	Run      RunOptions
	Hooks    SessionHooks
	Observer SessionObserver
}

// Session is a reusable Claude subprocess session.
//
// ScheduleTurn should preempt any previous active turn before publishing the new turn.
type Session interface {
	ScheduleTurn(ctx context.Context, blocks []map[string]any) (Turn, error)
	SessionID() string
	Close() error
}

// Turn represents one Claude session turn that can be run or interrupted in either order.
//
// Interrupt must not return until the active turn has completed.
// Done must close at the same completion point as Run returns and Interrupt returns.
type Turn interface {
	Run(ctx context.Context) (*ClaudeResult, error)
	Interrupt(ctx context.Context) error
	Done() <-chan struct{}
}

type turnState struct {
	initRespCh         chan error
	done               chan struct{}
	interruptRequested chan struct{}
	interruptDone      chan struct{}
	interruptErr       error
	interruptSent      bool
	interruptFinished  bool
	resultCh           chan *ClaudeResult
	errCh              chan error
	assistantTextParts []string
}

type scheduledTurn struct {
	session    *persistentProcessSession
	turn       *turnState
	inputBytes []byte
}

type persistentProcessSession struct {
	runOptions RunOptions
	cmd        *exec.Cmd
	cancel     context.CancelFunc
	stdinPipe  io.WriteCloser
	waitCh     chan error
	exitDone   chan struct{}
	scanDone   chan struct{}
	observer   SessionObserver
	client     *ClaudeClient

	writeMu sync.Mutex
	turnMu  sync.Mutex
	stateMu sync.Mutex

	currentTurn *turnState
	sessionID   string
	initialized bool
	waitErr     error
	closed      bool
	hooks       SessionHooks
}

// StartSession starts one reusable Claude CLI subprocess.
func StartSession(ctx context.Context, options SessionOptions) (Session, error) {
	if ctx == nil {
		return nil, errors.New("claude code session context is nil")
	}
	client := options.Client
	if client == nil {
		return nil, errors.New("claude code client is nil")
	}
	opts := options.Run
	if err := PreprocessOptions(&opts); err != nil {
		return nil, err
	}

	session := &persistentProcessSession{
		runOptions: opts,
		sessionID:  initialPersistentSessionID(opts),
		observer:   options.Observer,
		client:     client,
		hooks:      options.Hooks,
	}
	if err := session.ensureStarted(ctx); err != nil {
		return nil, err
	}
	if err := session.initialize(ctx); err != nil {
		_ = session.Close()
		return nil, err
	}
	return session, nil
}

func initialPersistentSessionID(opts RunOptions) string {
	sessionID := strings.TrimSpace(opts.ResumeID)
	if sessionID != "" {
		return sessionID
	}
	return strings.TrimSpace(opts.SessionID)
}

func (s *persistentProcessSession) SessionID() string {
	if s == nil {
		return ""
	}
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return s.sessionID
}

func (s *persistentProcessSession) ScheduleTurn(ctx context.Context, blocks []map[string]any) (Turn, error) {
	inputBytes, err := MarshalStreamJSONUserInput(blocks)
	if err != nil {
		return nil, err
	}
	return s.scheduleTurnWithInput(ctx, inputBytes)
}

//nolint:contextcheck // ScheduleTurn uses the caller context to wait for preemption of the previous turn.
func (s *persistentProcessSession) scheduleTurnWithInput(ctx context.Context, inputBytes []byte) (Turn, error) {
	if s == nil {
		return nil, errors.New("claude session is nil")
	}
	for {
		s.turnMu.Lock()
		if err := s.ensureStarted(ctx); err != nil {
			s.turnMu.Unlock()
			return nil, err
		}
		turn := &turnState{
			done:               make(chan struct{}),
			interruptRequested: make(chan struct{}),
			resultCh:           make(chan *ClaudeResult, 1),
			errCh:              make(chan error, 1),
		}
		currentTurn, err := s.setCurrentTurn(turn)
		s.turnMu.Unlock()
		if err == nil {
			return &scheduledTurn{
				session:    s,
				turn:       turn,
				inputBytes: inputBytes,
			}, nil
		}
		if currentTurn == nil {
			return nil, err
		}
		if ctx == nil {
			ctx = context.Background()
		}
		if interruptErr := (&scheduledTurn{session: s, turn: currentTurn}).Interrupt(ctx); interruptErr != nil {
			return nil, interruptErr
		}
	}
}

func (s *persistentProcessSession) ensureStarted(ctx context.Context) error {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()

	if s.closed {
		return ErrSessionClosed
	}
	if s.waitErr != nil {
		return s.waitErr
	}
	if s.cmd != nil {
		return nil
	}

	if s.client == nil {
		return errors.New("claude code client is nil")
	}

	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	args := BuildStreamProcessArgs(&s.runOptions)
	if s.observer.OnProcessSpawn != nil {
		s.observer.OnProcessSpawn(s.client, s.runOptions, append([]string(nil), args...))
	}
	slog.Info("starting claude session process", "path", s.client.BinPath, "args", args, "work_dir", s.runOptions.WorkingDirectory)

	//nolint:gosec // The Claude binary path is configured by the host process and arguments are generated from structured run options.
	cmd := exec.CommandContext(runCtx, s.client.BinPath, args...)
	if s.runOptions.WorkingDirectory != "" {
		cmd.Dir = s.runOptions.WorkingDirectory
	}
	cmd.Env = BuildCLIEnv(s.runOptions.WorkingDirectory)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return NewClaudeError(ErrorCommand, fmt.Sprintf("failed to get stdin pipe: %v", err))
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		_ = stdinPipe.Close()
		return NewClaudeError(ErrorCommand, fmt.Sprintf("failed to get stdout pipe: %v", err))
	}
	stderr := s.runOptions.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}
	cmd.Stderr = stderr
	if err = cmd.Start(); err != nil {
		cancel()
		_ = stdinPipe.Close()
		return NewClaudeError(ErrorCommand, fmt.Sprintf("failed to start command: %v", err))
	}
	if s.observer.OnProcessStarted != nil {
		s.observer.OnProcessStarted(cmd.Process.Pid)
	}

	s.cmd = cmd
	s.cancel = cancel
	s.stdinPipe = stdinPipe
	s.waitCh = make(chan error, 1)
	s.exitDone = make(chan struct{})
	s.scanDone = make(chan struct{})

	go func() {
		s.waitCh <- cmd.Wait()
	}()
	go s.scanStdout(stdout)
	go s.watchExit()

	return nil
}

func (s *persistentProcessSession) initialize(ctx context.Context) error {
	if s == nil {
		return errors.New("claude session is nil")
	}

	s.turnMu.Lock()
	defer s.turnMu.Unlock()

	if s.isInitialized() {
		return nil
	}

	turn := &turnState{
		initRespCh:         make(chan error, 1),
		done:               make(chan struct{}),
		interruptRequested: make(chan struct{}),
		resultCh:           make(chan *ClaudeResult, 1),
		errCh:              make(chan error, 1),
	}
	if _, err := s.setCurrentTurn(turn); err != nil {
		return err
	}
	defer s.clearCurrentTurn(turn)

	if err := s.writeJSONLine(map[string]any{
		"type":       "control_request",
		"request_id": "initialize",
		"request": map[string]any{
			"subtype": "initialize",
		},
	}); err != nil {
		return NewClaudeError(ErrorCommand, fmt.Sprintf("failed to write initialize request: %v", err))
	}

	select {
	case err := <-turn.initRespCh:
		return err
	case err := <-turn.errCh:
		return err
	case <-ctx.Done():
		_ = s.Close()
		return ctx.Err()
	}
}

func (t *scheduledTurn) Run(ctx context.Context) (*ClaudeResult, error) {
	if t == nil || t.session == nil || t.turn == nil {
		return nil, errors.New("claude scheduled turn is nil")
	}
	s := t.session
	turn := t.turn
	defer s.clearCurrentTurn(turn)

	if len(t.inputBytes) > 0 {
		if err := s.writeRaw(t.inputBytes); err != nil {
			return nil, NewClaudeError(ErrorCommand, fmt.Sprintf("failed to write user input: %v", err))
		}
	}

	go s.watchTurnInterrupt(ctx, turn)

	select {
	case result := <-turn.resultCh:
		if s.turnInterrupted(turn) {
			return nil, context.Canceled
		}
		return result, nil
	case err := <-turn.errCh:
		return nil, err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

//nolint:contextcheck // Interrupt accepts a caller-owned context for cancellation while waiting on the scheduled turn.
func (t *scheduledTurn) Interrupt(ctx context.Context) error {
	if t == nil || t.session == nil || t.turn == nil {
		return nil
	}
	s := t.session
	turn := t.turn
	if ctx == nil {
		ctx = context.Background()
	}

	s.stateMu.Lock()
	if s.closed {
		s.stateMu.Unlock()
		return ErrSessionClosed
	}
	if s.waitErr != nil {
		waitErr := s.waitErr
		s.stateMu.Unlock()
		return waitErr
	}
	interruptDone := s.signalTurnInterruptLocked(turn)
	done := turn.done
	s.stateMu.Unlock()

	select {
	case <-interruptDone:
		s.stateMu.Lock()
		err := turn.interruptErr
		s.stateMu.Unlock()
		return err
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (t *scheduledTurn) Done() <-chan struct{} {
	if t == nil || t.turn == nil {
		return nil
	}
	return t.turn.done
}

func (s *persistentProcessSession) setCurrentTurn(turn *turnState) (*turnState, error) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	if s.closed {
		return nil, ErrSessionClosed
	}
	if s.waitErr != nil {
		return nil, s.waitErr
	}
	if s.currentTurn != nil {
		return s.currentTurn, nil
	}
	s.currentTurn = turn
	return nil, nil
}

func (s *persistentProcessSession) isInitialized() bool {
	if s == nil {
		return false
	}
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return s.initialized
}

func (s *persistentProcessSession) markInitialized() {
	if s == nil {
		return
	}

	s.stateMu.Lock()
	s.initialized = true
	s.stateMu.Unlock()
}

func (s *persistentProcessSession) clearCurrentTurn(turn *turnState) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	if s.currentTurn == turn {
		s.currentTurn = nil
	}
	s.completeTurnInterruptLocked(turn, nil)
	if turn != nil && turn.done != nil {
		close(turn.done)
	}
}

func (s *persistentProcessSession) turnInterrupted(turn *turnState) bool {
	if turn == nil {
		return false
	}
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	if turn.interruptFinished || turn.interruptSent {
		return true
	}
	if turn.interruptRequested == nil {
		return false
	}
	select {
	case <-turn.interruptRequested:
		return true
	default:
		return false
	}
}

func (s *persistentProcessSession) signalTurnInterruptLocked(turn *turnState) chan struct{} {
	if turn == nil {
		return nil
	}
	if turn.interruptRequested != nil {
		select {
		case <-turn.interruptRequested:
		default:
			close(turn.interruptRequested)
		}
	}
	if turn.interruptDone == nil {
		turn.interruptDone = make(chan struct{})
	}
	return turn.interruptDone
}

func (s *persistentProcessSession) watchTurnInterrupt(ctx context.Context, turn *turnState) {
	if turn == nil {
		return
	}

	select {
	case <-turn.interruptRequested:
	case <-ctx.Done():
		s.stateMu.Lock()
		s.signalTurnInterruptLocked(turn)
		s.stateMu.Unlock()
	case <-turn.done:
		return
	}

	s.stateMu.Lock()
	if s.currentTurn != turn || turn.interruptFinished || turn.interruptSent {
		s.stateMu.Unlock()
		return
	}
	turn.interruptSent = true
	s.stateMu.Unlock()

	if err := s.writeJSONLine(map[string]any{
		"type":       "control_request",
		"request_id": "interrupt",
		"request": map[string]any{
			"subtype": "interrupt",
		},
	}); err != nil {
		s.stateMu.Lock()
		if s.currentTurn == turn {
			s.completeTurnInterruptLocked(turn, NewClaudeError(ErrorCommand, fmt.Sprintf("failed to write interrupt request: %v", err)))
		}
		s.stateMu.Unlock()
	}
}

func (s *persistentProcessSession) writeRaw(data []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.stdinPipe.Write(data)
	return err
}

func (s *persistentProcessSession) writeJSONLine(payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return s.writeRaw(append(data, '\n'))
}

func (s *persistentProcessSession) scanStdout(stdout io.Reader) {
	defer close(s.scanDone)

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}

		var envelope map[string]any
		if err := json.Unmarshal([]byte(line), &envelope); err != nil {
			s.failCurrentTurn(NewClaudeError(ErrorValidation, fmt.Sprintf("failed to parse JSON message: %v", err)))
			_ = s.Close()
			return
		}

		switch envelope["type"] {
		case "control_response":
			s.handleControlResponse(envelope)
		case "control_request":
			s.handleControlRequest(envelope)
		default:
			s.handleStreamMessage(envelope, line)
		}
	}

	if err := scanner.Err(); err != nil && !errors.Is(err, context.Canceled) {
		s.failCurrentTurn(NewClaudeError(ErrorCommand, fmt.Sprintf("failed to scan stream output: %v", err)))
	}
}

func (s *persistentProcessSession) handleControlResponse(envelope map[string]any) {
	turn := s.getCurrentTurn()
	if turn == nil {
		return
	}
	response, _ := envelope["response"].(map[string]any)
	requestID, _ := response["request_id"].(string)
	switch requestID {
	case "initialize":
		if turn.initRespCh == nil {
			return
		}
		subtype, _ := response["subtype"].(string)
		if subtype == "error" {
			errText := fmt.Sprintf("%v", response["error"])
			if strings.Contains(strings.ToLower(errText), "already initialized") {
				s.markInitialized()
				turn.initRespCh <- nil
				return
			}
			turn.initRespCh <- fmt.Errorf("initialize failed: %v", response["error"])
			return
		}
		s.markInitialized()
		turn.initRespCh <- nil
	case "interrupt":
		if turn.interruptDone == nil {
			return
		}
		subtype, _ := response["subtype"].(string)
		if subtype == "error" {
			s.stateMu.Lock()
			s.completeTurnInterruptLocked(turn, fmt.Errorf("interrupt failed: %v", response["error"]))
			s.stateMu.Unlock()
			return
		}
		s.stateMu.Lock()
		s.completeTurnInterruptLocked(turn, nil)
		s.stateMu.Unlock()
	default:
		return
	}
}

func (s *persistentProcessSession) handleControlRequest(envelope map[string]any) {
	requestID, _ := envelope["request_id"].(string)
	request, _ := envelope["request"].(map[string]any)
	subtype, _ := request["subtype"].(string)
	if s.observer.OnControlRequest != nil {
		s.observer.OnControlRequest(s.pid(), requestID, subtype)
	}

	handler := s.controlRequestHandler()
	var (
		response map[string]any
		err      error
	)
	if handler == nil {
		err = errors.New("control request handling is unavailable")
	} else {
		response, err = handler(request)
	}
	responseEnvelope := map[string]any{
		"request_id": requestID,
	}
	if err != nil {
		responseEnvelope["subtype"] = "error"
		responseEnvelope["error"] = err.Error()
	} else {
		responseEnvelope["subtype"] = "success"
		responseEnvelope["response"] = response
	}
	payload := map[string]any{
		"type":     "control_response",
		"response": responseEnvelope,
	}
	if err = s.writeJSONLine(payload); err != nil {
		s.failCurrentTurn(NewClaudeError(ErrorCommand, fmt.Sprintf("failed to write control response: %v", err)))
		_ = s.Close()
	}
}

func (s *persistentProcessSession) controlRequestHandler() func(map[string]any) (map[string]any, error) {
	if s == nil {
		return nil
	}

	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return s.hooks.HandleControlRequest
}

func (s *persistentProcessSession) handleStreamMessage(envelope map[string]any, line string) {
	var msg Message
	if err := json.Unmarshal([]byte(line), &msg); err != nil {
		s.failCurrentTurn(NewClaudeError(ErrorValidation, fmt.Sprintf("failed to decode stream message: %v", err)))
		_ = s.Close()
		return
	}

	switch msg.Type {
	case "system":
		s.handleSystemMessage(&msg)
	case "assistant":
		s.handleAssistantMessage(&msg)
	case "user":
		s.handleUserMessage(&msg)
	case "result":
		s.handleResultMessage(envelope, &msg)
	case "control_cancel_request":
		s.handleControlCancelRequest(envelope)
	default:
		if s.observer.OnUnsupportedMessage != nil {
			s.observer.OnUnsupportedMessage(s.pid(), msg.Type)
		}
	}
}

func (s *persistentProcessSession) handleSystemMessage(msg *Message) {
	if msg == nil {
		return
	}

	sessionID := strings.TrimSpace(msg.SessionID)
	if sessionID != "" {
		s.stateMu.Lock()
		s.sessionID = sessionID
		s.stateMu.Unlock()
	}
	if s.observer.OnSystemMessage != nil {
		s.observer.OnSystemMessage(s.pid(), msg)
	}
}

func (s *persistentProcessSession) handleAssistantMessage(msg *Message) {
	roleMessage, err := DecodeStreamRoleMessage(msg)
	if err != nil {
		if s.observer.OnAssistantDecodeError != nil {
			s.observer.OnAssistantDecodeError(s.pid(), err)
		}
		return
	}
	if s.observer.OnAssistantMessage != nil {
		s.observer.OnAssistantMessage(s.pid(), roleMessage)
	}

	turn := s.getCurrentTurn()
	if turn == nil {
		return
	}
	for _, block := range roleMessage.Content {
		if block.Type != "text" || block.Text == "" {
			continue
		}
		turn.assistantTextParts = append(turn.assistantTextParts, block.Text)
	}
}

func (s *persistentProcessSession) handleUserMessage(msg *Message) {
	roleMessage, err := DecodeStreamRoleMessage(msg)
	if err != nil {
		if s.observer.OnUserDecodeError != nil {
			s.observer.OnUserDecodeError(s.pid(), err)
		}
		return
	}
	if s.observer.OnUserMessage != nil {
		s.observer.OnUserMessage(s.pid(), roleMessage)
	}
}

func (s *persistentProcessSession) handleResultMessage(envelope map[string]any, msg *Message) {
	if msg == nil {
		return
	}

	inputTokens, outputTokens := ParseResultUsage(envelope)
	if s.observer.OnResultMessage != nil {
		s.observer.OnResultMessage(s.pid(), msg, inputTokens, outputTokens)
	}

	result := &ClaudeResult{
		Type:             msg.Type,
		Subtype:          msg.Subtype,
		Result:           msg.Result,
		StructuredOutput: msg.StructuredOutput,
		CostUSD:          msg.CostUSD,
		DurationMS:       msg.DurationMS,
		DurationAPIMS:    msg.DurationAPIMS,
		IsError:          msg.IsError,
		NumTurns:         msg.NumTurns,
		SessionID:        msg.SessionID,
	}

	s.stateMu.Lock()
	if strings.TrimSpace(result.SessionID) != "" {
		s.sessionID = strings.TrimSpace(result.SessionID)
	}
	turn := s.currentTurn
	s.stateMu.Unlock()
	if turn != nil && result.Result == "" && result.StructuredOutput == nil {
		result.Result = strings.Join(turn.assistantTextParts, "")
	}
	if turn != nil {
		turn.resultCh <- result
	}
}

func (s *persistentProcessSession) handleControlCancelRequest(envelope map[string]any) {
	if s.observer.OnControlCancelRequest == nil {
		return
	}
	requestID, _ := envelope["request_id"].(string)
	s.observer.OnControlCancelRequest(s.pid(), requestID)
}

func (s *persistentProcessSession) watchExit() {
	rawWaitErr := awaitProcess(s.waitCh)
	err := IgnoreExpectedExit(rawWaitErr)
	s.stateMu.Lock()
	closed := s.closed
	if s.waitErr == nil {
		if err == nil && !closed {
			err = ErrProcessExited
		}
		s.waitErr = err
	}
	turn := s.currentTurn
	s.stateMu.Unlock()
	close(s.exitDone)

	if s.observer.OnProcessExit != nil {
		s.observer.OnProcessExit(s.pid(), closed, rawWaitErr)
	}
	if turn != nil && err != nil {
		s.stateMu.Lock()
		s.completeTurnInterruptLocked(turn, err)
		s.stateMu.Unlock()
		turn.errCh <- err
	}
}

func (s *persistentProcessSession) getCurrentTurn() *turnState {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return s.currentTurn
}

func (s *persistentProcessSession) failCurrentTurn(err error) {
	s.stateMu.Lock()
	turn := s.currentTurn
	s.completeTurnInterruptLocked(turn, err)
	s.stateMu.Unlock()
	if turn != nil {
		select {
		case turn.errCh <- err:
		default:
		}
	}
}

func (s *persistentProcessSession) completeTurnInterruptLocked(turn *turnState, err error) {
	if turn == nil || turn.interruptDone == nil || turn.interruptFinished {
		return
	}
	turn.interruptErr = err
	turn.interruptFinished = true
	close(turn.interruptDone)
}

func (s *persistentProcessSession) Close() error {
	if s == nil {
		return nil
	}

	s.stateMu.Lock()
	if s.closed {
		s.stateMu.Unlock()
		return nil
	}
	s.closed = true
	cmd := s.cmd
	stdinPipe := s.stdinPipe
	cancel := s.cancel
	exitDone := s.exitDone
	scanDone := s.scanDone
	turn := s.currentTurn
	var turnDone chan struct{}
	if turn != nil && turn.done != nil {
		turnDone = s.signalTurnInterruptLocked(turn)
	}
	s.stateMu.Unlock()

	if turnDone != nil {
		select {
		case <-turn.done:
		case <-time.After(defaultProcessTerminateTimeout):
		}
	}

	if cmd == nil {
		return nil
	}

	if stdinPipe != nil {
		_ = stdinPipe.Close()
	}
	if cancel != nil {
		cancel()
	}
	var signalErr error
	if err := SignalProcessGroup(cmd, syscall.SIGTERM); err != nil {
		signalErr = errors.Join(signalErr, err)
	}
	if !s.waitForExit(defaultProcessTerminateTimeout) {
		if err := SignalProcessGroup(cmd, syscall.SIGKILL); err != nil {
			signalErr = errors.Join(signalErr, err)
		}
	}

	if exitDone != nil {
		<-exitDone
	}
	s.stateMu.Lock()
	err := s.waitErr
	closed := s.closed
	s.stateMu.Unlock()
	if scanDone != nil {
		<-scanDone
	}
	if errors.Is(err, ErrProcessExited) || (closed && errors.Is(err, context.Canceled)) {
		err = nil
	}
	return errors.Join(err, signalErr)
}

func (s *persistentProcessSession) waitForExit(timeout time.Duration) bool {
	if s == nil || s.exitDone == nil {
		return true
	}
	if timeout <= 0 {
		<-s.exitDone
		return true
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-s.exitDone:
		return true
	case <-timer.C:
		return false
	}
}

func (s *persistentProcessSession) pid() int {
	if s == nil || s.cmd == nil || s.cmd.Process == nil {
		return 0
	}
	return s.cmd.Process.Pid
}

func awaitProcess(waitCh <-chan error) error {
	if waitCh == nil {
		return nil
	}

	return <-waitCh
}

// IgnoreExpectedExit suppresses expected signal-based process exits.
func IgnoreExpectedExit(err error) error {
	if err == nil {
		return nil
	}

	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ProcessState == nil {
		return err
	}

	waitStatus, ok := exitErr.Sys().(syscall.WaitStatus)
	if !ok || !waitStatus.Signaled() {
		return err
	}

	switch waitStatus.Signal() {
	case syscall.SIGINT, syscall.SIGTERM, syscall.SIGKILL:
		return nil
	default:
		return err
	}
}

// SignalProcessGroup sends one signal to the Claude subprocess group.
func SignalProcessGroup(cmd *exec.Cmd, signal syscall.Signal) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}

	err := syscall.Kill(-cmd.Process.Pid, signal)
	if ignoreExpectedSignalProcessGroupError(err) {
		return nil
	}

	return err
}

func ignoreExpectedSignalProcessGroupError(err error) bool {
	if err == nil {
		return true
	}

	// macOS can report EPERM while a process group is already tearing down after
	// the session context canceled the leader. Treat that as an expected shutdown race.
	return errors.Is(err, os.ErrProcessDone) || errors.Is(err, syscall.ESRCH) || errors.Is(err, syscall.EPERM)
}
