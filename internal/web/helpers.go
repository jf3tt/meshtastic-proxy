package web

import "net/http"

// maxTextPayload is the maximum size of a text message payload in Meshtastic
// (limited by the MeshPacket data payload size minus protobuf overhead).
const maxTextPayload = 237

// maxRequestBody limits the size of POST request bodies to prevent
// memory exhaustion from oversized requests (4 KB is plenty for all
// JSON payloads used by the API).
const maxRequestBody = 4096

// jsonOK writes a standard {"ok":true} JSON response.
func jsonOK(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("{\"ok\":true}\n"))
}

// requirePost returns true if the request method is POST; otherwise it
// writes a 405 Method Not Allowed response and returns false.
func requirePost(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return false
	}
	return true
}
