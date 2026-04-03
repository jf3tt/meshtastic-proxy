package proxy

import (
	"context"
	"log/slog"
	"net"
	"sync"

	"github.com/jfett/meshtastic-proxy/internal/metrics"
	"github.com/jfett/meshtastic-proxy/internal/protocol"
)

// Client represents a single connected TCP client.
type Client struct {
	conn   net.Conn
	addr   string
	logger *slog.Logger
	m      *metrics.Metrics

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
}

// NewClient creates a new client handler.
func NewClient(conn net.Conn, logger *slog.Logger, m *metrics.Metrics, onMessage func([]byte), onClose func(*Client)) *Client {
	return &Client{
		conn:      conn,
		addr:      conn.RemoteAddr().String(),
		logger:    logger.With("client", conn.RemoteAddr().String()),
		m:         m,
		send:      make(chan []byte, 256),
		onMessage: onMessage,
		onClose:   onClose,
	}
}

// Addr returns the remote address of the client.
func (c *Client) Addr() string {
	return c.addr
}

// Send queues a frame for delivery to the client.
// Returns false if the client's buffer is full (slow consumer).
func (c *Client) Send(payload []byte) bool {
	select {
	case c.send <- payload:
		return true
	default:
		c.logger.Warn("client send buffer full, disconnecting slow consumer")
		c.Close()
		return false
	}
}

// WriteDirect writes a frame directly to the underlying connection,
// bypassing the send channel. It is intended for replaying cached config
// frames BEFORE Start is called, when the connection is not yet shared
// with any goroutine. It must NOT be called concurrently with Start or
// the write loop.
func (c *Client) WriteDirect(payload []byte) error {
	return protocol.WriteFrame(c.conn, payload)
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
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		payload, err := protocol.ReadFrame(c.conn)
		if err != nil {
			if ctx.Err() == nil {
				c.logger.Debug("client read error", "error", err)
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
