(function() {
    'use strict';

    // ── Human-readable bytes formatter ──
    function formatBytes(bytes) {
        if (bytes === 0) return '0 B';
        const units = ['B', 'KB', 'MB', 'GB'];
        const k = 1024;
        const i = Math.min(Math.floor(Math.log(bytes) / Math.log(k)), units.length - 1);
        const val = bytes / Math.pow(k, i);
        return (i === 0 ? val : val.toFixed(1)) + ' ' + units[i];
    }

    // ── Chart.js defaults for dark theme ──
    Chart.defaults.color = '#6b7280';
    Chart.defaults.borderColor = '#1f2937';

    // ── Traffic Line Chart ──
    const trafficCtx = document.getElementById('traffic-chart').getContext('2d');
    // Server-configured limit; will be updated from initial SSE snapshot.
    let maxTrafficPoints = initialTrafficSeries.length || 300;

    const trafficData = {
        labels: initialTrafficSeries.map(s => {
            const d = new Date(s.ts);
            return d.toLocaleTimeString([], {hour:'2-digit', minute:'2-digit', second:'2-digit'});
        }),
        datasets: [
            {
                label: 'From Node',
                data: initialTrafficSeries.map(s => s.fi),
                borderColor: '#60a5fa',
                backgroundColor: 'rgba(96, 165, 250, 0.1)',
                fill: true,
                tension: 0.3,
                pointRadius: 0,
                borderWidth: 1.5,
            },
            {
                label: 'To Node',
                data: initialTrafficSeries.map(s => s.fo),
                borderColor: '#fbbf24',
                backgroundColor: 'rgba(251, 191, 36, 0.1)',
                fill: true,
                tension: 0.3,
                pointRadius: 0,
                borderWidth: 1.5,
            },
        ],
    };

    const trafficChart = new Chart(trafficCtx, {
        type: 'line',
        data: trafficData,
        options: {
            responsive: true,
            maintainAspectRatio: false,
            interaction: { intersect: false, mode: 'index' },
            scales: {
                x: {
                    display: true,
                    ticks: { maxTicksLimit: 8, font: { size: 10 } },
                    grid: { color: '#1f2937' },
                },
                y: {
                    display: true,
                    beginAtZero: true,
                    ticks: { font: { size: 10 }, precision: 0 },
                    grid: { color: '#1f2937' },
                },
            },
            plugins: {
                legend: { position: 'top', labels: { boxWidth: 12, font: { size: 11 } } },
            },
            animation: { duration: 300 },
        },
    });

    // ── Message Types Doughnut Chart ──
    const typesCtx = document.getElementById('types-chart').getContext('2d');

    const typeLabels = Object.keys(initialTypeCounts);
    const typeValues = Object.values(initialTypeCounts);
    const typeColors = generateColors(typeLabels.length);

    const typesChart = new Chart(typesCtx, {
        type: 'doughnut',
        data: {
            labels: typeLabels,
            datasets: [{
                data: typeValues,
                backgroundColor: typeColors,
                borderWidth: 0,
            }],
        },
        options: {
            responsive: true,
            maintainAspectRatio: false,
            plugins: {
                legend: { display: false },
            },
            cutout: '60%',
            animation: { duration: 300 },
        },
    });

    function generateColors(n) {
        const palette = [
            '#60a5fa', '#f472b6', '#34d399', '#fbbf24', '#a78bfa',
            '#fb923c', '#22d3ee', '#e879f9', '#4ade80', '#f87171',
            '#818cf8', '#2dd4bf', '#facc15', '#c084fc', '#fb7185',
        ];
        const result = [];
        for (let i = 0; i < n; i++) {
            result.push(palette[i % palette.length]);
        }
        return result;
    }

    // ── Custom HTML Legend for Doughnut Chart ──
    function updateTypesLegend(chart) {
        const container = document.getElementById('types-legend');
        if (!container) return;
        const labels = chart.data.labels || [];
        const colors = chart.data.datasets[0].backgroundColor || [];
        let html = '';
        for (let i = 0; i < labels.length; i++) {
            const color = colors[i] || '#6b7280';
            const label = labels[i];
            html += '<div class="flex items-center gap-2 py-0.5">'
                + '<span class="flex-shrink-0 w-2.5 h-2.5 rounded-sm" style="background:' + color + '"></span>'
                + '<span class="text-gray-300 truncate text-[11px] leading-tight" title="' + label + '">' + label + '</span>'
                + '</div>';
        }
        container.innerHTML = html;
    }
    updateTypesLegend(typesChart);

    // ── SSE Connection ──
    const sseIndicator = document.getElementById('sse-indicator');
    const sseLabel = document.getElementById('sse-label');
    let reconnectDelay = 1000;
    let paused = false;

    const pauseBtn = document.getElementById('pause-btn');
    pauseBtn.addEventListener('click', function() {
        paused = !paused;
        if (paused) {
            pauseBtn.textContent = 'Paused';
            pauseBtn.classList.add('border-amber-500/50', 'text-amber-400');
            pauseBtn.classList.remove('border-gray-700', 'text-gray-300');
        } else {
            pauseBtn.textContent = 'Live';
            pauseBtn.classList.remove('border-amber-500/50', 'text-amber-400');
            pauseBtn.classList.add('border-gray-700', 'text-gray-300');
        }
    });

    function connectSSE() {
        const evtSource = new EventSource('/api/events');

        evtSource.onopen = function() {
            sseIndicator.className = 'w-2 h-2 rounded-full bg-emerald-400 shadow-[0_0_6px] shadow-emerald-400';
            sseLabel.textContent = 'Live';
            sseLabel.className = 'text-xs text-emerald-400';
            reconnectDelay = 1000;
        };

        evtSource.addEventListener('metrics', function(e) {
            if (paused) return;
            const snap = JSON.parse(e.data);
            updateMetricsUI(snap);
        });

        evtSource.addEventListener('message', function(e) {
            if (paused) return;
            const msg = JSON.parse(e.data);
            prependMessage(msg);
        });

        evtSource.addEventListener('clients', function(e) {
            const clients = JSON.parse(e.data);
            updateClientsUI(clients);
        });

        evtSource.addEventListener('node_directory', function(e) {
            nodeDir = JSON.parse(e.data);
            updateMapMarkers();
        });

        evtSource.addEventListener('node_position_update', function(e) {
            if (paused) return;
            const pos = JSON.parse(e.data);
            updateMapNode(pos);
        });

        evtSource.addEventListener('node_telemetry_update', function(e) {
            if (paused) return;
            const tel = JSON.parse(e.data);
            updateNodeTelemetry(tel);
        });

        evtSource.addEventListener('node_signal_update', function(e) {
            if (paused) return;
            const sig = JSON.parse(e.data);
            updateNodeSignal(sig);
        });

        evtSource.addEventListener('node_update', function(e) {
            if (paused) return;
            const info = JSON.parse(e.data);
            updateNodeInfo(info);
        });

        evtSource.addEventListener('traceroute_result', function(e) {
            const data = JSON.parse(e.data);
            handleTracerouteResult(data);
            appendTraceHistory(data);
        });

        evtSource.addEventListener('trace_history', function(e) {
            const data = JSON.parse(e.data);
            loadTraceHistory(data);
        });

        evtSource.addEventListener('node_favorite', function(e) {
            const data = JSON.parse(e.data);
            if (nodeDir && data.node_num) {
                var numStr = data.node_num.toString();
                if (nodeDir[numStr]) {
                    nodeDir[numStr].is_favorite = data.is_favorite;
                    if (typeof renderNodesTable === 'function') renderNodesTable();
                    if (typeof updateMapMarkers === 'function') updateMapMarkers();
                }
            }
        });

        evtSource.addEventListener('chat_history', function(e) {
            const messages = JSON.parse(e.data);
            loadChatHistory(messages);
        });

        evtSource.addEventListener('chat_message', function(e) {
            if (paused) return;
            const msg = JSON.parse(e.data);
            appendChatMessage(msg);
        });

        evtSource.onerror = function() {
            sseIndicator.className = 'w-2 h-2 rounded-full bg-red-400';
            sseLabel.textContent = 'Reconnecting...';
            sseLabel.className = 'text-xs text-red-400';
            evtSource.close();
            setTimeout(connectSSE, reconnectDelay);
            reconnectDelay = Math.min(reconnectDelay * 2, 30000);
        };
    }

    // ── Update UI from metrics snapshot or delta ──
    // Full snapshot has `traffic_series` (array); delta has `latest_sample` (object).
    function updateMetricsUI(snap) {
        // Badge
        const badge = document.getElementById('node-badge');
        if (snap.node_connected) {
            badge.textContent = 'Connected';
            badge.className = 'text-xs font-semibold px-3 py-1 rounded-full uppercase tracking-wider bg-emerald-500/15 text-emerald-400 border border-emerald-500/30';
        } else {
            badge.textContent = 'Disconnected';
            badge.className = 'text-xs font-semibold px-3 py-1 rounded-full uppercase tracking-wider bg-red-500/15 text-red-400 border border-red-500/30';
        }

        // Cards
        document.getElementById('card-node').textContent = snap.node_address;
        const statusEl = document.getElementById('card-node-status');
        if (snap.node_connected) {
            statusEl.innerHTML = '<span class="w-2 h-2 rounded-full bg-emerald-400 shadow-[0_0_6px] shadow-emerald-400 inline-block"></span> Connected';
        } else {
            statusEl.innerHTML = '<span class="w-2 h-2 rounded-full bg-red-400 shadow-[0_0_6px] shadow-red-400 inline-block"></span> Disconnected';
        }

        document.getElementById('card-clients').textContent = snap.active_clients;
        document.getElementById('card-uptime').textContent = snap.uptime_str;
        document.getElementById('card-reconnects').textContent = snap.node_reconnects + ' reconnects';
        document.getElementById('card-cache-frames').textContent = snap.config_cache_frames + ' frames';
        document.getElementById('card-cache-age').textContent = snap.config_cache_age ? 'updated ' + snap.config_cache_age + ' ago' : 'not cached';
        document.getElementById('card-frames-in').textContent = snap.frames_from_node;
        document.getElementById('card-bytes-in').textContent = formatBytes(snap.bytes_from_node);
        document.getElementById('card-frames-out').textContent = snap.frames_to_node;
        document.getElementById('card-bytes-out').textContent = formatBytes(snap.bytes_to_node);

        // Traffic chart
        if (snap.traffic_series && snap.traffic_series.length > 0) {
            // Full snapshot — replace all data and sync max points
            if (snap.max_traffic_samples) {
                maxTrafficPoints = snap.max_traffic_samples;
            }
            trafficData.labels = snap.traffic_series.map(s => {
                const d = new Date(s.ts);
                return d.toLocaleTimeString([], {hour:'2-digit', minute:'2-digit', second:'2-digit'});
            });
            trafficData.datasets[0].data = snap.traffic_series.map(s => s.fi);
            trafficData.datasets[1].data = snap.traffic_series.map(s => s.fo);
            trafficChart.update('none');
        } else if (snap.latest_sample) {
            // Delta — append single sample, trim to server-configured limit
            const s = snap.latest_sample;
            const d = new Date(s.ts);
            const label = d.toLocaleTimeString([], {hour:'2-digit', minute:'2-digit', second:'2-digit'});
            trafficData.labels.push(label);
            trafficData.datasets[0].data.push(s.fi);
            trafficData.datasets[1].data.push(s.fo);
            while (trafficData.labels.length > maxTrafficPoints) {
                trafficData.labels.shift();
                trafficData.datasets[0].data.shift();
                trafficData.datasets[1].data.shift();
            }
            trafficChart.update('none');
        }

        // Message type doughnut — full rebuild
        if (snap.message_type_counts) {
            const labels = Object.keys(snap.message_type_counts);
            const values = Object.values(snap.message_type_counts);
            typesChart.data.labels = labels;
            typesChart.data.datasets[0].data = values;
            typesChart.data.datasets[0].backgroundColor = generateColors(labels.length);
            typesChart.update('none');
            updateTypesLegend(typesChart);
        }

        // Node directory — update from full snapshot
        if (snap.node_dir) {
            nodeDir = snap.node_dir;
            updateMapMarkers();
        }
    }

    // ── Prepend a new message row to the table ──
    function prependMessage(msg) {
        const tbody = document.getElementById('messages-tbody');
        const tr = document.createElement('tr');
        tr.className = 'hover:bg-gray-800/50 new-row';

        const ts = new Date(msg.timestamp);
        const timeStr = ts.toLocaleTimeString([], {hour:'2-digit', minute:'2-digit', second:'2-digit', fractionalSecondDigits: 3});

        const dirTag = msg.direction === 'from_node'
            ? '<span class="inline-block text-[10px] font-bold px-2 py-0.5 rounded bg-blue-500/15 text-blue-400 border border-blue-500/30 font-mono uppercase">IN</span>'
            : '<span class="inline-block text-[10px] font-bold px-2 py-0.5 rounded bg-amber-500/15 text-amber-400 border border-amber-500/30 font-mono uppercase">OUT</span>';

        const fromHTML = Mesh.nodeName(msg.from);
        const toHTML = Mesh.nodeName(msg.to);

        const hopsStr = (msg.hop_limit || msg.hop_start) ? msg.hop_limit + '/' + msg.hop_start : '&mdash;';
        const rssiStr = msg.rx_rssi ? msg.rx_rssi : '&mdash;';
        const snrStr = msg.rx_snr ? msg.rx_snr.toFixed(1) : '&mdash;';

        let relayBadges = '';
        if (msg.via_mqtt) {
            relayBadges += '<span class="inline-block text-[10px] font-bold px-1.5 py-0.5 rounded bg-purple-500/15 text-purple-400 border border-purple-500/30 font-mono">MQTT</span> ';
        }
        if (msg.relay_node) {
            relayBadges += relayNameJS(msg.relay_node);
        }
        if (!relayBadges) relayBadges = '<span class="text-gray-600">&mdash;</span>';

        tr.innerHTML = `
            <td class="px-3 py-1.5 border-b border-gray-800/50 whitespace-nowrap"><code class="font-mono text-xs">${timeStr}</code></td>
            <td class="px-3 py-1.5 border-b border-gray-800/50 whitespace-nowrap">${dirTag}</td>
            <td class="px-3 py-1.5 border-b border-gray-800/50 whitespace-nowrap"><code class="font-mono text-xs">${Mesh.esc(msg.type)}</code></td>
            <td class="hidden sm:table-cell px-3 py-1.5 border-b border-gray-800/50 whitespace-nowrap"><code class="font-mono text-xs text-gray-400">${Mesh.esc(msg.port_num)}</code></td>
            <td class="px-3 py-1.5 border-b border-gray-800/50 whitespace-nowrap">${fromHTML}</td>
            <td class="px-3 py-1.5 border-b border-gray-800/50 whitespace-nowrap">${toHTML}</td>
            <td class="hidden sm:table-cell px-3 py-1.5 border-b border-gray-800/50 whitespace-nowrap"><code class="font-mono text-xs">${hopsStr}</code></td>
            <td class="px-3 py-1.5 border-b border-gray-800/50 text-right tabular-nums whitespace-nowrap"><code class="font-mono text-xs">${rssiStr}</code></td>
            <td class="px-3 py-1.5 border-b border-gray-800/50 text-right tabular-nums whitespace-nowrap"><code class="font-mono text-xs">${snrStr}</code></td>
            <td class="hidden sm:table-cell px-3 py-1.5 border-b border-gray-800/50 whitespace-nowrap">${relayBadges}</td>
            <td class="hidden sm:table-cell px-3 py-1.5 border-b border-gray-800/50 text-right tabular-nums whitespace-nowrap">${msg.size}B</td>
            <td class="px-3 py-1.5 border-b border-gray-800/50 max-w-xs truncate" title="${Mesh.esc(msg.payload || '')}"><span class="text-xs text-gray-300">${Mesh.esc(msg.payload || '')}</span></td>
        `;

        tbody.insertBefore(tr, tbody.firstChild);

        // Apply current filter to new row
        if (filterTerm && !tr.textContent.toLowerCase().includes(filterTerm)) {
            tr.style.display = 'none';
        }

        // Limit rows to 200
        while (tbody.children.length > 200) {
            tbody.removeChild(tbody.lastChild);
        }

        // Update count
        applyMessageFilter();
    }

    // ── Update client list from SSE ──
    function updateClientsUI(clients) {
        const section = document.getElementById('clients-section');
        const tbody = document.getElementById('clients-tbody');

        if (clients && clients.length > 0) {
            section.classList.remove('hidden');
            tbody.innerHTML = clients.map((addr, i) =>
                `<tr class="hover:bg-gray-800/50">
                    <td class="px-3 py-2 border-b border-gray-800/50">${i}</td>
                    <td class="px-3 py-2 border-b border-gray-800/50"><code class="font-mono text-xs">${Mesh.esc(addr)}</code></td>
                </tr>`
            ).join('');
        } else {
            section.classList.add('hidden');
        }
    }

    function relayNameJS(relayNode) {
        if (!relayNode) return '';
        const lastByte = relayNode & 0xFF;
        return '<span class="font-mono text-xs text-cyan-400">!' + lastByte.toString(16).padStart(2, '0') + '</span>';
    }

    // ── Message filter ──
    let filterTerm = '';
    const filterInput = document.getElementById('msg-filter');

    filterInput.addEventListener('input', function() {
        filterTerm = this.value.toLowerCase();
        applyMessageFilter();
    });

    function applyMessageFilter() {
        const tbody = document.getElementById('messages-tbody');
        const rows = tbody.querySelectorAll('tr');
        let visible = 0;
        for (const row of rows) {
            if (!filterTerm || row.textContent.toLowerCase().includes(filterTerm)) {
                row.style.display = '';
                visible++;
            } else {
                row.style.display = 'none';
            }
        }
        document.getElementById('msg-count').textContent = visible + ' messages' + (filterTerm ? ' (filtered)' : '');
    }

    // ── Bootstrap ──
    connectSSE();

    // ── Map (Leaflet) ──
    let mapInstance = null;
    let mapMarkers = {};   // nodeNum string → L.marker
    let mapInitialized = false;
    let traceRouteLayer = null;  // L.layerGroup for current traceroute visualization
    let pendingTraceroute = null; // {target: nodeNum, timer: timeoutId} or null
    let traceCooldown = null;     // {target: nodeNum, interval: id, remaining: int} or null

    // Expose initMap globally for tab switching
    window.initMap = function() {
        if (mapInitialized) {
            mapInstance.invalidateSize();
            return;
        }
        mapInitialized = true;

        mapInstance = L.map('map', {
            center: [0, 0],
            zoom: 2,
            zoomControl: true,
            attributionControl: true,
        });

        // OpenStreetMap dark-ish tiles (CartoDB Dark Matter)
        L.tileLayer('https://{s}.basemaps.cartocdn.com/dark_all/{z}/{x}/{y}{r}.png', {
            attribution: '&copy; <a href="https://www.openstreetmap.org/copyright">OpenStreetMap</a> &copy; <a href="https://carto.com/">CARTO</a>',
            subdomains: 'abcd',
            maxZoom: 19,
        }).addTo(mapInstance);

        // Populate initial markers from nodeDir
        updateMapMarkers();
    }

    function createNodeIcon(shortName) {
        return L.divIcon({
            className: 'node-marker',
            html: '<div class="node-marker-dot"></div>' +
                  (shortName ? '<div class="node-marker-label">' + Mesh.esc(shortName) + '</div>' : ''),
            iconSize: [14, 14],
            iconAnchor: [7, 7],
            popupAnchor: [0, -12],
        });
    }

    function nodePopupHTML(nodeNum, entry) {
        const hex = '!' + parseInt(nodeNum).toString(16).padStart(8, '0');
        let h = '<div class="np">';

        // ── Header: name + hex ID + badges ──
        h += '<div class="np-header">';
        if (entry.short_name) {
            h += '<span class="np-name">' + Mesh.esc(entry.short_name) + '</span>';
        }
        h += '<span class="np-hex">' + Mesh.esc(hex) + '</span>';
        if (entry.is_licensed) h += ' <span class="np-badge np-badge-ham">HAM</span>';
        if (entry.via_mqtt) h += ' <span class="np-badge np-badge-mqtt">MQTT</span>';
        if (entry.is_favorite) h += ' <span class="np-badge np-badge-fav">FAV</span>';
        h += '</div>';
        if (entry.long_name) {
            h += '<div style="font-size:12px;color:#9ca3af;margin-bottom:2px;">' + Mesh.esc(entry.long_name) + '</div>';
        }

        // ── Identity section ──
        const hasIdentity = entry.hw_model || entry.role || entry.user_id;
        if (hasIdentity) {
            h += '<div class="np-section"><div class="np-section-title">Identity</div>';
            if (entry.user_id) h += npRow('ID', entry.user_id);
            if (entry.hw_model) h += npRow('Hardware', Mesh.formatHwModel(entry.hw_model));
            if (entry.role) h += npRow('Role', Mesh.formatRole(entry.role));
            h += '</div>';
        }

        // ── Radio section ──
        const hasRadio = entry.snr || entry.rx_rssi || entry.rx_snr || entry.last_heard || entry.hops_away || entry.channel;
        if (hasRadio) {
            h += '<div class="np-section"><div class="np-section-title">Radio</div>';
            if (entry.rx_rssi) h += npRow('RSSI', entry.rx_rssi + ' dBm');
            if (entry.rx_snr) h += npRow('SNR (live)', entry.rx_snr.toFixed(1) + ' dB');
            if (entry.snr && !entry.rx_snr) h += npRow('SNR', entry.snr.toFixed(1) + ' dB');
            if (entry.hops_away !== undefined && entry.hops_away > 0) h += npRow('Hops', entry.hops_away.toString());
            if (entry.channel) h += npRow('Channel', entry.channel.toString());
            if (entry.last_heard) h += npRow('Last heard', Mesh.formatUnixAge(entry.last_heard));
            h += '</div>';
        }

        // ── Position section ──
        const hasPos = entry.latitude || entry.longitude;
        if (hasPos) {
            h += '<div class="np-section"><div class="np-section-title">Position</div>';
            h += npRow('Coords', entry.latitude.toFixed(6) + ', ' + entry.longitude.toFixed(6));
            if (entry.altitude) h += npRow('Altitude', entry.altitude + ' m');
            if (entry.ground_speed) h += npRow('Speed', entry.ground_speed + ' m/s');
            if (entry.ground_track) h += npRow('Heading', entry.ground_track + '&deg;');
            if (entry.sats_in_view) h += npRow('Satellites', entry.sats_in_view.toString());
            if (entry.position_time) h += npRow('Fix age', Mesh.formatUnixAge(entry.position_time));
            h += '</div>';
        }

        // ── Device telemetry section ──
        const hasDev = entry.battery_level || entry.voltage || entry.uptime_seconds || entry.channel_utilization || entry.air_util_tx;
        if (hasDev) {
            h += '<div class="np-section"><div class="np-section-title">Device</div>';
            if (entry.battery_level) {
                const pct = Math.min(entry.battery_level, 100);
                const color = pct > 50 ? '#34d399' : pct > 20 ? '#fbbf24' : '#f87171';
                h += '<div class="np-row"><span class="np-label">Battery</span>'
                   + '<span class="np-value"><span class="np-bar"><span class="np-bar-fill" style="width:' + pct + '%;background:' + color + '"></span></span>'
                   + pct + '%</span></div>';
            }
            if (entry.voltage) h += npRow('Voltage', entry.voltage.toFixed(2) + ' V');
            if (entry.uptime_seconds) h += npRow('Uptime', formatUptime(entry.uptime_seconds));
            if (entry.channel_utilization) h += npRow('Ch. util', entry.channel_utilization.toFixed(1) + '%');
            if (entry.air_util_tx) h += npRow('Air TX', entry.air_util_tx.toFixed(1) + '%');
            h += '</div>';
        }

        // ── Environment section ──
        const hasEnv = entry.temperature || entry.relative_humidity || entry.barometric_pressure;
        if (hasEnv) {
            h += '<div class="np-section"><div class="np-section-title">Environment</div>';
            if (entry.temperature) h += npRow('Temp', entry.temperature.toFixed(1) + ' &deg;C');
            if (entry.relative_humidity) h += npRow('Humidity', entry.relative_humidity.toFixed(1) + '%');
            if (entry.barometric_pressure) h += npRow('Pressure', entry.barometric_pressure.toFixed(1) + ' hPa');
            h += '</div>';
        }

        // ── Trace Route button ──
        h += '<div class="np-section" style="text-align:center;">';
        const tracing = pendingTraceroute !== null;
        const cooling = traceCooldown !== null;
        const tracingThis = tracing && pendingTraceroute.target === parseInt(nodeNum);
        const coolingThis = cooling && traceCooldown.target === parseInt(nodeNum);
        h += '<button class="np-trace-btn" data-trace-target="' + nodeNum + '"'
           + ((tracing || cooling) ? ' disabled' : '')
           + ' onclick="startTraceroute(' + nodeNum + ')">';
        if (coolingThis) {
            h += traceCooldown.remaining + 's';
        } else if (tracingThis) {
            h += Mesh.SVG_TRACE + 'Tracing…<span class="spinner"></span>';
        } else if (tracing || cooling) {
            h += Mesh.SVG_TRACE + 'Trace busy';
        } else {
            h += Mesh.SVG_TRACE + 'Trace Route';
        }
        h += '</button>';
        h += '</div>';

        h += '</div>';
        return h;
    }

    function npRow(label, value) {
        return '<div class="np-row"><span class="np-label">' + label + '</span><span class="np-value">' + value + '</span></div>';
    }

    function formatUptime(secs) {
        if (!secs) return '';
        const d = Math.floor(secs / 86400);
        const h = Math.floor((secs % 86400) / 3600);
        const m = Math.floor((secs % 3600) / 60);
        if (d > 0) return d + 'd ' + h + 'h';
        if (h > 0) return h + 'h ' + m + 'm';
        return m + 'm';
    }

    window.updateMapMarkers = function() {
        if (!mapInstance || !nodeDir) return;

        const nodesWithPos = [];

        // Add/update markers
        for (const [numStr, entry] of Object.entries(nodeDir)) {
            if (!entry.latitude && !entry.longitude) continue;
            nodesWithPos.push(numStr);

            if (mapMarkers[numStr]) {
                // Update existing marker position and popup
                mapMarkers[numStr].setLatLng([entry.latitude, entry.longitude]);
                mapMarkers[numStr].setIcon(createNodeIcon(entry.short_name));
                mapMarkers[numStr].setPopupContent(nodePopupHTML(numStr, entry));
            } else {
                // Create new marker
                const marker = L.marker([entry.latitude, entry.longitude], {
                    icon: createNodeIcon(entry.short_name),
                }).addTo(mapInstance);
                marker.bindPopup(nodePopupHTML(numStr, entry), { maxWidth: 300 });
                mapMarkers[numStr] = marker;
            }
        }

        // Remove markers for nodes no longer in directory
        for (const numStr of Object.keys(mapMarkers)) {
            if (!nodeDir[numStr] || (!nodeDir[numStr].latitude && !nodeDir[numStr].longitude)) {
                mapMarkers[numStr].remove();
                delete mapMarkers[numStr];
            }
        }

        // Fit bounds if we have nodes
        if (nodesWithPos.length > 0) {
            const bounds = L.latLngBounds(
                nodesWithPos.map(n => [nodeDir[n].latitude, nodeDir[n].longitude])
            );
            mapInstance.fitBounds(bounds, { padding: [50, 50], maxZoom: 14 });
            document.getElementById('map-no-nodes').classList.add('hidden');
        } else {
            document.getElementById('map-no-nodes').classList.remove('hidden');
        }

        // Rebuild dynamic filter options and apply filters
        rebuildFilterOptions();
        applyMapFilters();
        updateHeatmap();
        Mesh.bus.emit('nodesChanged');
    }

    window.updateMapNode = function(pos) {
        if (!nodeDir) return;

        // Update nodeDir with new position (including extended fields)
        const numStr = pos.node_num.toString();
        if (nodeDir[numStr]) {
            nodeDir[numStr].latitude = pos.latitude;
            nodeDir[numStr].longitude = pos.longitude;
            nodeDir[numStr].altitude = pos.altitude;
            if (pos.ground_speed) nodeDir[numStr].ground_speed = pos.ground_speed;
            if (pos.ground_track) nodeDir[numStr].ground_track = pos.ground_track;
            if (pos.sats_in_view) nodeDir[numStr].sats_in_view = pos.sats_in_view;
            if (pos.position_time) nodeDir[numStr].position_time = pos.position_time;
            nodeDir[numStr].seen_realtime = true;
            nodeDir[numStr].last_heard = Math.floor(Date.now() / 1000);
        } else {
            nodeDir[numStr] = {
                short_name: pos.short_name,
                long_name: pos.long_name,
                latitude: pos.latitude,
                longitude: pos.longitude,
                altitude: pos.altitude,
                ground_speed: pos.ground_speed || 0,
                ground_track: pos.ground_track || 0,
                sats_in_view: pos.sats_in_view || 0,
                position_time: pos.position_time || 0,
                seen_realtime: true,
                last_heard: Math.floor(Date.now() / 1000),
            };
        }

        // Update map marker if map is initialized
        if (!mapInstance) return;

        const entry = nodeDir[numStr];
        if (mapMarkers[numStr]) {
            mapMarkers[numStr].setLatLng([pos.latitude, pos.longitude]);
            mapMarkers[numStr].setIcon(createNodeIcon(entry.short_name));
            mapMarkers[numStr].setPopupContent(nodePopupHTML(numStr, entry));
        } else {
            const marker = L.marker([pos.latitude, pos.longitude], {
                icon: createNodeIcon(entry.short_name),
            }).addTo(mapInstance);
            marker.bindPopup(nodePopupHTML(numStr, entry), { maxWidth: 300 });
            mapMarkers[numStr] = marker;
            document.getElementById('map-no-nodes').classList.add('hidden');
        }

        // Apply filters to new/updated marker
        applyMapFilters();
        updateHeatmap();
        Mesh.bus.emit('nodesChanged');
    }

    // Update node telemetry data and refresh popup if open
    window.updateNodeTelemetry = function(tel) {
        if (!nodeDir) return;

        const numStr = tel.node_num.toString();
        if (!nodeDir[numStr]) {
            nodeDir[numStr] = { short_name: tel.short_name, long_name: tel.long_name };
        }
        const entry = nodeDir[numStr];
        entry.seen_realtime = true;
        entry.last_heard = Math.floor(Date.now() / 1000);

        // Device metrics
        if (tel.battery_level) entry.battery_level = tel.battery_level;
        if (tel.voltage) entry.voltage = tel.voltage;
        if (tel.channel_utilization) entry.channel_utilization = tel.channel_utilization;
        if (tel.air_util_tx) entry.air_util_tx = tel.air_util_tx;
        if (tel.uptime_seconds) entry.uptime_seconds = tel.uptime_seconds;

        // Environment metrics
        if (tel.temperature) entry.temperature = tel.temperature;
        if (tel.relative_humidity) entry.relative_humidity = tel.relative_humidity;
        if (tel.barometric_pressure) entry.barometric_pressure = tel.barometric_pressure;

        // Refresh popup if this node's popup is open
        if (mapInstance && mapMarkers[numStr] && mapMarkers[numStr].isPopupOpen()) {
            mapMarkers[numStr].setPopupContent(nodePopupHTML(numStr, entry));
        }

        // Re-apply filters (battery/telemetry data may have changed)
        applyMapFilters();
        Mesh.bus.emit('nodesChanged');
    }

    // ══════════════════════════════════════════════
    // ── Node Signal Updates ──
    // ══════════════════════════════════════════════

    window.updateNodeSignal = function(sig) {
        if (!nodeDir) return;

        const numStr = sig.node_num.toString();
        if (!nodeDir[numStr]) {
            nodeDir[numStr] = { short_name: sig.short_name, long_name: sig.long_name };
        }
        const entry = nodeDir[numStr];
        entry.seen_realtime = true;
        entry.last_heard = Math.floor(Date.now() / 1000);

        if (sig.rx_rssi) entry.rx_rssi = sig.rx_rssi;
        if (sig.rx_snr) entry.rx_snr = sig.rx_snr;

        // Refresh popup if this node's popup is open
        if (mapInstance && mapMarkers[numStr] && mapMarkers[numStr].isPopupOpen()) {
            mapMarkers[numStr].setPopupContent(nodePopupHTML(numStr, entry));
        }

        // Update heatmap if active
        updateHeatmap();
        Mesh.bus.emit('nodesChanged');
    }

    // ══════════════════════════════════════════════
    // ── Node Info Updates (NODEINFO_APP) ──
    // ══════════════════════════════════════════════

    window.updateNodeInfo = function(info) {
        if (!nodeDir) nodeDir = {};

        const numStr = info.node_num.toString();
        if (!nodeDir[numStr]) {
            nodeDir[numStr] = {};
        }
        const entry = nodeDir[numStr];
        entry.seen_realtime = true;
        entry.last_heard = Math.floor(Date.now() / 1000);

        // Update identity fields
        if (info.short_name) entry.short_name = info.short_name;
        if (info.long_name) entry.long_name = info.long_name;
        if (info.user_id) entry.user_id = info.user_id;
        if (info.hw_model) entry.hw_model = info.hw_model;
        if (info.role) entry.role = info.role;
        entry.is_licensed = info.is_licensed || false;

        // Refresh map marker icon/popup if it exists (name may have changed)
        if (mapInstance && mapMarkers[numStr]) {
            mapMarkers[numStr].setIcon(createNodeIcon(entry.short_name));
            if (mapMarkers[numStr].isPopupOpen()) {
                mapMarkers[numStr].setPopupContent(nodePopupHTML(numStr, entry));
            }
        }
        Mesh.bus.emit('nodeInfoChanged', info);
    }

    // ══════════════════════════════════════════════
    // ── Trace Route ──
    // ══════════════════════════════════════════════

    function clearTraceRoute() {
        if (traceRouteLayer) {
            mapInstance.removeLayer(traceRouteLayer);
            traceRouteLayer = null;
        }
    }

    // Find all trace buttons for a given node (map popup + nodes table)
    function findTraceBtns(nodeNum) {
        return document.querySelectorAll('[data-trace-target="' + nodeNum + '"]');
    }

    function startCooldown(nodeNum, startedAt) {
        var elapsed = startedAt ? Math.floor((Date.now() - startedAt) / 1000) : 0;
        var remaining = Math.max(30 - elapsed, 0);
        if (remaining <= 0) { resetTraceBtn(nodeNum); return; }
        var btns = findTraceBtns(nodeNum);
        btns.forEach(function(btn) {
            btn.disabled = true;
            btn.style.color = '#6b7280';
            btn.innerHTML = btn.classList.contains('na-trace-btn') ? remaining + 's' : remaining + 's';
        });
        traceCooldown = {
            target: nodeNum,
            remaining: remaining,
            interval: setInterval(function() {
                remaining--;
                traceCooldown.remaining = remaining;
                findTraceBtns(nodeNum).forEach(function(btn) {
                    btn.innerHTML = remaining + 's';
                });
                if (remaining <= 0) {
                    clearInterval(traceCooldown.interval);
                    traceCooldown = null;
                    resetTraceBtn(nodeNum);
                }
            }, 1000),
        };
    }

    function resetTraceBtn(nodeNum) {
        findTraceBtns(nodeNum).forEach(function(btn) {
            btn.disabled = false;
            btn.style.color = '';
            if (btn.classList.contains('na-trace-btn')) {
                btn.innerHTML = Mesh.SVG_TRACE_COMPACT;
            } else {
                btn.innerHTML = Mesh.SVG_TRACE + 'Trace Route';
            }
        });
    }

    // Expose trace state so other IIFEs (nodeAction, renderNodesTable) can
    // check it without direct access to the let-scoped variables.
    window.isTraceActive = function() {
        return pendingTraceroute !== null || traceCooldown !== null;
    };
    window.getTraceState = function(nodeNum) {
        var num = parseInt(nodeNum);
        var active = pendingTraceroute !== null || traceCooldown !== null;
        var tracingThis = pendingTraceroute !== null && pendingTraceroute.target === num;
        var coolingThis = traceCooldown !== null && traceCooldown.target === num;
        return {
            active: active,
            tracingThis: tracingThis,
            coolingThis: coolingThis,
            remaining: coolingThis ? traceCooldown.remaining : 0,
        };
    };

    window.startTraceroute = function(nodeNum) {
        // Guard: ignore if a traceroute is already in flight or cooldown active
        if (pendingTraceroute) return;
        if (traceCooldown) return;

        // Clear previous route
        clearTraceRoute();

        // Set all trace buttons to loading state
        findTraceBtns(nodeNum).forEach(function(btn) {
            btn.disabled = true;
            if (!btn.querySelector('.spinner')) {
                btn.innerHTML = '';
                var sp = document.createElement('span');
                sp.className = 'spinner';
                btn.appendChild(sp);
            }
        });

        // Set pending state and timeout BEFORE fetch so guards work immediately
        const timer = setTimeout(function() {
            // Add "no response" entry to trace history
            appendTraceHistory({
                from: pendingTraceroute ? pendingTraceroute.target : nodeNum,
                to: 0,
                route: [],
                timeout: true,
                timestamp: new Date().toISOString(),
            });
            var sa = pendingTraceroute ? pendingTraceroute.startedAt : null;
            pendingTraceroute = null;
            startCooldown(nodeNum, sa);
        }, 30000);

        pendingTraceroute = {target: nodeNum, timer: timer, startedAt: Date.now()};

        // Send traceroute request
        fetch('/api/traceroute', {
            method: 'POST',
            headers: {'Content-Type': 'application/json'},
            body: JSON.stringify({target: nodeNum}),
        }).catch(function(err) {
            console.error('Traceroute request failed:', err);
            if (pendingTraceroute) {
                clearTimeout(pendingTraceroute.timer);
                pendingTraceroute = null;
            }
            resetTraceBtn(nodeNum);
        });
    }

    window.handleTracerouteResult = function(data) {
        // data: {from: uint32, to: uint32, route: [], route_back: []}
        // from = target node that responded, to = our node
        // Full forward path: to → route[0] → route[1] → ... → from

        // Match to pending traceroute
        if (!pendingTraceroute || pendingTraceroute.target !== data.from) return;

        clearTimeout(pendingTraceroute.timer);
        var target = pendingTraceroute.target;
        var sa = pendingTraceroute.startedAt;
        pendingTraceroute = null;
        startCooldown(target, sa);

        drawTraceroute(data);
    }

    function drawTraceroute(data) {
        if (!mapInstance || !nodeDir) return;

        clearTraceRoute();

        // Build ordered list of node numbers: our node → hops → target
        const hopNums = [data.to];
        if (data.route) {
            for (let i = 0; i < data.route.length; i++) {
                hopNums.push(data.route[i]);
            }
        }
        hopNums.push(data.from);

        // Collect coordinates for nodes that have position data
        const coords = [];
        const coordHops = []; // parallel array: {num, index} for markers
        for (let i = 0; i < hopNums.length; i++) {
            const numStr = hopNums[i].toString();
            const entry = nodeDir[numStr];
            if (entry && (entry.latitude || entry.longitude)) {
                coords.push([entry.latitude, entry.longitude]);
                coordHops.push({num: hopNums[i], index: i});
            }
        }

        if (coords.length < 2) return; // Need at least 2 points to draw a line

        const layers = [];

        // Draw polyline with ant-path animation
        const line = L.polyline(coords, {
            className: 'trace-route-line',
            weight: 3,
            opacity: 0.9,
            interactive: true,
        });

        // Build popup content for the route line
        let popupHtml = '<div style="font-size:12px;color:#e5e7eb;line-height:1.6;">';
        popupHtml += '<div style="font-weight:600;color:#06b6d4;margin-bottom:4px;">Trace Route</div>';
        const snrFwd = data.snr_towards || [];
        for (let i = 0; i < hopNums.length; i++) {
            if (i > 0) {
                let snrTag = '';
                const snrIdx = i - 1;
                if (snrIdx < snrFwd.length && snrFwd[snrIdx] !== 0) {
                    snrTag = ' <span style="color:#6b7280;font-size:10px;">' + (snrFwd[snrIdx] / 4).toFixed(1) + 'dB</span>';
                }
                popupHtml += ' <span style="color:#4b5563;">→</span>' + snrTag + ' ';
            }
            const name = Mesh.nodeHexName(hopNums[i]);
            if (i === 0) {
                popupHtml += '<span style="color:#34d399;">' + Mesh.esc(name) + '</span>';
            } else if (i === hopNums.length - 1) {
                popupHtml += '<span style="color:#f59e0b;">' + Mesh.esc(name) + '</span>';
            } else {
                popupHtml += '<span style="color:#e5e7eb;">' + Mesh.esc(name) + '</span>';
            }
        }
        popupHtml += '<div style="color:#6b7280;margin-top:4px;font-size:11px;">'
            + hopNums.length + ' nodes, ' + (hopNums.length - 1) + ' hop(s)</div>';
        if (data.route_back && data.route_back.length > 0) {
            const snrBk = data.snr_back || [];
            popupHtml += '<div style="color:#6b7280;font-size:11px;">Return: ';
            popupHtml += Mesh.esc(Mesh.nodeHexName(data.from));
            for (let i = 0; i < data.route_back.length; i++) {
                let snrTag = '';
                if (i < snrBk.length && snrBk[i] !== 0) {
                    snrTag = ' <span style="font-size:9px;">' + (snrBk[i] / 4).toFixed(1) + 'dB</span>';
                }
                popupHtml += ' →' + snrTag + ' ' + Mesh.esc(Mesh.nodeHexName(data.route_back[i]));
            }
            popupHtml += ' → ' + Mesh.esc(Mesh.nodeHexName(data.to));
            popupHtml += '</div>';
        }
        popupHtml += '</div>';
        line.bindPopup(popupHtml, {maxWidth: 350});
        layers.push(line);

        // Add hop markers with numbers
        for (let i = 0; i < coordHops.length; i++) {
            const hop = coordHops[i];
            const name = Mesh.nodeHexName(hop.num);
            const isStart = (hop.index === 0);
            const isEnd = (hop.index === hopNums.length - 1);
            const color = isStart ? '#34d399' : isEnd ? '#f59e0b' : '#06b6d4';

            const marker = L.marker(coords[i], {
                icon: L.divIcon({
                    className: '',
                    html: '<div class="trace-hop-marker" style="border-color:' + color + ';color:' + color + ';">' + hop.index + '</div>',
                    iconSize: [22, 22],
                    iconAnchor: [11, 11],
                }),
                interactive: true,
            });
            marker.bindTooltip(name, {
                permanent: false,
                direction: 'top',
                offset: [0, -14],
                className: 'trace-hop-tooltip',
            });
            layers.push(marker);
        }

        traceRouteLayer = L.layerGroup(layers).addTo(mapInstance);

        // Fit map to show the full route
        const bounds = L.latLngBounds(coords);
        mapInstance.fitBounds(bounds, {padding: [60, 60], maxZoom: 15});
    }
    window.drawTraceroute = drawTraceroute;

    // ══════════════════════════════════════════════
    // ── Heatmap Layer ──
    // ══════════════════════════════════════════════

    let heatmapLayer = null;
    let heatmapMode = 'off'; // 'off', 'rssi', 'snr'

    // RSSI gradient: blue (weak) → yellow → red (strong / closer to 0)
    const rssiGradient = {
        0.0: '#1e3a5f',
        0.25: '#2563eb',
        0.5: '#22d3ee',
        0.75: '#fbbf24',
        1.0: '#ef4444',
    };

    // SNR gradient: red (low/negative) → yellow → green (high/positive)
    const snrGradient = {
        0.0: '#1e3a5f',
        0.25: '#ef4444',
        0.5: '#fbbf24',
        0.75: '#34d399',
        1.0: '#10b981',
    };

    function getHeatmapPoints(mode) {
        if (!nodeDir) return [];
        const points = [];
        for (const [numStr, entry] of Object.entries(nodeDir)) {
            if (!entry.latitude && !entry.longitude) continue;

            let intensity;
            if (mode === 'rssi') {
                // Typical LoRa RSSI: -115 dBm (weak) to -75 dBm (strong).
                // Values outside this range are clamped to 0..1.
                const rssi = entry.rx_rssi;
                if (!rssi) continue;
                const linear = Math.max(0, Math.min(1, (rssi + 115) / 40));
                // Power curve (γ=0.6) boosts mid-range values so typical
                // LoRa signals (-90…-100 dBm) render visibly on the map.
                intensity = Math.pow(linear, 0.6);
            } else if (mode === 'snr') {
                // Typical LoRa SNR: -15 dB (worst) to +12 dB (best).
                const snr = entry.rx_snr || entry.snr;
                if (!snr && snr !== 0) continue;
                const linear = Math.max(0, Math.min(1, (snr + 15) / 27));
                intensity = Math.pow(linear, 0.6);
            } else {
                continue;
            }

            points.push([entry.latitude, entry.longitude, intensity]);
        }
        return points;
    }

    function updateHeatmap() {
        if (!mapInstance) return;

        if (heatmapMode === 'off') {
            if (heatmapLayer) {
                mapInstance.removeLayer(heatmapLayer);
                heatmapLayer = null;
            }
            document.getElementById('hm-legend').style.display = 'none';
            return;
        }

        const points = getHeatmapPoints(heatmapMode);
        const gradient = heatmapMode === 'rssi' ? rssiGradient : snrGradient;
        const opts = {
            radius: 55,
            blur: 30,
            maxZoom: 17,
            max: 1.0,
            minOpacity: 0.35,
            gradient: gradient,
        };

        if (heatmapLayer) {
            heatmapLayer.setLatLngs(points);
            heatmapLayer.setOptions(opts);
        } else {
            heatmapLayer = L.heatLayer(points, opts).addTo(mapInstance);
        }

        // Update legend
        const legend = document.getElementById('hm-legend');
        legend.style.display = '';
        L.DomEvent.disableClickPropagation(legend);
        L.DomEvent.disableScrollPropagation(legend);

        const gradCSS = heatmapMode === 'rssi'
            ? 'linear-gradient(to right, #1e3a5f, #2563eb, #22d3ee, #fbbf24, #ef4444)'
            : 'linear-gradient(to right, #1e3a5f, #ef4444, #fbbf24, #34d399, #10b981)';
        document.getElementById('hm-legend-bar').style.background = gradCSS;

        if (heatmapMode === 'rssi') {
            document.getElementById('hm-legend-title').textContent = 'RSSI';
            document.getElementById('hm-label-low').textContent = '-115 dBm';
            document.getElementById('hm-label-high').textContent = '-75 dBm';
        } else {
            document.getElementById('hm-legend-title').textContent = 'SNR';
            document.getElementById('hm-label-low').textContent = '-15 dB';
            document.getElementById('hm-label-high').textContent = '+12 dB';
        }
    }

    // ══════════════════════════════════════════════
    // ── Map Search & Filters ──
    // ══════════════════════════════════════════════

    let filtersOpen = false;
    const mfBody = document.getElementById('mf-body');
    const mfChevron = document.getElementById('mf-chevron');
    const mfSearchInput = document.getElementById('mf-search-input');
    const mfNodeCount = document.getElementById('mf-node-count');

    // Toggle filter panel
    document.getElementById('mf-toggle').addEventListener('click', function() {
        filtersOpen = !filtersOpen;
        mfBody.style.display = filtersOpen ? '' : 'none';
        mfChevron.classList.toggle('open', filtersOpen);
    });

    // Prevent map interaction when interacting with the filter panel
    (function() {
        const panel = document.getElementById('map-filters');
        if (panel) {
            L.DomEvent.disableClickPropagation(panel);
            L.DomEvent.disableScrollPropagation(panel);
        }
    })();

    // State: which roles/hw are checked (null = not yet built, means "all")
    let checkedRoles = null;  // Set<string> or null
    let checkedHw = null;     // Set<string> or null

    // Build dynamic checkbox lists from nodeDir
    function rebuildFilterOptions() {
        if (!nodeDir) return;
        const roles = new Set();
        const hws = new Set();
        for (const entry of Object.values(nodeDir)) {
            if (entry.role) roles.add(entry.role);
            if (entry.hw_model) hws.add(entry.hw_model);
        }
        rebuildCheckboxGroup('mf-roles', 'mf-role', roles, checkedRoles);
        rebuildCheckboxGroup('mf-hw', 'mf-hwm', hws, checkedHw);
    }

    function rebuildCheckboxGroup(containerId, namePrefix, values, checkedSet) {
        const container = document.getElementById(containerId);
        if (!container) return;
        const sorted = Array.from(values).sort();
        // Preserve existing checked state
        const existing = {};
        container.querySelectorAll('input[type="checkbox"]').forEach(function(cb) {
            existing[cb.value] = cb.checked;
        });
        let html = '';
        for (const val of sorted) {
            // If checkedSet exists, use it; else if existed before, preserve; else default checked
            let isChecked;
            if (checkedSet !== null) {
                isChecked = checkedSet.has(val);
            } else if (val in existing) {
                isChecked = existing[val];
            } else {
                isChecked = true;
            }
            const label = namePrefix === 'mf-role' ? Mesh.formatRole(val) : Mesh.formatHwModel(val);
            html += '<label class="mf-option"><input type="checkbox" name="' + namePrefix + '" value="' + Mesh.esc(val) + '"'
                 + (isChecked ? ' checked' : '') + '> ' + Mesh.esc(label) + '</label>';
        }
        if (sorted.length === 0) {
            html = '<span style="font-size:10px;color:#4b5563;">No data</span>';
        }
        container.innerHTML = html;
        // Attach listeners
        container.querySelectorAll('input[type="checkbox"]').forEach(function(cb) {
            cb.addEventListener('change', function() { syncCheckedState(); applyMapFilters(); });
        });
    }

    // Read current checkbox state into checkedRoles / checkedHw
    function syncCheckedState() {
        checkedRoles = new Set();
        document.querySelectorAll('input[name="mf-role"]:checked').forEach(function(cb) {
            checkedRoles.add(cb.value);
        });
        checkedHw = new Set();
        document.querySelectorAll('input[name="mf-hwm"]:checked').forEach(function(cb) {
            checkedHw.add(cb.value);
        });
    }

    // Read radio filter values
    function getRadioValue(name) {
        const el = document.querySelector('input[name="' + name + '"]:checked');
        return el ? el.value : 'all';
    }

    // ── Core filter logic ──
    function nodePassesFilters(numStr, entry) {
        // Text search
        const q = mfSearchInput.value.toLowerCase().trim();
        if (q) {
            const hex = '!' + parseInt(numStr).toString(16).padStart(8, '0');
            const haystack = ((entry.short_name || '') + ' ' + (entry.long_name || '') + ' ' + hex + ' ' + (entry.user_id || '')).toLowerCase();
            if (!haystack.includes(q)) return false;
        }

        // Role filter
        if (checkedRoles !== null && checkedRoles.size > 0) {
            const role = entry.role || '';
            // If node has no role, only show if no roles are unchecked (i.e. all checked)
            if (role && !checkedRoles.has(role)) return false;
            // Count total available roles
            const totalRoles = document.querySelectorAll('input[name="mf-role"]').length;
            if (!role && checkedRoles.size < totalRoles) return false;
        }

        // Hardware model filter
        if (checkedHw !== null && checkedHw.size > 0) {
            const hw = entry.hw_model || '';
            if (hw && !checkedHw.has(hw)) return false;
            const totalHw = document.querySelectorAll('input[name="mf-hwm"]').length;
            if (!hw && checkedHw.size < totalHw) return false;
        }

        // Status filter
        const status = getRadioValue('mf-status');
        if (status !== 'all' && entry.last_heard) {
            const now = Math.floor(Date.now() / 1000);
            const age = now - entry.last_heard;
            const online = age < 900; // 15 minutes
            if (status === 'online' && !online) return false;
            if (status === 'offline' && online) return false;
        }
        // If no last_heard data: "online" filter hides them, "offline" shows them, "all" shows them
        if (status === 'online' && !entry.last_heard) return false;

        // Battery filter
        const battery = getRadioValue('mf-battery');
        if (battery === 'low') {
            if (!entry.battery_level || entry.battery_level >= 20) return false;
        } else if (battery === 'has') {
            if (!entry.battery_level) return false;
        }

        // MQTT filter
        const mqtt = getRadioValue('mf-mqtt');
        if (mqtt === 'direct' && entry.via_mqtt) return false;
        if (mqtt === 'mqtt' && !entry.via_mqtt) return false;

        return true;
    }

    function applyMapFilters() {
        if (!mapInstance || !nodeDir) return;

        let visible = 0;
        let total = 0;
        let singleMatch = null;

        for (const [numStr, entry] of Object.entries(nodeDir)) {
            if (!entry.latitude && !entry.longitude) continue;
            total++;

            const passes = nodePassesFilters(numStr, entry);
            if (passes) {
                visible++;
                singleMatch = numStr;
                // Show marker
                if (mapMarkers[numStr]) {
                    if (!mapInstance.hasLayer(mapMarkers[numStr])) {
                        mapMarkers[numStr].addTo(mapInstance);
                    }
                }
            } else {
                // Hide marker
                if (mapMarkers[numStr] && mapInstance.hasLayer(mapMarkers[numStr])) {
                    mapMarkers[numStr].remove();
                }
            }
        }

        // Update counter
        mfNodeCount.textContent = visible + ' / ' + total;

        // If exactly one match from text search, fly to it and open popup
        const q = mfSearchInput.value.trim();
        if (q && visible === 1 && singleMatch && mapMarkers[singleMatch]) {
            const entry = nodeDir[singleMatch];
            mapInstance.flyTo([entry.latitude, entry.longitude], 14, { duration: 0.5 });
            mapMarkers[singleMatch].openPopup();
        }
    }

    window.applyMapFilters = applyMapFilters;

    // ── Event listeners for filters ──
    mfSearchInput.addEventListener('input', applyMapFilters);

    // Radio buttons
    document.querySelectorAll('input[name="mf-status"], input[name="mf-battery"], input[name="mf-mqtt"]').forEach(function(el) {
        el.addEventListener('change', applyMapFilters);
    });

    // Heatmap radio buttons
    document.querySelectorAll('input[name="mf-heatmap"]').forEach(function(el) {
        el.addEventListener('change', function() {
            heatmapMode = getRadioValue('mf-heatmap');
            updateHeatmap();
        });
    });

    // Reset button
    document.getElementById('mf-reset').addEventListener('click', function() {
        mfSearchInput.value = '';
        document.querySelectorAll('#mf-body input[type="radio"][value="all"]').forEach(function(r) { r.checked = true; });
        document.querySelectorAll('#mf-body input[type="checkbox"]').forEach(function(cb) { cb.checked = true; });
        // Reset heatmap to off
        const hmOff = document.querySelector('input[name="mf-heatmap"][value="off"]');
        if (hmOff) hmOff.checked = true;
        heatmapMode = 'off';
        checkedRoles = null;
        checkedHw = null;
        applyMapFilters();
        updateHeatmap();
    });
})();
