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
		body, _ := io.ReadAll(r.Body)
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
		Model:            "claude-sonnet-4-5",
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
	if rec.Body.String() != string(reqBody) {
		t.Fatalf("response mismatch: %s", rec.Body.String())
	}
	if atomic.LoadInt32(&authCalls) != 1 {
		t.Fatalf("expected one auth call")
	}
}
