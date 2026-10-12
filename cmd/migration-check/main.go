// migration-check verifies a copied legacy deployment without exposing its values.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"kiro-proxy/config"
	"kiro-proxy/db"
	"os"
	"path/filepath"
	"reflect"
)

func preserved(want, got any, path string) error {
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			return fmt.Errorf("migration changed object at %s", path)
		}
		for k, v := range w {
			gv, exists := g[k]
			if !exists {
				// JSON omitempty can omit explicit zero-valued legacy fields.
				if v == nil || v == false || v == "" || v == float64(0) {
					continue
				}
				return fmt.Errorf("migration omitted field %s.%s", path, k)
			}
			if err := preserved(v, gv, path+"."+k); err != nil {
				return err
			}
		}
	case []any:
		g, ok := got.([]any)
		if !ok || len(w) != len(g) {
			return fmt.Errorf("migration changed array at %s", path)
		}
		for i, v := range w {
			if err := preserved(v, g[i], fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
	default:
		if !reflect.DeepEqual(want, got) {
			return fmt.Errorf("migration changed value at %s", path)
		}
	}
	return nil
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: migration-check <copied-data-directory>")
		os.Exit(2)
	}
	dir := os.Args[1]
	source := filepath.Join(dir, "config.json")
	raw, err := os.ReadFile(source)
	if err != nil {
		fail(err)
	}
	var old map[string]any
	if err = json.Unmarshal(raw, &old); err != nil {
		fail(err)
	}
	if err = os.Setenv("CONFIG_PATH", source); err != nil {
		fail(err)
	}
	if err = config.Init(filepath.Join(dir, "kiro.db")); err != nil {
		fail(err)
	}
	defer db.Close()
	encoded, err := json.Marshal(config.Get())
	if err != nil {
		fail(err)
	}
	var next map[string]any
	if err = json.Unmarshal(encoded, &next); err != nil {
		fail(err)
	}
	if err = preserved(old, next, "config"); err != nil {
		fail(err)
	}
	after, err := os.ReadFile(source)
	if err != nil {
		fail(err)
	}
	if !reflect.DeepEqual(raw, after) {
		fail(fmt.Errorf("migration modified original JSON"))
	}
	beforeState, _ := json.Marshal(config.Get())
	if err = config.Init(filepath.Join(dir, "kiro.db")); err != nil {
		fail(err)
	}
	afterState, _ := json.Marshal(config.Get())
	if !reflect.DeepEqual(beforeState, afterState) {
		fail(fmt.Errorf("migration changed state after reload"))
	}
	digest := sha256.Sum256(raw)
	fmt.Printf("migration verified: accounts=%d api_keys=%d source_sha256=%s reload_stable=true\n", len(config.GetAccounts()), len(config.ListApiKeys()), hex.EncodeToString(digest[:]))
}
func fail(err error) { fmt.Fprintln(os.Stderr, err); os.Exit(1) }
