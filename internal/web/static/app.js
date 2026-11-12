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

  /** @type {Record<FocusMode, { lines: [string, string, string], sub: string, roll: string, angle: string }>} */
  const MODE_COPY = {
    normal: {
      lines: ['PASS', 'URGENT', 'SIGNAL'],
      sub: 'Routing high-priority alerts across your devices in real time. Holding low-priority feeds for your scheduled digest.',
      roll: 'NORMAL',
      angle: '-38deg',
    },
    deep_work: {
      lines: ['DEEP', 'WORK', 'LOCKED'],
      sub: 'Silencing routine feeds and PR pings into your digest queue. Only critical outages break through.',
      roll: 'DEEP WORK',
      angle: '0deg',
    },
    dnd: {
      lines: ['SLEEP', 'MODE', 'SILENT'],
      sub: 'Holding updates until your next scheduled digest. Only high-confidence emergencies pierce Do Not Disturb.',
      roll: 'DND',
      angle: '38deg',
    },
  };

  /** @type {FocusMode[]} */
  const MODE_ORDER = ['normal', 'deep_work', 'dnd'];

  const qs = new URLSearchParams(location.search);
  const urlToken = qs.get('token');
  if (urlToken) {
    localStorage.setItem('fg_token', urlToken);
    history.replaceState({}, '', '/');
  }

  /** @type {FocusMode} */
  let currentMode = 'normal';
  /** @type {FilterStatus} */
  let currentFilter = 'queued';
  /** @type {string} */
  let searchQuery = '';
  /** @type {StoredItem[]} */
  let lastItems = [];
  /** @type {EventSource | null} */
  let evtSource = null;
  /** @type {number} */
  let scrollRAF = 0;

  /** @returns {string} */
  const token = () => localStorage.getItem('fg_token') || '';
  /** @returns {Record<string, string>} */
  const authHeaders = () => (token() ? { Authorization: 'Bearer ' + token() } : {});

  /**
   * Generates a procedural cellular crease heightfield (F2 - F1 Voronoi ridges)
   * on the full-page fiery-orange crumpled paper background.
   */
  function renderCrumpledPaper() {
    const shadowCanvas = document.getElementById('paper-shadow-canvas');
    const lightCanvas = document.getElementById('paper-light-canvas');
    if (!(shadowCanvas instanceof HTMLCanvasElement) || !(lightCanvas instanceof HTMLCanvasElement)) return;

    const w = 420;
    const h = 280;
    shadowCanvas.width = w;
    shadowCanvas.height = h;
    lightCanvas.width = w;
    lightCanvas.height = h;

    const sCtx = shadowCanvas.getContext('2d');
    const lCtx = lightCanvas.getContext('2d');
    if (!sCtx || !lCtx) return;

    let seed = 7331;
    const rand = () => {
      seed = (seed * 16807) % 2147483647;
      return (seed - 1) / 2147483646;
    };

    const macroPts = Array.from({ length: 46 }, () => ({ x: rand() * w, y: rand() * h }));
    const microPts = Array.from({ length: 135 }, () => ({ x: rand() * w, y: rand() * h }));

    /**
     * @param {number} x
     * @param {number} y
     * @param {Array<{x: number, y: number}>} pts
     * @param {number} scale
     */
    const ridgeHeight = (x, y, pts, scale) => {
      let d1 = 1e9;
      let d2 = 1e9;
      for (let i = 0; i < pts.length; i++) {
        const dx = x - pts[i].x;
        const dy = y - pts[i].y;
        const d = Math.sqrt(dx * dx + dy * dy);
        if (d < d1) {
          d2 = d1;
          d1 = d;
        } else if (d < d2) {
          d2 = d;
        }
      }
      return Math.min(1, (d2 - d1) / scale);
    };

    const heightMap = new Float32Array(w * h);
    for (let y = 0; y < h; y++) {
      for (let x = 0; x < w; x++) {
        const hMacro = ridgeHeight(x, y, macroPts, 38);
        const hMicro = ridgeHeight(x, y, microPts, 16);
        const grain = (rand() - 0.5) * 0.04;
        heightMap[y * w + x] = hMacro * 0.68 + hMicro * 0.32 + grain;
      }
    }

    const sImg = sCtx.createImageData(w, h);
    const lImg = lCtx.createImageData(w, h);
    const sData = sImg.data;
    const lData = lImg.data;

    for (let y = 0; y < h; y++) {
      const yUp = y > 0 ? y - 1 : 0;
      const yDn = y < h - 1 ? y + 1 : h - 1;
      for (let x = 0; x < w; x++) {
        const xLt = x > 0 ? x - 1 : 0;
        const xRt = x < w - 1 ? x + 1 : w - 1;
        const gx = (heightMap[y * w + xRt] - heightMap[y * w + xLt]) * 5.2;
        const gy = (heightMap[yDn * w + x] - heightMap[yUp * w + x]) * 5.2;
        const shade = gx * 0.58 + gy * 0.68;

        const idx = (y * w + x) * 4;
        const shadowVal = Math.max(150, Math.min(255, Math.round(234 + shade * 62)));
        sData[idx] = shadowVal;
        sData[idx + 1] = Math.round(shadowVal * 0.94);
        sData[idx + 2] = Math.round(shadowVal * 0.90);
        sData[idx + 3] = 255;

        const lightVal = Math.max(60, Math.min(255, Math.round(128 + shade * 115)));
        lData[idx] = lightVal;
        lData[idx + 1] = lightVal;
        lData[idx + 2] = lightVal;
        lData[idx + 3] = 255;
      }
    }

    sCtx.putImageData(sImg, 0, 0);
    lCtx.putImageData(lImg, 0, 0);
  }

  /** Updates `--tape-open` CSS variable (0..1) so scrolling unrolls the right-edge tape. */
  function updateScrollTape() {
    scrollRAF = 0;
    const maxScroll = Math.max(240, document.documentElement.scrollHeight - window.innerHeight);
    const progress = Math.max(0, Math.min(1, window.scrollY / maxScroll));
    document.documentElement.style.setProperty('--tape-open', progress.toFixed(4));
  }

  window.addEventListener('scroll', () => {
    if (!scrollRAF) {
      scrollRAF = requestAnimationFrame(updateScrollTape);
    }
  }, { passive: true });
  window.addEventListener('resize', () => {
    if (!scrollRAF) {
      scrollRAF = requestAnimationFrame(updateScrollTape);
    }
  }, { passive: true });

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
      document.getElementById('inbox-section')?.scrollIntoView({ behavior: 'smooth' });
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
    currentMode = st.stats.mode;
    document.querySelectorAll('.mode-pill').forEach((node) => {
      const btn = /** @type {HTMLButtonElement} */ (node);
      btn.classList.toggle('active', btn.dataset.mode === currentMode);
    });

    const copy = MODE_COPY[currentMode] || MODE_COPY.normal;
    setText('hl-line1', copy.lines[0]);
    setText('hl-line2', copy.lines[1]);
    setText('hl-line3', copy.lines[2]);
    setText('dynamic-subtitle', copy.sub);
    setText('roll-count', String(st.stats.queued_count));
    setText('roll-mode', copy.roll);
    setText('tape-rail-count', `${st.stats.queued_count} QUEUED`);
    setText('explore-label', `EXPLORE QUEUE (${st.stats.queued_count})`);

    const pagerBtn = document.getElementById('hero-dial-btn');
    if (pagerBtn) {
      pagerBtn.dataset.mode = currentMode;
    }

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
    const note = document.getElementById('search-active-note');
    if (!root) return;

    const list = items || [];
    const q = searchQuery.trim().toLowerCase();
    const filtered = q
      ? list.filter((it) =>
          (it.title || '').toLowerCase().includes(q) ||
          (it.body || '').toLowerCase().includes(q) ||
          (it.source_name || '').toLowerCase().includes(q) ||
          (it.source_type || '').toLowerCase().includes(q)
        )
      : list;

    if (note) {
      if (q) {
        note.textContent = `Filter: "${searchQuery}" (${filtered.length})`;
        note.classList.remove('hidden');
      } else {
        note.classList.add('hidden');
      }
    }

    if (filtered.length === 0) {
      root.innerHTML = '<div class="empty">No items match this view yet.</div>';
      return;
    }

    root.innerHTML = filtered.map((it) => {
      const safeURL = /^https?:\/\//i.test(it.url || '') ? it.url : '';
      const titleHTML = safeURL
        ? `<a href="${escapeHTML(safeURL)}" target="_blank" rel="noopener noreferrer">${escapeHTML(it.title)}</a>`
        : escapeHTML(it.title);
      const urgentClass = it.priority >= 4 ? ' card-urgent' : '';
      const reviewBadge = it.uncertain ? '<span class="badge review">review</span>' : '';
      const actions = it.status === 'queued' || it.status === 'pushed'
        ? `<button class="btn-card" data-action="read" data-id="${it.id}">Mark Read</button>
           <button class="btn-card" data-action="archived" data-id="${it.id}">Archive</button>`
        : '';
      return `
        <article class="card${urgentClass}">
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
      lastItems = data.items || [];
      renderStatus(st);
      renderItems(lastItems);
      updateScrollTape();
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

  /** @param {FocusMode} mode */
  async function switchMode(mode) {
    const st = /** @type {RuntimeStatus} */ (
      await api('/api/mode', { method: 'POST', body: JSON.stringify({ mode }) })
    );
    renderStatus(st);
  }

  /** @param {FilterStatus} status */
  function selectTab(status) {
    currentFilter = status;
    document.querySelectorAll('.tab').forEach((node) => {
      const tab = /** @type {HTMLButtonElement} */ (node);
      tab.classList.toggle('active', tab.dataset.status === status);
    });
    refresh();
  }

  document.querySelectorAll('.mode-pill').forEach((node) => {
    const btn = /** @type {HTMLButtonElement} */ (node);
    btn.addEventListener('click', () => {
      const next = /** @type {FocusMode} */ (btn.dataset.mode || 'normal');
      switchMode(next);
    });
  });

  document.getElementById('hero-dial-btn')?.addEventListener('click', () => {
    const idx = MODE_ORDER.indexOf(currentMode);
    const next = MODE_ORDER[(idx + 1) % MODE_ORDER.length];
    switchMode(next);
  });

  document.querySelectorAll('.tab').forEach((node) => {
    const tab = /** @type {HTMLButtonElement} */ (node);
    tab.addEventListener('click', () => {
      selectTab(/** @type {FilterStatus} */ (tab.dataset.status || 'queued'));
    });
  });

  document.querySelectorAll('[data-jump]').forEach((node) => {
    const btn = /** @type {HTMLButtonElement} */ (node);
    btn.addEventListener('click', () => {
      selectTab(/** @type {FilterStatus} */ (btn.dataset.jump || 'queued'));
      document.getElementById('inbox-section')?.scrollIntoView({ behavior: 'smooth' });
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

  const simBackdrop = document.getElementById('sim-backdrop');
  document.getElementById('sim-toggle')?.addEventListener('click', () => {
    simBackdrop?.classList.remove('hidden');
  });
  document.getElementById('sim-close')?.addEventListener('click', () => {
    simBackdrop?.classList.add('hidden');
  });
  simBackdrop?.addEventListener('click', (e) => {
    if (e.target === e.currentTarget) simBackdrop.classList.add('hidden');
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

  const searchPopover = document.getElementById('search-popover');
  const searchInput = document.getElementById('search-input');
  document.getElementById('search-toggle')?.addEventListener('click', () => {
    if (!searchPopover) return;
    searchPopover.classList.toggle('hidden');
    if (!searchPopover.classList.contains('hidden') && searchInput instanceof HTMLInputElement) {
      searchInput.focus();
    }
  });
  if (searchInput instanceof HTMLInputElement) {
    searchInput.addEventListener('input', () => {
      searchQuery = searchInput.value;
      renderItems(lastItems);
    });
  }
  document.getElementById('search-form')?.addEventListener('submit', (e) => {
    e.preventDefault();
    searchPopover?.classList.add('hidden');
    document.getElementById('inbox-section')?.scrollIntoView({ behavior: 'smooth' });
  });
  document.getElementById('search-clear')?.addEventListener('click', () => {
    searchQuery = '';
    if (searchInput instanceof HTMLInputElement) searchInput.value = '';
    renderItems(lastItems);
    searchPopover?.classList.add('hidden');
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

  window.addEventListener('keydown', (e) => {
    if (e.key === 'Escape') {
      simBackdrop?.classList.add('hidden');
      searchPopover?.classList.add('hidden');
    }
  });

  if ('serviceWorker' in navigator) {
    navigator.serviceWorker.register('/sw.js').catch(() => {});
  }

  renderCrumpledPaper();
  updateScrollTape();
  connectSSE();
  refresh();
})();
