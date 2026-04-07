package metrics

// sseBufferSize is the channel buffer size for SSE subscriber channels.
const sseBufferSize = 64

// Subscribe returns a channel that receives SSE events.
func (m *Metrics) Subscribe() chan Event {
	ch := make(chan Event, sseBufferSize)
	m.subMu.Lock()
	m.subscribers[ch] = struct{}{}
	m.subMu.Unlock()
	return ch
}

// Unsubscribe removes a subscriber channel.
func (m *Metrics) Unsubscribe(ch chan Event) {
	m.subMu.Lock()
	delete(m.subscribers, ch)
	m.subMu.Unlock()
	close(ch)
}

// publish sends an event to all SSE subscribers.
// Slow subscribers that can't keep up will have events dropped.
func (m *Metrics) publish(evt Event) {
	m.subMu.RLock()
	defer m.subMu.RUnlock()

	for ch := range m.subscribers {
		select {
		case ch <- evt:
		default:
			// Slow subscriber, drop event
		}
	}
}

// PublishClients sends a "clients" event to all SSE subscribers with the
// current list of connected client addresses.
func (m *Metrics) PublishClients(clients []string) {
	m.publish(Event{Type: "clients", Data: clients})
}
