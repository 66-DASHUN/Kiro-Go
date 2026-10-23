package proxy

import (
	"bytes"
	"io"
	"net/http"

	"kiro-proxy/config"
)

type probeReplayBody struct {
	io.Reader
	io.Closer
}

func (h *Handler) forwardCCTestProbe(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodPost || (r.URL.Path != "/v1/messages" && r.URL.Path != "/messages" && r.URL.Path != "/anthropic/v1/messages") {
		return false
	}
	entry := config.GetApiKeyEntry(apiKeyIDFromContext(r.Context()))
	if entry == nil || !entry.Enabled || !entry.CCTestTextHandling() || r.Body == nil || r.ContentLength > maxCCTestProbeBytes {
		return false
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxCCTestProbeBytes+1))
	if err != nil {
		h.sendClaudeError(w, http.StatusBadRequest, "invalid_request_error", "Unable to read request body")
		return true
	}
	// Preserve the original closer and unread tail for chunked/oversized bodies.
	r.Body = &probeReplayBody{Reader: io.MultiReader(bytes.NewReader(body), r.Body), Closer: r.Body}
	kind := inspectCCTestProbe(body)
	if kind == "" {
		return false
	}
	c := config.GetCCTestProxyConfig()
	return h.forwardProbeUpstream(w, r, entry, c, kind)
}
