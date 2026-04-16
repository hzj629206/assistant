package tunnel

import (
	"reflect"
	"testing"
)

func TestDeriveRemoteListenAddr(t *testing.T) {
	t.Parallel()

	got, err := deriveRemoteListenAddr("127.0.0.1:8421")
	if err != nil {
		t.Fatalf("deriveRemoteListenAddr() failed: %v", err)
	}
	if got != ":8421" {
		t.Fatalf("deriveRemoteListenAddr() = %q, want %q", got, ":8421")
	}
}

func TestDeriveLocalTargetURL(t *testing.T) {
	t.Parallel()

	got, err := deriveLocalTargetURL("127.0.0.1:8421")
	if err != nil {
		t.Fatalf("deriveLocalTargetURL() failed: %v", err)
	}
	if got != "http://127.0.0.1:8421" {
		t.Fatalf("deriveLocalTargetURL() = %q, want %q", got, "http://127.0.0.1:8421")
	}
}

func TestValidateLocalTargetAddr(t *testing.T) {
	t.Parallel()

	got, err := validateLocalTargetAddr("127.0.0.1:8421")
	if err != nil {
		t.Fatalf("validateLocalTargetAddr() failed: %v", err)
	}
	if got != "127.0.0.1:8421" {
		t.Fatalf("validateLocalTargetAddr() = %q, want %q", got, "127.0.0.1:8421")
	}
}

func TestValidateLocalTargetAddrRejectsInvalidFormat(t *testing.T) {
	t.Parallel()

	_, err := validateLocalTargetAddr("127.0.0.1")
	if err == nil {
		t.Fatal("expected invalid local target addr error")
	}
}

func TestNewSSHRejectsInvalidLocalTargetAddr(t *testing.T) {
	t.Parallel()

	_, err := NewSSH(SSHConfig{
		SSHAddr:         "example.com:22",
		SSHKey:          "/tmp/unused",
		LocalTargetAddr: "127.0.0.1",
	})
	if err == nil {
		t.Fatal("expected invalid local target addr error")
	}
}

func TestNewCloudflareRejectsInvalidLocalTargetAddr(t *testing.T) {
	t.Parallel()

	_, err := NewCloudflared(CloudflaredConfig{
		Token:           "token-123",
		LocalTargetAddr: "127.0.0.1",
	})
	if err == nil {
		t.Fatal("expected invalid local target addr error")
	}
}

func TestCloudflaredArgs(t *testing.T) {
	t.Parallel()

	got := cloudflaredArgs("token-123")
	want := []string{"tunnel", "run", "--token", "token-123", "--protocol", "http2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("cloudflaredArgs() = %#v, want %#v", got, want)
	}
}
