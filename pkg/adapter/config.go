package adapter

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	ListenAddr string
	Verbose    bool
	LogLevel   string

	GatewayBaseURL   string
	VertexAPIFormat  string
	Project          string
	Location         string
	Publisher        string
	Model            string
	ModelOverride    bool
	AnthropicVersion string

	AuthURL      string
	AuthUserID   string
	AuthPassword string
	AuthOTP      string
	AuthOTPType  string

	RefreshSkew      time.Duration
	GatewayTimeout   time.Duration
	AuthTimeout      time.Duration
	ForceRefreshOn4x bool
}

func LoadConfigFromEnv() (Config, error) {
	apiFormat := envOrDefault("VERTEX_API_FORMAT", "anthropic")
	publisherDefault := "anthropic"
	if apiFormat == "gemini" {
		publisherDefault = "google"
	}
	cfg := Config{
		ListenAddr:       envOrDefault("ADAPTER_LISTEN_ADDR", ":8080"),
		Verbose:          boolOrDefault("ADAPTER_VERBOSE", false),
		LogLevel:         envOrDefault("ADAPTER_LOG_LEVEL", "info"),
		GatewayBaseURL:   strings.TrimRight(os.Getenv("GATEWAY_BASE_URL"), "/"),
		VertexAPIFormat:  apiFormat,
		Project:          os.Getenv("VERTEX_PROJECT"),
		Location:         os.Getenv("VERTEX_LOCATION"),
		Publisher:        envOrDefault("VERTEX_PUBLISHER", publisherDefault),
		Model:            os.Getenv("VERTEX_MODEL"),
		ModelOverride:    boolOrDefault("MODEL_OVERRIDE", true),
		AnthropicVersion: envOrDefault("VERTEX_ANTHROPIC_VERSION", "vertex-2023-10-16"),
		AuthURL:          os.Getenv("AUTH_URL"),
		AuthUserID:       os.Getenv("AUTH_USER_ID"),
		AuthPassword:     os.Getenv("AUTH_PASSWORD"),
		AuthOTP:          os.Getenv("AUTH_OTP"),
		AuthOTPType:      envOrDefault("AUTH_OTP_TYPE", "TOTP"),
		RefreshSkew:      durationOrDefault("AUTH_REFRESH_SKEW", 90*time.Second),
		GatewayTimeout:   durationOrDefault("GATEWAY_TIMEOUT", 120*time.Second),
		AuthTimeout:      durationOrDefault("AUTH_TIMEOUT", 10*time.Second),
		ForceRefreshOn4x: boolOrDefault("FORCE_REFRESH_ON_401_403", true),
	}

	if cfg.GatewayBaseURL == "" || cfg.Project == "" || cfg.Location == "" || cfg.Model == "" {
		return Config{}, errors.New("GATEWAY_BASE_URL, VERTEX_PROJECT, VERTEX_LOCATION, VERTEX_MODEL are required")
	}
	if cfg.AuthURL == "" || cfg.AuthUserID == "" || cfg.AuthPassword == "" {
		return Config{}, errors.New("AUTH_URL, AUTH_USER_ID, AUTH_PASSWORD are required")
	}
	if cfg.AuthOTPType != "TOTP" && cfg.AuthOTPType != "PUSH" {
		return Config{}, fmt.Errorf("AUTH_OTP_TYPE must be TOTP or PUSH")
	}
	if !isValidLogLevel(cfg.LogLevel) {
		return Config{}, fmt.Errorf("ADAPTER_LOG_LEVEL must be one of: debug, info, warning, error")
	}
	if cfg.VertexAPIFormat != "anthropic" && cfg.VertexAPIFormat != "gemini" {
		return Config{}, fmt.Errorf("VERTEX_API_FORMAT must be anthropic or gemini")
	}
	if cfg.VertexAPIFormat == "gemini" && cfg.Publisher != "google" {
		return Config{}, fmt.Errorf("VERTEX_PUBLISHER must be google when VERTEX_API_FORMAT=gemini")
	}

	return cfg, nil
}

func (c Config) targetPath(op vertexOperation, model string) string {
	return fmt.Sprintf("/v1/projects/%s/locations/%s/publishers/%s/models/%s:%s", c.Project, c.Location, c.Publisher, model, op)
}

func envOrDefault(key, val string) string {
	if got := os.Getenv(key); got != "" {
		return got
	}
	return val
}

func durationOrDefault(key string, def time.Duration) time.Duration {
	if got := os.Getenv(key); got != "" {
		d, err := time.ParseDuration(got)
		if err == nil {
			return d
		}
	}
	return def
}

func boolOrDefault(key string, def bool) bool {
	if got := os.Getenv(key); got != "" {
		b, err := strconv.ParseBool(got)
		if err == nil {
			return b
		}
	}
	return def
}

func isValidLogLevel(v string) bool {
	switch strings.ToLower(v) {
	case "debug", "info", "warning", "error":
		return true
	default:
		return false
	}
}
