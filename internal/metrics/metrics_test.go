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
