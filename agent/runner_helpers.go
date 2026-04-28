package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

func joinRunnerContext(ctx context.Context, runnerCtx context.Context) (context.Context, func()) {
	if ctx == nil {
		ctx = context.Background()
	}
	if runnerCtx == nil {
		return context.WithCancel(ctx)
	}

	joinedCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(runnerCtx, cancel)
	return joinedCtx, func() {
		stop()
		cancel()
	}
}

func runSessionTurn(ctx context.Context, session Session, req TurnRequest) (TurnResult, error) {
	if session == nil {
		return TurnResult{}, errors.New("session is nil")
	}
	turn, err := session.ScheduleTurn(ctx, req)
	if err != nil {
		return TurnResult{}, err
	}
	return turn.Run(ctx)
}

func formatRunnerMode(mode string, approval string) string {
	mode = strings.TrimSpace(mode)
	approval = strings.TrimSpace(approval)
	switch {
	case mode != "" && approval != "":
		return mode + " / " + approval
	case mode != "":
		return mode
	default:
		return approval
	}
}

func statusConfigOptions(options ...SessionConfigOption) []SessionConfigOption {
	if len(options) == 0 {
		return nil
	}

	filtered := make([]SessionConfigOption, 0, len(options))
	for _, option := range options {
		name := strings.TrimSpace(option.Name)
		currentValue := strings.TrimSpace(option.CurrentValue)
		if name == "" && currentValue == "" && len(option.Options) == 0 {
			continue
		}

		normalized := SessionConfigOption{
			Name:         name,
			CurrentValue: currentValue,
		}
		if len(option.Options) != 0 {
			normalized.Options = make([]SessionConfigOptionChoice, 0, len(option.Options))
			for _, candidate := range option.Options {
				choiceName := strings.TrimSpace(candidate.Name)
				choiceDescription := strings.TrimSpace(candidate.Description)
				if choiceName == "" && choiceDescription == "" {
					continue
				}
				normalized.Options = append(normalized.Options, SessionConfigOptionChoice{
					Name:        choiceName,
					Description: choiceDescription,
				})
			}
		}
		filtered = append(filtered, normalized)
	}

	if len(filtered) == 0 {
		return nil
	}
	return filtered
}

func parseStatusConfigOptions(raw json.RawMessage) []SessionConfigOption {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}

	var structured []struct {
		Name         string `json:"name"`
		CurrentValue string `json:"currentValue"`
		Options      []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		} `json:"options"`
	}
	if err := json.Unmarshal(trimmed, &structured); err == nil {
		options := make([]SessionConfigOption, 0, len(structured))
		for _, item := range structured {
			choices := make([]SessionConfigOptionChoice, 0, len(item.Options))
			for _, choice := range item.Options {
				choices = append(choices, SessionConfigOptionChoice{
					Name:        choice.Name,
					Description: choice.Description,
				})
			}
			options = append(options, SessionConfigOption{
				Name:         item.Name,
				CurrentValue: item.CurrentValue,
				Options:      choices,
			})
		}
		return statusConfigOptions(options...)
	}

	var flat map[string]any
	if err := json.Unmarshal(trimmed, &flat); err == nil {
		keys := make([]string, 0, len(flat))
		for key := range flat {
			keys = append(keys, key)
		}
		sort.Strings(keys)

		options := make([]SessionConfigOption, 0, len(flat))
		for _, key := range keys {
			option := SessionConfigOption{
				Name:         key,
				CurrentValue: stringifyStatusConfigValue(flat[key]),
			}
			options = append(options, option)
		}
		return statusConfigOptions(options...)
	}

	return []SessionConfigOption{
		{
			Name:         "raw",
			CurrentValue: strings.TrimSpace(string(trimmed)),
		},
	}
}

func stringifyStatusConfigValue(value any) string {
	switch typed := value.(type) {
	case nil:
		return "null"
	case string:
		return strings.TrimSpace(typed)
	default:
		encoded, err := json.Marshal(typed)
		if err != nil {
			return strings.TrimSpace(fmt.Sprint(typed))
		}
		return string(encoded)
	}
}

func joinPromptBlocks(parts ...string) string {
	nonEmpty := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			nonEmpty = append(nonEmpty, trimmed)
		}
	}

	return strings.Join(nonEmpty, "\n\n")
}

func abbreviateLogText(text string, limit int) string {
	text = strings.TrimSpace(text)
	if limit <= 0 || len(text) <= limit {
		return text
	}
	if limit <= 3 {
		return text[:limit]
	}

	return text[:limit-3] + "..."
}

func allImagePaths(primary string, extra []string) []string {
	return allPaths(primary, extra)
}

func allFilePaths(primary string, extra []string) []string {
	return allPaths(primary, extra)
}

func allVideoPaths(primary string, extra []string) []string {
	return allPaths(primary, extra)
}

func allPaths(primary string, extra []string) []string {
	paths := make([]string, 0, 1+len(extra))
	seen := make(map[string]struct{}, 1+len(extra))
	appendPath := func(path string) {
		path = strings.TrimSpace(path)
		if path == "" {
			return
		}
		if _, ok := seen[path]; ok {
			return
		}
		seen[path] = struct{}{}
		paths = append(paths, path)
	}

	appendPath(primary)
	for _, path := range extra {
		appendPath(path)
	}

	return paths
}

func temporaryFilePathNotice() string {
	return "Path validity: local file paths are temporary and only valid for this turn."
}

func normalizeSessionTurnRequest(req TurnRequest, sessionConversationKey string, sessionRunnerThreadID string) (TurnRequest, error) {
	sessionConversationKey = strings.TrimSpace(sessionConversationKey)
	requestConversationKey := strings.TrimSpace(req.Conversation.Key)
	if requestConversationKey == "" {
		req.Conversation.Key = sessionConversationKey
	} else if sessionConversationKey != "" && requestConversationKey != sessionConversationKey {
		return TurnRequest{}, fmt.Errorf(
			"conversation key mismatch: session=%q request=%q",
			sessionConversationKey,
			requestConversationKey,
		)
	}

	sessionRunnerThreadID = strings.TrimSpace(sessionRunnerThreadID)
	requestRunnerThreadID := strings.TrimSpace(req.Conversation.RunnerThreadID)
	if requestRunnerThreadID == "" {
		req.Conversation.RunnerThreadID = sessionRunnerThreadID
	} else if sessionRunnerThreadID != "" && requestRunnerThreadID != sessionRunnerThreadID {
		return TurnRequest{}, fmt.Errorf(
			"runner thread id mismatch: session=%q request=%q",
			sessionRunnerThreadID,
			requestRunnerThreadID,
		)
	}

	return req, nil
}
