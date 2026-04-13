package proxy

import (
	"bufio"
	"context"
	"errors"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jfett/meshtastic-proxy/internal/metrics"
	"github.com/jfett/meshtastic-proxy/internal/protocol"
)

// DisconnectReason describes why a client disconnected.
type DisconnectReason string

const (
	DisconnectReasonReadError    DisconnectReason = "read_error"
	DisconnectReasonIdleTimeout  DisconnectReason = "idle_timeout"
	DisconnectReasonSlowConsumer DisconnectReason = "slow_consumer"
	DisconnectReasonClientSent   DisconnectReason = "client_disconnect" // client sent ToRadio.Disconnect
	DisconnectReasonServerClose  DisconnectReason = "server_close"      // proxy shutdown or max clients
)

// Client represents a single connected TCP client.
type Client struct {
	conn        net.Conn
	addr        string
	logger      *slog.Logger
	m           *metrics.Metrics
	idleTimeout time.Duration
	connectedAt time.Time

	// send is a buffered channel for outgoing frames to this client
	send chan []byte

	// onMessage is called when the client sends a ToRadio frame
	onMessage func(payload []byte)

	// onClose is called when the client disconnects
	onClose func(c *Client)

	closeOnce sync.Once

	// wg tracks the read/write loop goroutines launched by Start.
	wg sync.WaitGroup

	// cancel stops the read/write loops.
	cancel context.CancelFunc

	// disconnectReason records why this client was disconnected.
	disconnectMu     sync.Mutex
	disconnectReason DisconnectReason

	// configPhase tracks which iOS config phases have been completed.
	// Bit 0 (1): seen nonce 69420 (config-only phase).
	// Bit 1 (2): seen nonce 69421 (nodes-only phase).
	// Used to determine when to replay chat history for iOS clients.
	configPhase atomic.Uint32

	// replayMu serializes concurrent replayCachedConfig calls.
	// Prevents interleaved config frames if a client sends two
	// want_config_id requests before the first replay completes.
	replayMu sync.Mutex
}

// NewClient creates a new client handler.
func NewClient(conn net.Conn, logger *slog.Logger, m *metrics.Metrics, sendBuffer int, idleTimeout time.Duration, onMessage func([]byte), onClose func(*Client)) *Client {
	return &Client{
		conn:        conn,
		addr:        conn.RemoteAddr().String(),
		logger:      logger.With("client", conn.RemoteAddr().String()),
		m:           m,
		idleTimeout: idleTimeout,
		connectedAt: time.Now(),
		send:        make(chan []byte, sendBuffer),
		onMessage:   onMessage,
		onClose:     onClose,
	}
}

// Addr returns the remote address of the client.
func (c *Client) Addr() string {
	return c.addr
}

// SetDisconnectReason records the reason for disconnect. Only the first
// call takes effect (subsequent calls are ignored).
func (c *Client) SetDisconnectReason(reason DisconnectReason) {
	c.disconnectMu.Lock()
	defer c.disconnectMu.Unlock()
	if c.disconnectReason == "" {
		c.disconnectReason = reason
	}
}

// GetDisconnectReason returns the recorded disconnect reason.
func (c *Client) GetDisconnectReason() DisconnectReason {
	c.disconnectMu.Lock()
	defer c.disconnectMu.Unlock()
	return c.disconnectReason
}

// SessionDuration returns the time elapsed since the client connected.
func (c *Client) SessionDuration() time.Duration {
	return time.Since(c.connectedAt)
}

// Send queues a frame for delivery to the client.
// Returns false if the client's buffer is full (slow consumer).
func (c *Client) Send(payload []byte) bool {
	select {
	case c.send <- payload:
		return true
	default:
		c.logger.Warn("client send buffer full, disconnecting slow consumer")
		c.SetDisconnectReason(DisconnectReasonSlowConsumer)
		c.Close()
		return false
	}
}

// Start launches the client read and write loops as background goroutines.
// It returns immediately. The write loop begins draining the send channel
// as soon as Start returns, so it is safe to call Send (including cached
// config replay) after Start without risking a "slow consumer" disconnect.
// Call Wait to block until the client disconnects.
func (c *Client) Start(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	c.cancel = cancel

	c.wg.Add(2)

	// Writer goroutine: send frames to client
	go func() {
		defer c.wg.Done()
		c.writeLoop(ctx)
		cancel()
	}()

	// Reader goroutine: read frames from client
	go func() {
		defer c.wg.Done()
		c.readLoop(ctx)
		cancel()
	}()
}

// Wait blocks until both the read and write loops have finished, then
// closes the client connection. It must be called after Start.
func (c *Client) Wait() {
	c.wg.Wait()
	c.Close()
}

// Run starts the client read/write loops and blocks until the client
// disconnects. It is a convenience wrapper around Start + Wait.
func (c *Client) Run(ctx context.Context) {
	c.Start(ctx)
	c.Wait()
}

func (c *Client) readLoop(ctx context.Context) {
	// Wrap connection in a buffered reader to reduce syscall overhead.
	br := bufio.NewReaderSize(c.conn, 4096)

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		// Set read deadline to detect idle clients (phone sleeps, WiFi drops).
		// A value of 0 disables the idle timeout.
		if c.idleTimeout > 0 {
			_ = c.conn.SetReadDeadline(time.Now().Add(c.idleTimeout))
		}

		payload, err := protocol.ReadFrame(br)
		if err != nil {
			if ctx.Err() == nil {
				// Determine disconnect reason from the error type.
				var ne net.Error
				if errors.As(err, &ne) && ne.Timeout() {
					c.SetDisconnectReason(DisconnectReasonIdleTimeout)
					c.logger.Debug("client idle timeout", "error", err)
				} else {
					c.SetDisconnectReason(DisconnectReasonReadError)
					c.logger.Debug("client read error", "error", err)
				}
			}
			return
		}

		c.m.RecordMessage(metrics.MessageRecord{
			Direction: "to_node",
			Type:      "client_frame",
			Size:      len(payload),
			Client:    c.addr,
		})

		if c.onMessage != nil {
			c.onMessage(payload)
		}
	}
}

func (c *Client) writeLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case payload, ok := <-c.send:
			if !ok {
				return
			}
			if err := protocol.WriteFrame(c.conn, payload); err != nil {
				if ctx.Err() == nil {
					c.logger.Debug("client write error", "error", err)
				}
				return
			}
		}
	}
}

// Close gracefully closes the client connection.
func (c *Client) Close() {
	c.closeOnce.Do(func() {
		c.logger.Debug("closing client connection")
		_ = c.conn.Close()
		if c.onClose != nil {
			c.onClose(c)
		}
	})
}
