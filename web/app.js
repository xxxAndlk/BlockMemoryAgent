const API = '';

let currentEventSource = null;
let currentSessionId = null;

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
        return `
            <div class="session-card${currentSessionId === s.id ? ' active' : ''}" onclick="openDetail('${s.id}')">
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
    list.innerHTML = events.map(ev => eventHtml(ev)).join('');
    document.getElementById('event-count').textContent = events.length;
    list.scrollTop = list.scrollHeight;
}

function appendEvent(ev) {
    const list = document.getElementById('events-list');
    list.insertAdjacentHTML('beforeend', eventHtml(ev));
    const count = list.children.length;
    document.getElementById('event-count').textContent = count;
    list.scrollTop = list.scrollHeight;
}

function eventHtml(ev) {
    const time = ev.timestamp ? new Date(ev.timestamp).toLocaleTimeString() : '';
    const cls = 'event-item event-' + (ev.type || '');

    if (ev.type === 'tool_exec') {
        const statusIcon = ev.success ? '<span class="tool-success">OK</span>' : '<span class="tool-fail">FAIL</span>';
        let output = '';
        if (ev.tool_output) {
            const short = ev.tool_output.length > 2000 ? ev.tool_output.slice(0, 2000) + '\n... (truncated)' : ev.tool_output;
            output = `<div class="tool-output">${esc(short)}</div>`;
        }
        let error = '';
        if (ev.tool_error) {
            error = `<div class="tool-error">${esc(ev.tool_error)}</div>`;
        }
        return `
            <div class="${cls}">
                <div class="event-header">
                    <span class="event-time">${time}</span>
                    <span class="tool-name">${esc(ev.tool)}</span>
                    ${ev.tool_path ? '<span class="tool-path">' + esc(ev.tool_path) + '</span>' : ''}
                    ${statusIcon}
                </div>
                ${output}${error}
            </div>
        `;
    }

    return `
        <div class="${cls}">
            <div class="event-header">
                <span class="event-time">${time}</span>
                <span class="event-agent">${esc(ev.agent || '')}</span>
            </div>
            <div class="event-msg">${esc(ev.message || '')}</div>
        </div>
    `;
}

function renderSessionInfo(session) {
    const info = document.getElementById('session-info');
    const rows = [
        ['Session', session.id],
        ['Status', { running: 'Running', completed: 'Done', error: 'Failed' }[session.status] || session.status],
        ['Started', session.started_at ? new Date(session.started_at).toLocaleString() : '-'],
        ['Ended', session.ended_at ? new Date(session.ended_at).toLocaleString() : '-'],
        ['Result', session.result || '-'],
    ];
    info.innerHTML = rows.map(r => `
        <div class="info-row">
            <span class="label">${r[0]}</span>
            <span class="value">${esc(r[1])}</span>
        </div>
    `).join('');
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
