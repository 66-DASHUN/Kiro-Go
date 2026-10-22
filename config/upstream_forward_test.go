package config

import (
	"path/filepath"
	"testing"
)

func TestUpstreamForwardPersistenceAndIsolation(t *testing.T) {
	path := filepath.Join(configTempDir(t), "kiro.db")
	if err := Init(path); err != nil {
		t.Fatal(err)
	}
	forward := &UpstreamForwardConfig{Enabled: true, BaseURL: "https://upstream.example/v1", APIKey: "upstream-secret"}
	entry, err := AddApiKey(ApiKeyEntry{Key: "sk-forward", Enabled: true, UpstreamForward: forward})
	if err != nil {
		t.Fatal(err)
	}
	forward.APIKey = "mutated"
	snapshot := GetApiKeyEntry(entry.ID)
	if snapshot.UpstreamForward.APIKey != "upstream-secret" {
		t.Fatal("stored config aliases caller")
	}
	snapshot.UpstreamForward.APIKey = "changed"
	if GetApiKeyEntry(entry.ID).UpstreamForward.APIKey != "upstream-secret" {
		t.Fatal("snapshot aliases config")
	}
	if err := UpdateApiKey(entry.ID, ApiKeyEntry{Enabled: true, Name: "renamed"}); err != nil {
		t.Fatal(err)
	}
	if err := Init(path); err != nil {
		t.Fatal(err)
	}
	got := GetApiKeyEntry(entry.ID)
	if got.UpstreamForward == nil || got.UpstreamForward.APIKey != "upstream-secret" {
		t.Fatal("forward config lost across update/restart")
	}
}

func TestUpstreamForwardRejectsInvalidConfiguration(t *testing.T) {
	if err := Init(filepath.Join(configTempDir(t), "kiro.db")); err != nil {
		t.Fatal(err)
	}
	for _, c := range []UpstreamForwardConfig{
		{Enabled: true},
		{Enabled: true, BaseURL: "http://upstream.example", APIKey: "secret"},
		{Enabled: true, BaseURL: "https://user:pass@upstream.example", APIKey: "secret"},
		{Enabled: true, BaseURL: "https://upstream.example?key=secret", APIKey: "secret"},
		{Enabled: true, BaseURL: "https://upstream.example", APIKey: "bad\r\nsecret"},
	} {
		if _, err := AddApiKey(ApiKeyEntry{Key: "sk-invalid", Enabled: true, UpstreamForward: &c}); err == nil {
			t.Fatal("invalid upstream accepted")
		}
	}
}

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
