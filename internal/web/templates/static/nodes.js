(function() {
    'use strict';

    var nodesInitialized = false;
    var nodesFilterMode = 'all'; // 'all' or 'realtime'
    var nodesFilterMqtt = 'all'; // 'all', 'direct', 'mqtt'
    var nodesFilterPosition = 'all'; // 'all', 'gps'
    var nodesFilterRoles = new Set(); // empty = show all roles
    var nodesFiltersOpen = false;
    var nodesSortKey = 'short_name';
    var nodesSortDir = 'asc'; // 'asc' or 'desc'

    var nodesSearch = document.getElementById('nodes-search');
    var nodesTbody = document.getElementById('nodes-tbody');
    var nodesCount = document.getElementById('nodes-count');
    var nodesEmpty = document.getElementById('nodes-empty');

    // ── Initialize nodes tab ──
    window.initNodesTab = function() {
        if (nodesInitialized) {
            renderNodesTable();
            return;
        }
        nodesInitialized = true;

        // Attach sort handlers
        document.querySelectorAll('.nodes-th[data-sort]').forEach(function(th) {
            th.addEventListener('click', function() {
                var key = th.dataset.sort;
                if (nodesSortKey === key) {
                    nodesSortDir = nodesSortDir === 'asc' ? 'desc' : 'asc';
                } else {
                    nodesSortKey = key;
                    nodesSortDir = 'asc';
                }
                updateSortIcons();
                renderNodesTable();
            });
        });

        nodesSearch.addEventListener('input', renderNodesTable);

        updateSortIcons();
        if (nodesFiltersOpen) buildRoleCheckboxes();
        renderNodesTable();
    };

    // ── Filter toggle ──
    window.setNodesFilter = function(mode) {
        nodesFilterMode = mode;
        var btnAll = document.getElementById('nodes-filter-all');
        var btnRT = document.getElementById('nodes-filter-realtime');
        if (mode === 'all') {
            btnAll.className = 'nodes-filter-btn text-[10px] font-semibold px-2.5 py-1 rounded-md transition-colors bg-gray-700 text-gray-100';
            btnRT.className = 'nodes-filter-btn text-[10px] font-semibold px-2.5 py-1 rounded-md transition-colors text-gray-400 hover:text-gray-200';
        } else {
            btnRT.className = 'nodes-filter-btn text-[10px] font-semibold px-2.5 py-1 rounded-md transition-colors bg-gray-700 text-gray-100';
            btnAll.className = 'nodes-filter-btn text-[10px] font-semibold px-2.5 py-1 rounded-md transition-colors text-gray-400 hover:text-gray-200';
        }
        renderNodesTable();
    };

    // ── Filters panel ──
    window.toggleNodesFilters = function() {
        nodesFiltersOpen = !nodesFiltersOpen;
        var panel = document.getElementById('nodes-filters-panel');
        var btn = document.getElementById('nodes-filters-btn');
        if (nodesFiltersOpen) {
            panel.classList.remove('hidden');
            btn.classList.add('active');
            buildRoleCheckboxes();
        } else {
            panel.classList.add('hidden');
            btn.classList.remove('active');
        }
    };

    window.setNodesFilterMqtt = function(val) {
        nodesFilterMqtt = val;
        updateFiltersIndicator();
        renderNodesTable();
    };

    window.setNodesFilterPosition = function(val) {
        nodesFilterPosition = val;
        updateFiltersIndicator();
        renderNodesTable();
    };

    function handleRoleCheckbox(role, checked) {
        if (checked) {
            nodesFilterRoles.add(role);
        } else {
            nodesFilterRoles.delete(role);
        }
        updateFiltersIndicator();
        renderNodesTable();
    }

    window.resetNodesFilters = function() {
        nodesFilterMqtt = 'all';
        nodesFilterPosition = 'all';
        nodesFilterRoles.clear();
        // Reset radio buttons
        var mqttAll = document.querySelector('input[name="nf-mqtt"][value="all"]');
        var posAll = document.querySelector('input[name="nf-pos"][value="all"]');
        if (mqttAll) mqttAll.checked = true;
        if (posAll) posAll.checked = true;
        // Uncheck all role checkboxes
        document.querySelectorAll('#nf-role-checks input[type="checkbox"]').forEach(function(cb) {
            cb.checked = false;
        });
        updateFiltersIndicator();
        renderNodesTable();
    };

    function buildRoleCheckboxes() {
        var container = document.getElementById('nf-role-checks');
        if (!container || !nodeDir) return;
        // Collect unique roles
        var roles = {};
        for (var numStr in nodeDir) {
            if (!nodeDir.hasOwnProperty(numStr)) continue;
            var r = nodeDir[numStr].role || '';
            if (r) roles[r] = (roles[r] || 0) + 1;
        }
        var sortedRoles = Object.keys(roles).sort();
        if (sortedRoles.length === 0) {
            container.innerHTML = '<span class="text-[10px] text-gray-600">No roles</span>';
            return;
        }
        var html = '';
        for (var i = 0; i < sortedRoles.length; i++) {
            var role = sortedRoles[i];
            var checked = nodesFilterRoles.has(role) ? ' checked' : '';
            html += '<label class="nf-option"><input type="checkbox" value="' + role + '"'
                + checked + ' onchange="handleNfRole(this)"> '
                + Mesh.formatRole(role) + ' <span class="text-[10px] text-gray-600">(' + roles[role] + ')</span></label>';
        }
        container.innerHTML = html;
    }

    // Global handler for role checkboxes (called from onclick in dynamic HTML)
    window.handleNfRole = function(cb) {
        handleRoleCheckbox(cb.value, cb.checked);
    };

    function updateFiltersIndicator() {
        var dot = document.getElementById('nodes-filters-dot');
        if (!dot) return;
        var active = nodesFilterMqtt !== 'all' || nodesFilterPosition !== 'all' || nodesFilterRoles.size > 0;
        if (active) {
            dot.classList.remove('hidden');
        } else {
            dot.classList.add('hidden');
        }
    }

    // ── Sort icons ──
    function updateSortIcons() {
        document.querySelectorAll('.nodes-sort-icon').forEach(function(icon) {
            icon.className = 'nodes-sort-icon';
        });
        var activeTh = document.querySelector('.nodes-th[data-sort="' + nodesSortKey + '"] .nodes-sort-icon');
        if (activeTh) {
            activeTh.classList.add(nodesSortDir);
        }
    }

    // ── Render table ──
    window.renderNodesTable = function() {
        if (!nodeDir) {
            nodesEmpty.classList.remove('hidden');
            nodesCount.textContent = '0 nodes';
            return;
        }

        var searchTerm = (nodesSearch.value || '').toLowerCase().trim();

        // Collect entries
        var entries = [];
        for (var numStr in nodeDir) {
            if (!nodeDir.hasOwnProperty(numStr)) continue;
            var entry = nodeDir[numStr];
            var num = parseInt(numStr, 10);

            // Filter: realtime only
            if (nodesFilterMode === 'realtime' && !entry.seen_realtime) continue;

            // Filter: MQTT
            if (nodesFilterMqtt === 'direct' && entry.via_mqtt) continue;
            if (nodesFilterMqtt === 'mqtt' && !entry.via_mqtt) continue;

            // Filter: Position
            if (nodesFilterPosition === 'gps' && !entry.latitude && !entry.longitude) continue;

            // Filter: Role
            if (nodesFilterRoles.size > 0 && !nodesFilterRoles.has(entry.role || '')) continue;

            // Filter: search
            if (searchTerm) {
                var hex = '!' + ('00000000' + (num >>> 0).toString(16)).slice(-8);
                var haystack = ((entry.short_name || '') + ' ' + (entry.long_name || '') + ' ' + hex + ' ' + (entry.user_id || '') + ' ' + (entry.hw_model || '') + ' ' + (entry.role || '')).toLowerCase();
                if (haystack.indexOf(searchTerm) === -1) continue;
            }

            entries.push({num: num, numStr: numStr, entry: entry});
        }

        // Sort — favorites always pinned to the top
        entries.sort(function(a, b) {
            var fa = a.entry.is_favorite ? 0 : 1;
            var fb = b.entry.is_favorite ? 0 : 1;
            if (fa !== fb) return fa - fb;
            var va = getSortValue(a.entry, a.num, nodesSortKey);
            var vb = getSortValue(b.entry, b.num, nodesSortKey);
            var cmp;
            if (typeof va === 'number' && typeof vb === 'number') {
                cmp = va - vb;
            } else {
                cmp = String(va).localeCompare(String(vb));
            }
            return nodesSortDir === 'desc' ? -cmp : cmp;
        });

        // Build HTML
        var html = '';
        for (var i = 0; i < entries.length; i++) {
            html += buildNodeRow(entries[i].num, entries[i].numStr, entries[i].entry);
        }

        nodesTbody.innerHTML = html;

        // Counter
        var totalNodes = Object.keys(nodeDir).length;
        var hasFilters = nodesFilterMode !== 'all' || nodesFilterMqtt !== 'all' || nodesFilterPosition !== 'all' || nodesFilterRoles.size > 0 || searchTerm;
        var suffix = nodesFilterMode === 'realtime' ? ' realtime' : '';
        nodesCount.textContent = entries.length + (hasFilters ? ' / ' + totalNodes : '') + ' nodes' + suffix;

        // Empty state
        if (entries.length === 0) {
            nodesEmpty.classList.remove('hidden');
        } else {
            nodesEmpty.classList.add('hidden');
        }
    };

    function getSortValue(entry, num, key) {
        switch (key) {
            case 'short_name': return (entry.short_name || '').toLowerCase() || '!' + num.toString(16);
            case 'user_id': return entry.user_id || '!' + ('00000000' + (num >>> 0).toString(16)).slice(-8);
            case 'hw_model': return (entry.hw_model || '').toLowerCase();
            case 'role': return (entry.role || '').toLowerCase();
            case 'last_heard': return entry.last_heard || 0;
            case 'hops_away': return entry.hops_away || 9999;
            case 'rx_rssi': return entry.rx_rssi || -999;
            case 'rx_snr': return entry.rx_snr || entry.snr || -999;
            case 'battery_level': return entry.battery_level || -1;
            default: return '';
        }
    }

    function buildNodeRow(num, numStr, entry) {
        var hex = '!' + ('00000000' + (num >>> 0).toString(16)).slice(-8);
        var rt = entry.seen_realtime;

        // Name
        var nameCell = '';
        if (rt) nameCell += '<span class="nodes-rt-dot" title="Seen in real-time"></span>';
        if (entry.short_name) {
            nameCell += '<span class="font-semibold text-gray-100" title="' + Mesh.esc(entry.long_name || '') + '">' + Mesh.esc(entry.short_name) + '</span>';
            if (entry.long_name && entry.long_name !== entry.short_name) {
                nameCell += ' <span class="text-gray-500 text-[11px]">' + Mesh.esc(entry.long_name) + '</span>';
            }
        } else {
            nameCell += '<code class="font-mono text-xs text-gray-400">' + Mesh.esc(hex) + '</code>';
        }

        // Badges
        if (entry.is_licensed) nameCell += ' <span class="text-[9px] font-bold px-1 py-px rounded bg-amber-500/15 text-amber-400 border border-amber-500/30">HAM</span>';
        if (entry.via_mqtt) nameCell += ' <span class="text-[9px] font-bold px-1 py-px rounded bg-purple-500/15 text-purple-400 border border-purple-500/30">MQTT</span>';
        if (entry.is_favorite) nameCell += ' <span class="text-[9px] font-bold px-1 py-px rounded bg-pink-500/15 text-pink-400 border border-pink-500/30">FAV</span>';

        // ID
        var idCell = '<code class="font-mono text-[11px] text-gray-400">' + Mesh.esc(entry.user_id || hex) + '</code>';

        // Hardware
        var hwCell = entry.hw_model ? '<span class="text-gray-300">' + Mesh.esc(Mesh.formatHwModel(entry.hw_model)) + '</span>' : '<span class="text-gray-600">&mdash;</span>';

        // Role
        var roleCell = entry.role ? '<span class="text-gray-300">' + Mesh.esc(Mesh.formatRole(entry.role)) + '</span>' : '<span class="text-gray-600">&mdash;</span>';

        // Last heard
        var lhCell;
        if (entry.last_heard) {
            var now = Math.floor(Date.now() / 1000);
            var age = now - entry.last_heard;
            var online = age < 900;
            var ageStr = Mesh.formatUnixAge(entry.last_heard);
            var color = online ? 'text-emerald-400' : (age < 3600 ? 'text-gray-300' : 'text-gray-500');
            lhCell = '<span class="' + color + ' tabular-nums">' + ageStr + '</span>';
        } else {
            lhCell = '<span class="text-gray-600">&mdash;</span>';
        }

        // Hops
        var hopsCell = (entry.hops_away !== undefined && entry.hops_away > 0)
            ? '<code class="font-mono text-xs text-gray-300">' + entry.hops_away + '</code>'
            : '<span class="text-gray-600">&mdash;</span>';

        // RSSI
        var rssiCell;
        if (entry.rx_rssi) {
            var rssiColor = entry.rx_rssi > -90 ? 'text-emerald-400' : (entry.rx_rssi > -110 ? 'text-amber-400' : 'text-red-400');
            rssiCell = '<code class="font-mono text-xs ' + rssiColor + '">' + entry.rx_rssi + '</code>';
        } else {
            rssiCell = '<span class="text-gray-600">&mdash;</span>';
        }

        // SNR
        var snrVal = entry.rx_snr || entry.snr;
        var snrCell;
        if (snrVal) {
            var snrColor = snrVal > 5 ? 'text-emerald-400' : (snrVal > 0 ? 'text-amber-400' : 'text-red-400');
            snrCell = '<code class="font-mono text-xs ' + snrColor + '">' + snrVal.toFixed(1) + '</code>';
        } else {
            snrCell = '<span class="text-gray-600">&mdash;</span>';
        }

        // Battery
        var battCell;
        if (entry.battery_level) {
            var pct = Math.min(entry.battery_level, 100);
            var bColor = pct > 50 ? '#34d399' : (pct > 20 ? '#fbbf24' : '#f87171');
            battCell = '<span class="nodes-battery-bar"><span class="nodes-battery-fill" style="width:' + pct + '%;background:' + bColor + '"></span></span>'
                + '<span class="text-xs tabular-nums text-gray-300">' + pct + '%</span>';
            if (entry.voltage) {
                battCell += ' <span class="text-[10px] text-gray-500">' + entry.voltage.toFixed(1) + 'V</span>';
            }
        } else {
            battCell = '<span class="text-gray-600">&mdash;</span>';
        }

        // Coordinates
        var coordCell;
        if (entry.latitude || entry.longitude) {
            coordCell = '<code class="font-mono text-[11px] text-gray-300">'
                + entry.latitude.toFixed(5) + ', ' + entry.longitude.toFixed(5) + '</code>';
            if (entry.altitude) {
                coordCell += ' <span class="text-[10px] text-gray-500">' + entry.altitude + 'm</span>';
            }
        } else {
            coordCell = '<span class="text-gray-600">&mdash;</span>';
        }

        // Actions (trace button + three-dot menu)
        var ts = getTraceState(numStr);
        var traceBtnContent;
        if (ts.coolingThis) {
            traceBtnContent = ts.remaining + 's';
        } else if (ts.tracingThis) {
            traceBtnContent = '<span class="spinner"></span>';
        } else {
            traceBtnContent = Mesh.SVG_TRACE_COMPACT;
        }
        var actionsCell = '<div style="position:relative;display:flex;align-items:center;gap:2px;">'
            + '<button class="na-trace-btn" data-trace-target="' + numStr + '"'
            + (ts.active ? ' disabled' : '')
            + ' onclick="event.stopPropagation();startTraceroute(' + numStr + ')" title="Trace Route">'
            + traceBtnContent + '</button>'
            + '<button class="na-btn" onclick="event.stopPropagation();toggleNodeActions(\'' + numStr + '\',this)" title="Actions">&#8942;</button>'
            + '</div>';

        return '<tr class="hover:bg-gray-800/30 transition-colors" data-nodenum="' + numStr + '">'
            + '<td class="nodes-td">' + nameCell + '</td>'
            + '<td class="nodes-td">' + idCell + '</td>'
            + '<td class="nodes-td">' + hwCell + '</td>'
            + '<td class="nodes-td">' + roleCell + '</td>'
            + '<td class="nodes-td">' + lhCell + '</td>'
            + '<td class="nodes-td text-center">' + hopsCell + '</td>'
            + '<td class="nodes-td text-right">' + rssiCell + '</td>'
            + '<td class="nodes-td text-right">' + snrCell + '</td>'
            + '<td class="nodes-td">' + battCell + '</td>'
            + '<td class="nodes-td">' + coordCell + '</td>'
            + '<td class="nodes-td">' + actionsCell + '</td>'
            + '</tr>';
    }

    // ── Node Actions dropdown ──
    var activeDropdown = null;

    window.toggleNodeActions = function(numStr, btnEl) {
        // Close any existing dropdown
        closeNodeActions();

        var num = parseInt(numStr, 10);
        var entry = nodeDir ? nodeDir[numStr] : {};
        var favLabel = (entry && entry.is_favorite) ? 'Remove Favorite' : 'Add Favorite';
        var favIcon = '<svg viewBox="0 0 24 24" fill="' + (entry && entry.is_favorite ? 'currentColor' : 'none') + '" stroke="currentColor" stroke-width="2"><path d="M20.84 4.61a5.5 5.5 0 0 0-7.78 0L12 5.67l-1.06-1.06a5.5 5.5 0 0 0-7.78 7.78l1.06 1.06L12 21.23l7.78-7.78 1.06-1.06a5.5 5.5 0 0 0 0-7.78z"/></svg>';

        var dd = document.createElement('div');
        dd.className = 'na-dropdown';
        dd.innerHTML = ''
            + '<div class="na-item" onclick="nodeAction(\'position\',' + num + ')">'
            + Mesh.SVG_MAP_PIN
            + 'Request Position</div>'
            + '<div class="na-item" onclick="nodeAction(\'nodeinfo\',' + num + ')">'
            + Mesh.SVG_USER
            + 'Request NodeInfo</div>'
            + '<div class="na-item" onclick="nodeAction(\'storeforward\',' + num + ')">'
            + Mesh.SVG_ARCHIVE
            + 'Store &amp; Forward</div>'
            + '<div class="na-sep"></div>'
            + '<div class="na-item na-fav" onclick="nodeAction(\'favorite\',' + num + ')">'
            + favIcon + favLabel + '</div>';

        btnEl.parentElement.appendChild(dd);
        activeDropdown = dd;

        // Close on outside click (deferred so this click doesn't close it)
        setTimeout(function() {
            document.addEventListener('click', closeNodeActions, {once: true});
        }, 0);
    };

    function closeNodeActions() {
        if (activeDropdown) {
            activeDropdown.remove();
            activeDropdown = null;
        }
    }

    window.nodeAction = function(action, nodeNum) {
        closeNodeActions();
        var numStr = nodeNum.toString();
        var entry = nodeDir ? nodeDir[numStr] : null;

        switch (action) {
            case 'position':
                fetch('/api/request-position', {
                    method: 'POST',
                    headers: {'Content-Type': 'application/json'},
                    body: JSON.stringify({target: nodeNum}),
                }).then(function(r) {
                    if (!r.ok) throw new Error('Request failed');
                }).catch(function(err) { console.error('Position request failed:', err); });
                break;
            case 'nodeinfo':
                fetch('/api/request-nodeinfo', {
                    method: 'POST',
                    headers: {'Content-Type': 'application/json'},
                    body: JSON.stringify({target: nodeNum}),
                }).then(function(r) {
                    if (!r.ok) throw new Error('Request failed');
                }).catch(function(err) { console.error('NodeInfo request failed:', err); });
                break;
            case 'storeforward':
                fetch('/api/store-forward', {
                    method: 'POST',
                    headers: {'Content-Type': 'application/json'},
                    body: JSON.stringify({target: nodeNum}),
                }).then(function(r) {
                    if (!r.ok) throw new Error('Request failed');
                }).catch(function(err) { console.error('Store & Forward request failed:', err); });
                break;
            case 'favorite':
                var newFav = !(entry && entry.is_favorite);
                fetch('/api/favorite', {
                    method: 'POST',
                    headers: {'Content-Type': 'application/json'},
                    body: JSON.stringify({node_num: nodeNum, is_favorite: newFav}),
                }).then(function(r) {
                    if (!r.ok) throw new Error('Request failed');
                    // Optimistic update
                    if (nodeDir && nodeDir[numStr]) {
                        nodeDir[numStr].is_favorite = newFav;
                        renderNodesTable();
                    }
                }).catch(function(err) { console.error('Favorite toggle failed:', err); });
                break;
        }
    };

    // ── Trace History panel ──
    var traceHistoryOpen = false;
    var traceHistoryData = [];

    window.toggleTraceHistory = function() {
        var panel = document.getElementById('trace-history-panel');
        var btn = document.getElementById('trace-history-btn');
        traceHistoryOpen = !traceHistoryOpen;
        if (traceHistoryOpen) {
            panel.style.display = '';
            btn.classList.add('active');
        } else {
            panel.style.display = 'none';
            btn.classList.remove('active');
        }
    };

    function renderTraceHistory() {
        var list = document.getElementById('trace-history-list');
        var empty = document.getElementById('trace-history-empty');
        var countEl = document.getElementById('trace-history-count');

        if (!traceHistoryData || traceHistoryData.length === 0) {
            list.innerHTML = '';
            empty.style.display = '';
            if (countEl) countEl.textContent = '';
            return;
        }

        empty.style.display = 'none';
        if (countEl) countEl.textContent = '(' + traceHistoryData.length + ')';

        // Render newest first
        var html = '';
        for (var i = traceHistoryData.length - 1; i >= 0; i--) {
            var t = traceHistoryData[i];
            var ts = t.timestamp ? new Date(t.timestamp) : new Date();
            var timeStr = ts.toLocaleTimeString([], {hour:'2-digit', minute:'2-digit', second:'2-digit'});

            // Timeout entry: show target node + "No response" badge
            if (t.timeout) {
                var targetName = Mesh.esc(Mesh.nodeHexName(t.from));
                html += '<div class="th-entry">'
                    + '<span class="th-time">' + timeStr + '</span>'
                    + '<span class="th-route">'
                    + '<span class="th-hop th-hop-end">' + targetName + '</span>'
                    + '<span class="th-hops-badge" style="color:#f87171;border-color:rgba(248,113,113,0.3);background:rgba(248,113,113,0.08);">No response</span>'
                    + '</span></div>';
                continue;
            }

            // Build forward hop chain: to -> route[0] -> ... -> from
            var hops = [t.to];
            var routeLen = t.route ? t.route.length : 0;
            if (t.route) { for (var j = 0; j < t.route.length; j++) hops.push(t.route[j]); }
            hops.push(t.from);

            // snr_towards[k] is the SNR at which route[k] received the packet.
            var snr = t.snr_towards || [];

            var routeHtml = '';
            for (var k = 0; k < hops.length; k++) {
                if (k > 0) {
                    routeHtml += '<span class="th-arrow">&rarr;</span>';
                }
                var titleAttr = '';
                if (k > 0) {
                    var snrIdx = k - 1;
                    if (snrIdx < snr.length && snr[snrIdx] !== 0) {
                        titleAttr = ' title="SNR: ' + (snr[snrIdx] / 4).toFixed(1) + ' dB"';
                    }
                }
                var cls = k === 0 ? 'th-hop th-hop-start' : (k === hops.length - 1 ? 'th-hop th-hop-end' : 'th-hop');
                routeHtml += '<span class="' + cls + '"' + titleAttr + '>' + Mesh.esc(Mesh.nodeHexName(hops[k])) + '</span>';
            }

            var hopCount = hops.length - 1;
            routeHtml += '<span class="th-hops-badge">' + hopCount + ' hop' + (hopCount !== 1 ? 's' : '') + '</span>';

            html += '<div class="th-entry" onclick="showTraceOnMap(' + i + ')">'
                + '<span class="th-time">' + timeStr + '</span>'
                + '<span class="th-route">' + routeHtml + '</span>'
                + '</div>';

            // Return route: from -> route_back[0] -> ... -> to (displayed reversed)
            if (t.route_back && t.route_back.length > 0) {
                var backHops = [t.from];
                for (var j = 0; j < t.route_back.length; j++) backHops.push(t.route_back[j]);
                backHops.push(t.to);
                var snrB = t.snr_back || [];

                var backHtml = '';
                for (var k = backHops.length - 1; k >= 0; k--) {
                    if (k < backHops.length - 1) {
                        backHtml += '<span class="th-arrow">&larr;</span>';
                    }
                    var titleAttr = '';
                    // snr_back[j] corresponds to route_back[j]; in backHops: index j+1
                    // reversed display: backHops[k] where k=backHops.length-1 is to (start), k=0 is from (end)
                    // backHops[k] for 1 <= k <= route_back.length maps to route_back[k-1] / snr_back[k-1]
                    if (k >= 1 && k <= t.route_back.length) {
                        var sIdx = k - 1;
                        if (sIdx < snrB.length && snrB[sIdx] !== 0) {
                            titleAttr = ' title="SNR: ' + (snrB[sIdx] / 4).toFixed(1) + ' dB"';
                        }
                    }
                    var cls = k === backHops.length - 1 ? 'th-hop th-hop-start' : (k === 0 ? 'th-hop th-hop-end' : 'th-hop');
                    backHtml += '<span class="' + cls + '"' + titleAttr + '>' + Mesh.esc(Mesh.nodeHexName(backHops[k])) + '</span>';
                }
                backHtml += '<span class="th-hops-badge" style="color:#6b7280;border-color:rgba(107,114,128,0.2);background:rgba(107,114,128,0.1);">return</span>';

                html += '<div class="th-entry" onclick="showTraceOnMap(' + i + ')" style="padding-top:0;border-bottom-width:0;">'
                    + '<span class="th-time"></span>'
                    + '<span class="th-route" style="opacity:0.6;">' + backHtml + '</span>'
                    + '</div>';
            }
        }
        list.innerHTML = html;
    }

    window.showTraceOnMap = function(idx) {
        if (idx < 0 || idx >= traceHistoryData.length) return;
        drawTraceroute(traceHistoryData[idx]);
    };

    // Load initial trace history from SSE
    window.loadTraceHistory = function(data) {
        if (Array.isArray(data)) {
            traceHistoryData = data;
            renderTraceHistory();
        }
    };

    // Append new trace result and auto-open the panel
    window.appendTraceHistory = function(entry) {
        traceHistoryData.push(entry);
        renderTraceHistory();

        // Auto-open the panel if it is closed
        if (!traceHistoryOpen) {
            var panel = document.getElementById('trace-history-panel');
            var btn = document.getElementById('trace-history-btn');
            traceHistoryOpen = true;
            panel.style.display = '';
            btn.classList.add('active');
        }

        // Scroll to top since newest entries render first
        var panel = document.getElementById('trace-history-panel');
        panel.scrollTop = 0;
    };

    // ── Periodic refresh: keep "Last Heard" relative times up-to-date ──
    setInterval(function() {
        if (nodesInitialized && !document.getElementById('tab-nodes').classList.contains('hidden')) {
            renderNodesTable();
        }
    }, 30000);

    // ── SSE integration via EventBus ──
    // When app.js updates node data it emits events on Mesh.bus.
    // We subscribe here instead of monkey-patching window.* functions.

    Mesh.bus.on('nodesChanged', function() {
        if (nodesInitialized && !document.getElementById('tab-nodes').classList.contains('hidden')) {
            renderNodesTable();
        }
    });

    Mesh.bus.on('nodeInfoChanged', function(info) {
        if (nodesInitialized && !document.getElementById('tab-nodes').classList.contains('hidden')) {
            if (nodesFiltersOpen && info && info.role) buildRoleCheckboxes();
            renderNodesTable();
        }
    });
})();
