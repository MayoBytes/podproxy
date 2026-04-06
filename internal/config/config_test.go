package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"podproxy/internal/config"
)

func TestLoad_MissingFile_ReturnsDefaults(t *testing.T) {
	cfg, err := config.Load(filepath.Join(t.TempDir(), "notexist.yaml"))
	if err != nil {
		t.Fatalf("unexpected error for missing file: %v", err)
	}
	if cfg.Server.Port != 8080 {
		t.Errorf("port: want 8080, got %d", cfg.Server.Port)
	}
	if cfg.Server.BaseURL != "http://localhost:8080" {
		t.Errorf("base_url: want http://localhost:8080, got %s", cfg.Server.BaseURL)
	}
	if cfg.Storage.CacheDir != "./cache" {
		t.Errorf("cache_dir: want ./cache, got %s", cfg.Storage.CacheDir)
	}
	if cfg.Storage.DataDir != "./data" {
		t.Errorf("data_dir: want ./data, got %s", cfg.Storage.DataDir)
	}
	if cfg.Defaults.RefreshIntervalMinutes != 60 {
		t.Errorf("refresh_interval_minutes: want 60, got %d", cfg.Defaults.RefreshIntervalMinutes)
	}
	if cfg.Defaults.PrefetchConcurrency != 2 {
		t.Errorf("prefetch_concurrency: want 2, got %d", cfg.Defaults.PrefetchConcurrency)
	}
}

func TestLoad_ValidYAML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	content := `
server:
  port: 9090
  base_url: "http://myhost:9090"
storage:
  cache_dir: "/tmp/cache"
  data_dir: "/tmp/data"
defaults:
  refresh_interval_minutes: 30
  prefetch_concurrency: 4
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Server.Port != 9090 {
		t.Errorf("port: want 9090, got %d", cfg.Server.Port)
	}
	if cfg.Server.BaseURL != "http://myhost:9090" {
		t.Errorf("base_url: want http://myhost:9090, got %s", cfg.Server.BaseURL)
	}
	if cfg.Storage.CacheDir != "/tmp/cache" {
		t.Errorf("cache_dir: want /tmp/cache, got %s", cfg.Storage.CacheDir)
	}
	if cfg.Defaults.RefreshIntervalMinutes != 30 {
		t.Errorf("refresh_interval_minutes: want 30, got %d", cfg.Defaults.RefreshIntervalMinutes)
	}
	if cfg.Defaults.PrefetchConcurrency != 4 {
		t.Errorf("prefetch_concurrency: want 4, got %d", cfg.Defaults.PrefetchConcurrency)
	}
}

func TestLoad_ZeroPort_FallsBackToDefault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	os.WriteFile(path, []byte("server:\n  port: 0\n"), 0644)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Server.Port != 8080 {
		t.Errorf("port: want fallback 8080, got %d", cfg.Server.Port)
	}
}

func TestLoad_NoBaseURL_DerivedFromPort(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	os.WriteFile(path, []byte("server:\n  port: 7777\n"), 0644)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "http://localhost:7777"
	if cfg.Server.BaseURL != want {
		t.Errorf("base_url: want %s, got %s", want, cfg.Server.BaseURL)
	}
}

func TestLoad_InvalidYAML_ReturnsError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	os.WriteFile(path, []byte(":\tinvalid: yaml: {{{\n"), 0644)
	_, err := config.Load(path)
	if err == nil {
		t.Error("expected error for invalid YAML, got nil")
	}
}

func TestLoad_EnvOverrides(t *testing.T) {
	t.Setenv("PODPROXY_REFRESH_INTERVAL_MINUTES", "15")
	t.Setenv("PODPROXY_AUTO_PREFETCH", "true")
	t.Setenv("PODPROXY_PREFETCH_MAX_AGE_DAYS", "7")
	t.Setenv("PODPROXY_PREFETCH_CONCURRENCY", "4")
	t.Setenv("PODPROXY_MAX_BACKUPS", "10")
	t.Setenv("PODPROXY_BACKUP_INTERVAL_MINUTES", "60")

	cfg, err := config.Load(filepath.Join(t.TempDir(), "notexist.yaml"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Defaults.RefreshIntervalMinutes != 15 {
		t.Errorf("refresh_interval_minutes: want 15, got %d", cfg.Defaults.RefreshIntervalMinutes)
	}
	if !cfg.Defaults.AutoPrefetch {
		t.Error("auto_prefetch: want true, got false")
	}
	if cfg.Defaults.PrefetchMaxAgeDays != 7 {
		t.Errorf("prefetch_max_age_days: want 7, got %d", cfg.Defaults.PrefetchMaxAgeDays)
	}
	if cfg.Defaults.PrefetchConcurrency != 4 {
		t.Errorf("prefetch_concurrency: want 4, got %d", cfg.Defaults.PrefetchConcurrency)
	}
	if cfg.Backup.MaxBackups != 10 {
		t.Errorf("max_backups: want 10, got %d", cfg.Backup.MaxBackups)
	}
	if cfg.Backup.IntervalMinutes != 60 {
		t.Errorf("interval_minutes: want 60, got %d", cfg.Backup.IntervalMinutes)
	}
}

func TestAddr(t *testing.T) {
	cfg := &config.Config{Server: config.ServerConfig{Port: 9000}}
	if got := cfg.Addr(); got != ":9000" {
		t.Errorf("Addr: want :9000, got %s", got)
	}
}
