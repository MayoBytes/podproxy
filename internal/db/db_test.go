package db

import "testing"

// openTestDB opens a temporary SQLite database for testing.
// It is closed automatically when the test ends.
func openTestDB(t *testing.T) *DB {
	t.Helper()
	d, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open test DB: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}
