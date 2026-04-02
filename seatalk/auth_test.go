package seatalk

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/hzj629206/assistant/cache"
)

func TestObtainAccessToken(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("unexpected method: %s", r.Method)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Fatalf("unexpected content type: %s", got)
		}

		var req appAccessTokenRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request failed: %v", err)
		}
		if req.AppID != "app-id" {
			t.Fatalf("unexpected app id: %s", req.AppID)
		}
		if req.AppSecret != "app-secret" {
			t.Fatalf("unexpected app secret: %s", req.AppSecret)
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(appAccessTokenResponse{
			Code:           0,
			AppAccessToken: "token-123",
			Expire:         1590581487,
		}); err != nil {
			t.Fatalf("encode response failed: %v", err)
		}
	}))
	defer server.Close()

	token, err := obtainAccessToken(context.Background(), server.Client(), server.URL, "app-id", "app-secret")
	if err != nil {
		t.Fatalf("obtain access token failed: %v", err)
	}
	if token.Token != "token-123" {
		t.Fatalf("unexpected token: %s", token.Token)
	}
	if token.Expire != 1590581487 {
		t.Fatalf("unexpected expire: %d", token.Expire)
	}
}

func TestObtainAccessTokenAPIError(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(appAccessTokenResponse{
			Code:    10001,
			Message: "invalid credential",
		}); err != nil {
			t.Fatalf("encode response failed: %v", err)
		}
	}))
	defer server.Close()

	_, err := obtainAccessToken(context.Background(), server.Client(), server.URL, "app-id", "app-secret")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestGetAccessTokenUsesCache(t *testing.T) {
	resetAccessTokenCacheForTest(t)

	var requestCount atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(appAccessTokenResponse{
			Code:           0,
			AppAccessToken: "cached-token",
			Expire:         nowUnix() + 3600,
		}); err != nil {
			t.Fatalf("encode response failed: %v", err)
		}
	}))
	defer server.Close()

	originalEndpoint := appAccessTokenEndpoint
	appAccessTokenEndpoint = server.URL
	t.Cleanup(func() {
		appAccessTokenEndpoint = originalEndpoint
	})

	token1, err := GetAccessToken(context.Background(), server.Client(), "app-id", "app-secret")
	if err != nil {
		t.Fatalf("first get access token failed: %v", err)
	}
	token2, err := GetAccessToken(context.Background(), server.Client(), "app-id", "app-secret")
	if err != nil {
		t.Fatalf("second get access token failed: %v", err)
	}
	if token1 != "cached-token" || token2 != "cached-token" {
		t.Fatalf("unexpected token values: %q %q", token1, token2)
	}
	if got := requestCount.Load(); got != 1 {
		t.Fatalf("unexpected request count: %d", got)
	}
}

func TestGetAccessTokenRefreshesExpiredToken(t *testing.T) {
	resetAccessTokenCacheForTest(t)

	originalNowUnix := nowUnix
	nowUnix = func() int64 { return 1_700_000_000 }
	t.Cleanup(func() {
		nowUnix = originalNowUnix
	})

	var requestCount atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := requestCount.Add(1)
		w.Header().Set("Content-Type", "application/json")
		resp := appAccessTokenResponse{
			Code:           0,
			AppAccessToken: "fresh-token-1",
			Expire:         nowUnix() + 10,
		}
		if count > 1 {
			resp.AppAccessToken = "fresh-token-2"
			resp.Expire = nowUnix() + 3600
		}
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Fatalf("encode response failed: %v", err)
		}
	}))
	defer server.Close()

	originalEndpoint := appAccessTokenEndpoint
	appAccessTokenEndpoint = server.URL
	t.Cleanup(func() {
		appAccessTokenEndpoint = originalEndpoint
	})

	token1, err := GetAccessToken(context.Background(), server.Client(), "app-id", "app-secret")
	if err != nil {
		t.Fatalf("first get access token failed: %v", err)
	}
	token2, err := GetAccessToken(context.Background(), server.Client(), "app-id", "app-secret")
	if err != nil {
		t.Fatalf("second get access token failed: %v", err)
	}
	if token1 != "fresh-token-1" {
		t.Fatalf("unexpected first token: %q", token1)
	}
	if token2 != "fresh-token-2" {
		t.Fatalf("unexpected second token: %q", token2)
	}
	if got := requestCount.Load(); got != 2 {
		t.Fatalf("unexpected request count: %d", got)
	}
}

func resetAccessTokenCacheForTest(t *testing.T) {
	t.Helper()

	originalStore := cache.Global()
	cache.SetGlobal(cache.NewMemoryStorage())
	t.Cleanup(func() {
		cache.SetGlobal(originalStore)
	})
}
