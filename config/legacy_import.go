package config

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"kiro-proxy/db"
)

// ResolveDataDir keeps older CONFIG_PATH deployments in their existing directory.
func ResolveDataDir(dataDir, configPath string) string {
	if dataDir != "" {
		return dataDir
	}
	if configPath != "" {
		return filepath.Dir(configPath)
	}
	return "."
}

// ImportLegacyJSON imports only into a database without configuration. The
// original file is never changed, and existing database settings take priority.
func ImportLegacyJSON(path string) error {
	if _, ok, err := getSetting("config"); err != nil || ok {
		return err
	}
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read legacy configuration: %w", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return fmt.Errorf("parse legacy configuration: %w", err)
	}
	if fields == nil || isMissingOrNull(fields["accounts"]) || isMissingOrNull(fields["password"]) || isMissingOrNull(fields["port"]) {
		return errors.New("legacy configuration must contain password, port and accounts")
	}
	var candidate Config
	if err := json.Unmarshal(raw, &candidate); err != nil {
		return fmt.Errorf("parse legacy configuration: %w", err)
	}
	if candidate.Password == "" || candidate.Port < 1 || candidate.Port > 65535 {
		return errors.New("legacy configuration has invalid password or port")
	}
	if err := normalizeCompatibleConfig(&candidate, raw); err != nil {
		return fmt.Errorf("legacy configuration: %w", err)
	}
	if isMissingOrNull(fields["modelMappings"]) {
		// ef60b42 kept these aliases in its translator rather than its JSON.
		// Materialize them only for imported deployments, preserving routing.
		candidate.ModelMappings = []ModelMappingRule{
			{Key: "claude-sonnet-4-20250514", Value: "claude-sonnet-4"},
			{Key: "claude-3-5-sonnet", Value: "claude-sonnet-4.5"},
			{Key: "claude-3-opus", Value: "claude-sonnet-4.5"},
			{Key: "claude-3-sonnet", Value: "claude-sonnet-4"},
			{Key: "claude-3-haiku", Value: "claude-haiku-4.5"},
			{Key: "gpt-4-turbo", Value: "claude-sonnet-4.5"},
			{Key: "gpt-4o", Value: "claude-sonnet-4.5"},
			{Key: "gpt-4", Value: "claude-sonnet-4.5"},
			{Key: "gpt-3.5-turbo", Value: "claude-sonnet-4.5"},
		}
	}
	if !candidate.ApiKeyAccountMigrationCompleted {
		if keyID := firstEnabledApiKeyID(&candidate); keyID != "" {
			migrateAccountOwnership(candidate.Accounts, keyID)
			candidate.ApiKeyAccountMigrationCompleted = true
		}
	}
	data, err := json.Marshal(&candidate)
	if err != nil {
		return err
	}
	d, err := db.Get()
	if err != nil {
		return err
	}
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var existing string
	err = tx.QueryRow(`SELECT value FROM settings WHERE key='config'`).Scan(&existing)
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if _, err = tx.Exec(`INSERT INTO settings(key,value) VALUES('config',?)`, string(data)); err != nil {
		return fmt.Errorf("import legacy configuration: %w", err)
	}
	return tx.Commit()
}

func normalizeCompatibleConfig(c *Config, raw []byte) error {
	if err := normalizePromptCacheConfig(c, raw); err != nil {
		return err
	}
	for _, entry := range c.ApiKeys {
		if err := validateApiKeyPromptCacheOverrides(entry); err != nil {
			return err
		}
		if err := validateApiKeyLimits(entry.TokenLimit, entry.CreditLimit); err != nil {
			return err
		}
	}
	if c.ApiKey != "" && len(c.ApiKeys) == 0 {
		c.ApiKeys = []ApiKeyEntry{{ID: GenerateMachineId(), Name: "legacy", Key: c.ApiKey, Enabled: c.RequireApiKey, Migrated: true, CreatedAt: time.Now().Unix()}}
	}
	if c.SanitizeClaudeCodePrompt {
		c.FilterClaudeCode = true
		c.SanitizeClaudeCodePrompt = false
	}
	for i := range c.Accounts {
		normalizeLegacyAccount(&c.Accounts[i])
	}
	return nil
}

func normalizeLegacyAccount(account *Account) {
	if account.RefreshTokenFingerprint == "" {
		account.RefreshTokenFingerprint = RefreshTokenFingerprint(account.RefreshToken)
	}
	if account.LegacyAllowOverage {
		if account.OverageStatus == "" {
			account.OverageStatus = "ENABLED"
		}
		account.LegacyAllowOverage = false
	}
}

func firstEnabledApiKeyID(c *Config) string {
	if c == nil {
		return ""
	}
	for _, entry := range c.ApiKeys {
		if entry.Enabled && strings.TrimSpace(entry.ID) != "" && strings.TrimSpace(entry.Key) != "" {
			return entry.ID
		}
	}
	return ""
}
func migrateAccountOwnership(accounts []Account, keyID string) {
	for i := range accounts {
		if strings.TrimSpace(accounts[i].ApiKeyID) == "" {
			accounts[i].ApiKeyID = keyID
		}
	}
}

// Both cfgLock and credLock must be held. Existing explicit ownership is kept.
func migrateUnboundAccountsLocked() bool {
	if cfg == nil || cfg.ApiKeyAccountMigrationCompleted {
		return false
	}
	keyID := firstEnabledApiKeyID(cfg)
	if keyID == "" {
		return false
	}
	migrateAccountOwnership(cfg.Accounts, keyID)
	if credLoaded {
		migrateAccountOwnership(credentials, keyID)
	}
	cfg.ApiKeyAccountMigrationCompleted = true
	return true
}

func migrateLoadedAccountOwnership() error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	credLock.Lock()
	defer credLock.Unlock()
	previous := *cfg
	previous.Accounts = append([]Account(nil), cfg.Accounts...)
	previousCredentials := append([]Account(nil), credentials...)
	if !migrateUnboundAccountsLocked() {
		return nil
	}
	if err := saveConfigAndCredentialsLocked(); err != nil {
		*cfg = previous
		credentials = previousCredentials
		return err
	}
	return nil
}

// saveConfigAndCredentialsLocked is used when one mutation changes both native
// stores. Publish in-memory state only after all SQLite writes can commit.
func saveConfigAndCredentialsLocked() error {
	configData, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	credentialData, err := json.Marshal(credentials)
	if err != nil {
		return err
	}
	d, err := db.Get()
	if err != nil {
		return err
	}
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	put := func(key, value string) error {
		_, err := tx.Exec(`INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
		return err
	}
	if err := put("config", string(configData)); err != nil {
		return err
	}
	if credLoaded {
		if err := put(credentialsKey, string(credentialData)); err != nil {
			return err
		}
		if err := put(credentialsLoadedKey, "1"); err != nil {
			return err
		}
	}
	return tx.Commit()
}
