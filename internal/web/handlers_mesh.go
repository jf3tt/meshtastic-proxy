package web

import (
	"encoding/json"
	"net/http"

	pb "buf.build/gen/go/meshtastic/protobufs/protocolbuffers/go/meshtastic"
	"google.golang.org/protobuf/proto"
)

// sendMeshPacket is a helper that builds a ToRadio wrapping a MeshPacket with
// the given portnum, payload, WantResponse, and WantAck flags, marshals it,
// and calls sendToNodeFn.  It writes the appropriate HTTP error on failure and
// returns false; on success it returns true (caller should write the response).
func (s *Server) sendMeshPacket(w http.ResponseWriter, target uint32, portnum pb.PortNum, payload []byte) bool {
	toRadio := &pb.ToRadio{
		PayloadVariant: &pb.ToRadio_Packet{
			Packet: &pb.MeshPacket{
				To: target,
				PayloadVariant: &pb.MeshPacket_Decoded{
					Decoded: &pb.Data{
						Portnum:      portnum,
						Payload:      payload,
						WantResponse: true,
					},
				},
				WantAck: true,
			},
		},
	}

	data, err := proto.Marshal(toRadio)
	if err != nil {
		s.logger.Error("failed to marshal ToRadio", "error", err, "portnum", portnum.String())
		http.Error(w, "internal error", http.StatusInternalServerError)
		return false
	}

	s.sendToNodeFn(data)
	return true
}

// nodeActionRequest is the JSON body for node action endpoints
// (traceroute, request-position, request-nodeinfo, store-forward).
type nodeActionRequest struct {
	Target uint32 `json:"target"` // destination node number
}

// requireNodeAction performs the common validation for mesh action endpoints:
// POST method, sendToNodeFn available, valid JSON body with non-zero target.
// Returns the parsed target node number and true on success, or writes an
// HTTP error and returns 0, false.
func (s *Server) requireNodeAction(w http.ResponseWriter, r *http.Request) (uint32, bool) {
	if !requirePost(w, r) {
		return 0, false
	}

	if s.sendToNodeFn == nil {
		http.Error(w, "not available", http.StatusServiceUnavailable)
		return 0, false
	}

	var req nodeActionRequest
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return 0, false
	}

	if req.Target == 0 {
		http.Error(w, "target is required", http.StatusBadRequest)
		return 0, false
	}

	return req.Target, true
}

// handleAPITraceroute sends a traceroute request to the specified node.
// The result arrives asynchronously via SSE "traceroute_result" event.
func (s *Server) handleAPITraceroute(w http.ResponseWriter, r *http.Request) {
	target, ok := s.requireNodeAction(w, r)
	if !ok {
		return
	}

	routePayload, err := proto.Marshal(&pb.RouteDiscovery{})
	if err != nil {
		s.logger.Error("failed to marshal RouteDiscovery", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	if s.sendMeshPacket(w, target, pb.PortNum_TRACEROUTE_APP, routePayload) {
		jsonOK(w)
	}
}

// handleAPIRequestPosition sends a position request to the specified node.
// The result arrives asynchronously via SSE "node_position_update" event.
func (s *Server) handleAPIRequestPosition(w http.ResponseWriter, r *http.Request) {
	target, ok := s.requireNodeAction(w, r)
	if !ok {
		return
	}

	if s.sendMeshPacket(w, target, pb.PortNum_POSITION_APP, []byte{}) {
		jsonOK(w)
	}
}

// handleAPIRequestNodeInfo sends a nodeinfo request to the specified node.
// The result arrives asynchronously via SSE "node_update" event.
func (s *Server) handleAPIRequestNodeInfo(w http.ResponseWriter, r *http.Request) {
	target, ok := s.requireNodeAction(w, r)
	if !ok {
		return
	}

	if s.sendMeshPacket(w, target, pb.PortNum_NODEINFO_APP, []byte{}) {
		jsonOK(w)
	}
}

// handleAPIStoreForward sends a Store & Forward history request to the specified node.
// This asks a Store & Forward router node to replay missed messages.
func (s *Server) handleAPIStoreForward(w http.ResponseWriter, r *http.Request) {
	target, ok := s.requireNodeAction(w, r)
	if !ok {
		return
	}

	sfPayload, err := proto.Marshal(&pb.StoreAndForward{
		Rr:      pb.StoreAndForward_CLIENT_HISTORY,
		Variant: &pb.StoreAndForward_History_{History: &pb.StoreAndForward_History{}},
	})
	if err != nil {
		s.logger.Error("failed to marshal StoreAndForward payload", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	if s.sendMeshPacket(w, target, pb.PortNum_STORE_FORWARD_APP, sfPayload) {
		jsonOK(w)
	}
}
