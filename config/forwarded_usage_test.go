package config

import (
	"path/filepath"
	"testing"
)

func TestForwardedUsageAppliesActualTokenLimitWithoutCredits(t *testing.T) {
	if err := Init(filepath.Join(configTempDir(t), "kiro.db")); err != nil {
		t.Fatal(err)
	}
	entry, err := AddApiKey(ApiKeyEntry{Key: "sk-metered", Enabled: true, TokenLimit: 10, CreditLimit: 9})
	if err != nil {
		t.Fatal(err)
	}
	if err = RecordApiKeyForwardedUsage(entry.ID, 10); err != nil {
		t.Fatal(err)
	}
	got := GetApiKeyEntry(entry.ID)
	if got.Enabled || got.TokensUsed != 10 || got.CreditsUsed != 0 || got.RequestsCount != 1 {
		t.Fatalf("unexpected forwarded usage: %+v", got)
	}
}
