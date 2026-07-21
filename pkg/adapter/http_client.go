package adapter

import (
	"net/http"
	"time"
)

func newHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout:   timeout,
		Transport: insecureTLSVerificationTransport(),
	}
}
