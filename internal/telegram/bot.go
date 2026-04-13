// Package telegram provides a one-way bridge from the Meshtastic mesh network
// to a Telegram channel. It subscribes to chat message events via the metrics
// pub/sub system and forwards them to a Telegram channel using the Bot API.
//
// The integration is optional: it activates only when a bot token is configured
// (via TOML config or MESHTASTIC_TELEGRAM_TOKEN environment variable).
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/jfett/meshtastic-proxy/internal/config"
	"github.com/jfett/meshtastic-proxy/internal/metrics"
)

// apiBaseURL is the Telegram Bot API base URL. Variable for testing.
var apiBaseURL = "https://api.telegram.org"

// Bot forwards mesh chat messages to a Telegram channel.
type Bot struct {
	token    string
	chatID   int64
	channels map[uint32]bool // allowed mesh channel indices
	metrics  *metrics.Metrics
	logger   *slog.Logger
	client   *http.Client
}

// New creates a new Telegram bot.
func New(cfg config.TelegramConfig, m *metrics.Metrics, logger *slog.Logger) *Bot {
	// Build allowed channels map; default to primary channel [0] if not configured.
	channels := make(map[uint32]bool)
	if len(cfg.Channels) == 0 {
		channels[0] = true
	} else {
		for _, ch := range cfg.Channels {
			if ch >= 0 && ch <= 7 {
				channels[uint32(ch)] = true
			}
		}
	}

	return &Bot{
		token:    cfg.Token,
		chatID:   cfg.ChatID,
		channels: channels,
		metrics:  m,
		logger:   logger,
		client: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

// botUser represents the response from Telegram's getMe API.
type botUser struct {
	OK     bool `json:"ok"`
	Result struct {
		ID        int64  `json:"id"`
		IsBot     bool   `json:"is_bot"`
		FirstName string `json:"first_name"`
		Username  string `json:"username"`
	} `json:"result"`
	Description string `json:"description"`
}

// sendMessageRequest is the payload for Telegram's sendMessage API.
type sendMessageRequest struct {
	ChatID    int64  `json:"chat_id"`
	Text      string `json:"text"`
	ParseMode string `json:"parse_mode"`
}

// sendMessageResponse is the response from Telegram's sendMessage API.
type sendMessageResponse struct {
	OK          bool   `json:"ok"`
	Description string `json:"description"`
}

// Run starts the bot event loop. It validates the token, then subscribes to
// chat message events and forwards them to Telegram. Blocks until ctx is canceled.
func (b *Bot) Run(ctx context.Context) error {
	// Validate token by calling getMe
	user, err := b.getMe(ctx)
	if err != nil {
		return fmt.Errorf("telegram token validation failed: %w", err)
	}
	b.logger.Info("telegram bot authenticated",
		"bot_id", user.Result.ID,
		"bot_username", user.Result.Username,
		"chat_id", b.chatID,
		"channels", b.channelList(),
	)

	// Subscribe to events
	ch := b.metrics.Subscribe()
	defer b.metrics.Unsubscribe(ch)

	b.logger.Info("telegram bridge started, listening for chat messages")

	for {
		select {
		case <-ctx.Done():
			b.logger.Info("telegram bridge stopped")
			return nil
		case evt, ok := <-ch:
			if !ok {
				return nil
			}
			if evt.Type != "chat_message" {
				continue
			}
			msg, ok := evt.Data.(metrics.ChatMessage)
			if !ok {
				continue
			}
			// Only forward incoming messages from the mesh — skip outgoing
			// messages sent via the web chat or other clients to avoid
			// duplicates and echo loops.
			if msg.Direction != "incoming" {
				continue
			}
			// Only forward messages from allowed channels
			if !b.channels[msg.Channel] {
				continue
			}
			b.handleChatMessage(ctx, msg)
		}
	}
}

// handleChatMessage formats and sends a single chat message to Telegram.
func (b *Bot) handleChatMessage(ctx context.Context, msg metrics.ChatMessage) {
	nodeDir := b.metrics.NodeDirectory()
	text := formatChatMessage(msg, nodeDir)

	if err := b.sendMessage(ctx, text); err != nil {
		b.logger.Error("failed to send telegram message",
			"error", err,
			"from", msg.FromName,
			"text", msg.Text,
		)
		return
	}

	b.logger.Debug("forwarded message to telegram",
		"from", msg.FromName,
		"channel", msg.Channel,
		"text", msg.Text,
	)
}

// getMe calls the Telegram Bot API getMe endpoint to validate the token.
func (b *Bot) getMe(ctx context.Context) (*botUser, error) {
	url := fmt.Sprintf("%s/bot%s/getMe", apiBaseURL, b.token)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}

	resp, err := b.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("calling getMe: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	var user botUser
	if err := json.Unmarshal(body, &user); err != nil {
		return nil, fmt.Errorf("parsing response: %w", err)
	}

	if !user.OK {
		return nil, fmt.Errorf("API error: %s", user.Description)
	}

	return &user, nil
}

// sendMessage sends an HTML-formatted message to the configured Telegram chat.
func (b *Bot) sendMessage(ctx context.Context, text string) error {
	url := fmt.Sprintf("%s/bot%s/sendMessage", apiBaseURL, b.token)

	payload := sendMessageRequest{
		ChatID:    b.chatID,
		Text:      text,
		ParseMode: "HTML",
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshaling request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := b.client.Do(req)
	if err != nil {
		return fmt.Errorf("calling sendMessage: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("reading response: %w", err)
	}

	var result sendMessageResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return fmt.Errorf("parsing response: %w", err)
	}

	if !result.OK {
		return fmt.Errorf("API error: %s", result.Description)
	}

	return nil
}

// channelList returns sorted channel indices for logging.
func (b *Bot) channelList() []uint32 {
	list := make([]uint32, 0, len(b.channels))
	for ch := range b.channels {
		list = append(list, ch)
	}
	// Simple insertion sort (max 8 elements).
	for i := 1; i < len(list); i++ {
		for j := i; j > 0 && list[j] < list[j-1]; j-- {
			list[j], list[j-1] = list[j-1], list[j]
		}
	}
	return list
}
