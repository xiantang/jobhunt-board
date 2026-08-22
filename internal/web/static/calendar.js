// 日程页：一周为一屏，看板的面试和 Google 日历的会议铺在同一条时间线上。
// 首屏由服务端渲染的 JSON 起步，翻周之后每次都重新拉 /api/agenda。
(() => {
  const boardKey = document.documentElement.dataset.board;
  const agendaEl = document.getElementById('agenda');
  const rangeEl = document.getElementById('agenda-range');
  const statusEl = document.getElementById('google-status');
  const pendingEl = document.getElementById('agenda-pending');

  const readJSON = (id, fallback) => {
    try {
      return JSON.parse(document.getElementById(id).textContent) ?? fallback;
    } catch (e) {
      return fallback;
    }
  };

  const esc = (s) => String(s ?? '').replace(/[&<>"']/g, (c) =>
    ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));

  const MODES = { online: '线上', onsite: '现场', phone: '电话' };
  const RESULTS = { pending: '待进行', passed: '已通过', failed: '未通过', cancelled: '已取消' };

  // 视图状态：起始日 + 天数。默认一周，从今天开始。
  let from = todayISO();
  let days = 7;

  function todayISO() {
    const d = new Date();
    return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`;
  }

  const pad = (n) => String(n).padStart(2, '0');

  const hhmm = (iso) => {
    const d = new Date(iso);
    return `${pad(d.getHours())}:${pad(d.getMinutes())}`;
  };

  // ---------- 渲染 ----------

  function entryHTML(e) {
    const google = e.source === 'google';
    const time = e.all_day ? '全天' : `${hhmm(e.start)}–${hhmm(e.end)}`;

    // 面试用阶段配色，Google 的会议统一灰调——一眼分得清哪些是自己排的。
    const accent = google ? '' : ` style="--entry-color: ${esc(e.stage_color || '#0052cc')}"`;
    const meta = [];
    if (google) {
      meta.push('<span class="entry__src">Google 日历</span>');
    } else {
      meta.push(`<span class="entry__key">${esc(e.application_key)}</span>`);
      if (e.mode) meta.push(`<span>${esc(MODES[e.mode] || e.mode)}</span>`);
      if (e.result && e.result !== 'pending') meta.push(`<span>${esc(RESULTS[e.result])}</span>`);
      meta.push(e.synced
        ? '<span class="entry__synced" title="已同步到 Google 日历">✓ 已同步</span>'
        : `<button type="button" class="btn btn--tiny" data-sync="${e.round_id || ''}">同步到日历</button>`);
    }
    if (e.location) meta.push(`<span class="entry__where">${esc(e.location)}</span>`);

    const link = e.url
      ? `<a class="entry__link" href="${esc(e.url)}" target="_blank" rel="noopener">打开</a>` : '';

    return `<article class="entry entry--${google ? 'google' : 'interview'}${e.conflict ? ' entry--conflict' : ''}"${accent}
             ${google ? '' : `data-app="${e.application_id}"`}>
      <time class="entry__time">${esc(time)}</time>
      <div class="entry__body">
        <h4 class="entry__title">${esc(e.title)}</h4>
        <div class="entry__meta">${meta.join('')}${link}</div>
      </div>
      ${e.conflict ? '<span class="entry__warn" title="和同一天的另一场时间重叠">撞期</span>' : ''}
    </article>`;
  }

  function dayHTML(day) {
    const body = day.entries.length
      ? day.entries.map(entryHTML).join('')
      : '<p class="agenda-empty">空</p>';
    const [, m, d] = day.date.split('-');
    return `<section class="agenda-day${day.today ? ' agenda-day--today' : ''}">
      <header class="agenda-day__head">
        <b>${m}-${d}</b><span>${esc(day.weekday)}</span>
        ${day.today ? '<em>今天</em>' : ''}
      </header>
      <div class="agenda-day__body">${body}</div>
    </section>`;
  }

  function render(agenda) {
    agendaEl.innerHTML = agenda.days.map(dayHTML).join('');

    const last = agenda.days[agenda.days.length - 1];
    rangeEl.textContent = agenda.days.length
      ? `${agenda.days[0].date} ~ ${last.date}` : '';

    renderStatus(agenda.status, agenda.warning);
    renderPending(agenda.unscheduled || []);
  }

  function renderStatus(status, warning) {
    if (!status.enabled) {
      statusEl.innerHTML = '<span class="gstatus gstatus--off" title="服务端未配置 GOOGLE_CLIENT_ID / GOOGLE_CLIENT_SECRET">Google 日历未启用</span>';
      return;
    }
    if (!status.connected) {
      statusEl.innerHTML = `<a class="btn btn--primary" href="/oauth/google/connect?board=${encodeURIComponent(boardKey)}">连接 Google 日历</a>`;
      return;
    }
    const who = status.account && status.account.email ? status.account.email : '已连接';
    statusEl.innerHTML = `<span class="gstatus gstatus--on" title="${esc(who)}">✓ ${esc(who)}</span>
      <button type="button" class="btn btn--ghost" id="google-disconnect">断开</button>`;

    if (warning) API.toast(warning);
  }

  // 还没定时间的面试单独列出来——它们不在时间线上，但正是最该处理的。
  function renderPending(entries) {
    pendingEl.hidden = entries.length === 0;
    if (!entries.length) return;
    document.getElementById('agenda-pending-list').innerHTML = entries.map((e) =>
      `<article class="entry entry--interview entry--pending" data-app="${e.application_id}"
                style="--entry-color: ${esc(e.stage_color || '#0052cc')}">
        <time class="entry__time">待安排</time>
        <div class="entry__body">
          <h4 class="entry__title">${esc(e.title)}</h4>
          <div class="entry__meta"><span class="entry__key">${esc(e.application_key)}</span></div>
        </div>
      </article>`).join('');
  }

  // ---------- 交互 ----------

  async function reload() {
    try {
      render(await API.get(`/api/agenda?from=${from}&days=${days}`));
    } catch (err) {
      API.toast(err.message);
    }
  }

  function shift(offset) {
    const d = new Date(from + 'T00:00:00');
    d.setDate(d.getDate() + offset);
    from = `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`;
    reload();
  }

  document.querySelectorAll('[data-shift]').forEach((btn) => {
    btn.addEventListener('click', () => shift(Number(btn.dataset.shift)));
  });
  document.querySelector('[data-today]').addEventListener('click', () => {
    from = todayISO();
    reload();
  });

  // 点面试条目回看板，Google 的条目走它自己的链接，不拦。
  document.body.addEventListener('click', async (e) => {
    const sync = e.target.closest('[data-sync]');
    if (sync) {
      e.stopPropagation();
      sync.disabled = true;
      sync.textContent = '同步中…';
      try {
        await API.post(`/api/rounds/${sync.dataset.sync}/sync`, {});
        API.toast('已同步到 Google 日历', true);
        await reload();
      } catch (err) {
        API.toast(err.message);
        sync.disabled = false;
        sync.textContent = '同步到日历';
      }
      return;
    }

    const disconnect = e.target.closest('#google-disconnect');
    if (disconnect) {
      if (!confirm('断开 Google 日历？已经同步过去的日程不会被删掉，但之后不再自动更新。')) return;
      try {
        await API.post('/api/google/disconnect', {});
        API.toast('已断开', true);
        await reload();
      } catch (err) {
        API.toast(err.message);
      }
      return;
    }

    if (e.target.closest('.entry__link')) return;
    const entry = e.target.closest('.entry--interview');
    if (entry && entry.dataset.app) {
      location.href = `/boards/${boardKey}?open=${entry.dataset.app}`;
    }
  });

  // OAuth 回调跳回来时给一句结果提示。
  const notice = document.querySelector('[data-notice]');
  if (notice) {
    const messages = {
      connected: ['已连接 Google 日历', true],
      denied: ['你取消了授权', false],
      state_mismatch: ['授权校验失败，请重新点一次「连接 Google 日历」', false],
      failed: ['连接失败，请重试', false],
    };
    const [text, ok] = messages[notice.dataset.notice] || ['', false];
    if (text) API.toast(text, ok);
    history.replaceState(null, '', location.pathname);
  }

  render(readJSON('agenda-data', { days: [], status: {}, unscheduled: [] }));
})();
