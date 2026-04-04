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

	"github.com/jfett/meshtastic-proxy/internal/metrics"
)

//go:embed templates/*.html templates/static/*
var templateFS embed.FS

// Server provides an HTTP dashboard and API for monitoring the proxy.
type Server struct {
	listenAddr  string
	metrics     *metrics.Metrics
	logger      *slog.Logger
	clientsFn   func() []string // returns connected client addresses
	templates   *template.Template
	promHandler http.Handler // Prometheus metrics handler (nil = disabled)
}

// NewServer creates a new web server.
// promHandler is optional; when non-nil it is registered at GET /metrics.
func NewServer(listenAddr string, m *metrics.Metrics, logger *slog.Logger, clientsFn func() []string, promHandler http.Handler) *Server {
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

	return &Server{
		listenAddr:  listenAddr,
		metrics:     m,
		logger:      logger,
		clientsFn:   clientsFn,
		templates:   tmpl,
		promHandler: promHandler,
	}
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
