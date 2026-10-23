package proxy

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"kiro-proxy/config"
	"kiro-proxy/logger"
)

var probeUpstreamTransport = func() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.ResponseHeaderTimeout = 90 * time.Second
	t.DisableCompression = true
	return t
}()

// Only forwardCCTestProbe calls this transport, after a successful fingerprint
// match. There is no per-key full-forwarding route or destination configuration.
func (h *Handler) forwardProbeUpstream(w http.ResponseWriter, r *http.Request, entry *config.ApiKeyEntry, c config.CCTestProxyConfig, probeKind string) bool {
	if probeKind == "" {
		return false
	}
	const route, label = "cctest_upstream", "Probe upstream"
	if err := config.ValidateCCTestProxyConfig(c, true); err != nil {
		h.sendClaudeError(w, http.StatusBadGateway, "api_error", "Invalid probe upstream configuration")
		return true
	}
	target, _ := url.Parse(c.BaseURL)
	// Also catches a loop through the public reverse proxy for this request host.
	inHost := r.Host
	if u, err := url.Parse("https://" + inHost); err == nil && strings.EqualFold(strings.TrimSuffix(u.Host, ":443"), strings.TrimSuffix(target.Host, ":443")) {
		h.sendClaudeError(w, http.StatusBadGateway, "api_error", "Probe upstream must not point to this Kiro service")
		return true
	}
	path := "/v1/messages"
	base := strings.TrimSuffix(target.Path, "/")
	if strings.HasSuffix(base, "/v1") {
		path = strings.TrimPrefix(path, "/v1")
	}
	target.Path = base + path
	target.RawPath = ""
	target.RawQuery = r.URL.RawQuery
	transport := http.RoundTripper(probeUpstreamTransport)
	if h.upstreamClient != nil && h.upstreamClient.Transport != nil {
		transport = h.upstreamClient.Transport
	}
	var usage *upstreamUsageReader
	status := http.StatusBadGateway
	if isUpstreamInference(r.URL.Path, r.Method) {
		// Forwarding bypasses the local handlers that normally initialize and
		// emit diagnostics. Create the trace before applying upstream usage.
		diagnostic := usageDiagnostic{Route: route}
		if probeKind != "" {
			diagnostic.ProbeKind, diagnostic.ProbeRule = probeKind, cctestProbeRule
		}
		setRequestUsageDiagnostic(r.Context(), diagnostic)
		defer emitRequestUsageDiagnostic(r.Context())
		defer func() {
			if usage == nil {
				usage = &upstreamUsageReader{}
			}
			total := usage.input + usage.output + usage.creation + usage.read
			if err := config.RecordApiKeyForwardedUsage(entry.ID, total); err != nil {
				logger.Warnf("[Forward] Failed to persist upstream usage")
			}
			success := status >= 200 && status < 300 && !usage.readError && r.Context().Err() == nil
			message := label
			if !success {
				message = fmt.Sprintf("%s HTTP %d or interrupted response", label, status)
			}
			updateRequestUsageDiagnostic(r.Context(), func(d *usageDiagnostic) {
				d.Route = route
				d.Model = usage.model
				d.ReportedInputTokens = int(usage.input)
				d.ReportedOutputTokens = int(usage.output)
				d.CacheReadInputTokens = int(usage.read)
				d.CacheCreationInputTokens = int(usage.creation)
				d.CacheUsageSource = "upstream"
				d.CacheUsageEnabled = false
				d.InputTokenSource = "upstream"
				d.OutputTokenSource = "upstream"
				if !usage.seen {
					d.InputTokenSource = "upstream_unreported"
					d.OutputTokenSource = "upstream_unreported"
				}
			})
			recordFinalRequestWithAPIKey(r.Context(), entry.ID, entry.Key, nil, usage.model, int(usage.input+usage.creation+usage.read), int(usage.output), 0, success, status, message)
		}()
	}
	proxy := &httputil.ReverseProxy{
		Transport:     transport,
		FlushInterval: -1,
		// Transport errors can contain the URL; do not send or log secrets.
		ErrorLog: log.New(io.Discard, "", 0),
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			h.sendClaudeError(w, http.StatusBadGateway, "api_error", "Probe upstream connection failed")
		},
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL = target
			pr.Out.Host = target.Host
			originalHeaders := pr.Out.Header // Already stripped of hop-by-hop headers.
			pr.Out.Header = make(http.Header)
			for name, values := range originalHeaders {
				n := strings.ToLower(name)
				if n == "content-type" || n == "accept" || n == "user-agent" || n == "openai-beta" || strings.HasPrefix(n, "anthropic-") || strings.HasPrefix(n, "x-stainless-") {
					pr.Out.Header[name] = append([]string(nil), values...)
				}
			}
			pr.Out.Header.Set("Accept-Encoding", "identity")
			pr.Out.Header.Set("Authorization", "Bearer "+c.APIKey)
			if strings.Contains(path, "/messages") {
				pr.Out.Header.Set("X-Api-Key", c.APIKey)
			}
		},
		ModifyResponse: func(res *http.Response) error {
			status = res.StatusCode
			usage = &upstreamUsageReader{ReadCloser: res.Body, stream: strings.HasPrefix(res.Header.Get("Content-Type"), "text/event-stream")}
			res.Body = usage
			// ReverseProxy adds response headers; replace local trace headers so
			// clients see the actual upstream request ID exactly once.
			for name := range res.Header {
				w.Header().Del(name)
			}
			res.Header.Del("Set-Cookie")
			if probeKind != "" {
				res.Header.Set("X-Kiro-Route", route)
				res.Header.Set("X-Kiro-Probe-Rule", cctestProbeRule)
			}
			return nil
		},
	}
	proxy.ServeHTTP(w, r)
	return true
}
