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

// Advertiser broadcasts mDNS service records for the proxy.
// When Interfaces are configured, a separate mDNS server is started for each
// interface so that the service is discoverable across multiple subnets
// (e.g. when running with hostNetwork in Kubernetes).
type Advertiser struct {
	cfg     config.MDNSConfig
	port    int
	logger  *slog.Logger
	servers []*mdns.Server
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

// interfaceIPs returns the unicast IP addresses assigned to the given interface.
func interfaceIPs(iface *net.Interface) ([]net.IP, error) {
	addrs, err := iface.Addrs()
	if err != nil {
		return nil, fmt.Errorf("listing addresses for %s: %w", iface.Name, err)
	}

	var ips []net.IP
	for _, addr := range addrs {
		var ip net.IP
		switch v := addr.(type) {
		case *net.IPNet:
			ip = v.IP
		case *net.IPAddr:
			ip = v.IP
		}
		if ip != nil && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() {
			ips = append(ips, ip)
		}
	}

	if len(ips) == 0 {
		return nil, fmt.Errorf("no usable IP addresses on interface %s", iface.Name)
	}
	return ips, nil
}

// startServer creates and starts a single mDNS server.
// If iface is nil, the system default multicast interface is used and IPs are
// auto-detected from the hostname.
func (a *Advertiser) startServer(iface *net.Interface) (*mdns.Server, error) {
	txt := a.buildTXTRecords()

	var ips []net.IP
	var ifaceName string

	if iface != nil {
		ifaceName = iface.Name
		var err error
		ips, err = interfaceIPs(iface)
		if err != nil {
			return nil, err
		}
	} else {
		ifaceName = "default"
	}

	// Create the mDNS service zone.
	// service type "_meshtastic._tcp" matches what the Meshtastic firmware advertises.
	// domain "" defaults to "local."
	// hostName "" defaults to os.Hostname()
	service, err := mdns.NewMDNSService(
		a.cfg.Instance,     // instance name (e.g. "Meshtastic Proxy")
		"_meshtastic._tcp", // service type
		"",                 // domain (defaults to "local.")
		"",                 // hostName (defaults to os.Hostname())
		a.port,             // port
		ips,                // IPs (nil = auto-detect from hostname)
		txt,                // TXT records
	)
	if err != nil {
		return nil, fmt.Errorf("creating mDNS service for %s: %w", ifaceName, err)
	}

	server, err := mdns.NewServer(&mdns.Config{
		Zone:  service,
		Iface: iface,
	})
	if err != nil {
		return nil, fmt.Errorf("starting mDNS server on %s: %w", ifaceName, err)
	}

	a.logger.Info("mDNS advertisement started",
		"interface", ifaceName,
		"instance", a.cfg.Instance,
		"service", "_meshtastic._tcp",
		"port", a.port,
		"ips", fmt.Sprint(ips),
		"txt", txt,
	)

	return server, nil
}

// Run starts the mDNS server(s) and blocks until the context is cancelled.
// When Interfaces are configured, a separate mDNS server is started for each
// interface. When no interfaces are specified, a single server with
// auto-detected settings is used (original behaviour).
// On context cancellation all servers are shut down gracefully.
func (a *Advertiser) Run(ctx context.Context) error {
	if len(a.cfg.Interfaces) == 0 {
		// No explicit interfaces — single server with auto-detect (backward compatible).
		server, err := a.startServer(nil)
		if err != nil {
			return err
		}
		a.servers = []*mdns.Server{server}
	} else {
		// Start a server per configured interface.
		for _, name := range a.cfg.Interfaces {
			iface, err := net.InterfaceByName(name)
			if err != nil {
				return fmt.Errorf("looking up interface %q: %w", name, err)
			}
			server, err := a.startServer(iface)
			if err != nil {
				// Shut down any servers we already started.
				a.shutdownAll()
				return err
			}
			a.servers = append(a.servers, server)
		}
	}

	// Block until context is cancelled.
	<-ctx.Done()

	a.logger.Info("stopping mDNS advertisement")
	a.shutdownAll()

	return nil
}

// shutdownAll gracefully shuts down all running mDNS servers.
func (a *Advertiser) shutdownAll() {
	for _, s := range a.servers {
		if err := s.Shutdown(); err != nil {
			a.logger.Warn("error shutting down mDNS server", "error", err)
		}
	}
	a.servers = nil
}
