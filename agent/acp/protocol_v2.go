package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
)

// v2TurnState is guarded by processSession.turnMu. The prompt response only
// acknowledges insertion; completed closes on the foreground idle transition.
type v2TurnState struct {
	completed chan struct{}
	busy      bool
	finished  bool
	messages  map[string]string
	order     []string
	result    json.RawMessage
	reply     string
}

type v2SessionUpdate struct {
	SessionUpdate string          `json:"sessionUpdate"`
	MessageID     string          `json:"messageId"`
	SessionID     string          `json:"sessionId"`
	Content       json.RawMessage `json:"content"`
	State         string          `json:"state"`
	StopReason    string          `json:"stopReason"`
	Error         *rpcError       `json:"error"`
	ConfigOptions json.RawMessage `json:"configOptions"`
}

func (s *processSession) prepareV2Session(sessionID string) {
	s.sessionMu.Lock()
	s.sessionID = sessionID
	s.status = SessionMetadata{SessionID: sessionID, ProtocolVersion: 2}
	s.childSessions = make(map[string]string)
	s.replayComplete = false
	s.sessionMu.Unlock()
}

func (s *processSession) resumeV2Session(ctx context.Context, sessionID, workDir string, servers []MCPServer) error {
	s.prepareV2Session(sessionID)
	log.Printf("acp session/resume started: session_id=%s replay_from=start", sanitizeLogValue(sessionID))
	var response struct {
		ConfigOptions json.RawMessage `json:"configOptions"`
	}
	var decodeErr error
	_, err := s.transport.callWithCallbacks(ctx, "session/resume", map[string]any{
		"sessionId":  sessionID,
		"cwd":        workDir,
		"mcpServers": servers,
		"replayFrom": map[string]any{"type": "start"},
	}, nil, func(result json.RawMessage) {
		decodeErr = json.Unmarshal(result, &response)
		if decodeErr == nil {
			s.setSessionState(sessionID, SessionModes{}, response.ConfigOptions)
		}
	})
	if err != nil {
		return fmt.Errorf("acp session/resume failed: %w", err)
	}
	if decodeErr != nil {
		return fmt.Errorf("decode acp session/resume response failed: %w", decodeErr)
	}
	s.sessionMu.RLock()
	replayed := s.replayComplete
	s.sessionMu.RUnlock()
	if !replayed {
		log.Printf("acp session/resume completed without required replay_complete: session_id=%s", sanitizeLogValue(sessionID))
	}
	logConfigOptions("session/resume", sessionID, response.ConfigOptions)
	return nil
}

//nolint:contextcheck // The lifecycle context keeps terminal updates flowing after caller cancellation; the watcher forwards cancellation to the agent.
func (s *processSession) runV2Turn(ctx context.Context, turn *processActiveTurn, blocks []ContentBlock) (TurnResult, error) {
	if len(blocks) == 0 {
		return TurnResult{}, errors.New("acp v2 prompt must not be empty")
	}
	if err := ctx.Err(); err != nil {
		return TurnResult{}, err
	}
	state := &v2TurnState{completed: make(chan struct{}), messages: make(map[string]string)}
	s.turnMu.Lock()
	turn.v2 = state
	s.turnMu.Unlock()

	// Use the lifecycle context to keep draining terminal updates after the
	// caller cancels. Releasing the turn early would mix its output into the
	// next prompt. The watcher sends session/cancel only after prompt is sent.
	result, err := s.transport.callWithCallbacks(s.lifecycleCtx, "session/prompt", map[string]any{
		"sessionId": turn.sessionID,
		"prompt":    blocks,
	}, func() {
		go s.watchTurnInterrupt(ctx, turn)
	}, nil)
	if err != nil {
		s.turnMu.Lock()
		busy := state.busy
		s.turnMu.Unlock()
		if busy {
			_ = s.waitV2Turn(turn)
		}
		if s.wasTurnInterrupted(turn) {
			return TurnResult{}, context.Canceled
		}
		return TurnResult{}, fmt.Errorf("acp session/prompt failed: %w", err)
	}
	var accepted struct {
		MessageID string `json:"messageId"`
	}
	if err = json.Unmarshal(result, &accepted); err != nil || accepted.MessageID == "" {
		// The peer has violated the insertion contract. Stop this connection
		// rather than letting unfinished work leak into another turn.
		s.cancel()
		return TurnResult{}, errors.New("acp v2 session/prompt response is missing a valid messageId")
	}
	if err = s.waitV2Turn(turn); err != nil {
		return TurnResult{}, err
	}
	s.turnMu.Lock()
	reply := state.reply
	terminal := append(json.RawMessage(nil), state.result...)
	s.turnMu.Unlock()
	if s.wasTurnInterrupted(turn) {
		return TurnResult{}, context.Canceled
	}
	var completed v2SessionUpdate
	if err = json.Unmarshal(terminal, &completed); err != nil {
		return TurnResult{}, fmt.Errorf("decode acp v2 terminal state failed: %w", err)
	}
	logACPStopReason(turn.sessionID, terminal)
	switch completed.StopReason {
	case "error":
		if completed.Error != nil {
			return TurnResult{}, fmt.Errorf("acp foreground work failed: %w", completed.Error)
		}
		return TurnResult{}, errors.New("acp foreground work failed without error details")
	case "cancelled": //nolint:misspell // ACP uses the protocol spelling "cancelled".
		return TurnResult{}, context.Canceled
	}
	return TurnResult{SessionID: turn.sessionID, ReplyText: reply, RawResult: terminal}, nil
}

func (s *processSession) waitV2Turn(turn *processActiveTurn) error {
	// Prefer a completion already received before a subsequent process exit.
	select {
	case <-turn.v2.completed:
		return nil
	default:
	}
	select {
	case <-turn.v2.completed:
		return nil
	case <-s.transport.done:
		select {
		case <-turn.v2.completed:
			return nil
		default:
		}
		return fmt.Errorf("acp transport closed before foreground work completed: %w", s.transport.error())
	case <-s.lifecycleCtx.Done():
		return s.lifecycleCtx.Err()
	}
}

func (s *processSession) handleV2Notification(params json.RawMessage) {
	var envelope struct {
		SessionID string          `json:"sessionId"`
		Update    json.RawMessage `json:"update"`
	}
	if err := json.Unmarshal(params, &envelope); err != nil || envelope.SessionID == "" {
		return
	}
	var update v2SessionUpdate
	if err := json.Unmarshal(envelope.Update, &update); err != nil {
		log.Printf("acp v2 session/update decode failed: session_id=%s err=%v", sanitizeLogValue(envelope.SessionID), err)
		return
	}
	s.sessionMu.Lock()
	root := envelope.SessionID == s.sessionID
	_, child := s.childSessions[envelope.SessionID]
	known := root || child || s.sessionID == ""
	if known && update.SessionUpdate == "subagent_update" && update.SessionID != "" && update.SessionID != envelope.SessionID && update.SessionID != s.sessionID {
		if s.childSessions == nil {
			s.childSessions = make(map[string]string)
		}
		if _, registered := s.childSessions[update.SessionID]; !registered {
			s.childSessions[update.SessionID] = envelope.SessionID
		}
	}
	if root {
		switch update.SessionUpdate {
		case "config_option_update":
			if len(update.ConfigOptions) != 0 {
				s.status.ConfigOptions = append(json.RawMessage(nil), update.ConfigOptions...)
			}
		case "replay_complete":
			s.replayComplete = true
		}
	}
	s.sessionMu.Unlock()
	if !known {
		log.Printf("acp v2 update ignored for unknown session: session_id=%s", sanitizeLogValue(envelope.SessionID))
		return
	}
	if s.observer.OnSessionUpdate != nil {
		s.observer.OnSessionUpdate(SessionUpdate{
			SessionID: envelope.SessionID, SessionUpdate: update.SessionUpdate, MessageID: update.MessageID,
			Text: v2ContentText(update.Content), Raw: append(json.RawMessage(nil), envelope.Update...),
		})
	}
	if !root {
		return
	}
	s.turnMu.Lock()
	defer s.turnMu.Unlock()
	turn := s.activeTurn
	if turn == nil || !turn.runStarted || turn.v2 == nil || turn.v2.finished {
		return
	}
	state := turn.v2
	switch update.SessionUpdate {
	case "state_update":
		switch update.State {
		case "idle":
			// Repeated idle snapshots only update background activity. They do
			// not complete a newly scheduled prompt that has not started work.
			if !state.busy {
				return
			}
			state.result = append(json.RawMessage(nil), envelope.Update...)
			var reply strings.Builder
			for _, id := range state.order {
				reply.WriteString(state.messages[id])
			}
			state.reply = reply.String()
			state.finished = true
			close(state.completed)
		default:
			// Unknown future states are unconfirmed activity, never idle.
			if update.State != "" {
				state.busy = true
			}
		}
	case "agent_message", "agent_message_chunk":
		if update.MessageID == "" || !state.busy {
			return
		}
		if _, seen := state.messages[update.MessageID]; !seen {
			state.order = append(state.order, update.MessageID)
			state.messages[update.MessageID] = ""
		}
		if update.SessionUpdate == "agent_message_chunk" {
			state.messages[update.MessageID] += v2ContentText(update.Content)
		} else if len(update.Content) != 0 {
			state.messages[update.MessageID] = v2ContentText(update.Content)
		}
	}
}

func v2ContentText(raw json.RawMessage) string {
	var blocks []json.RawMessage
	if len(raw) == 0 {
		return ""
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		blocks = []json.RawMessage{raw}
	}
	var text strings.Builder
	for _, rawBlock := range blocks {
		var block struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			Name     string `json:"name"`
			URI      string `json:"uri"`
			MimeType string `json:"mimeType"`
		}
		if err := json.Unmarshal(rawBlock, &block); err != nil || block.Type == "" {
			continue
		}
		if block.Type == "text" {
			text.WriteString(block.Text)
			continue
		}
		label := block.Name
		if label == "" {
			label = block.URI
		}
		if label == "" {
			label = block.MimeType
		}
		text.WriteString("[" + block.Type)
		if label != "" {
			text.WriteString(": " + label)
		}
		text.WriteString("]")
	}
	return text.String()
}
