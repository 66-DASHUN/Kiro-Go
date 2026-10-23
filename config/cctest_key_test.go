package config

import (
	"path/filepath"
	"testing"
)

func TestCCTestKeySwitchPersistenceAndPatch(t *testing.T) {
	path := filepath.Join(configTempDir(t), "kiro.db")
	if err := Init(path); err != nil {
		t.Fatal(err)
	}
	on := true
	entry, err := AddApiKey(ApiKeyEntry{Key: "shared-fixture", Enabled: true, CCTestTextHandlingEnabled: &on})
	if err != nil {
		t.Fatal(err)
	}
	on = false
	if !GetApiKeyEntry(entry.ID).CCTestTextHandling() {
		t.Fatal("switch aliases caller")
	}
	snapshot := GetApiKeyEntry(entry.ID)
	*snapshot.CCTestTextHandlingEnabled = false
	if !GetApiKeyEntry(entry.ID).CCTestTextHandling() {
		t.Fatal("switch aliases returned snapshot")
	}
	if err := UpdateApiKey(entry.ID, ApiKeyEntry{Enabled: true, Name: "new name"}); err != nil {
		t.Fatal(err)
	}
	if err := Init(path); err != nil {
		t.Fatal(err)
	}
	if !GetApiKeyEntry(entry.ID).CCTestTextHandling() {
		t.Fatal("switch lost across patch/reload")
	}
	if err := UpdateApiKey(entry.ID, ApiKeyEntry{Enabled: true, CCTestTextHandlingEnabled: &on}); err != nil {
		t.Fatal(err)
	}
	if GetApiKeyEntry(entry.ID).CCTestTextHandling() {
		t.Fatal("explicit false did not disable")
	}
	if (ApiKeyEntry{}).CCTestTextHandling() {
		t.Fatal("legacy entry enabled by default")
	}
}

func TestCCTestKeyRejectsFullKeyForwardingConflict(t *testing.T) {
	if err := Init(filepath.Join(configTempDir(t), "kiro.db")); err != nil {
		t.Fatal(err)
	}
	on := true
	full := &UpstreamForwardConfig{Enabled: true, BaseURL: "https://upstream.example", APIKey: "fixture"}
	if _, err := AddApiKey(ApiKeyEntry{Key: "conflict", Enabled: true, CCTestTextHandlingEnabled: &on, UpstreamForward: full}); err == nil {
		t.Fatal("conflicting modes allowed on creation")
	}
	entry, err := AddApiKey(ApiKeyEntry{Key: "shared", Enabled: true, CCTestTextHandlingEnabled: &on})
	if err != nil {
		t.Fatal(err)
	}
	if err := UpdateApiKey(entry.ID, ApiKeyEntry{Enabled: true, UpstreamForward: full}); err == nil {
		t.Fatal("conflicting patch allowed")
	}
	if GetApiKeyEntry(entry.ID).UpstreamForward != nil {
		t.Fatal("rejected patch changed stored route")
	}
}
