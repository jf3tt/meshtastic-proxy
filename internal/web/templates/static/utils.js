/**
 * Shared utilities for the Meshtastic Proxy dashboard.
 *
 * Provides a `Mesh` namespace with:
 *   - HTML-safe escaping
 *   - Node name / hex formatting
 *   - Hardware model, role, and age formatting
 *   - Reusable SVG icon constants
 *   - A lightweight event bus (replaces window monkey-patching)
 */
/* exported Mesh */

'use strict';

var Mesh = (function () {
    // ── HTML escape (createElement approach, safe against XSS) ──────
    function esc(s) {
        if (!s) return '';
        var el = document.createElement('span');
        el.textContent = s;
        return el.innerHTML;
    }

    // ── Node hex ID ─────────────────────────────────────────────────
    function nodeHex(n) {
        if (!n) return '';
        return '!' + n.toString(16).padStart(8, '0');
    }

    // ── Node name (HTML) — returns <span> with tooltip ──────────────
    function nodeName(n) {
        if (!n) return '';
        var hex = nodeHex(n);
        var key = n.toString();
        if (nodeDir && nodeDir[key] && nodeDir[key].short_name) {
            var entry = nodeDir[key];
            return '<span title="' + esc(hex) + ' (' + esc(entry.long_name || '') + ')" class="cursor-help">' + esc(entry.short_name) + '</span>';
        }
        return '<code class="font-mono text-xs text-gray-400">' + esc(hex) + '</code>';
    }

    // ── Node name (plain text) — for chat messages ──────────────────
    function nodeNameText(n) {
        if (n === 0xFFFFFFFF || n === 4294967295) return 'Broadcast';
        if (nodeDir && nodeDir[n] && nodeDir[n].short_name) return nodeDir[n].short_name;
        return nodeHex(n);
    }

    // ── nodeHexName — short name or hex, plain text ─────────────────
    function nodeHexName(num) {
        if (!num) return '?';
        var numStr = num.toString();
        var entry = nodeDir ? nodeDir[numStr] : null;
        if (entry && entry.short_name) return entry.short_name;
        return '!' + num.toString(16).padStart(8, '0');
    }

    // ── Hardware model formatting ───────────────────────────────────
    function formatHwModel(m) {
        if (!m) return '';
        return m.replace(/_/g, ' ').replace(/\w\S*/g, function (t) {
            return t.charAt(0).toUpperCase() + t.substr(1).toLowerCase();
        });
    }

    // ── Role formatting ─────────────────────────────────────────────
    function formatRole(r) {
        if (!r) return '';
        return r.charAt(0).toUpperCase() + r.slice(1).toLowerCase().replace(/_/g, ' ');
    }

    // ── Unix timestamp → relative age ───────────────────────────────
    function formatUnixAge(ts) {
        if (!ts) return '';
        var now = Math.floor(Date.now() / 1000);
        var diff = now - ts;
        if (diff < 0) return 'just now';
        if (diff < 60) return diff + 's ago';
        if (diff < 3600) return Math.floor(diff / 60) + 'm ago';
        if (diff < 86400) return Math.floor(diff / 3600) + 'h ago';
        return Math.floor(diff / 86400) + 'd ago';
    }

    // ── SVG icon constants ──────────────────────────────────────────

    var SVG_TRACE = '<svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" style="vertical-align:-1px;margin-right:4px;">'
        + '<circle cx="12" cy="12" r="10"/><line x1="22" y1="12" x2="18" y2="12"/><line x1="6" y1="12" x2="2" y2="12"/><line x1="12" y1="6" x2="12" y2="2"/><line x1="12" y1="22" x2="12" y2="18"/></svg>';

    var SVG_TRACE_COMPACT = '<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">'
        + '<circle cx="12" cy="12" r="10"/><line x1="22" y1="12" x2="18" y2="12"/><line x1="6" y1="12" x2="2" y2="12"/><line x1="12" y1="6" x2="12" y2="2"/><line x1="12" y1="22" x2="12" y2="18"/></svg>';

    var SVG_ARROW_OUT = '<svg class="w-3 h-3 text-indigo-500 flex-shrink-0" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round"><polyline points="9 18 15 12 9 6"/></svg>';

    var SVG_ARROW_IN = '<svg class="w-3 h-3 text-emerald-500 flex-shrink-0" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round"><polyline points="15 18 9 12 15 6"/></svg>';

    var SVG_MAP_PIN = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M21 10c0 7-9 13-9 13s-9-6-9-13a9 9 0 0 1 18 0z"/><circle cx="12" cy="10" r="3"/></svg>';

    var SVG_USER = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M20 21v-2a4 4 0 0 0-4-4H8a4 4 0 0 0-4 4v2"/><circle cx="12" cy="7" r="4"/></svg>';

    var SVG_ARCHIVE = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><polyline points="21 8 21 21 3 21 3 8"/><rect x="1" y="3" width="22" height="5"/><line x1="10" y1="12" x2="14" y2="12"/></svg>';

    // ── Lightweight event bus ───────────────────────────────────────
    // Replaces the monkey-patching pattern where Nodes IIFE wrapped
    // five window.* functions to trigger renderNodesTable().
    //
    // Usage:
    //   Mesh.bus.emit('nodesChanged')            — from app.js
    //   Mesh.bus.emit('nodeInfoChanged', info)    — from app.js
    //   Mesh.bus.on('nodesChanged', fn)           — in nodes.js

    var bus = (function () {
        var target = document.createElement('div'); // private event target

        function on(event, fn) {
            target.addEventListener('mesh:' + event, function (e) { fn(e.detail); });
        }

        function emit(event, data) {
            target.dispatchEvent(new CustomEvent('mesh:' + event, { detail: data }));
        }

        return { on: on, emit: emit };
    })();

    // ── Public API ──────────────────────────────────────────────────
    return {
        esc: esc,
        nodeHex: nodeHex,
        nodeName: nodeName,
        nodeNameText: nodeNameText,
        nodeHexName: nodeHexName,
        formatHwModel: formatHwModel,
        formatRole: formatRole,
        formatUnixAge: formatUnixAge,
        bus: bus,
        SVG_TRACE: SVG_TRACE,
        SVG_TRACE_COMPACT: SVG_TRACE_COMPACT,
        SVG_ARROW_OUT: SVG_ARROW_OUT,
        SVG_ARROW_IN: SVG_ARROW_IN,
        SVG_MAP_PIN: SVG_MAP_PIN,
        SVG_USER: SVG_USER,
        SVG_ARCHIVE: SVG_ARCHIVE,
    };
})();

// Backward-compatible global alias used by Nodes IIFE and trace history.
window.nodeHexName = Mesh.nodeHexName;
