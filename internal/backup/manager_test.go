package backup_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"podproxy/internal/backup"
	"podproxy/internal/config"
	"podproxy/internal/db"
)

func openTestDB(t *testing.T) *db.DB {
	t.Helper()
	d, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open test DB: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func newManager(t *testing.T, d *db.DB, dir string, maxBackups int) *backup.Manager {
	t.Helper()
	cfg := &config.Config{
		Storage: config.StorageConfig{DataDir: t.TempDir()},
		Backup:  config.BackupConfig{Dir: dir, MaxBackups: maxBackups},
	}
	return backup.New(d, cfg)
}

// ---------------------------------------------------------------------------
// New
// ---------------------------------------------------------------------------

func TestNew_UsesExplicitDir(t *testing.T) {
	d := openTestDB(t)
	explicitDir := t.TempDir()
	m := newManager(t, d, explicitDir, 0)

	_, err := m.CreateBackup()
	if err != nil {
		t.Fatalf("CreateBackup: %v", err)
	}

	entries, err := os.ReadDir(explicitDir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("want 1 file in explicit dir, got %d", len(entries))
	}
}

func TestNew_DefaultsToDataDirBackups(t *testing.T) {
	d := openTestDB(t)
	dataDir := t.TempDir()
	cfg := &config.Config{
		Storage: config.StorageConfig{DataDir: dataDir},
		Backup:  config.BackupConfig{}, // no Dir
	}
	m := backup.New(d, cfg)

	if _, err := m.CreateBackup(); err != nil {
		t.Fatalf("CreateBackup: %v", err)
	}

	entries, err := os.ReadDir(filepath.Join(dataDir, "backups"))
	if err != nil {
		t.Fatalf("default backup dir not created: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("want 1 file in default dir, got %d", len(entries))
	}
}

// ---------------------------------------------------------------------------
// CreateBackup
// ---------------------------------------------------------------------------

func TestCreateBackup_ReturnsCorrectInfo(t *testing.T) {
	d := openTestDB(t)
	dir := t.TempDir()
	m := newManager(t, d, dir, 0)

	info, err := m.CreateBackup()
	if err != nil {
		t.Fatalf("CreateBackup: %v", err)
	}
	if info.Name == "" {
		t.Error("Name should not be empty")
	}
	if info.SizeBytes <= 0 {
		t.Errorf("SizeBytes should be > 0, got %d", info.SizeBytes)
	}
	if info.CreatedAt.IsZero() {
		t.Error("CreatedAt should not be zero")
	}
}

func TestCreateBackup_FileExistsOnDisk(t *testing.T) {
	d := openTestDB(t)
	dir := t.TempDir()
	m := newManager(t, d, dir, 0)

	info, err := m.CreateBackup()
	if err != nil {
		t.Fatalf("CreateBackup: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, info.Name)); err != nil {
		t.Errorf("backup file not found on disk: %v", err)
	}
}

// ---------------------------------------------------------------------------
// ListBackups
// ---------------------------------------------------------------------------

func TestListBackups_EmptyWhenDirDoesNotExist(t *testing.T) {
	d := openTestDB(t)
	nonExistent := filepath.Join(t.TempDir(), "does-not-exist")
	m := newManager(t, d, nonExistent, 0)

	backups, err := m.ListBackups()
	if err != nil {
		t.Fatalf("ListBackups: %v", err)
	}
	if len(backups) != 0 {
		t.Errorf("want 0 backups, got %d", len(backups))
	}
}

func TestListBackups_SortedNewestFirst(t *testing.T) {
	d := openTestDB(t)
	dir := t.TempDir()
	m := newManager(t, d, dir, 0)

	now := time.Now()
	files := []struct {
		name  string
		mtime time.Time
	}{
		{"b.db", now.Add(-1 * time.Hour)},
		{"a.db", now.Add(-3 * time.Hour)},
		{"c.db", now.Add(-30 * time.Minute)},
	}
	for _, f := range files {
		path := filepath.Join(dir, f.name)
		if err := os.WriteFile(path, []byte("x"), 0644); err != nil {
			t.Fatalf("setup: %v", err)
		}
		if err := os.Chtimes(path, f.mtime, f.mtime); err != nil {
			t.Fatalf("chtimes: %v", err)
		}
	}

	backups, err := m.ListBackups()
	if err != nil {
		t.Fatalf("ListBackups: %v", err)
	}
	if len(backups) != 3 {
		t.Fatalf("want 3 backups, got %d", len(backups))
	}
	if backups[0].Name != "c.db" {
		t.Errorf("first: want c.db, got %s", backups[0].Name)
	}
	if backups[1].Name != "b.db" {
		t.Errorf("second: want b.db, got %s", backups[1].Name)
	}
	if backups[2].Name != "a.db" {
		t.Errorf("third: want a.db, got %s", backups[2].Name)
	}
}

func TestListBackups_IgnoresNonDBFilesAndDirs(t *testing.T) {
	d := openTestDB(t)
	dir := t.TempDir()
	m := newManager(t, d, dir, 0)

	os.WriteFile(filepath.Join(dir, "backup.db"), []byte("x"), 0644)
	os.WriteFile(filepath.Join(dir, "readme.txt"), []byte("x"), 0644)
	os.MkdirAll(filepath.Join(dir, "subdir.db"), 0755)

	backups, err := m.ListBackups()
	if err != nil {
		t.Fatalf("ListBackups: %v", err)
	}
	if len(backups) != 1 {
		t.Errorf("want 1 backup (.db files only), got %d", len(backups))
	}
	if backups[0].Name != "backup.db" {
		t.Errorf("want backup.db, got %s", backups[0].Name)
	}
}

// ---------------------------------------------------------------------------
// Rotation
// ---------------------------------------------------------------------------

func TestCreateBackup_RotatesOldestBeyondMax(t *testing.T) {
	d := openTestDB(t)
	dir := t.TempDir()
	m := newManager(t, d, dir, 2)

	// Pre-populate two old backup files with clearly old modtimes.
	old1 := filepath.Join(dir, "old1.db")
	old2 := filepath.Join(dir, "old2.db")
	for _, path := range []string{old1, old2} {
		if err := os.WriteFile(path, []byte("old"), 0644); err != nil {
			t.Fatalf("setup: %v", err)
		}
	}
	t1 := time.Now().Add(-2 * time.Hour)
	t2 := time.Now().Add(-1 * time.Hour)
	os.Chtimes(old1, t1, t1)
	os.Chtimes(old2, t2, t2)

	if _, err := m.CreateBackup(); err != nil {
		t.Fatalf("CreateBackup: %v", err)
	}

	// After rotation: newest backup + old2; old1 (oldest) should be gone.
	dbFiles := countDBFiles(t, dir)
	if dbFiles != 2 {
		t.Errorf("want 2 backups after rotate, got %d", dbFiles)
	}
	if _, err := os.Stat(old1); !os.IsNotExist(err) {
		t.Error("oldest backup (old1.db) should have been removed")
	}
}

func TestCreateBackup_NoRotateWhenUnlimited(t *testing.T) {
	d := openTestDB(t)
	dir := t.TempDir()
	m := newManager(t, d, dir, 0) // MaxBackups=0 means unlimited

	for i := 0; i < 10; i++ {
		path := filepath.Join(dir, fmt.Sprintf("old%d.db", i))
		os.WriteFile(path, []byte("old"), 0644)
		mt := time.Now().Add(time.Duration(-(i + 1)) * time.Hour)
		os.Chtimes(path, mt, mt)
	}

	if _, err := m.CreateBackup(); err != nil {
		t.Fatalf("CreateBackup: %v", err)
	}

	dbFiles := countDBFiles(t, dir)
	if dbFiles != 11 {
		t.Errorf("want 11 backups (no rotation), got %d", dbFiles)
	}
}

func countDBFiles(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".db" {
			n++
		}
	}
	return n
}

// ---------------------------------------------------------------------------
// Start / Stop
// ---------------------------------------------------------------------------

func TestStop_SafeWhenNeverStarted(t *testing.T) {
	d := openTestDB(t)
	cfg := &config.Config{
		Backup: config.BackupConfig{IntervalMinutes: 0},
	}
	m := backup.New(d, cfg)
	m.Stop() // must not panic
	m.Stop() // must not panic
}

func TestStop_SafeToCallMultipleTimes(t *testing.T) {
	d := openTestDB(t)
	cfg := &config.Config{
		Storage: config.StorageConfig{DataDir: t.TempDir()},
		Backup:  config.BackupConfig{IntervalMinutes: 60}, // long enough it won't fire
	}
	m := backup.New(d, cfg)
	m.Start()
	m.Stop()
	m.Stop() // must not panic
}

func TestStart_NoopWhenIntervalIsZero(t *testing.T) {
	d := openTestDB(t)
	dir := t.TempDir()
	cfg := &config.Config{
		Storage: config.StorageConfig{DataDir: t.TempDir()},
		Backup:  config.BackupConfig{Dir: dir, IntervalMinutes: 0},
	}
	m := backup.New(d, cfg)
	m.Start() // should be a no-op
	m.Stop()  // must not panic

	if countDBFiles(t, dir) != 0 {
		t.Error("no backup should be created when IntervalMinutes is 0")
	}
}
