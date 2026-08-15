package codex

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Turn represents a completed turn.
type Turn struct {
	Items         []ThreadItem
	FinalResponse string
	Usage         *Usage
}

// StreamedTurn is the result of RunStreamed.
type StreamedTurn struct {
	Events <-chan ThreadEvent
	Done   <-chan error
}

// Thread represents a conversation with the agent.
type Thread struct {
	exec          *CodexExec
	options       CodexOptions
	id            string
	threadOptions ThreadOptions
}

// ID returns the thread identifier. It is populated after the first turn starts.
func (t *Thread) ID() string {
	return t.id
}

// NewThread constructs a thread.
func NewThread(exec *CodexExec, options CodexOptions, threadOptions ThreadOptions, id string) *Thread {
	return &Thread{
		exec:          exec,
		options:       options,
		id:            id,
		threadOptions: threadOptions,
	}
}

// RunStreamed sends input to the agent and streams events.
func (t *Thread) RunStreamed(input Input, turnOptions TurnOptions) (*StreamedTurn, error) {
	schemaFile, err := createOutputSchemaFile(turnOptions.OutputSchema)
	if err != nil {
		return nil, err
	}

	prompt, images := normalizeInput(input)
	options := t.threadOptions
	stream, err := t.exec.Run(CodexExecArgs{
		Input:                 prompt,
		BaseURL:               t.options.BaseURL,
		APIKey:                t.options.APIKey,
		Config:                t.options.Config,
		ThreadID:              t.id,
		Images:                images,
		Model:                 options.Model,
		SandboxMode:           options.SandboxMode,
		WorkingDirectory:      options.WorkingDirectory,
		SkipGitRepoCheck:      options.SkipGitRepoCheck,
		OutputSchemaFile:      schemaFile.SchemaPath,
		ModelReasoningEffort:  options.ModelReasoningEffort,
		Context:               turnOptions.Context,
		NetworkAccessEnabled:  options.NetworkAccessEnabled,
		WebSearchMode:         options.WebSearchMode,
		WebSearchEnabled:      options.WebSearchEnabled,
		ApprovalPolicy:        options.ApprovalPolicy,
		AdditionalDirectories: options.AdditionalDirectories,
	})
	if err != nil {
		_ = schemaFile.Cleanup()
		return nil, err
	}

	events := make(chan ThreadEvent)
	done := make(chan error, 1)

	go func() {
		defer close(events)
		var streamErr error

		for line := range stream.Lines {
			var event ThreadEvent
			if err := json.Unmarshal([]byte(line), &event); err != nil {
				if streamErr == nil {
					streamErr = fmt.Errorf("failed to parse item: %s: %w", line, err)
				}
				continue
			}
			if event.Type == "thread.started" && event.ThreadID != "" {
				t.id = event.ThreadID
			}
			events <- event
		}

		if streamErr == nil {
			streamErr = stream.Wait()
		} else {
			_ = stream.Wait()
		}

		if err := schemaFile.Cleanup(); err != nil && streamErr == nil {
			streamErr = err
		}
		done <- streamErr
		close(done)
	}()

	return &StreamedTurn{Events: events, Done: done}, nil
}

// Run sends input to the agent and returns the completed turn.
func (t *Thread) Run(input Input, turnOptions TurnOptions) (*Turn, error) {
	streamed, err := t.RunStreamed(input, turnOptions)
	if err != nil {
		return nil, err
	}

	var items []ThreadItem
	var finalResponse string
	var usage *Usage
	var turnFailure *ThreadError

	for event := range streamed.Events {
		switch event.Type {
		case "item.completed":
			if event.Item != nil {
				items = append(items, event.Item)
				if msg, ok := event.Item.(*AgentMessageItem); ok {
					finalResponse = msg.Text
				}
			}
		case "turn.completed":
			usage = event.Usage
		case "turn.failed":
			turnFailure = event.Error
		}
	}

	if err := <-streamed.Done; err != nil {
		return nil, err
	}
	if turnFailure != nil {
		return nil, errors.New(turnFailure.Message)
	}
	return &Turn{Items: items, FinalResponse: finalResponse, Usage: usage}, nil
}

func normalizeInput(input Input) (string, []string) {
	if len(input.Items) == 0 {
		return input.Text, nil
	}
	promptParts := make([]string, 0, len(input.Items))
	images := make([]string, 0, len(input.Items))
	for _, item := range input.Items {
		switch item.Type {
		case UserInputText:
			promptParts = append(promptParts, item.Text)
		case UserInputLocalImage:
			images = append(images, item.Path)
		}
	}
	return stringsJoin(promptParts, "\n\n"), images
}

func stringsJoin(parts []string, sep string) string {
	return strings.Join(parts, sep)
}
