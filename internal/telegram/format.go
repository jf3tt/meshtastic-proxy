package telegram

import (
	"fmt"
	"html"
	"strings"

	"github.com/jfett/meshtastic-proxy/internal/metrics"
)

// channelName returns a human-readable channel name from the channel index.
// channelDir holds names configured via telegram.channel_names (see
// config.TelegramConfig.ChannelNames). We don't know the mesh's actual
// channel/preset name (e.g. LongFast vs MediumFast) without that config, so
// an unconfigured channel falls back to a generic label instead of guessing.
func channelName(ch uint32, channelDir map[uint32]string) string {
	if name, ok := channelDir[ch]; ok && name != "" {
		return name
	}
	if ch == 0 {
		return "Primary"
	}
	return fmt.Sprintf("Ch %d", ch)
}

// formatChatMessage formats a ChatMessage as Telegram HTML.
//
// Example output:
//
//	<b>jfett</b> (<code>!f9b0552c</code>) - <i>MediumFast</i>
//	Signal: SNR: 6.5dB / RSSI: -85dBm
//
//	Text: Hello from mesh!
func formatChatMessage(msg metrics.ChatMessage, nodeDir map[uint32]metrics.NodeEntry, channelDir map[uint32]string) string {
	var b strings.Builder

	// Sender name
	senderName := msg.FromName
	if senderName == "" {
		senderName = fmt.Sprintf("!%08x", msg.From)
	}

	// Node ID from directory, falling back to the raw hex ID.
	nodeID := fmt.Sprintf("!%08x", msg.From)
	if entry, ok := nodeDir[msg.From]; ok && entry.UserID != "" {
		nodeID = entry.UserID
	}

	// Header: name (node id) - channel
	fmt.Fprintf(&b, "<b>%s</b> (<code>%s</code>) - <i>%s</i>\n",
		html.EscapeString(senderName),
		html.EscapeString(nodeID),
		html.EscapeString(channelName(msg.Channel, channelDir)),
	)

	// Signal line: SNR / RSSI, only when reported.
	var signal []string
	if msg.RxSnr != 0 {
		signal = append(signal, fmt.Sprintf("SNR: %.1fdB", msg.RxSnr))
	}
	if msg.RxRssi != 0 {
		signal = append(signal, fmt.Sprintf("RSSI: %ddBm", msg.RxRssi))
	}
	if msg.ViaMqtt {
		signal = append(signal, "via MQTT")
	}
	if len(signal) > 0 {
		fmt.Fprintf(&b, "Signal: %s\n", strings.Join(signal, " / "))
	}

	// Message text, on its own block.
	fmt.Fprintf(&b, "\nText: %s", html.EscapeString(msg.Text))

	return b.String()
}
