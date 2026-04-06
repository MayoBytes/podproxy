package db

import "fmt"

// Backup creates a consistent, defragmented copy of the database at destPath
// using SQLite's VACUUM INTO. Safe to call while the database is in use.
// destPath must not already exist; VACUUM INTO fails on existing files.
func (d *DB) Backup(destPath string) error {
	if _, err := d.DB.Exec("VACUUM INTO ?", destPath); err != nil {
		return fmt.Errorf("vacuum into: %w", err)
	}
	return nil
}
