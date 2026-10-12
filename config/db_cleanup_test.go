package config

import (
	"kiro-proxy/db"
	"testing"
)

// Register the SQLite close after TempDir so Windows removes the directory
// only after its database handle has been released.
func configTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Cleanup(func() { _ = db.Close() })
	return dir
}
