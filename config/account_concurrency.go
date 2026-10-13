package config

import "errors"

const DefaultAccountMaxConcurrency = 5
const MaxAccountMaxConcurrency = 2147483647

var ErrInvalidAccountConcurrency = errors.New("maxConcurrency must be an integer between 1 and 2147483647")

func (a Account) ConcurrencyLimit() int {
	if a.MaxConcurrency < 1 {
		return DefaultAccountMaxConcurrency
	}
	return a.MaxConcurrency
}

// Read the current setting instead of a pool snapshot captured before an admin edit.
func GetAccountConcurrencyLimit(account *Account) int {
	if account == nil {
		return DefaultAccountMaxConcurrency
	}
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	credLock.RLock()
	defer credLock.RUnlock()
	if account.ID != "" {
		var accounts []Account
		if credLoaded {
			accounts = credentials
		} else if cfg != nil {
			accounts = cfg.Accounts
		}
		for _, current := range accounts {
			if current.ID == account.ID {
				return current.ConcurrencyLimit()
			}
		}
	}
	return account.ConcurrencyLimit()
}
