package metrics

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// Default buffer sizes for ring buffers.
const (
	defaultMaxMessages       = 100
	defaultMaxTrafficSamples = 300
	defaultMaxChatMessages   = 500
	defaultMaxTraceHistory   = 100
)

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

	// Node connection counters
	NodeReconnects       atomic.Int64
	NodeConnectionErrors atomic.Int64

	// Config cache status
	ConfigCacheFrames atomic.Int64

	// configCacheUpdatedAt is the time the config cache was last populated.
	configCacheMu        sync.RWMutex
	configCacheUpdatedAt time.Time

	// Config cache replay counters (by request type)
	ConfigReplaysFull       atomic.Int64
	ConfigReplaysConfigOnly atomic.Int64
	ConfigReplaysNodesOnly  atomic.Int64

	// Message log (ring buffer)
	mu          sync.RWMutex
	messages    []MessageRecord
	maxMessages int

	// Chat message log (ring buffer, TEXT_MESSAGE_APP only)
	chatMu          sync.RWMutex
	chatMessages    []ChatMessage
	maxChatMessages int

	// Traceroute history (ring buffer)
	traceMu         sync.RWMutex
	traceHistory    []TracerouteEntry
	maxTraceHistory int

	// Traffic time-series (ring buffer)
	trafficMu      sync.RWMutex
	trafficSamples []TrafficSample
	maxSamples     int
	lastFramesIn   int64
	lastFramesOut  int64
	lastBytesIn    int64
	lastBytesOut   int64

	// Message type counters for doughnut chart
	typeMu     sync.RWMutex
	typeCounts map[string]int64

	// Node directory (populated from config cache NodeInfo frames)
	nodeDirMu sync.RWMutex
	nodeDir   map[uint32]NodeEntry // full node number → identity
	relayDir  map[uint8][]uint32   // last byte → list of full node numbers

	// Pub/sub for SSE
	subMu       sync.RWMutex
	subscribers map[chan Event]struct{}
}

// New creates a new Metrics instance.
func New(maxMessages, maxTrafficSamples int) *Metrics {
	if maxMessages <= 0 {
		maxMessages = defaultMaxMessages
	}
	if maxTrafficSamples <= 0 {
		maxTrafficSamples = defaultMaxTrafficSamples
	}
	return &Metrics{
		startTime:       time.Now(),
		messages:        make([]MessageRecord, 0, maxMessages),
		maxMessages:     maxMessages,
		chatMessages:    make([]ChatMessage, 0, defaultMaxChatMessages),
		maxChatMessages: defaultMaxChatMessages,
		traceHistory:    make([]TracerouteEntry, 0, defaultMaxTraceHistory),
		maxTraceHistory: defaultMaxTraceHistory,
		trafficSamples:  make([]TrafficSample, 0, maxTrafficSamples),
		maxSamples:      maxTrafficSamples,
		typeCounts:      make(map[string]int64),
		nodeDir:         make(map[uint32]NodeEntry),
		relayDir:        make(map[uint8][]uint32),
		subscribers:     make(map[chan Event]struct{}),
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
	m.trafficSamples = ringAppend(m.trafficSamples, m.maxSamples, sample)
	m.trafficMu.Unlock()

	// Publish metrics event
	m.publish(Event{Type: "metrics", Data: m.SnapshotDelta(sample)})
}

// Uptime returns the duration since the proxy started.
func (m *Metrics) Uptime() time.Duration {
	return time.Since(m.startTime)
}

// SetConfigCacheUpdated records the time and frame count of the latest config cache.
func (m *Metrics) SetConfigCacheUpdated(frames int) {
	m.ConfigCacheFrames.Store(int64(frames))
	m.configCacheMu.Lock()
	m.configCacheUpdatedAt = time.Now()
	m.configCacheMu.Unlock()
}

// ConfigCacheAge returns the time elapsed since the config cache was last updated.
// Returns zero if the cache has never been populated.
func (m *Metrics) ConfigCacheAge() time.Duration {
	m.configCacheMu.RLock()
	t := m.configCacheUpdatedAt
	m.configCacheMu.RUnlock()
	if t.IsZero() {
		return 0
	}
	return time.Since(t)
}

// Ready returns true if the proxy is ready to serve clients:
// the node is connected and the config cache is non-empty.
func (m *Metrics) Ready() bool {
	return m.NodeConnected.Load() && m.ConfigCacheFrames.Load() > 0
}

// PublishTraceroute stores the traceroute result in the history ring buffer
// and publishes a "traceroute_result" SSE event.
func (m *Metrics) PublishTraceroute(update TracerouteUpdate) {
	entry := TracerouteEntry{
		TracerouteUpdate: update,
		Timestamp:        time.Now(),
	}

	m.traceMu.Lock()
	m.traceHistory = ringAppend(m.traceHistory, m.maxTraceHistory, entry)
	m.traceMu.Unlock()

	m.publish(Event{Type: "traceroute_result", Data: entry})
}

// TraceHistory returns a copy of the traceroute history.
func (m *Metrics) TraceHistory() []TracerouteEntry {
	m.traceMu.RLock()
	defer m.traceMu.RUnlock()

	result := make([]TracerouteEntry, len(m.traceHistory))
	copy(result, m.traceHistory)
	return result
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
	m.messages = ringAppend(m.messages, m.maxMessages, rec)
	m.mu.Unlock()

	// Publish message event
	m.publish(Event{Type: "message", Data: rec})
}

// Messages returns a copy of the recent message log.
func (m *Metrics) Messages() []MessageRecord {
	m.mu.RLock()
	defer m.mu.RUnlock()

	result := make([]MessageRecord, len(m.messages))
	copy(result, m.messages)
	return result
}

// RecordChatMessage adds a chat message to the dedicated chat ring buffer
// and publishes a "chat_message" SSE event.
func (m *Metrics) RecordChatMessage(msg ChatMessage) {
	msg.Timestamp = time.Now()

	m.chatMu.Lock()
	m.chatMessages = ringAppend(m.chatMessages, m.maxChatMessages, msg)
	m.chatMu.Unlock()

	m.publish(Event{Type: "chat_message", Data: msg})
}

// ChatMessages returns a copy of the recent chat message log.
func (m *Metrics) ChatMessages() []ChatMessage {
	m.chatMu.RLock()
	defer m.chatMu.RUnlock()

	result := make([]ChatMessage, len(m.chatMessages))
	copy(result, m.chatMessages)
	return result
}

// TrafficSeries returns a copy of the traffic time-series.
func (m *Metrics) TrafficSeries() []TrafficSample {
	m.trafficMu.RLock()
	defer m.trafficMu.RUnlock()

	result := make([]TrafficSample, len(m.trafficSamples))
	copy(result, m.trafficSamples)
	return result
}

// MessageTypeCounts returns message type distribution.
func (m *Metrics) MessageTypeCounts() map[string]int64 {
	m.typeMu.RLock()
	defer m.typeMu.RUnlock()

	result := make(map[string]int64, len(m.typeCounts))
	for k, v := range m.typeCounts {
		result[k] = v
	}
	return result
}

// readCounters reads all atomic counters once, returning a consistent snapshot.
func (m *Metrics) readCounters() counters {
	uptime := m.Uptime()
	cacheAge := m.ConfigCacheAge()
	cacheAgeStr := ""
	if cacheAge > 0 {
		cacheAgeStr = formatDuration(cacheAge)
	}
	return counters{
		uptimeStr:               formatDuration(uptime),
		nodeConnected:           m.NodeConnected.Load(),
		nodeAddress:             m.NodeAddress,
		activeClients:           m.ActiveClients.Load(),
		bytesFromNode:           m.BytesFromNode.Load(),
		bytesToNode:             m.BytesToNode.Load(),
		framesFromNode:          m.FramesFromNode.Load(),
		framesToNode:            m.FramesToNode.Load(),
		messageTypeCounts:       m.MessageTypeCounts(),
		nodeReconnects:          m.NodeReconnects.Load(),
		nodeConnectionErrors:    m.NodeConnectionErrors.Load(),
		configCacheFrames:       m.ConfigCacheFrames.Load(),
		configCacheAge:          cacheAgeStr,
		configReplaysFull:       m.ConfigReplaysFull.Load(),
		configReplaysConfigOnly: m.ConfigReplaysConfigOnly.Load(),
		configReplaysNodesOnly:  m.ConfigReplaysNodesOnly.Load(),
	}
}

// Snapshot returns a consistent snapshot of the current metrics.
func (m *Metrics) Snapshot() Snapshot {
	c := m.readCounters()
	return Snapshot{
		Uptime:            m.Uptime(),
		UptimeStr:         c.uptimeStr,
		NodeConnected:     c.nodeConnected,
		NodeAddress:       c.nodeAddress,
		ActiveClients:     c.activeClients,
		BytesFromNode:     c.bytesFromNode,
		BytesToNode:       c.bytesToNode,
		FramesFromNode:    c.framesFromNode,
		FramesToNode:      c.framesToNode,
		Messages:          m.Messages(),
		TrafficSeries:     m.TrafficSeries(),
		MessageTypeCounts: c.messageTypeCounts,

		NodeReconnects:       c.nodeReconnects,
		NodeConnectionErrors: c.nodeConnectionErrors,

		ConfigCacheFrames: c.configCacheFrames,
		ConfigCacheAge:    c.configCacheAge,

		ConfigReplaysFull:       c.configReplaysFull,
		ConfigReplaysConfigOnly: c.configReplaysConfigOnly,
		ConfigReplaysNodesOnly:  c.configReplaysNodesOnly,

		MaxTrafficSamples: m.maxSamples,

		NodeDir: m.NodeDirectory(),
	}
}

// SnapshotDelta returns a lightweight delta with the given latest traffic sample.
func (m *Metrics) SnapshotDelta(sample TrafficSample) SnapshotDelta {
	c := m.readCounters()
	return SnapshotDelta{
		UptimeStr:         c.uptimeStr,
		NodeConnected:     c.nodeConnected,
		NodeAddress:       c.nodeAddress,
		ActiveClients:     c.activeClients,
		BytesFromNode:     c.bytesFromNode,
		BytesToNode:       c.bytesToNode,
		FramesFromNode:    c.framesFromNode,
		FramesToNode:      c.framesToNode,
		LatestSample:      sample,
		MessageTypeCounts: c.messageTypeCounts,

		NodeReconnects:       c.nodeReconnects,
		NodeConnectionErrors: c.nodeConnectionErrors,

		ConfigCacheFrames: c.configCacheFrames,
		ConfigCacheAge:    c.configCacheAge,

		ConfigReplaysFull:       c.configReplaysFull,
		ConfigReplaysConfigOnly: c.configReplaysConfigOnly,
		ConfigReplaysNodesOnly:  c.configReplaysNodesOnly,
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
