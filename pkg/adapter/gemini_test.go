package adapter

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestGeminiMessagesTransformUsesVertexGenerateContent(t *testing.T) {
	authSrv := newStaticAuthServer(t)
	defer authSrv.Close()

	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/projects/p/locations/us-central1/publishers/google/models/gemini-3.5-flash:generateContent" {
			t.Fatalf("unexpected Gemini path: %s", r.URL.Path)
		}
		if got := r.URL.RawQuery; got != "" {
			t.Fatalf("unexpected query: %s", got)
		}
		body, _ := io.ReadAll(r.Body)
		var got map[string]interface{}
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("decode Gemini request: %v body=%s", err, string(body))
		}
		if _, hasModel := got["model"]; hasModel {
			t.Fatalf("Gemini request must not include Anthropic model field: %s", string(body))
		}
		if _, hasContextManagement := got["context_management"]; hasContextManagement {
			t.Fatalf("Gemini request must not include Claude-only context_management: %s", string(body))
		}
		if got["systemInstruction"].(map[string]interface{})["parts"].([]interface{})[0].(map[string]interface{})["text"] != "You are precise." {
			t.Fatalf("systemInstruction mismatch: %s", string(body))
		}
		gen := got["generationConfig"].(map[string]interface{})
		if gen["maxOutputTokens"].(float64) != 4096 {
			t.Fatalf("maxOutputTokens mismatch: %s", string(body))
		}
		thinking := gen["thinkingConfig"].(map[string]interface{})
		if thinking["thinkingLevel"] != "HIGH" {
			t.Fatalf("expected xhigh to map to HIGH thinkingLevel: %s", string(body))
		}
		tools := got["tools"].([]interface{})
		decl := tools[0].(map[string]interface{})["functionDeclarations"].([]interface{})[0].(map[string]interface{})
		if decl["name"] != "read_file" {
			t.Fatalf("tool declaration mismatch: %s", string(body))
		}
		contents := got["contents"].([]interface{})
		if contents[0].(map[string]interface{})["role"] != "user" {
			t.Fatalf("role mismatch: %s", string(body))
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"responseId":"resp_1",
			"modelVersion":"gemini-3.5-flash",
			"candidates":[{"finishReason":"STOP","content":{"parts":[{"text":"pong"}]}}],
			"usageMetadata":{"promptTokenCount":10,"totalTokenCount":14,"cachedContentTokenCount":2}
		}`))
	}))
	defer gateway.Close()

	s, _ := NewServer(geminiTestConfig(gateway.URL, authSrv.URL))
	reqBody := []byte(`{
		"model":"claude-opus-4-8",
		"stream":false,
		"max_tokens":4096,
		"system":"You are precise.",
		"context_management":{"edits":[{"type":"clear_tool_uses_20250919"}]},
		"output_config":{"effort":"xhigh"},
		"messages":[{"role":"user","content":"ping"}],
		"tools":[{"name":"read_file","description":"Read file","input_schema":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}}]
	}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(reqBody))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var got map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got["type"] != "message" || got["role"] != "assistant" {
		t.Fatalf("unexpected Anthropic response: %s", rec.Body.String())
	}
	if got["content"].([]interface{})[0].(map[string]interface{})["text"] != "pong" {
		t.Fatalf("response text mismatch: %s", rec.Body.String())
	}
	usage := got["usage"].(map[string]interface{})
	if usage["input_tokens"].(float64) != 8 || usage["output_tokens"].(float64) != 4 || usage["cache_read_input_tokens"].(float64) != 2 {
		t.Fatalf("usage mismatch: %s", rec.Body.String())
	}
}

func TestGeminiToolResultAndToolUseMapping(t *testing.T) {
	rewritten, err := anthropicMessagesToGemini([]byte(`{
		"messages":[
			{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"read_file","input":{"path":"README.md"}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":[{"type":"text","text":"ok"}]}]}
		]
	}`))
	if err != nil {
		t.Fatalf("rewrite failed: %v", err)
	}
	var got map[string]interface{}
	_ = json.Unmarshal(rewritten, &got)
	contents := got["contents"].([]interface{})
	call := contents[0].(map[string]interface{})["parts"].([]interface{})[0].(map[string]interface{})["functionCall"].(map[string]interface{})
	if call["name"] != "read_file" || call["id"] != "call_1" {
		t.Fatalf("functionCall mismatch: %s", string(rewritten))
	}
	response := contents[1].(map[string]interface{})["parts"].([]interface{})[0].(map[string]interface{})["functionResponse"].(map[string]interface{})
	if response["name"] != "read_file" || response["id"] != "call_1" {
		t.Fatalf("functionResponse mismatch: %s", string(rewritten))
	}
}

func TestGeminiToolSchemaUsesParametersJSONSchemaForComplexKeywords(t *testing.T) {
	rewritten, err := anthropicMessagesToGemini([]byte(`{
		"messages":[{"role":"user","content":"hi"}],
		"tools":[{"name":"search","input_schema":{"type":"object","properties":{"filters":{"type":"object","additionalProperties":{"type":"string"}}}}}]
	}`))
	if err != nil {
		t.Fatalf("rewrite failed: %v", err)
	}
	var got map[string]interface{}
	_ = json.Unmarshal(rewritten, &got)
	decl := got["tools"].([]interface{})[0].(map[string]interface{})["functionDeclarations"].([]interface{})[0].(map[string]interface{})
	if _, ok := decl["parametersJsonSchema"]; !ok {
		t.Fatalf("expected complex schema to use parametersJsonSchema: %s", string(rewritten))
	}
	if _, ok := decl["parameters"]; ok {
		t.Fatalf("complex schema must not also use parameters: %s", string(rewritten))
	}
}

func TestGeminiStreamGenerateContentConvertsToAnthropicSSE(t *testing.T) {
	authSrv := newStaticAuthServer(t)
	defer authSrv.Close()

	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/projects/p/locations/us-central1/publishers/google/models/gemini-3.5-flash:streamGenerateContent" {
			t.Fatalf("unexpected stream path: %s", r.URL.Path)
		}
		if got := r.URL.Query().Get("alt"); got != "sse" {
			t.Fatalf("expected alt=sse, got query=%s", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"responseId\":\"resp_s\",\"modelVersion\":\"gemini-3.5-flash\",\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"he\"}]}}],\"usageMetadata\":{\"promptTokenCount\":3,\"totalTokenCount\":3}}\n\n"))
		_, _ = w.Write([]byte("data: {\"responseId\":\"resp_s\",\"modelVersion\":\"gemini-3.5-flash\",\"candidates\":[{\"finishReason\":\"STOP\",\"content\":{\"parts\":[{\"text\":\"hello\"}]}}],\"usageMetadata\":{\"promptTokenCount\":3,\"totalTokenCount\":5}}\n\n"))
	}))
	defer gateway.Close()

	s, _ := NewServer(geminiTestConfig(gateway.URL, authSrv.URL))
	reqBody := []byte(`{"model":"claude-sonnet-4-6","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(reqBody))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"event: message_start", "text_delta", `"text":"he"`, `"text":"llo"`, "event: message_delta", `"stop_reason":"end_turn"`, "event: message_stop"} {
		if !strings.Contains(body, want) {
			t.Fatalf("stream response missing %q: %s", want, body)
		}
	}
}

func TestGeminiCountTokens(t *testing.T) {
	authSrv := newStaticAuthServer(t)
	defer authSrv.Close()

	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/projects/p/locations/us-central1/publishers/google/models/gemini-3.5-flash:countTokens" {
			t.Fatalf("unexpected countTokens path: %s", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		if !bytes.Contains(body, []byte(`"contents"`)) {
			t.Fatalf("countTokens should receive Gemini contents: %s", string(body))
		}
		_, _ = w.Write([]byte(`{"totalTokens":123}`))
	}))
	defer gateway.Close()

	s, _ := NewServer(geminiTestConfig(gateway.URL, authSrv.URL))
	reqBody := []byte(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", bytes.NewReader(reqBody))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if strings.TrimSpace(rec.Body.String()) != `{"input_tokens":123}` {
		t.Fatalf("unexpected count_tokens response: %s", rec.Body.String())
	}
}

func TestGeminiRejectsUnknownTopLevelFields(t *testing.T) {
	_, err := anthropicMessagesToGemini([]byte(`{"messages":[{"role":"user","content":"hi"}],"unknown":true}`))
	if err == nil || !strings.Contains(err.Error(), "unsupported Anthropic field") {
		t.Fatalf("expected unknown field rejection, got %v", err)
	}
}

func newStaticAuthServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"id_token": "tok1", "expires_in": 3600})
	}))
}

func geminiTestConfig(gatewayURL, authURL string) Config {
	return Config{
		ListenAddr:       ":0",
		LogLevel:         "info",
		GatewayBaseURL:   gatewayURL,
		VertexAPIFormat:  "gemini",
		Project:          "p",
		Location:         "us-central1",
		Publisher:        "google",
		Model:            "gemini-3.5-flash",
		ModelOverride:    true,
		AnthropicVersion: "vertex-2023-10-16",
		AuthURL:          authURL,
		AuthUserID:       "u",
		AuthPassword:     "p",
		AuthOTPType:      "TOTP",
		RefreshSkew:      time.Minute,
		GatewayTimeout:   5 * time.Second,
		AuthTimeout:      5 * time.Second,
		ForceRefreshOn4x: true,
	}
}
