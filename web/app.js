'use strict';

/* AnimalGuide — интерфейс. Состояние прогона живёт на сервере, здесь —
   только то, что показано: выбранный диалог (он же в адресе страницы),
   выбранный ход журнала и идущий ход с его потоком событий. */

const app = {
  meta: null,
  convs: [],
  conv: null,          // диалог целиком (Detail)
  selected: null,      // ход, чей журнал открыт
  tab: 'events',
  live: null,          // идущий ход: {view, events, updates}
  stream: null,        // EventSource идущего хода
  panels: {},          // панели механизмов по имени хука: function(extra, conv) → html
  windows: {},         // окна: имя → {title, render()}
};
window.app = app;

/* ---------- мелочи ---------- */

function esc(s) {
  return String(s == null ? '' : s).replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
}
function md(text) {
  const safe = esc(text || '');
  try { return window.marked ? window.marked.parse(safe, { breaks: true }) : safe.replace(/\n/g, '<br>'); }
  catch (e) { return safe; }
}
function $(id) { return document.getElementById(id); }
function usd(c) {
  if (!c || !c.known) return '—';
  return '$' + (c.usd < 0.01 ? c.usd.toFixed(5) : c.usd.toFixed(4));
}
function num(n) { return (n || 0).toLocaleString('ru-RU'); }
function when(t) { return t ? new Date(t).toLocaleString('ru-RU', { hour: '2-digit', minute: '2-digit', day: '2-digit', month: '2-digit' }) : ''; }
function plural(n, one, few, many) {
  const m10 = n % 10, m100 = n % 100;
  if (m10 === 1 && m100 !== 11) return n + ' ' + one;
  if (m10 >= 2 && m10 <= 4 && (m100 < 10 || m100 >= 20)) return n + ' ' + few;
  return n + ' ' + many;
}
let toastTimer = null;
function toast(msg, bad) {
  const el = $('toast');
  el.textContent = msg;
  el.className = 'toast' + (bad ? ' bad' : '');
  el.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { el.hidden = true; }, bad ? 7000 : 3500);
}

async function api(method, path, body) {
  const opts = { method, headers: {} };
  if (body !== undefined) {
    opts.headers['Content-Type'] = 'application/json';
    opts.body = JSON.stringify(body);
  }
  const res = await fetch(path, opts);
  const text = await res.text();
  let data = {};
  try { data = text ? JSON.parse(text) : {}; } catch (e) { data = { error: text }; }
  if (!res.ok) throw new Error(data.error || ('HTTP ' + res.status));
  return data;
}

/* ---------- загрузка ---------- */

async function boot() {
  try {
    app.meta = await api('GET', '/api/meta');
  } catch (e) {
    toast('Сервер не отвечает: ' + e.message, true);
    return;
  }
  await refreshList();
  const id = hashId();
  if (id && app.convs.some(c => c.id === id)) {
    await loadConv(id);
  } else if (app.convs.length) {
    await loadConv(app.convs[0].id);
  } else {
    render();
  }
}

function hashId() {
  const m = /c=([0-9a-f]+)/.exec(location.hash);
  return m ? m[1] : '';
}

async function refreshList() {
  try {
    app.convs = (await api('GET', '/api/conversations')).conversations || [];
  } catch (e) { toast(e.message, true); }
}

async function loadConv(id, keepSelection) {
  try {
    const d = (await api('GET', '/api/conversations/' + id)).conversation;
    app.conv = d;
    if (location.hash !== '#c=' + id) history.replaceState(null, '', '#c=' + id);
    if (!keepSelection || !d.turnList.some(t => t.id === app.selected)) {
      app.selected = d.turnList.length ? d.turnList[d.turnList.length - 1].id : null;
    }
    if (d.active && (!app.live || app.live.view.id !== d.active.id)) follow(d.active.id);
  } catch (e) {
    toast(e.message, true);
  }
  render();
}

/* ---------- отрисовка ---------- */

function render() {
  renderDialogs();
  renderReadings();
  renderBranches();
  renderContext();
  renderMechanisms();
  renderExtPanels();
  renderFeed();
  renderJournal();
  const busy = !!app.live;
  $('send-button').disabled = busy;
  $('composer-hint').textContent = busy ? 'справочник отвечает…' : '';
}

function renderDialogs() {
  const sel = $('dialog-select');
  const cur = app.conv ? app.conv.id : '';
  let html = '';
  if (!app.convs.length) html = '<option value="">— диалогов нет —</option>';
  for (const c of app.convs) {
    const title = c.title || 'без названия';
    const lane = c.lane ? ' · ' + c.lane : '';
    html += `<option value="${esc(c.id)}"${c.id === cur ? ' selected' : ''}>${esc(title)}${esc(lane)} (${c.turns})</option>`;
  }
  sel.innerHTML = html;
}

function renderReadings() {
  const m = app.meta || {};
  const c = app.conv;
  const items = [['Модель', m.model || '—']];
  if (c) {
    items.push(['Ходов', num(c.turns)]);
    items.push(['Цена диалога', usd(sumCost(c.totals.cost, c.meter.cost))]);
    items.push(['Собеседник', (c.owners || []).join(', ') || '—']);
  }
  items.push(['Сервер запущен', when(m.serverStarted)]);
  $('readings').innerHTML = items.map(([k, v]) => `<div><dt>${esc(k)}</dt><dd>${esc(v)}</dd></div>`).join('');
}

function sumCost(a, b) {
  if (!a || !a.known) return b;
  if (!b || !b.known) return a;
  return { usd: a.usd + b.usd, known: true };
}

function renderBranches() {
  const c = app.conv;
  if (!c) { $('branches').innerHTML = '<span class="hint">нет диалога</span>'; return; }
  const byParent = {};
  for (const b of c.branchTree) (byParent[b.parent || ''] = byParent[b.parent || ''] || []).push(b);
  const walk = (parent, depth) => (byParent[parent] || []).map(b =>
    `<div class="branch${b.active ? ' active' : ''}" style="padding-left:${depth * 14}px">
       <button type="button" class="branch-name" data-action="switchBranch" data-arg="${esc(b.id)}" title="Перейти в ветку">${depth ? '↳ ' : ''}${esc(b.name)}</button>
       <span class="hint">${plural(b.turns, 'ход', 'хода', 'ходов')}</span>
     </div>` + walk(b.id, depth + 1)).join('');
  let html = walk('', 0);
  html += `<div class="cps"><button type="button" class="small" data-action="mark" title="Поставить точку сохранения на конце текущей ветки">+ точка</button>`;
  for (const cp of c.checkpoints) {
    html += `<button type="button" class="small cp" data-action="fork" data-arg="${esc(cp.id)}" title="Новая ветка от точки «${esc(cp.name)}»: всё до неё — общее, после — своё">⑂ ${esc(cp.name)}</button>`;
  }
  html += '</div>';
  $('branches').innerHTML = html;
}

const blockColors = {
  system: '#8a8f86', charter: '#a3412e', profile: '#6a4c93', 'memory.long': '#2c5a85', 'collection.state': '#a5701a',
  'memory.work': '#3f8a9c', facts: '#2f6b4f', tools: '#b8a88a', history: '#d4c29c', user: '#1f2a24',
};

function lastContext() {
  const c = app.conv;
  if (!c) return null;
  for (let i = c.turnList.length - 1; i >= 0; i--) {
    const ctx = c.turnList[i].context;
    if (ctx && ctx.estimate && ctx.estimate.total) return ctx;
  }
  return null;
}

function renderContext() {
  const ctx = lastContext();
  if (!ctx) { $('context').innerHTML = '<span class="hint">появится после первого хода с моделью</span>'; return; }
  const e = ctx.estimate;
  const parts = [['system', 'системный промпт', e.system]];
  const mechs = (app.meta && app.meta.mechanisms) || [];
  for (const m of mechs) if (e.blocks && e.blocks[m.name]) parts.push([m.name, m.title, e.blocks[m.name]]);
  parts.push(['tools', 'инструменты', e.tools], ['history', 'окно истории', e.history], ['user', 'реплика', e.user]);
  const total = e.total || 1;
  let bar = '', legend = '';
  for (const [key, title, n] of parts) {
    if (!n) continue;
    const color = blockColors[key] || '#999';
    bar += `<span style="width:${(n / total * 100).toFixed(2)}%;background:${color}" title="${esc(title)}: ${num(n)}"></span>`;
    legend += `<span><i style="background:${color}"></i>${esc(title)} ${num(n)}</span>`;
  }
  const constant = e.constant || 0;
  const over = constant > 6000;
  let fact = '';
  if (ctx.firstPrompt) fact = ` · факт первого запроса ${num(ctx.firstPrompt)}, из кэша ${num(ctx.cacheHit)}`;
  $('context').innerHTML = `<div class="hint">≈ ${num(total)} токенов${fact}</div>
    <div class="scale">${bar}</div><div class="legend">${legend}</div>
    <div class="budget${over ? ' over' : ''}">постоянная часть ≈ ${num(constant)} из 6 000 (бюджет раздела 10)</div>`;
}

function renderMechanisms() {
  const c = app.conv;
  const list = c ? c.mechanisms : ((app.meta && app.meta.mechanisms) || []);
  const on = list.filter(m => m.on).length;
  $('mech-count').textContent = `включено ${on} из ${list.length}` + (c ? '' : ' (для новых диалогов)');
  $('mechanisms').innerHTML = list.map(m => {
    const cost = [];
    if (m.cost.tokens) cost.push('≈' + m.cost.tokens + ' токенов');
    if (m.cost.requests) cost.push(m.cost.requests + ' запроса на ход');
    const tip = `${m.about}\nВыключен: ${m.fallback}\nЦена: ${cost.join(', ') || 'без токенов'}${m.cost.note ? ' — ' + m.cost.note : ''}\nВид: ${(m.kind || []).join(', ')}`;
    return `<button type="button" class="mech ${m.on ? 'on' : 'off'}" data-action="toggleMechanism" data-arg="${esc(m.name)}" data-on="${m.on ? '1' : ''}"
      title="${esc(tip)}"${c ? '' : ' disabled'}><span class="dot"></span>${esc(m.title)}</button>`;
  }).join('');
}

function renderExtPanels() {
  const box = $('ext-panels');
  const c = app.conv;
  let html = '';
  if (c && c.extras) {
    for (const [name, fn] of Object.entries(app.panels)) {
      if (c.extras[name] === undefined) continue;
      try { html += fn(c.extras[name], c) || ''; } catch (e) { html += `<section class="panel"><h2>${esc(name)}</h2><span class="hint">${esc(e.message)}</span></section>`; }
    }
  }
  box.innerHTML = html;
}

/* ---------- лента ---------- */

function renderFeed() {
  const c = app.conv;
  const feed = $('feed');
  if (!c || (!c.turnList.length && !app.live)) {
    feed.innerHTML = `<div class="empty-feed"><h2>Спросите про животное</h2>
      <p>Карточка собирается только из источников — русской Википедии и GBIF. Разделы раскрываются по клику, дерево кликабельное.</p>
      <div class="examples">
        ${['рысь', 'манул', 'шурундук пятнистый', 'Сравни рысь и манула', 'Расскажи про ежа: где живёт и чем питается?']
          .map(x => `<button type="button" data-action="example" data-arg="${esc(x)}">${esc(x)}</button>`).join('')}
      </div></div>`;
    return;
  }
  const started = app.meta ? new Date(app.meta.serverStarted) : null;
  const shown = new Set();   // карточки, уже показанные выше в ленте
  const state = cardsState();
  let html = '', seamDone = false;
  c.turnList.forEach((t, i) => {
    if (started && !seamDone && i > 0 && new Date(t.started) > started && new Date(c.turnList[i - 1].started) < started) {
      html += '<div class="seam">сервер перезапущен — разговор продолжается с того же места</div>';
      seamDone = true;
    }
    if (i > 0 && t.collection && t.collection !== c.turnList[i - 1].collection) {
      html += '<div class="seam">новая подборка</div>';
    }
    html += turnHTML(t, state, shown);
  });
  if (app.live) html += liveHTML(state, shown);
  const atBottom = window.innerHeight + window.scrollY >= document.body.scrollHeight - 80;
  feed.innerHTML = html;
  if (atBottom) window.scrollTo(0, document.body.scrollHeight);
}

// cardsState — карточки текущей ветки вместе с тем, что уже пришло в идущем
// ходе: карточка по мере сборки (ФТ-14).
function cardsState() {
  const c = app.conv;
  const st = c ? JSON.parse(JSON.stringify(c.cards)) : { cards: [], comparisons: [], notFound: [] };
  if (!app.live) return st;
  for (const u of app.live.updates) {
    if (u.kind === 'card') {
      const i = st.cards.findIndex(x => x.id === u.data.id);
      if (i >= 0) st.cards[i] = mergeCard(st.cards[i], u.data); else st.cards.push(u.data);
    } else if (u.kind === 'section') {
      const card = st.cards.find(x => x.id === u.data.cardId);
      if (card) {
        const i = card.sections.findIndex(s => s.key === u.data.section.key);
        if (i >= 0) card.sections[i] = u.data.section;
      }
    } else if (u.kind === 'neighbors') {
      const card = st.cards.find(x => x.id === u.data.cardId);
      if (card) {
        card.neighbors = (card.neighbors || []).filter(n => n.nodeKey !== u.data.neighbors.nodeKey);
        card.neighbors.push(u.data.neighbors);
      }
    } else if (u.kind === 'comparison') {
      st.comparisons.push(u.data);
    }
  }
  return st;
}

function mergeCard(old, fresh) {
  const out = Object.assign({}, fresh);
  out.sections = fresh.sections.map(s => {
    const had = (old.sections || []).find(x => x.key === s.key);
    return had && (had.status === 'read' || had.status === 'none') && s.status === 'unread' ? had : s;
  });
  if ((!out.tree || !out.tree.length) && old.tree) out.tree = old.tree;
  return out;
}

const kindTitles = { section: 'раздел', node: 'узел дерева', open: 'карточка', compare: 'сравнение' };

function turnHTML(t, state, shown) {
  const click = t.kind && t.kind !== 'message';
  let html = `<div class="turn${t.id === app.selected ? ' selected' : ''}" id="turn-${esc(t.id)}">`;
  html += `<div class="bubble user${click ? ' click' : ''}">${click ? '⤷ ' : ''}${esc(t.user)}</div>`;
  html += deltasHTML(t.cards || [], state, shown, t.id);
  if (t.status === 'failed') {
    html += `<div class="bubble reply error">Ход не удался: ${esc(t.error)}</div>`;
  } else if (t.reply && !(t.route === 'card' && (t.cards || []).some(d => d.kind === 'card'))) {
    html += `<div class="bubble reply">${md(t.reply)}</div>`;
  }
  html += chipsHTML(t);
  html += `<div class="turn-meta">
    <button type="button" class="link" data-action="selectTurn" data-arg="${esc(t.id)}">журнал хода</button>
    <span>${when(t.started)}</span><span>${routeTitle(t.route)}</span>
    <span>${plural(t.totals.llmCalls, 'запрос', 'запроса', 'запросов')} к модели</span>
    <span>${usd(t.totals.cost)}</span><span>${(t.totals.seconds || 0).toFixed(1)} с</span>
    ${mechDiff(t)}
  </div></div>`;
  return html;
}

function routeTitle(r) {
  return ({ card: 'карточка', section: 'раздел по клику', node: 'узел дерева', compare: 'сравнение', lead: 'ведущий диалога', collection: 'подборка' })[r] || '';
}

// mechDiff — ход прошёл не с тем набором механизмов, что просили (откат
// пути до источников): видно прямо в ленте.
function mechDiff(t) {
  const req = t.requested || {}, eff = t.effective || {};
  const diff = Object.keys(req).filter(k => !!req[k] !== !!eff[k]);
  return diff.length ? `<span class="chip warn" title="Механизм откатился на ходу — см. журнал">откат: ${esc(diff.join(', '))}</span>` : '';
}

// chipsHTML — правки механизмов под ответом (память, профиль, подборка,
// страж): их кладут хуки в extras хода, а рисуют зарегистрированные
// обработчики.
function chipsHTML(t) {
  const chips = [];
  for (const [name, fn] of Object.entries(app.chips)) {
    if (t.extras && t.extras[name] !== undefined) {
      try { chips.push(...(fn(t.extras[name], t) || [])); } catch (e) { /* чужие данные не роняют ленту */ }
    }
  }
  return chips.length ? `<div class="chips">${chips.join('')}</div>` : '';
}
app.chips = {};

function deltasHTML(deltas, state, shown, turnId) {
  let html = '';
  for (const d of deltas) {
    if (d.kind === 'card' && d.card) {
      if (shown.has(d.card.id)) { html += `<div class="hint">карточка «${esc(d.card.name)}» — выше в ленте</div>`; continue; }
      shown.add(d.card.id);
      const cur = state.cards.find(c => c.id === d.card.id) || d.card;
      html += cardHTML(cur);
    } else if (d.kind === 'notfound' && d.notFound) {
      html += `<div class="notfound"><b>Сведений о «${esc(d.notFound.query)}» нет.</b> ${esc(d.notFound.reason)}${d.notFound.gate ? ' <span class="chip">отказал привратник — дорогие шаги не делались</span>' : ''}</div>`;
    } else if (d.kind === 'comparison' && d.comparison) {
      const idx = state.comparisons.findIndex(x => x.a.id === d.comparison.a.id && x.b.id === d.comparison.b.id);
      html += compareHTML(d.comparison, idx);
    } else if (d.kind === 'neighbors' && d.neighbors) {
      const card = state.cards.find(c => c.id === d.cardId);
      if (card && !shown.has(card.id)) { shown.add(card.id); html += cardHTML(card); }
    }
  }
  return html;
}

function whyBtn(why, label) {
  if (!why) return '';
  const tip = `Почему так: ${why.tool}${why.sourceTitle ? ' — ' + why.sourceTitle : ''}. Нажмите — откроется событие журнала с этим вызовом.`;
  return `<button type="button" class="why" data-action="why" data-turn="${esc(why.turn || '')}" data-call="${esc(why.callId || '')}" title="${esc(tip)}">${label || '?'}</button>`;
}

function cardHTML(c) {
  const rank = c.rankRu || (c.rank || '').toLowerCase();
  let html = `<article class="acard${c.unverified ? ' unverified' : ''}" data-card="${esc(c.id)}">
    <div class="acard-head"><h3>${esc(c.name)}</h3>
      ${latinHidden() ? `<span class="hint" title="${esc(c.latin)}">латынь скрыта профилем</span>` : `<span class="latin">${esc(c.latin)}</span>`}${whyBtn(c.latinWhy)}
      ${rank ? `<span class="rank">${esc(rank)}</span>` : ''}
    </div>
    <p class="summary">${esc(c.summary)} ${whyBtn(c.summaryWhy)}</p>`;
  if (c.tree && c.tree.length) {
    html += '<div class="tree">' + c.tree.map((n, i) => {
      const self = n.key === c.taxonKey;
      const title = (n.nameRu ? n.nameRu : n.name);
      const btn = self
        ? `<button type="button" class="self" disabled title="${esc(n.name)}">${esc(title)}</button>`
        : `<button type="button" data-action="node" data-card="${esc(c.id)}" data-key="${n.key}" data-name="${esc(n.nameRu || n.name)}" title="${esc((n.rankRu || n.rank) + ': ' + n.name)} — показать, кто ещё входит">${esc(title)}</button>`;
      return (i ? '<span class="arrow">›</span>' : '') + btn;
    }).join('') + ' ' + whyBtn(c.treeWhy) + '</div>';
  }
  for (const n of c.neighbors || []) {
    html += `<div class="neighbors">В «${esc(n.nodeName)}» по GBIF (${plural(n.total, 'таксон', 'таксона', 'таксонов')}) ${whyBtn(n.why)}:
      <div class="list">${n.children.map(ch => `<button type="button" class="small" data-action="open" data-arg="${esc(ch.nameRu || ch.name)}"
        title="${esc((ch.rankRu || ch.rank) + ': ' + ch.name)} — открыть карточку">${esc(ch.nameRu ? ch.nameRu + ' (' + ch.name + ')' : ch.name)}</button>`).join('')}</div></div>`;
  }
  html += '<div class="sections">';
  for (const s of c.sections || []) {
    const st = `<span class="s-status s-${esc(s.status)}">${esc(statusTitle(s.status))}</span>`;
    if (s.status === 'read' || s.status === 'none') {
      html += `<details class="section"><summary><span class="s-title">${esc(s.title)}</span>${st} ${whyBtn(s.why)}</summary>
        <div class="s-body">${s.status === 'read' ? md(s.text) : esc(s.reason)}${s.heading ? `<div class="heading">раздел статьи: «${esc(s.heading)}»</div>` : ''}</div></details>`;
    } else if (s.status === 'reading') {
      html += `<div class="section"><div class="section-head"><span class="s-title">${esc(s.title)}</span>${st}<span class="thinking">специалист читает</span></div></div>`;
    } else {
      html += `<div class="section"><div class="section-head"><span class="s-title">${esc(s.title)}</span>${st}
        <button type="button" class="small" data-action="section" data-card="${esc(c.id)}" data-topic="${esc(s.key)}"${app.live ? ' disabled' : ''}>прочитать</button>
        ${s.reason ? `<span class="hint">${esc(s.reason)}</span>` : ''}</div></div>`;
    }
  }
  html += '</div>';
  if (c.notes && c.notes.length) html += '<ul class="notes">' + c.notes.map(n => `<li>${esc(n)}</li>`).join('') + '</ul>';
  html += `<div class="sources">Источники: ${(c.sources || []).map(s => `<a href="${esc(s.url)}" target="_blank" rel="noopener">${esc(s.title)}</a>`).join(' · ')}
    <button type="button" class="small" data-action="export" data-kind="card" data-arg="${esc(c.id)}">↓ markdown</button>
    <button type="button" class="small" data-action="compareWith" data-arg="${esc(c.name)}" title="Сравнение уйдёт в отдельную ветку">сравнить с…</button>
    <button type="button" class="small" data-action="bookmark" data-arg="${esc(c.name)}" title="Закладка — в долговременную память собеседника">☆ в закладки</button></div>`;
  return html + '</article>';
}

function statusTitle(s) {
  return ({ unread: 'не прочитан', reading: 'читается', read: 'прочитан', none: 'сведений нет' })[s] || s;
}

function compareHTML(cmp, idx) {
  const cell = x => `<td class="${x.confirmed ? '' : 'nodata'}">${esc(x.text)} ${x.confirmed ? whyBtn(x.why) : ''}</td>`;
  return `<div class="compare"><b>Сравнение: ${esc(cmp.a.name)} и ${esc(cmp.b.name)}</b>
    ${idx >= 0 ? `<button type="button" class="small" data-action="export" data-kind="comparison" data-arg="${idx}">↓ markdown</button>` : ''}
    <table class="cmp"><tr><th></th><th>${esc(cmp.a.name)}</th><th>${esc(cmp.b.name)}</th></tr>
    ${cmp.rows.map(r => `<tr><th>${esc(r.aspect)}</th>${cell(r.a)}${cell(r.b)}</tr>`).join('')}</table>
    ${cmp.notes && cmp.notes.length ? '<ul class="notes">' + cmp.notes.map(n => `<li>${esc(n)}</li>`).join('') + '</ul>' : ''}</div>`;
}

function liveHTML(state, shown) {
  const l = app.live;
  const v = l.view;
  let html = `<div class="turn${v.id === app.selected ? ' selected' : ''}" id="turn-${esc(v.id)}">`;
  html += `<div class="bubble user${v.kind !== 'message' ? ' click' : ''}">${esc(v.user || kindTitles[v.kind] || '')}</div>`;
  const deltas = [];
  for (const u of l.updates) {
    if (u.kind === 'card') deltas.push({ kind: 'card', card: u.data });
    if (u.kind === 'notfound') deltas.push({ kind: 'notfound', notFound: u.data });
    if (u.kind === 'comparison') deltas.push({ kind: 'comparison', comparison: u.data });
  }
  html += deltasHTML(deltas, state, shown, v.id);
  if (v.status === 'running') {
    const last = l.events.length ? l.events[l.events.length - 1].title : 'начинаю';
    html += `<div class="bubble reply"><span class="thinking">${esc(last)}</span></div>`;
  } else if (v.error) {
    html += `<div class="bubble reply error">${esc(v.error)}</div>`;
  } else if (v.reply) {
    html += `<div class="bubble reply">${md(v.reply)}</div>`;
  }
  html += `<div class="turn-meta"><span>${plural(v.totals.llmCalls, 'запрос', 'запроса', 'запросов')} к модели</span><span>${usd(v.totals.cost)}</span></div></div>`;
  return html;
}

/* ---------- журнал ---------- */

function selectedTurn() {
  if (app.live && app.selected === app.live.view.id) return { live: true, id: app.live.view.id, events: app.live.events };
  const c = app.conv;
  if (!c) return null;
  const t = c.turnList.find(x => x.id === app.selected);
  return t ? { id: t.id, events: t.events || [], turn: t } : null;
}

function renderJournal() {
  $('tab-events').classList.toggle('active', app.tab === 'events');
  $('tab-prompts').classList.toggle('active', app.tab === 'prompts');
  const sel = selectedTurn();
  const body = $('journal-body');
  if (!sel) { body.innerHTML = '<p class="hint">Журнал хода появится здесь.</p>'; $('journal-turn').textContent = ''; return; }
  $('journal-turn').textContent = (sel.live ? 'идёт ход · ' : '') + plural(sel.events.length, 'событие', 'события', 'событий');
  if (app.tab === 'prompts') { body.innerHTML = promptsHTML(sel.events); return; }
  const open = new Set([...body.querySelectorAll('.ev.open')].map(e => e.dataset.seq));
  body.innerHTML = sel.events.filter(e => e.kind !== 'prompt').map(e => {
    const cost = e.cost && e.cost.known ? usd(e.cost) : '';
    const tk = e.tokens ? `≈${num(e.tokens.estimated)}${e.tokens.actual ? ' / ' + num(e.tokens.actual) : ''}` : '';
    const meta = [tk, cost, e.seconds ? e.seconds.toFixed(1) + 'с' : ''].filter(Boolean).join(' · ');
    let detail = e.detail || '';
    if (e.hits) detail += (detail ? '\n\n' : '') + e.hits.map(h => '• ' + h.pattern + ': ' + h.fragment).join('\n');
    if (e.data && !detail) detail = JSON.stringify(e.data, null, 2);
    return `<div class="ev ev-kind-${esc(e.kind)}${open.has(String(e.seq)) ? ' open' : ''}" data-seq="${e.seq}" data-call="${esc(e.callId || '')}">
      <div class="ev-head" data-action="toggleEvent" data-arg="${e.seq}">
        <span class="ev-seq">${e.seq}</span><span class="ev-agent">${esc(e.agent)}</span>
        <span class="ev-title">${esc(e.title)}</span>${e.via === 'mcp' ? '<span class="ev-via">MCP</span>' : ''}
        <span class="ev-cost">${esc(meta)}</span></div>
      ${detail ? `<div class="ev-detail">${esc(detail)}</div>` : ''}</div>`;
  }).join('') || '<p class="hint">событий нет</p>';
}

function promptsHTML(events) {
  const prompts = events.filter(e => e.kind === 'prompt');
  if (!prompts.length) return '<p class="hint">В этом ходе модель не вызывалась — всё сделал код (клик по узлу дерева, сохранённый раздел).</p>';
  return prompts.map(e => {
    let p;
    try { p = JSON.parse(e.detail); } catch (x) { return `<pre>${esc(e.detail)}</pre>`; }
    let html = `<div class="prompt-block"><h4><span>${esc(p.agent)}: системный промпт</span><span>≈${num(p.estimate.system)}</span></h4><pre>${esc(p.system)}</pre></div>`;
    for (const b of p.blocks || []) {
      html += `<div class="prompt-block"><h4><span>блок «${esc(b.title || b.feature)}» (${esc(b.feature)})</span><span>≈${num(b.tokens)}</span></h4><pre>${esc(b.text)}</pre></div>`;
    }
    html += `<div class="prompt-block"><h4><span>окно истории: ${plural(p.history, 'сообщение', 'сообщения', 'сообщений')}</span><span>≈${num(p.estimate.history)}</span></h4></div>`;
    html += `<div class="prompt-block"><h4><span>реплика</span><span>≈${num(p.estimate.user)}</span></h4><pre>${esc(p.user)}</pre></div>`;
    html += `<div class="prompt-block"><h4><span>инструменты (отпечаток ${esc(p.fingerprint || '—')})</span><span>≈${num(p.estimate.tools)}</span></h4><pre>${esc((p.tools || []).map(t =>
      (t.final ? '■ ' : '• ') + t.name + (t.via === 'mcp' ? ' [MCP]' : '') + ' — ' + t.description).join('\n'))}</pre></div>`;
    return html;
  }).join('<hr>');
}

/* ---------- поток идущего хода ---------- */

function follow(turnId) {
  if (app.stream) app.stream.close();
  app.live = { view: { id: turnId, status: 'running', totals: { llmCalls: 0, cost: {} }, kind: 'message' }, events: [], updates: [] };
  app.selected = turnId;
  const es = new EventSource('/api/turns/' + turnId + '/events');
  app.stream = es;
  let pending = false;
  const redraw = () => {
    if (pending) return;
    pending = true;
    requestAnimationFrame(() => { pending = false; renderFeed(); renderJournal(); });
  };
  es.addEventListener('snapshot', ev => {
    const snap = JSON.parse(ev.data);
    app.live.view = snap.view;
    app.live.events = snap.events || [];
    app.live.updates = snap.updates || [];
    render();
  });
  es.addEventListener('log', ev => { app.live.events.push(JSON.parse(ev.data)); redraw(); });
  es.addEventListener('state', ev => { app.live.view = JSON.parse(ev.data); redraw(); });
  es.addEventListener('update', ev => { app.live.updates.push(JSON.parse(ev.data)); redraw(); });
  es.addEventListener('done', ev => {
    es.close();
    const v = JSON.parse(ev.data);
    const convId = v.conversationId;
    app.stream = null;
    app.live = null;
    if (v.status === 'failed') toast('Ход не удался: ' + v.error, true);
    refreshList().then(() => loadConv(convId));
  });
  es.onerror = () => {
    // Сервер перезапущен или ход уже записан: перечитываем диалог.
    es.close();
    if (app.stream === es) {
      app.stream = null;
      app.live = null;
      if (app.conv) loadConv(app.conv.id);
    }
  };
}

/* ---------- действия ---------- */

async function sendTurn(body) {
  if (app.live) { toast('Справочник ещё отвечает на предыдущее сообщение'); return; }
  try {
    let out;
    if (!app.conv) {
      out = await api('POST', '/api/conversations', body);
    } else {
      out = await api('POST', '/api/conversations/' + app.conv.id + '/turns', body);
    }
    if (!app.conv || app.conv.id !== out.conversationId) {
      await refreshList();
      const d = (await api('GET', '/api/conversations/' + out.conversationId)).conversation;
      app.conv = d;
      history.replaceState(null, '', '#c=' + d.id);
    }
    follow(out.turn.id);
    app.live.view = out.turn;
    render();
  } catch (e) { toast(e.message, true); }
}

const actions = {
  sendComposer() {
    const ta = $('composer-text');
    const text = ta.value.trim();
    if (!text) return;
    ta.value = '';
    sendTurn({ text });
  },
  example(x) { sendTurn({ text: x }); },
  section(_, el) { sendTurn({ kind: 'section', cardId: el.dataset.card, topic: el.dataset.topic }); },
  node(_, el) { sendTurn({ kind: 'node', cardId: el.dataset.card, nodeKey: Number(el.dataset.key), nodeName: el.dataset.name }); },
  open(name) { sendTurn({ kind: 'open', name, text: 'Открой карточку: ' + name }); },
  compareWith(name) {
    const other = prompt('С кем сравнить «' + name + '»? Сравнение уйдёт в отдельную ветку.');
    if (other && other.trim()) sendTurn({ kind: 'compare', a: name, b: other.trim(), text: 'Сравни: ' + name + ' и ' + other.trim() });
  },
  async newDialog() {
    try {
      const d = (await api('POST', '/api/conversations', { empty: true })).conversation;
      await refreshList();
      app.live = null;
      await loadConv(d.id);
      $('composer-text').focus();
    } catch (e) { toast(e.message, true); }
  },
  async pickDialog(id) { if (id) { app.live = null; if (app.stream) app.stream.close(); await loadConv(id); } },
  async switchBranch(id) { await convAction('switch', { branch: id }); },
  async mark() {
    const name = prompt('Название точки сохранения (можно пусто):', '');
    if (name === null) return;
    await convAction('checkpoints', { name });
  },
  async fork(cp) {
    const name = prompt('Название новой ветки:', '');
    if (name === null) return;
    await convAction('branches', { checkpoint: cp, name });
  },
  async toggleMechanism(name, el) { await convAction('features', { name, on: !el.dataset.on }); },
  selectTurn(id) { app.selected = id; renderFeed(); renderJournal(); },
  tab(t) { app.tab = t; renderJournal(); },
  toggleEvent(seq, el) { el.closest('.ev').classList.toggle('open'); },
  why(_, el) { showWhy(el.dataset.turn, el.dataset.call); },
  export(key, el) {
    if (!app.conv) return;
    location.href = `/api/conversations/${app.conv.id}/export?kind=${encodeURIComponent(el.dataset.kind)}&id=${encodeURIComponent(key)}`;
  },
  openWindow(name) { openWindow(name); },
  closeWindow() { $('window').close(); },
};
app.actions = actions;

async function convAction(action, body) {
  if (!app.conv) return;
  try {
    app.conv = (await api('POST', '/api/conversations/' + app.conv.id + '/' + action, body)).conversation;
    app.selected = app.conv.turnList.length ? app.conv.turnList[app.conv.turnList.length - 1].id : null;
    render();
  } catch (e) { toast(e.message, true); }
}

// showWhy — «почему так» (ФТ-12): журнал хода, откуда пришёл факт, и
// событие с этим вызовом инструмента.
function showWhy(turnId, callId) {
  if (turnId) app.selected = turnId;
  app.tab = 'events';
  renderFeed();
  renderJournal();
  if (!callId) return;
  const body = $('journal-body');
  const evs = [...body.querySelectorAll('.ev')].filter(e => e.dataset.call === callId);
  const target = evs.find(e => e.classList.contains('ev-kind-tool.result')) || evs[0];
  if (!target) { toast('Событие этого вызова в журнале не найдено'); return; }
  evs.forEach(e => e.classList.add('open'));
  target.classList.add('flash');
  target.scrollIntoView({ block: 'center' });
  setTimeout(() => target.classList.remove('flash'), 1600);
}

/* ---------- окна ---------- */

function openWindow(name) {
  if (name === 'windows') {
      const list = Object.entries(app.windows);
    $('window-title').textContent = 'Окна';
    $('window-body').innerHTML = `<div class="wlist">${list.map(([k, w]) =>
      `<button type="button" data-action="openWindow" data-arg="${esc(k)}">${esc(w.title)}</button>`).join('')}
      ${list.length ? '' : '<p class="hint">Окна появятся вместе с механизмами.</p>'}</div>`;
  } else {
    const w = app.windows[name];
    if (!w) return;
    $('window-title').textContent = w.title;
    $('window-body').innerHTML = '<p class="hint">загружаю…</p>';
    Promise.resolve(w.render()).then(html => { $('window-body').innerHTML = html; })
      .catch(e => { $('window-body').innerHTML = `<p class="hint">${esc(e.message)}</p>`; });
  }
  const dlg = $('window');
  if (!dlg.open) dlg.showModal();
}

app.windows.file = {
  title: 'Файл диалога',
  async render() {
    if (!app.conv) return '<p class="hint">нет диалога</p>';
    const f = await api('GET', '/api/conversations/' + app.conv.id + '/raw');
    return `<p class="hint">${esc(f.path)}</p><pre>${esc(f.json)}</pre>`;
  },
};

/* ---------- человек: профиль, память, карточка фактов ---------- */

function personView() { return app.conv && app.conv.extras ? app.conv.extras.persona : null; }

// latinHidden — профиль просит скрывать латынь (роль «ребёнок»): карточка
// показывает её только по наведению, а не в заголовке.
function latinHidden() {
  const p = personView();
  return !!(p && p.profile && p.profile.values && p.profile.values.latin && p.profile.values.latin.value === 'hide');
}

function fieldLabel(key, value) {
  const f = ((app.meta && app.meta.profileFields) || []).find(x => x.key === key);
  if (!f) return value;
  const o = f.options.find(x => x.value === value);
  return o ? o.title : value;
}

app.panels.persona = (v, conv) => {
  const fields = (app.meta && app.meta.profileFields) || [];
  const values = (v.profile && v.profile.values) || {};
  const on = name => conv.features && conv.features[name];
  let prof = fields.filter(f => values[f.key]).map(f =>
    `<span class="chip ok" title="${esc(f.title)}${values[f.key].quote ? ' — по словам: «' + esc(values[f.key].quote) + '»' : ''}">${esc(fieldLabel(f.key, values[f.key].value))}</span>`).join('');
  if (!prof) prof = '<span class="hint">анкета пуста — заполнится из разговора или руками</span>';
  if (!on('profile')) prof = '<span class="hint">профиль выключен в этом диалоге</span>';
  const limits = ((v.profile && v.profile.limits) || []).map(l => `<span class="chip warn">не: ${esc(l.text)}</span>`).join('');
  const entries = card => (card && card.entries) || [];
  const long = entries(v.long).map(e => `<div><b>${esc(e.key)}</b>: ${esc(e.value)}</div>`).join('') || '<span class="hint">пусто</span>';
  const work = v.work && v.work.id ? (entries(v.work).map(e => `<div><b>${esc(e.key)}</b>: ${esc(e.value)}</div>`).join('') || '<span class="hint">пусто</span>') : '';
  return `<section class="panel" id="panel-person"><h2>Собеседник «${esc(v.owner)}»
      <button type="button" class="small" data-action="openWindow" data-arg="people">анкета</button></h2>
      <div class="chips">${prof}${limits}</div>
      ${v.error ? `<div class="hint">${esc(v.error)}</div>` : ''}</section>
    <section class="panel" id="panel-memory"><h2>Память <button type="button" class="small" data-action="openWindow" data-arg="memory">целиком</button></h2>
      <div class="hint">долговременная${on('memory.long') ? '' : ' — выключена'} · ${esc(v.paths.long || '')}</div>${long}
      ${work ? `<div class="hint" style="margin-top:4px">рабочая: подборка «${esc(v.work.title || v.work.id)}»</div>${work}` : ''}
      ${conv.facts && conv.facts.entries && conv.facts.entries.length ? `<div class="hint" style="margin-top:4px">карточка фактов ветки</div>` +
        conv.facts.entries.map(e => `<div><b>${esc(e.key)}</b>: ${esc(e.value)}</div>`).join('') : ''}</section>`;
};

const layerTitle = { long: 'долговременная', work: 'рабочая' };
app.chips.memory = changes => (changes || []).map(c => {
  const cls = c.op === 'skip' ? 'warn' : '';
  const what = c.op === 'delete' ? `забыто «${c.key}»` : c.op === 'move' ? `«${c.key}» → ${layerTitle[c.layer]}` :
    c.op === 'skip' ? `не записано «${c.key}»` : `${layerTitle[c.layer] || c.layer}: ${c.key} = ${c.value}`;
  return `<span class="chip ${cls}" title="${esc(c.reason || 'память')}">🧠 ${esc(what)}</span>`;
});
app.chips.profile = changes => (changes || []).map(c => {
  const cls = c.op === 'reject' ? 'bad' : c.op === 'once' ? 'warn' : 'ok';
  const text = c.op === 'once' ? `разово: ${c.label}` : c.op === 'reject' ? `отклонено: ${c.title || c.value}` :
    c.op === 'limit' ? `ограничение: ${c.value}` : `профиль: ${c.title} — ${c.label}`;
  const tip = [c.reason, c.quote ? 'цитата: «' + c.quote + '»' : ''].filter(Boolean).join('\n');
  return `<span class="chip ${cls}" title="${esc(tip)}">👤 ${esc(text)}</span>`;
});
app.chips.facts = changes => (changes || []).map(c =>
  `<span class="chip ${c.op === 'skip' ? 'warn' : ''}" title="${esc(c.reason || 'карточка фактов ветки')}">📌 ${esc(c.op === 'delete' ? 'забыто ' + c.key : c.key + ': ' + (c.value || ''))}</span>`);
app.chips.checks = checks => {
  const def = (checks || []).filter(c => !c.na);
  if (!def.length) return [];
  const ok = def.filter(c => c.ok).length;
  const tip = checks.map(c => `${c.title}: ${c.na ? 'не определить' : c.ok ? 'соблюдено' : 'нарушено'} — ${c.got}`).join('\n');
  return [`<span class="chip ${ok === def.length ? 'ok' : 'bad'}" title="${esc(tip)}">профиль соблюдён ${ok} из ${def.length}</span>`];
};
app.chips.read = names => [`<span class="chip" title="Долговременная память: «уже читал»">📖 уже читал: ${esc((names || []).join(', '))}</span>`];

app.windows.people = {
  title: 'Картотека профилей',
  async render() {
    const out = await api('GET', '/api/people');
    const fields = app.meta.profileFields || [];
    const presets = app.meta.profilePresets || [];
    const current = personView() ? personView().owner : '';
    let html = '<p class="hint">Анкета — не пожелание, а правила: у каждого значения готовая строка промпта. Разовая просьба в разговоре анкету не меняет, повторённая — закрепляется.</p>';
    for (const p of out.people || []) {
      html += `<h3 style="margin:10px 0 4px">${esc(p.title || p.id)} ${p.id === current ? '<span class="chip ok">этот диалог</span>' : ''}
        <span class="hint">${esc(p.paths.profile)}</span></h3><table class="grid"><tr><th>поле</th><th>значение</th></tr>`;
      for (const f of fields) {
        const cur = p.profile.values[f.key] ? p.profile.values[f.key].value : '';
        html += `<tr><td>${esc(f.title)}${f.checked ? ' <span class="hint">(проверяется)</span>' : ''}</td><td>
          <select data-change="setProfileField" data-person="${esc(p.id)}" data-field="${esc(f.key)}">
          <option value="">— не задано —</option>${f.options.map(o => `<option value="${esc(o.value)}"${o.value === cur ? ' selected' : ''}>${esc(o.title)}</option>`).join('')}
          </select></td></tr>`;
      }
      html += `</table><div class="chips" style="margin-top:4px">${(p.profile.limits || []).map(l =>
        `<span class="chip warn">не: ${esc(l.text)} <button type="button" class="link" data-action="unlimit" data-person="${esc(p.id)}" data-arg="${esc(l.text)}">×</button></span>`).join('')}
        <button type="button" class="small" data-action="addLimit" data-arg="${esc(p.id)}">+ ограничение</button>
        ${presets.map(pr => `<button type="button" class="small" data-action="applyPreset" data-person="${esc(p.id)}" data-arg="${esc(pr.id)}">заготовка «${esc(pr.title)}»</button>`).join('')}
        </div>`;
    }
    if (!(out.people || []).length) html += '<p class="hint">Пока никого нет.</p>';
    return html;
  },
};

app.windows.memory = {
  title: 'Память целиком',
  async render() {
    const out = await api('GET', '/api/memory');
    const table = (card, layer) => `<h3 style="margin:10px 0 4px">${esc(layerTitle[layer])}: ${esc(card.title || card.id)} <span class="hint">версия ${card.version}</span></h3>
      <table class="grid"><tr><th>ключ</th><th>значение</th><th>ход</th><th>откуда</th><th></th></tr>${card.entries.map(e =>
        `<tr><td>${esc(e.key)}</td><td>${esc(e.value)}</td><td>${e.turn}</td><td>${esc(e.source || '')}</td>
         <td><button type="button" class="small" data-action="forgetMemory" data-layer="${layer}" data-id="${esc(card.id)}" data-arg="${esc(e.key)}">забыть</button></td></tr>`).join('')}</table>
      <button type="button" class="small" data-action="putMemory" data-layer="${layer}" data-arg="${esc(card.id)}">+ запись</button>`;
    let html = '<p class="hint">Три слоя по адресам: краткосрочная — в файле диалога, рабочая — по подборке, долговременная — по человеку. Один ключ живёт в одном слое.</p>';
    for (const c of out.long || []) html += table(c, 'long');
    for (const c of out.work || []) html += table(c, 'work');
    if (!(out.long || []).length && !(out.work || []).length) html += '<p class="hint">Память пуста.</p>';
    return html;
  },
};

async function afterWindowEdit(name) {
  try { $('window-body').innerHTML = await app.windows[name].render(); } catch (e) { toast(e.message, true); }
  if (app.conv) loadConv(app.conv.id, true);
}

Object.assign(actions, {
  async setProfileField(value, el) {
    try {
      await api('POST', `/api/people/${el.dataset.person}/profile`, value ? { op: 'set', field: el.dataset.field, value } : { op: 'clear', field: el.dataset.field });
      toast('Анкета записана');
      await afterWindowEdit('people');
    } catch (e) { toast(e.message, true); }
  },
  async addLimit(person) {
    const text = prompt('Чего справочнику не делать никогда (например, «не рассказывай про охоту»)?');
    if (!text) return;
    try { await api('POST', `/api/people/${person}/profile`, { op: 'limit', text }); await afterWindowEdit('people'); } catch (e) { toast(e.message, true); }
  },
  async unlimit(text, el) {
    try { await api('POST', `/api/people/${el.dataset.person}/profile`, { op: 'unlimit', text }); await afterWindowEdit('people'); } catch (e) { toast(e.message, true); }
  },
  async applyPreset(preset, el) {
    try { await api('POST', `/api/people/${el.dataset.person}/profile`, { op: 'preset', preset }); await afterWindowEdit('people'); } catch (e) { toast(e.message, true); }
  },
  async forgetMemory(key, el) {
    try { await api('POST', `/api/memory/${el.dataset.layer}/${el.dataset.id}`, { op: 'forget', key }); await afterWindowEdit('memory'); } catch (e) { toast(e.message, true); }
  },
  async putMemory(id, el) {
    const key = prompt('Ключ записи:');
    if (!key) return;
    const value = prompt('Значение:');
    if (!value) return;
    try { await api('POST', `/api/memory/${el.dataset.layer}/${id}`, { op: 'put', key, value }); await afterWindowEdit('memory'); } catch (e) { toast(e.message, true); }
  },
  async bookmark(name) {
    const p = personView();
    if (!p || !p.owner) { toast('У диалога нет собеседника'); return; }
    try { await api('POST', `/api/people/${p.owner}/bookmark`, { name }); toast('В закладках: ' + name); loadConv(app.conv.id, true); } catch (e) { toast(e.message, true); }
  },
});

/* ---------- подборка: этапы, виды, права этапа ---------- */

const stageTitle = { planning: 'план', collecting: 'сбор', validation: 'сверка', done: 'принята' };
const stageOrder = ['planning', 'collecting', 'validation', 'done'];
const itemMark = { pending: '○', done: '●', rework: '↺' };

function stagesHTML(st) {
  const at = stageOrder.indexOf(st.stage);
  return `<div class="stages">${stageOrder.map((s, i) =>
    `<span class="stage-step ${i < at ? 'past' : i === at ? 'now' : ''}">${esc(stageTitle[s])}</span>`).join('<span class="stage-arrow">→</span>')}
    ${st.paused ? '<span class="chip warn">на паузе</span>' : ''}</div>`;
}

app.panels.collection = v => {
  const st = v.state || {};
  const items = (st.items || []).map(it =>
    `<div class="item ${it.n === st.current && st.stage === 'collecting' ? 'current' : ''}" title="${esc(it.result || '')}">
      ${itemMark[it.status] || '○'} ${it.n}. ${esc(it.name)}${it.output ? ` <button type="button" class="link" data-action="open" data-arg="${esc(it.output.card.name || it.name)}">карточка</button>` : ''}</div>`).join('');
  const g = v.grant || {};
  const locked = (g.locked || []).map(l =>
    `<span class="chip warn" title="${esc(l.why)}${l.when ? '\nСтанет можно: ' + esc(l.when) : ''}">🔒 ${esc(l.tool)}</span>`).join('');
  const tools = (g.tools || []).map(t => `<span class="chip ok">${esc(t)}</span>`).join('');
  const report = st.report ? `<div class="hint">сверка: ${esc(st.report.summary || '')} — ${st.report.checks.filter(c => c.ok).length} из ${st.report.checks.length} сошлось</div>` : '';
  return `<section class="panel wide" id="panel-collection"><h2>Подборка «${esc(st.title || '')}»
      <button type="button" class="small" data-action="openWindow" data-arg="collections">все подборки</button>
      <a class="small" href="/api/collections/${esc(st.id)}/export">выгрузить</a></h2>
    ${v.enabled ? '' : '<div class="hint">состояние подборки выключено в этом диалоге — модель его не видит</div>'}
    ${stagesHTML(st)}
    <div class="hint">ждём: ${esc((v.expected || {}).text || '')}${st.goal ? ' · цель: ' + esc(st.goal) : ''}</div>
    <div class="items">${items || '<span class="hint">плана ещё нет</span>'}</div>${report}
    <div class="chips" title="${v.gated ? 'Права этапа: модели даны только эти инструменты' : 'Права этапа выключены: все инструменты сразу, правила — словами'}">
      ${v.gated ? '' : '<span class="chip bad">права этапа выключены</span>'}${tools}${locked}</div>
    <div class="hint">${esc(v.path || '')}${v.error ? ' · ' + esc(v.error) : ''}</div></section>`;
};

app.chips.collection = r => {
  const out = [];
  for (const c of r.changes || []) {
    const text = c.rejected ? `отклонено: ${c.event} — ${c.reason}` :
      c.from.stage === c.to.stage && !!c.from.paused === !!c.to.paused ? `подборка: ${c.event}` :
      `подборка: ${stageTitle[c.from.stage]}${c.from.paused ? ' (пауза)' : ''} → ${stageTitle[c.to.stage]}${c.to.paused ? ' (пауза)' : ''}`;
    out.push(`<span class="chip ${c.rejected ? 'warn' : 'ok'}" title="${esc([c.note, c.quote ? 'цитата: «' + c.quote + '»' : ''].filter(Boolean).join('\n'))}">📋 ${esc(text)}</span>`);
  }
  for (const w of r.works || []) out.push(`<span class="chip">📋 ${esc(w)}</span>`);
  for (const d of r.denials || []) {
    out.push(`<span class="chip bad" title="${esc(`Почему: ${d.reason}\nДоступно: ${d.available || '—'}\nЧто сделать: ${d.hint || '—'}`)}">🔒 нельзя: ${esc(d.what)}</span>`);
  }
  return out;
};

app.windows.collections = {
  title: 'Подборки',
  async render() {
    const out = await api('GET', '/api/collections');
    const list = out.collections || [];
    let html = '<p class="hint">Подборка живёт в своём файле, а не в диалоге: её можно продолжить в любом диалоге — этап, виды и отказы сохранятся.</p>';
    if (list.length) {
      html += `<table class="grid"><tr><th>подборка</th><th>этап</th><th>видов</th><th>ждём</th><th></th></tr>${list.map(c =>
        `<tr><td>${esc(c.title)}<div class="hint">${esc(c.path)}</div></td><td>${esc(stageTitle[c.stage] || c.stage)}${c.paused ? ' · пауза' : ''}</td>
         <td>${c.done} из ${c.items}</td><td>${esc(c.expected.text)}</td>
         <td><button type="button" class="small" data-action="continueCollection" data-arg="${esc(c.id)}"${app.conv ? '' : ' disabled'}>продолжить здесь</button>
             <a class="small" href="/api/collections/${esc(c.id)}/export">выгрузить</a>
             <button type="button" class="small" data-action="collectionFile" data-arg="${esc(c.id)}">файл</button></td></tr>`).join('')}</table>`;
    } else {
      html += '<p class="hint">Подборок нет. Начните с реплики «Собери подборку: хищники тайги, пять видов».</p>';
    }
    for (const p of out.problems || []) html += `<p class="hint">⚠ ${esc(p)}</p>`;
    return html;
  },
};

Object.assign(actions, {
  async continueCollection(id) {
    if (!app.conv) return;
    try {
      await api('POST', `/api/collections/${id}/continue`, { conversation: app.conv.id });
      actions.closeWindow();
      toast('Подборка подключена к диалогу');
      loadConv(app.conv.id, true);
    } catch (e) { toast(e.message, true); }
  },
  async collectionFile(id) {
    try {
      const f = await api('GET', '/api/collections/' + id);
      $('window-body').innerHTML = `<p class="hint">${esc(f.path)}</p><pre>${esc(f.json)}</pre>`;
    } catch (e) { toast(e.message, true); }
  },
});

/* ---------- MCP-сервер: окно и статус на пульте ---------- */

// Состояние клиента MCP живёт на сервере приложения (/api/mcp). Пульт
// показывает его значком, окно — инструменты со схемами и счётчики. Окно и
// значок сервер не поднимают: процесс запускается только ходом с mcp.
const mcpStatusText = {
  off: 'не запускался',
  ready: 'готов',
  dead: 'упал — поднимется следующим вызовом',
  drift: 'набор инструментов расходится с локальным',
  unavailable: 'отложен после частых падений',
};
const mcpStatusClass = { off: '', ready: 'ok', dead: 'warn', drift: 'bad', unavailable: 'bad' };

function mcpOn() {
  const c = app.conv;
  return !!(c && (c.mechanisms || []).some(m => m.name === 'mcp' && m.on));
}

async function refreshMCP() {
  try { app.mcp = await api('GET', '/api/mcp'); } catch (e) { app.mcp = null; }
  renderMCPPill();
}

function renderMCPPill() {
  let pill = $('mcp-pill');
  if (!pill) {
    const row = document.querySelector('.pult-row');
    if (!row) return;
    pill = document.createElement('button');
    pill.type = 'button';
    pill.id = 'mcp-pill';
    pill.dataset.action = 'openWindow';
    pill.dataset.arg = 'mcp';
    pill.style.cursor = 'pointer';
    row.appendChild(pill);
  }
  const v = app.mcp;
  const st = v ? v.status : 'off';
  pill.className = 'chip ' + (mcpStatusClass[st] || '');
  pill.textContent = `MCP: ${st}${mcpOn() ? '' : ' · в диалоге выключен'}`;
  const lines = [`MCP-сервер источников: ${mcpStatusText[st] || st}`];
  if (v && v.conn) lines.push(`${v.conn.server} ${v.conn.version}, протокол ${v.conn.protocol}${v.conn.pid ? ', pid ' + v.conn.pid : ''}`);
  if (v && v.reason) lines.push('причина: ' + v.reason);
  lines.push('Выключатель — «MCP-сервер источников» в механизмах диалога. Подробности — по клику.');
  pill.title = lines.join('\n');
}

function mcpSchemaHTML(schema) {
  let pretty = '';
  try { pretty = JSON.stringify(typeof schema === 'string' ? JSON.parse(schema) : schema, null, 2); } catch (e) { pretty = String(schema); }
  return `<pre>${esc(pretty)}</pre>`;
}

app.windows.mcp = {
  title: 'MCP-сервер',
  async render() {
    const v = await api('GET', '/api/mcp?server=1');
    app.mcp = v;
    renderMCPPill();
    const st = v.status;
    let html = `<p><span class="chip ${mcpStatusClass[st] || ''}">${esc(st)}</span> ${esc(mcpStatusText[st] || '')}
      ${v.reason ? `<div class="hint">причина: ${esc(v.reason)}</div>` : ''}</p>`;
    html += `<p class="hint">Инструменты источников идут через отдельный процесс по протоколу MCP (stdio): initialize → tools/list → tools/call.
      Описания сверяются с локальными побайтно — модель не видит разницы. Механизм в этом диалоге ${mcpOn() ? '<b>включён</b>' : '<b>выключен</b> — ходы идут в процессе приложения'}.</p>`;
    const rows = [];
    if (v.conn) {
      rows.push(['сервер', `${v.conn.server} ${v.conn.version}`], ['протокол', v.conn.protocol],
        ['процесс', v.conn.pid ? 'pid ' + v.conn.pid : '—'], ['подключение', `№${v.conn.n} с ${when(v.conn.since)}`]);
    }
    if (v.binary) rows.push(['бинарник', v.binary]);
    if (v.fingerprint || v.want) rows.push(['отпечаток', `${v.fingerprint || '—'} (локальный ${v.want || '—'})${v.fingerprint && v.fingerprint === v.want ? ' ✓' : ''}`]);
    rows.push(['перезапуски за минуту', String(v.restarts || 0)]);
    if (v.until) rows.push(['отложен до', when(v.until)]);
    if (v.skipped && v.skipped.length) rows.push(['пропущены', v.skipped.join(', ') + ' — не инструменты источников, модели не выдаются']);
    if (v.server) {
      rows.push(['работает', plural(v.server.uptime_seconds, 'секунду', 'секунды', 'секунд')],
        ['вызовов на сервере', String(v.server.total_calls)]);
      for (const s of v.server.sources || []) rows.push([s.name, s.base_url || 'адрес по умолчанию']);
    } else if (v.serverError) {
      rows.push(['server_info', v.serverError]);
    }
    html += `<table class="grid">${rows.map(([k, x]) => `<tr><th>${esc(k)}</th><td>${esc(x)}</td></tr>`).join('')}</table>`;
    if (!v.tools || !v.tools.length) {
      html += '<p class="hint">Список инструментов появится после первого хода с включённым MCP: сервер запускается лениво.</p>';
      return html;
    }
    const srvCalls = (v.server && v.server.calls) || {};
    const srvErrs = (v.server && v.server.errors) || {};
    html += `<h3>Инструменты (tools/list)</h3><table class="grid"><tr><th>инструмент</th><th>вызовов</th><th>ошибок</th><th>в среднем</th><th>на сервере</th></tr>${v.tools.map(t =>
      `<tr><td><b>${esc(t.name)}</b></td><td>${t.calls}</td><td>${t.errors}</td>
        <td>${t.calls ? Math.round(t.millis / t.calls) + ' мс' : '—'}</td>
        <td>${v.server ? (srvCalls[t.name] || 0) + (srvErrs[t.name] ? ` (ошибок ${srvErrs[t.name]})` : '') : '—'}</td></tr>
       <tr><td colspan="5"><details><summary class="hint">${esc(t.description)}</summary>${mcpSchemaHTML(t.inputSchema)}</details></td></tr>`).join('')}</table>`;
    html += '<p class="hint">«вызовов» — со стороны приложения за его жизнь; «на сервере» — счётчик текущего процесса сервера (server_info), после перезапуска он начинается с нуля.</p>';
    html += '<p><button type="button" class="small" data-action="openWindow" data-arg="mcp">обновить</button></p>';
    return html;
  },
};

/* ---------- вёрстка: высота пульта ---------- */

// Журнал липнет под пультом, а высота пульта меняется: панели механизмов
// появляются и пропадают, строка показаний переносится. Жёсткий отступ
// прятал верх журнала под пульт — меряем пульт и отдаём высоту в CSS.
function trackPultHeight() {
  const pult = $('pult');
  const apply = () => document.documentElement.style.setProperty('--pult-h', Math.ceil(pult.getBoundingClientRect().height) + 'px');
  apply();
  if (window.ResizeObserver) new ResizeObserver(apply).observe(pult);
  else window.addEventListener('resize', apply);
}
trackPultHeight();

/* ---------- свод и страж ---------- */

const ruleKindTitle = { sources: 'достоверность', safety: 'советы и безопасность', tone: 'тон' };
const actionTitle = { add: 'добавить', amend: 'изменить', retire: 'снять' };

function ruleTip(inv) {
  return [inv.rule, inv.because ? 'Почему: ' + inv.because : '', inv.instead ? 'Вместо: ' + inv.instead : '',
    (inv.except || []).length ? 'Не нарушает: ' + inv.except.join('; ') : ''].filter(Boolean).join('\n');
}

app.panels.charter = v => {
  const items = (v.items || []).map(inv => inv.status === 'retired'
    ? `<span class="chip rule-retired" title="${esc('Снято. ' + ruleTip(inv))}">${esc(inv.id)} ${esc(inv.title)}</span>`
    : `<span class="chip ok" title="${esc(ruleTip(inv))}">${esc(inv.id)} ${esc(inv.title)}</span>`).join('');
  const pending = (v.pending || []).map(a =>
    `<span class="chip warn" title="${esc('Основание: ' + a.reason + (a.cost ? '\nЦена: ' + a.cost : '') + '\nПринимает человек своими словами, не раньше следующего хода.')}">⏳ ${esc(actionTitle[a.action] || a.action)}: ${esc(a.proposed.title || a.item_id)}</span>`).join('');
  const flags = [];
  if (!v.charter) flags.push('<span class="chip bad" title="Те же правила уходят абзацем системного промпта: без сверки, стража и процедуры">свод выключен — правила словами</span>');
  if (!v.guard) flags.push('<span class="chip bad" title="Ответ уходит человеку без проверки по своду">страж выключен</span>');
  return `<section class="panel wide" id="panel-charter"><h2>Свод «${esc(v.title || '')}» <span class="hint">редакция ${v.version}</span>
      <button type="button" class="small" data-action="openWindow" data-arg="charter">свод</button></h2>
    <div class="chips">${flags.join('')}${items}${pending}</div>
    <div class="hint">${esc(v.path || '')}${v.error ? ' · ' + esc(v.error) : ''}</div></section>`;
};

app.chips.charter = r => {
  const out = [];
  if (!r.enabled) out.push('<span class="chip warn" title="Механизм charter выключен: правила ушли абзацем системного промпта">📜 свод выключен — правила словами</span>');
  for (const c of r.checks || []) {
    out.push(`<span class="chip ${(c.suspect || []).length ? 'warn' : ''}" title="${esc('Сверка: ' + c.answer)}">📜 сверка${(c.suspect || []).length ? ': похоже на ' + esc(c.suspect.join(', ')) : ' — чисто'}</span>`);
  }
  for (const a of r.amendments || []) {
    const text = a.rejected ? `${a.event} не принято — ${a.reason}` : `${a.summary}`;
    const tip = [a.amendment ? 'поправка ' + a.amendment : '', a.quote ? 'цитата: «' + a.quote + '»' : '', 'редакция ' + a.version].filter(Boolean).join('\n');
    out.push(`<span class="chip ${a.rejected ? 'warn' : 'ok'}" title="${esc(tip)}">📜 ${esc(text)}</span>`);
  }
  const g = r.guard || {};
  const rev = g.review || {};
  switch (g.status) {
    case 'refused': {
      const why = (rev.broken || []).map(v => `${v.invariant}: ${v.why || ''}${v.fragment ? ' — «' + v.fragment + '»' : ''}`).join('\n');
      out.push(`<span class="chip bad" title="${esc(why + '\n\nНе дошло до человека:\n' + (g.original || ''))}">🛡 страж: нарушен ${esc((rev.broken || []).map(v => v.invariant).join(', '))} — ответ заменён</span>`);
      break;
    }
    case 'passed':
      out.push(`<span class="chip ok" title="${esc((rev.screened || []).map(h => `${h.invariant} «${h.marker}»: ${h.fragment}${h.cleared ? ' — снято: ' + h.why : ''}`).join('\n'))}">🛡 страж: проверено, нарушений нет</span>`);
      break;
    case 'unchecked':
      out.push(`<span class="chip warn" title="${esc(rev.error || '')}">🛡 страж: ответ не проверен</span>`);
      break;
    case 'off':
      out.push('<span class="chip warn" title="Механизм guard выключен">🛡 страж выключен</span>');
      break;
  }
  return out;
};

app.windows.charter = {
  title: 'Свод',
  async render() {
    const out = await api('GET', '/api/charter');
    const c = out.charter || {};
    let html = `<p class="hint">Правила справочника, которые просьба не отменяет. Изменить свод можно только поправкой в разговоре: справочник предлагает, человек принимает своими словами и не раньше следующего хода. ${esc(out.summary || '')}.</p>
      <table class="grid"><tr><th>№</th><th>вид</th><th>правило</th><th>почему · вместо</th><th>статус</th></tr>${(c.items || []).map(inv =>
        `<tr class="${inv.status === 'retired' ? 'rule-retired' : ''}"><td>${esc(inv.id)}</td><td>${esc(ruleKindTitle[inv.kind] || inv.kind)}</td>
         <td><b>${esc(inv.title)}</b><div>${esc(inv.rule)}</div>${(inv.except || []).length ? `<div class="hint">не нарушает: ${esc(inv.except.join('; '))}</div>` : ''}</td>
         <td>${esc(inv.because || '')}${inv.instead ? `<div class="hint">вместо: ${esc(inv.instead)}</div>` : ''}</td>
         <td>${inv.status === 'retired' ? 'снято' : 'действует'}</td></tr>`).join('')}</table>`;
    if ((c.pending || []).length) {
      html += `<h3 style="margin:10px 0 4px">Открытые поправки</h3><table class="grid"><tr><th>поправка</th><th>что</th><th>основание</th><th>цена</th></tr>${c.pending.map(a =>
        `<tr><td>${esc(a.id)}</td><td>${esc(actionTitle[a.action] || a.action)}: ${esc(a.proposed.title || a.item_id)}${a.action !== 'retire' ? `<div class="hint">${esc(a.proposed.rule || '')}</div>` : ''}</td>
         <td>${esc(a.reason)}</td><td>${esc(a.cost || '')}</td></tr>`).join('')}</table>`;
    }
    if ((c.log || []).length) {
      html += `<h3 style="margin:10px 0 4px">Журнал изменений</h3><table class="grid"><tr><th>когда</th><th>ред.</th><th>что</th><th>слова человека</th></tr>${c.log.slice().reverse().map(l =>
        `<tr><td>${esc(when(l.at))}</td><td>${l.version}</td><td>${esc(actionTitle[l.action] || l.action)}: ${esc(l.title)}<div class="hint">${esc(l.summary)}</div></td><td>${l.quote ? '«' + esc(l.quote) + '»' : ''}</td></tr>`).join('')}</table>`;
    }
    html += `<details><summary>Блок свода в запросе</summary><pre>${esc(out.block)}</pre></details>
      <details><summary>Абзац правил при выключенном своде</summary><pre>${esc(out.plain)}</pre></details>
      <details><summary>Файл ${esc(out.path)}</summary><pre>${esc(out.json || 'файла ещё нет — действует заготовка')}</pre></details>`;
    return html;
  },
};

refreshMCP();
setInterval(() => { if (document.visibilityState === 'visible') refreshMCP(); }, 5000);

// ===== Интересные факты =====

/* Раздел «Интересные факты»: выпуски и сводки, которые собирает отдельный
   демон (раз в час — выпуск о случайном млекопитающем, раз в сутки —
   сводка). Приложение только пересылает REST /api/facts/* к демону; демона
   может не быть — тогда раздел показывает заглушку с подсказкой, а не
   ошибки. Тексты выпусков пересказывают внешние источники: всё из ответов
   идёт через esc(), ссылки — только http/https.

   Код раздела живёт одним куском здесь; в общих функциях его нет — кнопка
   на пульте и окно регистрируются отсюда же. */

const facts = {
  status: null,        // /api/facts/status (Status) или {conn, reason, hint} при сбое
  tab: 'feed',         // feed | summaries
  latest: null,        // {ok, code, data} ответа /api/facts/latest
  query: '',           // строка поиска
  search: null,        // {ok, code, data} ответа /api/facts/search
  detail: null,        // {id, res} — открытый выпуск
  summary: null,       // {id, res} — открытая сводка (id 0 — последняя)
  summaries: null,     // {ok, code, data} списка сводок
  busy: '',            // issue | summary — идёт платный запуск
  busySince: 0,
  result: null,        // {kind, html} — итог последнего запуска
  timer: null,         // опрос статуса, пока окно открыто
  tick: null,          // счётчик секунд ожидания
};
app.facts = facts;

const factsPollMs = 30000;
const factsCostNote = 'платно: около $0.002 за запрос к модели, расход пойдёт в дневной лимит демона';
const factsConnText = { ok: 'подключён', down: 'не отвечает', denied: 'отверг токен', off: 'выключен', unknown: 'проверяю…', none: 'нет в этом сервере' };
const factsConnClass = { ok: 'ok', down: 'bad', denied: 'bad', off: 'warn', unknown: '', none: 'warn' };
const factsIUCN = {
  LC: ['вызывает наименьшие опасения', 'ok'], NT: ['близок к уязвимому', 'warn'], VU: ['уязвимый', 'warn'],
  EN: ['вымирающий', 'bad'], CR: ['на грани исчезновения', 'bad'], EW: ['исчез в дикой природе', 'bad'],
  EX: ['исчез', 'bad'], DD: ['данных недостаточно', ''], NE: ['не оценён', ''],
};
const factsIssueStatus = { ok: ['', ''], thin: ['мало фактов', 'warn'], failed: ['не собрался', 'bad'] };
const factsRunStatus = { ok: 'готово', failed: 'сбой', budget: 'лимит расходов', skipped: 'пропущено' };
const factsOutOfRangeNote = 'Страны, где GBIF видел вид, а MDD не числит в ареале. Чаще это зоопарки, интродукция или ошибки определения, а не новый ареал.';
const factsDaemonHint = 'Запустите демон в отдельном окне: animals-mcp -http 127.0.0.1:8766 (токен — тот же MCP_TOKEN, что у приложения), затем нажмите «обновить».';

// factsAPI — запрос к /api/facts/* без исключений: {ok, code, data, error}.
// Код 0 — сеть или сервер приложения не ответили.
async function factsAPI(method, path, body) {
  const opts = { method, headers: {} };
  if (body !== undefined) {
    opts.headers['Content-Type'] = 'application/json';
    opts.body = JSON.stringify(body);
  }
  try {
    const res = await fetch(path, opts);
    const text = await res.text();
    let data = null;
    try { data = text ? JSON.parse(text) : {}; } catch (e) { data = { error: text }; }
    const error = res.ok ? '' : ((data && data.error) || ('HTTP ' + res.status));
    return { ok: res.ok, code: res.status, data: data || {}, error };
  } catch (e) {
    return { ok: false, code: 0, data: {}, error: 'сервер приложения не отвечает: ' + e.message };
  }
}

// factsURL — только http/https; остальное (javascript:, data:, мусор) — ''.
function factsURL(u) {
  try {
    const x = new URL(String(u || ''));
    return x.protocol === 'http:' || x.protocol === 'https:' ? x.href : '';
  } catch (e) { return ''; }
}
function factsUSD(v) { return typeof v === 'number' && isFinite(v) ? '$' + v.toFixed(4) : '—'; }
function factsList(v) { return Array.isArray(v) ? v : []; }
function factsJob(name) {
  const s = facts.status && facts.status.schedule;
  return s ? factsList(s.jobs).find(j => j.name === name) : null;
}
const factsConnected = () => !!(facts.status && facts.status.conn === 'ok');

async function factsLoadStatus() {
  const r = await factsAPI('GET', '/api/facts/status');
  if (r.ok) facts.status = r.data;
  else if (r.code === 404) facts.status = { conn: 'none', reason: 'сервер приложения не знает /api/facts', hint: 'обновите приложение' };
  else facts.status = { conn: 'down', reason: r.error, hint: '' };
  factsRenderButton();
}
async function factsLoadLatest() { facts.latest = await factsAPI('GET', '/api/facts/latest?limit=10'); }
async function factsLoadSummaries() {
  const [one, list] = await Promise.all([
    factsAPI('GET', '/api/facts/summary' + (facts.summary && facts.summary.id ? '?id=' + encodeURIComponent(facts.summary.id) : '')),
    factsAPI('GET', '/api/facts/summaries'),
  ]);
  facts.summary = { id: facts.summary ? facts.summary.id : 0, res: one };
  facts.summaries = list;
}

/* ---------- кнопка на пульте ---------- */

function factsRenderButton() {
  let btn = $('facts-button');
  if (!btn) {
    const anchor = $('windows-button');
    if (!anchor) return;
    btn = document.createElement('button');
    btn.type = 'button';
    btn.id = 'facts-button';
    btn.className = 'ghost facts-button';
    btn.dataset.action = 'openWindow';
    btn.dataset.arg = 'facts';
    anchor.insertAdjacentElement('afterend', btn);
  }
  const conn = facts.status ? facts.status.conn : 'unknown';
  btn.innerHTML = `<span class="facts-dot ${esc(factsConnClass[conn] || '')}"></span>Факты`;
  const lines = ['«Интересные факты»: выпуски и сводки демона', 'демон: ' + (factsConnText[conn] || conn)];
  if (facts.status && facts.status.reason) lines.push('причина: ' + facts.status.reason);
  btn.title = lines.join('\n');
}

/* ---------- окно ---------- */

app.windows.facts = {
  title: 'Интересные факты',
  async render() {
    await factsLoadStatus();
    await Promise.all([factsLoadLatest(), facts.tab === 'summaries' ? factsLoadSummaries() : null,
      facts.tab === 'pipeline' ? pipeLoadRuns() : null]);
    factsStartPoll();
    if (facts.tab === 'pipeline') setTimeout(pipeEnter, 0); // опрос идущего прогона — после отрисовки окна
    return `<div id="facts-root" class="facts">
      <div id="facts-status" class="facts-status">${factsStatusHTML()}</div>
      <div id="facts-actions" class="facts-actions">${factsActionsHTML()}</div>
      <div class="tabs facts-tabs" id="facts-tabs">${factsTabsHTML()}</div>
      <div id="facts-body" class="facts-body">${factsBodyHTML()}</div>
    </div>`;
  },
};

function factsStartPoll() {
  factsStopPoll();
  facts.timer = setInterval(async () => {
    if (!factsOpen()) { factsStopPoll(); return; }
    const was = facts.status && facts.status.conn;
    await factsLoadStatus();
    if (!factsOpen()) return;
    factsPaint('status');
    factsPaint('actions');
    // Демон поднялся, пока окно открыто: подтягиваем ленту без клика.
    if (was !== 'ok' && factsConnected()) {
      await factsLoadLatest();
      if (facts.tab === 'summaries') await factsLoadSummaries();
      factsPaint('body');
    }
  }, factsPollMs);
}
function factsStopPoll() {
  if (facts.timer) clearInterval(facts.timer);
  facts.timer = null;
}
function factsOpen() { return !!($('facts-root') && $('window').open); }
// Окно закрыто — опрос не нужен. Событие close приходит не везде (headless
// Edge его не шлёт), поэтому следим и за атрибутом open.
$('window').addEventListener('close', factsStopPoll);
if (window.MutationObserver) {
  new MutationObserver(() => { if (!$('window').open) factsStopPoll(); }).observe($('window'), { attributes: true, attributeFilter: ['open'] });
}

// factsPaint — перерисовать часть окна, если оно открыто.
function factsPaint(...parts) {
  if (!$('facts-root')) return;
  const fn = { status: factsStatusHTML, actions: factsActionsHTML, tabs: factsTabsHTML, body: factsBodyHTML };
  for (const p of parts) {
    const el = $('facts-' + p);
    if (el) el.innerHTML = fn[p]();
  }
}

function factsStatusHTML() {
  const s = facts.status || { conn: 'unknown' };
  const conn = s.conn || 'unknown';
  let html = `<div class="facts-status-row"><span class="chip ${esc(factsConnClass[conn] || '')}">демон ${esc(factsConnText[conn] || conn)}</span>`;
  if (s.server) html += `<span class="hint">${esc(s.server)}${s.version ? ' · ' + esc(s.version) : ''}</span>`;
  if (conn === 'ok') {
    const issue = factsJob('issue'), summary = factsJob('summary');
    if (issue) html += `<span><span class="lbl">следующий выпуск</span>${esc(issue.running ? 'идёт сейчас' : (issue.next_text || '—'))}</span>`;
    if (summary) html += `<span><span class="lbl">сводка</span>${esc(summary.running ? 'идёт сейчас' : (summary.next_text || '—'))}</span>`;
    const budget = s.schedule && s.schedule.budget_text;
    if (budget) html += `<span><span class="lbl">бюджет</span>${esc(budget)}</span>`;
  }
  html += `<button type="button" class="small" data-action="factsRefresh" title="Перечитать состояние, ленту и сводки">обновить</button></div>`;
  if (conn === 'ok') {
    const issue = factsJob('issue');
    if (issue && issue.last_text) html += `<div class="hint">последний выпуск: ${esc(issue.last_text)}</div>`;
  } else {
    if (s.reason) html += `<div class="hint">причина: ${esc(s.reason)}</div>`;
    html += `<div class="facts-hint">${esc(s.hint || (conn === 'unknown' ? '' : factsDaemonHint))}</div>`;
  }
  return html;
}

function factsActionsHTML() {
  const off = !factsConnected();
  const busy = !!facts.busy;
  const why = off ? ' title="Демон не подключён"' : busy ? ' title="Уже идёт запуск — дождитесь итога"' : '';
  let html = `<button type="button" class="solid" id="facts-run-issue" data-action="factsRun" data-arg="issue"${off || busy ? ' disabled' : ''}${why}>Собрать выпуск сейчас</button>
    <button type="button" id="facts-run-summary" data-action="factsRun" data-arg="summary"${off || busy ? ' disabled' : ''}${why}>Собрать сводку</button>
    <span class="hint">${esc(factsCostNote)}</span>`;
  if (busy) {
    const sec = Math.max(0, Math.round((Date.now() - facts.busySince) / 1000));
    html += `<div class="facts-wait" id="facts-wait"><span class="thinking">${facts.busy === 'issue' ? 'демон собирает выпуск — это секунды, иногда до пары минут' : 'демон пишет сводку'}</span> <span class="hint">${sec} с</span></div>`;
  } else if (facts.result) {
    html += `<div class="facts-result ${esc(facts.result.kind)}" id="facts-result">${facts.result.html}</div>`;
  }
  return html;
}

function factsTabsHTML() {
  return `<button type="button" class="tab${facts.tab === 'feed' ? ' active' : ''}" data-action="factsTab" data-arg="feed" id="facts-tab-feed">Лента</button>
    <button type="button" class="tab${facts.tab === 'summaries' ? ' active' : ''}" data-action="factsTab" data-arg="summaries" id="facts-tab-summaries">Сводки</button>
    <button type="button" class="tab${facts.tab === 'pipeline' ? ' active' : ''}" data-action="factsTab" data-arg="pipeline" id="facts-tab-pipeline" title="search → summarize → save_to_file: цепочка инструментов демона">Конвейер</button>`;
}

function factsBodyHTML() {
  if (facts.tab === 'pipeline') return pipeHTML(); // блок «Конвейер» в конце файла
  if (facts.tab === 'summaries') return factsSummariesHTML();
  if (facts.detail) return factsDetailHTML();
  return factsFeedHTML();
}

// factsProblem — заглушка вместо данных: 503 (демона нет), 422 (ошибка
// инструмента), прочее. null — ответ в порядке.
function factsProblem(r) {
  if (!r) return '<p class="hint">загружаю…</p>';
  if (r.ok) return null;
  if (r.code === 503 || r.code === 0 || r.code === 404) {
    const s = facts.status || {};
    const hint = s.hint || factsDaemonHint;
    return `<div class="facts-down"><b>Демон «Интересных фактов» не подключён.</b>
      <div>${esc(r.error)}</div>
      ${hint && !String(r.error).includes(hint) ? `<div class="facts-hint">${esc(hint)}</div>` : ''}
      <div class="hint">Выпуски собирает отдельный процесс; справочник работает и без него.</div></div>`;
  }
  return `<div class="facts-error">${esc(r.error)}</div>`;
}

/* ---------- лента и поиск ---------- */

function factsSearchHTML() {
  return `<form class="facts-search" data-submit="factsSearch" id="facts-search">
    <input type="search" id="facts-query" value="${esc(facts.query)}" placeholder="Поиск по выпускам: манул, Panthera, зрачки…" autocomplete="off">
    <button type="submit" class="small">Найти</button>
    ${facts.search ? '<button type="button" class="small" data-action="factsSearchReset">× вся лента</button>' : ''}
  </form>`;
}

function factsFeedHTML() {
  let html = factsSearchHTML();
  if (facts.search) {
    const bad = factsProblem(facts.search);
    if (bad) return html + bad;
    const d = facts.search.data;
    const rows = factsList(d.issues);
    html += `<div class="hint">найдено ${num(d.total)}${d.total > rows.length ? ', показаны ' + num(rows.length) : ''} по «${esc(facts.query)}»</div>`;
    if (!rows.length) return html + `<p class="hint">${esc(d.hint || 'ничего не найдено')}</p>`;
    html += `<table class="grid facts-found"><tr><th>№</th><th>когда</th><th>вид</th><th>заголовок</th><th></th></tr>${rows.map(r => {
      const st = factsIssueStatus[r.status] || [r.status, ''];
      return `<tr class="facts-row" data-action="factsIssue" data-arg="${esc(r.id)}"><td>${esc(r.id)}</td><td>${esc(when(r.created_at))}</td>
        <td>${esc(r.name_ru || '')} <span class="latin">${esc(r.sci_name)}</span></td><td>${esc(r.title || '')}</td>
        <td>${st[0] ? `<span class="chip ${esc(st[1])}">${esc(st[0])}</span>` : ''}</td></tr>`;
    }).join('')}</table>`;
    if (d.hint) html += `<p class="hint">${esc(d.hint)}</p>`;
    return html;
  }
  const bad = factsProblem(facts.latest);
  // Демона нет — искать негде: вместо строки поиска одна заглушка.
  if (bad) return facts.latest && facts.latest.code === 422 ? html + bad : bad;
  const d = facts.latest.data;
  const list = factsList(d.issues);
  if (!list.length) return html + `<p class="hint">${esc(d.hint || 'Выпусков ещё нет.')}</p>`;
  html += `<div class="facts-feed">${list.map(is => factsCardHTML(is, false)).join('')}</div>`;
  if (d.total > list.length) html += `<p class="hint">показаны последние ${num(list.length)} из ${num(d.total)} — остальные через поиск</p>`;
  return html;
}

function factsSourcesHTML(refs) {
  return factsList(refs).map(s => {
    const url = factsURL(s.url);
    const label = esc(s.id || '?');
    const tip = esc((s.title || 'источник') + (url ? '\n' + url : '\nссылки нет'));
    return url
      ? `<a class="facts-src" href="${esc(url)}" target="_blank" rel="noopener" title="${tip}">${label}</a>`
      : `<span class="facts-src none" title="${tip}">${label}</span>`;
  }).join('');
}

function factsIUCNChip(code) {
  if (!code) return '';
  const k = String(code).toUpperCase();
  const t = factsIUCN[k];
  return `<span class="chip ${esc(t ? t[1] : '')}" title="Статус МСОП (IUCN)">МСОП: ${esc(k)}${t ? ' — ' + esc(t[0]) : ''}</span>`;
}

// factsCardHTML — выпуск карточкой; full — в подробном виде (без клика).
function factsCardHTML(is, full) {
  const st = factsIssueStatus[is.status] || [is.status, ''];
  const head = `<div class="facts-card-head">
      <h3>${esc(is.title || is.name_ru || is.sci_name)}</h3>
      <span class="facts-species">${is.name_ru ? esc(is.name_ru) + ' ' : ''}<span class="latin">${esc(is.sci_name)}</span></span>
      ${factsIUCNChip(is.iucn)}${st[0] ? `<span class="chip ${esc(st[1])}">${esc(st[0])}</span>` : ''}
    </div>`;
  let html = `<article class="facts-card${full ? ' full' : ''} facts-${esc(is.status)}"${full ? '' : ` data-action="factsIssue" data-arg="${esc(is.id)}" title="Подробно: отброшенные факты, наблюдения, расход"`} data-issue="${esc(is.id)}">${head}`;
  if (is.lead) html += `<p class="facts-lead">${esc(is.lead)}</p>`;
  if (is.error) html += `<div class="facts-error">${esc(is.error)}</div>`;
  const list = factsList(is.facts);
  if (list.length) {
    html += `<ol class="facts-facts">${list.map(f => `<li>${esc(f.text)} <span class="facts-srcs">${factsSourcesHTML(f.sources)}</span></li>`).join('')}</ol>`;
  }
  const out = factsList(is.out_of_range);
  if (out.length) {
    html += `<div class="facts-range"><span class="chip warn" title="${esc(factsOutOfRangeNote)}">вне ареала MDD: ${esc(out.join(', '))}</span>
      <span class="hint">наблюдения вне ареала — чаще зоопарки, интродукция или ошибки определения</span></div>`;
  }
  html += `<div class="facts-meta"><span>выпуск №${esc(is.id)}</span><span>собран ${esc(when(is.created_at))}</span><span>${esc(factsUSD(is.cost_usd))}</span>
    ${full ? '' : '<span class="facts-more">подробно →</span>'}</div>`;
  return html + '</article>';
}

function factsDetailHTML() {
  const back = `<button type="button" class="small" data-action="factsBack" id="facts-back">← ${facts.search ? 'к поиску' : 'к ленте'}</button>`;
  const r = facts.detail.res;
  const bad = factsProblem(r);
  if (bad) return back + bad;
  const is = r.data;
  let html = back + factsCardHTML(is, true);
  const where = [is.order && 'отряд ' + is.order, is.family && 'семейство ' + is.family,
    factsList(is.realms).length && 'области: ' + factsList(is.realms).join(', ')].filter(Boolean);
  if (where.length) html += `<p class="hint">${esc(where.join(' · '))}</p>`;

  const dropped = factsList(is.dropped);
  html += `<h4 class="facts-h">Отброшенные факты <span class="hint">${num(dropped.length)}</span></h4>`;
  html += dropped.length
    ? `<ul class="facts-dropped">${dropped.map(f => `<li><span class="facts-dropped-text">${esc(f.text)}</span> <span class="facts-srcs">${factsSourcesHTML(f.sources)}</span>
        <div class="facts-reason">причина: ${esc(f.reason || 'не указана')}</div></li>`).join('')}</ul>`
    : '<p class="hint">Проверяющий не отбросил ни одного факта.</p>';

  const o = is.observations || {};
  html += `<h4 class="facts-h">Наблюдения GBIF</h4><p>всего ${num(o.total)}${o.window_days ? `, за последние ${num(o.window_days)} дн. — ${num(o.recent)}` : `, недавних — ${num(o.recent)}`}</p>`;
  const bc = factsList(o.by_country);
  if (bc.length) {
    const rangeTitle = { in: 'в ареале', uncertain: 'ареал под вопросом', out: 'вне ареала MDD', unknown: 'не сопоставлена' };
    html += `<table class="grid facts-countries"><tr><th>страна</th><th>наблюдений</th><th>ареал</th></tr>${bc.map(c =>
      `<tr class="${c.range === 'out' ? 'facts-out' : ''}"><td>${esc(c.name || c.code)}${c.code ? ` <span class="hint">${esc(c.code)}</span>` : ''}</td><td>${num(c.count)}</td><td>${esc(rangeTitle[c.range] || c.range || '')}</td></tr>`).join('')}</table>`;
  }

  const spend = factsList(is.spend);
  html += `<h4 class="facts-h">Расход по шагам${is.took ? ` <span class="hint">сборка ${esc(is.took)}</span>` : ''}</h4>`;
  const stepTitle = { editor: 'редактор', verifier: 'проверяющий' };
  html += spend.length
    ? `<table class="grid facts-spend"><tr><th>шаг</th><th>модель</th><th>запросов</th><th>токенов</th><th>цена</th><th>время</th></tr>${spend.map(s =>
        `<tr><td>${esc(stepTitle[s.step] || s.step)}</td><td>${esc(s.model)}</td><td>${num(s.requests)}</td><td>${num(s.tokens)}</td><td>${esc(factsUSD(s.cost_usd))}</td><td>${esc(s.took || '')}</td></tr>`).join('')}
        <tr class="facts-total"><td colspan="4">итого</td><td>${esc(factsUSD(is.cost_usd))}</td><td></td></tr></table>`
    : '<p class="hint">расхода нет</p>';
  return html;
}

/* ---------- сводки ---------- */

function factsCounts(list, map) {
  const items = factsList(list);
  if (!items.length) return '<span class="hint">—</span>';
  return items.map(c => `<span class="chip">${esc(map ? (map[c.key] || c.key) : c.key)} ${num(c.count)}</span>`).join('');
}

// factsText — текст сводки абзацами, без markdown: пересказ внешнего — не
// разметка.
function factsText(t) {
  return String(t || '').split(/\n{2,}/).map(p => `<p>${esc(p).replace(/\n/g, '<br>')}</p>`).join('');
}

function factsSummaryHTML(sm) {
  const a = sm.aggregate || {};
  const failures = factsList(a.failures);
  const species = factsList(a.species);
  const cost = sm.cost && typeof sm.cost.usd === 'number' ? sm.cost.usd : null;
  let html = `<div class="facts-summary" id="facts-summary" data-summary="${esc(sm.id)}">
    <h3>Сводка №${esc(sm.id)} <span class="hint">${esc(when(sm.from))} — ${esc(when(sm.to))} · написана ${esc(when(sm.created_at))}${sm.trigger ? ' · ' + esc(sm.trigger) : ''}</span></h3>`;
  if (sm.error) html += `<div class="facts-error">текст не написан: ${esc(sm.error)} — цифры агрегата ниже верны</div>`;
  html += `<div class="facts-summary-text">${factsText(sm.text) || '<p class="hint">текста нет</p>'}</div>`;
  const cell = (k, v, sub) => `<div class="facts-fig"><div class="facts-fig-v">${v}</div><div class="facts-fig-k">${esc(k)}</div>${sub ? `<div class="hint">${sub}</div>` : ''}</div>`;
  html += `<div class="facts-figs">
    ${cell('выпусков', num(a.issues), factsList(a.by_status).map(c => esc(c.key) + ' ' + num(c.count)).join(' · '))}
    ${cell('видов', num(species.length))}
    ${cell('фактов', num(a.facts), 'отброшено ' + num(a.dropped) + (a.dropped_share ? ' (' + Math.round(a.dropped_share * 100) + '%)' : ''))}
    ${cell('сбоев', num(failures.length), a.budget_skips ? 'упёрлись в лимит: ' + num(a.budget_skips) : '')}
    ${cell('расход', esc(factsUSD(a.cost_usd)), cost !== null ? 'сводка ' + esc(factsUSD(cost)) : '')}
  </div>`;
  html += `<table class="grid facts-agg">
    <tr><th>отряды</th><td>${factsCounts(a.by_order)}</td></tr>
    <tr><th>статусы МСОП</th><td>${factsCounts(a.by_iucn)}</td></tr>
    <tr><th>области</th><td>${factsCounts(a.by_realm)}</td></tr>
    ${a.picks || a.rejected ? `<tr><th>выбор видов</th><td>${num(a.picks)} выбрано, ${num(a.rejected)} отвергнуто ${factsCounts(a.rejected_by_reason)}</td></tr>` : ''}
    ${species.length ? `<tr><th>виды</th><td>${species.map(s => `<button type="button" class="small" data-action="factsIssue" data-arg="${esc(s.issue_id)}" title="${esc(s.title || '')}">${esc(s.name_ru || s.sci_name)}</button>`).join(' ')}</td></tr>` : ''}
    ${failures.length ? `<tr><th>сбои</th><td>${failures.map(f => `<div>${esc(f)}</div>`).join('')}</td></tr>` : ''}
    ${factsList(a.out_of_range_species).length ? `<tr><th>вне ареала MDD</th><td>${factsList(a.out_of_range_species).map(f => `<div>${esc(f)}</div>`).join('')}<div class="hint">${esc(factsOutOfRangeNote)}</div></td></tr>` : ''}
    ${factsList(a.mdd_release).length ? `<tr><th>релиз MDD</th><td>${factsList(a.mdd_release).map(f => `<div>${esc(f)}</div>`).join('')}</td></tr>` : ''}
  </table></div>`;
  return html;
}

function factsSummariesHTML() {
  let html = '';
  const one = facts.summary && facts.summary.res;
  if (!one) return '<p class="hint">загружаю…</p>';
  // 503 — одна заглушка на вкладку, а не две.
  if (!one.ok && (one.code === 503 || one.code === 0 || one.code === 404)) return factsProblem(one);
  html += one.ok ? factsSummaryHTML(one.data) : `<div class="facts-error">${esc(one.error)}</div>`;
  const list = facts.summaries;
  html += '<h4 class="facts-h">Прошлые сводки</h4>';
  const bad = factsProblem(list);
  if (bad) return html + bad;
  const rows = factsList(list.data.summaries);
  if (!rows.length) return html + `<p class="hint">${esc(list.data.hint || 'сводок ещё не было')}</p>`;
  const cur = one.ok ? one.data.id : null;
  html += `<div class="facts-summaries">${rows.map(s => `<button type="button" class="facts-sum-row${s.id === cur ? ' current' : ''}" data-action="factsSummary" data-arg="${esc(s.id)}">
      <b>№${esc(s.id)}</b> <span class="hint">${esc(when(s.from))} — ${esc(when(s.to))}${s.trigger ? ' · ' + esc(s.trigger) : ''}</span>
      <span class="facts-sum-preview">${esc(s.error ? 'ошибка: ' + s.error : s.text)}</span></button>`).join('')}</div>`;
  return html;
}

/* ---------- запуски ---------- */

function factsRunResultHTML(job, r) {
  if (!r.ok) {
    const kind = r.code === 503 || r.code === 0 ? 'bad' : 'warn';
    return { kind, html: `<b>${job === 'issue' ? 'Выпуск' : 'Сводка'} не собран${job === 'issue' ? '' : 'а'}.</b> ${esc(r.error)}` };
  }
  const d = r.data || {};
  if (job === 'summary') {
    // summary_build отвечает сводкой целиком.
    const cost = d.cost && typeof d.cost.usd === 'number' ? d.cost.usd : null;
    return { kind: d.error ? 'warn' : 'ok', html: `<b>Сводка №${esc(d.id)} собрана</b> за ${esc(when(d.from))} — ${esc(when(d.to))}${cost !== null ? ' · ' + esc(factsUSD(cost)) : ''}${d.error ? `<div>текст не написан: ${esc(d.error)}</div>` : ''}` };
  }
  const run = d.run || {};
  const kind = run.status === 'ok' ? 'ok' : run.status === 'skipped' ? '' : run.status === 'budget' ? 'warn' : 'bad';
  let html = `<b>${esc(factsRunStatus[run.status] || run.status || '?')}</b>`;
  if (run.status === 'budget') html += ' — задание не запускалось: дневной лимит расходов исчерпан';
  if (run.detail) html += ` · ${esc(run.detail)}`;
  if (run.took) html += ` · ${esc(run.took)}`;
  if (run.cost_usd) html += ` · ${esc(factsUSD(run.cost_usd))}`;
  if (run.error && run.error !== run.detail) html += `<div>ошибка: ${esc(run.error)}</div>`;
  if (d.hint) html += `<div class="hint">${esc(d.hint)}</div>`;
  if (d.issue) html += ` <button type="button" class="small" data-action="factsIssue" data-arg="${esc(d.issue.id)}">открыть выпуск №${esc(d.issue.id)}</button>`;
  return { kind, html };
}

async function factsRun(job) {
  if (facts.busy || !factsConnected()) return;
  const what = job === 'issue' ? 'Собрать выпуск о случайном виде сейчас?' : 'Собрать сводку за последние 24 часа сейчас?';
  if (!window.confirm(what + '\n\nЭто ' + factsCostNote + '.')) return;
  facts.busy = job;
  facts.busySince = Date.now();
  facts.result = null;
  factsPaint('actions');
  facts.tick = setInterval(() => {
    const w = $('facts-wait');
    if (w) w.querySelector('.hint').textContent = Math.round((Date.now() - facts.busySince) / 1000) + ' с';
  }, 1000);
  const r = job === 'issue'
    ? await factsAPI('POST', '/api/facts/run', { job: 'issue' })
    : await factsAPI('POST', '/api/facts/summary/build', { hours: 24 });
  clearInterval(facts.tick);
  facts.tick = null;
  facts.busy = '';
  facts.result = factsRunResultHTML(job, r);
  const good = r.ok && (job === 'summary' || (r.data.run && r.data.run.status === 'ok'));
  if (good) {
    if (job === 'issue') {
      facts.search = null;
      facts.detail = null;
      facts.tab = 'feed';
      await factsLoadLatest();
    } else {
      facts.summary = { id: r.data.id || 0, res: null };
      facts.tab = 'summaries';
      await factsLoadSummaries();
    }
  }
  await factsLoadStatus();
  if (!factsOpen()) {
    toast((job === 'issue' ? 'Выпуск: ' : 'Сводка: ') + (good ? 'готово' : 'не собрано — подробности в разделе «Интересные факты»'), !good);
    return;
  }
  factsPaint('status', 'actions', 'tabs', 'body');
}

/* ---------- действия раздела ---------- */

Object.assign(actions, {
  factsRun(job) { factsRun(job); },
  async factsTab(tab) {
    facts.tab = tab === 'summaries' || tab === 'pipeline' ? tab : 'feed';
    factsPaint('tabs', 'body');
    if (facts.tab === 'summaries' && !facts.summaries) { await factsLoadSummaries(); factsPaint('body'); }
    if (facts.tab === 'pipeline') pipeEnter();
  },
  async factsRefresh() {
    await factsLoadStatus();
    await Promise.all([factsLoadLatest(), facts.summaries || facts.tab === 'summaries' ? factsLoadSummaries() : null]);
    if (facts.detail) facts.detail = { id: facts.detail.id, res: await factsAPI('GET', '/api/facts/issue?id=' + encodeURIComponent(facts.detail.id)) };
    factsPaint('status', 'actions', 'body');
  },
  async factsIssue(id) {
    if (!id) return;
    facts.tab = 'feed';
    facts.detail = { id, res: null };
    factsPaint('tabs', 'body');
    const res = await factsAPI('GET', '/api/facts/issue?id=' + encodeURIComponent(id));
    if (!facts.detail || facts.detail.id !== id) return;
    facts.detail.res = res;
    factsPaint('body');
    const body = $('window-body');
    if (body) body.scrollTop = 0;
  },
  factsBack() { facts.detail = null; factsPaint('body'); },
  async factsSearch() {
    const input = $('facts-query');
    facts.query = input ? input.value.trim() : '';
    facts.detail = null;
    if (!facts.query) { facts.search = null; factsPaint('body'); return; }
    facts.search = null;
    const res = await factsAPI('GET', '/api/facts/search?text=' + encodeURIComponent(facts.query));
    facts.search = res;
    factsPaint('body');
  },
  factsSearchReset() { facts.search = null; facts.query = ''; facts.detail = null; factsPaint('body'); },
  async factsSummary(id) {
    facts.summary = { id, res: null };
    factsPaint('body');
    const res = await factsAPI('GET', '/api/facts/summary?id=' + encodeURIComponent(id));
    if (!facts.summary || facts.summary.id !== id) return;
    facts.summary.res = res;
    factsPaint('body');
  },
});

// Ссылка источника внутри карточки выпуска: карточка сама кликабельна
// (data-action), а общий обработчик гасит переход по умолчанию. Клик по
// ссылке ловим раньше него и не пускаем дальше — открывается источник, а не
// подробности.
document.addEventListener('click', ev => {
  if (ev.target.closest && ev.target.closest('a.facts-src')) ev.stopPropagation();
}, true);

factsLoadStatus();

/* ---------- события DOM ---------- */

document.addEventListener('click', ev => {
  const el = ev.target.closest('[data-action]');
  if (!el || el.disabled) return;
  const fn = actions[el.dataset.action];
  if (!fn) return;
  ev.preventDefault();
  fn(el.dataset.arg, el);
});
document.addEventListener('change', ev => {
  const el = ev.target.closest('[data-change]');
  if (el && actions[el.dataset.change]) actions[el.dataset.change](el.value, el);
});
document.addEventListener('submit', ev => {
  const el = ev.target.closest('[data-submit]');
  if (!el) return;
  ev.preventDefault();
  actions[el.dataset.submit]();
});
document.addEventListener('keydown', ev => {
  if (ev.target.id === 'composer-text' && ev.key === 'Enter' && !ev.shiftKey) {
    ev.preventDefault();
    actions.sendComposer();
  }
});
window.addEventListener('hashchange', () => {
  const id = hashId();
  if (id && (!app.conv || app.conv.id !== id)) loadConv(id);
});

boot();

// ===== Конвейер =====

/* Вкладка «Конвейер» в окне «Интересные факты»: цепочка из трёх
   MCP-инструментов демона — search получает досье о виде, summarize
   отбирает по нему проверенные факты, save_to_file сохраняет выпуск в файл.
   Цепочку ведёт приложение (код или модель), REST /api/pipeline/runs
   запускает прогон в фоне, а окно опрашивает его раз в 500 мс и рисует
   шаги по мере выполнения: итог шага, отпечатки sha256 и проверки передачи
   («вход шага N+1 = выход шага N»).

   Итоги шагов и превью файла — пересказ внешних источников: всё идёт через
   esc(), превью — текстом в <pre>, без разметки. Стабильные id и классы
   (#pipe-query, #pipe-run, .pipe-step[data-tool=…], #pipe-result) — для
   сценариев проверок и записи. */

const pipe = {
  form: { query: '', random: false, format: 'md', pass: 'inline', mode: 'code' },
  id: '',             // открытый прогон
  view: null,         // RunView открытого прогона: {id, mode, done, trace}
  error: null,        // {code, text, hint} — ошибка запуска или опроса
  runs: null,         // {ok, code, data, error} списка прогонов
  timer: null,        // опрос идущего прогона
  seq: 0,             // номер цикла опроса: новый отменяет прежний
  watching: false,    // прогон запущен отсюда — об итоге сказать тостом
  starting: false,    // POST ушёл, ответа ещё нет
};
app.pipe = pipe;

const pipePollMs = 500;
const pipeCostNote = 'summarize платный: около $0.002 за прогон (исполнитель «модель» — ещё его ходы)';
const pipeTools = [
  { tool: 'search', what: 'получить данные: досье о виде из MDD, Википедии и GBIF' },
  { tool: 'summarize', what: 'обработать: 3–5 проверенных фактов по досье' },
  { tool: 'save_to_file', what: 'сохранить: выпуск в файл у демона' },
];
const pipeStatusText = { pending: 'ожидает', running: 'идёт', ok: 'готово', failed: 'ошибка' };
const pipeStatusClass = { pending: '', running: 'warn', ok: 'ok', failed: 'bad' };
const pipeModeText = { code: 'код', agent: 'модель' };
const pipePassText = { inline: 'конверт целиком', ref: 'только отпечаток (ref)' };

// pipeDur — время из Go (time.Duration, наносекунды) словами.
function pipeDur(ns) {
  if (typeof ns !== 'number' || !isFinite(ns) || ns <= 0) return '';
  const s = ns / 1e9;
  if (s < 1) return Math.round(s * 1000) + ' мс';
  if (s < 60) return s.toFixed(1).replace('.', ',') + ' с';
  return Math.floor(s / 60) + ' мин ' + Math.round(s % 60) + ' с';
}
function pipeBytes(n) {
  if (typeof n !== 'number' || !isFinite(n)) return '';
  if (n < 1024) return n + ' Б';
  return (n / 1024).toFixed(1).replace('.', ',') + ' КБ';
}
function pipeShort(d) {
  const h = String(d || '').replace(/^sha256:/, '');
  return h.length > 12 ? h.slice(0, 12) : h;
}
function pipeRunning() { return !!(pipe.starting || (pipe.view && !pipe.view.done)); }
function pipeOpen() { return !!($('pipe-root') && $('window').open); }

// pipeSteps — три шага цепочки: из следа, недостающие — «ожидает».
function pipeSteps() {
  const got = factsList(pipe.view && pipe.view.trace && pipe.view.trace.steps);
  return pipeTools.map((t, i) => Object.assign({ n: i + 1, tool: t.tool, status: 'pending' },
    got.find(s => s.n === i + 1) || got.find(s => s.tool === t.tool) || {}));
}

/* ---------- отрисовка ---------- */

function pipeHTML() {
  return `<div id="pipe-root" class="pipe">
    ${pipeFormHTML()}
    <div id="pipe-view" class="pipe-view">${pipeViewHTML()}</div>
    <h4 class="facts-h">Последние прогоны</h4>
    <div id="pipe-runs">${pipeRunsHTML()}</div>
  </div>`;
}

function pipeRadio(name, value, label, title) {
  const on = pipe.form[name] === value;
  return `<label class="pipe-opt${on ? ' on' : ''}"${title ? ` title="${esc(title)}"` : ''}><input type="radio" name="pipe-${name}" id="pipe-${name}-${value}" value="${value}"${on ? ' checked' : ''}>${esc(label)}</label>`;
}

function pipeFormHTML() {
  const f = pipe.form;
  const off = !factsConnected();
  const busy = pipeRunning();
  const why = off ? 'Демон не подключён' : busy ? 'Прогон уже идёт — дождитесь итога' : 'search → summarize → save_to_file';
  return `<form id="pipe-form" class="pipe-form" data-submit="pipeRun" autocomplete="off">
    <div class="pipe-row">
      <input type="text" id="pipe-query" value="${esc(f.query)}" placeholder="манул, Otocolobus manul или 1006010"${f.random ? ' disabled' : ''} maxlength="200">
      <label class="pipe-check"><input type="checkbox" id="pipe-random"${f.random ? ' checked' : ''}> случайный вид</label>
      <button type="submit" class="solid" id="pipe-run"${off || busy ? ' disabled' : ''} title="${esc(why)}">Запустить цепочку</button>
    </div>
    <div class="pipe-row pipe-opts">
      <span class="pipe-group" id="pipe-format"><span class="lbl">формат</span>${pipeRadio('format', 'md', 'Markdown')}${pipeRadio('format', 'json', 'JSON')}</span>
      <span class="pipe-group" id="pipe-pass"><span class="lbl">передача</span>${pipeRadio('pass', 'inline', 'inline', 'следующий шаг получает конверт целиком и пересчитывает отпечаток')}${pipeRadio('pass', 'ref', 'ref', 'следующий шаг получает только отпечаток, данные достаёт демон')}</span>
      <span class="pipe-group" id="pipe-mode"><span class="lbl">исполнитель</span>${pipeRadio('mode', 'code', 'код', 'цепочку ведёт код приложения')}${pipeRadio('mode', 'agent', 'модель', 'цепочку ведёт модель: сама зовёт три инструмента по порядку')}</span>
    </div>
    <div class="hint">${esc(pipeCostNote)}</div>
  </form>`;
}

function pipeViewHTML() {
  let html = '';
  const e = pipe.error;
  if (e) {
    if (e.code === 503 || e.code === 0) {
      html += `<div class="facts-down pipe-error" id="pipe-error"><b>Демон «Интересных фактов» не подключён — цепочку запустить негде.</b>
        <div>${esc(e.text)}</div>${e.hint && !String(e.text).includes(e.hint) ? `<div class="facts-hint">${esc(e.hint)}</div>` : ''}</div>`;
    } else {
      html += `<div class="facts-error pipe-error" id="pipe-error">${esc(e.text)}</div>`;
    }
  }
  const v = pipe.view;
  const tr = v ? v.trace || {} : null;
  if (!v) {
    html += `<p class="hint" id="pipe-intro">Выход каждого шага — вход следующего. Отпечатки sha256 сверяются на каждой передаче: демон проверяет, что данные дошли целыми, приложение — что шаг обработал именно то, что ему передали.</p>`;
  } else {
    const req = tr.request || {};
    const what = req.random ? 'случайный вид' : `«${req.query || ''}»`;
    const state = !v.done ? '<span class="thinking">идёт</span>' : tr.ok ? '<span class="chip ok">готово</span>' : '<span class="chip bad">ошибка</span>';
    html += `<div class="pipe-status" id="pipe-status" data-run="${esc(v.id)}" data-done="${v.done ? '1' : '0'}">
      <b>Прогон ${esc(v.id)}</b> ${state}
      <span>${esc(what)}</span>
      <span class="hint">исполнитель: ${esc(pipeModeText[tr.mode] || tr.mode || '')} · формат ${esc(req.format || 'md')} · передача ${esc(pipePassText[req.pass] || req.pass || '')}</span>
      <span class="pipe-total"><span id="pipe-took">${esc(pipeDur(tr.took))}</span>${tr.cost_usd ? ' · <span id="pipe-cost">' + esc(factsUSD(tr.cost_usd)) + '</span>' : ''}</span>
    </div>`;
  }
  html += pipeChainHTML();
  if (v && v.done) html += pipeResultHTML(tr);
  return html;
}

// pipeChainHTML — схема: три карточки шагов и стрелки между ними.
function pipeChainHTML() {
  const steps = pipeSteps();
  const pass = pipe.view && pipe.view.trace && pipe.view.trace.request ? pipe.view.trace.request.pass : pipe.form.pass;
  let html = '<div class="pipe-chain" id="pipe-chain">';
  steps.forEach((s, i) => {
    if (i > 0) html += pipeArrowHTML(steps[i - 1], s, pass);
    html += pipeStepHTML(s, pipeTools[i].what);
  });
  return html + '</div>';
}

function pipeStepHTML(s, what) {
  const st = s.status || 'pending';
  let html = `<div class="pipe-step st-${esc(st)}" data-tool="${esc(s.tool)}" data-status="${esc(st)}" id="pipe-step-${esc(s.n)}">
    <div class="pipe-step-head"><span class="pipe-n">${esc(s.n)}</span><b class="pipe-tool">${esc(s.tool)}</b>
      <span class="chip ${esc(pipeStatusClass[st] || '')} pipe-state">${esc(pipeStatusText[st] || st)}</span></div>
    <div class="hint pipe-what">${esc(what)}</div>`;
  if (s.summary) html += `<div class="pipe-summary">${esc(s.summary)}</div>`;
  if (s.digest) html += `<div class="pipe-digest" title="${esc((s.kind ? s.kind + ' ' : '') + s.digest)}">выход${s.kind ? ' <span class="hint">' + esc(s.kind) + '</span>' : ''}: <code>${esc(pipeShort(s.digest))}</code></div>`;
  const checks = factsList(s.checks);
  if (checks.length) {
    html += `<ul class="pipe-checks">${checks.map(c => `<li class="${c.ok ? 'ok' : 'bad'}"><span class="pipe-mark">${c.ok ? '✓' : '✗'}</span> ${esc(c.name)}${c.note ? ` <span class="hint">${esc(c.note)}</span>` : ''}</li>`).join('')}</ul>`;
  }
  const meta = [pipeDur(s.took), s.cost_usd ? factsUSD(s.cost_usd) : '', s.bytes ? 'ответ ' + pipeBytes(s.bytes) : ''].filter(Boolean);
  if (meta.length) html += `<div class="pipe-meta">${meta.map(m => `<span>${esc(m)}</span>`).join('')}</div>`;
  if (s.error) html += `<div class="pipe-step-error">${esc(s.error)}</div>`;
  return html + '</div>';
}

// pipeArrowHTML — передача от шага a к шагу b: как передавали и сошлись
// ли отпечатки (вход b = выход a).
function pipeArrowHTML(a, b, pass) {
  let link = '';
  if (b.input && a.digest) {
    const ok = b.input === a.digest;
    link = `<span class="pipe-link ${ok ? 'ok' : 'bad'}" title="${esc('выход шага ' + a.n + ': ' + a.digest + '\nвход шага ' + b.n + ': ' + b.input)}">вход = выход шага ${esc(a.n)} ${ok ? '✓' : '✗'}</span>`;
  } else if (b.input) {
    link = `<span class="pipe-link" title="${esc(b.input)}">вход <code>${esc(pipeShort(b.input))}</code></span>`;
  }
  return `<div class="pipe-arrow${a.status === 'ok' ? ' lit' : ''}" data-link="${esc(a.n)}">
    <span class="pipe-arrow-line">→</span>
    <span class="hint">${esc(pass === 'ref' ? 'ref' : 'inline')}</span>
    ${link}
  </div>`;
}

function pipeResultHTML(tr) {
  const f = tr.file;
  if (!tr.ok || !f) {
    const failed = pipeSteps().find(s => s.status === 'failed');
    const where = failed ? `на шаге ${failed.n} (${failed.tool})` : '';
    return `<div class="pipe-result bad" id="pipe-result"><b>Цепочка не дошла до файла${where ? ' — ' + esc(where) : ''}.</b>
      <div class="pipe-result-error">${esc(tr.error || (failed && failed.error) || 'причина не названа')}</div></div>`;
  }
  const chain = factsList(f.chain).map(d => `<code title="${esc(d)}">${esc(pipeShort(d))}</code>`).join(' → ');
  return `<div class="pipe-result ok" id="pipe-result">
    <div class="pipe-result-head"><b>Файл сохранён:</b> <code id="pipe-file">${esc(f.path)}</code></div>
    <div class="pipe-result-meta">
      <span>${esc(f.format === 'json' ? 'JSON' : 'Markdown')}</span>
      <span id="pipe-size">${esc(pipeBytes(f.bytes))}</span>
      <span title="${esc(f.sha256)}">sha256 <code id="pipe-sha">${esc(pipeShort(f.sha256))}</code></span>
      <span>всего ${esc(pipeDur(tr.took))}</span>
      <span>цена ${esc(factsUSD(tr.cost_usd || 0))}</span>
    </div>
    ${chain ? `<div class="hint">цепочка отпечатков: ${chain}</div>` : ''}
    <pre id="pipe-preview" class="pipe-preview">${esc(f.preview || '')}</pre>
  </div>`;
}

function pipeRunsHTML() {
  const r = pipe.runs;
  if (!r) return '<p class="hint">загружаю…</p>';
  if (!r.ok) return `<p class="hint">${esc(r.code === 404 ? 'сервер приложения не знает /api/pipeline — обновите приложение' : r.error)}</p>`;
  const rows = factsList(r.data.runs);
  if (!rows.length) return '<p class="hint">Прогонов ещё не было — они живут в памяти приложения до перезапуска.</p>';
  return `<table class="grid pipe-runs"><tr><th>№</th><th>когда</th><th>вид</th><th>исполнитель</th><th>итог</th><th>файл</th><th>цена</th><th>время</th></tr>${rows.map(x => {
    const st = !x.done ? '<span class="chip warn">идёт</span>' : x.ok ? '<span class="chip ok">готово</span>' : `<span class="chip bad" title="${esc(x.error || '')}">ошибка</span>`;
    return `<tr class="pipe-run-row${x.id === pipe.id ? ' current' : ''}" data-action="pipeOpenRun" data-arg="${esc(x.id)}">
      <td>${esc(x.id)}</td><td>${esc(when(x.started))}</td><td>${esc(x.random ? 'случайный вид' : x.query || '')}</td>
      <td>${esc(pipeModeText[x.mode] || x.mode || '')}</td><td>${st}</td><td>${x.file ? `<code>${esc(x.file)}</code>` : ''}</td>
      <td>${x.cost_usd ? esc(factsUSD(x.cost_usd)) : '—'}</td><td>${esc(pipeDur(x.took))}</td></tr>`;
  }).join('')}</table>`;
}

// pipePaint — перерисовать часть вкладки, если она открыта. Форму не
// трогаем (в ней может печатать человек) — только кнопку запуска.
function pipePaint(...parts) {
  if (!$('pipe-root')) return;
  if (parts.includes('view')) $('pipe-view').innerHTML = pipeViewHTML();
  if (parts.includes('runs')) $('pipe-runs').innerHTML = pipeRunsHTML();
  const btn = $('pipe-run');
  if (btn) btn.disabled = !factsConnected() || pipeRunning();
}

/* ---------- запуск и опрос ---------- */

async function pipeLoadRuns() { pipe.runs = await factsAPI('GET', '/api/pipeline/runs'); }

// pipeEnter — вкладка открыта: список прогонов и опрос идущего.
async function pipeEnter() {
  await pipeLoadRuns();
  if (!pipe.id && pipe.runs.ok) {
    // Прогон шёл, пока окно было закрыто (или страницу перезагрузили):
    // показываем его.
    const live = factsList(pipe.runs.data.runs).find(x => !x.done);
    if (live) pipe.id = live.id;
  }
  pipePaint('runs');
  if (pipe.id && (!pipe.view || !pipe.view.done)) pipePoll();
}

function pipeStopPoll() {
  if (pipe.timer) clearTimeout(pipe.timer);
  pipe.timer = null;
}

// pipePoll — GET прогона раз в pipePollMs до done. Опрос идёт и при
// закрытом окне: прогон платный, и об итоге человеку скажет тост. Новый
// вызов отменяет прежний цикл (seq), двух опросов одного прогона не бывает.
async function pipePoll() {
  pipeStopPoll();
  const id = pipe.id;
  const seq = ++pipe.seq;
  if (!id) return;
  const r = await factsAPI('GET', '/api/pipeline/runs/' + encodeURIComponent(id));
  if (pipe.seq !== seq || pipe.id !== id) return; // тем временем открыли другой прогон
  if (r.ok) {
    pipe.view = r.data;
    pipe.error = null;
  } else {
    pipe.error = { code: r.code, text: r.error, hint: r.data && r.data.hint };
    if (r.code === 404) pipe.view = null;
  }
  const done = !r.ok || !!(pipe.view && pipe.view.done);
  if (done) {
    await pipeLoadRuns();
    if (pipe.seq !== seq) return;
  }
  pipePaint('view', done ? 'runs' : '');
  if (!done) {
    pipe.timer = setTimeout(pipePoll, pipePollMs);
  } else if (r.ok && pipe.watching && !pipeOpen()) {
    toast('Конвейер: ' + (pipe.view.trace.ok ? 'файл сохранён' : 'цепочка оборвалась — подробности во вкладке «Конвейер»'), !pipe.view.trace.ok);
  }
  if (done) pipe.watching = false;
}

async function pipeRun() {
  if (pipeRunning()) return;
  pipeReadForm();
  const f = pipe.form;
  if (!f.random && !f.query) {
    pipe.error = { code: 400, text: 'Введите вид — русское или латинское название, mdd-id — или отметьте «случайный вид».' };
    pipePaint('view');
    const q = $('pipe-query');
    if (q) q.focus();
    return;
  }
  const body = { query: f.random ? '' : f.query, random: f.random, format: f.format, pass: f.pass, mode: f.mode };
  pipe.starting = true;
  pipe.error = null;
  pipePaint('view');
  const r = await factsAPI('POST', '/api/pipeline/runs', body);
  pipe.starting = false;
  if (!r.ok) {
    pipe.error = { code: r.code, text: r.error, hint: r.data && r.data.hint };
    if (r.code === 503) { await factsLoadStatus(); factsPaint('status', 'actions'); }
    pipePaint('view');
    return;
  }
  pipe.id = r.data.id;
  pipe.watching = true;
  // До первого ответа опроса — три шага «ожидает».
  pipe.view = { id: pipe.id, mode: f.mode, done: false, trace: { request: body, mode: f.mode, steps: [] } };
  pipePaint('view');
  await pipeLoadRuns();
  pipePaint('runs');
  pipePoll();
}

// pipeReadForm — значения формы в состояние: форма перерисовывается вместе
// с окном (обновление состояния демона), и введённое не должно теряться.
function pipeReadForm() {
  const q = $('pipe-query');
  if (!q) return;
  const f = pipe.form;
  f.query = q.value.trim();
  f.random = !!($('pipe-random') && $('pipe-random').checked);
  for (const name of ['format', 'pass', 'mode']) {
    const el = document.querySelector(`#pipe-form input[name="pipe-${name}"]:checked`);
    if (el) f[name] = el.value;
  }
}

document.addEventListener('input', ev => { if (ev.target.closest && ev.target.closest('#pipe-form')) pipeReadForm(); });
document.addEventListener('change', ev => {
  if (!ev.target.closest || !ev.target.closest('#pipe-form')) return;
  pipeReadForm();
  const q = $('pipe-query');
  if (q) q.disabled = pipe.form.random;
  document.querySelectorAll('#pipe-form .pipe-opt').forEach(l => l.classList.toggle('on', !!l.querySelector('input:checked')));
});

Object.assign(actions, {
  pipeRun() { pipeRun(); },
  async pipeOpenRun(id) {
    if (!id || id === pipe.id) return;
    pipe.id = id;
    pipe.view = null;
    pipe.error = null;
    pipePaint('view', 'runs');
    await pipePoll();
  },
});
