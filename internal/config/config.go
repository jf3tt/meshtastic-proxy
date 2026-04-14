package config

import (
	"fmt"
	"os"
	"time"

	"github.com/BurntSushi/toml"
)

// Config represents the application configuration.
type Config struct {
	Node     NodeConfig     `toml:"node"`
	Proxy    ProxyConfig    `toml:"proxy"`
	Web      WebConfig      `toml:"web"`
	MDNS     MDNSConfig     `toml:"mdns"`
	Telegram TelegramConfig `toml:"telegram"`
	Logging  LoggingConfig  `toml:"logging"`
	Metrics  MetricsConfig  `toml:"metrics"`
}

// NodeConfig holds settings for the upstream Meshtastic node connection.
type NodeConfig struct {
	Address              string   `toml:"address"`
	ReconnectInterval    Duration `toml:"reconnect_interval"`
	MaxReconnectInterval Duration `toml:"max_reconnect_interval"`
	DialTimeout          Duration `toml:"dial_timeout"`
	ReadTimeout          Duration `toml:"read_timeout"`
	HeartbeatInterval    Duration `toml:"heartbeat_interval"`
	FromBuffer           int      `toml:"from_buffer"`
	ToBuffer             int      `toml:"to_buffer"`
}

// ProxyConfig holds settings for the client-facing TCP listener.
type ProxyConfig struct {
	Listen            string   `toml:"listen"`
	MaxClients        int      `toml:"max_clients"`
	ClientSendBuffer  int      `toml:"client_send_buffer"`
	ClientIdleTimeout Duration `toml:"client_idle_timeout"`
	IOSNodeInfoDelay  Duration `toml:"ios_nodeinfo_delay"`
	MaxChatCache      int      `toml:"max_chat_cache"`
	ReplayChatHistory bool     `toml:"replay_chat_history"`
}

// MetricsConfig holds settings for the metrics collector.
type MetricsConfig struct {
	MaxMessages       int `toml:"max_messages"`
	MaxTrafficSamples int `toml:"max_traffic_samples"`
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

// TelegramConfig holds settings for the Telegram bot integration.
// When Token is non-empty, the proxy forwards mesh text messages to a Telegram channel.
type TelegramConfig struct {
	Token    string `toml:"token"`    // Telegram Bot API token
	ChatID   int64  `toml:"chat_id"`  // Target channel/group chat ID
	Channels []int  `toml:"channels"` // Mesh channel indices to forward (e.g. [0, 1]); empty = primary only [0]
}

// MDNSConfig holds settings for mDNS service advertisement.
// When enabled, the proxy advertises itself as a _meshtastic._tcp service
// so that iOS/Android apps can discover it via Bonjour/mDNS.
type MDNSConfig struct {
	Enabled    bool     `toml:"enabled"`    // enable mDNS advertisement
	Instance   string   `toml:"instance"`   // mDNS instance name (e.g. "Meshtastic Proxy")
	Hostname   string   `toml:"hostname"`   // mDNS hostname for SRV/A records (e.g. "meshtastic-proxy"); .local. suffix added automatically; empty = os.Hostname()
	ShortName  string   `toml:"short_name"` // TXT record: node short name (4 chars)
	ID         string   `toml:"id"`         // TXT record: node ID (e.g. "!deadbeef")
	Interfaces []string `toml:"interfaces"` // network interfaces to advertise on (e.g. ["eth0"]); empty = auto-detect
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
	return []byte(d.String()), nil
}

// DefaultConfig returns a configuration with sensible defaults.
func DefaultConfig() *Config {
	return &Config{
		Node: NodeConfig{
			Address:              "localhost:4403",
			ReconnectInterval:    Duration{1 * time.Second},
			MaxReconnectInterval: Duration{30 * time.Second},
			DialTimeout:          Duration{10 * time.Second},
			ReadTimeout:          Duration{5 * time.Minute},
			HeartbeatInterval:    Duration{1 * time.Minute},
			FromBuffer:           256,
			ToBuffer:             64,
		},
		Proxy: ProxyConfig{
			Listen:            ":4404",
			MaxClients:        10,
			ClientSendBuffer:  256,
			ClientIdleTimeout: Duration{30 * time.Minute},
			IOSNodeInfoDelay:  Duration{50 * time.Millisecond},
			MaxChatCache:      1000,
			ReplayChatHistory: true,
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
		Metrics: MetricsConfig{
			MaxMessages:       200,
			MaxTrafficSamples: 300,
		},
	}
}

// Load reads a TOML configuration file and merges it with defaults.
// Environment variables override TOML values when set.
func Load(path string) (*Config, error) {
	cfg := DefaultConfig()

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file: %w", err)
	}

	if err := toml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parsing config file: %w", err)
	}

	applyEnvOverrides(cfg)

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	return cfg, nil
}

// applyEnvOverrides applies environment variable overrides to the config.
// Supported variables:
//
//	MESHTASTIC_NODE_ADDRESS     -> node.address
//	MESHTASTIC_PROXY_LISTEN     -> proxy.listen
//	MESHTASTIC_WEB_LISTEN       -> web.listen
//	MESHTASTIC_LOG_LEVEL        -> logging.level
//	MESHTASTIC_TELEGRAM_TOKEN   -> telegram.token
//	MESHTASTIC_TELEGRAM_CHAT_ID -> telegram.chat_id
func applyEnvOverrides(cfg *Config) {
	if v := os.Getenv("MESHTASTIC_NODE_ADDRESS"); v != "" {
		cfg.Node.Address = v
	}
	if v := os.Getenv("MESHTASTIC_PROXY_LISTEN"); v != "" {
		cfg.Proxy.Listen = v
	}
	if v := os.Getenv("MESHTASTIC_WEB_LISTEN"); v != "" {
		cfg.Web.Listen = v
	}
	if v := os.Getenv("MESHTASTIC_LOG_LEVEL"); v != "" {
		cfg.Logging.Level = v
	}
	if v := os.Getenv("MESHTASTIC_TELEGRAM_TOKEN"); v != "" {
		cfg.Telegram.Token = v
	}
	if v := os.Getenv("MESHTASTIC_TELEGRAM_CHAT_ID"); v != "" {
		var chatID int64
		if _, err := fmt.Sscanf(v, "%d", &chatID); err == nil {
			cfg.Telegram.ChatID = chatID
		}
	}
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
	if c.Proxy.ClientSendBuffer < 1 {
		return fmt.Errorf("proxy.client_send_buffer must be at least 1")
	}
	if c.Proxy.ClientIdleTimeout.Duration < 0 {
		return fmt.Errorf("proxy.client_idle_timeout must not be negative")
	}
	if c.Proxy.IOSNodeInfoDelay.Duration < 0 {
		return fmt.Errorf("proxy.ios_nodeinfo_delay must not be negative")
	}
	if c.Proxy.MaxChatCache < 0 {
		return fmt.Errorf("proxy.max_chat_cache must not be negative")
	}
	if c.Node.ReconnectInterval.Duration < 100*time.Millisecond {
		return fmt.Errorf("node.reconnect_interval must be at least 100ms")
	}
	if c.Node.MaxReconnectInterval.Duration < c.Node.ReconnectInterval.Duration {
		return fmt.Errorf("node.max_reconnect_interval must be >= reconnect_interval")
	}
	if c.Node.DialTimeout.Duration < 0 {
		return fmt.Errorf("node.dial_timeout must not be negative")
	}
	if c.Node.ReadTimeout.Duration < 0 {
		return fmt.Errorf("node.read_timeout must not be negative")
	}
	if c.Node.HeartbeatInterval.Duration < 0 {
		return fmt.Errorf("node.heartbeat_interval must not be negative")
	}
	if c.Node.HeartbeatInterval.Duration > 0 && c.Node.ReadTimeout.Duration > 0 &&
		c.Node.HeartbeatInterval.Duration >= c.Node.ReadTimeout.Duration {
		return fmt.Errorf("node.heartbeat_interval must be less than read_timeout")
	}
	if c.Node.FromBuffer < 1 {
		return fmt.Errorf("node.from_buffer must be at least 1")
	}
	if c.Node.ToBuffer < 1 {
		return fmt.Errorf("node.to_buffer must be at least 1")
	}
	if c.Web.Enabled && c.Web.Listen == "" {
		return fmt.Errorf("web.listen is required when web is enabled")
	}
	switch c.Logging.Level {
	case "debug", "info", "warn", "error":
		// valid
	default:
		return fmt.Errorf("logging.level must be one of: debug, info, warn, error (got %q)", c.Logging.Level)
	}
	switch c.Logging.Format {
	case "text", "json":
		// valid
	default:
		return fmt.Errorf("logging.format must be one of: text, json (got %q)", c.Logging.Format)
	}
	if c.Metrics.MaxMessages < 1 {
		return fmt.Errorf("metrics.max_messages must be at least 1")
	}
	if c.Metrics.MaxTrafficSamples < 1 {
		return fmt.Errorf("metrics.max_traffic_samples must be at least 1")
	}
	if c.MDNS.Enabled {
		if c.MDNS.Instance == "" {
			return fmt.Errorf("mdns.instance is required when mdns is enabled")
		}
		if len(c.MDNS.ShortName) > 4 {
			return fmt.Errorf("mdns.short_name must be at most 4 characters")
		}
		for i, iface := range c.MDNS.Interfaces {
			if iface == "" {
				return fmt.Errorf("mdns.interfaces[%d] must not be empty", i)
			}
		}
	}
	if c.Telegram.Token != "" && c.Telegram.ChatID == 0 {
		return fmt.Errorf("telegram.chat_id is required when telegram.token is set")
	}
	for i, ch := range c.Telegram.Channels {
		if ch < 0 || ch > 7 {
			return fmt.Errorf("telegram.channels[%d]: channel index %d out of range (must be 0-7)", i, ch)
		}
	}
	return nil
}
