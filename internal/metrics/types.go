package metrics

import "time"

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
	From       uint32   `json:"from"`                  // target node that responded
	To         uint32   `json:"to"`                    // requesting node (our node)
	Route      []uint32 `json:"route"`                 // forward hops (requester → target)
	RouteBack  []uint32 `json:"route_back"`            // return hops (target → requester)
	SnrTowards []int32  `json:"snr_towards,omitempty"` // SNR (dB×4) at each forward hop
	SnrBack    []int32  `json:"snr_back,omitempty"`    // SNR (dB×4) at each return hop
}

// TracerouteEntry is a TracerouteUpdate with a timestamp, stored in the
// trace history ring buffer for the Trace History panel.
type TracerouteEntry struct {
	TracerouteUpdate
	Timestamp time.Time `json:"timestamp"`
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

// Snapshot is a point-in-time snapshot of all metrics.
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
