package proxy

import (
	"encoding/json"
	"errors"
	"kiro-proxy/config"
	"net/http"
	"strings"
)

func normalizeRequestApiKeyID(raw string) (string, error) {
	id := strings.TrimSpace(raw)
	if id != "" && !config.IsApiKeyIDValid(id) {
		return "", config.ErrInvalidAccountApiKey
	}
	return id, nil
}

func accountBindingName(id string) string {
	if entry := config.GetApiKeyEntry(id); entry != nil {
		return entry.Name
	}
	return ""
}

func (h *Handler) writeAddAccountError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	if errors.Is(err, config.ErrInvalidAccountApiKey) || errors.Is(err, config.ErrInvalidAccountConcurrency) {
		status = http.StatusBadRequest
	}
	if errors.Is(err, config.ErrDuplicateKiroAPIKey) || errors.Is(err, config.ErrDuplicateRefreshToken) || errors.Is(err, config.ErrDuplicateAccountID) {
		status = http.StatusConflict
	}
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}
