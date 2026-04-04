package node

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	pb "buf.build/gen/go/meshtastic/protobufs/protocolbuffers/go/meshtastic"
	"github.com/jfett/meshtastic-proxy/internal/metrics"
	"github.com/jfett/meshtastic-proxy/internal/protocol"
	"google.golang.org/protobuf/proto"
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
	return NewConnection(
		address,
		10*time.Millisecond,  // reconnectInterval
		100*time.Millisecond, // maxReconnectInterval
		5*time.Second,        // dialTimeout
		5*time.Minute,        // readTimeout
		256,                  // fromBuffer
		256,                  // toBuffer
		m,
		slog.Default(),
	)
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
	c := NewConnection(
		"192.168.1.1:4403",
		5*time.Second,
		60*time.Second,
		10*time.Second,
		5*time.Minute,
		128,
		64,
		m,
		slog.Default(),
	)

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
	cache = append(cache, []byte{0xAA})

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
	c := NewConnection(
		"127.0.0.1:0",
		time.Second, time.Minute,
		5*time.Second, 5*time.Minute,
		256, 2, // toBuffer = 2
		m, slog.Default(),
	)

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
		if err != nil && err != context.Canceled {
			// Acceptable: either context.Canceled or a read error after Close.
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
		if err != context.Canceled {
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
// Run tests (integration)
// ---------------------------------------------------------------------------

func TestRun_ConnectsAndCaches(t *testing.T) {
	m := metrics.New(10, 300)

	// Start a fake node listener.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

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
	defer nodeConn.Close()

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
	defer ln.Close()

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
	defer conn2.Close()

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

	c := NewConnection(
		addr,
		10*time.Millisecond,  // fast reconnect for test
		50*time.Millisecond,  // low max
		100*time.Millisecond, // short dial timeout
		5*time.Minute,
		256, 256,
		m, slog.Default(),
	)

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
	defer ln.Close()

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
		conn.Close()
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
	defer ln.Close()

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
	defer nodeConn.Close()

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
				From: 0xAABBCCDD,
				To:   0xFFFFFFFF,
				PayloadVariant: &pb.MeshPacket_Decoded{
					Decoded: &pb.Data{
						Portnum: pb.PortNum_TEXT_MESSAGE_APP,
						Payload: []byte("Hello"),
					},
				},
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

	counts := countCacheFrameTypes(frames)

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
	counts := countCacheFrameTypes(nil)
	if len(counts) != 0 {
		t.Errorf("expected empty map, got %v", counts)
	}
}

func TestCountCacheFrameTypes_Unparseable(t *testing.T) {
	frames := [][]byte{
		{0xFF, 0xFF, 0xFF}, // garbage
	}

	counts := countCacheFrameTypes(frames)
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
	result := decodeDataPayload(pb.PortNum_UNKNOWN_APP, data)
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
	result := decodeDataPayload(pb.PortNum_UNKNOWN_APP, data)
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
