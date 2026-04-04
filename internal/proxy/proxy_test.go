package proxy

import (
	"context"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	pb "buf.build/gen/go/meshtastic/protobufs/protocolbuffers/go/meshtastic"
	"google.golang.org/protobuf/proto"

	"github.com/jfett/meshtastic-proxy/internal/metrics"
	"github.com/jfett/meshtastic-proxy/internal/node"
	"github.com/jfett/meshtastic-proxy/internal/protocol"
)

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// waitFor polls condition fn until it returns true or timeout elapses.
// Used to replace time.Sleep with deterministic synchronization.
func waitFor(t *testing.T, timeout time.Duration, fn func() bool, failMsg string) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		if fn() {
			return
		}
		select {
		case <-deadline:
			t.Fatal(failMsg)
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
}

// dialWithRetry dials addr repeatedly until success or timeout.
// Useful to wait for a listener to become ready without a fixed sleep.
func dialWithRetry(t *testing.T, addr string, timeout time.Duration) net.Conn {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			return conn
		}
		if time.Now().After(deadline) {
			t.Fatalf("dialWithRetry(%s): timed out after %v: %v", addr, timeout, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
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
func countFrameType(t *testing.T, frames []parsedFrame, typeName string) int {
	t.Helper()
	count := 0
	for _, pf := range frames {
		if pf.Msg == nil {
			continue
		}
		if node.FromRadioTypeName(pf.Msg) == typeName {
			count++
		}
	}
	return count
}

// hasFrameType checks if a frame of a given type exists in the slice.
func hasFrameType(t *testing.T, frames []parsedFrame, typeName string) bool {
	t.Helper()
	return countFrameType(t, frames, typeName) > 0
}

// getNodeInfoNums returns all NodeInfo num values from the frames.
func getNodeInfoNums(t *testing.T, frames []parsedFrame) []uint32 {
	t.Helper()
	var nums []uint32
	for _, pf := range frames {
		if pf.Msg == nil {
			continue
		}
		if v, ok := pf.Msg.GetPayloadVariant().(*pb.FromRadio_NodeInfo); ok {
			nums = append(nums, v.NodeInfo.GetNum())
		}
	}
	return nums
}

// getNodeInfoNumsRaw returns all NodeInfo num values from raw wire frames.
func getNodeInfoNumsRaw(t *testing.T, frames [][]byte) []uint32 {
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

// hasFrameTypeRaw checks if a frame of a given type exists in raw wire frames.
func hasFrameTypeRaw(t *testing.T, frames [][]byte, typeName string) bool {
	t.Helper()
	for _, frame := range frames {
		msg := &pb.FromRadio{}
		if err := proto.Unmarshal(frame, msg); err != nil {
			continue
		}
		if node.FromRadioTypeName(msg) == typeName {
			return true
		}
	}
	return false
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

	m := metrics.New(10, 300)
	p := New(Options{ListenAddr: ":0", MaxClients: 10, ClientSendBuffer: 256, IOSNodeInfoDelay: 50 * time.Millisecond, NodeConn: mockNode, Metrics: m, Logger: slog.Default()})

	serverConn, clientConn := newTestConnPair(t)
	client := NewClient(clientConn, slog.Default(), m, 256, 0, func([]byte) {}, func(*Client) {})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client.Start(ctx)

	clientNonce := uint32(69420)
	p.replayCachedConfig(client, clientNonce)

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

	m := metrics.New(10, 300)
	p := New(Options{ListenAddr: ":0", MaxClients: 10, ClientSendBuffer: 256, IOSNodeInfoDelay: 50 * time.Millisecond, NodeConn: mockNode, Metrics: m, Logger: slog.Default()})

	serverConn, clientConn := newTestConnPair(t)
	client := NewClient(clientConn, slog.Default(), m, 256, 0, func([]byte) {}, func(*Client) {})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client.Start(ctx)

	p.replayCachedConfig(client, nonceOnlyConfig)

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
	nodeNums := getNodeInfoNumsRaw(t, frames)
	if len(nodeNums) != 1 || nodeNums[0] != myNum {
		t.Errorf("config_only replay: NodeInfo nums = %v, want [%08x]", nodeNums, myNum)
	}
}

func TestReplayCachedConfig_NodesOnly(t *testing.T) {
	myNum := uint32(0x12345678)
	otherNums := []uint32{0xAAAAAAAA, 0xBBBBBBBB}
	cache := buildTestCache(t, myNum, otherNums)
	mockNode := newMockNodeConn(cache)

	m := metrics.New(10, 300)
	p := New(Options{ListenAddr: ":0", MaxClients: 10, ClientSendBuffer: 256, IOSNodeInfoDelay: 50 * time.Millisecond, NodeConn: mockNode, Metrics: m, Logger: slog.Default()})

	serverConn, clientConn := newTestConnPair(t)
	client := NewClient(clientConn, slog.Default(), m, 256, 0, func([]byte) {}, func(*Client) {})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client.Start(ctx)

	p.replayCachedConfig(client, nonceOnlyNodes)

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

	nodeNums := getNodeInfoNumsRaw(t, frames)
	if len(nodeNums) != 3 {
		t.Errorf("nodes_only replay: got %d NodeInfo frames, want 3", len(nodeNums))
	}

	if !hasFrameTypeRaw(t, frames, "config_complete_id") {
		t.Error("nodes_only replay: missing config_complete_id")
	}
}

func TestReplayCachedConfig_EmptyCache(t *testing.T) {
	mockNode := newMockNodeConn(nil) // empty cache

	m := metrics.New(10, 300)
	p := New(Options{ListenAddr: ":0", MaxClients: 10, ClientSendBuffer: 256, IOSNodeInfoDelay: 50 * time.Millisecond, NodeConn: mockNode, Metrics: m, Logger: slog.Default()})

	serverConn, clientConn := newTestConnPair(t)
	client := NewClient(clientConn, slog.Default(), m, 256, 0, func([]byte) {}, func(*Client) {})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client.Start(ctx)

	p.replayCachedConfig(client, 12345)

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

	m := metrics.New(10, 300)
	p := New(Options{ListenAddr: ":0", MaxClients: 10, ClientSendBuffer: 256, IOSNodeInfoDelay: 50 * time.Millisecond, NodeConn: mockNode, Metrics: m, Logger: slog.Default()})

	_, clientConn := newTestConnPair(t)
	closeCalled := make(chan struct{})
	client := NewClient(clientConn, slog.Default(), m, 256, 0, func([]byte) {}, func(*Client) {
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

	m := metrics.New(10, 300)
	p := New(Options{ListenAddr: ":0", MaxClients: 10, ClientSendBuffer: 256, IOSNodeInfoDelay: 50 * time.Millisecond, NodeConn: mockNode, Metrics: m, Logger: slog.Default()})

	// Use handleNewConnection which sets up the interception callback.
	serverConn, clientConn := newTestConnPair(t)
	_ = serverConn // proxy reads from clientConn

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p.handleNewConnection(ctx, clientConn)

	// Wait for the client to be registered.
	waitFor(t, 2*time.Second, func() bool { return p.Clients() == 1 }, "client was not registered")

	// Send want_config_id from the "client" (server side writes to the proxy client).
	wantConfig := marshalToRadio(t, &pb.ToRadio{
		PayloadVariant: &pb.ToRadio_WantConfigId{WantConfigId: 69420},
	})
	if err := protocol.WriteFrame(serverConn, wantConfig); err != nil {
		t.Fatalf("write want_config_id: %v", err)
	}

	// Client should have received config frames from replay.
	// ReadFrame with deadline acts as synchronization — no sleep needed.
	var replayedFrames int
	for {
		_ = serverConn.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, err := protocol.ReadFrame(serverConn)
		if err != nil {
			break
		}
		replayedFrames++
	}
	if replayedFrames == 0 {
		t.Error("client received no frames from config replay after want_config_id")
	}

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
}

func TestInterception_DisconnectNotForwarded(t *testing.T) {
	mockNode := newMockNodeConn(nil) // no cache needed for disconnect test

	m := metrics.New(10, 300)
	p := New(Options{ListenAddr: ":0", MaxClients: 10, ClientSendBuffer: 256, IOSNodeInfoDelay: 50 * time.Millisecond, NodeConn: mockNode, Metrics: m, Logger: slog.Default()})

	serverConn, clientConn := newTestConnPair(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p.handleNewConnection(ctx, clientConn)

	waitFor(t, 2*time.Second, func() bool { return p.Clients() == 1 }, "client was not registered")

	// Send disconnect from the "client".
	disconnect := marshalToRadio(t, &pb.ToRadio{
		PayloadVariant: &pb.ToRadio_Disconnect{Disconnect: true},
	})
	if err := protocol.WriteFrame(serverConn, disconnect); err != nil {
		t.Fatalf("write disconnect: %v", err)
	}

	// Wait for the client to be unregistered (disconnect was intercepted and processed).
	waitFor(t, 2*time.Second, func() bool { return p.Clients() == 0 }, "client was not unregistered after disconnect")

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

	m := metrics.New(10, 300)
	p := New(Options{ListenAddr: ":0", MaxClients: 10, ClientSendBuffer: 256, IOSNodeInfoDelay: 50 * time.Millisecond, NodeConn: mockNode, Metrics: m, Logger: slog.Default()})

	serverConn, clientConn := newTestConnPair(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p.handleNewConnection(ctx, clientConn)

	waitFor(t, 2*time.Second, func() bool { return p.Clients() == 1 }, "client was not registered")

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

	// Wait for the packet to be forwarded to the node.
	waitFor(t, 2*time.Second, func() bool { return len(mockNode.Sent()) > 0 }, "regular packet was not forwarded to node")

	// Packet should have been forwarded to the node.
	sent := mockNode.Sent()

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
// broadcast tests
// ---------------------------------------------------------------------------

func TestBroadcast_SendsToAllClients(t *testing.T) {
	mockNode := newMockNodeConn(nil)
	m := metrics.New(10, 300)
	p := New(Options{ListenAddr: ":0", MaxClients: 10, ClientSendBuffer: 256, IOSNodeInfoDelay: 50 * time.Millisecond, NodeConn: mockNode, Metrics: m, Logger: slog.Default()})

	const numClients = 3
	type clientPair struct {
		client *Client
		server net.Conn
	}
	pairs := make([]clientPair, numClients)

	for i := 0; i < numClients; i++ {
		serverConn, clientConn := newTestConnPair(t)
		client := NewClient(clientConn, slog.Default(), m, 256, 0, func([]byte) {}, func(*Client) {})
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		client.Start(ctx)
		p.registerClient(client)
		pairs[i] = clientPair{client: client, server: serverConn}
	}

	payload := []byte("broadcast-test")
	p.broadcast(payload)

	// All clients should receive the frame.
	for i, pair := range pairs {
		got := readFrame(t, pair.server, 2*time.Second)
		if string(got) != "broadcast-test" {
			t.Errorf("client %d: got %q, want %q", i, got, "broadcast-test")
		}
	}
}

func TestBroadcast_NoClientsNoPanic(t *testing.T) {
	mockNode := newMockNodeConn(nil)
	m := metrics.New(10, 300)
	p := New(Options{ListenAddr: ":0", MaxClients: 10, ClientSendBuffer: 256, IOSNodeInfoDelay: 50 * time.Millisecond, NodeConn: mockNode, Metrics: m, Logger: slog.Default()})

	// Should not panic with empty client map.
	p.broadcast([]byte("no-clients"))
}

func TestBroadcast_SkipsSlowConsumer(t *testing.T) {
	mockNode := newMockNodeConn(nil)
	m := metrics.New(10, 300)
	// Use a very small send buffer (1) to trigger slow consumer easily.
	p := New(Options{ListenAddr: ":0", MaxClients: 10, ClientSendBuffer: 1, IOSNodeInfoDelay: 50 * time.Millisecond, NodeConn: mockNode, Metrics: m, Logger: slog.Default()})

	_, clientConn := newTestConnPair(t)
	closeCalled := make(chan struct{}, 1)
	client := NewClient(clientConn, slog.Default(), m, 1, 0, func([]byte) {}, func(c *Client) {
		select {
		case closeCalled <- struct{}{}:
		default:
		}
	})
	// Do NOT start writeLoop — the send channel will fill up.
	p.registerClient(client)

	// First broadcast fills the buffer.
	p.broadcast([]byte("frame1"))

	// Second broadcast should trigger slow consumer disconnect (buffer full).
	p.broadcast([]byte("frame2"))

	select {
	case <-closeCalled:
		// Client was disconnected as expected.
	case <-time.After(2 * time.Second):
		t.Fatal("slow consumer was not disconnected")
	}
}

// ---------------------------------------------------------------------------
// broadcastLoop tests
// ---------------------------------------------------------------------------

func TestBroadcastLoop_ForwardsFromNode(t *testing.T) {
	mockNode := newMockNodeConn(nil)
	m := metrics.New(10, 300)
	p := New(Options{ListenAddr: ":0", MaxClients: 10, ClientSendBuffer: 256, IOSNodeInfoDelay: 50 * time.Millisecond, NodeConn: mockNode, Metrics: m, Logger: slog.Default()})

	// Register a client.
	serverConn, clientConn := newTestConnPair(t)
	client := NewClient(clientConn, slog.Default(), m, 256, 0, func([]byte) {}, func(*Client) {})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client.Start(ctx)
	p.registerClient(client)

	// Start broadcastLoop.
	go p.broadcastLoop(ctx)

	// Push a frame to the node's fromNode channel.
	testPayload := marshalFromRadio(t, &pb.FromRadio{
		PayloadVariant: &pb.FromRadio_Packet{
			Packet: &pb.MeshPacket{
				From: 0x12345678,
				To:   0xFFFFFFFF,
			},
		},
	})
	mockNode.fromNode <- testPayload

	// Client should receive the frame.
	got := readFrame(t, serverConn, 2*time.Second)
	msg := &pb.FromRadio{}
	if err := proto.Unmarshal(got, msg); err != nil {
		t.Fatalf("unmarshal received frame: %v", err)
	}
	if _, ok := msg.GetPayloadVariant().(*pb.FromRadio_Packet); !ok {
		t.Errorf("expected Packet variant, got %T", msg.GetPayloadVariant())
	}
}

func TestBroadcastLoop_MultipleFrames(t *testing.T) {
	mockNode := newMockNodeConn(nil)
	m := metrics.New(10, 300)
	p := New(Options{ListenAddr: ":0", MaxClients: 10, ClientSendBuffer: 256, IOSNodeInfoDelay: 50 * time.Millisecond, NodeConn: mockNode, Metrics: m, Logger: slog.Default()})

	serverConn, clientConn := newTestConnPair(t)
	client := NewClient(clientConn, slog.Default(), m, 256, 0, func([]byte) {}, func(*Client) {})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client.Start(ctx)
	p.registerClient(client)

	go p.broadcastLoop(ctx)

	// Send 5 frames through the node channel.
	const numFrames = 5
	for i := 0; i < numFrames; i++ {
		payload := marshalFromRadio(t, &pb.FromRadio{
			PayloadVariant: &pb.FromRadio_QueueStatus{
				QueueStatus: &pb.QueueStatus{Free: uint32(i)},
			},
		})
		mockNode.fromNode <- payload
	}

	// Read all 5 frames.
	for i := 0; i < numFrames; i++ {
		got := readFrame(t, serverConn, 2*time.Second)
		msg := &pb.FromRadio{}
		if err := proto.Unmarshal(got, msg); err != nil {
			t.Fatalf("frame %d: unmarshal error: %v", i, err)
		}
		qs, ok := msg.GetPayloadVariant().(*pb.FromRadio_QueueStatus)
		if !ok {
			t.Fatalf("frame %d: expected QueueStatus, got %T", i, msg.GetPayloadVariant())
		}
		if qs.QueueStatus.GetFree() != uint32(i) {
			t.Errorf("frame %d: Free = %d, want %d", i, qs.QueueStatus.GetFree(), i)
		}
	}
}

func TestBroadcastLoop_ContextCancel(t *testing.T) {
	mockNode := newMockNodeConn(nil)
	m := metrics.New(10, 300)
	p := New(Options{ListenAddr: ":0", MaxClients: 10, ClientSendBuffer: 256, IOSNodeInfoDelay: 50 * time.Millisecond, NodeConn: mockNode, Metrics: m, Logger: slog.Default()})

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		p.broadcastLoop(ctx)
		close(done)
	}()

	// Cancel the context.
	cancel()

	// broadcastLoop should return promptly.
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("broadcastLoop did not return after context cancel")
	}
}

func TestBroadcastLoop_BroadcastsToMultipleClients(t *testing.T) {
	mockNode := newMockNodeConn(nil)
	m := metrics.New(10, 300)
	p := New(Options{ListenAddr: ":0", MaxClients: 10, ClientSendBuffer: 256, IOSNodeInfoDelay: 50 * time.Millisecond, NodeConn: mockNode, Metrics: m, Logger: slog.Default()})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const numClients = 3
	serverConns := make([]net.Conn, numClients)
	for i := 0; i < numClients; i++ {
		serverConn, clientConn := newTestConnPair(t)
		client := NewClient(clientConn, slog.Default(), m, 256, 0, func([]byte) {}, func(*Client) {})
		client.Start(ctx)
		p.registerClient(client)
		serverConns[i] = serverConn
	}

	go p.broadcastLoop(ctx)

	// Send a frame from the node.
	payload := marshalFromRadio(t, &pb.FromRadio{
		PayloadVariant: &pb.FromRadio_Rebooted{Rebooted: true},
	})
	mockNode.fromNode <- payload

	// All clients should receive it.
	for i, serverConn := range serverConns {
		got := readFrame(t, serverConn, 2*time.Second)
		msg := &pb.FromRadio{}
		if err := proto.Unmarshal(got, msg); err != nil {
			t.Fatalf("client %d: unmarshal error: %v", i, err)
		}
		if _, ok := msg.GetPayloadVariant().(*pb.FromRadio_Rebooted); !ok {
			t.Errorf("client %d: expected Rebooted, got %T", i, msg.GetPayloadVariant())
		}
	}
}

// ---------------------------------------------------------------------------
// Proxy.Run tests
// ---------------------------------------------------------------------------

func TestRun_AcceptsConnections(t *testing.T) {
	mockNode := newMockNodeConn(nil)
	m := metrics.New(10, 300)

	// Find a free port.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	p := New(Options{ListenAddr: addr, MaxClients: 10, ClientSendBuffer: 256, IOSNodeInfoDelay: 50 * time.Millisecond, NodeConn: mockNode, Metrics: m, Logger: slog.Default()})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runErr := make(chan error, 1)
	go func() {
		runErr <- p.Run(ctx)
	}()

	// Connect 2 clients — dialWithRetry waits for listener to be ready.
	conn1 := dialWithRetry(t, addr, 2*time.Second)
	defer func() { _ = conn1.Close() }()

	conn2 := dialWithRetry(t, addr, 2*time.Second)
	defer func() { _ = conn2.Close() }()

	// Wait for both clients to be registered.
	waitFor(t, 2*time.Second, func() bool { return p.Clients() == 2 }, "expected 2 registered clients")

	cancel()
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func TestRun_MaxClientsRejected(t *testing.T) {
	mockNode := newMockNodeConn(nil)
	m := metrics.New(10, 300)

	// Find a free port.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	// maxClients = 1
	p := New(Options{ListenAddr: addr, MaxClients: 1, ClientSendBuffer: 256, IOSNodeInfoDelay: 50 * time.Millisecond, NodeConn: mockNode, Metrics: m, Logger: slog.Default()})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runErr := make(chan error, 1)
	go func() {
		runErr <- p.Run(ctx)
	}()

	// First client should be accepted.
	conn1 := dialWithRetry(t, addr, 2*time.Second)
	defer func() { _ = conn1.Close() }()

	waitFor(t, 2*time.Second, func() bool { return p.Clients() == 1 }, "first client was not registered")

	// Second client should be rejected (connection accepted then immediately closed).
	conn2 := dialWithRetry(t, addr, 2*time.Second)
	defer func() { _ = conn2.Close() }()

	// The rejected connection should be closed by the server.
	// Try to read — should get EOF.
	_ = conn2.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1)
	_, readErr := conn2.Read(buf)
	if readErr == nil {
		t.Error("expected read error on rejected connection, got nil")
	}

	// Client count should still be 1.
	if got := p.Clients(); got != 1 {
		t.Errorf("Clients() = %d, want 1 after rejected connection", got)
	}

	cancel()
	<-runErr
}

func TestRun_GracefulShutdown(t *testing.T) {
	mockNode := newMockNodeConn(nil)
	m := metrics.New(10, 300)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	p := New(Options{ListenAddr: addr, MaxClients: 10, ClientSendBuffer: 256, IOSNodeInfoDelay: 50 * time.Millisecond, NodeConn: mockNode, Metrics: m, Logger: slog.Default()})

	ctx, cancel := context.WithCancel(context.Background())

	runErr := make(chan error, 1)
	go func() {
		runErr <- p.Run(ctx)
	}()

	// Connect 3 clients.
	conns := make([]net.Conn, 3)
	for i := 0; i < 3; i++ {
		c := dialWithRetry(t, addr, 2*time.Second)
		conns[i] = c
		defer func() { _ = c.Close() }()
	}

	waitFor(t, 2*time.Second, func() bool { return p.Clients() == 3 }, "expected 3 registered clients")

	// Cancel context — triggers graceful shutdown.
	cancel()

	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after graceful shutdown")
	}

	// All client connections should be closed by the server.
	for i, c := range conns {
		_ = c.SetReadDeadline(time.Now().Add(time.Second))
		buf := make([]byte, 1)
		_, readErr := c.Read(buf)
		if readErr == nil {
			t.Errorf("client %d: expected connection to be closed after shutdown", i)
		}
	}
}

func TestRun_BroadcastToClients(t *testing.T) {
	// Full integration: Run starts both accept loop and broadcastLoop.
	// Connect a client, send a frame from the node, verify the client receives it.
	mockNode := newMockNodeConn(nil)
	m := metrics.New(10, 300)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	p := New(Options{ListenAddr: addr, MaxClients: 10, ClientSendBuffer: 256, IOSNodeInfoDelay: 50 * time.Millisecond, NodeConn: mockNode, Metrics: m, Logger: slog.Default()})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runErr := make(chan error, 1)
	go func() {
		runErr <- p.Run(ctx)
	}()

	// Connect a client.
	conn := dialWithRetry(t, addr, 2*time.Second)
	defer func() { _ = conn.Close() }()

	waitFor(t, 2*time.Second, func() bool { return p.Clients() == 1 }, "client was not registered")

	// Send a frame from the node — broadcastLoop should deliver it.
	payload := marshalFromRadio(t, &pb.FromRadio{
		PayloadVariant: &pb.FromRadio_Packet{
			Packet: &pb.MeshPacket{
				From: 0xDEADBEEF,
				To:   0xFFFFFFFF,
			},
		},
	})
	mockNode.fromNode <- payload

	// Client should receive the frame.
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	received, readErr := protocol.ReadFrame(conn)
	if readErr != nil {
		t.Fatalf("read frame: %v", readErr)
	}

	msg := &pb.FromRadio{}
	if err := proto.Unmarshal(received, msg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	pkt, ok := msg.GetPayloadVariant().(*pb.FromRadio_Packet)
	if !ok {
		t.Fatalf("expected Packet, got %T", msg.GetPayloadVariant())
	}
	if pkt.Packet.GetFrom() != 0xDEADBEEF {
		t.Errorf("From = %08x, want DEADBEEF", pkt.Packet.GetFrom())
	}

	cancel()
	<-runErr
}

func TestRun_ClientDisconnectUnregisters(t *testing.T) {
	mockNode := newMockNodeConn(nil)
	m := metrics.New(10, 300)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	p := New(Options{ListenAddr: addr, MaxClients: 10, ClientSendBuffer: 256, IOSNodeInfoDelay: 50 * time.Millisecond, NodeConn: mockNode, Metrics: m, Logger: slog.Default()})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runErr := make(chan error, 1)
	go func() {
		runErr <- p.Run(ctx)
	}()

	// Connect a client.
	conn := dialWithRetry(t, addr, 2*time.Second)

	waitFor(t, 2*time.Second, func() bool { return p.Clients() == 1 }, "client was not registered")

	// Disconnect the client.
	_ = conn.Close()

	// Wait for unregister to happen.
	waitFor(t, 3*time.Second, func() bool { return p.Clients() == 0 }, "client was not unregistered after disconnect")

	cancel()
	<-runErr
}

// ---------------------------------------------------------------------------
// node.FromRadioTypeName tests (integration — verifies proxy uses it correctly)
// ---------------------------------------------------------------------------

func TestFromRadioTypeName(t *testing.T) {
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
			got := node.FromRadioTypeName(tt.msg)
			if got != tt.want {
				t.Errorf("FromRadioTypeName() = %q, want %q", got, tt.want)
			}
		})
	}
}
