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
	"time"
)

type Server struct {
	cfg     Config
	tokens  *tokenProvider
	gateway *http.Client
	handler http.Handler
}

type messagesRequest struct {
	Model    string          `json:"model"`
	Stream   bool            `json:"stream"`
	Messages json.RawMessage `json:"messages"`
}

func NewServer(cfg Config) (*Server, error) {
	s := &Server{
		cfg:     cfg,
		tokens:  newTokenProvider(cfg),
		gateway: &http.Client{Timeout: cfg.GatewayTimeout},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.healthz)
	mux.HandleFunc("/v1/messages", s.messages)
	s.handler = loggingMiddleware(mux)
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

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "failed to read body", http.StatusBadRequest)
		return
	}

	var req messagesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "invalid json body", http.StatusBadRequest)
		return
	}

	token, err := s.tokens.GetBearerToken(ctx)
	if err != nil {
		http.Error(w, fmt.Sprintf("get token failed: %v", err), http.StatusBadGateway)
		return
	}

	resp, err := s.forward(ctx, body, token, req.Stream)
	if err != nil {
		http.Error(w, fmt.Sprintf("upstream failed: %v", err), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if s.cfg.ForceRefreshOn4x && (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) {
		newToken, ferr := s.tokens.ForceRefresh(context.Background())
		if ferr == nil {
			resp.Body.Close()
			resp, err = s.forward(ctx, body, newToken, req.Stream)
			if err != nil {
				http.Error(w, fmt.Sprintf("retry upstream failed: %v", err), http.StatusBadGateway)
				return
			}
			defer resp.Body.Close()
		}
	}

	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	if req.Stream {
		streamCopy(w, resp.Body)
		return
	}
	_, _ = io.Copy(w, resp.Body)
}

func (s *Server) forward(ctx context.Context, body []byte, bearer string, stream bool) (*http.Response, error) {
	endpoint, err := url.Parse(s.cfg.GatewayBaseURL + s.cfg.targetPath(stream))
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

func streamCopy(w http.ResponseWriter, r io.Reader) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		_, _ = io.Copy(w, r)
		return
	}
	buf := make([]byte, 32*1024)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			_, _ = w.Write(buf[:n])
			flusher.Flush()
		}
		if err != nil {
			return
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

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		_ = start
	})
}
