package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type vertexOperation string

const (
	operationRawPredict            vertexOperation = "rawPredict"
	operationStreamRawPredict      vertexOperation = "streamRawPredict"
	operationGenerateContent       vertexOperation = "generateContent"
	operationStreamGenerateContent vertexOperation = "streamGenerateContent"
	operationCountTokens           vertexOperation = "countTokens"
)

type Server struct {
	cfg     Config
	tokens  *tokenProvider
	gateway *http.Client
	handler http.Handler
	logger  *Logger
	reqID   uint64

	thoughtSignaturesMu sync.Mutex
	thoughtSignatures   map[string]string
}

type messagesRequest struct {
	Model    string          `json:"model"`
	Stream   bool            `json:"stream"`
	Messages json.RawMessage `json:"messages"`
}

func NewServer(cfg Config) (*Server, error) {
	if !isValidLogLevel(cfg.LogLevel) {
		return nil, fmt.Errorf("invalid log level: %s", cfg.LogLevel)
	}
	s := &Server{
		cfg:     cfg,
		tokens:  newTokenProvider(cfg),
		gateway: &http.Client{Timeout: cfg.GatewayTimeout},
		logger:  NewLogger(cfg.LogLevel, cfg.Verbose),

		thoughtSignatures: map[string]string{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.healthz)
	mux.HandleFunc("/v1/messages", s.messages)
	mux.HandleFunc("/v1/messages/count_tokens", s.countTokens)
	s.handler = loggingMiddleware(mux, s.logger)
	return s, nil
}

func (s *Server) Handler() http.Handler { return s.handler }

func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (s *Server) messages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ctx := r.Context()
	requestID := atomic.AddUint64(&s.reqID, 1)
	startedAt := time.Now()

	body, err := io.ReadAll(r.Body)
	if err != nil {
		s.logger.Warnf("request_id=%d failed to read body: %v", requestID, err)
		http.Error(w, "failed to read body", http.StatusBadRequest)
		return
	}

	var req messagesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		s.logger.Warnf("request_id=%d invalid json body: %v", requestID, err)
		http.Error(w, "invalid json body", http.StatusBadRequest)
		return
	}

	s.logger.Debugf("request_id=%d inbound_headers=%s", requestID, formatHeaders(r.Header))
	s.logger.Debugf("request_id=%d inbound_messages_request_json=\n%s", requestID, prettyJSONOrRaw(body))

	rewrittenBody, targetModel, stream, op, err := s.rewriteRequestForVertex(body, req)
	if err != nil {
		s.logger.Warnf("request_id=%d failed to rewrite request: %v", requestID, err)
		http.Error(w, "invalid messages request", http.StatusBadRequest)
		return
	}

	s.logger.Infof("request_id=%d path=%s method=%s stream=%t model=%s", requestID, r.URL.Path, r.Method, stream, targetModel)
	s.logger.Debugf("request_id=%d rewritten_vertex_request_json=\n%s", requestID, prettyJSONOrRaw(rewrittenBody))

	token, err := s.tokens.GetBearerToken(ctx)
	if err != nil {
		s.logger.Errorf("request_id=%d token retrieval failed: %v", requestID, err)
		http.Error(w, fmt.Sprintf("get token failed: %v", err), http.StatusBadGateway)
		return
	}

	targetURL := s.cfg.GatewayBaseURL + s.cfg.targetPath(op, targetModel)
	resp, err := s.forward(ctx, rewrittenBody, token, op, targetModel)
	if err != nil {
		s.logger.Errorf("request_id=%d upstream request failed target=%s err=%v", requestID, targetURL, err)
		http.Error(w, fmt.Sprintf("upstream failed: %v", err), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if s.cfg.ForceRefreshOn4x && (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) {
		s.logger.Warnf("request_id=%d upstream status=%d; forcing token refresh and retry", requestID, resp.StatusCode)
		newToken, ferr := s.tokens.ForceRefresh(context.Background())
		if ferr == nil {
			resp.Body.Close()
			resp, err = s.forward(ctx, rewrittenBody, newToken, op, targetModel)
			if err != nil {
				s.logger.Errorf("request_id=%d retry upstream failed target=%s err=%v", requestID, targetURL, err)
				http.Error(w, fmt.Sprintf("retry upstream failed: %v", err), http.StatusBadGateway)
				return
			}
			defer resp.Body.Close()
		} else {
			s.logger.Errorf("request_id=%d force token refresh failed: %v", requestID, ferr)
		}
	}

	if stream {
		copyHeaders(w.Header(), resp.Header)
		if s.cfg.VertexAPIFormat == "gemini" && resp.StatusCode >= 200 && resp.StatusCode < 300 {
			w.Header().Set("Content-Type", "text/event-stream")
		}
		w.WriteHeader(resp.StatusCode)
		var captured []byte
		var copied int
		if s.cfg.VertexAPIFormat == "gemini" && resp.StatusCode >= 200 && resp.StatusCode < 300 {
			captured, copied = s.streamGeminiAsAnthropic(w, resp.Body)
		} else {
			captured, copied = streamCopyAndCapture(w, resp.Body)
		}
		dur := time.Since(startedAt)
		if resp.StatusCode >= 500 {
			s.logger.Errorf("request_id=%d completed stream status=%d bytes=%d duration=%s target=%s", requestID, resp.StatusCode, copied, dur, targetURL)
		} else if resp.StatusCode >= 400 {
			s.logger.Warnf("request_id=%d completed stream status=%d bytes=%d duration=%s target=%s", requestID, resp.StatusCode, copied, dur, targetURL)
		} else {
			s.logger.Infof("request_id=%d completed stream status=%d bytes=%d duration=%s target=%s", requestID, resp.StatusCode, copied, dur, targetURL)
		}
		s.logger.Debugf("request_id=%d upstream_response_headers=%s", requestID, formatHeaders(resp.Header))
		s.logger.Debugf("request_id=%d upstream_stream_response=\n%s", requestID, string(captured))
		return
	}

	respBody, _ := io.ReadAll(resp.Body)
	convertedGemini := false
	if s.cfg.VertexAPIFormat == "gemini" && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		converted, signatures, err := geminiResponseToAnthropicWithSignatures(respBody)
		if err != nil {
			s.logger.Warnf("request_id=%d failed to convert Gemini response: %v", requestID, err)
			http.Error(w, "invalid Gemini response", http.StatusBadGateway)
			return
		}
		s.rememberThoughtSignatures(signatures)
		respBody = converted
		convertedGemini = true
	}
	copyHeaders(w.Header(), resp.Header)
	if convertedGemini {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(respBody)
	dur := time.Since(startedAt)
	if resp.StatusCode >= 500 {
		s.logger.Errorf("request_id=%d completed status=%d bytes=%d duration=%s target=%s", requestID, resp.StatusCode, len(respBody), dur, targetURL)
	} else if resp.StatusCode >= 400 {
		s.logger.Warnf("request_id=%d completed status=%d bytes=%d duration=%s target=%s", requestID, resp.StatusCode, len(respBody), dur, targetURL)
	} else {
		s.logger.Infof("request_id=%d completed status=%d bytes=%d duration=%s target=%s", requestID, resp.StatusCode, len(respBody), dur, targetURL)
	}
	s.logger.Debugf("request_id=%d upstream_response_headers=%s", requestID, formatHeaders(resp.Header))
	s.logger.Debugf("request_id=%d upstream_messages_response_json=\n%s", requestID, prettyJSONOrRaw(respBody))
}

func (s *Server) rewriteRequestForVertex(body []byte, parsed messagesRequest) ([]byte, string, bool, vertexOperation, error) {
	var payload map[string]interface{}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, "", false, "", err
	}

	targetModel := parsed.Model
	if targetModel == "" || s.cfg.ModelOverride {
		targetModel = s.cfg.Model
	}
	if s.cfg.VertexAPIFormat == "gemini" {
		rewritten, err := anthropicMessagesToGeminiWithSignatures(body, s.snapshotThoughtSignatures())
		if err != nil {
			return nil, "", false, "", err
		}
		op := operationGenerateContent
		if parsed.Stream {
			op = operationStreamGenerateContent
		}
		return rewritten, targetModel, parsed.Stream, op, nil
	}
	delete(payload, "model")
	payload["anthropic_version"] = s.cfg.AnthropicVersion

	rewritten, err := json.Marshal(payload)
	if err != nil {
		return nil, "", false, "", err
	}
	op := operationRawPredict
	if parsed.Stream {
		op = operationStreamRawPredict
	}
	return rewritten, targetModel, parsed.Stream, op, nil
}

func (s *Server) rememberThoughtSignatures(signatures map[string]string) {
	if len(signatures) == 0 {
		return
	}
	s.thoughtSignaturesMu.Lock()
	defer s.thoughtSignaturesMu.Unlock()
	for id, signature := range signatures {
		if id != "" && signature != "" {
			s.thoughtSignatures[id] = signature
		}
	}
}

func (s *Server) snapshotThoughtSignatures() map[string]string {
	s.thoughtSignaturesMu.Lock()
	defer s.thoughtSignaturesMu.Unlock()
	if len(s.thoughtSignatures) == 0 {
		return nil
	}
	copied := make(map[string]string, len(s.thoughtSignatures))
	for id, signature := range s.thoughtSignatures {
		copied[id] = signature
	}
	return copied
}

func (s *Server) forward(ctx context.Context, body []byte, bearer string, op vertexOperation, model string) (*http.Response, error) {
	target := s.cfg.GatewayBaseURL + s.cfg.targetPath(op, model)
	if op == operationStreamGenerateContent {
		target += "?alt=sse"
	}
	endpoint, err := url.Parse(target)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", bearer)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if op == operationStreamRawPredict || op == operationStreamGenerateContent {
		req.Header.Set("Accept", "text/event-stream")
	}

	return s.gateway.Do(req)
}

func (s *Server) countTokens(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	requestID := atomic.AddUint64(&s.reqID, 1)
	if s.cfg.VertexAPIFormat != "gemini" {
		http.Error(w, "count_tokens is only implemented for VERTEX_API_FORMAT=gemini", http.StatusNotImplemented)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		s.logger.Warnf("request_id=%d failed to read count_tokens body: %v", requestID, err)
		http.Error(w, "failed to read body", http.StatusBadRequest)
		return
	}
	var req messagesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		s.logger.Warnf("request_id=%d invalid count_tokens json body: %v", requestID, err)
		http.Error(w, "invalid json body", http.StatusBadRequest)
		return
	}
	s.logger.Debugf("request_id=%d inbound_headers=%s", requestID, formatHeaders(r.Header))
	s.logger.Debugf("request_id=%d inbound_count_tokens_request_json=\n%s", requestID, prettyJSONOrRaw(body))
	targetModel := req.Model
	if targetModel == "" || s.cfg.ModelOverride {
		targetModel = s.cfg.Model
	}
	rewritten, err := anthropicMessagesToGeminiWithSignatures(body, s.snapshotThoughtSignatures())
	if err != nil {
		s.logger.Warnf("request_id=%d failed to rewrite count_tokens request: %v", requestID, err)
		http.Error(w, "invalid messages request", http.StatusBadRequest)
		return
	}
	token, err := s.tokens.GetBearerToken(r.Context())
	if err != nil {
		s.logger.Errorf("request_id=%d token retrieval failed: %v", requestID, err)
		http.Error(w, fmt.Sprintf("get token failed: %v", err), http.StatusBadGateway)
		return
	}
	resp, err := s.forward(r.Context(), rewritten, token, operationCountTokens, targetModel)
	if err != nil {
		s.logger.Errorf("request_id=%d upstream count_tokens failed: %v", requestID, err)
		http.Error(w, fmt.Sprintf("upstream failed: %v", err), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	copyHeaders(w.Header(), resp.Header)
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		converted, err := geminiCountTokensToAnthropic(respBody)
		if err != nil {
			s.logger.Warnf("request_id=%d failed to convert Gemini count_tokens response: %v", requestID, err)
			http.Error(w, "invalid Gemini count_tokens response", http.StatusBadGateway)
			return
		}
		respBody = converted
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(respBody)
}

func streamCopyAndCapture(w http.ResponseWriter, r io.Reader) ([]byte, int) {
	flusher, ok := w.(http.Flusher)
	captured := bytes.NewBuffer(nil)
	copied := 0
	if !ok {
		n, _ := io.Copy(io.MultiWriter(w, captured), r)
		return captured.Bytes(), int(n)
	}
	buf := make([]byte, 32*1024)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			_, _ = w.Write(chunk)
			_, _ = captured.Write(chunk)
			copied += n
			flusher.Flush()
		}
		if err != nil {
			return captured.Bytes(), copied
		}
	}
}

func copyHeaders(dst, src http.Header) {
	for k, vs := range src {
		lk := strings.ToLower(k)
		if lk == "content-length" || lk == "connection" || lk == "transfer-encoding" {
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

func loggingMiddleware(next http.Handler, logger *Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		if r.URL.Path != "/v1/messages" {
			logger.Debugf("path=%s method=%s duration=%s", r.URL.Path, r.Method, time.Since(start))
		}
	})
}

func prettyJSONOrRaw(data []byte) string {
	var dst bytes.Buffer
	if err := json.Indent(&dst, data, "", "  "); err == nil {
		return dst.String()
	}
	return string(data)
}

func formatHeaders(h http.Header) string {
	parts := make([]string, 0, len(h))
	for k, values := range h {
		if strings.EqualFold(k, "Authorization") {
			parts = append(parts, k+": [REDACTED]")
			continue
		}
		parts = append(parts, fmt.Sprintf("%s: %s", k, strings.Join(values, ",")))
	}
	return strings.Join(parts, " | ")
}
