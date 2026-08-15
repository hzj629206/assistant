package codex

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

const internalOriginatorEnv = "CODEX_INTERNAL_ORIGINATOR_OVERRIDE"
const goSDKOriginator = "codex_sdk_go"

// CodexExecArgs describes CLI arguments for codex exec.
type CodexExecArgs struct {
	Input string

	BaseURL  string
	APIKey   string
	Config   map[string]any
	ThreadID string
	Images   []string

	Model                 string
	SandboxMode           SandboxMode
	WorkingDirectory      string
	AdditionalDirectories []string
	SkipGitRepoCheck      bool
	OutputSchemaFile      string
	ModelReasoningEffort  ModelReasoningEffort
	NetworkAccessEnabled  *bool
	WebSearchMode         WebSearchMode
	WebSearchEnabled      *bool
	ApprovalPolicy        ApprovalMode

	//nolint:containedctx // The caller owns turn cancellation and the process inherits it.
	Context context.Context
}

// ExecStream streams lines from the Codex CLI.
type ExecStream struct {
	Lines <-chan string
	wait  func() error
}

// Wait blocks until the process exits and returns any error.
func (s *ExecStream) Wait() error {
	if s == nil || s.wait == nil {
		return nil
	}
	return s.wait()
}

// CodexExec launches the Codex CLI.
type CodexExec struct {
	executablePath string
	envOverride    map[string]string
}

// NewCodexExec creates a new exec wrapper.
func NewCodexExec(executablePath string, env map[string]string) *CodexExec {
	path := executablePath
	if path == "" {
		path = findCodexPath()
	}
	return &CodexExec{
		executablePath: path,
		envOverride:    env,
	}
}

// Run launches the Codex CLI and streams JSONL lines.
func (c *CodexExec) Run(args CodexExecArgs) (*ExecStream, error) {
	commandArgs, err := buildCommandArgs(args)
	if err != nil {
		return nil, err
	}

	ctx := args.Context
	if ctx == nil {
		ctx = context.Background()
	}

	//nolint:gosec // The binary path and args are explicit Codex runner configuration.
	cmd := exec.CommandContext(ctx, c.executablePath, commandArgs...)
	cmd.Env = buildEnv(c.envOverride, args.BaseURL, args.APIKey)
	cmd.Stdin = strings.NewReader(args.Input)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return nil, err
	}

	lines := make(chan string)
	scanErrCh := make(chan error, 1)

	go func() {
		defer close(lines)
		defer close(scanErrCh)
		scanner := bufio.NewScanner(stdout)
		buf := make([]byte, 0, 64*1024)
		scanner.Buffer(buf, 10*1024*1024)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
		if err := scanner.Err(); err != nil {
			scanErrCh <- err
		}
	}()

	wait := func() error {
		var scanErr error
		for err := range scanErrCh {
			if err != nil {
				scanErr = err
			}
		}
		waitErr := cmd.Wait()
		if scanErr != nil {
			return scanErr
		}
		if waitErr == nil {
			return nil
		}
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			return fmt.Errorf("codex exec exited with %v: %s", exitErr.ProcessState, strings.TrimSpace(stderr.String()))
		}
		return waitErr
	}

	return &ExecStream{Lines: lines, wait: wait}, nil
}

func buildCommandArgs(args CodexExecArgs) ([]string, error) {
	commandArgs := []string{"exec", "--experimental-json"}

	configOverrides, err := flattenConfigOverrides(args.Config)
	if err != nil {
		return nil, err
	}
	for _, override := range configOverrides {
		commandArgs = appendConfigOverride(commandArgs, override)
	}

	if args.Model != "" {
		commandArgs = append(commandArgs, "--model", args.Model)
	}
	if args.SandboxMode != "" {
		commandArgs = append(commandArgs, "--sandbox", string(args.SandboxMode))
	}
	if args.WorkingDirectory != "" {
		commandArgs = append(commandArgs, "--cd", args.WorkingDirectory)
	}
	for _, dir := range args.AdditionalDirectories {
		commandArgs = append(commandArgs, "--add-dir", dir)
	}
	if args.SkipGitRepoCheck {
		commandArgs = append(commandArgs, "--skip-git-repo-check")
	}
	if args.OutputSchemaFile != "" {
		commandArgs = append(commandArgs, "--output-schema", args.OutputSchemaFile)
	}
	if args.ModelReasoningEffort != "" {
		commandArgs = appendConfigOverride(commandArgs, fmt.Sprintf("model_reasoning_effort=%q", args.ModelReasoningEffort))
	}
	if args.NetworkAccessEnabled != nil {
		commandArgs = appendConfigOverride(commandArgs, fmt.Sprintf("sandbox_workspace_write.network_access=%t", *args.NetworkAccessEnabled))
	}
	if args.WebSearchMode != "" {
		commandArgs = appendConfigOverride(commandArgs, fmt.Sprintf("web_search=%q", args.WebSearchMode))
	} else if args.WebSearchEnabled != nil {
		if *args.WebSearchEnabled {
			commandArgs = appendConfigOverride(commandArgs, "web_search=\"live\"")
		} else {
			commandArgs = appendConfigOverride(commandArgs, "web_search=\"disabled\"")
		}
	}
	if args.ApprovalPolicy != "" {
		commandArgs = appendConfigOverride(commandArgs, fmt.Sprintf("approval_policy=%q", args.ApprovalPolicy))
	}
	for _, image := range args.Images {
		commandArgs = append(commandArgs, "--image", image)
	}
	if args.ThreadID != "" {
		commandArgs = append(commandArgs, "resume", args.ThreadID)
	}

	return commandArgs, nil
}

func appendConfigOverride(commandArgs []string, override string) []string {
	return append(commandArgs, "--config", override)
}

func flattenConfigOverrides(config map[string]any) ([]string, error) {
	if len(config) == 0 {
		return nil, nil
	}

	keys := make([]string, 0, len(config))
	for key := range config {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	overrides := make([]string, 0, len(config))
	for _, key := range keys {
		flattened, err := flattenConfigValue(key, reflect.ValueOf(config[key]))
		if err != nil {
			return nil, err
		}
		overrides = append(overrides, flattened...)
	}
	return overrides, nil
}

func flattenConfigValue(path string, value reflect.Value) ([]string, error) {
	value = unwrapReflectValue(value)
	if !value.IsValid() {
		return nil, fmt.Errorf("config %s: nil values are not supported", path)
	}

	if value.Kind() == reflect.Map {
		if value.Type().Key().Kind() != reflect.String {
			return nil, fmt.Errorf("config %s: map keys must be strings", path)
		}

		keys := make([]string, 0, value.Len())
		iter := value.MapRange()
		for iter.Next() {
			keys = append(keys, iter.Key().String())
		}
		sort.Strings(keys)

		overrides := make([]string, 0, value.Len())
		for _, key := range keys {
			childPath := key
			if path != "" {
				childPath = path + "." + key
			}
			flattened, err := flattenConfigValue(childPath, value.MapIndex(reflect.ValueOf(key)))
			if err != nil {
				return nil, err
			}
			overrides = append(overrides, flattened...)
		}
		return overrides, nil
	}

	literal, err := encodeTOMLLiteral(value)
	if err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	return []string{path + "=" + literal}, nil
}

func encodeTOMLLiteral(value reflect.Value) (string, error) {
	value = unwrapReflectValue(value)
	if !value.IsValid() {
		return "", errors.New("nil values are not supported")
	}

	if number, ok := value.Interface().(json.Number); ok {
		return number.String(), nil
	}

	switch value.Kind() {
	case reflect.String:
		encoded, err := json.Marshal(value.String())
		if err != nil {
			return "", err
		}
		return string(encoded), nil
	case reflect.Bool:
		return strconv.FormatBool(value.Bool()), nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.FormatInt(value.Int(), 10), nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return strconv.FormatUint(value.Uint(), 10), nil
	case reflect.Float32, reflect.Float64:
		return formatTOMLFloat(value.Float()), nil
	case reflect.Slice, reflect.Array:
		parts := make([]string, 0, value.Len())
		for i := range value.Len() {
			part, err := encodeTOMLLiteral(value.Index(i))
			if err != nil {
				return "", err
			}
			parts = append(parts, part)
		}
		return "[" + strings.Join(parts, ", ") + "]", nil
	case reflect.Map:
		if value.Type().Key().Kind() != reflect.String {
			return "", errors.New("map keys must be strings")
		}

		keys := make([]string, 0, value.Len())
		iter := value.MapRange()
		for iter.Next() {
			keys = append(keys, iter.Key().String())
		}
		sort.Strings(keys)

		parts := make([]string, 0, len(keys))
		for _, key := range keys {
			part, err := encodeTOMLLiteral(value.MapIndex(reflect.ValueOf(key)))
			if err != nil {
				return "", err
			}
			parts = append(parts, key+"="+part)
		}
		return "{" + strings.Join(parts, ", ") + "}", nil
	default:
		return "", fmt.Errorf("unsupported value type %s", value.Kind())
	}
}

func unwrapReflectValue(value reflect.Value) reflect.Value {
	for value.IsValid() && (value.Kind() == reflect.Interface || value.Kind() == reflect.Pointer) {
		if value.IsNil() {
			return reflect.Value{}
		}
		value = value.Elem()
	}
	return value
}

func formatTOMLFloat(value float64) string {
	switch {
	case math.IsNaN(value):
		return "nan"
	case math.IsInf(value, 1):
		return "inf"
	case math.IsInf(value, -1):
		return "-inf"
	default:
		return strconv.FormatFloat(value, 'g', -1, 64)
	}
}

func buildEnv(override map[string]string, baseURL string, apiKey string) []string {
	env := map[string]string{}
	if override != nil {
		maps.Copy(env, override)
	} else {
		for _, entry := range os.Environ() {
			parts := strings.SplitN(entry, "=", 2)
			if len(parts) == 2 {
				env[parts[0]] = parts[1]
			}
		}
	}
	if _, ok := env[internalOriginatorEnv]; !ok {
		env[internalOriginatorEnv] = goSDKOriginator
	}
	if baseURL != "" {
		env["OPENAI_BASE_URL"] = baseURL
	}
	if apiKey != "" {
		env["CODEX_API_KEY"] = apiKey
	}
	list := make([]string, 0, len(env))
	for key, value := range env {
		list = append(list, key+"="+value)
	}
	return list
}

func findCodexPath() string {
	if path, err := exec.LookPath("codex"); err == nil {
		return path
	}

	targetTriple := ""
	switch runtime.GOOS {
	case "linux", "android":
		switch runtime.GOARCH {
		case "amd64":
			targetTriple = "x86_64-unknown-linux-musl"
		case "arm64":
			targetTriple = "aarch64-unknown-linux-musl"
		}
	case "darwin":
		switch runtime.GOARCH {
		case "amd64":
			targetTriple = "x86_64-apple-darwin"
		case "arm64":
			targetTriple = "aarch64-apple-darwin"
		}
	case "windows":
		switch runtime.GOARCH {
		case "amd64":
			targetTriple = "x86_64-pc-windows-msvc"
		case "arm64":
			targetTriple = "aarch64-pc-windows-msvc"
		}
	}
	if targetTriple == "" {
		panic(fmt.Sprintf("unsupported platform: %s (%s)", runtime.GOOS, runtime.GOARCH))
	}

	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		panic("unable to determine codex sdk path")
	}
	moduleDir := filepath.Dir(currentFile)
	vendorRoot := filepath.Join(moduleDir, "vendor")
	archRoot := filepath.Join(vendorRoot, targetTriple)
	codexBinaryName := "codex"
	if runtime.GOOS == "windows" {
		codexBinaryName = "codex.exe"
	}
	return filepath.Join(archRoot, "codex", codexBinaryName)
}
