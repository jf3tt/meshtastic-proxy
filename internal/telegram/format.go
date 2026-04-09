package telegram

import (
	"fmt"
	"html"
	"strings"

	"github.com/jfett/meshtastic-proxy/internal/metrics"
)

// channelName returns a human-readable channel name from the channel index.
// Channel 0 is always "LongFast" (primary), others are numbered.
func channelName(ch uint32) string {
	if ch == 0 {
		return "LongFast"
	}
	return fmt.Sprintf("Ch %d", ch)
}

// formatChatMessage formats a ChatMessage as Telegram HTML.
//
// Example output:
//
//	<b>jfett (👽)</b> → <i>LongFast</i>
//	Hello from mesh!
//
//	<code>!f9b0552c</code> · -85 dBm · SNR 6.5 dB
func formatChatMessage(msg metrics.ChatMessage, nodeDir map[uint32]metrics.NodeEntry) string {
	var b strings.Builder

	// Sender name
	senderName := msg.FromName
	if senderName == "" {
		senderName = fmt.Sprintf("!%08x", msg.From)
	}
	fmt.Fprintf(&b, "<b>%s</b>", html.EscapeString(senderName))

	// Direction arrow and channel
	fmt.Fprintf(&b, " → <i>%s</i>\n", html.EscapeString(channelName(msg.Channel)))

	// Message text
	b.WriteString(html.EscapeString(msg.Text))

	// Metadata line
	var meta []string

	// Node ID from directory
	if entry, ok := nodeDir[msg.From]; ok && entry.UserID != "" {
		meta = append(meta, fmt.Sprintf("<code>%s</code>", html.EscapeString(entry.UserID)))
	} else {
		meta = append(meta, fmt.Sprintf("<code>!%08x</code>", msg.From))
	}

	// RSSI
	if msg.RxRssi != 0 {
		meta = append(meta, fmt.Sprintf("%d dBm", msg.RxRssi))
	}

	// SNR
	if msg.RxSnr != 0 {
		meta = append(meta, fmt.Sprintf("SNR %.1f dB", msg.RxSnr))
	}

	// Via MQTT
	if msg.ViaMqtt {
		meta = append(meta, "via MQTT")
	}

	if len(meta) > 0 {
		b.WriteString("\n\n")
		b.WriteString(strings.Join(meta, " · "))
	}

	return b.String()
}
