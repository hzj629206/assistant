package config

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/hzj629206/assistant/seatalk"
)

// Config contains the process-wide runtime configuration.
type Config struct {
	ListenAddr string         `json:"listen_addr" yaml:"listen_addr"`
	Tunnel     TunnelConfig   `json:"tunnel" yaml:"tunnel"`
	Codex      CodexConfig    `json:"codex" yaml:"codex"`
	Claude     ClaudeConfig   `json:"claude" yaml:"claude"`
	SeaTalk    seatalk.Config `json:"seatalk" yaml:"seatalk"`
}

// TunnelConfig contains optional public tunnel settings.
type TunnelConfig struct {
	SSHAddr          string `json:"ssh_addr" yaml:"ssh_addr"`
	SSHUser          string `json:"ssh_user" yaml:"ssh_user"`
	SSHKey           string `json:"ssh_key" yaml:"ssh_key"`
	CloudflaredToken string `json:"cloudflared_token" yaml:"cloudflared_token"`
}

// CodexConfig contains runner selection and model options.
type CodexConfig struct {
	Backend                 string   `json:"backend" yaml:"backend"`
	Model                   string   `json:"model" yaml:"model"`
	ReasoningEffort         string   `json:"reasoning_effort" yaml:"reasoning_effort"`
	Sandbox                 string   `json:"sandbox" yaml:"sandbox"`
	AdditionalWritableRoots []string `json:"additional_writable_roots" yaml:"additional_writable_roots"`
}

// ClaudeConfig contains the minimal Claude Code CLI daemon configuration.
// All other Claude Code capabilities should be configured directly in the CLI environment.
type ClaudeConfig struct {
	Model                 string   `json:"model" yaml:"model"`
	ReasoningEffort       string   `json:"reasoning_effort" yaml:"reasoning_effort"`
	Permission            string   `json:"permission" yaml:"permission"`
	AdditionalDirectories []string `json:"additional_directories" yaml:"additional_directories"`
}

type flagOverlay struct {
	listenAddr            string
	codexBackend          string
	codexModel            string
	codexReasoningEffort  string
	codexSandbox          string
	claudeModel           string
	claudeReasoningEffort string
	claudePermission      string
}

// ParseConfig loads defaults, then an optional config file, then a limited set of command-line overrides.
func ParseConfig(programName string, args []string) (Config, error) {
	if strings.TrimSpace(programName) == "" {
		return Config{}, errors.New("program name is required")
	}

	cfg, err := defaultConfig()
	if err != nil {
		return Config{}, err
	}

	overlay := flagOverlay{
		listenAddr:            "",
		codexBackend:          cfg.Codex.Backend,
		codexModel:            cfg.Codex.Model,
		codexReasoningEffort:  cfg.Codex.ReasoningEffort,
		codexSandbox:          cfg.Codex.Sandbox,
		claudeModel:           cfg.Claude.Model,
		claudeReasoningEffort: cfg.Claude.ReasoningEffort,
		claudePermission:      cfg.Claude.Permission,
	}

	var configPath string
	fs := newFlagSet(programName, &overlay, &configPath)
	if err = fs.Parse(args); err != nil {
		return Config{}, err
	}

	if configPath != "" {
		if err = loadConfigFile(configPath, &cfg); err != nil {
			return Config{}, err
		}
	} else if configPath, err = defaultConfigPath(); err != nil {
		return Config{}, err
	} else if err = loadOptionalConfigFile(configPath, &cfg); err != nil {
		return Config{}, err
	}
	if err = normalizeCodexConfig(&cfg.Codex); err != nil {
		return Config{}, err
	}
	normalizeClaudeConfig(&cfg.Claude)
	fs.Visit(func(f *flag.Flag) {
		applyFlagOverride(&cfg, overlay, f.Name)
	})
	if err = normalizeCodexConfig(&cfg.Codex); err != nil {
		return Config{}, err
	}
	normalizeClaudeConfig(&cfg.Claude)
	if err = validateSeaTalkConfig(&cfg.SeaTalk); err != nil {
		return Config{}, err
	}

	return cfg, nil
}

func defaultConfig() (Config, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return Config{}, fmt.Errorf("resolve home dir failed: %w", err)
	}

	return Config{
		ListenAddr: ":8421",
		Tunnel: TunnelConfig{
			SSHAddr:          "",
			SSHUser:          "",
			SSHKey:           filepath.Join(homeDir, ".ssh", "id_rsa"),
			CloudflaredToken: "",
		},
		Codex: CodexConfig{
			Backend: "appserver",
		},
		Claude: ClaudeConfig{},
		SeaTalk: seatalk.Config{
			AppID:               "",
			AppSecret:           "",
			SigningSecret:       "",
			EmployeeInfoEnabled: false,
		},
	}, nil
}

func newFlagSet(programName string, overlay *flagOverlay, configPath *string) *flag.FlagSet {
	fs := flag.NewFlagSet(programName, flag.ContinueOnError)
	fs.StringVar(&overlay.listenAddr, "listen-addr", overlay.listenAddr, "HTTP server listen address override")
	fs.StringVar(&overlay.codexBackend, "codex-backend", overlay.codexBackend, "Codex backend override")
	fs.StringVar(&overlay.codexModel, "codex-model", overlay.codexModel, "Codex model name override")
	fs.StringVar(&overlay.codexReasoningEffort, "codex-reasoning-effort", overlay.codexReasoningEffort, "Codex reasoning effort override")
	fs.StringVar(&overlay.codexSandbox, "codex-sandbox", overlay.codexSandbox, "Codex sandbox override")
	fs.StringVar(&overlay.claudeModel, "claude-model", overlay.claudeModel, "Claude model name override")
	fs.StringVar(&overlay.claudeReasoningEffort, "claude-reasoning-effort", overlay.claudeReasoningEffort, "Claude effort override")
	fs.StringVar(&overlay.claudePermission, "claude-permission", overlay.claudePermission, "Claude permission mode override")
	fs.StringVar(configPath, "f", *configPath, "path to config file")
	fs.StringVar(configPath, "config", *configPath, "path to config file")
	return fs
}

func applyFlagOverride(cfg *Config, overlay flagOverlay, name string) {
	switch name {
	case "listen-addr":
		cfg.ListenAddr = strings.TrimSpace(overlay.listenAddr)
	case "codex-backend":
		cfg.Codex.Backend = overlay.codexBackend
	case "codex-model":
		cfg.Codex.Model = overlay.codexModel
	case "codex-reasoning-effort":
		cfg.Codex.ReasoningEffort = overlay.codexReasoningEffort
	case "codex-sandbox":
		cfg.Codex.Sandbox = overlay.codexSandbox
	case "claude-model":
		cfg.Claude.Model = overlay.claudeModel
	case "claude-reasoning-effort":
		cfg.Claude.ReasoningEffort = overlay.claudeReasoningEffort
	case "claude-permission":
		cfg.Claude.Permission = overlay.claudePermission
	}
}

func loadConfigFile(path string, cfg *Config) error {
	log.Printf("loading config file: %s", path)

	//nolint:gosec // Config file path is an explicit local runtime input.
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open config file %q failed: %w", path, err)
	}
	defer func() {
		_ = file.Close()
	}()

	decoder := yaml.NewDecoder(file)
	decoder.KnownFields(true)
	if err := decoder.Decode(cfg); err != nil {
		return fmt.Errorf("decode config file %q failed: %w", path, err)
	}
	return nil
}

func loadOptionalConfigFile(path string, cfg *Config) error {
	if strings.TrimSpace(path) == "" {
		return nil
	}

	if err := loadConfigFile(path, cfg); err != nil {
		var pathErr *os.PathError
		if errors.As(err, &pathErr) && errors.Is(pathErr.Err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	return nil
}

func defaultConfigPath() (string, error) {
	homeDir := strings.TrimSpace(os.Getenv("HOME"))
	if homeDir == "" {
		var err error
		homeDir, err = os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home dir failed: %w", err)
		}
	}

	return filepath.Join(homeDir, ".assistant", "config.yml"), nil
}

func normalizeCodexConfig(cfg *CodexConfig) error {
	if cfg == nil {
		return nil
	}

	cfg.Backend = strings.TrimSpace(strings.ToLower(cfg.Backend))
	cfg.Model = strings.TrimSpace(cfg.Model)
	cfg.ReasoningEffort = strings.TrimSpace(strings.ToLower(cfg.ReasoningEffort))
	cfg.Sandbox = strings.TrimSpace(strings.ToLower(cfg.Sandbox))
	cfg.AdditionalWritableRoots = normalizeCodexAdditionalWritableRoots(cfg.AdditionalWritableRoots)

	if cfg.Backend == "" {
		cfg.Backend = "appserver"
	}

	switch cfg.Backend {
	case "appserver", "exec":
	default:
		return fmt.Errorf("unsupported codex backend %q", cfg.Backend)
	}

	switch cfg.ReasoningEffort {
	case "", "none", "minimal", "low", "medium", "high", "xhigh":
	default:
		return fmt.Errorf("unsupported codex reasoning effort %q", cfg.ReasoningEffort)
	}

	switch cfg.Sandbox {
	case "", "read-only", "workspace-write", "danger-full-access":
	default:
		return fmt.Errorf("unsupported codex sandbox %q", cfg.Sandbox)
	}

	return nil
}

func normalizeCodexAdditionalWritableRoots(roots []string) []string {
	if len(roots) == 0 {
		return nil
	}

	normalized := make([]string, 0, len(roots))
	seen := make(map[string]struct{}, len(roots))
	for _, root := range roots {
		root = strings.TrimSpace(root)
		if root == "" {
			continue
		}

		if absoluteRoot, err := filepath.Abs(root); err == nil {
			root = absoluteRoot
		}
		root = filepath.Clean(root)
		if _, ok := seen[root]; ok {
			continue
		}
		seen[root] = struct{}{}
		normalized = append(normalized, root)
	}
	if len(normalized) == 0 {
		return nil
	}
	return normalized
}

func normalizeClaudeConfig(cfg *ClaudeConfig) {
	if cfg == nil {
		return
	}

	cfg.Model = strings.TrimSpace(cfg.Model)
	cfg.Permission = normalizeClaudePermission(strings.TrimSpace(strings.ToLower(cfg.Permission)))
	cfg.ReasoningEffort = strings.TrimSpace(strings.ToLower(cfg.ReasoningEffort))
	switch cfg.ReasoningEffort {
	case "", "low", "medium", "high", "xhigh", "max":
	default:
		log.Printf("ignoring unsupported claude effort %q", cfg.ReasoningEffort)
		cfg.ReasoningEffort = ""
	}
	cfg.AdditionalDirectories = normalizeClaudeAdditionalDirectories(cfg.AdditionalDirectories)
}

func normalizeClaudePermission(value string) string {
	switch value {
	case "", "default", "auto", "plan":
		return value
	case "accept-edits":
		return "accept-edits"
	case "bypass-permissions":
		return "bypass-permissions"
	case "dont-ask":
		return "dont-ask"
	default:
		log.Printf("ignoring unsupported claude permission %q", value)
		return ""
	}
}

func normalizeClaudeAdditionalDirectories(paths []string) []string {
	if len(paths) == 0 {
		return nil
	}

	directories := make([]string, 0, len(paths))
	seen := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		directory := strings.TrimSpace(path)
		if directory == "" {
			continue
		}
		if absolutePath, err := filepath.Abs(directory); err == nil {
			directory = absolutePath
		}
		directory = filepath.Clean(directory)
		if _, ok := seen[directory]; ok {
			continue
		}
		seen[directory] = struct{}{}
		directories = append(directories, directory)
	}
	if len(directories) == 0 {
		return nil
	}

	return directories
}

func validateSeaTalkConfig(cfg *seatalk.Config) error {
	if cfg == nil {
		return nil
	}
	if strings.TrimSpace(cfg.AppID) == "" {
		return errors.New("seatalk app_id is required")
	}
	if strings.TrimSpace(cfg.AppSecret) == "" {
		return errors.New("seatalk app_secret is required")
	}
	if strings.TrimSpace(cfg.SigningSecret) == "" {
		return errors.New("seatalk signing_secret is required")
	}
	return nil
}

// NormalizeRemoteSSHAddr appends the default SSH port when the address omits one.
func NormalizeRemoteSSHAddr(addr string) string {
	if addr == "" {
		return ""
	}

	if _, _, err := net.SplitHostPort(addr); err == nil {
		return addr
	} else {
		var addrErr *net.AddrError
		if !errors.As(err, &addrErr) {
			return addr
		}
		if !strings.Contains(addrErr.Err, "missing port") && !strings.Contains(addrErr.Err, "too many colons") {
			return addr
		}
	}

	if strings.HasPrefix(addr, "[") && strings.HasSuffix(addr, "]") {
		addr = strings.TrimPrefix(strings.TrimSuffix(addr, "]"), "[")
	}

	return net.JoinHostPort(addr, "22")
}

// DeriveLocalTargetAddr maps the listen address to a loopback TCP target for local tunnel forwarding.
func DeriveLocalTargetAddr(listenAddr string) (string, error) {
	host, port, err := net.SplitHostPort(listenAddr)
	if err != nil {
		return "", fmt.Errorf("split listen addr %q failed: %w", listenAddr, err)
	}

	host = strings.Trim(strings.TrimSpace(host), "[]")
	switch host {
	case "", "0.0.0.0", "::":
		host = "127.0.0.1"
	}

	return net.JoinHostPort(host, port), nil
}
