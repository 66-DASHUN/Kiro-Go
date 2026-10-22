package proxy

import (
	"encoding/json"
	"net/http"

	"kiro-proxy/config"
)

type apiKeyView struct {
	UpstreamForward                   *apiKeyUpstreamView `json:"upstreamForward,omitempty"`
	PromptCacheEmulationEnabled       *bool               `json:"promptCacheEmulationEnabled"`
	PromptCacheEmulationMode          *string             `json:"promptCacheEmulationMode"`
	PromptCacheEmulationRatio         *float64            `json:"promptCacheEmulationRatio"`
	PromptCacheCreationEmulationRatio *float64            `json:"promptCacheCreationEmulationRatio"`
	PromptCacheReadEmulationRatio     *float64            `json:"promptCacheReadEmulationRatio"`
	AccountCount                      int                 `json:"accountCount"`
	ID                                string              `json:"id"`
	Name                              string              `json:"name,omitempty"`
	Key                               string              `json:"key,omitempty"`
	KeyMasked                         string              `json:"keyMasked"`
	Enabled                           bool                `json:"enabled"`
	CreatedAt                         int64               `json:"createdAt"`
	LastUsedAt                        int64               `json:"lastUsedAt,omitempty"`
	TokenLimit                        int64               `json:"tokenLimit,omitempty"`
	CreditLimit                       float64             `json:"creditLimit,omitempty"`
	TokensUsed                        int64               `json:"tokensUsed"`
	CreditsUsed                       float64             `json:"creditsUsed"`
	RequestsCount                     int64               `json:"requestsCount"`
}

func toApiKeyView(entry config.ApiKeyEntry) apiKeyView {
	count := 0
	for _, account := range config.GetAccounts() {
		if account.ApiKeyID == entry.ID {
			count++
		}
	}
	return apiKeyView{
		UpstreamForward:                   upstreamView(entry.UpstreamForward),
		AccountCount:                      count,
		PromptCacheEmulationEnabled:       entry.PromptCacheEmulationEnabled,
		PromptCacheEmulationMode:          entry.PromptCacheEmulationMode,
		PromptCacheEmulationRatio:         entry.PromptCacheEmulationRatio,
		PromptCacheCreationEmulationRatio: entry.PromptCacheCreationEmulationRatio,
		PromptCacheReadEmulationRatio:     entry.PromptCacheReadEmulationRatio,

		ID:            entry.ID,
		Name:          entry.Name,
		KeyMasked:     config.MaskApiKey(entry.Key),
		Enabled:       entry.Enabled,
		CreatedAt:     entry.CreatedAt,
		LastUsedAt:    entry.LastUsedAt,
		TokenLimit:    entry.TokenLimit,
		CreditLimit:   entry.CreditLimit,
		TokensUsed:    entry.TokensUsed,
		CreditsUsed:   entry.CreditsUsed,
		RequestsCount: entry.RequestsCount,
	}
}

func toApiKeyDetailView(entry config.ApiKeyEntry) apiKeyView {
	view := toApiKeyView(entry)
	view.Key = entry.Key
	return view
}

func (h *Handler) apiListApiKeys(w http.ResponseWriter, _ *http.Request) {
	entries := config.ListApiKeys()
	out := make([]apiKeyView, len(entries))
	for i, entry := range entries {
		out[i] = toApiKeyView(entry)
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"apiKeys": out})
}

func (h *Handler) apiGetApiKey(w http.ResponseWriter, _ *http.Request, id string) {
	entry := config.GetApiKeyEntry(id)
	if entry == nil {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"error": "API key not found"})
		return
	}
	json.NewEncoder(w).Encode(toApiKeyDetailView(*entry))
}

type apiKeyCreateRequest struct {
	UpstreamForward                   *apiKeyUpstreamRequest `json:"upstreamForward,omitempty"`
	PromptCacheEmulationEnabled       *bool                  `json:"promptCacheEmulationEnabled"`
	PromptCacheEmulationMode          *string                `json:"promptCacheEmulationMode"`
	PromptCacheEmulationRatio         *float64               `json:"promptCacheEmulationRatio"`
	PromptCacheCreationEmulationRatio *float64               `json:"promptCacheCreationEmulationRatio"`
	PromptCacheReadEmulationRatio     *float64               `json:"promptCacheReadEmulationRatio"`

	Name        string  `json:"name,omitempty"`
	Key         string  `json:"key,omitempty"`
	Enabled     *bool   `json:"enabled,omitempty"`
	TokenLimit  int64   `json:"tokenLimit,omitempty"`
	CreditLimit float64 `json:"creditLimit,omitempty"`
}

func (h *Handler) apiCreateApiKey(w http.ResponseWriter, r *http.Request) {
	var req apiKeyCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	keyValue := req.Key
	if keyValue == "" {
		keyValue = config.GenerateApiKeyValue()
	}

	entry, err := config.AddApiKey(config.ApiKeyEntry{
		PromptCacheEmulationEnabled:       req.PromptCacheEmulationEnabled,
		PromptCacheEmulationMode:          req.PromptCacheEmulationMode,
		PromptCacheEmulationRatio:         req.PromptCacheEmulationRatio,
		PromptCacheCreationEmulationRatio: req.PromptCacheCreationEmulationRatio,
		PromptCacheReadEmulationRatio:     req.PromptCacheReadEmulationRatio,

		Name:            req.Name,
		UpstreamForward: mergeUpstreamConfig(nil, req.UpstreamForward),
		Key:             keyValue,
		Enabled:         enabled,
		TokenLimit:      req.TokenLimit,
		CreditLimit:     req.CreditLimit,
	})
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	h.pool.Reload()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"id":      entry.ID,
		"key":     entry.Key,
		"apiKey":  toApiKeyView(entry),
	})
}

type apiKeyUpdateRequest struct {
	UpstreamForward                   *apiKeyUpstreamRequest `json:"upstreamForward,omitempty"`
	ClearPromptCacheOverrides         bool                   `json:"clearPromptCacheOverrides,omitempty"`
	PromptCacheEmulationEnabled       *bool                  `json:"promptCacheEmulationEnabled"`
	PromptCacheEmulationMode          *string                `json:"promptCacheEmulationMode"`
	PromptCacheEmulationRatio         *float64               `json:"promptCacheEmulationRatio"`
	PromptCacheCreationEmulationRatio *float64               `json:"promptCacheCreationEmulationRatio"`
	PromptCacheReadEmulationRatio     *float64               `json:"promptCacheReadEmulationRatio"`

	Name        *string  `json:"name,omitempty"`
	Key         *string  `json:"key,omitempty"`
	Enabled     *bool    `json:"enabled,omitempty"`
	TokenLimit  *int64   `json:"tokenLimit,omitempty"`
	CreditLimit *float64 `json:"creditLimit,omitempty"`
}

func (h *Handler) apiUpdateApiKey(w http.ResponseWriter, r *http.Request, id string) {
	existing := config.GetApiKeyEntry(id)
	if existing == nil {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"error": "API key not found"})
		return
	}

	var req apiKeyUpdateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	patch := *existing
	if req.UpstreamForward != nil {
		patch.UpstreamForward = mergeUpstreamConfig(existing.UpstreamForward, req.UpstreamForward)
	}
	if req.Name != nil {
		patch.Name = *req.Name
	}
	if req.Key != nil {
		patch.Key = *req.Key
	}
	if req.Enabled != nil {
		patch.Enabled = *req.Enabled
	}
	if req.TokenLimit != nil {
		patch.TokenLimit = *req.TokenLimit
	}
	if req.CreditLimit != nil {
		patch.CreditLimit = *req.CreditLimit
	}

	if req.PromptCacheEmulationEnabled != nil {
		patch.PromptCacheEmulationEnabled = req.PromptCacheEmulationEnabled
	}
	if req.PromptCacheEmulationMode != nil {
		patch.PromptCacheEmulationMode = req.PromptCacheEmulationMode
	}
	if req.PromptCacheEmulationRatio != nil {
		patch.PromptCacheEmulationRatio = req.PromptCacheEmulationRatio
	}
	if req.PromptCacheCreationEmulationRatio != nil {
		patch.PromptCacheCreationEmulationRatio = req.PromptCacheCreationEmulationRatio
	}
	if req.PromptCacheReadEmulationRatio != nil {
		patch.PromptCacheReadEmulationRatio = req.PromptCacheReadEmulationRatio
	}
	updateKey := config.UpdateApiKey
	if req.ClearPromptCacheOverrides {
		updateKey = config.UpdateApiKeyAndClearPromptCacheOverrides
	}
	if err := updateKey(id, patch); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	h.pool.Reload()
	updated := config.GetApiKeyEntry(id)
	if updated == nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": "failed to reload API key"})
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"apiKey":  toApiKeyView(*updated),
	})
}

func (h *Handler) apiDeleteApiKey(w http.ResponseWriter, _ *http.Request, id string) {
	if err := config.DeleteApiKey(id); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

func (h *Handler) apiResetApiKeyUsage(w http.ResponseWriter, _ *http.Request, id string) {
	if err := config.ResetApiKeyUsage(id); err != nil {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	updated := config.GetApiKeyEntry(id)
	if updated == nil {
		json.NewEncoder(w).Encode(map[string]bool{"success": true})
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"apiKey":  toApiKeyView(*updated),
	})
}
