package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server   ServerConfig   `yaml:"server"`
	Storage  StorageConfig  `yaml:"storage"`
	Defaults DefaultsConfig `yaml:"defaults"`
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
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
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
	applyDerivedDefaults(cfg)

	return cfg, nil
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
