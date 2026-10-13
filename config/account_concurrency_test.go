package config

import (
	"encoding/json"
	"path/filepath"
	"testing"
)

func concurrencyJSON(t *testing.T, account Account) float64 {
	t.Helper()
	raw, err := json.Marshal(account)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]interface{}
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	value, _ := fields["maxConcurrency"].(float64)
	return value
}

func TestAccountConcurrencyDefaultOnAdd(t *testing.T) {
	if err := Init(filepath.Join(configTempDir(t), "kiro.db")); err != nil {
		t.Fatal(err)
	}
	if err := AddAccount(Account{ID: "new"}); err != nil {
		t.Fatal(err)
	}
	if got := concurrencyJSON(t, GetAccounts()[0]); got != 5 {
		t.Fatalf("new account concurrency=%v, want 5", got)
	}
}

func TestAccountConcurrencyPersistenceAndStaleStatus(t *testing.T) {
	for _, separate := range []bool{false, true} {
		t.Run(map[bool]string{false: "config", true: "credentials"}[separate], func(t *testing.T) {
			path := filepath.Join(configTempDir(t), "kiro.db")
			if err := Init(path); err != nil {
				t.Fatal(err)
			}
			var account Account
			if err := json.Unmarshal([]byte(`{"id":"one","accessToken":"old","refreshToken":"refresh","maxConcurrency":2}`), &account); err != nil {
				t.Fatal(err)
			}
			add := AddAccount
			if separate {
				add = AddCredential
			}
			if err := add(account); err != nil {
				t.Fatal(err)
			}
			if got := concurrencyJSON(t, GetAccounts()[0]); got != 2 {
				t.Fatalf("concurrency=%v, want 2", got)
			}
			stale := account
			// A background status snapshot predates an admin edit and must not undo it.
			if err := mutateConcurrencyForTest("one", 3); err != nil {
				t.Fatal(err)
			}
			if err := UpdateAccountToken("one", "rotated", "rotated-refresh", 123); err != nil {
				t.Fatal(err)
			}
			stale.Nickname = "status-updated"
			if err := UpdateAccount("one", stale); err != nil {
				t.Fatal(err)
			}
			if got := concurrencyJSON(t, GetAccounts()[0]); got != 3 {
				t.Fatalf("stale status overwrote concurrency: %v", got)
			}
			if err := Init(path); err != nil {
				t.Fatal(err)
			}
			got := GetAccounts()[0]
			if concurrencyJSON(t, got) != 3 || got.AccessToken != "rotated" || got.Nickname != "status-updated" {
				t.Fatalf("persisted setting/status lost")
			}
		})
	}
}

func mutateConcurrencyForTest(id string, limit int) error {
	_, err := UpdateAccountPatch(id, AccountPatch{MaxConcurrency: &limit})
	return err
}

func TestAccountConcurrencyLegacyAndBackup(t *testing.T) {
	if err := Init(filepath.Join(configTempDir(t), "kiro.db")); err != nil {
		t.Fatal(err)
	}
	if err := ReplaceCredentials(true, []Account{{ID: "legacy"}}); err != nil {
		t.Fatal(err)
	}
	legacy := GetAccounts()[0]
	if legacy.ConcurrencyLimit() != 5 || GetAccountConcurrencyLimit(&legacy) != 5 {
		t.Fatal("legacy account does not default to 5")
	}
	for _, limit := range []int{0, -1} {
		if err := mutateConcurrencyForTest("legacy", limit); err == nil {
			t.Fatalf("invalid patch %d accepted", limit)
		}
	}
	if err := mutateConcurrencyForTest("legacy", 2); err != nil {
		t.Fatal(err)
	}
	if GetAccountConcurrencyLimit(&legacy) != 2 {
		t.Fatal("stale snapshot bypasses current setting")
	}
	backup, err := CreateBackup("manual", "concurrency")
	if err != nil {
		t.Fatal(err)
	}
	if err := mutateConcurrencyForTest("legacy", 7); err != nil {
		t.Fatal(err)
	}
	if err := RestoreBackup(backup.ID); err != nil {
		t.Fatal(err)
	}
	if GetAccountConcurrencyLimit(&legacy) != 2 {
		t.Fatal("backup did not restore concurrency")
	}
}

func TestAccountConcurrencyRejectsNegativeAddition(t *testing.T) {
	if err := Init(filepath.Join(configTempDir(t), "kiro.db")); err != nil {
		t.Fatal(err)
	}
	var account Account
	json.Unmarshal([]byte(`{"id":"bad","maxConcurrency":-1}`), &account)
	if err := AddAccount(account); err == nil {
		t.Fatal("negative concurrency accepted")
	}
	if len(GetAccounts()) != 0 {
		t.Fatal("invalid addition changed accounts")
	}
}
