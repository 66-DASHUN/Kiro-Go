package config

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
)

var (
	ErrAccountNotFound       = errors.New("account not found")
	ErrDuplicateAccountID    = errors.New("account ID already exists")
	ErrDuplicateRefreshToken = errors.New("account refresh token already exists")
	ErrDuplicateAPIKey       = ErrDuplicateKiroAPIKey
	ErrEmptyAPIKey           = ErrEmptyKiroAPIKey
	ErrInvalidAccountApiKey  = errors.New("apiKeyId must reference an enabled API key")
	ErrApiKeyInUse           = errors.New("api key is still bound to one or more accounts")
)

func GetConfigDir() string { return GetDataDir() }
func GetApiKey() string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return ""
	}
	return cfg.ApiKey
}

func ApiKeyIDExists(id string) bool { return GetApiKeyEntry(strings.TrimSpace(id)) != nil }
func IsApiKeyIDValid(id string) bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	return isApiKeyIDValidLocked(strings.TrimSpace(id))
}
func isApiKeyIDValidLocked(id string) bool {
	if cfg == nil || id == "" {
		return false
	}
	for _, entry := range cfg.ApiKeys {
		if entry.ID == id {
			return entry.Enabled && strings.TrimSpace(entry.Key) != ""
		}
	}
	return false
}
func RefreshTokenFingerprint(token string) string {
	if token == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(token))
	return fmt.Sprintf("%x", sum[:])
}
func APIKeyFingerprint(apiKey string) string { return KiroAPIKeyFingerprint(apiKey) }
func AccountCredentialExists(token string) bool {
	if token == "" {
		return false
	}
	fingerprint := RefreshTokenFingerprint(token)
	for _, account := range GetAccounts() {
		if account.RefreshToken == token || account.RefreshTokenFingerprint == fingerprint {
			return true
		}
	}
	return false
}
func AccountIDExists(id string) bool {
	if id == "" {
		return false
	}
	for _, account := range GetAccounts() {
		if account.ID == id {
			return true
		}
	}
	return false
}

// All compound mutations lock configuration before credentials, matching the
// backup/restore lock order. This also makes ownership validation atomic with
// enabling, disabling or deleting its API key.
func addAccount(account Account, separate bool) error {
	if account.MaxConcurrency < 0 || account.MaxConcurrency > MaxAccountMaxConcurrency {
		return ErrInvalidAccountConcurrency
	}
	if account.MaxConcurrency == 0 {
		account.MaxConcurrency = DefaultAccountMaxConcurrency
	}
	if IsAPIKeyAccount(&account) {
		if err := NormalizeAPIKeyAccount(&account); err != nil {
			return err
		}
	}
	if account.RefreshTokenFingerprint == "" {
		account.RefreshTokenFingerprint = RefreshTokenFingerprint(account.RefreshToken)
	}
	account.ApiKeyID = strings.TrimSpace(account.ApiKeyID)
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return errors.New("config not initialized")
	}
	if account.ApiKeyID != "" && !isApiKeyIDValidLocked(account.ApiKeyID) {
		return ErrInvalidAccountApiKey
	}
	AutoSnapshotBeforeSave()
	credLock.Lock()
	defer credLock.Unlock()
	useCredentials := separate || credLoaded
	accounts := &cfg.Accounts
	if useCredentials {
		accounts = &credentials
	}
	for _, existing := range *accounts {
		if account.ID != "" && existing.ID == account.ID {
			return ErrDuplicateAccountID
		}
		fingerprint := existing.RefreshTokenFingerprint
		if fingerprint == "" {
			fingerprint = RefreshTokenFingerprint(existing.RefreshToken)
		}
		if account.RefreshToken != "" && account.RefreshToken == existing.RefreshToken || account.RefreshTokenFingerprint != "" && account.RefreshTokenFingerprint == fingerprint {
			return ErrDuplicateRefreshToken
		}
	}
	if hasKiroAPIKey(*accounts, account.KiroApiKey) {
		return ErrDuplicateKiroAPIKey
	}
	previous := append([]Account(nil), (*accounts)...)
	previousLoaded := credLoaded
	*accounts = append(*accounts, account)
	if useCredentials {
		credLoaded = true
	}
	if err := saveConfigAndCredentialsLocked(); err != nil {
		*accounts = previous
		credLoaded = previousLoaded
		return err
	}
	return nil
}

func mutateAccount(id string, separate bool, mutate func(*Account) error) (Account, error) {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return Account{}, errors.New("config not initialized")
	}
	AutoSnapshotBeforeSave()
	credLock.Lock()
	defer credLock.Unlock()
	accounts := &cfg.Accounts
	if separate || credLoaded {
		accounts = &credentials
	}
	for i, current := range *accounts {
		if current.ID != id {
			continue
		}
		updated := current
		previousMarker := cfg.ApiKeyAccountMigrationCompleted
		if err := mutate(&updated); err != nil {
			cfg.ApiKeyAccountMigrationCompleted = previousMarker
			return Account{}, err
		}
		(*accounts)[i] = updated
		if err := saveConfigAndCredentialsLocked(); err != nil {
			(*accounts)[i] = current
			cfg.ApiKeyAccountMigrationCompleted = previousMarker
			return Account{}, err
		}
		return updated, nil
	}
	return Account{}, ErrAccountNotFound
}

func UpdateAccountPatch(id string, patch AccountPatch) (Account, error) {
	return mutateAccount(id, false, func(updated *Account) error {
		if patch.MaxConcurrency != nil {
			if *patch.MaxConcurrency < 1 || *patch.MaxConcurrency > MaxAccountMaxConcurrency {
				return ErrInvalidAccountConcurrency
			}
			updated.MaxConcurrency = *patch.MaxConcurrency
		}
		if patch.ApiKeyID != nil {
			keyID := strings.TrimSpace(*patch.ApiKeyID)
			if keyID != "" && !isApiKeyIDValidLocked(keyID) {
				return ErrInvalidAccountApiKey
			}
			updated.ApiKeyID = keyID
			cfg.ApiKeyAccountMigrationCompleted = true
		}
		if patch.Nickname != nil {
			updated.Nickname = *patch.Nickname
		}
		if patch.Enabled != nil {
			updated.Enabled = *patch.Enabled
		}
		if patch.MachineID != nil {
			updated.MachineId = *patch.MachineID
		}
		if patch.Weight != nil {
			updated.Weight = *patch.Weight
		}
		if patch.ProxyURL != nil {
			updated.ProxyURL = *patch.ProxyURL
		}
		if patch.Silent != nil {
			updated.Silent = *patch.Silent
		}
		if patch.SilentReason != nil {
			updated.SilentReason = *patch.SilentReason
		}
		if patch.SilentTime != nil {
			updated.SilentTime = *patch.SilentTime
		}
		if patch.BanStatus != nil {
			updated.BanStatus = *patch.BanStatus
		}
		if patch.BanReason != nil {
			updated.BanReason = *patch.BanReason
		}
		if patch.BanTime != nil {
			updated.BanTime = *patch.BanTime
		}
		return nil
	})
}

func deleteAccount(id string, separate bool) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return errors.New("config not initialized")
	}
	AutoSnapshotBeforeSave()
	credLock.Lock()
	defer credLock.Unlock()
	accounts := &cfg.Accounts
	if separate || credLoaded {
		accounts = &credentials
	}
	for i, account := range *accounts {
		if account.ID != id {
			continue
		}
		previous := append([]Account(nil), (*accounts)...)
		*accounts = append((*accounts)[:i], (*accounts)[i+1:]...)
		if err := saveConfigAndCredentialsLocked(); err != nil {
			*accounts = previous
			return err
		}
		return nil
	}
	return nil
}
func UpdateAccountApiKeyID(id, keyID string) error {
	_, err := UpdateAccountPatch(id, AccountPatch{ApiKeyID: &keyID})
	return err
}
func ClearAccountBanStatus(id string) error {
	_, err := mutateAccount(id, false, func(account *Account) error {
		account.BanStatus = "ACTIVE"
		account.BanReason = ""
		account.BanTime = 0
		return nil
	})
	return err
}
func UpdateAccountCredentialState(id, accessToken, refreshToken string, expiresAt int64, profileArn string) error {
	_, err := mutateAccount(id, false, func(account *Account) error {
		if account.RefreshTokenFingerprint == "" {
			account.RefreshTokenFingerprint = RefreshTokenFingerprint(account.RefreshToken)
		}
		account.AccessToken = accessToken
		if refreshToken != "" {
			account.RefreshToken = refreshToken
		}
		account.ExpiresAt = expiresAt
		if profileArn != "" {
			account.ProfileArn = profileArn
		}
		return nil
	})
	return err
}
