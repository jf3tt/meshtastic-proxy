package node

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	pb "buf.build/gen/go/meshtastic/protobufs/protocolbuffers/go/meshtastic"
	"google.golang.org/protobuf/proto"

	"github.com/jfett/meshtastic-proxy/internal/metrics"
	"github.com/jfett/meshtastic-proxy/internal/protocol"
)

// Connection manages a persistent TCP connection to a Meshtastic node
// with automatic reconnection using exponential backoff.
type Connection struct {
	address              string
	reconnectInterval    time.Duration
	maxReconnectInterval time.Duration
	dialTimeout          time.Duration
	readTimeout          time.Duration
	heartbeatInterval    time.Duration

	metrics *metrics.Metrics
	logger  *slog.Logger

	// mu protects conn
	mu   sync.Mutex
	conn net.Conn

	// fromNode delivers parsed FromRadio frames to the proxy hub
	fromNode chan []byte

	// toNode serializes writes to the node
	toNode chan []byte

	// configCache stores the last known configuration for new clients
	configMu    sync.RWMutex
	configCache [][]byte // serialized FromRadio frames (config sequence)

	// myNodeNum is the node number of the connected node, extracted from my_info.
	myNodeNum atomic.Uint32
}

// ConnectionOptions holds all configuration for creating a new Connection.
type ConnectionOptions struct {
	Address              string
	ReconnectInterval    time.Duration
	MaxReconnectInterval time.Duration
	DialTimeout          time.Duration
	ReadTimeout          time.Duration
	HeartbeatInterval    time.Duration
	FromBuffer           int
	ToBuffer             int
	Metrics              *metrics.Metrics
	Logger               *slog.Logger
}

// NewConnection creates a new node connection manager.
func NewConnection(opts ConnectionOptions) *Connection {
	return &Connection{
		address:              opts.Address,
		reconnectInterval:    opts.ReconnectInterval,
		maxReconnectInterval: opts.MaxReconnectInterval,
		dialTimeout:          opts.DialTimeout,
		readTimeout:          opts.ReadTimeout,
		heartbeatInterval:    opts.HeartbeatInterval,
		metrics:              opts.Metrics,
		logger:               opts.Logger,
		fromNode:             make(chan []byte, opts.FromBuffer),
		toNode:               make(chan []byte, opts.ToBuffer),
	}
}

// FromNode returns a read-only channel of raw FromRadio frame payloads.
func (c *Connection) FromNode() <-chan []byte {
	return c.fromNode
}

// Send queues a raw ToRadio frame payload for sending to the node.
func (c *Connection) Send(payload []byte) {
	select {
	case c.toNode <- payload:
		c.metrics.FramesToNode.Add(1)
		c.metrics.BytesToNode.Add(int64(len(payload)))
		c.metrics.RecordMessage(decodeToRadio(payload))

		// Record outgoing TEXT_MESSAGE_APP as a chat message
		if chat := ExtractChatMessageFromToRadio(payload); chat != nil {
			if chat.From == 0 {
				chat.From = c.myNodeNum.Load()
			}
			c.recordChatMessage(chat, "outgoing")
		}
	default:
		c.logger.Warn("toNode channel full, dropping frame")
	}
}

// recordChatMessage resolves node names from the directory and records
// a chat message in the metrics ring buffer.
func (c *Connection) recordChatMessage(chat *ChatMessageData, direction string) {
	dir := c.metrics.NodeDirectory()

	fromName := ""
	if entry, ok := dir[chat.From]; ok && entry.ShortName != "" {
		fromName = entry.ShortName
	}

	toName := ""
	if chat.To == 0xFFFFFFFF {
		toName = "Broadcast"
	} else if entry, ok := dir[chat.To]; ok && entry.ShortName != "" {
		toName = entry.ShortName
	}

	c.metrics.RecordChatMessage(metrics.ChatMessage{
		From:      chat.From,
		To:        chat.To,
		Channel:   chat.Channel,
		Text:      chat.Text,
		FromName:  fromName,
		ToName:    toName,
		Direction: direction,
		ViaMqtt:   chat.ViaMqtt,
		RxRssi:    chat.RxRssi,
		RxSnr:     chat.RxSnr,
	})
}

// ConfigCache returns the cached configuration frames for replaying to new clients.
func (c *Connection) ConfigCache() [][]byte {
	c.configMu.RLock()
	defer c.configMu.RUnlock()

	result := make([][]byte, len(c.configCache))
	for i, frame := range c.configCache {
		cp := make([]byte, len(frame))
		copy(cp, frame)
		result[i] = cp
	}
	return result
}

// UpsertCachedNodeInfo inserts or replaces a FromRadio_NodeInfo frame in the
// config cache so that newly discovered nodes (via NODEINFO_APP packets) are
// available to clients that connect later. The synthesized frame uses the
// identity fields from NodeInfoData and sets LastHeard to the current time.
//
// If a NodeInfo frame for the same node number already exists in the cache it
// is replaced in-place. Otherwise a new frame is inserted immediately before
// the final ConfigCompleteId frame so that filterConfigCache and
// replayCachedConfig pick it up without any changes.
func (c *Connection) UpsertCachedNodeInfo(ni *NodeInfoData) {
	if ni == nil || ni.NodeNum == 0 {
		return
	}

	// Synthesize a FromRadio_NodeInfo protobuf frame.
	frame := buildNodeInfoFrame(ni)
	if frame == nil {
		return
	}

	c.configMu.Lock()
	defer c.configMu.Unlock()

	if len(c.configCache) == 0 {
		return // no config cached yet — nothing to augment
	}

	// Look for an existing NodeInfo frame with the same node number.
	for i, raw := range c.configCache {
		msg := &pb.FromRadio{}
		if err := proto.Unmarshal(raw, msg); err != nil {
			continue
		}
		v, ok := msg.GetPayloadVariant().(*pb.FromRadio_NodeInfo)
		if !ok || v.NodeInfo == nil {
			continue
		}
		if v.NodeInfo.GetNum() == ni.NodeNum {
			// Merge User fields into the existing frame, preserving
			// Position, DeviceMetrics, Snr, and other NodeInfo fields
			// that are not available in a NODEINFO_APP broadcast.
			if merged := mergeNodeInfoUser(raw, ni); merged != nil {
				c.configCache[i] = merged
			} else {
				// Fallback to synthesized frame if merge fails.
				c.configCache[i] = frame
			}
			return
		}
	}

	// Not found — insert before the last frame (ConfigCompleteId).
	if len(c.configCache) == 0 {
		c.configCache = append(c.configCache, frame)
		return
	}
	// Check if the last frame is ConfigCompleteId. If so, insert before it
	// to ensure the new NodeInfo is delivered to clients during config replay.
	lastIdx := len(c.configCache) - 1
	if isConfigCompleteFrame(c.configCache[lastIdx]) {
		c.configCache = append(c.configCache, nil)               // grow by one
		copy(c.configCache[lastIdx+1:], c.configCache[lastIdx:]) // shift last frame right
		c.configCache[lastIdx] = frame
	} else {
		c.configCache = append(c.configCache, frame)
	}
}

// isConfigCompleteFrame returns true if the raw frame is a FromRadio_ConfigCompleteId.
func isConfigCompleteFrame(raw []byte) bool {
	msg := &pb.FromRadio{}
	if err := proto.Unmarshal(raw, msg); err != nil {
		return false
	}
	_, ok := msg.GetPayloadVariant().(*pb.FromRadio_ConfigCompleteId)
	return ok
}

// buildNodeInfoFrame synthesizes a serialized FromRadio_NodeInfo protobuf
// from NodeInfoData. Returns nil on marshal failure.
func buildNodeInfoFrame(ni *NodeInfoData) []byte {
	hwModel := pb.HardwareModel(pb.HardwareModel_value[ni.HwModel])
	role := pb.Config_DeviceConfig_Role(pb.Config_DeviceConfig_Role_value[ni.Role])

	now := uint32(time.Now().Unix()) //nolint:gosec // G115: unix timestamp fits uint32 until 2106

	user := &pb.User{
		Id:         ni.UserID,
		ShortName:  ni.ShortName,
		LongName:   ni.LongName,
		HwModel:    hwModel,
		Role:       role,
		IsLicensed: ni.IsLicensed,
		PublicKey:  ni.PublicKey,
		Macaddr:    ni.Macaddr,
	}
	if ni.IsUnmessagable != nil {
		user.SetIsUnmessagable(*ni.IsUnmessagable)
	}

	msg := &pb.FromRadio{
		PayloadVariant: &pb.FromRadio_NodeInfo{
			NodeInfo: &pb.NodeInfo{
				Num:       ni.NodeNum,
				User:      user,
				LastHeard: now,
			},
		},
	}

	raw, err := proto.Marshal(msg)
	if err != nil {
		return nil
	}
	return raw
}

// mergeNodeInfoUser updates the User fields in an existing serialized
// FromRadio_NodeInfo frame with data from a NODEINFO_APP broadcast, while
// preserving all other NodeInfo fields (Position, DeviceMetrics, Snr,
// HopsAway, ViaMqtt, IsFavorite, Channel, etc.) that are not present in
// the broadcast. Returns the re-serialized frame, or nil on failure.
func mergeNodeInfoUser(existingRaw []byte, ni *NodeInfoData) []byte {
	msg := &pb.FromRadio{}
	if err := proto.Unmarshal(existingRaw, msg); err != nil {
		return nil
	}
	v, ok := msg.GetPayloadVariant().(*pb.FromRadio_NodeInfo)
	if !ok || v.NodeInfo == nil {
		return nil
	}

	hwModel := pb.HardwareModel(pb.HardwareModel_value[ni.HwModel])
	role := pb.Config_DeviceConfig_Role(pb.Config_DeviceConfig_Role_value[ni.Role])

	// Update User fields while preserving the rest of NodeInfo.
	user := v.NodeInfo.User
	if user == nil {
		user = &pb.User{}
		v.NodeInfo.User = user
	}
	user.Id = ni.UserID
	user.ShortName = ni.ShortName
	user.LongName = ni.LongName
	user.HwModel = hwModel
	user.Role = role
	user.IsLicensed = ni.IsLicensed
	user.PublicKey = ni.PublicKey
	user.Macaddr = ni.Macaddr //nolint:staticcheck // deprecated but needed for client compat
	if ni.IsUnmessagable != nil {
		user.SetIsUnmessagable(*ni.IsUnmessagable)
	}
	// When IsUnmessagable is nil (broadcast didn't include the field),
	// preserve whatever value the cached frame already has.

	// Update LastHeard to current time.
	v.NodeInfo.LastHeard = uint32(time.Now().Unix()) //nolint:gosec // G115: unix timestamp fits uint32 until 2106

	raw, err := proto.Marshal(msg)
	if err != nil {
		return nil
	}
	return raw
}

// MyNodeNum returns the node number of the connected Meshtastic node.
// Returns 0 if the config has not yet been received.
func (c *Connection) MyNodeNum() uint32 {
	return c.myNodeNum.Load()
}

// Run starts the connection manager. It connects to the node, reads frames,
// and handles reconnection. It blocks until the context is canceled.
func (c *Connection) Run(ctx context.Context) {
	backoff := c.reconnectInterval
	firstConnect := true

	for {
		select {
		case <-ctx.Done():
			c.close()
			return
		default:
		}

		c.logger.Info("connecting to node", "address", c.address)

		conn, err := c.dial(ctx)
		if err != nil {
			c.logger.Error("failed to connect to node", "error", err, "retry_in", backoff)
			c.metrics.NodeConnected.Store(false)
			c.metrics.NodeConnectionErrors.Add(1)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, c.maxReconnectInterval)
			continue
		}

		// Connection established
		c.mu.Lock()
		c.conn = conn
		c.mu.Unlock()
		c.metrics.NodeConnected.Store(true)
		if !firstConnect {
			c.metrics.NodeReconnects.Add(1)
		}
		firstConnect = false
		backoff = c.reconnectInterval
		c.logger.Info("connected to node",
			"address", c.address,
			"local_addr", conn.LocalAddr().String(),
		)

		connectedAt := time.Now()

		// Request config from node
		c.requestConfig()

		// Run read/write loops
		err = c.runConnection(ctx, conn)
		if err != nil {
			c.logger.Error("node connection error",
				"error", err,
				"uptime", time.Since(connectedAt).Round(time.Millisecond),
			)
		}

		c.metrics.NodeConnected.Store(false)
		c.close()

		// Drain stale frames from toNode/fromNode channels so the next
		// connection starts with a clean slate. Without this, leftover
		// heartbeats or client frames queued just before disconnect would
		// be sent to the node before want_config_id on reconnect.
		drained := c.drainChannels()
		if drained > 0 {
			c.logger.Debug("drained stale frames before reconnect", "count", drained)
		}

		c.logger.Info("disconnected from node, reconnecting", "retry_in", backoff)

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, c.maxReconnectInterval)
	}
}

func (c *Connection) dial(ctx context.Context) (net.Conn, error) {
	d := net.Dialer{
		Timeout:   c.dialTimeout,
		KeepAlive: 15 * time.Second,
	}
	conn, err := d.DialContext(ctx, "tcp", c.address)
	if err != nil {
		return nil, fmt.Errorf("dialing %s: %w", c.address, err)
	}

	// Set aggressive TCP keepalive parameters to keep ESP32 lwIP PCB alive.
	// The ESP32 lwIP stack silently discards TCP PCBs after ~60-90s of
	// inactivity. Sending empty TCP ACK probes every 15s prevents this.
	// TCP_KEEPIDLE=15s, TCP_KEEPINTVL=5s, TCP_KEEPCNT=3.
	if tc, ok := conn.(*net.TCPConn); ok {
		if err := tc.SetKeepAlive(true); err != nil {
			c.logger.Warn("failed to enable TCP keepalive", "error", err)
		}
		if err := tc.SetKeepAlivePeriod(15 * time.Second); err != nil {
			c.logger.Warn("failed to set TCP keepalive period", "error", err)
		}
	}

	return conn, nil
}

func (c *Connection) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil {
		_ = c.conn.Close()
		c.conn = nil
	}
}

// drainChannels discards any stale frames left in the toNode and fromNode
// channels after a disconnect. Returns the total number of drained frames.
func (c *Connection) drainChannels() int {
	drained := 0
	for len(c.toNode) > 0 {
		<-c.toNode
		drained++
	}
	for len(c.fromNode) > 0 {
		<-c.fromNode
		drained++
	}
	return drained
}

func (c *Connection) runConnection(ctx context.Context, conn net.Conn) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	goroutines := 2
	if c.heartbeatInterval > 0 {
		goroutines = 3
	}
	errCh := make(chan error, goroutines)

	// Reader goroutine
	go func() {
		errCh <- c.readLoop(ctx, conn)
	}()

	// Writer goroutine
	go func() {
		errCh <- c.writeLoop(ctx, conn)
	}()

	// Heartbeat goroutine (keeps the connection alive when mesh is quiet)
	if c.heartbeatInterval > 0 {
		go func() {
			errCh <- c.heartbeatLoop(ctx)
		}()
	}

	// Wait for first error or context cancellation
	select {
	case err := <-errCh:
		cancel()
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Connection) readLoop(ctx context.Context, conn net.Conn) error {
	// Wrap connection in a buffered reader to reduce syscall overhead.
	// ReadFrame scans byte-by-byte for magic bytes; bufio batches reads.
	br := bufio.NewReaderSize(conn, 4096)

	// Collect config frames during handshake
	var collectingConfig bool
	var configFrames [][]byte

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		// Set read deadline to detect silent node death.
		// If no data arrives within readTimeout, the read returns a timeout
		// error and we reconnect.
		if c.readTimeout > 0 {
			_ = conn.SetReadDeadline(time.Now().Add(c.readTimeout))
		}

		payload, err := protocol.ReadFrame(br)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("reading frame from node: %w", err)
		}

		c.metrics.FramesFromNode.Add(1)
		c.metrics.BytesFromNode.Add(int64(len(payload)))

		// Decode and record
		rec := decodeFromRadio(payload)
		c.metrics.RecordMessage(rec)

		// Update node position from POSITION_APP packets in real time
		if pos := ExtractPosition(payload); pos != nil {
			c.metrics.UpdateNodePosition(metrics.PositionUpdate{
				NodeNum:      pos.NodeNum,
				Latitude:     pos.Latitude,
				Longitude:    pos.Longitude,
				Altitude:     pos.Altitude,
				GroundSpeed:  pos.GroundSpeed,
				GroundTrack:  pos.GroundTrack,
				SatsInView:   pos.SatsInView,
				PositionTime: pos.PositionTime,
			})
		}

		// Update node telemetry from TELEMETRY_APP packets in real time
		if tel := ExtractTelemetry(payload); tel != nil {
			c.metrics.UpdateNodeTelemetry(metrics.TelemetryUpdate{
				NodeNum:            tel.NodeNum,
				BatteryLevel:       tel.BatteryLevel,
				Voltage:            tel.Voltage,
				ChannelUtilization: tel.ChannelUtilization,
				AirUtilTx:          tel.AirUtilTx,
				UptimeSeconds:      tel.UptimeSeconds,
				Temperature:        tel.Temperature,
				RelativeHumidity:   tel.RelativeHumidity,
				BarometricPressure: tel.BarometricPressure,
			})
		}

		// Update node signal quality from MeshPacket RxRssi/RxSnr in real time
		if sig := ExtractSignal(payload); sig != nil {
			c.metrics.UpdateNodeSignal(metrics.SignalUpdate{
				NodeNum: sig.NodeNum,
				RxRssi:  sig.RxRssi,
				RxSnr:   sig.RxSnr,
			})
		}

		// Record incoming TEXT_MESSAGE_APP as a chat message
		if chat := ExtractChatMessage(payload); chat != nil {
			c.recordChatMessage(chat, "incoming")
		}

		// Update node directory from NODEINFO_APP packets in real time.
		// This catches new nodes that join the mesh after the initial
		// config cache was built, and also updates identity fields
		// (name, hw model, role) for existing nodes.
		if ni := ExtractNodeInfo(payload); ni != nil {
			c.metrics.UpsertNode(metrics.NodeInfoUpdate{
				NodeNum:    ni.NodeNum,
				ShortName:  ni.ShortName,
				LongName:   ni.LongName,
				UserID:     ni.UserID,
				HwModel:    ni.HwModel,
				Role:       ni.Role,
				IsLicensed: ni.IsLicensed,
			})

			// Keep config cache up-to-date so clients that connect later
			// receive the new node during config replay.
			c.UpsertCachedNodeInfo(ni)
		}

		// Publish traceroute responses so the dashboard can visualize the route.
		if tr := ExtractTraceroute(payload); tr != nil {
			c.metrics.PublishTraceroute(metrics.TracerouteUpdate{
				From:       tr.From,
				To:         tr.To,
				Route:      tr.Route,
				RouteBack:  tr.RouteBack,
				SnrTowards: tr.SnrTowards,
				SnrBack:    tr.SnrBack,
			})
		}

		// Config caching logic
		switch rec.Type {
		case "my_info":
			collectingConfig = true
			configFrames = [][]byte{}
			configFrames = append(configFrames, copyBytes(payload))

			// Extract and cache the node number for later use.
			if myNum := extractMyNodeNum(payload); myNum != 0 {
				c.myNodeNum.Store(myNum)
			}
		case "config_complete_id":
			configFrames = append(configFrames, copyBytes(payload))
			if collectingConfig {
				c.configMu.Lock()
				c.configCache = configFrames
				c.configMu.Unlock()
				collectingConfig = false
				c.metrics.SetConfigCacheUpdated(len(configFrames))

				// Build node directory from NodeInfo frames in the config cache.
				nodeDir := ExtractNodeDirectory(configFrames)
				c.metrics.SetNodeDirectory(nodeDir)

				c.logger.Info("node config cached",
					"frames", len(configFrames),
					"breakdown", CountCacheFrameTypes(configFrames),
					"nodes", len(nodeDir),
				)

				// Send the first heartbeat right after config handshake,
				// matching the Python CLI behavior.
				c.sendPostConfigHeartbeat()
			}
		default:
			if collectingConfig {
				configFrames = append(configFrames, copyBytes(payload))
			}
		}

		// Forward to proxy hub
		payloadCopy := copyBytes(payload)
		select {
		case c.fromNode <- payloadCopy:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (c *Connection) writeLoop(ctx context.Context, conn net.Conn) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case payload := <-c.toNode:
			if err := protocol.WriteFrame(conn, payload); err != nil {
				return fmt.Errorf("writing frame to node: %w", err)
			}
		}
	}
}

// heartbeatLoop periodically sends a heartbeat to the node to prevent
// read_timeout from triggering a reconnect when the mesh is quiet.
// The Meshtastic node responds with QueueStatus, which resets the read deadline.
//
// The first heartbeat is sent from readLoop immediately after config_complete_id
// is received (see sendPostConfigHeartbeat), matching the behavior of the
// official Meshtastic Python CLI. This loop handles only the periodic
// heartbeats after that.
func (c *Connection) heartbeatLoop(ctx context.Context) error {
	payload, err := proto.Marshal(&pb.ToRadio{
		PayloadVariant: &pb.ToRadio_Heartbeat{
			Heartbeat: &pb.Heartbeat{},
		},
	})
	if err != nil {
		return fmt.Errorf("marshaling heartbeat: %w", err)
	}

	ticker := time.NewTicker(c.heartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			c.Send(payload)
			c.logger.Debug("heartbeat sent to node")
		}
	}
}

// sendPostConfigHeartbeat sends a single heartbeat to the node right after
// config_complete_id is received. This matches the Python CLI behavior where
// the first heartbeat follows immediately after the config handshake completes,
// rather than being sent before or during config delivery.
func (c *Connection) sendPostConfigHeartbeat() {
	payload, err := proto.Marshal(&pb.ToRadio{
		PayloadVariant: &pb.ToRadio_Heartbeat{
			Heartbeat: &pb.Heartbeat{},
		},
	})
	if err != nil {
		c.logger.Error("failed to marshal post-config heartbeat", "error", err)
		return
	}
	c.Send(payload)
	c.logger.Debug("post-config heartbeat sent to node")
}

func (c *Connection) requestConfig() {
	nonce := uint32(time.Now().UnixNano() & 0xFFFFFFFF)
	toRadio := &pb.ToRadio{
		PayloadVariant: &pb.ToRadio_WantConfigId{
			WantConfigId: nonce,
		},
	}

	data, err := proto.Marshal(toRadio)
	if err != nil {
		c.logger.Error("failed to marshal want_config_id", "error", err)
		return
	}

	c.logger.Debug("requesting node config", "nonce", nonce)
	c.Send(data)
}
