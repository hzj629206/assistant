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
	ListenAddr    string         `json:"listen_addr" yaml:"listen_addr"`
	RemoteSSHAddr string         `json:"remote_ssh_addr" yaml:"remote_ssh_addr"`
	RemoteSSHUser string         `json:"remote_ssh_user" yaml:"remote_ssh_user"`
	SSHKeyPath    string         `json:"ssh_key_path" yaml:"ssh_key_path"`
	Codex         CodexConfig    `json:"codex" yaml:"codex"`
	SeaTalk       seatalk.Config `json:"seatalk" yaml:"seatalk"`
}

// CodexConfig contains runner selection and model options.
type CodexConfig struct {
	Backend         string `json:"backend" yaml:"backend"`
	Model           string `json:"model" yaml:"model"`
	ReasoningEffort string `json:"reasoning_effort" yaml:"reasoning_effort"`
	Sandbox         string `json:"sandbox" yaml:"sandbox"`
}

type flagOverlay struct {
	listenAddr           string
	codexBackend         string
	codexModel           string
	codexReasoningEffort string
	codexSandbox         string
}

// ParseConfig loads defaults, then an optional config file, then a limited set of command-line overrides.
func ParseConfig(args []string) (Config, error) {
	cfg, err := defaultConfig()
	if err != nil {
		return Config{}, err
	}

	overlay := flagOverlay{
		listenAddr:           "",
		codexBackend:         cfg.Codex.Backend,
		codexModel:           cfg.Codex.Model,
		codexReasoningEffort: cfg.Codex.ReasoningEffort,
		codexSandbox:         cfg.Codex.Sandbox,
	}

	var configPath string
	fs := newFlagSet(&overlay, &configPath)
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
	fs.Visit(func(f *flag.Flag) {
		applyFlagOverride(&cfg, overlay, f.Name)
	})
	if err = normalizeCodexConfig(&cfg.Codex); err != nil {
		return Config{}, err
	}
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
		ListenAddr:    ":8421",
		RemoteSSHAddr: "",
		RemoteSSHUser: "",
		SSHKeyPath:    filepath.Join(homeDir, ".ssh", "id_rsa"),
		Codex: CodexConfig{
			Backend:         "appserver",
			Model:           "gpt-5.4",
			ReasoningEffort: "low",
			Sandbox:         "read-only",
		},
		SeaTalk: seatalk.Config{
			AppID:               "",
			AppSecret:           "",
			SigningSecret:       "",
			EmployeeInfoEnabled: false,
		},
	}, nil
}

func newFlagSet(overlay *flagOverlay, configPath *string) *flag.FlagSet {
	fs := flag.NewFlagSet("assistant", flag.ContinueOnError)
	fs.StringVar(&overlay.listenAddr, "listen-addr", overlay.listenAddr, "HTTP server listen address override")
	fs.StringVar(&overlay.codexBackend, "codex-backend", overlay.codexBackend, "Codex backend override")
	fs.StringVar(&overlay.codexModel, "codex-model", overlay.codexModel, "Codex model name override")
	fs.StringVar(&overlay.codexReasoningEffort, "codex-reasoning-effort", overlay.codexReasoningEffort, "Codex reasoning effort override")
	fs.StringVar(&overlay.codexSandbox, "codex-sandbox", overlay.codexSandbox, "Codex sandbox override")
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

	if cfg.Backend == "" {
		cfg.Backend = "appserver"
	}
	if cfg.Model == "" {
		cfg.Model = "gpt-5.4"
	}
	if cfg.ReasoningEffort == "" {
		cfg.ReasoningEffort = "low"
	}
	if cfg.Sandbox == "" {
		cfg.Sandbox = "read-only"
	}

	switch cfg.Backend {
	case "appserver", "exec", "noop":
	default:
		return fmt.Errorf("unsupported codex backend %q", cfg.Backend)
	}

	switch cfg.ReasoningEffort {
	case "none", "minimal", "low", "medium", "high", "xhigh":
	default:
		return fmt.Errorf("unsupported codex reasoning effort %q", cfg.ReasoningEffort)
	}

	switch cfg.Sandbox {
	case "read-only", "workspace-write", "danger-full-access":
	default:
		return fmt.Errorf("unsupported codex sandbox %q", cfg.Sandbox)
	}

	return nil
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

// DeriveRemoteListenAddr keeps only the listen port for remote forwarding.
func DeriveRemoteListenAddr(listenAddr string) (string, error) {
	_, port, err := net.SplitHostPort(listenAddr)
	if err != nil {
		return "", fmt.Errorf("split listen addr %q failed: %w", listenAddr, err)
	}

	return ":" + port, nil
}
