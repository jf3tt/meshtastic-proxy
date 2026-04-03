package discovery

import (
	"context"
	"log/slog"
	"os"
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

func TestRunAndShutdown(t *testing.T) {
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
			t.Fatalf("Run returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after context cancellation")
	}
}
