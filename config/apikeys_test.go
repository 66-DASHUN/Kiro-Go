package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

func TestApiKeyMigrationFromLegacyField(t *testing.T) {
	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "config.json")

	// Seed a config file in the legacy shape (no apiKeys list, single ApiKey field).
	seed := map[string]interface{}{
		"password":      "p",
		"port":          8080,
		"host":          "0.0.0.0",
		"apiKey":        "legacy-secret",
		"requireApiKey": true,
		"accounts":      []interface{}{},
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

	keys := ListApiKeys()
	if len(keys) != 1 {
		t.Fatalf("expected one migrated key, got %d", len(keys))
	}
	migrated := keys[0]
	if migrated.Key != "legacy-secret" {
		t.Fatalf("expected migrated key value, got %q", migrated.Key)
	}
	if !migrated.Migrated {
		t.Fatalf("expected migrated flag to be true")
	}
	if !migrated.Enabled {
		t.Fatalf("expected migrated key to be enabled")
	}
	if migrated.ID == "" {
		t.Fatalf("expected migrated key to have an ID")
	}

	// Reload and confirm migration was persisted (no second migration entry appears).
	if err := Init(cfgFile); err != nil {
		t.Fatalf("re-init: %v", err)
	}
	if got := len(ListApiKeys()); got != 1 {
		t.Fatalf("expected migration to be idempotent, got %d entries", got)
	}
}

// Public deployments (RequireApiKey=false) must not silently start enforcing
// auth after upgrade. The migrated legacy entry is created disabled so the
// service stays open until an operator explicitly toggles auth on.
func TestApiKeyMigrationPublicDeploymentStaysDisabled(t *testing.T) {
	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "config.json")

	seed := map[string]interface{}{
		"password":      "p",
		"port":          8080,
		"host":          "0.0.0.0",
		"apiKey":        "legacy-secret",
		"requireApiKey": false,
		"accounts":      []interface{}{},
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

	keys := ListApiKeys()
	if len(keys) != 1 {
		t.Fatalf("expected one migrated key, got %d", len(keys))
	}
	if keys[0].Enabled {
		t.Fatalf("expected migrated key to be disabled when legacy deployment was public")
	}
	if !keys[0].Migrated {
		t.Fatalf("expected migrated flag to remain set")
	}
}

func TestApiKeyCRUD(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := Init(cfgFile); err != nil {
		t.Fatalf("init: %v", err)
	}

	created, err := AddApiKey(ApiKeyEntry{Name: "alpha", Key: "sk-alpha", Enabled: true, TokenLimit: 1000})
	if err != nil {
		t.Fatalf("add alpha: %v", err)
	}
	if created.ID == "" {
		t.Fatalf("expected ID to be assigned")
	}
	if created.CreatedAt == 0 {
		t.Fatalf("expected CreatedAt to be set")
	}

	if _, err := AddApiKey(ApiKeyEntry{Name: "dup", Key: "sk-alpha", Enabled: true}); err == nil {
		t.Fatalf("expected duplicate add to fail")
	}

	if _, err := AddApiKey(ApiKeyEntry{Name: "empty", Key: "", Enabled: true}); err == nil {
		t.Fatalf("expected empty key add to fail")
	}

	if err := UpdateApiKey(created.ID, ApiKeyEntry{
		Name:        "alpha-renamed",
		Enabled:     false,
		TokenLimit:  2000,
		CreditLimit: 5.5,
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	got := GetApiKeyEntry(created.ID)
	if got == nil {
		t.Fatalf("expected entry to exist after update")
	}
	if got.Name != "alpha-renamed" {
		t.Fatalf("expected name to be updated, got %q", got.Name)
	}
	if got.Enabled {
		t.Fatalf("expected enabled to be flipped off")
	}
	if got.TokenLimit != 2000 || got.CreditLimit != 5.5 {
		t.Fatalf("expected limits to be updated, got token=%d credit=%v", got.TokenLimit, got.CreditLimit)
	}
	if got.Key != "sk-alpha" {
		t.Fatalf("expected key value to remain unchanged when patch.Key is empty, got %q", got.Key)
	}

	if found := FindApiKeyByValue("sk-alpha"); found == nil || found.ID != created.ID {
		t.Fatalf("FindApiKeyByValue should locate the entry")
	}
	if found := FindApiKeyByValue("does-not-exist"); found != nil {
		t.Fatalf("FindApiKeyByValue should return nil for unknown keys")
	}

	if err := DeleteApiKey(created.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if got := GetApiKeyEntry(created.ID); got != nil {
		t.Fatalf("expected entry to be removed")
	}
	if len(ListApiKeys()) != 0 {
		t.Fatalf("expected list to be empty after delete")
	}
}

func TestRecordApiKeyUsageConcurrent(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := Init(cfgFile); err != nil {
		t.Fatalf("init: %v", err)
	}
	created, err := AddApiKey(ApiKeyEntry{Name: "race", Key: "sk-race", Enabled: true})
	if err != nil {
		t.Fatalf("add: %v", err)
	}

	const goroutines = 16
	const perGoroutine = 25
	var wg sync.WaitGroup
	wg.Add(goroutines)

	var failures int32
	for g := 0; g < goroutines; g++ {
		go func() {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				if err := RecordApiKeyUsage(created.ID, 7, 0.5); err != nil {
					atomic.AddInt32(&failures, 1)
					return
				}
			}
		}()
	}
	wg.Wait()

	if failures != 0 {
		t.Fatalf("RecordApiKeyUsage encountered %d errors", failures)
	}
	got := GetApiKeyEntry(created.ID)
	if got == nil {
		t.Fatalf("entry missing after concurrent updates")
	}
	expectedTokens := int64(goroutines * perGoroutine * 7)
	expectedCredits := float64(goroutines*perGoroutine) * 0.5
	expectedRequests := int64(goroutines * perGoroutine)
	if got.TokensUsed != expectedTokens {
		t.Fatalf("TokensUsed mismatch: got %d want %d", got.TokensUsed, expectedTokens)
	}
	if got.CreditsUsed != expectedCredits {
		t.Fatalf("CreditsUsed mismatch: got %v want %v", got.CreditsUsed, expectedCredits)
	}
	if got.RequestsCount != expectedRequests {
		t.Fatalf("RequestsCount mismatch: got %d want %d", got.RequestsCount, expectedRequests)
	}
}

func TestResetApiKeyUsage(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := Init(cfgFile); err != nil {
		t.Fatalf("init: %v", err)
	}
	created, err := AddApiKey(ApiKeyEntry{Name: "reset", Key: "sk-reset", Enabled: true})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := RecordApiKeyUsage(created.ID, 100, 1.5); err != nil {
		t.Fatalf("record: %v", err)
	}
	if err := ResetApiKeyUsage(created.ID); err != nil {
		t.Fatalf("reset: %v", err)
	}
	got := GetApiKeyEntry(created.ID)
	if got == nil {
		t.Fatalf("entry missing")
	}
	if got.TokensUsed != 0 || got.CreditsUsed != 0 || got.RequestsCount != 0 {
		t.Fatalf("expected counters to be zeroed, got %+v", got)
	}
}

func TestApiKeyOverLimit(t *testing.T) {
	tests := []struct {
		name       string
		entry      ApiKeyEntry
		wantToken  bool
		wantCredit bool
	}{
		{"unlimited", ApiKeyEntry{TokensUsed: 100, CreditsUsed: 5}, false, false},
		{"under token limit", ApiKeyEntry{TokenLimit: 200, TokensUsed: 100}, false, false},
		{"at token limit", ApiKeyEntry{TokenLimit: 100, TokensUsed: 100}, true, false},
		{"over token limit", ApiKeyEntry{TokenLimit: 100, TokensUsed: 150}, true, false},
		{"over credit limit", ApiKeyEntry{CreditLimit: 1, CreditsUsed: 2}, false, true},
		{"both over", ApiKeyEntry{TokenLimit: 1, TokensUsed: 2, CreditLimit: 1, CreditsUsed: 2}, true, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotT, gotC := ApiKeyOverLimit(tc.entry)
			if gotT != tc.wantToken || gotC != tc.wantCredit {
				t.Fatalf("ApiKeyOverLimit(%+v) = (%v,%v), want (%v,%v)",
					tc.entry, gotT, gotC, tc.wantToken, tc.wantCredit)
			}
		})
	}
}

func TestMaskApiKey(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"short", "short"},
		{"sk-1234567890", "sk-123****7890"},
	}
	for _, tc := range tests {
		if got := MaskApiKey(tc.in); got != tc.want {
			t.Fatalf("MaskApiKey(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestGenerateApiKeyValueIsUnique(t *testing.T) {
	a := GenerateApiKeyValue()
	b := GenerateApiKeyValue()
	if a == b {
		t.Fatalf("expected unique generated keys, got identical %q", a)
	}
	if len(a) < 10 {
		t.Fatalf("expected non-trivial key length, got %q", a)
	}
}

func TestPromptCacheSettingsInheritGlobalDefaults(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := Init(cfgFile); err != nil {
		t.Fatalf("init: %v", err)
	}

	settings := EffectivePromptCacheSettings("missing")
	if !settings.Enabled {
		t.Fatal("expected prompt cache emulation to be enabled by default")
	}
	if settings.Mode != "uniform" {
		t.Fatalf("mode=%q, want uniform", settings.Mode)
	}
	if settings.Ratio != 1 || settings.CreationRatio != 1 || settings.ReadRatio != 1 {
		t.Fatalf("settings=%+v, want all ratios 1", settings)
	}
}

func TestPromptCacheSettingsUseAPIKeyOverrideAndInheritMissingFields(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := Init(cfgFile); err != nil {
		t.Fatalf("init: %v", err)
	}
	globalRatio := 0.8
	globalMode := "independent"
	if err := UpdatePromptCacheSettings(true, globalMode, globalRatio, 0.6, 0.4); err != nil {
		t.Fatalf("update global settings: %v", err)
	}
	keyRatio := 0.7
	entry, err := AddApiKey(ApiKeyEntry{
		Key:                       "sk-cache",
		Enabled:                   true,
		PromptCacheEmulationRatio: &keyRatio,
	})
	if err != nil {
		t.Fatalf("add key: %v", err)
	}

	settings := EffectivePromptCacheSettings(entry.ID)
	if settings.Ratio != 0.7 {
		t.Fatalf("ratio=%v, want 0.7", settings.Ratio)
	}
	if settings.CreationRatio != 0.6 || settings.ReadRatio != 0.4 {
		t.Fatalf("settings=%+v, want independent fields to inherit global ratios", settings)
	}

	missing := EffectivePromptCacheSettings("unknown")
	if missing.Ratio != globalRatio || missing.CreationRatio != 0.6 || missing.ReadRatio != 0.4 {
		t.Fatalf("missing key settings=%+v, want global settings", missing)
	}
}

func TestPromptCacheSettingsRejectInvalidValues(t *testing.T) {
	for _, ratio := range []float64{-0.01, 1.01} {
		if err := ValidatePromptCacheRatio(ratio); err == nil {
			t.Fatalf("expected ratio %v to be rejected", ratio)
		}
	}
	if err := ValidatePromptCacheSettings(PromptCacheSettings{Enabled: true, Mode: "invalid", Ratio: 1, CreationRatio: 1, ReadRatio: 1}); err == nil {
		t.Fatal("expected invalid mode to be rejected")
	}
}

func TestPromptCacheSettingsLoadLegacyJSONDefaults(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	legacy := []byte(`{"password":"p","port":8080,"host":"0.0.0.0","accounts":[],"apiKeys":[{"id":"legacy-key","key":"sk-legacy","enabled":true}]}`)
	if err := os.WriteFile(cfgFile, legacy, 0600); err != nil {
		t.Fatalf("write legacy config: %v", err)
	}
	if err := Init(cfgFile); err != nil {
		t.Fatalf("init legacy config: %v", err)
	}
	settings := EffectivePromptCacheSettings("legacy-key")
	if !settings.Enabled || settings.Mode != "uniform" || settings.Ratio != 1 || settings.CreationRatio != 1 || settings.ReadRatio != 1 {
		t.Fatalf("legacy settings=%+v, want default settings", settings)
	}
}

func TestApiKeyPromptCacheOverridesAreDeepCopied(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init: %v", err)
	}
	ratio := 0.25
	entry, err := AddApiKey(ApiKeyEntry{Key: "sk-copy", Enabled: true, PromptCacheEmulationRatio: &ratio})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	ratio = 0.75
	if got := *GetApiKeyEntry(entry.ID).PromptCacheEmulationRatio; got != 0.25 {
		t.Fatalf("stored ratio changed through add input pointer: got %v", got)
	}

	listed := ListApiKeys()
	*listed[0].PromptCacheEmulationRatio = 0.9
	found := FindApiKeyByValue("sk-copy")
	if got := *found.PromptCacheEmulationRatio; got != 0.25 {
		t.Fatalf("stored ratio changed through list output pointer: got %v", got)
	}
	got := GetApiKeyEntry(entry.ID)
	*got.PromptCacheEmulationRatio = 0.8
	if got := *FindApiKeyByValue("sk-copy").PromptCacheEmulationRatio; got != 0.25 {
		t.Fatalf("stored ratio changed through get output pointer: got %v", got)
	}
}

func TestPromptCacheSettingsExplicitNullUsesDefaults(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	legacy := []byte(`{"password":"p","port":8080,"host":"0.0.0.0","accounts":[],"promptCacheEmulationEnabled":null,"promptCacheEmulationMode":null,"promptCacheEmulationRatio":null,"promptCacheCreationEmulationRatio":null,"promptCacheReadEmulationRatio":null}`)
	if err := os.WriteFile(cfgFile, legacy, 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if err := Init(cfgFile); err != nil {
		t.Fatalf("init: %v", err)
	}
	got := EffectivePromptCacheSettings("")
	want := defaultPromptCacheSettings()
	if got != want {
		t.Fatalf("settings=%+v, want defaults=%+v", got, want)
	}
}

func TestApiKeyPromptCacheOverridesPersistAcrossAddUpdateAndReload(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := Init(cfgFile); err != nil {
		t.Fatalf("init: %v", err)
	}
	ratio := 0.3
	created, err := AddApiKey(ApiKeyEntry{Key: "sk-persist", Enabled: true, PromptCacheEmulationRatio: &ratio})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	updated := 0.4
	if err := UpdateApiKey(created.ID, ApiKeyEntry{Enabled: true, PromptCacheEmulationRatio: &updated}); err != nil {
		t.Fatalf("update: %v", err)
	}
	updated = 0.9
	if err := Init(cfgFile); err != nil {
		t.Fatalf("reload: %v", err)
	}
	got := GetApiKeyEntry(created.ID)
	if got == nil || got.PromptCacheEmulationRatio == nil || *got.PromptCacheEmulationRatio != 0.4 {
		t.Fatalf("persisted override=%+v, want 0.4", got)
	}
}

func TestClearPromptCacheOverridesRestoresInheritance(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init: %v", err)
	}
	enabled := false
	mode := "independent"
	ratio, creation, read := 0.2, 0.3, 0.4
	created, err := AddApiKey(ApiKeyEntry{
		Key: "sk-clear", Enabled: true,
		PromptCacheEmulationEnabled: &enabled, PromptCacheEmulationMode: &mode,
		PromptCacheEmulationRatio: &ratio, PromptCacheCreationEmulationRatio: &creation,
		PromptCacheReadEmulationRatio: &read,
	})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := ClearPromptCacheOverrides(created.ID); err != nil {
		t.Fatalf("clear: %v", err)
	}
	got := GetApiKeyEntry(created.ID)
	if got == nil || got.PromptCacheEmulationEnabled != nil || got.PromptCacheEmulationMode != nil ||
		got.PromptCacheEmulationRatio != nil || got.PromptCacheCreationEmulationRatio != nil || got.PromptCacheReadEmulationRatio != nil {
		t.Fatalf("overrides were not cleared: %+v", got)
	}
}
