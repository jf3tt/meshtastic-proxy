package metrics

import (
	"context"
	"testing"
	"time"
)

func TestNewMetrics(t *testing.T) {
	m := New(10)
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
	m := New(0)
	if m.maxMessages != 100 {
		t.Fatalf("expected default maxMessages=100, got %d", m.maxMessages)
	}
}

func TestUptime(t *testing.T) {
	m := New(10)
	time.Sleep(10 * time.Millisecond)
	uptime := m.Uptime()
	if uptime < 10*time.Millisecond {
		t.Fatalf("uptime too small: %v", uptime)
	}
}

func TestRecordAndRetrieveMessages(t *testing.T) {
	m := New(5)

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
	m := New(3)

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
	m := New(10)

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
	m := New(10)
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
	m := New(10)

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
	m := New(100)

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
	m := New(10)

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
	m := New(10)
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
	m := New(10)
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
	m := New(10)

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
	m := New(10)

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
	m := New(10)

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
