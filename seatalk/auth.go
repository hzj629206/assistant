package seatalk

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/hzj629206/assistant/cache"
)

const appAccessTokenRefreshSkew = 30 * time.Second

// AppAccessToken contains the token returned by the SeaTalk auth API.
type AppAccessToken struct {
	Token  string
	Expire int64
}

type appAccessTokenRequest struct {
	AppID     string `json:"app_id"`
	AppSecret string `json:"app_secret"`
}

type appAccessTokenResponse struct {
	Code           int    `json:"code"`
	Message        string `json:"message"`
	AppAccessToken string `json:"app_access_token"`
	Expire         int64  `json:"expire"`
}

var (
	//nolint:gosec // This is a public API endpoint constant, not a credential.
	appAccessTokenEndpoint = "https://openapi.seatalk.io/auth/app_access_token"
	nowUnix                = func() int64 { return time.Now().Unix() }
)

// ObtainAccessToken requests a fresh app access token with the provided app credentials.
func ObtainAccessToken(ctx context.Context, client *http.Client, appID, appSecret string) (AppAccessToken, error) {
	return obtainAccessToken(ctx, client, appAccessTokenEndpoint, appID, appSecret)
}

// GetAccessToken returns a cached app access token or fetches a fresh one when the cached token is missing or expired.
func GetAccessToken(ctx context.Context, client *http.Client, appID, appSecret string) (string, error) {
	cacheKey := accessTokenCacheKey(appID, appSecret)

	token, err := getCachedAccessToken(ctx, cacheKey)
	if err == nil && token.Expire > nowUnix()+int64(appAccessTokenRefreshSkew/time.Second) {
		return token.Token, nil
	}
	if err != nil && !errors.Is(err, cache.ErrNotFound) {
		return "", fmt.Errorf("get access token failed: read cache: %w", err)
	}

	token, err = ObtainAccessToken(ctx, client, appID, appSecret)
	if err != nil {
		return "", err
	}

	if err = setCachedAccessToken(ctx, cacheKey, token); err != nil {
		return "", fmt.Errorf("get access token failed: write cache: %w", err)
	}
	return token.Token, nil
}

func obtainAccessToken(ctx context.Context, client *http.Client, endpoint string, appID string, appSecret string) (AppAccessToken, error) {
	if appID == "" {
		return AppAccessToken{}, errors.New("obtain access token failed: app id is empty")
	}
	if appSecret == "" {
		return AppAccessToken{}, errors.New("obtain access token failed: app secret is empty")
	}
	if client == nil {
		client = http.DefaultClient
	}

	reqBody, err := json.Marshal(appAccessTokenRequest{
		AppID:     appID,
		AppSecret: appSecret,
	})
	if err != nil {
		return AppAccessToken{}, fmt.Errorf("obtain access token failed: encode request body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(reqBody))
	if err != nil {
		return AppAccessToken{}, fmt.Errorf("obtain access token failed: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req) //nolint:gosec // Endpoint is controlled by trusted configuration/private flow.
	if err != nil {
		return AppAccessToken{}, fmt.Errorf("obtain access token failed: send request: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return AppAccessToken{}, fmt.Errorf("obtain access token failed: read response body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return AppAccessToken{}, fmt.Errorf("obtain access token failed: unexpected status %d: %s", resp.StatusCode, string(respBody))
	}

	var apiResp appAccessTokenResponse
	if err := json.Unmarshal(respBody, &apiResp); err != nil {
		return AppAccessToken{}, fmt.Errorf("obtain access token failed: decode response body: %w", err)
	}
	if apiResp.Code != 0 {
		if apiResp.Message != "" {
			return AppAccessToken{}, fmt.Errorf("obtain access token failed: api returned code %d: %s", apiResp.Code, apiResp.Message)
		}
		return AppAccessToken{}, fmt.Errorf("obtain access token failed: api returned code %d", apiResp.Code)
	}
	if apiResp.AppAccessToken == "" {
		return AppAccessToken{}, errors.New("obtain access token failed: empty app_access_token in response")
	}

	return AppAccessToken{
		Token:  apiResp.AppAccessToken,
		Expire: apiResp.Expire,
	}, nil
}

func accessTokenCacheKey(appID, appSecret string) string {
	return appID + "\x00" + appSecret
}

func getCachedAccessToken(ctx context.Context, key string) (AppAccessToken, error) {
	data, err := cache.Global().Get(ctx, key)
	if err != nil {
		return AppAccessToken{}, err
	}

	var token AppAccessToken
	if err := json.Unmarshal(data, &token); err != nil {
		return AppAccessToken{}, fmt.Errorf("decode cached access token: %w", err)
	}
	return token, nil
}

func setCachedAccessToken(ctx context.Context, key string, token AppAccessToken) error {
	data, err := json.Marshal(token)
	if err != nil {
		return fmt.Errorf("encode cached access token: %w", err)
	}

	ttl := time.Until(time.Unix(token.Expire, 0))
	return cache.Global().Set(ctx, key, data, ttl)
}
