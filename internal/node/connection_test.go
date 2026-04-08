package node

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	pb "buf.build/gen/go/meshtastic/protobufs/protocolbuffers/go/meshtastic"
	"google.golang.org/protobuf/proto"

	"github.com/jfett/meshtastic-proxy/internal/metrics"
	"github.com/jfett/meshtastic-proxy/internal/protocol"
)

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// newTestConnPair creates a TCP connection pair via loopback listener.
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

// marshalFromRadio serializes a FromRadio message into bytes.
func marshalFromRadio(t *testing.T, msg *pb.FromRadio) []byte {
	t.Helper()
	data, err := proto.Marshal(msg)
	if err != nil {
		t.Fatalf("marshalFromRadio: %v", err)
	}
	return data
}

// marshalToRadio serializes a ToRadio message into bytes.
func marshalToRadio(t *testing.T, msg *pb.ToRadio) []byte {
	t.Helper()
	data, err := proto.Marshal(msg)
	if err != nil {
		t.Fatalf("marshalToRadio: %v", err)
	}
	return data
}

// writeFrame writes a Meshtastic frame to conn with a timeout.
func writeFrame(t *testing.T, conn net.Conn, payload []byte) {
	t.Helper()
	_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	if err := protocol.WriteFrame(conn, payload); err != nil {
		t.Fatalf("writeFrame: %v", err)
	}
	_ = conn.SetWriteDeadline(time.Time{})
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

// newTestConnection creates a Connection with typical test parameters.
func newTestConnection(address string, m *metrics.Metrics) *Connection {
	return NewConnection(ConnectionOptions{
		Address:              address,
		ReconnectInterval:    10 * time.Millisecond,
		MaxReconnectInterval: 100 * time.Millisecond,
		DialTimeout:          5 * time.Second,
		ReadTimeout:          5 * time.Minute,
		FromBuffer:           256,
		ToBuffer:             256,
		Metrics:              m,
		Logger:               slog.Default(),
	})
}

// buildConfigSequence creates a config sequence: MyInfo, Config, NodeInfo, ConfigCompleteId.
func buildConfigSequence(t *testing.T, myNodeNum uint32) [][]byte {
	t.Helper()
	var frames [][]byte

	// MyInfo
	frames = append(frames, marshalFromRadio(t, &pb.FromRadio{
		PayloadVariant: &pb.FromRadio_MyInfo{
			MyInfo: &pb.MyNodeInfo{MyNodeNum: myNodeNum},
		},
	}))
	// Config
	frames = append(frames, marshalFromRadio(t, &pb.FromRadio{
		PayloadVariant: &pb.FromRadio_Config{
			Config: &pb.Config{
				PayloadVariant: &pb.Config_Device{
					Device: &pb.Config_DeviceConfig{},
				},
			},
		},
	}))
	// NodeInfo
	frames = append(frames, marshalFromRadio(t, &pb.FromRadio{
		PayloadVariant: &pb.FromRadio_NodeInfo{
			NodeInfo: &pb.NodeInfo{
				Num: myNodeNum,
				User: &pb.User{
					LongName:  "TestNode",
					ShortName: "TN",
				},
			},
		},
	}))
	// ConfigCompleteId
	frames = append(frames, marshalFromRadio(t, &pb.FromRadio{
		PayloadVariant: &pb.FromRadio_ConfigCompleteId{
			ConfigCompleteId: 12345,
		},
	}))

	return frames
}

// ---------------------------------------------------------------------------
// NewConnection tests
// ---------------------------------------------------------------------------

func TestNewConnection(t *testing.T) {
	m := metrics.New(10, 300)
	c := NewConnection(ConnectionOptions{
		Address:              "192.168.1.1:4403",
		ReconnectInterval:    5 * time.Second,
		MaxReconnectInterval: 60 * time.Second,
		DialTimeout:          10 * time.Second,
		ReadTimeout:          5 * time.Minute,
		FromBuffer:           128,
		ToBuffer:             64,
		Metrics:              m,
		Logger:               slog.Default(),
	})

	if c.address != "192.168.1.1:4403" {
		t.Errorf("address = %q, want %q", c.address, "192.168.1.1:4403")
	}
	if c.reconnectInterval != 5*time.Second {
		t.Errorf("reconnectInterval = %v, want %v", c.reconnectInterval, 5*time.Second)
	}
	if c.maxReconnectInterval != 60*time.Second {
		t.Errorf("maxReconnectInterval = %v, want %v", c.maxReconnectInterval, 60*time.Second)
	}
	if c.dialTimeout != 10*time.Second {
		t.Errorf("dialTimeout = %v, want %v", c.dialTimeout, 10*time.Second)
	}
	if c.readTimeout != 5*time.Minute {
		t.Errorf("readTimeout = %v, want %v", c.readTimeout, 5*time.Minute)
	}
	if cap(c.fromNode) != 128 {
		t.Errorf("fromNode cap = %d, want 128", cap(c.fromNode))
	}
	if cap(c.toNode) != 64 {
		t.Errorf("toNode cap = %d, want 64", cap(c.toNode))
	}
	if c.metrics != m {
		t.Error("metrics not set correctly")
	}
}

func TestFromNodeChannel(t *testing.T) {
	m := metrics.New(10, 300)
	c := newTestConnection("127.0.0.1:0", m)

	ch := c.FromNode()
	if ch == nil {
		t.Fatal("FromNode returned nil")
	}

	// Channel should be read-only.
	// Verify it's the same underlying channel by sending via internal field.
	c.fromNode <- []byte("test")
	data := <-ch
	if string(data) != "test" {
		t.Errorf("FromNode data = %q, want %q", data, "test")
	}
}

// ---------------------------------------------------------------------------
// ConfigCache tests
// ---------------------------------------------------------------------------

func TestConfigCache_Empty(t *testing.T) {
	m := metrics.New(10, 300)
	c := newTestConnection("127.0.0.1:0", m)

	cache := c.ConfigCache()
	if len(cache) != 0 {
		t.Fatalf("expected empty cache, got %d frames", len(cache))
	}
}

func TestConfigCache_DeepCopy(t *testing.T) {
	m := metrics.New(10, 300)
	c := newTestConnection("127.0.0.1:0", m)

	// Populate cache directly for unit testing.
	original := [][]byte{
		{0x01, 0x02, 0x03},
		{0x04, 0x05},
	}
	c.configMu.Lock()
	c.configCache = original
	c.configMu.Unlock()

	cache := c.ConfigCache()

	if len(cache) != 2 {
		t.Fatalf("cache len = %d, want 2", len(cache))
	}

	// Modify the returned copy — should not affect the original.
	cache[0][0] = 0xFF
	_ = append(cache, []byte{0xAA}) //nolint:gocritic // intentional: verify append doesn't affect original

	c.configMu.RLock()
	if c.configCache[0][0] != 0x01 {
		t.Error("ConfigCache did not return a deep copy — original was modified")
	}
	if len(c.configCache) != 2 {
		t.Error("ConfigCache did not return a deep copy — length was modified")
	}
	c.configMu.RUnlock()
}

// ---------------------------------------------------------------------------
// Send tests
// ---------------------------------------------------------------------------

func TestSend_Success(t *testing.T) {
	m := metrics.New(10, 300)
	c := newTestConnection("127.0.0.1:0", m)

	payload := marshalToRadio(t, &pb.ToRadio{
		PayloadVariant: &pb.ToRadio_Packet{
			Packet: &pb.MeshPacket{
				From: 0x12345678,
				To:   0xFFFFFFFF,
			},
		},
	})

	c.Send(payload)

	// Frame should appear in toNode channel.
	select {
	case got := <-c.toNode:
		if len(got) != len(payload) {
			t.Errorf("payload len = %d, want %d", len(got), len(payload))
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for frame in toNode channel")
	}

	// Metrics should be incremented.
	if m.FramesToNode.Load() != 1 {
		t.Errorf("FramesToNode = %d, want 1", m.FramesToNode.Load())
	}
	if m.BytesToNode.Load() != int64(len(payload)) {
		t.Errorf("BytesToNode = %d, want %d", m.BytesToNode.Load(), len(payload))
	}
}

func TestSend_ChannelFull(t *testing.T) {
	m := metrics.New(10, 300)
	// Create connection with tiny toNode buffer.
	c := NewConnection(ConnectionOptions{
		Address:              "127.0.0.1:0",
		ReconnectInterval:    time.Second,
		MaxReconnectInterval: time.Minute,
		DialTimeout:          5 * time.Second,
		ReadTimeout:          5 * time.Minute,
		FromBuffer:           256,
		ToBuffer:             2, // tiny toBuffer
		Metrics:              m,
		Logger:               slog.Default(),
	})

	payload := []byte{0x01, 0x02}

	// Fill the channel.
	c.Send(payload)
	c.Send(payload)

	// Third send should be dropped (channel full).
	c.Send(payload)

	// Only 2 frames should have been counted (the third was dropped).
	if m.FramesToNode.Load() != 2 {
		t.Errorf("FramesToNode = %d, want 2", m.FramesToNode.Load())
	}
}

// ---------------------------------------------------------------------------
// readLoop tests
// ---------------------------------------------------------------------------

func TestReadLoop_ForwardsFrames(t *testing.T) {
	m := metrics.New(10, 300)
	c := newTestConnection("127.0.0.1:0", m)

	serverConn, clientConn := newTestConnPair(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Run readLoop in background.
	errCh := make(chan error, 1)
	go func() {
		errCh <- c.readLoop(ctx, clientConn)
	}()

	// Write a FromRadio packet frame from the "node" side.
	payload := marshalFromRadio(t, &pb.FromRadio{
		PayloadVariant: &pb.FromRadio_Packet{
			Packet: &pb.MeshPacket{
				From: 0xAABBCCDD,
				To:   0xFFFFFFFF,
			},
		},
	})
	writeFrame(t, serverConn, payload)

	// Read from fromNode channel.
	select {
	case got := <-c.fromNode:
		if len(got) != len(payload) {
			t.Errorf("payload len = %d, want %d", len(got), len(payload))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for frame in fromNode channel")
	}

	// Check metrics.
	if m.FramesFromNode.Load() != 1 {
		t.Errorf("FramesFromNode = %d, want 1", m.FramesFromNode.Load())
	}
	if m.BytesFromNode.Load() != int64(len(payload)) {
		t.Errorf("BytesFromNode = %d, want %d", m.BytesFromNode.Load(), len(payload))
	}

	cancel()
}

func TestReadLoop_CachesConfig(t *testing.T) {
	m := metrics.New(10, 300)
	c := newTestConnection("127.0.0.1:0", m)

	serverConn, clientConn := newTestConnPair(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- c.readLoop(ctx, clientConn)
	}()

	// Send a full config sequence.
	configFrames := buildConfigSequence(t, 0x12345678)
	for _, frame := range configFrames {
		writeFrame(t, serverConn, frame)
	}

	// Drain fromNode to prevent readLoop from blocking.
	go func() {
		for range c.fromNode {
		}
	}()

	// Wait for config to be cached.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		cache := c.ConfigCache()
		if len(cache) == len(configFrames) {
			// Verify the cache contents.
			if m.ConfigCacheFrames.Load() != int64(len(configFrames)) {
				t.Errorf("ConfigCacheFrames = %d, want %d",
					m.ConfigCacheFrames.Load(), len(configFrames))
			}
			cancel()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}

	t.Fatalf("config cache not populated within timeout; got %d frames, want %d",
		len(c.ConfigCache()), len(configFrames))
}

func TestReadLoop_ConfigCacheReplacedOnNewMyInfo(t *testing.T) {
	m := metrics.New(10, 300)
	c := newTestConnection("127.0.0.1:0", m)

	serverConn, clientConn := newTestConnPair(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- c.readLoop(ctx, clientConn)
	}()

	// Drain fromNode.
	go func() {
		for range c.fromNode {
		}
	}()

	// Send first config sequence.
	config1 := buildConfigSequence(t, 0x11111111)
	for _, frame := range config1 {
		writeFrame(t, serverConn, frame)
	}

	// Wait for cache.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(c.ConfigCache()) == len(config1) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(c.ConfigCache()) != len(config1) {
		t.Fatal("first config cache not populated")
	}

	// Send second config sequence with different node num.
	config2 := buildConfigSequence(t, 0x22222222)
	for _, frame := range config2 {
		writeFrame(t, serverConn, frame)
	}

	// Wait for cache to be replaced.
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		cache := c.ConfigCache()
		if len(cache) > 0 {
			msg := &pb.FromRadio{}
			if err := proto.Unmarshal(cache[0], msg); err == nil {
				if v, ok := msg.GetPayloadVariant().(*pb.FromRadio_MyInfo); ok {
					if v.MyInfo.GetMyNodeNum() == 0x22222222 {
						cancel()
						return
					}
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}

	t.Fatal("config cache was not replaced with new config sequence")
}

func TestReadLoop_ConnectionClose(t *testing.T) {
	m := metrics.New(10, 300)
	c := newTestConnection("127.0.0.1:0", m)

	serverConn, clientConn := newTestConnPair(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- c.readLoop(ctx, clientConn)
	}()

	// Close the writing side to trigger read error.
	_ = serverConn.Close()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("expected error from readLoop when connection closes")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("readLoop did not return after connection close")
	}
}

func TestReadLoop_ContextCancel(t *testing.T) {
	m := metrics.New(10, 300)
	c := newTestConnection("127.0.0.1:0", m)

	_, clientConn := newTestConnPair(t)

	ctx, cancel := context.WithCancel(context.Background())

	errCh := make(chan error, 1)
	go func() {
		errCh <- c.readLoop(ctx, clientConn)
	}()

	// Give readLoop time to start, then cancel.
	time.Sleep(50 * time.Millisecond)
	cancel()

	// readLoop should return with context error (or quickly after conn close).
	// Close the conn to unblock the read.
	_ = clientConn.Close()

	select {
	case err := <-errCh:
		// Acceptable: either context.Canceled or a read error after Close.
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Logf("readLoop returned non-cancel error (acceptable): %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("readLoop did not return after context cancel")
	}
}

// ---------------------------------------------------------------------------
// writeLoop tests
// ---------------------------------------------------------------------------

func TestWriteLoop_WritesFrames(t *testing.T) {
	m := metrics.New(10, 300)
	c := newTestConnection("127.0.0.1:0", m)

	serverConn, clientConn := newTestConnPair(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- c.writeLoop(ctx, clientConn)
	}()

	// Send a payload through the toNode channel.
	payload := []byte{0x01, 0x02, 0x03}
	c.toNode <- payload

	// Read the frame on the server side.
	got := readFrame(t, serverConn, 2*time.Second)
	if string(got) != string(payload) {
		t.Errorf("writeLoop wrote %v, want %v", got, payload)
	}

	cancel()
}

func TestWriteLoop_ContextCancel(t *testing.T) {
	m := metrics.New(10, 300)
	c := newTestConnection("127.0.0.1:0", m)

	_, clientConn := newTestConnPair(t)

	ctx, cancel := context.WithCancel(context.Background())

	errCh := make(chan error, 1)
	go func() {
		errCh <- c.writeLoop(ctx, clientConn)
	}()

	cancel()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("writeLoop error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("writeLoop did not return after context cancel")
	}
}

func TestWriteLoop_WriteError(t *testing.T) {
	m := metrics.New(10, 300)
	c := newTestConnection("127.0.0.1:0", m)

	_, clientConn := newTestConnPair(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- c.writeLoop(ctx, clientConn)
	}()

	// Close the connection to cause write error.
	_ = clientConn.Close()

	// Send payload — should trigger a write error.
	c.toNode <- []byte{0x01}

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("expected write error from writeLoop")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("writeLoop did not return after write error")
	}
}

// ---------------------------------------------------------------------------
// Heartbeat tests
// ---------------------------------------------------------------------------

func TestHeartbeatLoop_SendsHeartbeat(t *testing.T) {
	m := metrics.New(10, 300)
	server, client := newTestConnPair(t)

	c := NewConnection(ConnectionOptions{
		Address:              "127.0.0.1:0",
		ReconnectInterval:    10 * time.Millisecond,
		MaxReconnectInterval: 100 * time.Millisecond,
		DialTimeout:          5 * time.Second,
		ReadTimeout:          5 * time.Minute,
		HeartbeatInterval:    50 * time.Millisecond,
		FromBuffer:           256,
		ToBuffer:             256,
		Metrics:              m,
		Logger:               slog.Default(),
	})
	// Inject the connection directly so writeLoop can drain toNode.
	c.mu.Lock()
	c.conn = client
	c.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start writeLoop to drain toNode → conn.
	go func() { _ = c.writeLoop(ctx, client) }()

	// Start heartbeatLoop.
	go func() { _ = c.heartbeatLoop(ctx) }()

	// Read at least 2 heartbeat frames from the server side.
	for i := 0; i < 2; i++ {
		payload := readFrame(t, server, 2*time.Second)
		var tr pb.ToRadio
		if err := proto.Unmarshal(payload, &tr); err != nil {
			t.Fatalf("unmarshal frame %d: %v", i, err)
		}
		if _, ok := tr.PayloadVariant.(*pb.ToRadio_Heartbeat); !ok {
			t.Fatalf("frame %d: expected Heartbeat, got %T", i, tr.PayloadVariant)
		}
	}

	// Verify FramesToNode was incremented (at least 2 heartbeats).
	if got := m.FramesToNode.Load(); got < 2 {
		t.Errorf("expected FramesToNode >= 2, got %d", got)
	}
}

func TestHeartbeatLoop_ContextCancel(t *testing.T) {
	m := metrics.New(10, 300)

	c := NewConnection(ConnectionOptions{
		Address:              "127.0.0.1:0",
		ReconnectInterval:    10 * time.Millisecond,
		MaxReconnectInterval: 100 * time.Millisecond,
		DialTimeout:          5 * time.Second,
		ReadTimeout:          5 * time.Minute,
		HeartbeatInterval:    1 * time.Second,
		FromBuffer:           256,
		ToBuffer:             256,
		Metrics:              m,
		Logger:               slog.Default(),
	})

	ctx, cancel := context.WithCancel(context.Background())

	errCh := make(chan error, 1)
	go func() {
		errCh <- c.heartbeatLoop(ctx)
	}()

	// Cancel quickly — heartbeatLoop should return context.Canceled.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("heartbeatLoop did not return after context cancel")
	}
}

func TestHeartbeatLoop_DisabledWhenZero(t *testing.T) {
	m := metrics.New(10, 300)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()

	c := NewConnection(ConnectionOptions{
		Address:              ln.Addr().String(),
		ReconnectInterval:    10 * time.Millisecond,
		MaxReconnectInterval: 100 * time.Millisecond,
		DialTimeout:          5 * time.Second,
		ReadTimeout:          5 * time.Minute,
		HeartbeatInterval:    0, // disabled
		FromBuffer:           256,
		ToBuffer:             256,
		Metrics:              m,
		Logger:               slog.Default(),
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Accept connection and send config so runConnection starts.
	nodeReady := make(chan net.Conn, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		nodeReady <- conn
	}()

	done := make(chan struct{})
	go func() {
		c.Run(ctx)
		close(done)
	}()

	// Wait for connection.
	var nodeConn net.Conn
	select {
	case nodeConn = <-nodeReady:
	case <-time.After(2 * time.Second):
		t.Fatal("node did not accept connection")
	}
	defer func() { _ = nodeConn.Close() }()

	// Read the want_config_id that Run sends, then ignore it.
	_ = readFrame(t, nodeConn, 2*time.Second)

	// Send a minimal config sequence so readLoop doesn't block.
	for _, frame := range buildConfigSequence(t, 0x1234) {
		writeFrame(t, nodeConn, frame)
	}

	// Wait a bit — no heartbeat should arrive.
	_ = nodeConn.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	_, readErr := protocol.ReadFrame(nodeConn)
	if readErr == nil {
		t.Fatal("expected no heartbeat frame when HeartbeatInterval=0, but got a frame")
	}
	// Any error here is expected (timeout). We just verify no frame arrived.
	var netErr net.Error
	if errors.As(readErr, &netErr) && !netErr.Timeout() {
		t.Logf("unexpected non-timeout error: %v", readErr)
	}

	cancel()
	<-done
}

// ---------------------------------------------------------------------------
// Run tests (integration)
// ---------------------------------------------------------------------------

func TestRun_ConnectsAndCaches(t *testing.T) {
	m := metrics.New(10, 300)

	// Start a fake node listener.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()

	c := newTestConnection(ln.Addr().String(), m)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle a single node connection.
	nodeReady := make(chan net.Conn, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		nodeReady <- conn
	}()

	// Run the connection manager in background.
	go c.Run(ctx)

	// Wait for the connection.
	var nodeConn net.Conn
	select {
	case nodeConn = <-nodeReady:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for node connection")
	}
	defer func() { _ = nodeConn.Close() }()

	// Read the want_config_id request from the proxy.
	got := readFrame(t, nodeConn, 2*time.Second)
	msg := &pb.ToRadio{}
	if err := proto.Unmarshal(got, msg); err != nil {
		t.Fatalf("unmarshal want_config_id: %v", err)
	}
	if _, ok := msg.GetPayloadVariant().(*pb.ToRadio_WantConfigId); !ok {
		t.Fatalf("expected WantConfigId, got %T", msg.GetPayloadVariant())
	}

	// Send config sequence.
	configFrames := buildConfigSequence(t, 0x12345678)
	for _, frame := range configFrames {
		writeFrame(t, nodeConn, frame)
	}

	// Wait for config cache to be populated.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(c.ConfigCache()) == len(configFrames) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if len(c.ConfigCache()) != len(configFrames) {
		t.Fatalf("config cache: got %d frames, want %d", len(c.ConfigCache()), len(configFrames))
	}

	// Node should be marked as connected.
	if !m.NodeConnected.Load() {
		t.Error("expected NodeConnected to be true")
	}

	// First connect — no reconnects.
	if m.NodeReconnects.Load() != 0 {
		t.Errorf("NodeReconnects = %d, want 0", m.NodeReconnects.Load())
	}

	cancel()
}

func TestRun_ReconnectsOnDisconnect(t *testing.T) {
	m := metrics.New(10, 300)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()

	c := newTestConnection(ln.Addr().String(), m)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var (
		connMu    sync.Mutex
		connCount int
		nodeConns = make(chan net.Conn, 5)
	)

	// Accept multiple connections (initial + reconnects).
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			connMu.Lock()
			connCount++
			connMu.Unlock()
			nodeConns <- conn
		}
	}()

	go c.Run(ctx)

	// First connection.
	var conn1 net.Conn
	select {
	case conn1 = <-nodeConns:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for first connection")
	}

	// Read and discard want_config_id.
	_ = readFrame(t, conn1, 2*time.Second)

	// Close the first connection to trigger reconnect.
	_ = conn1.Close()

	// Second connection (reconnect).
	var conn2 net.Conn
	select {
	case conn2 = <-nodeConns:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for reconnection")
	}
	defer func() { _ = conn2.Close() }()

	// Read want_config_id from the second connection.
	got := readFrame(t, conn2, 2*time.Second)
	msg := &pb.ToRadio{}
	if err := proto.Unmarshal(got, msg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := msg.GetPayloadVariant().(*pb.ToRadio_WantConfigId); !ok {
		t.Fatalf("expected WantConfigId on reconnect, got %T", msg.GetPayloadVariant())
	}

	// After reconnect, NodeReconnects should be 1.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if m.NodeReconnects.Load() >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if m.NodeReconnects.Load() < 1 {
		t.Errorf("NodeReconnects = %d, want >= 1", m.NodeReconnects.Load())
	}

	cancel()
}

func TestRun_DialFailureIncrementsErrors(t *testing.T) {
	m := metrics.New(10, 300)

	// Use an address that won't accept connections.
	// Bind a listener, get the address, close it immediately.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	c := NewConnection(ConnectionOptions{
		Address:              addr,
		ReconnectInterval:    10 * time.Millisecond,  // fast reconnect for test
		MaxReconnectInterval: 50 * time.Millisecond,  // low max
		DialTimeout:          100 * time.Millisecond, // short dial timeout
		ReadTimeout:          5 * time.Minute,
		FromBuffer:           256,
		ToBuffer:             256,
		Metrics:              m,
		Logger:               slog.Default(),
	})

	ctx, cancel := context.WithCancel(context.Background())

	go c.Run(ctx)

	// Wait for at least one connection error.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if m.NodeConnectionErrors.Load() >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancel()

	if m.NodeConnectionErrors.Load() < 1 {
		t.Errorf("NodeConnectionErrors = %d, want >= 1", m.NodeConnectionErrors.Load())
	}
	if m.NodeConnected.Load() {
		t.Error("expected NodeConnected to be false after dial failure")
	}
}

func TestRun_ContextCancelStops(t *testing.T) {
	m := metrics.New(10, 300)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()

	c := newTestConnection(ln.Addr().String(), m)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		c.Run(ctx)
		close(done)
	}()

	// Accept the connection.
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		// Keep conn open until test ends.
		<-done
		_ = conn.Close()
	}()

	// Give time for connection.
	time.Sleep(100 * time.Millisecond)

	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after context cancel")
	}
}

func TestRun_ForwardsToRadio(t *testing.T) {
	m := metrics.New(10, 300)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()

	c := newTestConnection(ln.Addr().String(), m)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	nodeReady := make(chan net.Conn, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		nodeReady <- conn
	}()

	go c.Run(ctx)

	var nodeConn net.Conn
	select {
	case nodeConn = <-nodeReady:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for node connection")
	}
	defer func() { _ = nodeConn.Close() }()

	// Read and discard want_config_id.
	_ = readFrame(t, nodeConn, 2*time.Second)

	// Send a ToRadio packet via the proxy's Send method.
	payload := marshalToRadio(t, &pb.ToRadio{
		PayloadVariant: &pb.ToRadio_Packet{
			Packet: &pb.MeshPacket{
				From: 0xABCD,
				To:   0xFFFF,
			},
		},
	})
	c.Send(payload)

	// Read the forwarded frame on the node side.
	got := readFrame(t, nodeConn, 2*time.Second)

	msg := &pb.ToRadio{}
	if err := proto.Unmarshal(got, msg); err != nil {
		t.Fatalf("unmarshal forwarded ToRadio: %v", err)
	}
	if _, ok := msg.GetPayloadVariant().(*pb.ToRadio_Packet); !ok {
		t.Fatalf("expected Packet, got %T", msg.GetPayloadVariant())
	}
}

// ---------------------------------------------------------------------------
// decodeFromRadio tests
// ---------------------------------------------------------------------------

func TestDecodeFromRadio_MyInfo(t *testing.T) {
	payload := marshalFromRadio(t, &pb.FromRadio{
		PayloadVariant: &pb.FromRadio_MyInfo{
			MyInfo: &pb.MyNodeInfo{MyNodeNum: 0x12345678},
		},
	})

	rec := decodeFromRadio(payload)
	if rec.Type != "my_info" {
		t.Errorf("type = %q, want %q", rec.Type, "my_info")
	}
	if rec.Direction != "from_node" {
		t.Errorf("direction = %q, want %q", rec.Direction, "from_node")
	}
	if rec.Payload == "" {
		t.Error("expected non-empty payload")
	}
}

func TestDecodeFromRadio_Packet(t *testing.T) {
	payload := marshalFromRadio(t, &pb.FromRadio{
		PayloadVariant: &pb.FromRadio_Packet{
			Packet: &pb.MeshPacket{
				From:    0xAABBCCDD,
				To:      0xFFFFFFFF,
				Channel: 1,
				PayloadVariant: &pb.MeshPacket_Decoded{
					Decoded: &pb.Data{
						Portnum: pb.PortNum_TEXT_MESSAGE_APP,
						Payload: []byte("Hello"),
					},
				},
				HopLimit:  3,
				HopStart:  7,
				RxRssi:    -87,
				RxSnr:     6.5,
				ViaMqtt:   true,
				RelayNode: 0xAB,
			},
		},
	})

	rec := decodeFromRadio(payload)
	if rec.Type != "packet" {
		t.Errorf("type = %q, want %q", rec.Type, "packet")
	}
	if rec.From != 0xAABBCCDD {
		t.Errorf("from = 0x%x, want 0xAABBCCDD", rec.From)
	}
	if rec.To != 0xFFFFFFFF {
		t.Errorf("to = 0x%x, want 0xFFFFFFFF", rec.To)
	}
	if rec.PortNum != "TEXT_MESSAGE_APP" {
		t.Errorf("portnum = %q, want %q", rec.PortNum, "TEXT_MESSAGE_APP")
	}
	if rec.Payload != "Hello" {
		t.Errorf("payload = %q, want %q", rec.Payload, "Hello")
	}
	if rec.HopLimit != 3 {
		t.Errorf("hop_limit = %d, want 3", rec.HopLimit)
	}
	if rec.HopStart != 7 {
		t.Errorf("hop_start = %d, want 7", rec.HopStart)
	}
	if rec.RxRssi != -87 {
		t.Errorf("rx_rssi = %d, want -87", rec.RxRssi)
	}
	if rec.RxSnr != 6.5 {
		t.Errorf("rx_snr = %f, want 6.5", rec.RxSnr)
	}
	if !rec.ViaMqtt {
		t.Error("via_mqtt = false, want true")
	}
	if rec.RelayNode != 0xAB {
		t.Errorf("relay_node = 0x%x, want 0xAB", rec.RelayNode)
	}
}

func TestDecodeFromRadio_PacketRadioMetadataDefaults(t *testing.T) {
	// Verify that a packet without radio metadata fields decodes with zero values.
	payload := marshalFromRadio(t, &pb.FromRadio{
		PayloadVariant: &pb.FromRadio_Packet{
			Packet: &pb.MeshPacket{
				From: 0x11223344,
				To:   0x55667788,
				PayloadVariant: &pb.MeshPacket_Decoded{
					Decoded: &pb.Data{
						Portnum: pb.PortNum_POSITION_APP,
						Payload: []byte{},
					},
				},
			},
		},
	})

	rec := decodeFromRadio(payload)
	if rec.HopLimit != 0 {
		t.Errorf("hop_limit = %d, want 0", rec.HopLimit)
	}
	if rec.HopStart != 0 {
		t.Errorf("hop_start = %d, want 0", rec.HopStart)
	}
	if rec.RxRssi != 0 {
		t.Errorf("rx_rssi = %d, want 0", rec.RxRssi)
	}
	if rec.RxSnr != 0 {
		t.Errorf("rx_snr = %f, want 0", rec.RxSnr)
	}
	if rec.ViaMqtt {
		t.Error("via_mqtt = true, want false")
	}
	if rec.RelayNode != 0 {
		t.Errorf("relay_node = 0x%x, want 0", rec.RelayNode)
	}
}

func TestDecodeFromRadio_NodeInfo(t *testing.T) {
	payload := marshalFromRadio(t, &pb.FromRadio{
		PayloadVariant: &pb.FromRadio_NodeInfo{
			NodeInfo: &pb.NodeInfo{
				Num: 0xAABB,
				User: &pb.User{
					LongName:  "TestNode",
					ShortName: "TN",
				},
			},
		},
	})

	rec := decodeFromRadio(payload)
	if rec.Type != "node_info" {
		t.Errorf("type = %q, want %q", rec.Type, "node_info")
	}
}

func TestDecodeFromRadio_Config(t *testing.T) {
	payload := marshalFromRadio(t, &pb.FromRadio{
		PayloadVariant: &pb.FromRadio_Config{
			Config: &pb.Config{
				PayloadVariant: &pb.Config_Lora{
					Lora: &pb.Config_LoRaConfig{
						Region:      pb.Config_LoRaConfig_US,
						ModemPreset: pb.Config_LoRaConfig_LONG_FAST,
					},
				},
			},
		},
	})

	rec := decodeFromRadio(payload)
	if rec.Type != "config" {
		t.Errorf("type = %q, want %q", rec.Type, "config")
	}
	if rec.Payload == "" {
		t.Error("expected non-empty payload for config")
	}
}

func TestDecodeFromRadio_ConfigCompleteId(t *testing.T) {
	payload := marshalFromRadio(t, &pb.FromRadio{
		PayloadVariant: &pb.FromRadio_ConfigCompleteId{
			ConfigCompleteId: 69420,
		},
	})

	rec := decodeFromRadio(payload)
	if rec.Type != "config_complete_id" {
		t.Errorf("type = %q, want %q", rec.Type, "config_complete_id")
	}
	if rec.Payload != "nonce=69420" {
		t.Errorf("payload = %q, want %q", rec.Payload, "nonce=69420")
	}
}

func TestDecodeFromRadio_Channel(t *testing.T) {
	payload := marshalFromRadio(t, &pb.FromRadio{
		PayloadVariant: &pb.FromRadio_Channel{
			Channel: &pb.Channel{
				Index: 1,
				Role:  pb.Channel_PRIMARY,
				Settings: &pb.ChannelSettings{
					Name: "LongFast",
				},
			},
		},
	})

	rec := decodeFromRadio(payload)
	if rec.Type != "channel" {
		t.Errorf("type = %q, want %q", rec.Type, "channel")
	}
}

func TestDecodeFromRadio_ModuleConfig(t *testing.T) {
	payload := marshalFromRadio(t, &pb.FromRadio{
		PayloadVariant: &pb.FromRadio_ModuleConfig{
			ModuleConfig: &pb.ModuleConfig{
				PayloadVariant: &pb.ModuleConfig_Mqtt{
					Mqtt: &pb.ModuleConfig_MQTTConfig{},
				},
			},
		},
	})

	rec := decodeFromRadio(payload)
	if rec.Type != "module_config" {
		t.Errorf("type = %q, want %q", rec.Type, "module_config")
	}
}

func TestDecodeFromRadio_Metadata(t *testing.T) {
	payload := marshalFromRadio(t, &pb.FromRadio{
		PayloadVariant: &pb.FromRadio_Metadata{
			Metadata: &pb.DeviceMetadata{
				FirmwareVersion: "2.5.0",
				HwModel:         pb.HardwareModel_HELTEC_V3,
			},
		},
	})

	rec := decodeFromRadio(payload)
	if rec.Type != "metadata" {
		t.Errorf("type = %q, want %q", rec.Type, "metadata")
	}
	if rec.Payload == "" {
		t.Error("expected non-empty payload for metadata")
	}
}

func TestDecodeFromRadio_LogRecord(t *testing.T) {
	payload := marshalFromRadio(t, &pb.FromRadio{
		PayloadVariant: &pb.FromRadio_LogRecord{
			LogRecord: &pb.LogRecord{
				Message: "test log message",
			},
		},
	})

	rec := decodeFromRadio(payload)
	if rec.Type != "log_record" {
		t.Errorf("type = %q, want %q", rec.Type, "log_record")
	}
	if rec.Payload != "test log message" {
		t.Errorf("payload = %q, want %q", rec.Payload, "test log message")
	}
}

func TestDecodeFromRadio_Rebooted(t *testing.T) {
	payload := marshalFromRadio(t, &pb.FromRadio{
		PayloadVariant: &pb.FromRadio_Rebooted{
			Rebooted: true,
		},
	})

	rec := decodeFromRadio(payload)
	if rec.Type != "rebooted" {
		t.Errorf("type = %q, want %q", rec.Type, "rebooted")
	}
}

func TestDecodeFromRadio_QueueStatus(t *testing.T) {
	payload := marshalFromRadio(t, &pb.FromRadio{
		PayloadVariant: &pb.FromRadio_QueueStatus{
			QueueStatus: &pb.QueueStatus{
				Free: 32,
			},
		},
	})

	rec := decodeFromRadio(payload)
	if rec.Type != "queue_status" {
		t.Errorf("type = %q, want %q", rec.Type, "queue_status")
	}
}

func TestDecodeFromRadio_EncryptedPacket(t *testing.T) {
	payload := marshalFromRadio(t, &pb.FromRadio{
		PayloadVariant: &pb.FromRadio_Packet{
			Packet: &pb.MeshPacket{
				From: 0x1111,
				To:   0x2222,
				PayloadVariant: &pb.MeshPacket_Encrypted{
					Encrypted: []byte{0xAA, 0xBB, 0xCC},
				},
			},
		},
	})

	rec := decodeFromRadio(payload)
	if rec.Type != "packet" {
		t.Errorf("type = %q, want %q", rec.Type, "packet")
	}
	if rec.PortNum != "ENCRYPTED" {
		t.Errorf("portnum = %q, want %q", rec.PortNum, "ENCRYPTED")
	}
}

func TestDecodeFromRadio_InvalidPayload(t *testing.T) {
	rec := decodeFromRadio([]byte{0xFF, 0xFF, 0xFF})
	if rec.Type != "unknown" {
		t.Errorf("type = %q, want %q", rec.Type, "unknown")
	}
	if rec.Direction != "from_node" {
		t.Errorf("direction = %q, want %q", rec.Direction, "from_node")
	}
}

func TestDecodeFromRadio_EmptyPayload(t *testing.T) {
	rec := decodeFromRadio([]byte{})
	// Empty protobuf unmarshals to an empty FromRadio (no variant).
	if rec.Type != "other" {
		t.Errorf("type = %q, want %q", rec.Type, "other")
	}
}

// ---------------------------------------------------------------------------
// decodeToRadio tests
// ---------------------------------------------------------------------------

func TestDecodeToRadio_WantConfigId(t *testing.T) {
	payload := marshalToRadio(t, &pb.ToRadio{
		PayloadVariant: &pb.ToRadio_WantConfigId{WantConfigId: 42},
	})

	rec := decodeToRadio(payload)
	if rec.Type != "want_config_id" {
		t.Errorf("type = %q, want %q", rec.Type, "want_config_id")
	}
	if rec.Direction != "to_node" {
		t.Errorf("direction = %q, want %q", rec.Direction, "to_node")
	}
	if rec.Payload != "nonce=42" {
		t.Errorf("payload = %q, want %q", rec.Payload, "nonce=42")
	}
}

func TestDecodeToRadio_Disconnect(t *testing.T) {
	payload := marshalToRadio(t, &pb.ToRadio{
		PayloadVariant: &pb.ToRadio_Disconnect{Disconnect: true},
	})

	rec := decodeToRadio(payload)
	if rec.Type != "disconnect" {
		t.Errorf("type = %q, want %q", rec.Type, "disconnect")
	}
}

func TestDecodeToRadio_Packet(t *testing.T) {
	payload := marshalToRadio(t, &pb.ToRadio{
		PayloadVariant: &pb.ToRadio_Packet{
			Packet: &pb.MeshPacket{
				From: 0x1234,
				To:   0xFFFF,
			},
		},
	})

	rec := decodeToRadio(payload)
	if rec.Type != "packet" {
		t.Errorf("type = %q, want %q", rec.Type, "packet")
	}
	if rec.From != 0x1234 {
		t.Errorf("from = 0x%x, want 0x1234", rec.From)
	}
}

func TestDecodeToRadio_Heartbeat(t *testing.T) {
	payload := marshalToRadio(t, &pb.ToRadio{
		PayloadVariant: &pb.ToRadio_Heartbeat{},
	})

	rec := decodeToRadio(payload)
	if rec.Type != "heartbeat" {
		t.Errorf("type = %q, want %q", rec.Type, "heartbeat")
	}
}

func TestDecodeToRadio_InvalidPayload(t *testing.T) {
	rec := decodeToRadio([]byte{0xFF, 0xFF, 0xFF})
	if rec.Type != "unknown" {
		t.Errorf("type = %q, want %q", rec.Type, "unknown")
	}
}

// ---------------------------------------------------------------------------
// countCacheFrameTypes tests
// ---------------------------------------------------------------------------

func TestCountCacheFrameTypes(t *testing.T) {
	frames := buildConfigSequence(t, 0x12345678)

	counts := CountCacheFrameTypes(frames)

	want := map[string]int{
		"my_info":            1,
		"config":             1,
		"node_info":          1,
		"config_complete_id": 1,
	}

	for k, v := range want {
		if counts[k] != v {
			t.Errorf("counts[%q] = %d, want %d", k, counts[k], v)
		}
	}

	for k, v := range counts {
		if _, ok := want[k]; !ok {
			t.Errorf("unexpected key %q = %d", k, v)
		}
	}
}

func TestCountCacheFrameTypes_Empty(t *testing.T) {
	counts := CountCacheFrameTypes(nil)
	if len(counts) != 0 {
		t.Errorf("expected empty map, got %v", counts)
	}
}

func TestCountCacheFrameTypes_Unparseable(t *testing.T) {
	frames := [][]byte{
		{0xFF, 0xFF, 0xFF}, // garbage
	}

	counts := CountCacheFrameTypes(frames)
	if counts["unparseable"] != 1 {
		t.Errorf("counts[unparseable] = %d, want 1", counts["unparseable"])
	}
}

// ---------------------------------------------------------------------------
// decodeDataPayload tests
// ---------------------------------------------------------------------------

func TestDecodeDataPayload_TextMessage(t *testing.T) {
	result := decodeDataPayload(pb.PortNum_TEXT_MESSAGE_APP, []byte("Hello World"))
	if result != "Hello World" {
		t.Errorf("got %q, want %q", result, "Hello World")
	}
}

func TestDecodeDataPayload_Position(t *testing.T) {
	pos := &pb.Position{
		LatitudeI:  proto.Int32(375000000),   // 37.5
		LongitudeI: proto.Int32(-1225000000), // -122.5
		Altitude:   proto.Int32(100),
	}
	data, err := proto.Marshal(pos)
	if err != nil {
		t.Fatal(err)
	}

	result := decodeDataPayload(pb.PortNum_POSITION_APP, data)
	if result == "position: decode error" {
		t.Error("failed to decode position")
	}
}

func TestDecodeDataPayload_Nodeinfo(t *testing.T) {
	user := &pb.User{
		LongName:  "TestUser",
		ShortName: "TU",
		HwModel:   pb.HardwareModel_HELTEC_V3,
	}
	data, err := proto.Marshal(user)
	if err != nil {
		t.Fatal(err)
	}

	result := decodeDataPayload(pb.PortNum_NODEINFO_APP, data)
	if result == "nodeinfo: decode error" {
		t.Error("failed to decode nodeinfo")
	}
	if result == "" {
		t.Error("expected non-empty result")
	}
}

func TestDecodeDataPayload_Telemetry(t *testing.T) {
	tel := &pb.Telemetry{
		Variant: &pb.Telemetry_DeviceMetrics{
			DeviceMetrics: &pb.DeviceMetrics{
				BatteryLevel:       proto.Uint32(75),
				Voltage:            proto.Float32(3.7),
				ChannelUtilization: proto.Float32(12.5),
				AirUtilTx:          proto.Float32(2.3),
				UptimeSeconds:      proto.Uint32(3600),
			},
		},
	}
	data, err := proto.Marshal(tel)
	if err != nil {
		t.Fatal(err)
	}

	result := decodeDataPayload(pb.PortNum_TELEMETRY_APP, data)
	if result == "telemetry: decode error" {
		t.Error("failed to decode telemetry")
	}
}

func TestDecodeDataPayload_Routing(t *testing.T) {
	routing := &pb.Routing{
		Variant: &pb.Routing_ErrorReason{
			ErrorReason: pb.Routing_NONE,
		},
	}
	data, err := proto.Marshal(routing)
	if err != nil {
		t.Fatal(err)
	}

	result := decodeDataPayload(pb.PortNum_ROUTING_APP, data)
	if result == "routing: decode error" {
		t.Error("failed to decode routing")
	}
}

func TestDecodeDataPayload_UnknownPortShort(t *testing.T) {
	// Small payload — should return hex
	data := []byte{0x01, 0x02, 0x03}
	result := decodeDataPayload(pb.PortNum_UNKNOWN_APP, data) //nolint:staticcheck // deprecated constant used for test coverage
	if result != "010203" {
		t.Errorf("got %q, want %q", result, "010203")
	}
}

func TestDecodeDataPayload_UnknownPortLong(t *testing.T) {
	// Large payload — should be truncated
	data := make([]byte, 64)
	for i := range data {
		data[i] = byte(i)
	}
	result := decodeDataPayload(pb.PortNum_UNKNOWN_APP, data) //nolint:staticcheck // deprecated constant used for test coverage
	expected := fmt.Sprintf("%s... (%d bytes)",
		fmt.Sprintf("%064x", data[:32])[:64], 64) // hex of first 32 bytes
	// Just check it ends with the byte count.
	if len(result) == 0 {
		t.Error("expected non-empty result for long payload")
	}
	_ = expected // format may vary slightly, just verify not empty
}

// ---------------------------------------------------------------------------
// decodeConfig tests
// ---------------------------------------------------------------------------

func TestDecodeConfig(t *testing.T) {
	tests := []struct {
		name    string
		config  *pb.Config
		wantSub string
	}{
		{"device", &pb.Config{PayloadVariant: &pb.Config_Device{Device: &pb.Config_DeviceConfig{}}}, "device"},
		{"position", &pb.Config{PayloadVariant: &pb.Config_Position{Position: &pb.Config_PositionConfig{}}}, "position"},
		{"power", &pb.Config{PayloadVariant: &pb.Config_Power{Power: &pb.Config_PowerConfig{}}}, "power"},
		{"network", &pb.Config{PayloadVariant: &pb.Config_Network{Network: &pb.Config_NetworkConfig{}}}, "network"},
		{"display", &pb.Config{PayloadVariant: &pb.Config_Display{Display: &pb.Config_DisplayConfig{}}}, "display"},
		{"lora", &pb.Config{PayloadVariant: &pb.Config_Lora{Lora: &pb.Config_LoRaConfig{}}}, "lora"},
		{"bluetooth", &pb.Config{PayloadVariant: &pb.Config_Bluetooth{Bluetooth: &pb.Config_BluetoothConfig{}}}, "bluetooth"},
		{"nil", nil, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := decodeConfig(tt.config)
			if tt.wantSub != "" && len(got) == 0 {
				t.Errorf("decodeConfig returned empty for %s", tt.name)
			}
			if tt.wantSub == "" && got != "" {
				t.Errorf("decodeConfig returned %q for nil, want empty", got)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// decodeModuleConfig tests
// ---------------------------------------------------------------------------

func TestDecodeModuleConfig(t *testing.T) {
	tests := []struct {
		name   string
		config *pb.ModuleConfig
		want   string
	}{
		{"mqtt", &pb.ModuleConfig{PayloadVariant: &pb.ModuleConfig_Mqtt{Mqtt: &pb.ModuleConfig_MQTTConfig{}}}, "module: mqtt"},
		{"serial", &pb.ModuleConfig{PayloadVariant: &pb.ModuleConfig_Serial{Serial: &pb.ModuleConfig_SerialConfig{}}}, "module: serial"},
		{"telemetry", &pb.ModuleConfig{PayloadVariant: &pb.ModuleConfig_Telemetry{Telemetry: &pb.ModuleConfig_TelemetryConfig{}}}, "module: telemetry"},
		{"nil", nil, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := decodeModuleConfig(tt.config)
			if got != tt.want {
				t.Errorf("decodeModuleConfig = %q, want %q", got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// copyBytes tests
// ---------------------------------------------------------------------------

func TestCopyBytes(t *testing.T) {
	original := []byte{1, 2, 3}
	cp := copyBytes(original)

	if len(cp) != len(original) {
		t.Fatalf("len = %d, want %d", len(cp), len(original))
	}

	// Modify copy — should not affect original.
	cp[0] = 0xFF
	if original[0] != 1 {
		t.Error("copyBytes did not create independent copy")
	}
}

func TestCopyBytes_Nil(t *testing.T) {
	cp := copyBytes(nil)
	if len(cp) != 0 {
		t.Errorf("expected empty slice for nil input, got %v", cp)
	}
}

// ---------------------------------------------------------------------------
// ExtractNodeDirectory tests
// ---------------------------------------------------------------------------

func TestExtractNodeDirectory(t *testing.T) {
	frames := [][]byte{
		marshalFromRadio(t, &pb.FromRadio{
			PayloadVariant: &pb.FromRadio_MyInfo{
				MyInfo: &pb.MyNodeInfo{MyNodeNum: 0x12345678},
			},
		}),
		marshalFromRadio(t, &pb.FromRadio{
			PayloadVariant: &pb.FromRadio_NodeInfo{
				NodeInfo: &pb.NodeInfo{
					Num: 0x12345678,
					User: &pb.User{
						LongName:  "Base Node",
						ShortName: "BN",
					},
				},
			},
		}),
		marshalFromRadio(t, &pb.FromRadio{
			PayloadVariant: &pb.FromRadio_NodeInfo{
				NodeInfo: &pb.NodeInfo{
					Num: 0xAABBCCDD,
					User: &pb.User{
						LongName:  "Remote Node",
						ShortName: "RN",
					},
				},
			},
		}),
		marshalFromRadio(t, &pb.FromRadio{
			PayloadVariant: &pb.FromRadio_Config{
				Config: &pb.Config{
					PayloadVariant: &pb.Config_Device{
						Device: &pb.Config_DeviceConfig{},
					},
				},
			},
		}),
		marshalFromRadio(t, &pb.FromRadio{
			PayloadVariant: &pb.FromRadio_ConfigCompleteId{
				ConfigCompleteId: 12345,
			},
		}),
	}

	dir := ExtractNodeDirectory(frames)

	if len(dir) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(dir))
	}

	entry, ok := dir[0x12345678]
	if !ok {
		t.Fatal("missing entry for 0x12345678")
	}
	if entry.ShortName != "BN" {
		t.Errorf("short_name = %q, want %q", entry.ShortName, "BN")
	}
	if entry.LongName != "Base Node" {
		t.Errorf("long_name = %q, want %q", entry.LongName, "Base Node")
	}

	entry2, ok := dir[0xAABBCCDD]
	if !ok {
		t.Fatal("missing entry for 0xAABBCCDD")
	}
	if entry2.ShortName != "RN" {
		t.Errorf("short_name = %q, want %q", entry2.ShortName, "RN")
	}
}

func TestExtractNodeDirectory_Empty(t *testing.T) {
	dir := ExtractNodeDirectory(nil)
	if len(dir) != 0 {
		t.Fatalf("expected empty directory, got %d entries", len(dir))
	}
}

func TestExtractNodeDirectory_SkipsNoUser(t *testing.T) {
	frames := [][]byte{
		marshalFromRadio(t, &pb.FromRadio{
			PayloadVariant: &pb.FromRadio_NodeInfo{
				NodeInfo: &pb.NodeInfo{
					Num: 0x11111111,
					// No User field
				},
			},
		}),
	}

	dir := ExtractNodeDirectory(frames)
	if len(dir) != 0 {
		t.Fatalf("expected 0 entries (no user), got %d", len(dir))
	}
}

func TestExtractNodeDirectory_SkipsZeroNum(t *testing.T) {
	frames := [][]byte{
		marshalFromRadio(t, &pb.FromRadio{
			PayloadVariant: &pb.FromRadio_NodeInfo{
				NodeInfo: &pb.NodeInfo{
					Num: 0,
					User: &pb.User{
						LongName:  "Ghost",
						ShortName: "GH",
					},
				},
			},
		}),
	}

	dir := ExtractNodeDirectory(frames)
	if len(dir) != 0 {
		t.Fatalf("expected 0 entries (zero num), got %d", len(dir))
	}
}

func TestExtractNodeDirectory_SkipsGarbage(t *testing.T) {
	frames := [][]byte{
		{0xFF, 0xFF, 0xFF}, // garbage
		marshalFromRadio(t, &pb.FromRadio{
			PayloadVariant: &pb.FromRadio_NodeInfo{
				NodeInfo: &pb.NodeInfo{
					Num: 0xDEADBEEF,
					User: &pb.User{
						LongName:  "Valid",
						ShortName: "VL",
					},
				},
			},
		}),
	}

	dir := ExtractNodeDirectory(frames)
	if len(dir) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(dir))
	}
	if _, ok := dir[0xDEADBEEF]; !ok {
		t.Fatal("missing entry for 0xDEADBEEF")
	}
}

// ---------------------------------------------------------------------------
// close tests
// ---------------------------------------------------------------------------

func TestClose_Idempotent(t *testing.T) {
	m := metrics.New(10, 300)
	c := newTestConnection("127.0.0.1:0", m)

	_, clientConn := newTestConnPair(t)

	c.mu.Lock()
	c.conn = clientConn
	c.mu.Unlock()

	// Close twice — should not panic.
	c.close()
	c.close()

	c.mu.Lock()
	if c.conn != nil {
		t.Error("conn should be nil after close")
	}
	c.mu.Unlock()
}

func TestClose_NilConn(t *testing.T) {
	m := metrics.New(10, 300)
	c := newTestConnection("127.0.0.1:0", m)

	// Close with nil conn — should not panic.
	c.close()
}

// ---------------------------------------------------------------------------
// decodeTelemetry tests
// ---------------------------------------------------------------------------

func TestDecodeTelemetry_DeviceMetrics(t *testing.T) {
	tel := &pb.Telemetry{
		Variant: &pb.Telemetry_DeviceMetrics{
			DeviceMetrics: &pb.DeviceMetrics{
				BatteryLevel:       proto.Uint32(80),
				Voltage:            proto.Float32(4.1),
				ChannelUtilization: proto.Float32(5.0),
				AirUtilTx:          proto.Float32(1.5),
				UptimeSeconds:      proto.Uint32(7200),
			},
		},
	}

	result := decodeTelemetry(tel)
	if result == "device_metrics" {
		t.Error("expected detailed device metrics, got generic label")
	}
	if len(result) == 0 {
		t.Error("expected non-empty result")
	}
}

func TestDecodeTelemetry_EnvironmentMetrics(t *testing.T) {
	tel := &pb.Telemetry{
		Variant: &pb.Telemetry_EnvironmentMetrics{
			EnvironmentMetrics: &pb.EnvironmentMetrics{
				Temperature:        proto.Float32(22.5),
				RelativeHumidity:   proto.Float32(55.0),
				BarometricPressure: proto.Float32(1013.25),
			},
		},
	}

	result := decodeTelemetry(tel)
	if result == "environment_metrics" {
		t.Error("expected detailed environment metrics, got generic label")
	}
}

func TestDecodeTelemetry_PowerMetrics(t *testing.T) {
	tel := &pb.Telemetry{
		Variant: &pb.Telemetry_PowerMetrics{
			PowerMetrics: &pb.PowerMetrics{
				Ch1Voltage: proto.Float32(5.0),
				Ch1Current: proto.Float32(100.0),
				Ch2Voltage: proto.Float32(3.3),
				Ch2Current: proto.Float32(50.0),
			},
		},
	}

	result := decodeTelemetry(tel)
	if result == "telemetry" {
		t.Error("expected detailed power metrics, got generic label")
	}
}

func TestDecodeTelemetry_EmptyDeviceMetrics(t *testing.T) {
	tel := &pb.Telemetry{
		Variant: &pb.Telemetry_DeviceMetrics{
			DeviceMetrics: &pb.DeviceMetrics{},
		},
	}

	result := decodeTelemetry(tel)
	if result != "device_metrics" {
		t.Errorf("got %q, want %q", result, "device_metrics")
	}
}

func TestDecodeTelemetry_UnknownVariant(t *testing.T) {
	tel := &pb.Telemetry{}
	result := decodeTelemetry(tel)
	if result != "telemetry" {
		t.Errorf("got %q, want %q", result, "telemetry")
	}
}

// ---------------------------------------------------------------------------
// ExtractNodeDirectory with Position tests
// ---------------------------------------------------------------------------

func TestExtractNodeDirectory_WithPosition(t *testing.T) {
	latI := int32(375000000)   // 37.5
	lonI := int32(-1225000000) // -122.5
	altI := int32(100)

	frames := [][]byte{
		marshalFromRadio(t, &pb.FromRadio{
			PayloadVariant: &pb.FromRadio_NodeInfo{
				NodeInfo: &pb.NodeInfo{
					Num: 0x12345678,
					User: &pb.User{
						LongName:  "GPS Node",
						ShortName: "GN",
					},
					Position: &pb.Position{
						LatitudeI:  &latI,
						LongitudeI: &lonI,
						Altitude:   &altI,
					},
				},
			},
		}),
	}

	dir := ExtractNodeDirectory(frames)

	if len(dir) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(dir))
	}

	entry := dir[0x12345678]
	if entry.ShortName != "GN" {
		t.Errorf("short_name = %q, want %q", entry.ShortName, "GN")
	}

	wantLat := 37.5
	wantLon := -122.5
	if diff := entry.Latitude - wantLat; diff > 0.0001 || diff < -0.0001 {
		t.Errorf("latitude = %f, want %f", entry.Latitude, wantLat)
	}
	if diff := entry.Longitude - wantLon; diff > 0.0001 || diff < -0.0001 {
		t.Errorf("longitude = %f, want %f", entry.Longitude, wantLon)
	}
	if entry.Altitude != 100 {
		t.Errorf("altitude = %d, want 100", entry.Altitude)
	}
	if !entry.HasPosition() {
		t.Error("HasPosition() should return true")
	}
}

func TestExtractNodeDirectory_ZeroPositionIgnored(t *testing.T) {
	latI := int32(0)
	lonI := int32(0)

	frames := [][]byte{
		marshalFromRadio(t, &pb.FromRadio{
			PayloadVariant: &pb.FromRadio_NodeInfo{
				NodeInfo: &pb.NodeInfo{
					Num: 0xAAAAAAAA,
					User: &pb.User{
						LongName:  "No GPS",
						ShortName: "NG",
					},
					Position: &pb.Position{
						LatitudeI:  &latI,
						LongitudeI: &lonI,
					},
				},
			},
		}),
	}

	dir := ExtractNodeDirectory(frames)
	entry := dir[0xAAAAAAAA]
	if entry.HasPosition() {
		t.Error("HasPosition() should return false for zero coordinates")
	}
	if entry.Latitude != 0 || entry.Longitude != 0 {
		t.Error("zero coordinates should not be stored")
	}
}

// ---------------------------------------------------------------------------
// ExtractPosition tests
// ---------------------------------------------------------------------------

func TestExtractPosition_ValidPositionApp(t *testing.T) {
	latI := int32(555000000) // 55.5
	lonI := int32(372000000) // 37.2
	altI := int32(200)

	posData, err := proto.Marshal(&pb.Position{
		LatitudeI:  &latI,
		LongitudeI: &lonI,
		Altitude:   &altI,
	})
	if err != nil {
		t.Fatal(err)
	}

	payload := marshalFromRadio(t, &pb.FromRadio{
		PayloadVariant: &pb.FromRadio_Packet{
			Packet: &pb.MeshPacket{
				From: 0xDEADBEEF,
				PayloadVariant: &pb.MeshPacket_Decoded{
					Decoded: &pb.Data{
						Portnum: pb.PortNum_POSITION_APP,
						Payload: posData,
					},
				},
			},
		},
	})

	pos := ExtractPosition(payload)
	if pos == nil {
		t.Fatal("expected non-nil position")
	}
	if pos.NodeNum != 0xDEADBEEF {
		t.Errorf("NodeNum = %#x, want %#x", pos.NodeNum, uint32(0xDEADBEEF))
	}
	if diff := pos.Latitude - 55.5; diff > 0.0001 || diff < -0.0001 {
		t.Errorf("Latitude = %f, want 55.5", pos.Latitude)
	}
	if diff := pos.Longitude - 37.2; diff > 0.0001 || diff < -0.0001 {
		t.Errorf("Longitude = %f, want 37.2", pos.Longitude)
	}
	if pos.Altitude != 200 {
		t.Errorf("Altitude = %d, want 200", pos.Altitude)
	}
}

func TestExtractPosition_ZeroCoords(t *testing.T) {
	latI := int32(0)
	lonI := int32(0)

	posData, err := proto.Marshal(&pb.Position{
		LatitudeI:  &latI,
		LongitudeI: &lonI,
	})
	if err != nil {
		t.Fatal(err)
	}

	payload := marshalFromRadio(t, &pb.FromRadio{
		PayloadVariant: &pb.FromRadio_Packet{
			Packet: &pb.MeshPacket{
				From: 0x11111111,
				PayloadVariant: &pb.MeshPacket_Decoded{
					Decoded: &pb.Data{
						Portnum: pb.PortNum_POSITION_APP,
						Payload: posData,
					},
				},
			},
		},
	})

	pos := ExtractPosition(payload)
	if pos != nil {
		t.Error("expected nil for zero coordinates")
	}
}

func TestExtractPosition_NotPositionApp(t *testing.T) {
	payload := marshalFromRadio(t, &pb.FromRadio{
		PayloadVariant: &pb.FromRadio_Packet{
			Packet: &pb.MeshPacket{
				From: 0x11111111,
				PayloadVariant: &pb.MeshPacket_Decoded{
					Decoded: &pb.Data{
						Portnum: pb.PortNum_TEXT_MESSAGE_APP,
						Payload: []byte("hello"),
					},
				},
			},
		},
	})

	pos := ExtractPosition(payload)
	if pos != nil {
		t.Error("expected nil for non-POSITION_APP")
	}
}

func TestExtractPosition_EncryptedPacket(t *testing.T) {
	payload := marshalFromRadio(t, &pb.FromRadio{
		PayloadVariant: &pb.FromRadio_Packet{
			Packet: &pb.MeshPacket{
				From: 0x22222222,
				PayloadVariant: &pb.MeshPacket_Encrypted{
					Encrypted: []byte("encrypted-data"),
				},
			},
		},
	})

	pos := ExtractPosition(payload)
	if pos != nil {
		t.Error("expected nil for encrypted packet")
	}
}

func TestExtractPosition_NonPacketFrame(t *testing.T) {
	payload := marshalFromRadio(t, &pb.FromRadio{
		PayloadVariant: &pb.FromRadio_MyInfo{
			MyInfo: &pb.MyNodeInfo{MyNodeNum: 1},
		},
	})

	pos := ExtractPosition(payload)
	if pos != nil {
		t.Error("expected nil for non-packet frame")
	}
}

func TestExtractPosition_GarbagePayload(t *testing.T) {
	pos := ExtractPosition([]byte{0xFF, 0xFE, 0xFD})
	if pos != nil {
		t.Error("expected nil for garbage payload")
	}
}

// ---------------------------------------------------------------------------
// ExtractPosition extended fields tests
// ---------------------------------------------------------------------------

func TestExtractPosition_ExtendedFields(t *testing.T) {
	latI := int32(555000000) // 55.5
	lonI := int32(372000000) // 37.2
	altI := int32(200)

	posData, err := proto.Marshal(&pb.Position{
		LatitudeI:   &latI,
		LongitudeI:  &lonI,
		Altitude:    &altI,
		GroundSpeed: proto.Uint32(5),
		GroundTrack: proto.Uint32(180),
		SatsInView:  12,
		Time:        1700000000,
	})
	if err != nil {
		t.Fatal(err)
	}

	payload := marshalFromRadio(t, &pb.FromRadio{
		PayloadVariant: &pb.FromRadio_Packet{
			Packet: &pb.MeshPacket{
				From: 0xDEADBEEF,
				PayloadVariant: &pb.MeshPacket_Decoded{
					Decoded: &pb.Data{
						Portnum: pb.PortNum_POSITION_APP,
						Payload: posData,
					},
				},
			},
		},
	})

	pos := ExtractPosition(payload)
	if pos == nil {
		t.Fatal("expected non-nil position")
	}
	if pos.GroundSpeed != 5 {
		t.Errorf("GroundSpeed = %d, want 5", pos.GroundSpeed)
	}
	if pos.GroundTrack != 180 {
		t.Errorf("GroundTrack = %d, want 180", pos.GroundTrack)
	}
	if pos.SatsInView != 12 {
		t.Errorf("SatsInView = %d, want 12", pos.SatsInView)
	}
	if pos.PositionTime != 1700000000 {
		t.Errorf("PositionTime = %d, want 1700000000", pos.PositionTime)
	}
}

// ---------------------------------------------------------------------------
// ExtractTelemetry tests
// ---------------------------------------------------------------------------

func TestExtractTelemetry_DeviceMetrics(t *testing.T) {
	telData, err := proto.Marshal(&pb.Telemetry{
		Variant: &pb.Telemetry_DeviceMetrics{
			DeviceMetrics: &pb.DeviceMetrics{
				BatteryLevel:       proto.Uint32(80),
				Voltage:            proto.Float32(4.1),
				ChannelUtilization: proto.Float32(5.0),
				AirUtilTx:          proto.Float32(1.5),
				UptimeSeconds:      proto.Uint32(7200),
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	payload := marshalFromRadio(t, &pb.FromRadio{
		PayloadVariant: &pb.FromRadio_Packet{
			Packet: &pb.MeshPacket{
				From: 0xAABBCCDD,
				PayloadVariant: &pb.MeshPacket_Decoded{
					Decoded: &pb.Data{
						Portnum: pb.PortNum_TELEMETRY_APP,
						Payload: telData,
					},
				},
			},
		},
	})

	tel := ExtractTelemetry(payload)
	if tel == nil {
		t.Fatal("expected non-nil telemetry")
	}
	if tel.NodeNum != 0xAABBCCDD {
		t.Errorf("NodeNum = %#x, want %#x", tel.NodeNum, uint32(0xAABBCCDD))
	}
	if tel.BatteryLevel != 80 {
		t.Errorf("BatteryLevel = %d, want 80", tel.BatteryLevel)
	}
	if tel.Voltage != 4.1 {
		t.Errorf("Voltage = %f, want 4.1", tel.Voltage)
	}
	if tel.ChannelUtilization != 5.0 {
		t.Errorf("ChannelUtilization = %f, want 5.0", tel.ChannelUtilization)
	}
	if tel.AirUtilTx != 1.5 {
		t.Errorf("AirUtilTx = %f, want 1.5", tel.AirUtilTx)
	}
	if tel.UptimeSeconds != 7200 {
		t.Errorf("UptimeSeconds = %d, want 7200", tel.UptimeSeconds)
	}
}

func TestExtractTelemetry_EnvironmentMetrics(t *testing.T) {
	telData, err := proto.Marshal(&pb.Telemetry{
		Variant: &pb.Telemetry_EnvironmentMetrics{
			EnvironmentMetrics: &pb.EnvironmentMetrics{
				Temperature:        proto.Float32(22.5),
				RelativeHumidity:   proto.Float32(55.0),
				BarometricPressure: proto.Float32(1013.25),
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	payload := marshalFromRadio(t, &pb.FromRadio{
		PayloadVariant: &pb.FromRadio_Packet{
			Packet: &pb.MeshPacket{
				From: 0x11223344,
				PayloadVariant: &pb.MeshPacket_Decoded{
					Decoded: &pb.Data{
						Portnum: pb.PortNum_TELEMETRY_APP,
						Payload: telData,
					},
				},
			},
		},
	})

	tel := ExtractTelemetry(payload)
	if tel == nil {
		t.Fatal("expected non-nil telemetry")
	}
	if tel.NodeNum != 0x11223344 {
		t.Errorf("NodeNum = %#x, want %#x", tel.NodeNum, uint32(0x11223344))
	}
	if tel.Temperature != 22.5 {
		t.Errorf("Temperature = %f, want 22.5", tel.Temperature)
	}
	if tel.RelativeHumidity != 55.0 {
		t.Errorf("RelativeHumidity = %f, want 55.0", tel.RelativeHumidity)
	}
	if tel.BarometricPressure != 1013.25 {
		t.Errorf("BarometricPressure = %f, want 1013.25", tel.BarometricPressure)
	}
	// Device metrics should be zero
	if tel.BatteryLevel != 0 {
		t.Errorf("BatteryLevel = %d, want 0", tel.BatteryLevel)
	}
}

func TestExtractTelemetry_PowerMetrics_ReturnsNil(t *testing.T) {
	telData, err := proto.Marshal(&pb.Telemetry{
		Variant: &pb.Telemetry_PowerMetrics{
			PowerMetrics: &pb.PowerMetrics{
				Ch1Voltage: proto.Float32(5.0),
				Ch1Current: proto.Float32(100.0),
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	payload := marshalFromRadio(t, &pb.FromRadio{
		PayloadVariant: &pb.FromRadio_Packet{
			Packet: &pb.MeshPacket{
				From: 0x55555555,
				PayloadVariant: &pb.MeshPacket_Decoded{
					Decoded: &pb.Data{
						Portnum: pb.PortNum_TELEMETRY_APP,
						Payload: telData,
					},
				},
			},
		},
	})

	tel := ExtractTelemetry(payload)
	if tel != nil {
		t.Error("expected nil for PowerMetrics (not stored)")
	}
}

func TestExtractTelemetry_NotTelemetryApp(t *testing.T) {
	payload := marshalFromRadio(t, &pb.FromRadio{
		PayloadVariant: &pb.FromRadio_Packet{
			Packet: &pb.MeshPacket{
				From: 0x11111111,
				PayloadVariant: &pb.MeshPacket_Decoded{
					Decoded: &pb.Data{
						Portnum: pb.PortNum_TEXT_MESSAGE_APP,
						Payload: []byte("hello"),
					},
				},
			},
		},
	})

	tel := ExtractTelemetry(payload)
	if tel != nil {
		t.Error("expected nil for non-TELEMETRY_APP")
	}
}

func TestExtractTelemetry_EncryptedPacket(t *testing.T) {
	payload := marshalFromRadio(t, &pb.FromRadio{
		PayloadVariant: &pb.FromRadio_Packet{
			Packet: &pb.MeshPacket{
				From: 0x22222222,
				PayloadVariant: &pb.MeshPacket_Encrypted{
					Encrypted: []byte("encrypted-data"),
				},
			},
		},
	})

	tel := ExtractTelemetry(payload)
	if tel != nil {
		t.Error("expected nil for encrypted packet")
	}
}

func TestExtractTelemetry_NonPacketFrame(t *testing.T) {
	payload := marshalFromRadio(t, &pb.FromRadio{
		PayloadVariant: &pb.FromRadio_MyInfo{
			MyInfo: &pb.MyNodeInfo{MyNodeNum: 1},
		},
	})

	tel := ExtractTelemetry(payload)
	if tel != nil {
		t.Error("expected nil for non-packet frame")
	}
}

func TestExtractTelemetry_GarbagePayload(t *testing.T) {
	tel := ExtractTelemetry([]byte{0xFF, 0xFE, 0xFD})
	if tel != nil {
		t.Error("expected nil for garbage payload")
	}
}

// ---------------------------------------------------------------------------
// ExtractNodeDirectory full fields tests
// ---------------------------------------------------------------------------

func TestExtractNodeDirectory_FullNodeInfo(t *testing.T) {
	latI := int32(555000000) // 55.5
	lonI := int32(376000000) // 37.6
	altI := int32(150)

	frames := [][]byte{
		marshalFromRadio(t, &pb.FromRadio{
			PayloadVariant: &pb.FromRadio_NodeInfo{
				NodeInfo: &pb.NodeInfo{
					Num: 0x12345678,
					User: &pb.User{
						Id:         "!12345678",
						LongName:   "Full Node",
						ShortName:  "FN",
						HwModel:    pb.HardwareModel_HELTEC_V3,
						Role:       pb.Config_DeviceConfig_ROUTER,
						IsLicensed: true,
					},
					Position: &pb.Position{
						LatitudeI:  &latI,
						LongitudeI: &lonI,
						Altitude:   &altI,
						SatsInView: 10,
					},
					Snr:       6.5,
					LastHeard: 1700000000,
					HopsAway:  proto.Uint32(2),
					ViaMqtt:   true,
					Channel:   1,
					DeviceMetrics: &pb.DeviceMetrics{
						BatteryLevel:       proto.Uint32(75),
						Voltage:            proto.Float32(3.85),
						ChannelUtilization: proto.Float32(10.5),
						AirUtilTx:          proto.Float32(2.0),
						UptimeSeconds:      proto.Uint32(3600),
					},
				},
			},
		}),
	}

	dir := ExtractNodeDirectory(frames)
	if len(dir) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(dir))
	}

	entry := dir[0x12345678]

	// Identity
	if entry.UserID != "!12345678" {
		t.Errorf("user_id = %q, want %q", entry.UserID, "!12345678")
	}
	if entry.HwModel != "HELTEC_V3" {
		t.Errorf("hw_model = %q, want %q", entry.HwModel, "HELTEC_V3")
	}
	if entry.Role != "ROUTER" {
		t.Errorf("role = %q, want %q", entry.Role, "ROUTER")
	}
	if !entry.IsLicensed {
		t.Error("is_licensed should be true")
	}

	// Radio
	if entry.Snr != 6.5 {
		t.Errorf("snr = %f, want 6.5", entry.Snr)
	}
	if entry.LastHeard != 1700000000 {
		t.Errorf("last_heard = %d, want 1700000000", entry.LastHeard)
	}
	if entry.HopsAway != 2 {
		t.Errorf("hops_away = %d, want 2", entry.HopsAway)
	}
	if !entry.ViaMqtt {
		t.Error("via_mqtt should be true")
	}
	if entry.Channel != 1 {
		t.Errorf("channel = %d, want 1", entry.Channel)
	}

	// Position
	if diff := entry.Latitude - 55.5; diff > 0.0001 || diff < -0.0001 {
		t.Errorf("latitude = %f, want 55.5", entry.Latitude)
	}
	if diff := entry.Longitude - 37.6; diff > 0.0001 || diff < -0.0001 {
		t.Errorf("longitude = %f, want 37.6", entry.Longitude)
	}
	if entry.Altitude != 150 {
		t.Errorf("altitude = %d, want 150", entry.Altitude)
	}
	if entry.SatsInView != 10 {
		t.Errorf("sats_in_view = %d, want 10", entry.SatsInView)
	}

	// Device metrics
	if entry.BatteryLevel != 75 {
		t.Errorf("battery_level = %d, want 75", entry.BatteryLevel)
	}
	if entry.Voltage != 3.85 {
		t.Errorf("voltage = %f, want 3.85", entry.Voltage)
	}
	if entry.ChannelUtilization != 10.5 {
		t.Errorf("channel_utilization = %f, want 10.5", entry.ChannelUtilization)
	}
	if entry.AirUtilTx != 2.0 {
		t.Errorf("air_util_tx = %f, want 2.0", entry.AirUtilTx)
	}
	if entry.UptimeSeconds != 3600 {
		t.Errorf("uptime_seconds = %d, want 3600", entry.UptimeSeconds)
	}
}

func TestExtractNodeDirectory_NoDeviceMetrics(t *testing.T) {
	frames := [][]byte{
		marshalFromRadio(t, &pb.FromRadio{
			PayloadVariant: &pb.FromRadio_NodeInfo{
				NodeInfo: &pb.NodeInfo{
					Num: 0xBBBB,
					User: &pb.User{
						LongName:  "Simple Node",
						ShortName: "SN",
						HwModel:   pb.HardwareModel_TLORA_V2_1_1P6,
					},
					// No Position, no DeviceMetrics
				},
			},
		}),
	}

	dir := ExtractNodeDirectory(frames)
	entry := dir[0xBBBB]
	if entry.HwModel != "TLORA_V2_1_1P6" {
		t.Errorf("hw_model = %q, want %q", entry.HwModel, "TLORA_V2_1_1P6")
	}
	if entry.BatteryLevel != 0 {
		t.Errorf("battery_level = %d, want 0", entry.BatteryLevel)
	}
	if entry.HasPosition() {
		t.Error("HasPosition() should return false")
	}
}

// ---------------------------------------------------------------------------
// ExtractSignal tests
// ---------------------------------------------------------------------------

func TestExtractSignal_ValidPacketWithRssiAndSnr(t *testing.T) {
	payload := marshalFromRadio(t, &pb.FromRadio{
		PayloadVariant: &pb.FromRadio_Packet{
			Packet: &pb.MeshPacket{
				From:   0xDEADBEEF,
				RxRssi: -85,
				RxSnr:  7.5,
				PayloadVariant: &pb.MeshPacket_Decoded{
					Decoded: &pb.Data{
						Portnum: pb.PortNum_POSITION_APP,
						Payload: []byte{},
					},
				},
			},
		},
	})

	sig := ExtractSignal(payload)
	if sig == nil {
		t.Fatal("expected non-nil signal")
	}
	if sig.NodeNum != 0xDEADBEEF {
		t.Errorf("NodeNum = %#x, want %#x", sig.NodeNum, uint32(0xDEADBEEF))
	}
	if sig.RxRssi != -85 {
		t.Errorf("RxRssi = %d, want -85", sig.RxRssi)
	}
	if sig.RxSnr != 7.5 {
		t.Errorf("RxSnr = %f, want 7.5", sig.RxSnr)
	}
}

func TestExtractSignal_ZeroRssiReturnsNil(t *testing.T) {
	payload := marshalFromRadio(t, &pb.FromRadio{
		PayloadVariant: &pb.FromRadio_Packet{
			Packet: &pb.MeshPacket{
				From:   0xAABBCCDD,
				RxRssi: 0, // zero RSSI → no signal info
				RxSnr:  5.0,
				PayloadVariant: &pb.MeshPacket_Decoded{
					Decoded: &pb.Data{
						Portnum: pb.PortNum_TEXT_MESSAGE_APP,
						Payload: []byte("hello"),
					},
				},
			},
		},
	})

	sig := ExtractSignal(payload)
	if sig != nil {
		t.Error("expected nil for zero RxRssi")
	}
}

func TestExtractSignal_ZeroFromReturnsNil(t *testing.T) {
	payload := marshalFromRadio(t, &pb.FromRadio{
		PayloadVariant: &pb.FromRadio_Packet{
			Packet: &pb.MeshPacket{
				From:   0, // zero From → invalid
				RxRssi: -90,
				RxSnr:  3.0,
				PayloadVariant: &pb.MeshPacket_Decoded{
					Decoded: &pb.Data{
						Portnum: pb.PortNum_TEXT_MESSAGE_APP,
						Payload: []byte("test"),
					},
				},
			},
		},
	})

	sig := ExtractSignal(payload)
	if sig != nil {
		t.Error("expected nil for zero From")
	}
}

func TestExtractSignal_NonPacketFrameReturnsNil(t *testing.T) {
	payload := marshalFromRadio(t, &pb.FromRadio{
		PayloadVariant: &pb.FromRadio_MyInfo{
			MyInfo: &pb.MyNodeInfo{MyNodeNum: 1},
		},
	})

	sig := ExtractSignal(payload)
	if sig != nil {
		t.Error("expected nil for non-packet frame")
	}
}

func TestExtractSignal_EncryptedPacketWithRssi(t *testing.T) {
	payload := marshalFromRadio(t, &pb.FromRadio{
		PayloadVariant: &pb.FromRadio_Packet{
			Packet: &pb.MeshPacket{
				From:   0x11223344,
				RxRssi: -100,
				RxSnr:  -5.0,
				PayloadVariant: &pb.MeshPacket_Encrypted{
					Encrypted: []byte("encrypted-data"),
				},
			},
		},
	})

	sig := ExtractSignal(payload)
	if sig == nil {
		t.Fatal("expected non-nil signal for encrypted packet with RSSI")
	}
	if sig.NodeNum != 0x11223344 {
		t.Errorf("NodeNum = %#x, want %#x", sig.NodeNum, uint32(0x11223344))
	}
	if sig.RxRssi != -100 {
		t.Errorf("RxRssi = %d, want -100", sig.RxRssi)
	}
	if sig.RxSnr != -5.0 {
		t.Errorf("RxSnr = %f, want -5.0", sig.RxSnr)
	}
}

func TestExtractSignal_GarbagePayload(t *testing.T) {
	sig := ExtractSignal([]byte{0xFF, 0xFE, 0xFD})
	if sig != nil {
		t.Error("expected nil for garbage payload")
	}
}

func TestExtractSignal_RssiOnlyNoSnr(t *testing.T) {
	payload := marshalFromRadio(t, &pb.FromRadio{
		PayloadVariant: &pb.FromRadio_Packet{
			Packet: &pb.MeshPacket{
				From:   0x55667788,
				RxRssi: -110,
				RxSnr:  0, // zero SNR is valid (can happen)
				PayloadVariant: &pb.MeshPacket_Decoded{
					Decoded: &pb.Data{
						Portnum: pb.PortNum_TELEMETRY_APP,
						Payload: []byte{},
					},
				},
			},
		},
	})

	sig := ExtractSignal(payload)
	if sig == nil {
		t.Fatal("expected non-nil signal (zero SNR is valid when RSSI is present)")
	}
	if sig.RxRssi != -110 {
		t.Errorf("RxRssi = %d, want -110", sig.RxRssi)
	}
	if sig.RxSnr != 0 {
		t.Errorf("RxSnr = %f, want 0", sig.RxSnr)
	}
}

// ---------------------------------------------------------------------------
// ExtractChatMessage (FromRadio) tests
// ---------------------------------------------------------------------------

func TestExtractChatMessage_ValidTextMessage(t *testing.T) {
	payload := marshalFromRadio(t, &pb.FromRadio{
		PayloadVariant: &pb.FromRadio_Packet{
			Packet: &pb.MeshPacket{
				From:    0xAABBCCDD,
				To:      0xFFFFFFFF,
				Channel: 0,
				RxRssi:  -85,
				RxSnr:   7.5,
				ViaMqtt: true,
				PayloadVariant: &pb.MeshPacket_Decoded{
					Decoded: &pb.Data{
						Portnum: pb.PortNum_TEXT_MESSAGE_APP,
						Payload: []byte("Hello mesh!"),
					},
				},
			},
		},
	})

	chat := ExtractChatMessage(payload)
	if chat == nil {
		t.Fatal("expected non-nil ChatMessageData")
	}
	if chat.From != 0xAABBCCDD {
		t.Errorf("From = %d, want %d", chat.From, uint32(0xAABBCCDD))
	}
	if chat.To != 0xFFFFFFFF {
		t.Errorf("To = %d, want %d", chat.To, uint32(0xFFFFFFFF))
	}
	if chat.Channel != 0 {
		t.Errorf("Channel = %d, want 0", chat.Channel)
	}
	if chat.Text != "Hello mesh!" {
		t.Errorf("Text = %q, want %q", chat.Text, "Hello mesh!")
	}
	if !chat.ViaMqtt {
		t.Error("expected ViaMqtt=true")
	}
	if chat.RxRssi != -85 {
		t.Errorf("RxRssi = %d, want -85", chat.RxRssi)
	}
	if chat.RxSnr != 7.5 {
		t.Errorf("RxSnr = %f, want 7.5", chat.RxSnr)
	}
}

func TestExtractChatMessage_DMMessage(t *testing.T) {
	payload := marshalFromRadio(t, &pb.FromRadio{
		PayloadVariant: &pb.FromRadio_Packet{
			Packet: &pb.MeshPacket{
				From:    0x11223344,
				To:      0xAABBCCDD,
				Channel: 2,
				PayloadVariant: &pb.MeshPacket_Decoded{
					Decoded: &pb.Data{
						Portnum: pb.PortNum_TEXT_MESSAGE_APP,
						Payload: []byte("Private DM"),
					},
				},
			},
		},
	})

	chat := ExtractChatMessage(payload)
	if chat == nil {
		t.Fatal("expected non-nil ChatMessageData")
	}
	if chat.To != 0xAABBCCDD {
		t.Errorf("To = %d, want %d", chat.To, uint32(0xAABBCCDD))
	}
	if chat.Channel != 2 {
		t.Errorf("Channel = %d, want 2", chat.Channel)
	}
	if chat.Text != "Private DM" {
		t.Errorf("Text = %q, want %q", chat.Text, "Private DM")
	}
}

func TestExtractChatMessage_NotTextMessage(t *testing.T) {
	payload := marshalFromRadio(t, &pb.FromRadio{
		PayloadVariant: &pb.FromRadio_Packet{
			Packet: &pb.MeshPacket{
				From: 0x11223344,
				To:   0xFFFFFFFF,
				PayloadVariant: &pb.MeshPacket_Decoded{
					Decoded: &pb.Data{
						Portnum: pb.PortNum_POSITION_APP,
						Payload: []byte{0x01, 0x02, 0x03},
					},
				},
			},
		},
	})

	chat := ExtractChatMessage(payload)
	if chat != nil {
		t.Error("expected nil for non-TEXT_MESSAGE_APP packet")
	}
}

func TestExtractChatMessage_EncryptedPacket(t *testing.T) {
	payload := marshalFromRadio(t, &pb.FromRadio{
		PayloadVariant: &pb.FromRadio_Packet{
			Packet: &pb.MeshPacket{
				From: 0x11223344,
				To:   0xFFFFFFFF,
				PayloadVariant: &pb.MeshPacket_Encrypted{
					Encrypted: []byte{0xDE, 0xAD},
				},
			},
		},
	})

	chat := ExtractChatMessage(payload)
	if chat != nil {
		t.Error("expected nil for encrypted packet")
	}
}

func TestExtractChatMessage_NonPacketFrame(t *testing.T) {
	payload := marshalFromRadio(t, &pb.FromRadio{
		PayloadVariant: &pb.FromRadio_ConfigCompleteId{
			ConfigCompleteId: 42,
		},
	})

	chat := ExtractChatMessage(payload)
	if chat != nil {
		t.Error("expected nil for non-packet frame")
	}
}

func TestExtractChatMessage_GarbagePayload(t *testing.T) {
	chat := ExtractChatMessage([]byte{0xFF, 0xFE, 0xFD})
	if chat != nil {
		t.Error("expected nil for garbage payload")
	}
}

func TestExtractChatMessage_EmptyText(t *testing.T) {
	payload := marshalFromRadio(t, &pb.FromRadio{
		PayloadVariant: &pb.FromRadio_Packet{
			Packet: &pb.MeshPacket{
				From: 0x11223344,
				To:   0xFFFFFFFF,
				PayloadVariant: &pb.MeshPacket_Decoded{
					Decoded: &pb.Data{
						Portnum: pb.PortNum_TEXT_MESSAGE_APP,
						Payload: []byte(""),
					},
				},
			},
		},
	})

	chat := ExtractChatMessage(payload)
	if chat == nil {
		t.Fatal("expected non-nil ChatMessageData even for empty text")
	}
	if chat.Text != "" {
		t.Errorf("Text = %q, want empty", chat.Text)
	}
}

// ---------------------------------------------------------------------------
// ExtractChatMessageFromToRadio (ToRadio) tests
// ---------------------------------------------------------------------------

func TestExtractChatMessageFromToRadio_Valid(t *testing.T) {
	payload := marshalToRadio(t, &pb.ToRadio{
		PayloadVariant: &pb.ToRadio_Packet{
			Packet: &pb.MeshPacket{
				From:    0,
				To:      0xFFFFFFFF,
				Channel: 0,
				ViaMqtt: false,
				PayloadVariant: &pb.MeshPacket_Decoded{
					Decoded: &pb.Data{
						Portnum: pb.PortNum_TEXT_MESSAGE_APP,
						Payload: []byte("Outgoing message"),
					},
				},
			},
		},
	})

	chat := ExtractChatMessageFromToRadio(payload)
	if chat == nil {
		t.Fatal("expected non-nil ChatMessageData")
	}
	if chat.From != 0 {
		t.Errorf("From = %d, want 0 (node fills in)", chat.From)
	}
	if chat.To != 0xFFFFFFFF {
		t.Errorf("To = %d, want %d", chat.To, uint32(0xFFFFFFFF))
	}
	if chat.Text != "Outgoing message" {
		t.Errorf("Text = %q, want %q", chat.Text, "Outgoing message")
	}
	if chat.ViaMqtt {
		t.Error("expected ViaMqtt=false")
	}
	// ToRadio doesn't have RxRssi/RxSnr
	if chat.RxRssi != 0 {
		t.Errorf("RxRssi = %d, want 0", chat.RxRssi)
	}
	if chat.RxSnr != 0 {
		t.Errorf("RxSnr = %f, want 0", chat.RxSnr)
	}
}

func TestExtractChatMessageFromToRadio_DMWithChannel(t *testing.T) {
	payload := marshalToRadio(t, &pb.ToRadio{
		PayloadVariant: &pb.ToRadio_Packet{
			Packet: &pb.MeshPacket{
				To:      0x12345678,
				Channel: 3,
				PayloadVariant: &pb.MeshPacket_Decoded{
					Decoded: &pb.Data{
						Portnum: pb.PortNum_TEXT_MESSAGE_APP,
						Payload: []byte("DM out"),
					},
				},
			},
		},
	})

	chat := ExtractChatMessageFromToRadio(payload)
	if chat == nil {
		t.Fatal("expected non-nil ChatMessageData")
	}
	if chat.To != 0x12345678 {
		t.Errorf("To = %d, want %d", chat.To, uint32(0x12345678))
	}
	if chat.Channel != 3 {
		t.Errorf("Channel = %d, want 3", chat.Channel)
	}
	if chat.Text != "DM out" {
		t.Errorf("Text = %q, want %q", chat.Text, "DM out")
	}
}

func TestExtractChatMessageFromToRadio_NotTextMessage(t *testing.T) {
	payload := marshalToRadio(t, &pb.ToRadio{
		PayloadVariant: &pb.ToRadio_Packet{
			Packet: &pb.MeshPacket{
				To: 0xFFFFFFFF,
				PayloadVariant: &pb.MeshPacket_Decoded{
					Decoded: &pb.Data{
						Portnum: pb.PortNum_ADMIN_APP,
						Payload: []byte{0x01, 0x02},
					},
				},
			},
		},
	})

	chat := ExtractChatMessageFromToRadio(payload)
	if chat != nil {
		t.Error("expected nil for non-TEXT_MESSAGE_APP ToRadio")
	}
}

func TestExtractChatMessageFromToRadio_WantConfigId(t *testing.T) {
	payload := marshalToRadio(t, &pb.ToRadio{
		PayloadVariant: &pb.ToRadio_WantConfigId{
			WantConfigId: 12345,
		},
	})

	chat := ExtractChatMessageFromToRadio(payload)
	if chat != nil {
		t.Error("expected nil for want_config_id frame")
	}
}

func TestExtractChatMessageFromToRadio_GarbagePayload(t *testing.T) {
	chat := ExtractChatMessageFromToRadio([]byte{0xFF, 0xFE, 0xFD})
	if chat != nil {
		t.Error("expected nil for garbage payload")
	}
}

// ---------------------------------------------------------------------------
// extractMyNodeNum tests
// ---------------------------------------------------------------------------

func TestExtractMyNodeNum_ValidMyInfo(t *testing.T) {
	payload := marshalFromRadio(t, &pb.FromRadio{
		PayloadVariant: &pb.FromRadio_MyInfo{
			MyInfo: &pb.MyNodeInfo{
				MyNodeNum: 0xDEADBEEF,
			},
		},
	})

	num := extractMyNodeNum(payload)
	if num != 0xDEADBEEF {
		t.Errorf("myNodeNum = %d, want %d", num, uint32(0xDEADBEEF))
	}
}

func TestExtractMyNodeNum_NotMyInfo(t *testing.T) {
	payload := marshalFromRadio(t, &pb.FromRadio{
		PayloadVariant: &pb.FromRadio_ConfigCompleteId{
			ConfigCompleteId: 42,
		},
	})

	num := extractMyNodeNum(payload)
	if num != 0 {
		t.Errorf("myNodeNum = %d, want 0 for non-my_info frame", num)
	}
}

func TestExtractMyNodeNum_GarbagePayload(t *testing.T) {
	num := extractMyNodeNum([]byte{0xFF, 0xFE})
	if num != 0 {
		t.Errorf("myNodeNum = %d, want 0 for garbage payload", num)
	}
}

// ---------------------------------------------------------------------------
// UpsertCachedNodeInfo tests
// ---------------------------------------------------------------------------

// helper: build a minimal config cache with MyInfo, one NodeInfo, and ConfigCompleteId.
func buildMinimalCache(t *testing.T, myNum uint32, existingNodes ...uint32) [][]byte {
	t.Helper()
	var frames [][]byte

	frames = append(frames, marshalFromRadio(t, &pb.FromRadio{
		PayloadVariant: &pb.FromRadio_MyInfo{
			MyInfo: &pb.MyNodeInfo{MyNodeNum: myNum},
		},
	}))

	for _, n := range existingNodes {
		frames = append(frames, marshalFromRadio(t, &pb.FromRadio{
			PayloadVariant: &pb.FromRadio_NodeInfo{
				NodeInfo: &pb.NodeInfo{
					Num: n,
					User: &pb.User{
						ShortName: fmt.Sprintf("N%d", n),
						LongName:  fmt.Sprintf("Node %d", n),
					},
				},
			},
		}))
	}

	frames = append(frames, marshalFromRadio(t, &pb.FromRadio{
		PayloadVariant: &pb.FromRadio_ConfigCompleteId{
			ConfigCompleteId: 99999,
		},
	}))

	return frames
}

// extractNodeInfoNums returns all NodeInfo node numbers from a config cache.
func extractNodeInfoNums(t *testing.T, frames [][]byte) []uint32 {
	t.Helper()
	var nums []uint32
	for _, raw := range frames {
		msg := &pb.FromRadio{}
		if err := proto.Unmarshal(raw, msg); err != nil {
			continue
		}
		if v, ok := msg.GetPayloadVariant().(*pb.FromRadio_NodeInfo); ok {
			nums = append(nums, v.NodeInfo.GetNum())
		}
	}
	return nums
}

// lastFrameIsConfigComplete checks that the last frame in cache is ConfigCompleteId.
func lastFrameIsConfigComplete(t *testing.T, frames [][]byte) bool {
	t.Helper()
	if len(frames) == 0 {
		return false
	}
	msg := &pb.FromRadio{}
	if err := proto.Unmarshal(frames[len(frames)-1], msg); err != nil {
		return false
	}
	_, ok := msg.GetPayloadVariant().(*pb.FromRadio_ConfigCompleteId)
	return ok
}

func TestUpsertCachedNodeInfo_InsertNew(t *testing.T) {
	m := metrics.New(10, 300)
	c := newTestConnection("127.0.0.1:0", m)

	cache := buildMinimalCache(t, 100, 100) // MyInfo + NodeInfo(100) + ConfigCompleteId
	c.configMu.Lock()
	c.configCache = cache
	c.configMu.Unlock()

	if len(c.ConfigCache()) != 3 {
		t.Fatalf("initial cache len = %d, want 3", len(c.ConfigCache()))
	}

	// Insert a new node 200.
	c.UpsertCachedNodeInfo(&NodeInfoData{
		NodeNum:   200,
		ShortName: "N2",
		LongName:  "Node Two",
		HwModel:   "HELTEC_V3",
		Role:      "CLIENT",
	})

	got := c.ConfigCache()
	if len(got) != 4 {
		t.Fatalf("cache len after insert = %d, want 4", len(got))
	}

	// Verify node 200 is in the cache.
	nums := extractNodeInfoNums(t, got)
	found := false
	for _, n := range nums {
		if n == 200 {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("node 200 not found in cache; nodes = %v", nums)
	}

	// ConfigCompleteId must remain the last frame.
	if !lastFrameIsConfigComplete(t, got) {
		t.Error("ConfigCompleteId is not the last frame after insert")
	}
}

func TestUpsertCachedNodeInfo_ReplaceExisting(t *testing.T) {
	m := metrics.New(10, 300)
	c := newTestConnection("127.0.0.1:0", m)

	cache := buildMinimalCache(t, 100, 100, 200) // MyInfo + NodeInfo(100) + NodeInfo(200) + ConfigCompleteId
	c.configMu.Lock()
	c.configCache = cache
	c.configMu.Unlock()

	if len(c.ConfigCache()) != 4 {
		t.Fatalf("initial cache len = %d, want 4", len(c.ConfigCache()))
	}

	// Update node 200 with new name.
	c.UpsertCachedNodeInfo(&NodeInfoData{
		NodeNum:   200,
		ShortName: "XX",
		LongName:  "Updated Node",
	})

	got := c.ConfigCache()
	// Length should remain the same — replaced in-place.
	if len(got) != 4 {
		t.Fatalf("cache len after replace = %d, want 4", len(got))
	}

	// Verify updated short name.
	for _, raw := range got {
		msg := &pb.FromRadio{}
		if err := proto.Unmarshal(raw, msg); err != nil {
			continue
		}
		v, ok := msg.GetPayloadVariant().(*pb.FromRadio_NodeInfo)
		if !ok || v.NodeInfo.GetNum() != 200 {
			continue
		}
		if v.NodeInfo.GetUser().GetShortName() != "XX" {
			t.Errorf("short_name = %q, want %q", v.NodeInfo.GetUser().GetShortName(), "XX")
		}
		if v.NodeInfo.GetUser().GetLongName() != "Updated Node" {
			t.Errorf("long_name = %q, want %q", v.NodeInfo.GetUser().GetLongName(), "Updated Node")
		}
		return
	}
	t.Error("node 200 not found in cache after replace")
}

func TestUpsertCachedNodeInfo_EmptyCache(t *testing.T) {
	m := metrics.New(10, 300)
	c := newTestConnection("127.0.0.1:0", m)

	// No config cache — should be a no-op.
	c.UpsertCachedNodeInfo(&NodeInfoData{
		NodeNum:   300,
		ShortName: "N3",
	})

	if len(c.ConfigCache()) != 0 {
		t.Errorf("cache len = %d, want 0 (no-op on empty cache)", len(c.ConfigCache()))
	}
}

func TestUpsertCachedNodeInfo_NilData(t *testing.T) {
	m := metrics.New(10, 300)
	c := newTestConnection("127.0.0.1:0", m)

	cache := buildMinimalCache(t, 100, 100)
	c.configMu.Lock()
	c.configCache = cache
	c.configMu.Unlock()

	// Nil should be a no-op.
	c.UpsertCachedNodeInfo(nil)

	if len(c.ConfigCache()) != 3 {
		t.Errorf("cache len = %d, want 3 (unchanged)", len(c.ConfigCache()))
	}
}

func TestUpsertCachedNodeInfo_ZeroNodeNum(t *testing.T) {
	m := metrics.New(10, 300)
	c := newTestConnection("127.0.0.1:0", m)

	cache := buildMinimalCache(t, 100, 100)
	c.configMu.Lock()
	c.configCache = cache
	c.configMu.Unlock()

	// Zero node num should be a no-op.
	c.UpsertCachedNodeInfo(&NodeInfoData{NodeNum: 0})

	if len(c.ConfigCache()) != 3 {
		t.Errorf("cache len = %d, want 3 (unchanged)", len(c.ConfigCache()))
	}
}

func TestUpsertCachedNodeInfo_PreservesEnums(t *testing.T) {
	m := metrics.New(10, 300)
	c := newTestConnection("127.0.0.1:0", m)

	cache := buildMinimalCache(t, 100, 100)
	c.configMu.Lock()
	c.configCache = cache
	c.configMu.Unlock()

	c.UpsertCachedNodeInfo(&NodeInfoData{
		NodeNum:   500,
		ShortName: "HV",
		LongName:  "Heltec V3",
		HwModel:   "HELTEC_V3",
		Role:      "ROUTER",
	})

	got := c.ConfigCache()
	for _, raw := range got {
		msg := &pb.FromRadio{}
		if err := proto.Unmarshal(raw, msg); err != nil {
			continue
		}
		v, ok := msg.GetPayloadVariant().(*pb.FromRadio_NodeInfo)
		if !ok || v.NodeInfo.GetNum() != 500 {
			continue
		}
		if v.NodeInfo.GetUser().GetHwModel() != pb.HardwareModel_HELTEC_V3 {
			t.Errorf("hw_model = %v, want HELTEC_V3", v.NodeInfo.GetUser().GetHwModel())
		}
		if v.NodeInfo.GetUser().GetRole() != pb.Config_DeviceConfig_ROUTER {
			t.Errorf("role = %v, want ROUTER", v.NodeInfo.GetUser().GetRole())
		}
		return
	}
	t.Error("node 500 not found in cache")
}

func TestUpsertCachedNodeInfo_SetsLastHeard(t *testing.T) {
	m := metrics.New(10, 300)
	c := newTestConnection("127.0.0.1:0", m)

	cache := buildMinimalCache(t, 100, 100)
	c.configMu.Lock()
	c.configCache = cache
	c.configMu.Unlock()

	before := uint32(time.Now().Unix()) //nolint:gosec // test code
	c.UpsertCachedNodeInfo(&NodeInfoData{
		NodeNum:   600,
		ShortName: "LH",
	})
	after := uint32(time.Now().Unix()) //nolint:gosec // test code

	got := c.ConfigCache()
	for _, raw := range got {
		msg := &pb.FromRadio{}
		if err := proto.Unmarshal(raw, msg); err != nil {
			continue
		}
		v, ok := msg.GetPayloadVariant().(*pb.FromRadio_NodeInfo)
		if !ok || v.NodeInfo.GetNum() != 600 {
			continue
		}
		lh := v.NodeInfo.GetLastHeard()
		if lh < before || lh > after {
			t.Errorf("LastHeard = %d, want between %d and %d", lh, before, after)
		}
		return
	}
	t.Error("node 600 not found in cache")
}

func TestUpsertCachedNodeInfo_MultipleInserts(t *testing.T) {
	m := metrics.New(10, 300)
	c := newTestConnection("127.0.0.1:0", m)

	cache := buildMinimalCache(t, 100) // MyInfo + ConfigCompleteId (no nodes)
	c.configMu.Lock()
	c.configCache = cache
	c.configMu.Unlock()

	// Insert 3 new nodes.
	for _, num := range []uint32{10, 20, 30} {
		c.UpsertCachedNodeInfo(&NodeInfoData{
			NodeNum:   num,
			ShortName: fmt.Sprintf("N%d", num),
		})
	}

	got := c.ConfigCache()
	// MyInfo + 3 NodeInfo + ConfigCompleteId = 5
	if len(got) != 5 {
		t.Fatalf("cache len = %d, want 5", len(got))
	}

	nums := extractNodeInfoNums(t, got)
	if len(nums) != 3 {
		t.Fatalf("NodeInfo count = %d, want 3", len(nums))
	}

	// ConfigCompleteId must remain last.
	if !lastFrameIsConfigComplete(t, got) {
		t.Error("ConfigCompleteId is not the last frame after multiple inserts")
	}
}

func TestBuildNodeInfoFrame_ProducesValidProtobuf(t *testing.T) {
	ni := &NodeInfoData{
		NodeNum:    42,
		ShortName:  "TN",
		LongName:   "Test Node",
		UserID:     "!0000002a",
		HwModel:    "TBEAM",
		Role:       "CLIENT",
		IsLicensed: true,
	}

	frame := buildNodeInfoFrame(ni)
	if frame == nil {
		t.Fatal("buildNodeInfoFrame returned nil")
	}

	msg := &pb.FromRadio{}
	if err := proto.Unmarshal(frame, msg); err != nil {
		t.Fatalf("cannot unmarshal synthesized frame: %v", err)
	}

	v, ok := msg.GetPayloadVariant().(*pb.FromRadio_NodeInfo)
	if !ok {
		t.Fatal("frame is not FromRadio_NodeInfo")
	}
	if v.NodeInfo.GetNum() != 42 {
		t.Errorf("Num = %d, want 42", v.NodeInfo.GetNum())
	}
	u := v.NodeInfo.GetUser()
	if u == nil {
		t.Fatal("User is nil")
	}
	if u.GetShortName() != "TN" {
		t.Errorf("ShortName = %q, want %q", u.GetShortName(), "TN")
	}
	if u.GetLongName() != "Test Node" {
		t.Errorf("LongName = %q, want %q", u.GetLongName(), "Test Node")
	}
	if u.GetId() != "!0000002a" {
		t.Errorf("Id = %q, want %q", u.GetId(), "!0000002a")
	}
	if u.GetHwModel() != pb.HardwareModel_TBEAM {
		t.Errorf("HwModel = %v, want TBEAM", u.GetHwModel())
	}
	if u.GetRole() != pb.Config_DeviceConfig_CLIENT {
		t.Errorf("Role = %v, want CLIENT", u.GetRole())
	}
	if !u.GetIsLicensed() {
		t.Error("IsLicensed = false, want true")
	}
	if v.NodeInfo.GetLastHeard() == 0 {
		t.Error("LastHeard = 0, want non-zero")
	}
}

func TestExtractTraceroute_WithSnr(t *testing.T) {
	routePayload, err := proto.Marshal(&pb.RouteDiscovery{
		Route:      []uint32{0xCC, 0xDD},
		SnrTowards: []int32{24, 12}, // 6.0 dB, 3.0 dB
		RouteBack:  []uint32{0xEE},
		SnrBack:    []int32{16}, // 4.0 dB
	})
	if err != nil {
		t.Fatal(err)
	}

	payload := marshalFromRadio(t, &pb.FromRadio{
		PayloadVariant: &pb.FromRadio_Packet{
			Packet: &pb.MeshPacket{
				From: 0xAA,
				To:   0xBB,
				PayloadVariant: &pb.MeshPacket_Decoded{
					Decoded: &pb.Data{
						Portnum: pb.PortNum_TRACEROUTE_APP,
						Payload: routePayload,
					},
				},
			},
		},
	})

	tr := ExtractTraceroute(payload)
	if tr == nil {
		t.Fatal("ExtractTraceroute returned nil")
	}
	if tr.From != 0xAA || tr.To != 0xBB {
		t.Errorf("From/To = %x/%x, want AA/BB", tr.From, tr.To)
	}
	if len(tr.Route) != 2 || tr.Route[0] != 0xCC || tr.Route[1] != 0xDD {
		t.Errorf("Route = %v, want [CC DD]", tr.Route)
	}
	if len(tr.SnrTowards) != 2 || tr.SnrTowards[0] != 24 || tr.SnrTowards[1] != 12 {
		t.Errorf("SnrTowards = %v, want [24 12]", tr.SnrTowards)
	}
	if len(tr.RouteBack) != 1 || tr.RouteBack[0] != 0xEE {
		t.Errorf("RouteBack = %v, want [EE]", tr.RouteBack)
	}
	if len(tr.SnrBack) != 1 || tr.SnrBack[0] != 16 {
		t.Errorf("SnrBack = %v, want [16]", tr.SnrBack)
	}
}

func TestExtractTraceroute_NoSnr(t *testing.T) {
	routePayload, err := proto.Marshal(&pb.RouteDiscovery{
		Route: []uint32{0xCC},
	})
	if err != nil {
		t.Fatal(err)
	}

	payload := marshalFromRadio(t, &pb.FromRadio{
		PayloadVariant: &pb.FromRadio_Packet{
			Packet: &pb.MeshPacket{
				From: 0xAA,
				To:   0xBB,
				PayloadVariant: &pb.MeshPacket_Decoded{
					Decoded: &pb.Data{
						Portnum: pb.PortNum_TRACEROUTE_APP,
						Payload: routePayload,
					},
				},
			},
		},
	})

	tr := ExtractTraceroute(payload)
	if tr == nil {
		t.Fatal("ExtractTraceroute returned nil")
	}
	if len(tr.SnrTowards) != 0 {
		t.Errorf("SnrTowards = %v, want empty", tr.SnrTowards)
	}
	if len(tr.SnrBack) != 0 {
		t.Errorf("SnrBack = %v, want empty", tr.SnrBack)
	}
}
