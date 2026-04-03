package proxy

import (
	"context"
	"log/slog"
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

// buildTestCache builds a realistic config cache with the given node numbers.
// It creates: MyInfo, Metadata, Config(device), ModuleConfig(mqtt), Channel(0),
// own NodeInfo, other NodeInfos, ConfigCompleteId.
func buildTestCache(t *testing.T, myNodeNum uint32, otherNodeNums []uint32) [][]byte {
	t.Helper()
	var frames [][]byte

	// MyInfo
	frames = append(frames, marshalFromRadio(t, &pb.FromRadio{
		PayloadVariant: &pb.FromRadio_MyInfo{
			MyInfo: &pb.MyNodeInfo{MyNodeNum: myNodeNum},
		},
	}))
	// Metadata
	frames = append(frames, marshalFromRadio(t, &pb.FromRadio{
		PayloadVariant: &pb.FromRadio_Metadata{
			Metadata: &pb.DeviceMetadata{FirmwareVersion: "2.5.0"},
		},
	}))
	// Config (device)
	frames = append(frames, marshalFromRadio(t, &pb.FromRadio{
		PayloadVariant: &pb.FromRadio_Config{
			Config: &pb.Config{
				PayloadVariant: &pb.Config_Device{
					Device: &pb.Config_DeviceConfig{},
				},
			},
		},
	}))
	// ModuleConfig (mqtt)
	frames = append(frames, marshalFromRadio(t, &pb.FromRadio{
		PayloadVariant: &pb.FromRadio_ModuleConfig{
			ModuleConfig: &pb.ModuleConfig{
				PayloadVariant: &pb.ModuleConfig_Mqtt{
					Mqtt: &pb.ModuleConfig_MQTTConfig{},
				},
			},
		},
	}))
	// Channel (index 0)
	frames = append(frames, marshalFromRadio(t, &pb.FromRadio{
		PayloadVariant: &pb.FromRadio_Channel{
			Channel: &pb.Channel{Index: 0},
		},
	}))
	// Own NodeInfo
	frames = append(frames, marshalFromRadio(t, &pb.FromRadio{
		PayloadVariant: &pb.FromRadio_NodeInfo{
			NodeInfo: &pb.NodeInfo{
				Num: myNodeNum,
				User: &pb.User{
					LongName:  "MyNode",
					ShortName: "MN",
				},
			},
		},
	}))
	// Other NodeInfos
	for _, num := range otherNodeNums {
		frames = append(frames, marshalFromRadio(t, &pb.FromRadio{
			PayloadVariant: &pb.FromRadio_NodeInfo{
				NodeInfo: &pb.NodeInfo{
					Num: num,
					User: &pb.User{
						LongName:  "OtherNode",
						ShortName: "ON",
					},
				},
			},
		}))
	}
	// ConfigCompleteId
	frames = append(frames, marshalFromRadio(t, &pb.FromRadio{
		PayloadVariant: &pb.FromRadio_ConfigCompleteId{
			ConfigCompleteId: 99999,
		},
	}))

	return frames
}

// mockNodeConn implements NodeConnection for testing.
type mockNodeConn struct {
	cache    [][]byte
	fromNode chan []byte
	mu       sync.Mutex
	sent     [][]byte
}

func newMockNodeConn(cache [][]byte) *mockNodeConn {
	return &mockNodeConn{
		cache:    cache,
		fromNode: make(chan []byte, 256),
	}
}

func (m *mockNodeConn) ConfigCache() [][]byte {
	// Return a deep copy like the real implementation.
	result := make([][]byte, len(m.cache))
	for i, frame := range m.cache {
		cp := make([]byte, len(frame))
		copy(cp, frame)
		result[i] = cp
	}
	return result
}

func (m *mockNodeConn) FromNode() <-chan []byte { return m.fromNode }

func (m *mockNodeConn) Send(payload []byte) {
	m.mu.Lock()
	m.sent = append(m.sent, payload)
	m.mu.Unlock()
}

func (m *mockNodeConn) Sent() [][]byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make([][]byte, len(m.sent))
	copy(cp, m.sent)
	return cp
}

// countFrameType counts how many frames of a given type are in a slice.
func countFrameType(t *testing.T, frames [][]byte, typeName string) int {
	t.Helper()
	count := 0
	for _, frame := range frames {
		msg := &pb.FromRadio{}
		if err := proto.Unmarshal(frame, msg); err != nil {
			continue
		}
		if frameTypeName(msg) == typeName {
			count++
		}
	}
	return count
}

// hasFrameType checks if a frame of a given type exists in the slice.
func hasFrameType(t *testing.T, frames [][]byte, typeName string) bool {
	t.Helper()
	return countFrameType(t, frames, typeName) > 0
}

// getNodeInfoNums returns all NodeInfo num values from the frames.
func getNodeInfoNums(t *testing.T, frames [][]byte) []uint32 {
	t.Helper()
	var nums []uint32
	for _, frame := range frames {
		msg := &pb.FromRadio{}
		if err := proto.Unmarshal(frame, msg); err != nil {
			continue
		}
		if v, ok := msg.GetPayloadVariant().(*pb.FromRadio_NodeInfo); ok {
			nums = append(nums, v.NodeInfo.GetNum())
		}
	}
	return nums
}

// ---------------------------------------------------------------------------
// filterConfigCache tests
// ---------------------------------------------------------------------------

func TestFilterConfigCache_FullConfig(t *testing.T) {
	cache := buildTestCache(t, 0x12345678, []uint32{0xAAAAAAAA, 0xBBBBBBBB})

	result := filterConfigCache(cache, 12345) // random nonce → full config

	if len(result.Frames) != len(cache) {
		t.Fatalf("full config: got %d frames, want %d", len(result.Frames), len(cache))
	}

	// Stats should have frame counts.
	if result.Stats.FrameCounts["my_info"] != 1 {
		t.Errorf("expected 1 my_info, got %d", result.Stats.FrameCounts["my_info"])
	}
	if result.Stats.FrameCounts["node_info"] != 3 { // own + 2 others
		t.Errorf("expected 3 node_info, got %d", result.Stats.FrameCounts["node_info"])
	}
	if result.Stats.FrameCounts["config_complete_id"] != 1 {
		t.Errorf("expected 1 config_complete_id, got %d", result.Stats.FrameCounts["config_complete_id"])
	}
}

func TestFilterConfigCache_ConfigOnly(t *testing.T) {
	myNum := uint32(0x12345678)
	otherNums := []uint32{0xAAAAAAAA, 0xBBBBBBBB, 0xCCCCCCCC}
	cache := buildTestCache(t, myNum, otherNums)

	result := filterConfigCache(cache, nonceOnlyConfig) // 69420

	// Should include: MyInfo, Metadata, Config, ModuleConfig, Channel,
	// own NodeInfo, ConfigCompleteId = 7 frames.
	if len(result.Frames) != 7 {
		t.Fatalf("config_only: got %d frames, want 7", len(result.Frames))
	}

	// Must include config types.
	if !hasFrameType(t, result.Frames, "my_info") {
		t.Error("config_only: missing my_info")
	}
	if !hasFrameType(t, result.Frames, "metadata") {
		t.Error("config_only: missing metadata")
	}
	if !hasFrameType(t, result.Frames, "config") {
		t.Error("config_only: missing config")
	}
	if !hasFrameType(t, result.Frames, "module_config") {
		t.Error("config_only: missing module_config")
	}
	if !hasFrameType(t, result.Frames, "channel") {
		t.Error("config_only: missing channel")
	}
	if !hasFrameType(t, result.Frames, "config_complete_id") {
		t.Error("config_only: missing config_complete_id")
	}

	// Must include own NodeInfo only.
	nodeNums := getNodeInfoNums(t, result.Frames)
	if len(nodeNums) != 1 {
		t.Fatalf("config_only: got %d NodeInfo frames, want 1", len(nodeNums))
	}
	if nodeNums[0] != myNum {
		t.Errorf("config_only: NodeInfo num = %08x, want %08x", nodeNums[0], myNum)
	}
}

func TestFilterConfigCache_NodesOnly(t *testing.T) {
	myNum := uint32(0x12345678)
	otherNums := []uint32{0xAAAAAAAA, 0xBBBBBBBB}
	cache := buildTestCache(t, myNum, otherNums)

	result := filterConfigCache(cache, nonceOnlyNodes) // 69421

	// Should include: all NodeInfo (3) + ConfigCompleteId (1) = 4 frames.
	if len(result.Frames) != 4 {
		t.Fatalf("nodes_only: got %d frames, want 4", len(result.Frames))
	}

	// All NodeInfo must be present.
	nodeNums := getNodeInfoNums(t, result.Frames)
	if len(nodeNums) != 3 {
		t.Fatalf("nodes_only: got %d NodeInfo frames, want 3", len(nodeNums))
	}

	// Must include ConfigCompleteId.
	if !hasFrameType(t, result.Frames, "config_complete_id") {
		t.Error("nodes_only: missing config_complete_id")
	}

	// Must NOT include config types.
	if hasFrameType(t, result.Frames, "my_info") {
		t.Error("nodes_only: should not include my_info")
	}
	if hasFrameType(t, result.Frames, "config") {
		t.Error("nodes_only: should not include config")
	}
	if hasFrameType(t, result.Frames, "module_config") {
		t.Error("nodes_only: should not include module_config")
	}
	if hasFrameType(t, result.Frames, "channel") {
		t.Error("nodes_only: should not include channel")
	}
	if hasFrameType(t, result.Frames, "metadata") {
		t.Error("nodes_only: should not include metadata")
	}
}

func TestFilterConfigCache_ConfigOnly_OwnNodeFound(t *testing.T) {
	myNum := uint32(0x12345678)
	cache := buildTestCache(t, myNum, []uint32{0xAAAAAAAA})

	result := filterConfigCache(cache, nonceOnlyConfig)

	if !result.Stats.OwnNodeFound {
		t.Error("expected OwnNodeFound=true")
	}
	if result.Stats.MyNodeNum != myNum {
		t.Errorf("MyNodeNum = %08x, want %08x", result.Stats.MyNodeNum, myNum)
	}
}

func TestFilterConfigCache_ConfigOnly_NoOwnNode(t *testing.T) {
	// Build a cache where the own NodeInfo is missing — only other nodes present.
	myNum := uint32(0x12345678)
	var frames [][]byte

	// MyInfo
	frames = append(frames, marshalFromRadio(t, &pb.FromRadio{
		PayloadVariant: &pb.FromRadio_MyInfo{
			MyInfo: &pb.MyNodeInfo{MyNodeNum: myNum},
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
	// OTHER NodeInfo only (not own)
	frames = append(frames, marshalFromRadio(t, &pb.FromRadio{
		PayloadVariant: &pb.FromRadio_NodeInfo{
			NodeInfo: &pb.NodeInfo{Num: 0xAAAAAAAA},
		},
	}))
	// ConfigCompleteId
	frames = append(frames, marshalFromRadio(t, &pb.FromRadio{
		PayloadVariant: &pb.FromRadio_ConfigCompleteId{
			ConfigCompleteId: 99999,
		},
	}))

	result := filterConfigCache(frames, nonceOnlyConfig)

	if result.Stats.OwnNodeFound {
		t.Error("expected OwnNodeFound=false when own NodeInfo is not in cache")
	}
	if result.Stats.MyNodeNum != myNum {
		t.Errorf("MyNodeNum = %08x, want %08x", result.Stats.MyNodeNum, myNum)
	}

	// No NodeInfo should be included.
	nodeNums := getNodeInfoNums(t, result.Frames)
	if len(nodeNums) != 0 {
		t.Errorf("expected 0 NodeInfo frames, got %d", len(nodeNums))
	}
}

func TestFilterConfigCache_EmptyCache(t *testing.T) {
	result := filterConfigCache(nil, nonceOnlyConfig)
	if len(result.Frames) != 0 {
		t.Fatalf("empty cache: got %d frames, want 0", len(result.Frames))
	}
	if len(result.Stats.FrameCounts) != 0 {
		t.Errorf("empty cache: expected empty FrameCounts, got %v", result.Stats.FrameCounts)
	}
}

func TestFilterConfigCache_NoMyInfo(t *testing.T) {
	// Cache without MyInfo — myNodeNum stays 0, no NodeInfo should match.
	var frames [][]byte

	// Config (no MyInfo before it)
	frames = append(frames, marshalFromRadio(t, &pb.FromRadio{
		PayloadVariant: &pb.FromRadio_Config{
			Config: &pb.Config{
				PayloadVariant: &pb.Config_Device{
					Device: &pb.Config_DeviceConfig{},
				},
			},
		},
	}))
	// NodeInfo with non-zero num
	frames = append(frames, marshalFromRadio(t, &pb.FromRadio{
		PayloadVariant: &pb.FromRadio_NodeInfo{
			NodeInfo: &pb.NodeInfo{Num: 0xAAAAAAAA},
		},
	}))
	// ConfigCompleteId
	frames = append(frames, marshalFromRadio(t, &pb.FromRadio{
		PayloadVariant: &pb.FromRadio_ConfigCompleteId{
			ConfigCompleteId: 99999,
		},
	}))

	result := filterConfigCache(frames, nonceOnlyConfig)

	if result.Stats.MyNodeNum != 0 {
		t.Errorf("expected MyNodeNum=0, got %08x", result.Stats.MyNodeNum)
	}
	if result.Stats.OwnNodeFound {
		t.Error("expected OwnNodeFound=false when no MyInfo in cache")
	}

	// Config + ConfigCompleteId should be included, but NOT NodeInfo.
	nodeNums := getNodeInfoNums(t, result.Frames)
	if len(nodeNums) != 0 {
		t.Errorf("expected 0 NodeInfo frames, got %d", len(nodeNums))
	}
	if !hasFrameType(t, result.Frames, "config") {
		t.Error("expected config frame to be included")
	}
	if !hasFrameType(t, result.Frames, "config_complete_id") {
		t.Error("expected config_complete_id frame to be included")
	}
}

func TestFilterConfigCache_FrameCounts(t *testing.T) {
	cache := buildTestCache(t, 0x12345678, []uint32{0xAAAAAAAA, 0xBBBBBBBB})

	result := filterConfigCache(cache, nonceOnlyConfig)

	want := map[string]int{
		"my_info":            1,
		"metadata":           1,
		"config":             1,
		"module_config":      1,
		"channel":            1,
		"node_info":          1, // own only
		"config_complete_id": 1,
	}

	for k, v := range want {
		if result.Stats.FrameCounts[k] != v {
			t.Errorf("FrameCounts[%s] = %d, want %d", k, result.Stats.FrameCounts[k], v)
		}
	}

	// Verify no unexpected keys.
	for k, v := range result.Stats.FrameCounts {
		if _, ok := want[k]; !ok {
			t.Errorf("unexpected FrameCounts key %q = %d", k, v)
		}
	}
}

// ---------------------------------------------------------------------------
// decodeToRadioType tests
// ---------------------------------------------------------------------------

func TestDecodeToRadioType_WantConfigId(t *testing.T) {
	data := marshalToRadio(t, &pb.ToRadio{
		PayloadVariant: &pb.ToRadio_WantConfigId{WantConfigId: 69420},
	})

	msg, err := decodeToRadioType(data)
	if err != nil {
		t.Fatalf("decodeToRadioType: %v", err)
	}

	v, ok := msg.GetPayloadVariant().(*pb.ToRadio_WantConfigId)
	if !ok {
		t.Fatalf("expected WantConfigId variant, got %T", msg.GetPayloadVariant())
	}
	if v.WantConfigId != 69420 {
		t.Errorf("nonce = %d, want 69420", v.WantConfigId)
	}
}

func TestDecodeToRadioType_Disconnect(t *testing.T) {
	data := marshalToRadio(t, &pb.ToRadio{
		PayloadVariant: &pb.ToRadio_Disconnect{Disconnect: true},
	})

	msg, err := decodeToRadioType(data)
	if err != nil {
		t.Fatalf("decodeToRadioType: %v", err)
	}

	_, ok := msg.GetPayloadVariant().(*pb.ToRadio_Disconnect)
	if !ok {
		t.Fatalf("expected Disconnect variant, got %T", msg.GetPayloadVariant())
	}
}

func TestDecodeToRadioType_Packet(t *testing.T) {
	data := marshalToRadio(t, &pb.ToRadio{
		PayloadVariant: &pb.ToRadio_Packet{
			Packet: &pb.MeshPacket{
				From: 0x12345678,
				To:   0xFFFFFFFF,
			},
		},
	})

	msg, err := decodeToRadioType(data)
	if err != nil {
		t.Fatalf("decodeToRadioType: %v", err)
	}

	_, ok := msg.GetPayloadVariant().(*pb.ToRadio_Packet)
	if !ok {
		t.Fatalf("expected Packet variant, got %T", msg.GetPayloadVariant())
	}
}

func TestDecodeToRadioType_InvalidPayload(t *testing.T) {
	_, err := decodeToRadioType([]byte{0xFF, 0xFF, 0xFF})
	if err == nil {
		t.Fatal("expected error for invalid protobuf payload")
	}
}

// ---------------------------------------------------------------------------
// replayCachedConfig tests
// ---------------------------------------------------------------------------

func TestReplayCachedConfig_NonceSubstitution(t *testing.T) {
	myNum := uint32(0x12345678)
	cache := buildTestCache(t, myNum, []uint32{0xAAAAAAAA})
	mockNode := newMockNodeConn(cache)

	m := metrics.New(10)
	p := New(":0", 10, mockNode, m, slog.Default())

	serverConn, clientConn := newTestConnPair(t)
	client := NewClient(clientConn, slog.Default(), m, func([]byte) {}, func(*Client) {})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client.Start(ctx)

	clientNonce := uint32(69420)
	p.replayCachedConfig(client, clientNonce)

	// Give writeLoop time to drain the send channel.
	time.Sleep(100 * time.Millisecond)

	// Read all frames from the server side and find ConfigCompleteId.
	var foundNonce uint32
	var frameCount int
	for {
		_ = serverConn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		payload, err := protocol.ReadFrame(serverConn)
		if err != nil {
			break
		}
		frameCount++
		msg := &pb.FromRadio{}
		if err := proto.Unmarshal(payload, msg); err != nil {
			continue
		}
		if v, ok := msg.GetPayloadVariant().(*pb.FromRadio_ConfigCompleteId); ok {
			foundNonce = v.ConfigCompleteId
		}
	}

	if frameCount == 0 {
		t.Fatal("no frames received from replay")
	}
	if foundNonce != clientNonce {
		t.Errorf("ConfigCompleteId nonce = %d, want %d", foundNonce, clientNonce)
	}
}

func TestReplayCachedConfig_ConfigOnly(t *testing.T) {
	myNum := uint32(0x12345678)
	otherNums := []uint32{0xAAAAAAAA, 0xBBBBBBBB}
	cache := buildTestCache(t, myNum, otherNums)
	mockNode := newMockNodeConn(cache)

	m := metrics.New(10)
	p := New(":0", 10, mockNode, m, slog.Default())

	serverConn, clientConn := newTestConnPair(t)
	client := NewClient(clientConn, slog.Default(), m, func([]byte) {}, func(*Client) {})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client.Start(ctx)

	p.replayCachedConfig(client, nonceOnlyConfig)

	time.Sleep(100 * time.Millisecond)

	// Collect all frames.
	var frames [][]byte
	for {
		_ = serverConn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		payload, err := protocol.ReadFrame(serverConn)
		if err != nil {
			break
		}
		frames = append(frames, payload)
	}

	// Should be 7 frames: MyInfo, Metadata, Config, ModuleConfig, Channel,
	// own NodeInfo, ConfigCompleteId.
	if len(frames) != 7 {
		t.Fatalf("config_only replay: got %d frames, want 7", len(frames))
	}

	// Only own NodeInfo.
	nodeNums := getNodeInfoNums(t, frames)
	if len(nodeNums) != 1 || nodeNums[0] != myNum {
		t.Errorf("config_only replay: NodeInfo nums = %v, want [%08x]", nodeNums, myNum)
	}
}

func TestReplayCachedConfig_NodesOnly(t *testing.T) {
	myNum := uint32(0x12345678)
	otherNums := []uint32{0xAAAAAAAA, 0xBBBBBBBB}
	cache := buildTestCache(t, myNum, otherNums)
	mockNode := newMockNodeConn(cache)

	m := metrics.New(10)
	p := New(":0", 10, mockNode, m, slog.Default())

	serverConn, clientConn := newTestConnPair(t)
	client := NewClient(clientConn, slog.Default(), m, func([]byte) {}, func(*Client) {})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client.Start(ctx)

	p.replayCachedConfig(client, nonceOnlyNodes)

	time.Sleep(100 * time.Millisecond)

	var frames [][]byte
	for {
		_ = serverConn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		payload, err := protocol.ReadFrame(serverConn)
		if err != nil {
			break
		}
		frames = append(frames, payload)
	}

	// Should be 4 frames: 3 NodeInfo + 1 ConfigCompleteId.
	if len(frames) != 4 {
		t.Fatalf("nodes_only replay: got %d frames, want 4", len(frames))
	}

	nodeNums := getNodeInfoNums(t, frames)
	if len(nodeNums) != 3 {
		t.Errorf("nodes_only replay: got %d NodeInfo frames, want 3", len(nodeNums))
	}

	if !hasFrameType(t, frames, "config_complete_id") {
		t.Error("nodes_only replay: missing config_complete_id")
	}
}

func TestReplayCachedConfig_EmptyCache(t *testing.T) {
	mockNode := newMockNodeConn(nil) // empty cache

	m := metrics.New(10)
	p := New(":0", 10, mockNode, m, slog.Default())

	serverConn, clientConn := newTestConnPair(t)
	client := NewClient(clientConn, slog.Default(), m, func([]byte) {}, func(*Client) {})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client.Start(ctx)

	p.replayCachedConfig(client, 12345)

	time.Sleep(100 * time.Millisecond)

	// Should receive no frames.
	_ = serverConn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	_, err := protocol.ReadFrame(serverConn)
	if err == nil {
		t.Fatal("expected no frames from empty cache replay")
	}
}

func TestReplayCachedConfig_ClientDisconnect(t *testing.T) {
	// Build a large cache to increase chance of hitting the disconnect.
	myNum := uint32(0x12345678)
	others := make([]uint32, 200)
	for i := range others {
		others[i] = uint32(0xAA000000 + i)
	}
	cache := buildTestCache(t, myNum, others)
	mockNode := newMockNodeConn(cache)

	m := metrics.New(10)
	p := New(":0", 10, mockNode, m, slog.Default())

	_, clientConn := newTestConnPair(t)
	closeCalled := make(chan struct{})
	client := NewClient(clientConn, slog.Default(), m, func([]byte) {}, func(*Client) {
		close(closeCalled)
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client.Start(ctx)

	// Close the client connection immediately to trigger disconnect during replay.
	_ = clientConn.Close()

	// replayCachedConfig should handle the disconnect gracefully (not panic).
	p.replayCachedConfig(client, 12345)

	// If we get here without panic, the test passes.
}

// ---------------------------------------------------------------------------
// ToRadio interception tests (via handleNewConnection onMessage callback)
// ---------------------------------------------------------------------------

func TestInterception_WantConfigIdNotForwarded(t *testing.T) {
	myNum := uint32(0x12345678)
	cache := buildTestCache(t, myNum, nil)
	mockNode := newMockNodeConn(cache)

	m := metrics.New(10)
	p := New(":0", 10, mockNode, m, slog.Default())

	// Use handleNewConnection which sets up the interception callback.
	serverConn, clientConn := newTestConnPair(t)
	_ = serverConn // proxy reads from clientConn

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p.handleNewConnection(ctx, clientConn)

	// Give time for the client loops to start.
	time.Sleep(50 * time.Millisecond)

	// Send want_config_id from the "client" (server side writes to the proxy client).
	wantConfig := marshalToRadio(t, &pb.ToRadio{
		PayloadVariant: &pb.ToRadio_WantConfigId{WantConfigId: 69420},
	})
	if err := protocol.WriteFrame(serverConn, wantConfig); err != nil {
		t.Fatalf("write want_config_id: %v", err)
	}

	// Give time for interception + replayCachedConfig.
	time.Sleep(200 * time.Millisecond)

	// The want_config_id should NOT have been forwarded to the node.
	sent := mockNode.Sent()
	for _, frame := range sent {
		msg := &pb.ToRadio{}
		if err := proto.Unmarshal(frame, msg); err != nil {
			continue
		}
		if _, ok := msg.GetPayloadVariant().(*pb.ToRadio_WantConfigId); ok {
			t.Error("want_config_id was forwarded to node — should have been intercepted")
		}
	}

	// Client should have received config frames from replay.
	var replayedFrames int
	for {
		_ = serverConn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		_, err := protocol.ReadFrame(serverConn)
		if err != nil {
			break
		}
		replayedFrames++
	}
	if replayedFrames == 0 {
		t.Error("client received no frames from config replay after want_config_id")
	}
}

func TestInterception_DisconnectNotForwarded(t *testing.T) {
	mockNode := newMockNodeConn(nil) // no cache needed for disconnect test

	m := metrics.New(10)
	p := New(":0", 10, mockNode, m, slog.Default())

	serverConn, clientConn := newTestConnPair(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p.handleNewConnection(ctx, clientConn)

	time.Sleep(50 * time.Millisecond)

	// Send disconnect from the "client".
	disconnect := marshalToRadio(t, &pb.ToRadio{
		PayloadVariant: &pb.ToRadio_Disconnect{Disconnect: true},
	})
	if err := protocol.WriteFrame(serverConn, disconnect); err != nil {
		t.Fatalf("write disconnect: %v", err)
	}

	time.Sleep(200 * time.Millisecond)

	// Disconnect should NOT have been forwarded to the node.
	sent := mockNode.Sent()
	for _, frame := range sent {
		msg := &pb.ToRadio{}
		if err := proto.Unmarshal(frame, msg); err != nil {
			continue
		}
		if _, ok := msg.GetPayloadVariant().(*pb.ToRadio_Disconnect); ok {
			t.Error("disconnect was forwarded to node — should have been intercepted")
		}
	}
}

func TestInterception_RegularPacketForwarded(t *testing.T) {
	mockNode := newMockNodeConn(nil)

	m := metrics.New(10)
	p := New(":0", 10, mockNode, m, slog.Default())

	serverConn, clientConn := newTestConnPair(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p.handleNewConnection(ctx, clientConn)

	time.Sleep(50 * time.Millisecond)

	// Send a regular packet from the "client".
	packet := marshalToRadio(t, &pb.ToRadio{
		PayloadVariant: &pb.ToRadio_Packet{
			Packet: &pb.MeshPacket{
				From: 0x12345678,
				To:   0xFFFFFFFF,
			},
		},
	})
	if err := protocol.WriteFrame(serverConn, packet); err != nil {
		t.Fatalf("write packet: %v", err)
	}

	// Give time for forwarding.
	time.Sleep(200 * time.Millisecond)

	// Packet should have been forwarded to the node.
	sent := mockNode.Sent()
	if len(sent) == 0 {
		t.Fatal("regular packet was not forwarded to node")
	}

	// Verify it's the same packet.
	msg := &pb.ToRadio{}
	if err := proto.Unmarshal(sent[0], msg); err != nil {
		t.Fatalf("unmarshal forwarded packet: %v", err)
	}
	if _, ok := msg.GetPayloadVariant().(*pb.ToRadio_Packet); !ok {
		t.Errorf("forwarded frame type = %T, want *ToRadio_Packet", msg.GetPayloadVariant())
	}
}

// ---------------------------------------------------------------------------
// frameTypeName tests
// ---------------------------------------------------------------------------

func TestFrameTypeName(t *testing.T) {
	tests := []struct {
		name string
		msg  *pb.FromRadio
		want string
	}{
		{"my_info", &pb.FromRadio{PayloadVariant: &pb.FromRadio_MyInfo{MyInfo: &pb.MyNodeInfo{}}}, "my_info"},
		{"node_info", &pb.FromRadio{PayloadVariant: &pb.FromRadio_NodeInfo{NodeInfo: &pb.NodeInfo{}}}, "node_info"},
		{"config", &pb.FromRadio{PayloadVariant: &pb.FromRadio_Config{Config: &pb.Config{}}}, "config"},
		{"module_config", &pb.FromRadio{PayloadVariant: &pb.FromRadio_ModuleConfig{ModuleConfig: &pb.ModuleConfig{}}}, "module_config"},
		{"channel", &pb.FromRadio{PayloadVariant: &pb.FromRadio_Channel{Channel: &pb.Channel{}}}, "channel"},
		{"config_complete_id", &pb.FromRadio{PayloadVariant: &pb.FromRadio_ConfigCompleteId{ConfigCompleteId: 1}}, "config_complete_id"},
		{"metadata", &pb.FromRadio{PayloadVariant: &pb.FromRadio_Metadata{Metadata: &pb.DeviceMetadata{}}}, "metadata"},
		{"packet", &pb.FromRadio{PayloadVariant: &pb.FromRadio_Packet{Packet: &pb.MeshPacket{}}}, "packet"},
		{"queue_status", &pb.FromRadio{PayloadVariant: &pb.FromRadio_QueueStatus{QueueStatus: &pb.QueueStatus{}}}, "queue_status"},
		{"log_record", &pb.FromRadio{PayloadVariant: &pb.FromRadio_LogRecord{LogRecord: &pb.LogRecord{}}}, "log_record"},
		{"rebooted", &pb.FromRadio{PayloadVariant: &pb.FromRadio_Rebooted{Rebooted: true}}, "rebooted"},
		{"unknown", &pb.FromRadio{}, "unknown"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := frameTypeName(tt.msg)
			if got != tt.want {
				t.Errorf("frameTypeName() = %q, want %q", got, tt.want)
			}
		})
	}
}
