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
	if got := EffectivePromptCacheSettings(key.ID); got.Mode != mode || !got.Enabled {
		t.Fatalf("lost lifecycle policy: %+v", got)
	}
}
