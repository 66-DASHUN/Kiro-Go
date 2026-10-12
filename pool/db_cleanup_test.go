package pool

import (
	"kiro-proxy/db"
	"testing"
)

// Register close after TempDir so LIFO cleanup releases SQLite before removal.
func poolTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close SQLite: %v", err)
		}
	})
	return dir
}
