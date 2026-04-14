package proxy

import (
	"fmt"
	"time"

	pb "buf.build/gen/go/meshtastic/protobufs/protocolbuffers/go/meshtastic"
	"google.golang.org/protobuf/proto"

	"github.com/jfett/meshtastic-proxy/internal/node"
)

// Special nonces used by the iOS Meshtastic app to request partial config.
// See firmware PhoneAPI.h: SPECIAL_NONCE_ONLY_CONFIG / SPECIAL_NONCE_ONLY_NODES.
const (
	nonceOnlyConfig = 69420 // config + channels + modules, skip NodeInfo DB
	nonceOnlyNodes  = 69421 // NodeInfo DB only, skip config
)

// configPhase bitmask values for tracking iOS two-phase config completion.
const (
	configPhaseConfig uint32 = 1 << iota // bit 0: seen nonce 69420
	configPhaseNodes                     // bit 1: seen nonce 69421
)

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

// replayCachedConfig sends the cached node configuration to a client that
// has requested it via want_config_id. The ConfigCompleteId nonce in the
// cache is replaced with the client's nonce so the client accepts the
// config sequence. This is called from the client's readLoop goroutine
// (via onMessage), so the write loop is already running and frames are
// delivered through the send channel.
func (p *Proxy) replayCachedConfig(c *Client, clientNonce uint32) {
	// Serialize concurrent replays for the same client to prevent
	// interleaved config/nodes frames from two rapid want_config_id requests.
	c.replayMu.Lock()
	defer c.replayMu.Unlock()

	frames := p.nodeConn.ConfigCache()
	if len(frames) == 0 {
		p.logger.Debug("no cached config for replay", "client", c.Addr())
		return
	}

	// Track iOS config phases for chat replay timing.
	switch clientNonce {
	case nonceOnlyConfig:
		c.configPhase.Or(configPhaseConfig)
	case nonceOnlyNodes:
		c.configPhase.Or(configPhaseNodes)
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
				raw, err := proto.Marshal(patched)
				if err != nil {
					p.logger.Error("failed to marshal patched ConfigCompleteId",
						"client", c.Addr(), "error", err)
					continue
				}
				outFrame = raw
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

	// Replay chat history after config delivery, if appropriate.
	// For iOS: only after the nodes-only phase (69421), which is the second
	// and final phase. Not after config-only (69420), because the client
	// hasn't finished loading yet.
	// For Python CLI / random nonce: replay immediately after the single
	// config replay.
	if p.shouldReplayChat(clientNonce, c) {
		p.replayChatHistory(c)
	}
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

// shouldReplayChat determines whether chat history should be replayed after
// a config replay with the given nonce. The logic handles iOS two-phase config:
//   - Random nonce (Python CLI, etc.): replay immediately — single config phase.
//   - Nonce 69420 (iOS config-only, first phase): do NOT replay — wait for second phase.
//   - Nonce 69421 (iOS nodes-only, second phase): replay — both phases complete.
func (p *Proxy) shouldReplayChat(nonce uint32, c *Client) bool {
	if !p.replayChatEnabled || p.maxChatCache <= 0 {
		return false
	}

	switch nonce {
	case nonceOnlyConfig:
		// iOS first phase — do not replay yet.
		return false
	case nonceOnlyNodes:
		// iOS second phase — replay if config phase was also seen.
		return c.configPhase.Load()&configPhaseConfig != 0
	default:
		// Random nonce (Python CLI, etc.) — single phase, replay now.
		return true
	}
}

// replayChatHistory sends all cached text messages (and their ACKs) to the
// client. Called after config replay is complete for the final phase.
// Each message is followed by its routing ACK if one has been received,
// so that iOS shows the correct "delivered" status instead of
// "waiting to be acknowledged".
func (p *Proxy) replayChatHistory(c *Client) {
	entries := p.chatCacheSnapshot()
	if len(entries) == 0 {
		return
	}

	p.logger.Debug("replaying chat history",
		"client", c.Addr(),
		"messages", len(entries),
	)

	sent := 0
	for _, entry := range entries {
		if !c.Send(entry.message) {
			p.logger.Debug("chat replay interrupted, client disconnected",
				"client", c.Addr(),
				"sent", sent,
				"total", len(entries),
			)
			return
		}
		sent++

		// Send the routing ACK immediately after the message so that
		// the client sees the message as delivered.
		if entry.ack != nil {
			if !c.Send(entry.ack) {
				p.logger.Debug("chat replay interrupted during ACK, client disconnected",
					"client", c.Addr(),
					"sent", sent,
					"total", len(entries),
				)
				return
			}
		}
	}

	p.logger.Debug("replayed chat history to client",
		"client", c.Addr(),
		"messages", sent,
	)
}
