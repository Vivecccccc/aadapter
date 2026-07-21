//go:build darwin

package adapter

import (
	"crypto/tls"
	"net/http"
)

func insecureTLSVerificationTransport() http.RoundTripper {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	return transport
}
