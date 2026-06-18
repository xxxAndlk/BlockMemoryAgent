const API = '';

// ===== State =====
let currentView = 'dashboard';
let currentSessionId = null;
let currentEventSource = null;
let sessions = [];
let activeSession = null;
let selectedAgentId = null;
let selectedFilePath = null;
let selectedMemAgent = null;
let selectedSkillAgent = null;
let eventCount = 0;

// ===== View Switching =====
function switchView(view) {
    currentView = view;
    document.querySelectorAll('.nav-item').forEach(el => el.classList.remove('active'));
    const nav = document.querySelector(`.nav-item[data-view="${view}"]`);
    if (nav) nav.classList.add('active');

    document.querySelectorAll('.view').forEach(el => el.classList.remove('active'));
    const v = document.getElementById('view-' + view);
    if (v) v.classList.add('active');

    // Update breadcrumb/title
    const titles = {
        dashboard: 'Overview',
        sessions: 'Sessions',
        agents: 'Agent Hierarchy',
        board: 'Task Board',
        memory: 'Memory Explorer',
        skills: 'Skill Assembly',
        files: 'File Explorer',
        knowledge: 'Knowledge Base',
        history: 'Session History',
        health: 'System Health',
        settings: 'Settings'
    };
    document.getElementById('breadcrumb').textContent = titles[view] || view;
    document.getElementById('page-title').textContent = titles[view] || view;

    // Refresh data for the view
    if (view === 'dashboard') refreshDashboard();
    if (view === 'sessions') refreshSessions();
    if (view === 'agents') refreshAgents();
    if (view === 'board') refreshBoard();
    if (view === 'memory') refreshMemory();
    if (view === 'skills') refreshSkills();
    if (view === 'files') refreshFiles();
    if (view === 'knowledge') { /* lazy */ }
    if (view === 'history') refreshHistory();
    if (view === 'health') refreshHealth();
    if (view === 'settings') refreshSettings();
}

// ===== Dashboard =====
function refreshDashboard() {
    refreshSessions().then(() => {
        document.getElementById('stat-sessions').textContent = sessions.length;
        const completed = sessions.filter(s => s.status === 'completed').length;
        const rate = sessions.length > 0 ? Math.round((completed / sessions.length) * 100) : 0;
        document.getElementById('stat-completion').textContent = rate + '%';
        // LLM calls and tokens from stats events
        let totalCalls = 0;
        let totalTokens = 0;
        sessions.forEach(s => {
            (s.events || []).forEach(ev => {
                if (ev.type === 'stats' && ev.message) {
                    const m = ev.message.match(/调用(\d+)次/);
                    if (m) totalCalls += parseInt(m[1]);
                    const tm = ev.message.match(/输入Token=(\d+), 输出Token=(\d+)/);
                    if (tm) totalTokens += parseInt(tm[1]) + parseInt(tm[2]);
                }
            });
        });
        document.getElementById('stat-llm-calls').textContent = totalCalls;
        document.getElementById('stat-total-tokens').textContent = totalTokens.toLocaleString();
        document.getElementById('stat-avg-tokens').textContent = totalCalls > 0 ? Math.round(totalTokens / totalCalls).toLocaleString() : '0';

        // Recent sessions table
        const list = document.getElementById('dash-session-list');
        if (sessions.length === 0) {
            list.innerHTML = '<div class="empty-state">No sessions yet. Create one to get started.</div>';
            return;
        }
        list.innerHTML = sessions.slice().reverse().slice(0, 5).map(s => `
            <div class="session-card ${s.status}" onclick="goToSession('${s.id}')">
                <div class="session-card-goal">${esc(s.goal)}</div>
                <div class="session-card-meta">
                    <span class="session-card-time">${fmtTime(s.started_at)}</span>
                    <span class="status-badge status-${s.status}">${s.status}</span>
                </div>
            </div>
        `).join('');
    });
}

function quickStart(goal) {
    document.getElementById('goal-input').value = goal;
    createSession();
}

function goToSession(id) {
    switchView('sessions');
    openSessionDetail(id);
}

// ===== Sessions =====
async function refreshSessions() {
    try {
        const resp = await fetch(API + '/api/sessions');
        sessions = await resp.json();
        renderSessionCards();
        document.getElementById('nav-session-count').textContent = sessions.length;
        return sessions;
    } catch (e) {
        console.error('refresh sessions error:', e);
        return [];
    }
}

function renderSessionCards() {
    const list = document.getElementById('session-cards');
    if (!sessions || sessions.length === 0) {
        list.innerHTML = '<div class="empty-state">No sessions</div>';
        return;
    }
    list.innerHTML = sessions.slice().reverse().map(s => `
        <div class="session-card ${s.status} ${currentSessionId === s.id ? 'active' : ''}" onclick="openSessionDetail('${s.id}')">
            <div class="session-card-goal">${esc(s.goal)}</div>
            <div class="session-card-meta">
                <span class="session-card-time">${fmtTime(s.started_at)}</span>
                <span class="status-badge status-${s.status}">${s.status}</span>
            </div>
        </div>
    `).join('');
}

async function openSessionDetail(id) {
    currentSessionId = id;
    renderSessionCards();

    try {
        const resp = await fetch(API + '/api/sessions/' + id);
        activeSession = await resp.json();
    } catch (e) {
        console.error('fetch session error:', e);
        return;
    }

    const panel = document.getElementById('session-detail-panel');
    if (!activeSession) {
        panel.innerHTML = '<div class="empty-state large">Session not found</div>';
        return;
    }

    panel.innerHTML = `
        <div class="panel-header-bar">
            <h3>${esc(activeSession.goal)}</h3>
            <span class="status-badge status-${activeSession.status}">${activeSession.status}</span>
        </div>
        <div class="session-detail-content">
            <div class="tab-bar">
                <button class="tab-btn active" onclick="switchSessionTab(this,'log')">Execution Log</button>
                <button class="tab-btn" onclick="switchSessionTab(this,'agents')">Agents</button>
                <button class="tab-btn" onclick="switchSessionTab(this,'board')">Task Board</button>
                <button class="tab-btn" onclick="switchSessionTab(this,'trace')">Trace</button>
                <button class="tab-btn" onclick="switchSessionTab(this,'metrics')">Metrics</button>
                <button class="tab-btn" onclick="switchSessionTab(this,'mailbox')">Mailbox</button>
            </div>
            <div class="tab-content" id="session-tab-content">
                ${renderLogTab(activeSession)}
            </div>
        </div>
    `;

    // SSE
    if (currentEventSource) currentEventSource.close();
    currentEventSource = new EventSource(API + '/api/sessions/' + id + '/stream');
    currentEventSource.onmessage = function(e) {
        try {
            const data = JSON.parse(e.data);
            if (data.type === 'done') {
                currentEventSource.close(); currentEventSource = null;
                refreshSessions(); return;
            }
            if (data.type) {
                const content = document.getElementById('session-tab-content');
                if (content && content.dataset.tab === 'log') {
                    appendEventToLog(data);
                }
            }
        } catch (err) {}
    };
    currentEventSource.onerror = function() {
        if (currentEventSource) { currentEventSource.close(); currentEventSource = null; }
    };
}

function switchSessionTab(btn, tab) {
    btn.parentElement.querySelectorAll('.tab-btn').forEach(b => b.classList.remove('active'));
    btn.classList.add('active');
    const content = document.getElementById('session-tab-content');
    content.dataset.tab = tab;
    if (!activeSession) return;
    if (tab === 'log') content.innerHTML = renderLogTab(activeSession);
    else if (tab === 'agents') content.innerHTML = renderAgentsTab(activeSession);
    else if (tab === 'board') content.innerHTML = renderBoardTab(activeSession);
    else if (tab === 'trace') content.innerHTML = renderTraceTab(activeSession);
    else if (tab === 'metrics') content.innerHTML = renderMetricsTab(activeSession);
    else if (tab === 'mailbox') content.innerHTML = renderMailboxTab(activeSession);
}

function renderLogTab(session) {
    const events = session.events || [];
    return `<div class="events-list" id="log-events">${events.map((ev, i) => eventHtml(ev, i)).join('')}</div>`;
}

function appendEventToLog(ev) {
    const list = document.getElementById('log-events');
    if (!list) return;
    const idx = list.children.length;
    list.insertAdjacentHTML('beforeend', eventHtml(ev, idx));
    list.scrollTop = list.scrollHeight;
}

function renderAgentsTab(session) {
    // Build a simple tree from agent_done events
    const agents = [];
    (session.events || []).forEach(ev => {
        if (ev.type === 'agent_done') {
            const m = ev.message.match(/类型: ([^,]+), 领域: ([^,]+), 状态: ([^)]+)/);
            if (m) agents.push({ type: m[1].trim(), domain: m[2].trim(), status: m[3].trim(), agent: ev.agent });
        }
    });
    if (agents.length === 0) return '<div class="empty-state">No agent data available</div>';

    return `
        <div class="agent-tree" style="padding:12px">
            ${agents.map((a, i) => `
                <div class="tree-node" style="padding-left:${i*12}px">
                    <span class="tree-indent"></span>
                    <span class="tree-icon">◆</span>
                    <span class="tree-name">${esc(a.agent)}</span>
                    <span class="tree-type tree-type-${a.type}">${a.type}</span>
                    <span class="status-badge status-${a.status}">${a.status}</span>
                </div>
            `).join('')}
        </div>
    `;
}

function renderBoardTab(session) {
    // Mock kanban based on events - real board API not yet implemented
    return `
        <div class="kanban-board" style="padding:12px;gap:8px">
            <div class="kanban-column" data-status="pending">
                <div class="kanban-header">Pending</div>
                <div class="kanban-items">
                    <div class="kanban-card"><div class="kanban-card-title">Analyze task</div></div>
                </div>
            </div>
            <div class="kanban-column" data-status="in_progress">
                <div class="kanban-header">In Progress</div>
                <div class="kanban-items">
                    ${session.status === 'running' ? '<div class="kanban-card"><div class="kanban-card-title">Execute task</div></div>' : ''}
                </div>
            </div>
            <div class="kanban-column" data-status="done">
                <div class="kanban-header">Done</div>
                <div class="kanban-items">
                    ${session.status !== 'running' ? '<div class="kanban-card"><div class="kanban-card-title">Complete</div></div>' : ''}
                </div>
            </div>
        </div>
    `;
}

function renderMetricsTab(session) {
    let stats = 'No stats available';
    (session.events || []).forEach(ev => {
        if (ev.type === 'stats') stats = ev.message;
    });
    return `<div style="padding:16px"><div class="snapshot-section"><h4>LLM Statistics</h4><div class="snapshot-item">${esc(stats)}</div></div></div>`;
}

function renderMailboxTab(session) {
    return '<div style="padding:16px"><div class="empty-state">Mailbox API not yet implemented</div></div>';
}

// ===== Trace Tab =====
function renderTraceTab(session) {
    const events = session.events || [];

    // 1. Token 消耗统计
    const tokenEvents = events.filter(ev => ev.kind === 'token_usage');
    const callerStats = {};
    tokenEvents.forEach(ev => {
        const m = ev.message.match(/\[(.+?)\] Token 消耗: in=(\d+) out=(\d+) dur=(.+)/);
        if (!m) return;
        const caller = m[1];
        const inT = parseInt(m[2]) || 0;
        const outT = parseInt(m[3]) || 0;
        if (!callerStats[caller]) {
            callerStats[caller] = { calls: 0, input: 0, output: 0 };
        }
        callerStats[caller].calls++;
        callerStats[caller].input += inT;
        callerStats[caller].output += outT;
    });

    let tokenTable = '<div class="empty-state" style="padding:20px">暂无 Token 消耗记录</div>';
    if (Object.keys(callerStats).length > 0) {
        const rows = Object.entries(callerStats).map(([caller, s]) =>
            `<tr><td>${esc(caller)}</td><td class="num">${s.calls}</td><td class="num">${s.input}</td><td class="num">${s.output}</td><td class="num">${s.input + s.output}</td></tr>`
        ).join('');
        tokenTable = `
            <table class="token-table">
                <thead><tr><th>调用者</th><th>次数</th><th>输入Token</th><th>输出Token</th><th>合计</th></tr></thead>
                <tbody>${rows}</tbody>
            </table>`;
    }

    // 2. Prompt 查看器
    const promptEvents = events.filter(ev => ev.kind === 'prompt');
    let promptList = '<div class="empty-state" style="padding:20px">暂无 Prompt 记录</div>';
    if (promptEvents.length > 0) {
        promptList = promptEvents.map((ev, i) => {
            const pid = 'trace-prompt-' + i;
            return `
                <div class="prompt-item">
                    <div class="prompt-header" onclick="toggleOutput('${pid}', this.querySelector('.toggle-btn'))">
                        <span>${esc(ev.message || '')}</span>
                        <button class="toggle-btn">show</button>
                    </div>
                    <div id="${pid}" class="prompt-body">${esc(ev.prompt || ev.detail || '')}</div>
                </div>`;
        }).join('');
    }

    // 3. Agent 创建历史
    const agentEvents = events.filter(ev => ev.kind === 'agent_created');
    let agentList = '<div class="empty-state" style="padding:20px">暂无 Agent 创建记录</div>';
    if (agentEvents.length > 0) {
        agentList = agentEvents.map(ev => `
            <div class="agent-create-card">
                <div class="agent-create-title">${esc(ev.message || '')}</div>
                <div class="agent-create-meta">${esc(ev.detail || '')}</div>
            </div>`
        ).join('');
    }

    // 4. 图执行步骤流
    const stepEvents = events.filter(ev => ev.kind === 'graph_step');
    let stepFlow = '<div class="empty-state" style="padding:20px">暂无图执行步骤记录</div>';
    if (stepEvents.length > 0) {
        stepFlow = '<div class="step-flow">' + stepEvents.map((ev, i) => {
            const m = ev.message.match(/Step (\d+): (.+?) → (.+?) \(action=(.+)\)/);
            if (!m) return `<div class="step-flow-item"><span class="step-flow-num">#${i+1}</span>${esc(ev.message)}</div>`;
            return `
                <div class="step-flow-item">
                    <span class="step-flow-num">#${m[1]}</span>
                    <span>${esc(m[2])}</span>
                    <span class="step-flow-arrow">→</span>
                    <span>${esc(m[3])}</span>
                    <span class="step-flow-action">${esc(m[4])}</span>
                </div>`;
        }).join('') + '</div>';
    }

    return `
        <div class="trace-layout">
            <div class="trace-section">
                <div class="trace-section-header">Token 消耗统计</div>
                <div class="trace-section-body">${tokenTable}</div>
            </div>
            <div class="trace-section">
                <div class="trace-section-header">Prompt 查看器 (${promptEvents.length})</div>
                <div class="trace-section-body">${promptList}</div>
            </div>
            <div class="trace-section">
                <div class="trace-section-header">Agent 创建历史 (${agentEvents.length})</div>
                <div class="trace-section-body">${agentList}</div>
            </div>
            <div class="trace-section">
                <div class="trace-section-header">图执行步骤流 (${stepEvents.length})></div>
                <div class="trace-section-body">${stepFlow}</div>
            </div>
        </div>
    `;
}

// ===== Event HTML =====
function eventHtml(ev, idx) {
    const time = ev.timestamp ? new Date(ev.timestamp).toLocaleTimeString() : '';
    const kind = ev.kind || '';
    const cls = kind ? ('event-item event-progress event-kind-' + kind) : ('event-item event-' + (ev.type || ''));
    const kindLabel = kind ? `<span class="kind-badge kind-${kind}">${kind}</span>` : '';

    // 特殊调试事件类型渲染
    if (ev.kind === 'prompt' || ev.kind === 'agent_created' || ev.kind === 'token_usage' || ev.kind === 'graph_step') {
        const detailHtml = ev.detail_json || ev.detail || '';
        const hasDetail = detailHtml.length > 0 && detailHtml.length < 2000;
        const detailId = 'detail-' + idx;
        const toggleBtn = hasDetail ? `<button class="toggle-btn" onclick="toggleOutput('${detailId}',this)">show</button>` : '';
        const detailBlock = hasDetail ? `<div id="${detailId}" class="tool-output">${esc(detailHtml)}</div>` : '';
        return `
            <div class="${cls}">
                <div class="event-header">
                    <span class="event-time">${time}</span>
                    <span class="event-agent">${esc(ev.agent || '')}</span>
                    ${kindLabel}
                    ${toggleBtn}
                </div>
                <div class="event-msg">${msgHtml}</div>
                ${detailBlock}
            </div>
        `;
    }

    if (ev.type === 'tool_exec') {
        const statusIcon = ev.success ? '<span class="tool-success">OK</span>' : '<span class="tool-fail">FAIL</span>';
        const hasOutput = !!(ev.tool_output && ev.tool_output.length > 0);
        const hasError = !!(ev.tool_error && ev.tool_error.length > 0);
        const outputId = 'tool-out-' + idx;
        let output = '';
        if (hasOutput) {
            const short = ev.tool_output.length > 2000 ? ev.tool_output.slice(0, 2000) + '\n... (truncated)' : ev.tool_output;
            output = `<div id="${outputId}" class="tool-output">${esc(short)}</div>`;
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

function toggleOutput(id, btn) {
    const el = document.getElementById(id);
    if (!el) return;
    if (el.classList.contains('show')) {
        el.classList.remove('show');
        btn.textContent = 'show';
    } else {
        el.classList.add('show');
        btn.textContent = 'hide';
    }
}

function simpleMarkdown(text) {
    if (!text) return '';
    let html = esc(text);
    html = html.replace(/```(\w*)\n([\s\S]*?)```/g, '<pre class="code-block"><code>$2</code></pre>');
    html = html.replace(/`([^`]+)`/g, '<code class="inline-code">$1</code>');
    html = html.replace(/\*\*([^*]+)\*\*/g, '<strong>$1</strong>');
    html = html.replace(/\n/g, '<br>');
    return html;
}

// ===== Create Session =====
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
        if (!resp.ok) { alert('Failed: ' + await resp.text()); return; }
        const session = await resp.json();
        input.value = '';
        await refreshSessions();
        switchView('sessions');
        openSessionDetail(session.id);
    } catch (e) {
        alert('Network error: ' + e.message);
    } finally {
        btn.disabled = false;
        btn.textContent = 'Run';
    }
}

// ===== Agent Tree View =====
function refreshAgents() {
    const tree = document.getElementById('agent-tree');
    if (!activeSession || activeSession.id !== currentSessionId) {
        // Use most recent session
        const s = sessions.find(s => s.status === 'running') || sessions[sessions.length - 1];
        if (!s) { tree.innerHTML = '<div class="empty-state">No sessions available</div>'; return; }
        activeSession = s;
    }
    tree.innerHTML = renderAgentsTab(activeSession);
}

// ===== Task Board View =====
function refreshBoard() {
    const board = document.getElementById('kanban-board');
    if (!activeSession) {
        const s = sessions.find(s => s.status === 'running') || sessions[sessions.length - 1];
        if (!s) return;
        activeSession = s;
    }
    document.getElementById('board-title').textContent = activeSession.goal || 'Task Board';
    document.getElementById('board-status').textContent = activeSession.status;
    document.getElementById('board-status').className = 'board-status status-badge status-' + activeSession.status;
    // Fill kanban columns based on events
    const agents = [];
    (activeSession.events || []).forEach(ev => {
        if (ev.type === 'agent_done') {
            const m = ev.message.match(/类型: ([^,]+), 领域: ([^,]+), 状态: ([^)]+)/);
            if (m) agents.push({ name: ev.agent, type: m[1], domain: m[2], status: m[3] });
        }
    });

    const pending = agents.filter(a => a.status === 'idle' || a.status === 'pending');
    const progress = agents.filter(a => a.status === 'active' || a.status === 'in_progress');
    const done = agents.filter(a => a.status === 'done' || a.status === 'completed');
    const failed = agents.filter(a => a.status === 'error' || a.status === 'failed');

    fillKanban('kanban-pending', pending);
    fillKanban('kanban-in_progress', progress);
    fillKanban('kanban-done', done);
    fillKanban('kanban-failed', failed);
}

function fillKanban(id, items) {
    const el = document.getElementById(id);
    if (!el) return;
    if (items.length === 0) { el.innerHTML = '<div class="empty-state" style="padding:20px 0;font-size:11px">Empty</div>'; return; }
    el.innerHTML = items.map(a => `
        <div class="kanban-card">
            <div class="kanban-card-title">${esc(a.name)}</div>
            <div class="kanban-card-meta">
                <span class="kanban-card-assignee">${esc(a.domain)}</span>
                <span class="tree-type tree-type-${a.type}">${a.type}</span>
            </div>
        </div>
    `).join('');
}

// ===== Memory View =====
function refreshMemory() {
    // Mock - real API not yet implemented
    const selector = document.getElementById('memory-agent-selector');
    const content = document.getElementById('memory-content');
    if (!activeSession) {
        selector.innerHTML = '<div class="empty-state">No active session</div>';
        content.innerHTML = '<div class="empty-state">Select a session first</div>';
        return;
    }
    const agents = [];
    (activeSession.events || []).forEach(ev => {
        if (ev.type === 'agent_done') agents.push({ name: ev.agent });
    });
    if (agents.length === 0) {
        selector.innerHTML = '<div class="empty-state">No agents</div>';
        return;
    }
    selector.innerHTML = agents.map((a, i) => `
        <div class="agent-selector-item ${selectedMemAgent === i ? 'active' : ''}" onclick="selectMemAgent(${i})">
            <span>◆</span> ${esc(a.name)}
        </div>
    `).join('');
    if (selectedMemAgent === null && agents.length > 0) selectedMemAgent = 0;
    renderMemoryContent();
}

function selectMemAgent(idx) {
    selectedMemAgent = idx;
    refreshMemory();
}

function switchMemTab(tab) {
    document.querySelectorAll('.mem-tab').forEach(b => b.classList.remove('active'));
    event.target.classList.add('active');
    renderMemoryContent(tab);
}

function renderMemoryContent(tab) {
    const content = document.getElementById('memory-content');
    if (!activeSession) return;
    const evs = activeSession.events || [];

    if (tab === 'search') {
        content.innerHTML = `
            <div class="kb-search-box" style="margin-bottom:12px">
                <input type="text" placeholder="Search memory..." id="mem-search-input">
                <button class="btn-primary" onclick="alert('Memory search API not yet implemented')">Search</button>
            </div>
            <div class="empty-state">Memory search requires backend API</div>
        `;
        return;
    }
    if (tab === 'compression') {
        content.innerHTML = `
            <div class="snapshot-section">
                <h4>Compression Levels</h4>
                <div class="snapshot-item">Level 0 (Raw): <strong>${evs.length}</strong> episodes</div>
                <div class="snapshot-item">Level 1 (Standard): 0</div>
                <div class="snapshot-item">Level 2 (Compact): 0</div>
                <div class="snapshot-item">Level 3 (Marker): 0</div>
            </div>
        `;
        return;
    }

    // Snapshot
    const summaries = [];
    evs.forEach((ev, i) => {
        if (ev.type === 'progress' || ev.type === 'tool_exec') {
            summaries.push({ step: i, content: ev.message });
        }
    });

    content.innerHTML = `
        <div class="snapshot-section">
            <h4>Key Summaries</h4>
            ${summaries.slice(-5).map(s => `
                <div class="snapshot-item">
                    <div class="snap-id">Step ${s.step}</div>
                    <div class="snap-content">${esc(s.content)}</div>
                </div>
            `).join('') || '<div class="snapshot-item">No summaries yet</div>'}
        </div>
        <div class="snapshot-section">
            <h4>Open Issues</h4>
            <div class="snapshot-item">No open issues</div>
        </div>
    `;
}

// ===== Skills View =====
function refreshSkills() {
    const selector = document.getElementById('skills-agent-selector');
    const cards = document.getElementById('skill-cards');
    if (!activeSession) {
        selector.innerHTML = '<div class="empty-state">No active session</div>';
        cards.innerHTML = '<div class="empty-state">Select an agent</div>';
        return;
    }
    const agents = [];
    (activeSession.events || []).forEach(ev => {
        if (ev.type === 'agent_done') agents.push({ name: ev.agent });
    });
    selector.innerHTML = agents.map((a, i) => `
        <div class="agent-selector-item ${selectedSkillAgent === i ? 'active' : ''}" onclick="selectSkillAgent(${i})">
            <span>◆</span> ${esc(a.name)}
        </div>
    `).join('');
    if (selectedSkillAgent === null && agents.length > 0) selectedSkillAgent = 0;

    // Mock skills
    cards.innerHTML = `
        <div class="skill-card">
            <div class="skill-card-header">
                <span class="skill-card-id">read_file</span>
                <span class="skill-card-cost">cost: 5</span>
            </div>
            <div class="skill-card-name">Read File</div>
            <div class="skill-card-desc">读取本地文件并返回文本内容</div>
            <div class="skill-card-footer">
                <span class="skill-tag">io</span>
                <span class="skill-tag">fs</span>
            </div>
        </div>
        <div class="skill-card">
            <div class="skill-card-header">
                <span class="skill-card-id">write_file</span>
                <span class="skill-card-cost">cost: 8</span>
            </div>
            <div class="skill-card-name">Write File</div>
            <div class="skill-card-desc">把文本内容写入本地文件</div>
            <div class="skill-card-footer">
                <span class="skill-tag">io</span>
                <span class="skill-tag">fs</span>
            </div>
        </div>
        <div class="skill-card">
            <div class="skill-card-header">
                <span class="skill-card-id">run_command</span>
                <span class="skill-card-cost">cost: 10</span>
            </div>
            <div class="skill-card-name">Run Command</div>
            <div class="skill-card-desc">在沙箱内执行 shell 命令并返回输出</div>
            <div class="skill-card-footer">
                <span class="skill-tag">shell</span>
            </div>
        </div>
    `;
}

function selectSkillAgent(idx) {
    selectedSkillAgent = idx;
    refreshSkills();
}

// ===== Files View =====
function refreshFiles() {
    const list = document.getElementById('file-list');
    const preview = document.getElementById('file-preview');
    const path = document.getElementById('preview-path');

    if (!activeSession) {
        list.innerHTML = '<div class="empty-state">No active session</div>';
        preview.innerHTML = '<code>Select a file to preview</code>';
        return;
    }

    // Find WriteFile tool execs
    const files = [];
    (activeSession.events || []).forEach(ev => {
        if (ev.type === 'tool_exec' && ev.tool === 'WriteFile' && ev.tool_path) {
            files.push({ path: ev.tool_path, success: ev.success });
        }
    });

    if (files.length === 0) {
        list.innerHTML = '<div class="empty-state">No files created yet</div>';
        return;
    }

    list.innerHTML = files.map((f, i) => `
        <div class="file-item ${selectedFilePath === f.path ? 'active' : ''}" onclick="selectFile('${esc(f.path)}')">
            <span class="file-icon">📄</span>
            <span class="file-name">${esc(f.path)}</span>
            ${f.success ? '' : '<span style="color:var(--accent-red)">✗</span>'}
        </div>
    `).join('');
}

function selectFile(path) {
    selectedFilePath = path;
    document.getElementById('preview-path').textContent = path;
    // Fetch file content (best effort)
    fetch(API + '/static/' + path)
        .then(r => r.ok ? r.text() : Promise.reject())
        .then(text => {
            document.getElementById('file-preview').innerHTML = '<code>' + esc(text) + '</code>';
        })
        .catch(() => {
            document.getElementById('file-preview').innerHTML = '<code>Unable to load file content</code>';
        });
    refreshFiles();
}

// ===== Knowledge =====
function searchKnowledge() {
    const q = document.getElementById('kb-query').value.trim();
    const results = document.getElementById('knowledge-results');
    if (!q) return;
    results.innerHTML = '<div class="empty-state">Searching...</div>';
    setTimeout(() => {
        results.innerHTML = `
            <div class="kb-result">
                <div class="kb-result-score">similarity: 0.92</div>
                <div class="kb-result-content">Mock result for "${esc(q)}"</div>
                <div class="kb-result-meta">source: global_knowledge | access: 5</div>
            </div>
        `;
    }, 500);
}

// ===== History =====
function refreshHistory() {
    const timeline = document.getElementById('history-timeline');
    if (sessions.length === 0) {
        timeline.innerHTML = '<div class="empty-state">No session history</div>';
        return;
    }
    timeline.innerHTML = sessions.slice().reverse().map(s => `
        <div class="history-item">
            <div class="history-goal">${esc(s.goal)}</div>
            <div class="history-summary">${esc(s.result || 'No result')}</div>
            <div class="history-meta">
                <span>${s.id}</span>
                <span class="status-badge status-${s.status}">${s.status}</span>
                <span>${fmtTime(s.started_at)}</span>
            </div>
        </div>
    `).join('');
}

// ===== Health =====
function refreshHealth() {
    // Mock - real health API not yet implemented
    document.getElementById('health-pg').textContent = 'Connected';
    document.getElementById('health-pg').className = 'health-status online';
    document.getElementById('health-pg-detail').textContent = 'agent_private_memory: active';

    document.getElementById('health-redis').textContent = 'Connected';
    document.getElementById('health-redis').className = 'health-status online';
    document.getElementById('health-redis-detail').textContent = 'snapshots: active';

    document.getElementById('health-llm').textContent = 'Connected';
    document.getElementById('health-llm').className = 'health-status online';
    document.getElementById('health-llm-detail').textContent = 'avg latency: ~4s';
}

// ===== Settings =====
function refreshSettings() {
    // Mock soul content
    document.getElementById('soul-content').innerHTML = `
        <textarea placeholder="Enter soul configuration (markdown)..."># Soul Configuration

## Personality
You are a helpful, precise AI assistant.

## Communication Style
- Concise and direct
- Technical accuracy first
- Always verify before claiming</textarea>
    `;
}

function saveSoul() {
    alert('Soul save API not yet implemented');
}

// ===== Pause/Resume =====
function togglePause() {
    const btn = document.getElementById('pause-btn');
    if (btn.classList.contains('active')) {
        btn.classList.remove('active');
        btn.textContent = '⏸';
        alert('Resume API not yet implemented');
    } else {
        btn.classList.add('active');
        btn.textContent = '▶';
        alert('Pause API not yet implemented');
    }
}

// ===== Helpers =====
function esc(s) {
    const d = document.createElement('div');
    d.textContent = s || '';
    return d.innerHTML;
}

function fmtTime(ts) {
    if (!ts) return '-';
    return new Date(ts).toLocaleTimeString();
}

// ===== Init =====
document.addEventListener('DOMContentLoaded', function() {
    document.getElementById('goal-input').addEventListener('keydown', function(e) {
        if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); createSession(); }
    });
    refreshSessions().then(() => {
        refreshDashboard();
    });
    setInterval(refreshSessions, 5000);

    // Temperature slider
    const slider = document.getElementById('temp-slider');
    if (slider) {
        slider.addEventListener('input', function() {
            document.getElementById('temp-value').textContent = (this.value / 100).toFixed(2);
        });
    }
});
