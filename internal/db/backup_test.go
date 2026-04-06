package db

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBackup_CreatesFile(t *testing.T) {
	d := openTestDB(t)
	dest := filepath.Join(t.TempDir(), "backup.db")

	if err := d.Backup(dest); err != nil {
		t.Fatalf("Backup: %v", err)
	}

	fi, err := os.Stat(dest)
	if err != nil {
		t.Fatalf("backup file not created: %v", err)
	}
	if fi.Size() == 0 {
		t.Error("backup file is empty")
	}
}

func TestBackup_FailsOnExistingFile(t *testing.T) {
	d := openTestDB(t)
	dest := filepath.Join(t.TempDir(), "backup.db")

	if err := os.WriteFile(dest, []byte("existing"), 0644); err != nil {
		t.Fatalf("setup: %v", err)
	}

	if err := d.Backup(dest); err == nil {
		t.Error("Backup should fail when destination already exists")
	}
}
