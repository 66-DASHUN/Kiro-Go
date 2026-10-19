package proxy

import (
	"fmt"
	refkiro "kiro-proxy/internal/sub2apiref/kiro"
	"net/http"
)

func init() {
	// The original image estimator accepts URL sources, including ones found in
	// nested message data. Apply the same destination policy as image forwarding.
	refkiro.ConfigureRemoteImageTransport(exactImageTransport{}, checkRemoteImageRedirect)
}

type exactImageTransport struct{}

func (exactImageTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.URL == nil || (request.URL.Scheme != "http" && request.URL.Scheme != "https") || request.URL.Hostname() == "" || request.URL.User != nil {
		return nil, fmt.Errorf("image URL must be a public HTTP or HTTPS URL without credentials")
	}
	// A fresh transport prevents sharing environment proxies or unguarded pools.
	// DisableKeepAlives closes the connection after its response body is consumed.
	transport := newGuardedRemoteImageTransport()
	transport.DisableKeepAlives = true
	defer transport.CloseIdleConnections()
	return transport.RoundTrip(request)
}

func checkRemoteImageRedirect(request *http.Request, via []*http.Request) error {
	if len(via) >= 5 || (request.URL.Scheme != "http" && request.URL.Scheme != "https") || request.URL.User != nil {
		return fmt.Errorf("unsupported image redirect")
	}
	return nil
}
