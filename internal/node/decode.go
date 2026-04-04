package node

import (
	"encoding/hex"
	"fmt"
	"strings"

	pb "buf.build/gen/go/meshtastic/protobufs/protocolbuffers/go/meshtastic"
	"github.com/jfett/meshtastic-proxy/internal/metrics"
	"google.golang.org/protobuf/proto"
)

// ---------------------------------------------------------------------------
// Protobuf decoding helpers
// ---------------------------------------------------------------------------

// decodeFromRadio fully decodes a FromRadio protobuf payload into a MessageRecord.
func decodeFromRadio(payload []byte) metrics.MessageRecord {
	rec := metrics.MessageRecord{
		Direction: "from_node",
		Size:      len(payload),
	}

	msg := &pb.FromRadio{}
	if err := proto.Unmarshal(payload, msg); err != nil {
		rec.Type = "unknown"
		rec.Payload = "unmarshal error: " + err.Error()
		return rec
	}

	switch v := msg.GetPayloadVariant().(type) {
	case *pb.FromRadio_Packet:
		rec.Type = "packet"
		decodeMeshPacket(v.Packet, &rec)

	case *pb.FromRadio_MyInfo:
		rec.Type = "my_info"
		if v.MyInfo != nil {
			rec.Payload = fmt.Sprintf("my_node_num=!%08x", v.MyInfo.GetMyNodeNum())
		}

	case *pb.FromRadio_NodeInfo:
		rec.Type = "node_info"
		if v.NodeInfo != nil {
			u := v.NodeInfo.GetUser()
			if u != nil {
				rec.Payload = fmt.Sprintf("!%08x %s (%s)",
					v.NodeInfo.GetNum(), u.GetLongName(), u.GetShortName())
			} else {
				rec.Payload = fmt.Sprintf("!%08x", v.NodeInfo.GetNum())
			}
		}

	case *pb.FromRadio_Config:
		rec.Type = "config"
		rec.Payload = decodeConfig(v.Config)

	case *pb.FromRadio_LogRecord:
		rec.Type = "log_record"
		if v.LogRecord != nil {
			rec.Payload = v.LogRecord.GetMessage()
		}

	case *pb.FromRadio_ConfigCompleteId:
		rec.Type = "config_complete_id"
		rec.Payload = fmt.Sprintf("nonce=%d", v.ConfigCompleteId)

	case *pb.FromRadio_Rebooted:
		rec.Type = "rebooted"
		rec.Payload = fmt.Sprintf("rebooted=%v", v.Rebooted)

	case *pb.FromRadio_ModuleConfig:
		rec.Type = "module_config"
		rec.Payload = decodeModuleConfig(v.ModuleConfig)

	case *pb.FromRadio_Channel:
		rec.Type = "channel"
		if v.Channel != nil {
			s := v.Channel.GetSettings()
			name := ""
			if s != nil {
				name = s.GetName()
			}
			rec.Payload = fmt.Sprintf("index=%d role=%s name=%q",
				v.Channel.GetIndex(), v.Channel.GetRole().String(), name)
		}

	case *pb.FromRadio_QueueStatus:
		rec.Type = "queue_status"
		if v.QueueStatus != nil {
			rec.Payload = fmt.Sprintf("free=%d", v.QueueStatus.GetFree())
		}

	case *pb.FromRadio_Metadata:
		rec.Type = "metadata"
		if v.Metadata != nil {
			rec.Payload = fmt.Sprintf("fw=%s hw=%s",
				v.Metadata.GetFirmwareVersion(), v.Metadata.GetHwModel().String())
		}

	default:
		rec.Type = "other"
	}

	return rec
}

// decodeToRadio fully decodes a ToRadio protobuf payload into a MessageRecord.
func decodeToRadio(payload []byte) metrics.MessageRecord {
	rec := metrics.MessageRecord{
		Direction: "to_node",
		Size:      len(payload),
	}

	msg := &pb.ToRadio{}
	if err := proto.Unmarshal(payload, msg); err != nil {
		rec.Type = "unknown"
		rec.Payload = "unmarshal error: " + err.Error()
		return rec
	}

	switch v := msg.GetPayloadVariant().(type) {
	case *pb.ToRadio_Packet:
		rec.Type = "packet"
		decodeMeshPacket(v.Packet, &rec)

	case *pb.ToRadio_WantConfigId:
		rec.Type = "want_config_id"
		rec.Payload = fmt.Sprintf("nonce=%d", v.WantConfigId)

	case *pb.ToRadio_Disconnect:
		rec.Type = "disconnect"
		rec.Payload = fmt.Sprintf("disconnect=%v", v.Disconnect)

	case *pb.ToRadio_Heartbeat:
		rec.Type = "heartbeat"

	default:
		rec.Type = "other"
	}

	return rec
}

// decodeMeshPacket extracts from/to/channel/portnum/radio metadata and decodes the payload.
func decodeMeshPacket(pkt *pb.MeshPacket, rec *metrics.MessageRecord) {
	if pkt == nil {
		return
	}

	rec.From = pkt.GetFrom()
	rec.To = pkt.GetTo()
	rec.Channel = pkt.GetChannel()
	rec.HopLimit = pkt.GetHopLimit()
	rec.HopStart = pkt.GetHopStart()
	rec.RxRssi = pkt.GetRxRssi()
	rec.RxSnr = pkt.GetRxSnr()
	rec.ViaMqtt = pkt.GetViaMqtt()
	rec.RelayNode = pkt.GetRelayNode()

	switch v := pkt.GetPayloadVariant().(type) {
	case *pb.MeshPacket_Decoded:
		if v.Decoded == nil {
			return
		}
		rec.PortNum = v.Decoded.GetPortnum().String()
		rec.Payload = decodeDataPayload(v.Decoded.GetPortnum(), v.Decoded.GetPayload())

	case *pb.MeshPacket_Encrypted:
		rec.PortNum = "ENCRYPTED"
		rec.Payload = fmt.Sprintf("encrypted (%d bytes)", len(v.Encrypted))
	}
}

// decodeDataPayload decodes the inner payload based on the portnum.
func decodeDataPayload(portnum pb.PortNum, data []byte) string {
	switch portnum {
	case pb.PortNum_TEXT_MESSAGE_APP:
		return string(data)

	case pb.PortNum_POSITION_APP:
		pos := &pb.Position{}
		if err := proto.Unmarshal(data, pos); err != nil {
			return "position: decode error"
		}
		lat := float64(pos.GetLatitudeI()) / 1e7
		lon := float64(pos.GetLongitudeI()) / 1e7
		alt := pos.GetAltitude()
		return fmt.Sprintf("lat=%.6f lon=%.6f alt=%dm", lat, lon, alt)

	case pb.PortNum_NODEINFO_APP:
		user := &pb.User{}
		if err := proto.Unmarshal(data, user); err != nil {
			return "nodeinfo: decode error"
		}
		return fmt.Sprintf("%s (%s) hw=%s",
			user.GetLongName(), user.GetShortName(), user.GetHwModel().String())

	case pb.PortNum_TELEMETRY_APP:
		tel := &pb.Telemetry{}
		if err := proto.Unmarshal(data, tel); err != nil {
			return "telemetry: decode error"
		}
		return decodeTelemetry(tel)

	case pb.PortNum_ROUTING_APP:
		routing := &pb.Routing{}
		if err := proto.Unmarshal(data, routing); err != nil {
			return "routing: decode error"
		}
		switch v := routing.GetVariant().(type) {
		case *pb.Routing_ErrorReason:
			return fmt.Sprintf("error=%s", v.ErrorReason.String())
		case *pb.Routing_RouteReply:
			return "route_reply"
		case *pb.Routing_RouteRequest:
			return "route_request"
		default:
			return "routing"
		}

	case pb.PortNum_ADMIN_APP:
		return fmt.Sprintf("admin (%d bytes)", len(data))

	case pb.PortNum_STORE_FORWARD_APP:
		return fmt.Sprintf("store_forward (%d bytes)", len(data))

	case pb.PortNum_TRACEROUTE_APP:
		route := &pb.RouteDiscovery{}
		if err := proto.Unmarshal(data, route); err != nil {
			return fmt.Sprintf("traceroute (%d bytes)", len(data))
		}
		var hops []string
		for _, h := range route.GetRoute() {
			hops = append(hops, fmt.Sprintf("!%08x", h))
		}
		return fmt.Sprintf("route: %s", strings.Join(hops, " -> "))

	case pb.PortNum_NEIGHBORINFO_APP:
		ni := &pb.NeighborInfo{}
		if err := proto.Unmarshal(data, ni); err != nil {
			return fmt.Sprintf("neighborinfo (%d bytes)", len(data))
		}
		return fmt.Sprintf("neighbors=%d node=!%08x",
			len(ni.GetNeighbors()), ni.GetNodeId())

	default:
		if len(data) <= 32 {
			return hex.EncodeToString(data)
		}
		return fmt.Sprintf("%s... (%d bytes)", hex.EncodeToString(data[:32]), len(data))
	}
}

// decodeTelemetry formats telemetry data into a human-readable string.
func decodeTelemetry(tel *pb.Telemetry) string {
	switch v := tel.GetVariant().(type) {
	case *pb.Telemetry_DeviceMetrics:
		dm := v.DeviceMetrics
		var parts []string
		if dm.GetBatteryLevel() > 0 {
			parts = append(parts, fmt.Sprintf("bat=%d%%", dm.GetBatteryLevel()))
		}
		if dm.GetVoltage() > 0 {
			parts = append(parts, fmt.Sprintf("%.2fV", dm.GetVoltage()))
		}
		if dm.GetChannelUtilization() > 0 {
			parts = append(parts, fmt.Sprintf("ch_util=%.1f%%", dm.GetChannelUtilization()))
		}
		if dm.GetAirUtilTx() > 0 {
			parts = append(parts, fmt.Sprintf("air_tx=%.1f%%", dm.GetAirUtilTx()))
		}
		if dm.GetUptimeSeconds() > 0 {
			parts = append(parts, fmt.Sprintf("up=%ds", dm.GetUptimeSeconds()))
		}
		if len(parts) == 0 {
			return "device_metrics"
		}
		return strings.Join(parts, " ")

	case *pb.Telemetry_EnvironmentMetrics:
		em := v.EnvironmentMetrics
		var parts []string
		if em.GetTemperature() != 0 {
			parts = append(parts, fmt.Sprintf("temp=%.1f°C", em.GetTemperature()))
		}
		if em.GetRelativeHumidity() != 0 {
			parts = append(parts, fmt.Sprintf("hum=%.1f%%", em.GetRelativeHumidity()))
		}
		if em.GetBarometricPressure() != 0 {
			parts = append(parts, fmt.Sprintf("press=%.1fhPa", em.GetBarometricPressure()))
		}
		if len(parts) == 0 {
			return "environment_metrics"
		}
		return strings.Join(parts, " ")

	case *pb.Telemetry_PowerMetrics:
		pm := v.PowerMetrics
		return fmt.Sprintf("ch1=%.2fV/%.1fmA ch2=%.2fV/%.1fmA",
			pm.GetCh1Voltage(), pm.GetCh1Current(),
			pm.GetCh2Voltage(), pm.GetCh2Current())

	default:
		return "telemetry"
	}
}

// decodeConfig returns a short description of a Config message.
func decodeConfig(cfg *pb.Config) string {
	if cfg == nil {
		return ""
	}
	switch v := cfg.GetPayloadVariant().(type) {
	case *pb.Config_Device:
		return fmt.Sprintf("device: role=%s", v.Device.GetRole().String())
	case *pb.Config_Position:
		return fmt.Sprintf("position: gps=%s", v.Position.GetGpsMode().String())
	case *pb.Config_Power:
		return fmt.Sprintf("power: sds=%ds ls=%ds",
			v.Power.GetSdsSecs(), v.Power.GetLsSecs())
	case *pb.Config_Network:
		return fmt.Sprintf("network: wifi=%v", v.Network.GetWifiEnabled())
	case *pb.Config_Display:
		return fmt.Sprintf("display: units=%s", v.Display.GetUnits().String())
	case *pb.Config_Lora:
		return fmt.Sprintf("lora: region=%s modem=%s",
			v.Lora.GetRegion().String(), v.Lora.GetModemPreset().String())
	case *pb.Config_Bluetooth:
		return fmt.Sprintf("bluetooth: enabled=%v mode=%s",
			v.Bluetooth.GetEnabled(), v.Bluetooth.GetMode().String())
	default:
		return "config"
	}
}

// decodeModuleConfig returns a short description of a ModuleConfig message.
func decodeModuleConfig(cfg *pb.ModuleConfig) string {
	if cfg == nil {
		return ""
	}
	switch cfg.GetPayloadVariant().(type) {
	case *pb.ModuleConfig_Mqtt:
		return "module: mqtt"
	case *pb.ModuleConfig_Serial:
		return "module: serial"
	case *pb.ModuleConfig_ExternalNotification:
		return "module: ext_notification"
	case *pb.ModuleConfig_StoreForward:
		return "module: store_forward"
	case *pb.ModuleConfig_RangeTest:
		return "module: range_test"
	case *pb.ModuleConfig_Telemetry:
		return "module: telemetry"
	case *pb.ModuleConfig_CannedMessage:
		return "module: canned_message"
	case *pb.ModuleConfig_Audio:
		return "module: audio"
	case *pb.ModuleConfig_RemoteHardware:
		return "module: remote_hardware"
	case *pb.ModuleConfig_NeighborInfo:
		return "module: neighbor_info"
	case *pb.ModuleConfig_AmbientLighting:
		return "module: ambient_lighting"
	case *pb.ModuleConfig_DetectionSensor:
		return "module: detection_sensor"
	case *pb.ModuleConfig_Paxcounter:
		return "module: paxcounter"
	default:
		return "module: unknown"
	}
}

// FromRadioTypeName returns a short string identifying the FromRadio payload type.
// Exported for use by other packages (e.g. proxy).
func FromRadioTypeName(msg *pb.FromRadio) string {
	switch msg.GetPayloadVariant().(type) {
	case *pb.FromRadio_MyInfo:
		return "my_info"
	case *pb.FromRadio_NodeInfo:
		return "node_info"
	case *pb.FromRadio_Config:
		return "config"
	case *pb.FromRadio_ModuleConfig:
		return "module_config"
	case *pb.FromRadio_Channel:
		return "channel"
	case *pb.FromRadio_ConfigCompleteId:
		return "config_complete_id"
	case *pb.FromRadio_Metadata:
		return "metadata"
	case *pb.FromRadio_DeviceuiConfig:
		return "deviceui_config"
	case *pb.FromRadio_FileInfo:
		return "file_info"
	case *pb.FromRadio_Packet:
		return "packet"
	case *pb.FromRadio_QueueStatus:
		return "queue_status"
	case *pb.FromRadio_LogRecord:
		return "log_record"
	case *pb.FromRadio_Rebooted:
		return "rebooted"
	default:
		return "unknown"
	}
}

// CountCacheFrameTypes returns a count of each FromRadio frame type in the cache.
// Exported for use by other packages.
func CountCacheFrameTypes(frames [][]byte) map[string]int {
	counts := make(map[string]int)
	for _, frame := range frames {
		msg := &pb.FromRadio{}
		if err := proto.Unmarshal(frame, msg); err != nil {
			counts["unparseable"]++
			continue
		}
		counts[FromRadioTypeName(msg)]++
	}
	return counts
}

func copyBytes(b []byte) []byte {
	cp := make([]byte, len(b))
	copy(cp, b)
	return cp
}

// ExtractNodeDirectory parses NodeInfo frames from the config cache and
// returns a map of node number → NodeEntry for name resolution.
func ExtractNodeDirectory(frames [][]byte) map[uint32]metrics.NodeEntry {
	dir := make(map[uint32]metrics.NodeEntry)
	for _, frame := range frames {
		msg := &pb.FromRadio{}
		if err := proto.Unmarshal(frame, msg); err != nil {
			continue
		}
		ni, ok := msg.GetPayloadVariant().(*pb.FromRadio_NodeInfo)
		if !ok || ni.NodeInfo == nil {
			continue
		}
		num := ni.NodeInfo.GetNum()
		if num == 0 {
			continue
		}
		u := ni.NodeInfo.GetUser()
		if u == nil {
			continue
		}
		dir[num] = metrics.NodeEntry{
			ShortName: u.GetShortName(),
			LongName:  u.GetLongName(),
		}
	}
	return dir
}
