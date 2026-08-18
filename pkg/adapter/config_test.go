package adapter

import (
	"strings"
	"testing"
)

func TestGeminiProductionConfigurationValidation(t *testing.T) {
	setRequired := func(t *testing.T) {
		t.Helper()
		t.Setenv("GATEWAY_BASE_URL", "https://gateway.example.com")
		t.Setenv("VERTEX_PROJECT", "project-a")
		t.Setenv("VERTEX_LOCATION", "global")
		t.Setenv("VERTEX_MODEL", "gemini-3.6-flash")
		t.Setenv("VERTEX_API_FORMAT", "gemini")
		t.Setenv("VERTEX_PUBLISHER", "google")
		t.Setenv("AUTH_URL", "https://auth.example.com/token")
		t.Setenv("AUTH_USER_ID", "user")
		t.Setenv("AUTH_PASSWORD", "password")
		t.Setenv("ADAPTER_API_KEY", "")
		t.Setenv("ALLOW_UNAUTHENTICATED", "false")
	}

	t.Run("Gemini 3.6 global is accepted", func(t *testing.T) {
		setRequired(t)
		cfg, err := LoadConfigFromEnv()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Location != "global" || cfg.ListenAddr != "127.0.0.1:8080" {
			t.Fatalf("unexpected config: %#v", cfg)
		}
	})

	t.Run("invalid location is explicit", func(t *testing.T) {
		setRequired(t)
		t.Setenv("VERTEX_LOCATION", "us-central1")
		_, err := LoadConfigFromEnv()
		if err == nil || !strings.Contains(err.Error(), "requires VERTEX_LOCATION=global") {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("non-loopback requires authentication opt-in", func(t *testing.T) {
		setRequired(t)
		t.Setenv("ADAPTER_LISTEN_ADDR", ":8080")
		_, err := LoadConfigFromEnv()
		if err == nil || !strings.Contains(err.Error(), "ADAPTER_API_KEY") {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}
