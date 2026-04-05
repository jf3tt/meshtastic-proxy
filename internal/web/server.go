package web

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"time"

	pb "buf.build/gen/go/meshtastic/protobufs/protocolbuffers/go/meshtastic"
	"google.golang.org/protobuf/proto"

	"github.com/jfett/meshtastic-proxy/internal/metrics"
)

// ServerOption is a functional option for configuring the web server.
type ServerOption func(*Server)

// WithChatSupport configures the server with chat functionality.
// sendToNodeFn sends a raw ToRadio payload to the node.
// configCacheFn returns the cached config frames for channel extraction.
// myNodeNumFn returns the connected node's number.
func WithChatSupport(sendToNodeFn func([]byte), configCacheFn func() [][]byte, myNodeNumFn func() uint32) ServerOption {
	return func(s *Server) {
		s.sendToNodeFn = sendToNodeFn
		s.configCacheFn = configCacheFn
		s.myNodeNumFn = myNodeNumFn
	}
}

//go:embed templates/*.html templates/static/*
var templateFS embed.FS

// Server provides an HTTP dashboard and API for monitoring the proxy.
type Server struct {
	listenAddr    string
	metrics       *metrics.Metrics
	logger        *slog.Logger
	clientsFn     func() []string // returns connected client addresses
	sendToNodeFn  func(payload []byte)
	configCacheFn func() [][]byte
	myNodeNumFn   func() uint32
	templates     *template.Template
	promHandler   http.Handler // Prometheus metrics handler (nil = disabled)
}

// NewServer creates a new web server.
// promHandler is optional; when non-nil it is registered at GET /metrics.
func NewServer(listenAddr string, m *metrics.Metrics, logger *slog.Logger, clientsFn func() []string, promHandler http.Handler, opts ...ServerOption) *Server {
	funcMap := template.FuncMap{
		"json": func(v any) template.JS {
			b, _ := json.Marshal(v)
			return template.JS(b)
		},
		"nodeHex": func(n uint32) string {
			if n == 0 {
				return ""
			}
			return fmt.Sprintf("!%08x", n)
		},
		"nodeName": func(n uint32, dir map[uint32]metrics.NodeEntry) template.HTML {
			if n == 0 {
				return ""
			}
			hex := fmt.Sprintf("!%08x", n)
			if entry, ok := dir[n]; ok && entry.ShortName != "" {
				return template.HTML(fmt.Sprintf(
					`<span title="%s (%s)" class="cursor-help">%s</span>`,
					hex, template.HTMLEscapeString(entry.LongName),
					template.HTMLEscapeString(entry.ShortName),
				))
			}
			return template.HTML(fmt.Sprintf(
				`<code class="font-mono text-xs text-gray-400">%s</code>`, hex))
		},
		"relayName": func(relayNode uint32) string {
			if relayNode == 0 {
				return ""
			}
			return fmt.Sprintf("!%02x", uint8(relayNode&0xFF))
		},
		"formatBytes": func(b int64) string {
			if b == 0 {
				return "0 B"
			}
			const unit = 1024.0
			units := []string{"B", "KB", "MB", "GB"}
			f := float64(b)
			i := 0
			for f >= unit && i < len(units)-1 {
				f /= unit
				i++
			}
			if i == 0 {
				return fmt.Sprintf("%d B", b)
			}
			return fmt.Sprintf("%.1f %s", f, units[i])
		},
	}

	tmpl := template.Must(template.New("").Funcs(funcMap).ParseFS(templateFS, "templates/*.html"))

	s := &Server{
		listenAddr:  listenAddr,
		metrics:     m,
		logger:      logger,
		clientsFn:   clientsFn,
		templates:   tmpl,
		promHandler: promHandler,
	}

	for _, opt := range opts {
		opt(s)
	}

	return s
}

// buildMux creates the HTTP multiplexer with all routes.
func (s *Server) buildMux() *http.ServeMux {
	mux := http.NewServeMux()

	// Static files — use fs.Sub to shift the root from the embed directory
	// so that /static/tailwind.css maps to templates/static/tailwind.css.
	staticFS, _ := fs.Sub(templateFS, "templates")
	mux.Handle("/static/", http.FileServer(http.FS(staticFS)))

	// Health endpoints
	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.HandleFunc("/readyz", s.handleReadyz)

	// Dashboard
	mux.HandleFunc("/", s.handleDashboard)

	// API endpoints
	mux.HandleFunc("/api/metrics", s.handleAPIMetrics)
	mux.HandleFunc("/api/clients", s.handleAPIClients)
	mux.HandleFunc("/api/events", s.handleSSE)

	// Chat API endpoints
	mux.HandleFunc("/api/chat/messages", s.handleAPIChatMessages)
	mux.HandleFunc("/api/chat/send", s.handleAPIChatSend)
	mux.HandleFunc("/api/chat/channels", s.handleAPIChatChannels)

	// Traceroute API
	mux.HandleFunc("/api/traceroute", s.handleAPITraceroute)

	// Prometheus metrics endpoint
	if s.promHandler != nil {
		mux.Handle("/metrics", s.promHandler)
	}

	return mux
}

// Run starts the HTTP server. Blocks until the context is canceled.
func (s *Server) Run(ctx context.Context) error {
	mux := s.buildMux()

	srv := &http.Server{
		Addr:              s.listenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			s.logger.Error("http server shutdown error", "error", err)
		}
	}()

	s.logger.Info("web dashboard listening", "address", s.listenAddr)

	err := srv.ListenAndServe()
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

// handleHealthz returns 200 OK unconditionally — the proxy process is alive.
func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

// handleReadyz returns 200 if the proxy is ready to serve clients (node
// connected and config cache populated), or 503 otherwise.
func (s *Server) handleReadyz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if s.metrics.Ready() {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	} else {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("not ready\n"))
	}
}

// DashboardData contains all data passed to the dashboard template.
type DashboardData struct {
	Metrics metrics.Snapshot
	Clients []string
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	data := DashboardData{
		Metrics: s.metrics.Snapshot(),
		Clients: s.clientsFn(),
	}

	var buf bytes.Buffer
	if err := s.templates.ExecuteTemplate(&buf, "dashboard.html", data); err != nil {
		s.logger.Error("template render error", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if _, err := buf.WriteTo(w); err != nil {
		s.logger.Debug("dashboard write error", "error", err)
	}
}

func (s *Server) handleAPIMetrics(w http.ResponseWriter, r *http.Request) {
	snap := s.metrics.Snapshot()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(snap); err != nil {
		s.logger.Error("failed to encode metrics response", "error", err)
	}
}

func (s *Server) handleAPIClients(w http.ResponseWriter, r *http.Request) {
	clients := s.clientsFn()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{
		"clients": clients,
		"count":   len(clients),
	}); err != nil {
		s.logger.Error("failed to encode clients response", "error", err)
	}
}

// handleAPIChatMessages returns the chat message history as JSON.
func (s *Server) handleAPIChatMessages(w http.ResponseWriter, _ *http.Request) {
	messages := s.metrics.ChatMessages()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(messages); err != nil {
		s.logger.Error("failed to encode chat messages response", "error", err)
	}
}

// chatSendRequest is the JSON body for POST /api/chat/send.
type chatSendRequest struct {
	Text    string `json:"text"`
	To      uint32 `json:"to"`
	Channel uint32 `json:"channel"`
}

// handleAPIChatSend sends a text message via the connected Meshtastic node.
func (s *Server) handleAPIChatSend(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if s.sendToNodeFn == nil {
		http.Error(w, "chat not available", http.StatusServiceUnavailable)
		return
	}

	var req chatSendRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}

	if req.Text == "" {
		http.Error(w, "text is required", http.StatusBadRequest)
		return
	}
	if len(req.Text) > 237 {
		http.Error(w, "text too long (max 237 bytes)", http.StatusBadRequest)
		return
	}

	// Default to broadcast
	to := req.To
	if to == 0 {
		to = 0xFFFFFFFF
	}

	// Build ToRadio with MeshPacket containing TEXT_MESSAGE_APP
	toRadio := &pb.ToRadio{
		PayloadVariant: &pb.ToRadio_Packet{
			Packet: &pb.MeshPacket{
				To:      to,
				Channel: req.Channel,
				PayloadVariant: &pb.MeshPacket_Decoded{
					Decoded: &pb.Data{
						Portnum: pb.PortNum_TEXT_MESSAGE_APP,
						Payload: []byte(req.Text),
					},
				},
				WantAck: true,
			},
		},
	}

	data, err := proto.Marshal(toRadio)
	if err != nil {
		s.logger.Error("failed to marshal chat ToRadio", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	s.sendToNodeFn(data)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"ok":true}` + "\n"))
}

// tracerouteRequest is the JSON body for POST /api/traceroute.
type tracerouteRequest struct {
	Target uint32 `json:"target"` // destination node number
}

// handleAPITraceroute sends a traceroute request to the specified node.
// The result arrives asynchronously via SSE "traceroute_result" event.
func (s *Server) handleAPITraceroute(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if s.sendToNodeFn == nil {
		http.Error(w, "traceroute not available", http.StatusServiceUnavailable)
		return
	}

	var req tracerouteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}

	if req.Target == 0 {
		http.Error(w, "target is required", http.StatusBadRequest)
		return
	}

	// Build ToRadio with MeshPacket containing TRACEROUTE_APP + WantResponse
	routePayload, err := proto.Marshal(&pb.RouteDiscovery{})
	if err != nil {
		s.logger.Error("failed to marshal RouteDiscovery", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	toRadio := &pb.ToRadio{
		PayloadVariant: &pb.ToRadio_Packet{
			Packet: &pb.MeshPacket{
				To: req.Target,
				PayloadVariant: &pb.MeshPacket_Decoded{
					Decoded: &pb.Data{
						Portnum:      pb.PortNum_TRACEROUTE_APP,
						Payload:      routePayload,
						WantResponse: true,
					},
				},
				WantAck: true,
			},
		},
	}

	data, err := proto.Marshal(toRadio)
	if err != nil {
		s.logger.Error("failed to marshal traceroute ToRadio", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	s.sendToNodeFn(data)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"ok":true}` + "\n"))
}

// channelInfo is a single channel entry returned by the channels API.
type channelInfo struct {
	Index int32  `json:"index"`
	Name  string `json:"name"`
	Role  string `json:"role"`
}

// handleAPIChatChannels returns the list of configured channels from the config cache.
func (s *Server) handleAPIChatChannels(w http.ResponseWriter, _ *http.Request) {
	var channels []channelInfo

	if s.configCacheFn != nil {
		frames := s.configCacheFn()
		for _, frame := range frames {
			msg := &pb.FromRadio{}
			if err := proto.Unmarshal(frame, msg); err != nil {
				continue
			}
			ch, ok := msg.GetPayloadVariant().(*pb.FromRadio_Channel)
			if !ok || ch.Channel == nil {
				continue
			}
			role := ch.Channel.GetRole()
			if role == pb.Channel_DISABLED {
				continue
			}
			name := ""
			if s := ch.Channel.GetSettings(); s != nil {
				name = s.GetName()
			}
			if name == "" && ch.Channel.GetIndex() == 0 {
				name = "Primary"
			}
			channels = append(channels, channelInfo{
				Index: ch.Channel.GetIndex(),
				Name:  name,
				Role:  role.String(),
			})
		}
	}

	if channels == nil {
		channels = []channelInfo{}
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(channels); err != nil {
		s.logger.Error("failed to encode channels response", "error", err)
	}
}

// handleSSE streams Server-Sent Events to the browser.
func (s *Server) handleSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	ch := s.metrics.Subscribe()
	defer s.metrics.Unsubscribe(ch)

	ctx := r.Context()

	// Send initial snapshot so the client has data immediately
	snap := s.metrics.Snapshot()
	snapJSON, _ := json.Marshal(snap)
	_, _ = fmt.Fprintf(w, "event: metrics\ndata: %s\n\n", snapJSON)

	// Send initial client list
	clientsJSON, _ := json.Marshal(s.clientsFn())
	_, _ = fmt.Fprintf(w, "event: clients\ndata: %s\n\n", clientsJSON)

	// Send initial node directory
	nodeDirJSON, _ := json.Marshal(s.metrics.NodeDirectory())
	_, _ = fmt.Fprintf(w, "event: node_directory\ndata: %s\n\n", nodeDirJSON)

	// Send initial chat history
	chatJSON, _ := json.Marshal(s.metrics.ChatMessages())
	_, _ = fmt.Fprintf(w, "event: chat_history\ndata: %s\n\n", chatJSON)
	flusher.Flush()

	for {
		select {
		case <-ctx.Done():
			return
		case evt, ok := <-ch:
			if !ok {
				return
			}

			data, err := json.Marshal(evt.Data)
			if err != nil {
				continue
			}

			_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", evt.Type, data)
			flusher.Flush()
		}
	}
}
