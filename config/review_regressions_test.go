package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"kiro-proxy/db"
)

func TestReviewAccountPatchAppliesOnlyExplicitStatusFields(t *testing.T) {
	if err := Init(compatibilityDB(t)); err != nil {
		t.Fatal(err)
	}
	before := Account{ID: "account", RefreshToken: "refresh", Silent: true, SilentReason: "pause", SilentTime: 12, BanStatus: "BANNED", BanReason: "reason", BanTime: 13}
	if err := AddAccount(before); err != nil {
		t.Fatal(err)
	}
	before = GetAccounts()[0]
	var patch AccountPatch
	if err := json.Unmarshal([]byte(`{"Silent":false,"SilentReason":"","SilentTime":0,"BanStatus":"ACTIVE","BanReason":"","BanTime":0}`), &patch); err != nil {
		t.Fatal(err)
	}
	got, err := UpdateAccountPatch("account", patch)
	if err != nil {
		t.Fatal(err)
	}
	if got.Silent || got.SilentReason != "" || got.SilentTime != 0 || got.BanStatus != "ACTIVE" || got.BanReason != "" || got.BanTime != 0 {
		t.Fatal("explicit status patch fields ignored")
	}
	before.Silent = false
	before.SilentReason = ""
	before.SilentTime = 0
	before.BanStatus = "ACTIVE"
	before.BanReason = ""
	before.BanTime = 0
	if before != got {
		t.Fatal("status patch replaced omitted account fields")
	}
	if got2, err := UpdateAccountPatch("account", AccountPatch{}); err != nil || got2 != got {
		t.Fatal("omitted status patch fields changed")
	}
}

func TestReviewLegacyImportRejectsPartiallyPopulatedDatabase(t *testing.T) {
	for _, state := range []string{"credentials", "flag", "requests", "cooldowns", "errors", "responses", "backups"} {
		t.Run(state, func(t *testing.T) {
			path := compatibilityDB(t)
			if err := db.Init(filepath.Dir(path)); err != nil {
				t.Fatal(err)
			}
			d, err := db.Get()
			if err != nil {
				t.Fatal(err)
			}
			statement := map[string]string{
				"credentials": `INSERT INTO settings VALUES('credentials','[{"id":"native","refreshToken":"native"}]')`,
				"flag":        `INSERT INTO settings VALUES('credentials_loaded','1')`,
				"requests":    `INSERT INTO requests(ts) VALUES(1)`,
				"cooldowns":   `INSERT INTO cooldowns VALUES('native',123)`,
				"errors":      `INSERT INTO errors(ts) VALUES(1)`,
				"responses":   `INSERT INTO responses(id,created_at,response_json,messages_json) VALUES('native',1,'{}','[]')`,
				"backups":     `INSERT INTO backup_entries(id,created_at) VALUES('native',1)`,
			}[state]
			if _, err := d.Exec(statement); err != nil {
				t.Fatal(err)
			}
			legacy := filepath.Join(filepath.Dir(path), "config.json")
			if err := os.WriteFile(legacy, []byte(`{"password":"legacy","port":8321,"accounts":[{"id":"legacy","refreshToken":"legacy"}]}`), 0600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("CONFIG_PATH", legacy)
			if err := Init(path); err == nil {
				t.Fatal("nonempty database imported stale legacy JSON")
			}
			if _, ok, err := getSetting("config"); err != nil || ok {
				t.Fatal("failed import wrote configuration")
			}
		})
	}
}

func reviewBackupBytes(t *testing.T, c map[string]any, credentials []any) []byte {
	t.Helper()
	if c == nil {
		c = map[string]any{"password": "backup", "port": 8321, "host": "127.0.0.1", "accounts": []any{map[string]any{"id": "config-account", "refreshToken": "config-refresh"}}, "apiKeys": []any{map[string]any{"id": "key", "key": "sk-key", "enabled": true}}}
	}
	payload := map[string]any{"format": backupFormat, "version": backupVersion, "config": c, "credentialsLoaded": credentials != nil}
	if credentials != nil {
		payload["credentials"] = credentials
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestReviewNativeBackupRestoresMissingCacheDefaultsAndOwnership(t *testing.T) {
	path := compatibilityDB(t)
	if err := Init(path); err != nil {
		t.Fatal(err)
	}
	raw := reviewBackupBytes(t, nil, []any{map[string]any{"id": "native", "refreshToken": "native-refresh"}})
	if err := RestoreFromBytes(raw, "native old backup"); err != nil {
		t.Fatal(err)
	}
	if got := EffectivePromptCacheSettings("key"); got != defaultPromptCacheSettings() {
		t.Fatalf("missing backup defaults became zero policy: %+v", got)
	}
	if !Get().ApiKeyAccountMigrationCompleted || Get().Accounts[0].ApiKeyID != "key" || GetAccounts()[0].ApiKeyID != "key" {
		t.Fatal("backup ownership migration was not performed on both stores")
	}
	if Get().ModelMappings != nil {
		t.Fatal("native backup adopted legacy deployment aliases")
	}
	if GetAccounts()[0].RefreshTokenFingerprint != RefreshTokenFingerprint("native-refresh") {
		t.Fatal("credential fingerprint not normalized")
	}
	if err := Init(path); err != nil {
		t.Fatal(err)
	}
	if EffectivePromptCacheSettings("key") != defaultPromptCacheSettings() {
		t.Fatal("backup defaults lost on restart")
	}
}

func TestReviewNativeBackupRejectsInvalidCacheAndUnknownOwnership(t *testing.T) {
	for _, field := range []string{"global mode", "global ratio", "override mode", "override ratio", "unknown ownership"} {
		t.Run(field, func(t *testing.T) {
			if err := Init(compatibilityDB(t)); err != nil {
				t.Fatal(err)
			}
			before := GetPort()
			var fields map[string]any
			if err := json.Unmarshal(reviewBackupBytes(t, nil, nil), &fields); err != nil {
				t.Fatal(err)
			}
			c := fields["config"].(map[string]any)
			switch field {
			case "global mode":
				c["promptCacheEmulationMode"] = "invalid"
			case "global ratio":
				c["promptCacheReadEmulationRatio"] = 2
			case "override mode":
				c["apiKeys"].([]any)[0].(map[string]any)["promptCacheEmulationMode"] = "invalid"
			case "override ratio":
				c["apiKeys"].([]any)[0].(map[string]any)["promptCacheEmulationRatio"] = 2
			case "unknown ownership":
				c["accounts"].([]any)[0].(map[string]any)["apiKeyId"] = "missing"
			}
			raw := reviewBackupBytes(t, c, nil)
			if err := RestoreFromBytes(raw, "invalid"); err == nil {
				t.Fatal("invalid backup accepted")
			}
			if GetPort() != before {
				t.Fatal("invalid backup changed config")
			}
		})
	}
}

func TestReviewBackupKeepsExplicitEmptyModelMappings(t *testing.T) {
	if err := Init(compatibilityDB(t)); err != nil {
		t.Fatal(err)
	}
	if err := UpdateModelMappings([]ModelMappingRule{}); err != nil {
		t.Fatal(err)
	}
	if Get().ModelMappings == nil {
		t.Error("Get snapshot collapsed explicit empty mappings")
	}
	data, _, _, _, _, err := currentBackupData()
	if err != nil {
		t.Fatal(err)
	}
	var payload backupPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Config.ModelMappings == nil {
		t.Error("manual backup collapsed explicit empty mappings")
	}
	if err := RestoreFromBytes(data, "empty mappings"); err != nil {
		t.Fatal(err)
	}
	if GetModelMappings() == nil || len(GetModelMappings()) != 0 {
		t.Fatal("restored empty mappings became defaults")
	}
	if err := UpdateBackupConfig(BackupConfig{AutoEnabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := UpdateSettingsPatch(nil, "changed-password"); err != nil {
		t.Fatal(err)
	}
	entries, err := ListBackups(true)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Kind == "auto" {
			_, data, err := readBackupBytesLocked(entry.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(data, &payload); err != nil {
				t.Fatal(err)
			}
			if payload.Config.ModelMappings == nil {
				t.Fatal("auto backup collapsed explicit empty mappings")
			}
		}
	}
}

func TestReviewBackupPreservesExplicitZeroCacheAndUnboundMarker(t *testing.T) {
	path:=compatibilityDB(t)
	if err:=Init(path);err!=nil{t.Fatal(err)}
	var envelope map[string]any
	if err:=json.Unmarshal(reviewBackupBytes(t,nil,nil),&envelope);err!=nil{t.Fatal(err)}
	c:=envelope["config"].(map[string]any)
	c["apiKeyAccountMigrationCompleted"]=true
	c["promptCacheEmulationEnabled"]=false
	c["promptCacheEmulationMode"]="independent"
	c["promptCacheEmulationRatio"]=0
	c["promptCacheCreationEmulationRatio"]=0
	c["promptCacheReadEmulationRatio"]=0
	if err:=RestoreFromBytes(reviewBackupBytes(t,c,nil),"explicit zero");err!=nil{t.Fatal(err)}
	want:=PromptCacheSettings{Mode:"independent"}
	if EffectivePromptCacheSettings("key")!=want{t.Fatal("restore replaced explicit zero policy with defaults")}
	if GetAccounts()[0].ApiKeyID!=""{t.Fatal("completed migration marker ignored during restore")}
	if err:=Init(path);err!=nil{t.Fatal(err)}
	if GetAccounts()[0].ApiKeyID!=""||EffectivePromptCacheSettings("key")!=want{t.Fatal("explicit restore values did not persist")}
}

func TestReviewBackupPreservesDisabledKeyOwnershipAndRejectsDanglingCredentialBinding(t *testing.T) {
	for _,dangling:=range []bool{false,true}{t.Run(map[bool]string{false:"disabled valid",true:"dangling invalid"}[dangling],func(t *testing.T){
		if err:=Init(compatibilityDB(t));err!=nil{t.Fatal(err)}
		var envelope map[string]any
		if err:=json.Unmarshal(reviewBackupBytes(t,nil,nil),&envelope);err!=nil{t.Fatal(err)}
		c:=envelope["config"].(map[string]any)
		c["apiKeyAccountMigrationCompleted"]=true
		c["apiKeys"].([]any)[0].(map[string]any)["enabled"]=false
		c["accounts"].([]any)[0].(map[string]any)["apiKeyId"]="key"
		binding:="key";if dangling{binding="missing"}
		creds:=[]any{map[string]any{"id":"native","refreshToken":"token","apiKeyId":binding}}
		err:=RestoreFromBytes(reviewBackupBytes(t,c,creds),"ownership")
		if dangling{if err==nil{t.Fatal("dangling credential ownership accepted")};return}
		if err!=nil{t.Fatal(err)}
		if Get().Accounts[0].ApiKeyID!="key"||GetAccounts()[0].ApiKeyID!="key"{t.Fatal("disabled key's explicit historical ownership rewritten")}
	})}
}
