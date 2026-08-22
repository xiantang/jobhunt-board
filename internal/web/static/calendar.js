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

  // 时间小工具。这几个必须定义在下面的 `let from = thisWeekISO()` 之前——
  // const 声明有暂时性死区，函数声明虽然会提升，但它引用的 pad 不会。
  const pad = (n) => String(n).padStart(2, '0');

  const ymd = (d) => `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`;

  // 日程页固定按自然周显示，周日打头。翻页翻的是整周，
  // 这样「周三在第几列」永远是同一个位置，不随今天是周几而漂移。
  const weekStart = (d) => {
    const start = new Date(d);
    start.setDate(start.getDate() - start.getDay());
    return ymd(start);
  };

  const thisWeekISO = () => weekStart(new Date());

  const hhmm = (iso) => {
    const d = new Date(iso);
    return `${pad(d.getHours())}:${pad(d.getMinutes())}`;
  };

  // 视图状态：起始日 + 天数。默认一周，从今天开始。
  let from = thisWeekISO();
  let days = 7;

  // ---------- 渲染 ----------

  // 一小时在网格上占多高。整页的定位算术都从这一个常数推出来。
  const HOUR = 44;
  const MIN_BLOCK = 22; // 再短的会议也得留出能点中、能看清标题的高度

  const minutesOf = (iso) => {
    const d = new Date(iso);
    return d.getHours() * 60 + d.getMinutes();
  };

  // 把一天里重叠的条目排成并排的几列。
  // 先按开始时间排，再贪心地把每一条塞进第一个「已经空出来」的列；
  // 一簇互相重叠的条目共享同一个列数，宽度按列数均分——
  // 这正是 Google 日历里两场撞在一起时各占一半的效果。
  function layout(entries) {
    const evs = entries.map((e) => {
      const start = minutesOf(e.start);
      const end = Math.max(minutesOf(e.end), start + 20);
      return { e, start, end, col: 0, cols: 1 };
    });
    evs.sort((a, b) => a.start - b.start || a.end - b.end);

    let cluster = [];
    let clusterEnd = -1;
    const flush = () => {
      const cols = cluster.reduce((n, it) => Math.max(n, it.col + 1), 0);
      cluster.forEach((it) => { it.cols = cols; });
      cluster = [];
    };

    const colEnds = [];
    for (const it of evs) {
      if (it.start >= clusterEnd) { // 和前一簇完全断开了，结算上一簇
        flush();
        colEnds.length = 0;
      }
      let col = colEnds.findIndex((end) => end <= it.start);
      if (col === -1) col = colEnds.length;
      colEnds[col] = it.end;
      it.col = col;
      cluster.push(it);
      clusterEnd = Math.max(clusterEnd, it.end);
    }
    flush();
    return evs;
  }

  // 网格里的一块。位置由 top/height 定，宽度由并排列数定。
  function blockHTML(it) {
    const e = it.e;
    const google = e.source === 'google';
    const task = e.source === 'task';
    const top = (it.start / 60) * HOUR;
    const height = Math.max(((it.end - it.start) / 60) * HOUR, MIN_BLOCK);
    const width = 100 / it.cols;
    // 任务只有一个截止时刻：显示「18:00 截止」，而不是一段假的时间区间。
    const time = task ? `${hhmm(e.due || e.end)} 截止` : `${hhmm(e.start)}–${hhmm(e.end)}`;

    // 悬停能看到全部信息——块里塞不下的都放进 title。
    const tip = [time, e.title];
    if (e.application_key) tip.push(e.application_key);
    if (e.mode) tip.push(MODES[e.mode] || e.mode);
    if (e.result && e.result !== 'pending') tip.push(RESULTS[e.result]);
    if (e.location) tip.push(e.location);
    if (!google) tip.push(e.synced ? '✓ 已同步到 Google 日历' : '未同步');
    // DDL 过了还挂着「待进行」，是这一页上最该被看见的东西。
    const overdue = task && e.result === 'pending' && new Date(e.due || e.end) < new Date();
    if (overdue) tip.push('已过截止时间，还没回填结果');

    const style = `top:${top.toFixed(1)}px; height:${height.toFixed(1)}px;`
      + ` left:${(it.col * width).toFixed(3)}%; width:${width.toFixed(3)}%;`
      + (google ? '' : ` --entry-color:${esc(e.stage_color || '#0052cc')};`);

    const sync = !google && !e.synced && e.round_id
      ? `<button type="button" class="ev__sync" data-sync="${e.round_id}" title="同步到 Google 日历">↑</button>` : '';

    const short = height < 34 ? ' ev--short' : '';
    const flavor = google ? 'google' : (task ? 'task' : 'interview');

    return `<article class="ev ev--${flavor}${short}${overdue ? ' ev--overdue' : ''}"
             style="${style}" title="${esc(tip.join(' · '))}"
             ${google ? (e.url ? `data-url="${esc(e.url)}"` : '') : `data-app="${e.application_id}"`}>
      <span class="ev__title">${task ? '⏰ ' : ''}${esc(e.title)}${task && short ? ' · ' + esc(time) : ''}</span>
      <span class="ev__time">${esc(time)}</span>
      ${sync}
    </article>`;
  }

  // 顶部横条：全天事件，外加当天所有任务的 DDL。
  //
  // DDL 常常压在 23:59，缩在网格最底下、要滚到底才看得见——而它恰恰是
  // 「今天必须处理」的那一条。所以它在时间轴上照画，同时在顶部再挂一次。
  function stripHTML(entries) {
    return entries.map((e) => {
      const google = e.source === 'google';
      const task = e.source === 'task';
      const flavor = google ? 'google' : (task ? 'task' : 'interview');
      const style = google ? '' : ` style="--entry-color:${esc(e.stage_color || '#0891b2')}"`;
      const when = task && !e.all_day ? ` ${hhmm(e.due || e.end)} 截止` : '';
      const overdue = task && e.result === 'pending' && new Date(e.due || e.end) < new Date();
      return `<article class="ev ev--strip ev--${flavor}${overdue ? ' ev--overdue' : ''}"${style}
               title="${esc(e.title + when)}"
               ${google ? (e.url ? `data-url="${esc(e.url)}"` : '') : `data-app="${e.application_id}"`}>
        <span class="ev__title">${task ? '⏰ ' : ''}${esc(e.title)}</span>
        ${when ? `<span class="ev__due">${esc(when.trim())}</span>` : ''}
      </article>`;
    }).join('');
  }

  // 顶部横条上要显示的：全天事件 + 任务的 DDL。
  const stripEntries = (day) => day.entries.filter((e) => e.all_day || e.source === 'task');

  // 当前时刻那条红线。只画在「今天」那一列上。
  function nowLineHTML(day) {
    if (!day.today) return '';
    const d = new Date();
    const top = ((d.getHours() * 60 + d.getMinutes()) / 60) * HOUR;
    return `<div class="week__now" style="top:${top.toFixed(1)}px"></div>`;
  }

  function render(agenda) {
    const days = agenda.days || [];
    const hours = Array.from({ length: 24 }, (_, h) =>
      `<div class="week__hour" style="height:${HOUR}px"><span>${pad(h)}:00</span></div>`).join('');

    const head = days.map((day) => {
      const [, m, d] = day.date.split('-');
      return `<div class="week__dayhead${day.today ? ' is-today' : ''}">
        <span class="week__weekday">${esc(day.weekday)}</span>
        <b class="week__date">${Number(d)}</b>
        <span class="week__month">${Number(m)}月</span>
      </div>`;
    }).join('');

    const anyStrip = days.some((day) => stripEntries(day).length > 0);
    const strip = anyStrip
      ? `<div class="week__allday">
          <div class="week__gutter-label">全天 / 截止</div>
          ${days.map((day) =>
            `<div class="week__allday-col">${stripHTML(stripEntries(day))}</div>`).join('')}
        </div>`
      : '';

    const cols = days.map((day) => {
      const timed = layout(day.entries.filter((e) => !e.all_day));
      return `<div class="week__col${day.today ? ' is-today' : ''}" style="height:${24 * HOUR}px">
        ${nowLineHTML(day)}${timed.map(blockHTML).join('')}
      </div>`;
    }).join('');

    agendaEl.innerHTML = `<div class="week">
      <div class="week__head">
        <div class="week__gutter-label">GMT+8</div>
        ${head}
      </div>
      ${strip}
      <div class="week__body" id="week-body">
        <div class="week__hours">${hours}</div>
        ${cols}
      </div>
    </div>`;

    scrollToFirst(days);

    const last = days[days.length - 1];
    rangeEl.textContent = days.length ? `${days[0].date} ~ ${last.date}` : '';

    renderStatus(agenda.status, agenda.warning);
    renderPending(agenda.unscheduled || []);
  }

  // 一进来就停在最早那场的前一小时。整天都是空的就停在 8 点——
  // 从 0 点开始看，屏幕上一半是夜里，什么也没有。
  function scrollToFirst(days) {
    const body = document.getElementById('week-body');
    if (!body) return;
    let earliest = 8 * 60;
    for (const day of days) {
      for (const e of day.entries) {
        if (!e.all_day) earliest = Math.min(earliest, minutesOf(e.start));
      }
    }
    body.scrollTop = Math.max(0, ((earliest - 60) / 60) * HOUR);
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
        <time class="entry__time">${e.source === 'task' ? '待定 DDL' : '待安排'}</time>
        <div class="entry__body">
          <h4 class="entry__title" title="${esc(e.title)}">${esc(e.title)}</h4>
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
    from = weekStart(d); // 保险起见再对齐一次：起点永远落在周日
    reload();
  }

  document.querySelectorAll('[data-shift]').forEach((btn) => {
    btn.addEventListener('click', () => shift(Number(btn.dataset.shift)));
  });
  document.querySelector('[data-today]').addEventListener('click', () => {
    from = thisWeekISO();
    reload();
  });

  // 点面试块回看板对应的那张卡，点 Google 的块跳去 Google 日历。
  document.body.addEventListener('click', async (e) => {
    const sync = e.target.closest('[data-sync]');
    if (sync) {
      e.stopPropagation();
      sync.disabled = true;
      sync.textContent = '…';
      try {
        await API.post(`/api/rounds/${sync.dataset.sync}/sync`, {});
        API.toast('已同步到 Google 日历', true);
        await reload();
      } catch (err) {
        API.toast(err.message);
        sync.disabled = false;
        sync.textContent = '↑';
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

    const block = e.target.closest('[data-app], [data-url]');
    if (!block) return;
    if (block.dataset.url) { // Google 的会议直接跳到它在日历上的那条
      window.open(block.dataset.url, '_blank', 'noopener');
      return;
    }
    location.href = `/boards/${boardKey}?open=${block.dataset.app}`;
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
