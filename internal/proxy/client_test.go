package proxy

import (
	"context"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/jfett/meshtastic-proxy/internal/metrics"
	"github.com/jfett/meshtastic-proxy/internal/protocol"
)

// newTestConnPair creates a TCP connection pair via loopback listener.
// Unlike net.Pipe, TCP has kernel-level send/receive buffers so
// WriteFrame does not block until the other side reads.
func newTestConnPair(t *testing.T) (server net.Conn, client net.Conn) {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	connCh := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		connCh <- c
	}()

	clientConn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		_ = ln.Close()
		t.Fatal(err)
	}

	serverConn := <-connCh
	_ = ln.Close()

	t.Cleanup(func() {
		_ = serverConn.Close()
		_ = clientConn.Close()
	})

	return serverConn, clientConn
}

// newTestClient creates a Client backed by a TCP loopback connection and
// returns the client, the server-side conn (to read/write raw frames),
// and a channel that is closed when onClose fires.
func newTestClient(t *testing.T) (*Client, net.Conn, <-chan struct{}) {
	t.Helper()

	serverConn, clientConn := newTestConnPair(t)

	m := metrics.New(10, 300)
	closeCh := make(chan struct{})

	client := NewClient(
		clientConn,
		slog.Default(),
		m,
		256,
		0,                       // no idle timeout in tests
		func(payload []byte) {}, // onMessage — no-op
		func(c *Client) {
			close(closeCh)
		},
	)

	return client, serverConn, closeCh
}

// readFrame reads one Meshtastic frame from conn with a timeout.
func readFrame(t *testing.T, conn net.Conn, timeout time.Duration) []byte {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	payload, err := protocol.ReadFrame(conn)
	if err != nil {
		t.Fatalf("readFrame: %v", err)
	}
	_ = conn.SetReadDeadline(time.Time{})
	return payload
}

// TestClientStartWait verifies that Start launches the write loop immediately
// so that Send works, and Wait blocks until the client disconnects.
func TestClientStartWait(t *testing.T) {
	client, serverConn, closeCh := newTestClient(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	client.Start(ctx)

	// Send a frame — writeLoop should be running and deliver it.
	data := []byte("hello")
	if !client.Send(data) {
		t.Fatal("Send returned false; writeLoop not running?")
	}

	got := readFrame(t, serverConn, 2*time.Second)
	if string(got) != "hello" {
		t.Fatalf("got %q, want %q", got, "hello")
	}

	// Close the server side to terminate readLoop, which cascades to shutdown.
	_ = serverConn.Close()

	// Wait must return promptly.
	done := make(chan struct{})
	go func() {
		client.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Wait did not return within timeout")
	}

	// onClose should have fired.
	select {
	case <-closeCh:
	case <-time.After(time.Second):
		t.Fatal("onClose was not called")
	}
}

// TestWriteDirectCachedConfig verifies that WriteDirect can deliver more
// frames than the send channel buffer (256) because it writes directly
// to the underlying connection, bypassing the channel entirely.
func TestWriteDirectCachedConfig(t *testing.T) {
	client, serverConn, _ := newTestClient(t)

	// Do NOT call Start yet — WriteDirect is meant to be used before Start.
	const numFrames = 200

	// Writer goroutine — mimics sendCachedConfig using WriteDirect.
	writeDone := make(chan error, 1)
	go func() {
		for i := 0; i < numFrames; i++ {
			frame := []byte{byte(i >> 8), byte(i)}
			if err := client.WriteDirect(frame); err != nil {
				writeDone <- err
				return
			}
		}
		writeDone <- nil
	}()

	// Reader — consume all frames on the server side.
	received := 0
	for i := 0; i < numFrames; i++ {
		_ = serverConn.SetReadDeadline(time.Now().Add(5 * time.Second))
		payload, err := protocol.ReadFrame(serverConn)
		if err != nil {
			t.Errorf("frame %d: read error: %v", i, err)
			break
		}
		wantHi := byte(i >> 8)
		wantLo := byte(i)
		if len(payload) != 2 || payload[0] != wantHi || payload[1] != wantLo {
			t.Errorf("frame %d: got %v, want [%d %d]", i, payload, wantHi, wantLo)
		}
		received++
	}

	if err := <-writeDone; err != nil {
		t.Fatalf("WriteDirect error: %v", err)
	}

	if received != numFrames {
		t.Fatalf("received %d frames, want %d", received, numFrames)
	}
}

// TestWriteDirectThenRun verifies the full lifecycle: WriteDirect for
// cached config, then Run for live traffic — the exact flow used by
// proxy.handleNewConnection.
func TestWriteDirectThenRun(t *testing.T) {
	client, serverConn, closeCh := newTestClient(t)

	// Phase 1: send cached config via WriteDirect.
	const cachedFrames = 5
	for i := 0; i < cachedFrames; i++ {
		if err := client.WriteDirect([]byte{byte(i)}); err != nil {
			t.Fatalf("WriteDirect frame %d: %v", i, err)
		}
	}

	// Read the cached frames.
	for i := 0; i < cachedFrames; i++ {
		got := readFrame(t, serverConn, 2*time.Second)
		if len(got) != 1 || got[0] != byte(i) {
			t.Fatalf("cached frame %d: got %v, want [%d]", i, got, i)
		}
	}

	// Phase 2: start the client loops (Run = Start + Wait).
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		client.Run(ctx)
		close(done)
	}()

	// Send a live frame through the channel.
	// No sleep needed: Send enqueues to the buffered channel, and
	// readFrame waits with a deadline for the writeLoop to drain it.
	if !client.Send([]byte("live")) {
		t.Fatal("Send returned false after Run started")
	}

	got := readFrame(t, serverConn, 2*time.Second)
	if string(got) != "live" {
		t.Fatalf("live frame: got %q, want %q", got, "live")
	}

	// Shutdown.
	_ = serverConn.Close()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return within timeout")
	}

	select {
	case <-closeCh:
	case <-time.After(time.Second):
		t.Fatal("onClose was not called")
	}
}

// TestClientRunBackwardsCompatible verifies that Run (Start+Wait wrapper)
// still works correctly.
func TestClientRunBackwardsCompatible(t *testing.T) {
	client, serverConn, closeCh := newTestClient(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		client.Run(ctx)
		close(done)
	}()

	// Send a frame. No sleep needed: Send enqueues to the buffered
	// channel, and readFrame waits with a deadline for delivery.
	if !client.Send([]byte("run-test")) {
		t.Fatal("Send returned false")
	}

	got := readFrame(t, serverConn, 2*time.Second)
	if string(got) != "run-test" {
		t.Fatalf("got %q, want %q", got, "run-test")
	}

	// Close the server side to terminate the client loops.
	// (cancel alone is not enough because readLoop blocks on conn.Read)
	_ = serverConn.Close()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return within timeout")
	}

	select {
	case <-closeCh:
	case <-time.After(time.Second):
		t.Fatal("onClose was not called")
	}
}

// TestSendBufferFullDisconnects verifies that when writeLoop is NOT running,
// filling the buffer causes a slow-consumer disconnect.
func TestSendBufferFullDisconnects(t *testing.T) {
	serverConn, clientConn := newTestConnPair(t)
	_ = serverConn // not used, just keep alive

	m := metrics.New(10, 300)
	closed := make(chan struct{})

	client := NewClient(
		clientConn,
		slog.Default(),
		m,
		256,
		0,
		func(payload []byte) {},
		func(c *Client) {
			close(closed)
		},
	)

	// Do NOT call Start — writeLoop is not running.
	// Fill the buffer (256 slots).
	for i := 0; i < 256; i++ {
		if !client.Send([]byte{byte(i)}) {
			t.Fatalf("Send returned false at frame %d before buffer full", i)
		}
	}

	// The 257th send should fail — buffer full, no consumer.
	if client.Send([]byte{0xFF}) {
		t.Fatal("Send should have returned false (buffer full)")
	}

	// onClose should have been called.
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("onClose was not called after buffer overflow")
	}
}
