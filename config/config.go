package config

import (
	"errors"
	"flag"
	"fmt"
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
}

type flagOverlay struct {
	listenAddr           string
	remoteSSHAddr        string
	remoteSSHUser        string
	sshKeyPath           string
	codexBackend         string
	codexModel           string
	codexReasoningEffort string
	seatalkAppID         string
	seatalkAppSecret     string
	seatalkSigningSecret string
	seatalkEmployeeInfo  bool
}

// ParseConfig loads defaults, then an optional config file, then command-line overrides.
func ParseConfig(args []string) (Config, error) {
	cfg, err := defaultConfig()
	if err != nil {
		return Config{}, err
	}

	overlay := flagOverlay{
		listenAddr:           cfg.ListenAddr,
		remoteSSHAddr:        cfg.RemoteSSHAddr,
		remoteSSHUser:        cfg.RemoteSSHUser,
		sshKeyPath:           cfg.SSHKeyPath,
		codexBackend:         cfg.Codex.Backend,
		codexModel:           cfg.Codex.Model,
		codexReasoningEffort: cfg.Codex.ReasoningEffort,
		seatalkAppID:         cfg.SeaTalk.AppID,
		seatalkAppSecret:     cfg.SeaTalk.AppSecret,
		seatalkSigningSecret: cfg.SeaTalk.SigningSecret,
		seatalkEmployeeInfo:  cfg.SeaTalk.EmployeeInfoEnabled,
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
			Model:           "gpt-5.4-mini",
			ReasoningEffort: "low",
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
	fs.StringVar(&overlay.listenAddr, "listen-addr", overlay.listenAddr, "HTTP server listen address")
	fs.StringVar(&overlay.remoteSSHAddr, "remote-ssh-addr", overlay.remoteSSHAddr, "SSH server address in <host>:<port> format, default port 22")
	fs.StringVar(&overlay.remoteSSHUser, "remote-ssh-user", overlay.remoteSSHUser, "remote SSH username")
	fs.StringVar(&overlay.sshKeyPath, "ssh-key", overlay.sshKeyPath, "path to the SSH private key")
	fs.StringVar(&overlay.codexBackend, "codex-backend", overlay.codexBackend, "Codex backend: appserver, cli, or noop")
	fs.StringVar(&overlay.codexModel, "codex-model", overlay.codexModel, "Codex model name")
	fs.StringVar(&overlay.codexReasoningEffort, "codex-reasoning-effort", overlay.codexReasoningEffort, "Codex reasoning effort")
	fs.StringVar(&overlay.seatalkAppID, "seatalk-app-id", overlay.seatalkAppID, "SeaTalk application ID")
	fs.StringVar(&overlay.seatalkAppSecret, "seatalk-app-secret", overlay.seatalkAppSecret, "SeaTalk application secret")
	fs.StringVar(&overlay.seatalkSigningSecret, "seatalk-signing-secret", overlay.seatalkSigningSecret, "SeaTalk request signing secret")
	fs.BoolVar(&overlay.seatalkEmployeeInfo, "seatalk-employee-info-enabled", overlay.seatalkEmployeeInfo, "enable SeaTalk employee info tools and lookups")
	fs.StringVar(configPath, "f", *configPath, "path to config file")
	fs.StringVar(configPath, "config", *configPath, "path to config file")
	return fs
}

func applyFlagOverride(cfg *Config, overlay flagOverlay, name string) {
	switch name {
	case "listen-addr":
		cfg.ListenAddr = overlay.listenAddr
	case "remote-ssh-addr":
		cfg.RemoteSSHAddr = overlay.remoteSSHAddr
	case "remote-ssh-user":
		cfg.RemoteSSHUser = overlay.remoteSSHUser
	case "ssh-key":
		cfg.SSHKeyPath = overlay.sshKeyPath
	case "codex-backend":
		cfg.Codex.Backend = overlay.codexBackend
	case "codex-model":
		cfg.Codex.Model = overlay.codexModel
	case "codex-reasoning-effort":
		cfg.Codex.ReasoningEffort = overlay.codexReasoningEffort
	case "seatalk-app-id":
		cfg.SeaTalk.AppID = overlay.seatalkAppID
	case "seatalk-app-secret":
		cfg.SeaTalk.AppSecret = overlay.seatalkAppSecret
	case "seatalk-signing-secret":
		cfg.SeaTalk.SigningSecret = overlay.seatalkSigningSecret
	case "seatalk-employee-info-enabled":
		cfg.SeaTalk.EmployeeInfoEnabled = overlay.seatalkEmployeeInfo
	}
}

func loadConfigFile(path string, cfg *Config) error {
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

	if cfg.Backend == "" {
		cfg.Backend = "appserver"
	}
	if cfg.Model == "" {
		cfg.Model = "gpt-5.4-mini"
	}
	if cfg.ReasoningEffort == "" {
		cfg.ReasoningEffort = "low"
	}

	switch cfg.Backend {
	case "appserver", "cli", "noop":
	default:
		return fmt.Errorf("unsupported codex backend %q", cfg.Backend)
	}

	switch cfg.ReasoningEffort {
	case "none", "minimal", "low", "medium", "high", "xhigh":
	default:
		return fmt.Errorf("unsupported codex reasoning effort %q", cfg.ReasoningEffort)
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
