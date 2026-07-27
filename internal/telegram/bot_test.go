package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jfett/meshtastic-proxy/internal/config"
	"github.com/jfett/meshtastic-proxy/internal/metrics"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestFormatChatMessage tests HTML message formatting.
func TestFormatChatMessage(t *testing.T) {
	tests := []struct {
		name       string
		msg        metrics.ChatMessage
		nodeDir    map[uint32]metrics.NodeEntry
		channelDir map[uint32]string
		wantSub    []string // substrings that must appear
		wantNot    []string // substrings that must NOT appear
	}{
		{
			name: "basic message with name, no resolved channel name",
			msg: metrics.ChatMessage{
				From:     0xf9b0552c,
				FromName: "jfett",
				Channel:  0,
				Text:     "Hello!",
			},
			nodeDir: map[uint32]metrics.NodeEntry{
				0xf9b0552c: {UserID: "!f9b0552c", ShortName: "jfet"},
			},
			wantSub: []string{
				"<b>jfett</b>",
				"<i>Primary</i>",
				"Text: Hello!",
				"<code>!f9b0552c</code>",
			},
			wantNot: []string{"LongFast"},
		},
		{
			name: "channel name from telegram.channel_names config",
			msg: metrics.ChatMessage{
				From:     0xf9b0552c,
				FromName: "jfett",
				Channel:  0,
				Text:     "Hello!",
			},
			nodeDir:    map[uint32]metrics.NodeEntry{},
			channelDir: map[uint32]string{0: "MediumFast"},
			wantSub:    []string{"<i>MediumFast</i>"},
			wantNot:    []string{"LongFast"},
		},
		{
			name: "message with RSSI and SNR",
			msg: metrics.ChatMessage{
				From:     0x12345678,
				FromName: "test",
				Channel:  0,
				Text:     "Hi",
				RxRssi:   -85,
				RxSnr:    6.5,
			},
			nodeDir: map[uint32]metrics.NodeEntry{},
			wantSub: []string{
				"Signal: SNR: 6.5dB / RSSI: -85dBm",
			},
		},
		{
			name: "message via MQTT",
			msg: metrics.ChatMessage{
				From:     0xaabbccdd,
				FromName: "remote",
				Channel:  0,
				Text:     "from far away",
				ViaMqtt:  true,
			},
			nodeDir: map[uint32]metrics.NodeEntry{},
			wantSub: []string{"via MQTT"},
		},
		{
			name: "non-primary channel",
			msg: metrics.ChatMessage{
				From:     0x11111111,
				FromName: "user",
				Channel:  2,
				Text:     "test",
			},
			nodeDir: map[uint32]metrics.NodeEntry{},
			wantSub: []string{"<i>Ch 2</i>"},
			wantNot: []string{"LongFast", "Primary"},
		},
		{
			name: "no sender name — fallback to hex",
			msg: metrics.ChatMessage{
				From:    0xdeadbeef,
				Channel: 0,
				Text:    "anon",
			},
			nodeDir: map[uint32]metrics.NodeEntry{},
			wantSub: []string{"<b>!deadbeef</b>"},
		},
		{
			name: "HTML escaping in text",
			msg: metrics.ChatMessage{
				From:     0x11111111,
				FromName: "user<script>",
				Channel:  0,
				Text:     "<b>bold</b> & \"quoted\"",
			},
			nodeDir: map[uint32]metrics.NodeEntry{},
			wantSub: []string{
				"user&lt;script&gt;",
				"Text: &lt;b&gt;bold&lt;/b&gt; &amp; &#34;quoted&#34;",
			},
			wantNot: []string{"<script>"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := formatChatMessage(tt.msg, tt.nodeDir, tt.channelDir)
			for _, sub := range tt.wantSub {
				if !strings.Contains(result, sub) {
					t.Errorf("result missing %q\ngot: %s", sub, result)
				}
			}
			for _, sub := range tt.wantNot {
				if strings.Contains(result, sub) {
					t.Errorf("result should not contain %q\ngot: %s", sub, result)
				}
			}
		})
	}
}

// TestGetMe tests bot token validation against a mock Telegram API.
func TestGetMe(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/botTEST_TOKEN/getMe" {
			t.Errorf("unexpected path: %s", r.URL.Path)
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(botUser{
			OK: true,
			Result: struct {
				ID        int64  `json:"id"`
				IsBot     bool   `json:"is_bot"`
				FirstName string `json:"first_name"`
				Username  string `json:"username"`
			}{
				ID:        123456,
				IsBot:     true,
				FirstName: "TestBot",
				Username:  "test_bot",
			},
		})
	}))
	defer server.Close()

	// Override API base URL for test
	origURL := apiBaseURL
	apiBaseURL = server.URL
	defer func() { apiBaseURL = origURL }()

	bot := New(config.TelegramConfig{
		Token:  "TEST_TOKEN",
		ChatID: -100123,
	}, metrics.New(10, 10), testLogger())

	user, err := bot.getMe(context.Background())
	if err != nil {
		t.Fatalf("getMe failed: %v", err)
	}
	if user.Result.Username != "test_bot" {
		t.Errorf("username = %q, want %q", user.Result.Username, "test_bot")
	}
}

// TestGetMe_InvalidToken tests that an invalid token returns an error.
func TestGetMe_InvalidToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(botUser{
			OK:          false,
			Description: "Unauthorized",
		})
	}))
	defer server.Close()

	origURL := apiBaseURL
	apiBaseURL = server.URL
	defer func() { apiBaseURL = origURL }()

	bot := New(config.TelegramConfig{
		Token:  "BAD_TOKEN",
		ChatID: -100123,
	}, metrics.New(10, 10), testLogger())

	_, err := bot.getMe(context.Background())
	if err == nil {
		t.Fatal("expected error for invalid token")
	}
	if !strings.Contains(err.Error(), "Unauthorized") {
		t.Errorf("error = %q, want to contain 'Unauthorized'", err.Error())
	}
}

// TestSendMessage tests sending a message to the mock Telegram API.
func TestSendMessage(t *testing.T) {
	var received sendMessageRequest

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/sendMessage") {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Errorf("failed to decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(sendMessageResponse{OK: true})
	}))
	defer server.Close()

	origURL := apiBaseURL
	apiBaseURL = server.URL
	defer func() { apiBaseURL = origURL }()

	bot := New(config.TelegramConfig{
		Token:  "TEST_TOKEN",
		ChatID: -100999,
	}, metrics.New(10, 10), testLogger())

	err := bot.sendMessage(context.Background(), "<b>Hello</b>")
	if err != nil {
		t.Fatalf("sendMessage failed: %v", err)
	}

	if received.ChatID != -100999 {
		t.Errorf("chat_id = %d, want %d", received.ChatID, -100999)
	}
	if received.ParseMode != "HTML" {
		t.Errorf("parse_mode = %q, want %q", received.ParseMode, "HTML")
	}
	if received.Text != "<b>Hello</b>" {
		t.Errorf("text = %q, want %q", received.Text, "<b>Hello</b>")
	}
}

// TestRun_ForwardsMessages tests the full event loop: metrics publish
// chat_message events and the bot forwards both incoming and outgoing to Telegram.
func TestRun_ForwardsMessages(t *testing.T) {
	var mu sync.Mutex
	var sentMessages []sendMessageRequest

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if strings.HasSuffix(r.URL.Path, "/getMe") {
			json.NewEncoder(w).Encode(botUser{
				OK: true,
				Result: struct {
					ID        int64  `json:"id"`
					IsBot     bool   `json:"is_bot"`
					FirstName string `json:"first_name"`
					Username  string `json:"username"`
				}{ID: 1, IsBot: true, Username: "test_bot"},
			})
			return
		}

		if strings.HasSuffix(r.URL.Path, "/sendMessage") {
			var req sendMessageRequest
			json.NewDecoder(r.Body).Decode(&req)
			mu.Lock()
			sentMessages = append(sentMessages, req)
			mu.Unlock()
			json.NewEncoder(w).Encode(sendMessageResponse{OK: true})
			return
		}
	}))
	defer server.Close()

	origURL := apiBaseURL
	apiBaseURL = server.URL
	defer func() { apiBaseURL = origURL }()

	m := metrics.New(10, 10)

	bot := New(config.TelegramConfig{
		Token:  "TEST_TOKEN",
		ChatID: -100123,
	}, m, testLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start bot in background
	errCh := make(chan error, 1)
	go func() {
		errCh <- bot.Run(ctx)
	}()

	// Give the bot time to subscribe
	time.Sleep(100 * time.Millisecond)

	// Publish an incoming chat message
	m.RecordChatMessage(metrics.ChatMessage{
		From:      0xf9b0552c,
		FromName:  "jfett",
		Channel:   0,
		Text:      "Hello from mesh!",
		Direction: "incoming",
	})

	// Publish an outgoing message (should NOT be forwarded — only incoming
	// messages are bridged to Telegram to avoid duplicates and echo loops).
	m.RecordChatMessage(metrics.ChatMessage{
		From:      0xf9b0552c,
		FromName:  "jfett",
		Channel:   0,
		Text:      "Outgoing message",
		Direction: "outgoing",
	})

	// Wait for messages to be processed
	time.Sleep(200 * time.Millisecond)
	cancel()

	// Wait for Run to finish
	if err := <-errCh; err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	if len(sentMessages) != 1 {
		t.Fatalf("expected 1 sent message (incoming only), got %d", len(sentMessages))
	}
	if !strings.Contains(sentMessages[0].Text, "Hello from mesh!") {
		t.Errorf("message text = %q, want to contain 'Hello from mesh!'", sentMessages[0].Text)
	}
}

// TestRun_ChannelFiltering tests that only messages from allowed channels are forwarded.
func TestRun_ChannelFiltering(t *testing.T) {
	tests := []struct {
		name      string
		channels  []int    // config channels (empty = default [0])
		msgChans  []uint32 // channels to send messages on
		wantCount int      // expected forwarded messages
	}{
		{
			name:      "default channels — only primary",
			channels:  nil,
			msgChans:  []uint32{0, 1, 2},
			wantCount: 1, // only channel 0
		},
		{
			name:      "explicit single channel",
			channels:  []int{1},
			msgChans:  []uint32{0, 1, 2},
			wantCount: 1, // only channel 1
		},
		{
			name:      "multiple channels",
			channels:  []int{0, 2},
			msgChans:  []uint32{0, 1, 2, 3},
			wantCount: 2, // channels 0 and 2
		},
		{
			name:      "no matching channels",
			channels:  []int{5},
			msgChans:  []uint32{0, 1, 2},
			wantCount: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var mu sync.Mutex
			var sentMessages []sendMessageRequest

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")

				if strings.HasSuffix(r.URL.Path, "/getMe") {
					json.NewEncoder(w).Encode(botUser{
						OK: true,
						Result: struct {
							ID        int64  `json:"id"`
							IsBot     bool   `json:"is_bot"`
							FirstName string `json:"first_name"`
							Username  string `json:"username"`
						}{ID: 1, IsBot: true, Username: "test_bot"},
					})
					return
				}

				if strings.HasSuffix(r.URL.Path, "/sendMessage") {
					var req sendMessageRequest
					json.NewDecoder(r.Body).Decode(&req)
					mu.Lock()
					sentMessages = append(sentMessages, req)
					mu.Unlock()
					json.NewEncoder(w).Encode(sendMessageResponse{OK: true})
					return
				}
			}))
			defer server.Close()

			origURL := apiBaseURL
			apiBaseURL = server.URL
			defer func() { apiBaseURL = origURL }()

			m := metrics.New(10, 10)

			bot := New(config.TelegramConfig{
				Token:    "TEST_TOKEN",
				ChatID:   -100123,
				Channels: tt.channels,
			}, m, testLogger())

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			errCh := make(chan error, 1)
			go func() {
				errCh <- bot.Run(ctx)
			}()

			// Give the bot time to subscribe
			time.Sleep(100 * time.Millisecond)

			// Publish messages on different channels
			for _, ch := range tt.msgChans {
				m.RecordChatMessage(metrics.ChatMessage{
					From:      0x12345678,
					FromName:  "test",
					Channel:   ch,
					Text:      fmt.Sprintf("msg on ch %d", ch),
					Direction: "incoming",
				})
			}

			// Wait for messages to be processed
			time.Sleep(200 * time.Millisecond)
			cancel()

			if err := <-errCh; err != nil {
				t.Fatalf("Run returned error: %v", err)
			}

			mu.Lock()
			defer mu.Unlock()

			if len(sentMessages) != tt.wantCount {
				t.Errorf("sent %d messages, want %d", len(sentMessages), tt.wantCount)
			}
		})
	}
}

// TestChannelList tests the channelList helper for logging.
func TestChannelList(t *testing.T) {
	tests := []struct {
		name     string
		channels []int
		want     []uint32
	}{
		{"default", nil, []uint32{0}},
		{"single", []int{2}, []uint32{2}},
		{"multiple sorted", []int{3, 1, 0}, []uint32{0, 1, 3}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bot := New(config.TelegramConfig{
				Token:    "T",
				ChatID:   1,
				Channels: tt.channels,
			}, metrics.New(10, 10), testLogger())

			got := bot.channelList()
			if len(got) != len(tt.want) {
				t.Fatalf("channelList() = %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("channelList()[%d] = %d, want %d", i, got[i], tt.want[i])
				}
			}
		})
	}
}

// TestChannelName tests channel name resolution.
func TestChannelName(t *testing.T) {
	// Nothing resolved: channel 0 falls back to "Primary" (not a guessed
	// preset like "LongFast" — the mesh may run MediumFast, ShortFast, etc.),
	// others fall back to "Ch N".
	if got := channelName(0, nil); got != "Primary" {
		t.Errorf("channelName(0, nil) = %q, want %q", got, "Primary")
	}
	if got := channelName(1, nil); got != "Ch 1" {
		t.Errorf("channelName(1, nil) = %q, want %q", got, "Ch 1")
	}
	if got := channelName(3, nil); got != "Ch 3" {
		t.Errorf("channelName(3, nil) = %q, want %q", got, "Ch 3")
	}

	// A configured name (telegram.channel_names) takes priority.
	dir := map[uint32]string{0: "MediumFast", 2: "Admin"}
	if got := channelName(0, dir); got != "MediumFast" {
		t.Errorf("channelName(0, dir) = %q, want %q", got, "MediumFast")
	}
	if got := channelName(2, dir); got != "Admin" {
		t.Errorf("channelName(2, dir) = %q, want %q", got, "Admin")
	}
	if got := channelName(1, dir); got != "Ch 1" {
		t.Errorf("channelName(1, dir) = %q, want %q (no entry for channel 1)", got, "Ch 1")
	}
}

// TestBot_channelNames tests that telegram.channel_names from config is
// parsed into the bot's channel index -> display name map used for
// formatting (invalid/out-of-range keys are ignored).
func TestBot_channelNames(t *testing.T) {
	b := New(config.TelegramConfig{
		Token:  "x",
		ChatID: 1,
		ChannelNames: map[string]string{
			"0":   "MediumFast",
			"1":   "Admin",
			"":    "ignored (not a number)",
			"abc": "ignored (not a number)",
			"-1":  "ignored (out of range)",
			"8":   "ignored (out of range)",
			"2":   "", // ignored (empty name)
		},
	}, metrics.New(10, 10), testLogger())

	if got := b.channelNames[0]; got != "MediumFast" {
		t.Errorf("channelNames[0] = %q, want %q", got, "MediumFast")
	}
	if got := b.channelNames[1]; got != "Admin" {
		t.Errorf("channelNames[1] = %q, want %q", got, "Admin")
	}
	if len(b.channelNames) != 2 {
		t.Errorf("channelNames = %v, want only entries for 0 and 1", b.channelNames)
	}
}
