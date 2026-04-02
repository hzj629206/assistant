package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultConfigUsesBuiltInDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := defaultConfig()
	if err != nil {
		t.Fatalf("defaultConfig failed: %v", err)
	}

	if cfg.ListenAddr != ":8421" {
		t.Fatalf("unexpected listen addr: %s", cfg.ListenAddr)
	}
	if cfg.RemoteSSHAddr != "" {
		t.Fatalf("unexpected remote ssh addr: %s", cfg.RemoteSSHAddr)
	}
	if cfg.RemoteSSHUser != "" {
		t.Fatalf("unexpected remote ssh user: %s", cfg.RemoteSSHUser)
	}
	if cfg.SSHKeyPath == "" {
		t.Fatal("expected non-empty ssh key path")
	}
	if cfg.Codex.Backend != "appserver" || cfg.Codex.Model != "gpt-5.4-mini" || cfg.Codex.ReasoningEffort != "low" {
		t.Fatalf("unexpected codex defaults: %+v", cfg.Codex)
	}
	if cfg.SeaTalk.AppID != "" || cfg.SeaTalk.AppSecret != "" || cfg.SeaTalk.SigningSecret != "" {
		t.Fatalf("unexpected seatalk defaults: %+v", cfg.SeaTalk)
	}
}

func TestDefaultConfigPathUsesHomeDirectory(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)

	path, err := defaultConfigPath()
	if err != nil {
		t.Fatalf("defaultConfigPath failed: %v", err)
	}

	want := filepath.Join(homeDir, ".assistant", "config.yml")
	if path != want {
		t.Fatalf("unexpected config path: got %q want %q", path, want)
	}
}

func TestParseConfigLoadsDefaultConfigFile(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	configDir := filepath.Join(homeDir, ".assistant")
	if err := os.MkdirAll(configDir, 0o750); err != nil {
		t.Fatalf("create config dir failed: %v", err)
	}

	path := filepath.Join(configDir, "config.yml")
	content := []byte(`listen_addr: "127.0.0.1:9090"
remote_ssh_user: admin
codex:
  backend: cli
  model: gpt-5.4
  reasoning_effort: medium
seatalk:
  app_id: app-id
`)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("write config file failed: %v", err)
	}

	cfg, err := ParseConfig(nil)
	if err != nil {
		t.Fatalf("ParseConfig failed: %v", err)
	}

	if cfg.ListenAddr != "127.0.0.1:9090" {
		t.Fatalf("unexpected listen addr: %s", cfg.ListenAddr)
	}
	if cfg.RemoteSSHUser != "admin" {
		t.Fatalf("unexpected remote ssh user: %s", cfg.RemoteSSHUser)
	}
	if cfg.Codex.Backend != "cli" || cfg.Codex.Model != "gpt-5.4" || cfg.Codex.ReasoningEffort != "medium" {
		t.Fatalf("unexpected codex config: %+v", cfg.Codex)
	}
	if cfg.SeaTalk.AppID != "app-id" {
		t.Fatalf("unexpected seatalk app id: %s", cfg.SeaTalk.AppID)
	}
}

func TestParseConfigExplicitFileOverridesDefaultPath(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	configDir := filepath.Join(homeDir, ".assistant")
	if err := os.MkdirAll(configDir, 0o750); err != nil {
		t.Fatalf("create config dir failed: %v", err)
	}

	defaultPath := filepath.Join(configDir, "config.yml")
	if err := os.WriteFile(defaultPath, []byte("listen_addr: \"127.0.0.1:9090\"\n"), 0o600); err != nil {
		t.Fatalf("write default config failed: %v", err)
	}

	explicitPath := filepath.Join(t.TempDir(), "custom.yml")
	if err := os.WriteFile(explicitPath, []byte("listen_addr: \"127.0.0.1:9191\"\n"), 0o600); err != nil {
		t.Fatalf("write explicit config failed: %v", err)
	}

	cfg, err := ParseConfig([]string{"-f", explicitPath})
	if err != nil {
		t.Fatalf("ParseConfig failed: %v", err)
	}

	if cfg.ListenAddr != "127.0.0.1:9191" {
		t.Fatalf("unexpected listen addr: %s", cfg.ListenAddr)
	}
}

func TestParseConfigCopiesVisitedFlags(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)

	cfg, err := ParseConfig([]string{
		"--listen-addr", "127.0.0.1:9090",
		"--remote-ssh-user", "admin",
		"--codex-backend", "cli",
		"--codex-model", "gpt-5.4",
		"--codex-reasoning-effort", "medium",
		"--seatalk-app-id", "app-id",
		"--seatalk-employee-info-enabled=false",
	})
	if err != nil {
		t.Fatalf("ParseConfig failed: %v", err)
	}

	if cfg.ListenAddr != "127.0.0.1:9090" {
		t.Fatalf("unexpected listen addr: %s", cfg.ListenAddr)
	}
	if cfg.RemoteSSHUser != "admin" {
		t.Fatalf("unexpected remote ssh user: %s", cfg.RemoteSSHUser)
	}
	if cfg.Codex.Backend != "cli" {
		t.Fatalf("unexpected codex backend: %s", cfg.Codex.Backend)
	}
	if cfg.Codex.Model != "gpt-5.4" {
		t.Fatalf("unexpected codex model: %s", cfg.Codex.Model)
	}
	if cfg.Codex.ReasoningEffort != "medium" {
		t.Fatalf("unexpected codex reasoning effort: %s", cfg.Codex.ReasoningEffort)
	}
	if cfg.SeaTalk.AppID != "app-id" {
		t.Fatalf("unexpected app id: %s", cfg.SeaTalk.AppID)
	}
	if cfg.SeaTalk.EmployeeInfoEnabled {
		t.Fatal("expected employee info to be disabled by flag")
	}
}
