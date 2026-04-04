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
	if cfg.Node.DialTimeout.Duration != 10*time.Second {
		t.Fatalf("unexpected default dial timeout: %v", cfg.Node.DialTimeout.Duration)
	}
	if cfg.Node.ReadTimeout.Duration != 5*time.Minute {
		t.Fatalf("unexpected default read timeout: %v", cfg.Node.ReadTimeout.Duration)
	}
	if cfg.Node.FromBuffer != 256 {
		t.Fatalf("unexpected default from buffer: %d", cfg.Node.FromBuffer)
	}
	if cfg.Node.ToBuffer != 64 {
		t.Fatalf("unexpected default to buffer: %d", cfg.Node.ToBuffer)
	}
	if cfg.Proxy.Listen != ":4404" {
		t.Fatalf("unexpected default proxy listen: %s", cfg.Proxy.Listen)
	}
	if cfg.Proxy.MaxClients != 10 {
		t.Fatalf("unexpected default max clients: %d", cfg.Proxy.MaxClients)
	}
	if cfg.Proxy.ClientSendBuffer != 256 {
		t.Fatalf("unexpected default client send buffer: %d", cfg.Proxy.ClientSendBuffer)
	}
	if cfg.Proxy.ClientIdleTimeout.Duration != 30*time.Minute {
		t.Fatalf("unexpected default client idle timeout: %v", cfg.Proxy.ClientIdleTimeout.Duration)
	}
	if cfg.Proxy.IOSNodeInfoDelay.Duration != 50*time.Millisecond {
		t.Fatalf("unexpected default iOS nodeinfo delay: %v", cfg.Proxy.IOSNodeInfoDelay.Duration)
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
	if cfg.MDNS.Hostname != "" {
		t.Fatalf("expected empty default mdns hostname, got: %s", cfg.MDNS.Hostname)
	}
	if cfg.MDNS.ShortName != "PRXY" {
		t.Fatalf("unexpected default mdns short_name: %s", cfg.MDNS.ShortName)
	}
	if len(cfg.MDNS.Interfaces) != 0 {
		t.Fatalf("expected empty default mdns interfaces, got: %v", cfg.MDNS.Interfaces)
	}
	if cfg.Node.ReconnectInterval.Duration != time.Second {
		t.Fatalf("unexpected default reconnect interval: %v", cfg.Node.ReconnectInterval.Duration)
	}
	if cfg.Metrics.MaxMessages != 200 {
		t.Fatalf("unexpected default max messages: %d", cfg.Metrics.MaxMessages)
	}
	if cfg.Metrics.MaxTrafficSamples != 300 {
		t.Fatalf("unexpected default max traffic samples: %d", cfg.Metrics.MaxTrafficSamples)
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
hostname = "my-proxy"
short_name = "TEST"
id = "!aabbccdd"
interfaces = ["eth0", "wlan0"]

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
	if cfg.MDNS.Hostname != "my-proxy" {
		t.Fatalf("unexpected mdns hostname: %s", cfg.MDNS.Hostname)
	}
	if cfg.MDNS.ShortName != "TEST" {
		t.Fatalf("unexpected mdns short_name: %s", cfg.MDNS.ShortName)
	}
	if cfg.MDNS.ID != "!aabbccdd" {
		t.Fatalf("unexpected mdns id: %s", cfg.MDNS.ID)
	}
	if len(cfg.MDNS.Interfaces) != 2 || cfg.MDNS.Interfaces[0] != "eth0" || cfg.MDNS.Interfaces[1] != "wlan0" {
		t.Fatalf("unexpected mdns interfaces: %v", cfg.MDNS.Interfaces)
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
		{
			name: "mdns empty interface name",
			modify: func(c *Config) {
				c.MDNS.Interfaces = []string{"eth0", ""}
			},
			wantErr: true,
		},
		{
			name: "mdns valid interfaces",
			modify: func(c *Config) {
				c.MDNS.Interfaces = []string{"eth0", "wlan0"}
			},
			wantErr: false,
		},
		{
			name: "mdns empty interfaces list",
			modify: func(c *Config) {
				c.MDNS.Interfaces = []string{}
			},
			wantErr: false,
		},
		{
			name:    "zero client send buffer",
			modify:  func(c *Config) { c.Proxy.ClientSendBuffer = 0 },
			wantErr: true,
		},
		{
			name:    "zero from buffer",
			modify:  func(c *Config) { c.Node.FromBuffer = 0 },
			wantErr: true,
		},
		{
			name:    "zero to buffer",
			modify:  func(c *Config) { c.Node.ToBuffer = 0 },
			wantErr: true,
		},
		{
			name:    "zero metrics max messages",
			modify:  func(c *Config) { c.Metrics.MaxMessages = 0 },
			wantErr: true,
		},
		{
			name:    "zero metrics max traffic samples",
			modify:  func(c *Config) { c.Metrics.MaxTrafficSamples = 0 },
			wantErr: true,
		},
		{
			name: "web listen empty when enabled",
			modify: func(c *Config) {
				c.Web.Enabled = true
				c.Web.Listen = ""
			},
			wantErr: true,
		},
		{
			name: "web listen empty when disabled",
			modify: func(c *Config) {
				c.Web.Enabled = false
				c.Web.Listen = ""
			},
			wantErr: false,
		},
		{
			name:    "invalid logging level",
			modify:  func(c *Config) { c.Logging.Level = "trace" },
			wantErr: true,
		},
		{
			name:    "invalid logging format",
			modify:  func(c *Config) { c.Logging.Format = "yaml" },
			wantErr: true,
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

func TestLoadConfigNewFields(t *testing.T) {
	content := `
[node]
address = "10.0.0.1:4403"
reconnect_interval = "2s"
max_reconnect_interval = "60s"
dial_timeout = "5s"
read_timeout = "3m"
from_buffer = 128
to_buffer = 32

[proxy]
listen = ":5555"
max_clients = 20
client_send_buffer = 512
client_idle_timeout = "15m"
ios_nodeinfo_delay = "100ms"

[web]
listen = ":9090"
enabled = false

[mdns]
enabled = false

[logging]
level = "debug"
format = "json"

[metrics]
max_messages = 500
max_traffic_samples = 600
`
	tmpFile, err := os.CreateTemp("", "config-new-*.toml")
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

	if cfg.Node.DialTimeout.Duration != 5*time.Second {
		t.Fatalf("unexpected dial timeout: %v", cfg.Node.DialTimeout.Duration)
	}
	if cfg.Node.ReadTimeout.Duration != 3*time.Minute {
		t.Fatalf("unexpected read timeout: %v", cfg.Node.ReadTimeout.Duration)
	}
	if cfg.Node.FromBuffer != 128 {
		t.Fatalf("unexpected from buffer: %d", cfg.Node.FromBuffer)
	}
	if cfg.Node.ToBuffer != 32 {
		t.Fatalf("unexpected to buffer: %d", cfg.Node.ToBuffer)
	}
	if cfg.Proxy.ClientSendBuffer != 512 {
		t.Fatalf("unexpected client send buffer: %d", cfg.Proxy.ClientSendBuffer)
	}
	if cfg.Proxy.ClientIdleTimeout.Duration != 15*time.Minute {
		t.Fatalf("unexpected client idle timeout: %v", cfg.Proxy.ClientIdleTimeout.Duration)
	}
	if cfg.Proxy.IOSNodeInfoDelay.Duration != 100*time.Millisecond {
		t.Fatalf("unexpected iOS nodeinfo delay: %v", cfg.Proxy.IOSNodeInfoDelay.Duration)
	}
	if cfg.Metrics.MaxMessages != 500 {
		t.Fatalf("unexpected max messages: %d", cfg.Metrics.MaxMessages)
	}
	if cfg.Metrics.MaxTrafficSamples != 600 {
		t.Fatalf("unexpected max traffic samples: %d", cfg.Metrics.MaxTrafficSamples)
	}
}

func TestEnvOverrides(t *testing.T) {
	content := `
[node]
address = "10.0.0.1:4403"
[logging]
level = "info"
format = "text"
`
	tmpFile, err := os.CreateTemp("", "config-env-*.toml")
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

	// Set env vars
	t.Setenv("MESHTASTIC_NODE_ADDRESS", "192.168.1.1:4403")
	t.Setenv("MESHTASTIC_PROXY_LISTEN", ":9999")
	t.Setenv("MESHTASTIC_WEB_LISTEN", ":7777")
	t.Setenv("MESHTASTIC_LOG_LEVEL", "debug")

	cfg, err := Load(tmpFile.Name())
	if err != nil {
		t.Fatalf("failed to load config: %v", err)
	}

	if cfg.Node.Address != "192.168.1.1:4403" {
		t.Fatalf("env override for node address failed: got %s", cfg.Node.Address)
	}
	if cfg.Proxy.Listen != ":9999" {
		t.Fatalf("env override for proxy listen failed: got %s", cfg.Proxy.Listen)
	}
	if cfg.Web.Listen != ":7777" {
		t.Fatalf("env override for web listen failed: got %s", cfg.Web.Listen)
	}
	if cfg.Logging.Level != "debug" {
		t.Fatalf("env override for log level failed: got %s", cfg.Logging.Level)
	}
}
