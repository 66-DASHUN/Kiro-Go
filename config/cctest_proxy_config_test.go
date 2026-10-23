package config

import (
	"path/filepath"
	"testing"
)

func TestUpdateCCTestProxyConfigPreservesOmittedKeyAndCanClear(t *testing.T) {
	if err := Init(filepath.Join(configTempDir(t), "kiro.db")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	baseURL := "https://upstream.example/v1"
	apiKey := "upstream-secret"
	if err := UpdateCCTestProxyConfig(&baseURL, &apiKey, false); err != nil {
		t.Fatalf("save CCTest proxy config: %v", err)
	}

	updatedURL := "https://another-upstream.example/v1"
	if err := UpdateCCTestProxyConfig(&updatedURL, nil, false); err != nil {
		t.Fatalf("update CCTest proxy URL: %v", err)
	}
	got := Get()
	if got.CCTestProxyBaseURL != updatedURL || got.CCTestProxyAPIKey != apiKey {
		t.Fatalf("URL update should preserve the saved key")
	}

	if err := UpdateCCTestProxyConfig(nil, nil, true); err != nil {
		t.Fatalf("clear CCTest proxy key: %v", err)
	}
	got = Get()
	if got.CCTestProxyBaseURL != updatedURL || got.CCTestProxyAPIKey != "" {
		t.Fatalf("clear key should preserve URL and clear only the key")
	}
}

func TestUpdateCCTestProxyConfigRejectsUnsafeValuesWithoutChangingState(t *testing.T) {
	if err := Init(filepath.Join(configTempDir(t), "kiro.db")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	baseURL := "https://upstream.example/v1"
	apiKey := "upstream-secret"
	if err := UpdateCCTestProxyConfig(&baseURL, &apiKey, false); err != nil {
		t.Fatalf("seed CCTest proxy config: %v", err)
	}

	for _, tc := range []struct {
		name    string
		baseURL *string
		apiKey  *string
	}{
		{name: "HTTP URL", baseURL: stringPtr("http://upstream.example")},
		{name: "URL credentials", baseURL: stringPtr("https://user:pass@upstream.example")},
		{name: "URL query", baseURL: stringPtr("https://upstream.example?token=secret")},
		{name: "API key line break", apiKey: stringPtr("secret\nforged")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := UpdateCCTestProxyConfig(tc.baseURL, tc.apiKey, false); err == nil {
				t.Fatal("expected invalid CCTest proxy value to be rejected")
			}
			got := Get()
			if got.CCTestProxyBaseURL != baseURL || got.CCTestProxyAPIKey != apiKey {
				t.Fatal("rejected update changed the saved CCTest proxy config")
			}
		})
	}
}

func stringPtr(value string) *string { return &value }
