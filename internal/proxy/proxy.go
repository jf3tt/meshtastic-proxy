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

// Special nonces used by the iOS Meshtastic app to request partial config.
// See firmware PhoneAPI.h: SPECIAL_NONCE_ONLY_CONFIG / SPECIAL_NONCE_ONLY_NODES.
const (
	nonceOnlyConfig = 69420 // config + channels + modules, skip NodeInfo DB
	nonceOnlyNodes  = 69421 // NodeInfo DB only, skip config
)

// NodeConnection defines the interface for the node connection used by Proxy.
// This allows substituting a mock in tests.
type NodeConnection interface {
	ConfigCache() [][]byte
	FromNode() <-chan []byte
	Send(payload []byte)
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

	// Filter frames based on special nonces (iOS two-phase config).
	result := filterConfigCache(frames, clientNonce)

	// Determine request type for logging and metrics.
	reqType := "full"
	switch clientNonce {
	case nonceOnlyConfig:
		reqType = "config_only"
		p.metrics.ConfigReplaysConfigOnly.Add(1)
	case nonceOnlyNodes:
		reqType = "nodes_only"
		p.metrics.ConfigReplaysNodesOnly.Add(1)
	default:
		p.metrics.ConfigReplaysFull.Add(1)
	}

	// Log filter diagnostics.
	p.logger.Debug("config cache filtered",
		"client", c.Addr(),
		"nonce", clientNonce,
		"type", reqType,
		"my_node_num", fmt.Sprintf("!%08x", result.Stats.MyNodeNum),
		"own_node_found", result.Stats.OwnNodeFound,
		"frame_counts", result.Stats.FrameCounts,
		"filtered_frames", len(result.Frames),
		"total_cached", len(frames),
	)

	sent := 0
	for _, pf := range result.Frames {
		outFrame := pf.Raw

		if pf.Msg != nil {
			p.logger.Debug("replaying frame",
				"client", c.Addr(),
				"seq", sent,
				"type", node.FromRadioTypeName(pf.Msg),
			)

			if _, ok := pf.Msg.GetPayloadVariant().(*pb.FromRadio_ConfigCompleteId); ok {
				// Replace nonce with the client's nonce.
				patched, ok := proto.Clone(pf.Msg).(*pb.FromRadio)
				if !ok {
					continue
				}
				patched.PayloadVariant = &pb.FromRadio_ConfigCompleteId{
					ConfigCompleteId: clientNonce,
				}
				if raw, err := proto.Marshal(patched); err == nil {
					outFrame = raw
				}
			}
		}

		if !c.Send(outFrame) {
			p.logger.Debug("replay interrupted, client disconnected",
				"client", c.Addr(),
				"sent", sent,
				"total", len(result.Frames),
			)
			return
		}
		sent++

		// After sending the connected node's own NodeInfo during config-only
		// replay, pause briefly so the iOS app's CoreData viewContext can merge
		// the newly created NodeInfoEntity before ConfigCompleteId arrives.
		if clientNonce == nonceOnlyConfig && pf.Msg != nil {
			if ni, ok := pf.Msg.GetPayloadVariant().(*pb.FromRadio_NodeInfo); ok &&
				ni.NodeInfo.GetNum() == result.Stats.MyNodeNum {
				p.logger.Debug("pausing after own NodeInfo for CoreData sync",
					"client", c.Addr(),
					"delay", p.iosNodeInfoDelay,
				)
				time.Sleep(p.iosNodeInfoDelay)
			}
		}
	}

	p.logger.Debug("replayed cached config to client",
		"client", c.Addr(),
		"frames", sent,
		"total_cached", len(frames),
		"type", reqType,
		"client_nonce", clientNonce,
	)
}

// filterStats contains diagnostic information about a filterConfigCache operation.
type filterStats struct {
	MyNodeNum    uint32
	OwnNodeFound bool
	FrameCounts  map[string]int
}

// parsedFrame holds a raw frame alongside its pre-parsed protobuf message.
// If Msg is nil, the frame could not be parsed.
type parsedFrame struct {
	Raw []byte
	Msg *pb.FromRadio
}

// filterResult contains filtered config frames and diagnostic statistics.
type filterResult struct {
	Frames []parsedFrame
	Stats  filterStats
}

// filterConfigCache returns a subset of cached config frames based on the
// client's nonce. The firmware (PhoneAPI.cpp) supports two special nonces:
//   - nonceOnlyConfig (69420): config frames only, skip other nodes' NodeInfo
//   - nonceOnlyNodes  (69421): NodeInfo frames only, skip config
//
// Any other nonce returns all frames unmodified (full config).
// The ConfigCompleteId frame is always included.
// The returned filterResult includes diagnostic statistics about the filtering.
func filterConfigCache(frames [][]byte, nonce uint32) filterResult {
	stats := filterStats{
		FrameCounts: make(map[string]int),
	}

	// Parse all frames once upfront.
	parsed := make([]parsedFrame, len(frames))
	for i, frame := range frames {
		msg := &pb.FromRadio{}
		if err := proto.Unmarshal(frame, msg); err != nil {
			parsed[i] = parsedFrame{Raw: frame, Msg: nil}
		} else {
			parsed[i] = parsedFrame{Raw: frame, Msg: msg}
		}
	}

	if nonce != nonceOnlyConfig && nonce != nonceOnlyNodes {
		// Full config — count frame types for diagnostics.
		for _, pf := range parsed {
			if pf.Msg == nil {
				stats.FrameCounts["unparseable"]++
			} else {
				stats.FrameCounts[node.FromRadioTypeName(pf.Msg)]++
			}
		}
		return filterResult{Frames: parsed, Stats: stats}
	}

	// Find my_node_num so we can identify own NodeInfo.
	for _, pf := range parsed {
		if pf.Msg == nil {
			continue
		}
		if v, ok := pf.Msg.GetPayloadVariant().(*pb.FromRadio_MyInfo); ok {
			stats.MyNodeNum = v.MyInfo.GetMyNodeNum()
			break
		}
	}

	result := make([]parsedFrame, 0, len(parsed))
	for _, pf := range parsed {
		if pf.Msg == nil {
			result = append(result, pf) // keep unparseable frames
			stats.FrameCounts["unparseable"]++
			continue
		}

		typeName := node.FromRadioTypeName(pf.Msg)

		switch v := pf.Msg.GetPayloadVariant().(type) {
		case *pb.FromRadio_ConfigCompleteId:
			// Always included — nonce is patched later by replayCachedConfig.
			result = append(result, pf)
			stats.FrameCounts[typeName]++

		case *pb.FromRadio_NodeInfo:
			if nonce == nonceOnlyConfig {
				// Config-only: include own NodeInfo, skip others.
				if v.NodeInfo.GetNum() == stats.MyNodeNum {
					result = append(result, pf)
					stats.OwnNodeFound = true
					stats.FrameCounts[typeName]++
				}
			} else {
				// Nodes-only: include all NodeInfo.
				result = append(result, pf)
				stats.FrameCounts[typeName]++
			}

		case *pb.FromRadio_MyInfo,
			*pb.FromRadio_DeviceuiConfig,
			*pb.FromRadio_Metadata,
			*pb.FromRadio_Channel,
			*pb.FromRadio_Config,
			*pb.FromRadio_ModuleConfig,
			*pb.FromRadio_FileInfo:
			// Config frames — include for config-only, skip for nodes-only.
			if nonce == nonceOnlyConfig {
				result = append(result, pf)
				stats.FrameCounts[typeName]++
			}

		default:
			// Unknown types: include for config-only (conservative).
			if nonce == nonceOnlyConfig {
				result = append(result, pf)
				stats.FrameCounts[typeName]++
			}
		}
	}
	return filterResult{Frames: result, Stats: stats}
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

// decodeToRadioType unmarshals a ToRadio protobuf for type inspection.
func decodeToRadioType(payload []byte) (*pb.ToRadio, error) {
	msg := &pb.ToRadio{}
	return msg, proto.Unmarshal(payload, msg)
}
