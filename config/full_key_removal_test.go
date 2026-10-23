package config

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func TestRemovedFullKeyConfigIsDroppedAndProbeSwitchPreserved(t *testing.T) {
	path := filepath.Join(configTempDir(t), "kiro.db")
	if err := Init(path); err != nil {
		t.Fatal(err)
	}
	var entry ApiKeyEntry
	if err := json.Unmarshal([]byte(`{"key":"legacy-fixture","enabled":true,"cctestTextHandlingEnabled":true,"upstreamForward":{"enabled":true,"baseUrl":"https://old.example","apiKey":"old-fixture-secret"}}`), &entry); err != nil {
		t.Fatal(err)
	}
	entry, err := AddApiKey(entry)
	if err != nil {
		t.Fatalf("legacy full-key config must not interfere with probe switch: %v", err)
	}
	if err := Init(path); err != nil {
		t.Fatal(err)
	}
	got := GetApiKeyEntry(entry.ID)
	if got == nil || !got.CCTestTextHandling() {
		t.Fatal("probe switch or key lost")
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "upstreamForward") || strings.Contains(string(raw), "old-fixture-secret") {
		t.Fatal("removed setting or credential persisted")
	}
}
