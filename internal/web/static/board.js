// 看板交互：阶段流转（拖拽 + 下拉）、新建投递、详情抽屉、面试排期、阶段配置。
// 首屏由服务端渲染；此后每次变更都用后端返回的最新看板重绘，保证界面与数据库一致。
(() => {
  const boardKey = document.documentElement.dataset.board;
  const boardEl = document.getElementById('board');
  const drawer = document.getElementById('drawer');
  const stageDrawer = document.getElementById('stage-drawer');
  const query = location.search; // 带上当前筛选条件，返回的看板才和界面一致

  const readJSON = (id, fallback) => {
    try {
      return JSON.parse(document.getElementById(id).textContent) ?? fallback;
    } catch (e) {
      return fallback;
    }
  };

  const KINDS = readJSON('kind-data', []);
  const MEMBERS = readJSON('member-data', []);
  // STAGES 是当前看板的阶段配置，阶段可随时增删改，所以它是可变的。
  let STAGES = (readJSON('board-data', { columns: [] }).columns || []).map(toStage);

  const INTENT = { low: '低', normal: '中', high: '高' };
  const MODES = [['online', '线上'], ['onsite', '现场'], ['phone', '电话']];
  const RESULTS = [['pending', '待进行'], ['passed', '已通过'], ['failed', '未通过'], ['cancelled', '已取消']];

  function toStage(col) {
    return { key: col.key, label: col.label, kind: col.kind, color: col.color, id: col.id,
             requires_owner: col.requires_owner, terminal: col.terminal };
  }

  const esc = (s) => String(s ?? '').replace(/[&<>"']/g, (c) =>
    ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));

  const stageIndex = (key) => STAGES.findIndex((s) => s.key === key);

  // canMove 是后端 workflow.Flow.Can 的前端镜像，只用于把非法选项置灰；
  // 真正的规则以后端为准，拖到非法列时会收到 409 并回滚。
  function canMove(fromKey, toKey) {
    if (fromKey === toKey) return true;
    const from = STAGES[stageIndex(fromKey)];
    const to = STAGES[stageIndex(toKey)];
    if (!from || !to) return false;
    if (to.terminal || from.terminal) return true;
    return Math.abs(stageIndex(toKey) - stageIndex(fromKey)) === 1;
  }

  // ---------- 渲染 ----------

  function moveOptions(currentKey) {
    return ['<option value="">移动到…</option>']
      .concat(STAGES
        .filter((s) => s.key !== currentKey)
        .map((s) => `<option value="${esc(s.key)}"${canMove(currentKey, s.key) ? '' : ' disabled'}>→ ${esc(s.label)}</option>`))
      .join('');
  }

  // hydrateMoves 给每张卡片的下拉填充可选阶段。首屏的卡片由模板渲染，
  // 下拉留空由这里补齐，避免同一份规则在 Go 模板和 JS 里写两遍。
  function hydrateMoves(root = document) {
    root.querySelectorAll('.card').forEach((card) => {
      const select = card.querySelector('[data-move]');
      if (select) select.innerHTML = moveOptions(card.dataset.stage);
    });
  }

  function cardHTML(app) {
    const avatar = app.owner
      ? `<span class="avatar" style="background: ${esc(app.owner.color)}" title="${esc(app.owner.name)}">${esc([...app.owner.name][0] || '?')}</span>`
      : '<span class="avatar avatar--empty" title="未指定跟进人">?</span>';

    const sub = (app.role || app.channel)
      ? `<p class="card__sub">${esc(app.role)}${app.role && app.channel ? ' · ' : ''}${esc(app.channel)}</p>`
      : '';

    const round = app.next_round
      ? `<p class="card__round${overdue(app.next_round) ? ' card__round--overdue' : ''}">
           <span>📅 ${esc(whenText(app.next_round.scheduled_at))}</span>
           <span class="card__meeting">${esc(meetingText(app.next_round))}</span>
         </p>`
      : '';

    const rounds = app.round_count > 0 ? `<span class="tag tag--rounds">${app.round_count} 轮</span>` : '';
    const warn = needsSchedule(app) ? ' card--warn' : '';

    return `<article class="card intent-${esc(app.intent)}${warn}" draggable="true" data-app="${app.id}" data-stage="${esc(app.stage_key)}">
      <h3 class="card__title">${esc(app.company)}</h3>
      ${sub}
      ${round}
      <div class="card__meta">
        <span class="card__key">${esc(app.key)}</span>
        <span class="tag tag--${esc(app.intent)}">意向${INTENT[app.intent] || '中'}</span>
        ${rounds}
        ${avatar}
      </div>
      <div class="card__actions">
        <select class="card__move" data-move aria-label="移动到其他阶段">${moveOptions(app.stage_key)}</select>
      </div>
    </article>`;
  }

  function createFormHTML() {
    const owners = MEMBERS.map((m) => `<option value="${m.id}">${esc(m.name)}</option>`).join('');
    return `<form class="create" id="create-form">
      <input type="text" name="company" placeholder="+ 新增投递，输入公司名" maxlength="60" autocomplete="off">
      <input type="text" name="role" placeholder="岗位（选填）" maxlength="60" autocomplete="off">
      <div class="create__row">
        <select name="owner_id"><option value="0">未指定</option>${owners}</select>
        <select name="intent">
          <option value="low">意向低</option>
          <option value="normal" selected>意向中</option>
          <option value="high">意向高</option>
        </select>
        <button type="submit" class="btn btn--primary">创建</button>
      </div>
    </form>`;
  }

  function columnHTML(col, isEntry) {
    return `<section class="column column--${esc(col.kind)}" data-stage="${esc(col.key)}" style="--stage-color: ${esc(col.color)}">
      <header class="column__head">
        <h2>${esc(col.label)}</h2>
        <span class="badge" data-count>${col.count}</span>
      </header>
      <div class="column__body" data-dropzone>${col.applications.map(cardHTML).join('')}</div>
      ${isEntry ? createFormHTML() : ''}
    </section>`;
  }

  // renderBoard 整体重建看板：阶段本身可能被增删改序，只填卡片是不够的。
  function renderBoard(board) {
    STAGES = board.columns.map(toStage);
    boardEl.innerHTML = board.columns.map((col, i) => columnHTML(col, i === 0)).join('');
    renderSummary(board.summary);
    renderStageFilter(board.columns);
  }

  function renderSummary(s) {
    document.querySelector('.summary__bar span').style.width = `${s.percent}%`;
    for (const key of ['total', 'active', 'offer', 'rejected', 'upcoming']) {
      const el = document.querySelector(`[data-summary="${key}"]`);
      if (el) el.textContent = s[key];
    }
  }

  // renderStageFilter 让顶部的阶段筛选跟着阶段配置一起更新。
  function renderStageFilter(columns) {
    const group = document.querySelectorAll('.filters__group')[1];
    if (!group) return;
    const params = new URLSearchParams(location.search);
    const current = params.get('stage') || 'all';
    const link = (key, label) => {
      const p = new URLSearchParams(location.search);
      p.set('stage', key);
      return `<a class="chip ${current === key ? 'chip--on' : ''}" href="?${p}">${esc(label)}</a>`;
    };
    group.innerHTML = '<span class="filters__label">阶段</span>' + link('all', '全部') +
      columns.map((c) => link(c.key, c.label)).join('');
  }

  async function refresh() {
    try {
      renderBoard(await API.get(`/api/boards/${boardKey}/board${query}`));
    } catch (err) {
      API.toast(err.message);
    }
  }

  // ---------- 阶段流转（拖拽 + 下拉共用一个接口） ----------

  async function move(appId, to, index) {
    try {
      const data = await API.patch(`/api/applications/${appId}/stage${query}`, { to, index });
      renderBoard(data.board);
      const stage = STAGES[stageIndex(to)];
      API.toast(`已推进到「${stage ? stage.label : to}」`, true);
    } catch (err) {
      API.toast(err.message);
      await refresh(); // 失败时用服务端数据回滚乐观更新
    }
  }

  let dragging = null;

  boardEl.addEventListener('dragstart', (e) => {
    const card = e.target.closest('.card');
    if (!card) return;
    dragging = card;
    card.classList.add('dragging');
    e.dataTransfer.effectAllowed = 'move';
    e.dataTransfer.setData('text/plain', card.dataset.app);
  });

  boardEl.addEventListener('dragend', () => {
    if (dragging) dragging.classList.remove('dragging');
    dragging = null;
    document.querySelectorAll('.dragover').forEach((el) => el.classList.remove('dragover'));
  });

  boardEl.addEventListener('dragover', (e) => {
    const zone = e.target.closest('[data-dropzone]');
    if (!zone || !dragging) return;
    e.preventDefault();
    zone.classList.add('dragover');
  });

  boardEl.addEventListener('dragleave', (e) => {
    const zone = e.target.closest('[data-dropzone]');
    if (zone && !zone.contains(e.relatedTarget)) zone.classList.remove('dragover');
  });

  boardEl.addEventListener('drop', (e) => {
    const zone = e.target.closest('[data-dropzone]');
    if (!zone || !dragging) return;
    e.preventDefault();
    zone.classList.remove('dragover');

    const card = dragging;
    const to = zone.closest('.column').dataset.stage;
    const target = e.target.closest('.card');

    // 乐观更新：先把卡片放过去，请求失败再由 refresh 拉回真实状态。
    if (target && target !== card) {
      const before = e.clientY < target.getBoundingClientRect().top + target.offsetHeight / 2;
      zone.insertBefore(card, before ? target : target.nextSibling);
    } else if (!target) {
      zone.appendChild(card);
    }
    move(card.dataset.app, to, [...zone.children].indexOf(card));
  });

  // ---------- 卡片交互 ----------

  boardEl.addEventListener('change', (e) => {
    const select = e.target.closest('[data-move]');
    if (!select || !select.value) return;
    const card = select.closest('.card');
    const to = select.value;
    select.value = '';
    move(card.dataset.app, to, -1);
  });

  boardEl.addEventListener('click', (e) => {
    if (e.target.closest('[data-move]') || e.target.closest('.create')) return;
    const card = e.target.closest('.card');
    if (card) openDrawer(card.dataset.app);
  });

  // ---------- 新建投递 ----------

  boardEl.addEventListener('submit', async (e) => {
    const form = e.target.closest('#create-form');
    if (!form) return;
    e.preventDefault();

    const data = new FormData(form);
    const company = String(data.get('company') || '').trim();
    if (!company) { // 前端先挡一次，后端仍会返回 400 兜底
      API.toast('公司名称不能为空');
      return;
    }
    const owner = Number(data.get('owner_id'));
    try {
      const res = await API.post(`/api/boards/${boardKey}/applications${query}`, {
        company,
        role: String(data.get('role') || '').trim(),
        owner_id: owner > 0 ? owner : null,
        intent: data.get('intent'),
      });
      renderBoard(res.board);
      API.toast(`已创建 ${res.application.key}`, true);
    } catch (err) {
      API.toast(err.message);
    }
  });

  // ---------- 详情抽屉 ----------

  const drawerForm = document.getElementById('drawer-form');
  const roundsEl = document.getElementById('drawer-rounds');
  let currentAppId = null;

  async function openDrawer(appId) {
    try {
      const data = await API.get(`/api/applications/${appId}`);
      const app = data.application;
      currentAppId = app.id;

      document.getElementById('drawer-key').textContent = app.key;
      document.getElementById('drawer-stage').textContent = app.stage_label;
      drawerForm.company.value = app.company;
      drawerForm.role.value = app.role;
      drawerForm.channel.value = app.channel;
      drawerForm.notes.value = app.notes;
      drawerForm.owner_id.value = String(app.owner_id || 0);
      drawerForm.intent.value = app.intent;

      renderRounds(data.rounds);
      document.getElementById('drawer-events').innerHTML = data.events
        .map((ev) => `<li>${esc(ev.text)}<time>${new Date(ev.created_at).toLocaleString('zh-CN')}</time></li>`)
        .join('');
      drawer.hidden = false;
    } catch (err) {
      API.toast(err.message);
    }
  }

  document.querySelectorAll('[data-close]').forEach((btn) => {
    btn.addEventListener('click', () => { document.getElementById(btn.dataset.close).hidden = true; });
  });

  drawerForm.addEventListener('submit', async (e) => {
    e.preventDefault();
    if (!currentAppId) return;
    try {
      const data = await API.patch(`/api/applications/${currentAppId}${query}`, {
        company: drawerForm.company.value,
        role: drawerForm.role.value,
        channel: drawerForm.channel.value,
        notes: drawerForm.notes.value,
        owner_id: Number(drawerForm.owner_id.value),
        intent: drawerForm.intent.value,
      });
      renderBoard(data.board);
      API.toast('已保存', true);
      openDrawer(currentAppId); // 重新拉一次，日志随之更新
    } catch (err) {
      API.toast(err.message);
    }
  });

  // ---------- 面试排期 ----------

  const options = (pairs, current) => pairs
    .map(([v, l]) => `<option value="${v}"${v === current ? ' selected' : ''}>${l}</option>`).join('');

  function roundHTML(r) {
    return `<div class="round${overdue(r) ? ' round--overdue' : ''}" data-round="${r.id}">
      <div class="round__head">
        <b>${esc(r.stage_label)}</b>
        <select data-field="result">${options(RESULTS, r.result)}</select>
        <button type="button" class="btn btn--tiny btn--danger" data-del-round>删除</button>
      </div>
      <div class="round__row">
        <label>面试时间<input type="datetime-local" data-field="scheduled_at" value="${toLocalInput(r.scheduled_at)}"></label>
        <label>时长（分钟）<input type="number" data-field="duration_min" min="5" max="600" value="${r.duration_min}"></label>
        <label>方式<select data-field="mode">${options(MODES, r.mode)}</select></label>
      </div>
      <div class="round__row">
        <label>会议链接<input type="url" data-field="meeting_url" placeholder="https://…" value="${esc(r.meeting_url)}"></label>
        <label>地点 / 会议室<input type="text" data-field="meeting_place" maxlength="120" value="${esc(r.meeting_place)}"></label>
        <label>面试官<input type="text" data-field="interviewer" maxlength="60" value="${esc(r.interviewer)}"></label>
      </div>
      <label>面试记录<textarea data-field="notes" rows="2" maxlength="2000">${esc(r.notes)}</textarea></label>
      <button type="button" class="btn btn--tiny btn--primary" data-save-round>保存这一轮</button>
    </div>`;
  }

  function renderRounds(rounds) {
    roundsEl.innerHTML = rounds.length
      ? rounds.map(roundHTML).join('')
      : '<p class="empty">还没有面试记录，点「+ 安排面试」添加一场。</p>';
  }

  document.getElementById('add-round').addEventListener('click', async () => {
    if (!currentAppId) return;
    try {
      const data = await API.post(`/api/applications/${currentAppId}/rounds${query}`, {});
      renderBoard(data.board);
      renderRounds(data.rounds);
      API.toast('已添加一场待安排的面试', true);
    } catch (err) {
      API.toast(err.message);
    }
  });

  roundsEl.addEventListener('click', async (e) => {
    const row = e.target.closest('[data-round]');
    if (!row) return;
    const id = row.dataset.round;

    if (e.target.closest('[data-save-round]')) {
      const field = (name) => row.querySelector(`[data-field="${name}"]`);
      try {
        const data = await API.patch(`/api/rounds/${id}${query}`, {
          scheduled_at: toRFC3339(field('scheduled_at').value),
          duration_min: Number(field('duration_min').value),
          mode: field('mode').value,
          meeting_url: field('meeting_url').value.trim(),
          meeting_place: field('meeting_place').value.trim(),
          interviewer: field('interviewer').value.trim(),
          result: field('result').value,
          notes: field('notes').value,
        });
        renderBoard(data.board);
        renderRounds(data.rounds);
        API.toast('面试信息已保存', true);
      } catch (err) {
        API.toast(err.message);
      }
      return;
    }

    if (e.target.closest('[data-del-round]')) {
      if (!confirm('确定删除这条面试记录？')) return;
      try {
        const data = await API.del(`/api/rounds/${id}${query}`);
        renderBoard(data.board);
        renderRounds(data.rounds);
        API.toast('已删除', true);
      } catch (err) {
        API.toast(err.message);
      }
    }
  });

  // ---------- 阶段配置 ----------

  function stageRowHTML(s, i, total) {
    return `<div class="stage-row" data-stage-id="${s.id}">
      <input type="color" data-field="color" value="${esc(s.color)}" title="列头颜色">
      <input type="text" data-field="label" value="${esc(s.label)}" maxlength="20">
      <select data-field="kind">${options(KINDS.map((k) => [k.value, k.label]), s.kind)}</select>
      <label class="stage-row__flag" title="进入该阶段前必须先指定跟进人">
        <input type="checkbox" data-field="requires_owner"${s.requires_owner ? ' checked' : ''}> 需跟进人
      </label>
      <button type="button" class="btn btn--tiny" data-shift="-1"${i === 0 ? ' disabled' : ''}>↑</button>
      <button type="button" class="btn btn--tiny" data-shift="1"${i === total - 1 ? ' disabled' : ''}>↓</button>
      <button type="button" class="btn btn--tiny btn--danger" data-del-stage>删除</button>
    </div>`;
  }

  function renderStages() {
    document.getElementById('stage-list').innerHTML =
      STAGES.map((s, i) => stageRowHTML(s, i, STAGES.length)).join('');
  }

  document.getElementById('open-stages').addEventListener('click', () => {
    renderStages();
    stageDrawer.hidden = false;
  });

  const stageList = document.getElementById('stage-list');

  // 阶段配置改完立刻生效：后端连带返回最新看板，这里一次性重绘。
  async function applyStage(promise, okText) {
    try {
      const data = await promise;
      renderBoard(data.board);
      renderStages();
      API.toast(okText, true);
    } catch (err) {
      API.toast(err.message);
      renderStages(); // 失败时用当前配置回滚输入框
    }
  }

  stageList.addEventListener('change', (e) => {
    const row = e.target.closest('[data-stage-id]');
    const field = e.target.closest('[data-field]');
    if (!row || !field) return;
    const name = field.dataset.field;
    const value = field.type === 'checkbox' ? field.checked : field.value;
    applyStage(API.patch(`/api/stages/${row.dataset.stageId}${query}`, { [name]: value }), '阶段已更新');
  });

  stageList.addEventListener('click', (e) => {
    const row = e.target.closest('[data-stage-id]');
    if (!row) return;
    const id = row.dataset.stageId;

    const shift = e.target.closest('[data-shift]');
    if (shift) {
      const index = STAGES.findIndex((s) => String(s.id) === id);
      applyStage(API.patch(`/api/stages/${id}/position${query}`,
        { index: index + Number(shift.dataset.shift) }), '列顺序已调整');
      return;
    }
    if (e.target.closest('[data-del-stage]')) {
      if (!confirm('确定删除这个阶段？')) return;
      applyStage(API.del(`/api/stages/${id}${query}`), '阶段已删除');
    }
  });

  document.getElementById('stage-form').addEventListener('submit', async (e) => {
    e.preventDefault();
    const form = e.target;
    const label = form.label.value.trim();
    if (!label) return;
    await applyStage(API.post(`/api/boards/${boardKey}/stages${query}`,
      { label, kind: form.kind.value }), '阶段已添加');
    form.reset();
  });

  // ---------- 成员 ----------

  document.getElementById('current-member').addEventListener('change', async (e) => {
    try {
      const data = await API.post('/api/session/member', { member_id: Number(e.target.value) });
      API.toast(`当前用户已切换为 ${data.member.name}`, true);
    } catch (err) {
      API.toast(err.message);
    }
  });

  document.getElementById('add-member').addEventListener('click', async () => {
    const name = prompt('新成员姓名');
    if (name === null) return;
    try {
      await API.post('/api/members', { name: name.trim(), role: 'member' });
      API.toast('成员已添加，正在刷新', true);
      location.reload(); // 成员出现在多个下拉里，整页刷新最省事
    } catch (err) {
      API.toast(err.message);
    }
  });

  // ---------- 时间与文案小工具 ----------

  function whenText(iso) {
    if (!iso) return '待安排';
    const d = new Date(iso);
    return `${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}`;
  }

  function meetingText(r) {
    if (r.meeting_place) return r.meeting_place;
    if (r.meeting_url) return '线上会议';
    return (MODES.find(([v]) => v === r.mode) || [, '线上'])[1];
  }

  const overdue = (r) => r.result === 'pending' && r.scheduled_at && new Date(r.scheduled_at) < new Date();

  const needsSchedule = (app) => app.stage_kind === 'interview' &&
    (!app.next_round || !app.next_round.scheduled_at);

  const pad = (n) => String(n).padStart(2, '0');

  // datetime-local 只认本地时间字符串，接口用 RFC3339，这里做双向转换。
  function toLocalInput(iso) {
    if (!iso) return '';
    const d = new Date(iso);
    return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`;
  }

  function toRFC3339(local) {
    if (!local) return ''; // 空串 = 退回「待安排」
    const d = new Date(local);
    return isNaN(d) ? '' : d.toISOString();
  }

  hydrateMoves(); // 首屏卡片由模板渲染，这里补上流转下拉
})();
