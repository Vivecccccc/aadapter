package adapter

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestMessagesProxyAndTokenRefresh(t *testing.T) {
	var authCalls int32
	authSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&authCalls, 1)
		var payload map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode auth payload: %v", err)
		}
		if _, ok := payload["userid"]; !ok {
			t.Fatalf("missing userid field: %#v", payload)
		}
		if _, bad := payload["user_id"]; bad {
			t.Fatalf("unexpected user_id field: %#v", payload)
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"id_token":   "tok1",
			"expires_in": 3600,
		})
	}))
	defer authSrv.Close()

	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer tok1" {
			t.Fatalf("unexpected auth: %q", got)
		}
		if r.URL.Path != "/v1/projects/p/locations/us-central1/publishers/anthropic/models/default-model:rawPredict" {
			t.Fatalf("expected env model to override request model, got path: %s", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		var got map[string]interface{}
		_ = json.Unmarshal(body, &got)
		if _, hasModel := got["model"]; hasModel {
			t.Fatalf("rewritten request should not include model: %s", string(body))
		}
		if got["anthropic_version"] != "vertex-2023-10-16" {
			t.Fatalf("expected anthropic_version in rewritten body: %s", string(body))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	defer gateway.Close()

	cfg := Config{
		ListenAddr:       ":0",
		LogLevel:         "info",
		GatewayBaseURL:   gateway.URL,
		Project:          "p",
		Location:         "us-central1",
		Publisher:        "anthropic",
		Model:            "default-model",
		ModelOverride:    true,
		AnthropicVersion: "vertex-2023-10-16",
		AuthURL:          authSrv.URL,
		AuthUserID:       "u",
		AuthPassword:     "p",
		AuthOTPType:      "TOTP",
		RefreshSkew:      60,
		GatewayTimeout:   5e9,
		AuthTimeout:      5e9,
		ForceRefreshOn4x: true,
	}

	s, _ := NewServer(cfg)

	reqBody := []byte(`{"model":"claude-sonnet-4-5","stream":false,"messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(reqBody))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"anthropic_version":"vertex-2023-10-16"`)) {
		t.Fatalf("response mismatch: %s", rec.Body.String())
	}
	if atomic.LoadInt32(&authCalls) != 1 {
		t.Fatalf("expected one auth call")
	}
}

func TestAnthropicVersionHeaderMappedToBodyAndModelFallback(t *testing.T) {
	authSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"id_token": "tok1", "expires_in": 3600})
	}))
	defer authSrv.Close()

	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/projects/p/locations/us-central1/publishers/anthropic/models/default-model:rawPredict" {
			t.Fatalf("expected fallback model path, got: %s", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		var got map[string]interface{}
		_ = json.Unmarshal(body, &got)
		if got["anthropic_version"] != "vertex-2023-06-01" {
			t.Fatalf("expected mapped anthropic_version, got body=%s", string(body))
		}
		_, _ = w.Write(body)
	}))
	defer gateway.Close()

	cfg := Config{
		ListenAddr:       ":0",
		LogLevel:         "debug",
		GatewayBaseURL:   gateway.URL,
		Project:          "p",
		Location:         "us-central1",
		Publisher:        "anthropic",
		Model:            "default-model",
		ModelOverride:    true,
		AnthropicVersion: "vertex-2023-10-16",
		AuthURL:          authSrv.URL,
		AuthUserID:       "u",
		AuthPassword:     "p",
		AuthOTPType:      "TOTP",
		RefreshSkew:      60,
		GatewayTimeout:   5e9,
		AuthTimeout:      5e9,
	}
	s, _ := NewServer(cfg)

	reqBody := []byte(`{"stream":false,"messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(reqBody))
	req.Header.Set("anthropic-version", "2023-06-01")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestModelOverrideDisabledUsesRequestModel(t *testing.T) {
	authSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"id_token": "tok1", "expires_in": 3600})
	}))
	defer authSrv.Close()

	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/projects/p/locations/us-central1/publishers/anthropic/models/claude-sonnet-4-5:rawPredict" {
			t.Fatalf("expected request model path when override disabled, got: %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer gateway.Close()

	cfg := Config{
		ListenAddr:       ":0",
		LogLevel:         "info",
		GatewayBaseURL:   gateway.URL,
		Project:          "p",
		Location:         "us-central1",
		Publisher:        "anthropic",
		Model:            "default-model",
		ModelOverride:    false,
		AnthropicVersion: "vertex-2023-10-16",
		AuthURL:          authSrv.URL,
		AuthUserID:       "u",
		AuthPassword:     "p",
		AuthOTPType:      "TOTP",
		RefreshSkew:      60,
		GatewayTimeout:   5e9,
		AuthTimeout:      5e9,
	}
	s, _ := NewServer(cfg)

	reqBody := []byte(`{"model":"claude-sonnet-4-5","stream":false,"messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(reqBody))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}
