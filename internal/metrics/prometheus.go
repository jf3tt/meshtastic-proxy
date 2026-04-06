package metrics

import (
	"fmt"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

const namespace = "meshtastic_proxy"

// prometheusCollector implements prometheus.Collector by reading live values
// from the existing *Metrics atomic counters on every scrape. This avoids
// duplicating state — Prometheus metrics are thin wrappers over the same data.
type prometheusCollector struct {
	m *Metrics

	// Descriptors (registered once, used for Describe + Collect).
	infoDesc                  *prometheus.Desc
	uptimeDesc                *prometheus.Desc
	nodeConnectedDesc         *prometheus.Desc
	activeClientsDesc         *prometheus.Desc
	bytesFromNodeDesc         *prometheus.Desc
	bytesToNodeDesc           *prometheus.Desc
	framesFromNodeDesc        *prometheus.Desc
	framesToNodeDesc          *prometheus.Desc
	nodeReconnectsDesc        *prometheus.Desc
	nodeConnectionErrorsDesc  *prometheus.Desc
	configCacheFramesDesc     *prometheus.Desc
	configCacheAgeSecondsDesc *prometheus.Desc
	configReplaysTotalDesc    *prometheus.Desc
	messagesTotalDesc         *prometheus.Desc
	meshNodesDesc             *prometheus.Desc
	nodeRssiDesc              *prometheus.Desc
	nodeSnrDesc               *prometheus.Desc
}

func newPrometheusCollector(m *Metrics) *prometheusCollector {
	return &prometheusCollector{
		m: m,

		infoDesc: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "", "info"),
			"Proxy instance information.",
			[]string{"node_address"}, nil,
		),
		uptimeDesc: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "", "uptime_seconds"),
			"Time since the proxy started, in seconds.",
			nil, nil,
		),
		nodeConnectedDesc: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "", "node_connected"),
			"Whether the Meshtastic node TCP connection is up (1) or down (0).",
			nil, nil,
		),
		activeClientsDesc: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "", "active_clients"),
			"Number of currently connected proxy clients.",
			nil, nil,
		),
		bytesFromNodeDesc: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "", "bytes_from_node_total"),
			"Total bytes received from the Meshtastic node.",
			nil, nil,
		),
		bytesToNodeDesc: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "", "bytes_to_node_total"),
			"Total bytes sent to the Meshtastic node.",
			nil, nil,
		),
		framesFromNodeDesc: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "", "frames_from_node_total"),
			"Total Meshtastic protocol frames received from the node.",
			nil, nil,
		),
		framesToNodeDesc: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "", "frames_to_node_total"),
			"Total Meshtastic protocol frames sent to the node.",
			nil, nil,
		),
		nodeReconnectsDesc: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "", "node_reconnects_total"),
			"Total number of reconnections to the Meshtastic node.",
			nil, nil,
		),
		nodeConnectionErrorsDesc: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "", "node_connection_errors_total"),
			"Total connection errors when connecting to the Meshtastic node.",
			nil, nil,
		),
		configCacheFramesDesc: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "", "config_cache_frames"),
			"Number of frames currently held in the config cache.",
			nil, nil,
		),
		configCacheAgeSecondsDesc: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "", "config_cache_age_seconds"),
			"Seconds since the config cache was last populated.",
			nil, nil,
		),
		configReplaysTotalDesc: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "", "config_replays_total"),
			"Total config cache replays by type.",
			[]string{"type"}, nil,
		),
		messagesTotalDesc: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "", "messages_total"),
			"Total proxied Meshtastic messages by application port type.",
			[]string{"port_num"}, nil,
		),
		meshNodesDesc: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "", "mesh_nodes"),
			"Number of known nodes in the mesh network.",
			nil, nil,
		),
		nodeRssiDesc: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "node", "rssi_dbm"),
			"Last received RSSI in dBm for a mesh node.",
			[]string{"node_num", "short_name"}, nil,
		),
		nodeSnrDesc: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "node", "snr_db"),
			"Last received SNR in dB for a mesh node.",
			[]string{"node_num", "short_name"}, nil,
		),
	}
}

// Describe sends all descriptor definitions to the channel.
func (c *prometheusCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.infoDesc
	ch <- c.uptimeDesc
	ch <- c.nodeConnectedDesc
	ch <- c.activeClientsDesc
	ch <- c.bytesFromNodeDesc
	ch <- c.bytesToNodeDesc
	ch <- c.framesFromNodeDesc
	ch <- c.framesToNodeDesc
	ch <- c.nodeReconnectsDesc
	ch <- c.nodeConnectionErrorsDesc
	ch <- c.configCacheFramesDesc
	ch <- c.configCacheAgeSecondsDesc
	ch <- c.configReplaysTotalDesc
	ch <- c.messagesTotalDesc
	ch <- c.meshNodesDesc
	ch <- c.nodeRssiDesc
	ch <- c.nodeSnrDesc
}

// Collect reads current values from the Metrics struct and sends them
// as prometheus.Metric values. Called on every Prometheus scrape.
func (c *prometheusCollector) Collect(ch chan<- prometheus.Metric) {
	m := c.m

	// Info metric (always 1, carries labels).
	ch <- prometheus.MustNewConstMetric(
		c.infoDesc, prometheus.GaugeValue, 1, m.NodeAddress,
	)

	// Uptime.
	ch <- prometheus.MustNewConstMetric(
		c.uptimeDesc, prometheus.GaugeValue, m.Uptime().Seconds(),
	)

	// Node connection state.
	nodeUp := 0.0
	if m.NodeConnected.Load() {
		nodeUp = 1.0
	}
	ch <- prometheus.MustNewConstMetric(
		c.nodeConnectedDesc, prometheus.GaugeValue, nodeUp,
	)

	// Active clients.
	ch <- prometheus.MustNewConstMetric(
		c.activeClientsDesc, prometheus.GaugeValue, float64(m.ActiveClients.Load()),
	)

	// Traffic counters (monotonic → CounterValue).
	ch <- prometheus.MustNewConstMetric(
		c.bytesFromNodeDesc, prometheus.CounterValue, float64(m.BytesFromNode.Load()),
	)
	ch <- prometheus.MustNewConstMetric(
		c.bytesToNodeDesc, prometheus.CounterValue, float64(m.BytesToNode.Load()),
	)
	ch <- prometheus.MustNewConstMetric(
		c.framesFromNodeDesc, prometheus.CounterValue, float64(m.FramesFromNode.Load()),
	)
	ch <- prometheus.MustNewConstMetric(
		c.framesToNodeDesc, prometheus.CounterValue, float64(m.FramesToNode.Load()),
	)

	// Node connection reliability.
	ch <- prometheus.MustNewConstMetric(
		c.nodeReconnectsDesc, prometheus.CounterValue, float64(m.NodeReconnects.Load()),
	)
	ch <- prometheus.MustNewConstMetric(
		c.nodeConnectionErrorsDesc, prometheus.CounterValue, float64(m.NodeConnectionErrors.Load()),
	)

	// Config cache.
	ch <- prometheus.MustNewConstMetric(
		c.configCacheFramesDesc, prometheus.GaugeValue, float64(m.ConfigCacheFrames.Load()),
	)
	cacheAge := m.ConfigCacheAge()
	ch <- prometheus.MustNewConstMetric(
		c.configCacheAgeSecondsDesc, prometheus.GaugeValue, cacheAge.Seconds(),
	)

	// Config replays by type.
	ch <- prometheus.MustNewConstMetric(
		c.configReplaysTotalDesc, prometheus.CounterValue,
		float64(m.ConfigReplaysFull.Load()), "full",
	)
	ch <- prometheus.MustNewConstMetric(
		c.configReplaysTotalDesc, prometheus.CounterValue,
		float64(m.ConfigReplaysConfigOnly.Load()), "config_only",
	)
	ch <- prometheus.MustNewConstMetric(
		c.configReplaysTotalDesc, prometheus.CounterValue,
		float64(m.ConfigReplaysNodesOnly.Load()), "nodes_only",
	)

	// Messages by port_num (from typeCounts map).
	for portNum, count := range m.MessageTypeCounts() {
		ch <- prometheus.MustNewConstMetric(
			c.messagesTotalDesc, prometheus.CounterValue,
			float64(count), portNum,
		)
	}

	// Mesh nodes count + per-node signal quality.
	m.nodeDirMu.RLock()
	nodeCount := len(m.nodeDir)
	for num, entry := range m.nodeDir {
		numStr := fmt.Sprintf("%d", num)
		name := entry.ShortName

		if entry.RxRssi != 0 {
			ch <- prometheus.MustNewConstMetric(
				c.nodeRssiDesc, prometheus.GaugeValue,
				float64(entry.RxRssi), numStr, name,
			)
		}
		if entry.RxSnr != 0 {
			ch <- prometheus.MustNewConstMetric(
				c.nodeSnrDesc, prometheus.GaugeValue,
				float64(entry.RxSnr), numStr, name,
			)
		}
	}
	m.nodeDirMu.RUnlock()
	ch <- prometheus.MustNewConstMetric(
		c.meshNodesDesc, prometheus.GaugeValue, float64(nodeCount),
	)
}

// NewPrometheusRegistry creates a clean Prometheus registry with all
// meshtastic_proxy_* metrics, Go runtime collectors, and process collectors.
// The returned registry should be used with promhttp.HandlerFor().
func NewPrometheusRegistry(m *Metrics) *prometheus.Registry {
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		newPrometheusCollector(m),
	)
	return reg
}
