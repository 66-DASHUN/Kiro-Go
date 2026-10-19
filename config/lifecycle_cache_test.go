package config

import "testing"

func TestLifecycleCacheModePersists(t *testing.T) {
	path := compatibilityDB(t)
	if err := Init(path); err != nil {
		t.Fatal(err)
	}
	if err := UpdatePromptCacheSettings(true, "prefix_lifecycle", .9, .1, .2); err != nil {
		t.Fatal(err)
	}
	mode := "prefix_lifecycle"
	key, err := AddApiKey(ApiKeyEntry{ID: "lifecycle", Key: "sk-lifecycle-test", Enabled: true, PromptCacheEmulationMode: &mode})
	if err != nil {
		t.Fatal(err)
	}
	if err := Init(path); err != nil {
		t.Fatal(err)
	}
	if got := EffectivePromptCacheSettings(key.ID); got.Mode != mode || !got.Enabled || got.Ratio != .9 || got.APIKeyID != key.ID {
		t.Fatalf("lost lifecycle policy: %+v", got)
	}
	ratio := .7
	if err := UpdateApiKey(key.ID, ApiKeyEntry{Enabled: true, PromptCacheEmulationRatio: &ratio}); err != nil {
		t.Fatal(err)
	}
	if err := Init(path); err != nil {
		t.Fatal(err)
	}
	if got := EffectivePromptCacheSettings(key.ID); got.Mode != mode || got.Ratio != .7 {
		t.Fatalf("per-Key admission target did not persist: %+v", got)
	}
	if got := EffectivePromptCacheSettings(""); got.Ratio != .9 {
		t.Fatalf("per-Key override changed global target: %+v", got)
	}
}
