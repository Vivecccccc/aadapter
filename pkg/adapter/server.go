package adapter

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
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
	cfg           Config
	tokens        *tokenProvider
	gateway       *http.Client
	streamGateway *http.Client
	handler       http.Handler
	logger        *Logger
	reqID         uint64
	signatures    *signatureStore
}

type messagesRequest struct {
	Model         string          `json:"model"`
	Stream        bool            `json:"stream"`
	Messages      json.RawMessage `json:"messages"`
	StopSequences []string        `json:"stop_sequences"`
}

func NewServer(cfg Config) (*Server, error) {
	cfg = cfg.withRuntimeDefaults()
	if !isValidLogLevel(cfg.LogLevel) {
		return nil, fmt.Errorf("invalid log level: %s", cfg.LogLevel)
	}
	if cfg.GatewayTimeout <= 0 || cfg.AuthTimeout <= 0 || cfg.RequestReadTimeout <= 0 || cfg.RefreshSkew < 0 {
		return nil, fmt.Errorf("invalid timeout configuration")
	}
	if cfg.MaxRequestBodyBytes <= 0 || cfg.MaxResponseBodyBytes <= 0 || cfg.MaxDebugCaptureBytes <= 0 || cfg.MaxStreamEventBytes <= 0 ||
		cfg.SignatureTTL <= 0 || cfg.SignatureMaxSessions <= 0 || cfg.SignatureMaxEntries <= 0 {
		return nil, fmt.Errorf("invalid resource limit configuration")
	}
	s := &Server{
		cfg:           cfg,
		tokens:        newTokenProvider(cfg),
		gateway:       newHTTPClient(cfg.GatewayTimeout, cfg.InsecureSkipTLSVerify, false),
		streamGateway: newHTTPClient(cfg.GatewayTimeout, cfg.InsecureSkipTLSVerify, true),
		logger:        NewLogger(cfg.LogLevel, cfg.Verbose),
		signatures:    newSignatureStore(cfg.SignatureTTL, cfg.SignatureMaxSessions, cfg.SignatureMaxEntries),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.healthz)
	mux.HandleFunc("/v1/messages", s.messages)
	mux.HandleFunc("/v1/messages/count_tokens", s.countTokens)
	s.handler = loggingMiddleware(adapterAuthMiddleware(mux, cfg.AdapterAPIKey), s.logger)
	if cfg.InsecureSkipTLSVerify {
		s.logger.Warnf("TLS certificate verification is disabled by explicit configuration")
	}
	return s, nil
}

func (s *Server) Handler() http.Handler { return s.handler }

func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (s *Server) messages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAnthropicError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	ctx := r.Context()
	requestID := atomic.AddUint64(&s.reqID, 1)
	startedAt := time.Now()

	body, err := readRequestBody(w, r, s.cfg.MaxRequestBodyBytes)
	if err != nil {
		s.logger.Warnf("request_id=%d failed to read body: %v", requestID, err)
		if err == errBodyTooLarge {
			writeAnthropicError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", err.Error())
		} else {
			writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "failed to read body")
		}
		return
	}

	var req messagesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		s.logger.Warnf("request_id=%d invalid json body: %v", requestID, err)
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON body")
		return
	}

	s.logger.Debugf("request_id=%d inbound_headers=%s", requestID, formatHeaders(r.Header))
	s.logger.Debugf("request_id=%d inbound_messages_request_json=\n%s", requestID, debugJSON(body, s.cfg.MaxDebugCaptureBytes))

	sessionID := r.Header.Get(claudeCodeSessionHeader)
	rewrittenBody, targetModel, stream, op, err := s.rewriteRequestForVertex(body, req, sessionID)
	if err != nil {
		s.logger.Warnf("request_id=%d failed to rewrite request: %v", requestID, err)
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	s.logger.Infof("request_id=%d path=%s method=%s stream=%t model=%s", requestID, r.URL.Path, r.Method, stream, targetModel)
	s.logger.Debugf("request_id=%d rewritten_vertex_request_json=\n%s", requestID, debugJSON(rewrittenBody, s.cfg.MaxDebugCaptureBytes))

	token, err := s.tokens.GetBearerToken(ctx)
	if err != nil {
		s.logger.Errorf("request_id=%d token retrieval failed: %v", requestID, err)
		writeAnthropicError(w, http.StatusBadGateway, "api_error", fmt.Sprintf("get token failed: %v", err))
		return
	}

	targetURL := s.cfg.GatewayBaseURL + s.cfg.targetPath(op, targetModel)
	resp, err := s.forward(ctx, rewrittenBody, token, op, targetModel)
	if err != nil {
		s.logger.Errorf("request_id=%d upstream request failed target=%s err=%v", requestID, targetURL, err)
		writeAnthropicError(w, http.StatusBadGateway, "api_error", fmt.Sprintf("upstream failed: %v", err))
		return
	}
	defer resp.Body.Close()

	if s.cfg.ForceRefreshOn4x && (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) {
		s.logger.Warnf("request_id=%d upstream status=%d; forcing token refresh and retry", requestID, resp.StatusCode)
		newToken, ferr := s.tokens.RefreshAfterRejection(ctx, token)
		if ferr == nil {
			resp.Body.Close()
			resp, err = s.forward(ctx, rewrittenBody, newToken, op, targetModel)
			if err != nil {
				s.logger.Errorf("request_id=%d retry upstream failed target=%s err=%v", requestID, targetURL, err)
				writeAnthropicError(w, http.StatusBadGateway, "api_error", fmt.Sprintf("retry upstream failed: %v", err))
				return
			}
			defer resp.Body.Close()
		} else {
			s.logger.Errorf("request_id=%d force token refresh failed: %v", requestID, ferr)
		}
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, readErr := readResponseBody(resp.Body, s.cfg.MaxResponseBodyBytes)
		if readErr != nil {
			writeAnthropicError(w, http.StatusBadGateway, "api_error", fmt.Sprintf("failed to read upstream error: %v", readErr))
			return
		}
		errorType, message := upstreamErrorToAnthropic(respBody, resp.StatusCode)
		writeAnthropicError(w, resp.StatusCode, errorType, message)
		return
	}

	if stream {
		copyHeaders(w.Header(), resp.Header)
		if s.cfg.VertexAPIFormat == "gemini" {
			w.Header().Set("Content-Type", "text/event-stream")
		}
		w.WriteHeader(resp.StatusCode)
		var captured string
		var copied int
		var streamErr error
		if s.cfg.VertexAPIFormat == "gemini" {
			captured, copied, streamErr = s.streamGeminiAsAnthropic(w, resp.Body, sessionID, req.StopSequences, targetModel)
		} else {
			captured, copied, streamErr = streamCopyAndCapture(w, resp.Body, s.cfg.MaxDebugCaptureBytes)
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
		if streamErr != nil {
			s.logger.Errorf("request_id=%d stream conversion failed: %v", requestID, streamErr)
			if s.cfg.VertexAPIFormat != "gemini" {
				writeSSEError(w, streamErr)
			}
		}
		s.logger.Debugf("request_id=%d upstream_stream_response=\n%s", requestID, captured)
		return
	}

	respBody, readErr := readResponseBody(resp.Body, s.cfg.MaxResponseBodyBytes)
	if readErr != nil {
		writeAnthropicError(w, http.StatusBadGateway, "api_error", fmt.Sprintf("failed to read upstream response: %v", readErr))
		return
	}
	convertedGemini := false
	if s.cfg.VertexAPIFormat == "gemini" && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		converted, signatures, err := geminiResponseToAnthropicWithDefaults(respBody, req.StopSequences, targetModel)
		if err != nil {
			s.logger.Warnf("request_id=%d failed to convert Gemini response: %v", requestID, err)
			writeAnthropicError(w, http.StatusBadGateway, "api_error", "invalid Gemini response: "+err.Error())
			return
		}
		s.signatures.remember(sessionID, signatures)
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
	s.logger.Debugf("request_id=%d upstream_messages_response_json=\n%s", requestID, debugJSON(respBody, s.cfg.MaxDebugCaptureBytes))
}

func (s *Server) rewriteRequestForVertex(body []byte, parsed messagesRequest, sessionID string) ([]byte, string, bool, vertexOperation, error) {
	var payload map[string]interface{}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, "", false, "", err
	}

	targetModel := parsed.Model
	if targetModel == "" || s.cfg.ModelOverride {
		targetModel = s.cfg.Model
	}
	if err := validateVertexResourceSegment("model", targetModel); err != nil {
		return nil, "", false, "", err
	}
	if s.cfg.VertexAPIFormat == "gemini" {
		if err := validateGeminiModel(targetModel); err != nil {
			return nil, "", false, "", err
		}
	}
	if s.cfg.VertexAPIFormat == "gemini" {
		rewritten, err := anthropicMessagesToGeminiForModel(body, s.signatures.snapshot(sessionID), targetModel)
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

	client := s.gateway
	if op == operationStreamRawPredict || op == operationStreamGenerateContent {
		client = s.streamGateway
	}
	return client.Do(req)
}

func (s *Server) countTokens(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAnthropicError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	requestID := atomic.AddUint64(&s.reqID, 1)
	if s.cfg.VertexAPIFormat != "gemini" {
		writeAnthropicError(w, http.StatusNotImplemented, "invalid_request_error", "count_tokens is only implemented for VERTEX_API_FORMAT=gemini")
		return
	}
	body, err := readRequestBody(w, r, s.cfg.MaxRequestBodyBytes)
	if err != nil {
		s.logger.Warnf("request_id=%d failed to read count_tokens body: %v", requestID, err)
		if err == errBodyTooLarge {
			writeAnthropicError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", err.Error())
		} else {
			writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "failed to read body")
		}
		return
	}
	var req messagesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		s.logger.Warnf("request_id=%d invalid count_tokens json body: %v", requestID, err)
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON body")
		return
	}
	s.logger.Debugf("request_id=%d inbound_headers=%s", requestID, formatHeaders(r.Header))
	s.logger.Debugf("request_id=%d inbound_count_tokens_request_json=\n%s", requestID, debugJSON(body, s.cfg.MaxDebugCaptureBytes))
	targetModel := req.Model
	if targetModel == "" || s.cfg.ModelOverride {
		targetModel = s.cfg.Model
	}
	if err := validateVertexResourceSegment("model", targetModel); err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if err := validateGeminiModel(targetModel); err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	sessionID := r.Header.Get(claudeCodeSessionHeader)
	rewritten, err := anthropicMessagesToGeminiForModel(body, s.signatures.snapshot(sessionID), targetModel)
	if err != nil {
		s.logger.Warnf("request_id=%d failed to rewrite count_tokens request: %v", requestID, err)
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	token, err := s.tokens.GetBearerToken(r.Context())
	if err != nil {
		s.logger.Errorf("request_id=%d token retrieval failed: %v", requestID, err)
		writeAnthropicError(w, http.StatusBadGateway, "api_error", fmt.Sprintf("get token failed: %v", err))
		return
	}
	resp, err := s.forward(r.Context(), rewritten, token, operationCountTokens, targetModel)
	if err != nil {
		s.logger.Errorf("request_id=%d upstream count_tokens failed: %v", requestID, err)
		writeAnthropicError(w, http.StatusBadGateway, "api_error", fmt.Sprintf("upstream failed: %v", err))
		return
	}
	defer resp.Body.Close()
	if s.cfg.ForceRefreshOn4x && (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) {
		if newToken, refreshErr := s.tokens.RefreshAfterRejection(r.Context(), token); refreshErr == nil {
			resp.Body.Close()
			resp, err = s.forward(r.Context(), rewritten, newToken, operationCountTokens, targetModel)
			if err != nil {
				writeAnthropicError(w, http.StatusBadGateway, "api_error", fmt.Sprintf("retry upstream failed: %v", err))
				return
			}
			defer resp.Body.Close()
		}
	}
	respBody, readErr := readResponseBody(resp.Body, s.cfg.MaxResponseBodyBytes)
	if readErr != nil {
		writeAnthropicError(w, http.StatusBadGateway, "api_error", fmt.Sprintf("failed to read upstream response: %v", readErr))
		return
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		errorType, message := upstreamErrorToAnthropic(respBody, resp.StatusCode)
		writeAnthropicError(w, resp.StatusCode, errorType, message)
		return
	}
	copyHeaders(w.Header(), resp.Header)
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		converted, err := geminiCountTokensToAnthropic(respBody)
		if err != nil {
			s.logger.Warnf("request_id=%d failed to convert Gemini count_tokens response: %v", requestID, err)
			writeAnthropicError(w, http.StatusBadGateway, "api_error", "invalid Gemini count_tokens response: "+err.Error())
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

func streamCopyAndCapture(w http.ResponseWriter, r io.Reader, captureLimit int64) (string, int, error) {
	flusher, ok := w.(http.Flusher)
	captured := newCappedBuffer(captureLimit)
	copied := 0
	if !ok {
		n, err := io.Copy(io.MultiWriter(w, captured), r)
		return captured.String(), int(n), err
	}
	buf := make([]byte, 32*1024)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			written, writeErr := w.Write(chunk)
			_, _ = captured.Write(chunk[:written])
			copied += written
			flusher.Flush()
			if writeErr != nil {
				return captured.String(), copied, writeErr
			}
			if written != len(chunk) {
				return captured.String(), copied, io.ErrShortWrite
			}
		}
		if err != nil {
			if err == io.EOF {
				err = nil
			}
			return captured.String(), copied, err
		}
	}
}

func copyHeaders(dst, src http.Header) {
	for k, vs := range src {
		lk := strings.ToLower(k)
		if lk == "content-length" || lk == "connection" || lk == "transfer-encoding" || lk == "content-md5" || lk == "digest" || lk == "etag" ||
			lk == "keep-alive" || lk == "proxy-authenticate" || lk == "proxy-authorization" || lk == "te" || lk == "trailer" || lk == "upgrade" {
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

func debugJSON(data []byte, limit int64) string {
	if int64(len(data)) <= limit {
		return prettyJSONOrRaw(data)
	}
	return string(data[:limit]) + "\n[debug capture truncated]"
}

func formatHeaders(h http.Header) string {
	parts := make([]string, 0, len(h))
	for k, values := range h {
		if isSensitiveHeader(k) {
			parts = append(parts, k+": [REDACTED]")
			continue
		}
		parts = append(parts, fmt.Sprintf("%s: %s", k, strings.Join(values, ",")))
	}
	return strings.Join(parts, " | ")
}

func isSensitiveHeader(name string) bool {
	switch strings.ToLower(name) {
	case "authorization", "proxy-authorization", "x-api-key", "cookie", "set-cookie":
		return true
	default:
		return false
	}
}

func adapterAuthMiddleware(next http.Handler, apiKey string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if apiKey == "" || r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		provided := r.Header.Get("x-api-key")
		if provided == "" {
			provided = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		}
		if len(provided) != len(apiKey) || subtle.ConstantTimeCompare([]byte(provided), []byte(apiKey)) != 1 {
			writeAnthropicError(w, http.StatusUnauthorized, "authentication_error", "invalid adapter API key")
			return
		}
		next.ServeHTTP(w, r)
	})
}
