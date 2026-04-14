package proxy

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	pb "buf.build/gen/go/meshtastic/protobufs/protocolbuffers/go/meshtastic"
	"google.golang.org/protobuf/proto"

	"github.com/jfett/meshtastic-proxy/internal/metrics"
	"github.com/jfett/meshtastic-proxy/internal/node"
)

// NodeConnection defines the interface for the node connection used by Proxy.
// This allows substituting a mock in tests.
type NodeConnection interface {
	ConfigCache() [][]byte
	FromNode() <-chan []byte
	Send(payload []byte)
	MyNodeNum() uint32
}

// Proxy is the main hub that accepts client connections and multiplexes
// traffic between clients and the Meshtastic node.
type Proxy struct {
	listenAddr        string
	maxClients        int
	clientSendBuffer  int
	clientIdleTimeout time.Duration
	iosNodeInfoDelay  time.Duration
	maxChatCache      int
	replayChatEnabled bool
	nodeConn          NodeConnection
	metrics           *metrics.Metrics
	logger            *slog.Logger

	mu      sync.RWMutex
	clients map[*Client]struct{}
	wg      sync.WaitGroup // tracks client goroutines for graceful shutdown

	// chatMu protects chatCache.
	chatMu    sync.RWMutex
	chatCache []chatEntry // ring buffer of cached chat messages with optional ACKs
}

// chatEntry holds a cached text message alongside its optional routing ACK.
// The ACK may arrive after the text message; cacheACK fills it in later.
type chatEntry struct {
	message  []byte // FromRadio text message protobuf payload
	packetID uint32 // MeshPacket.Id for matching the routing ACK
	ack      []byte // FromRadio routing ACK protobuf payload (nil until ACK arrives)
}

// Options holds all configuration for creating a new Proxy.
type Options struct {
	ListenAddr        string
	MaxClients        int
	ClientSendBuffer  int
	ClientIdleTimeout time.Duration
	IOSNodeInfoDelay  time.Duration
	MaxChatCache      int
	ReplayChatHistory bool
	NodeConn          NodeConnection
	Metrics           *metrics.Metrics
	Logger            *slog.Logger
}

// New creates a new Proxy instance.
func New(opts Options) *Proxy {
	return &Proxy{
		listenAddr:        opts.ListenAddr,
		maxClients:        opts.MaxClients,
		clientSendBuffer:  opts.ClientSendBuffer,
		clientIdleTimeout: opts.ClientIdleTimeout,
		iosNodeInfoDelay:  opts.IOSNodeInfoDelay,
		maxChatCache:      opts.MaxChatCache,
		replayChatEnabled: opts.ReplayChatHistory,
		nodeConn:          opts.NodeConn,
		metrics:           opts.Metrics,
		logger:            opts.Logger,
		clients:           make(map[*Client]struct{}),
	}
}

// Run starts the proxy. It listens for client connections and
// broadcasts node frames to all connected clients. Blocks until ctx is canceled.
func (p *Proxy) Run(ctx context.Context) error {
	// Start the broadcast pump in background
	go p.broadcastLoop(ctx)

	// Listen for client connections
	lc := net.ListenConfig{}
	listener, err := lc.Listen(ctx, "tcp", p.listenAddr)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", p.listenAddr, err)
	}
	defer func() { _ = listener.Close() }()

	p.logger.Info("proxy listening for clients", "address", p.listenAddr)

	// Accept connections in a loop
	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()

	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				// Graceful shutdown: close all active clients and wait.
				p.closeAllClients()
				p.wg.Wait()
				return nil
			}
			p.logger.Error("accept error", "error", err)
			continue
		}

		p.handleNewConnection(ctx, conn)
	}
}

// Clients returns the current number of connected clients.
func (p *Proxy) Clients() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.clients)
}

// SendToNode sends a raw ToRadio payload to the connected Meshtastic node.
// Used by the web chat to send outgoing messages.
func (p *Proxy) SendToNode(payload []byte) {
	p.nodeConn.Send(payload)
}

// ClientAddrs returns the remote addresses of all connected clients.
func (p *Proxy) ClientAddrs() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()

	return p.clientAddrsLocked()
}

func (p *Proxy) handleNewConnection(ctx context.Context, conn net.Conn) {
	// Create client with callbacks.
	// Declare client first so the onMessage closure can reference it.
	var client *Client
	client = NewClient(
		conn,
		p.logger,
		p.metrics,
		p.clientSendBuffer,
		p.clientIdleTimeout,
		func(payload []byte) {
			// Intercept client-originated ToRadio frames that must not
			// reach the node: want_config_id (answered from cache) and
			// disconnect (handled locally).
			if msg, err := decodeToRadioType(payload); err == nil {
				switch v := msg.GetPayloadVariant().(type) {
				case *pb.ToRadio_WantConfigId:
					p.logger.Debug("client sent want_config_id, replying from cache",
						"client", client.Addr(),
						"nonce", v.WantConfigId,
					)
					go p.replayCachedConfig(client, v.WantConfigId)
					return // do NOT forward to node
				case *pb.ToRadio_Disconnect:
					p.logger.Debug("client sent disconnect, closing client",
						"client", client.Addr(),
					)
					client.SetDisconnectReason(DisconnectReasonClientSent)
					client.Close()
					return // do NOT forward to node
				}
			}
			// Forward all other ToRadio frames to node.
			// Echo MeshPackets to other connected clients so they see
			// messages sent by their peers through the shared node.
			p.echoToOtherClients(payload, client)
			// Cache outgoing text messages for replay to new/reconnecting clients.
			if fromRadioPayload := p.toRadioToFromRadio(payload); fromRadioPayload != nil {
				if isTextMessageFrame(fromRadioPayload) {
					p.cacheTextMessage(fromRadioPayload)
				}
			}
			p.nodeConn.Send(payload)
		},
		func(c *Client) {
			// Unregister client on disconnect
			p.unregisterClient(c)
		},
	)

	// Atomically check max clients and register under the same lock
	// to prevent TOCTOU races where concurrent accepts exceed the limit.
	p.mu.Lock()
	if len(p.clients) >= p.maxClients {
		p.mu.Unlock()
		p.logger.Warn("max clients reached, rejecting connection",
			"client", conn.RemoteAddr(),
			"max", p.maxClients,
		)
		_ = conn.Close()
		return
	}
	p.clients[client] = struct{}{}
	count := len(p.clients)
	addrs := p.clientAddrsLocked()
	p.mu.Unlock()

	p.metrics.ActiveClients.Store(int64(count))
	p.metrics.PublishClients(addrs)
	p.logger.Info("client connected", "client", client.Addr(), "total_clients", count)

	// Start the client read/write loops immediately. Config delivery
	// is deferred until the client sends want_config_id (which all
	// standard Meshtastic clients do after TCP connect). The onMessage
	// callback above intercepts that request and replies from cache
	// with the client's nonce via replayCachedConfig.
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		client.Run(ctx)
	}()
}

func (p *Proxy) registerClient(c *Client) {
	p.mu.Lock()
	p.clients[c] = struct{}{}
	count := len(p.clients)
	addrs := p.clientAddrsLocked()
	p.mu.Unlock()

	p.metrics.ActiveClients.Store(int64(count))
	p.metrics.PublishClients(addrs)
	p.logger.Info("client connected", "client", c.Addr(), "total_clients", count)
}

func (p *Proxy) unregisterClient(c *Client) {
	p.mu.Lock()
	delete(p.clients, c)
	count := len(p.clients)
	addrs := p.clientAddrsLocked()
	p.mu.Unlock()

	reason := c.GetDisconnectReason()
	if reason == "" {
		reason = DisconnectReasonServerClose
	}

	p.metrics.ActiveClients.Store(int64(count))
	p.metrics.PublishClients(addrs)
	p.logger.Info("client disconnected",
		"client", c.Addr(),
		"reason", string(reason),
		"session_duration", c.SessionDuration().Round(time.Second).String(),
		"total_clients", count,
	)
}

// clientAddrsLocked returns client addresses. Caller must hold p.mu.
func (p *Proxy) clientAddrsLocked() []string {
	addrs := make([]string, 0, len(p.clients))
	for c := range p.clients {
		addrs = append(addrs, c.Addr())
	}
	return addrs
}

// closeAllClients closes all active client connections for graceful shutdown.
func (p *Proxy) closeAllClients() {
	p.mu.RLock()
	clients := make([]*Client, 0, len(p.clients))
	for c := range p.clients {
		clients = append(clients, c)
	}
	p.mu.RUnlock()

	for _, c := range clients {
		c.Close()
	}
}

// broadcastLoop reads frames from the node and broadcasts them to all clients.
//
// Config frames (MyInfo, Config, ModuleConfig, Channel, NodeInfo, Metadata,
// ConfigCompleteId, DeviceuiConfig, FileInfo) are NOT broadcast. Clients
// receive their config exclusively via replayCachedConfig when they send
// want_config_id. Broadcasting config frames would corrupt the iOS app's
// state machine — especially during node reconnects, where 170+ config
// frames (including ConfigCompleteId with the proxy's own nonce) would be
// delivered to already-connected clients that never requested them.
func (p *Proxy) broadcastLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case payload, ok := <-p.nodeConn.FromNode():
			if !ok {
				p.logger.Info("node channel closed, stopping broadcast loop")
				return
			}
			if isConfigFrame(payload) {
				p.logger.Debug("suppressing config frame from broadcast",
					"type", classifyFromRadio(payload),
				)
				continue
			}
			// Cache incoming text messages for replay to new/reconnecting clients.
			if isTextMessageFrame(payload) {
				p.cacheTextMessage(payload)
			} else if packetID, ok := isRoutingACKFrame(payload); ok {
				// Associate routing ACK with a previously cached text message.
				p.cacheACK(packetID, payload)
			}
			p.broadcast(payload)
		}
	}
}

// isConfigFrame returns true if the payload is a FromRadio config frame
// that should NOT be broadcast to clients. Config frames are delivered
// exclusively through replayCachedConfig.
//
// Config frame types: MyInfo, NodeInfo, Config, ModuleConfig, Channel,
// ConfigCompleteId, Metadata, DeviceuiConfig, FileInfo.
//
// Runtime frame types (broadcast normally): Packet, QueueStatus, LogRecord,
// Rebooted, MqttClientProxyMessage, XModem, and any unknown/unparseable
// frames.
func isConfigFrame(payload []byte) bool {
	msg := &pb.FromRadio{}
	if err := proto.Unmarshal(payload, msg); err != nil {
		return false // unparseable → treat as runtime, broadcast it
	}

	switch msg.GetPayloadVariant().(type) {
	case *pb.FromRadio_MyInfo,
		*pb.FromRadio_NodeInfo,
		*pb.FromRadio_Config,
		*pb.FromRadio_ModuleConfig,
		*pb.FromRadio_Channel,
		*pb.FromRadio_ConfigCompleteId,
		*pb.FromRadio_Metadata,
		*pb.FromRadio_DeviceuiConfig,
		*pb.FromRadio_FileInfo:
		return true
	default:
		return false
	}
}

// classifyFromRadio returns a short type name for a FromRadio payload.
// Used for debug logging only.
func classifyFromRadio(payload []byte) string {
	msg := &pb.FromRadio{}
	if err := proto.Unmarshal(payload, msg); err != nil {
		return "unparseable"
	}
	return node.FromRadioTypeName(msg)
}

func (p *Proxy) broadcast(payload []byte) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	for c := range p.clients {
		// Safe to share the same slice: payload from the node channel is
		// never mutated after being received, and WriteFrame copies it
		// into a new frame buffer before writing.
		c.Send(payload)
	}
}

// broadcastToOthers sends a payload to all connected clients except the sender.
// Used to echo outgoing MeshPackets so that other clients see messages sent
// by their peers through the shared node connection.
func (p *Proxy) broadcastToOthers(payload []byte, sender *Client) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	for c := range p.clients {
		if c != sender {
			c.Send(payload)
		}
	}
}

// echoToOtherClients converts an outgoing ToRadio MeshPacket into a FromRadio
// frame and delivers it to all clients except the sender. This ensures that
// when multiple clients share the same node, messages sent by one client are
// visible to the others. If the MeshPacket has From == 0 (the node normally
// fills this in), the proxy substitutes the node's own number.
func (p *Proxy) echoToOtherClients(payload []byte, sender *Client) {
	msg := &pb.ToRadio{}
	if err := proto.Unmarshal(payload, msg); err != nil {
		return
	}

	pkt, ok := msg.GetPayloadVariant().(*pb.ToRadio_Packet)
	if !ok || pkt.Packet == nil {
		return // not a MeshPacket (e.g. heartbeat) — nothing to echo
	}

	// Clone the packet so we don't mutate the original payload's
	// deserialized message if it's referenced elsewhere.
	meshPkt, ok := proto.Clone(pkt.Packet).(*pb.MeshPacket)
	if !ok {
		return
	}

	// Fill in From if the client left it as 0 (node normally fills it).
	if meshPkt.GetFrom() == 0 {
		if nodeNum := p.nodeConn.MyNodeNum(); nodeNum != 0 {
			meshPkt.From = nodeNum
		}
	}

	// Set RxTime if not already populated. The firmware normally sets this
	// on received packets; for outgoing packets echoed to other clients the
	// proxy fills it in so that iOS (which sorts messages by RxTime) shows
	// them in chronological order.
	if meshPkt.GetRxTime() == 0 {
		meshPkt.RxTime = uint32(time.Now().Unix()) //nolint:gosec // G115: Unix timestamp fits uint32 until 2106
	}

	fromRadio := &pb.FromRadio{
		PayloadVariant: &pb.FromRadio_Packet{
			Packet: meshPkt,
		},
	}

	data, err := proto.Marshal(fromRadio)
	if err != nil {
		p.logger.Warn("failed to marshal echo FromRadio", "error", err)
		return
	}

	p.broadcastToOthers(data, sender)
}

// decodeToRadioType unmarshals a ToRadio protobuf for type inspection.
func decodeToRadioType(payload []byte) (*pb.ToRadio, error) {
	msg := &pb.ToRadio{}
	return msg, proto.Unmarshal(payload, msg)
}

// isTextMessageFrame returns true if the FromRadio payload contains a decoded
// MeshPacket with PortNum_TEXT_MESSAGE_APP. Used to identify chat messages
// for caching.
func isTextMessageFrame(payload []byte) bool {
	msg := &pb.FromRadio{}
	if err := proto.Unmarshal(payload, msg); err != nil {
		return false
	}
	pkt, ok := msg.GetPayloadVariant().(*pb.FromRadio_Packet)
	if !ok || pkt.Packet == nil {
		return false
	}
	decoded, ok := pkt.Packet.GetPayloadVariant().(*pb.MeshPacket_Decoded)
	if !ok || decoded.Decoded == nil {
		return false
	}
	return decoded.Decoded.GetPortnum() == pb.PortNum_TEXT_MESSAGE_APP
}

// toRadioToFromRadio converts a ToRadio MeshPacket payload into a FromRadio
// payload suitable for caching. If the MeshPacket has From == 0, the node's
// own number is substituted. Returns nil if the payload is not a ToRadio
// MeshPacket or cannot be converted.
func (p *Proxy) toRadioToFromRadio(payload []byte) []byte {
	msg := &pb.ToRadio{}
	if err := proto.Unmarshal(payload, msg); err != nil {
		return nil
	}

	pkt, ok := msg.GetPayloadVariant().(*pb.ToRadio_Packet)
	if !ok || pkt.Packet == nil {
		return nil
	}

	meshPkt, ok := proto.Clone(pkt.Packet).(*pb.MeshPacket)
	if !ok {
		return nil
	}

	if meshPkt.GetFrom() == 0 {
		if nodeNum := p.nodeConn.MyNodeNum(); nodeNum != 0 {
			meshPkt.From = nodeNum
		}
	}

	// Set RxTime if not already populated. The firmware normally sets this
	// on received packets; for outgoing packets converted to FromRadio for
	// caching the proxy fills it in so that iOS (which sorts messages by
	// RxTime) shows them in chronological order during chat replay.
	if meshPkt.GetRxTime() == 0 {
		meshPkt.RxTime = uint32(time.Now().Unix()) //nolint:gosec // G115: Unix timestamp fits uint32 until 2106
	}

	fromRadio := &pb.FromRadio{
		PayloadVariant: &pb.FromRadio_Packet{
			Packet: meshPkt,
		},
	}

	data, err := proto.Marshal(fromRadio)
	if err != nil {
		return nil
	}
	return data
}

// cacheTextMessage appends a FromRadio text message payload to the chat cache.
// If the cache exceeds maxChatCache, the oldest entries are dropped.
// Does nothing if maxChatCache is 0 (disabled).
func (p *Proxy) cacheTextMessage(payload []byte) {
	if p.maxChatCache <= 0 {
		return
	}

	// Make a copy so we don't retain references to the original buffer.
	cp := make([]byte, len(payload))
	copy(cp, payload)

	// Extract MeshPacket.Id for ACK matching.
	packetID := extractPacketID(payload)

	p.chatMu.Lock()
	p.chatCache = append(p.chatCache, chatEntry{
		message:  cp,
		packetID: packetID,
	})
	if len(p.chatCache) > p.maxChatCache {
		// Drop oldest entries beyond the limit.
		excess := len(p.chatCache) - p.maxChatCache
		// Zero out dropped slice elements to allow GC.
		for i := 0; i < excess; i++ {
			p.chatCache[i] = chatEntry{}
		}
		p.chatCache = p.chatCache[excess:]
	}
	p.chatMu.Unlock()
}

// chatCacheSnapshot returns a copy of the current chat cache.
func (p *Proxy) chatCacheSnapshot() []chatEntry {
	p.chatMu.RLock()
	defer p.chatMu.RUnlock()

	if len(p.chatCache) == 0 {
		return nil
	}

	result := make([]chatEntry, len(p.chatCache))
	for i, entry := range p.chatCache {
		msgCp := make([]byte, len(entry.message))
		copy(msgCp, entry.message)
		e := chatEntry{
			message:  msgCp,
			packetID: entry.packetID,
		}
		if entry.ack != nil {
			ackCp := make([]byte, len(entry.ack))
			copy(ackCp, entry.ack)
			e.ack = ackCp
		}
		result[i] = e
	}
	return result
}

// extractPacketID extracts MeshPacket.Id from a FromRadio protobuf payload.
// Returns 0 if the payload cannot be parsed or is not a MeshPacket.
func extractPacketID(payload []byte) uint32 {
	msg := &pb.FromRadio{}
	if err := proto.Unmarshal(payload, msg); err != nil {
		return 0
	}
	pkt, ok := msg.GetPayloadVariant().(*pb.FromRadio_Packet)
	if !ok || pkt.Packet == nil {
		return 0
	}
	return pkt.Packet.GetId()
}

// isRoutingACKFrame checks whether the FromRadio payload is a routing ACK
// (PortNum_ROUTING_APP with ErrorReason == NONE and a non-zero RequestId).
// Returns the RequestId (which matches the original message's MeshPacket.Id)
// and true if this is an ACK; otherwise returns 0, false.
func isRoutingACKFrame(payload []byte) (packetID uint32, ok bool) {
	msg := &pb.FromRadio{}
	if err := proto.Unmarshal(payload, msg); err != nil {
		return 0, false
	}
	pkt, isPkt := msg.GetPayloadVariant().(*pb.FromRadio_Packet)
	if !isPkt || pkt.Packet == nil {
		return 0, false
	}
	decoded, isDec := pkt.Packet.GetPayloadVariant().(*pb.MeshPacket_Decoded)
	if !isDec || decoded.Decoded == nil {
		return 0, false
	}
	if decoded.Decoded.GetPortnum() != pb.PortNum_ROUTING_APP {
		return 0, false
	}
	reqID := decoded.Decoded.GetRequestId()
	if reqID == 0 {
		return 0, false
	}
	// Parse the Routing protobuf to check ErrorReason.
	routing := &pb.Routing{}
	if err := proto.Unmarshal(decoded.Decoded.GetPayload(), routing); err != nil {
		return 0, false
	}
	errReason, isErr := routing.GetVariant().(*pb.Routing_ErrorReason)
	if !isErr {
		return 0, false
	}
	if errReason.ErrorReason != pb.Routing_NONE {
		return 0, false // NACK — not a successful ACK
	}
	return reqID, true
}

// cacheACK associates a routing ACK payload with a previously cached text
// message that has the matching packetID. Searches from the end of the cache
// (ACKs typically arrive shortly after the message). Does nothing if no match
// is found or if the entry already has an ACK.
func (p *Proxy) cacheACK(packetID uint32, payload []byte) {
	if p.maxChatCache <= 0 || packetID == 0 {
		return
	}

	cp := make([]byte, len(payload))
	copy(cp, payload)

	p.chatMu.Lock()
	defer p.chatMu.Unlock()

	// Search from the end — ACK usually arrives soon after the message.
	for i := len(p.chatCache) - 1; i >= 0; i-- {
		if p.chatCache[i].packetID == packetID && p.chatCache[i].ack == nil {
			p.chatCache[i].ack = cp
			return
		}
	}
}
