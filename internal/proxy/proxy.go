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
	nodeConn          NodeConnection
	metrics           *metrics.Metrics
	logger            *slog.Logger

	mu      sync.RWMutex
	clients map[*Client]struct{}
	wg      sync.WaitGroup // tracks client goroutines for graceful shutdown
}

// Options holds all configuration for creating a new Proxy.
type Options struct {
	ListenAddr        string
	MaxClients        int
	ClientSendBuffer  int
	ClientIdleTimeout time.Duration
	IOSNodeInfoDelay  time.Duration
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
	p.mu.RLock()
	count := len(p.clients)
	p.mu.RUnlock()

	if count >= p.maxClients {
		p.logger.Warn("max clients reached, rejecting connection",
			"client", conn.RemoteAddr(),
			"max", p.maxClients,
		)
		_ = conn.Close()
		return
	}

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
			p.nodeConn.Send(payload)
		},
		func(c *Client) {
			// Unregister client on disconnect
			p.unregisterClient(c)
		},
	)

	p.registerClient(client)

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
func (p *Proxy) broadcastLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case payload := <-p.nodeConn.FromNode():
			// Log config-related frames for debugging multi-client issues
			msg := &pb.FromRadio{}
			if err := proto.Unmarshal(payload, msg); err == nil {
				switch v := msg.GetPayloadVariant().(type) {
				case *pb.FromRadio_MyInfo:
					p.logger.Debug("broadcasting my_info from node",
						"node_num", v.MyInfo.GetMyNodeNum(),
					)
				case *pb.FromRadio_ConfigCompleteId:
					p.logger.Debug("broadcasting config_complete_id from node",
						"nonce", v.ConfigCompleteId,
					)
				}
			}
			p.broadcast(payload)
		}
	}
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
