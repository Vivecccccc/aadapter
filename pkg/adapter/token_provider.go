package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

type tokenProvider struct {
	cfg    Config
	client *http.Client

	mu        sync.RWMutex
	bearer    string
	expiresAt time.Time

	sf singleflight.Group
}

type authRequest struct {
	UserID   string `json:"userid"`
	Password string `json:"password"`
	OTP      string `json:"otp"`
	OTPType  string `json:"otp_type"`
}

type authResponse struct {
	AccessToken string `json:"access_token"`
	Scope       string `json:"scope"`
	IDToken     string `json:"id_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int    `json:"expires_in"`
}

func newTokenProvider(cfg Config) *tokenProvider {
	return &tokenProvider{
		cfg: cfg,
		client: &http.Client{
			Timeout: cfg.AuthTimeout,
		},
	}
}

func (p *tokenProvider) GetBearerToken(ctx context.Context) (string, error) {
	p.mu.RLock()
	tok := p.bearer
	exp := p.expiresAt
	p.mu.RUnlock()

	if tok != "" && time.Now().Add(p.cfg.RefreshSkew).Before(exp) {
		return tok, nil
	}

	v, err, _ := p.sf.Do("refresh", func() (interface{}, error) {
		return p.refresh(ctx)
	})
	if err != nil {
		return "", err
	}
	return v.(string), nil
}

func (p *tokenProvider) ForceRefresh(ctx context.Context) (string, error) {
	p.sf.Forget("refresh")
	v, err, _ := p.sf.Do("refresh", func() (interface{}, error) {
		return p.refresh(ctx)
	})
	if err != nil {
		return "", err
	}
	return v.(string), nil
}

func (p *tokenProvider) refresh(ctx context.Context) (string, error) {
	body, err := json.Marshal(authRequest{
		UserID:   p.cfg.AuthUserID,
		Password: p.cfg.AuthPassword,
		OTP:      p.cfg.AuthOTP,
		OTPType:  p.cfg.AuthOTPType,
	})
	if err != nil {
		return "", fmt.Errorf("marshal auth request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.cfg.AuthURL, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("build auth request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("do auth request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("auth request failed: status=%d", resp.StatusCode)
	}

	var parsed authResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return "", fmt.Errorf("decode auth response: %w", err)
	}
	if parsed.IDToken == "" {
		return "", fmt.Errorf("auth response missing id_token")
	}
	if parsed.ExpiresIn <= 0 {
		parsed.ExpiresIn = 300
	}

	token := "Bearer " + parsed.IDToken
	expiresAt := time.Now().Add(time.Duration(parsed.ExpiresIn) * time.Second)

	p.mu.Lock()
	p.bearer = token
	p.expiresAt = expiresAt
	p.mu.Unlock()

	return token, nil
}
