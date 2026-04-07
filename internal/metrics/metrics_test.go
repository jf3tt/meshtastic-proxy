package metrics

import (
	"context"
	"testing"
	"time"
)

func TestNewMetrics(t *testing.T) {
	m := New(10, 300)
	if m == nil {
		t.Fatal("New returned nil")
	}
	if m.ActiveClients.Load() != 0 {
		t.Fatal("expected 0 active clients")
	}
	if m.NodeConnected.Load() {
		t.Fatal("expected node disconnected initially")
	}
}

func TestNewMetricsDefaultMax(t *testing.T) {
	m := New(0, 0)
	if m.maxMessages != 100 {
		t.Fatalf("expected default maxMessages=100, got %d", m.maxMessages)
	}
	if m.maxSamples != 300 {
		t.Fatalf("expected default maxSamples=300, got %d", m.maxSamples)
	}
}

func TestUptime(t *testing.T) {
	m := New(10, 300)
	time.Sleep(10 * time.Millisecond)
	uptime := m.Uptime()
	if uptime < 10*time.Millisecond {
		t.Fatalf("uptime too small: %v", uptime)
	}
}

func TestRecordAndRetrieveMessages(t *testing.T) {
	m := New(5, 300)

	for i := 0; i < 3; i++ {
		m.RecordMessage(MessageRecord{
			Direction: "from_node",
			Type:      "packet",
			Size:      100 + i,
		})
	}

	msgs := m.Messages()
	if len(msgs) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(msgs))
	}

	for _, msg := range msgs {
		if msg.Timestamp.IsZero() {
			t.Fatal("expected non-zero timestamp")
		}
	}
}

func TestMessageRingBuffer(t *testing.T) {
	m := New(3, 300)

	for i := 0; i < 5; i++ {
		m.RecordMessage(MessageRecord{
			Direction: "from_node",
			Type:      "packet",
			Size:      i,
		})
	}

	msgs := m.Messages()
	if len(msgs) != 3 {
		t.Fatalf("expected 3 messages (ring buffer), got %d", len(msgs))
	}

	if msgs[0].Size != 2 {
		t.Fatalf("expected first message size 2, got %d", msgs[0].Size)
	}
	if msgs[2].Size != 4 {
		t.Fatalf("expected last message size 4, got %d", msgs[2].Size)
	}
}

func TestMessageRecordExpandedFields(t *testing.T) {
	m := New(10, 300)

	m.RecordMessage(MessageRecord{
		Direction: "from_node",
		Type:      "mesh_packet",
		Size:      200,
		PortNum:   "TEXT_MESSAGE_APP",
		From:      0xaabbccdd,
		To:        0x11223344,
		Channel:   2,
		Payload:   "Hello mesh!",
	})

	msgs := m.Messages()
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}

	msg := msgs[0]
	if msg.PortNum != "TEXT_MESSAGE_APP" {
		t.Fatalf("expected PortNum TEXT_MESSAGE_APP, got %s", msg.PortNum)
	}
	if msg.From != 0xaabbccdd {
		t.Fatalf("expected From 0xaabbccdd, got 0x%x", msg.From)
	}
	if msg.To != 0x11223344 {
		t.Fatalf("expected To 0x11223344, got 0x%x", msg.To)
	}
	if msg.Channel != 2 {
		t.Fatalf("expected Channel 2, got %d", msg.Channel)
	}
	if msg.Payload != "Hello mesh!" {
		t.Fatalf("expected Payload 'Hello mesh!', got %q", msg.Payload)
	}
}

func TestSnapshot(t *testing.T) {
	m := New(10, 300)
	m.NodeAddress = "192.168.1.100:4403"
	m.NodeConnected.Store(true)
	m.ActiveClients.Store(3)
	m.BytesFromNode.Store(1024)
	m.BytesToNode.Store(512)
	m.FramesFromNode.Store(10)
	m.FramesToNode.Store(5)

	m.RecordMessage(MessageRecord{
		Direction: "from_node",
		Type:      "packet",
		Size:      100,
		PortNum:   "POSITION_APP",
	})

	snap := m.Snapshot()

	if !snap.NodeConnected {
		t.Fatal("expected node connected")
	}
	if snap.NodeAddress != "192.168.1.100:4403" {
		t.Fatalf("unexpected node address: %s", snap.NodeAddress)
	}
	if snap.ActiveClients != 3 {
		t.Fatalf("expected 3 clients, got %d", snap.ActiveClients)
	}
	if snap.BytesFromNode != 1024 {
		t.Fatalf("expected 1024 bytes from node, got %d", snap.BytesFromNode)
	}
	if snap.BytesToNode != 512 {
		t.Fatalf("expected 512 bytes to node, got %d", snap.BytesToNode)
	}
	if len(snap.Messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(snap.Messages))
	}
	if snap.UptimeStr == "" {
		t.Fatal("expected non-empty uptime string")
	}
	if snap.TrafficSeries == nil {
		t.Fatal("expected non-nil TrafficSeries")
	}
	if snap.MessageTypeCounts == nil {
		t.Fatal("expected non-nil MessageTypeCounts")
	}
	if snap.MessageTypeCounts["POSITION_APP"] != 1 {
		t.Fatalf("expected POSITION_APP count 1, got %d", snap.MessageTypeCounts["POSITION_APP"])
	}
}

func TestAtomicCounters(t *testing.T) {
	m := New(10, 300)

	done := make(chan struct{})
	for i := 0; i < 100; i++ {
		go func() {
			m.BytesFromNode.Add(1)
			m.FramesFromNode.Add(1)
			done <- struct{}{}
		}()
	}
	for i := 0; i < 100; i++ {
		<-done
	}

	if m.BytesFromNode.Load() != 100 {
		t.Fatalf("expected 100 bytes, got %d", m.BytesFromNode.Load())
	}
	if m.FramesFromNode.Load() != 100 {
		t.Fatalf("expected 100 frames, got %d", m.FramesFromNode.Load())
	}
}

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{5 * time.Second, "5s"},
		{65 * time.Second, "1m 5s"},
		{3661 * time.Second, "1h 1m 1s"},
		{90061 * time.Second, "1d 1h 1m 1s"},
	}

	for _, tt := range tests {
		got := formatDuration(tt.d)
		if got != tt.want {
			t.Errorf("formatDuration(%v) = %q, want %q", tt.d, got, tt.want)
		}
	}
}

func TestMessageTypeCounts(t *testing.T) {
	m := New(100, 300)

	// Record messages with different PortNum values
	m.RecordMessage(MessageRecord{Type: "mesh_packet", PortNum: "TEXT_MESSAGE_APP", Size: 50})
	m.RecordMessage(MessageRecord{Type: "mesh_packet", PortNum: "TEXT_MESSAGE_APP", Size: 60})
	m.RecordMessage(MessageRecord{Type: "mesh_packet", PortNum: "POSITION_APP", Size: 70})
	m.RecordMessage(MessageRecord{Type: "config", Size: 30}) // no PortNum, falls back to Type

	counts := m.MessageTypeCounts()

	if counts["TEXT_MESSAGE_APP"] != 2 {
		t.Fatalf("expected TEXT_MESSAGE_APP=2, got %d", counts["TEXT_MESSAGE_APP"])
	}
	if counts["POSITION_APP"] != 1 {
		t.Fatalf("expected POSITION_APP=1, got %d", counts["POSITION_APP"])
	}
	if counts["config"] != 1 {
		t.Fatalf("expected config=1, got %d", counts["config"])
	}

	// Ensure it returns a copy
	counts["TEXT_MESSAGE_APP"] = 999
	counts2 := m.MessageTypeCounts()
	if counts2["TEXT_MESSAGE_APP"] != 2 {
		t.Fatal("MessageTypeCounts did not return a copy")
	}
}

func TestTrafficSeries(t *testing.T) {
	m := New(10, 300)

	// Initially empty
	series := m.TrafficSeries()
	if len(series) != 0 {
		t.Fatalf("expected empty traffic series, got %d", len(series))
	}

	// Simulate traffic and sample
	m.FramesFromNode.Store(10)
	m.FramesToNode.Store(5)
	m.BytesFromNode.Store(1000)
	m.BytesToNode.Store(500)

	// Initialize last counters and manually sample
	m.lastFramesIn = 0
	m.lastFramesOut = 0
	m.lastBytesIn = 0
	m.lastBytesOut = 0
	m.sampleTraffic()

	series = m.TrafficSeries()
	if len(series) != 1 {
		t.Fatalf("expected 1 traffic sample, got %d", len(series))
	}

	s := series[0]
	if s.FramesIn != 10 {
		t.Fatalf("expected FramesIn=10, got %d", s.FramesIn)
	}
	if s.FramesOut != 5 {
		t.Fatalf("expected FramesOut=5, got %d", s.FramesOut)
	}
	if s.BytesIn != 1000 {
		t.Fatalf("expected BytesIn=1000, got %d", s.BytesIn)
	}
	if s.BytesOut != 500 {
		t.Fatalf("expected BytesOut=500, got %d", s.BytesOut)
	}
	if s.Timestamp.IsZero() {
		t.Fatal("expected non-zero timestamp in traffic sample")
	}

	// Ensure it returns a copy
	series[0].FramesIn = 999
	series2 := m.TrafficSeries()
	if series2[0].FramesIn != 10 {
		t.Fatal("TrafficSeries did not return a copy")
	}
}

func TestTrafficSeriesRingBuffer(t *testing.T) {
	m := New(10, 300)
	m.maxSamples = 3 // override for test

	// Sample 5 times
	for i := 0; i < 5; i++ {
		m.FramesFromNode.Add(1)
		m.sampleTraffic()
	}

	series := m.TrafficSeries()
	if len(series) != 3 {
		t.Fatalf("expected 3 samples (ring buffer), got %d", len(series))
	}
}

func TestStartSampler(t *testing.T) {
	m := New(10, 300)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	m.FramesFromNode.Store(5)
	m.StartSampler(ctx)

	// Wait for at least one sample
	time.Sleep(1200 * time.Millisecond)

	series := m.TrafficSeries()
	if len(series) == 0 {
		t.Fatal("expected at least one traffic sample from sampler")
	}
}

func TestSubscribeUnsubscribe(t *testing.T) {
	m := New(10, 300)

	ch := m.Subscribe()

	// Publish a message event
	m.RecordMessage(MessageRecord{
		Direction: "from_node",
		Type:      "packet",
		Size:      50,
	})

	// Should receive the event
	select {
	case evt := <-ch:
		if evt.Type != "message" {
			t.Fatalf("expected event type 'message', got %q", evt.Type)
		}
		msg, ok := evt.Data.(MessageRecord)
		if !ok {
			t.Fatal("expected event data to be MessageRecord")
		}
		if msg.Size != 50 {
			t.Fatalf("expected message size 50, got %d", msg.Size)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("timed out waiting for event")
	}

	m.Unsubscribe(ch)

	// Channel should be closed after unsubscribe
	_, ok := <-ch
	if ok {
		t.Fatal("expected channel to be closed after Unsubscribe")
	}
}

func TestSubscribeMultipleListeners(t *testing.T) {
	m := New(10, 300)

	ch1 := m.Subscribe()
	ch2 := m.Subscribe()

	m.RecordMessage(MessageRecord{
		Direction: "to_node",
		Type:      "want_config",
		Size:      20,
	})

	// Both should receive the event
	for i, ch := range []chan Event{ch1, ch2} {
		select {
		case evt := <-ch:
			if evt.Type != "message" {
				t.Fatalf("subscriber %d: expected type 'message', got %q", i, evt.Type)
			}
		case <-time.After(100 * time.Millisecond):
			t.Fatalf("subscriber %d: timed out waiting for event", i)
		}
	}

	m.Unsubscribe(ch1)
	m.Unsubscribe(ch2)
}

func TestPublishDropsSlowSubscriber(t *testing.T) {
	m := New(10, 300)

	ch := m.Subscribe()

	// Fill the channel buffer (64 capacity)
	for i := 0; i < 70; i++ {
		m.RecordMessage(MessageRecord{
			Direction: "from_node",
			Type:      "packet",
			Size:      i,
		})
	}

	// Drain what we can — should be at most 64
	count := 0
	for {
		select {
		case <-ch:
			count++
		default:
			goto done
		}
	}
done:
	if count > 64 {
		t.Fatalf("expected at most 64 events (buffer size), got %d", count)
	}

	m.Unsubscribe(ch)
}

// ---------------------------------------------------------------------------
// Node directory tests
// ---------------------------------------------------------------------------

func TestSetNodeDirectory(t *testing.T) {
	m := New(10, 300)

	dir := map[uint32]NodeEntry{
		0x12345678: {ShortName: "BN", LongName: "Base Node"},
		0xAABBCCDD: {ShortName: "RN", LongName: "Remote Node"},
	}

	m.SetNodeDirectory(dir)

	got := m.NodeDirectory()
	if len(got) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(got))
	}
	if got[0x12345678].ShortName != "BN" {
		t.Errorf("short_name = %q, want %q", got[0x12345678].ShortName, "BN")
	}
	if got[0xAABBCCDD].LongName != "Remote Node" {
		t.Errorf("long_name = %q, want %q", got[0xAABBCCDD].LongName, "Remote Node")
	}
}

func TestNodeDirectory_ReturnsCopy(t *testing.T) {
	m := New(10, 300)
	m.SetNodeDirectory(map[uint32]NodeEntry{
		0x11: {ShortName: "A", LongName: "Alpha"},
	})

	dir := m.NodeDirectory()
	dir[0x22] = NodeEntry{ShortName: "B", LongName: "Beta"}

	// Original should not be affected
	original := m.NodeDirectory()
	if len(original) != 1 {
		t.Fatalf("expected 1 entry in original, got %d", len(original))
	}
}

func TestNodeDirectory_Empty(t *testing.T) {
	m := New(10, 300)
	dir := m.NodeDirectory()
	if dir == nil {
		t.Fatal("expected non-nil empty map")
	}
	if len(dir) != 0 {
		t.Fatalf("expected 0 entries, got %d", len(dir))
	}
}

func TestResolveRelay(t *testing.T) {
	m := New(10, 300)
	m.SetNodeDirectory(map[uint32]NodeEntry{
		0x123456AB: {ShortName: "N1", LongName: "Node One"},
		0xDEADBEAB: {ShortName: "N2", LongName: "Node Two"},
		0x000000FF: {ShortName: "N3", LongName: "Node Three"},
	})

	// Both 0x123456AB and 0xDEADBEAB have last byte 0xAB
	entries := m.ResolveRelay(0xAB)
	if len(entries) != 2 {
		t.Fatalf("expected 2 matches for 0xAB, got %d", len(entries))
	}
	names := map[string]bool{}
	for _, e := range entries {
		names[e.ShortName] = true
	}
	if !names["N1"] || !names["N2"] {
		t.Errorf("expected N1 and N2, got %v", names)
	}

	// 0xFF should match only N3
	entries = m.ResolveRelay(0xFF)
	if len(entries) != 1 {
		t.Fatalf("expected 1 match for 0xFF, got %d", len(entries))
	}
	if entries[0].ShortName != "N3" {
		t.Errorf("short_name = %q, want %q", entries[0].ShortName, "N3")
	}

	// No match
	entries = m.ResolveRelay(0x01)
	if len(entries) != 0 {
		t.Fatalf("expected 0 matches for 0x01, got %d", len(entries))
	}
}

func TestSetNodeDirectory_PublishesEvent(t *testing.T) {
	m := New(10, 300)

	ch := m.Subscribe()
	defer m.Unsubscribe(ch)

	dir := map[uint32]NodeEntry{
		0x11: {ShortName: "A", LongName: "Alpha"},
	}
	m.SetNodeDirectory(dir)

	select {
	case evt := <-ch:
		if evt.Type != "node_directory" {
			t.Fatalf("expected event type 'node_directory', got %q", evt.Type)
		}
		data, ok := evt.Data.(map[uint32]NodeEntry)
		if !ok {
			t.Fatal("expected event data to be map[uint32]NodeEntry")
		}
		if len(data) != 1 {
			t.Fatalf("expected 1 entry in event data, got %d", len(data))
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("timed out waiting for node_directory event")
	}
}

func TestSetNodeDirectory_ReplacesOld(t *testing.T) {
	m := New(10, 300)

	m.SetNodeDirectory(map[uint32]NodeEntry{
		0x11: {ShortName: "A", LongName: "Alpha"},
		0x22: {ShortName: "B", LongName: "Beta"},
	})

	m.SetNodeDirectory(map[uint32]NodeEntry{
		0x33: {ShortName: "C", LongName: "Charlie"},
	})

	dir := m.NodeDirectory()
	if len(dir) != 1 {
		t.Fatalf("expected 1 entry after replace, got %d", len(dir))
	}
	if _, ok := dir[0x33]; !ok {
		t.Fatal("missing entry for 0x33")
	}
}

func TestSnapshotIncludesNodeDir(t *testing.T) {
	m := New(10, 300)
	m.SetNodeDirectory(map[uint32]NodeEntry{
		0xAA: {ShortName: "X", LongName: "X-Ray"},
	})

	snap := m.Snapshot()
	if len(snap.NodeDir) != 1 {
		t.Fatalf("expected 1 entry in snapshot NodeDir, got %d", len(snap.NodeDir))
	}
	if snap.NodeDir[0xAA].ShortName != "X" {
		t.Errorf("short_name = %q, want %q", snap.NodeDir[0xAA].ShortName, "X")
	}
}

// ---------------------------------------------------------------------------
// UpdateNodePosition tests
// ---------------------------------------------------------------------------

func TestUpdateNodePosition_ExistingNode(t *testing.T) {
	m := New(10, 300)

	m.SetNodeDirectory(map[uint32]NodeEntry{
		0x12345678: {ShortName: "BN", LongName: "Base Node"},
	})

	ch := m.Subscribe()
	defer m.Unsubscribe(ch)

	m.UpdateNodePosition(PositionUpdate{
		NodeNum:   0x12345678,
		Latitude:  55.7558,
		Longitude: 37.6173,
		Altitude:  150,
	})

	dir := m.NodeDirectory()
	entry := dir[0x12345678]
	if entry.ShortName != "BN" {
		t.Errorf("short_name = %q, want %q", entry.ShortName, "BN")
	}
	if entry.Latitude != 55.7558 {
		t.Errorf("latitude = %f, want 55.7558", entry.Latitude)
	}
	if entry.Longitude != 37.6173 {
		t.Errorf("longitude = %f, want 37.6173", entry.Longitude)
	}
	if entry.Altitude != 150 {
		t.Errorf("altitude = %d, want 150", entry.Altitude)
	}
	if !entry.HasPosition() {
		t.Error("HasPosition() should return true")
	}

	// Verify SSE event was published
	select {
	case evt := <-ch:
		if evt.Type != "node_position_update" {
			t.Fatalf("expected event type 'node_position_update', got %q", evt.Type)
		}
		pos, ok := evt.Data.(PositionUpdate)
		if !ok {
			t.Fatal("expected event data to be PositionUpdate")
		}
		if pos.NodeNum != 0x12345678 {
			t.Errorf("NodeNum = %#x, want %#x", pos.NodeNum, uint32(0x12345678))
		}
		if pos.Latitude != 55.7558 {
			t.Errorf("Latitude = %f, want 55.7558", pos.Latitude)
		}
		if pos.ShortName != "BN" {
			t.Errorf("ShortName = %q, want %q", pos.ShortName, "BN")
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("timed out waiting for node_position_update event")
	}
}

func TestUpdateNodePosition_UnknownNode(t *testing.T) {
	m := New(10, 300)

	// No SetNodeDirectory call — nodeDir is nil initially
	m.UpdateNodePosition(PositionUpdate{
		NodeNum:   0xAABBCCDD,
		Latitude:  40.7128,
		Longitude: -74.0060,
		Altitude:  10,
	})

	dir := m.NodeDirectory()
	entry := dir[0xAABBCCDD]
	if entry.Latitude != 40.7128 {
		t.Errorf("latitude = %f, want 40.7128", entry.Latitude)
	}
	if entry.Longitude != -74.0060 {
		t.Errorf("longitude = %f, want -74.0060", entry.Longitude)
	}
	// ShortName should be empty for unknown node
	if entry.ShortName != "" {
		t.Errorf("short_name should be empty, got %q", entry.ShortName)
	}
}

func TestUpdateNodePosition_OverwritesPrevious(t *testing.T) {
	m := New(10, 300)
	m.SetNodeDirectory(map[uint32]NodeEntry{
		0x11: {ShortName: "A", LongName: "Alpha", Latitude: 10, Longitude: 20, Altitude: 100},
	})

	m.UpdateNodePosition(PositionUpdate{
		NodeNum:   0x11,
		Latitude:  30,
		Longitude: 40,
		Altitude:  200,
	})

	dir := m.NodeDirectory()
	entry := dir[0x11]
	if entry.Latitude != 30 {
		t.Errorf("latitude = %f, want 30", entry.Latitude)
	}
	if entry.Longitude != 40 {
		t.Errorf("longitude = %f, want 40", entry.Longitude)
	}
	if entry.Altitude != 200 {
		t.Errorf("altitude = %d, want 200", entry.Altitude)
	}
	// Name should be preserved
	if entry.ShortName != "A" {
		t.Errorf("short_name = %q, want %q", entry.ShortName, "A")
	}
}

func TestNodeEntry_HasPosition(t *testing.T) {
	tests := []struct {
		name  string
		entry NodeEntry
		want  bool
	}{
		{"zero", NodeEntry{}, false},
		{"lat only", NodeEntry{Latitude: 1.0}, true},
		{"lon only", NodeEntry{Longitude: 1.0}, true},
		{"both", NodeEntry{Latitude: 55.0, Longitude: 37.0}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.entry.HasPosition(); got != tt.want {
				t.Errorf("HasPosition() = %v, want %v", got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// UpdateNodePosition extended fields tests
// ---------------------------------------------------------------------------

func TestUpdateNodePosition_ExtendedFields(t *testing.T) {
	m := New(10, 300)
	m.SetNodeDirectory(map[uint32]NodeEntry{
		0x11: {ShortName: "M", LongName: "Mobile"},
	})

	ch := m.Subscribe()
	defer m.Unsubscribe(ch)

	m.UpdateNodePosition(PositionUpdate{
		NodeNum:      0x11,
		Latitude:     55.7,
		Longitude:    37.6,
		Altitude:     200,
		GroundSpeed:  5,
		GroundTrack:  180,
		SatsInView:   12,
		PositionTime: 1700000000,
	})

	dir := m.NodeDirectory()
	entry := dir[0x11]
	if entry.GroundSpeed != 5 {
		t.Errorf("ground_speed = %d, want 5", entry.GroundSpeed)
	}
	if entry.GroundTrack != 180 {
		t.Errorf("ground_track = %d, want 180", entry.GroundTrack)
	}
	if entry.SatsInView != 12 {
		t.Errorf("sats_in_view = %d, want 12", entry.SatsInView)
	}
	if entry.PositionTime != 1700000000 {
		t.Errorf("position_time = %d, want 1700000000", entry.PositionTime)
	}

	// Verify SSE event includes extended fields
	select {
	case evt := <-ch:
		pos := evt.Data.(PositionUpdate)
		if pos.GroundSpeed != 5 {
			t.Errorf("event GroundSpeed = %d, want 5", pos.GroundSpeed)
		}
		if pos.SatsInView != 12 {
			t.Errorf("event SatsInView = %d, want 12", pos.SatsInView)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("timed out waiting for event")
	}
}

func TestUpdateNodePosition_ZeroExtendedFieldsPreserved(t *testing.T) {
	m := New(10, 300)
	m.SetNodeDirectory(map[uint32]NodeEntry{
		0x11: {
			ShortName:   "M",
			LongName:    "Mobile",
			GroundSpeed: 10,
			SatsInView:  8,
		},
	})

	// Update with zero extended fields — should NOT overwrite existing values
	m.UpdateNodePosition(PositionUpdate{
		NodeNum:   0x11,
		Latitude:  55.7,
		Longitude: 37.6,
		Altitude:  200,
		// GroundSpeed, SatsInView etc. are zero
	})

	dir := m.NodeDirectory()
	entry := dir[0x11]
	if entry.GroundSpeed != 10 {
		t.Errorf("ground_speed = %d, want 10 (preserved)", entry.GroundSpeed)
	}
	if entry.SatsInView != 8 {
		t.Errorf("sats_in_view = %d, want 8 (preserved)", entry.SatsInView)
	}
}

// ---------------------------------------------------------------------------
// UpdateNodeTelemetry tests
// ---------------------------------------------------------------------------

func TestUpdateNodeTelemetry_DeviceMetrics(t *testing.T) {
	m := New(10, 300)
	m.SetNodeDirectory(map[uint32]NodeEntry{
		0xAA: {ShortName: "BN", LongName: "Base Node"},
	})

	ch := m.Subscribe()
	defer m.Unsubscribe(ch)

	m.UpdateNodeTelemetry(TelemetryUpdate{
		NodeNum:            0xAA,
		BatteryLevel:       75,
		Voltage:            3.85,
		ChannelUtilization: 12.5,
		AirUtilTx:          2.3,
		UptimeSeconds:      7200,
	})

	dir := m.NodeDirectory()
	entry := dir[0xAA]
	if entry.BatteryLevel != 75 {
		t.Errorf("battery_level = %d, want 75", entry.BatteryLevel)
	}
	if entry.Voltage != 3.85 {
		t.Errorf("voltage = %f, want 3.85", entry.Voltage)
	}
	if entry.ChannelUtilization != 12.5 {
		t.Errorf("channel_utilization = %f, want 12.5", entry.ChannelUtilization)
	}
	if entry.AirUtilTx != 2.3 {
		t.Errorf("air_util_tx = %f, want 2.3", entry.AirUtilTx)
	}
	if entry.UptimeSeconds != 7200 {
		t.Errorf("uptime_seconds = %d, want 7200", entry.UptimeSeconds)
	}
	if entry.ShortName != "BN" {
		t.Errorf("short_name = %q, want %q (preserved)", entry.ShortName, "BN")
	}

	// Verify SSE event
	select {
	case evt := <-ch:
		if evt.Type != "node_telemetry_update" {
			t.Fatalf("expected event type 'node_telemetry_update', got %q", evt.Type)
		}
		tel := evt.Data.(TelemetryUpdate)
		if tel.NodeNum != 0xAA {
			t.Errorf("NodeNum = %#x, want %#x", tel.NodeNum, uint32(0xAA))
		}
		if tel.BatteryLevel != 75 {
			t.Errorf("BatteryLevel = %d, want 75", tel.BatteryLevel)
		}
		if tel.ShortName != "BN" {
			t.Errorf("ShortName = %q, want %q", tel.ShortName, "BN")
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("timed out waiting for node_telemetry_update event")
	}
}

func TestUpdateNodeTelemetry_EnvironmentMetrics(t *testing.T) {
	m := New(10, 300)
	m.SetNodeDirectory(map[uint32]NodeEntry{
		0xBB: {ShortName: "EN", LongName: "Env Node"},
	})

	m.UpdateNodeTelemetry(TelemetryUpdate{
		NodeNum:            0xBB,
		Temperature:        22.5,
		RelativeHumidity:   55.0,
		BarometricPressure: 1013.25,
	})

	dir := m.NodeDirectory()
	entry := dir[0xBB]
	if entry.Temperature != 22.5 {
		t.Errorf("temperature = %f, want 22.5", entry.Temperature)
	}
	if entry.RelativeHumidity != 55.0 {
		t.Errorf("relative_humidity = %f, want 55.0", entry.RelativeHumidity)
	}
	if entry.BarometricPressure != 1013.25 {
		t.Errorf("barometric_pressure = %f, want 1013.25", entry.BarometricPressure)
	}
}

func TestUpdateNodeTelemetry_UnknownNode(t *testing.T) {
	m := New(10, 300)

	// No SetNodeDirectory — nodeDir is nil initially
	m.UpdateNodeTelemetry(TelemetryUpdate{
		NodeNum:      0xCC,
		BatteryLevel: 50,
	})

	dir := m.NodeDirectory()
	entry := dir[0xCC]
	if entry.BatteryLevel != 50 {
		t.Errorf("battery_level = %d, want 50", entry.BatteryLevel)
	}
	if entry.ShortName != "" {
		t.Errorf("short_name should be empty, got %q", entry.ShortName)
	}
}

func TestUpdateNodeTelemetry_ZeroFieldsPreserved(t *testing.T) {
	m := New(10, 300)
	m.SetNodeDirectory(map[uint32]NodeEntry{
		0xDD: {
			ShortName:    "TN",
			LongName:     "Test Node",
			BatteryLevel: 80,
			Voltage:      4.1,
			Temperature:  25.0,
		},
	})

	// Update with only environment metrics — device metrics should be preserved
	m.UpdateNodeTelemetry(TelemetryUpdate{
		NodeNum:            0xDD,
		Temperature:        22.0,
		RelativeHumidity:   60.0,
		BarometricPressure: 1015.0,
	})

	dir := m.NodeDirectory()
	entry := dir[0xDD]
	// Preserved from original
	if entry.BatteryLevel != 80 {
		t.Errorf("battery_level = %d, want 80 (preserved)", entry.BatteryLevel)
	}
	if entry.Voltage != 4.1 {
		t.Errorf("voltage = %f, want 4.1 (preserved)", entry.Voltage)
	}
	// Updated
	if entry.Temperature != 22.0 {
		t.Errorf("temperature = %f, want 22.0", entry.Temperature)
	}
	if entry.RelativeHumidity != 60.0 {
		t.Errorf("relative_humidity = %f, want 60.0", entry.RelativeHumidity)
	}
}

// ---------------------------------------------------------------------------
// UpdateNodeSignal tests
// ---------------------------------------------------------------------------

func TestUpdateNodeSignal_ExistingNode(t *testing.T) {
	m := New(10, 300)
	m.SetNodeDirectory(map[uint32]NodeEntry{
		0xAA: {ShortName: "BN", LongName: "Base Node"},
	})

	ch := m.Subscribe()
	defer m.Unsubscribe(ch)

	m.UpdateNodeSignal(SignalUpdate{
		NodeNum: 0xAA,
		RxRssi:  -87,
		RxSnr:   6.5,
	})

	dir := m.NodeDirectory()
	entry := dir[0xAA]
	if entry.RxRssi != -87 {
		t.Errorf("rx_rssi = %d, want -87", entry.RxRssi)
	}
	if entry.RxSnr != 6.5 {
		t.Errorf("rx_snr = %f, want 6.5", entry.RxSnr)
	}
	if entry.ShortName != "BN" {
		t.Errorf("short_name = %q, want %q (preserved)", entry.ShortName, "BN")
	}

	// Verify SSE event
	select {
	case evt := <-ch:
		if evt.Type != "node_signal_update" {
			t.Fatalf("expected event type 'node_signal_update', got %q", evt.Type)
		}
		sig, ok := evt.Data.(SignalUpdate)
		if !ok {
			t.Fatal("expected event data to be SignalUpdate")
		}
		if sig.NodeNum != 0xAA {
			t.Errorf("NodeNum = %#x, want %#x", sig.NodeNum, uint32(0xAA))
		}
		if sig.RxRssi != -87 {
			t.Errorf("RxRssi = %d, want -87", sig.RxRssi)
		}
		if sig.RxSnr != 6.5 {
			t.Errorf("RxSnr = %f, want 6.5", sig.RxSnr)
		}
		if sig.ShortName != "BN" {
			t.Errorf("ShortName = %q, want %q", sig.ShortName, "BN")
		}
		if sig.LongName != "Base Node" {
			t.Errorf("LongName = %q, want %q", sig.LongName, "Base Node")
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("timed out waiting for node_signal_update event")
	}
}

func TestUpdateNodeSignal_UnknownNode(t *testing.T) {
	m := New(10, 300)

	// No SetNodeDirectory — nodeDir is nil initially
	m.UpdateNodeSignal(SignalUpdate{
		NodeNum: 0xCC,
		RxRssi:  -95,
		RxSnr:   3.0,
	})

	dir := m.NodeDirectory()
	entry := dir[0xCC]
	if entry.RxRssi != -95 {
		t.Errorf("rx_rssi = %d, want -95", entry.RxRssi)
	}
	if entry.RxSnr != 3.0 {
		t.Errorf("rx_snr = %f, want 3.0", entry.RxSnr)
	}
	if entry.ShortName != "" {
		t.Errorf("short_name should be empty, got %q", entry.ShortName)
	}
}

func TestUpdateNodeSignal_ZeroFieldsPreserved(t *testing.T) {
	m := New(10, 300)
	m.SetNodeDirectory(map[uint32]NodeEntry{
		0xDD: {
			ShortName: "SN",
			LongName:  "Signal Node",
			RxRssi:    -80,
			RxSnr:     5.0,
		},
	})

	// Update with only RSSI — SNR should be preserved
	m.UpdateNodeSignal(SignalUpdate{
		NodeNum: 0xDD,
		RxRssi:  -90,
		// RxSnr is zero, should not overwrite
	})

	dir := m.NodeDirectory()
	entry := dir[0xDD]
	if entry.RxRssi != -90 {
		t.Errorf("rx_rssi = %d, want -90", entry.RxRssi)
	}
	if entry.RxSnr != 5.0 {
		t.Errorf("rx_snr = %f, want 5.0 (preserved)", entry.RxSnr)
	}
}

func TestUpdateNodeSignal_OverwritesPrevious(t *testing.T) {
	m := New(10, 300)
	m.SetNodeDirectory(map[uint32]NodeEntry{
		0xEE: {
			ShortName: "ON",
			LongName:  "Old Node",
			RxRssi:    -70,
			RxSnr:     10.0,
		},
	})

	m.UpdateNodeSignal(SignalUpdate{
		NodeNum: 0xEE,
		RxRssi:  -100,
		RxSnr:   -5.0,
	})

	dir := m.NodeDirectory()
	entry := dir[0xEE]
	if entry.RxRssi != -100 {
		t.Errorf("rx_rssi = %d, want -100", entry.RxRssi)
	}
	if entry.RxSnr != -5.0 {
		t.Errorf("rx_snr = %f, want -5.0", entry.RxSnr)
	}
	// Name should be preserved
	if entry.ShortName != "ON" {
		t.Errorf("short_name = %q, want %q", entry.ShortName, "ON")
	}
}

func TestUpdateNodeSignal_PreservesOtherFields(t *testing.T) {
	m := New(10, 300)
	m.SetNodeDirectory(map[uint32]NodeEntry{
		0xFF: {
			ShortName:    "FN",
			LongName:     "Full Node",
			Latitude:     55.7,
			Longitude:    37.6,
			BatteryLevel: 85,
			Voltage:      4.0,
		},
	})

	m.UpdateNodeSignal(SignalUpdate{
		NodeNum: 0xFF,
		RxRssi:  -88,
		RxSnr:   7.5,
	})

	dir := m.NodeDirectory()
	entry := dir[0xFF]
	// Signal fields updated
	if entry.RxRssi != -88 {
		t.Errorf("rx_rssi = %d, want -88", entry.RxRssi)
	}
	if entry.RxSnr != 7.5 {
		t.Errorf("rx_snr = %f, want 7.5", entry.RxSnr)
	}
	// Other fields preserved
	if entry.Latitude != 55.7 {
		t.Errorf("latitude = %f, want 55.7 (preserved)", entry.Latitude)
	}
	if entry.Longitude != 37.6 {
		t.Errorf("longitude = %f, want 37.6 (preserved)", entry.Longitude)
	}
	if entry.BatteryLevel != 85 {
		t.Errorf("battery_level = %d, want 85 (preserved)", entry.BatteryLevel)
	}
}

// ---------------------------------------------------------------------------
// Chat message ring buffer tests
// ---------------------------------------------------------------------------

func TestRecordChatMessage(t *testing.T) {
	m := New(10, 300)

	m.RecordChatMessage(ChatMessage{
		From:      0xAA,
		To:        0xFFFFFFFF,
		Channel:   0,
		Text:      "hello mesh",
		FromName:  "AA",
		ToName:    "Broadcast",
		Direction: "incoming",
	})

	msgs := m.ChatMessages()
	if len(msgs) != 1 {
		t.Fatalf("expected 1 chat message, got %d", len(msgs))
	}
	if msgs[0].Text != "hello mesh" {
		t.Errorf("text = %q, want %q", msgs[0].Text, "hello mesh")
	}
	if msgs[0].Timestamp.IsZero() {
		t.Error("expected non-zero timestamp")
	}
	if msgs[0].From != 0xAA {
		t.Errorf("from = %d, want %d", msgs[0].From, 0xAA)
	}
	if msgs[0].To != 0xFFFFFFFF {
		t.Errorf("to = %d, want %d", msgs[0].To, 0xFFFFFFFF)
	}
}

func TestChatMessageRingBuffer(t *testing.T) {
	m := New(10, 300)
	// maxChatMessages is hardcoded to 500 in New(), so fill it up
	for i := 0; i < 510; i++ {
		m.RecordChatMessage(ChatMessage{
			From: uint32(i),
			Text: "msg",
		})
	}
	msgs := m.ChatMessages()
	if len(msgs) != 500 {
		t.Fatalf("expected 500 chat messages (ring buffer), got %d", len(msgs))
	}
	// First message should be from index 10 (oldest 10 evicted)
	if msgs[0].From != 10 {
		t.Errorf("oldest message from = %d, want 10", msgs[0].From)
	}
}

func TestChatMessages_ReturnsCopy(t *testing.T) {
	m := New(10, 300)
	m.RecordChatMessage(ChatMessage{Text: "test"})

	msgs1 := m.ChatMessages()
	msgs2 := m.ChatMessages()

	msgs1[0].Text = "modified"
	if msgs2[0].Text == "modified" {
		t.Error("ChatMessages should return a copy, not a reference")
	}
}

func TestChatMessages_Empty(t *testing.T) {
	m := New(10, 300)
	msgs := m.ChatMessages()
	if len(msgs) != 0 {
		t.Fatalf("expected 0 chat messages, got %d", len(msgs))
	}
}

func TestRecordChatMessage_PublishesSSE(t *testing.T) {
	m := New(10, 300)
	ch := m.Subscribe()
	defer m.Unsubscribe(ch)

	m.RecordChatMessage(ChatMessage{Text: "sse test"})

	select {
	case evt := <-ch:
		if evt.Type != "chat_message" {
			t.Errorf("event type = %q, want %q", evt.Type, "chat_message")
		}
		msg, ok := evt.Data.(ChatMessage)
		if !ok {
			t.Fatal("event data is not ChatMessage")
		}
		if msg.Text != "sse test" {
			t.Errorf("text = %q, want %q", msg.Text, "sse test")
		}
	case <-time.After(time.Second):
		t.Fatal("did not receive SSE event")
	}
}

// ---------------------------------------------------------------------------
// SeenRealtime tests
// ---------------------------------------------------------------------------

func TestSeenRealtime_SetNodeDirectory_DoesNotSetFlag(t *testing.T) {
	m := New(10, 300)
	m.SetNodeDirectory(map[uint32]NodeEntry{
		0x11: {ShortName: "A", LongName: "Alpha"},
		0x22: {ShortName: "B", LongName: "Beta"},
	})

	dir := m.NodeDirectory()
	for num, entry := range dir {
		if entry.SeenRealtime {
			t.Errorf("node %08x: SeenRealtime should be false after SetNodeDirectory", num)
		}
	}
}

func TestSeenRealtime_UpdateNodePosition(t *testing.T) {
	m := New(10, 300)
	m.SetNodeDirectory(map[uint32]NodeEntry{
		0x11: {ShortName: "A"},
	})

	m.UpdateNodePosition(PositionUpdate{NodeNum: 0x11, Latitude: 55.75, Longitude: 37.62})

	entry := m.NodeDirectory()[0x11]
	if !entry.SeenRealtime {
		t.Error("SeenRealtime should be true after UpdateNodePosition")
	}
}

func TestSeenRealtime_UpdateNodeTelemetry(t *testing.T) {
	m := New(10, 300)
	m.SetNodeDirectory(map[uint32]NodeEntry{
		0x11: {ShortName: "A"},
	})

	m.UpdateNodeTelemetry(TelemetryUpdate{NodeNum: 0x11, BatteryLevel: 85})

	entry := m.NodeDirectory()[0x11]
	if !entry.SeenRealtime {
		t.Error("SeenRealtime should be true after UpdateNodeTelemetry")
	}
}

func TestSeenRealtime_UpdateNodeSignal(t *testing.T) {
	m := New(10, 300)
	m.SetNodeDirectory(map[uint32]NodeEntry{
		0x11: {ShortName: "A"},
	})

	m.UpdateNodeSignal(SignalUpdate{NodeNum: 0x11, RxRssi: -95, RxSnr: 6.5})

	entry := m.NodeDirectory()[0x11]
	if !entry.SeenRealtime {
		t.Error("SeenRealtime should be true after UpdateNodeSignal")
	}
}

func TestSeenRealtime_UpsertNode(t *testing.T) {
	m := New(10, 300)
	m.SetNodeDirectory(map[uint32]NodeEntry{
		0x11: {ShortName: "A"},
	})

	m.UpsertNode(NodeInfoUpdate{NodeNum: 0x11, ShortName: "A2"})

	entry := m.NodeDirectory()[0x11]
	if !entry.SeenRealtime {
		t.Error("SeenRealtime should be true after UpsertNode")
	}
}

func TestSeenRealtime_NewNodeViaUpsert(t *testing.T) {
	m := New(10, 300)

	// No SetNodeDirectory — brand new node discovered in real time
	m.UpsertNode(NodeInfoUpdate{NodeNum: 0x99, ShortName: "New"})

	entry := m.NodeDirectory()[0x99]
	if !entry.SeenRealtime {
		t.Error("SeenRealtime should be true for newly discovered node via UpsertNode")
	}
}

func TestSeenRealtime_PreservedAcrossUpdates(t *testing.T) {
	m := New(10, 300)
	m.SetNodeDirectory(map[uint32]NodeEntry{
		0x11: {ShortName: "A"},
		0x22: {ShortName: "B"},
	})

	// Only node 0x11 sends a real-time packet
	m.UpdateNodeSignal(SignalUpdate{NodeNum: 0x11, RxRssi: -90})

	dir := m.NodeDirectory()
	if !dir[0x11].SeenRealtime {
		t.Error("node 0x11 should be SeenRealtime=true")
	}
	if dir[0x22].SeenRealtime {
		t.Error("node 0x22 should remain SeenRealtime=false")
	}
}

// ── LastHeard is refreshed on real-time updates ──

func TestLastHeard_UpdateNodePosition(t *testing.T) {
	m := New(10, 300)
	m.SetNodeDirectory(map[uint32]NodeEntry{
		0x11: {ShortName: "A", LastHeard: 1000},
	})

	m.UpdateNodePosition(PositionUpdate{NodeNum: 0x11, Latitude: 55.75, Longitude: 37.62})

	entry := m.NodeDirectory()[0x11]
	if entry.LastHeard <= 1000 {
		t.Errorf("LastHeard should be refreshed to ~now, got %d", entry.LastHeard)
	}
}

func TestLastHeard_UpdateNodeTelemetry(t *testing.T) {
	m := New(10, 300)
	m.SetNodeDirectory(map[uint32]NodeEntry{
		0x11: {ShortName: "A", LastHeard: 1000},
	})

	m.UpdateNodeTelemetry(TelemetryUpdate{NodeNum: 0x11, BatteryLevel: 85})

	entry := m.NodeDirectory()[0x11]
	if entry.LastHeard <= 1000 {
		t.Errorf("LastHeard should be refreshed to ~now, got %d", entry.LastHeard)
	}
}

func TestLastHeard_UpdateNodeSignal(t *testing.T) {
	m := New(10, 300)
	m.SetNodeDirectory(map[uint32]NodeEntry{
		0x11: {ShortName: "A", LastHeard: 1000},
	})

	m.UpdateNodeSignal(SignalUpdate{NodeNum: 0x11, RxRssi: -95, RxSnr: 6.5})

	entry := m.NodeDirectory()[0x11]
	if entry.LastHeard <= 1000 {
		t.Errorf("LastHeard should be refreshed to ~now, got %d", entry.LastHeard)
	}
}

func TestLastHeard_UpsertNode(t *testing.T) {
	m := New(10, 300)
	m.SetNodeDirectory(map[uint32]NodeEntry{
		0x11: {ShortName: "A", LastHeard: 1000},
	})

	m.UpsertNode(NodeInfoUpdate{NodeNum: 0x11, ShortName: "A2"})

	entry := m.NodeDirectory()[0x11]
	if entry.LastHeard <= 1000 {
		t.Errorf("LastHeard should be refreshed to ~now, got %d", entry.LastHeard)
	}
}

func TestLastHeard_NotUpdatedBySetNodeDirectory(t *testing.T) {
	m := New(10, 300)
	m.SetNodeDirectory(map[uint32]NodeEntry{
		0x11: {ShortName: "A", LastHeard: 1000},
	})

	entry := m.NodeDirectory()[0x11]
	if entry.LastHeard != 1000 {
		t.Errorf("SetNodeDirectory should preserve original LastHeard, got %d", entry.LastHeard)
	}
}

// ---------------------------------------------------------------------------
// Traceroute history tests
// ---------------------------------------------------------------------------

func TestTraceHistory_EmptyByDefault(t *testing.T) {
	m := New(10, 300)
	h := m.TraceHistory()
	if len(h) != 0 {
		t.Errorf("expected empty trace history, got %d entries", len(h))
	}
}

func TestPublishTraceroute_StoresInHistory(t *testing.T) {
	m := New(10, 300)

	m.PublishTraceroute(TracerouteUpdate{
		From:       0xAABBCCDD,
		To:         0x11223344,
		Route:      []uint32{0x55667788},
		SnrTowards: []int32{24}, // 6.0 dB (scaled by 4)
	})

	h := m.TraceHistory()
	if len(h) != 1 {
		t.Fatalf("expected 1 trace entry, got %d", len(h))
	}
	if h[0].From != 0xAABBCCDD {
		t.Errorf("From = %d, want %d", h[0].From, uint32(0xAABBCCDD))
	}
	if h[0].To != 0x11223344 {
		t.Errorf("To = %d, want %d", h[0].To, uint32(0x11223344))
	}
	if len(h[0].Route) != 1 || h[0].Route[0] != 0x55667788 {
		t.Errorf("Route = %v, want [0x55667788]", h[0].Route)
	}
	if len(h[0].SnrTowards) != 1 || h[0].SnrTowards[0] != 24 {
		t.Errorf("SnrTowards = %v, want [24]", h[0].SnrTowards)
	}
	if h[0].Timestamp.IsZero() {
		t.Error("expected non-zero timestamp")
	}
}

func TestPublishTraceroute_MultipleEntries(t *testing.T) {
	m := New(10, 300)

	for i := uint32(0); i < 5; i++ {
		m.PublishTraceroute(TracerouteUpdate{From: i + 1, To: 1})
	}

	h := m.TraceHistory()
	if len(h) != 5 {
		t.Fatalf("expected 5 trace entries, got %d", len(h))
	}
	// Verify order (oldest first)
	for i := 0; i < 5; i++ {
		if h[i].From != uint32(i+1) {
			t.Errorf("h[%d].From = %d, want %d", i, h[i].From, i+1)
		}
	}
}

func TestTraceHistory_RingBufferOverflow(t *testing.T) {
	m := New(10, 300)
	// maxTraceHistory = 100

	for i := uint32(0); i < 110; i++ {
		m.PublishTraceroute(TracerouteUpdate{From: i, To: 1})
	}

	h := m.TraceHistory()
	if len(h) != 100 {
		t.Fatalf("expected 100 trace entries (ring buffer limit), got %d", len(h))
	}
	// Oldest entry should be #10 (first 10 were evicted)
	if h[0].From != 10 {
		t.Errorf("oldest entry From = %d, want 10", h[0].From)
	}
	// Newest entry should be #109
	if h[99].From != 109 {
		t.Errorf("newest entry From = %d, want 109", h[99].From)
	}
}

func TestTraceHistory_ReturnsCopy(t *testing.T) {
	m := New(10, 300)
	m.PublishTraceroute(TracerouteUpdate{From: 1, To: 2})

	h1 := m.TraceHistory()
	h1[0].From = 999

	h2 := m.TraceHistory()
	if h2[0].From != 1 {
		t.Error("TraceHistory should return a copy, but modification was visible")
	}
}

func TestPublishTraceroute_SSEEvent(t *testing.T) {
	m := New(10, 300)
	ch := m.Subscribe()
	defer m.Unsubscribe(ch)

	m.PublishTraceroute(TracerouteUpdate{From: 0xAA, To: 0xBB})

	select {
	case evt := <-ch:
		if evt.Type != "traceroute_result" {
			t.Errorf("event type = %q, want %q", evt.Type, "traceroute_result")
		}
		entry, ok := evt.Data.(TracerouteEntry)
		if !ok {
			t.Fatalf("expected TracerouteEntry, got %T", evt.Data)
		}
		if entry.From != 0xAA {
			t.Errorf("From = %d, want %d", entry.From, 0xAA)
		}
	case <-time.After(time.Second):
		t.Error("timed out waiting for traceroute_result SSE event")
	}
}

// ---------------------------------------------------------------------------
// SetFavorite tests
// ---------------------------------------------------------------------------

func TestSetFavorite_Success(t *testing.T) {
	m := New(10, 300)
	m.SetNodeDirectory(map[uint32]NodeEntry{
		0x11: {ShortName: "A", IsFavorite: false},
	})

	ok := m.SetFavorite(0x11, true)
	if !ok {
		t.Error("SetFavorite returned false, expected true")
	}

	entry := m.NodeDirectory()[0x11]
	if !entry.IsFavorite {
		t.Error("expected IsFavorite=true after SetFavorite(true)")
	}

	// Toggle off
	ok = m.SetFavorite(0x11, false)
	if !ok {
		t.Error("SetFavorite returned false, expected true")
	}

	entry = m.NodeDirectory()[0x11]
	if entry.IsFavorite {
		t.Error("expected IsFavorite=false after SetFavorite(false)")
	}
}

func TestSetFavorite_NodeNotFound(t *testing.T) {
	m := New(10, 300)
	m.SetNodeDirectory(map[uint32]NodeEntry{
		0x11: {ShortName: "A"},
	})

	ok := m.SetFavorite(0x99, true)
	if ok {
		t.Error("SetFavorite returned true for non-existent node, expected false")
	}
}

func TestSetFavorite_PublishesSSE(t *testing.T) {
	m := New(10, 300)

	ch := m.Subscribe()
	defer m.Unsubscribe(ch)

	m.SetNodeDirectory(map[uint32]NodeEntry{
		0x11: {ShortName: "A"},
	})

	// Drain node_directory event from SetNodeDirectory
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for node_directory event")
	}

	m.SetFavorite(0x11, true)

	select {
	case evt := <-ch:
		if evt.Type != "node_favorite" {
			t.Errorf("event type = %q, want %q", evt.Type, "node_favorite")
		}
	case <-time.After(time.Second):
		t.Error("timed out waiting for node_favorite SSE event")
	}
}
