package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/jfett/meshtastic-proxy/internal/config"
	"github.com/jfett/meshtastic-proxy/internal/discovery"
	"github.com/jfett/meshtastic-proxy/internal/metrics"
	"github.com/jfett/meshtastic-proxy/internal/node"
	"github.com/jfett/meshtastic-proxy/internal/proxy"
	"github.com/jfett/meshtastic-proxy/internal/web"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func main() {
	configPath := flag.String("config", "config.toml", "path to TOML config file")
	flag.Parse()

	// Load configuration
	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	// Setup logger
	logger := setupLogger(cfg.Logging)
	logger.Info("starting meshtastic-proxy",
		"node", cfg.Node.Address,
		"proxy_listen", cfg.Proxy.Listen,
		"web_listen", cfg.Web.Listen,
	)

	// Create context with signal handling
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Create metrics collector
	m := metrics.New(cfg.Metrics.MaxMessages, cfg.Metrics.MaxTrafficSamples)
	m.NodeAddress = cfg.Node.Address

	// Start traffic sampler (1s interval, ring buffer for charts)
	m.StartSampler(ctx)

	// Create node connection
	nodeConn := node.NewConnection(node.ConnectionOptions{
		Address:              cfg.Node.Address,
		ReconnectInterval:    cfg.Node.ReconnectInterval.Duration,
		MaxReconnectInterval: cfg.Node.MaxReconnectInterval.Duration,
		DialTimeout:          cfg.Node.DialTimeout.Duration,
		ReadTimeout:          cfg.Node.ReadTimeout.Duration,
		FromBuffer:           cfg.Node.FromBuffer,
		ToBuffer:             cfg.Node.ToBuffer,
		Metrics:              m,
		Logger:               logger.With("component", "node"),
	})

	// Create proxy hub
	proxyHub := proxy.New(proxy.Options{
		ListenAddr:        cfg.Proxy.Listen,
		MaxClients:        cfg.Proxy.MaxClients,
		ClientSendBuffer:  cfg.Proxy.ClientSendBuffer,
		ClientIdleTimeout: cfg.Proxy.ClientIdleTimeout.Duration,
		IOSNodeInfoDelay:  cfg.Proxy.IOSNodeInfoDelay.Duration,
		NodeConn:          nodeConn,
		Metrics:           m,
		Logger:            logger.With("component", "proxy"),
	})

	// Start all components
	errCh := make(chan error, 4)

	// Start node connection manager
	go func() {
		nodeConn.Run(ctx)
		errCh <- nil
	}()

	// Start proxy listener
	go func() {
		errCh <- proxyHub.Run(ctx)
	}()

	// Start web dashboard
	if cfg.Web.Enabled {
		promRegistry := metrics.NewPrometheusRegistry(m)
		promHandler := promhttp.HandlerFor(promRegistry, promhttp.HandlerOpts{})

		webServer := web.NewServer(
			cfg.Web.Listen,
			m,
			logger.With("component", "web"),
			proxyHub.ClientAddrs,
			promHandler,
		)
		go func() {
			errCh <- webServer.Run(ctx)
		}()
	}

	// Start mDNS service advertisement
	if cfg.MDNS.Enabled {
		adv, err := discovery.NewAdvertiser(
			cfg.MDNS,
			cfg.Proxy.Listen,
			logger.With("component", "mdns"),
		)
		if err != nil {
			logger.Error("failed to create mDNS advertiser", "error", err)
		} else {
			go func() {
				errCh <- adv.Run(ctx)
			}()
		}
	}

	// Wait for shutdown signal or error
	select {
	case <-ctx.Done():
		logger.Info("received shutdown signal")
	case err := <-errCh:
		if err != nil {
			logger.Error("component error", "error", err)
			cancel()
		}
	}

	logger.Info("meshtastic-proxy stopped")
}

func setupLogger(cfg config.LoggingConfig) *slog.Logger {
	var level slog.Level
	switch cfg.Level {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}

	opts := &slog.HandlerOptions{Level: level}

	var handler slog.Handler
	if cfg.Format == "json" {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	} else {
		handler = slog.NewTextHandler(os.Stdout, opts)
	}

	return slog.New(handler)
}
