package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"

	"github.com/godeps/codex-sdk-go"
)

type toolLoopResponse struct {
	Action        string          `json:"action"`
	Message       string          `json:"message,omitempty"`
	ToolName      string          `json:"tool_name,omitempty"`
	ToolInput     json.RawMessage `json:"tool_input,omitempty"`
	ToolInputJSON string          `json:"tool_input_json,omitempty"`
}

type codexTurnContext struct {
	prompts []string
	tools   []Tool
}

func (s *codexRunnerSession) runToolLoop(ctx context.Context, req TurnRequest, thread codexThread, input codex.Input) (string, error) {
	if s == nil || s.runner == nil {
		return "", errors.New("codex session is nil")
	}

	prompts, tools := s.runner.globalContext()
	return s.runToolLoopWithContext(ctx, req, thread, input, codexTurnContext{
		prompts: prompts,
		tools:   tools,
	})
}

func (s *codexRunnerSession) runToolLoopWithContext(ctx context.Context, req TurnRequest, thread codexThread, input codex.Input, turnContext codexTurnContext) (string, error) {
	if s == nil || s.runner == nil {
		return "", errors.New("codex session is nil")
	}

	currentInput := input
	toolCtx := ContextWithTurnRequest(ctx, req)
	maxIterations := s.runner.effectiveMaxToolIterations()
	for iteration := range maxIterations {
		log.Printf(
			"codex runner tool loop iteration: conversation=%s thread_id=%s iteration=%d",
			req.Conversation.Key,
			thread.ID(),
			iteration+1,
		)
		turn, runErr := s.runThreadTurn(req, thread, currentInput, codex.TurnOptions{
			OutputSchema: toolLoopResponseSchema(),
			Context:      ctx,
		})
		if runErr != nil {
			return "", runErr
		}

		decision, parseErr := parseToolLoopResponse(turn.FinalResponse)
		if parseErr != nil {
			log.Printf(
				"codex runner tool loop invalid response: conversation=%s iteration=%d err=%v",
				req.Conversation.Key,
				iteration+1,
				parseErr,
			)
			currentInput = buildToolLoopErrorInput(parseErr, turn.FinalResponse)
			continue
		}
		log.Printf(
			"codex runner tool loop decision: conversation=%s iteration=%d action=%s tool=%s message_len=%d",
			req.Conversation.Key,
			iteration+1,
			decision.Action,
			decision.ToolName,
			len(decision.Message),
		)

		switch decision.Action {
		case "respond":
			if strings.TrimSpace(decision.Message) == "" {
				decisionErr := errors.New("tool loop returned an empty assistant message")
				log.Printf(
					"codex runner tool loop invalid response: conversation=%s iteration=%d err=%v",
					req.Conversation.Key,
					iteration+1,
					decisionErr,
				)
				currentInput = buildToolLoopErrorInput(decisionErr, turn.FinalResponse)
				continue
			}
			return decision.Message, nil
		case "call_tool":
			tool, ok := findToolIn(turnContext.tools, decision.ToolName)
			if !ok {
				decisionErr := fmt.Errorf("tool loop requested unknown tool %q", decision.ToolName)
				log.Printf(
					"codex runner tool loop invalid response: conversation=%s iteration=%d err=%v",
					req.Conversation.Key,
					iteration+1,
					decisionErr,
				)
				currentInput = buildToolLoopErrorInput(decisionErr, turn.FinalResponse)
				continue
			}

			log.Printf(
				"codex runner calling tool: conversation=%s iteration=%d tool=%s input_bytes=%d",
				req.Conversation.Key,
				iteration+1,
				tool.Name(),
				len(decision.ToolInput),
			)
			result, callErr := tool.Call(toolCtx, decision.ToolInput)
			if callErr != nil {
				log.Printf(
					"codex runner tool failed: conversation=%s iteration=%d tool=%s err=%v",
					req.Conversation.Key,
					iteration+1,
					tool.Name(),
					callErr,
				)
			} else {
				log.Printf(
					"codex runner tool completed: conversation=%s iteration=%d tool=%s",
					req.Conversation.Key,
					iteration+1,
					tool.Name(),
				)
			}
			currentInput, runErr = buildToolResultInput(tool.Name(), decision.ToolInput, result, callErr)
			if runErr != nil {
				return "", runErr
			}
		case "silent":
			return "", nil
		default:
			decisionErr := fmt.Errorf("tool loop returned unsupported action %q", decision.Action)
			log.Printf(
				"codex runner tool loop invalid response: conversation=%s iteration=%d err=%v",
				req.Conversation.Key,
				iteration+1,
				decisionErr,
			)
			currentInput = buildToolLoopErrorInput(decisionErr, turn.FinalResponse)
			continue
		}
	}

	return "", fmt.Errorf("tool loop exceeded %d iterations", maxIterations)
}

func findToolIn(tools []Tool, name string) (Tool, bool) {
	for _, tool := range tools {
		if tool.Name() == name {
			return tool, true
		}
	}

	return nil, false
}

func parseToolLoopResponse(raw string) (toolLoopResponse, error) {
	var response toolLoopResponse
	if err := json.Unmarshal([]byte(raw), &response); err != nil {
		return toolLoopResponse{}, fmt.Errorf("decode tool loop response failed: %w", err)
	}
	if len(response.ToolInput) == 0 && strings.TrimSpace(response.ToolInputJSON) != "" {
		response.ToolInput = json.RawMessage(response.ToolInputJSON)
	}
	if response.Action == "call_tool" && len(response.ToolInput) == 0 {
		response.ToolInput = json.RawMessage("{}")
	}
	if err := validateToolLoopResponse(response); err != nil {
		return toolLoopResponse{}, err
	}

	return response, nil
}

func validateToolLoopResponse(response toolLoopResponse) error {
	switch response.Action {
	case "respond":
		return nil
	case "silent":
		return nil
	case "call_tool":
		if strings.TrimSpace(response.ToolName) == "" {
			return errors.New("tool loop response missing tool_name for call_tool action")
		}
		if !json.Valid(response.ToolInput) {
			return errors.New("tool loop response tool_input_json is not valid JSON")
		}

		var payload map[string]any
		if err := json.Unmarshal(response.ToolInput, &payload); err != nil {
			return errors.New("tool loop response tool_input_json must decode to a JSON object")
		}
		return nil
	default:
		return fmt.Errorf("tool loop response has unsupported action %q", response.Action)
	}
}

func buildToolResultInput(toolName string, toolInput json.RawMessage, result any, err error) (codex.Input, error) {
	inputJSON := "{}"
	if len(toolInput) != 0 {
		inputJSON = string(toolInput)
	}

	var status string
	var payload string
	if err != nil {
		status = "error"
		payload = err.Error()
	} else {
		status = "ok"
		data, marshalErr := json.Marshal(result)
		if marshalErr != nil {
			return codex.Input{}, fmt.Errorf("encode tool result failed: %w", marshalErr)
		}
		payload = string(data)
	}

	return codex.TextInput(strings.TrimSpace(fmt.Sprintf(`
Tool execution finished.

Tool: %s
Status: %s
Input:
%s

Result:
%s

Continue the task. Return valid JSON matching the schema for either another tool call or the final user-facing response.
`, toolName, status, inputJSON, payload))), nil
}

func toolLoopResponseSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action": map[string]any{
				"type": "string",
				"enum": []any{"respond", "call_tool", "silent"},
			},
			"message": map[string]any{
				"type": "string",
			},
			"tool_name": map[string]any{
				"type": "string",
			},
			"tool_input_json": map[string]any{
				"type": "string",
			},
		},
		"required":             []any{"action", "message", "tool_name", "tool_input_json"},
		"additionalProperties": false,
	}
}

func buildToolLoopErrorInput(err error, rawResponse string) codex.Input {
	return codex.TextInput(strings.TrimSpace(fmt.Sprintf(`
The previous JSON response was invalid and could not be executed.

Error:
%v

Previous response:
%s

Return a corrected JSON object that matches the schema exactly. Do not add prose outside the JSON object.
`, err, rawResponse)))
}
