package proxy

import (
	"encoding/json"
	"fmt"
	"kiro-go/config"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMicrosoftSSOProfileSelectionExplicitEmptyApiKeyUnbindsSnapshot(t *testing.T) {
	h := newMicrosoftHandlerForTest(t)
	key, err := config.AddApiKey(config.ApiKeyEntry{ID: "profile-clear-key", Key: "sk-profile-clear", Enabled: true})
	if err != nil {
		t.Fatalf("seed API key: %v", err)
	}
	account := config.Account{
		ID:           "microsoft-profile-clear-account",
		ApiKeyID:     key.ID,
		AccessToken:  "access-token",
		RefreshToken: "refresh-token",
		ExpiresAt:    time.Now().Add(time.Hour).Unix(),
	}
	profiles := []KiroProfile{{
		ARN:    "arn:aws:codewhisperer:us-east-1:123456789012:profile/clear",
		Name:   "Clear profile",
		Region: "us-east-1",
	}}
	selectionID, expired, err := h.storeMicrosoftProfileSelection("microsoft-profile-clear-session", account, profiles)
	discardDetachedMicrosoftProfileSelections(expired)
	if err != nil {
		t.Fatalf("store profile selection: %v", err)
	}
	t.Cleanup(func() { h.removeMicrosoftProfileSelection(selectionID, nil) })

	body, err := json.Marshal(map[string]string{
		"selectionId": selectionID,
		"profileArn":  profiles[0].ARN,
		"apiKeyId":    "",
	})
	if err != nil {
		t.Fatalf("marshal selection request: %v", err)
	}
	recorder := httptest.NewRecorder()
	h.apiSelectMicrosoftSSOProfile(
		recorder,
		httptest.NewRequest(http.MethodPost, "/auth/microsoft-sso/select-profile", strings.NewReader(string(body))),
	)
	if recorder.Code != http.StatusOK {
		t.Fatalf("selection status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	accounts := config.GetAccounts()
	if len(accounts) != 1 {
		t.Fatalf("accounts = %+v, want one persisted account", accounts)
	}
	if accounts[0].ApiKeyID != "" {
		t.Fatalf("explicit empty apiKeyId did not unbind account: %q", accounts[0].ApiKeyID)
	}
	var response struct {
		Account struct {
			ApiKeyID string `json:"apiKeyId"`
		} `json:"account"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode selection response: %v", err)
	}
	if response.Account.ApiKeyID != "" {
		t.Fatalf("selection response apiKeyId = %q, want empty", response.Account.ApiKeyID)
	}
}

func TestMicrosoftSSOProfileSelectionRejectsInvalidExplicitApiKey(t *testing.T) {
	h := newMicrosoftHandlerForTest(t)
	account := config.Account{
		ID:           "microsoft-profile-invalid-account",
		AccessToken:  "access-token",
		RefreshToken: "refresh-token",
		ExpiresAt:    time.Now().Add(time.Hour).Unix(),
	}
	profiles := []KiroProfile{{
		ARN:    "arn:aws:codewhisperer:us-east-1:123456789012:profile/invalid",
		Name:   "Invalid profile key",
		Region: "us-east-1",
	}}
	selectionID, expired, err := h.storeMicrosoftProfileSelection("microsoft-profile-invalid-session", account, profiles)
	discardDetachedMicrosoftProfileSelections(expired)
	if err != nil {
		t.Fatalf("store profile selection: %v", err)
	}
	t.Cleanup(func() { h.removeMicrosoftProfileSelection(selectionID, nil) })

	body := fmt.Sprintf(`{"selectionId":%q,"profileArn":%q,"apiKeyId":"missing-key"}`, selectionID, profiles[0].ARN)
	recorder := httptest.NewRecorder()
	h.apiSelectMicrosoftSSOProfile(
		recorder,
		httptest.NewRequest(http.MethodPost, "/auth/microsoft-sso/select-profile", strings.NewReader(body)),
	)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("invalid key selection status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	if len(config.GetAccounts()) != 0 {
		t.Fatalf("invalid key selection persisted accounts: %+v", config.GetAccounts())
	}
}
