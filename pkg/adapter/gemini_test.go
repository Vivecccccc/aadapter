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
		if r.URL.Path != "/v1/projects/p/locations/global/publishers/google/models/gemini-3.5-flash:generateContent" {
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
			{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"read_file","input":{"path":"README.md"},"thought_signature":"sig_a"}]},
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
	part := contents[0].(map[string]interface{})["parts"].([]interface{})[0].(map[string]interface{})
	if part["thoughtSignature"] != "sig_a" {
		t.Fatalf("thoughtSignature must be preserved on functionCall part: %s", string(rewritten))
	}
	response := contents[1].(map[string]interface{})["parts"].([]interface{})[0].(map[string]interface{})["functionResponse"].(map[string]interface{})
	if response["name"] != "read_file" || response["id"] != "call_1" {
		t.Fatalf("functionResponse mismatch: %s", string(rewritten))
	}
}

func TestGeminiToolUseResponsePreservesThoughtSignature(t *testing.T) {
	converted, err := geminiResponseToAnthropic([]byte(`{
		"responseId":"resp_tool",
		"modelVersion":"gemini-3.5-flash",
		"candidates":[{"finishReason":"STOP","content":{"parts":[{"functionCall":{"name":"read_file","args":{"path":"README.md"}},"thoughtSignature":"sig_a"}]}}],
		"usageMetadata":{"promptTokenCount":5,"totalTokenCount":8}
	}`))
	if err != nil {
		t.Fatalf("convert failed: %v", err)
	}
	var got map[string]interface{}
	_ = json.Unmarshal(converted, &got)
	var block map[string]interface{}
	for _, raw := range got["content"].([]interface{}) {
		candidate := raw.(map[string]interface{})
		if candidate["type"] == "tool_use" {
			block = candidate
			break
		}
	}
	if block == nil {
		t.Fatalf("missing tool_use block: %s", converted)
	}
	if block["type"] != "tool_use" || block["thought_signature"] != "sig_a" {
		t.Fatalf("tool_use thought_signature mismatch: %s", string(converted))
	}
	if id, _ := block["id"].(string); !strings.HasPrefix(id, synthesizedToolIDPrefix) {
		t.Fatalf("expected synthesized tool_use id, got: %s", string(converted))
	}
	request, _ := json.Marshal(map[string]interface{}{
		"messages": []interface{}{
			map[string]interface{}{"role": "user", "content": "read it"},
			map[string]interface{}{"role": "assistant", "content": got["content"]},
			map[string]interface{}{"role": "user", "content": []interface{}{map[string]interface{}{"type": "tool_result", "tool_use_id": block["id"], "content": "ok"}}},
		},
	})
	rewritten, err := anthropicMessagesToGeminiForModel(request, nil, "gemini-3.5-flash")
	if err != nil {
		t.Fatalf("stateless signature round-trip failed: %v", err)
	}
	if !bytes.Contains(rewritten, []byte(`"thoughtSignature":"sig_a"`)) {
		t.Fatalf("signature carrier was not restored without server state: %s", rewritten)
	}
}

func TestGeminiServerRestoresStoredThoughtSignatureWhenClaudeDropsIt(t *testing.T) {
	authSrv := newStaticAuthServer(t)
	defer authSrv.Close()

	requestNumber := 0
	var firstToolID string
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestNumber++
		body, _ := io.ReadAll(r.Body)
		switch requestNumber {
		case 1:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"responseId":"resp_tool",
				"modelVersion":"gemini-3.5-flash",
				"candidates":[{"finishReason":"STOP","content":{"parts":[{"functionCall":{"name":"Bash","args":{}},"thoughtSignature":"sig_server"}]}}],
				"usageMetadata":{"promptTokenCount":5,"totalTokenCount":8}
			}`))
		case 2:
			var got map[string]interface{}
			if err := json.Unmarshal(body, &got); err != nil {
				t.Fatalf("decode second Gemini request: %v body=%s", err, string(body))
			}
			contents := got["contents"].([]interface{})
			modelContent := contents[1].(map[string]interface{})
			parts := modelContent["parts"].([]interface{})
			callPart := parts[0].(map[string]interface{})
			if callPart["thoughtSignature"] != "sig_server" {
				t.Fatalf("stored thoughtSignature not restored: %s", string(body))
			}
			call := callPart["functionCall"].(map[string]interface{})
			if call["name"] != "Bash" {
				t.Fatalf("functionCall mismatch: %s", string(body))
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"responseId":"resp_done",
				"modelVersion":"gemini-3.5-flash",
				"candidates":[{"finishReason":"STOP","content":{"parts":[{"text":"done"}]}}],
				"usageMetadata":{"promptTokenCount":8,"totalTokenCount":9}
			}`))
		default:
			t.Fatalf("unexpected gateway request %d", requestNumber)
		}
	}))
	defer gateway.Close()

	s, _ := NewServer(geminiTestConfig(gateway.URL, authSrv.URL))
	firstReq := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader([]byte(`{
		"model":"claude-sonnet-4-6",
		"messages":[{"role":"user","content":"run pwd"}],
		"tools":[{"name":"Bash","input_schema":{"type":"object"}}]
	}`)))
	firstReq.Header.Set(claudeCodeSessionHeader, "session-a")
	firstRec := httptest.NewRecorder()
	s.Handler().ServeHTTP(firstRec, firstReq)
	if firstRec.Code != http.StatusOK {
		t.Fatalf("first status=%d body=%s", firstRec.Code, firstRec.Body.String())
	}
	var firstResp map[string]interface{}
	_ = json.Unmarshal(firstRec.Body.Bytes(), &firstResp)
	var firstBlock map[string]interface{}
	for _, raw := range firstResp["content"].([]interface{}) {
		candidate := raw.(map[string]interface{})
		if candidate["type"] == "tool_use" {
			firstBlock = candidate
			break
		}
	}
	if firstBlock == nil {
		t.Fatalf("first response missing tool_use: %s", firstRec.Body.String())
	}
	firstToolID = firstBlock["id"].(string)
	if firstBlock["thought_signature"] != "sig_server" {
		t.Fatalf("first response missing thought_signature: %s", firstRec.Body.String())
	}

	secondBody := []byte(`{
		"model":"claude-sonnet-4-6",
		"messages":[
			{"role":"user","content":"run pwd"},
			{"role":"assistant","content":[{"type":"tool_use","id":"` + firstToolID + `","name":"Bash","input":{}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"` + firstToolID + `","content":[{"type":"text","text":"ok"}]}]}
		],
		"tools":[{"name":"Bash","input_schema":{"type":"object"}}]
	}`)
	secondReq := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(secondBody))
	secondReq.Header.Set(claudeCodeSessionHeader, "session-a")
	secondRec := httptest.NewRecorder()
	s.Handler().ServeHTTP(secondRec, secondReq)
	if secondRec.Code != http.StatusOK {
		t.Fatalf("second status=%d body=%s", secondRec.Code, secondRec.Body.String())
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
		if r.URL.Path != "/v1/projects/p/locations/global/publishers/google/models/gemini-3.5-flash:streamGenerateContent" {
			t.Fatalf("unexpected stream path: %s", r.URL.Path)
		}
		if got := r.URL.Query().Get("alt"); got != "sse" {
			t.Fatalf("expected alt=sse, got query=%s", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"responseId\":\"resp_s\",\"modelVersion\":\"gemini-3.5-flash\",\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"he\"}]}}],\"usageMetadata\":{\"promptTokenCount\":3,\"totalTokenCount\":3}}\n\n"))
		_, _ = w.Write([]byte("data: {\"responseId\":\"resp_s\",\"modelVersion\":\"gemini-3.5-flash\",\"candidates\":[{\"finishReason\":\"STOP\",\"content\":{\"parts\":[{\"text\":\"llo\"}]}}],\"usageMetadata\":{\"promptTokenCount\":3,\"totalTokenCount\":5}}\n\n"))
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
	for _, want := range []string{"event: message_start", `"content":[]`, `"stop_reason":null`, "text_delta", `"text":"he"`, `"text":"llo"`, "event: message_delta", `"stop_reason":"end_turn"`, "event: message_stop"} {
		if !strings.Contains(body, want) {
			t.Fatalf("stream response missing %q: %s", want, body)
		}
	}
}

func TestGeminiStreamPreservesToolUseThoughtSignature(t *testing.T) {
	authSrv := newStaticAuthServer(t)
	defer authSrv.Close()

	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"responseId\":\"resp_s\",\"modelVersion\":\"gemini-3.5-flash\",\"candidates\":[{\"finishReason\":\"STOP\",\"content\":{\"parts\":[{\"functionCall\":{\"name\":\"read_file\",\"args\":{\"path\":\"README.md\"}},\"thoughtSignature\":\"sig_stream\"},{\"functionCall\":{\"name\":\"read_file\",\"args\":{\"path\":\"README.md\"}}}]}}],\"usageMetadata\":{\"promptTokenCount\":3,\"totalTokenCount\":5}}\n\n"))
	}))
	defer gateway.Close()

	s, _ := NewServer(geminiTestConfig(gateway.URL, authSrv.URL))
	reqBody := []byte(`{"model":"claude-sonnet-4-6","stream":true,"messages":[{"role":"user","content":"hi"}],"tools":[{"name":"read_file","input_schema":{"type":"object"}}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(reqBody))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"event: content_block_start", `"type":"tool_use"`, `"input":{}`, `"thought_signature":"sig_stream"`, `"stop_reason":"tool_use"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("stream tool response missing %q: %s", want, body)
		}
	}
	if got := strings.Count(body, `"type":"tool_use"`); got != 2 {
		t.Fatalf("expected two parallel tool calls, got %d: %s", got, body)
	}
}

func TestGemini35And36ModelSpecificRequestRules(t *testing.T) {
	body := []byte(`{
		"messages":[{"role":"user","content":"hello"}],
		"max_tokens":100,"temperature":0.2,"top_p":0.8,"top_k":12,"stop_sequences":["END"]
	}`)
	for _, model := range []string{"gemini-3.5-flash", "gemini-3.6-flash"} {
		rewritten, err := anthropicMessagesToGeminiForModel(body, nil, model)
		if err != nil {
			t.Fatalf("%s rewrite failed: %v", model, err)
		}
		var got map[string]interface{}
		_ = json.Unmarshal(rewritten, &got)
		cfg := got["generationConfig"].(map[string]interface{})
		for _, forbidden := range []string{"topK", "stopSequences"} {
			if _, ok := cfg[forbidden]; ok {
				t.Fatalf("%s must not forward %s: %s", model, forbidden, rewritten)
			}
		}
		if model == "gemini-3.5-flash" {
			if cfg["temperature"] != 0.2 || cfg["topP"] != 0.8 {
				t.Fatalf("Gemini 3.5 must preserve supported sampling controls: %s", rewritten)
			}
		} else if _, hasTemperature := cfg["temperature"]; hasTemperature {
			t.Fatalf("Gemini 3.6 must omit custom sampling controls: %s", rewritten)
		}
	}
	_, err := anthropicMessagesToGeminiForModel([]byte(`{"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"prefill"}]}`), nil, "gemini-3.6-flash")
	if err == nil || !strings.Contains(err.Error(), "final input turn") {
		t.Fatalf("expected clear Gemini 3.6 prefill rejection, got %v", err)
	}
}

func TestGeminiStopSequencesAreAppliedLocally(t *testing.T) {
	converted, _, err := geminiResponseToAnthropicWithSignaturesAndStops([]byte(`{
		"responseId":"r","modelVersion":"gemini-3.5-flash",
		"candidates":[{"finishReason":"STOP","content":{"parts":[{"text":"before EN"},{"text":"D after"}]}}],
		"usageMetadata":{"promptTokenCount":1,"totalTokenCount":4}
	}`), []string{"END"})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]interface{}
	_ = json.Unmarshal(converted, &got)
	if got["stop_reason"] != "stop_sequence" || got["stop_sequence"] != "END" {
		t.Fatalf("stop metadata mismatch: %s", converted)
	}
	blocks := got["content"].([]interface{})
	combined := ""
	for _, raw := range blocks {
		combined += raw.(map[string]interface{})["text"].(string)
	}
	if combined != "before " {
		t.Fatalf("unexpected truncated text %q", combined)
	}
}

func TestGeminiStreamingStopSequenceAcrossEvents(t *testing.T) {
	authSrv := newStaticAuthServer(t)
	defer authSrv.Close()
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"before EN\"}]}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"candidates\":[{\"finishReason\":\"STOP\",\"content\":{\"parts\":[{\"text\":\"D after\"}]}}]}\n\n"))
	}))
	defer gateway.Close()

	s, _ := NewServer(geminiTestConfig(gateway.URL, authSrv.URL))
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(`{"stream":true,"stop_sequences":["END"],"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	body := rec.Body.String()
	for _, want := range []string{`"text":"before "`, `"stop_reason":"stop_sequence"`, `"stop_sequence":"END"`, `"id":"msg_gemini_`, `"model":"gemini-3.5-flash"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("stream response missing %q: %s", want, body)
		}
	}
	if strings.Contains(body, "after") {
		t.Fatalf("stream leaked text after stop sequence: %s", body)
	}
}

func TestGeminiRejectsUnsupportedToolSemantics(t *testing.T) {
	base := `{"messages":[{"role":"user","content":"hi"}],"tools":[{"name":"read_file","input_schema":{"type":"object"}}],`
	for _, suffix := range []string{
		`"tool_choice":{"type":"auto","disable_parallel_tool_use":true}}`,
		`"container":{"id":"container_1"}}`,
		`"tools":"not-an-array"}`,
	} {
		if _, err := anthropicMessagesToGemini([]byte(base + suffix)); err == nil {
			t.Fatalf("expected request to be rejected: %s", suffix)
		}
	}
	converted, err := anthropicMessagesToGemini([]byte(`{
		"messages":[{"role":"user","content":"hi"}],
		"tools":[
			{"name":"BatchTool","input_schema":{"type":"object"}},
			{"name":"read_file","input_schema":{"type":"object"}}
		]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(converted, []byte("BatchTool")) || !bytes.Contains(converted, []byte("read_file")) {
		t.Fatalf("legacy BatchTool filtering mismatch: %s", converted)
	}
}

func TestSignatureStoreIsSessionScopedAndBounded(t *testing.T) {
	store := newSignatureStore(time.Hour, 2, 1)
	store.remember("session-a", map[string]string{"tool:1": "sig-a", "tool:2": "sig-b"})
	store.remember("session-b", map[string]string{"tool:1": "sig-other"})
	if got := store.snapshot("session-b")["tool:1"]; got != "sig-other" {
		t.Fatalf("session isolation failed: %q", got)
	}
	if got := store.snapshot("session-a"); len(got) != 1 {
		t.Fatalf("per-session bound failed: %#v", got)
	}
	if got := store.snapshot(""); got != nil {
		t.Fatalf("anonymous requests must not share signatures: %#v", got)
	}
}

func TestMalformedGeminiStreamEmitsAnthropicError(t *testing.T) {
	authSrv := newStaticAuthServer(t)
	defer authSrv.Close()
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {not-json}\n\n"))
	}))
	defer gateway.Close()
	s, _ := NewServer(geminiTestConfig(gateway.URL, authSrv.URL))
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(`{"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, "event: error") || strings.Contains(body, "event: message_stop") {
		t.Fatalf("malformed stream must terminate with an error event: %s", body)
	}
}

func TestGeminiCountTokens(t *testing.T) {
	authSrv := newStaticAuthServer(t)
	defer authSrv.Close()

	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/projects/p/locations/global/publishers/google/models/gemini-3.5-flash:countTokens" {
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
		Location:         "global",
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
