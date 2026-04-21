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
	normalizeCodexConfig(&cfg)
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

func TestNormalizeACPConfig(t *testing.T) {
	t.Parallel()

	cfg := ACPConfig{
		Command:    " agent ",
		Args:       []string{" acp ", " ", "extra"},
		Env:        []string{" A=B ", " ", "C=D"},
		AuthMethod: " token ",
	}
	normalizeACPConfig(&cfg)

	if cfg.Command != "agent" {
		t.Fatalf("unexpected command: %q", cfg.Command)
	}
	if !reflect.DeepEqual(cfg.Args, []string{"acp", "extra"}) {
		t.Fatalf("unexpected args: %#v", cfg.Args)
	}
	if !reflect.DeepEqual(cfg.Env, []string{"A=B", "C=D"}) {
		t.Fatalf("unexpected env: %#v", cfg.Env)
	}
	if cfg.AuthMethod != "token" {
		t.Fatalf("unexpected auth method: %q", cfg.AuthMethod)
	}

	cfg = ACPConfig{}
	normalizeACPConfig(&cfg)
	if cfg.Command != "" {
		t.Fatalf("unexpected default command: %q", cfg.Command)
	}
	if cfg.Args != nil {
		t.Fatalf("unexpected default args: %#v", cfg.Args)
	}
}

func TestNormalizeClaudeConfig(t *testing.T) {
	t.Parallel()

	rootOne := t.TempDir()
	rootTwo := filepath.Join(rootOne, "..", filepath.Base(rootOne))
	cfg := ClaudeConfig{
		Model:                 " claude-sonnet-4-5 ",
		Permission:            " ACCEPT-EDITS ",
		ReasoningEffort:       " XHIGH ",
		AdditionalDirectories: []string{" ", rootOne, rootTwo, rootOne},
	}
	normalizeClaudeConfig(&cfg)

	if cfg.Model != "claude-sonnet-4-5" {
		t.Fatalf("unexpected claude model: %s", cfg.Model)
	}
	if cfg.Permission != "accept-edits" {
		t.Fatalf("unexpected claude permission: %s", cfg.Permission)
	}
	if cfg.ReasoningEffort != "xhigh" {
		t.Fatalf("unexpected claude effort: %s", cfg.ReasoningEffort)
	}
	if len(cfg.AdditionalDirectories) != 1 || cfg.AdditionalDirectories[0] != filepath.Clean(rootOne) {
		t.Fatalf("unexpected claude additional directories: %#v", cfg.AdditionalDirectories)
	}

	cfg = ClaudeConfig{}
	normalizeClaudeConfig(&cfg)
	if cfg.Model != "" {
		t.Fatalf("unexpected default claude model: %s", cfg.Model)
	}
	if cfg.Permission != "" {
		t.Fatalf("unexpected default claude permission: %s", cfg.Permission)
	}
	if cfg.ReasoningEffort != "" {
		t.Fatalf("unexpected default claude effort: %s", cfg.ReasoningEffort)
	}

	cfg = ClaudeConfig{Permission: "unsupported", ReasoningEffort: "unsupported"}
	normalizeClaudeConfig(&cfg)
	if cfg.Permission != "" {
		t.Fatalf("unexpected unsupported claude permission: %s", cfg.Permission)
	}
	if cfg.ReasoningEffort != "" {
		t.Fatalf("unexpected unsupported claude effort: %s", cfg.ReasoningEffort)
	}
}

func TestNormalizeCodexConfigClearsUnsupportedValues(t *testing.T) {
	t.Parallel()

	cfg := CodexConfig{
		Backend:         "invalid",
		Model:           "gpt-5.4-mini",
		ReasoningEffort: "low",
		Sandbox:         "read-only",
	}
	normalizeCodexConfig(&cfg)
	if cfg.Backend != "appserver" {
		t.Fatalf("unexpected backend after normalization: %q", cfg.Backend)
	}

	cfg = CodexConfig{
		Backend:         "appserver",
		Model:           "gpt-5.4-mini",
		ReasoningEffort: "extreme",
		Sandbox:         "read-only",
	}
	normalizeCodexConfig(&cfg)
	if cfg.ReasoningEffort != "" {
		t.Fatalf("unexpected reasoning effort after normalization: %q", cfg.ReasoningEffort)
	}

	cfg = CodexConfig{
		Backend:         "appserver",
		Model:           "gpt-5.4-mini",
		ReasoningEffort: "low",
		Sandbox:         "invalid",
	}
	normalizeCodexConfig(&cfg)
	if cfg.Sandbox != "" {
		t.Fatalf("unexpected sandbox after normalization: %q", cfg.Sandbox)
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

func TestDeriveLocalTargetAddr(t *testing.T) {
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
			want: "127.0.0.1:8080",
		},
		{
			name: "ipv4 any host",
			in:   "0.0.0.0:9090",
			want: "127.0.0.1:9090",
		},
		{
			name: "ipv6 any host",
			in:   "[::]:6000",
			want: "127.0.0.1:6000",
		},
		{
			name: "ipv4 loopback",
			in:   "127.0.0.1:7000",
			want: "127.0.0.1:7000",
		},
		{
			name: "hostname",
			in:   "localhost:7100",
			want: "localhost:7100",
		},
		{
			name: "ipv6 loopback",
			in:   "[::1]:7200",
			want: "[::1]:7200",
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

			got, err := DeriveLocalTargetAddr(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("DeriveLocalTargetAddr(%q) returned nil error", tc.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("DeriveLocalTargetAddr(%q) failed: %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("DeriveLocalTargetAddr(%q) = %q, want %q", tc.in, got, tc.want)
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
	if cfg.Tunnel.SSHAddr != "" {
		t.Fatalf("unexpected ssh addr: %s", cfg.Tunnel.SSHAddr)
	}
	if cfg.Tunnel.SSHUser != "" {
		t.Fatalf("unexpected ssh user: %s", cfg.Tunnel.SSHUser)
	}
	if cfg.Tunnel.SSHKey == "" {
		t.Fatal("expected non-empty ssh key")
	}
	if cfg.Tunnel.CloudflaredToken != "" {
		t.Fatalf("unexpected cloudflared token: %s", cfg.Tunnel.CloudflaredToken)
	}
	if cfg.Codex.Backend != "appserver" || cfg.Codex.Model != "" || cfg.Codex.ReasoningEffort != "" || cfg.Codex.Sandbox != "" {
		t.Fatalf("unexpected codex defaults: %+v", cfg.Codex)
	}
	if cfg.ACP.Command != "" || cfg.ACP.Args != nil || cfg.ACP.Env != nil || cfg.ACP.AuthMethod != "" {
		t.Fatalf("unexpected acp defaults: %+v", cfg.ACP)
	}
	if cfg.Claude.Model != "" {
		t.Fatalf("unexpected claude defaults: %+v", cfg.Claude)
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

func TestNewFlagSetUsesProgramName(t *testing.T) {
	t.Parallel()

	overlay := flagOverlay{}
	var configPath string

	fs := newFlagSet("codexd", &overlay, &configPath)
	if fs.Name() != "codexd" {
		t.Fatalf("unexpected flag set name: %s", fs.Name())
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
tunnel:
  ssh_user: admin
  cloudflared_token: cloudflare-token
acp:
  command: cursor-agent
  args:
    - acp
    - --fast
  env:
    - ACP_ENV=1
  auth_method: cursor_login
codex:
  backend: exec
  model: gpt-5.4
  reasoning_effort: medium
  sandbox: workspace-write
  additional_writable_roots:
    - /tmp/status.json
    - /var/tmp/assistant-state
claude:
  model: claude-sonnet-4-5
  permission: dont-ask
  reasoning_effort: high
  additional_directories:
    - /tmp/claude-a
    - /tmp/claude-a
    - /tmp/../tmp/claude-a
seatalk:
  app_id: app-id
  app_secret: app-secret
  signing_secret: signing-secret
`)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("write config file failed: %v", err)
	}

	cfg, err := ParseConfig("assistant-test", nil)
	if err != nil {
		t.Fatalf("ParseConfig failed: %v", err)
	}

	if cfg.ListenAddr != "127.0.0.1:9090" {
		t.Fatalf("unexpected listen addr: %s", cfg.ListenAddr)
	}
	if cfg.Tunnel.SSHUser != "admin" {
		t.Fatalf("unexpected ssh user: %s", cfg.Tunnel.SSHUser)
	}
	if cfg.Tunnel.CloudflaredToken != "cloudflare-token" {
		t.Fatalf("unexpected cloudflared token: %s", cfg.Tunnel.CloudflaredToken)
	}
	if cfg.ACP.Command != "cursor-agent" || cfg.ACP.AuthMethod != "cursor_login" {
		t.Fatalf("unexpected acp config: %+v", cfg.ACP)
	}
	if !reflect.DeepEqual(cfg.ACP.Args, []string{"acp", "--fast"}) {
		t.Fatalf("unexpected acp args: %#v", cfg.ACP.Args)
	}
	if !reflect.DeepEqual(cfg.ACP.Env, []string{"ACP_ENV=1"}) {
		t.Fatalf("unexpected acp env: %#v", cfg.ACP.Env)
	}
	if cfg.Codex.Backend != "exec" || cfg.Codex.Model != "gpt-5.4" || cfg.Codex.ReasoningEffort != "medium" || cfg.Codex.Sandbox != "workspace-write" {
		t.Fatalf("unexpected codex config: %+v", cfg.Codex)
	}
	if !reflect.DeepEqual(cfg.Codex.AdditionalWritableRoots, []string{"/tmp/status.json", "/var/tmp/assistant-state"}) {
		t.Fatalf("unexpected additional writable roots: %#v", cfg.Codex.AdditionalWritableRoots)
	}
	if cfg.Claude.Model != "claude-sonnet-4-5" {
		t.Fatalf("unexpected claude config: %+v", cfg.Claude)
	}
	if cfg.Claude.Permission != "dont-ask" {
		t.Fatalf("unexpected claude permission: %s", cfg.Claude.Permission)
	}
	if cfg.Claude.ReasoningEffort != "high" {
		t.Fatalf("unexpected claude effort: %s", cfg.Claude.ReasoningEffort)
	}
	if !reflect.DeepEqual(cfg.Claude.AdditionalDirectories, []string{"/tmp/claude-a"}) {
		t.Fatalf("unexpected claude additional directories: %#v", cfg.Claude.AdditionalDirectories)
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

	_, err := ParseConfig("assistant-test", nil)
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

	_, err := ParseConfig("assistant-test", nil)
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

	_, err := ParseConfig("assistant-test", nil)
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

	cfg, err := ParseConfig("assistant-test", []string{"-f", explicitPath})
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

	cfg, err := ParseConfig("assistant-test", []string{
		"--listen-addr", "127.0.0.1:9090",
		"--codex-backend", "appserver",
		"--codex-model", "gpt-5.4",
		"--claude-model", "claude-sonnet-4-6",
		"--claude-permission", "plan",
		"--claude-reasoning-effort", "max",
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
	if cfg.Claude.Model != "claude-sonnet-4-6" {
		t.Fatalf("unexpected claude model: %s", cfg.Claude.Model)
	}
	if cfg.Claude.Permission != "plan" {
		t.Fatalf("unexpected claude permission: %s", cfg.Claude.Permission)
	}
	if cfg.Claude.ReasoningEffort != "max" {
		t.Fatalf("unexpected claude effort: %s", cfg.Claude.ReasoningEffort)
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

func TestParseConfigRequiresProgramName(t *testing.T) {
	t.Parallel()

	_, err := ParseConfig("", nil)
	if err == nil {
		t.Fatal("expected missing program name error")
	}
	if err.Error() != "program name is required" {
		t.Fatalf("unexpected error: %v", err)
	}
}
