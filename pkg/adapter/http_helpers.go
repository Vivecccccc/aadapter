package adapter

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

var errBodyTooLarge = errors.New("body exceeds configured size limit")

func readRequestBody(w http.ResponseWriter, r *http.Request, limit int64) ([]byte, error) {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return nil, errBodyTooLarge
		}
		return nil, err
	}
	return body, nil
}

func readResponseBody(r io.Reader, limit int64) ([]byte, error) {
	limited := io.LimitReader(r, limit+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, errBodyTooLarge
	}
	return body, nil
}

func writeAnthropicError(w http.ResponseWriter, status int, errorType, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"type":  "error",
		"error": map[string]interface{}{"type": errorType, "message": message},
	})
}

func writeSSEError(w http.ResponseWriter, err error) {
	payload, marshalErr := json.Marshal(map[string]interface{}{
		"type": "error", "error": map[string]interface{}{"type": "api_error", "message": err.Error()},
	})
	if marshalErr != nil {
		return
	}
	_, _ = fmt.Fprintf(w, "event: error\ndata: %s\n\n", payload)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

func upstreamErrorToAnthropic(body []byte, status int) (string, string) {
	errorType := "api_error"
	switch status {
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		errorType = "invalid_request_error"
	case http.StatusUnauthorized:
		errorType = "authentication_error"
	case http.StatusForbidden:
		errorType = "permission_error"
	case http.StatusNotFound:
		errorType = "not_found_error"
	case http.StatusTooManyRequests:
		errorType = "rate_limit_error"
	}
	message := http.StatusText(status)
	var parsed struct {
		Error struct {
			Message string `json:"message"`
			Status  string `json:"status"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &parsed) == nil && parsed.Error.Message != "" {
		message = parsed.Error.Message
	}
	if message == "" {
		message = fmt.Sprintf("upstream returned HTTP %d", status)
	}
	return errorType, message
}

type cappedBuffer struct {
	buf       bytes.Buffer
	remaining int64
	truncated bool
}

func newCappedBuffer(limit int64) *cappedBuffer { return &cappedBuffer{remaining: limit} }

func (b *cappedBuffer) Write(p []byte) (int, error) {
	originalLen := len(p)
	if int64(len(p)) > b.remaining {
		p = p[:b.remaining]
		b.truncated = true
	}
	if len(p) > 0 {
		_, _ = b.buf.Write(p)
		b.remaining -= int64(len(p))
	}
	return originalLen, nil
}

func (b *cappedBuffer) String() string {
	if b.truncated {
		return b.buf.String() + "\n[debug capture truncated]"
	}
	return b.buf.String()
}
