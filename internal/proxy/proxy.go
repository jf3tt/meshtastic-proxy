package proxy

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sync"

	pb "buf.build/gen/go/meshtastic/protobufs/protocolbuffers/go/meshtastic"
	"github.com/jfett/meshtastic-proxy/internal/metrics"
	"github.com/jfett/meshtastic-proxy/internal/node"
	"google.golang.org/protobuf/proto"
)

// Proxy is the main hub that accepts client connections and multiplexes
// traffic between clients and the Meshtastic node.
type Proxy struct {
	listenAddr string
	maxClients int
	nodeConn   *node.Connection
	metrics    *metrics.Metrics
	logger     *slog.Logger

	mu      sync.RWMutex
	clients map[*Client]struct{}
}

// New creates a new Proxy instance.
func New(listenAddr string, maxClients int, nodeConn *node.Connection, m *metrics.Metrics, logger *slog.Logger) *Proxy {
	return &Proxy{
		listenAddr: listenAddr,
		maxClients: maxClients,
		nodeConn:   nodeConn,
		metrics:    m,
		logger:     logger,
		clients:    make(map[*Client]struct{}),
	}
}

// Run starts the proxy. It listens for client connections and
// broadcasts node frames to all connected clients. Blocks until ctx is cancelled.
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
				return nil // graceful shutdown
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

// ClientAddrs returns the remote addresses of all connected clients.
func (p *Proxy) ClientAddrs() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()

	addrs := make([]string, 0, len(p.clients))
	for c := range p.clients {
		addrs = append(addrs, c.Addr())
	}
	return addrs
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
					client.Close()
					return // do NOT forward to node
				}
			}
			// Forward all other ToRadio frames to node
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
	go client.Run(ctx)
}

func (p *Proxy) registerClient(c *Client) {
	p.mu.Lock()
	p.clients[c] = struct{}{}
	count := len(p.clients)
	p.mu.Unlock()

	p.metrics.ActiveClients.Store(int64(count))
	p.logger.Info("client connected", "client", c.Addr(), "total_clients", count)
}

func (p *Proxy) unregisterClient(c *Client) {
	p.mu.Lock()
	delete(p.clients, c)
	count := len(p.clients)
	p.mu.Unlock()

	p.metrics.ActiveClients.Store(int64(count))
	p.logger.Info("client disconnected", "client", c.Addr(), "total_clients", count)
}

// replayCachedConfig sends the cached node configuration to a client that
// has requested it via want_config_id. The ConfigCompleteId nonce in the
// cache is replaced with the client's nonce so the client accepts the
// config sequence. This is called from the client's readLoop goroutine
// (via onMessage), so the write loop is already running and frames are
// delivered through the send channel.
func (p *Proxy) replayCachedConfig(c *Client, clientNonce uint32) {
	frames := p.nodeConn.ConfigCache()
	if len(frames) == 0 {
		p.logger.Debug("no cached config for replay", "client", c.Addr())
		return
	}

	sent := 0
	for _, frame := range frames {
		// Check if this frame is ConfigCompleteId and replace the nonce
		outFrame := frame
		msg := &pb.FromRadio{}
		if err := proto.Unmarshal(frame, msg); err == nil {
			if _, ok := msg.GetPayloadVariant().(*pb.FromRadio_ConfigCompleteId); ok {
				msg.PayloadVariant = &pb.FromRadio_ConfigCompleteId{
					ConfigCompleteId: clientNonce,
				}
				if patched, err := proto.Marshal(msg); err == nil {
					outFrame = patched
				}
			}
		}

		if !c.Send(outFrame) {
			p.logger.Debug("replay interrupted, client disconnected",
				"client", c.Addr(),
				"sent", sent,
				"total", len(frames),
			)
			return
		}
		sent++
	}

	p.logger.Debug("replayed cached config to client",
		"client", c.Addr(),
		"frames", sent,
		"client_nonce", clientNonce,
	)
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
		// Make a copy for each client
		cp := make([]byte, len(payload))
		copy(cp, payload)
		c.Send(cp)
	}
}

// decodeToRadioType unmarshals a ToRadio protobuf for type inspection.
func decodeToRadioType(payload []byte) (*pb.ToRadio, error) {
	msg := &pb.ToRadio{}
	return msg, proto.Unmarshal(payload, msg)
}
