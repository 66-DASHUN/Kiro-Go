package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalizeAPIKeyAccountPipeRegionAndMachineId(t *testing.T) {
	account := Account{
		KiroApiKey: " ksk_test_key|eu-central-1 ",
		AuthMethod: "API KEY",
	}
	if err := NormalizeAPIKeyAccount(&account); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if account.KiroApiKey != "ksk_test_key" {
		t.Fatalf("key = %q", account.KiroApiKey)
	}
	if account.AccessToken != "ksk_test_key" {
		t.Fatalf("accessToken should mirror api key, got %q", account.AccessToken)
	}
	if account.AuthMethod != "api_key" {
		t.Fatalf("authMethod = %q", account.AuthMethod)
	}
	if account.Region != "eu-central-1" {
		t.Fatalf("region = %q", account.Region)
	}
	if account.RefreshToken != "" || account.ProfileArn != "" || account.ExpiresAt != 0 {
		t.Fatalf("oauth fields should be cleared: %+v", account)
	}
	wantMachine := MachineIdFromAPIKey("ksk_test_key")
	if account.MachineId != wantMachine {
		t.Fatalf("machineId = %q, want %q", account.MachineId, wantMachine)
	}
	if !IsAPIKeyAccount(&account) {
		t.Fatal("expected IsAPIKeyAccount true")
	}
}

func TestAddAccountRejectsDuplicateAPIKey(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	first := Account{ID: "api-1", KiroApiKey: "ksk_dup", AuthMethod: "api_key", Enabled: true}
	if err := AddAccount(first); err != nil {
		t.Fatalf("add first: %v", err)
	}
	second := Account{ID: "api-2", KiroApiKey: "ksk_dup", AuthMethod: "api_key", Enabled: true}
	if err := AddAccount(second); err != ErrDuplicateAPIKey {
		t.Fatalf("expected ErrDuplicateAPIKey, got %v", err)
	}
}

func TestSplitKiroAPIKeyAndRegionValidation(t *testing.T) {
	key, region, err := SplitKiroAPIKeyAndRegion("ksk_abc|us-east-1")
	if err != nil || key != "ksk_abc" || region != "us-east-1" {
		t.Fatalf("got key=%q region=%q err=%v", key, region, err)
	}
	if _, _, err := SplitKiroAPIKeyAndRegion("ksk_abc|us-east-1|extra"); err == nil {
		t.Fatal("expected multi-pipe error")
	}
	if _, _, err := SplitKiroAPIKeyAndRegion("|us-east-1"); err == nil {
		t.Fatal("expected empty key error")
	}
}

func TestUpdateSettingsPatchPreservesOmittedAPIKeyFields(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	if err := UpdateSettings("proxy-api-key", true, "admin-password"); err != nil {
		t.Fatalf("seed settings: %v", err)
	}

	if err := UpdateSettingsPatch(nil, nil, "new-admin-password"); err != nil {
		t.Fatalf("patch settings: %v", err)
	}

	if got := GetApiKey(); got != "proxy-api-key" {
		t.Fatalf("expected API key to be preserved, got %q", got)
	}
	if !IsApiKeyRequired() {
		t.Fatalf("expected requireApiKey to stay enabled")
	}
	if got := GetPassword(); got != "new-admin-password" {
		t.Fatalf("expected password to update, got %q", got)
	}
}

func TestUpdateSettingsPatchCanExplicitlyDisableAPIKey(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	if err := UpdateSettings("proxy-api-key", true, "admin-password"); err != nil {
		t.Fatalf("seed settings: %v", err)
	}

	emptyKey := ""
	requireAPIKey := false
	if err := UpdateSettingsPatch(&emptyKey, &requireAPIKey, ""); err != nil {
		t.Fatalf("patch settings: %v", err)
	}

	if got := GetApiKey(); got != "" {
		t.Fatalf("expected API key to be cleared, got %q", got)
	}
	if IsApiKeyRequired() {
		t.Fatalf("expected requireApiKey to be disabled")
	}
	if got := GetPassword(); got != "admin-password" {
		t.Fatalf("expected password to be preserved, got %q", got)
	}
}

func TestUpdateAccountStaleSnapshotPreservesCredentialRotation(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	account := Account{
		ID:            "rotation-account",
		AccessToken:   "access-1",
		RefreshToken:  "refresh-1",
		ClientID:      "client",
		AuthMethod:    "external_idp",
		Region:        "us-east-1",
		ExpiresAt:     100,
		ProfileArn:    "arn:aws:codewhisperer:us-east-1:123456789012:profile/one",
		TokenEndpoint: "https://login.microsoftonline.com/tenant/oauth2/v2.0/token",
		IssuerURL:     "https://login.microsoftonline.com/tenant/v2.0",
		Scopes:        "scope-one",
		Enabled:       true,
	}
	if err := AddAccount(account); err != nil {
		t.Fatalf("add account: %v", err)
	}
	stale := GetAccounts()[0]

	const rotatedProfile = "arn:aws:codewhisperer:eu-central-1:123456789012:profile/two"
	if err := UpdateAccountCredentialState(
		account.ID,
		"access-2",
		"refresh-2",
		200,
		rotatedProfile,
	); err != nil {
		t.Fatalf("rotate credential: %v", err)
	}

	stale.Enabled = false
	stale.BanStatus = "BANNED"
	stale.BanReason = "stale status update"
	if err := UpdateAccount(account.ID, stale); err != nil {
		t.Fatalf("apply stale status snapshot: %v", err)
	}

	got := GetAccounts()[0]
	if got.AccessToken != "access-2" ||
		got.RefreshToken != "refresh-2" ||
		got.ExpiresAt != 200 ||
		got.ProfileArn != rotatedProfile {
		t.Fatalf("stale status update reverted credential state: %+v", got)
	}
	if got.RefreshTokenFingerprint != RefreshTokenFingerprint("refresh-1") {
		t.Fatalf("original refresh token fingerprint = %q", got.RefreshTokenFingerprint)
	}
	if got.Enabled || got.BanStatus != "BANNED" || got.BanReason != "stale status update" {
		t.Fatalf("status fields were not applied: %+v", got)
	}
}

func TestUpdateAccountStaleSnapshotPreservesApiKeyBinding(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init: %v", err)
	}
	if _, err := AddApiKey(ApiKeyEntry{ID: "key-one", Key: "sk-one", Enabled: true}); err != nil {
		t.Fatalf("add key one: %v", err)
	}
	if _, err := AddApiKey(ApiKeyEntry{ID: "key-two", Key: "sk-two", Enabled: true}); err != nil {
		t.Fatalf("add key two: %v", err)
	}
	account := Account{ID: "binding-account", ApiKeyID: "key-one", AccessToken: "access", Enabled: true}
	if err := AddAccount(account); err != nil {
		t.Fatalf("add account: %v", err)
	}

	stale := GetAccounts()[0]
	if err := UpdateAccountApiKeyID(account.ID, "key-two"); err != nil {
		t.Fatalf("update binding: %v", err)
	}
	stale.Nickname = "status update"
	if err := UpdateAccount(account.ID, stale); err != nil {
		t.Fatalf("apply stale snapshot: %v", err)
	}

	got := GetAccounts()[0]
	if got.ApiKeyID != "key-two" {
		t.Fatalf("stale snapshot reverted api key binding to %q", got.ApiKeyID)
	}
	if got.Nickname != "status update" {
		t.Fatalf("stale snapshot did not update nickname: %q", got.Nickname)
	}
}

func TestUpdateAccountPatchOnlyChangesSpecifiedFieldsAfterBindingChange(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init: %v", err)
	}
	if _, err := AddApiKey(ApiKeyEntry{ID: "key-one", Key: "sk-one", Enabled: true}); err != nil {
		t.Fatalf("add key one: %v", err)
	}
	if _, err := AddApiKey(ApiKeyEntry{ID: "key-two", Key: "sk-two", Enabled: true}); err != nil {
		t.Fatalf("add key two: %v", err)
	}
	account := Account{ID: "patch-account", ApiKeyID: "key-one", AccessToken: "access", Enabled: true}
	if err := AddAccount(account); err != nil {
		t.Fatalf("add account: %v", err)
	}
	if err := UpdateAccountApiKeyID(account.ID, "key-two"); err != nil {
		t.Fatalf("update binding: %v", err)
	}

	nickname := "patched"
	updated, err := UpdateAccountPatch(account.ID, AccountPatch{Nickname: &nickname})
	if err != nil {
		t.Fatalf("patch account: %v", err)
	}
	if updated.ApiKeyID != "key-two" || updated.Nickname != nickname {
		t.Fatalf("patch result=%+v, want key-two and nickname", updated)
	}
	if got := GetAccounts()[0]; got.ApiKeyID != "key-two" || got.Nickname != nickname {
		t.Fatalf("stored account=%+v, want binding preserved", got)
	}
}

func TestUpdateAccountPatchCanExplicitlyUnbindApiKey(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init: %v", err)
	}
	if _, err := AddApiKey(ApiKeyEntry{ID: "key-one", Key: "sk-one", Enabled: true}); err != nil {
		t.Fatalf("add key: %v", err)
	}
	account := Account{ID: "unbind-account", ApiKeyID: "key-one", AccessToken: "access", Enabled: true}
	if err := AddAccount(account); err != nil {
		t.Fatalf("add account: %v", err)
	}
	empty := ""
	updated, err := UpdateAccountPatch(account.ID, AccountPatch{ApiKeyID: &empty})
	if err != nil {
		t.Fatalf("unbind account: %v", err)
	}
	if updated.ApiKeyID != "" || GetAccounts()[0].ApiKeyID != "" {
		t.Fatalf("expected explicit unbind, result=%+v stored=%+v", updated, GetAccounts()[0])
	}
}

// TestAccountAllowOverageMigration verifies that a config.json from before the
// upstream-Overages-switch refactor (which carried `allowOverage: true` per
// account) is migrated into OverageStatus="ENABLED" on first load, and that
// the legacy field is cleared so future saves don't re-emit it.
func TestAccountAllowOverageMigration(t *testing.T) {
	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "config.json")

	seed := map[string]interface{}{
		"password":      "p",
		"port":          8080,
		"host":          "0.0.0.0",
		"requireApiKey": false,
		"accounts": []map[string]interface{}{
			{"id": "acc-allow", "enabled": true, "allowOverage": true},
			{"id": "acc-deny", "enabled": true, "allowOverage": false},
			{"id": "acc-already-set", "enabled": true, "allowOverage": true, "overageStatus": "DISABLED"},
		},
	}
	raw, err := json.MarshalIndent(seed, "", "  ")
	if err != nil {
		t.Fatalf("marshal seed: %v", err)
	}
	if err := os.WriteFile(cfgFile, raw, 0600); err != nil {
		t.Fatalf("write seed: %v", err)
	}

	if err := Init(cfgFile); err != nil {
		t.Fatalf("init: %v", err)
	}

	accounts := GetAccounts()
	byID := map[string]Account{}
	for _, a := range accounts {
		byID[a.ID] = a
	}

	if got := byID["acc-allow"].OverageStatus; got != "ENABLED" {
		t.Fatalf("expected acc-allow to migrate to OverageStatus=ENABLED, got %q", got)
	}
	if byID["acc-allow"].LegacyAllowOverage {
		t.Fatalf("expected legacy allowOverage to be cleared after migration")
	}
	if got := byID["acc-deny"].OverageStatus; got != "" {
		t.Fatalf("expected acc-deny to keep empty OverageStatus, got %q", got)
	}
	// Pre-set OverageStatus must win over the legacy field.
	if got := byID["acc-already-set"].OverageStatus; got != "DISABLED" {
		t.Fatalf("expected acc-already-set OverageStatus to be preserved, got %q", got)
	}
	if byID["acc-already-set"].LegacyAllowOverage {
		t.Fatalf("expected legacy field to still be cleared on acc-already-set")
	}

	// Re-read the file and confirm legacy field is gone (so it doesn't drift
	// back in on later saves).
	on_disk, err := os.ReadFile(cfgFile)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	var reloaded struct {
		Accounts []map[string]interface{} `json:"accounts"`
	}
	if err := json.Unmarshal(on_disk, &reloaded); err != nil {
		t.Fatalf("decode reload: %v", err)
	}
	for _, a := range reloaded.Accounts {
		if _, ok := a["allowOverage"]; ok {
			t.Fatalf("expected allowOverage to be omitted from persisted file, got %+v", a)
		}
	}
}

func TestAccountAPIKeyIDJSONPersistence(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := Init(cfgFile); err != nil {
		t.Fatalf("init: %v", err)
	}
	if _, err := AddApiKey(ApiKeyEntry{ID: "key-1", Key: "sk-one", Enabled: true}); err != nil {
		t.Fatalf("add key: %v", err)
	}
	if err := AddAccount(Account{ID: "account-1", ApiKeyID: "key-1", RefreshToken: "refresh-1", Enabled: true}); err != nil {
		t.Fatalf("add account: %v", err)
	}

	raw, err := os.ReadFile(cfgFile)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var persisted struct {
		Accounts []map[string]interface{} `json:"accounts"`
	}
	if err := json.Unmarshal(raw, &persisted); err != nil {
		t.Fatalf("decode config: %v", err)
	}
	if got := persisted.Accounts[0]["apiKeyId"]; got != "key-1" {
		t.Fatalf("persisted apiKeyId = %v, want key-1", got)
	}

	if err := Load(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := GetAccounts()[0].ApiKeyID; got != "key-1" {
		t.Fatalf("reloaded apiKeyId = %q, want key-1", got)
	}
}

func TestAccountAPIKeyIDMigrationAssignsFirstEnabledKey(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	seed := map[string]interface{}{
		"password": "p",
		"port":     8080,
		"host":     "0.0.0.0",
		"apiKeys": []map[string]interface{}{
			{"id": "disabled", "key": "sk-disabled", "enabled": false},
			{"id": "key-1", "key": "sk-one", "enabled": true},
			{"id": "key-2", "key": "sk-two", "enabled": true},
		},
		"accounts": []map[string]interface{}{
			{"id": "legacy-1", "enabled": true},
			{"id": "explicit", "apiKeyId": "key-2", "enabled": true},
			{"id": "legacy-2", "apiKeyId": "", "enabled": true},
		},
	}
	raw, err := json.Marshal(seed)
	if err != nil {
		t.Fatalf("marshal seed: %v", err)
	}
	if err := os.WriteFile(cfgFile, raw, 0600); err != nil {
		t.Fatalf("write seed: %v", err)
	}

	if err := Init(cfgFile); err != nil {
		t.Fatalf("init: %v", err)
	}
	accounts := GetAccounts()
	byID := make(map[string]Account, len(accounts))
	for _, account := range accounts {
		byID[account.ID] = account
	}
	if got := byID["legacy-1"].ApiKeyID; got != "key-1" {
		t.Fatalf("legacy-1 apiKeyId = %q, want key-1", got)
	}
	if got := byID["legacy-2"].ApiKeyID; got != "key-1" {
		t.Fatalf("legacy-2 apiKeyId = %q, want key-1", got)
	}
	if got := byID["explicit"].ApiKeyID; got != "key-2" {
		t.Fatalf("explicit apiKeyId = %q, want key-2", got)
	}

	persisted, err := os.ReadFile(cfgFile)
	if err != nil {
		t.Fatalf("read migrated config: %v", err)
	}
	if !strings.Contains(string(persisted), `"apiKeyId": "key-1"`) {
		t.Fatalf("migrated apiKeyId was not persisted: %s", persisted)
	}

	if err := Load(); err != nil {
		t.Fatalf("second reload: %v", err)
	}
	if got := GetAccounts()[0].ApiKeyID; got != "key-1" {
		t.Fatalf("second reload changed apiKeyId to %q", got)
	}
}

func TestExplicitApiKeyUnbindSurvivesReloadAfterLegacyMigration(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	seed := map[string]interface{}{
		"password": "p",
		"port":     8080,
		"host":     "0.0.0.0",
		"apiKeys": []map[string]interface{}{
			{"id": "key-one", "key": "sk-one", "enabled": true},
		},
		"accounts": []map[string]interface{}{
			{"id": "legacy", "enabled": true},
		},
	}
	raw, err := json.Marshal(seed)
	if err != nil {
		t.Fatalf("marshal seed: %v", err)
	}
	if err := os.WriteFile(cfgFile, raw, 0600); err != nil {
		t.Fatalf("write seed: %v", err)
	}
	if err := Init(cfgFile); err != nil {
		t.Fatalf("init: %v", err)
	}
	if got := GetAccounts()[0].ApiKeyID; got != "key-one" {
		t.Fatalf("legacy account apiKeyId=%q, want key-one", got)
	}
	empty := ""
	if _, err := UpdateAccountPatch("legacy", AccountPatch{ApiKeyID: &empty}); err != nil {
		t.Fatalf("explicit unbind: %v", err)
	}
	if err := Load(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := GetAccounts()[0].ApiKeyID; got != "" {
		t.Fatalf("explicit unbind was re-migrated to %q", got)
	}

	persisted, err := os.ReadFile(cfgFile)
	if err != nil {
		t.Fatalf("read persisted config: %v", err)
	}
	var decoded map[string]interface{}
	if err := json.Unmarshal(persisted, &decoded); err != nil {
		t.Fatalf("decode persisted config: %v", err)
	}
	if completed, ok := decoded["apiKeyAccountMigrationCompleted"].(bool); !ok || !completed {
		t.Fatalf("expected persisted migration marker, got %v", decoded["apiKeyAccountMigrationCompleted"])
	}
}

func TestAPIKeyIDValidityHelpers(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := Init(cfgFile); err != nil {
		t.Fatalf("init: %v", err)
	}
	if _, err := AddApiKey(ApiKeyEntry{ID: "enabled", Key: "sk-enabled", Enabled: true}); err != nil {
		t.Fatalf("add enabled key: %v", err)
	}
	if _, err := AddApiKey(ApiKeyEntry{ID: "disabled", Key: "sk-disabled", Enabled: false}); err != nil {
		t.Fatalf("add disabled key: %v", err)
	}
	if !ApiKeyIDExists("enabled") || !ApiKeyIDExists("disabled") {
		t.Fatal("expected both configured key IDs to exist")
	}
	if ApiKeyIDExists("missing") {
		t.Fatal("missing key ID should not exist")
	}
	if !IsApiKeyIDValid("enabled") {
		t.Fatal("enabled key ID should be valid")
	}
	if IsApiKeyIDValid("disabled") || IsApiKeyIDValid("missing") || IsApiKeyIDValid("") {
		t.Fatal("disabled, missing, and empty key IDs should be invalid")
	}
}

func TestDeleteApiKeyRejectsBoundAccounts(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := Init(cfgFile); err != nil {
		t.Fatalf("init: %v", err)
	}
	key, err := AddApiKey(ApiKeyEntry{ID: "bound-key", Key: "sk-bound", Enabled: true})
	if err != nil {
		t.Fatalf("add key: %v", err)
	}
	if err := AddAccount(Account{ID: "bound-account", ApiKeyID: key.ID, Enabled: true}); err != nil {
		t.Fatalf("add account: %v", err)
	}
	if err := DeleteApiKey(key.ID); err != ErrApiKeyInUse {
		t.Fatalf("delete bound key error=%v, want %v", err, ErrApiKeyInUse)
	}
	if GetApiKeyEntry(key.ID) == nil {
		t.Fatal("bound key was deleted despite account binding")
	}
	if got := GetAccounts()[0].ApiKeyID; got != key.ID {
		t.Fatalf("account binding changed after rejected delete: %q", got)
	}
}

func TestEnablingFirstUsableApiKeyMigratesLegacyAccounts(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := Init(cfgFile); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := AddAccount(Account{ID: "legacy-before-key", Enabled: true}); err != nil {
		t.Fatalf("add legacy account: %v", err)
	}
	key, err := AddApiKey(ApiKeyEntry{ID: "disabled-key", Key: "sk-disabled", Enabled: false})
	if err != nil {
		t.Fatalf("add disabled key: %v", err)
	}
	if cfg.ApiKeyAccountMigrationCompleted {
		t.Fatal("migration marker should remain false while no usable key exists")
	}
	if err := UpdateApiKey(key.ID, ApiKeyEntry{Enabled: true}); err != nil {
		t.Fatalf("enable key: %v", err)
	}
	accounts := GetAccounts()
	if len(accounts) != 1 || accounts[0].ApiKeyID != key.ID {
		t.Fatalf("legacy account was not migrated after enabling key: %+v", accounts)
	}
	if !cfg.ApiKeyAccountMigrationCompleted {
		t.Fatal("migration marker should be set after enabling first usable key")
	}
	if err := Load(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := GetAccounts()[0].ApiKeyID; got != key.ID {
		t.Fatalf("reloaded binding=%q, want %q", got, key.ID)
	}
}
