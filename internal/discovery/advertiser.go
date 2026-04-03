// Package discovery provides mDNS service advertisement for the proxy.
// It advertises the proxy as a _meshtastic._tcp service so that
// iOS/Android Meshtastic apps can auto-discover it via Bonjour/mDNS.
package discovery

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"strconv"

	"github.com/hashicorp/mdns"

	"github.com/jfett/meshtastic-proxy/internal/config"
)

// Advertiser broadcasts an mDNS service record for the proxy.
type Advertiser struct {
	cfg    config.MDNSConfig
	port   int
	logger *slog.Logger
	server *mdns.Server
}

// NewAdvertiser creates a new mDNS advertiser.
// proxyListen is the proxy listen address (e.g. ":4404" or "0.0.0.0:4403")
// from which the port is extracted for the mDNS service record.
func NewAdvertiser(cfg config.MDNSConfig, proxyListen string, logger *slog.Logger) (*Advertiser, error) {
	_, portStr, err := net.SplitHostPort(proxyListen)
	if err != nil {
		return nil, fmt.Errorf("parsing proxy listen address: %w", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return nil, fmt.Errorf("parsing proxy port: %w", err)
	}

	return &Advertiser{
		cfg:    cfg,
		port:   port,
		logger: logger,
	}, nil
}

// buildTXTRecords returns the TXT records matching the Meshtastic firmware format.
func (a *Advertiser) buildTXTRecords() []string {
	var txt []string

	if a.cfg.ShortName != "" {
		txt = append(txt, "shortname="+a.cfg.ShortName)
	}
	if a.cfg.ID != "" {
		txt = append(txt, "id="+a.cfg.ID)
	}
	txt = append(txt, "pio_env=proxy")

	return txt
}

// Run starts the mDNS server and blocks until the context is cancelled.
// On context cancellation, the mDNS server is shut down gracefully.
func (a *Advertiser) Run(ctx context.Context) error {
	txt := a.buildTXTRecords()

	// Create the mDNS service zone.
	// service type "_meshtastic._tcp" matches what the Meshtastic firmware advertises.
	// domain "" defaults to "local."
	// hostName "" defaults to os.Hostname()
	// ips nil auto-detects from hostname
	service, err := mdns.NewMDNSService(
		a.cfg.Instance,     // instance name (e.g. "Meshtastic Proxy")
		"_meshtastic._tcp", // service type
		"",                 // domain (defaults to "local.")
		"",                 // hostName (defaults to os.Hostname())
		a.port,             // port
		nil,                // IPs (auto-detect)
		txt,                // TXT records
	)
	if err != nil {
		return fmt.Errorf("creating mDNS service: %w", err)
	}

	// Start the mDNS server — it immediately begins responding to queries.
	server, err := mdns.NewServer(&mdns.Config{
		Zone: service,
	})
	if err != nil {
		return fmt.Errorf("starting mDNS server: %w", err)
	}
	a.server = server

	a.logger.Info("mDNS advertisement started",
		"instance", a.cfg.Instance,
		"service", "_meshtastic._tcp",
		"port", a.port,
		"txt", txt,
	)

	// Block until context is cancelled.
	<-ctx.Done()

	a.logger.Info("stopping mDNS advertisement")
	if err := server.Shutdown(); err != nil {
		return fmt.Errorf("shutting down mDNS server: %w", err)
	}

	return nil
}
