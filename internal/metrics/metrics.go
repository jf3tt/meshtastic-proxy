package metrics

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// nowUnix32 returns the current Unix timestamp as uint32.
// Safe until year 2106; gosec G115 flags the int64→uint32 narrowing,
// but Unix timestamps are always positive and fit in uint32 for the
// foreseeable future.
func nowUnix32() uint32 {
	return uint32(time.Now().Unix()) //nolint:gosec // G115: unix timestamp fits uint32 until 2106
}

// NodeEntry stores all known information about a single mesh node.
type NodeEntry struct {
	// Identity (from NodeInfo.User)
	ShortName  string `json:"short_name"`
	LongName   string `json:"long_name"`
	UserID     string `json:"user_id,omitempty"`     // hex ID e.g. "!aabbccdd"
	HwModel    string `json:"hw_model,omitempty"`    // e.g. "HELTEC_V3"
	Role       string `json:"role,omitempty"`        // e.g. "CLIENT", "ROUTER"
	IsLicensed bool   `json:"is_licensed,omitempty"` // HAM radio licensed

	// Radio (from NodeInfo)
	Snr        float32 `json:"snr,omitempty"`
	LastHeard  uint32  `json:"last_heard,omitempty"` // unix timestamp
	HopsAway   uint32  `json:"hops_away,omitempty"`
	ViaMqtt    bool    `json:"via_mqtt,omitempty"`
	IsFavorite bool    `json:"is_favorite,omitempty"`
	Channel    uint32  `json:"channel,omitempty"`

	// Position (from NodeInfo.Position or POSITION_APP)
	Latitude     float64 `json:"latitude,omitempty"`
	Longitude    float64 `json:"longitude,omitempty"`
	Altitude     int32   `json:"altitude,omitempty"`
	GroundSpeed  uint32  `json:"ground_speed,omitempty"` // m/s
	GroundTrack  uint32  `json:"ground_track,omitempty"` // degrees (0-360)
	SatsInView   uint32  `json:"sats_in_view,omitempty"`
	PositionTime uint32  `json:"position_time,omitempty"` // unix timestamp of position fix

	// Device telemetry (from TELEMETRY_APP DeviceMetrics)
	BatteryLevel       uint32  `json:"battery_level,omitempty"`       // 0-100%
	Voltage            float32 `json:"voltage,omitempty"`             // volts
	ChannelUtilization float32 `json:"channel_utilization,omitempty"` // percentage
	AirUtilTx          float32 `json:"air_util_tx,omitempty"`         // percentage
	UptimeSeconds      uint32  `json:"uptime_seconds,omitempty"`

	// Environment telemetry (from TELEMETRY_APP EnvironmentMetrics)
	Temperature        float32 `json:"temperature,omitempty"`         // Celsius
	RelativeHumidity   float32 `json:"relative_humidity,omitempty"`   // percentage
	BarometricPressure float32 `json:"barometric_pressure,omitempty"` // hPa

	// Signal quality (updated from incoming MeshPacket RxRssi/RxSnr)
	RxRssi int32   `json:"rx_rssi,omitempty"` // RSSI at receiver (dBm, negative)
	RxSnr  float32 `json:"rx_snr,omitempty"`  // SNR at receiver (dB)

	// SeenRealtime is true if the proxy has received at least one real-time
	// packet from this node (MeshPacket, NODEINFO_APP, POSITION_APP, etc.)
	// after the initial config cache was populated. Nodes known only from the
	// config cache will have this set to false.
	SeenRealtime bool `json:"seen_realtime"`
}

// HasPosition returns true if the node has valid GPS coordinates.
func (e NodeEntry) HasPosition() bool {
	return e.Latitude != 0 || e.Longitude != 0
}

// PositionUpdate is published via SSE when a node's position changes.
type PositionUpdate struct {
	NodeNum      uint32  `json:"node_num"`
	ShortName    string  `json:"short_name"`
	LongName     string  `json:"long_name"`
	Latitude     float64 `json:"latitude"`
	Longitude    float64 `json:"longitude"`
	Altitude     int32   `json:"altitude"`
	GroundSpeed  uint32  `json:"ground_speed,omitempty"`
	GroundTrack  uint32  `json:"ground_track,omitempty"`
	SatsInView   uint32  `json:"sats_in_view,omitempty"`
	PositionTime uint32  `json:"position_time,omitempty"`
}

// TelemetryUpdate is published via SSE when a node's telemetry changes.
type TelemetryUpdate struct {
	NodeNum            uint32  `json:"node_num"`
	ShortName          string  `json:"short_name"`
	LongName           string  `json:"long_name"`
	BatteryLevel       uint32  `json:"battery_level,omitempty"`
	Voltage            float32 `json:"voltage,omitempty"`
	ChannelUtilization float32 `json:"channel_utilization,omitempty"`
	AirUtilTx          float32 `json:"air_util_tx,omitempty"`
	UptimeSeconds      uint32  `json:"uptime_seconds,omitempty"`
	Temperature        float32 `json:"temperature,omitempty"`
	RelativeHumidity   float32 `json:"relative_humidity,omitempty"`
	BarometricPressure float32 `json:"barometric_pressure,omitempty"`
}

// SignalUpdate is published via SSE when a node's signal quality changes
// (from an incoming MeshPacket's RxRssi/RxSnr fields).
type SignalUpdate struct {
	NodeNum   uint32  `json:"node_num"`
	ShortName string  `json:"short_name"`
	LongName  string  `json:"long_name"`
	RxRssi    int32   `json:"rx_rssi"`
	RxSnr     float32 `json:"rx_snr"`
}

// NodeInfoUpdate is published via SSE when a node's identity is discovered
// or updated from a NODEINFO_APP packet received after the initial config cache.
type NodeInfoUpdate struct {
	NodeNum    uint32 `json:"node_num"`
	ShortName  string `json:"short_name"`
	LongName   string `json:"long_name"`
	UserID     string `json:"user_id,omitempty"`
	HwModel    string `json:"hw_model,omitempty"`
	Role       string `json:"role,omitempty"`
	IsLicensed bool   `json:"is_licensed,omitempty"`
	IsNew      bool   `json:"is_new"` // true if this is a newly discovered node
}

// TracerouteUpdate is published via SSE when a TRACEROUTE_APP response is received.
type TracerouteUpdate struct {
	From      uint32   `json:"from"`       // target node that responded
	To        uint32   `json:"to"`         // requesting node (our node)
	Route     []uint32 `json:"route"`      // forward hops (requester → target)
	RouteBack []uint32 `json:"route_back"` // return hops (target → requester)
}

// ChatMessage stores a single text message from the mesh network.
type ChatMessage struct {
	Timestamp time.Time `json:"timestamp"`
	From      uint32    `json:"from"`      // sender node number
	To        uint32    `json:"to"`        // destination (0xFFFFFFFF = broadcast)
	Channel   uint32    `json:"channel"`   // channel index
	Text      string    `json:"text"`      // message text (UTF-8)
	FromName  string    `json:"from_name"` // short name of sender (from node directory)
	ToName    string    `json:"to_name"`   // short name of recipient (from node directory)
	Direction string    `json:"direction"` // "incoming" or "outgoing"
	ViaMqtt   bool      `json:"via_mqtt"`  // relayed through MQTT
	RxRssi    int32     `json:"rx_rssi"`   // RSSI at receiver (dBm)
	RxSnr     float32   `json:"rx_snr"`    // SNR at receiver (dB)
}

// MessageRecord stores information about a proxied message.
type MessageRecord struct {
	Timestamp time.Time `json:"timestamp"`
	Direction string    `json:"direction"`  // "from_node" or "to_node"
	Type      string    `json:"type"`       // protobuf message type
	Size      int       `json:"size"`       // payload size in bytes
	Client    string    `json:"client"`     // client address (for to_node direction)
	PortNum   string    `json:"port_num"`   // e.g. "TEXT_MESSAGE_APP", "POSITION_APP"
	From      uint32    `json:"from"`       // source node number
	To        uint32    `json:"to"`         // destination node number
	Channel   uint32    `json:"channel"`    // channel index
	HopLimit  uint32    `json:"hop_limit"`  // remaining hop count
	HopStart  uint32    `json:"hop_start"`  // initial hop count (0 = unknown)
	RxRssi    int32     `json:"rx_rssi"`    // RSSI at receiver (dBm, negative)
	RxSnr     float32   `json:"rx_snr"`     // SNR at receiver (dB)
	ViaMqtt   bool      `json:"via_mqtt"`   // relayed through MQTT gateway
	RelayNode uint32    `json:"relay_node"` // last byte of relay node number (0 = direct)
	Payload   string    `json:"payload"`    // decoded human-readable payload
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
	Type string `json:"type"` // "metrics" or "message"
	Data any    `json:"data"`
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
		maxMessages = 100
	}
	if maxTrafficSamples <= 0 {
		maxTrafficSamples = 300
	}
	return &Metrics{
		startTime:       time.Now(),
		messages:        make([]MessageRecord, 0, maxMessages),
		maxMessages:     maxMessages,
		chatMessages:    make([]ChatMessage, 0, 500),
		maxChatMessages: 500,
		trafficSamples:  make([]TrafficSample, 0, maxTrafficSamples),
		maxSamples:      maxTrafficSamples,
		typeCounts:      make(map[string]int64),
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
	if len(m.trafficSamples) >= m.maxSamples {
		copy(m.trafficSamples, m.trafficSamples[1:])
		m.trafficSamples = m.trafficSamples[:len(m.trafficSamples)-1]
	}
	m.trafficSamples = append(m.trafficSamples, sample)
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

// SetNodeDirectory replaces the node directory with the given map and rebuilds
// the relay lookup index. It also publishes a "node_directory" SSE event.
func (m *Metrics) SetNodeDirectory(dir map[uint32]NodeEntry) {
	relay := make(map[uint8][]uint32, len(dir))
	for num := range dir {
		key := uint8(num & 0xFF)
		relay[key] = append(relay[key], num)
	}

	m.nodeDirMu.Lock()
	m.nodeDir = dir
	m.relayDir = relay
	m.nodeDirMu.Unlock()

	m.publish(Event{Type: "node_directory", Data: dir})
}

// NodeDirectory returns a copy of the current node directory.
func (m *Metrics) NodeDirectory() map[uint32]NodeEntry {
	m.nodeDirMu.RLock()
	defer m.nodeDirMu.RUnlock()

	result := make(map[uint32]NodeEntry, len(m.nodeDir))
	for k, v := range m.nodeDir {
		result[k] = v
	}
	return result
}

// ResolveRelay returns the node entries whose last byte matches the given
// relay_node value. Returns nil if no matches are found.
func (m *Metrics) ResolveRelay(relayByte uint8) []NodeEntry {
	m.nodeDirMu.RLock()
	defer m.nodeDirMu.RUnlock()

	nums := m.relayDir[relayByte]
	if len(nums) == 0 {
		return nil
	}
	result := make([]NodeEntry, 0, len(nums))
	for _, num := range nums {
		if entry, ok := m.nodeDir[num]; ok {
			result = append(result, entry)
		}
	}
	return result
}

// UpdateNodePosition updates the position of a single node in the directory
// and publishes an SSE event so the map updates in real time.
func (m *Metrics) UpdateNodePosition(update PositionUpdate) {
	m.nodeDirMu.Lock()
	if m.nodeDir == nil {
		m.nodeDir = make(map[uint32]NodeEntry)
	}
	entry := m.nodeDir[update.NodeNum]
	entry.SeenRealtime = true
	entry.LastHeard = nowUnix32()
	entry.Latitude = update.Latitude
	entry.Longitude = update.Longitude
	entry.Altitude = update.Altitude
	if update.GroundSpeed > 0 {
		entry.GroundSpeed = update.GroundSpeed
	}
	if update.GroundTrack > 0 {
		entry.GroundTrack = update.GroundTrack
	}
	if update.SatsInView > 0 {
		entry.SatsInView = update.SatsInView
	}
	if update.PositionTime > 0 {
		entry.PositionTime = update.PositionTime
	}
	m.nodeDir[update.NodeNum] = entry

	// Fill names for SSE event from stored entry.
	update.ShortName = entry.ShortName
	update.LongName = entry.LongName
	m.nodeDirMu.Unlock()

	m.publish(Event{Type: "node_position_update", Data: update})
}

// UpdateNodeTelemetry updates telemetry data for a single node in the directory
// and publishes an SSE event so the dashboard updates in real time.
func (m *Metrics) UpdateNodeTelemetry(update TelemetryUpdate) {
	m.nodeDirMu.Lock()
	if m.nodeDir == nil {
		m.nodeDir = make(map[uint32]NodeEntry)
	}
	entry := m.nodeDir[update.NodeNum]
	entry.SeenRealtime = true
	entry.LastHeard = nowUnix32()

	// Device metrics
	if update.BatteryLevel > 0 {
		entry.BatteryLevel = update.BatteryLevel
	}
	if update.Voltage > 0 {
		entry.Voltage = update.Voltage
	}
	if update.ChannelUtilization > 0 {
		entry.ChannelUtilization = update.ChannelUtilization
	}
	if update.AirUtilTx > 0 {
		entry.AirUtilTx = update.AirUtilTx
	}
	if update.UptimeSeconds > 0 {
		entry.UptimeSeconds = update.UptimeSeconds
	}

	// Environment metrics
	if update.Temperature != 0 {
		entry.Temperature = update.Temperature
	}
	if update.RelativeHumidity != 0 {
		entry.RelativeHumidity = update.RelativeHumidity
	}
	if update.BarometricPressure != 0 {
		entry.BarometricPressure = update.BarometricPressure
	}

	m.nodeDir[update.NodeNum] = entry

	// Fill names for SSE event from stored entry.
	update.ShortName = entry.ShortName
	update.LongName = entry.LongName
	m.nodeDirMu.Unlock()

	m.publish(Event{Type: "node_telemetry_update", Data: update})
}

// UpdateNodeSignal updates the signal quality (RSSI/SNR) for a single node
// in the directory and publishes an SSE event so the heatmap updates in real time.
// Called when a MeshPacket is received with non-zero RxRssi from a known node.
func (m *Metrics) UpdateNodeSignal(update SignalUpdate) {
	m.nodeDirMu.Lock()
	if m.nodeDir == nil {
		m.nodeDir = make(map[uint32]NodeEntry)
	}
	entry := m.nodeDir[update.NodeNum]
	entry.SeenRealtime = true
	entry.LastHeard = nowUnix32()

	if update.RxRssi != 0 {
		entry.RxRssi = update.RxRssi
	}
	if update.RxSnr != 0 {
		entry.RxSnr = update.RxSnr
	}

	m.nodeDir[update.NodeNum] = entry

	// Fill names for SSE event from stored entry.
	update.ShortName = entry.ShortName
	update.LongName = entry.LongName
	m.nodeDirMu.Unlock()

	m.publish(Event{Type: "node_signal_update", Data: update})
}

// UpsertNode updates the identity fields of a node in the directory (or creates
// a new entry) and publishes a "node_update" SSE event. This is called when a
// NODEINFO_APP packet is received after the initial config cache, allowing the
// dashboard to discover new nodes joining the mesh in real time.
func (m *Metrics) UpsertNode(update NodeInfoUpdate) {
	m.nodeDirMu.Lock()
	if m.nodeDir == nil {
		m.nodeDir = make(map[uint32]NodeEntry)
	}

	entry, exists := m.nodeDir[update.NodeNum]
	update.IsNew = !exists
	entry.SeenRealtime = true
	entry.LastHeard = nowUnix32()

	// Update identity fields
	if update.ShortName != "" {
		entry.ShortName = update.ShortName
	}
	if update.LongName != "" {
		entry.LongName = update.LongName
	}
	if update.UserID != "" {
		entry.UserID = update.UserID
	}
	if update.HwModel != "" {
		entry.HwModel = update.HwModel
	}
	if update.Role != "" {
		entry.Role = update.Role
	}
	entry.IsLicensed = update.IsLicensed

	m.nodeDir[update.NodeNum] = entry

	// Update relay index if this is a new node
	if update.IsNew {
		if m.relayDir == nil {
			m.relayDir = make(map[uint8][]uint32)
		}
		key := uint8(update.NodeNum & 0xFF)
		m.relayDir[key] = append(m.relayDir[key], update.NodeNum)
	}
	m.nodeDirMu.Unlock()

	m.publish(Event{Type: "node_update", Data: update})
}

// PublishTraceroute publishes a "traceroute_result" SSE event when a
// TRACEROUTE_APP response is received from the mesh network.
func (m *Metrics) PublishTraceroute(update TracerouteUpdate) {
	m.publish(Event{Type: "traceroute_result", Data: update})
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
	if len(m.chatMessages) >= m.maxChatMessages {
		copy(m.chatMessages, m.chatMessages[1:])
		m.chatMessages = m.chatMessages[:len(m.chatMessages)-1]
	}
	m.chatMessages = append(m.chatMessages, msg)
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

// PublishClients sends a "clients" event to all SSE subscribers with the
// current list of connected client addresses.
func (m *Metrics) PublishClients(clients []string) {
	m.publish(Event{Type: "clients", Data: clients})
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

	// Node connection reliability
	NodeReconnects       int64 `json:"node_reconnects"`
	NodeConnectionErrors int64 `json:"node_connection_errors"`

	// Config cache status
	ConfigCacheFrames int64  `json:"config_cache_frames"`
	ConfigCacheAge    string `json:"config_cache_age"` // human-readable, e.g. "5m 12s"

	// Config replay counters
	ConfigReplaysFull       int64 `json:"config_replays_full"`
	ConfigReplaysConfigOnly int64 `json:"config_replays_config_only"`
	ConfigReplaysNodesOnly  int64 `json:"config_replays_nodes_only"`

	// Configured limits (for JS to use)
	MaxTrafficSamples int `json:"max_traffic_samples"`

	// Node directory for name resolution
	NodeDir map[uint32]NodeEntry `json:"node_dir"`
}

// counters holds a consistent reading of all atomic counters and derived values.
type counters struct {
	uptimeStr               string
	nodeConnected           bool
	nodeAddress             string
	activeClients           int64
	bytesFromNode           int64
	bytesToNode             int64
	framesFromNode          int64
	framesToNode            int64
	messageTypeCounts       map[string]int64
	nodeReconnects          int64
	nodeConnectionErrors    int64
	configCacheFrames       int64
	configCacheAge          string
	configReplaysFull       int64
	configReplaysConfigOnly int64
	configReplaysNodesOnly  int64
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

// SnapshotDelta is a lightweight metrics update sent via SSE every second.
// It contains only scalar counters and the latest traffic sample, omitting
// the full message log and traffic series (which are sent in the initial
// full Snapshot and via individual "message" events).
type SnapshotDelta struct {
	UptimeStr         string           `json:"uptime_str"`
	NodeConnected     bool             `json:"node_connected"`
	NodeAddress       string           `json:"node_address"`
	ActiveClients     int64            `json:"active_clients"`
	BytesFromNode     int64            `json:"bytes_from_node"`
	BytesToNode       int64            `json:"bytes_to_node"`
	FramesFromNode    int64            `json:"frames_from_node"`
	FramesToNode      int64            `json:"frames_to_node"`
	LatestSample      TrafficSample    `json:"latest_sample"`
	MessageTypeCounts map[string]int64 `json:"message_type_counts"`

	NodeReconnects       int64 `json:"node_reconnects"`
	NodeConnectionErrors int64 `json:"node_connection_errors"`

	ConfigCacheFrames int64  `json:"config_cache_frames"`
	ConfigCacheAge    string `json:"config_cache_age"`

	ConfigReplaysFull       int64 `json:"config_replays_full"`
	ConfigReplaysConfigOnly int64 `json:"config_replays_config_only"`
	ConfigReplaysNodesOnly  int64 `json:"config_replays_nodes_only"`
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
