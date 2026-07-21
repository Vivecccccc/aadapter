//go:build !darwin

package adapter

import "net/http"

func insecureTLSVerificationTransport() http.RoundTripper {
	return nil
}
