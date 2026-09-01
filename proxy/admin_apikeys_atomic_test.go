package proxy

import (
	"encoding/json"
	"kiro-go/config"
	accountpool "kiro-go/pool"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestApiCreateApiKeyReloadsPoolAfterLegacyAccountMigration(t *testing.T) {
	mustInitConfig(t)
	if err := config.AddAccount(config.Account{
		ID:          "legacy-create-account",
		AccessToken: "access-token",
		AuthMethod:  "social",
		Region:      "us-east-1",
		Enabled:     true,
	}); err != nil {
		t.Fatalf("add legacy account: %v", err)
	}

	p := accountpool.GetPool()
	p.Reload()
	h := &Handler{pool: p}
	rec := httptest.NewRecorder()
	h.apiCreateApiKey(rec, httptest.NewRequest(http.MethodPost, "/admin/api/api-keys", strings.NewReader(`{"name":"created","key":"sk-created","enabled":true}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("create status=%d body=%s", rec.Code, rec.Body.String())
	}
	var payload struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	if payload.ID == "" {
		t.Fatal("create response did not include key ID")
	}
	account := p.GetNextForModelForAPIKeyExcluding(payload.ID, "model", nil)
	if account == nil || account.ID != "legacy-create-account" {
		t.Fatalf("pool did not reload migrated account: got %+v", account)
	}
}

func TestApiUpdateApiKeyReloadsPoolAfterEnablingMigratingKey(t *testing.T) {
	mustInitConfig(t)
	key, err := config.AddApiKey(config.ApiKeyEntry{ID: "disabled-update-key", Key: "sk-disabled-update", Enabled: false})
	if err != nil {
		t.Fatalf("add disabled key: %v", err)
	}
	if err := config.AddAccount(config.Account{
		ID:          "legacy-update-account",
		AccessToken: "access-token",
		AuthMethod:  "social",
		Region:      "us-east-1",
		Enabled:     true,
	}); err != nil {
		t.Fatalf("add legacy account: %v", err)
	}

	p := accountpool.GetPool()
	p.Reload()
	h := &Handler{pool: p}
	rec := httptest.NewRecorder()
	h.apiUpdateApiKey(rec, httptest.NewRequest(http.MethodPut, "/admin/api/api-keys/"+key.ID, strings.NewReader(`{"enabled":true}`)), key.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("update status=%d body=%s", rec.Code, rec.Body.String())
	}
	account := p.GetNextForModelForAPIKeyExcluding(key.ID, "model", nil)
	if account == nil || account.ID != "legacy-update-account" {
		t.Fatalf("pool did not reload migrated account after enable: got %+v", account)
	}
}

func TestWriteAddAccountErrorMapsInvalidApiKeyToBadRequest(t *testing.T) {
	rec := httptest.NewRecorder()
	(&Handler{}).writeAddAccountError(rec, config.ErrInvalidAccountApiKey)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s, want 400", rec.Code, rec.Body.String())
	}
}
