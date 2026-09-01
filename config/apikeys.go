package config

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
	"time"
)

// ListApiKeys returns a snapshot of all configured API key entries.
func ListApiKeys() []ApiKeyEntry {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return nil
	}
	out := make([]ApiKeyEntry, len(cfg.ApiKeys))
	for i := range cfg.ApiKeys {
		out[i] = cloneApiKeyEntry(cfg.ApiKeys[i])
	}
	return out
}

// GetApiKeyEntry returns a copy of the entry with the given ID, or nil if not found.
func GetApiKeyEntry(id string) *ApiKeyEntry {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return nil
	}
	for i := range cfg.ApiKeys {
		if cfg.ApiKeys[i].ID == id {
			cp := cloneApiKeyEntry(cfg.ApiKeys[i])
			return &cp
		}
	}
	return nil
}

// AddApiKey appends a new API key entry. Generates ID and CreatedAt if missing,
// rejects empty Key values, and refuses duplicates of an existing Key.
func AddApiKey(entry ApiKeyEntry) (ApiKeyEntry, error) {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return ApiKeyEntry{}, errors.New("config not initialized")
	}
	entry.Key = strings.TrimSpace(entry.Key)
	if entry.Key == "" {
		return ApiKeyEntry{}, errors.New("api key value must not be empty")
	}
	if err := validateApiKeyPromptCacheOverrides(entry); err != nil {
		return ApiKeyEntry{}, err
	}
	entry = cloneApiKeyEntry(entry)
	for _, existing := range cfg.ApiKeys {
		if existing.Key == entry.Key {
			return ApiKeyEntry{}, errors.New("api key already exists")
		}
	}
	if entry.ID == "" {
		entry.ID = newUUID()
	}
	if entry.CreatedAt == 0 {
		entry.CreatedAt = time.Now().Unix()
	}
	previousMarker := cfg.ApiKeyAccountMigrationCompleted
	var previousAccounts []Account
	if !previousMarker {
		// The first enabled API key may be added after an old config was loaded
		// without any usable key. Keep a copy so a failed save can roll back the
		// one-time account migration together with the API-key append.
		previousAccounts = append([]Account(nil), cfg.Accounts...)
	}
	cfg.ApiKeys = append(cfg.ApiKeys, entry)
	migrateUnboundAccountsLocked()
	if err := saveLocked(); err != nil {
		// Roll back the in-memory append so we don't leave inconsistent state.
		cfg.ApiKeys = cfg.ApiKeys[:len(cfg.ApiKeys)-1]
		if !previousMarker {
			cfg.ApiKeyAccountMigrationCompleted = previousMarker
			cfg.Accounts = previousAccounts
		}
		return ApiKeyEntry{}, err
	}
	return cloneApiKeyEntry(entry), nil
}

// UpdateApiKey applies a patch to an existing API key. Patch semantics:
//   - Name, Key are overwritten when non-empty in patch.
//   - Enabled, TokenLimit, CreditLimit are always overwritten (zero values are valid).
//   - Counters (TokensUsed/CreditsUsed/RequestsCount) are not touched here; use
//     RecordApiKeyUsage or ResetApiKeyUsage instead.
//   - Migrated stays as-is once true; only flips when explicitly set in patch.
func UpdateApiKey(id string, patch ApiKeyEntry) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return errors.New("config not initialized")
	}
	idx := -1
	for i := range cfg.ApiKeys {
		if cfg.ApiKeys[i].ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		return errors.New("api key not found")
	}
	original := cloneApiKeyEntry(cfg.ApiKeys[idx])
	candidate := cloneApiKeyEntry(original)
	if patch.Name != "" {
		candidate.Name = patch.Name
	}
	if patch.Key != "" {
		newKey := strings.TrimSpace(patch.Key)
		// Reject duplicates against any other entry.
		for j := range cfg.ApiKeys {
			if j != idx && cfg.ApiKeys[j].Key == newKey {
				return errors.New("api key value collides with existing entry")
			}
		}
		candidate.Key = newKey
	}
	candidate.Enabled = patch.Enabled
	candidate.TokenLimit = patch.TokenLimit
	candidate.CreditLimit = patch.CreditLimit
	if patch.PromptCacheEmulationEnabled != nil {
		value := *patch.PromptCacheEmulationEnabled
		candidate.PromptCacheEmulationEnabled = &value
	}
	if patch.PromptCacheEmulationMode != nil {
		value := *patch.PromptCacheEmulationMode
		candidate.PromptCacheEmulationMode = &value
	}
	if patch.PromptCacheEmulationRatio != nil {
		value := *patch.PromptCacheEmulationRatio
		candidate.PromptCacheEmulationRatio = &value
	}
	if patch.PromptCacheCreationEmulationRatio != nil {
		value := *patch.PromptCacheCreationEmulationRatio
		candidate.PromptCacheCreationEmulationRatio = &value
	}
	if patch.PromptCacheReadEmulationRatio != nil {
		value := *patch.PromptCacheReadEmulationRatio
		candidate.PromptCacheReadEmulationRatio = &value
	}
	if patch.Migrated {
		candidate.Migrated = true
	}
	if err := validateApiKeyPromptCacheOverrides(candidate); err != nil {
		return err
	}
	cfg.ApiKeys[idx] = candidate
	if err := saveLocked(); err != nil {
		cfg.ApiKeys[idx] = original
		return err
	}
	return nil
}

// ClearPromptCacheOverrides removes all per-key prompt-cache overrides so the
// entry inherits the current global policy.
func ClearPromptCacheOverrides(id string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return errors.New("config not initialized")
	}
	for i := range cfg.ApiKeys {
		if cfg.ApiKeys[i].ID != id {
			continue
		}
		original := cloneApiKeyEntry(cfg.ApiKeys[i])
		cfg.ApiKeys[i].PromptCacheEmulationEnabled = nil
		cfg.ApiKeys[i].PromptCacheEmulationMode = nil
		cfg.ApiKeys[i].PromptCacheEmulationRatio = nil
		cfg.ApiKeys[i].PromptCacheCreationEmulationRatio = nil
		cfg.ApiKeys[i].PromptCacheReadEmulationRatio = nil
		if err := saveLocked(); err != nil {
			cfg.ApiKeys[i] = original
			return err
		}
		return nil
	}
	return errors.New("api key not found")
}

func cloneApiKeyEntry(entry ApiKeyEntry) ApiKeyEntry {
	if entry.PromptCacheEmulationEnabled != nil {
		value := *entry.PromptCacheEmulationEnabled
		entry.PromptCacheEmulationEnabled = &value
	}
	if entry.PromptCacheEmulationMode != nil {
		value := *entry.PromptCacheEmulationMode
		entry.PromptCacheEmulationMode = &value
	}
	if entry.PromptCacheEmulationRatio != nil {
		value := *entry.PromptCacheEmulationRatio
		entry.PromptCacheEmulationRatio = &value
	}
	if entry.PromptCacheCreationEmulationRatio != nil {
		value := *entry.PromptCacheCreationEmulationRatio
		entry.PromptCacheCreationEmulationRatio = &value
	}
	if entry.PromptCacheReadEmulationRatio != nil {
		value := *entry.PromptCacheReadEmulationRatio
		entry.PromptCacheReadEmulationRatio = &value
	}
	return entry
}

func validateApiKeyPromptCacheOverrides(entry ApiKeyEntry) error {
	if entry.PromptCacheEmulationMode != nil && *entry.PromptCacheEmulationMode != promptCacheModeUniform && *entry.PromptCacheEmulationMode != promptCacheModeIndependent {
		return errors.New("prompt cache mode must be uniform or independent")
	}
	for name, value := range map[string]*float64{
		"ratio":          entry.PromptCacheEmulationRatio,
		"creation ratio": entry.PromptCacheCreationEmulationRatio,
		"read ratio":     entry.PromptCacheReadEmulationRatio,
	} {
		if value != nil {
			if err := ValidatePromptCacheRatio(*value); err != nil {
				return errors.New("invalid prompt cache " + name + ": " + err.Error())
			}
		}
	}
	return nil
}

// DeleteApiKey removes the API key entry with the given ID. Returns nil even if
// the ID is unknown (idempotent), matching the existing DeleteAccount style.
func DeleteApiKey(id string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return errors.New("config not initialized")
	}
	for _, account := range cfg.Accounts {
		if strings.TrimSpace(account.ApiKeyID) == id {
			return ErrApiKeyInUse
		}
	}
	for i, e := range cfg.ApiKeys {
		if e.ID == id {
			previous := append([]ApiKeyEntry(nil), cfg.ApiKeys...)
			cfg.ApiKeys = append(cfg.ApiKeys[:i], cfg.ApiKeys[i+1:]...)
			if err := saveLocked(); err != nil {
				cfg.ApiKeys = previous
				return err
			}
			return nil
		}
	}
	return nil
}

// FindApiKeyByValue returns a copy of the entry whose Key matches the given value,
// or nil if no match. O(n) linear scan.
func FindApiKeyByValue(key string) *ApiKeyEntry {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil || key == "" {
		return nil
	}
	for i := range cfg.ApiKeys {
		if cfg.ApiKeys[i].Key == key {
			cp := cloneApiKeyEntry(cfg.ApiKeys[i])
			return &cp
		}
	}
	return nil
}

// HasApiKeys returns true when at least one API key entry is configured.
func HasApiKeys() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return false
	}
	return len(cfg.ApiKeys) > 0
}

// RecordApiKeyUsage atomically adds tokens and credits to the entry's counters,
// updates LastUsedAt, increments RequestsCount, and persists.
func RecordApiKeyUsage(id string, tokens int64, credits float64) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return errors.New("config not initialized")
	}
	for i := range cfg.ApiKeys {
		if cfg.ApiKeys[i].ID == id {
			if tokens > 0 {
				cfg.ApiKeys[i].TokensUsed += tokens
			}
			if credits > 0 {
				cfg.ApiKeys[i].CreditsUsed += credits
			}
			cfg.ApiKeys[i].RequestsCount++
			cfg.ApiKeys[i].LastUsedAt = time.Now().Unix()
			return saveLocked()
		}
	}
	return errors.New("api key not found")
}

// ResetApiKeyUsage clears TokensUsed/CreditsUsed/RequestsCount for the entry.
// LastUsedAt is preserved so operators can still see when the key was last used.
func ResetApiKeyUsage(id string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return errors.New("config not initialized")
	}
	for i := range cfg.ApiKeys {
		if cfg.ApiKeys[i].ID == id {
			cfg.ApiKeys[i].TokensUsed = 0
			cfg.ApiKeys[i].CreditsUsed = 0
			cfg.ApiKeys[i].RequestsCount = 0
			return saveLocked()
		}
	}
	return errors.New("api key not found")
}

// GenerateApiKeyValue returns a new random 32-byte hex API key prefixed with "sk-".
func GenerateApiKeyValue() string {
	buf := make([]byte, 32)
	_, _ = rand.Read(buf)
	return "sk-" + hex.EncodeToString(buf)
}

// MaskApiKey produces a display-friendly masked version: keeps first 6 and last 4
// characters, replaces the middle with "****". Returns "" for empty input and
// the original string if it's too short to mask meaningfully.
func MaskApiKey(key string) string {
	if key == "" {
		return ""
	}
	if len(key) <= 10 {
		return key
	}
	return key[:6] + "****" + key[len(key)-4:]
}

// ApiKeyOverLimit returns (overToken, overCredit) for the entry. Limits with value 0
// are ignored. The function does not lock; callers should pass a copied entry.
func ApiKeyOverLimit(e ApiKeyEntry) (overToken bool, overCredit bool) {
	if e.TokenLimit > 0 && e.TokensUsed >= e.TokenLimit {
		overToken = true
	}
	if e.CreditLimit > 0 && e.CreditsUsed >= e.CreditLimit {
		overCredit = true
	}
	return
}
