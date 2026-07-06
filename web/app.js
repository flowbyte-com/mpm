// MPM Web UI — Vanilla JS SPA

const API = '/api';
let token = '';
let currentView = 'search';
let memOffset = 0;
let memLimit = 50;

// ==================== Auth ====================

async function loadToken() {
  try {
    const res = await fetch('/api/status');
    if (res.ok) return true;
  } catch {}
  // Prompt for token if not authenticated
  const input = prompt('Enter your openclaw auth token (from ~/.openclaw/openclaw.json → gateway.auth.token):');
  if (input) {
    token = input.trim();
    localStorage.setItem('mpm_token', token);
    return true;
  }
  return false;
}

function getToken() {
  if (!token) {
    token = localStorage.getItem('mpm_token') || '';
  }
  return token;
}

async function apiFetch(path, options = {}) {
  const t = getToken();
  const res = await fetch(API + path, {
    ...options,
    headers: {
      'Content-Type': 'application/json',
      ...(t ? { 'Authorization': 'Bearer ' + t } : {}),
      ...(options.headers || {}),
    },
  });
  if (res.status === 401) {
    localStorage.removeItem('mpm_token');
    token = '';
    showToast('Authentication required', 'error');
    return null;
  }
  if (!res.ok) {
    const body = await res.json().catch(() => ({}));
    throw new Error(body.error || `HTTP ${res.status}`);
  }
  return res.json();
}

// ==================== Toast ====================

let toastTimer;
function showToast(msg, type = '') {
  const el = document.getElementById('toast');
  el.textContent = msg;
  el.className = 'toast ' + type;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => el.classList.add('hidden'), 3000);
}

// ==================== Routing ====================

function showView(name) {
  document.querySelectorAll('.view').forEach(v => v.classList.remove('active'));
  document.querySelectorAll('.nav-btn').forEach(b => b.classList.remove('active'));
  document.getElementById('view-' + name).classList.add('active');
  document.querySelector('.nav-btn[data-view="' + name + '"]')?.classList.add('active');
  currentView = name;

  if (name === 'memories') loadMemories();
  else if (name === 'topics') loadTopics();
  else if (name === 'lessons') loadLessons();
  else if (name === 'directives') loadDirectives();
  else if (name === 'telemetry') loadTelemetry();
  else if (name === 'search') {
    document.getElementById('search-input')?.focus();
  }
}

document.addEventListener('DOMContentLoaded', async () => {
  // Nav routing
  document.querySelectorAll('.nav-btn').forEach(btn => {
    btn.addEventListener('click', () => showView(btn.dataset.view));
  });

  // Modal close
  document.getElementById('modal-close').addEventListener('click', closeModal);
  document.getElementById('modal').addEventListener('click', e => {
    if (e.target.id === 'modal') closeModal();
  });

  // Search
  document.getElementById('search-btn').addEventListener('click', doSearch);
  document.getElementById('search-input').addEventListener('keydown', e => {
    if (e.key === 'Enter') doSearch();
  });

  // Memories
  document.getElementById('add-memory-btn').addEventListener('click', () => openMemoryModal());
  document.getElementById('memories-collection-filter').addEventListener('change', loadMemories);

  // Topics
  document.getElementById('add-topic-btn').addEventListener('click', () => openTopicModal());

  // Lessons
  document.getElementById('add-lesson-btn').addEventListener('click', () => openLessonModal());
  document.getElementById('lessons-type-filter').addEventListener('change', loadLessons);

  // Keyboard shortcuts
  document.addEventListener('keydown', handleKeyboard);

  // Auth check + start
  const ok = await loadToken();
  if (ok) {
    showView('search');
    updateStatus();
    setInterval(updateStatus, 30000);
  }
});

// ==================== Keyboard Shortcuts ====================

function handleKeyboard(e) {
  const tag = document.activeElement?.tagName;
  const isEditing = tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT';

  // Escape - close modal
  if (e.key === 'Escape') {
    closeModal();
    return;
  }

  // / - focus search (when not editing text)
  if (e.key === '/' && !isEditing) {
    e.preventDefault();
    showView('search');
    document.getElementById('search-input')?.focus();
    return;
  }

  // g + key - navigation (vim-style)
  if (e.key === 'g' && !isEditing && !e.ctrlKey && !e.metaKey) {
    // Wait for next key
    document.body.dataset.expectG = '1';
    setTimeout(() => { document.body.dataset.expectG = ''; }, 1000);
    return;
  }

  if (document.body.dataset.expectG === '1') {
    document.body.dataset.expectG = '';
    switch (e.key) {
      case 'm': showView('memories'); break;
      case 't': showView('topics'); break;
      case 'l': showView('lessons'); break;
      case 'd': showView('directives'); break;
      case 'e': showView('telemetry'); break;
      case 's': showView('search'); document.getElementById('search-input')?.focus(); break;
    }
    return;
  }

  // Ctrl+K - focus search
  if ((e.ctrlKey || e.metaKey) && e.key === 'k') {
    e.preventDefault();
    showView('search');
    document.getElementById('search-input')?.focus();
    return;
  }
}

// ==================== Auth ====================

// ==================== Status ====================

async function updateStatus() {
  try {
    const data = await apiFetch('/status');
    if (!data) return;
    const el = document.getElementById('status');
    const ingest = data.ingest || {};
    el.textContent = `mem ${data.memories || 0} · topic ${data.topics || 0} · lesson ${data.lessons || 0}${ingest.pending !== undefined ? ' · ingest ' + ingest.pending : ''}`;
  } catch {}
}

// ==================== Search ====================

async function doSearch() {
  const q = document.getElementById('search-input').value.trim();
  const el = document.getElementById('search-results');
  if (!q) { el.innerHTML = ''; return; }

  el.innerHTML = '<div class="loading"><div class="spinner"></div>Searching...</div>';
  try {
    const data = await apiFetch('/search?q=' + encodeURIComponent(q) + '&limit=20');
    if (!data) return;
    renderSearchResults(data, el);
  } catch (err) {
    el.innerHTML = '<div class="empty-state"><p>Search failed: ' + esc(err.message) + '</p></div>';
  }
}

function renderSearchResults(data, container) {
  const { memories = [], topics = [], lessons = [] } = data;
  if (!memories.length && !topics.length && !lessons.length) {
    container.innerHTML = '<div class="empty-state"><p>No results found</p></div>';
    return;
  }
  let html = '';

  if (memories.length) {
    html += '<div class="result-group"><h3>Memories</h3>';
    memories.forEach(m => {
      html += renderMemoryCard(m, false);
    });
    html += '</div>';
  }

  if (topics.length) {
    html += '<div class="result-group"><h3>Topics</h3>';
    topics.forEach(t => {
      html += '<div class="card" onclick="showTopic(' + q(t.id) + ')"><div class="card-title">' + esc(t.name) + '</div>';
      if (t.description) html += '<div class="card-content">' + esc(t.description) + '</div>';
      html += '</div>';
    });
    html += '</div>';
  }

  if (lessons.length) {
    html += '<div class="result-group"><h3>Lessons</h3>';
    lessons.forEach(l => {
      html += '<div class="card" onclick="showLesson(' + q(l.id) + ')"><div class="card-title">' + esc(l.content.slice(0, 80)) + '</div>';
      html += '<div class="card-meta">' + (l.type || 'insight') + '</div></div>';
    });
    html += '</div>';
  }

  container.innerHTML = html;
}

// ==================== Memories ====================

async function loadMemories() {
  const collection = document.getElementById('memories-collection-filter')?.value || '';
  const primeOnly = currentView === 'directives';
  const el = document.getElementById('memories-list');
  el.innerHTML = '<div class="loading"><div class="spinner"></div>Loading...</div>';

  try {
    let url = '/memories?limit=' + memLimit + '&offset=' + memOffset;
    if (collection) url += '&collection=' + encodeURIComponent(collection);
    if (primeOnly) url += '&is_prime_directive=1';
    const data = await apiFetch(url);
    if (!data) return;
    renderMemories(data.items || [], el);
    renderMemPagination(data);
  } catch (err) {
    el.innerHTML = '<div class="empty-state"><p>Failed to load: ' + esc(err.message) + '</p></div>';
  }
}

function renderMemories(items, el) {
  if (!items.length) {
    el.innerHTML = '<div class="empty-state"><p>📭 No memories yet</p><p>Add your first memory with the + button above</p></div>';
    return;
  }
  el.innerHTML = items.map(m => renderMemoryCard(m, true)).join('');
}

function renderMemoryCard(m, showActions) {
  const tags = parseTags(m.tags);
  const meta = parseJSON(m.metadata, {});
  const provenance = meta.provenance || {};
  const isPrime = tags.includes('is_prime_directive') || meta.is_prime_directive === true || (typeof m.metadata === 'string' && m.metadata.includes('is_prime_directive'));
  const slashed = meta.slashed === true || (meta.weight != null && meta.weight <= 1);
  const client = provenance.client;
  const compute = provenance.compute;
  const tagHtml = tags.filter(t => t && t !== 'is_prime_directive').map(t => '<span class="tag">' + esc(t) + '</span>').join('');
  const primeHtml = isPrime ? '<span class="tag directive">prime</span>' : '';
  const content = m.content ? esc(m.content.slice(0, 300) + (m.content.length > 300 ? '…' : '')) : '';
  let provHtml = '';
  if (client) provHtml += '<span class="tag client-badge">' + esc(client) + '</span>';
  if (compute) provHtml += '<span class="tag compute-' + esc(compute) + '">' + esc(compute) + '</span>';
  let html = '<div class="card' + (slashed ? ' slashed' : '') + '">';
  html += '<div class="card-header">';
  html += '<div>';
  if (m.collection && m.collection !== 'memories') html += '<span class="tag" style="margin-bottom:0.3rem">' + esc(m.collection) + '</span><br>';
  html += content.split('\n').map(l => l).join('<br>');
  if (tagHtml || primeHtml || provHtml) html += '<div style="margin-top:0.5rem">' + primeHtml + provHtml + tagHtml + '</div>';
  html += '</div>';
  if (showActions) {
    html += '<div class="card-actions">';
    html += '<button class="btn-ghost" onclick="openMemoryModal(\'' + m.id + '\')">Edit</button>';
    html += '<button class="btn-danger" onclick="deleteMemory(\'' + m.id + '\')">Shred</button>';
    html += '</div>';
  }
  html += '</div>';
  if (m.created_at) html += '<div class="card-meta">' + m.created_at.split('T')[0] + '</div>';
  html += '</div>';
  return html;
}

function renderMemPagination(data) {
  const el = document.getElementById('memories-pagination');
  if (!el) return;
  const total = data.count || 0;
  const pages = Math.ceil(total / memLimit);
  const cur = Math.floor(memOffset / memLimit) + 1;
  if (pages <= 1) { el.innerHTML = ''; return; }
  let html = '';
  if (memOffset > 0) html += '<button onclick="memOffset(-' + memLimit + ')">Prev</button>';
  for (let i = 1; i <= pages; i++) {
    if (i === 1 || i === pages || (i >= cur - 2 && i <= cur + 2)) {
      html += '<button class="' + (i === cur ? 'active' : '') + '" onclick="memOffset(' + ((i-1)*memLimit - memOffset) + ')">' + i + '</button>';
    } else if (i === cur - 3 || i === cur + 3) {
      html += '<button disabled>…</button>';
    }
  }
  if (memOffset + memLimit < total) html += '<button onclick="memOffset(' + memLimit + ')">Next</button>';
  el.innerHTML = html;
}

function memOffset(delta) {
  memOffset = Math.max(0, memOffset + delta);
  loadMemories();
}

async function deleteMemory(id) {
  if (!confirm('Permanently shred this memory? This cannot be undone.')) return;
  try {
    await apiFetch('/memories/' + id, { method: 'DELETE' });
    showToast('Memory shredded', 'success');
    loadMemories();
    updateStatus();
  } catch (err) {
    showToast('Failed: ' + err.message, 'error');
  }
}

function openMemoryModal(id) {
  const isNew = !id;
  const modal = document.getElementById('modal');
  document.getElementById('modal-title').textContent = isNew ? 'Add Memory' : 'Edit Memory';

  let existing = { content: '', collection: 'memories', tags: [], metadata: {} };
  if (!isNew) {
    const cards = document.querySelectorAll('#memories-list .card');
    // Find the card by onclick attribute - simpler to fetch
    apiFetch('/memories/' + id).then(m => {
      if (m) {
        existing = {
          content: m.content || '',
          collection: m.collection || 'memories',
          tags: parseTags(m.tags),
          metadata: parseJSON(m.metadata, {}),
        };
        document.getElementById('modal-body').innerHTML = memFormHTML(existing, id);
        setupMemForm(id);
      }
    });
  }

  document.getElementById('modal-body').innerHTML = memFormHTML(existing, id);
  modal.classList.remove('hidden');
  if (isNew) setupMemForm(null);
}

function memFormHTML(m, id) {
  const tagsStr = (m.tags || []).join(', ');
  return '<form id="mem-form" onsubmit="return false;"><div class="form-group"><label>Content</label><textarea id="mem-content" placeholder="Memory content...">' + esc(m.content || '') + '</textarea></div><div class="form-group"><label>Collection</label><input type="text" id="mem-collection" value="' + esc(m.collection || 'memories') + '"></div><div class="form-group"><label>Tags (comma-separated)</label><input type="text" id="mem-tags" value="' + esc(tagsStr) + '" placeholder="tag1, tag2"></div><div class="form-group"><label>Metadata JSON (optional)</label><input type="text" id="mem-metadata" value=\'' + esc(JSON.stringify(m.metadata || {})) + '\' placeholder=\'{}\'></div><div class="form-actions"><button class="btn-ghost" onclick="closeModal()">Cancel</button><button class="btn-primary" id="mem-submit">' + (id ? 'Save' : 'Add') + '</button></div></form>';
}

function setupMemForm(id) {
  document.getElementById('mem-submit').addEventListener('click', async () => {
    const content = document.getElementById('mem-content').value.trim();
    if (!content) { showToast('Content required', 'error'); return; }
    const collection = document.getElementById('mem-collection').value.trim() || 'memories';
    const tagsStr = document.getElementById('mem-tags').value.trim();
    const tags = tagsStr ? tagsStr.split(',').map(t => t.trim()).filter(Boolean) : [];
    const metaStr = document.getElementById('mem-metadata').value.trim();
    let metadata = {};
    try { metadata = parseJSON(metaStr, {}); } catch {}

    try {
      if (id) {
        await apiFetch('/memories/' + id, {
          method: 'PUT',
          body: JSON.stringify({ content, tags, metadata: metaStr }),
        });
        showToast('Memory updated', 'success');
      } else {
        await apiFetch('/memories', {
          method: 'POST',
          body: JSON.stringify({ content, collection, tags, metadata }),
        });
        showToast('Memory added', 'success');
      }
      closeModal();
      loadMemories();
      updateStatus();
    } catch (err) {
      showToast('Error: ' + err.message, 'error');
    }
  });
}

// ==================== Topics ====================

async function loadTopics() {
  const el = document.getElementById('topics-list');
  el.innerHTML = '<div class="loading"><div class="spinner"></div>Loading...</div>';
  try {
    const data = await apiFetch('/topics');
    if (!data) return;
    renderTopics(data.items || [], el);
  } catch (err) {
    el.innerHTML = '<div class="empty-state"><p>Failed to load: ' + esc(err.message) + '</p></div>';
  }
}

function renderTopics(items, el) {
  if (!items.length) {
    el.innerHTML = '<div class="empty-state"><p>📁 No topics yet</p><p>Create one with the + button above</p></div>';
    return;
  }
  el.innerHTML = items.map(t => {
    let html = '<div class="card">';
    html += '<div class="card-header"><div class="card-title">' + esc(t.name) + '</div>';
    html += '<div class="card-actions"><button class="btn-ghost" onclick="openTopicModal(\'' + t.id + '\')">Edit</button>';
    html += '<button class="btn-danger" onclick="deleteTopic(\'' + t.id + '\')">Delete</button></div></div>';
    if (t.description) html += '<div class="card-content">' + esc(t.description) + '</div>';
    html += '<div class="card-meta">' + (t.memory_count || 0) + ' memories · ' + (t.created_at || '').split('T')[0] + '</div>';
    html += '</div>';
    return html;
  }).join('');
}

async function deleteTopic(id) {
  if (!confirm('Delete this topic?')) return;
  try {
    await apiFetch('/topics/' + id, { method: 'DELETE' });
    showToast('Topic deleted', 'success');
    loadTopics();
    updateStatus();
  } catch (err) {
    showToast('Failed: ' + err.message, 'error');
  }
}

function openTopicModal(id) {
  const isNew = !id;
  const modal = document.getElementById('modal');
  document.getElementById('modal-title').textContent = isNew ? 'New Topic' : 'Edit Topic';

  let existing = { name: '', description: '' };
  if (!isNew) {
    apiFetch('/topics/' + id).then(t => {
      if (t) {
        existing = { name: t.name || '', description: t.description || '' };
        document.getElementById('modal-body').innerHTML = topicFormHTML(existing, id);
        setupTopicForm(id);
      }
    });
  }

  document.getElementById('modal-body').innerHTML = topicFormHTML(existing, id);
  modal.classList.remove('hidden');
  if (isNew) setupTopicForm(null);
}

function topicFormHTML(t, id) {
  return '<form id="topic-form"><div class="form-group"><label>Name</label><input type="text" id="topic-name" value="' + esc(t.name || '') + '" required></div><div class="form-group"><label>Description</label><textarea id="topic-desc">' + esc(t.description || '') + '</textarea></div><div class="form-actions"><button class="btn-ghost" onclick="closeModal()">Cancel</button><button class="btn-primary" id="topic-submit">' + (id ? 'Save' : 'Create') + '</button></div></form>';
}

function setupTopicForm(id) {
  document.getElementById('topic-submit').addEventListener('click', async () => {
    const name = document.getElementById('topic-name').value.trim();
    const description = document.getElementById('topic-desc').value.trim();
    if (!name) { showToast('Name required', 'error'); return; }
    try {
      if (id) {
        // Note: edit via create since API doesn't have explicit edit endpoint
        await apiFetch('/topics', { method: 'POST', body: JSON.stringify({ name, description }) });
        // For now, topics are created fresh - edit would need a PUT endpoint
        showToast('Topic updated (created new)', 'success');
      } else {
        await apiFetch('/topics', { method: 'POST', body: JSON.stringify({ name, description }) });
        showToast('Topic created', 'success');
      }
      closeModal();
      loadTopics();
    } catch (err) {
      showToast('Error: ' + err.message, 'error');
    }
  });
}

// ==================== Lessons ====================

async function loadLessons() {
  const typeFilter = document.getElementById('lessons-type-filter')?.value || '';
  const el = document.getElementById('lessons-list');
  el.innerHTML = '<div class="loading"><div class="spinner"></div>Loading...</div>';
  try {
    let url = '/lessons';
    if (typeFilter) url += '?type=' + encodeURIComponent(typeFilter);
    const data = await apiFetch(url);
    if (!data) return;
    renderLessons(data.items || [], el);
  } catch (err) {
    el.innerHTML = '<div class="empty-state"><p>Failed to load: ' + esc(err.message) + '</p></div>';
  }
}

function renderLessons(items, el) {
  if (!items.length) {
    el.innerHTML = '<div class="empty-state"><p>📝 No lessons yet</p><p>Add one with the + button above</p></div>';
    return;
  }
  el.innerHTML = items.map(l => {
    const tagHtml = (l.tags || []).map(t => '<span class="tag">' + esc(t) + '</span>').join('');
    let html = '<div class="card">';
    html += '<div class="card-header"><div>';
    html += '<span class="tag" style="margin-bottom:0.3rem;background:rgba(63,185,80,0.15);color:var(--success)">' + (l.type || 'insight') + '</span>';
    html += '<div class="card-content" style="margin-top:0.3rem">' + esc(l.content || '') + '</div>';
    if (tagHtml) html += '<div style="margin-top:0.5rem">' + tagHtml + '</div>';
    html += '</div><div class="card-actions"><button class="btn-danger" onclick="deleteLesson(\'' + l.id + '\')">Delete</button></div></div>';
    html += '</div>';
    return html;
  }).join('');
}

async function deleteLesson(id) {
  if (!confirm('Delete this lesson?')) return;
  try {
    await apiFetch('/lessons/' + id, { method: 'DELETE' });
    showToast('Lesson deleted', 'success');
    loadLessons();
    updateStatus();
  } catch (err) {
    showToast('Failed: ' + err.message, 'error');
  }
}

function openLessonModal() {
  const modal = document.getElementById('modal');
  document.getElementById('modal-title').textContent = 'Add Lesson';
  document.getElementById('modal-body').innerHTML = lessonFormHTML();
  modal.classList.remove('hidden');
  setupLessonForm();
}

function lessonFormHTML() {
  return '<form id="lesson-form"><div class="form-group"><label>Content</label><textarea id="lesson-content" placeholder="Lesson content..." required></textarea></div><div class="form-group"><label>Type</label><select id="lesson-type"><option value="insight">Insight</option><option value="warning">Warning</option><option value="practice">Practice</option></select></div><div class="form-group"><label>Tags (comma-separated)</label><input type="text" id="lesson-tags" placeholder="tag1, tag2"></div><div class="form-actions"><button class="btn-ghost" onclick="closeModal()">Cancel</button><button class="btn-primary" id="lesson-submit">Add</button></div></form>';
}

function setupLessonForm() {
  document.getElementById('lesson-submit').addEventListener('click', async () => {
    const content = document.getElementById('lesson-content').value.trim();
    const type = document.getElementById('lesson-type').value;
    const tagsStr = document.getElementById('lesson-tags').value.trim();
    const tags = tagsStr ? tagsStr.split(',').map(t => t.trim()).filter(Boolean) : [];
    if (!content) { showToast('Content required', 'error'); return; }
    try {
      await apiFetch('/lessons', {
        method: 'POST',
        body: JSON.stringify({ content, type, tags }),
      });
      showToast('Lesson added', 'success');
      closeModal();
      loadLessons();
      updateStatus();
    } catch (err) {
      showToast('Error: ' + err.message, 'error');
    }
  });
}

// ==================== Directives ====================

async function loadDirectives() {
  const el = document.getElementById('directives-list');
  el.innerHTML = '<div class="loading"><div class="spinner"></div>Loading...</div>';
  try {
    const data = await apiFetch('/memories?is_prime_directive=1&limit=100');
    if (!data) return;
    renderMemories(data.items || [], el);
  } catch (err) {
    el.innerHTML = '<div class="empty-state"><p>Failed to load: ' + esc(err.message) + '</p></div>';
  }
}

// ==================== Telemetry ====================

async function loadTelemetry() {
  const el = document.getElementById('telemetry-dashboard');
  el.innerHTML = '<div class="loading"><div class="spinner"></div>Loading telemetry...</div>';
  try {
    const data = await apiFetch('/stats');
    if (!data) return;
    renderTelemetry(data, el);
  } catch (err) {
    el.innerHTML = '<div class="empty-state"><p>Failed to load telemetry: ' + esc(err.message) + '</p></div>';
  }
}

function renderTelemetry(data, container) {
  let html = '';
  for (const [agent, models] of Object.entries(data)) {
    html += '<div class="telemetry-agent"><div class="telemetry-agent-name">' + esc(agent) + '</div>';
    for (const [model, personas] of Object.entries(models)) {
      html += '<div class="telemetry-model"><div class="telemetry-model-name">' + esc(model) + '</div>';
      for (const [persona, stats] of Object.entries(personas)) {
        const active = stats.active || 0;
        const decayed = stats.decayed || 0;
        const total = active + decayed;
        const isr = total > 0 ? ((active / total) * 100).toFixed(1) : 0;
        const isrClass = isr > 80 ? 'isr-high' : (isr < 40 ? 'isr-low' : '');
        html += '<div class="telemetry-persona">';
        html += '<span class="telemetry-persona-name">' + esc(persona) + '</span>';
        html += '<span class="telemetry-stat">Active: <strong>' + active + '</strong></span>';
        html += '<span class="telemetry-stat">Decayed: <strong>' + decayed + '</strong></span>';
        html += '<span class="telemetry-stat isr ' + isrClass + '">ISR: <strong>' + isr + '%</strong></span>';
        html += '</div>';
      }
      html += '</div>';
    }
    html += '</div>';
  }
  container.innerHTML = html || '<div class="empty-state"><p>No telemetry data available</p></div>';
}

// ==================== Utilities ====================

function closeModal() {
  document.getElementById('modal').classList.add('hidden');
}

function esc(s) {
  if (s == null) return '';
  return String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');
}

function q(s) {
  return String(s).replace(/'/g, "\\'");
}

function parseTags(tags) {
  if (!tags) return [];
  if (Array.isArray(tags)) return tags;
  try { return JSON.parse(tags); } catch {}
  return [];
}

function parseJSON(s, fallback) {
  if (!s) return fallback;
  if (typeof s === 'object') return s;
  try { return JSON.parse(s); } catch {}
  return fallback;
}
