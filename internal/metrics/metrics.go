package metrics

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// MessageRecord stores information about a proxied message.
type MessageRecord struct {
	Timestamp time.Time `json:"timestamp"`
	Direction string    `json:"direction"` // "from_node" or "to_node"
	Type      string    `json:"type"`      // protobuf message type
	Size      int       `json:"size"`      // payload size in bytes
	Client    string    `json:"client"`    // client address (for to_node direction)
	PortNum   string    `json:"port_num"`  // e.g. "TEXT_MESSAGE_APP", "POSITION_APP"
	From      uint32    `json:"from"`      // source node number
	To        uint32    `json:"to"`        // destination node number
	Channel   uint32    `json:"channel"`   // channel index
	Payload   string    `json:"payload"`   // decoded human-readable payload
}

// TrafficSample is a single point in the traffic time-series.
type TrafficSample struct {
	Timestamp time.Time `json:"ts"`
	FramesIn  int64     `json:"fi"`
	FramesOut int64     `json:"fo"`
	BytesIn   int64     `json:"bi"`
	BytesOut  int64     `json:"bo"`
}

// Event is sent to SSE subscribers.
type Event struct {
	Type string      `json:"type"` // "metrics" or "message"
	Data interface{} `json:"data"`
}

// Metrics collects runtime statistics about the proxy.
type Metrics struct {
	startTime time.Time

	// Connection state
	NodeConnected atomic.Bool
	NodeAddress   string

	// Client tracking
	ActiveClients atomic.Int64

	// Traffic counters
	BytesFromNode  atomic.Int64
	BytesToNode    atomic.Int64
	FramesFromNode atomic.Int64
	FramesToNode   atomic.Int64

	// Message log (ring buffer)
	mu          sync.Mutex
	messages    []MessageRecord
	maxMessages int

	// Traffic time-series (ring buffer)
	trafficMu      sync.Mutex
	trafficSamples []TrafficSample
	maxSamples     int
	lastFramesIn   int64
	lastFramesOut  int64
	lastBytesIn    int64
	lastBytesOut   int64

	// Message type counters for doughnut chart
	typeMu     sync.Mutex
	typeCounts map[string]int64

	// Pub/sub for SSE
	subMu       sync.RWMutex
	subscribers map[chan Event]struct{}
}

// New creates a new Metrics instance.
func New(maxMessages int) *Metrics {
	if maxMessages <= 0 {
		maxMessages = 100
	}
	return &Metrics{
		startTime:      time.Now(),
		messages:       make([]MessageRecord, 0, maxMessages),
		maxMessages:    maxMessages,
		trafficSamples: make([]TrafficSample, 0, 300),
		maxSamples:     300, // 5 minutes at 1s interval
		typeCounts:     make(map[string]int64),
		subscribers:    make(map[chan Event]struct{}),
	}
}

// StartSampler starts a goroutine that samples traffic counters every second.
func (m *Metrics) StartSampler(ctx context.Context) {
	m.lastFramesIn = m.FramesFromNode.Load()
	m.lastFramesOut = m.FramesToNode.Load()
	m.lastBytesIn = m.BytesFromNode.Load()
	m.lastBytesOut = m.BytesToNode.Load()

	ticker := time.NewTicker(1 * time.Second)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				m.sampleTraffic()
			}
		}
	}()
}

func (m *Metrics) sampleTraffic() {
	fi := m.FramesFromNode.Load()
	fo := m.FramesToNode.Load()
	bi := m.BytesFromNode.Load()
	bo := m.BytesToNode.Load()

	sample := TrafficSample{
		Timestamp: time.Now(),
		FramesIn:  fi - m.lastFramesIn,
		FramesOut: fo - m.lastFramesOut,
		BytesIn:   bi - m.lastBytesIn,
		BytesOut:  bo - m.lastBytesOut,
	}

	m.lastFramesIn = fi
	m.lastFramesOut = fo
	m.lastBytesIn = bi
	m.lastBytesOut = bo

	m.trafficMu.Lock()
	if len(m.trafficSamples) >= m.maxSamples {
		copy(m.trafficSamples, m.trafficSamples[1:])
		m.trafficSamples = m.trafficSamples[:len(m.trafficSamples)-1]
	}
	m.trafficSamples = append(m.trafficSamples, sample)
	m.trafficMu.Unlock()

	// Publish metrics event
	m.publish(Event{Type: "metrics", Data: m.Snapshot()})
}

// Uptime returns the duration since the proxy started.
func (m *Metrics) Uptime() time.Duration {
	return time.Since(m.startTime)
}

// RecordMessage adds a message to the log.
func (m *Metrics) RecordMessage(rec MessageRecord) {
	rec.Timestamp = time.Now()

	// Update type counters
	key := rec.Type
	if rec.PortNum != "" {
		key = rec.PortNum
	}
	m.typeMu.Lock()
	m.typeCounts[key]++
	m.typeMu.Unlock()

	m.mu.Lock()
	if len(m.messages) >= m.maxMessages {
		copy(m.messages, m.messages[1:])
		m.messages = m.messages[:len(m.messages)-1]
	}
	m.messages = append(m.messages, rec)
	m.mu.Unlock()

	// Publish message event
	m.publish(Event{Type: "message", Data: rec})
}

// Messages returns a copy of the recent message log.
func (m *Metrics) Messages() []MessageRecord {
	m.mu.Lock()
	defer m.mu.Unlock()

	result := make([]MessageRecord, len(m.messages))
	copy(result, m.messages)
	return result
}

// TrafficSeries returns a copy of the traffic time-series.
func (m *Metrics) TrafficSeries() []TrafficSample {
	m.trafficMu.Lock()
	defer m.trafficMu.Unlock()

	result := make([]TrafficSample, len(m.trafficSamples))
	copy(result, m.trafficSamples)
	return result
}

// MessageTypeCounts returns message type distribution.
func (m *Metrics) MessageTypeCounts() map[string]int64 {
	m.typeMu.Lock()
	defer m.typeMu.Unlock()

	result := make(map[string]int64, len(m.typeCounts))
	for k, v := range m.typeCounts {
		result[k] = v
	}
	return result
}

// Subscribe returns a channel that receives SSE events.
func (m *Metrics) Subscribe() chan Event {
	ch := make(chan Event, 64)
	m.subMu.Lock()
	m.subscribers[ch] = struct{}{}
	m.subMu.Unlock()
	return ch
}

// Unsubscribe removes a subscriber channel.
func (m *Metrics) Unsubscribe(ch chan Event) {
	m.subMu.Lock()
	delete(m.subscribers, ch)
	m.subMu.Unlock()
	close(ch)
}

func (m *Metrics) publish(evt Event) {
	m.subMu.RLock()
	defer m.subMu.RUnlock()

	for ch := range m.subscribers {
		select {
		case ch <- evt:
		default:
			// Slow subscriber, drop event
		}
	}
}

// Snapshot returns a point-in-time snapshot of all metrics.
type Snapshot struct {
	Uptime            time.Duration    `json:"uptime"`
	UptimeStr         string           `json:"uptime_str"`
	NodeConnected     bool             `json:"node_connected"`
	NodeAddress       string           `json:"node_address"`
	ActiveClients     int64            `json:"active_clients"`
	BytesFromNode     int64            `json:"bytes_from_node"`
	BytesToNode       int64            `json:"bytes_to_node"`
	FramesFromNode    int64            `json:"frames_from_node"`
	FramesToNode      int64            `json:"frames_to_node"`
	Messages          []MessageRecord  `json:"messages"`
	TrafficSeries     []TrafficSample  `json:"traffic_series"`
	MessageTypeCounts map[string]int64 `json:"message_type_counts"`
}

// Snapshot returns a consistent snapshot of the current metrics.
func (m *Metrics) Snapshot() Snapshot {
	uptime := m.Uptime()
	return Snapshot{
		Uptime:            uptime,
		UptimeStr:         formatDuration(uptime),
		NodeConnected:     m.NodeConnected.Load(),
		NodeAddress:       m.NodeAddress,
		ActiveClients:     m.ActiveClients.Load(),
		BytesFromNode:     m.BytesFromNode.Load(),
		BytesToNode:       m.BytesToNode.Load(),
		FramesFromNode:    m.FramesFromNode.Load(),
		FramesToNode:      m.FramesToNode.Load(),
		Messages:          m.Messages(),
		TrafficSeries:     m.TrafficSeries(),
		MessageTypeCounts: m.MessageTypeCounts(),
	}
}

func formatDuration(d time.Duration) string {
	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	minutes := int(d.Minutes()) % 60
	seconds := int(d.Seconds()) % 60

	if days > 0 {
		return fmt.Sprintf("%dd %dh %dm %ds", days, hours, minutes, seconds)
	}
	if hours > 0 {
		return fmt.Sprintf("%dh %dm %ds", hours, minutes, seconds)
	}
	if minutes > 0 {
		return fmt.Sprintf("%dm %ds", minutes, seconds)
	}
	return fmt.Sprintf("%ds", seconds)
}
