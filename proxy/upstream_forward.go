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

var dedicatedUpstreamTransport = func() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.ResponseHeaderTimeout = 90 * time.Second
	t.DisableCompression = true
	return t
}()

func upstreamAPIPath(path string) string {
	path = strings.TrimPrefix(path, "/anthropic")
	if !strings.HasPrefix(path, "/v1/") {
		path = "/v1" + path
	}
	if path == "/v1/codex-models" || path == "/v1/codex/models" {
		return "/v1/models"
	}
	return path
}

func isDedicatedClaudePath(path string) bool {
	switch path {
	case "/v1/messages", "/messages", "/anthropic/v1/messages", "/v1/messages/count_tokens", "/messages/count_tokens":
		return true
	}
	return false
}

func sendDedicatedUpstreamConfigError(h *Handler, w http.ResponseWriter, r *http.Request, message string) {
	if isDedicatedClaudePath(r.URL.Path) {
		h.sendClaudeError(w, http.StatusBadGateway, "api_error", message)
		return
	}
	h.sendOpenAIError(w, http.StatusBadGateway, "api_error", message)
}

// forwardDedicatedUpstream runs before local decoding, mapping, cache emulation,
// and account reservations. This is a route per key, never a detector heuristic.
func (h *Handler) forwardDedicatedUpstream(w http.ResponseWriter, r *http.Request) bool {
	entry := config.GetApiKeyEntry(apiKeyIDFromContext(r.Context()))
	if entry == nil || entry.UpstreamForward == nil || !entry.UpstreamForward.Enabled {
		return false
	}
	if entry.CCTestTextHandling() {
		sendDedicatedUpstreamConfigError(h, w, r, "Conflicting probe-only and full-key forwarding settings")
		return true
	}
	return h.forwardConfiguredUpstream(w, r, entry, entry.UpstreamForward, "")
}

// Both forwarding policies share the same byte-preserving transport. A probe
// match changes the route label, never the upstream response or usage values.
func (h *Handler) forwardConfiguredUpstream(w http.ResponseWriter, r *http.Request, entry *config.ApiKeyEntry, c *config.UpstreamForwardConfig, probeKind string) bool {
	route, label := "dedicated_upstream", "Dedicated upstream"
	if probeKind != "" {
		route, label = "cctest_upstream", "Probe upstream"
	}
	if err := config.ValidateUpstreamForward(c); err != nil {
		sendDedicatedUpstreamConfigError(h, w, r, "Invalid dedicated upstream configuration")
		return true
	}
	target, _ := url.Parse(c.BaseURL)
	// Also catches a loop through the public reverse proxy for this request host.
	inHost := r.Host
	if u, err := url.Parse("https://" + inHost); err == nil && strings.EqualFold(strings.TrimSuffix(u.Host, ":443"), strings.TrimSuffix(target.Host, ":443")) {
		sendDedicatedUpstreamConfigError(h, w, r, "Dedicated upstream must not point to this Kiro service")
		return true
	}
	path := upstreamAPIPath(r.URL.Path)
	base := strings.TrimSuffix(target.Path, "/")
	if strings.HasSuffix(base, "/v1") {
		path = strings.TrimPrefix(path, "/v1")
	}
	target.Path = base + path
	target.RawPath = ""
	target.RawQuery = r.URL.RawQuery
	transport := http.RoundTripper(dedicatedUpstreamTransport)
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
			// Use the incoming endpoint: the configured upstream base path may
			// itself contain "messages" even when this is an OpenAI request.
			if isDedicatedClaudePath(r.URL.Path) {
				h.sendClaudeError(w, http.StatusBadGateway, "api_error", "Dedicated upstream connection failed")
			} else {
				h.sendOpenAIError(w, http.StatusBadGateway, "api_error", "Dedicated upstream connection failed")
			}
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
