package adapter

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	ListenAddr           string
	Verbose              bool
	LogLevel             string
	AdapterAPIKey        string
	AllowUnauthenticated bool

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

	RefreshSkew        time.Duration
	GatewayTimeout     time.Duration
	AuthTimeout        time.Duration
	RequestReadTimeout time.Duration
	ForceRefreshOn4x   bool

	InsecureSkipTLSVerify bool
	MaxRequestBodyBytes   int64
	MaxResponseBodyBytes  int64
	MaxDebugCaptureBytes  int64
	MaxStreamEventBytes   int
	SignatureTTL          time.Duration
	SignatureMaxSessions  int
	SignatureMaxEntries   int
}

var vertexResourceSegmentPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._@~-]*$`)

func LoadConfigFromEnv() (Config, error) {
	if err := validateOptionalEnvironment(); err != nil {
		return Config{}, err
	}
	apiFormat := envOrDefault("VERTEX_API_FORMAT", "anthropic")
	publisherDefault := "anthropic"
	if apiFormat == "gemini" {
		publisherDefault = "google"
	}
	cfg := Config{
		ListenAddr:            envOrDefault("ADAPTER_LISTEN_ADDR", "127.0.0.1:8080"),
		Verbose:               boolOrDefault("ADAPTER_VERBOSE", false),
		LogLevel:              envOrDefault("ADAPTER_LOG_LEVEL", "info"),
		AdapterAPIKey:         os.Getenv("ADAPTER_API_KEY"),
		AllowUnauthenticated:  boolOrDefault("ALLOW_UNAUTHENTICATED", false),
		GatewayBaseURL:        strings.TrimRight(os.Getenv("GATEWAY_BASE_URL"), "/"),
		VertexAPIFormat:       apiFormat,
		Project:               os.Getenv("VERTEX_PROJECT"),
		Location:              os.Getenv("VERTEX_LOCATION"),
		Publisher:             envOrDefault("VERTEX_PUBLISHER", publisherDefault),
		Model:                 os.Getenv("VERTEX_MODEL"),
		ModelOverride:         boolOrDefault("MODEL_OVERRIDE", true),
		AnthropicVersion:      envOrDefault("VERTEX_ANTHROPIC_VERSION", "vertex-2023-10-16"),
		AuthURL:               os.Getenv("AUTH_URL"),
		AuthUserID:            os.Getenv("AUTH_USER_ID"),
		AuthPassword:          os.Getenv("AUTH_PASSWORD"),
		AuthOTP:               os.Getenv("AUTH_OTP"),
		AuthOTPType:           envOrDefault("AUTH_OTP_TYPE", "TOTP"),
		RefreshSkew:           durationOrDefault("AUTH_REFRESH_SKEW", 90*time.Second),
		GatewayTimeout:        durationOrDefault("GATEWAY_TIMEOUT", 120*time.Second),
		AuthTimeout:           durationOrDefault("AUTH_TIMEOUT", 10*time.Second),
		RequestReadTimeout:    durationOrDefault("REQUEST_READ_TIMEOUT", 30*time.Second),
		ForceRefreshOn4x:      boolOrDefault("FORCE_REFRESH_ON_401_403", true),
		InsecureSkipTLSVerify: boolOrDefault("INSECURE_SKIP_TLS_VERIFY", false),
		MaxRequestBodyBytes:   int64OrDefault("MAX_REQUEST_BODY_BYTES", 32<<20),
		MaxResponseBodyBytes:  int64OrDefault("MAX_RESPONSE_BODY_BYTES", 64<<20),
		MaxDebugCaptureBytes:  int64OrDefault("MAX_DEBUG_CAPTURE_BYTES", 1<<20),
		MaxStreamEventBytes:   intOrDefault("MAX_STREAM_EVENT_BYTES", 10<<20),
		SignatureTTL:          durationOrDefault("SIGNATURE_TTL", 6*time.Hour),
		SignatureMaxSessions:  intOrDefault("SIGNATURE_MAX_SESSIONS", 1024),
		SignatureMaxEntries:   intOrDefault("SIGNATURE_MAX_ENTRIES_PER_SESSION", 2048),
	}

	if cfg.GatewayBaseURL == "" || cfg.Project == "" || cfg.Location == "" || cfg.Model == "" {
		return Config{}, errors.New("GATEWAY_BASE_URL, VERTEX_PROJECT, VERTEX_LOCATION, VERTEX_MODEL are required")
	}
	if cfg.AuthURL == "" || cfg.AuthUserID == "" || cfg.AuthPassword == "" {
		return Config{}, errors.New("AUTH_URL, AUTH_USER_ID, AUTH_PASSWORD are required")
	}
	parsedAuth, err := url.ParseRequestURI(cfg.AuthURL)
	if err != nil || parsedAuth.Host == "" || (parsedAuth.Scheme != "http" && parsedAuth.Scheme != "https") {
		return Config{}, errors.New("AUTH_URL must be a valid absolute http or https URL")
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
	if _, err := url.ParseRequestURI(cfg.GatewayBaseURL); err != nil {
		return Config{}, fmt.Errorf("GATEWAY_BASE_URL must be a valid absolute URL: %w", err)
	}
	parsedGateway, _ := url.Parse(cfg.GatewayBaseURL)
	if parsedGateway.Scheme != "http" && parsedGateway.Scheme != "https" || parsedGateway.Host == "" {
		return Config{}, errors.New("GATEWAY_BASE_URL must use http or https and include a host")
	}
	if parsedGateway.RawQuery != "" || parsedGateway.Fragment != "" {
		return Config{}, errors.New("GATEWAY_BASE_URL must not contain a query or fragment")
	}
	for name, value := range map[string]string{
		"VERTEX_PROJECT": cfg.Project, "VERTEX_LOCATION": cfg.Location,
		"VERTEX_PUBLISHER": cfg.Publisher, "VERTEX_MODEL": cfg.Model,
	} {
		if err := validateVertexResourceSegment(name, value); err != nil {
			return Config{}, err
		}
	}
	if cfg.VertexAPIFormat == "gemini" {
		if err := validateGeminiModelLocation(cfg.Model, cfg.Location); err != nil {
			return Config{}, err
		}
	}
	if cfg.MaxRequestBodyBytes <= 0 || cfg.MaxResponseBodyBytes <= 0 || cfg.MaxDebugCaptureBytes <= 0 || cfg.MaxStreamEventBytes <= 0 {
		return Config{}, errors.New("body, capture, and stream size limits must be positive")
	}
	if cfg.SignatureTTL <= 0 || cfg.SignatureMaxSessions <= 0 || cfg.SignatureMaxEntries <= 0 {
		return Config{}, errors.New("signature cache limits must be positive")
	}
	if cfg.AuthTimeout <= 0 || cfg.GatewayTimeout <= 0 || cfg.RequestReadTimeout <= 0 || cfg.RefreshSkew < 0 {
		return Config{}, errors.New("timeouts must be positive and AUTH_REFRESH_SKEW must not be negative")
	}
	if cfg.AdapterAPIKey == "" && !cfg.AllowUnauthenticated && !isLoopbackListenAddress(cfg.ListenAddr) {
		return Config{}, errors.New("ADAPTER_API_KEY is required for a non-loopback ADAPTER_LISTEN_ADDR unless ALLOW_UNAUTHENTICATED=true is explicitly set")
	}

	return cfg, nil
}

func (c Config) withRuntimeDefaults() Config {
	if c.GatewayTimeout == 0 {
		c.GatewayTimeout = 120 * time.Second
	}
	if c.AuthTimeout == 0 {
		c.AuthTimeout = 10 * time.Second
	}
	if c.MaxRequestBodyBytes == 0 {
		c.MaxRequestBodyBytes = 32 << 20
	}
	if c.MaxResponseBodyBytes == 0 {
		c.MaxResponseBodyBytes = 64 << 20
	}
	if c.MaxDebugCaptureBytes == 0 {
		c.MaxDebugCaptureBytes = 1 << 20
	}
	if c.MaxStreamEventBytes == 0 {
		c.MaxStreamEventBytes = 10 << 20
	}
	if c.SignatureTTL == 0 {
		c.SignatureTTL = 6 * time.Hour
	}
	if c.RequestReadTimeout == 0 {
		c.RequestReadTimeout = 30 * time.Second
	}
	if c.SignatureMaxSessions == 0 {
		c.SignatureMaxSessions = 1024
	}
	if c.SignatureMaxEntries == 0 {
		c.SignatureMaxEntries = 2048
	}
	return c
}

func validateOptionalEnvironment() error {
	for _, key := range []string{"ADAPTER_VERBOSE", "MODEL_OVERRIDE", "FORCE_REFRESH_ON_401_403", "INSECURE_SKIP_TLS_VERIFY", "ALLOW_UNAUTHENTICATED"} {
		if value := os.Getenv(key); value != "" {
			if _, err := strconv.ParseBool(value); err != nil {
				return fmt.Errorf("%s must be a boolean: %w", key, err)
			}
		}
	}
	for _, key := range []string{"AUTH_REFRESH_SKEW", "AUTH_TIMEOUT", "GATEWAY_TIMEOUT", "REQUEST_READ_TIMEOUT", "SIGNATURE_TTL"} {
		if value := os.Getenv(key); value != "" {
			if _, err := time.ParseDuration(value); err != nil {
				return fmt.Errorf("%s must be a duration: %w", key, err)
			}
		}
	}
	for _, key := range []string{"MAX_REQUEST_BODY_BYTES", "MAX_RESPONSE_BODY_BYTES", "MAX_DEBUG_CAPTURE_BYTES", "MAX_STREAM_EVENT_BYTES", "SIGNATURE_MAX_SESSIONS", "SIGNATURE_MAX_ENTRIES_PER_SESSION"} {
		if value := os.Getenv(key); value != "" {
			bitSize := 64
			if key == "MAX_STREAM_EVENT_BYTES" || key == "SIGNATURE_MAX_SESSIONS" || key == "SIGNATURE_MAX_ENTRIES_PER_SESSION" {
				bitSize = strconv.IntSize
			}
			if n, err := strconv.ParseInt(value, 10, bitSize); err != nil || n <= 0 {
				return fmt.Errorf("%s must be a positive integer", key)
			}
		}
	}
	return nil
}

func isLoopbackListenAddress(address string) bool {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (c Config) targetPath(op vertexOperation, model string) string {
	return fmt.Sprintf("/v1/projects/%s/locations/%s/publishers/%s/models/%s:%s", c.Project, c.Location, c.Publisher, model, op)
}

func validateVertexResourceSegment(name, value string) error {
	if !vertexResourceSegmentPattern.MatchString(value) {
		return fmt.Errorf("%s contains characters that are unsafe in a Vertex resource path", name)
	}
	return nil
}

func validateGeminiModelLocation(model, location string) error {
	if !strings.HasPrefix(model, "gemini-") {
		return fmt.Errorf("VERTEX_API_FORMAT=gemini requires a Gemini model, got %q", model)
	}
	switch model {
	case "gemini-3.6-flash":
		if location != "global" {
			return errors.New("gemini-3.6-flash requires VERTEX_LOCATION=global")
		}
	case "gemini-3.5-flash":
		allowed := map[string]bool{
			"global": true, "us": true, "eu": true,
			"northamerica-northeast1": true, "europe-west2": true, "europe-west3": true,
			"asia-northeast1": true, "asia-south1": true, "asia-southeast1": true,
		}
		if !allowed[location] {
			return fmt.Errorf("gemini-3.5-flash is not available for online prediction in location %q", location)
		}
	case "gemini-3.7-flash":
		return errors.New("gemini-3.7-flash is not a published Vertex model")
	}
	return nil
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

func int64OrDefault(key string, def int64) int64 {
	if got := os.Getenv(key); got != "" {
		if n, err := strconv.ParseInt(got, 10, 64); err == nil {
			return n
		}
	}
	return def
}

func intOrDefault(key string, def int) int {
	if got := os.Getenv(key); got != "" {
		if n, err := strconv.Atoi(got); err == nil {
			return n
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
