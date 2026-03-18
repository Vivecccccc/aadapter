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
	"sync/atomic"
	"time"
)

type Server struct {
	cfg     Config
	tokens  *tokenProvider
	gateway *http.Client
	handler http.Handler
	logger  *Logger
	reqID   uint64
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
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.healthz)
	mux.HandleFunc("/v1/messages", s.messages)
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

	rewrittenBody, targetModel, stream, err := s.rewriteRequestForVertex(body, r.Header, req)
	if err != nil {
		s.logger.Warnf("request_id=%d failed to rewrite request: %v", requestID, err)
		http.Error(w, "invalid messages request", http.StatusBadRequest)
		return
	}

	s.logger.Infof("request_id=%d path=%s method=%s stream=%t model=%s", requestID, r.URL.Path, r.Method, stream, targetModel)
	s.logger.Debugf("request_id=%d inbound_headers=%s", requestID, formatHeaders(r.Header))
	s.logger.Debugf("request_id=%d inbound_messages_request_json=\n%s", requestID, prettyJSONOrRaw(body))
	s.logger.Debugf("request_id=%d rewritten_vertex_request_json=\n%s", requestID, prettyJSONOrRaw(rewrittenBody))

	token, err := s.tokens.GetBearerToken(ctx)
	if err != nil {
		s.logger.Errorf("request_id=%d token retrieval failed: %v", requestID, err)
		http.Error(w, fmt.Sprintf("get token failed: %v", err), http.StatusBadGateway)
		return
	}

	targetURL := s.cfg.GatewayBaseURL + s.cfg.targetPath(stream, targetModel)
	resp, err := s.forward(ctx, rewrittenBody, token, stream, targetModel)
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
			resp, err = s.forward(ctx, rewrittenBody, newToken, stream, targetModel)
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

	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)

	if stream {
		captured, copied := streamCopyAndCapture(w, resp.Body)
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

func (s *Server) rewriteRequestForVertex(body []byte, headers http.Header, parsed messagesRequest) ([]byte, string, bool, error) {
	var payload map[string]interface{}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, "", false, err
	}

	targetModel := parsed.Model
	if targetModel == "" || s.cfg.ModelOverride {
		targetModel = s.cfg.Model
	}
	delete(payload, "model")

	if _, exists := payload["anthropic_version"]; !exists {
		if headerVersion := headers.Get("anthropic-version"); headerVersion != "" {
			payload["anthropic_version"] = toVertexAnthropicVersion(headerVersion)
		} else {
			payload["anthropic_version"] = s.cfg.AnthropicVersion
		}
	}

	rewritten, err := json.Marshal(payload)
	if err != nil {
		return nil, "", false, err
	}
	return rewritten, targetModel, parsed.Stream, nil
}

func toVertexAnthropicVersion(v string) string {
	trimmed := strings.TrimSpace(v)
	if trimmed == "" {
		return ""
	}
	if strings.HasPrefix(strings.ToLower(trimmed), "vertex-") {
		return trimmed
	}
	return "vertex-" + trimmed
}

func (s *Server) forward(ctx context.Context, body []byte, bearer string, stream bool, model string) (*http.Response, error) {
	endpoint, err := url.Parse(s.cfg.GatewayBaseURL + s.cfg.targetPath(stream, model))
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
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	}

	return s.gateway.Do(req)
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
