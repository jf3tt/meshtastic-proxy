(function() {
    'use strict';

    var chatMessages = document.getElementById('chat-messages');
    var chatEmpty = document.getElementById('chat-empty');
    var chatInput = document.getElementById('chat-input');
    var chatSendBtn = document.getElementById('chat-send-btn');
    var chatChannelList = document.getElementById('chat-channel-list');
    var chatChannelHeader = document.getElementById('chat-channel-header');

    var chatInitialized = false;
    var allChatMessages = [];
    var chatChannels = [];
    var chatAutoScroll = true;
    var activeChannel = 0;

    // ── Initialize chat (called on first tab show) ──
    window.initChat = function() {
        if (chatInitialized) return;
        chatInitialized = true;
        loadChannels();
    };

    // ── Load channels from API ──
    function loadChannels() {
        fetch('/api/chat/channels')
            .then(function(r) { return r.json(); })
            .then(function(channels) {
                chatChannels = channels;
                renderSidebar(channels);
                if (channels.length > 0) {
                    selectChannel(channels[0].index);
                }
            })
            .catch(function(err) { console.error('Failed to load channels:', err); });
    }

    // ── Render channel sidebar ──
    function renderSidebar(channels) {
        chatChannelList.innerHTML = '';
        channels.forEach(function(ch) {
            var btn = document.createElement('button');
            btn.dataset.channel = ch.index;
            btn.className = 'chat-channel-btn w-full flex items-center gap-2 px-3 py-1.5 text-xs rounded-md transition-colors text-gray-400 hover:bg-gray-800/50 hover:text-gray-200';
            btn.innerHTML =
                '<span class="text-gray-600 text-[11px]">#</span>' +
                '<span class="truncate flex-1 text-left">' + Mesh.esc(ch.name || ('Ch ' + ch.index)) + '</span>' +
                '<span class="chat-channel-badge text-[10px] font-mono tabular-nums text-gray-600 flex-shrink-0"></span>';
            btn.addEventListener('click', function() { selectChannel(ch.index); });
            chatChannelList.appendChild(btn);
        });
    }

    // ── Select a channel ──
    function selectChannel(index) {
        activeChannel = index;

        // Update sidebar active state
        var btns = chatChannelList.querySelectorAll('.chat-channel-btn');
        btns.forEach(function(btn) {
            if (parseInt(btn.dataset.channel, 10) === index) {
                btn.className = 'chat-channel-btn w-full flex items-center gap-2 px-3 py-1.5 text-xs rounded-md transition-colors bg-gray-800 text-gray-100';
            } else {
                btn.className = 'chat-channel-btn w-full flex items-center gap-2 px-3 py-1.5 text-xs rounded-md transition-colors text-gray-400 hover:bg-gray-800/50 hover:text-gray-200';
            }
        });

        // Update channel header
        var ch = chatChannels.find(function(c) { return c.index === index; });
        chatChannelHeader.textContent = ch ? (ch.name || ('Ch ' + index)) : ('Ch ' + index);

        // Filter messages and update empty state
        applyChatFilter();
    }

    // ── Format timestamp ──
    function formatTime(ts) {
        var d = new Date(ts);
        var h = String(d.getHours()).padStart(2, '0');
        var m = String(d.getMinutes()).padStart(2, '0');
        var s = String(d.getSeconds()).padStart(2, '0');
        return h + ':' + m + ':' + s;
    }

    // ── Create message element ──
    function createMessageEl(msg) {
        var div = document.createElement('div');
        div.dataset.channel = msg.channel;

        var isOutgoing = msg.direction === 'outgoing';
        var isBroadcast = msg.to === 0xFFFFFFFF || msg.to === 4294967295;
        var isDM = !isBroadcast;

        var fromDisplay = msg.from_name || Mesh.nodeNameText(msg.from);
        var toDisplay = msg.to_name || Mesh.nodeNameText(msg.to);

        // Left border color: outgoing=indigo, DM=amber, broadcast incoming=blue
        var borderColor = 'border-l-blue-500/40';
        var nameColor = 'text-blue-400';
        if (isOutgoing) { borderColor = 'border-l-indigo-500/40'; nameColor = 'text-indigo-400'; }
        else if (isDM) { borderColor = 'border-l-amber-500/40'; nameColor = 'text-amber-400'; }

        div.className = 'chat-msg flex items-baseline gap-2 py-1 px-2 border-l-2 ' + borderColor + ' hover:bg-gray-800/30 rounded-r-sm transition-colors';

        // Pill badges
        var metaBadges = '';
        if (msg.via_mqtt) metaBadges += '<span class="text-[9px] font-bold uppercase px-1.5 py-px rounded bg-purple-500/15 text-purple-400 border border-purple-500/30">MQTT</span>';
        if (isDM) metaBadges += '<span class="text-[9px] font-bold uppercase px-1.5 py-px rounded bg-amber-500/15 text-amber-400 border border-amber-500/30">DM</span>';

        // Signal info
        var signalInfo = '';
        if (msg.rx_rssi && msg.rx_rssi !== 0) {
            signalInfo = '<span class="text-[10px] text-gray-600 tabular-nums">' + msg.rx_rssi + 'dBm';
            if (msg.rx_snr && msg.rx_snr !== 0) signalInfo += ' ' + msg.rx_snr.toFixed(1) + 'dB';
            signalInfo += '</span>';
        }

        // Direction arrow: colored
        var arrowSvg = isOutgoing ? Mesh.SVG_ARROW_OUT : Mesh.SVG_ARROW_IN;

        div.innerHTML =
            '<span class="text-gray-600 text-[10px] w-[3.2rem] flex-shrink-0 text-right tabular-nums">' + formatTime(msg.timestamp) + '</span>' +
            arrowSvg +
            '<span class="' + nameColor + ' font-semibold flex-shrink-0" title="' + Mesh.nodeHex(msg.from) + '">' + Mesh.esc(fromDisplay) + '</span>' +
            (isDM ? '<span class="text-gray-600 flex-shrink-0 text-[10px]">\u2192</span><span class="text-amber-300 flex-shrink-0" title="' + Mesh.nodeHex(msg.to) + '">' + Mesh.esc(toDisplay) + '</span>' : '') +
            '<span class="text-gray-300 break-all">' + Mesh.esc(msg.text) + '</span>' +
            (metaBadges || signalInfo ? '<span class="flex items-center gap-1 flex-shrink-0 ml-auto">' + metaBadges + signalInfo + '</span>' : '');

        return div;
    }

    // ── Auto-scroll detection ──
    chatMessages.addEventListener('scroll', function() {
        var threshold = 40;
        chatAutoScroll = (chatMessages.scrollHeight - chatMessages.scrollTop - chatMessages.clientHeight) < threshold;
    });

    window.scrollChatToBottom = function() {
        chatMessages.scrollTop = chatMessages.scrollHeight;
    };

    // ── Filter messages by active channel ──
    function applyChatFilter() {
        var msgEls = chatMessages.querySelectorAll('.chat-msg');
        var visible = 0;
        msgEls.forEach(function(el) {
            if (String(el.dataset.channel) === String(activeChannel)) {
                el.style.display = '';
                visible++;
            } else {
                el.style.display = 'none';
            }
        });
        chatEmpty.style.display = visible === 0 ? '' : 'none';
    }

    // ── Update badge counts in sidebar ──
    function updateBadges() {
        var counts = {};
        allChatMessages.forEach(function(msg) {
            counts[msg.channel] = (counts[msg.channel] || 0) + 1;
        });
        var btns = chatChannelList.querySelectorAll('.chat-channel-btn');
        btns.forEach(function(btn) {
            var ch = parseInt(btn.dataset.channel, 10);
            var badge = btn.querySelector('.chat-channel-badge');
            if (badge) {
                var count = counts[ch] || 0;
                badge.textContent = count > 0 ? count : '';
            }
        });
    }

    // ── Load chat history (from SSE initial event) ──
    window.loadChatHistory = function(messages) {
        if (!messages || messages.length === 0) return;
        allChatMessages = messages;
        // Clear existing
        chatMessages.querySelectorAll('.chat-msg').forEach(function(el) { el.remove(); });
        messages.forEach(function(msg) {
            chatMessages.appendChild(createMessageEl(msg));
        });
        chatEmpty.style.display = messages.length > 0 ? 'none' : '';
        updateBadges();
        applyChatFilter();
        scrollChatToBottom();
    };

    // ── Append single chat message (from SSE stream) ──
    window.appendChatMessage = function(msg) {
        allChatMessages.push(msg);
        // Trim to 500
        if (allChatMessages.length > 500) {
            allChatMessages.shift();
            var first = chatMessages.querySelector('.chat-msg');
            if (first) first.remove();
        }

        var el = createMessageEl(msg);
        chatMessages.appendChild(el);
        chatEmpty.style.display = 'none';

        // Apply current filter
        if (String(msg.channel) !== String(activeChannel)) {
            el.style.display = 'none';
        }

        updateBadges();

        if (chatAutoScroll && String(msg.channel) === String(activeChannel)) scrollChatToBottom();
    };

    // ── Send message ──
    function sendMessage() {
        var text = chatInput.value.trim();
        if (!text) return;

        var channel = activeChannel;
        var to = 0xFFFFFFFF; // always broadcast

        chatSendBtn.disabled = true;
        chatInput.disabled = true;

        fetch('/api/chat/send', {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ text: text, to: to, channel: channel })
        })
        .then(function(r) {
            if (!r.ok) return r.text().then(function(t) { throw new Error(t); });
            chatInput.value = '';
            updateSendButton();
            autoResizeTextarea();
        })
        .catch(function(err) {
            console.error('Send failed:', err);
        })
        .finally(function() {
            chatSendBtn.disabled = false;
            chatInput.disabled = false;
            chatInput.focus();
            updateSendButton();
            autoResizeTextarea();
        });
    }

    // ── Enable/disable send button based on input + character counter ──
    var chatCharCount = document.getElementById('chat-char-count');
    function updateSendButton() {
        var len = chatInput.value.length;
        chatSendBtn.disabled = chatInput.value.trim().length === 0;
        chatCharCount.textContent = (237 - len);
        if (len >= 237) chatCharCount.className = 'absolute right-3 bottom-2 text-[10px] font-mono tabular-nums pointer-events-none select-none text-red-400';
        else if (len > 200) chatCharCount.className = 'absolute right-3 bottom-2 text-[10px] font-mono tabular-nums pointer-events-none select-none text-amber-400';
        else chatCharCount.className = 'absolute right-3 bottom-2 text-[10px] font-mono tabular-nums pointer-events-none select-none text-gray-600';
    }

    // ── Textarea auto-resize (max 4 lines) ──
    function autoResizeTextarea() {
        chatInput.style.height = 'auto';
        var maxHeight = parseInt(getComputedStyle(chatInput).lineHeight, 10) * 4 + 16;
        chatInput.style.height = Math.min(chatInput.scrollHeight, maxHeight) + 'px';
        if (chatInput.scrollHeight > maxHeight) {
            chatInput.style.overflowY = 'auto';
        } else {
            chatInput.style.overflowY = 'hidden';
        }
    }

    chatInput.addEventListener('input', function() {
        updateSendButton();
        autoResizeTextarea();
    });
    chatInput.addEventListener('keydown', function(e) {
        if (e.key === 'Enter' && !e.shiftKey) {
            e.preventDefault();
            if (chatInput.value.trim()) sendMessage();
        }
    });
    chatSendBtn.addEventListener('click', sendMessage);
})();
