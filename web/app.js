const API = '';

let currentEventSource = null;
let currentSessionId = null;
let lastEventCount = 0;

async function createSession() {
    const input = document.getElementById('goal-input');
    const btn = document.getElementById('submit-btn');
    const goal = input.value.trim();
    if (!goal) return;

    btn.disabled = true;
    btn.textContent = 'Running...';

    try {
        const resp = await fetch(API + '/api/sessions', {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ goal })
        });
        if (!resp.ok) {
            alert('Failed: ' + await resp.text());
            return;
        }
        const session = await resp.json();
        input.value = '';
        refreshSessions();
        openDetail(session.id);
    } catch (e) {
        alert('Network error: ' + e.message);
    } finally {
        btn.disabled = false;
        btn.textContent = 'Run';
    }
}

async function refreshSessions() {
    try {
        const resp = await fetch(API + '/api/sessions');
        const sessions = await resp.json();
        renderSessions(sessions);
    } catch (e) {
        console.error('refresh sessions error:', e);
    }
}

function renderSessions(sessions) {
    const list = document.getElementById('sessions-list');
    if (!sessions || sessions.length === 0) {
        list.innerHTML = '<p class="empty-hint">No sessions yet</p>';
        return;
    }
    list.innerHTML = sessions.reverse().map(s => {
        const statusClass = 'status-' + s.status;
        const statusText = { running: 'Running', completed: 'Done', error: 'Failed' }[s.status] || s.status;
        const time = new Date(s.started_at).toLocaleTimeString();
        const runningClass = s.status === 'running' ? ' running-pulse' : '';
        return `
            <div class="session-card${currentSessionId === s.id ? ' active' : ''}${runningClass}" onclick="openDetail('${s.id}')">
                <div class="goal">${esc(s.goal)}</div>
                <div class="meta">
                    <span class="time">${time}</span>
                    <span class="status-badge ${statusClass}">${statusText}</span>
                </div>
            </div>
        `;
    }).join('');
}

function openDetail(sessionId) {
    currentSessionId = sessionId;
    const detail = document.getElementById('session-detail');
    detail.style.display = 'flex';
    document.getElementById('events-list').innerHTML = '';
    document.getElementById('event-count').textContent = '0';
    lastEventCount = 0;

    fetchSessionDetail(sessionId);
    refreshSessions();

    if (currentEventSource) currentEventSource.close();
    currentEventSource = new EventSource(API + '/api/sessions/' + sessionId + '/stream');

    currentEventSource.onmessage = function(e) {
        try {
            const data = JSON.parse(e.data);
            if (data.type === 'done') {
                currentEventSource.close();
                currentEventSource = null;
                fetchSessionDetail(sessionId);
                refreshSessions();
                return;
            }
            if (data.type) appendEvent(data);
            if (data.goal) renderDetailHeader(data);
        } catch (err) {
            console.error('parse SSE error:', err);
        }
    };

    currentEventSource.onerror = function() {
        currentEventSource.close();
        currentEventSource = null;
        fetchSessionDetail(sessionId);
    };
}

function closeDetail() {
    document.getElementById('session-detail').style.display = 'none';
    currentSessionId = null;
    if (currentEventSource) { currentEventSource.close(); currentEventSource = null; }
    refreshSessions();
}

async function fetchSessionDetail(sessionId) {
    try {
        const resp = await fetch(API + '/api/sessions/' + sessionId);
        const session = await resp.json();
        renderDetailHeader(session);
        renderEvents(session.events || []);
        renderSessionInfo(session);
    } catch (e) {
        console.error('fetch detail error:', e);
    }
}

function renderDetailHeader(session) {
    document.getElementById('detail-goal').textContent = session.goal;
    const badge = document.getElementById('detail-status');
    const statusText = { running: 'Running', completed: 'Done', error: 'Failed' }[session.status] || session.status;
    badge.textContent = statusText;
    badge.className = 'status-badge status-' + session.status;
}

function renderEvents(events) {
    const list = document.getElementById('events-list');
    list.innerHTML = events.map((ev, idx) => eventHtml(ev, idx)).join('');
    document.getElementById('event-count').textContent = events.length;
    list.scrollTop = list.scrollHeight;
    lastEventCount = events.length;
}

function appendEvent(ev) {
    const list = document.getElementById('events-list');
    const idx = list.children.length;
    list.insertAdjacentHTML('beforeend', eventHtml(ev, idx));
    const count = list.children.length;
    document.getElementById('event-count').textContent = count;
    list.scrollTop = list.scrollHeight;
    lastEventCount = count;
}

function eventHtml(ev, idx) {
    const time = ev.timestamp ? new Date(ev.timestamp).toLocaleTimeString() : '';
    const kind = ev.kind || '';
    const cls = kind ? ('event-item event-progress event-kind-' + kind) : ('event-item event-' + (ev.type || ''));
    const kindLabel = kind ? `<span class="kind-badge kind-${kind}">${kind}</span>` : '';

    if (ev.type === 'tool_exec') {
        const statusIcon = ev.success ? '<span class="tool-success">OK</span>' : '<span class="tool-fail">FAIL</span>';
        const hasOutput = !!(ev.tool_output && ev.tool_output.length > 0);
        const hasError = !!(ev.tool_error && ev.tool_error.length > 0);
        const outputId = 'tool-out-' + idx;
        let output = '';
        if (hasOutput) {
            const short = ev.tool_output.length > 2000 ? ev.tool_output.slice(0, 2000) + '\n... (truncated)' : ev.tool_output;
            output = `<div id="${outputId}" class="tool-output" style="display:none">${esc(short)}</div>`;
        }
        let error = '';
        if (hasError) {
            error = `<div class="tool-error">${esc(ev.tool_error)}</div>`;
        }
        const toggleBtn = hasOutput ? `<button class="toggle-btn" onclick="toggleOutput('${outputId}',this)">show</button>` : '';
        return `
            <div class="${cls}">
                <div class="event-header">
                    <span class="event-time">${time}</span>
                    <span class="tool-name">${esc(ev.tool)}</span>
                    ${ev.tool_path ? '<span class="tool-path">' + esc(ev.tool_path) + '</span>' : ''}
                    ${toggleBtn}
                    ${statusIcon}
                </div>
                ${output}${error}
            </div>
        `;
    }

    // Simple markdown: code blocks, bold, newlines
    const msgHtml = simpleMarkdown(ev.message || '');

    return `
        <div class="${cls}">
            <div class="event-header">
                <span class="event-time">${time}</span>
                <span class="event-agent">${esc(ev.agent || '')}</span>
                ${kindLabel}
            </div>
            <div class="event-msg">${msgHtml}</div>
        </div>
    `;
}

function simpleMarkdown(text) {
    if (!text) return '';
    // Escape first
    let html = esc(text);
    // Code blocks ``` ... ```
    html = html.replace(/```(\w*)\n([\s\S]*?)```/g, '<pre class="code-block"><code>$2</code></pre>');
    // Inline code `...`
    html = html.replace(/`([^`]+)`/g, '<code class="inline-code">$1</code>');
    // Bold **...**
    html = html.replace(/\*\*([^*]+)\*\*/g, '<strong>$1</strong>');
    // Newlines
    html = html.replace(/\n/g, '<br>');
    return html;
}

function toggleOutput(id, btn) {
    const el = document.getElementById(id);
    if (!el) return;
    if (el.style.display === 'none') {
        el.style.display = 'block';
        btn.textContent = 'hide';
    } else {
        el.style.display = 'none';
        btn.textContent = 'show';
    }
}

function renderSessionInfo(session) {
    const info = document.getElementById('session-info');
    const resultHtml = session.result ? simpleMarkdown(session.result) : '-';
    const rows = [
        ['Session', session.id],
        ['Status', { running: 'Running', completed: 'Done', error: 'Failed' }[session.status] || session.status],
        ['Started', session.started_at ? new Date(session.started_at).toLocaleString() : '-'],
        ['Ended', session.ended_at ? new Date(session.ended_at).toLocaleString() : '-'],
    ];
    info.innerHTML = rows.map(r => `
        <div class="info-row">
            <span class="label">${r[0]}</span>
            <span class="value">${esc(r[1])}</span>
        </div>
    `).join('') + `
        <div class="info-row result-row">
            <span class="label">Result</span>
        </div>
        <div class="result-body">${resultHtml}</div>
    `;
}

function esc(s) {
    const d = document.createElement('div');
    d.textContent = s || '';
    return d.innerHTML;
}

document.addEventListener('DOMContentLoaded', function() {
    document.getElementById('goal-input').addEventListener('keydown', function(e) {
        if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); createSession(); }
    });
    refreshSessions();
    setInterval(refreshSessions, 5000);
});
