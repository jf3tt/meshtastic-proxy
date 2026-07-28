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
	DisconnectReasonWriteError   DisconnectReason = "write_error"
	DisconnectReasonIdleTimeout  DisconnectReason = "idle_timeout"
	DisconnectReasonSlowConsumer DisconnectReason = "slow_consumer"
	DisconnectReasonClientSent   DisconnectReason = "client_disconnect" // client sent ToRadio.Disconnect
	DisconnectReasonServerClose  DisconnectReason = "server_close"      // proxy shutdown or max clients

	// A full queue is often a short config-replay burst rather than a slow
	// client. Give the writer time to drain it before disconnecting. Config
	// replay gets a longer allowance because it can contain hundreds of frames.
	clientSendTimeout       = time.Second
	clientReplaySendTimeout = 15 * time.Second
	clientWriteTimeout      = 15 * time.Second
	replayRequestBuffer     = 2
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
	closed    atomic.Bool
	done      chan struct{}

	// wg tracks the read/write loop goroutines launched by Start.
	wg sync.WaitGroup

	// cancel stops the read/write loops. cancelMu handles the small window
	// where Close races with Start installing the cancellation function.
	cancelMu sync.Mutex
	cancel   context.CancelFunc

	// replayRequests bounds pending want_config_id work per client. A dedicated
	// proxy worker drains it, preventing repeated requests from spawning an
	// unbounded number of goroutines waiting on replayMu.
	replayRequests chan uint32

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
		conn:           conn,
		addr:           conn.RemoteAddr().String(),
		logger:         logger.With("client", conn.RemoteAddr().String()),
		m:              m,
		idleTimeout:    idleTimeout,
		connectedAt:    time.Now(),
		send:           make(chan []byte, sendBuffer),
		done:           make(chan struct{}),
		replayRequests: make(chan uint32, replayRequestBuffer),
		onMessage:      onMessage,
		onClose:        onClose,
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

// Send queues a runtime frame for delivery to the client. The common path is
// non-blocking. If the queue is temporarily full, it waits briefly for the
// writer to drain a slot before treating the client as a slow consumer.
func (c *Client) Send(payload []byte) bool {
	return c.sendWithTimeout(payload, clientSendTimeout)
}

// SendReplay queues a config or chat replay frame with a longer timeout.
// Replays can contain hundreds of frames, so transient queue pressure is
// expected and should apply backpressure rather than disconnect the client.
func (c *Client) SendReplay(payload []byte) bool {
	return c.sendWithTimeout(payload, clientReplaySendTimeout)
}

func (c *Client) sendWithTimeout(payload []byte, timeout time.Duration) bool {
	if c.closed.Load() {
		return false
	}

	// Avoid allocating a timer unless the queue is actually full.
	select {
	case <-c.done:
		return false
	case c.send <- payload:
		return true
	default:
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-c.done:
		return false
	case c.send <- payload:
		return true
	case <-timer.C:
		c.logger.Warn("client send buffer remained full, disconnecting slow consumer",
			"waited", timeout,
		)
		c.SetDisconnectReason(DisconnectReasonSlowConsumer)
		c.Close()
		return false
	}
}

// QueueReplay schedules a config replay without creating a goroutine per
// request. Standard clients issue at most two requests; excess requests are
// dropped once the small pending queue is full.
func (c *Client) QueueReplay(nonce uint32) bool {
	if c.closed.Load() {
		return false
	}

	select {
	case <-c.done:
		return false
	case c.replayRequests <- nonce:
		return true
	default:
		c.logger.Debug("config replay request queue full, dropping request", "nonce", nonce)
		return false
	}
}

// ReplayRequests returns the bounded stream of requested config nonces.
func (c *Client) ReplayRequests() <-chan uint32 {
	return c.replayRequests
}

// Done is closed when the client starts closing.
func (c *Client) Done() <-chan struct{} {
	return c.done
}

// Start launches the client read and write loops as background goroutines.
// It returns immediately. The write loop begins draining the send channel
// as soon as Start returns, so it is safe to call Send (including cached
// config replay) after Start without risking a "slow consumer" disconnect.
// Call Wait to block until the client disconnects.
func (c *Client) Start(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)

	c.cancelMu.Lock()
	if c.closed.Load() {
		c.cancelMu.Unlock()
		cancel()
		return
	}
	c.cancel = cancel
	c.cancelMu.Unlock()

	c.wg.Add(2)

	// Closing from either loop also closes the socket, which releases the
	// other loop if it is blocked in network I/O.
	go func() {
		defer c.wg.Done()
		c.writeLoop(ctx)
		cancel()
		c.Close()
	}()

	go func() {
		defer c.wg.Done()
		c.readLoop(ctx)
		cancel()
		c.Close()
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
		case payload := <-c.send:
			if err := c.conn.SetWriteDeadline(time.Now().Add(clientWriteTimeout)); err != nil {
				if ctx.Err() == nil {
					c.SetDisconnectReason(DisconnectReasonWriteError)
					c.logger.Debug("client write deadline error", "error", err)
				}
				return
			}
			if err := protocol.WriteFrame(c.conn, payload); err != nil {
				if ctx.Err() == nil {
					c.SetDisconnectReason(DisconnectReasonWriteError)
					c.logger.Debug("client write error", "error", err)
				}
				return
			}
		}
	}
}

// Close gracefully closes the client connection. Concurrent callers return
// immediately; the first caller cancels the loops, closes the socket, and
// invokes the unregister callback exactly once.
func (c *Client) Close() {
	if !c.closed.CompareAndSwap(false, true) {
		return
	}

	c.closeOnce.Do(func() {
		c.logger.Debug("closing client connection")
		close(c.done)

		c.cancelMu.Lock()
		if c.cancel != nil {
			c.cancel()
		}
		c.cancelMu.Unlock()

		_ = c.conn.Close()
		if c.onClose != nil {
			c.onClose(c)
		}
	})
}
