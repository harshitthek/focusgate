// @ts-check
(() => {
  /**
   * @typedef {'normal' | 'deep_work' | 'dnd'} FocusMode
   * @typedef {'queued' | 'pushed' | 'digested' | 'archived' | 'read'} ItemStatus
   * @typedef {'queued' | 'pushed' | 'digested' | 'archived' | 'all'} FilterStatus
   *
   * @typedef {Object} Stats
   * @property {FocusMode} mode
   * @property {number} queued_count
   * @property {number} pushed_today
   * @property {number} digested_today
   * @property {number} archived_today
   *
   * @typedef {Object} RuntimeStatus
   * @property {Stats} stats
   * @property {number} heap_mb
   * @property {number} sys_mb
   * @property {number} db_size_kb
   * @property {string} classifier
   * @property {boolean} ntfy_enabled
   *
   * @typedef {Object} StoredItem
   * @property {number} id
   * @property {string} source_type
   * @property {string} source_name
   * @property {string} title
   * @property {string} body
   * @property {string} url
   * @property {number} priority
   * @property {boolean} uncertain
   * @property {string} decision_source
   * @property {string} decision_reason
   * @property {ItemStatus} status
   * @property {string} created_at
   */

  const qs = new URLSearchParams(location.search);
  const urlToken = qs.get('token');
  if (urlToken) {
    localStorage.setItem('fg_token', urlToken);
    history.replaceState({}, '', '/');
  }

  /** @type {FilterStatus} */
  let currentFilter = 'queued';
  /** @type {EventSource | null} */
  let evtSource = null;

  /** @returns {string} */
  const token = () => localStorage.getItem('fg_token') || '';
  /** @returns {Record<string, string>} */
  const authHeaders = () => (token() ? { Authorization: 'Bearer ' + token() } : {});

  /**
   * @template T
   * @param {string} path
   * @param {RequestInit} [opts]
   * @returns {Promise<T>}
   */
  async function api(path, opts = {}) {
    const res = await fetch(path, {
      ...opts,
      headers: {
        'Content-Type': 'application/json',
        ...authHeaders(),
        ...(/** @type {Record<string, string>} */ (opts.headers) || {}),
      },
    });
    if (res.status === 401) {
      document.getElementById('auth-panel')?.classList.remove('hidden');
      throw new Error('unauthorized');
    }
    if (!res.ok) {
      const msg = await res.text();
      throw new Error(msg || res.statusText);
    }
    return /** @type {Promise<T>} */ (res.json());
  }

  /**
   * @param {string} id
   * @param {string} text
   */
  function setText(id, text) {
    const el = document.getElementById(id);
    if (el) el.textContent = text;
  }

  /** @param {RuntimeStatus | null | undefined} st */
  function renderStatus(st) {
    if (!st || !st.stats) return;
    const mode = st.stats.mode;
    document.querySelectorAll('.mode-pill').forEach((node) => {
      const btn = /** @type {HTMLButtonElement} */ (node);
      btn.classList.toggle('active', btn.dataset.mode === mode);
    });
    setText('stat-queued', String(st.stats.queued_count));
    setText('stat-pushed', String(st.stats.pushed_today));
    setText('stat-digested', String(st.stats.digested_today));
    setText('stat-archived', String(st.stats.archived_today));
    setText('stat-classifier', st.classifier);
    setText(
      'metrics-badge',
      `Heap ${st.heap_mb.toFixed(1)} MB · DB ${st.db_size_kb.toFixed(1)} KB`
    );
  }

  /**
   * @param {string | null | undefined} str
   * @returns {string}
   */
  function escapeHTML(str) {
    const map = /** @type {Record<string, string>} */ ({
      '&': '&amp;',
      '<': '&lt;',
      '>': '&gt;',
      '"': '&quot;',
      "'": '&#39;',
    });
    return (str || '').replace(/[&<>"']/g, (c) => map[c] || c);
  }

  /** @param {StoredItem[] | null | undefined} items */
  function renderItems(items) {
    const root = document.getElementById('items-list');
    if (!root) return;
    if (!items || items.length === 0) {
      root.innerHTML = '<div class="empty">No items in this view yet.</div>';
      return;
    }
    root.innerHTML = items.map((it) => {
      const safeURL = /^https?:\/\//i.test(it.url || '') ? it.url : '';
      const titleHTML = safeURL
        ? `<a href="${escapeHTML(safeURL)}" target="_blank" rel="noopener noreferrer">${escapeHTML(it.title)}</a>`
        : escapeHTML(it.title);
      const reviewBadge = it.uncertain ? '<span class="badge review">review</span>' : '';
      const actions = it.status === 'queued' || it.status === 'pushed'
        ? `<button class="btn small" data-action="read" data-id="${it.id}">Mark Read</button>
           <button class="btn small ghost" data-action="archived" data-id="${it.id}">Archive</button>`
        : '';
      return `
        <article class="card">
          <div class="card-top">
            <div class="badges">
              <span class="badge">${escapeHTML(it.source_type)}</span>
              <span class="badge">${escapeHTML(it.source_name)}</span>
              <span class="badge prio-${it.priority}">P${it.priority}</span>
              ${reviewBadge}
            </div>
            <span>${new Date(it.created_at).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })}</span>
          </div>
          <h3 class="card-title">${titleHTML}</h3>
          ${it.body ? `<p class="card-body">${escapeHTML(it.body)}</p>` : ''}
          <div class="card-foot">
            <span>${escapeHTML(it.decision_source)} · ${escapeHTML(it.decision_reason)}</span>
            <div>${actions}</div>
          </div>
        </article>`;
    }).join('');
  }

  async function refresh() {
    try {
      const [st, data] = await Promise.all([
        /** @type {Promise<RuntimeStatus>} */ (api('/api/status')),
        /** @type {Promise<{items: StoredItem[]}>} */ (
          api(`/api/items?status=${encodeURIComponent(currentFilter)}&limit=50`)
        ),
      ]);
      document.getElementById('auth-panel')?.classList.add('hidden');
      renderStatus(st);
      renderItems(data.items);
    } catch (_) {}
  }

  function connectSSE() {
    if (evtSource) evtSource.close();
    const url = token() ? `/api/events?token=${encodeURIComponent(token())}` : '/api/events';
    evtSource = new EventSource(url);
    const dot = document.getElementById('sse-dot');
    evtSource.onopen = () => dot?.classList.add('live');
    evtSource.onerror = () => dot?.classList.remove('live');
    evtSource.addEventListener('status', (e) => {
      try {
        const parsed = /** @type {{data: RuntimeStatus}} */ (JSON.parse(/** @type {MessageEvent} */ (e).data));
        renderStatus(parsed.data);
      } catch (_) {}
    });
    evtSource.addEventListener('item', () => refresh());
    evtSource.addEventListener('digest', () => refresh());
    evtSource.addEventListener('mode', () => refresh());
  }

  document.querySelectorAll('.mode-pill').forEach((node) => {
    const btn = /** @type {HTMLButtonElement} */ (node);
    btn.addEventListener('click', async () => {
      const st = /** @type {RuntimeStatus} */ (
        await api('/api/mode', { method: 'POST', body: JSON.stringify({ mode: btn.dataset.mode }) })
      );
      renderStatus(st);
    });
  });

  document.querySelectorAll('.tab').forEach((node) => {
    const tab = /** @type {HTMLButtonElement} */ (node);
    tab.addEventListener('click', () => {
      document.querySelectorAll('.tab').forEach((t) => t.classList.remove('active'));
      tab.classList.add('active');
      currentFilter = /** @type {FilterStatus} */ (tab.dataset.status || 'queued');
      refresh();
    });
  });

  document.getElementById('items-list')?.addEventListener('click', async (e) => {
    const target = /** @type {HTMLElement | null} */ (e.target);
    const btn = /** @type {HTMLButtonElement | null} */ (target?.closest('button[data-action]') || null);
    if (!btn) return;
    await api(`/api/items/${btn.dataset.id}/status`, {
      method: 'POST',
      body: JSON.stringify({ status: btn.dataset.action }),
    });
    refresh();
  });

  document.getElementById('sync-btn')?.addEventListener('click', async (e) => {
    const btn = /** @type {HTMLButtonElement} */ (e.currentTarget);
    btn.disabled = true;
    try { await api('/api/sync', { method: 'POST' }); await refresh(); } finally { btn.disabled = false; }
  });

  document.getElementById('flush-btn')?.addEventListener('click', async (e) => {
    const btn = /** @type {HTMLButtonElement} */ (e.currentTarget);
    btn.disabled = true;
    try { await api('/api/digest/flush', { method: 'POST' }); await refresh(); } finally { btn.disabled = false; }
  });

  document.getElementById('sim-toggle')?.addEventListener('click', () => {
    document.getElementById('sim-panel')?.classList.toggle('hidden');
  });
  document.getElementById('sim-close')?.addEventListener('click', () => {
    document.getElementById('sim-panel')?.classList.add('hidden');
  });

  document.getElementById('sim-form')?.addEventListener('submit', async (e) => {
    e.preventDefault();
    const out = document.getElementById('sim-result');
    if (!out) return;
    const payload = {
      source_type: /** @type {HTMLSelectElement} */ (document.getElementById('sim-source')).value,
      mode: /** @type {HTMLSelectElement} */ (document.getElementById('sim-mode')).value,
      title: /** @type {HTMLInputElement} */ (document.getElementById('sim-title')).value,
      body: /** @type {HTMLTextAreaElement} */ (document.getElementById('sim-body')).value,
      sender: /** @type {HTMLInputElement} */ (document.getElementById('sim-sender')).value,
    };
    try {
      const dec = await api('/api/simulate', { method: 'POST', body: JSON.stringify(payload) });
      out.textContent = JSON.stringify(dec, null, 2);
    } catch (err) {
      out.textContent = 'Simulation error: ' + (err instanceof Error ? err.message : String(err));
    }
    out.classList.remove('hidden');
  });

  document.getElementById('auth-form')?.addEventListener('submit', (e) => {
    e.preventDefault();
    const input = /** @type {HTMLInputElement | null} */ (document.getElementById('auth-input'));
    const val = input ? input.value.trim() : '';
    if (val) {
      localStorage.setItem('fg_token', val);
      connectSSE();
      refresh();
    }
  });

  if ('serviceWorker' in navigator) {
    navigator.serviceWorker.register('/sw.js').catch(() => {});
  }

  connectSSE();
  refresh();
})();
