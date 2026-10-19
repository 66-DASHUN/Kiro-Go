package config

import "testing"

func TestPrefixDefaultAndLegacySettings(t *testing.T) {
	path := compatibilityDB(t)
	if err := Init(path); err != nil {
		t.Fatal(err)
	}
	if got := EffectivePromptCacheSettings(""); got.Mode != "sub2api" || got.Ratio != .95 {
		t.Fatalf("new default is not total cache rate: %+v", got)
	}
	for _, mode := range []string{"sub2api", "prefix_total", "prefix", "uniform", "independent"} {
		if err := UpdatePromptCacheSettings(true, mode, .8, .3, .4); err != nil {
			t.Fatal(err)
		}
		if err := Init(path); err != nil {
			t.Fatal(err)
		}
		got := EffectivePromptCacheSettings("")
		if got.Mode != mode {
			t.Fatalf("explicit mode changed after reload: %+v", got)
		}
		if mode == "prefix" && (got.Ratio != .8 || got.ReadRatio != .8 || got.CreationRatio != 1) {
			t.Fatalf("prefix ratio semantics lost: %+v", got)
		}
	}
}

func TestPrefixPerKeyPolicyPersists(t *testing.T) {
	path := compatibilityDB(t)
	if err := Init(path); err != nil {
		t.Fatal(err)
	}
	mode, ratio, disabled := "prefix", .8, false
	key, err := AddApiKey(ApiKeyEntry{ID: "prefix-key", Key: "sk-prefix-fixture", Enabled: true, PromptCacheEmulationMode: &mode, PromptCacheEmulationRatio: &ratio})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AddApiKey(ApiKeyEntry{ID: "disabled-key", Key: "sk-disabled-fixture", Enabled: true, PromptCacheEmulationEnabled: &disabled}); err != nil {
		t.Fatal(err)
	}
	if err := Init(path); err != nil {
		t.Fatal(err)
	}
	if got := EffectivePromptCacheSettings(key.ID); got.Mode != "prefix" || got.ReadRatio != .8 || got.CreationRatio != 1 {
		t.Fatalf("per-Key prefix not persisted: %+v", got)
	}
	if got := EffectivePromptCacheSettings("disabled-key"); got.Enabled {
		t.Fatal("disabled override lost")
	}
	if err := ClearPromptCacheOverrides(key.ID); err != nil {
		t.Fatal(err)
	}
	if got := EffectivePromptCacheSettings(key.ID); got.Mode != "sub2api" || got.Ratio != .95 {
		t.Fatalf("inheritance lost: %+v", got)
	}
}
