package config

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"kiro-proxy/db"
)

func GenerateMachineId() string {
	bytes := make([]byte, 16)
	rand.Read(bytes)
	bytes[6] = (bytes[6] & 0x0f) | 0x40
	bytes[8] = (bytes[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		bytes[0:4], bytes[4:6], bytes[6:8], bytes[8:10], bytes[10:16])
}

type Account struct {
	ID       string `json:"id"`
	Email    string `json:"email,omitempty"`
	UserId   string `json:"userId,omitempty"`
	Nickname string `json:"nickname,omitempty"`
	ApiKeyID string `json:"apiKeyId,omitempty"`

	AccessToken             string `json:"accessToken"`
	RefreshToken            string `json:"refreshToken"`
	RefreshTokenFingerprint string `json:"refreshTokenFingerprint,omitempty"`
	//! KiroApiKey (ksk_...) is a headless credential: used as the Bearer token directly, never OAuth-refreshed.
	KiroApiKey   string `json:"kiroApiKey,omitempty"`
	ClientID     string `json:"clientId,omitempty"`
	ClientSecret string `json:"clientSecret,omitempty"`
	AuthMethod   string `json:"authMethod"`
	Provider     string `json:"provider,omitempty"`
	Region       string `json:"region"`
	StartUrl     string `json:"startUrl,omitempty"`
	ExpiresAt    int64  `json:"expiresAt,omitempty"`
	MachineId    string `json:"machineId,omitempty"`
	ProfileArn   string `json:"profileArn,omitempty"`

	//! Microsoft Enterprise SSO (AuthMethod "external_idp") refreshes against the tenant's Entra token endpoint, not AWS OIDC.
	TokenEndpoint string `json:"tokenEndpoint,omitempty"`
	IssuerURL     string `json:"issuerUrl,omitempty"`
	Scopes        string `json:"scopes,omitempty"`

	ProxyURL string `json:"proxyURL,omitempty"`

	Weight int `json:"weight,omitempty"`

	OverageStatus      string  `json:"overageStatus,omitempty"`
	OverageCapability  string  `json:"overageCapability,omitempty"`
	OverageCap         float64 `json:"overageCap,omitempty"`
	OverageRate        float64 `json:"overageRate,omitempty"`
	CurrentOverages    float64 `json:"currentOverages,omitempty"`
	OverageCheckedAt   int64   `json:"overageCheckedAt,omitempty"`
	LegacyAllowOverage bool    `json:"allowOverage,omitempty"`

	Enabled      bool   `json:"enabled"`
	Silent       bool   `json:"silent,omitempty"`
	SilentReason string `json:"silentReason,omitempty"`
	SilentTime   int64  `json:"silentTime,omitempty"`
	BanStatus    string `json:"banStatus,omitempty"`
	BanReason    string `json:"banReason,omitempty"`
	BanTime      int64  `json:"banTime,omitempty"`

	SubscriptionType  string `json:"subscriptionType,omitempty"`
	SubscriptionTitle string `json:"subscriptionTitle,omitempty"`
	DaysRemaining     int    `json:"daysRemaining,omitempty"`

	UsageCurrent  float64 `json:"usageCurrent,omitempty"`
	UsageLimit    float64 `json:"usageLimit,omitempty"`
	UsagePercent  float64 `json:"usagePercent,omitempty"`
	NextResetDate string  `json:"nextResetDate,omitempty"`
	LastRefresh   int64   `json:"lastRefresh,omitempty"`

	TrialUsageCurrent float64 `json:"trialUsageCurrent,omitempty"`
	TrialUsageLimit   float64 `json:"trialUsageLimit,omitempty"`
	TrialUsagePercent float64 `json:"trialUsagePercent,omitempty"`
	TrialStatus       string  `json:"trialStatus,omitempty"`
	TrialExpiresAt    int64   `json:"trialExpiresAt,omitempty"`

	RequestCount int     `json:"requestCount,omitempty"`
	ErrorCount   int     `json:"errorCount,omitempty"`
	LastUsed     int64   `json:"lastUsed,omitempty"`
	TotalTokens  int     `json:"totalTokens,omitempty"`
	TotalCredits float64 `json:"totalCredits,omitempty"`
}

type PromptFilterRule struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Type    string `json:"type"`
	Match   string `json:"match"`
	Replace string `json:"replace,omitempty"`
	Enabled bool   `json:"enabled"`
}

type ModelMappingRule struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type AccountPatch struct {
	ApiKeyID     *string
	Nickname     *string
	Enabled      *bool
	MachineID    *string
	Weight       *int
	ProxyURL     *string
	Silent       *bool
	SilentReason *string
	SilentTime   *int64
	BanStatus    *string
	BanReason    *string
	BanTime      *int64
}

type PromptCacheSettings struct {
	Enabled       bool    `json:"promptCacheEmulationEnabled"`
	Mode          string  `json:"promptCacheEmulationMode"`
	Ratio         float64 `json:"promptCacheEmulationRatio"`
	CreationRatio float64 `json:"promptCacheCreationEmulationRatio"`
	ReadRatio     float64 `json:"promptCacheReadEmulationRatio"`
}

const (
	PromptCacheModeUniform     = "uniform"
	PromptCacheModeIndependent = "independent"
	promptCacheModeUniform     = PromptCacheModeUniform
	promptCacheModeIndependent = PromptCacheModeIndependent
)

func ValidatePromptCacheRatio(r float64) error {
	if math.IsNaN(r) || math.IsInf(r, 0) || r < 0 || r > 1 {
		return fmt.Errorf("prompt cache ratio must be finite and within [0,1], got %v", r)
	}
	return nil
}
func ValidatePromptCacheSettings(s PromptCacheSettings) error {
	if s.Mode != promptCacheModeUniform && s.Mode != promptCacheModeIndependent {
		return fmt.Errorf("prompt cache mode must be %q or %q, got %q", promptCacheModeUniform, promptCacheModeIndependent, s.Mode)
	}
	for n, v := range map[string]float64{"ratio": s.Ratio, "creation ratio": s.CreationRatio, "read ratio": s.ReadRatio} {
		if err := ValidatePromptCacheRatio(v); err != nil {
			return fmt.Errorf("invalid prompt cache %s: %w", n, err)
		}
	}
	return nil
}

type ApiKeyEntry struct {
	ID         string `json:"id"`
	Name       string `json:"name,omitempty"`
	Key        string `json:"key"`
	Enabled    bool   `json:"enabled"`
	Migrated   bool   `json:"migrated,omitempty"`
	CreatedAt  int64  `json:"createdAt"`
	LastUsedAt int64  `json:"lastUsedAt,omitempty"`

	TokenLimit  int64   `json:"tokenLimit,omitempty"`
	CreditLimit float64 `json:"creditLimit,omitempty"`

	TokensUsed                        int64    `json:"tokensUsed,omitempty"`
	CreditsUsed                       float64  `json:"creditsUsed,omitempty"`
	RequestsCount                     int64    `json:"requestsCount,omitempty"`
	PromptCacheEmulationEnabled       *bool    `json:"promptCacheEmulationEnabled,omitempty"`
	PromptCacheEmulationMode          *string  `json:"promptCacheEmulationMode,omitempty"`
	PromptCacheEmulationRatio         *float64 `json:"promptCacheEmulationRatio,omitempty"`
	PromptCacheCreationEmulationRatio *float64 `json:"promptCacheCreationEmulationRatio,omitempty"`
	PromptCacheReadEmulationRatio     *float64 `json:"promptCacheReadEmulationRatio,omitempty"`
}

type Config struct {
	Password                          string        `json:"password"`
	Port                              int           `json:"port"`
	Host                              string        `json:"host"`
	RequireApiKey                     bool          `json:"requireApiKey"`
	ApiKey                            string        `json:"apiKey,omitempty"`
	ApiKeys                           []ApiKeyEntry `json:"apiKeys,omitempty"`
	KiroVersion                       string        `json:"kiroVersion,omitempty"`
	SystemVersion                     string        `json:"systemVersion,omitempty"`
	NodeVersion                       string        `json:"nodeVersion,omitempty"`
	Accounts                          []Account     `json:"accounts"`
	ApiKeyAccountMigrationCompleted   bool          `json:"apiKeyAccountMigrationCompleted,omitempty"`
	PromptCacheEmulationEnabled       bool          `json:"promptCacheEmulationEnabled"`
	PromptCacheEmulationMode          string        `json:"promptCacheEmulationMode"`
	PromptCacheEmulationRatio         float64       `json:"promptCacheEmulationRatio"`
	PromptCacheCreationEmulationRatio float64       `json:"promptCacheCreationEmulationRatio"`
	PromptCacheReadEmulationRatio     float64       `json:"promptCacheReadEmulationRatio"`

	ThinkingSuffix       string `json:"thinkingSuffix,omitempty"`
	OpenAIThinkingFormat string `json:"openaiThinkingFormat,omitempty"`
	ClaudeThinkingFormat string `json:"claudeThinkingFormat,omitempty"`

	PreferredEndpoint string `json:"preferredEndpoint,omitempty"`

	EndpointFallback *bool `json:"endpointFallback,omitempty"`

	AllowOverUsage bool `json:"allowOverUsage,omitempty"`

	LenientStreamIntegrity bool `json:"lenientStreamIntegrity,omitempty"`

	SessionAffinity *bool `json:"sessionAffinity,omitempty"`

	FirstRunPasswordPending bool `json:"firstRunPasswordPending,omitempty"`

	ProxyURL string `json:"proxyURL,omitempty"`

	FilterClaudeCode         bool `json:"filterClaudeCode,omitempty"`
	SanitizeClaudeCodePrompt bool `json:"sanitizeClaudeCodePrompt,omitempty"`

	FilterEnvNoise bool `json:"filterEnvNoise,omitempty"`

	FilterStripBoundaries bool `json:"filterStripBoundaries,omitempty"`

	PromptFilterRules []PromptFilterRule `json:"promptFilterRules,omitempty"`

	ModelMappings []ModelMappingRule `json:"modelMappings"`

	LogLevel string `json:"logLevel,omitempty"`

	MaxRetriesPerAccount int `json:"maxRetriesPerAccount,omitempty"`
	MaxRetriesPerRequest int `json:"maxRetriesPerRequest,omitempty"`
	RetryBaseDelayMs     int `json:"retryBaseDelayMs,omitempty"`
	RetryMaxDelayMs      int `json:"retryMaxDelayMs,omitempty"`

	TotalRequests   int     `json:"totalRequests,omitempty"`
	SuccessRequests int     `json:"successRequests,omitempty"`
	FailedRequests  int     `json:"failedRequests,omitempty"`
	TotalTokens     int     `json:"totalTokens,omitempty"`
	TotalCredits    float64 `json:"totalCredits,omitempty"`

	Backup BackupConfig `json:"backup,omitempty"`
}

type AccountInfo struct {
	Email             string
	UserId            string
	SubscriptionType  string
	SubscriptionTitle string
	DaysRemaining     int
	UsageCurrent      float64
	UsageLimit        float64
	UsagePercent      float64
	NextResetDate     string
	LastRefresh       int64
	TrialUsageCurrent float64
	TrialUsageLimit   float64
	TrialUsagePercent float64
	TrialStatus       string
	TrialExpiresAt    int64
}

const Version = "2.1.0"

var (
	cfg     *Config
	cfgLock sync.RWMutex
)

var defaultModelMappings = []ModelMappingRule{
	{Key: "claude-sonnet-4-20250514", Value: "claude-sonnet-5"},
	{Key: "claude-3-5-sonnet", Value: "claude-sonnet-5"},
	{Key: "claude-3-sonnet", Value: "claude-sonnet-5"},
	{Key: "claude-3-opus", Value: "claude-opus-5"},
	{Key: "claude-3-haiku", Value: "claude-haiku-4.5"},
	{Key: "gpt-5.4-mini", Value: "claude-haiku-4.5"},
	{Key: "gpt-5", Value: "claude-opus-5"},
	{Key: "gpt-4-turbo", Value: "claude-sonnet-5"},
	{Key: "gpt-3.5-turbo", Value: "claude-sonnet-5"},
}

func Init(path string) error {
	dir := filepath.Dir(path)
	if err := db.Init(dir); err != nil {
		return fmt.Errorf("db init: %w", err)
	}
	legacyPath := os.Getenv("CONFIG_PATH")
	if legacyPath == "" {
		if strings.EqualFold(filepath.Ext(path), ".json") {
			legacyPath = path
		} else {
			legacyPath = filepath.Join(dir, "config.json")
		}
	}
	if err := ImportLegacyJSON(legacyPath); err != nil {
		return err
	}
	if err := Load(); err != nil {
		return err
	}

	if err := LoadCredentials(); err != nil {
		return fmt.Errorf("load credentials: %w", err)
	}
	if err := migrateLoadedAccountOwnership(); err != nil {
		return err
	}

	return nil
}

func Load() error {
	cfgLock.Lock()
	defer cfgLock.Unlock()

	raw, ok, err := getSetting("config")
	if err != nil {
		return err
	}
	if !ok {
		password := os.Getenv("ADMIN_PASSWORD")
		generated := password == ""
		if generated {
			var err error
			if password, err = generateAdminPassword(); err != nil {
				return err
			}
		}
		cfg = &Config{
			Password:                          password,
			FirstRunPasswordPending:           generated,
			Port:                              8080,
			Host:                              "0.0.0.0",
			RequireApiKey:                     false,
			Accounts:                          []Account{},
			PromptCacheEmulationEnabled:       true,
			PromptCacheEmulationMode:          promptCacheModeUniform,
			PromptCacheEmulationRatio:         1,
			PromptCacheCreationEmulationRatio: 1,
			PromptCacheReadEmulationRatio:     1,
		}
		if err := saveLocked(); err != nil {
			return err
		}
		return nil
	}

	var c Config
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		return err
	}
	before, _ := json.Marshal(&c)
	if err := normalizeCompatibleConfig(&c, []byte(raw)); err != nil {
		return err
	}
	after, _ := json.Marshal(&c)
	previous := cfg
	cfg = &c
	if string(before) != string(after) {
		if err := saveLocked(); err != nil {
			cfg = previous
			return err
		}
	}
	_, err = shortenPendingAdminPasswordLocked()
	if err != nil {
		return err
	}
	return nil
}

func Save() error {
	AutoSnapshotBeforeSave()
	return saveLocked()
}

// saveLocked writes the in-memory cfg to settings without acquiring cfgLock.
// Caller must already hold cfgLock for writing.
func saveLocked() error {
	data, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	return setSetting("config", string(data))
}

func SetPassword(password string) {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.Password = password
	cfg.FirstRunPasswordPending = false
}

func Get() *Config {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return nil
	}
	copyCfg := *cfg
	if cfg.Accounts != nil {
		copyCfg.Accounts = append([]Account(nil), cfg.Accounts...)
	}
	if cfg.ApiKeys != nil {
		copyCfg.ApiKeys = make([]ApiKeyEntry, len(cfg.ApiKeys))
		for i := range cfg.ApiKeys {
			copyCfg.ApiKeys[i] = cloneApiKeyEntry(cfg.ApiKeys[i])
		}
	}
	if cfg.EndpointFallback != nil {
		value := *cfg.EndpointFallback
		copyCfg.EndpointFallback = &value
	}
	if cfg.SessionAffinity != nil {
		value := *cfg.SessionAffinity
		copyCfg.SessionAffinity = &value
	}
	if cfg.PromptFilterRules != nil {
		copyCfg.PromptFilterRules = append([]PromptFilterRule(nil), cfg.PromptFilterRules...)
	}
	if cfg.ModelMappings != nil {
		copyCfg.ModelMappings = copyModelMappings(cfg.ModelMappings)
	}
	return &copyCfg
}

func GetPassword() string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	return cfg.Password
}

func GetPort() int {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg.Port == 0 {
		return 8080
	}
	return cfg.Port
}

func GetHost() string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg.Host == "" {
		return "127.0.0.1"
	}
	return cfg.Host
}

func GetDataDir() string {
	return db.DataDir()
}

func GetAccounts() []Account {

	if CredentialsLoaded() {
		return GetCredentials()
	}

	cfgLock.RLock()
	defer cfgLock.RUnlock()
	accounts := make([]Account, len(cfg.Accounts))
	copy(accounts, cfg.Accounts)
	return accounts
}

func GetEnabledAccounts() []Account {

	all := GetAccounts()
	var accounts []Account
	for _, a := range all {
		if a.Enabled && !a.Silent && (a.BanStatus == "" || a.BanStatus == "ACTIVE") {
			accounts = append(accounts, a)
		}
	}
	return accounts
}

func AddAccount(account Account) error { return addAccount(account, false) }

func UpdateAccount(id string, account Account) error {
	_, err := mutateAccount(id, false, func(current *Account) error {
		preserveAccountCredentials(&account, *current)
		account.ID = current.ID
		*current = account
		return nil
	})
	return err
}

// AuthMethodExternalIdp marks a Microsoft Enterprise SSO account.
const AuthMethodExternalIdp = "external_idp"

func IsExternalIdpAccount(account *Account) bool {
	return account != nil && strings.EqualFold(strings.TrimSpace(account.AuthMethod), AuthMethodExternalIdp)
}

// Status/admin updates must not replace credentials rotated after their snapshot.
func preserveAccountCredentials(account *Account, current Account) {
	account.AccessToken = current.AccessToken
	account.RefreshToken = current.RefreshToken
	account.RefreshTokenFingerprint = current.RefreshTokenFingerprint
	if account.RefreshTokenFingerprint == "" {
		account.RefreshTokenFingerprint = RefreshTokenFingerprint(current.RefreshToken)
	}
	account.ApiKeyID = current.ApiKeyID
	account.KiroApiKey = current.KiroApiKey
	account.ClientID = current.ClientID
	account.ClientSecret = current.ClientSecret
	account.AuthMethod = current.AuthMethod
	account.Provider = current.Provider
	account.Region = current.Region
	account.StartUrl = current.StartUrl
	account.ExpiresAt = current.ExpiresAt
	account.ProfileArn = current.ProfileArn
	account.TokenEndpoint = current.TokenEndpoint
	account.IssuerURL = current.IssuerURL
	account.Scopes = current.Scopes
}

func UpdateAccountOverageStatus(id, status, capability string, cap, rate, current float64, checkedAt int64) error {
	_, err := mutateAccount(id, false, func(account *Account) error {
		if status != "" {
			account.OverageStatus = status
		}
		if capability != "" {
			account.OverageCapability = capability
		}
		account.OverageCap = cap
		account.OverageRate = rate
		account.CurrentOverages = current
		if checkedAt > 0 {
			account.OverageCheckedAt = checkedAt
		}
		return nil
	})
	return err
}
func DisableAccountOverage(id string) error {
	return UpdateAccountOverageStatus(id, "DISABLED", "", 0, 0, 0, time.Now().Unix())
}
func SetAccountEnabled(id string, enabled bool) error {
	_, err := mutateAccount(id, false, func(account *Account) error {
		account.Enabled = enabled
		if !enabled {
			account.BanStatus = "DISABLED"
			account.BanTime = time.Now().Unix()
		}
		return nil
	})
	return err
}
func SetAccountBanStatus(id, status, reason string) error {
	_, err := mutateAccount(id, false, func(account *Account) error {
		account.BanStatus = status
		account.BanReason = reason
		account.BanTime = time.Now().Unix()
		if status == "BANNED" || status == "DISABLED" {
			account.Enabled = false
		}
		return nil
	})
	return err
}
func UpdateAccountProfileArn(id, profileArn string) error {
	_, err := mutateAccount(id, false, func(account *Account) error { account.ProfileArn = profileArn; return nil })
	return err
}
func DeleteAccount(id string) error { return deleteAccount(id, false) }
func UpdateAccountToken(id, accessToken, refreshToken string, expiresAt int64) error {
	return UpdateAccountCredentialState(id, accessToken, refreshToken, expiresAt, "")
}

func IsApiKeyRequired() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	return cfg.RequireApiKey
}

func UpdateSettingsPatch(requireApiKey *bool, password string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if requireApiKey != nil {
		cfg.RequireApiKey = *requireApiKey
	}
	if password != "" {
		cfg.Password = password
		cfg.FirstRunPasswordPending = false
	}
	return Save()
}

func UpdateStats(totalReq, successReq, failedReq, totalTokens int, totalCredits float64) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.TotalRequests = totalReq
	cfg.SuccessRequests = successReq
	cfg.FailedRequests = failedReq
	cfg.TotalTokens = totalTokens
	cfg.TotalCredits = totalCredits
	return Save()
}

func GetStats() (int, int, int, int, float64) {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	return cfg.TotalRequests, cfg.SuccessRequests, cfg.FailedRequests, cfg.TotalTokens, cfg.TotalCredits
}

func UpdateAccountStats(id string, requestCount, errorCount, totalTokens int, totalCredits float64, lastUsed int64) error {

	if CredentialsLoaded() {
		return UpdateCredentialStats(id, requestCount, errorCount, totalTokens, totalCredits, lastUsed)
	}

	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i, a := range cfg.Accounts {
		if a.ID == id {
			cfg.Accounts[i].RequestCount = requestCount
			cfg.Accounts[i].ErrorCount = errorCount
			cfg.Accounts[i].TotalTokens = totalTokens
			cfg.Accounts[i].TotalCredits = totalCredits
			cfg.Accounts[i].LastUsed = lastUsed
			return Save()
		}
	}
	return nil
}

func UpdateAccountInfo(id string, info AccountInfo) error {

	if CredentialsLoaded() {

		acc := GetCredentialByID(id)
		if acc != nil {
			if info.Email != "" {
				acc.Email = info.Email
			}
			if info.UserId != "" {
				acc.UserId = info.UserId
			}
			if info.Email != "" || info.UserId != "" {
				if err := UpdateCredential(*acc); err != nil {
					return err
				}
			}
		}
		return UpdateCredentialInfo(id, info)
	}

	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i, a := range cfg.Accounts {
		if a.ID == id {
			if info.Email != "" {
				cfg.Accounts[i].Email = info.Email
			}
			if info.UserId != "" {
				cfg.Accounts[i].UserId = info.UserId
			}
			cfg.Accounts[i].SubscriptionType = info.SubscriptionType
			cfg.Accounts[i].SubscriptionTitle = info.SubscriptionTitle
			cfg.Accounts[i].DaysRemaining = info.DaysRemaining
			cfg.Accounts[i].UsageCurrent = info.UsageCurrent
			cfg.Accounts[i].UsageLimit = info.UsageLimit
			cfg.Accounts[i].UsagePercent = info.UsagePercent
			cfg.Accounts[i].NextResetDate = info.NextResetDate
			cfg.Accounts[i].LastRefresh = info.LastRefresh
			cfg.Accounts[i].TrialUsageCurrent = info.TrialUsageCurrent
			cfg.Accounts[i].TrialUsageLimit = info.TrialUsageLimit
			cfg.Accounts[i].TrialUsagePercent = info.TrialUsagePercent
			cfg.Accounts[i].TrialStatus = info.TrialStatus
			cfg.Accounts[i].TrialExpiresAt = info.TrialExpiresAt
			return Save()
		}
	}
	return nil
}

func GetFilterClaudeCode() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return false
	}
	return cfg.FilterClaudeCode
}

func GetFilterEnvNoise() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return false
	}
	return cfg.FilterEnvNoise
}

func GetFilterStripBoundaries() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return false
	}
	return cfg.FilterStripBoundaries
}

type PromptFilterConfig struct {
	FilterClaudeCode      bool               `json:"filterClaudeCode"`
	FilterEnvNoise        bool               `json:"filterEnvNoise"`
	FilterStripBoundaries bool               `json:"filterStripBoundaries"`
	Rules                 []PromptFilterRule `json:"rules"`
}

func GetPromptFilterConfig() PromptFilterConfig {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return PromptFilterConfig{Rules: []PromptFilterRule{}}
	}
	rules := make([]PromptFilterRule, len(cfg.PromptFilterRules))
	copy(rules, cfg.PromptFilterRules)
	return PromptFilterConfig{
		FilterClaudeCode:      cfg.FilterClaudeCode,
		FilterEnvNoise:        cfg.FilterEnvNoise,
		FilterStripBoundaries: cfg.FilterStripBoundaries,
		Rules:                 rules,
	}
}

func UpdatePromptFilterConfig(filterClaudeCode, filterEnvNoise, filterStripBoundaries bool, rules []PromptFilterRule) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.FilterClaudeCode = filterClaudeCode
	cfg.FilterEnvNoise = filterEnvNoise
	cfg.FilterStripBoundaries = filterStripBoundaries
	if rules != nil {
		cfg.PromptFilterRules = rules
	}
	return Save()
}

func GetPromptFilterRules() []PromptFilterRule {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return nil
	}
	rules := make([]PromptFilterRule, len(cfg.PromptFilterRules))
	copy(rules, cfg.PromptFilterRules)
	return rules
}

func DefaultModelMappings() []ModelMappingRule {
	return copyModelMappings(defaultModelMappings)
}

func GetModelMappings() []ModelMappingRule {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil || cfg.ModelMappings == nil {
		return DefaultModelMappings()
	}
	return copyModelMappings(cfg.ModelMappings)
}

func UpdateModelMappings(mappings []ModelMappingRule) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cleaned := make([]ModelMappingRule, 0, len(mappings))
	for _, mapping := range mappings {
		key := strings.ToLower(strings.TrimSpace(mapping.Key))
		value := strings.TrimSpace(mapping.Value)
		if key == "" || value == "" {
			continue
		}
		cleaned = append(cleaned, ModelMappingRule{Key: key, Value: value})
	}
	cfg.ModelMappings = cleaned
	return Save()
}

func copyModelMappings(mappings []ModelMappingRule) []ModelMappingRule {
	if mappings == nil {
		return nil
	}
	copied := make([]ModelMappingRule, len(mappings))
	copy(copied, mappings)
	return copied
}

type ThinkingConfig struct {
	Suffix       string `json:"suffix"`
	OpenAIFormat string `json:"openaiFormat"`
	ClaudeFormat string `json:"claudeFormat"`
}

func GetThinkingConfig() ThinkingConfig {
	cfgLock.RLock()
	defer cfgLock.RUnlock()

	suffix := cfg.ThinkingSuffix
	if suffix == "" {
		suffix = "-thinking"
	}
	openaiFormat := cfg.OpenAIThinkingFormat
	if openaiFormat == "" {
		openaiFormat = "reasoning_content"
	}
	claudeFormat := cfg.ClaudeThinkingFormat
	if claudeFormat == "" {
		claudeFormat = "thinking"
	}

	return ThinkingConfig{
		Suffix:       suffix,
		OpenAIFormat: openaiFormat,
		ClaudeFormat: claudeFormat,
	}
}

func UpdateThinkingConfig(suffix, openaiFormat, claudeFormat string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.ThinkingSuffix = suffix
	cfg.OpenAIThinkingFormat = openaiFormat
	cfg.ClaudeThinkingFormat = claudeFormat
	return Save()
}

func GetPreferredEndpoint() string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg.PreferredEndpoint == "" {
		return "auto"
	}
	return cfg.PreferredEndpoint
}

func UpdatePreferredEndpoint(endpoint string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.PreferredEndpoint = endpoint
	return Save()
}

func GetEndpointFallback() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg.EndpointFallback == nil {
		return true
	}
	return *cfg.EndpointFallback
}

func UpdateEndpointFallback(enabled bool) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.EndpointFallback = &enabled
	return Save()
}

func GetProxyURL() string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	return cfg.ProxyURL
}

func UpdateProxySettings(proxyURL string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.ProxyURL = proxyURL
	return Save()
}

func GetAllowOverUsage() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return false
	}
	return cfg.AllowOverUsage
}

func UpdateAllowOverUsage(allow bool) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.AllowOverUsage = allow
	return Save()
}

// GetLenientStreamIntegrity reports whether a stream carrying answer text counts
// as complete without metering or a stop reason. Off by default: the same shape
// is what a stream truncated mid-answer leaves behind.
func GetLenientStreamIntegrity() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return false
	}
	return cfg.LenientStreamIntegrity
}

func UpdateLenientStreamIntegrity(lenient bool) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.LenientStreamIntegrity = lenient
	return Save()
}

// GetSessionAffinity reports whether follow-up turns of a conversation go back
// to the account that served the previous turn. On by default: that account
// already holds the conversation's prompt cache.
func GetSessionAffinity() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil || cfg.SessionAffinity == nil {
		return true
	}
	return *cfg.SessionAffinity
}

func UpdateSessionAffinity(enabled bool) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.SessionAffinity = &enabled
	return Save()
}

func GetLogLevel() string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return "info"
	}
	if cfg.LogLevel == "" {
		return "info"
	}
	return cfg.LogLevel
}

func GetRetryConfig() (maxPerAccount, maxPerRequest, baseDelayMs, maxDelayMs int) {
	cfgLock.RLock()
	defer cfgLock.RUnlock()

	maxPerAccount = 3
	maxPerRequest = 9
	baseDelayMs = 100
	maxDelayMs = 5000

	if cfg != nil {
		if cfg.MaxRetriesPerAccount > 0 {
			maxPerAccount = cfg.MaxRetriesPerAccount
		}
		if cfg.MaxRetriesPerRequest > 0 {
			maxPerRequest = cfg.MaxRetriesPerRequest
		}
		if cfg.RetryBaseDelayMs > 0 {
			baseDelayMs = cfg.RetryBaseDelayMs
		}
		if cfg.RetryMaxDelayMs > 0 {
			maxDelayMs = cfg.RetryMaxDelayMs
		}
	}

	return
}

func UpdateLogLevel(level string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.LogLevel = level
	return Save()
}

type KiroClientConfig struct {
	KiroVersion   string
	SystemVersion string
	NodeVersion   string
}

func GetKiroClientConfig() KiroClientConfig {
	cfgLock.RLock()
	defer cfgLock.RUnlock()

	kiroVersion := "0.11.107"
	if cfg != nil && cfg.KiroVersion != "" {
		kiroVersion = cfg.KiroVersion
	}

	systemVersion := ""
	if cfg != nil {
		systemVersion = cfg.SystemVersion
	}
	if systemVersion == "" {
		systemVersion = defaultSystemVersion()
	}

	nodeVersion := "22.22.0"
	if cfg != nil && cfg.NodeVersion != "" {
		nodeVersion = cfg.NodeVersion
	}

	return KiroClientConfig{
		KiroVersion:   kiroVersion,
		SystemVersion: systemVersion,
		NodeVersion:   nodeVersion,
	}
}

func defaultSystemVersion() string {
	switch runtime.GOOS {
	case "windows":
		return "win32#10.0.22631"
	case "darwin":
		return "darwin#24.6.0"
	default:
		return "linux#6.6.87"
	}
}
