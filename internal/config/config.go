package config

import (
	"fmt"
	"os"
	"time"

	"github.com/BurntSushi/toml"
)

// Config represents the application configuration.
type Config struct {
	Node    NodeConfig    `toml:"node"`
	Proxy   ProxyConfig   `toml:"proxy"`
	Web     WebConfig     `toml:"web"`
	MDNS    MDNSConfig    `toml:"mdns"`
	Logging LoggingConfig `toml:"logging"`
}

// NodeConfig holds settings for the upstream Meshtastic node connection.
type NodeConfig struct {
	Address              string   `toml:"address"`
	ReconnectInterval    Duration `toml:"reconnect_interval"`
	MaxReconnectInterval Duration `toml:"max_reconnect_interval"`
}

// ProxyConfig holds settings for the client-facing TCP listener.
type ProxyConfig struct {
	Listen     string `toml:"listen"`
	MaxClients int    `toml:"max_clients"`
}

// WebConfig holds settings for the web dashboard.
type WebConfig struct {
	Listen  string `toml:"listen"`
	Enabled bool   `toml:"enabled"`
}

// LoggingConfig holds logging settings.
type LoggingConfig struct {
	Level  string `toml:"level"`  // debug, info, warn, error
	Format string `toml:"format"` // text, json
}

// MDNSConfig holds settings for mDNS service advertisement.
// When enabled, the proxy advertises itself as a _meshtastic._tcp service
// so that iOS/Android apps can discover it via Bonjour/mDNS.
type MDNSConfig struct {
	Enabled   bool   `toml:"enabled"`    // enable mDNS advertisement
	Instance  string `toml:"instance"`   // mDNS instance name (e.g. "Meshtastic Proxy")
	ShortName string `toml:"short_name"` // TXT record: node short name (4 chars)
	ID        string `toml:"id"`         // TXT record: node ID (e.g. "!deadbeef")
}

// Duration is a wrapper around time.Duration that supports TOML string parsing.
type Duration struct {
	time.Duration
}

// UnmarshalText parses a duration string (e.g., "1s", "30s", "5m").
func (d *Duration) UnmarshalText(text []byte) error {
	var err error
	d.Duration, err = time.ParseDuration(string(text))
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", string(text), err)
	}
	return nil
}

// MarshalText returns the duration as a string.
func (d Duration) MarshalText() ([]byte, error) {
	return []byte(d.Duration.String()), nil
}

// DefaultConfig returns a configuration with sensible defaults.
func DefaultConfig() *Config {
	return &Config{
		Node: NodeConfig{
			Address:              "localhost:4403",
			ReconnectInterval:    Duration{1 * time.Second},
			MaxReconnectInterval: Duration{30 * time.Second},
		},
		Proxy: ProxyConfig{
			Listen:     ":4404",
			MaxClients: 10,
		},
		Web: WebConfig{
			Listen:  ":8080",
			Enabled: true,
		},
		MDNS: MDNSConfig{
			Enabled:   true,
			Instance:  "Meshtastic Proxy",
			ShortName: "PRXY",
		},
		Logging: LoggingConfig{
			Level:  "info",
			Format: "text",
		},
	}
}

// Load reads a TOML configuration file and merges it with defaults.
func Load(path string) (*Config, error) {
	cfg := DefaultConfig()

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file: %w", err)
	}

	if err := toml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parsing config file: %w", err)
	}

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	return cfg, nil
}

// Validate checks that the configuration values are valid.
func (c *Config) Validate() error {
	if c.Node.Address == "" {
		return fmt.Errorf("node.address is required")
	}
	if c.Proxy.Listen == "" {
		return fmt.Errorf("proxy.listen is required")
	}
	if c.Proxy.MaxClients < 1 {
		return fmt.Errorf("proxy.max_clients must be at least 1")
	}
	if c.Node.ReconnectInterval.Duration < 100*time.Millisecond {
		return fmt.Errorf("node.reconnect_interval must be at least 100ms")
	}
	if c.Node.MaxReconnectInterval.Duration < c.Node.ReconnectInterval.Duration {
		return fmt.Errorf("node.max_reconnect_interval must be >= reconnect_interval")
	}
	if c.MDNS.Enabled {
		if c.MDNS.Instance == "" {
			return fmt.Errorf("mdns.instance is required when mdns is enabled")
		}
		if len(c.MDNS.ShortName) > 4 {
			return fmt.Errorf("mdns.short_name must be at most 4 characters")
		}
	}
	return nil
}
