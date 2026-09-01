package proxy

import (
	"encoding/json"
	"fmt"
	"kiro-go/config"
	accountpool "kiro-go/pool"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestImportCredentialsPersistsApiKeyID(t *testing.T) {
	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	key, err := config.AddApiKey(config.ApiKeyEntry{ID: "group-a", Key: "sk-group-a", Enabled: true})
	if err != nil {
		t.Fatalf("add api key: %v", err)
	}
	// The API-key import path starts a best-effort model refresh. Keep that
	// goroutine offline so this test only exercises persistence and binding.
	kiroRestHttpStore.Store(&http.Client{
		Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, fmt.Errorf("network disabled in test")
		}),
	})
	t.Cleanup(func() {
		kiroRestHttpStore.Store(&http.Client{Transport: &http.Transport{}})
	})

	h := &Handler{pool: accountpool.GetPool()}
	rec := httptest.NewRecorder()
	body := `{"kiroApiKey":"ksk_import_binding","apiKeyId":"` + key.ID + `","authMethod":"api_key"}`
	h.apiImportCredentials(rec, httptest.NewRequest(http.MethodPost, "/admin/api/auth/credentials", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	accounts := config.GetAccounts()
	if len(accounts) != 1 || accounts[0].ApiKeyID != key.ID {
		t.Fatalf("accounts=%+v, want apiKeyId %q", accounts, key.ID)
	}
}

func TestAccountFullIncludesApiKeyID(t *testing.T) {
	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	key, err := config.AddApiKey(config.ApiKeyEntry{ID: "group-full", Key: "sk-group-full", Enabled: true})
	if err != nil {
		t.Fatalf("add api key: %v", err)
	}
	account := config.Account{
		ID:           "full-account",
		ApiKeyID:     key.ID,
		AccessToken:  "access-token",
		RefreshToken: "refresh-token",
		Enabled:      true,
	}
	if err := config.AddAccount(account); err != nil {
		t.Fatalf("add account: %v", err)
	}

	h := &Handler{pool: accountpool.GetPool()}
	rec := httptest.NewRecorder()
	h.apiGetAccountFull(rec, httptest.NewRequest(http.MethodGet, "/admin/api/accounts/full-account/full", nil), account.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var payload map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got, _ := payload["apiKeyId"].(string); got != key.ID {
		t.Fatalf("apiKeyId=%q, want %q; payload=%+v", got, key.ID, payload)
	}
}

func TestExportAccountsIncludesApiKeyID(t *testing.T) {
	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	key, err := config.AddApiKey(config.ApiKeyEntry{ID: "group-export", Key: "sk-group-export", Enabled: true})
	if err != nil {
		t.Fatalf("add api key: %v", err)
	}
	if err := config.AddAccount(config.Account{
		ID:           "export-account",
		ApiKeyID:     key.ID,
		AccessToken:  "access-token",
		RefreshToken: "refresh-token",
		AuthMethod:   "social",
		Provider:     "Google",
		Region:       "us-east-1",
		Enabled:      true,
	}); err != nil {
		t.Fatalf("add account: %v", err)
	}

	h := &Handler{pool: accountpool.GetPool()}
	rec := httptest.NewRecorder()
	h.apiExportAccounts(rec, httptest.NewRequest(http.MethodPost, "/admin/api/export", strings.NewReader(`{}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Accounts []struct {
			ApiKeyID    string                 `json:"apiKeyId"`
			Credentials map[string]interface{} `json:"credentials"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(payload.Accounts) != 1 {
		t.Fatalf("accounts=%+v", payload.Accounts)
	}
	if got := payload.Accounts[0].ApiKeyID; got != key.ID {
		t.Fatalf("apiKeyId=%q, want %q; account=%+v", got, key.ID, payload.Accounts[0])
	}
	if got, _ := payload.Accounts[0].Credentials["apiKeyId"].(string); got != key.ID {
		t.Fatalf("credentials.apiKeyId=%q, want %q; credentials=%+v", got, key.ID, payload.Accounts[0].Credentials)
	}
}

func TestCreateApiKeyReloadsPoolAfterLegacyAccountMigration(t *testing.T) {
	if err := config.Init(t.TempDir() + "/config.json"); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	account := config.Account{
		ID:          "legacy-pool-account",
		AccessToken: "access-token",
		Enabled:     true,
	}
	if err := config.AddAccount(account); err != nil {
		t.Fatalf("add account: %v", err)
	}
	p := accountpool.GetPool()
	p.Reload()
	h := &Handler{pool: p}
	rec := httptest.NewRecorder()
	h.apiCreateApiKey(rec, httptest.NewRequest(http.MethodPost, "/admin/api/api-keys", strings.NewReader(`{"name":"pool-key","key":"sk-pool-key","enabled":true}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	keys := config.ListApiKeys()
	if len(keys) != 1 {
		t.Fatalf("keys=%+v", keys)
	}
	selected := p.GetNextForModelForAPIKeyExcluding(keys[0].ID, "model", nil)
	if selected == nil || selected.ID != account.ID {
		t.Fatalf("pool selection=%v, want migrated account %q immediately after create", selected, account.ID)
	}
}
