package config

import "testing"

func TestTotalCacheDefaultAndExplicitLegacyPersistence(t *testing.T) {
	path := compatibilityDB(t)
	if err := Init(path); err != nil {
		t.Fatal(err)
	}
	if got := EffectivePromptCacheSettings(""); got.Mode != "sub2api" || got.Ratio != .95 {
		t.Fatalf("new default must control total cache rate: %+v", got)
	}
	for _, mode := range []string{"prefix_total", "prefix", "uniform", "independent"} {
		if err := UpdatePromptCacheSettings(true, mode, .8, .3, .4); err != nil {
			t.Fatal(err)
		}
		if err := Init(path); err != nil {
			t.Fatal(err)
		}
		if got := EffectivePromptCacheSettings(""); got.Mode != mode || got.Ratio != .8 {
			t.Fatalf("explicit mode changed: %+v", got)
		}
	}
	mode, ratio := "prefix_total", .8
	key, err := AddApiKey(ApiKeyEntry{ID: "total-key", Key: "sk-total-fixture", Enabled: true, PromptCacheEmulationMode: &mode, PromptCacheEmulationRatio: &ratio})
	if err != nil {
		t.Fatal(err)
	}
	if err := Init(path); err != nil {
		t.Fatal(err)
	}
	if got := EffectivePromptCacheSettings(key.ID); got.Mode != mode || got.Ratio != ratio {
		t.Fatalf("key override lost: %+v", got)
	}
}

func TestTotalCachePerKeyZeroDisabledAndInheritancePersist(t *testing.T) {
	path := compatibilityDB(t)
	if err := Init(path); err != nil {
		t.Fatal(err)
	}
	if err := UpdatePromptCacheSettings(true, PromptCacheModePrefixTotal, .8, 1, .8); err != nil {
		t.Fatal(err)
	}
	mode, zero, high, disabled := PromptCacheModePrefixTotal, 0.0, .95, false
	entries := []ApiKeyEntry{
		{ID: "inherited", Key: "sk-inherited-fixture", Enabled: true},
		{ID: "zero", Key: "sk-zero-fixture", Enabled: true, PromptCacheEmulationMode: &mode, PromptCacheEmulationRatio: &zero},
		{ID: "high", Key: "sk-high-fixture", Enabled: true, PromptCacheEmulationRatio: &high},
		{ID: "disabled", Key: "sk-disabled-total-fixture", Enabled: true, PromptCacheEmulationEnabled: &disabled},
	}
	for _, entry := range entries {
		if _, err := AddApiKey(entry); err != nil {
			t.Fatal(err)
		}
	}
	if err := Init(path); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		id      string
		ratio   float64
		enabled bool
	}{{"inherited", .8, true}, {"zero", 0, true}, {"high", .95, true}, {"disabled", .8, false}} {
		got := EffectivePromptCacheSettings(test.id)
		if got.Mode != mode || got.Ratio != test.ratio || got.Enabled != test.enabled {
			t.Fatalf("%s policy lost after reload: %+v", test.id, got)
		}
	}
	if err := ClearPromptCacheOverrides("zero"); err != nil {
		t.Fatal(err)
	}
	if err := Init(path); err != nil {
		t.Fatal(err)
	}
	if got := EffectivePromptCacheSettings("zero"); got.Mode != mode || got.Ratio != .8 || !got.Enabled {
		t.Fatalf("cleared override did not inherit global policy: %+v", got)
	}
}
