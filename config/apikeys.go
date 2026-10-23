package config

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/url"
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
	AutoSnapshotBeforeSave()
	credLock.Lock()
	defer credLock.Unlock()
	if cfg == nil {
		return ApiKeyEntry{}, errors.New("config not initialized")
	}
	entry.Key = strings.TrimSpace(entry.Key)
	if entry.Key == "" {
		return ApiKeyEntry{}, errors.New("api key value must not be empty")
	}
	if err := ValidateUpstreamForward(entry.UpstreamForward); err != nil {
		return ApiKeyEntry{}, err
	}
	if err := validateApiKeyProbeRouting(entry); err != nil {
		return ApiKeyEntry{}, err
	}
	if err := validateApiKeyPromptCacheOverrides(entry); err != nil {
		return ApiKeyEntry{}, err
	}
	if err := validateApiKeyLimits(entry.TokenLimit, entry.CreditLimit); err != nil {
		return ApiKeyEntry{}, err
	}
	entry = cloneApiKeyEntry(entry)
	for _, existing := range cfg.ApiKeys {
		if entry.ID != "" && existing.ID == entry.ID {
			return ApiKeyEntry{}, errors.New("api key ID already exists")
		}
		if existing.Key == entry.Key {
			return ApiKeyEntry{}, errors.New("api key already exists")
		}
	}
	if entry.ID == "" {
		entry.ID = GenerateMachineId()
	}
	if entry.CreatedAt == 0 {
		entry.CreatedAt = time.Now().Unix()
	}
	previousMarker := cfg.ApiKeyAccountMigrationCompleted
	previousCredentials := append([]Account(nil), credentials...)
	var previousAccounts []Account
	if !previousMarker {
		// The first enabled API key may be added after an old config was loaded
		// without any usable key. Keep a copy so a failed save can roll back the
		// one-time account migration together with the API-key append.
		previousAccounts = append([]Account(nil), cfg.Accounts...)
	}
	cfg.ApiKeys = append(cfg.ApiKeys, entry)
	migrateUnboundAccountsLocked()
	if err := saveConfigAndCredentialsLocked(); err != nil {
		// Roll back the in-memory append so we don't leave inconsistent state.
		cfg.ApiKeys = cfg.ApiKeys[:len(cfg.ApiKeys)-1]
		if !previousMarker {
			cfg.ApiKeyAccountMigrationCompleted = previousMarker
			cfg.Accounts = previousAccounts
			credentials = previousCredentials
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
	return updateApiKey(id, func(idx int, candidate *ApiKeyEntry) error {
		return applyApiKeyPatchLocked(idx, patch, candidate)
	}, true)
}

// UpdateApiKeyAndClearPromptCacheOverrides applies an API-key patch and removes
// all per-key prompt-cache overrides in one locked transaction. The combined
// operation performs a single persistence write, so a save failure cannot leave
// ordinary fields updated while cache settings remain partially changed.
func UpdateApiKeyAndClearPromptCacheOverrides(id string, patch ApiKeyEntry) error {
	return updateApiKey(id, func(idx int, candidate *ApiKeyEntry) error {
		if err := applyApiKeyPatchLocked(idx, patch, candidate); err != nil {
			return err
		}
		clearPromptCacheOverrides(candidate)
		return nil
	}, true)
}

// updateApiKey applies mutate and persists the resulting entry atomically. The
// caller-provided mutation runs while cfgLock is held and must only modify the
// candidate; the original entry is restored if migration or persistence fails.
// runMigration preserves the historical behavior of ordinary updates while
// allowing a clear-only operation to remain side-effect free for legacy
// account ownership migration.
func updateApiKey(id string, mutate func(idx int, candidate *ApiKeyEntry) error, runMigration bool) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	AutoSnapshotBeforeSave()
	credLock.Lock()
	defer credLock.Unlock()
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
	if err := mutate(idx, &candidate); err != nil {
		return err
	}
	if err := ValidateUpstreamForward(candidate.UpstreamForward); err != nil {
		return err
	}
	if err := validateApiKeyProbeRouting(candidate); err != nil {
		return err
	}
	if err := validateApiKeyPromptCacheOverrides(candidate); err != nil {
		return err
	}
	if err := validateApiKeyLimits(candidate.TokenLimit, candidate.CreditLimit); err != nil {
		return err
	}
	previousMarker := cfg.ApiKeyAccountMigrationCompleted
	previousCredentials := append([]Account(nil), credentials...)
	var previousAccounts []Account
	migrationAttempted := false
	cfg.ApiKeys[idx] = candidate
	if runMigration && !previousMarker && candidate.Enabled && strings.TrimSpace(candidate.ID) != "" && strings.TrimSpace(candidate.Key) != "" {
		previousAccounts = append([]Account(nil), cfg.Accounts...)
		migrationAttempted = migrateUnboundAccountsLocked()
	}
	if err := saveConfigAndCredentialsLocked(); err != nil {
		cfg.ApiKeys[idx] = original
		if migrationAttempted {
			cfg.Accounts = previousAccounts
			cfg.ApiKeyAccountMigrationCompleted = previousMarker
			credentials = previousCredentials
		}
		return err
	}
	return nil
}

// ClearPromptCacheOverrides removes all per-key prompt-cache overrides so the
// entry inherits the current global policy.
func ClearPromptCacheOverrides(id string) error {
	return updateApiKey(id, func(_ int, candidate *ApiKeyEntry) error {
		clearPromptCacheOverrides(candidate)
		return nil
	}, false)
}

func applyApiKeyPatchLocked(idx int, patch ApiKeyEntry, candidate *ApiKeyEntry) error {
	if patch.CCTestTextHandlingEnabled != nil {
		value := *patch.CCTestTextHandlingEnabled
		candidate.CCTestTextHandlingEnabled = &value
	}
	if patch.UpstreamForward != nil {
		forward := *patch.UpstreamForward
		candidate.UpstreamForward = &forward
	}
	if patch.Name != "" {
		candidate.Name = patch.Name
	}
	if patch.Key != "" {
		newKey := strings.TrimSpace(patch.Key)
		if newKey == "" {
			return errors.New("api key value must not be empty")
		}
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
	return nil
}

func clearPromptCacheOverrides(entry *ApiKeyEntry) {
	entry.PromptCacheEmulationEnabled = nil
	entry.PromptCacheEmulationMode = nil
	entry.PromptCacheEmulationRatio = nil
	entry.PromptCacheCreationEmulationRatio = nil
	entry.PromptCacheReadEmulationRatio = nil
}

func cloneApiKeyEntry(entry ApiKeyEntry) ApiKeyEntry {
	if entry.CCTestTextHandlingEnabled != nil {
		value := *entry.CCTestTextHandlingEnabled
		entry.CCTestTextHandlingEnabled = &value
	}
	if entry.UpstreamForward != nil {
		forward := *entry.UpstreamForward
		entry.UpstreamForward = &forward
	}
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

// ValidateUpstreamForward does not include supplied values in errors because
// URLs and credentials may contain secrets.
func ValidateUpstreamForward(c *UpstreamForwardConfig) error {
	if c == nil {
		return nil
	}
	if strings.ContainsAny(c.APIKey, "\r\n") {
		return errors.New("invalid upstream API key")
	}
	if c.BaseURL != "" {
		u, err := url.Parse(c.BaseURL)
		if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.ContainsAny(c.BaseURL, "\r\n") {
			return errors.New("upstream base URL must be HTTPS without credentials, query or fragment")
		}
	}
	if c.Enabled && (c.BaseURL == "" || strings.TrimSpace(c.APIKey) == "") {
		return errors.New("enabled upstream requires base URL and API key")
	}
	return nil
}

func validateApiKeyPromptCacheOverrides(entry ApiKeyEntry) error {
	if entry.PromptCacheEmulationMode != nil && *entry.PromptCacheEmulationMode != PromptCacheModeLifecycle && *entry.PromptCacheEmulationMode != PromptCacheModeSub2API && *entry.PromptCacheEmulationMode != PromptCacheModePrefixTotal && *entry.PromptCacheEmulationMode != PromptCacheModePrefix && *entry.PromptCacheEmulationMode != promptCacheModeUniform && *entry.PromptCacheEmulationMode != promptCacheModeIndependent {
		return errors.New("prompt cache mode must be prefix_lifecycle, sub2api, prefix_total, prefix, uniform or independent")
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
	AutoSnapshotBeforeSave()
	credLock.RLock()
	defer credLock.RUnlock()
	if cfg == nil {
		return errors.New("config not initialized")
	}
	for _, account := range cfg.Accounts {
		if strings.TrimSpace(account.ApiKeyID) == id {
			return ErrApiKeyInUse
		}
	}
	if credLoaded {
		for _, account := range credentials {
			if strings.TrimSpace(account.ApiKeyID) == id {
				return ErrApiKeyInUse
			}
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

func validateApiKeyLimits(tokenLimit int64, creditLimit float64) error {
	if tokenLimit < 0 {
		return errors.New("token limit must be zero or positive")
	}
	if creditLimit < 0 {
		return errors.New("credit limit must be zero or positive")
	}
	return nil
}

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

// RecordApiKeyForwardedUsage applies the actual usage reported by a dedicated
// upstream route. It has no Kiro-credit estimate and applies token limits to
// subsequent calls after this response is accounted for.
func RecordApiKeyForwardedUsage(id string, tokens int64) error {
	if tokens < 0 {
		return errors.New("upstream token usage must be zero or positive")
	}
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return errors.New("config not initialized")
	}
	for i := range cfg.ApiKeys {
		if cfg.ApiKeys[i].ID != id {
			continue
		}
		old := cloneApiKeyEntry(cfg.ApiKeys[i])
		cfg.ApiKeys[i].TokensUsed += tokens
		cfg.ApiKeys[i].RequestsCount++
		cfg.ApiKeys[i].LastUsedAt = time.Now().Unix()
		if cfg.ApiKeys[i].TokenLimit > 0 && cfg.ApiKeys[i].TokensUsed >= cfg.ApiKeys[i].TokenLimit {
			cfg.ApiKeys[i].Enabled = false
		}
		if err := saveLocked(); err != nil {
			cfg.ApiKeys[i] = old
			return err
		}
		return nil
	}
	return errors.New("api key not found")
}

func ReserveApiKeyUsage(id string, tokens int64, credits float64) error {
	if tokens < 0 || credits < 0 {
		return errors.New("usage reservation must be zero or positive")
	}
	return reserveApiKeyUsageLocked(id, tokens, credits)
}

func ReserveApiKeyRequestUsage(id string, tokens int64) (float64, error) {
	if tokens < 0 {
		return 0, errors.New("usage reservation must be zero or positive")
	}
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return 0, errors.New("config not initialized")
	}
	for i := range cfg.ApiKeys {
		if cfg.ApiKeys[i].ID != id {
			continue
		}
		credits := 0.0
		if cfg.ApiKeys[i].CreditLimit > 0 && cfg.ApiKeys[i].CreditsUsed >= cfg.ApiKeys[i].CreditLimit {
			return 0, errors.New("credit limit exceeded")
		}
		if err := reserveApiKeyUsageAtIndexLocked(i, tokens, credits); err != nil {
			return 0, err
		}
		return credits, nil
	}
	return 0, errors.New("api key not found")
}

func reserveApiKeyUsageLocked(id string, tokens int64, credits float64) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return errors.New("config not initialized")
	}
	for i := range cfg.ApiKeys {
		if cfg.ApiKeys[i].ID != id {
			continue
		}
		return reserveApiKeyUsageAtIndexLocked(i, tokens, credits)
	}
	return errors.New("api key not found")
}

func reserveApiKeyUsageAtIndexLocked(i int, tokens int64, credits float64) error {
	if !cfg.ApiKeys[i].Enabled {
		return errors.New("api key disabled")
	}
	if cfg.ApiKeys[i].TokenLimit > 0 && cfg.ApiKeys[i].TokensUsed+tokens > cfg.ApiKeys[i].TokenLimit {
		return errors.New("token limit exceeded")
	}
	if cfg.ApiKeys[i].CreditLimit > 0 && cfg.ApiKeys[i].CreditsUsed+credits > cfg.ApiKeys[i].CreditLimit {
		return errors.New("credit limit exceeded")
	}
	cfg.ApiKeys[i].TokensUsed += tokens
	cfg.ApiKeys[i].CreditsUsed += credits
	if err := saveLocked(); err != nil {
		cfg.ApiKeys[i].TokensUsed -= tokens
		cfg.ApiKeys[i].CreditsUsed -= credits
		return err
	}
	return nil
}

func ReleaseApiKeyUsageReservation(id string, tokens int64, credits float64) error {
	if tokens < 0 || credits < 0 {
		return errors.New("usage reservation must be zero or positive")
	}
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return errors.New("config not initialized")
	}
	for i := range cfg.ApiKeys {
		if cfg.ApiKeys[i].ID != id {
			continue
		}
		cfg.ApiKeys[i].TokensUsed -= tokens
		if cfg.ApiKeys[i].TokensUsed < 0 {
			cfg.ApiKeys[i].TokensUsed = 0
		}
		cfg.ApiKeys[i].CreditsUsed -= credits
		if cfg.ApiKeys[i].CreditsUsed < 0 {
			cfg.ApiKeys[i].CreditsUsed = 0
		}
		return saveLocked()
	}
	return errors.New("api key not found")
}

func FinalizeApiKeyUsageReservation(id string, reservedTokens int64, reservedCredits float64, actualTokens int64, actualCredits float64) error {
	if reservedTokens < 0 || reservedCredits < 0 || actualTokens < 0 || actualCredits < 0 {
		return errors.New("api key usage must be zero or positive")
	}
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return errors.New("config not initialized")
	}
	for i := range cfg.ApiKeys {
		if cfg.ApiKeys[i].ID != id {
			continue
		}
		cfg.ApiKeys[i].TokensUsed += actualTokens - reservedTokens
		if cfg.ApiKeys[i].TokensUsed < 0 {
			cfg.ApiKeys[i].TokensUsed = 0
		}
		cfg.ApiKeys[i].CreditsUsed += actualCredits - reservedCredits
		if cfg.ApiKeys[i].CreditsUsed < 0 {
			cfg.ApiKeys[i].CreditsUsed = 0
		}
		if cfg.ApiKeys[i].TokenLimit > 0 && cfg.ApiKeys[i].TokensUsed >= cfg.ApiKeys[i].TokenLimit {
			cfg.ApiKeys[i].Enabled = false
		}
		if cfg.ApiKeys[i].CreditLimit > 0 && cfg.ApiKeys[i].CreditsUsed >= cfg.ApiKeys[i].CreditLimit {
			cfg.ApiKeys[i].Enabled = false
		}
		cfg.ApiKeys[i].RequestsCount++
		cfg.ApiKeys[i].LastUsedAt = time.Now().Unix()
		return saveLocked()
	}
	return errors.New("api key not found")
}

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

func GenerateApiKeyValue() string {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "sk-" + strings.ReplaceAll(GenerateMachineId(), "-", "")
	}
	return "sk-" + hex.EncodeToString(buf)
}

func MaskApiKey(key string) string {
	if key == "" {
		return ""
	}
	if len(key) <= 10 {
		return key
	}
	return key[:6] + "****" + key[len(key)-4:]
}

func ApiKeyOverLimit(entry ApiKeyEntry) (overToken bool, overCredit bool) {
	if entry.TokenLimit > 0 && entry.TokensUsed >= entry.TokenLimit {
		overToken = true
	}
	if entry.CreditLimit > 0 && entry.CreditsUsed >= entry.CreditLimit {
		overCredit = true
	}
	return
}
