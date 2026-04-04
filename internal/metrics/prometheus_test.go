package metrics

import (
	"bytes"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/prometheus/common/expfmt"
)

func TestNewPrometheusRegistry(t *testing.T) {
	m := New(100, 300)
	reg := NewPrometheusRegistry(m)

	// Gather should succeed without errors.
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather() error: %v", err)
	}

	// We expect our custom metrics plus go_* and process_* collectors.
	if len(families) == 0 {
		t.Fatal("expected at least one metric family, got 0")
	}

	// Verify our custom metrics are present.
	names := make(map[string]bool)
	for _, f := range families {
		names[f.GetName()] = true
	}

	expected := []string{
		"meshtastic_proxy_info",
		"meshtastic_proxy_uptime_seconds",
		"meshtastic_proxy_node_connected",
		"meshtastic_proxy_active_clients",
		"meshtastic_proxy_bytes_from_node_total",
		"meshtastic_proxy_bytes_to_node_total",
		"meshtastic_proxy_frames_from_node_total",
		"meshtastic_proxy_frames_to_node_total",
		"meshtastic_proxy_node_reconnects_total",
		"meshtastic_proxy_node_connection_errors_total",
		"meshtastic_proxy_config_cache_frames",
		"meshtastic_proxy_config_cache_age_seconds",
		"meshtastic_proxy_config_replays_total",
		"meshtastic_proxy_mesh_nodes",
	}

	for _, name := range expected {
		if !names[name] {
			t.Errorf("expected metric %q not found in gathered families", name)
		}
	}

	// Verify go runtime metrics are present.
	if !names["go_goroutines"] {
		t.Error("expected go_goroutines metric from Go collector")
	}
}

func TestPrometheusCollectorValues(t *testing.T) {
	m := New(100, 300)
	m.NodeAddress = "10.0.0.1:4403"

	// Set up known state.
	m.NodeConnected.Store(true)
	m.ActiveClients.Store(3)
	m.BytesFromNode.Store(1024)
	m.BytesToNode.Store(512)
	m.FramesFromNode.Store(100)
	m.FramesToNode.Store(50)
	m.NodeReconnects.Store(2)
	m.NodeConnectionErrors.Store(5)
	m.ConfigCacheFrames.Store(172)
	m.ConfigReplaysFull.Store(10)
	m.ConfigReplaysConfigOnly.Store(7)
	m.ConfigReplaysNodesOnly.Store(3)

	// Add some messages to populate typeCounts.
	m.RecordMessage(MessageRecord{Type: "mesh_packet", PortNum: "TEXT_MESSAGE_APP", Size: 50})
	m.RecordMessage(MessageRecord{Type: "mesh_packet", PortNum: "TEXT_MESSAGE_APP", Size: 60})
	m.RecordMessage(MessageRecord{Type: "mesh_packet", PortNum: "POSITION_APP", Size: 70})

	// Add some nodes to the directory.
	m.SetNodeDirectory(map[uint32]NodeEntry{
		0x12345678: {ShortName: "TST1", LongName: "Test Node 1"},
		0xABCDEF01: {ShortName: "TST2", LongName: "Test Node 2"},
		0x11223344: {ShortName: "TST3", LongName: "Test Node 3"},
	})

	collector := newPrometheusCollector(m)

	// Test node_connected = 1 when connected.
	val := testutil.ToFloat64(newConstCollector(collector, "meshtastic_proxy_node_connected"))
	if val != 1.0 {
		t.Errorf("node_connected = %v, want 1.0", val)
	}

	// Test active_clients.
	val = testutil.ToFloat64(newConstCollector(collector, "meshtastic_proxy_active_clients"))
	if val != 3.0 {
		t.Errorf("active_clients = %v, want 3.0", val)
	}

	// Test traffic counters.
	val = testutil.ToFloat64(newConstCollector(collector, "meshtastic_proxy_bytes_from_node_total"))
	if val != 1024.0 {
		t.Errorf("bytes_from_node_total = %v, want 1024.0", val)
	}
	val = testutil.ToFloat64(newConstCollector(collector, "meshtastic_proxy_bytes_to_node_total"))
	if val != 512.0 {
		t.Errorf("bytes_to_node_total = %v, want 512.0", val)
	}
	val = testutil.ToFloat64(newConstCollector(collector, "meshtastic_proxy_frames_from_node_total"))
	if val != 100.0 {
		t.Errorf("frames_from_node_total = %v, want 100.0", val)
	}
	val = testutil.ToFloat64(newConstCollector(collector, "meshtastic_proxy_frames_to_node_total"))
	if val != 50.0 {
		t.Errorf("frames_to_node_total = %v, want 50.0", val)
	}

	// Test node reliability counters.
	val = testutil.ToFloat64(newConstCollector(collector, "meshtastic_proxy_node_reconnects_total"))
	if val != 2.0 {
		t.Errorf("node_reconnects_total = %v, want 2.0", val)
	}
	val = testutil.ToFloat64(newConstCollector(collector, "meshtastic_proxy_node_connection_errors_total"))
	if val != 5.0 {
		t.Errorf("node_connection_errors_total = %v, want 5.0", val)
	}

	// Test config cache.
	val = testutil.ToFloat64(newConstCollector(collector, "meshtastic_proxy_config_cache_frames"))
	if val != 172.0 {
		t.Errorf("config_cache_frames = %v, want 172.0", val)
	}

	// Test mesh_nodes.
	val = testutil.ToFloat64(newConstCollector(collector, "meshtastic_proxy_mesh_nodes"))
	if val != 3.0 {
		t.Errorf("mesh_nodes = %v, want 3.0", val)
	}

	// Test messages_total with labels — use full Gather to check label values.
	reg := prometheus.NewRegistry()
	reg.MustRegister(collector)

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather() error: %v", err)
	}

	messageCounts := make(map[string]float64)
	replayCounts := make(map[string]float64)
	for _, f := range families {
		switch f.GetName() {
		case "meshtastic_proxy_messages_total":
			for _, metric := range f.GetMetric() {
				for _, lp := range metric.GetLabel() {
					if lp.GetName() == "port_num" {
						messageCounts[lp.GetValue()] = metric.GetCounter().GetValue()
					}
				}
			}
		case "meshtastic_proxy_config_replays_total":
			for _, metric := range f.GetMetric() {
				for _, lp := range metric.GetLabel() {
					if lp.GetName() == "type" {
						replayCounts[lp.GetValue()] = metric.GetCounter().GetValue()
					}
				}
			}
		}
	}

	// Verify messages_total by port_num.
	if messageCounts["TEXT_MESSAGE_APP"] != 2 {
		t.Errorf("messages_total{port_num=TEXT_MESSAGE_APP} = %v, want 2", messageCounts["TEXT_MESSAGE_APP"])
	}
	if messageCounts["POSITION_APP"] != 1 {
		t.Errorf("messages_total{port_num=POSITION_APP} = %v, want 1", messageCounts["POSITION_APP"])
	}

	// Verify config_replays_total by type.
	if replayCounts["full"] != 10 {
		t.Errorf("config_replays_total{type=full} = %v, want 10", replayCounts["full"])
	}
	if replayCounts["config_only"] != 7 {
		t.Errorf("config_replays_total{type=config_only} = %v, want 7", replayCounts["config_only"])
	}
	if replayCounts["nodes_only"] != 3 {
		t.Errorf("config_replays_total{type=nodes_only} = %v, want 3", replayCounts["nodes_only"])
	}
}

func TestPrometheusNodeDisconnected(t *testing.T) {
	m := New(100, 300)
	m.NodeConnected.Store(false)

	collector := newPrometheusCollector(m)

	val := testutil.ToFloat64(newConstCollector(collector, "meshtastic_proxy_node_connected"))
	if val != 0.0 {
		t.Errorf("node_connected = %v, want 0.0", val)
	}
}

func TestPrometheusInfoLabel(t *testing.T) {
	m := New(100, 300)
	m.NodeAddress = "192.168.1.100:4403"

	reg := prometheus.NewRegistry()
	reg.MustRegister(newPrometheusCollector(m))

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather() error: %v", err)
	}

	for _, f := range families {
		if f.GetName() == "meshtastic_proxy_info" {
			for _, metric := range f.GetMetric() {
				for _, lp := range metric.GetLabel() {
					if lp.GetName() == "node_address" && lp.GetValue() == "192.168.1.100:4403" {
						return // found it
					}
				}
			}
			t.Error("meshtastic_proxy_info missing node_address label")
			return
		}
	}
	t.Error("meshtastic_proxy_info metric not found")
}

func TestPrometheusNoMessagesYield(t *testing.T) {
	m := New(100, 300)

	// With no messages recorded, messages_total should not appear.
	reg := prometheus.NewRegistry()
	reg.MustRegister(newPrometheusCollector(m))

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather() error: %v", err)
	}

	for _, f := range families {
		if f.GetName() == "meshtastic_proxy_messages_total" {
			t.Error("expected no messages_total when no messages recorded")
		}
	}
}

func TestPrometheusOutputFormat(t *testing.T) {
	m := New(100, 300)
	m.NodeAddress = "10.0.0.1:4403"
	m.NodeConnected.Store(true)
	m.ActiveClients.Store(2)
	m.RecordMessage(MessageRecord{Type: "mesh_packet", PortNum: "TEXT_MESSAGE_APP", Size: 42})

	reg := NewPrometheusRegistry(m)

	// Gather all metrics and render to text exposition format.
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather() error: %v", err)
	}

	var buf bytes.Buffer
	enc := expfmt.NewEncoder(&buf, expfmt.NewFormat(expfmt.TypeTextPlain))
	for _, f := range families {
		if err := enc.Encode(f); err != nil {
			t.Fatalf("Encode(%s) error: %v", f.GetName(), err)
		}
	}

	text := buf.String()

	mustContain := []string{
		"meshtastic_proxy_info{node_address=\"10.0.0.1:4403\"} 1",
		"meshtastic_proxy_node_connected 1",
		"meshtastic_proxy_active_clients 2",
		"meshtastic_proxy_messages_total{port_num=\"TEXT_MESSAGE_APP\"} 1",
		"meshtastic_proxy_mesh_nodes 0",
		"go_goroutines",
	}

	for _, want := range mustContain {
		if !strings.Contains(text, want) {
			t.Errorf("output missing expected string %q", want)
		}
	}
}

// newConstCollector wraps a full collector to extract a single metric by name
// for use with testutil.ToFloat64. It returns a prometheus.Collector that only
// reports the first metric matching the given name (no labels).
func newConstCollector(c prometheus.Collector, name string) prometheus.Collector {
	return &singleMetricCollector{inner: c, name: name}
}

type singleMetricCollector struct {
	inner prometheus.Collector
	name  string
}

func (s *singleMetricCollector) Describe(ch chan<- *prometheus.Desc) {
	// Collect all descs and filter.
	all := make(chan *prometheus.Desc, 100)
	go func() {
		s.inner.Describe(all)
		close(all)
	}()
	for d := range all {
		if strings.Contains(d.String(), "\""+s.name+"\"") {
			ch <- d
		}
	}
}

func (s *singleMetricCollector) Collect(ch chan<- prometheus.Metric) {
	// Collect all metrics and filter by desc name.
	all := make(chan prometheus.Metric, 100)
	go func() {
		s.inner.Collect(all)
		close(all)
	}()
	for m := range all {
		if strings.Contains(m.Desc().String(), "\""+s.name+"\"") {
			ch <- m
		}
	}
}
