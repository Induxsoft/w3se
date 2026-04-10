package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Config is the top-level configuration loaded from w3se.config.
type Config struct {
	Server   ServerConfig   `json:"server"`
	Logging  LoggingConfig  `json:"logging"`
	Channels []ChannelConfig `json:"channels"`
}

type ServerConfig struct {
	Host                string   `json:"host"`
	Port                int      `json:"port"`
	TLS                 TLSConfig `json:"tls"`
	TrustedProxies      []string `json:"trusted_proxies"`
	ReadTimeoutMs       int      `json:"read_timeout_ms"`
	WriteTimeoutMs      int      `json:"write_timeout_ms"`
	MaxConnections      int      `json:"max_connections"`
	MaxMessageSizeBytes int      `json:"max_message_size_bytes"`
	ShutdownWaitMs      int      `json:"shutdown_wait_ms"`
}

type TLSConfig struct {
	Enabled bool   `json:"enabled"`
	Cert    string `json:"cert"`
	Key     string `json:"key"`
}

type LoggingConfig struct {
	Level  string `json:"level"`
	Format string `json:"format"`
	Output string `json:"output"`
}

type ChannelConfig struct {
	Name        string            `json:"name"`
	Paths       ChannelPaths      `json:"paths"`
	Auth        ChannelAuth       `json:"auth"`
	Webhooks    ChannelWebhooks   `json:"webhooks"`
	Connections ChannelConnections `json:"connections"`
}

type ChannelPaths struct {
	SSE string `json:"sse"`
	WS  string `json:"ws"`
}

type ChannelAuth struct {
	TokenHeader string        `json:"token_header"`
	Tokens      []string      `json:"tokens"`
	RateLimit   RateLimitConfig `json:"rate_limit"`
}

type RateLimitConfig struct {
	RequestsPerMinute int    `json:"requests_per_minute"`
	Per               string `json:"per"`
}

type ChannelWebhooks struct {
	URL       string            `json:"url"`
	Method    string            `json:"method"`
	Headers   map[string]string `json:"headers"`
	TimeoutMs int               `json:"timeout_ms"`
}

type ChannelConnections struct {
	IdleTimeoutMs      int `json:"idle_timeout_ms"`
	HeartbeatIntervalMs int `json:"heartbeat_interval_ms"`
}

// DefaultConfig returns a Config with all default values applied.
func DefaultConfig() Config {
	return Config{
		Server: ServerConfig{
			Host:                "0.0.0.0",
			Port:                8080,
			ReadTimeoutMs:       5000,
			WriteTimeoutMs:      10000,
			MaxConnections:      10000,
			MaxMessageSizeBytes: 1048576,
			ShutdownWaitMs:      500,
		},
		Logging: LoggingConfig{
			Level:  "info",
			Format: "json",
			Output: "stdout",
		},
		Channels: []ChannelConfig{},
	}
}

// DefaultChannelConfig returns default values for channel-level settings.
func DefaultChannelConfig() ChannelConfig {
	return ChannelConfig{
		Webhooks: ChannelWebhooks{
			Method:    "POST",
			Headers:   map[string]string{},
			TimeoutMs: 5000,
		},
		Connections: ChannelConnections{
			IdleTimeoutMs:       30000,
			HeartbeatIntervalMs: 15000,
		},
	}
}

// LoadConfig loads w3se.config and all *.channel files from the executable's directory.
func LoadConfig() (Config, error) {
	execPath, err := os.Executable()
	if err != nil {
		return Config{}, fmt.Errorf("cannot determine executable path: %w", err)
	}
	dir := filepath.Dir(execPath)

	// Also check current working directory as fallback
	cwd, _ := os.Getwd()

	cfg := DefaultConfig()

	// Try to load w3se.config from exec dir first, then cwd
	configPath := filepath.Join(dir, "w3se.config")
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		configPath = filepath.Join(cwd, "w3se.config")
	}

	if data, err := os.ReadFile(configPath); err == nil {
		if err := json.Unmarshal(data, &cfg); err != nil {
			return Config{}, fmt.Errorf("error parsing w3se.config: %w", err)
		}
		applyServerDefaults(&cfg)
	} else if !os.IsNotExist(err) {
		return Config{}, fmt.Errorf("error reading w3se.config: %w", err)
	}

	// Apply defaults to inline channels
	for i := range cfg.Channels {
		applyChannelDefaults(&cfg.Channels[i])
	}

	// Load *.channel files from both directories
	channelFiles := findChannelFiles(dir)
	if dir != cwd {
		channelFiles = append(channelFiles, findChannelFiles(cwd)...)
	}

	// Deduplicate by filename
	seen := map[string]bool{}
	unique := []string{}
	for _, f := range channelFiles {
		base := filepath.Base(f)
		if !seen[base] {
			seen[base] = true
			unique = append(unique, f)
		}
	}

	for _, path := range unique {
		data, err := os.ReadFile(path)
		if err != nil {
			return Config{}, fmt.Errorf("error reading %s: %w", path, err)
		}

		var ch ChannelConfig
		if err := json.Unmarshal(data, &ch); err != nil {
			return Config{}, fmt.Errorf("error parsing %s: %w", path, err)
		}

		if ch.Name == "" {
			return Config{}, fmt.Errorf("channel in %s has no name", path)
		}

		applyChannelDefaults(&ch)

		// .channel files override inline channels with the same name
		replaced := false
		for i, existing := range cfg.Channels {
			if existing.Name == ch.Name {
				cfg.Channels[i] = ch
				replaced = true
				break
			}
		}
		if !replaced {
			cfg.Channels = append(cfg.Channels, ch)
		}
	}

	// Validate channels
	if err := validateChannels(cfg.Channels); err != nil {
		return Config{}, err
	}

	return cfg, nil
}

func findChannelFiles(dir string) []string {
	pattern := filepath.Join(dir, "*.channel")
	matches, _ := filepath.Glob(pattern)
	return matches
}

func applyServerDefaults(cfg *Config) {
	d := DefaultConfig()
	if cfg.Server.Host == "" {
		cfg.Server.Host = d.Server.Host
	}
	if cfg.Server.Port == 0 {
		cfg.Server.Port = d.Server.Port
	}
	if cfg.Server.ReadTimeoutMs == 0 {
		cfg.Server.ReadTimeoutMs = d.Server.ReadTimeoutMs
	}
	if cfg.Server.WriteTimeoutMs == 0 {
		cfg.Server.WriteTimeoutMs = d.Server.WriteTimeoutMs
	}
	if cfg.Server.MaxConnections == 0 {
		cfg.Server.MaxConnections = d.Server.MaxConnections
	}
	if cfg.Server.MaxMessageSizeBytes == 0 {
		cfg.Server.MaxMessageSizeBytes = d.Server.MaxMessageSizeBytes
	}
	if cfg.Server.ShutdownWaitMs == 0 {
		cfg.Server.ShutdownWaitMs = d.Server.ShutdownWaitMs
	}
	if cfg.Logging.Level == "" {
		cfg.Logging.Level = d.Logging.Level
	}
	if cfg.Logging.Format == "" {
		cfg.Logging.Format = d.Logging.Format
	}
	if cfg.Logging.Output == "" {
		cfg.Logging.Output = d.Logging.Output
	}
}

func applyChannelDefaults(ch *ChannelConfig) {
	d := DefaultChannelConfig()
	if ch.Webhooks.Method == "" {
		ch.Webhooks.Method = d.Webhooks.Method
	}
	if ch.Webhooks.Headers == nil {
		ch.Webhooks.Headers = d.Webhooks.Headers
	}
	if ch.Webhooks.TimeoutMs == 0 {
		ch.Webhooks.TimeoutMs = d.Webhooks.TimeoutMs
	}
	if ch.Connections.IdleTimeoutMs == 0 {
		ch.Connections.IdleTimeoutMs = d.Connections.IdleTimeoutMs
	}
	if ch.Connections.HeartbeatIntervalMs == 0 {
		ch.Connections.HeartbeatIntervalMs = d.Connections.HeartbeatIntervalMs
	}
}

func validateChannels(channels []ChannelConfig) error {
	names := map[string]bool{}
	paths := map[string]string{}

	for _, ch := range channels {
		if ch.Name == "" {
			return fmt.Errorf("channel has empty name")
		}
		if names[ch.Name] {
			return fmt.Errorf("duplicate channel name: %s", ch.Name)
		}
		names[ch.Name] = true

		if ch.Paths.SSE == "" || ch.Paths.WS == "" {
			return fmt.Errorf("channel %s: paths.sse and paths.ws are required", ch.Name)
		}
		if ch.Webhooks.URL == "" {
			return fmt.Errorf("channel %s: webhooks.url is required", ch.Name)
		}

		// Check for path conflicts
		for _, p := range []string{ch.Paths.SSE, ch.Paths.WS} {
			normalized := strings.TrimSuffix(p, "/")
			if owner, exists := paths[normalized]; exists {
				return fmt.Errorf("path %s used by both channels %s and %s", p, owner, ch.Name)
			}
			paths[normalized] = ch.Name
		}
	}

	return nil
}
