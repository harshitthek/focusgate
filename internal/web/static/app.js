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
   * @typedef {Object} SourceSummary
   * @property {string} kind
   * @property {string} name
   * @property {boolean} enabled
   * @property {string} detail
   *
   * @typedef {Object} RuntimeStatus
   * @property {Stats} stats
   * @property {number} heap_mb
   * @property {number} sys_mb
   * @property {number} db_size_kb
   * @property {string} classifier
   * @property {boolean} ntfy_enabled
   * @property {boolean} [desktop_enabled]
   * @property {string} [next_digest_at]
   * @property {string} [next_digest_hhmm]
   * @property {string[]} [digest_times]
   * @property {number} [api_used_today]
   * @property {number} [api_daily_limit]
   * @property {SourceSummary[]} [sources]
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
  /** @type {boolean} */
  let hasInitialStatus = false;
  /** @type {number} */
  let flickerTimer = 0;
  /** @type {FilterStatus} */
  let currentFilter = 'queued';
  /** @type {string} */
  let sourceFilter = 'all';
  /** @type {string} */
  let searchQuery = '';
  /** @type {StoredItem[]} */
  let lastItems = [];
  /** @type {StoredItem[]} */
  let filteredItems = [];
  /** @type {RuntimeStatus | null} */
  let lastStatus = null;
  /** @type {boolean} */
  let sfxEnabled = localStorage.getItem('fg_sfx') === 'on';
  /** @type {AudioContext | null} */
  let audioCtx = null;
  /** @type {number} */
  let refreshSeq = 0;
  /** @type {EventSource | null} */
  let evtSource = null;
  /** @type {number} */
  let scrollRAF = 0;

  /** @returns {string} */
  const token = () => localStorage.getItem('fg_session') || localStorage.getItem('fg_token') || '';
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
      let d1Sq = 1e18;
      let d2Sq = 1e18;
      for (let i = 0; i < pts.length; i++) {
        const dx = x - pts[i].x;
        const dy = y - pts[i].y;
        const dSq = dx * dx + dy * dy;
        if (dSq < d1Sq) {
          d2Sq = d1Sq;
          d1Sq = dSq;
        } else if (dSq < d2Sq) {
          d2Sq = dSq;
        }
      }
      return Math.min(1, (Math.sqrt(d2Sq) - Math.sqrt(d1Sq)) / scale);
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

  /** Updates `--tape-open` CSS variable (0..1) and tape labels as the user scrolls. */
  function updateScrollTape() {
    scrollRAF = 0;
    const maxScroll = Math.max(240, document.documentElement.scrollHeight - window.innerHeight);
    const progress = Math.max(0, Math.min(1, window.scrollY / maxScroll));
    document.documentElement.style.setProperty('--tape-open', progress.toFixed(4));

    const isOpen = progress >= 0.2;
    const strip = document.getElementById('tape-unroll-strip');
    strip?.classList.toggle('is-open', isOpen);
    const topChip = document.getElementById('tape-top-chip');
    if (topChip) {
      const isVisible = isOpen && (strip ? strip.offsetHeight > 80 : true) && topChip.offsetParent !== null;
      topChip.classList.toggle('is-visible', isVisible);
      topChip.tabIndex = isVisible ? 0 : -1;
      topChip.setAttribute('aria-hidden', String(!isVisible));
    }

    const pct = Math.round(progress * 100);
    setText('tape-rail-pct', `${pct}%`);
    document.getElementById('tape-unroll-strip')?.setAttribute('aria-valuenow', String(pct));
    const spoolBtn = document.getElementById('tape-spool-badge');
    const dirText = document.getElementById('tape-spool-dir');
    if (progress > 0.25) {
      spoolBtn?.setAttribute('aria-label', 'Rewind tape back to top');
      if (dirText) dirText.textContent = 'TOP ↑';
    } else {
      spoolBtn?.setAttribute('aria-label', 'Hold and drag PULL ↓ on tape below to unroll');
      if (dirText) dirText.textContent = 'DRAG ↓';
    }
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

  /** Triggers randomized LED flicker across the pager dot matrix when clicked or changed. */
  function triggerPagerFlicker() {
    const pagerBtn = document.getElementById('hero-dial-btn');
    const dots = Array.from(document.querySelectorAll('#pager-grille .pager-dot'));
    if (!dots.length) return;

    if (flickerTimer) {
      window.clearInterval(flickerTimer);
      window.clearTimeout(flickerTimer);
      flickerTimer = 0;
    }

    const flickerClasses = ['lit-orange', 'lit-amber', 'lit-cream'];
    const clearDots = () => {
      dots.forEach((dot) => dot.classList.remove(...flickerClasses));
      pagerBtn?.classList.remove('is-flickering');
    };

    /** @param {number} density */
    const paintRandomFrame = (density) => {
      dots.forEach((dot) => {
        dot.classList.remove(...flickerClasses);
        if (Math.random() < density) {
          const cls = flickerClasses[Math.floor(Math.random() * flickerClasses.length)];
          dot.classList.add(cls);
        }
      });
    };

    pagerBtn?.classList.add('is-flickering');

    if (window.matchMedia('(prefers-reduced-motion: reduce)').matches) {
      paintRandomFrame(0.35);
      flickerTimer = window.setTimeout(() => {
        clearDots();
        flickerTimer = 0;
      }, 240);
      return;
    }

    let step = 0;
    const totalSteps = 12;
    paintRandomFrame(0.5);
    flickerTimer = window.setInterval(() => {
      step += 1;
      if (step >= totalSteps) {
        window.clearInterval(flickerTimer);
        flickerTimer = 0;
        clearDots();
        return;
      }
      const density = step > totalSteps - 3 ? 0.24 : 0.45;
      paintRandomFrame(density);
    }, 60);
  }

  /** Plays a subtle hardware pager beep when SFX is enabled by the user. @param {FocusMode} mode */
  function playPagerChirp(mode) {
    if (!sfxEnabled) return;
    const win = /** @type {Window & { webkitAudioContext?: typeof AudioContext }} */ (window);
    const AudioCtor = window.AudioContext || win.webkitAudioContext;
    if (!AudioCtor) return;
    try {
      if (!audioCtx) audioCtx = new AudioCtor();
      if (audioCtx.state === 'suspended') audioCtx.resume().catch(() => {});
      const now = audioCtx.currentTime;
      const baseFreq = mode === 'deep_work' ? 740 : mode === 'dnd' ? 520 : 880;
      const osc = audioCtx.createOscillator();
      const gain = audioCtx.createGain();
      osc.type = 'triangle';
      osc.frequency.setValueAtTime(baseFreq, now);
      osc.frequency.setValueAtTime(baseFreq * 1.25, now + 0.045);
      gain.gain.setValueAtTime(0.001, now);
      gain.gain.exponentialRampToValueAtTime(0.06, now + 0.01);
      gain.gain.exponentialRampToValueAtTime(0.0001, now + 0.11);
      osc.connect(gain);
      gain.connect(audioCtx.destination);
      osc.start(now);
      osc.stop(now + 0.12);
    } catch (_) {}
  }

  /** Updates the next scheduled digest countdown badge in the inbox bar. */
  function updateNextDigestBadge() {
    const st = lastStatus;
    if (!st || !st.next_digest_at || !st.next_digest_hhmm) {
      setText('next-digest-badge', 'DIGEST MANUAL');
      return;
    }
    const diffMs = new Date(st.next_digest_at).getTime() - Date.now();
    if (!Number.isFinite(diffMs) || diffMs <= 0) {
      setText('next-digest-badge', `NEXT ${st.next_digest_hhmm} (soon)`);
      return;
    }
    const totalMins = Math.max(1, Math.round(diffMs / 60000));
    const hrs = Math.floor(totalMins / 60);
    const mins = totalMins % 60;
    const rem = hrs > 0 ? `${hrs}h ${mins}m` : `${mins}m`;
    setText('next-digest-badge', `NEXT ${st.next_digest_hhmm} (${rem})`);
  }

  /** Renders runtime delivery channels, TypeSafe API quota, and sources list inside the drawer. */
  function renderSourcesDrawer() {
    const st = lastStatus;
    if (!st) return;
    setText('chan-desktop', `Desktop ${st.desktop_enabled ? 'ON' : 'OFF'}`);
    setText('chan-ntfy', `ntfy ${st.ntfy_enabled ? 'ON' : 'OFF'}`);
    const used = st.api_used_today ?? 0;
    const limit = st.api_daily_limit ?? 200;
    setText('quota-usage', `API ${used} / ${limit} today`);
    const slots = (st.digest_times && st.digest_times.length) ? st.digest_times.join(' · ') : 'none';
    setText('digest-schedule-text', `Digest slots: ${slots}`);

    const listEl = document.getElementById('sources-list');
    if (!listEl) return;
    const sources = st.sources || [];
    if (!sources.length) {
      listEl.innerHTML = '<div class="empty">No sources configured.</div>';
      return;
    }
    listEl.innerHTML = sources.map((src) => `
      <div class="source-row">
        <div class="source-row-left">
          <span class="badge">${escapeHTML(src.kind)}</span>
          <span class="source-row-name">${escapeHTML(src.name)}</span>
        </div>
        <span class="source-row-detail">${src.enabled ? 'ACTIVE' : 'OFF'} · ${escapeHTML(src.detail)}</span>
      </div>`).join('');
  }

  let modeSeq = 0;
  let isSwitchingMode = false;
  /** @type {FocusMode | null} */
  let pendingMode = null;

  /** @type {ReturnType<typeof setTimeout> | null} */
  let activeToastTimeout = null;

  /**
   * @param {string} msg
   * @param {(() => void) | null} [retryFn]
   */
  function showActionError(msg, retryFn) {
    let container = document.getElementById('action-error-toast');
    if (!container) {
      container = document.createElement('div');
      container.id = 'action-error-toast';
      container.setAttribute('role', 'alert');
      container.style.cssText =
        'position: fixed; bottom: 20px; right: 20px; z-index: 9999; background: #fff1f0; border: 1px solid #ffa39e; padding: 12px 16px; border-radius: 8px; box-shadow: 0 4px 12px rgba(0,0,0,0.15); display: flex; align-items: center; gap: 12px; font-family: var(--font-mono, monospace); font-size: 13px; color: #cf1322;';
      document.body.appendChild(container);
    }
    if (activeToastTimeout !== null) {
      clearTimeout(activeToastTimeout);
      activeToastTimeout = null;
    }
    container.innerHTML = `
      <span>${escapeHTML(msg)}</span>
      ${retryFn ? '<button class="pill-cta small-cta" id="toast-retry-btn" type="button" style="padding: 4px 10px; font-size: 11px;"><span>RETRY</span></button>' : ''}
      <button type="button" id="toast-close-btn" style="background: none; border: none; font-size: 16px; cursor: pointer; color: #cf1322; line-height: 1;" aria-label="Close">&times;</button>
    `;
    container.style.display = 'flex';
    document.getElementById('toast-close-btn')?.addEventListener('click', () => {
      if (activeToastTimeout !== null) {
        clearTimeout(activeToastTimeout);
        activeToastTimeout = null;
      }
      if (container) container.style.display = 'none';
    });
    if (retryFn) {
      document.getElementById('toast-retry-btn')?.addEventListener('click', () => {
        if (activeToastTimeout !== null) {
          clearTimeout(activeToastTimeout);
          activeToastTimeout = null;
        }
        if (container) container.style.display = 'none';
        retryFn();
      });
    }
    activeToastTimeout = setTimeout(() => {
      if (container) container.style.display = 'none';
      activeToastTimeout = null;
    }, 6000);
  }

  /**
   * @param {RuntimeStatus | null | undefined} st
   * @param {number} [seq]
   */
  function renderStatus(st, seq) {
    if (!st || !st.stats) return;
    if (seq !== undefined && seq < modeSeq) return;
    if (isSwitchingMode && pendingMode) {
      if (st.stats.mode !== pendingMode) {
        st = { ...st, stats: { ...st.stats, mode: currentMode } };
      }
    }
    lastStatus = st;
    const prevMode = currentMode;
    const modeChanged = hasInitialStatus && prevMode !== st.stats.mode;
    hasInitialStatus = true;
    currentMode = st.stats.mode;
    document.querySelectorAll('.mode-pill').forEach((node) => {
      const btn = /** @type {HTMLButtonElement} */ (node);
      const isActive = btn.dataset.mode === currentMode;
      btn.classList.toggle('active', isActive);
      btn.setAttribute('aria-pressed', String(isActive));
    });

    const copy = MODE_COPY[currentMode] || MODE_COPY.normal;
    setText('hl-line1', copy.lines[0]);
    setText('hl-line2', copy.lines[1]);
    setText('hl-line3', copy.lines[2]);
    setText('dynamic-subtitle', copy.sub);
    setText('roll-count', String(st.stats.queued_count));
    setText('roll-mode', copy.roll);
    const classifierShort = String(st.classifier || 'hybrid').split(/[\s(]/)[0].toUpperCase();
    setText('lcd-pushed', `PUSH ${st.stats.pushed_today}`);
    setText('lcd-classifier', classifierShort || 'HYBRID');
    setText('lcd-digested', `DIG ${st.stats.digested_today}`);
    setText('tape-rail-count', `${st.stats.queued_count} QUEUED`);
    setText('explore-label', `EXPLORE QUEUE (${st.stats.queued_count})`);

    const modeTags = {
      normal: { mascot: 'SCAN', fx: 'LIVE' },
      deep_work: { mascot: 'ZONE', fx: 'FOCUS' },
      dnd: { mascot: 'SLEEP', fx: 'MUTE' },
    };
    const tags = modeTags[currentMode] || modeTags.normal;
    const mascotEl = document.querySelector('.mascot-tag');
    if (mascotEl) mascotEl.textContent = tags.mascot;
    setText('lcd-fx-tag', tags.fx);

    const pagerBtn = document.getElementById('hero-dial-btn');
    if (pagerBtn) {
      pagerBtn.dataset.mode = currentMode;
    }

    if (modeChanged) {
      triggerPagerFlicker();
      playPagerChirp(currentMode);
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
    updateNextDigestBadge();
    renderSourcesDrawer();
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
    const bySource = sourceFilter === 'all'
      ? list
      : list.filter((it) => (it.source_type || '').toLowerCase() === sourceFilter);
    const filtered = q
      ? bySource.filter((it) =>
          (it.title || '').toLowerCase().includes(q) ||
          (it.body || '').toLowerCase().includes(q) ||
          (it.source_name || '').toLowerCase().includes(q) ||
          (it.source_type || '').toLowerCase().includes(q)
        )
      : bySource;
    filteredItems = filtered;

    const bulkReadBtn = /** @type {HTMLButtonElement | null} */ (document.getElementById('bulk-read-btn'));
    const bulkArchBtn = /** @type {HTMLButtonElement | null} */ (document.getElementById('bulk-archive-btn'));
    if (bulkReadBtn) bulkReadBtn.disabled = filtered.length === 0;
    if (bulkArchBtn) bulkArchBtn.disabled = filtered.length === 0;

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
        : `<button class="btn-card" data-action="queued" data-id="${it.id}">Re-queue</button>`;
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
    const seq = ++refreshSeq;
    const startModeSeq = modeSeq;
    const reqFilter = currentFilter;
    const reqSource = sourceFilter;
    const reqQuery = searchQuery.trim();
    try {
      const params = new URLSearchParams({
        status: reqFilter,
        limit: '50',
      });
      if (reqSource !== 'all') {
        params.set('source', reqSource);
      }
      if (reqQuery) {
        params.set('q', reqQuery);
      }
      const [st, data] = await Promise.all([
        /** @type {Promise<RuntimeStatus>} */ (api('/api/status')),
        /** @type {Promise<{items: StoredItem[]}>} */ (
          api(`/api/items?${params.toString()}`)
        ),
      ]);
      if (seq !== refreshSeq || reqFilter !== currentFilter || reqSource !== sourceFilter || reqQuery !== searchQuery.trim() || startModeSeq !== modeSeq) {
        return;
      }
      document.getElementById('auth-panel')?.classList.add('hidden');
      lastItems = data.items || [];
      const root = document.getElementById('items-list');
      root?.classList.remove('is-stale');
      renderStatus(st);
      renderItems(lastItems);
      updateScrollTape();
    } catch (err) {
      if (err instanceof Error && err.message === 'unauthorized') {
        return;
      }
      if (seq !== refreshSeq || reqFilter !== currentFilter || reqSource !== sourceFilter || reqQuery !== searchQuery.trim() || startModeSeq !== modeSeq) {
        return;
      }
      const root = document.getElementById('items-list');
      if (root && lastItems.length === 0) {
        root.innerHTML = `
          <div class="empty">
            <p>Failed to load inbox (${escapeHTML(err instanceof Error ? err.message : String(err))}).</p>
            <button class="pill-cta small-cta" id="inbox-retry-btn" type="button" style="margin-top: 10px;">
              <span>RETRY</span>
            </button>
          </div>`;
        document.getElementById('inbox-retry-btn')?.addEventListener('click', () => refresh());
      } else if (lastItems.length > 0) {
        root?.classList.add('is-stale');
        showActionError(`Failed to update inbox (${err instanceof Error ? err.message : String(err)}). Showing cached items.`, () => refresh());
      }
    }
  }

  let sseConnectSeq = 0;

  async function connectSSE() {
    const attemptSeq = ++sseConnectSeq;
    if (evtSource) {
      evtSource.close();
      evtSource = null;
    }
    let url = '/api/events';
    if (token()) {
      try {
        const res = /** @type {{ ticket?: string }} */ (
          await api('/api/auth/ticket', { method: 'POST' })
        );
        if (res.ticket) {
          url = `/api/events?ticket=${encodeURIComponent(res.ticket)}`;
        }
      } catch (_) {}
    }
    if (attemptSeq !== sseConnectSeq) {
      return;
    }
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
    if (isSwitchingMode) return;
    isSwitchingMode = true;
    pendingMode = mode;
    const seq = ++modeSeq;
    try {
      const st = /** @type {RuntimeStatus} */ (
        await api('/api/mode', { method: 'POST', body: JSON.stringify({ mode }) })
      );
      if (seq === modeSeq) {
        renderStatus(st, seq);
      }
    } catch (err) {
      showActionError(`Failed to switch mode: ${err instanceof Error ? err.message : String(err)}`, () => {
        switchMode(mode);
      });
    } finally {
      isSwitchingMode = false;
      pendingMode = null;
    }
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

  document.querySelectorAll('.pager-key[data-pager-key]').forEach((node) => {
    node.addEventListener('click', (e) => {
      e.stopPropagation();
      const target = /** @type {SVGElement} */ (node);
      const next = /** @type {FocusMode} */ (target.dataset.pagerKey || 'normal');
      triggerPagerFlicker();
      switchMode(next);
    });
  });

  document.getElementById('hero-dial-btn')?.addEventListener('click', () => {
    triggerPagerFlicker();
    const idx = MODE_ORDER.indexOf(currentMode);
    const next = MODE_ORDER[(idx + 1) % MODE_ORDER.length];
    switchMode(next);
  });

  document.getElementById('tape-spool-badge')?.addEventListener('click', () => {
    const maxScroll = Math.max(240, document.documentElement.scrollHeight - window.innerHeight);
    if (window.scrollY / maxScroll > 0.25) {
      window.scrollTo({ top: 0, behavior: 'smooth' });
    }
  });

  (() => {
    const railEl = document.getElementById('scroll-tape-rail');
    const trackEl = document.getElementById('tape-rail-track');
    const stripEl = document.getElementById('tape-unroll-strip');
    const topChipEl = document.getElementById('tape-top-chip');
    if (!railEl || !trackEl || !stripEl) return;

    topChipEl?.addEventListener('pointerdown', (e) => e.stopPropagation());
    topChipEl?.addEventListener('click', (e) => {
      e.stopPropagation();
      window.scrollTo({ top: 0, behavior: 'smooth' });
    });

    /** @type {{ pointerId: number, trackTop: number, usableRange: number, minHeight: number, grabOffset: number } | null} */
    let dragState = null;

    /** @param {number} progress */
    const applyScrollProgress = (progress) => {
      const clamped = Math.max(0, Math.min(1, progress));
      const maxScroll = Math.max(240, document.documentElement.scrollHeight - window.innerHeight);
      window.scrollTo(0, clamped * maxScroll);
      updateScrollTape();
    };

    const endDrag = () => {
      if (!dragState) return;
      if (stripEl.hasPointerCapture(dragState.pointerId)) {
        stripEl.releasePointerCapture(dragState.pointerId);
      }
      dragState = null;
      railEl.classList.remove('is-dragging');
      document.documentElement.classList.remove('tape-dragging');
    };

    stripEl.addEventListener('pointerdown', (e) => {
      if (e.pointerType === 'mouse' && e.button !== 0) return;
      if (topChipEl && e.target instanceof Node && topChipEl.contains(e.target)) return;
      e.preventDefault();

      const trackRect = trackEl.getBoundingClientRect();
      const stripRect = stripEl.getBoundingClientRect();
      const minHeight = window.innerWidth <= 780 ? 22 : 30;
      const usableRange = Math.max(80, trackRect.height - minHeight);

      dragState = {
        pointerId: e.pointerId,
        trackTop: trackRect.top,
        usableRange,
        minHeight,
        grabOffset: e.clientY - stripRect.bottom,
      };
      railEl.classList.add('is-dragging');
      document.documentElement.classList.add('tape-dragging');
      stripEl.setPointerCapture(e.pointerId);
    });

    stripEl.addEventListener('pointermove', (e) => {
      if (!dragState || e.pointerId !== dragState.pointerId) return;
      const targetBottom = e.clientY - dragState.grabOffset;
      const extend = targetBottom - dragState.trackTop - dragState.minHeight;
      applyScrollProgress(extend / dragState.usableRange);
    });

    stripEl.addEventListener('pointerup', (e) => {
      if (dragState && e.pointerId === dragState.pointerId) endDrag();
    });
    stripEl.addEventListener('pointercancel', (e) => {
      if (dragState && e.pointerId === dragState.pointerId) endDrag();
    });

    stripEl.addEventListener('keydown', (e) => {
      const maxScroll = Math.max(240, document.documentElement.scrollHeight - window.innerHeight);
      const cur = Math.max(0, Math.min(1, window.scrollY / maxScroll));
      if (e.key === 'ArrowDown' || e.key === 'PageDown') {
        e.preventDefault();
        applyScrollProgress(cur + 0.15);
      } else if (e.key === 'ArrowUp' || e.key === 'PageUp') {
        e.preventDefault();
        applyScrollProgress(cur - 0.15);
      } else if (e.key === 'Home') {
        e.preventDefault();
        applyScrollProgress(0);
      } else if (e.key === 'End') {
        e.preventDefault();
        applyScrollProgress(1);
      }
    });
  })();

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

  /**
   * @param {string} itemId
   * @param {string} status
   * @param {HTMLButtonElement | null} [btn]
   */
  async function mutateItemStatus(itemId, status, btn) {
    try {
      await api(`/api/items/${itemId}/status`, {
        method: 'POST',
        body: JSON.stringify({ status }),
      });
      refresh();
    } catch (err) {
      if (btn) btn.disabled = false;
      showActionError(`Failed to update item: ${err instanceof Error ? err.message : String(err)}`, () => {
        if (btn) btn.disabled = true;
        mutateItemStatus(itemId, status, btn);
      });
    }
  }

  document.getElementById('items-list')?.addEventListener('click', async (e) => {
    if (document.getElementById('items-list')?.classList.contains('is-stale')) return;
    const target = /** @type {HTMLElement | null} */ (e.target);
    const btn = /** @type {HTMLButtonElement | null} */ (target?.closest('button[data-action]') || null);
    if (!btn || btn.disabled) return;
    const itemId = btn.dataset.id || '';
    const nextStatus = btn.dataset.action || '';
    btn.disabled = true;
    await mutateItemStatus(itemId, nextStatus, btn);
  });

  document.getElementById('sync-btn')?.addEventListener('click', async (e) => {
    const btn = /** @type {HTMLButtonElement} */ (e.currentTarget);
    btn.disabled = true;
    try {
      await api('/api/sync', { method: 'POST' });
      await refresh();
    } catch (err) {
      showActionError(`Sync failed: ${err instanceof Error ? err.message : String(err)}`);
    } finally {
      btn.disabled = false;
    }
  });

  document.getElementById('flush-btn')?.addEventListener('click', async (e) => {
    const btn = /** @type {HTMLButtonElement} */ (e.currentTarget);
    btn.disabled = true;
    try {
      await api('/api/digest/flush', { method: 'POST' });
      await refresh();
    } catch (err) {
      showActionError(`Digest flush failed: ${err instanceof Error ? err.message : String(err)}`);
    } finally {
      btn.disabled = false;
    }
  });

  /**
   * Wires accessible modal focus entry, Tab focus trapping, and trigger restoration.
   * @param {HTMLElement | null} backdrop
   * @param {HTMLElement | null} trigger
   * @param {HTMLElement | null} closeBtn
   * @param {(() => void)} [onBeforeOpen]
   * @returns {() => void} closeDrawer
   */
  function wireModalFocus(backdrop, trigger, closeBtn, onBeforeOpen) {
    const getFocusables = () => {
      if (!backdrop) return /** @type {HTMLElement[]} */ ([]);
      return Array.from(backdrop.querySelectorAll('a[href], button:not([disabled]), input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])'))
        .filter((el) => /** @type {HTMLElement} */ (el).offsetParent !== null)
        .map((el) => /** @type {HTMLElement} */ (el));
    };

    const closeDrawer = () => {
      if (!backdrop || backdrop.classList.contains('hidden')) return;
      backdrop.classList.add('hidden');
      trigger?.focus();
    };

    trigger?.addEventListener('click', () => {
      if (!backdrop) return;
      if (onBeforeOpen) onBeforeOpen();
      backdrop.classList.remove('hidden');
      const focusables = getFocusables();
      (focusables[0] || backdrop).focus();
    });

    closeBtn?.addEventListener('click', closeDrawer);
    backdrop?.addEventListener('click', (e) => {
      if (e.target === e.currentTarget) closeDrawer();
    });

    backdrop?.addEventListener('keydown', (e) => {
      if (e.key !== 'Tab') return;
      const focusables = getFocusables();
      if (!focusables.length) return;
      const first = focusables[0];
      const last = focusables[focusables.length - 1];
      if (e.shiftKey && document.activeElement === first) {
        e.preventDefault();
        last.focus();
      } else if (!e.shiftKey && document.activeElement === last) {
        e.preventDefault();
        first.focus();
      }
    });

    return closeDrawer;
  }

  const simBackdrop = document.getElementById('sim-backdrop');
  const closeSimDrawer = wireModalFocus(
    simBackdrop,
    document.getElementById('sim-toggle'),
    document.getElementById('sim-close')
  );

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
  /** @type {number} */
  let searchDebounceTimer = 0;
  if (searchInput instanceof HTMLInputElement) {
    searchInput.addEventListener('input', () => {
      searchQuery = searchInput.value;
      renderItems(lastItems);
      window.clearTimeout(searchDebounceTimer);
      searchDebounceTimer = window.setTimeout(refresh, 250);
    });
  }
  document.getElementById('search-form')?.addEventListener('submit', (e) => {
    e.preventDefault();
    searchPopover?.classList.add('hidden');
    document.getElementById('inbox-section')?.scrollIntoView({ behavior: 'smooth' });
    refresh();
  });
  document.getElementById('search-clear')?.addEventListener('click', () => {
    searchQuery = '';
    if (searchInput instanceof HTMLInputElement) searchInput.value = '';
    renderItems(lastItems);
    refresh();
    searchPopover?.classList.add('hidden');
  });

  let ghDevicePollTimer = 0;

  async function checkAuthStatus() {
    try {
      const res = /** @type {{ setup_required?: boolean, authenticated?: boolean, has_github?: boolean, github_user?: any }} */ (
        await fetch('/api/auth/status').then((r) => r.json())
      );
      if (res.setup_required) {
        document.getElementById('setup-backdrop')?.classList.remove('hidden');
      } else if (!res.authenticated) {
        document.getElementById('auth-panel')?.classList.remove('hidden');
      } else {
        document.getElementById('setup-backdrop')?.classList.add('hidden');
        document.getElementById('auth-panel')?.classList.add('hidden');
      }
      return res;
    } catch (_) {
      return null;
    }
  }

  document.getElementById('setup-form')?.addEventListener('submit', async (e) => {
    e.preventDefault();
    const passInput = /** @type {HTMLInputElement | null} */ (document.getElementById('setup-pass'));
    const confirmInput = /** @type {HTMLInputElement | null} */ (document.getElementById('setup-pass-confirm'));
    const pass = passInput ? passInput.value : '';
    const confirm = confirmInput ? confirmInput.value : '';
    const errEl = document.getElementById('setup-error');
    if (errEl) errEl.classList.add('hidden');

    if (pass.length < 4) {
      if (errEl) {
        errEl.textContent = 'Passphrase must be at least 4 characters long.';
        errEl.classList.remove('hidden');
      }
      return;
    }
    if (pass !== confirm) {
      if (errEl) {
        errEl.textContent = 'Passphrases do not match. Please verify.';
        errEl.classList.remove('hidden');
      }
      return;
    }

    try {
      const res = await fetch('/api/auth/setup', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ passphrase: pass }),
      });
      const data = await res.json();
      if (!res.ok) {
        throw new Error(data.error || 'Failed to initialize vault');
      }
      if (data.token) {
        localStorage.setItem('fg_session', data.token);
      }
      document.getElementById('setup-backdrop')?.classList.add('hidden');
      document.getElementById('auth-panel')?.classList.add('hidden');
      connectSSE();
      refresh();
    } catch (err) {
      if (errEl) {
        errEl.textContent = err instanceof Error ? err.message : String(err);
        errEl.classList.remove('hidden');
      }
    }
  });

  document.getElementById('auth-form')?.addEventListener('submit', async (e) => {
    e.preventDefault();
    const input = /** @type {HTMLInputElement | null} */ (document.getElementById('auth-input'));
    const val = input ? input.value : '';
    const errEl = document.getElementById('auth-error');
    if (errEl) errEl.classList.add('hidden');
    if (!val) return;

    try {
      const res = await fetch('/api/auth/login', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ passphrase: val }),
      });
      const data = await res.json();
      if (!res.ok) {
        // Fall back to legacy token check if passphrase failed using trimmed token value
        const tokenVal = val.trim();
        try {
          await api('/api/status', { headers: { Authorization: 'Bearer ' + tokenVal } });
          localStorage.removeItem('fg_session');
          localStorage.setItem('fg_token', tokenVal);
          document.getElementById('auth-panel')?.classList.add('hidden');
          connectSSE();
          refresh();
          return;
        } catch (_) {
          throw new Error(data.error || 'Invalid credentials');
        }
      }
      if (data.token) {
        localStorage.setItem('fg_session', data.token);
        localStorage.setItem('fg_token', data.token);
      }
      document.getElementById('auth-panel')?.classList.add('hidden');
      if (input) input.value = '';
      connectSSE();
      refresh();
    } catch (err) {
      if (errEl) {
        errEl.textContent = err instanceof Error ? err.message : String(err);
        errEl.classList.remove('hidden');
      }
    }
  });

  async function renderProfileDrawer() {
    try {
      const prof = /** @type {{ auth_configured?: boolean, github?: { connected?: boolean, token_preview?: string, participating_only?: boolean, user?: any, client_id?: string } }} */ (
        await api('/api/profile')
      );
      const gh = prof.github || {};
      const user = gh.user || null;

      const avatarEl = /** @type {HTMLImageElement | null} */ (document.getElementById('profile-avatar'));
      const placeholderEl = document.getElementById('profile-avatar-placeholder');
      const nameEl = document.getElementById('profile-name');

      if (user && user.avatar_url) {
        if (avatarEl) {
          avatarEl.src = user.avatar_url;
          avatarEl.classList.remove('hidden');
        }
        placeholderEl?.classList.add('hidden');
        if (nameEl) nameEl.textContent = user.name || user.login || 'Vault Master';
      } else {
        avatarEl?.classList.add('hidden');
        placeholderEl?.classList.remove('hidden');
        if (nameEl) nameEl.textContent = 'Vault Master';
      }

      const badge = document.getElementById('gh-badge-status');
      const connectedPanel = document.getElementById('gh-connected-panel');
      const disconnectedPanel = document.getElementById('gh-disconnected-panel');

      if (gh.connected && user) {
        if (badge) {
          badge.textContent = 'CONNECTED';
          badge.className = 'badge review';
        }
        connectedPanel?.classList.remove('hidden');
        disconnectedPanel?.classList.add('hidden');

        const ghAvatar = /** @type {HTMLImageElement | null} */ (document.getElementById('gh-user-avatar'));
        if (ghAvatar && user.avatar_url) ghAvatar.src = user.avatar_url;
        const ghHandle = document.getElementById('gh-login-handle');
        if (ghHandle) ghHandle.textContent = `@${user.login || ''}`;
        const ghBio = document.getElementById('gh-bio');
        if (ghBio) ghBio.textContent = user.bio || `${user.public_repos || 0} public repositories`;

        const partToggle = /** @type {HTMLInputElement | null} */ (document.getElementById('gh-participating-toggle'));
        if (partToggle) partToggle.checked = gh.participating_only !== false;
      } else {
        if (badge) {
          badge.textContent = 'DISCONNECTED';
          badge.className = 'badge';
        }
        connectedPanel?.classList.add('hidden');
        disconnectedPanel?.classList.remove('hidden');
        document.getElementById('gh-device-active-box')?.classList.add('hidden');
      }

      const clientInput = /** @type {HTMLInputElement | null} */ (document.getElementById('gh-custom-client-id'));
      if (clientInput && gh.client_id) clientInput.value = gh.client_id;
    } catch (_) {}
  }

  let ghDeviceFlowSeq = 0;

  function stopGitHubDevicePoll() {
    ghDeviceFlowSeq++;
    if (ghDevicePollTimer) {
      window.clearTimeout(ghDevicePollTimer);
      ghDevicePollTimer = 0;
    }
  }

  async function startGitHubDeviceFlow() {
    stopGitHubDevicePoll();
    const currentSeq = ghDeviceFlowSeq;
    const btn = /** @type {HTMLButtonElement | null} */ (document.getElementById('gh-device-start-btn'));
    const box = document.getElementById('gh-device-active-box');
    const userCodeEl = document.getElementById('gh-user-code');
    const linkEl = /** @type {HTMLAnchorElement | null} */ (document.getElementById('gh-device-link'));
    const statusTextEl = document.getElementById('gh-device-status-text');

    if (btn) btn.disabled = true;
    try {
      const res = /** @type {{ user_code?: string, device_code?: string, verification_uri?: string, interval?: number, client_id?: string, error?: string }} */ (
        await api('/api/github/device/start', { method: 'POST' })
      );
      if (currentSeq !== ghDeviceFlowSeq) return;
      if (res.error) {
        throw new Error(res.error);
      }
      if (userCodeEl && res.user_code) userCodeEl.textContent = res.user_code;
      if (linkEl && res.verification_uri) linkEl.href = res.verification_uri;
      box?.classList.remove('hidden');
      if (statusTextEl) statusTextEl.textContent = 'Waiting for authorization on GitHub...';

      let pollIntervalMs = (res.interval || 5) * 1000;
      const deviceCode = res.device_code;
      const clientID = res.client_id;

      const poll = async () => {
        if (currentSeq !== ghDeviceFlowSeq) return;
        try {
          const pollRes = /** @type {{ status?: string, error_description?: string, interval?: number, user?: any }} */ (
            await api('/api/github/device/poll', {
              method: 'POST',
              body: JSON.stringify({ device_code: deviceCode, client_id: clientID }),
            })
          );
          if (currentSeq !== ghDeviceFlowSeq) return;
          if (pollRes.status === 'success') {
            stopGitHubDevicePoll();
            box?.classList.add('hidden');
            await renderProfileDrawer();
            refresh();
            return;
          }
          if (pollRes.status === 'slow_down') {
            if (pollRes.interval) {
              pollIntervalMs = pollRes.interval * 1000;
            } else {
              pollIntervalMs += 5000;
            }
            ghDevicePollTimer = window.setTimeout(poll, pollIntervalMs);
          } else if (pollRes.status === 'authorization_pending') {
            ghDevicePollTimer = window.setTimeout(poll, pollIntervalMs);
          } else {
            if (statusTextEl) statusTextEl.textContent = pollRes.error_description || pollRes.status || 'Authorization halted';
          }
        } catch (pollErr) {
          if (currentSeq !== ghDeviceFlowSeq) return;
          if (statusTextEl) statusTextEl.textContent = pollErr instanceof Error ? pollErr.message : String(pollErr);
        }
      };

      ghDevicePollTimer = window.setTimeout(poll, pollIntervalMs);
    } catch (err) {
      if (currentSeq !== ghDeviceFlowSeq) return;
      alert('Could not start GitHub device connect: ' + (err instanceof Error ? err.message : String(err)));
    } finally {
      if (btn) btn.disabled = false;
    }
  }

  document.getElementById('gh-device-start-btn')?.addEventListener('click', startGitHubDeviceFlow);

  document.getElementById('gh-pat-form')?.addEventListener('submit', async (e) => {
    e.preventDefault();
    const input = /** @type {HTMLInputElement | null} */ (document.getElementById('gh-pat-input'));
    const tokenVal = input ? input.value.trim() : '';
    const errEl = document.getElementById('gh-pat-error');
    if (errEl) errEl.classList.add('hidden');
    if (!tokenVal) return;

    try {
      await api('/api/github/token', {
        method: 'POST',
        body: JSON.stringify({ token: tokenVal }),
      });
      if (input) input.value = '';
      await renderProfileDrawer();
      refresh();
    } catch (err) {
      if (errEl) {
        errEl.textContent = err instanceof Error ? err.message : String(err);
        errEl.classList.remove('hidden');
      }
    }
  });

  document.getElementById('gh-participating-toggle')?.addEventListener('change', async (e) => {
    const chk = /** @type {HTMLInputElement} */ (e.currentTarget);
    try {
      await api('/api/github/settings', {
        method: 'POST',
        body: JSON.stringify({ participating_only: chk.checked }),
      });
    } catch (err) {
      showActionError('Failed to save preference: ' + (err instanceof Error ? err.message : String(err)));
    }
  });

  document.getElementById('gh-sync-btn')?.addEventListener('click', async () => {
    try {
      await api('/api/sync', { method: 'POST' });
      refresh();
    } catch (err) {
      showActionError('Sync failed: ' + (err instanceof Error ? err.message : String(err)));
    }
  });

  document.getElementById('gh-disconnect-btn')?.addEventListener('click', async () => {
    if (!confirm('Disconnect GitHub account from FocusGate?')) return;
    try {
      await api('/api/github/disconnect', { method: 'POST' });
      await renderProfileDrawer();
      refresh();
    } catch (err) {
      showActionError('Disconnect failed: ' + (err instanceof Error ? err.message : String(err)));
    }
  });

  document.getElementById('gh-save-client-id-btn')?.addEventListener('click', async () => {
    const input = /** @type {HTMLInputElement | null} */ (document.getElementById('gh-custom-client-id'));
    const val = input ? input.value.trim() : '';
    try {
      await api('/api/github/settings', {
        method: 'POST',
        body: JSON.stringify({ client_id: val }),
      });
      alert('GitHub Client ID saved.');
    } catch (err) {
      showActionError('Failed to save Client ID: ' + (err instanceof Error ? err.message : String(err)));
    }
  });

  document.getElementById('change-pass-form')?.addEventListener('submit', async (e) => {
    e.preventDefault();
    const form = /** @type {HTMLFormElement} */ (e.currentTarget);
    const curr = /** @type {HTMLInputElement | null} */ (document.getElementById('change-pass-curr'))?.value || '';
    const next = /** @type {HTMLInputElement | null} */ (document.getElementById('change-pass-next'))?.value || '';
    const errEl = document.getElementById('change-pass-error');
    const succEl = document.getElementById('change-pass-success');
    errEl?.classList.add('hidden');
    succEl?.classList.add('hidden');

    try {
      const res = /** @type {{ status?: string, token?: string }} */ (
        await api('/api/auth/change-passphrase', {
          method: 'POST',
          body: JSON.stringify({ current_passphrase: curr, new_passphrase: next }),
        })
      );
      if (res.token) {
        localStorage.setItem('fg_session', res.token);
      }
      succEl?.classList.remove('hidden');
      form.reset();
    } catch (err) {
      if (errEl) {
        errEl.textContent = err instanceof Error ? err.message : String(err);
        errEl.classList.remove('hidden');
      }
    }
  });

  const profileBackdrop = document.getElementById('profile-backdrop');
  const closeProfileDrawer = wireModalFocus(
    profileBackdrop,
    document.getElementById('profile-toggle'),
    document.getElementById('profile-close'),
    renderProfileDrawer
  );

  document.getElementById('logout-btn')?.addEventListener('click', async () => {
    localStorage.removeItem('fg_session');
    localStorage.removeItem('fg_token');
    refreshSeq++;
    lastItems = [];
    filteredItems = [];
    lastStatus = null;
    const root = document.getElementById('items-list');
    if (root) root.innerHTML = '';
    closeProfileDrawer();
    try {
      await api('/api/auth/logout', { method: 'POST' });
    } catch (_) {}
    document.getElementById('auth-panel')?.classList.remove('hidden');
    document.getElementById('inbox-section')?.scrollIntoView({ behavior: 'smooth' });
    refresh();
  });

  const sourcesBackdrop = document.getElementById('sources-backdrop');
  const closeSourcesDrawer = wireModalFocus(
    sourcesBackdrop,
    document.getElementById('sources-toggle'),
    document.getElementById('sources-close'),
    renderSourcesDrawer
  );

  document.getElementById('test-notify-btn')?.addEventListener('click', async (e) => {
    const btn = /** @type {HTMLButtonElement} */ (e.currentTarget);
    const out = document.getElementById('notify-test-result');
    btn.disabled = true;
    try {
      const res = /** @type {{ sent?: string[], error?: string }} */ (
        await api('/api/notify/test', { method: 'POST' })
      );
      if (out) {
        const sent = (res.sent || []).join(', ');
        out.textContent = sent
          ? `Sent test alert via: ${sent}`
          : 'No delivery channels enabled in config.';
        out.classList.remove('hidden');
      }
    } catch (err) {
      if (out) {
        out.textContent = 'Test alert failed: ' + (err instanceof Error ? err.message : String(err));
        out.classList.remove('hidden');
      }
    } finally {
      btn.disabled = false;
    }
  });

  const sfxBtn = document.getElementById('sfx-toggle');
  const syncSfxButton = () => {
    if (!sfxBtn) return;
    sfxBtn.setAttribute('aria-pressed', sfxEnabled ? 'true' : 'false');
    sfxBtn.textContent = sfxEnabled ? 'SFX ON' : 'SFX OFF';
  };
  syncSfxButton();
  sfxBtn?.addEventListener('click', () => {
    sfxEnabled = !sfxEnabled;
    localStorage.setItem('fg_sfx', sfxEnabled ? 'on' : 'off');
    syncSfxButton();
    if (sfxEnabled) playPagerChirp(currentMode);
  });

  document.getElementById('source-pills')?.addEventListener('click', (e) => {
    const target = /** @type {HTMLElement | null} */ (e.target);
    const pill = /** @type {HTMLButtonElement | null} */ (target?.closest('button[data-source]') || null);
    if (!pill) return;
    sourceFilter = pill.dataset.source || 'all';
    document.querySelectorAll('#source-pills .source-pill').forEach((n) => {
      const b = /** @type {HTMLButtonElement} */ (n);
      b.classList.toggle('active', b.dataset.source === sourceFilter);
    });
    renderItems(lastItems);
    refresh();
  });

  /**
   * @param {'read' | 'archived'} targetStatus
   * @param {number[]} [retryIds]
   * @param {string} [retryFromStatus]
   */
  async function runBulkTriage(targetStatus, retryIds, retryFromStatus) {
    let ids = retryIds;
    let fromStatus = retryFromStatus;

    if (!ids) {
      if (!filteredItems.length) return;
      const root = document.getElementById('items-list');
      if (root?.classList.contains('is-stale')) {
        showActionError('Cannot run bulk actions on stale items. Please refresh first.', () => refresh());
        return;
      }
      const currentTabItems = currentFilter === 'all'
        ? filteredItems
        : filteredItems.filter((it) => it.status === currentFilter);
      if (!currentTabItems.length) return;
      ids = currentTabItems.map((it) => it.id);
      fromStatus = currentFilter === 'all' ? '' : currentFilter;
    }

    const bulkReadBtn = /** @type {HTMLButtonElement | null} */ (document.getElementById('bulk-read-btn'));
    const bulkArchBtn = /** @type {HTMLButtonElement | null} */ (document.getElementById('bulk-archive-btn'));
    if (bulkReadBtn) bulkReadBtn.disabled = true;
    if (bulkArchBtn) bulkArchBtn.disabled = true;
    try {
      await api('/api/items/bulk', {
        method: 'POST',
        body: JSON.stringify({ ids, from_status: fromStatus, status: targetStatus }),
      });
      refresh();
    } catch (err) {
      if (bulkReadBtn) bulkReadBtn.disabled = false;
      if (bulkArchBtn) bulkArchBtn.disabled = false;
      showActionError(`Bulk triage failed: ${err instanceof Error ? err.message : String(err)}`, () => {
        runBulkTriage(targetStatus, ids, fromStatus);
      });
    }
  }
  document.getElementById('bulk-read-btn')?.addEventListener('click', () => runBulkTriage('read'));
  document.getElementById('bulk-archive-btn')?.addEventListener('click', () => runBulkTriage('archived'));

  window.addEventListener('keydown', (e) => {
    if (e.key === 'Escape') {
      closeSimDrawer();
      closeSourcesDrawer();
      searchPopover?.classList.add('hidden');
      return;
    }
    const isModalOpen = (simBackdrop && !simBackdrop.classList.contains('hidden')) ||
                        (sourcesBackdrop && !sourcesBackdrop.classList.contains('hidden'));
    if (isModalOpen) return;

    const active = document.activeElement;
    const isTyping = active && (
      active.tagName === 'INPUT' ||
      active.tagName === 'TEXTAREA' ||
      active.tagName === 'SELECT' ||
      /** @type {HTMLElement} */ (active).isContentEditable
    );
    if (isTyping || e.ctrlKey || e.metaKey || e.altKey) return;
    if (e.key === '/') {
      e.preventDefault();
      searchPopover?.classList.remove('hidden');
      if (searchInput instanceof HTMLInputElement) searchInput.focus();
    } else if (e.key === 'r' || e.key === 'R') {
      document.getElementById('sync-btn')?.click();
    } else if (e.key === '1') {
      triggerPagerFlicker();
      switchMode('normal');
    } else if (e.key === '2') {
      triggerPagerFlicker();
      switchMode('deep_work');
    } else if (e.key === '3') {
      triggerPagerFlicker();
      switchMode('dnd');
    }
  });

  if ('serviceWorker' in navigator) {
    navigator.serviceWorker.register('/sw.js').catch(() => {});
  }

  window.setInterval(updateNextDigestBadge, 30000);
  updateScrollTape();
  checkAuthStatus();
  connectSSE();
  refresh();

  if ('requestIdleCallback' in window) {
    /** @type {any} */ (window).requestIdleCallback(() => renderCrumpledPaper(), { timeout: 1200 });
  } else {
    setTimeout(renderCrumpledPaper, 100);
  }
})();
