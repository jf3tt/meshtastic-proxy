package proxy

import (
	"context"
	"fmt"
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
	cache     [][]byte
	fromNode  chan []byte
	mu        sync.Mutex
	sent      [][]byte
	myNodeNum uint32
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

func (m *mockNodeConn) MyNodeNum() uint32 { return m.myNodeNum }

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
// broadcastToOthers / echoToOtherClients tests
// ---------------------------------------------------------------------------

func TestBroadcastToOthers_SkipsSender(t *testing.T) {
	mockNode := newMockNodeConn(nil)
	m := metrics.New(10, 300)
	p := New(Options{ListenAddr: ":0", MaxClients: 10, ClientSendBuffer: 256, IOSNodeInfoDelay: 50 * time.Millisecond, NodeConn: mockNode, Metrics: m, Logger: slog.Default()})

	const numClients = 3
	type clientPair struct {
		client *Client
		server net.Conn
	}
	pairs := make([]clientPair, numClients)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	for i := 0; i < numClients; i++ {
		serverConn, clientConn := newTestConnPair(t)
		client := NewClient(clientConn, slog.Default(), m, 256, 0, func([]byte) {}, func(*Client) {})
		client.Start(ctx)
		p.registerClient(client)
		pairs[i] = clientPair{client: client, server: serverConn}
	}

	// broadcastToOthers with sender = pairs[0].client
	payload := []byte("echo-test")
	p.broadcastToOthers(payload, pairs[0].client)

	// Clients 1 and 2 should receive the frame.
	for i := 1; i < numClients; i++ {
		got := readFrame(t, pairs[i].server, 2*time.Second)
		if string(got) != "echo-test" {
			t.Errorf("client %d: got %q, want %q", i, got, "echo-test")
		}
	}

	// Client 0 (sender) should NOT receive the frame.
	_ = pairs[0].server.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	_, err := protocol.ReadFrame(pairs[0].server)
	if err == nil {
		t.Error("sender received the echo frame — should have been excluded")
	}
}

func TestBroadcastToOthers_NoClientsNoPanic(t *testing.T) {
	mockNode := newMockNodeConn(nil)
	m := metrics.New(10, 300)
	p := New(Options{ListenAddr: ":0", MaxClients: 10, ClientSendBuffer: 256, IOSNodeInfoDelay: 50 * time.Millisecond, NodeConn: mockNode, Metrics: m, Logger: slog.Default()})

	// Should not panic with empty client map.
	p.broadcastToOthers([]byte("no-clients"), nil)
}

func TestEchoToOtherClients_MeshPacket(t *testing.T) {
	mockNode := newMockNodeConn(nil)
	mockNode.myNodeNum = 0x12345678
	m := metrics.New(10, 300)
	p := New(Options{ListenAddr: ":0", MaxClients: 10, ClientSendBuffer: 256, IOSNodeInfoDelay: 50 * time.Millisecond, NodeConn: mockNode, Metrics: m, Logger: slog.Default()})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Create 2 clients: sender (A) and receiver (B).
	serverA, clientA := newTestConnPair(t)
	clientObjA := NewClient(clientA, slog.Default(), m, 256, 0, func([]byte) {}, func(*Client) {})
	clientObjA.Start(ctx)
	p.registerClient(clientObjA)

	serverB, clientB := newTestConnPair(t)
	clientObjB := NewClient(clientB, slog.Default(), m, 256, 0, func([]byte) {}, func(*Client) {})
	clientObjB.Start(ctx)
	p.registerClient(clientObjB)

	// Client A sends a MeshPacket.
	toRadioPayload := marshalToRadio(t, &pb.ToRadio{
		PayloadVariant: &pb.ToRadio_Packet{
			Packet: &pb.MeshPacket{
				From: 0x12345678,
				To:   0xFFFFFFFF,
				Id:   42,
				PayloadVariant: &pb.MeshPacket_Decoded{
					Decoded: &pb.Data{
						Portnum: pb.PortNum_TEXT_MESSAGE_APP,
						Payload: []byte("hello from A"),
					},
				},
			},
		},
	})

	p.echoToOtherClients(toRadioPayload, clientObjA)

	// Client B should receive a FromRadio with the same MeshPacket.
	got := readFrame(t, serverB, 2*time.Second)
	msg := &pb.FromRadio{}
	if err := proto.Unmarshal(got, msg); err != nil {
		t.Fatalf("unmarshal echo: %v", err)
	}
	pkt, ok := msg.GetPayloadVariant().(*pb.FromRadio_Packet)
	if !ok {
		t.Fatalf("expected FromRadio_Packet, got %T", msg.GetPayloadVariant())
	}
	if pkt.Packet.GetFrom() != 0x12345678 {
		t.Errorf("From = %08x, want 12345678", pkt.Packet.GetFrom())
	}
	if pkt.Packet.GetTo() != 0xFFFFFFFF {
		t.Errorf("To = %08x, want FFFFFFFF", pkt.Packet.GetTo())
	}
	if pkt.Packet.GetId() != 42 {
		t.Errorf("Id = %d, want 42", pkt.Packet.GetId())
	}
	decoded, ok := pkt.Packet.GetPayloadVariant().(*pb.MeshPacket_Decoded)
	if !ok {
		t.Fatalf("expected MeshPacket_Decoded, got %T", pkt.Packet.GetPayloadVariant())
	}
	if string(decoded.Decoded.GetPayload()) != "hello from A" {
		t.Errorf("payload = %q, want %q", decoded.Decoded.GetPayload(), "hello from A")
	}

	// Client A (sender) should NOT receive the echo.
	_ = serverA.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	_, err := protocol.ReadFrame(serverA)
	if err == nil {
		t.Error("sender received the echo frame — should have been excluded")
	}
}

func TestEchoToOtherClients_FillsFromZero(t *testing.T) {
	mockNode := newMockNodeConn(nil)
	mockNode.myNodeNum = 0xAABBCCDD
	m := metrics.New(10, 300)
	p := New(Options{ListenAddr: ":0", MaxClients: 10, ClientSendBuffer: 256, IOSNodeInfoDelay: 50 * time.Millisecond, NodeConn: mockNode, Metrics: m, Logger: slog.Default()})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Sender and receiver.
	_, clientA := newTestConnPair(t)
	clientObjA := NewClient(clientA, slog.Default(), m, 256, 0, func([]byte) {}, func(*Client) {})
	clientObjA.Start(ctx)
	p.registerClient(clientObjA)

	serverB, clientB := newTestConnPair(t)
	clientObjB := NewClient(clientB, slog.Default(), m, 256, 0, func([]byte) {}, func(*Client) {})
	clientObjB.Start(ctx)
	p.registerClient(clientObjB)

	// Client A sends a MeshPacket with From=0 (node fills it normally).
	toRadioPayload := marshalToRadio(t, &pb.ToRadio{
		PayloadVariant: &pb.ToRadio_Packet{
			Packet: &pb.MeshPacket{
				From: 0, // proxy should fill this with myNodeNum
				To:   0xFFFFFFFF,
			},
		},
	})

	p.echoToOtherClients(toRadioPayload, clientObjA)

	// Client B should receive the packet with From = myNodeNum.
	got := readFrame(t, serverB, 2*time.Second)
	msg := &pb.FromRadio{}
	if err := proto.Unmarshal(got, msg); err != nil {
		t.Fatalf("unmarshal echo: %v", err)
	}
	pkt, ok := msg.GetPayloadVariant().(*pb.FromRadio_Packet)
	if !ok {
		t.Fatalf("expected FromRadio_Packet, got %T", msg.GetPayloadVariant())
	}
	if pkt.Packet.GetFrom() != 0xAABBCCDD {
		t.Errorf("From = %08x, want AABBCCDD (myNodeNum)", pkt.Packet.GetFrom())
	}
}

func TestEchoToOtherClients_IgnoresNonPacket(t *testing.T) {
	mockNode := newMockNodeConn(nil)
	m := metrics.New(10, 300)
	p := New(Options{ListenAddr: ":0", MaxClients: 10, ClientSendBuffer: 256, IOSNodeInfoDelay: 50 * time.Millisecond, NodeConn: mockNode, Metrics: m, Logger: slog.Default()})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_, clientA := newTestConnPair(t)
	clientObjA := NewClient(clientA, slog.Default(), m, 256, 0, func([]byte) {}, func(*Client) {})
	clientObjA.Start(ctx)
	p.registerClient(clientObjA)

	serverB, clientB := newTestConnPair(t)
	clientObjB := NewClient(clientB, slog.Default(), m, 256, 0, func([]byte) {}, func(*Client) {})
	clientObjB.Start(ctx)
	p.registerClient(clientObjB)

	// Send a Heartbeat (not a MeshPacket) — should NOT be echoed.
	heartbeat := marshalToRadio(t, &pb.ToRadio{
		PayloadVariant: &pb.ToRadio_Heartbeat{
			Heartbeat: &pb.Heartbeat{},
		},
	})

	p.echoToOtherClients(heartbeat, clientObjA)

	// Client B should NOT receive anything.
	_ = serverB.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	_, err := protocol.ReadFrame(serverB)
	if err == nil {
		t.Error("non-packet frame was echoed — should have been ignored")
	}
}

func TestEchoToOtherClients_InvalidPayload(t *testing.T) {
	mockNode := newMockNodeConn(nil)
	m := metrics.New(10, 300)
	p := New(Options{ListenAddr: ":0", MaxClients: 10, ClientSendBuffer: 256, IOSNodeInfoDelay: 50 * time.Millisecond, NodeConn: mockNode, Metrics: m, Logger: slog.Default()})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	serverB, clientB := newTestConnPair(t)
	clientObjB := NewClient(clientB, slog.Default(), m, 256, 0, func([]byte) {}, func(*Client) {})
	clientObjB.Start(ctx)
	p.registerClient(clientObjB)

	// Invalid protobuf should not panic or send anything.
	p.echoToOtherClients([]byte{0xFF, 0xFF, 0xFF}, nil)

	_ = serverB.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	_, err := protocol.ReadFrame(serverB)
	if err == nil {
		t.Error("invalid payload caused an echo — should have been ignored")
	}
}

func TestInterception_RegularPacketEchoedToOtherClients(t *testing.T) {
	// Integration test: client A sends a MeshPacket via handleNewConnection,
	// client B (registered separately) should receive it as a FromRadio echo.
	mockNode := newMockNodeConn(nil)
	mockNode.myNodeNum = 0x12345678
	m := metrics.New(10, 300)
	p := New(Options{ListenAddr: ":0", MaxClients: 10, ClientSendBuffer: 256, IOSNodeInfoDelay: 50 * time.Millisecond, NodeConn: mockNode, Metrics: m, Logger: slog.Default()})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Register client B first (simple client, not via handleNewConnection).
	serverB, clientB := newTestConnPair(t)
	clientObjB := NewClient(clientB, slog.Default(), m, 256, 0, func([]byte) {}, func(*Client) {})
	clientObjB.Start(ctx)
	p.registerClient(clientObjB)

	// Client A connects via handleNewConnection.
	serverA, clientConnA := newTestConnPair(t)
	p.handleNewConnection(ctx, clientConnA)

	waitFor(t, 2*time.Second, func() bool { return p.Clients() == 2 }, "expected 2 clients")

	// Client A sends a MeshPacket.
	packet := marshalToRadio(t, &pb.ToRadio{
		PayloadVariant: &pb.ToRadio_Packet{
			Packet: &pb.MeshPacket{
				From: 0x12345678,
				To:   0xFFFFFFFF,
				Id:   99,
				PayloadVariant: &pb.MeshPacket_Decoded{
					Decoded: &pb.Data{
						Portnum: pb.PortNum_TEXT_MESSAGE_APP,
						Payload: []byte("integration test"),
					},
				},
			},
		},
	})
	if err := protocol.WriteFrame(serverA, packet); err != nil {
		t.Fatalf("write packet: %v", err)
	}

	// Client B should receive the echoed FromRadio.
	got := readFrame(t, serverB, 2*time.Second)
	msg := &pb.FromRadio{}
	if err := proto.Unmarshal(got, msg); err != nil {
		t.Fatalf("unmarshal echo: %v", err)
	}
	pkt, ok := msg.GetPayloadVariant().(*pb.FromRadio_Packet)
	if !ok {
		t.Fatalf("expected FromRadio_Packet, got %T", msg.GetPayloadVariant())
	}
	if pkt.Packet.GetFrom() != 0x12345678 {
		t.Errorf("From = %08x, want 12345678", pkt.Packet.GetFrom())
	}
	if pkt.Packet.GetId() != 99 {
		t.Errorf("Id = %d, want 99", pkt.Packet.GetId())
	}

	// Also verify the packet was forwarded to the node.
	waitFor(t, 2*time.Second, func() bool { return len(mockNode.Sent()) > 0 }, "packet was not forwarded to node")
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

// ---------------------------------------------------------------------------
// isConfigFrame tests
// ---------------------------------------------------------------------------

func TestIsConfigFrame_ConfigTypes(t *testing.T) {
	// All config frame types must return true.
	configFrames := []*pb.FromRadio{
		{PayloadVariant: &pb.FromRadio_MyInfo{MyInfo: &pb.MyNodeInfo{MyNodeNum: 1}}},
		{PayloadVariant: &pb.FromRadio_NodeInfo{NodeInfo: &pb.NodeInfo{Num: 1}}},
		{PayloadVariant: &pb.FromRadio_Config{Config: &pb.Config{
			PayloadVariant: &pb.Config_Device{Device: &pb.Config_DeviceConfig{}},
		}}},
		{PayloadVariant: &pb.FromRadio_ModuleConfig{ModuleConfig: &pb.ModuleConfig{
			PayloadVariant: &pb.ModuleConfig_Mqtt{Mqtt: &pb.ModuleConfig_MQTTConfig{}},
		}}},
		{PayloadVariant: &pb.FromRadio_Channel{Channel: &pb.Channel{Index: 0}}},
		{PayloadVariant: &pb.FromRadio_ConfigCompleteId{ConfigCompleteId: 99999}},
		{PayloadVariant: &pb.FromRadio_Metadata{Metadata: &pb.DeviceMetadata{FirmwareVersion: "2.5.0"}}},
		{PayloadVariant: &pb.FromRadio_DeviceuiConfig{}},
		{PayloadVariant: &pb.FromRadio_FileInfo{FileInfo: &pb.FileInfo{FileName: "test.bin"}}},
	}

	for _, msg := range configFrames {
		payload := marshalFromRadio(t, msg)
		typeName := node.FromRadioTypeName(msg)
		if !isConfigFrame(payload) {
			t.Errorf("isConfigFrame(%s) = false, want true", typeName)
		}
	}
}

func TestIsConfigFrame_RuntimeTypes(t *testing.T) {
	// All runtime frame types must return false.
	runtimeFrames := []*pb.FromRadio{
		{PayloadVariant: &pb.FromRadio_Packet{Packet: &pb.MeshPacket{
			From: 0x12345678, To: 0xFFFFFFFF,
		}}},
		{PayloadVariant: &pb.FromRadio_QueueStatus{QueueStatus: &pb.QueueStatus{Free: 10}}},
		{PayloadVariant: &pb.FromRadio_LogRecord{LogRecord: &pb.LogRecord{Message: "test"}}},
		{PayloadVariant: &pb.FromRadio_Rebooted{Rebooted: true}},
	}

	for _, msg := range runtimeFrames {
		payload := marshalFromRadio(t, msg)
		typeName := node.FromRadioTypeName(msg)
		if isConfigFrame(payload) {
			t.Errorf("isConfigFrame(%s) = true, want false", typeName)
		}
	}
}

func TestIsConfigFrame_InvalidPayload(t *testing.T) {
	// Invalid protobuf should return false (broadcast, not suppress).
	if isConfigFrame([]byte{0xFF, 0xFF, 0xFF}) {
		t.Error("isConfigFrame(invalid) = true, want false")
	}
}

func TestIsConfigFrame_EmptyFromRadio(t *testing.T) {
	// FromRadio with no payload variant set should return false.
	payload := marshalFromRadio(t, &pb.FromRadio{})
	if isConfigFrame(payload) {
		t.Error("isConfigFrame(empty) = true, want false")
	}
}

// ---------------------------------------------------------------------------
// broadcastLoop config filtering tests
// ---------------------------------------------------------------------------

func TestBroadcastLoop_FiltersConfigFrames(t *testing.T) {
	// Verify that broadcastLoop does NOT broadcast config frames to clients,
	// but DOES broadcast runtime frames (MeshPacket, QueueStatus).
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

	// Send a config frame (MyInfo) — should be suppressed.
	configPayload := marshalFromRadio(t, &pb.FromRadio{
		PayloadVariant: &pb.FromRadio_MyInfo{
			MyInfo: &pb.MyNodeInfo{MyNodeNum: 0x12345678},
		},
	})
	mockNode.fromNode <- configPayload

	// Send a config frame (ConfigCompleteId) — should be suppressed.
	completePayload := marshalFromRadio(t, &pb.FromRadio{
		PayloadVariant: &pb.FromRadio_ConfigCompleteId{
			ConfigCompleteId: 99999,
		},
	})
	mockNode.fromNode <- completePayload

	// Send a runtime frame (MeshPacket) — should be broadcast.
	runtimePayload := marshalFromRadio(t, &pb.FromRadio{
		PayloadVariant: &pb.FromRadio_Packet{
			Packet: &pb.MeshPacket{
				From: 0xDEADBEEF,
				To:   0xFFFFFFFF,
				Id:   42,
			},
		},
	})
	mockNode.fromNode <- runtimePayload

	// The client should receive ONLY the MeshPacket, not the config frames.
	got := readFrame(t, serverConn, 2*time.Second)
	msg := &pb.FromRadio{}
	if err := proto.Unmarshal(got, msg); err != nil {
		t.Fatalf("unmarshal received frame: %v", err)
	}
	pkt, ok := msg.GetPayloadVariant().(*pb.FromRadio_Packet)
	if !ok {
		t.Fatalf("expected Packet variant, got %T", msg.GetPayloadVariant())
	}
	if pkt.Packet.GetFrom() != 0xDEADBEEF {
		t.Errorf("From = %08x, want DEADBEEF", pkt.Packet.GetFrom())
	}
	if pkt.Packet.GetId() != 42 {
		t.Errorf("Id = %d, want 42", pkt.Packet.GetId())
	}

	// No more frames should be available (config frames were suppressed).
	_ = serverConn.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	_, err := protocol.ReadFrame(serverConn)
	if err == nil {
		t.Error("received unexpected frame — config frames should have been filtered")
	}
}

func TestBroadcastLoop_AllConfigTypesSuppressed(t *testing.T) {
	// Verify that ALL config frame types are suppressed by broadcastLoop,
	// while a runtime frame sent afterwards is delivered.
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

	// Send all config frame types — all should be suppressed.
	configFrames := []*pb.FromRadio{
		{PayloadVariant: &pb.FromRadio_MyInfo{MyInfo: &pb.MyNodeInfo{MyNodeNum: 1}}},
		{PayloadVariant: &pb.FromRadio_NodeInfo{NodeInfo: &pb.NodeInfo{Num: 1}}},
		{PayloadVariant: &pb.FromRadio_Config{Config: &pb.Config{
			PayloadVariant: &pb.Config_Device{Device: &pb.Config_DeviceConfig{}},
		}}},
		{PayloadVariant: &pb.FromRadio_ModuleConfig{ModuleConfig: &pb.ModuleConfig{
			PayloadVariant: &pb.ModuleConfig_Mqtt{Mqtt: &pb.ModuleConfig_MQTTConfig{}},
		}}},
		{PayloadVariant: &pb.FromRadio_Channel{Channel: &pb.Channel{Index: 0}}},
		{PayloadVariant: &pb.FromRadio_Metadata{Metadata: &pb.DeviceMetadata{}}},
		{PayloadVariant: &pb.FromRadio_ConfigCompleteId{ConfigCompleteId: 99999}},
		{PayloadVariant: &pb.FromRadio_DeviceuiConfig{}},
	}
	for _, msg := range configFrames {
		mockNode.fromNode <- marshalFromRadio(t, msg)
	}

	// Send a sentinel runtime frame so we know all config frames have been processed.
	sentinel := marshalFromRadio(t, &pb.FromRadio{
		PayloadVariant: &pb.FromRadio_QueueStatus{
			QueueStatus: &pb.QueueStatus{Free: 77},
		},
	})
	mockNode.fromNode <- sentinel

	// The only frame the client should receive is the sentinel QueueStatus.
	got := readFrame(t, serverConn, 2*time.Second)
	msg2 := &pb.FromRadio{}
	if err := proto.Unmarshal(got, msg2); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	qs, ok := msg2.GetPayloadVariant().(*pb.FromRadio_QueueStatus)
	if !ok {
		t.Fatalf("expected QueueStatus (sentinel), got %T", msg2.GetPayloadVariant())
	}
	if qs.QueueStatus.GetFree() != 77 {
		t.Errorf("sentinel Free = %d, want 77", qs.QueueStatus.GetFree())
	}
}

// ---------------------------------------------------------------------------
// Chat cache test helpers
// ---------------------------------------------------------------------------

// buildTextMessagePayload creates a FromRadio protobuf payload containing a
// decoded MeshPacket with TEXT_MESSAGE_APP and the given text.
func buildTextMessagePayload(t *testing.T, from, to uint32, text string) []byte {
	t.Helper()
	return marshalFromRadio(t, &pb.FromRadio{
		PayloadVariant: &pb.FromRadio_Packet{
			Packet: &pb.MeshPacket{
				From: from,
				To:   to,
				Id:   uint32(len(text)), // unique enough for tests
				PayloadVariant: &pb.MeshPacket_Decoded{
					Decoded: &pb.Data{
						Portnum: pb.PortNum_TEXT_MESSAGE_APP,
						Payload: []byte(text),
					},
				},
			},
		},
	})
}

// buildToRadioTextMessage creates a ToRadio protobuf payload containing a
// MeshPacket with TEXT_MESSAGE_APP.
func buildToRadioTextMessage(t *testing.T, from, to uint32, text string) []byte {
	t.Helper()
	return marshalToRadio(t, &pb.ToRadio{
		PayloadVariant: &pb.ToRadio_Packet{
			Packet: &pb.MeshPacket{
				From: from,
				To:   to,
				Id:   uint32(len(text)),
				PayloadVariant: &pb.MeshPacket_Decoded{
					Decoded: &pb.Data{
						Portnum: pb.PortNum_TEXT_MESSAGE_APP,
						Payload: []byte(text),
					},
				},
			},
		},
	})
}

// buildPositionPayload creates a FromRadio protobuf payload containing a
// decoded MeshPacket with POSITION_APP (non-text).
func buildPositionPayload(t *testing.T, from uint32) []byte {
	t.Helper()
	return marshalFromRadio(t, &pb.FromRadio{
		PayloadVariant: &pb.FromRadio_Packet{
			Packet: &pb.MeshPacket{
				From: from,
				To:   0xFFFFFFFF,
				PayloadVariant: &pb.MeshPacket_Decoded{
					Decoded: &pb.Data{
						Portnum: pb.PortNum_POSITION_APP,
						Payload: []byte{0x01, 0x02},
					},
				},
			},
		},
	})
}

// newTestProxy creates a Proxy with a mockNodeConn and the given maxChatCache.
func newTestProxy(t *testing.T, cache [][]byte, maxChatCache int) (*Proxy, *mockNodeConn) {
	t.Helper()
	mockNode := newMockNodeConn(cache)
	mockNode.myNodeNum = 0x12345678
	m := metrics.New(10, 300)
	p := New(Options{
		ListenAddr:        ":0",
		MaxClients:        10,
		ClientSendBuffer:  256,
		IOSNodeInfoDelay:  0, // no delay for tests
		MaxChatCache:      maxChatCache,
		ReplayChatHistory: true,
		NodeConn:          mockNode,
		Metrics:           m,
		Logger:            slog.Default(),
	})
	return p, mockNode
}

// ---------------------------------------------------------------------------
// isTextMessageFrame tests
// ---------------------------------------------------------------------------

func TestIsTextMessageFrame_TextMessage(t *testing.T) {
	payload := buildTextMessagePayload(t, 0x12345678, 0xFFFFFFFF, "hello")
	if !isTextMessageFrame(payload) {
		t.Error("isTextMessageFrame returned false for TEXT_MESSAGE_APP")
	}
}

func TestIsTextMessageFrame_PositionMessage(t *testing.T) {
	payload := buildPositionPayload(t, 0x12345678)
	if isTextMessageFrame(payload) {
		t.Error("isTextMessageFrame returned true for POSITION_APP")
	}
}

func TestIsTextMessageFrame_ConfigFrame(t *testing.T) {
	payload := marshalFromRadio(t, &pb.FromRadio{
		PayloadVariant: &pb.FromRadio_MyInfo{MyInfo: &pb.MyNodeInfo{MyNodeNum: 1}},
	})
	if isTextMessageFrame(payload) {
		t.Error("isTextMessageFrame returned true for MyInfo config frame")
	}
}

func TestIsTextMessageFrame_InvalidPayload(t *testing.T) {
	if isTextMessageFrame([]byte{0xFF, 0xFF}) {
		t.Error("isTextMessageFrame returned true for invalid payload")
	}
}

func TestIsTextMessageFrame_EncryptedPacket(t *testing.T) {
	// Encrypted packets have MeshPacket_Encrypted, not MeshPacket_Decoded.
	payload := marshalFromRadio(t, &pb.FromRadio{
		PayloadVariant: &pb.FromRadio_Packet{
			Packet: &pb.MeshPacket{
				From: 0x12345678,
				To:   0xFFFFFFFF,
				PayloadVariant: &pb.MeshPacket_Encrypted{
					Encrypted: []byte{0x01, 0x02, 0x03},
				},
			},
		},
	})
	if isTextMessageFrame(payload) {
		t.Error("isTextMessageFrame returned true for encrypted packet")
	}
}

// ---------------------------------------------------------------------------
// cacheTextMessage tests
// ---------------------------------------------------------------------------

func TestCacheTextMessage_IncomingMessages(t *testing.T) {
	p, _ := newTestProxy(t, nil, 100)

	msg1 := buildTextMessagePayload(t, 0xAAAAAAAA, 0xFFFFFFFF, "hello")
	msg2 := buildTextMessagePayload(t, 0xBBBBBBBB, 0xFFFFFFFF, "world")

	p.cacheTextMessage(msg1)
	p.cacheTextMessage(msg2)

	snapshot := p.chatCacheSnapshot()
	if len(snapshot) != 2 {
		t.Fatalf("cache size = %d, want 2", len(snapshot))
	}
}

func TestCacheTextMessage_RingBufferEviction(t *testing.T) {
	p, _ := newTestProxy(t, nil, 5) // max 5 messages

	// Add 7 messages — oldest 2 should be evicted.
	for i := 0; i < 7; i++ {
		msg := buildTextMessagePayload(t, 0x12345678, 0xFFFFFFFF, fmt.Sprintf("msg%d", i))
		p.cacheTextMessage(msg)
	}

	snapshot := p.chatCacheSnapshot()
	if len(snapshot) != 5 {
		t.Fatalf("cache size = %d, want 5 (max)", len(snapshot))
	}

	// Verify oldest messages were evicted: first cached message should be "msg2".
	fr := &pb.FromRadio{}
	if err := proto.Unmarshal(snapshot[0].message, fr); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	pkt := fr.GetPacket()
	decoded := pkt.GetDecoded()
	if string(decoded.GetPayload()) != "msg2" {
		t.Errorf("oldest message = %q, want %q", decoded.GetPayload(), "msg2")
	}
}

func TestCacheTextMessage_DisabledWhenZero(t *testing.T) {
	p, _ := newTestProxy(t, nil, 0) // disabled

	msg := buildTextMessagePayload(t, 0x12345678, 0xFFFFFFFF, "should not cache")
	p.cacheTextMessage(msg)

	snapshot := p.chatCacheSnapshot()
	if len(snapshot) != 0 {
		t.Fatalf("cache size = %d, want 0 (disabled)", len(snapshot))
	}
}

func TestCacheTextMessage_SnapshotIsCopy(t *testing.T) {
	p, _ := newTestProxy(t, nil, 100)

	msg := buildTextMessagePayload(t, 0x12345678, 0xFFFFFFFF, "test")
	p.cacheTextMessage(msg)

	snapshot1 := p.chatCacheSnapshot()
	// Mutate the snapshot.
	snapshot1[0].message[0] = 0xFF

	// Original cache should not be affected.
	snapshot2 := p.chatCacheSnapshot()
	if snapshot2[0].message[0] == 0xFF {
		t.Error("chatCacheSnapshot returned a reference, not a copy")
	}
}

// ---------------------------------------------------------------------------
// toRadioToFromRadio tests
// ---------------------------------------------------------------------------

func TestToRadioToFromRadio_TextMessage(t *testing.T) {
	p, _ := newTestProxy(t, nil, 100)

	toRadioPayload := buildToRadioTextMessage(t, 0, 0xFFFFFFFF, "hello from client")
	fromRadioPayload := p.toRadioToFromRadio(toRadioPayload)
	if fromRadioPayload == nil {
		t.Fatal("toRadioToFromRadio returned nil for text message")
	}

	// Should be a valid FromRadio with text message.
	if !isTextMessageFrame(fromRadioPayload) {
		t.Error("converted payload is not a text message frame")
	}

	// From should be filled with myNodeNum (0x12345678).
	fr := &pb.FromRadio{}
	if err := proto.Unmarshal(fromRadioPayload, fr); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	pkt := fr.GetPacket()
	if pkt.GetFrom() != 0x12345678 {
		t.Errorf("From = %08x, want 12345678", pkt.GetFrom())
	}
}

func TestToRadioToFromRadio_NonPacket(t *testing.T) {
	p, _ := newTestProxy(t, nil, 100)

	heartbeat := marshalToRadio(t, &pb.ToRadio{
		PayloadVariant: &pb.ToRadio_Heartbeat{Heartbeat: &pb.Heartbeat{}},
	})

	result := p.toRadioToFromRadio(heartbeat)
	if result != nil {
		t.Error("toRadioToFromRadio should return nil for non-packet")
	}
}

func TestToRadioToFromRadio_InvalidPayload(t *testing.T) {
	p, _ := newTestProxy(t, nil, 100)

	result := p.toRadioToFromRadio([]byte{0xFF, 0xFF})
	if result != nil {
		t.Error("toRadioToFromRadio should return nil for invalid payload")
	}
}

// ---------------------------------------------------------------------------
// Chat cache integration in broadcastLoop
// ---------------------------------------------------------------------------

func TestBroadcastLoop_CachesTextMessages(t *testing.T) {
	p, mockNode := newTestProxy(t, nil, 100)

	serverConn, clientConn := newTestConnPair(t)
	m := metrics.New(10, 300)
	client := NewClient(clientConn, slog.Default(), m, 256, 0, func([]byte) {}, func(*Client) {})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client.Start(ctx)
	p.registerClient(client)

	go p.broadcastLoop(ctx)

	// Send a text message from the node.
	textPayload := buildTextMessagePayload(t, 0xAAAAAAAA, 0xFFFFFFFF, "cached msg")
	mockNode.fromNode <- textPayload

	// Wait for the client to receive it (proves broadcastLoop processed it).
	readFrame(t, serverConn, 2*time.Second)

	// Verify it was cached.
	snapshot := p.chatCacheSnapshot()
	if len(snapshot) != 1 {
		t.Fatalf("chat cache size = %d, want 1", len(snapshot))
	}
}

func TestBroadcastLoop_DoesNotCacheNonText(t *testing.T) {
	p, mockNode := newTestProxy(t, nil, 100)

	serverConn, clientConn := newTestConnPair(t)
	m := metrics.New(10, 300)
	client := NewClient(clientConn, slog.Default(), m, 256, 0, func([]byte) {}, func(*Client) {})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client.Start(ctx)
	p.registerClient(client)

	go p.broadcastLoop(ctx)

	// Send a position message from the node.
	posPayload := buildPositionPayload(t, 0xAAAAAAAA)
	mockNode.fromNode <- posPayload

	// Wait for the client to receive it.
	readFrame(t, serverConn, 2*time.Second)

	// Verify it was NOT cached.
	snapshot := p.chatCacheSnapshot()
	if len(snapshot) != 0 {
		t.Fatalf("chat cache size = %d, want 0 (position should not be cached)", len(snapshot))
	}
}

// ---------------------------------------------------------------------------
// shouldReplayChat tests
// ---------------------------------------------------------------------------

func TestShouldReplayChat_RandomNonce(t *testing.T) {
	p, _ := newTestProxy(t, nil, 100)
	m := metrics.New(10, 300)
	_, clientConn := newTestConnPair(t)
	client := NewClient(clientConn, slog.Default(), m, 256, 0, func([]byte) {}, func(*Client) {})

	if !p.shouldReplayChat(12345, client) {
		t.Error("shouldReplayChat should return true for random nonce")
	}
}

func TestShouldReplayChat_ConfigOnlyNonce(t *testing.T) {
	p, _ := newTestProxy(t, nil, 100)
	m := metrics.New(10, 300)
	_, clientConn := newTestConnPair(t)
	client := NewClient(clientConn, slog.Default(), m, 256, 0, func([]byte) {}, func(*Client) {})

	if p.shouldReplayChat(nonceOnlyConfig, client) {
		t.Error("shouldReplayChat should return false for config-only nonce (first phase)")
	}
}

func TestShouldReplayChat_NodesOnlyNonceAfterConfig(t *testing.T) {
	p, _ := newTestProxy(t, nil, 100)
	m := metrics.New(10, 300)
	_, clientConn := newTestConnPair(t)
	client := NewClient(clientConn, slog.Default(), m, 256, 0, func([]byte) {}, func(*Client) {})

	// Simulate iOS: config phase was already seen.
	client.configPhase.Or(configPhaseConfig)

	if !p.shouldReplayChat(nonceOnlyNodes, client) {
		t.Error("shouldReplayChat should return true for nodes-only nonce after config phase")
	}
}

func TestShouldReplayChat_NodesOnlyNonceWithoutConfig(t *testing.T) {
	p, _ := newTestProxy(t, nil, 100)
	m := metrics.New(10, 300)
	_, clientConn := newTestConnPair(t)
	client := NewClient(clientConn, slog.Default(), m, 256, 0, func([]byte) {}, func(*Client) {})

	// Nodes-only without prior config phase — shouldn't happen normally,
	// but handle it defensively.
	if p.shouldReplayChat(nonceOnlyNodes, client) {
		t.Error("shouldReplayChat should return false for nodes-only nonce without config phase")
	}
}

func TestShouldReplayChat_Disabled(t *testing.T) {
	p, _ := newTestProxy(t, nil, 0) // disabled
	m := metrics.New(10, 300)
	_, clientConn := newTestConnPair(t)
	client := NewClient(clientConn, slog.Default(), m, 256, 0, func([]byte) {}, func(*Client) {})

	if p.shouldReplayChat(12345, client) {
		t.Error("shouldReplayChat should return false when maxChatCache=0")
	}
}

func TestShouldReplayChat_ReplayDisabledByConfig(t *testing.T) {
	p, _ := newTestProxy(t, nil, 100)
	p.replayChatEnabled = false // explicitly disabled via config
	m := metrics.New(10, 300)
	_, clientConn := newTestConnPair(t)
	client := NewClient(clientConn, slog.Default(), m, 256, 0, func([]byte) {}, func(*Client) {})

	if p.shouldReplayChat(12345, client) {
		t.Error("shouldReplayChat should return false when replay_chat_history is disabled")
	}
	// iOS nodes-only phase should also be suppressed.
	client.configPhase.Or(configPhaseConfig)
	if p.shouldReplayChat(nonceOnlyNodes, client) {
		t.Error("shouldReplayChat should return false for iOS nodes-only when replay_chat_history is disabled")
	}
}

// ---------------------------------------------------------------------------
// replayChatHistory tests
// ---------------------------------------------------------------------------

func TestReplayChatHistory_DeliversMessages(t *testing.T) {
	p, _ := newTestProxy(t, nil, 100)

	// Pre-populate the chat cache.
	for i := 0; i < 3; i++ {
		msg := buildTextMessagePayload(t, 0xAAAAAAAA, 0xFFFFFFFF, fmt.Sprintf("chat%d", i))
		p.cacheTextMessage(msg)
	}

	serverConn, clientConn := newTestConnPair(t)
	m := metrics.New(10, 300)
	client := NewClient(clientConn, slog.Default(), m, 256, 0, func([]byte) {}, func(*Client) {})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client.Start(ctx)

	p.replayChatHistory(client)

	// Read 3 chat messages.
	for i := 0; i < 3; i++ {
		got := readFrame(t, serverConn, 2*time.Second)
		fr := &pb.FromRadio{}
		if err := proto.Unmarshal(got, fr); err != nil {
			t.Fatalf("msg %d: unmarshal: %v", i, err)
		}
		pkt := fr.GetPacket()
		if pkt == nil {
			t.Fatalf("msg %d: no packet", i)
		}
		decoded := pkt.GetDecoded()
		if decoded == nil {
			t.Fatalf("msg %d: no decoded data", i)
		}
		want := fmt.Sprintf("chat%d", i)
		if string(decoded.GetPayload()) != want {
			t.Errorf("msg %d: text = %q, want %q", i, decoded.GetPayload(), want)
		}
	}
}

func TestReplayChatHistory_EmptyCache(t *testing.T) {
	p, _ := newTestProxy(t, nil, 100)

	serverConn, clientConn := newTestConnPair(t)
	m := metrics.New(10, 300)
	client := NewClient(clientConn, slog.Default(), m, 256, 0, func([]byte) {}, func(*Client) {})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client.Start(ctx)

	p.replayChatHistory(client)

	// Should receive no frames.
	_ = serverConn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	_, err := protocol.ReadFrame(serverConn)
	if err == nil {
		t.Error("expected no frames from empty chat cache")
	}
}

// ---------------------------------------------------------------------------
// End-to-end chat replay integration tests
// ---------------------------------------------------------------------------

func TestChatReplay_AfterRandomNonce(t *testing.T) {
	// Python CLI scenario: single want_config_id with random nonce.
	// Chat history should be replayed immediately after config replay.
	myNum := uint32(0x12345678)
	cache := buildTestCache(t, myNum, nil) // simple cache
	p, _ := newTestProxy(t, cache, 100)

	// Pre-populate chat cache.
	chatMsg := buildTextMessagePayload(t, 0xAAAAAAAA, 0xFFFFFFFF, "cached chat")
	p.cacheTextMessage(chatMsg)

	serverConn, clientConn := newTestConnPair(t)
	m := metrics.New(10, 300)
	client := NewClient(clientConn, slog.Default(), m, 256, 0, func([]byte) {}, func(*Client) {})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client.Start(ctx)

	// Random nonce → full config + chat replay.
	p.replayCachedConfig(client, 12345)

	// Read all frames: config frames + chat message.
	var frames [][]byte
	for {
		_ = serverConn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		payload, err := protocol.ReadFrame(serverConn)
		if err != nil {
			break
		}
		frames = append(frames, payload)
	}

	// Should have: config frames (len(cache)) + 1 chat message.
	expectedConfig := len(cache) // all frames in cache
	expectedTotal := expectedConfig + 1
	if len(frames) != expectedTotal {
		t.Fatalf("got %d frames, want %d (config %d + chat 1)", len(frames), expectedTotal, expectedConfig)
	}

	// Last frame should be the chat message.
	lastFrame := frames[len(frames)-1]
	if !isTextMessageFrame(lastFrame) {
		t.Error("last frame is not a text message")
	}
}

func TestChatReplay_AfterIOSNodesPhase(t *testing.T) {
	// iOS scenario: first want_config_id with 69420, then 69421.
	// Chat should be replayed only after the second (69421) phase.
	myNum := uint32(0x12345678)
	cache := buildTestCache(t, myNum, []uint32{0xBBBBBBBB})
	p, _ := newTestProxy(t, cache, 100)

	// Pre-populate chat cache.
	chatMsg := buildTextMessagePayload(t, 0xAAAAAAAA, 0xFFFFFFFF, "ios cached chat")
	p.cacheTextMessage(chatMsg)

	serverConn, clientConn := newTestConnPair(t)
	m := metrics.New(10, 300)
	client := NewClient(clientConn, slog.Default(), m, 256, 0, func([]byte) {}, func(*Client) {})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client.Start(ctx)

	// Phase 1: config-only (69420) — should NOT include chat.
	p.replayCachedConfig(client, nonceOnlyConfig)

	var phase1Frames [][]byte
	for {
		_ = serverConn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		payload, err := protocol.ReadFrame(serverConn)
		if err != nil {
			break
		}
		phase1Frames = append(phase1Frames, payload)
	}

	// Verify no chat messages in phase 1.
	for _, frame := range phase1Frames {
		if isTextMessageFrame(frame) {
			t.Error("chat message was replayed during config-only phase (69420)")
		}
	}

	// Phase 2: nodes-only (69421) — should include chat after nodes.
	p.replayCachedConfig(client, nonceOnlyNodes)

	var phase2Frames [][]byte
	for {
		_ = serverConn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		payload, err := protocol.ReadFrame(serverConn)
		if err != nil {
			break
		}
		phase2Frames = append(phase2Frames, payload)
	}

	// Last frame in phase 2 should be the chat message.
	foundChat := false
	for _, frame := range phase2Frames {
		if isTextMessageFrame(frame) {
			foundChat = true
		}
	}
	if !foundChat {
		t.Error("chat message was NOT replayed after nodes-only phase (69421)")
	}
}

func TestChatReplay_NotAfterIOSConfigPhase(t *testing.T) {
	// Verify chat is NOT replayed after the first iOS phase (69420).
	myNum := uint32(0x12345678)
	cache := buildTestCache(t, myNum, nil)
	p, _ := newTestProxy(t, cache, 100)

	chatMsg := buildTextMessagePayload(t, 0xAAAAAAAA, 0xFFFFFFFF, "should not replay yet")
	p.cacheTextMessage(chatMsg)

	serverConn, clientConn := newTestConnPair(t)
	m := metrics.New(10, 300)
	client := NewClient(clientConn, slog.Default(), m, 256, 0, func([]byte) {}, func(*Client) {})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client.Start(ctx)

	p.replayCachedConfig(client, nonceOnlyConfig)

	var frames [][]byte
	for {
		_ = serverConn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		payload, err := protocol.ReadFrame(serverConn)
		if err != nil {
			break
		}
		frames = append(frames, payload)
	}

	for _, frame := range frames {
		if isTextMessageFrame(frame) {
			t.Error("chat message was replayed during config-only phase — should wait for nodes phase")
		}
	}
}

func TestChatReplay_OutgoingMessagesCached(t *testing.T) {
	// Verify that outgoing (ToRadio) text messages are cached and replayed.
	myNum := uint32(0x12345678)
	cache := buildTestCache(t, myNum, nil)
	p, _ := newTestProxy(t, cache, 100)

	// Simulate an outgoing text message being cached.
	toRadioPayload := buildToRadioTextMessage(t, 0, 0xFFFFFFFF, "outgoing msg")
	fromRadioPayload := p.toRadioToFromRadio(toRadioPayload)
	if fromRadioPayload == nil {
		t.Fatal("toRadioToFromRadio returned nil")
	}
	p.cacheTextMessage(fromRadioPayload)

	// Now replay to a new client.
	serverConn, clientConn := newTestConnPair(t)
	m := metrics.New(10, 300)
	client := NewClient(clientConn, slog.Default(), m, 256, 0, func([]byte) {}, func(*Client) {})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client.Start(ctx)

	p.replayCachedConfig(client, 12345)

	var frames [][]byte
	for {
		_ = serverConn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		payload, err := protocol.ReadFrame(serverConn)
		if err != nil {
			break
		}
		frames = append(frames, payload)
	}

	// Last frame should be the outgoing text message (now as FromRadio).
	lastFrame := frames[len(frames)-1]
	if !isTextMessageFrame(lastFrame) {
		t.Error("outgoing message was not replayed as chat history")
	}

	// Verify From was filled with myNodeNum.
	fr := &pb.FromRadio{}
	if err := proto.Unmarshal(lastFrame, fr); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if fr.GetPacket().GetFrom() != myNum {
		t.Errorf("From = %08x, want %08x", fr.GetPacket().GetFrom(), myNum)
	}
}

// ---------------------------------------------------------------------------
// ACK caching test helpers
// ---------------------------------------------------------------------------

// buildTextMessagePayloadWithID creates a FromRadio text message with a specific packet ID.
func buildTextMessagePayloadWithID(t *testing.T, from, to, id uint32, text string) []byte {
	t.Helper()
	return marshalFromRadio(t, &pb.FromRadio{
		PayloadVariant: &pb.FromRadio_Packet{
			Packet: &pb.MeshPacket{
				From: from,
				To:   to,
				Id:   id,
				PayloadVariant: &pb.MeshPacket_Decoded{
					Decoded: &pb.Data{
						Portnum: pb.PortNum_TEXT_MESSAGE_APP,
						Payload: []byte(text),
					},
				},
			},
		},
	})
}

// buildRoutingACKPayload creates a FromRadio routing ACK for the given requestID.
// This simulates the ACK that the node sends back after delivering a message.
func buildRoutingACKPayload(t *testing.T, from, to, id, requestID uint32) []byte {
	t.Helper()
	routingData, err := proto.Marshal(&pb.Routing{
		Variant: &pb.Routing_ErrorReason{
			ErrorReason: pb.Routing_NONE,
		},
	})
	if err != nil {
		t.Fatalf("marshal routing: %v", err)
	}
	return marshalFromRadio(t, &pb.FromRadio{
		PayloadVariant: &pb.FromRadio_Packet{
			Packet: &pb.MeshPacket{
				From: from,
				To:   to,
				Id:   id,
				PayloadVariant: &pb.MeshPacket_Decoded{
					Decoded: &pb.Data{
						Portnum:   pb.PortNum_ROUTING_APP,
						RequestId: requestID,
						Payload:   routingData,
					},
				},
			},
		},
	})
}

// buildRoutingNACKPayload creates a FromRadio routing NACK (error) for the given requestID.
func buildRoutingNACKPayload(t *testing.T, from, to, id, requestID uint32, reason pb.Routing_Error) []byte {
	t.Helper()
	routingData, err := proto.Marshal(&pb.Routing{
		Variant: &pb.Routing_ErrorReason{
			ErrorReason: reason,
		},
	})
	if err != nil {
		t.Fatalf("marshal routing: %v", err)
	}
	return marshalFromRadio(t, &pb.FromRadio{
		PayloadVariant: &pb.FromRadio_Packet{
			Packet: &pb.MeshPacket{
				From: from,
				To:   to,
				Id:   id,
				PayloadVariant: &pb.MeshPacket_Decoded{
					Decoded: &pb.Data{
						Portnum:   pb.PortNum_ROUTING_APP,
						RequestId: requestID,
						Payload:   routingData,
					},
				},
			},
		},
	})
}

// ---------------------------------------------------------------------------
// isRoutingACKFrame tests
// ---------------------------------------------------------------------------

func TestIsRoutingACKFrame_ValidACK(t *testing.T) {
	ackPayload := buildRoutingACKPayload(t, 0xBBBBBBBB, 0x12345678, 200, 42)
	packetID, ok := isRoutingACKFrame(ackPayload)
	if !ok {
		t.Fatal("isRoutingACKFrame returned false for valid ACK")
	}
	if packetID != 42 {
		t.Errorf("packetID = %d, want 42", packetID)
	}
}

func TestIsRoutingACKFrame_NACK(t *testing.T) {
	nackPayload := buildRoutingNACKPayload(t, 0xBBBBBBBB, 0x12345678, 200, 42, pb.Routing_NO_RESPONSE)
	_, ok := isRoutingACKFrame(nackPayload)
	if ok {
		t.Error("isRoutingACKFrame returned true for NACK (NO_RESPONSE)")
	}
}

func TestIsRoutingACKFrame_NonRouting(t *testing.T) {
	textPayload := buildTextMessagePayload(t, 0x12345678, 0xFFFFFFFF, "hello")
	_, ok := isRoutingACKFrame(textPayload)
	if ok {
		t.Error("isRoutingACKFrame returned true for text message")
	}
}

func TestIsRoutingACKFrame_InvalidPayload(t *testing.T) {
	_, ok := isRoutingACKFrame([]byte{0xFF, 0xFF})
	if ok {
		t.Error("isRoutingACKFrame returned true for invalid payload")
	}
}

func TestIsRoutingACKFrame_ZeroRequestID(t *testing.T) {
	// Routing packet with RequestId=0 should not be treated as ACK.
	routingData, err := proto.Marshal(&pb.Routing{
		Variant: &pb.Routing_ErrorReason{ErrorReason: pb.Routing_NONE},
	})
	if err != nil {
		t.Fatal(err)
	}
	payload := marshalFromRadio(t, &pb.FromRadio{
		PayloadVariant: &pb.FromRadio_Packet{
			Packet: &pb.MeshPacket{
				From: 0xBBBBBBBB,
				To:   0x12345678,
				Id:   200,
				PayloadVariant: &pb.MeshPacket_Decoded{
					Decoded: &pb.Data{
						Portnum:   pb.PortNum_ROUTING_APP,
						RequestId: 0, // zero
						Payload:   routingData,
					},
				},
			},
		},
	})
	_, ok := isRoutingACKFrame(payload)
	if ok {
		t.Error("isRoutingACKFrame returned true for RequestId=0")
	}
}

func TestIsRoutingACKFrame_EncryptedPacket(t *testing.T) {
	payload := marshalFromRadio(t, &pb.FromRadio{
		PayloadVariant: &pb.FromRadio_Packet{
			Packet: &pb.MeshPacket{
				From:           0xBBBBBBBB,
				To:             0x12345678,
				PayloadVariant: &pb.MeshPacket_Encrypted{Encrypted: []byte{0x01}},
			},
		},
	})
	_, ok := isRoutingACKFrame(payload)
	if ok {
		t.Error("isRoutingACKFrame returned true for encrypted packet")
	}
}

func TestIsRoutingACKFrame_ConfigFrame(t *testing.T) {
	payload := marshalFromRadio(t, &pb.FromRadio{
		PayloadVariant: &pb.FromRadio_MyInfo{MyInfo: &pb.MyNodeInfo{MyNodeNum: 1}},
	})
	_, ok := isRoutingACKFrame(payload)
	if ok {
		t.Error("isRoutingACKFrame returned true for config frame")
	}
}

// ---------------------------------------------------------------------------
// extractPacketID tests
// ---------------------------------------------------------------------------

func TestExtractPacketID_TextMessage(t *testing.T) {
	payload := buildTextMessagePayloadWithID(t, 0x12345678, 0xFFFFFFFF, 42, "hello")
	id := extractPacketID(payload)
	if id != 42 {
		t.Errorf("extractPacketID = %d, want 42", id)
	}
}

func TestExtractPacketID_ConfigFrame(t *testing.T) {
	payload := marshalFromRadio(t, &pb.FromRadio{
		PayloadVariant: &pb.FromRadio_MyInfo{MyInfo: &pb.MyNodeInfo{MyNodeNum: 1}},
	})
	id := extractPacketID(payload)
	if id != 0 {
		t.Errorf("extractPacketID = %d, want 0 for config frame", id)
	}
}

func TestExtractPacketID_InvalidPayload(t *testing.T) {
	id := extractPacketID([]byte{0xFF})
	if id != 0 {
		t.Errorf("extractPacketID = %d, want 0 for invalid payload", id)
	}
}

// ---------------------------------------------------------------------------
// cacheACK tests
// ---------------------------------------------------------------------------

func TestCacheACK_MatchesMessage(t *testing.T) {
	p, _ := newTestProxy(t, nil, 100)

	// Cache a text message with packet ID 42.
	textPayload := buildTextMessagePayloadWithID(t, 0x12345678, 0xFFFFFFFF, 42, "hello")
	p.cacheTextMessage(textPayload)

	// Cache the ACK for packet ID 42.
	ackPayload := buildRoutingACKPayload(t, 0xBBBBBBBB, 0x12345678, 200, 42)
	p.cacheACK(42, ackPayload)

	snapshot := p.chatCacheSnapshot()
	if len(snapshot) != 1 {
		t.Fatalf("cache size = %d, want 1", len(snapshot))
	}
	if snapshot[0].ack == nil {
		t.Fatal("ACK was not associated with the text message")
	}
	if snapshot[0].packetID != 42 {
		t.Errorf("packetID = %d, want 42", snapshot[0].packetID)
	}
}

func TestCacheACK_NoMatch(t *testing.T) {
	p, _ := newTestProxy(t, nil, 100)

	// Cache a text message with packet ID 42.
	textPayload := buildTextMessagePayloadWithID(t, 0x12345678, 0xFFFFFFFF, 42, "hello")
	p.cacheTextMessage(textPayload)

	// Try to cache ACK for a different packet ID (99).
	ackPayload := buildRoutingACKPayload(t, 0xBBBBBBBB, 0x12345678, 200, 99)
	p.cacheACK(99, ackPayload)

	snapshot := p.chatCacheSnapshot()
	if snapshot[0].ack != nil {
		t.Error("ACK was associated with wrong message — should have been ignored")
	}
}

func TestCacheACK_DuplicateIgnored(t *testing.T) {
	p, _ := newTestProxy(t, nil, 100)

	textPayload := buildTextMessagePayloadWithID(t, 0x12345678, 0xFFFFFFFF, 42, "hello")
	p.cacheTextMessage(textPayload)

	ack1 := buildRoutingACKPayload(t, 0xBBBBBBBB, 0x12345678, 200, 42)
	ack2 := buildRoutingACKPayload(t, 0xCCCCCCCC, 0x12345678, 300, 42)
	p.cacheACK(42, ack1)
	p.cacheACK(42, ack2) // should be ignored — already has ACK

	snapshot := p.chatCacheSnapshot()
	// The first ACK should be kept, not replaced.
	fr := &pb.FromRadio{}
	if err := proto.Unmarshal(snapshot[0].ack, fr); err != nil {
		t.Fatalf("unmarshal ack: %v", err)
	}
	pkt := fr.GetPacket()
	if pkt.GetFrom() != 0xBBBBBBBB {
		t.Errorf("ACK From = %08x, want BBBBBBBB (first ACK)", pkt.GetFrom())
	}
}

func TestCacheACK_DisabledWhenZero(t *testing.T) {
	p, _ := newTestProxy(t, nil, 0) // disabled

	// Should not panic when cache is disabled.
	p.cacheACK(42, []byte{0x01, 0x02})
}

func TestCacheACK_ZeroPacketID(t *testing.T) {
	p, _ := newTestProxy(t, nil, 100)

	textPayload := buildTextMessagePayloadWithID(t, 0x12345678, 0xFFFFFFFF, 42, "hello")
	p.cacheTextMessage(textPayload)

	// cacheACK with packetID=0 should do nothing.
	p.cacheACK(0, []byte{0x01})

	snapshot := p.chatCacheSnapshot()
	if snapshot[0].ack != nil {
		t.Error("cacheACK with packetID=0 should not match any entry")
	}
}

func TestCacheACK_SnapshotCopiesACK(t *testing.T) {
	p, _ := newTestProxy(t, nil, 100)

	textPayload := buildTextMessagePayloadWithID(t, 0x12345678, 0xFFFFFFFF, 42, "hello")
	p.cacheTextMessage(textPayload)

	ackPayload := buildRoutingACKPayload(t, 0xBBBBBBBB, 0x12345678, 200, 42)
	p.cacheACK(42, ackPayload)

	snapshot1 := p.chatCacheSnapshot()
	// Mutate the snapshot ACK.
	snapshot1[0].ack[0] = 0xFF

	// Original cache should not be affected.
	snapshot2 := p.chatCacheSnapshot()
	if snapshot2[0].ack[0] == 0xFF {
		t.Error("chatCacheSnapshot returned ACK reference, not a copy")
	}
}

// ---------------------------------------------------------------------------
// broadcastLoop ACK caching integration tests
// ---------------------------------------------------------------------------

func TestBroadcastLoop_CachesRoutingACK(t *testing.T) {
	p, mockNode := newTestProxy(t, nil, 100)

	serverConn, clientConn := newTestConnPair(t)
	m := metrics.New(10, 300)
	client := NewClient(clientConn, slog.Default(), m, 256, 0, func([]byte) {}, func(*Client) {})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client.Start(ctx)
	p.registerClient(client)

	go p.broadcastLoop(ctx)

	// Send a text message from the node with ID=42.
	textPayload := buildTextMessagePayloadWithID(t, 0xAAAAAAAA, 0xFFFFFFFF, 42, "hello mesh")
	mockNode.fromNode <- textPayload

	// Wait for the text message to be received by client (proves broadcastLoop processed it).
	readFrame(t, serverConn, 2*time.Second)

	// Now send the routing ACK for packet ID 42.
	ackPayload := buildRoutingACKPayload(t, 0xBBBBBBBB, 0xAAAAAAAA, 200, 42)
	mockNode.fromNode <- ackPayload

	// Wait for the ACK to be received by client.
	readFrame(t, serverConn, 2*time.Second)

	// Verify the ACK was associated with the cached text message.
	snapshot := p.chatCacheSnapshot()
	if len(snapshot) != 1 {
		t.Fatalf("cache size = %d, want 1", len(snapshot))
	}
	if snapshot[0].ack == nil {
		t.Fatal("routing ACK was not cached alongside the text message")
	}
	if snapshot[0].packetID != 42 {
		t.Errorf("packetID = %d, want 42", snapshot[0].packetID)
	}
}

func TestBroadcastLoop_DoesNotCacheNACK(t *testing.T) {
	p, mockNode := newTestProxy(t, nil, 100)

	serverConn, clientConn := newTestConnPair(t)
	m := metrics.New(10, 300)
	client := NewClient(clientConn, slog.Default(), m, 256, 0, func([]byte) {}, func(*Client) {})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client.Start(ctx)
	p.registerClient(client)

	go p.broadcastLoop(ctx)

	// Send a text message from the node.
	textPayload := buildTextMessagePayloadWithID(t, 0xAAAAAAAA, 0xFFFFFFFF, 42, "nack test")
	mockNode.fromNode <- textPayload
	readFrame(t, serverConn, 2*time.Second)

	// Send a NACK (not ACK) for packet ID 42.
	nackPayload := buildRoutingNACKPayload(t, 0xBBBBBBBB, 0xAAAAAAAA, 200, 42, pb.Routing_NO_RESPONSE)
	mockNode.fromNode <- nackPayload
	readFrame(t, serverConn, 2*time.Second)

	// Verify the NACK was NOT cached.
	snapshot := p.chatCacheSnapshot()
	if len(snapshot) != 1 {
		t.Fatalf("cache size = %d, want 1", len(snapshot))
	}
	if snapshot[0].ack != nil {
		t.Error("NACK was cached — only ACKs (ErrorReason=NONE) should be cached")
	}
}

// ---------------------------------------------------------------------------
// replayChatHistory with ACK tests
// ---------------------------------------------------------------------------

func TestReplayChatHistory_WithACK(t *testing.T) {
	p, _ := newTestProxy(t, nil, 100)

	// Cache a text message and its ACK.
	textPayload := buildTextMessagePayloadWithID(t, 0xAAAAAAAA, 0xFFFFFFFF, 42, "acked msg")
	p.cacheTextMessage(textPayload)

	ackPayload := buildRoutingACKPayload(t, 0xBBBBBBBB, 0xAAAAAAAA, 200, 42)
	p.cacheACK(42, ackPayload)

	serverConn, clientConn := newTestConnPair(t)
	m := metrics.New(10, 300)
	client := NewClient(clientConn, slog.Default(), m, 256, 0, func([]byte) {}, func(*Client) {})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client.Start(ctx)

	p.replayChatHistory(client)

	// Should receive 2 frames: text message + ACK.
	frame1 := readFrame(t, serverConn, 2*time.Second)
	if !isTextMessageFrame(frame1) {
		t.Error("first replayed frame is not a text message")
	}

	frame2 := readFrame(t, serverConn, 2*time.Second)
	packetID, ok := isRoutingACKFrame(frame2)
	if !ok {
		t.Fatal("second replayed frame is not a routing ACK")
	}
	if packetID != 42 {
		t.Errorf("ACK requestID = %d, want 42", packetID)
	}
}

func TestReplayChatHistory_WithoutACK(t *testing.T) {
	p, _ := newTestProxy(t, nil, 100)

	// Cache a text message without ACK.
	textPayload := buildTextMessagePayloadWithID(t, 0xAAAAAAAA, 0xFFFFFFFF, 42, "no ack yet")
	p.cacheTextMessage(textPayload)

	serverConn, clientConn := newTestConnPair(t)
	m := metrics.New(10, 300)
	client := NewClient(clientConn, slog.Default(), m, 256, 0, func([]byte) {}, func(*Client) {})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client.Start(ctx)

	p.replayChatHistory(client)

	// Should receive only 1 frame: the text message.
	frame1 := readFrame(t, serverConn, 2*time.Second)
	if !isTextMessageFrame(frame1) {
		t.Error("replayed frame is not a text message")
	}

	// No more frames.
	_ = serverConn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	_, err := protocol.ReadFrame(serverConn)
	if err == nil {
		t.Error("received unexpected second frame — ACK should be nil")
	}
}

func TestReplayChatHistory_MixedACKAndNoACK(t *testing.T) {
	p, _ := newTestProxy(t, nil, 100)

	// Message 1: with ACK.
	text1 := buildTextMessagePayloadWithID(t, 0xAAAAAAAA, 0xFFFFFFFF, 42, "msg with ack")
	p.cacheTextMessage(text1)
	ack1 := buildRoutingACKPayload(t, 0xBBBBBBBB, 0xAAAAAAAA, 200, 42)
	p.cacheACK(42, ack1)

	// Message 2: without ACK.
	text2 := buildTextMessagePayloadWithID(t, 0xCCCCCCCC, 0xFFFFFFFF, 43, "msg without ack")
	p.cacheTextMessage(text2)

	// Message 3: with ACK.
	text3 := buildTextMessagePayloadWithID(t, 0xDDDDDDDD, 0xFFFFFFFF, 44, "another acked")
	p.cacheTextMessage(text3)
	ack3 := buildRoutingACKPayload(t, 0xBBBBBBBB, 0xDDDDDDDD, 300, 44)
	p.cacheACK(44, ack3)

	serverConn, clientConn := newTestConnPair(t)
	m := metrics.New(10, 300)
	client := NewClient(clientConn, slog.Default(), m, 256, 0, func([]byte) {}, func(*Client) {})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client.Start(ctx)

	p.replayChatHistory(client)

	// Expected frames: text1, ack1, text2, text3, ack3 = 5 frames total.
	var frames [][]byte
	for {
		_ = serverConn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		payload, err := protocol.ReadFrame(serverConn)
		if err != nil {
			break
		}
		frames = append(frames, payload)
	}

	if len(frames) != 5 {
		t.Fatalf("got %d frames, want 5 (3 messages + 2 ACKs)", len(frames))
	}

	// Frame 0: text message 1.
	if !isTextMessageFrame(frames[0]) {
		t.Error("frame 0: expected text message")
	}
	// Frame 1: ACK for message 1.
	if _, ok := isRoutingACKFrame(frames[1]); !ok {
		t.Error("frame 1: expected routing ACK")
	}
	// Frame 2: text message 2 (no ACK follows).
	if !isTextMessageFrame(frames[2]) {
		t.Error("frame 2: expected text message")
	}
	// Frame 3: text message 3.
	if !isTextMessageFrame(frames[3]) {
		t.Error("frame 3: expected text message")
	}
	// Frame 4: ACK for message 3.
	if _, ok := isRoutingACKFrame(frames[4]); !ok {
		t.Error("frame 4: expected routing ACK")
	}
}

// ---------------------------------------------------------------------------
// End-to-end chat replay with ACK integration tests
// ---------------------------------------------------------------------------

func TestChatReplay_AfterRandomNonce_WithACK(t *testing.T) {
	// Python CLI scenario: config replay + chat replay with ACK.
	myNum := uint32(0x12345678)
	cache := buildTestCache(t, myNum, nil)
	p, _ := newTestProxy(t, cache, 100)

	// Cache a text message + ACK.
	chatMsg := buildTextMessagePayloadWithID(t, 0xAAAAAAAA, 0xFFFFFFFF, 42, "hello with ack")
	p.cacheTextMessage(chatMsg)
	ackMsg := buildRoutingACKPayload(t, 0xBBBBBBBB, 0xAAAAAAAA, 200, 42)
	p.cacheACK(42, ackMsg)

	serverConn, clientConn := newTestConnPair(t)
	m := metrics.New(10, 300)
	client := NewClient(clientConn, slog.Default(), m, 256, 0, func([]byte) {}, func(*Client) {})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client.Start(ctx)

	// Random nonce → full config + chat replay.
	p.replayCachedConfig(client, 12345)

	var frames [][]byte
	for {
		_ = serverConn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		payload, err := protocol.ReadFrame(serverConn)
		if err != nil {
			break
		}
		frames = append(frames, payload)
	}

	// Should have: config frames + 1 chat message + 1 ACK.
	expectedConfig := len(cache)
	expectedTotal := expectedConfig + 2 // message + ACK
	if len(frames) != expectedTotal {
		t.Fatalf("got %d frames, want %d (config %d + chat 1 + ack 1)", len(frames), expectedTotal, expectedConfig)
	}

	// Second-to-last frame should be the chat message.
	chatFrame := frames[len(frames)-2]
	if !isTextMessageFrame(chatFrame) {
		t.Error("second-to-last frame is not a text message")
	}

	// Last frame should be the ACK.
	ackFrame := frames[len(frames)-1]
	packetID, ok := isRoutingACKFrame(ackFrame)
	if !ok {
		t.Error("last frame is not a routing ACK")
	}
	if packetID != 42 {
		t.Errorf("ACK requestID = %d, want 42", packetID)
	}
}

func TestChatReplay_AfterIOSNodesPhase_WithACK(t *testing.T) {
	// iOS scenario: two-phase config, chat+ACK replayed after second phase.
	myNum := uint32(0x12345678)
	cache := buildTestCache(t, myNum, []uint32{0xBBBBBBBB})
	p, _ := newTestProxy(t, cache, 100)

	// Cache text message + ACK.
	chatMsg := buildTextMessagePayloadWithID(t, 0xAAAAAAAA, 0xFFFFFFFF, 42, "ios acked chat")
	p.cacheTextMessage(chatMsg)
	ackMsg := buildRoutingACKPayload(t, 0xBBBBBBBB, 0xAAAAAAAA, 200, 42)
	p.cacheACK(42, ackMsg)

	serverConn, clientConn := newTestConnPair(t)
	m := metrics.New(10, 300)
	client := NewClient(clientConn, slog.Default(), m, 256, 0, func([]byte) {}, func(*Client) {})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client.Start(ctx)

	// Phase 1: config-only (69420) — no chat.
	p.replayCachedConfig(client, nonceOnlyConfig)

	var phase1Frames [][]byte
	for {
		_ = serverConn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		payload, err := protocol.ReadFrame(serverConn)
		if err != nil {
			break
		}
		phase1Frames = append(phase1Frames, payload)
	}

	for _, frame := range phase1Frames {
		if isTextMessageFrame(frame) {
			t.Error("chat message replayed during config-only phase")
		}
		if _, ok := isRoutingACKFrame(frame); ok {
			t.Error("routing ACK replayed during config-only phase")
		}
	}

	// Phase 2: nodes-only (69421) — should include chat + ACK.
	p.replayCachedConfig(client, nonceOnlyNodes)

	var phase2Frames [][]byte
	for {
		_ = serverConn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		payload, err := protocol.ReadFrame(serverConn)
		if err != nil {
			break
		}
		phase2Frames = append(phase2Frames, payload)
	}

	foundChat := false
	foundACK := false
	for _, frame := range phase2Frames {
		if isTextMessageFrame(frame) {
			foundChat = true
		}
		if _, ok := isRoutingACKFrame(frame); ok {
			foundACK = true
		}
	}
	if !foundChat {
		t.Error("chat message was NOT replayed after nodes-only phase")
	}
	if !foundACK {
		t.Error("routing ACK was NOT replayed after nodes-only phase")
	}
}

// ---------------------------------------------------------------------------
// RxTime population for outgoing messages
// ---------------------------------------------------------------------------

func TestToRadioToFromRadio_SetsRxTime(t *testing.T) {
	p, _ := newTestProxy(t, nil, 100)

	before := uint32(time.Now().Unix())
	toRadioPayload := buildToRadioTextMessage(t, 0, 0xFFFFFFFF, "hello")
	result := p.toRadioToFromRadio(toRadioPayload)
	after := uint32(time.Now().Unix())

	if result == nil {
		t.Fatal("toRadioToFromRadio returned nil")
	}

	fr := &pb.FromRadio{}
	if err := proto.Unmarshal(result, fr); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	rxTime := fr.GetPacket().GetRxTime()
	if rxTime < before || rxTime > after {
		t.Errorf("RxTime = %d, want in [%d, %d]", rxTime, before, after)
	}
}

func TestToRadioToFromRadio_PreservesExistingRxTime(t *testing.T) {
	p, _ := newTestProxy(t, nil, 100)

	existingRxTime := uint32(1700000000) // some past timestamp
	toRadio := marshalToRadio(t, &pb.ToRadio{
		PayloadVariant: &pb.ToRadio_Packet{
			Packet: &pb.MeshPacket{
				From:   0x12345678,
				To:     0xFFFFFFFF,
				Id:     99,
				RxTime: existingRxTime,
				PayloadVariant: &pb.MeshPacket_Decoded{
					Decoded: &pb.Data{
						Portnum: pb.PortNum_TEXT_MESSAGE_APP,
						Payload: []byte("with rxtime"),
					},
				},
			},
		},
	})

	result := p.toRadioToFromRadio(toRadio)
	if result == nil {
		t.Fatal("toRadioToFromRadio returned nil")
	}

	fr := &pb.FromRadio{}
	if err := proto.Unmarshal(result, fr); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if fr.GetPacket().GetRxTime() != existingRxTime {
		t.Errorf("RxTime = %d, want %d (should not be overwritten)", fr.GetPacket().GetRxTime(), existingRxTime)
	}
}

func TestEchoToOtherClients_SetsRxTime(t *testing.T) {
	mockNode := newMockNodeConn(nil)
	mockNode.myNodeNum = 0x12345678
	m := metrics.New(10, 300)
	p := New(Options{
		ListenAddr:       ":0",
		MaxClients:       10,
		ClientSendBuffer: 256,
		IOSNodeInfoDelay: 50 * time.Millisecond,
		NodeConn:         mockNode,
		Metrics:          m,
		Logger:           slog.Default(),
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_, clientA := newTestConnPair(t)
	clientObjA := NewClient(clientA, slog.Default(), m, 256, 0, func([]byte) {}, func(*Client) {})
	clientObjA.Start(ctx)
	p.registerClient(clientObjA)

	serverB, clientB := newTestConnPair(t)
	clientObjB := NewClient(clientB, slog.Default(), m, 256, 0, func([]byte) {}, func(*Client) {})
	clientObjB.Start(ctx)
	p.registerClient(clientObjB)

	before := uint32(time.Now().Unix())
	toRadioPayload := marshalToRadio(t, &pb.ToRadio{
		PayloadVariant: &pb.ToRadio_Packet{
			Packet: &pb.MeshPacket{
				From: 0x12345678,
				To:   0xFFFFFFFF,
				Id:   77,
				PayloadVariant: &pb.MeshPacket_Decoded{
					Decoded: &pb.Data{
						Portnum: pb.PortNum_TEXT_MESSAGE_APP,
						Payload: []byte("echo test"),
					},
				},
			},
		},
	})
	p.echoToOtherClients(toRadioPayload, clientObjA)
	after := uint32(time.Now().Unix())

	got := readFrame(t, serverB, 2*time.Second)
	fr := &pb.FromRadio{}
	if err := proto.Unmarshal(got, fr); err != nil {
		t.Fatalf("unmarshal echo: %v", err)
	}
	rxTime := fr.GetPacket().GetRxTime()
	if rxTime < before || rxTime > after {
		t.Errorf("RxTime = %d, want in [%d, %d]", rxTime, before, after)
	}
}

func TestEchoToOtherClients_PreservesExistingRxTime(t *testing.T) {
	mockNode := newMockNodeConn(nil)
	mockNode.myNodeNum = 0x12345678
	m := metrics.New(10, 300)
	p := New(Options{
		ListenAddr:       ":0",
		MaxClients:       10,
		ClientSendBuffer: 256,
		IOSNodeInfoDelay: 50 * time.Millisecond,
		NodeConn:         mockNode,
		Metrics:          m,
		Logger:           slog.Default(),
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_, clientA := newTestConnPair(t)
	clientObjA := NewClient(clientA, slog.Default(), m, 256, 0, func([]byte) {}, func(*Client) {})
	clientObjA.Start(ctx)
	p.registerClient(clientObjA)

	serverB, clientB := newTestConnPair(t)
	clientObjB := NewClient(clientB, slog.Default(), m, 256, 0, func([]byte) {}, func(*Client) {})
	clientObjB.Start(ctx)
	p.registerClient(clientObjB)

	existingRxTime := uint32(1700000000)
	toRadioPayload := marshalToRadio(t, &pb.ToRadio{
		PayloadVariant: &pb.ToRadio_Packet{
			Packet: &pb.MeshPacket{
				From:   0x12345678,
				To:     0xFFFFFFFF,
				Id:     88,
				RxTime: existingRxTime,
				PayloadVariant: &pb.MeshPacket_Decoded{
					Decoded: &pb.Data{
						Portnum: pb.PortNum_TEXT_MESSAGE_APP,
						Payload: []byte("keep my rxtime"),
					},
				},
			},
		},
	})
	p.echoToOtherClients(toRadioPayload, clientObjA)

	got := readFrame(t, serverB, 2*time.Second)
	fr := &pb.FromRadio{}
	if err := proto.Unmarshal(got, fr); err != nil {
		t.Fatalf("unmarshal echo: %v", err)
	}
	if fr.GetPacket().GetRxTime() != existingRxTime {
		t.Errorf("RxTime = %d, want %d (should not be overwritten)", fr.GetPacket().GetRxTime(), existingRxTime)
	}
}
