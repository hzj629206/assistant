package config

import "testing"

func TestNormalizeCodexConfig(t *testing.T) {
	t.Parallel()

	cfg := CodexConfig{
		Backend:         " AppServer ",
		Model:           " gpt-5.4-mini ",
		ReasoningEffort: " LOW ",
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
}

func TestNormalizeCodexConfigRejectsUnsupportedValues(t *testing.T) {
	t.Parallel()

	cfg := CodexConfig{
		Backend:         "invalid",
		Model:           "gpt-5.4-mini",
		ReasoningEffort: "low",
	}
	if err := normalizeCodexConfig(&cfg); err == nil {
		t.Fatal("expected unsupported backend error")
	}

	cfg = CodexConfig{
		Backend:         "appserver",
		Model:           "gpt-5.4-mini",
		ReasoningEffort: "extreme",
	}
	if err := normalizeCodexConfig(&cfg); err == nil {
		t.Fatal("expected unsupported reasoning effort error")
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
