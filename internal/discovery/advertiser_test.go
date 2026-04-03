package discovery

import (
	"context"
	"log/slog"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/mdns"

	"github.com/jfett/meshtastic-proxy/internal/config"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

func TestNewAdvertiser(t *testing.T) {
	cfg := config.MDNSConfig{
		Enabled:   true,
		Instance:  "Test Proxy",
		ShortName: "TEST",
		ID:        "!deadbeef",
	}

	adv, err := NewAdvertiser(cfg, ":4404", testLogger())
	if err != nil {
		t.Fatalf("NewAdvertiser failed: %v", err)
	}
	if adv.port != 4404 {
		t.Fatalf("expected port 4404, got %d", adv.port)
	}
}

func TestNewAdvertiserWithHost(t *testing.T) {
	cfg := config.MDNSConfig{
		Enabled:  true,
		Instance: "Test Proxy",
	}

	adv, err := NewAdvertiser(cfg, "0.0.0.0:4403", testLogger())
	if err != nil {
		t.Fatalf("NewAdvertiser failed: %v", err)
	}
	if adv.port != 4403 {
		t.Fatalf("expected port 4403, got %d", adv.port)
	}
}

func TestNewAdvertiserInvalidAddress(t *testing.T) {
	cfg := config.MDNSConfig{
		Enabled:  true,
		Instance: "Test",
	}

	_, err := NewAdvertiser(cfg, "invalid-no-port", testLogger())
	if err == nil {
		t.Fatal("expected error for invalid address")
	}
}

func TestBuildTXTRecords(t *testing.T) {
	tests := []struct {
		name     string
		cfg      config.MDNSConfig
		expected []string
	}{
		{
			name: "all fields",
			cfg: config.MDNSConfig{
				ShortName: "PRXY",
				ID:        "!aabbccdd",
			},
			expected: []string{"shortname=PRXY", "id=!aabbccdd", "pio_env=proxy"},
		},
		{
			name: "no ID",
			cfg: config.MDNSConfig{
				ShortName: "TEST",
			},
			expected: []string{"shortname=TEST", "pio_env=proxy"},
		},
		{
			name:     "no shortname no ID",
			cfg:      config.MDNSConfig{},
			expected: []string{"pio_env=proxy"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			adv := &Advertiser{cfg: tt.cfg}
			got := adv.buildTXTRecords()

			if len(got) != len(tt.expected) {
				t.Fatalf("expected %d TXT records, got %d: %v", len(tt.expected), len(got), got)
			}
			for i, v := range got {
				if v != tt.expected[i] {
					t.Fatalf("TXT[%d]: expected %q, got %q", i, tt.expected[i], v)
				}
			}
		})
	}
}

func TestInterfaceIPs(t *testing.T) {
	// loopback usually has only 127.0.0.1/::1 which we filter out.
	// However, some systems attach extra addresses to lo (e.g. Tailscale),
	// so we simply verify that interfaceIPs never returns loopback or
	// link-local addresses regardless of the interface.
	lo, err := net.InterfaceByName("lo")
	if err != nil {
		t.Skipf("skipping: loopback interface not available: %v", err)
	}
	ips, err := interfaceIPs(lo)
	if err != nil {
		// Expected on most systems — no usable IPs on pure loopback.
		return
	}
	// If IPs were returned (e.g. Tailscale adds addresses to lo),
	// verify none of them are loopback or link-local.
	for _, ip := range ips {
		if ip.IsLoopback() {
			t.Errorf("interfaceIPs returned loopback address: %s", ip)
		}
		if ip.IsLinkLocalUnicast() {
			t.Errorf("interfaceIPs returned link-local address: %s", ip)
		}
	}
}

func TestInterfaceIPsNonLoopback(t *testing.T) {
	// Find any non-loopback interface with at least one unicast address.
	ifaces, err := net.Interfaces()
	if err != nil {
		t.Fatalf("listing interfaces: %v", err)
	}

	for _, iface := range ifaces {
		if iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		if iface.Flags&net.FlagUp == 0 {
			continue
		}
		ips, err := interfaceIPs(&iface)
		if err != nil {
			continue // no usable IPs on this interface
		}
		if len(ips) == 0 {
			t.Fatalf("interfaceIPs returned empty slice without error for %s", iface.Name)
		}
		// At least one interface with IPs found — test passes.
		t.Logf("interface %s has IPs: %v", iface.Name, ips)
		return
	}

	t.Skip("no non-loopback interface with usable IPs found")
}

func TestRunAndShutdownDefault(t *testing.T) {
	cfg := config.MDNSConfig{
		Enabled:   true,
		Instance:  "Test Proxy",
		ShortName: "TEST",
		ID:        "!12345678",
	}

	adv, err := NewAdvertiser(cfg, ":4404", testLogger())
	if err != nil {
		t.Fatalf("NewAdvertiser failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)

	go func() {
		errCh <- adv.Run(ctx)
	}()

	// Give the mDNS server time to start
	time.Sleep(200 * time.Millisecond)

	// Verify the service is discoverable via mDNS lookup.
	// This may not work in CI/containers without multicast support,
	// so we only log a warning instead of failing.
	entriesCh := make(chan *mdns.ServiceEntry, 4)
	found := false

	go func() {
		params := &mdns.QueryParam{
			Service: "_meshtastic._tcp",
			Timeout: 2 * time.Second,
			Entries: entriesCh,
		}
		_ = mdns.Query(params)
	}()

	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()

loop:
	for {
		select {
		case entry := <-entriesCh:
			if entry != nil && entry.Port == 4404 {
				found = true
				hasPioEnv := false
				hasShortname := false
				for _, txt := range entry.InfoFields {
					if txt == "pio_env=proxy" {
						hasPioEnv = true
					}
					if txt == "shortname=TEST" {
						hasShortname = true
					}
				}
				if !hasPioEnv {
					t.Error("missing pio_env=proxy TXT record")
				}
				if !hasShortname {
					t.Error("missing shortname=TEST TXT record")
				}
				break loop
			}
		case <-timer.C:
			break loop
		}
	}

	if !found {
		t.Log("mDNS service not discovered — multicast likely unavailable (CI/container); skipping discovery check")
	}

	// Shutdown — this MUST work regardless of multicast availability
	cancel()

	select {
	case err := <-errCh:
		if err != nil {
			// In CI/container environments the host may have no resolvable IP
			// addresses, causing mDNS service creation to fail. Treat this as a
			// skipped test rather than a hard failure.
			if strings.Contains(err.Error(), "could not determine host IP addresses") {
				t.Skipf("skipping: mDNS unavailable in this environment: %v", err)
			}
			t.Fatalf("Run returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after context cancellation")
	}
}

func TestRunWithInterface(t *testing.T) {
	// Find a non-loopback, multicast-capable interface to test with.
	ifaceName := findMulticastInterface(t)

	cfg := config.MDNSConfig{
		Enabled:    true,
		Instance:   "Test Interface",
		ShortName:  "IFCE",
		Interfaces: []string{ifaceName},
	}

	adv, err := NewAdvertiser(cfg, ":4405", testLogger())
	if err != nil {
		t.Fatalf("NewAdvertiser failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)

	go func() {
		errCh <- adv.Run(ctx)
	}()

	// Give the mDNS server time to start
	time.Sleep(200 * time.Millisecond)

	// Verify that servers slice was populated.
	if len(adv.servers) != 1 {
		t.Fatalf("expected 1 server, got %d", len(adv.servers))
	}

	cancel()

	select {
	case err := <-errCh:
		if err != nil {
			if strings.Contains(err.Error(), "no usable IP") || strings.Contains(err.Error(), "no multicast") {
				t.Skipf("skipping: interface %s not usable for mDNS: %v", ifaceName, err)
			}
			t.Fatalf("Run returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after context cancellation")
	}

	// After shutdown, servers should be cleared.
	if len(adv.servers) != 0 {
		t.Fatalf("expected 0 servers after shutdown, got %d", len(adv.servers))
	}
}

func TestRunWithInvalidInterface(t *testing.T) {
	cfg := config.MDNSConfig{
		Enabled:    true,
		Instance:   "Test Invalid",
		ShortName:  "INVL",
		Interfaces: []string{"nonexistent0"},
	}

	adv, err := NewAdvertiser(cfg, ":4406", testLogger())
	if err != nil {
		t.Fatalf("NewAdvertiser failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err = adv.Run(ctx)
	if err == nil {
		t.Fatal("expected error for nonexistent interface")
	}
	if !strings.Contains(err.Error(), "nonexistent0") {
		t.Fatalf("error should mention the interface name, got: %v", err)
	}
}

// findMulticastInterface returns the name of a non-loopback, up, multicast-capable
// interface with at least one usable IP. Skips the test if none is found.
func findMulticastInterface(t *testing.T) string {
	t.Helper()

	ifaces, err := net.Interfaces()
	if err != nil {
		t.Skipf("cannot list interfaces: %v", err)
	}

	for _, iface := range ifaces {
		if iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		if iface.Flags&net.FlagUp == 0 {
			continue
		}
		if iface.Flags&net.FlagMulticast == 0 {
			continue
		}
		// Check that the interface has at least one usable IP.
		if _, err := interfaceIPs(&iface); err != nil {
			continue
		}
		return iface.Name
	}

	t.Skip("no multicast-capable interface with usable IPs found")
	return ""
}
