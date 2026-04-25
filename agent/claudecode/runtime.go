package claudecode

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
)

const (
	defaultEntrypoint = "sdk-py"
	defaultSDKVersion = "go-sdk"
)

// StreamRoleMessage represents a decoded assistant or user stream message payload.
type StreamRoleMessage struct {
	Role    string               `json:"role"`
	Content []StreamContentBlock `json:"content"`
}

// StreamContentBlock represents one content block inside a role message.
type StreamContentBlock struct {
	Type     string         `json:"type"`
	Name     string         `json:"name,omitempty"`
	Text     string         `json:"text,omitempty"`
	Thinking string         `json:"thinking,omitempty"`
	Input    map[string]any `json:"input,omitempty"`
	IsError  bool           `json:"is_error,omitempty"`
	Content  any            `json:"content,omitempty"`
}

// ResolveSessionID returns the latest non-empty Claude session ID.
func ResolveSessionID(previous string, result *ClaudeResult) string {
	if result != nil && strings.TrimSpace(result.SessionID) != "" {
		return strings.TrimSpace(result.SessionID)
	}

	return previous
}

// BuildUserContentBlocks builds Claude user content blocks from text and local images.
func BuildUserContentBlocks(prompt string, imagePaths []string) ([]map[string]any, error) {
	content := make([]map[string]any, 0, 1+len(imagePaths))
	if text := strings.TrimSpace(prompt); text != "" {
		content = append(content, map[string]any{
			"type": "text",
			"text": text,
		})
	}

	for _, imagePath := range imagePaths {
		block, err := BuildImageContentBlock(imagePath)
		if err != nil {
			return nil, err
		}
		content = append(content, block)
	}

	if len(content) == 0 {
		content = append(content, map[string]any{
			"type": "text",
			"text": "",
		})
	}

	return content, nil
}

// MarshalStreamJSONUserInput encodes one Claude stream-json user input event.
func MarshalStreamJSONUserInput(blocks []map[string]any) ([]byte, error) {
	content := blocks
	if len(content) == 0 {
		content = []map[string]any{{
			"type": "text",
			"text": "",
		}}
	}

	payload := map[string]any{
		"type": "user",
		"message": map[string]any{
			"role":    "user",
			"content": content,
		},
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode claude stream input failed: %w", err)
	}

	return append(data, '\n'), nil
}

// BuildImageContentBlock encodes one local image attachment for Claude stream-json input.
func BuildImageContentBlock(imagePath string) (map[string]any, error) {
	imagePath = strings.TrimSpace(imagePath)
	if imagePath == "" {
		return nil, errors.New("image path is empty")
	}

	//nolint:gosec // Image paths come from normalized local attachments selected by the host application.
	imageBytes, err := os.ReadFile(imagePath)
	if err != nil {
		return nil, fmt.Errorf("read image %q failed: %w", imagePath, err)
	}
	mediaType := http.DetectContentType(imageBytes)
	if !strings.HasPrefix(mediaType, "image/") {
		return nil, fmt.Errorf("unsupported image media type %q for %s", mediaType, imagePath)
	}

	return map[string]any{
		"type": "image",
		"source": map[string]any{
			"type":       "base64",
			"media_type": mediaType,
			"data":       base64.StdEncoding.EncodeToString(imageBytes),
		},
	}, nil
}

// DecodeStreamRoleMessage decodes one stream role message payload.
func DecodeStreamRoleMessage(msg *Message) (StreamRoleMessage, error) {
	if msg == nil || len(msg.Message) == 0 {
		return StreamRoleMessage{}, nil
	}

	var roleMessage StreamRoleMessage
	err := json.Unmarshal(msg.Message, &roleMessage)
	if err != nil {
		return StreamRoleMessage{}, err
	}

	return roleMessage, nil
}

// ParseResultUsage extracts input and output token usage from a result envelope.
func ParseResultUsage(envelope map[string]any) (int, int) {
	usage, _ := envelope["usage"].(map[string]any)
	if usage == nil {
		return 0, 0
	}

	return parseUsageInt(usage["input_tokens"]), parseUsageInt(usage["output_tokens"])
}

func parseUsageInt(value any) int {
	switch typed := value.(type) {
	case float64:
		return int(typed)
	case int:
		return typed
	case int64:
		return int(typed)
	case json.Number:
		number, err := typed.Int64()
		if err != nil {
			return 0
		}
		return int(number)
	default:
		return 0
	}
}

// TempFileExists reports whether a cached argument file still exists.
func TempFileExists(path string) (bool, error) {
	info, err := os.Stat(path)
	if err == nil {
		return !info.IsDir(), nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}

// WriteArgumentTempFile writes one temp file used for Claude CLI file-based arguments.
func WriteArgumentTempFile(name string, content string) (string, error) {
	if strings.TrimSpace(content) == "" {
		return "", nil
	}

	file, err := os.CreateTemp("", "claudecode-"+name+"-*.txt")
	if err != nil {
		return "", fmt.Errorf("create claude %s temp file failed: %w", name, err)
	}
	path := file.Name()
	_, err = file.WriteString(content)
	if closeErr := file.Close(); closeErr != nil && err == nil {
		err = closeErr
	}
	if err != nil {
		RemoveTempFile(path)
		return "", fmt.Errorf("write claude %s temp file failed: %w", name, err)
	}

	return path, nil
}

// ReplaceArgWithFile swaps an inline CLI argument for its file-based variant.
func ReplaceArgWithFile(args []string, inlineFlag string, fileFlag string, filePath string) []string {
	for index := 0; index < len(args)-1; index++ {
		if args[index] != inlineFlag {
			continue
		}

		replaced := make([]string, 0, len(args))
		replaced = append(replaced, args[:index]...)
		replaced = append(replaced, fileFlag, filePath)
		replaced = append(replaced, args[index+2:]...)
		return replaced
	}

	return args
}

// RemoveTempFile removes one cached argument temp file.
func RemoveTempFile(path string) {
	_ = os.Remove(path)
}

func shouldUseFileArgument(path string) bool {
	path = strings.TrimSpace(path)
	if path == "" {
		return false
	}

	info, err := os.Stat(path)
	if err != nil {
		return false
	}

	return !info.IsDir()
}

// BuildStreamProcessArgs builds the Claude CLI argv for one persistent stream session.
func BuildStreamProcessArgs(opts *RunOptions) []string {
	if opts == nil {
		return BuildCLIArgsForInputAndOutput(nil, StreamJSONInput, StreamJSONOutput)
	}

	cloned := *opts
	cloned.NoSessionPersistence = false
	return BuildCLIArgsForInputAndOutput(&cloned, StreamJSONInput, StreamJSONOutput)
}

// BuildCLIArgsForInputAndOutput builds the Claude CLI argv for one explicit input/output format pair.
func BuildCLIArgsForInputAndOutput(opts *RunOptions, inputFormat InputFormat, outputFormat OutputFormat) []string {
	args := BuildCLIArgs(opts)
	if outputFormat != "" {
		args = append([]string{"--output-format", string(outputFormat)}, args...)
	}
	if inputFormat != "" {
		args = append(args, "--input-format", string(inputFormat))
	}
	if opts != nil && opts.OutputSchema != nil {
		data, err := json.Marshal(opts.OutputSchema)
		if err == nil {
			args = append(args, "--json-schema", string(data))
		}
	}
	return args
}

// BuildCLIArgs builds the Claude CLI argv for a run.
func BuildCLIArgs(opts *RunOptions) []string {
	args := make([]string, 0, 32)
	if opts == nil {
		return args
	}

	if opts.Verbose {
		args = append(args, "--verbose")
	}

	if opts.SystemPrompt != "" {
		args = append(args, "--system-prompt", opts.SystemPrompt)
	}
	if opts.AppendPrompt != "" {
		args = append(args, "--append-system-prompt", opts.AppendPrompt)
	}
	if len(opts.AllowedTools) > 0 {
		args = append(args, "--allowedTools", strings.Join(opts.AllowedTools, ","))
	}
	if len(opts.DisallowedTools) > 0 {
		args = append(args, "--disallowedTools", strings.Join(opts.DisallowedTools, ","))
	}
	args = append(args, "--permission-prompt-tool", "stdio")
	if opts.PermissionMode != "" {
		args = append(args, "--permission-mode", string(opts.PermissionMode))
	}
	if opts.ResumeID != "" {
		args = append(args, "--resume", opts.ResumeID)
	} else if opts.Continue {
		args = append(args, "--continue")
	}
	if opts.SessionID != "" {
		args = append(args, "--session-id", opts.SessionID)
	}
	if opts.ForkSession {
		args = append(args, "--fork-session")
	}
	if opts.ModelAlias != "" {
		args = append(args, "--model", opts.ModelAlias)
	} else if opts.Model != "" {
		args = append(args, "--model", opts.Model)
	}
	if opts.Effort != "" {
		args = append(args, "--effort", string(opts.Effort))
	}
	if opts.Settings != "" {
		args = append(args, "--settings", opts.Settings)
	}
	for _, dir := range opts.AddDirectories {
		args = append(args, "--add-dir", dir)
	}
	if len(opts.MCPConfigs) > 0 {
		args = append(args, "--mcp-config")
		args = append(args, opts.MCPConfigs...)
	}
	if opts.StrictMCPConfig {
		args = append(args, "--strict-mcp-config")
	}
	if len(opts.SettingSources) > 0 {
		args = append(args, "--setting-sources", strings.Join(opts.SettingSources, ","))
	}
	if len(opts.Tools) > 0 {
		args = append(args, "--tools", strings.Join(opts.Tools, ","))
	}
	if opts.IncludeHookEvents {
		args = append(args, "--include-hook-events")
	}
	if opts.IncludePartialMessages {
		args = append(args, "--include-partial-messages")
	}
	if opts.ReplayUserMessages {
		args = append(args, "--replay-user-messages")
	}
	if opts.Debug {
		args = append(args, "--debug")
	}
	if opts.DebugToStderr {
		args = append(args, "--debug-to-stderr")
	}
	if opts.EnableAuthStatus {
		args = append(args, "--enable-auth-status")
	}
	if opts.DebugFile != "" {
		args = append(args, "--debug-file", opts.DebugFile)
	}
	if opts.Bare {
		args = append(args, "--bare")
	}
	if opts.Brief {
		args = append(args, "--brief")
	}
	if len(opts.Betas) > 0 {
		args = append(args, "--betas", strings.Join(opts.Betas, ","))
	}
	if len(opts.Files) > 0 {
		args = append(args, "--file")
		args = append(args, opts.Files...)
	}
	if opts.ExcludeDynamicSystemPromptSections {
		args = append(args, "--exclude-dynamic-system-prompt-sections")
	}
	if opts.MaxThinkingTokens > 0 {
		args = append(args, "--max-thinking-tokens", strconv.Itoa(opts.MaxThinkingTokens))
	}
	if opts.MaxBudgetUSD > 0 {
		args = append(args, "--max-budget-usd", fmt.Sprintf("%g", opts.MaxBudgetUSD))
	}
	if opts.NoSessionPersistence {
		args = append(args, "--no-session-persistence")
	}
	if opts.NoChrome {
		args = append(args, "--no-chrome")
	}

	if shouldUseFileArgument(opts.SystemPrompt) {
		args = ReplaceArgWithFile(args, "--system-prompt", "--system-prompt-file", opts.SystemPrompt)
	}
	if shouldUseFileArgument(opts.AppendPrompt) {
		args = ReplaceArgWithFile(args, "--append-system-prompt", "--append-system-prompt-file", opts.AppendPrompt)
	}

	return args
}

// BuildCLIEnv builds the environment for a Claude CLI subprocess.
func BuildCLIEnv(workingDirectory string) []string {
	env := make([]string, 0, len(os.Environ())+2)
	for _, item := range os.Environ() {
		if strings.HasPrefix(item, "CLAUDECODE=") {
			continue
		}
		env = append(env, item)
	}
	env = append(env, "CLAUDE_CODE_ENTRYPOINT="+defaultEntrypoint)
	env = append(env, "CLAUDE_AGENT_SDK_VERSION="+defaultSDKVersion)
	if workingDirectory != "" {
		env = append(env, "PWD="+workingDirectory)
	}
	return env
}
