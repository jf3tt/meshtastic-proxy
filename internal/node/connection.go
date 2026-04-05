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
		c.logger.Info("connected to node", "address", c.address)

		// Request config from node
		c.requestConfig()

		// Run read/write loops
		err = c.runConnection(ctx, conn)
		if err != nil {
			c.logger.Error("node connection error", "error", err)
		}

		c.metrics.NodeConnected.Store(false)
		c.close()
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
		Timeout: c.dialTimeout,
	}
	conn, err := d.DialContext(ctx, "tcp", c.address)
	if err != nil {
		return nil, fmt.Errorf("dialing %s: %w", c.address, err)
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

func (c *Connection) runConnection(ctx context.Context, conn net.Conn) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	errCh := make(chan error, 2)

	// Reader goroutine
	go func() {
		errCh <- c.readLoop(ctx, conn)
	}()

	// Writer goroutine
	go func() {
		errCh <- c.writeLoop(ctx, conn)
	}()

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
		}

		// Publish traceroute responses so the dashboard can visualize the route.
		if tr := ExtractTraceroute(payload); tr != nil {
			c.metrics.PublishTraceroute(metrics.TracerouteUpdate{
				From:      tr.From,
				To:        tr.To,
				Route:     tr.Route,
				RouteBack: tr.RouteBack,
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
