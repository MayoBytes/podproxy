package config

import (
	"fmt"
	"os"
	"strconv"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server   ServerConfig   `yaml:"server"`
	Storage  StorageConfig  `yaml:"storage"`
	Defaults DefaultsConfig `yaml:"defaults"`
	Backup   BackupConfig   `yaml:"backup"`
}

type ServerConfig struct {
	Port    int    `yaml:"port"`
	BaseURL string `yaml:"base_url"`
}

type StorageConfig struct {
	CacheDir string `yaml:"cache_dir"`
	DataDir  string `yaml:"data_dir"`
}

type DefaultsConfig struct {
	RefreshIntervalMinutes int  `yaml:"refresh_interval_minutes"`
	AutoPrefetch           bool `yaml:"auto_prefetch"`
	PrefetchMaxAgeDays     int  `yaml:"prefetch_max_age_days"`
	PrefetchConcurrency    int  `yaml:"prefetch_concurrency"`
}

type BackupConfig struct {
	Dir             string `yaml:"dir"`              // defaults to {data_dir}/backups
	MaxBackups      int    `yaml:"max_backups"`       // 0 = unlimited
	IntervalMinutes int    `yaml:"interval_minutes"`  // 0 = disabled
}

const defaultPort = 8080

func Load(path string) (*Config, error) {
	cfg := &Config{
		Server: ServerConfig{
			Port: defaultPort,
		},
		Storage: StorageConfig{
			CacheDir: "./cache",
			DataDir:  "./data",
		},
		Defaults: DefaultsConfig{
			RefreshIntervalMinutes: 60,
			AutoPrefetch:           false,
			PrefetchMaxAgeDays:     30,
			PrefetchConcurrency:    2,
		},
		Backup: BackupConfig{
			MaxBackups: 5,
		},
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			applyEnvOverrides(cfg)
			applyDerivedDefaults(cfg)
			return cfg, nil
		}
		return nil, fmt.Errorf("read config: %w", err)
	}

	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	if cfg.Server.Port == 0 {
		cfg.Server.Port = defaultPort
	}
	applyEnvOverrides(cfg)
	applyDerivedDefaults(cfg)

	return cfg, nil
}

// applyEnvOverrides overrides config fields from environment variables if set.
//
// Supported variables:
//
//	PODPROXY_PORT                     – server.port
//	PODPROXY_BASE_URL                 – server.base_url
//	PODPROXY_REFRESH_INTERVAL_MINUTES – defaults.refresh_interval_minutes
//	PODPROXY_AUTO_PREFETCH            – defaults.auto_prefetch (1/true/yes)
//	PODPROXY_PREFETCH_MAX_AGE_DAYS    – defaults.prefetch_max_age_days
//	PODPROXY_PREFETCH_CONCURRENCY     – defaults.prefetch_concurrency
//	PODPROXY_MAX_BACKUPS              – backup.max_backups
//	PODPROXY_BACKUP_INTERVAL_MINUTES  – backup.interval_minutes
func applyEnvOverrides(cfg *Config) {
	if v := os.Getenv("PODPROXY_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil {
			cfg.Server.Port = p
		}
	}
	if v := os.Getenv("PODPROXY_BASE_URL"); v != "" {
		cfg.Server.BaseURL = v
	}
	if v := os.Getenv("PODPROXY_REFRESH_INTERVAL_MINUTES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Defaults.RefreshIntervalMinutes = n
		}
	}
	if v := os.Getenv("PODPROXY_AUTO_PREFETCH"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			cfg.Defaults.AutoPrefetch = b
		}
	}
	if v := os.Getenv("PODPROXY_PREFETCH_MAX_AGE_DAYS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Defaults.PrefetchMaxAgeDays = n
		}
	}
	if v := os.Getenv("PODPROXY_PREFETCH_CONCURRENCY"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Defaults.PrefetchConcurrency = n
		}
	}
	if v := os.Getenv("PODPROXY_MAX_BACKUPS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Backup.MaxBackups = n
		}
	}
	if v := os.Getenv("PODPROXY_BACKUP_INTERVAL_MINUTES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Backup.IntervalMinutes = n
		}
	}
}

// applyDerivedDefaults fills in values that depend on other config fields.
func applyDerivedDefaults(cfg *Config) {
	if cfg.Server.BaseURL == "" {
		cfg.Server.BaseURL = fmt.Sprintf("http://localhost:%d", cfg.Server.Port)
	}
}

func (c *Config) Addr() string {
	return fmt.Sprintf(":%d", c.Server.Port)
}
