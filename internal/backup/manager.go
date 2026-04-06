package backup

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"podproxy/internal/config"
	"podproxy/internal/db"
)

// BackupInfo describes a single database backup file.
type BackupInfo struct {
	Name      string    `json:"name"`
	SizeBytes int64     `json:"size_bytes"`
	CreatedAt time.Time `json:"created_at"`
}

// Manager handles on-demand and scheduled database backups.
type Manager struct {
	database *db.DB
	cfg      *config.Config
	dir      string
	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once
}

// New creates a Manager. dir defaults to {data_dir}/backups when not set in config.
func New(database *db.DB, cfg *config.Config) *Manager {
	dir := cfg.Backup.Dir
	if dir == "" {
		dir = filepath.Join(cfg.Storage.DataDir, "backups")
	}
	return &Manager{
		database: database,
		cfg:      cfg,
		dir:      dir,
	}
}

// CreateBackup writes a new backup file and rotates old ones.
func (m *Manager) CreateBackup() (*BackupInfo, error) {
	if err := os.MkdirAll(m.dir, 0755); err != nil {
		return nil, fmt.Errorf("create backup dir: %w", err)
	}

	now := time.Now().UTC()
	name := fmt.Sprintf("podproxy-%s.db", now.Format("2006-01-02T15-04-05Z"))
	destPath := filepath.Join(m.dir, name)

	if _, err := os.Stat(destPath); err == nil {
		return nil, fmt.Errorf("backup already exists: %s", name)
	}

	if err := m.database.Backup(destPath); err != nil {
		return nil, err
	}

	fi, err := os.Stat(destPath)
	if err != nil {
		return nil, fmt.Errorf("stat backup: %w", err)
	}

	info := &BackupInfo{
		Name:      name,
		SizeBytes: fi.Size(),
		CreatedAt: now,
	}

	if err := m.rotate(); err != nil {
		log.Printf("backup: rotate: %v", err)
	}

	return info, nil
}

// ListBackups returns all backup files sorted newest-first.
func (m *Manager) ListBackups() ([]BackupInfo, error) {
	entries, err := os.ReadDir(m.dir)
	if os.IsNotExist(err) {
		return []BackupInfo{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read backup dir: %w", err)
	}

	backups := []BackupInfo{}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".db" {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		backups = append(backups, BackupInfo{
			Name:      e.Name(),
			SizeBytes: fi.Size(),
			CreatedAt: fi.ModTime().UTC(),
		})
	}

	sort.Slice(backups, func(i, j int) bool {
		return backups[i].CreatedAt.After(backups[j].CreatedAt)
	})

	return backups, nil
}

// rotate removes the oldest backups beyond the configured maximum.
func (m *Manager) rotate() error {
	max := m.cfg.Backup.MaxBackups
	if max <= 0 {
		return nil
	}

	backups, err := m.ListBackups()
	if err != nil {
		return err
	}

	if len(backups) <= max {
		return nil
	}

	for _, b := range backups[max:] {
		path := filepath.Join(m.dir, b.Name)
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			log.Printf("backup: remove %s: %v", path, err)
		}
	}
	return nil
}

// Start begins the scheduled backup goroutine if interval_minutes > 0.
func (m *Manager) Start() {
	if m.cfg.Backup.IntervalMinutes <= 0 {
		return
	}
	m.stop = make(chan struct{})
	m.done = make(chan struct{})
	go m.run()
	keeping := fmt.Sprintf("%d", m.cfg.Backup.MaxBackups)
	if m.cfg.Backup.MaxBackups <= 0 {
		keeping = "unlimited"
	}
	log.Printf("backup: scheduled every %d minutes, keeping %s copies in %s",
		m.cfg.Backup.IntervalMinutes, keeping, m.dir)
}

func (m *Manager) run() {
	defer close(m.done)
	interval := time.Duration(m.cfg.Backup.IntervalMinutes) * time.Minute
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			info, err := m.CreateBackup()
			if err != nil {
				log.Printf("backup: scheduled backup failed: %v", err)
			} else {
				log.Printf("backup: created %s (%d bytes)", info.Name, info.SizeBytes)
			}
		case <-m.stop:
			return
		}
	}
}

// Stop halts the scheduled backup goroutine and waits for it to exit.
// Safe to call multiple times or when Start was never called.
func (m *Manager) Stop() {
	if m.stop == nil {
		return
	}
	m.stopOnce.Do(func() { close(m.stop) })
	<-m.done
}
