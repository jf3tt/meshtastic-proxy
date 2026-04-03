package config

import (
	"os"
	"testing"
	"time"
)

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()

	if cfg.Node.Address != "localhost:4403" {
		t.Fatalf("unexpected default node address: %s", cfg.Node.Address)
	}
	if cfg.Proxy.Listen != ":4404" {
		t.Fatalf("unexpected default proxy listen: %s", cfg.Proxy.Listen)
	}
	if cfg.Proxy.MaxClients != 10 {
		t.Fatalf("unexpected default max clients: %d", cfg.Proxy.MaxClients)
	}
	if cfg.Web.Listen != ":8080" {
		t.Fatalf("unexpected default web listen: %s", cfg.Web.Listen)
	}
	if !cfg.Web.Enabled {
		t.Fatal("expected web enabled by default")
	}
	if !cfg.MDNS.Enabled {
		t.Fatal("expected mdns enabled by default")
	}
	if cfg.MDNS.Instance != "Meshtastic Proxy" {
		t.Fatalf("unexpected default mdns instance: %s", cfg.MDNS.Instance)
	}
	if cfg.MDNS.ShortName != "PRXY" {
		t.Fatalf("unexpected default mdns short_name: %s", cfg.MDNS.ShortName)
	}
	if cfg.Node.ReconnectInterval.Duration != time.Second {
		t.Fatalf("unexpected default reconnect interval: %v", cfg.Node.ReconnectInterval.Duration)
	}

	if err := cfg.Validate(); err != nil {
		t.Fatalf("default config should be valid: %v", err)
	}
}

func TestLoadConfig(t *testing.T) {
	content := `
[node]
address = "10.0.0.1:4403"
reconnect_interval = "2s"
max_reconnect_interval = "60s"

[proxy]
listen = ":5555"
max_clients = 20

[web]
listen = ":9090"
enabled = false

[mdns]
enabled = true
instance = "My Node"
short_name = "TEST"
id = "!aabbccdd"

[logging]
level = "debug"
format = "json"
`
	tmpFile, err := os.CreateTemp("", "config-*.toml")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Remove(tmpFile.Name()) }()

	if _, err := tmpFile.WriteString(content); err != nil {
		t.Fatal(err)
	}
	if err := tmpFile.Close(); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(tmpFile.Name())
	if err != nil {
		t.Fatalf("failed to load config: %v", err)
	}

	if cfg.Node.Address != "10.0.0.1:4403" {
		t.Fatalf("unexpected node address: %s", cfg.Node.Address)
	}
	if cfg.Node.ReconnectInterval.Duration != 2*time.Second {
		t.Fatalf("unexpected reconnect interval: %v", cfg.Node.ReconnectInterval.Duration)
	}
	if cfg.Node.MaxReconnectInterval.Duration != 60*time.Second {
		t.Fatalf("unexpected max reconnect interval: %v", cfg.Node.MaxReconnectInterval.Duration)
	}
	if cfg.Proxy.Listen != ":5555" {
		t.Fatalf("unexpected proxy listen: %s", cfg.Proxy.Listen)
	}
	if cfg.Proxy.MaxClients != 20 {
		t.Fatalf("unexpected max clients: %d", cfg.Proxy.MaxClients)
	}
	if cfg.Web.Enabled {
		t.Fatal("expected web disabled")
	}
	if !cfg.MDNS.Enabled {
		t.Fatal("expected mdns enabled")
	}
	if cfg.MDNS.Instance != "My Node" {
		t.Fatalf("unexpected mdns instance: %s", cfg.MDNS.Instance)
	}
	if cfg.MDNS.ShortName != "TEST" {
		t.Fatalf("unexpected mdns short_name: %s", cfg.MDNS.ShortName)
	}
	if cfg.MDNS.ID != "!aabbccdd" {
		t.Fatalf("unexpected mdns id: %s", cfg.MDNS.ID)
	}
	if cfg.Logging.Level != "debug" {
		t.Fatalf("unexpected log level: %s", cfg.Logging.Level)
	}
	if cfg.Logging.Format != "json" {
		t.Fatalf("unexpected log format: %s", cfg.Logging.Format)
	}
}

func TestValidation(t *testing.T) {
	tests := []struct {
		name    string
		modify  func(*Config)
		wantErr bool
	}{
		{
			name:    "valid default",
			modify:  func(c *Config) {},
			wantErr: false,
		},
		{
			name:    "empty node address",
			modify:  func(c *Config) { c.Node.Address = "" },
			wantErr: true,
		},
		{
			name:    "empty proxy listen",
			modify:  func(c *Config) { c.Proxy.Listen = "" },
			wantErr: true,
		},
		{
			name:    "zero max clients",
			modify:  func(c *Config) { c.Proxy.MaxClients = 0 },
			wantErr: true,
		},
		{
			name:    "reconnect interval too small",
			modify:  func(c *Config) { c.Node.ReconnectInterval = Duration{10 * time.Millisecond} },
			wantErr: true,
		},
		{
			name: "max reconnect < reconnect",
			modify: func(c *Config) {
				c.Node.ReconnectInterval = Duration{5 * time.Second}
				c.Node.MaxReconnectInterval = Duration{1 * time.Second}
			},
			wantErr: true,
		},
		{
			name:    "mdns empty instance",
			modify:  func(c *Config) { c.MDNS.Instance = "" },
			wantErr: true,
		},
		{
			name:    "mdns short_name too long",
			modify:  func(c *Config) { c.MDNS.ShortName = "TOOLONG" },
			wantErr: true,
		},
		{
			name: "mdns disabled skips validation",
			modify: func(c *Config) {
				c.MDNS.Enabled = false
				c.MDNS.Instance = "" // would fail if enabled
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := DefaultConfig()
			tt.modify(cfg)
			err := cfg.Validate()
			if tt.wantErr && err == nil {
				t.Fatal("expected error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestDurationMarshalUnmarshal(t *testing.T) {
	d := Duration{5 * time.Second}

	text, err := d.MarshalText()
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}

	var d2 Duration
	if err := d2.UnmarshalText(text); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	if d.Duration != d2.Duration {
		t.Fatalf("round-trip failed: %v != %v", d.Duration, d2.Duration)
	}
}

func TestLoadNonExistentFile(t *testing.T) {
	_, err := Load("/nonexistent/path/config.toml")
	if err == nil {
		t.Fatal("expected error for non-existent file")
	}
}
