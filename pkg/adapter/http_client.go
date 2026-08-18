package adapter

import (
	"crypto/tls"
	"net/http"
	"time"
)

func newHTTPClient(timeout time.Duration, insecureSkipTLSVerify bool, streaming bool) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = timeout
	if insecureSkipTLSVerify {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} // #nosec G402 -- explicit opt-in for private gateways.
	}
	client := &http.Client{Transport: transport}
	if !streaming {
		client.Timeout = timeout
	}
	return client
}
