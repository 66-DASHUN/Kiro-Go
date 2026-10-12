package config

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"kiro-proxy/db"
)

func compatibilityDB(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Cleanup(func() { _ = db.Close() })
	t.Setenv("CONFIG_PATH", "")
	t.Setenv("ADMIN_PASSWORD", "fixture-password")
	return filepath.Join(dir, "kiro.db")
}

func jsonObject(t *testing.T, value any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestLegacyJSONImportPreservesDeployment(t *testing.T) {
	path := compatibilityDB(t)
	legacyPath := filepath.Join(filepath.Dir(path), "config.json")
	fixture := map[string]any{
		"password": "legacy-password", "port": 8321, "host": "127.0.0.1", "requireApiKey": true,
		"apiKeyAccountMigrationCompleted": true, "totalRequests": 199, "successRequests": 190,
		"failedRequests": 9, "totalTokens": 2311, "totalCredits": 14.5,
		"promptCacheEmulationEnabled": false, "promptCacheEmulationMode": "independent",
		"promptCacheEmulationRatio": 0.6, "promptCacheCreationEmulationRatio": 0.2, "promptCacheReadEmulationRatio": 0.7,
		"modelMappings": []any{map[string]any{"key": "custom", "value": "claude-custom"}},
		"apiKeys": []any{
			map[string]any{"id": "key-a", "key": "sk-fixture-a", "enabled": true, "createdAt": 11, "tokensUsed": 17, "creditsUsed": 2.5, "requestsCount": 3, "promptCacheEmulationEnabled": true, "promptCacheEmulationMode": "uniform", "promptCacheEmulationRatio": 0.4, "promptCacheCreationEmulationRatio": 0.3, "promptCacheReadEmulationRatio": 0.8},
			map[string]any{"id": "key-b", "key": "sk-fixture-b", "enabled": false, "createdAt": 12, "migrated": true},
		},
	}
	accounts := make([]any, 6)
	for i := range accounts {
		accounts[i] = map[string]any{"id": fmt.Sprintf("account-%d", i), "accessToken": fmt.Sprintf("access-%d", i), "refreshToken": fmt.Sprintf("refresh-%d", i), "refreshTokenFingerprint": fmt.Sprintf("fingerprint-%d", i), "clientSecret": fmt.Sprintf("client-secret-%d", i), "enabled": i != 2, "apiKeyId": "key-a", "requestCount": i + 10, "totalTokens": i + 123, "totalCredits": float64(i) + 0.5}
		accounts[i].(map[string]any)["authMethod"] = "social"
		accounts[i].(map[string]any)["region"] = "us-east-1"
	}
	delete(accounts[5].(map[string]any), "apiKeyId")
	fixture["accounts"] = accounts
	raw, _ := json.Marshal(fixture)
	if err := os.WriteFile(legacyPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONFIG_PATH", legacyPath)
	if err := Init(path); err != nil {
		t.Fatal(err)
	}
	got := jsonObject(t, Get())
	for name, want := range fixture {
		wantRaw, _ := json.Marshal(want)
		gotRaw, _ := json.Marshal(got[name])
		if string(wantRaw) != string(gotRaw) {
			t.Errorf("legacy %s changed or was lost", name)
		}
	}
	if len(GetAccounts()) != 6 || len(ListApiKeys()) != 2 || GetPort() != 8321 {
		t.Fatal("deployment counts/port lost")
	}
	remaining, _ := os.ReadFile(legacyPath)
	if !reflect.DeepEqual(raw, remaining) {
		t.Fatal("legacy file was modified")
	}
	if err := Init(path); err != nil {
		t.Fatal(err)
	}
	if jsonObject(t, GetAccounts()[5])["apiKeyId"] != nil {
		t.Fatal("explicitly unbound account rebound on restart")
	}
}

func TestLegacyJSONImportFailsClosedAndDatabaseWins(t *testing.T) {
	for _, populated := range []bool{false, true} {
		t.Run(fmt.Sprint(populated), func(t *testing.T) {
			path := compatibilityDB(t)
			if populated {
				if err := Init(path); err != nil {
					t.Fatal(err)
				}
			}
			legacy := filepath.Join(filepath.Dir(path), "config.json")
			if err := os.WriteFile(legacy, []byte(`{"password":`), 0600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("CONFIG_PATH", legacy)
			err := Init(path)
			if populated {
				if err != nil {
					t.Fatalf("populated database must win: %v", err)
				}
				if GetPassword() != "fixture-password" {
					t.Fatal("database replaced")
				}
			} else {
				if err == nil {
					t.Fatal("invalid legacy JSON created a fresh configuration")
				}
				if _, ok, _ := getSetting("config"); ok {
					t.Fatal("invalid import left partial config")
				}
			}
		})
	}
}

func TestCompatibilityCacheOverrideValidation(t *testing.T) {
	if err := Init(compatibilityDB(t)); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{`{"key":"sk-mode","enabled":true,"promptCacheEmulationMode":"invalid"}`, `{"key":"sk-ratio","enabled":true,"promptCacheEmulationRatio":1.1}`} {
		var entry ApiKeyEntry
		if err := json.Unmarshal([]byte(raw), &entry); err != nil {
			t.Fatal(err)
		}
		if _, err := AddApiKey(entry); err == nil {
			t.Error("invalid cache override accepted")
		}
	}
}

func TestCompatibilityAccountBindingAndRotatedTokenDeduplication(t *testing.T) {
	for _, separate := range []bool{false, true} {
		t.Run(fmt.Sprint(separate), func(t *testing.T) {
			if err := Init(compatibilityDB(t)); err != nil {
				t.Fatal(err)
			}
			if separate {
				if err := ReplaceCredentials(true, nil); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := AddApiKey(ApiKeyEntry{ID: "disabled", Key: "sk-disabled", Enabled: false}); err != nil {
				t.Fatal(err)
			}
			var bad Account
			_ = json.Unmarshal([]byte(`{"id":"bad","apiKeyId":"disabled","refreshToken":"bad-token"}`), &bad)
			if err := AddAccount(bad); err == nil {
				t.Error("binding to a disabled key accepted")
			}
			if err := AddAccount(Account{ID: "one", RefreshToken: "original"}); err != nil {
				t.Fatal(err)
			}
			if err := UpdateAccountToken("one", "access", "rotated", 123); err != nil {
				t.Fatal(err)
			}
			if err := AddAccount(Account{ID: "two", RefreshToken: "original"}); err == nil {
				t.Error("original refresh token imported after rotation")
			}
		})
	}
}

func TestCompatibilityLegacySQLiteNormalizationPersists(t *testing.T) {
	path := compatibilityDB(t)
	if err := db.Init(filepath.Dir(path)); err != nil {
		t.Fatal(err)
	}
	if err := setSetting("config", `{"password":"legacy","port":8321,"accounts":[],"apiKey":"legacy-key","requireApiKey":false}`); err != nil {
		t.Fatal(err)
	}
	if err := Init(path); err != nil {
		t.Fatal(err)
	}
	first := ListApiKeys()[0]
	if first.Enabled || !first.Migrated {
		t.Fatal("single key auth policy changed")
	}
	if err := Init(path); err != nil {
		t.Fatal(err)
	}
	if ListApiKeys()[0].ID != first.ID {
		t.Fatal("legacy key ID changes on each restart")
	}
}

func TestCompatibilityFingerprintForNativeCredentialsLoadedBeforeRotation(t *testing.T) {
	path := compatibilityDB(t)
	if err := Init(path); err != nil {
		t.Fatal(err)
	}
	if err := ReplaceCredentials(true, []Account{{ID: "native", RefreshToken: "original-native"}}); err != nil {
		t.Fatal(err)
	}
	if err := LoadCredentials(); err != nil {
		t.Fatal(err)
	}
	if err := UpdateCredentialToken("native", "new", "rotated-native", 10); err != nil {
		t.Fatal(err)
	}
	if !AccountCredentialExists("original-native") {
		t.Fatal("native credential's original token fingerprint lost")
	}
}

func TestCompatibilityCacheInheritanceAndSnapshots(t *testing.T) {
	if err := Init(compatibilityDB(t)); err != nil {
		t.Fatal(err)
	}
	if got := EffectivePromptCacheSettings(""); got != defaultPromptCacheSettings() {
		t.Fatalf("old-file defaults lost: %+v", got)
	}
	if err := UpdatePromptCacheSettings(false, "independent", 0.6, 0.2, 0.7); err != nil {
		t.Fatal(err)
	}
	enabled, ratio := true, 0.4
	key, err := AddApiKey(ApiKeyEntry{Key: "sk-cache", Enabled: true, PromptCacheEmulationEnabled: &enabled, PromptCacheReadEmulationRatio: &ratio})
	if err != nil {
		t.Fatal(err)
	}
	enabled = false
	ratio = 0.9
	got := EffectivePromptCacheSettings(key.ID)
	if !got.Enabled || got.CreationRatio != 0.2 || got.ReadRatio != 0.4 {
		t.Fatalf("inheritance or input cloning lost: %+v", got)
	}
	for _, snap := range []ApiKeyEntry{key, *GetApiKeyEntry(key.ID), ListApiKeys()[0], Get().ApiKeys[0], *FindApiKeyByValue(key.Key)} {
		*snap.PromptCacheEmulationEnabled = false
		*snap.PromptCacheReadEmulationRatio = 0.9
	}
	if EffectivePromptCacheSettings(key.ID) != got {
		t.Fatal("snapshot changed internal cache overrides")
	}
	if err := UpdateApiKeyAndClearPromptCacheOverrides(key.ID, ApiKeyEntry{Name: "cleared", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if GetApiKeyEntry(key.ID).PromptCacheReadEmulationRatio != nil || EffectivePromptCacheSettings(key.ID).ReadRatio != 0.7 {
		t.Fatal("clear did not resume inheritance")
	}
	for _, bad := range []float64{-0.1, 1.1, math.NaN(), math.Inf(1)} {
		if err := UpdatePromptCacheSettings(true, "uniform", bad, 0.3, 0.2); err == nil {
			t.Error("invalid ratio accepted")
		}
	}
	if err := UpdatePromptCacheSettings(true, "uniform", 0.3, 0.1, 0.2); err != nil {
		t.Fatal(err)
	}
	if s := EffectivePromptCacheSettings(key.ID); s.CreationRatio != 0.3 || s.ReadRatio != 0.3 {
		t.Fatal("uniform ratios not resolved")
	}
}

func TestCompatibilityBindingPatchPreservesOwnershipAndMarker(t *testing.T) {
	for _, separate := range []bool{false, true} {
		t.Run(fmt.Sprint(separate), func(t *testing.T) {
			path := compatibilityDB(t)
			if err := Init(path); err != nil {
				t.Fatal(err)
			}
			if separate {
				if err := ReplaceCredentials(true, nil); err != nil {
					t.Fatal(err)
				}
			}
			for _, entry := range []ApiKeyEntry{{ID: "one", Key: "sk-one", Enabled: true}, {ID: "two", Key: "sk-two", Enabled: true}, {ID: "disabled", Key: "sk-disabled", Enabled: false}} {
				if _, err := AddApiKey(entry); err != nil {
					t.Fatal(err)
				}
			}
			if err := AddAccount(Account{ID: "account", ApiKeyID: "one", RefreshToken: "original"}); err != nil {
				t.Fatal(err)
			}
			stale := GetAccounts()[0]
			if err := UpdateAccountApiKeyID("account", " two "); err != nil {
				t.Fatal(err)
			}
			nickname := "nickname"
			updated, err := UpdateAccountPatch("account", AccountPatch{Nickname: &nickname})
			if err != nil || updated.ApiKeyID != "two" {
				t.Fatal("omitted binding patch lost ownership")
			}
			if err := UpdateAccount("account", stale); err != nil {
				t.Fatal(err)
			}
			if GetAccounts()[0].ApiKeyID != "two" {
				t.Fatal("snapshot reverted newer binding")
			}
			if err := UpdateAccountApiKeyID("account", "disabled"); err == nil {
				t.Fatal("disabled binding accepted")
			}
			if err := DeleteApiKey("two"); err == nil {
				t.Fatal("bound API key deleted")
			}
			if err := UpdateAccountApiKeyID("account", ""); err != nil {
				t.Fatal(err)
			}
			if err := Init(path); err != nil {
				t.Fatal(err)
			}
			if GetAccounts()[0].ApiKeyID != "" || !Get().ApiKeyAccountMigrationCompleted {
				t.Fatal("explicit unbind did not survive restart")
			}
		})
	}
}

func TestCompatibilityAtomicSettersRollbackWhenSQLiteFails(t *testing.T) {
	for _, separate := range []bool{false, true} {
		t.Run(fmt.Sprint(separate), func(t *testing.T) {
			if err := Init(compatibilityDB(t)); err != nil {
				t.Fatal(err)
			}
			if separate {
				if err := ReplaceCredentials(true, nil); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := AddApiKey(ApiKeyEntry{ID: "owner", Key: "sk-owner", Enabled: true}); err != nil {
				t.Fatal(err)
			}
			if err := AddAccount(Account{ID: "account", ApiKeyID: "owner", RefreshToken: "original", Enabled: true, ProfileArn: "profile"}); err != nil {
				t.Fatal(err)
			}
			before := GetAccounts()[0]
			d, err := db.Get()
			if err != nil {
				t.Fatal(err)
			}
			if _, err = d.Exec(`CREATE TRIGGER reject_settings_update BEFORE UPDATE ON settings BEGIN SELECT RAISE(ABORT,'test write failure'); END`); err != nil {
				t.Fatal(err)
			}
			for name, mutate := range map[string]func() error{
				"token":   func() error { return UpdateAccountCredentialState("account", "new", "rotated", 10, "new-profile") },
				"enabled": func() error { return SetAccountEnabled("account", false) },
				"ban":     func() error { return SetAccountBanStatus("account", "BANNED", "reason") },
				"profile": func() error { return UpdateAccountProfileArn("account", "new-profile") },
				"delete":  func() error { return DeleteAccount("account") },
				"unbind":  func() error { return UpdateAccountApiKeyID("account", "") },
			} {
				if err := mutate(); err == nil {
					t.Errorf("%s persistence failure ignored", name)
				}
				accounts := GetAccounts()
				if len(accounts) != 1 || accounts[0] != before {
					t.Fatalf("%s published unpersisted account state", name)
				}
			}
		})
	}
}

func TestCompatibilityLegacyImportTransactionRollback(t *testing.T) {
	path := compatibilityDB(t)
	if err := db.Init(filepath.Dir(path)); err != nil {
		t.Fatal(err)
	}
	d, err := db.Get()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`CREATE TRIGGER reject_config_import BEFORE INSERT ON settings WHEN NEW.key='config' BEGIN SELECT RAISE(ABORT,'test import failure'); END`); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(filepath.Dir(path), "config.json")
	if err := os.WriteFile(legacy, []byte(`{"password":"legacy","port":8321,"accounts":[{"id":"a","refreshToken":"secret"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONFIG_PATH", legacy)
	if err := Init(path); err == nil {
		t.Fatal("import write failure ignored")
	}
	var count int
	if err := d.QueryRow(`SELECT COUNT(*) FROM settings`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("failed import left partial settings")
	}
}

func TestCompatibilityDataDirectoryResolution(t *testing.T) {
	if got := ResolveDataDir("explicit", filepath.Join("old", "config.json")); got != "explicit" {
		t.Fatal("DATA_DIR did not win")
	}
	if got := ResolveDataDir("", filepath.Join("old", "config.json")); got != "old" {
		t.Fatal("CONFIG_PATH directory ignored")
	}
	if got := ResolveDataDir("", ""); got != "." {
		t.Fatal("default data directory changed")
	}
}

func TestCompatibilityAPIKeyMutationKeepsNativeAutoSnapshots(t *testing.T) {
	if err := Init(compatibilityDB(t)); err != nil {
		t.Fatal(err)
	}
	if err := UpdateBackupConfig(BackupConfig{AutoEnabled: true, AutoKeep: 5}); err != nil {
		t.Fatal(err)
	}
	if _, err := AddApiKey(ApiKeyEntry{ID: "key", Key: "sk-key", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	backups, err := ListBackups(true)
	if err != nil {
		t.Fatal(err)
	}
	if len(backups) != 1 {
		t.Fatal("API key add stopped creating native auto snapshots")
	}
}

func TestCompatibilityPendingMigrationAndClearOnly(t *testing.T) {
	for _, separate := range []bool{false, true} {
		t.Run(fmt.Sprint(separate), func(t *testing.T) {
			if err := Init(compatibilityDB(t)); err != nil {
				t.Fatal(err)
			}
			if separate {
				if err := ReplaceCredentials(true, nil); err != nil {
					t.Fatal(err)
				}
			}
			if err := AddAccount(Account{ID: "legacy", RefreshToken: "refresh"}); err != nil {
				t.Fatal(err)
			}
			if _, err := AddApiKey(ApiKeyEntry{ID: "disabled", Key: "sk-disabled", Enabled: false}); err != nil {
				t.Fatal(err)
			}
			if Get().ApiKeyAccountMigrationCompleted {
				t.Fatal("migration completed without enabled key")
			}
			if err := ClearPromptCacheOverrides("disabled"); err != nil {
				t.Fatal(err)
			}
			if GetAccounts()[0].ApiKeyID != "" || Get().ApiKeyAccountMigrationCompleted {
				t.Fatal("clear-only attempted ownership migration")
			}
			if err := UpdateApiKey("disabled", ApiKeyEntry{Enabled: true}); err != nil {
				t.Fatal(err)
			}
			if GetAccounts()[0].ApiKeyID != "disabled" || !Get().ApiKeyAccountMigrationCompleted {
				t.Fatal("first enabled key did not migrate legacy accounts")
			}
		})
	}
}

func TestCompatibilityDualStoreMutationRollsBackOnCredentialWriteFailure(t *testing.T) {
	if err := Init(compatibilityDB(t)); err != nil {
		t.Fatal(err)
	}
	if err := ReplaceCredentials(true, []Account{{ID: "account", RefreshToken: "original"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := AddApiKey(ApiKeyEntry{ID: "key", Key: "sk-key", Enabled: false}); err != nil {
		t.Fatal(err)
	}
	d, err := db.Get()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`CREATE TRIGGER reject_credential_update BEFORE UPDATE ON settings WHEN NEW.key='credentials' BEGIN SELECT RAISE(ABORT,'test credentials failure'); END`); err != nil {
		t.Fatal(err)
	}
	before := GetAccounts()[0]
	if err := UpdateApiKey("key", ApiKeyEntry{Enabled: true}); err == nil {
		t.Fatal("credential persistence failure ignored")
	}
	if Get().ApiKeyAccountMigrationCompleted || GetApiKeyEntry("key").Enabled || GetAccounts()[0] != before {
		t.Fatal("failed migration published partial in-memory state")
	}
	var stored Config
	raw, _, err := getSetting("config")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(raw), &stored); err != nil {
		t.Fatal(err)
	}
	if stored.ApiKeyAccountMigrationCompleted || stored.ApiKeys[0].Enabled {
		t.Fatal("SQLite transaction persisted partial migration")
	}
}

func TestCompatibilityCacheCombinedPatchRollback(t *testing.T) {
	if err := Init(compatibilityDB(t)); err != nil {
		t.Fatal(err)
	}
	ratio := 0.4
	if _, err := AddApiKey(ApiKeyEntry{ID: "key", Key: "sk-key", Enabled: true, Name: "before", PromptCacheReadEmulationRatio: &ratio}); err != nil {
		t.Fatal(err)
	}
	d, err := db.Get()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`CREATE TRIGGER reject_config_update BEFORE UPDATE ON settings BEGIN SELECT RAISE(ABORT,'test config failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := UpdateApiKeyAndClearPromptCacheOverrides("key", ApiKeyEntry{Name: "after", Enabled: true}); err == nil {
		t.Fatal("persistence failure ignored")
	}
	if key := GetApiKeyEntry("key"); key.Name != "before" || key.PromptCacheReadEmulationRatio == nil || *key.PromptCacheReadEmulationRatio != 0.4 {
		t.Fatal("combined cache update did not roll back")
	}
}

func TestCompatibilityLegacyImportMaterializesHistoricalModelMappings(t *testing.T) {
	path := compatibilityDB(t)
	legacy := filepath.Join(filepath.Dir(path), "config.json")
	if err := os.WriteFile(legacy, []byte(`{"password":"legacy","port":8321,"accounts":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONFIG_PATH", legacy)
	if err := Init(path); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, mapping := range GetModelMappings() {
		got[mapping.Key] = mapping.Value
	}
	if got["claude-sonnet-4-20250514"] != "claude-sonnet-4" || got["gpt-4o"] != "claude-sonnet-4.5" {
		t.Fatal("legacy import silently adopted the new model redirects")
	}
}

func TestCompatibilityLegacyImportPreservesExplicitEmptyMappings(t *testing.T) {
	path := compatibilityDB(t)
	legacy := filepath.Join(filepath.Dir(path), "config.json")
	if err := os.WriteFile(legacy, []byte(`{"password":"legacy","port":8321,"accounts":[],"modelMappings":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Init(legacy); err != nil {
		t.Fatal(err)
	}
	if len(GetModelMappings()) != 0 {
		t.Fatal("explicit empty mappings replaced by defaults")
	}
	if err := Init(path); err != nil {
		t.Fatal(err)
	}
	if len(GetModelMappings()) != 0 {
		t.Fatal("explicit empty mappings lost on restart")
	}
}
