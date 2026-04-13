package node

import (
	pb "buf.build/gen/go/meshtastic/protobufs/protocolbuffers/go/meshtastic"
	"google.golang.org/protobuf/proto"

	"github.com/jfett/meshtastic-proxy/internal/metrics"
)

// latLonDivisor converts Meshtastic fixed-point lat/lon (int32 × 1e7) to float64 degrees.
const latLonDivisor = 1e7

// ---------------------------------------------------------------------------
// Extract helper
// ---------------------------------------------------------------------------

// extractDecodedPacket unmarshals a FromRadio payload and extracts the decoded
// MeshPacket and Data fields, filtering by the expected portnum.
// Returns the MeshPacket and Data if the payload matches, otherwise nil.
func extractDecodedPacket(payload []byte, portnum pb.PortNum) (*pb.MeshPacket, *pb.Data) {
	msg := &pb.FromRadio{}
	if err := proto.Unmarshal(payload, msg); err != nil {
		return nil, nil
	}
	pkt, ok := msg.GetPayloadVariant().(*pb.FromRadio_Packet)
	if !ok || pkt.Packet == nil {
		return nil, nil
	}
	decoded, ok := pkt.Packet.GetPayloadVariant().(*pb.MeshPacket_Decoded)
	if !ok || decoded.Decoded == nil {
		return nil, nil
	}
	if decoded.Decoded.GetPortnum() != portnum {
		return nil, nil
	}
	return pkt.Packet, decoded.Decoded
}

// ---------------------------------------------------------------------------
// Extract types
// ---------------------------------------------------------------------------

// NodePosition holds structured position data extracted from a POSITION_APP packet.
type NodePosition struct {
	NodeNum      uint32
	Latitude     float64
	Longitude    float64
	Altitude     int32
	GroundSpeed  uint32
	GroundTrack  uint32
	SatsInView   uint32
	PositionTime uint32 // unix timestamp of position fix
}

// NodeTelemetry holds structured telemetry data extracted from a TELEMETRY_APP packet.
type NodeTelemetry struct {
	NodeNum            uint32
	BatteryLevel       uint32
	Voltage            float32
	ChannelUtilization float32
	AirUtilTx          float32
	UptimeSeconds      uint32
	Temperature        float32
	RelativeHumidity   float32
	BarometricPressure float32
}

// NodeSignal holds per-packet signal quality extracted from a MeshPacket.
type NodeSignal struct {
	NodeNum uint32
	RxRssi  int32
	RxSnr   float32
}

// NodeInfoData holds identity fields extracted from a NODEINFO_APP packet.
// This is used for real-time node discovery — when a new node joins the mesh
// and sends a NODEINFO_APP broadcast, the proxy can add it to the node directory
// without waiting for a full config re-request.
type NodeInfoData struct {
	NodeNum        uint32
	ShortName      string
	LongName       string
	UserID         string
	HwModel        string
	Role           string
	IsLicensed     bool
	PublicKey      []byte // PKI public key for computing shared secrets
	Macaddr        []byte // deprecated but preserved for pass-through fidelity
	IsUnmessagable *bool  // nil = not set in protobuf (proto3 optional)
}

// TracerouteData holds the route discovered by a TRACEROUTE_APP response.
type TracerouteData struct {
	From       uint32   // who responded (target node)
	To         uint32   // who requested (our node)
	Route      []uint32 // forward hops (from requester toward target)
	RouteBack  []uint32 // return hops (from target back to requester)
	SnrTowards []int32  // SNR (dB×4) at each forward hop
	SnrBack    []int32  // SNR (dB×4) at each return hop
}

// ChatMessageData holds raw chat message fields extracted from a MeshPacket.
// The caller is responsible for enriching it with node names from the directory.
type ChatMessageData struct {
	From    uint32
	To      uint32
	Channel uint32
	Text    string
	ViaMqtt bool
	RxRssi  int32
	RxSnr   float32
}

// ---------------------------------------------------------------------------
// Extract functions
// ---------------------------------------------------------------------------

// ExtractPosition tries to extract a structured position update from a
// FromRadio payload containing a decoded MeshPacket with PortNum_POSITION_APP.
// Returns nil if the payload is not a position update or has zero coordinates.
func ExtractPosition(payload []byte) *NodePosition {
	pkt, data := extractDecodedPacket(payload, pb.PortNum_POSITION_APP)
	if pkt == nil {
		return nil
	}
	pos := &pb.Position{}
	if err := proto.Unmarshal(data.GetPayload(), pos); err != nil {
		return nil
	}
	lat := float64(pos.GetLatitudeI()) / latLonDivisor
	lon := float64(pos.GetLongitudeI()) / latLonDivisor
	if lat == 0 && lon == 0 {
		return nil
	}
	return &NodePosition{
		NodeNum:      pkt.GetFrom(),
		Latitude:     lat,
		Longitude:    lon,
		Altitude:     pos.GetAltitude(),
		GroundSpeed:  pos.GetGroundSpeed(),
		GroundTrack:  pos.GetGroundTrack(),
		SatsInView:   pos.GetSatsInView(),
		PositionTime: pos.GetTime(),
	}
}

// ExtractSignal tries to extract signal quality (RSSI/SNR) from a FromRadio
// payload containing a decoded MeshPacket. Returns nil if the payload is not
// a MeshPacket, has no From node, or has zero RxRssi.
func ExtractSignal(payload []byte) *NodeSignal {
	msg := &pb.FromRadio{}
	if err := proto.Unmarshal(payload, msg); err != nil {
		return nil
	}
	pkt, ok := msg.GetPayloadVariant().(*pb.FromRadio_Packet)
	if !ok || pkt.Packet == nil {
		return nil
	}
	from := pkt.Packet.GetFrom()
	rssi := pkt.Packet.GetRxRssi()
	snr := pkt.Packet.GetRxSnr()
	if from == 0 || rssi == 0 {
		return nil
	}
	return &NodeSignal{
		NodeNum: from,
		RxRssi:  rssi,
		RxSnr:   snr,
	}
}

// ExtractTelemetry tries to extract structured telemetry from a FromRadio
// payload containing a decoded MeshPacket with PortNum_TELEMETRY_APP.
// Returns nil if the payload is not a telemetry update or cannot be decoded.
func ExtractTelemetry(payload []byte) *NodeTelemetry {
	pkt, data := extractDecodedPacket(payload, pb.PortNum_TELEMETRY_APP)
	if pkt == nil {
		return nil
	}
	tel := &pb.Telemetry{}
	if err := proto.Unmarshal(data.GetPayload(), tel); err != nil {
		return nil
	}

	result := &NodeTelemetry{
		NodeNum: pkt.GetFrom(),
	}

	switch v := tel.GetVariant().(type) {
	case *pb.Telemetry_DeviceMetrics:
		if dm := v.DeviceMetrics; dm != nil {
			result.BatteryLevel = dm.GetBatteryLevel()
			result.Voltage = dm.GetVoltage()
			result.ChannelUtilization = dm.GetChannelUtilization()
			result.AirUtilTx = dm.GetAirUtilTx()
			result.UptimeSeconds = dm.GetUptimeSeconds()
		}
	case *pb.Telemetry_EnvironmentMetrics:
		if em := v.EnvironmentMetrics; em != nil {
			result.Temperature = em.GetTemperature()
			result.RelativeHumidity = em.GetRelativeHumidity()
			result.BarometricPressure = em.GetBarometricPressure()
		}
	default:
		// PowerMetrics and other telemetry types are not stored in NodeEntry
		return nil
	}

	return result
}

// ExtractNodeInfo tries to extract node identity from a FromRadio payload
// containing a decoded MeshPacket with PortNum_NODEINFO_APP.
// Returns nil if the payload is not a NODEINFO_APP packet or cannot be decoded.
func ExtractNodeInfo(payload []byte) *NodeInfoData {
	pkt, data := extractDecodedPacket(payload, pb.PortNum_NODEINFO_APP)
	if pkt == nil {
		return nil
	}
	user := &pb.User{}
	if err := proto.Unmarshal(data.GetPayload(), user); err != nil {
		return nil
	}
	from := pkt.GetFrom()
	if from == 0 {
		return nil
	}
	result := &NodeInfoData{
		NodeNum:    from,
		ShortName:  user.GetShortName(),
		LongName:   user.GetLongName(),
		UserID:     user.GetId(),
		HwModel:    user.GetHwModel().String(),
		Role:       user.GetRole().String(),
		IsLicensed: user.GetIsLicensed(),
		PublicKey:  user.GetPublicKey(),
		Macaddr:    user.GetMacaddr(), //nolint:staticcheck // deprecated but needed for client compat
	}
	if user.HasIsUnmessagable() {
		v := user.GetIsUnmessagable()
		result.IsUnmessagable = &v
	}
	return result
}

// ExtractTraceroute tries to extract a traceroute response from a FromRadio
// payload containing a decoded MeshPacket with PortNum_TRACEROUTE_APP.
// Returns nil if the payload is not a TRACEROUTE_APP packet or cannot be decoded.
func ExtractTraceroute(payload []byte) *TracerouteData {
	pkt, data := extractDecodedPacket(payload, pb.PortNum_TRACEROUTE_APP)
	if pkt == nil {
		return nil
	}
	route := &pb.RouteDiscovery{}
	if err := proto.Unmarshal(data.GetPayload(), route); err != nil {
		return nil
	}
	return &TracerouteData{
		From:       pkt.GetFrom(),
		To:         pkt.GetTo(),
		Route:      route.GetRoute(),
		RouteBack:  route.GetRouteBack(),
		SnrTowards: route.GetSnrTowards(),
		SnrBack:    route.GetSnrBack(),
	}
}

// ExtractChatMessage tries to extract a text message from a FromRadio payload.
// Returns nil if the payload is not a TEXT_MESSAGE_APP packet.
func ExtractChatMessage(payload []byte) *ChatMessageData {
	pkt, data := extractDecodedPacket(payload, pb.PortNum_TEXT_MESSAGE_APP)
	if pkt == nil {
		return nil
	}
	return &ChatMessageData{
		From:    pkt.GetFrom(),
		To:      pkt.GetTo(),
		Channel: pkt.GetChannel(),
		Text:    string(data.GetPayload()),
		ViaMqtt: pkt.GetViaMqtt(),
		RxRssi:  pkt.GetRxRssi(),
		RxSnr:   pkt.GetRxSnr(),
	}
}

// ExtractChatMessageFromToRadio tries to extract a text message from a ToRadio payload.
// Returns nil if the payload is not a ToRadio_Packet with TEXT_MESSAGE_APP.
func ExtractChatMessageFromToRadio(payload []byte) *ChatMessageData {
	msg := &pb.ToRadio{}
	if err := proto.Unmarshal(payload, msg); err != nil {
		return nil
	}
	pkt, ok := msg.GetPayloadVariant().(*pb.ToRadio_Packet)
	if !ok || pkt.Packet == nil {
		return nil
	}
	decoded, ok := pkt.Packet.GetPayloadVariant().(*pb.MeshPacket_Decoded)
	if !ok || decoded.Decoded == nil {
		return nil
	}
	if decoded.Decoded.GetPortnum() != pb.PortNum_TEXT_MESSAGE_APP {
		return nil
	}
	return &ChatMessageData{
		From:    pkt.Packet.GetFrom(),
		To:      pkt.Packet.GetTo(),
		Channel: pkt.Packet.GetChannel(),
		Text:    string(decoded.Decoded.GetPayload()),
		ViaMqtt: pkt.Packet.GetViaMqtt(),
	}
}

// ExtractNodeDirectory parses NodeInfo frames from the config cache and
// returns a map of node number → NodeEntry with all available fields.
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
		entry := metrics.NodeEntry{
			Snr:        ni.NodeInfo.GetSnr(),
			LastHeard:  ni.NodeInfo.GetLastHeard(),
			HopsAway:   ni.NodeInfo.GetHopsAway(),
			ViaMqtt:    ni.NodeInfo.GetViaMqtt(),
			IsFavorite: ni.NodeInfo.GetIsFavorite(),
			Channel:    ni.NodeInfo.GetChannel(),
		}
		if u != nil {
			entry.ShortName = u.GetShortName()
			entry.LongName = u.GetLongName()
			entry.UserID = u.GetId()
			entry.HwModel = u.GetHwModel().String()
			entry.Role = u.GetRole().String()
			entry.IsLicensed = u.GetIsLicensed()
		}

		// Position from NodeInfo
		if pos := ni.NodeInfo.GetPosition(); pos != nil {
			lat := float64(pos.GetLatitudeI()) / latLonDivisor
			lon := float64(pos.GetLongitudeI()) / latLonDivisor
			if lat != 0 || lon != 0 {
				entry.Latitude = lat
				entry.Longitude = lon
				entry.Altitude = pos.GetAltitude()
				entry.SatsInView = pos.GetSatsInView()
				entry.GroundSpeed = pos.GetGroundSpeed()
				entry.GroundTrack = pos.GetGroundTrack()
				entry.PositionTime = pos.GetTime()
			}
		}

		// Device metrics from NodeInfo (populated by firmware)
		if dm := ni.NodeInfo.GetDeviceMetrics(); dm != nil {
			entry.BatteryLevel = dm.GetBatteryLevel()
			entry.Voltage = dm.GetVoltage()
			entry.ChannelUtilization = dm.GetChannelUtilization()
			entry.AirUtilTx = dm.GetAirUtilTx()
			entry.UptimeSeconds = dm.GetUptimeSeconds()
		}

		dir[num] = entry
	}
	return dir
}
