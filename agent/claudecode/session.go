package claudecode

import (
	"bufio"
	"bytes"
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

// TurnHooks provides per-turn control callbacks for one persistent Claude session.
type TurnHooks struct {
	ShouldInitialize     func() bool
	HandleControlRequest func(map[string]any) (map[string]any, error)
}

// SessionObserver receives optional runtime callbacks from one persistent Claude session.
type SessionObserver struct {
	OnProcessSpawn         func(client *ClaudeClient, opts RunOptions, args []string)
	OnProcessStarted       func(pid int)
	OnStderrClosed         func(pid int, byteCount int, stderrText string)
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

// Session is a reusable Claude subprocess session.
type Session interface {
	RunTurn(ctx context.Context, blocks []map[string]any, hooks TurnHooks) (*ClaudeResult, error)
	CurrentSessionID() string
	Close() error
}

type turnState struct {
	handleControlRequest func(map[string]any) (map[string]any, error)
	initRespCh           chan error
	resultCh             chan *ClaudeResult
	errCh                chan error
}

type persistentProcessSession struct {
	runOptions RunOptions
	cmd        *exec.Cmd
	cancel     context.CancelFunc
	stdinPipe  io.WriteCloser
	waitCh     chan error
	exitDone   chan struct{}
	stderrDone chan struct{}
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
}

// StartSession starts one reusable Claude CLI subprocess.
func StartSession(ctx context.Context, client *ClaudeClient, opts RunOptions, observer SessionObserver) (Session, error) {
	if ctx == nil {
		return nil, errors.New("claude code session context is nil")
	}
	if client == nil {
		return nil, errors.New("claude code client is nil")
	}
	if err := PreprocessOptions(&opts); err != nil {
		return nil, err
	}

	session := &persistentProcessSession{
		runOptions: opts,
		sessionID:  strings.TrimSpace(opts.ResumeID),
		observer:   observer,
		client:     client,
	}

	return session, nil
}

func (s *persistentProcessSession) CurrentSessionID() string {
	if s == nil {
		return ""
	}
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return s.sessionID
}

// RunTurn runs one stream-json turn against the persistent Claude session.
func (s *persistentProcessSession) RunTurn(ctx context.Context, blocks []map[string]any, hooks TurnHooks) (*ClaudeResult, error) {
	inputBytes, err := MarshalStreamJSONUserInput(blocks)
	if err != nil {
		return nil, err
	}
	return s.runTurnWithInput(ctx, inputBytes, hooks)
}

func (s *persistentProcessSession) runTurnWithInput(ctx context.Context, inputBytes []byte, hooks TurnHooks) (*ClaudeResult, error) {
	if s == nil {
		return nil, errors.New("claude session is nil")
	}
	s.turnMu.Lock()
	defer s.turnMu.Unlock()

	if err := s.ensureStarted(ctx); err != nil {
		return nil, err
	}

	return s.runTurn(ctx, inputBytes, hooks)
}

func (s *persistentProcessSession) ensureStarted(ctx context.Context) error {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()

	if s.closed {
		return errors.New("claude session is closed")
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
	stderr, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		_ = stdinPipe.Close()
		return NewClaudeError(ErrorCommand, fmt.Sprintf("failed to get stderr pipe: %v", err))
	}
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
	s.stderrDone = make(chan struct{})
	s.scanDone = make(chan struct{})

	go func() {
		s.waitCh <- cmd.Wait()
	}()
	go s.captureStderr(stderr)
	go s.scanStdout(stdout)
	go s.watchExit()

	return nil
}

func (s *persistentProcessSession) runTurn(ctx context.Context, inputBytes []byte, hooks TurnHooks) (*ClaudeResult, error) {
	turn := &turnState{
		handleControlRequest: hooks.HandleControlRequest,
		resultCh:             make(chan *ClaudeResult, 1),
		errCh:                make(chan error, 1),
	}
	if s.shouldInitialize(hooks) {
		turn.initRespCh = make(chan error, 1)
	}

	if err := s.setCurrentTurn(turn); err != nil {
		return nil, err
	}
	defer s.clearCurrentTurn(turn)

	if turn.initRespCh != nil {
		if err := s.writeJSONLine(map[string]any{
			"type":       "control_request",
			"request_id": "initialize",
			"request": map[string]any{
				"subtype": "initialize",
			},
		}); err != nil {
			return nil, NewClaudeError(ErrorCommand, fmt.Sprintf("failed to write initialize request: %v", err))
		}

		select {
		case err := <-turn.initRespCh:
			if err != nil {
				return nil, err
			}
		case err := <-turn.errCh:
			return nil, err
		case <-ctx.Done():
			_ = s.Close()
			return nil, ctx.Err()
		}
	}

	if len(inputBytes) > 0 {
		if err := s.writeRaw(inputBytes); err != nil {
			return nil, NewClaudeError(ErrorCommand, fmt.Sprintf("failed to write user input: %v", err))
		}
	}

	select {
	case result := <-turn.resultCh:
		return result, nil
	case err := <-turn.errCh:
		return nil, err
	case <-ctx.Done():
		_ = s.Close()
		return nil, ctx.Err()
	}
}

func (s *persistentProcessSession) setCurrentTurn(turn *turnState) error {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	if s.closed {
		return errors.New("claude session is closed")
	}
	if s.waitErr != nil {
		return s.waitErr
	}
	s.currentTurn = turn
	return nil
}

func (s *persistentProcessSession) shouldInitialize(hooks TurnHooks) bool {
	if s == nil || hooks.ShouldInitialize == nil || !hooks.ShouldInitialize() || hooks.HandleControlRequest == nil {
		return false
	}

	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return !s.initialized
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

func (s *persistentProcessSession) captureStderr(stderr io.Reader) {
	defer close(s.stderrDone)
	stderrBuf := new(bytes.Buffer)
	_, _ = io.Copy(stderrBuf, stderr)
	if s.observer.OnStderrClosed != nil {
		s.observer.OnStderrClosed(s.pid(), stderrBuf.Len(), strings.TrimSpace(stderrBuf.String()))
	}
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
	if turn == nil || turn.initRespCh == nil {
		return
	}
	response, _ := envelope["response"].(map[string]any)
	requestID, _ := response["request_id"].(string)
	if requestID != "initialize" {
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
}

func (s *persistentProcessSession) handleControlRequest(envelope map[string]any) {
	turn := s.getCurrentTurn()
	if turn == nil || turn.handleControlRequest == nil {
		s.failCurrentTurn(errors.New("received control request without active control handler"))
		_ = s.Close()
		return
	}

	requestID, _ := envelope["request_id"].(string)
	request, _ := envelope["request"].(map[string]any)
	subtype, _ := request["subtype"].(string)
	if s.observer.OnControlRequest != nil {
		s.observer.OnControlRequest(s.pid(), requestID, subtype)
	}

	response, err := turn.handleControlRequest(request)
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
	s.stateMu.Unlock()
	if turn != nil {
		select {
		case turn.errCh <- err:
		default:
		}
	}
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
	stderrDone := s.stderrDone
	scanDone := s.scanDone
	s.stateMu.Unlock()

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
	s.stateMu.Unlock()
	if stderrDone != nil {
		<-stderrDone
	}
	if scanDone != nil {
		<-scanDone
	}
	if errors.Is(err, ErrProcessExited) {
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
	if err == nil || errors.Is(err, os.ErrProcessDone) || errors.Is(err, syscall.ESRCH) {
		return nil
	}

	return err
}
