package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestNormalizeCodexConfig(t *testing.T) {
	t.Parallel()

	rootOne := t.TempDir()
	rootTwo := filepath.Join(rootOne, "..", filepath.Base(rootOne))
	cfg := CodexConfig{
		Backend:                 " AppServer ",
		Model:                   " gpt-5.4-mini ",
		ReasoningEffort:         " LOW ",
		Sandbox:                 " READ-ONLY ",
		AdditionalWritableRoots: []string{" ", rootOne, rootTwo},
	}
	if err := normalizeCodexConfig(&cfg); err != nil {
		t.Fatalf("normalizeCodexConfig failed: %v", err)
	}
	if cfg.Backend != "appserver" {
		t.Fatalf("unexpected backend: %s", cfg.Backend)
	}
	if cfg.Model != "gpt-5.4-mini" {
		t.Fatalf("unexpected model: %s", cfg.Model)
	}
	if cfg.ReasoningEffort != "low" {
		t.Fatalf("unexpected reasoning effort: %s", cfg.ReasoningEffort)
	}
	if cfg.Sandbox != "read-only" {
		t.Fatalf("unexpected sandbox: %s", cfg.Sandbox)
	}
	if len(cfg.AdditionalWritableRoots) != 1 || cfg.AdditionalWritableRoots[0] != filepath.Clean(rootOne) {
		t.Fatalf("unexpected additional writable roots: %#v", cfg.AdditionalWritableRoots)
	}
}

func TestNormalizeCodexConfigRejectsUnsupportedValues(t *testing.T) {
	t.Parallel()

	cfg := CodexConfig{
		Backend:         "invalid",
		Model:           "gpt-5.4-mini",
		ReasoningEffort: "low",
		Sandbox:         "read-only",
	}
	if err := normalizeCodexConfig(&cfg); err == nil {
		t.Fatal("expected unsupported backend error")
	}

	cfg = CodexConfig{
		Backend:         "appserver",
		Model:           "gpt-5.4-mini",
		ReasoningEffort: "extreme",
		Sandbox:         "read-only",
	}
	if err := normalizeCodexConfig(&cfg); err == nil {
		t.Fatal("expected unsupported reasoning effort error")
	}

	cfg = CodexConfig{
		Backend:         "appserver",
		Model:           "gpt-5.4-mini",
		ReasoningEffort: "low",
		Sandbox:         "invalid",
	}
	if err := normalizeCodexConfig(&cfg); err == nil {
		t.Fatal("expected unsupported sandbox error")
	}
}

func TestNormalizeRemoteSSHAddr(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "empty",
			in:   "",
			want: "",
		},
		{
			name: "hostname without port",
			in:   "example.com",
			want: "example.com:22",
		},
		{
			name: "hostname with port",
			in:   "example.com:2200",
			want: "example.com:2200",
		},
		{
			name: "ipv4 without port",
			in:   "192.0.2.10",
			want: "192.0.2.10:22",
		},
		{
			name: "ipv4 with port",
			in:   "192.0.2.10:2200",
			want: "192.0.2.10:2200",
		},
		{
			name: "ipv6 without port",
			in:   "2001:db8::10",
			want: "[2001:db8::10]:22",
		},
		{
			name: "bracketed ipv6 without port",
			in:   "[2001:db8::10]",
			want: "[2001:db8::10]:22",
		},
		{
			name: "ipv6 with port",
			in:   "[2001:db8::10]:2200",
			want: "[2001:db8::10]:2200",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := NormalizeRemoteSSHAddr(tc.in)
			if got != tc.want {
				t.Fatalf("NormalizeRemoteSSHAddr(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestDeriveRemoteListenAddr(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{
			name: "empty host",
			in:   ":8080",
			want: ":8080",
		},
		{
			name: "ipv4 host",
			in:   "127.0.0.1:9090",
			want: ":9090",
		},
		{
			name: "hostname",
			in:   "localhost:7000",
			want: ":7000",
		},
		{
			name: "ipv6",
			in:   "[::1]:6000",
			want: ":6000",
		},
		{
			name:    "missing port",
			in:      "127.0.0.1",
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := DeriveRemoteListenAddr(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("DeriveRemoteListenAddr(%q) returned nil error", tc.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("DeriveRemoteListenAddr(%q) failed: %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("DeriveRemoteListenAddr(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

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
	if cfg.Codex.Backend != "appserver" || cfg.Codex.Model != "gpt-5.4" || cfg.Codex.ReasoningEffort != "low" || cfg.Codex.Sandbox != "read-only" {
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
  backend: exec
  model: gpt-5.4
  reasoning_effort: medium
  sandbox: workspace-write
  additional_writable_roots:
    - /tmp/status.json
    - /var/tmp/assistant-state
seatalk:
  app_id: app-id
  app_secret: app-secret
  signing_secret: signing-secret
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
	if cfg.Codex.Backend != "exec" || cfg.Codex.Model != "gpt-5.4" || cfg.Codex.ReasoningEffort != "medium" || cfg.Codex.Sandbox != "workspace-write" {
		t.Fatalf("unexpected codex config: %+v", cfg.Codex)
	}
	if !reflect.DeepEqual(cfg.Codex.AdditionalWritableRoots, []string{"/tmp/status.json", "/var/tmp/assistant-state"}) {
		t.Fatalf("unexpected additional writable roots: %#v", cfg.Codex.AdditionalWritableRoots)
	}
	if cfg.SeaTalk.AppID != "app-id" {
		t.Fatalf("unexpected seatalk app id: %s", cfg.SeaTalk.AppID)
	}
}

func TestParseConfigRejectsMissingSeaTalkAppID(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)

	path := filepath.Join(homeDir, ".assistant", "config.yml")
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("create config dir failed: %v", err)
	}
	if err := os.WriteFile(path, []byte(`listen_addr: "127.0.0.1:8421"
seatalk:
  app_secret: app-secret
  signing_secret: signing-secret
`), 0o600); err != nil {
		t.Fatalf("write config file failed: %v", err)
	}

	_, err := ParseConfig(nil)
	if err == nil {
		t.Fatal("expected missing seatalk app_id error")
	}
	if err.Error() != "seatalk app_id is required" {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestParseConfigRejectsMissingSeaTalkAppSecret(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)

	path := filepath.Join(homeDir, ".assistant", "config.yml")
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("create config dir failed: %v", err)
	}
	if err := os.WriteFile(path, []byte(`listen_addr: "127.0.0.1:8421"
seatalk:
  app_id: app-id
  signing_secret: signing-secret
`), 0o600); err != nil {
		t.Fatalf("write config file failed: %v", err)
	}

	_, err := ParseConfig(nil)
	if err == nil {
		t.Fatal("expected missing seatalk app_secret error")
	}
	if err.Error() != "seatalk app_secret is required" {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestParseConfigRejectsMissingSeaTalkSigningSecret(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)

	path := filepath.Join(homeDir, ".assistant", "config.yml")
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("create config dir failed: %v", err)
	}
	if err := os.WriteFile(path, []byte(`listen_addr: "127.0.0.1:8421"
seatalk:
  app_id: app-id
  app_secret: app-secret
`), 0o600); err != nil {
		t.Fatalf("write config file failed: %v", err)
	}

	_, err := ParseConfig(nil)
	if err == nil {
		t.Fatal("expected missing seatalk signing_secret error")
	}
	if err.Error() != "seatalk signing_secret is required" {
		t.Fatalf("unexpected error: %v", err)
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
	if err := os.WriteFile(defaultPath, []byte("listen_addr: \"127.0.0.1:9090\"\nseatalk:\n  app_id: default-app-id\n  app_secret: default-app-secret\n  signing_secret: default-signing-secret\n"), 0o600); err != nil {
		t.Fatalf("write default config failed: %v", err)
	}

	explicitPath := filepath.Join(t.TempDir(), "custom.yml")
	if err := os.WriteFile(explicitPath, []byte("listen_addr: \"127.0.0.1:9191\"\nseatalk:\n  app_id: custom-app-id\n  app_secret: custom-app-secret\n  signing_secret: custom-signing-secret\n"), 0o600); err != nil {
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

func TestParseConfigAppliesSupportedFlagOverrides(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)

	path := filepath.Join(homeDir, ".assistant", "config.yml")
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("create config dir failed: %v", err)
	}
	if err := os.WriteFile(path, []byte(`listen_addr: "127.0.0.1:8421"
codex:
  backend: exec
  model: gpt-5.4-mini
  reasoning_effort: low
  sandbox: read-only
seatalk:
  app_id: app-id
  app_secret: app-secret
  signing_secret: signing-secret
`), 0o600); err != nil {
		t.Fatalf("write config file failed: %v", err)
	}

	cfg, err := ParseConfig([]string{
		"--listen-addr", "127.0.0.1:9090",
		"--codex-backend", "appserver",
		"--codex-model", "gpt-5.4",
		"--codex-reasoning-effort", "medium",
		"--codex-sandbox", "workspace-write",
	})
	if err != nil {
		t.Fatalf("ParseConfig failed: %v", err)
	}

	if cfg.ListenAddr != "127.0.0.1:9090" {
		t.Fatalf("unexpected listen addr: %s", cfg.ListenAddr)
	}
	if cfg.Codex.Model != "gpt-5.4" {
		t.Fatalf("unexpected codex model: %s", cfg.Codex.Model)
	}
	if cfg.Codex.ReasoningEffort != "medium" {
		t.Fatalf("unexpected codex reasoning effort: %s", cfg.Codex.ReasoningEffort)
	}
	if cfg.Codex.Backend != "appserver" {
		t.Fatalf("unexpected codex backend: %s", cfg.Codex.Backend)
	}
	if cfg.Codex.Sandbox != "workspace-write" {
		t.Fatalf("unexpected codex sandbox: %s", cfg.Codex.Sandbox)
	}
}
