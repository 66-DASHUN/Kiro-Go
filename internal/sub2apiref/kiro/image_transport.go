// Transport integration facade. The pinned image counter and its token math
// remain unchanged; the host application supplies its outbound-network policy.
package kiro

import "net/http"

// ConfigureRemoteImageTransport must be called once at application startup,
// before any image counting or payload translation can run. Preserve the
// reference timeout and all other client settings while applying host guards.
func ConfigureRemoteImageTransport(transport http.RoundTripper, redirects func(*http.Request, []*http.Request) error) {
	client := *kiroRemoteImageHTTPClient
	client.Transport = transport
	client.CheckRedirect = redirects
	kiroRemoteImageHTTPClient = &client
}
