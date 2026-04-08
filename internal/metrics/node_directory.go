package metrics

import "time"

// SetNodeDirectory replaces the node directory with the given map and rebuilds
// the relay lookup index. It also publishes a "node_directory" SSE event.
func (m *Metrics) SetNodeDirectory(dir map[uint32]NodeEntry) {
	relay := make(map[uint8][]uint32, len(dir))
	for num := range dir {
		key := uint8(num & 0xFF)
		relay[key] = append(relay[key], num)
	}

	m.nodeDirMu.Lock()
	// Preserve runtime-only fields from existing entries. SeenRealtime and
	// IsFavorite are set by real-time updates / user actions and must survive
	// a full directory replacement (which happens on node reconnect).
	for num, newEntry := range dir {
		if old, ok := m.nodeDir[num]; ok {
			if old.SeenRealtime {
				newEntry.SeenRealtime = true
			}
			if old.IsFavorite {
				newEntry.IsFavorite = true
			}
			dir[num] = newEntry
		}
	}
	m.nodeDir = dir
	m.relayDir = relay
	m.nodeDirMu.Unlock()

	m.publish(Event{Type: "node_directory", Data: dir})
}

// NodeDirectory returns a copy of the current node directory.
func (m *Metrics) NodeDirectory() map[uint32]NodeEntry {
	m.nodeDirMu.RLock()
	defer m.nodeDirMu.RUnlock()

	result := make(map[uint32]NodeEntry, len(m.nodeDir))
	for k, v := range m.nodeDir {
		result[k] = v
	}
	return result
}

// ResolveRelay returns the node entries whose last byte matches the given
// relay_node value. Returns nil if no matches are found.
func (m *Metrics) ResolveRelay(relayByte uint8) []NodeEntry {
	m.nodeDirMu.RLock()
	defer m.nodeDirMu.RUnlock()

	nums := m.relayDir[relayByte]
	if len(nums) == 0 {
		return nil
	}
	result := make([]NodeEntry, 0, len(nums))
	for _, num := range nums {
		if entry, ok := m.nodeDir[num]; ok {
			result = append(result, entry)
		}
	}
	return result
}

// updateNodeEntry is a helper that locks the node directory, looks up (or creates)
// an entry for the given node, marks it as seen in real time, and calls the
// provided update function to mutate the entry. It returns the updated entry.
func (m *Metrics) updateNodeEntry(nodeNum uint32, fn func(entry *NodeEntry)) NodeEntry {
	m.nodeDirMu.Lock()
	entry := m.nodeDir[nodeNum]
	entry.SeenRealtime = true
	entry.LastHeard = nowUnix32()
	fn(&entry)
	m.nodeDir[nodeNum] = entry
	m.nodeDirMu.Unlock()
	return entry
}

// UpdateNodePosition updates the position of a single node in the directory
// and publishes an SSE event so the map updates in real time.
func (m *Metrics) UpdateNodePosition(update PositionUpdate) {
	entry := m.updateNodeEntry(update.NodeNum, func(e *NodeEntry) {
		e.Latitude = update.Latitude
		e.Longitude = update.Longitude
		e.Altitude = update.Altitude
		if update.GroundSpeed > 0 {
			e.GroundSpeed = update.GroundSpeed
		}
		if update.GroundTrack > 0 {
			e.GroundTrack = update.GroundTrack
		}
		if update.SatsInView > 0 {
			e.SatsInView = update.SatsInView
		}
		if update.PositionTime > 0 {
			e.PositionTime = update.PositionTime
		}
	})

	// Fill names for SSE event from stored entry.
	update.ShortName = entry.ShortName
	update.LongName = entry.LongName
	m.publish(Event{Type: "node_position_update", Data: update})
}

// UpdateNodeTelemetry updates telemetry data for a single node in the directory
// and publishes an SSE event so the dashboard updates in real time.
func (m *Metrics) UpdateNodeTelemetry(update TelemetryUpdate) {
	entry := m.updateNodeEntry(update.NodeNum, func(e *NodeEntry) {
		// Device metrics
		if update.BatteryLevel > 0 {
			e.BatteryLevel = update.BatteryLevel
		}
		if update.Voltage > 0 {
			e.Voltage = update.Voltage
		}
		if update.ChannelUtilization > 0 {
			e.ChannelUtilization = update.ChannelUtilization
		}
		if update.AirUtilTx > 0 {
			e.AirUtilTx = update.AirUtilTx
		}
		if update.UptimeSeconds > 0 {
			e.UptimeSeconds = update.UptimeSeconds
		}

		// Environment metrics
		if update.Temperature != 0 {
			e.Temperature = update.Temperature
		}
		if update.RelativeHumidity != 0 {
			e.RelativeHumidity = update.RelativeHumidity
		}
		if update.BarometricPressure != 0 {
			e.BarometricPressure = update.BarometricPressure
		}
	})

	// Fill names for SSE event from stored entry.
	update.ShortName = entry.ShortName
	update.LongName = entry.LongName
	m.publish(Event{Type: "node_telemetry_update", Data: update})
}

// UpdateNodeSignal updates the signal quality (RSSI/SNR) for a single node
// in the directory and publishes an SSE event so the heatmap updates in real time.
// Called when a MeshPacket is received with non-zero RxRssi from a known node.
func (m *Metrics) UpdateNodeSignal(update SignalUpdate) {
	entry := m.updateNodeEntry(update.NodeNum, func(e *NodeEntry) {
		if update.RxRssi != 0 {
			e.RxRssi = update.RxRssi
		}
		if update.RxSnr != 0 {
			e.RxSnr = update.RxSnr
		}
	})

	// Fill names for SSE event from stored entry.
	update.ShortName = entry.ShortName
	update.LongName = entry.LongName
	m.publish(Event{Type: "node_signal_update", Data: update})
}

// UpsertNode updates the identity fields of a node in the directory (or creates
// a new entry) and publishes a "node_update" SSE event. This is called when a
// NODEINFO_APP packet is received after the initial config cache, allowing the
// dashboard to discover new nodes joining the mesh in real time.
func (m *Metrics) UpsertNode(update NodeInfoUpdate) {
	m.nodeDirMu.Lock()
	entry, exists := m.nodeDir[update.NodeNum]
	update.IsNew = !exists
	entry.SeenRealtime = true
	entry.LastHeard = nowUnix32()

	// Update identity fields
	if update.ShortName != "" {
		entry.ShortName = update.ShortName
	}
	if update.LongName != "" {
		entry.LongName = update.LongName
	}
	if update.UserID != "" {
		entry.UserID = update.UserID
	}
	if update.HwModel != "" {
		entry.HwModel = update.HwModel
	}
	if update.Role != "" {
		entry.Role = update.Role
	}
	entry.IsLicensed = update.IsLicensed

	m.nodeDir[update.NodeNum] = entry

	// Update relay index if this is a new node.
	if update.IsNew {
		key := uint8(update.NodeNum & 0xFF)
		m.relayDir[key] = append(m.relayDir[key], update.NodeNum)
	}
	m.nodeDirMu.Unlock()

	m.publish(Event{Type: "node_update", Data: update})
}

// SetFavorite toggles the IsFavorite flag for a node in the directory.
// Returns false if the node is not found.
func (m *Metrics) SetFavorite(nodeNum uint32, fav bool) bool {
	m.nodeDirMu.Lock()
	entry, ok := m.nodeDir[nodeNum]
	if !ok {
		m.nodeDirMu.Unlock()
		return false
	}
	entry.IsFavorite = fav
	m.nodeDir[nodeNum] = entry
	m.nodeDirMu.Unlock()

	// Publish update so all SSE clients see the change.
	m.publish(Event{Type: "node_favorite", Data: map[string]any{
		"node_num":    nodeNum,
		"is_favorite": fav,
	}})
	return true
}

// nowUnix32 returns the current Unix timestamp as uint32.
// Safe until year 2106; gosec G115 flags the int64→uint32 narrowing,
// but Unix timestamps are always positive and fit in uint32 for the
// foreseeable future.
func nowUnix32() uint32 {
	return uint32(time.Now().Unix()) //nolint:gosec // G115: unix timestamp fits uint32 until 2106
}
