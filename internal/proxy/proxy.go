package proxy

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sync"

	"github.com/jfett/meshtastic-proxy/internal/metrics"
	"github.com/jfett/meshtastic-proxy/internal/node"
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

	// Create client with callbacks
	client := NewClient(
		conn,
		p.logger,
		p.metrics,
		func(payload []byte) {
			// Forward ToRadio from client to node
			p.nodeConn.Send(payload)
		},
		func(c *Client) {
			// Unregister client on disconnect
			p.unregisterClient(c)
		},
	)

	p.registerClient(client)

	// Send cached config to new client
	go func() {
		p.sendCachedConfig(client)
		client.Run(ctx)
	}()
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

func (p *Proxy) sendCachedConfig(c *Client) {
	frames := p.nodeConn.ConfigCache()
	if len(frames) == 0 {
		p.logger.Debug("no cached config to send", "client", c.Addr())
		return
	}

	p.logger.Debug("sending cached config to client", "client", c.Addr(), "frames", len(frames))
	for _, frame := range frames {
		if !c.Send(frame) {
			return // client disconnected or buffer full
		}
	}
}

// broadcastLoop reads frames from the node and broadcasts them to all clients.
func (p *Proxy) broadcastLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case payload := <-p.nodeConn.FromNode():
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
