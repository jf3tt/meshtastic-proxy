package web

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/jfett/meshtastic-proxy/internal/metrics"
)

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// newPromHTTPHandler wraps a Prometheus registry into an http.Handler.
func newPromHTTPHandler(reg *prometheus.Registry) http.Handler {
	return promhttp.HandlerFor(reg, promhttp.HandlerOpts{})
}

// newTestServer creates a Server backed by fresh metrics and a mock clientsFn.
func newTestServer(t *testing.T, clients []string) *Server {
	t.Helper()
	m := metrics.New(10, 300)
	m.NodeAddress = "10.10.0.3:4403"

	clientsFn := func() []string {
		return clients
	}

	return NewServer(":0", m, slog.Default(), clientsFn, nil)
}

// newTestServerWithMetrics creates a Server with a pre-configured Metrics.
func newTestServerWithMetrics(t *testing.T, m *metrics.Metrics, clients []string) *Server {
	t.Helper()
	clientsFn := func() []string {
		return clients
	}
	return NewServer(":0", m, slog.Default(), clientsFn, nil)
}

// ---------------------------------------------------------------------------
// /healthz tests
// ---------------------------------------------------------------------------

func TestHealthz_Returns200(t *testing.T) {
	s := newTestServer(t, nil)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()

	s.handleHealthz(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	body := rec.Body.String()
	if body != "ok\n" {
		t.Errorf("body = %q, want %q", body, "ok\n")
	}

	ct := rec.Header().Get("Content-Type")
	if ct != "text/plain; charset=utf-8" {
		t.Errorf("Content-Type = %q, want %q", ct, "text/plain; charset=utf-8")
	}
}

// ---------------------------------------------------------------------------
// /readyz tests
// ---------------------------------------------------------------------------

func TestReadyz_NotReady(t *testing.T) {
	m := metrics.New(10, 300)
	// Node not connected, cache empty → not ready.
	s := newTestServerWithMetrics(t, m, nil)

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()

	s.handleReadyz(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}

	body := rec.Body.String()
	if body != "not ready\n" {
		t.Errorf("body = %q, want %q", body, "not ready\n")
	}
}

func TestReadyz_Ready(t *testing.T) {
	m := metrics.New(10, 300)
	m.NodeConnected.Store(true)
	m.ConfigCacheFrames.Store(42)
	s := newTestServerWithMetrics(t, m, nil)

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()

	s.handleReadyz(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	body := rec.Body.String()
	if body != "ok\n" {
		t.Errorf("body = %q, want %q", body, "ok\n")
	}
}

func TestReadyz_ConnectedButNoCacheNotReady(t *testing.T) {
	m := metrics.New(10, 300)
	m.NodeConnected.Store(true)
	// ConfigCacheFrames = 0 → not ready.
	s := newTestServerWithMetrics(t, m, nil)

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()

	s.handleReadyz(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
}

func TestReadyz_CacheButNotConnectedNotReady(t *testing.T) {
	m := metrics.New(10, 300)
	m.ConfigCacheFrames.Store(10)
	// NodeConnected = false → not ready.
	s := newTestServerWithMetrics(t, m, nil)

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()

	s.handleReadyz(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
}

// ---------------------------------------------------------------------------
// /api/metrics tests
// ---------------------------------------------------------------------------

func TestAPIMetrics_ReturnsJSON(t *testing.T) {
	m := metrics.New(10, 300)
	m.NodeAddress = "10.10.0.3:4403"
	m.NodeConnected.Store(true)
	m.ActiveClients.Store(2)
	m.BytesFromNode.Store(1024)
	m.FramesFromNode.Store(10)
	s := newTestServerWithMetrics(t, m, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/metrics", nil)
	rec := httptest.NewRecorder()

	s.handleAPIMetrics(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	ct := rec.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("Content-Type = %q, want %q", ct, "application/json")
	}

	var snap metrics.Snapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &snap); err != nil {
		t.Fatalf("failed to decode JSON: %v", err)
	}

	if snap.NodeAddress != "10.10.0.3:4403" {
		t.Errorf("NodeAddress = %q, want %q", snap.NodeAddress, "10.10.0.3:4403")
	}
	if !snap.NodeConnected {
		t.Error("expected NodeConnected=true")
	}
	if snap.ActiveClients != 2 {
		t.Errorf("ActiveClients = %d, want 2", snap.ActiveClients)
	}
	if snap.BytesFromNode != 1024 {
		t.Errorf("BytesFromNode = %d, want 1024", snap.BytesFromNode)
	}
	if snap.FramesFromNode != 10 {
		t.Errorf("FramesFromNode = %d, want 10", snap.FramesFromNode)
	}
}

// ---------------------------------------------------------------------------
// /api/clients tests
// ---------------------------------------------------------------------------

func TestAPIClients_Empty(t *testing.T) {
	s := newTestServer(t, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/clients", nil)
	rec := httptest.NewRecorder()

	s.handleAPIClients(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var result map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("failed to decode JSON: %v", err)
	}

	count, ok := result["count"].(float64)
	if !ok || count != 0 {
		t.Errorf("count = %v, want 0", result["count"])
	}
}

func TestAPIClients_WithClients(t *testing.T) {
	clients := []string{"10.10.0.13:54321", "10.10.0.14:12345"}
	s := newTestServer(t, clients)

	req := httptest.NewRequest(http.MethodGet, "/api/clients", nil)
	rec := httptest.NewRecorder()

	s.handleAPIClients(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	ct := rec.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("Content-Type = %q, want %q", ct, "application/json")
	}

	var result map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("failed to decode JSON: %v", err)
	}

	count, ok := result["count"].(float64)
	if !ok || count != 2 {
		t.Errorf("count = %v, want 2", result["count"])
	}

	clientsRaw, ok := result["clients"].([]any)
	if !ok || len(clientsRaw) != 2 {
		t.Errorf("clients = %v, want 2 items", result["clients"])
	}
}

// ---------------------------------------------------------------------------
// / (dashboard) tests
// ---------------------------------------------------------------------------

func TestDashboard_ReturnsHTML(t *testing.T) {
	s := newTestServer(t, nil)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()

	s.handleDashboard(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	ct := rec.Header().Get("Content-Type")
	if ct != "text/html; charset=utf-8" {
		t.Errorf("Content-Type = %q, want %q", ct, "text/html; charset=utf-8")
	}

	body := rec.Body.String()
	if !strings.Contains(body, "<html") && !strings.Contains(body, "<!DOCTYPE") && !strings.Contains(body, "<!doctype") {
		t.Error("dashboard response does not contain HTML")
	}
}

func TestDashboard_NotFoundForOtherPaths(t *testing.T) {
	s := newTestServer(t, nil)

	req := httptest.NewRequest(http.MethodGet, "/nonexistent", nil)
	rec := httptest.NewRecorder()

	s.handleDashboard(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

// ---------------------------------------------------------------------------
// /api/events (SSE) tests
// ---------------------------------------------------------------------------

func TestSSE_InitialSnapshot(t *testing.T) {
	m := metrics.New(10, 300)
	m.NodeAddress = "10.10.0.3:4403"
	m.NodeConnected.Store(true)
	clients := []string{"10.10.0.13:54321"}
	s := newTestServerWithMetrics(t, m, clients)

	// Use a context with timeout to cancel the SSE stream.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	req := httptest.NewRequest(http.MethodGet, "/api/events", nil).WithContext(ctx)
	rec := httptest.NewRecorder()

	// Run handleSSE in a goroutine since it blocks.
	done := make(chan struct{})
	go func() {
		s.handleSSE(rec, req)
		close(done)
	}()

	// Cancel quickly after initial data is sent.
	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handleSSE did not return after context cancel")
	}

	body := rec.Body.String()

	// Should contain initial metrics event.
	if !strings.Contains(body, "event: metrics") {
		t.Error("SSE response missing initial metrics event")
	}

	// Should contain initial clients event.
	if !strings.Contains(body, "event: clients") {
		t.Error("SSE response missing initial clients event")
	}

	// Should have correct headers.
	ct := rec.Header().Get("Content-Type")
	if ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want %q", ct, "text/event-stream")
	}

	cc := rec.Header().Get("Cache-Control")
	if cc != "no-cache" {
		t.Errorf("Cache-Control = %q, want %q", cc, "no-cache")
	}
}

func TestSSE_ReceivesPublishedEvents(t *testing.T) {
	m := metrics.New(10, 300)
	s := newTestServerWithMetrics(t, m, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req := httptest.NewRequest(http.MethodGet, "/api/events", nil).WithContext(ctx)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		s.handleSSE(rec, req)
		close(done)
	}()

	// Wait for subscription to be established.
	time.Sleep(100 * time.Millisecond)

	// Publish a message event.
	m.RecordMessage(metrics.MessageRecord{
		Direction: "from_node",
		Type:      "packet",
		Size:      42,
	})

	// Give time for the event to be written.
	time.Sleep(100 * time.Millisecond)
	cancel()

	<-done

	body := rec.Body.String()

	// Should contain the published message event.
	if !strings.Contains(body, "event: message") {
		t.Error("SSE response missing published message event")
	}
}

// ---------------------------------------------------------------------------
// Run integration test
// ---------------------------------------------------------------------------

func TestRun_StartsAndStops(t *testing.T) {
	m := metrics.New(10, 300)
	s := NewServer("127.0.0.1:0", m, slog.Default(), func() []string { return nil }, nil)

	ctx, cancel := context.WithCancel(context.Background())

	errCh := make(chan error, 1)
	go func() {
		errCh <- s.Run(ctx)
	}()

	// Give the server time to start.
	time.Sleep(100 * time.Millisecond)

	cancel()

	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("Run returned error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after context cancel")
	}
}

func TestRun_ServesRequests(t *testing.T) {
	m := metrics.New(10, 300)
	m.NodeConnected.Store(true)
	m.ConfigCacheFrames.Store(10)

	// Use a test HTTP server via httptest for more reliable testing.
	mux := http.NewServeMux()
	s := NewServer(":0", m, slog.Default(), func() []string { return nil }, nil)

	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.HandleFunc("/readyz", s.handleReadyz)
	mux.HandleFunc("/api/metrics", s.handleAPIMetrics)
	mux.HandleFunc("/api/clients", s.handleAPIClients)
	mux.HandleFunc("/", s.handleDashboard)

	ts := httptest.NewServer(mux)
	defer ts.Close()

	// Test /healthz
	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("/healthz status = %d, want 200", resp.StatusCode)
	}
	if string(body) != "ok\n" {
		t.Errorf("/healthz body = %q, want %q", body, "ok\n")
	}

	// Test /readyz (should be ready)
	resp, err = http.Get(ts.URL + "/readyz")
	if err != nil {
		t.Fatalf("GET /readyz: %v", err)
	}
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("/readyz status = %d, want 200", resp.StatusCode)
	}

	// Test /api/metrics
	resp, err = http.Get(ts.URL + "/api/metrics")
	if err != nil {
		t.Fatalf("GET /api/metrics: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("/api/metrics status = %d, want 200", resp.StatusCode)
	}
	if resp.Header.Get("Content-Type") != "application/json" {
		t.Errorf("/api/metrics Content-Type = %q", resp.Header.Get("Content-Type"))
	}
}

// ---------------------------------------------------------------------------
// Static file serving tests
// ---------------------------------------------------------------------------

func TestStaticFiles_TailwindCSS(t *testing.T) {
	s := newTestServer(t, nil)

	// Build a full mux as Run() does, to test the static file handler.
	mux := s.buildMux()
	ts := httptest.NewServer(mux)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/static/tailwind.css")
	if err != nil {
		t.Fatalf("GET /static/tailwind.css: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	if len(body) == 0 {
		t.Error("tailwind.css body is empty")
	}

	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "css") {
		t.Errorf("Content-Type = %q, want something containing 'css'", ct)
	}
}

func TestStaticFiles_StyleCSS(t *testing.T) {
	s := newTestServer(t, nil)
	mux := s.buildMux()
	ts := httptest.NewServer(mux)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/static/style.css")
	if err != nil {
		t.Fatalf("GET /static/style.css: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

func TestStaticFiles_ChartJS(t *testing.T) {
	s := newTestServer(t, nil)
	mux := s.buildMux()
	ts := httptest.NewServer(mux)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/static/chart.js")
	if err != nil {
		t.Fatalf("GET /static/chart.js: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

func TestStaticFiles_NotFound(t *testing.T) {
	s := newTestServer(t, nil)
	mux := s.buildMux()
	ts := httptest.NewServer(mux)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/static/nonexistent.css")
	if err != nil {
		t.Fatalf("GET /static/nonexistent.css: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != 404 {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// formatBytes template function tests
// ---------------------------------------------------------------------------

func TestFormatBytesTemplateFunc(t *testing.T) {
	m := metrics.New(10, 300)
	s := newTestServerWithMetrics(t, m, nil)

	if s.templates == nil {
		t.Fatal("templates not initialized")
	}

	// Verify the dashboard template renders without error for various byte values.
	// This exercises the formatBytes template function indirectly.
	for _, bytes := range []int64{0, 500, 1024, 1048576, 1073741824} {
		m.BytesFromNode.Store(bytes)
		m.BytesToNode.Store(bytes)

		req := httptest.NewRequest(http.MethodGet, "/", nil)
		rec := httptest.NewRecorder()
		s.handleDashboard(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("dashboard failed for bytes=%d: status=%d", bytes, rec.Code)
		}
	}
}

// TestNewServer verifies the server is created with correct fields.
func TestNewServer(t *testing.T) {
	m := metrics.New(10, 300)
	clients := []string{"10.0.0.1:1234"}
	s := NewServer(":8090", m, slog.Default(), func() []string { return clients }, nil)

	if s.listenAddr != ":8090" {
		t.Errorf("listenAddr = %q, want %q", s.listenAddr, ":8090")
	}
	if s.metrics != m {
		t.Error("metrics not set correctly")
	}
	if s.templates == nil {
		t.Error("templates not initialized")
	}
	if s.clientsFn == nil {
		t.Error("clientsFn not set")
	}

	got := s.clientsFn()
	if len(got) != 1 || got[0] != "10.0.0.1:1234" {
		t.Errorf("clientsFn() = %v, want [10.0.0.1:1234]", got)
	}
}

// ---------------------------------------------------------------------------
// /metrics (Prometheus) tests
// ---------------------------------------------------------------------------

func TestPrometheusMetrics_Endpoint(t *testing.T) {
	m := metrics.New(10, 300)
	m.NodeAddress = "10.10.0.3:4403"
	m.NodeConnected.Store(true)
	m.ActiveClients.Store(2)
	m.BytesFromNode.Store(4096)
	m.RecordMessage(metrics.MessageRecord{Type: "mesh_packet", PortNum: "TEXT_MESSAGE_APP", Size: 50})

	promRegistry := metrics.NewPrometheusRegistry(m)
	promHandler := newPromHTTPHandler(promRegistry)

	s := NewServer(":0", m, slog.Default(), func() []string { return nil }, promHandler)
	mux := s.buildMux()
	ts := httptest.NewServer(mux)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}

	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "text/plain") && !strings.Contains(ct, "text/openmetrics") {
		t.Errorf("Content-Type = %q, want text/plain or text/openmetrics", ct)
	}

	body, _ := io.ReadAll(resp.Body)
	text := string(body)

	mustContain := []string{
		"meshtastic_proxy_node_connected 1",
		"meshtastic_proxy_active_clients 2",
		"meshtastic_proxy_bytes_from_node_total 4096",
		"meshtastic_proxy_info{node_address=\"10.10.0.3:4403\"} 1",
		"meshtastic_proxy_messages_total{port_num=\"TEXT_MESSAGE_APP\"} 1",
		"go_goroutines",
	}

	for _, want := range mustContain {
		if !strings.Contains(text, want) {
			t.Errorf("response missing expected string %q", want)
		}
	}
}

func TestPrometheusMetrics_DisabledWhenNilHandler(t *testing.T) {
	s := newTestServer(t, nil)
	mux := s.buildMux()
	ts := httptest.NewServer(mux)
	defer ts.Close()

	// With nil promHandler, /metrics should 404 (falls through to dashboard handler).
	resp, err := http.Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != 404 {
		t.Errorf("status = %d, want 404 when promHandler is nil", resp.StatusCode)
	}
}
