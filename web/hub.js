'use strict';

/* Окно «MCP-серверы» (v20): реестр серверов, маршруты инструментов и
   длинный флоу агента. Подключается после app.js и пользуется его общими
   помощниками (app, esc, actions, factsAPI, toast, when, pipeDur, pipeBytes).

   Вкладка «Серверы» — таблица серверов реестра (цветная метка, транспорт,
   адрес, статус с причиной и подсказкой, имя из initialize, PID, сколько
   инструментов выдано модели и сколько скрыто) и маршруты «инструмент →
   сервер»: скрытые строки приглушены, с причиной. Вкладка «Длинный флоу» —
   запуск заготовки, вызовы по мере хода (сервер — цветной меткой, «← из №k» —
   откуда пришли аргументы), итог: проверки выбора, маршрута и порядка,
   счётчики самих серверов, ответ модели и файл блокнота.

   Всё, что пришло с серверов и от модели (аргументы, ответы, причины,
   превью файла), — данные, а не разметка (ИП-14): только через esc(), превью
   — текстом в <pre>. Цвет сервера из конфигурации проверяется, прежде чем
   попасть в style. Стабильные id и классы (#hub-connect, #hub-run,
   #hub-preset, #hub-species, .hub-server[data-name], .hub-route[data-tool],
   .hub-call[data-n], .hub-check, #hub-result, [data-hub-tab]) — для
   сценариев проверок и записи. */

const hub = {
  tab: 'servers',       // servers | flow
  servers: null,        // {ok, code, data, error} — GET /api/hub/servers или POST connect
  connectError: '',     // POST connect не удался
  connecting: false,
  presets: null,        // {ok, code, data, error} — GET /api/hub/presets
  form: { preset: '', species: '' },
  id: '',               // открытый прогон
  view: null,           // FlowView открытого прогона
  error: null,          // {code, text, why, hint, id} — ошибка запуска или опроса
  runs: null,           // {ok, code, data, error} — GET /api/hub/flows
  timer: null,          // опрос идущего прогона
  seq: 0,               // номер цикла опроса: новый отменяет прежний
  starting: false,      // POST ушёл, ответа ещё нет
  watching: false,      // прогон запущен отсюда — об итоге сказать тостом
  lastRunning: 0,       // номер идущего вызова, до которого уже прокрутили
  pollMs: 700,
};
app.hub = hub;

const hubStatusText = { ok: 'подключён', down: 'не отвечает', mismatch: 'не тот сервер', denied: 'отверг токен', idle: 'не подключался' };
const hubStatusClass = { ok: 'ok', down: 'bad', mismatch: 'bad', denied: 'bad', idle: '' };
const hubTransportText = { stdio: 'stdio', http: 'HTTP' };
const hubLevel = { ok: ['✓', 'ok', 'пройдена'], fail: ['✗', 'bad', 'провал'], warn: ['⚠', 'warn', 'предупреждение'] };
const hubStateText = { running: 'идёт', done: 'готово', failed: 'сбой' };
const hubPalette = ['#2f6b4f', '#2c5a85', '#a5701a', '#6a4c93', '#a3412e', '#3d7f8c'];
const hubNoRoute = '#8a8478';

/* ---------- мелочи ---------- */

const hubList = v => (Array.isArray(v) ? v : []);
function hubCut(s, n) {
  s = String(s == null ? '' : s);
  return s.length > n ? s.slice(0, n - 1) + '…' : s;
}
// hubSafeColor — цвет из конфигурации в style: только #hex, имя цвета или
// rgb()/hsl() из цифр; иначе ''.
function hubSafeColor(c) {
  c = String(c || '').trim();
  return /^#[0-9a-f]{3,8}$/i.test(c) || /^[a-z]{3,20}$/i.test(c) || /^(rgb|hsl)a?\([\d\s.,%]+\)$/i.test(c) ? c : '';
}
function hubServerList() { return hub.servers && hub.servers.ok ? hubList(hub.servers.data.servers) : []; }
function hubColor(name) {
  if (!name) return hubNoRoute;
  const list = hubServerList();
  const i = list.findIndex(s => s.name === name);
  if (i < 0) return hubPalette[Math.abs([...name].reduce((h, ch) => h * 31 + ch.charCodeAt(0), 7)) % hubPalette.length];
  return hubSafeColor(list[i].color) || hubPalette[i % hubPalette.length];
}
// hubTag — цветная метка сервера.
function hubTag(name, extra) {
  return `<span class="hub-tag${name ? '' : ' none'}" style="--hub-c:${esc(hubColor(name))}"><i class="hub-dot"></i>${esc(name || 'нет маршрута')}${extra ? ' ' + extra : ''}</span>`;
}
function hubMs(ns) {
  if (typeof ns !== 'number' || !isFinite(ns) || ns <= 0) return '';
  const ms = ns / 1e6;
  return (ms < 1 ? '<1' : Math.round(ms).toLocaleString('ru-RU')) + ' мс';
}
// hubArgs — аргументы вызова коротко: «ключ: значение, …».
function hubArgs(a) {
  let o = a;
  if (typeof a === 'string') { try { o = JSON.parse(a); } catch (e) { return a; } }
  if (o && typeof o === 'object' && !Array.isArray(o)) {
    const parts = Object.entries(o).map(([k, v]) => k + ': ' + hubCut(typeof v === 'string' ? v : JSON.stringify(v), 42));
    return parts.length ? parts.join(', ') : '—';
  }
  return o == null ? '—' : JSON.stringify(o);
}
function hubPretty(a) {
  try { return JSON.stringify(typeof a === 'string' ? JSON.parse(a) : a, null, 2); } catch (e) { return String(a); }
}
function hubUSD(v) { return typeof v === 'number' && isFinite(v) ? '$' + v.toFixed(4) : '—'; }
function hubPreset(id) {
  const list = hub.presets && hub.presets.ok ? hubList(hub.presets.data) : [];
  return list.find(p => p.id === id) || list[0] || null;
}
function hubCanRun() { return !!(hub.servers && hub.servers.ok && hub.servers.data.canRun); }
function hubRunning() { return !!(hub.starting || (hub.view && hub.view.state === 'running')); }
function hubOpen() { return !!($('hub-root') && $('window').open); }

/* ---------- загрузка ---------- */

async function hubLoadServers() {
  const r = await factsAPI('GET', '/api/hub/servers');
  hub.servers = r;
  hubRenderButton();
}
async function hubLoadPresets() {
  hub.presets = await factsAPI('GET', '/api/hub/presets');
  const p = hubPreset(hub.form.preset);
  if (p && !hub.form.preset) {
    hub.form.preset = p.id;
    if (!hub.form.species) hub.form.species = p.species || '';
  }
}
async function hubLoadRuns() { hub.runs = await factsAPI('GET', '/api/hub/flows'); }

/* ---------- кнопка на пульте ---------- */

// Рядом с «Окна ▾», как кнопка «Факты»: точки — статусы серверов.
function hubRenderButton() {
  let btn = $('hub-button');
  if (!btn) {
    const anchor = $('windows-button');
    if (!anchor) return;
    btn = document.createElement('button');
    btn.type = 'button';
    btn.id = 'hub-button';
    btn.className = 'ghost hub-button';
    btn.dataset.action = 'openWindow';
    btn.dataset.arg = 'hub';
    anchor.insertAdjacentElement('afterend', btn);
  }
  const list = hubServerList();
  const dots = list.map(s => `<span class="hub-pdot ${esc(hubStatusClass[s.status] || '')}" style="--hub-c:${esc(hubColor(s.name))}"></span>`).join('');
  btn.innerHTML = `${dots || '<span class="hub-pdot"></span>'}MCP-серверы`;
  const lines = ['Реестр MCP-серверов, маршруты инструментов и длинный флоу'];
  for (const s of list) lines.push(`${s.name}: ${hubStatusText[s.status] || s.status}${s.reason ? ' — ' + s.reason : ''}`);
  if (hub.servers && !hub.servers.ok) lines.push(hub.servers.code === 404 ? 'сервер приложения не знает /api/hub' : hub.servers.error);
  btn.title = lines.join('\n');
}

/* ---------- окно ---------- */

app.windows.hub = {
  title: 'MCP-серверы',
  async render() {
    await Promise.all([hubLoadServers(), hubLoadPresets(), hubLoadRuns()]);
    if (!hub.id && hub.runs.ok) {
      // Флоу шёл, пока окно было закрыто (или страницу перезагрузили).
      const live = hubList(hub.runs.data.flows).find(x => x.state === 'running');
      if (live) { hub.id = live.id; hub.tab = 'flow'; }
    }
    if (hub.id && (!hub.view || hub.view.state === 'running')) setTimeout(hubPoll, 0); // после отрисовки окна
    return `<div id="hub-root" class="hub">
      <div class="tabs hub-tabs" id="hub-tabs">${hubTabsHTML()}</div>
      <div id="hub-body" class="hub-body">${hubBodyHTML()}</div>
    </div>`;
  },
};

function hubTabsHTML() {
  const tab = (id, label, title) => `<button type="button" class="tab${hub.tab === id ? ' active' : ''}" data-action="hubTab" data-arg="${id}" data-hub-tab="${id}" id="hub-tab-${id}" title="${esc(title)}">${esc(label)}</button>`;
  const n = hubServerList().length;
  return tab('servers', 'Серверы' + (n ? ' · ' + n : ''), 'серверы реестра и маршруты инструментов') +
    tab('flow', 'Длинный флоу', 'агент сам выбирает инструменты разных серверов; код проверяет выбор, маршрут и порядок');
}

function hubBodyHTML() { return hub.tab === 'flow' ? hubFlowHTML() : hubServersHTML(); }

// hubPaint — перерисовать часть окна, если оно открыто. Форму флоу не
// трогаем (в ней может печатать человек) — только кнопку запуска.
function hubPaint(...parts) {
  if (!$('hub-root')) return;
  if (parts.includes('tabs')) $('hub-tabs').innerHTML = hubTabsHTML();
  if (parts.includes('body')) $('hub-body').innerHTML = hubBodyHTML();
  if (parts.includes('servers') && $('hub-servers-root')) $('hub-servers-root').outerHTML = hubServersHTML();
  if (parts.includes('view') && $('hub-view')) $('hub-view').innerHTML = hubViewHTML();
  if (parts.includes('runs') && $('hub-runs')) $('hub-runs').innerHTML = hubRunsHTML();
  const btn = $('hub-run');
  if (btn) btn.disabled = !hubCanRun() || hubRunning();
}

/* ---------- вкладка «Серверы» ---------- */

function hubServersHTML() {
  let html = '<div id="hub-servers-root" class="hub-servers-root">';
  const r = hub.servers;
  if (!r) return html + '<p class="hint">загружаю…</p></div>';
  if (!r.ok) {
    return html + `<div class="facts-down hub-off" id="hub-off"><b>Реестр серверов недоступен.</b>
      <div>${esc(r.code === 404 ? 'сервер приложения не знает /api/hub — обновите приложение' : r.error)}</div></div></div>`;
  }
  const d = r.data;
  const list = hubList(d.servers);
  const routes = hubList(d.routes);
  const on = list.filter(s => s.status === 'ok').length;
  const given = routes.filter(x => !x.hidden).length;
  html += `<div class="hub-bar">
    <button type="button" class="solid" id="hub-connect" data-action="hubConnect"${hub.connecting || !list.length ? ' disabled' : ''}>${hub.connecting ? '<span class="thinking">подключаю</span>' : 'Подключить все'}</button>
    <span class="hub-sum">${list.length ? `подключено <b>${on}</b> из ${list.length}` : ''}${routes.length ? ` · модели выдано <b>${given}</b> инструментов, скрыто ${routes.length - given}` : ''}</span>
  </div>`;
  if (!list.length) {
    html += `<div class="facts-down hub-off" id="hub-off"><b>Реестр MCP-серверов выключен.</b><div>${esc(d.why || 'серверов в конфигурации нет')}</div></div>`;
  } else if (d.why && !d.canRun) {
    html += `<div class="hint hub-why">флоу недоступен: ${esc(d.why)}</div>`;
  }
  if (hub.connectError || d.connectError) html += `<div class="facts-error" id="hub-connect-error">${esc(hub.connectError || d.connectError)}</div>`;
  if (list.length) {
    html += `<table class="grid hub-servers"><tr><th>сервер</th><th>транспорт и адрес</th><th>статус</th><th>initialize</th><th>PID</th><th>инструменты</th><th>вызовов</th></tr>
      ${list.map(hubServerRowHTML).join('')}</table>`;
  }
  html += '<h4 class="hub-h">Маршруты: инструмент → сервер</h4>';
  if (d.routesError) html += `<div class="facts-error">${esc(d.routesError)}</div>`;
  if (!routes.length) {
    html += `<p class="hint" id="hub-routes-empty">Маршруты появятся после подключения — «Подключить все». Каждое имя инструмента закреплено за одним сервером; то, что сервер отдал, но модели не выдано, видно здесь с причиной.</p>`;
  } else {
    html += `<table class="grid hub-routes" id="hub-routes"><tr><th>инструмент</th><th>сервер</th><th>модели</th></tr>
      ${routes.map(x => `<tr class="hub-route${x.hidden ? ' hidden' : ''}" data-tool="${esc(x.tool)}" data-server="${esc(x.server)}" style="--hub-c:${esc(hubColor(x.server))}">
        <td><code>${esc(x.tool)}</code></td><td>${hubTag(x.server)}</td>
        <td>${x.hidden ? `<span class="hub-reason">скрыт: ${esc(x.reason || 'не выдан')}</span>` : '<span class="hub-given">выдан</span>'}</td></tr>`).join('')}</table>`;
  }
  return html + '</div>';
}

function hubServerRowHTML(s) {
  const st = s.status || 'idle';
  const init = s.reported ? `<code>${esc(s.reported)}</code>${s.version ? ' <span class="hint">' + esc(s.version) + '</span>' : ''}` : '<span class="hint">—</span>';
  return `<tr class="hub-server st-${esc(st)}" data-name="${esc(s.name)}" data-status="${esc(st)}" style="--hub-c:${esc(hubColor(s.name))}">
    <td><span class="hub-swatch"></span><b>${esc(s.name)}</b><div class="hint">${esc(s.title || '')}</div></td>
    <td><span class="chip hub-transport">${esc(hubTransportText[s.transport] || s.transport || '')}</span> <code class="hub-addr" title="${esc(s.addr || '')}">${esc(hubCut(s.addr, 60))}</code></td>
    <td><span class="chip ${esc(hubStatusClass[st] || '')} hub-status">${esc(hubStatusText[st] || st)}</span>
      ${s.reason ? `<div class="hub-status-reason">${esc(s.reason)}</div>` : ''}${s.hint ? `<div class="hub-status-hint">${esc(s.hint)}</div>` : ''}</td>
    <td>${init}</td>
    <td>${s.pid ? esc(s.pid) : '<span class="hint">—</span>'}</td>
    <td class="hub-num"><b>${esc(s.tools || 0)}</b> выдано${s.hidden ? ` · <span class="hint">${esc(s.hidden)} скрыто</span>` : ''}</td>
    <td class="hub-num">${esc(s.calls || 0)}</td></tr>`;
}

/* ---------- вкладка «Длинный флоу» ---------- */

function hubFlowHTML() {
  return `<div id="hub-flow" class="hub-flow">
    ${hubFormHTML()}
    <div id="hub-view" class="hub-view">${hubViewHTML()}</div>
    <h4 class="hub-h">Последние прогоны</h4>
    <div id="hub-runs">${hubRunsHTML()}</div>
  </div>`;
}

function hubFormHTML() {
  const r = hub.presets;
  const list = r && r.ok ? hubList(r.data) : [];
  const p = hubPreset(hub.form.preset);
  const can = hubCanRun();
  const busy = hubRunning();
  const why = !can ? 'флоу недоступен: ' + ((hub.servers && hub.servers.ok && hub.servers.data.why) || 'нет реестра или модели')
    : busy ? 'Флоу уже идёт — дождитесь итога' : 'модель получает инструменты всех серверов и сама выбирает, что и в каком порядке вызвать';
  return `<form id="hub-form" class="hub-form" data-submit="hubRun" autocomplete="off">
    <div class="hub-row">
      <label class="lbl" for="hub-preset">заготовка</label>
      <select id="hub-preset">${list.map(x => `<option value="${esc(x.id)}"${p && x.id === p.id ? ' selected' : ''}>${esc(x.title || x.id)}</option>`).join('')}</select>
      <label class="lbl" for="hub-species">вид</label>
      <input type="text" id="hub-species" value="${esc(hub.form.species)}" placeholder="${esc(p ? p.species : 'манул')}" maxlength="200">
      <button type="submit" class="solid" id="hub-run"${!can || busy ? ' disabled' : ''} title="${esc(why)}">Запустить флоу</button>
    </div>
    <div class="hint">${esc(can ? 'Платно: модель ведёт флоу в 10–20 ходов (порядка $0.01). Проверяет итог код, а не слова модели.' : why)}</div>
  </form>`;
}

function hubViewHTML() {
  let html = '';
  const e = hub.error;
  if (e) {
    if (e.code === 503) {
      html += `<div class="facts-down hub-error" id="hub-error"><b>Флоу запустить нельзя.</b><div>${esc(e.text)}</div>${e.hint ? `<div class="facts-hint">${esc(e.hint)}</div>` : ''}</div>`;
    } else if (e.code === 409 && e.id) {
      html += `<div class="facts-error hub-error" id="hub-error">${esc(e.text)} <button type="button" class="small" data-action="hubOpenRun" data-arg="${esc(e.id)}">открыть ${esc(e.id)}</button></div>`;
    } else {
      html += `<div class="facts-error hub-error" id="hub-error">${esc(e.text)}</div>`;
    }
  }
  const v = hub.view;
  if (!v) {
    return html + `<p class="hint" id="hub-intro">Модель получает инструменты всех серверов реестра без префиксов и сама решает, какие вызвать и в каком порядке.
      Каждый вызов уходит на сервер по маршруту. После флоу код сверяет выбор инструментов, маршрут, зависимости данных (значение из ответа одного вызова — в аргументах другого), порядок и счётчики самих серверов.</p>
      ${hubLegendHTML([])}`;
  }
  const calls = hubList(v.calls);
  const tr = v.trace || null;
  const state = v.state === 'running' ? '<span class="thinking">идёт</span>'
    : v.state === 'done' && tr && tr.ok ? '<span class="chip ok">готово</span>'
      : v.state === 'done' ? '<span class="chip bad">проверки не пройдены</span>' : '<span class="chip bad">сбой</span>';
  const cost = tr && tr.costUsd ? ' · ' + hubUSD(tr.costUsd) : '';
  html += `<div class="hub-status" id="hub-status" data-run="${esc(v.id)}" data-state="${esc(v.state)}">
    <b>Прогон ${esc(v.id)}</b> ${state}
    <span>${esc(v.title || v.preset || '')}</span><span>«${esc(v.species || '')}»</span>
    <span class="hub-total"><span id="hub-count">${esc(calls.length)} ${esc(hubCallsWord(calls.length))}</span> · <span id="hub-took">${esc(pipeDur(v.took) || '0 мс')}</span>${esc(cost)}</span>
  </div>`;
  html += hubLegendHTML(calls);
  html += hubCallsHTML(calls, v.state === 'running');
  if (v.state !== 'running') html += hubResultHTML(v, tr);
  return html;
}

function hubCallsWord(n) {
  const m10 = n % 10, m100 = n % 100;
  if (m10 === 1 && m100 !== 11) return 'вызов';
  if (m10 >= 2 && m10 <= 4 && (m100 < 10 || m100 >= 20)) return 'вызова';
  return 'вызовов';
}

// hubLegendHTML — серверы реестра цветными метками и сколько вызовов
// каждый обслужил в этом прогоне.
function hubLegendHTML(calls) {
  const names = hubServerList().map(s => s.name);
  for (const c of calls) if (!names.includes(c.server || '')) names.push(c.server || '');
  if (!names.length) return '';
  return `<div class="hub-legend" id="hub-legend">${names.map(n => {
    const k = calls.filter(c => (c.server || '') === n).length;
    return hubTag(n, calls.length ? `<b class="hub-legend-n">${esc(k)}</b>` : '');
  }).join('')}</div>`;
}

function hubCallState(c, running) {
  if (c.pending) return running ? 'running' : 'fail';
  return c.ok ? 'ok' : 'fail';
}

function hubCallsHTML(calls, running) {
  if (!calls.length) {
    return `<p class="hint" id="hub-calls-empty">${running ? '<span class="thinking">модель думает над первым ходом</span>' : 'Вызовов не было.'}</p>`;
  }
  const mark = { running: '<span class="thinking"></span>', ok: '✓', fail: '✗' };
  return `<table class="grid hub-calls" id="hub-calls"><tr><th>№</th><th>ход</th><th>сервер</th><th>инструмент</th><th>аргументы</th><th></th><th>мс</th><th>откуда</th></tr>
    ${calls.map(c => {
      const st = hubCallState(c, running);
      const from = hubList(c.from).map(k => `<span class="hub-from" data-from="${esc(k)}">← из №${esc(k)}</span>`).join(' ');
      return `<tr class="hub-call st-${st}" data-n="${esc(c.n)}" data-server="${esc(c.server || '')}" data-tool="${esc(c.tool)}" data-state="${st}" style="--hub-c:${esc(hubColor(c.server))}">
        <td class="hub-n">${esc(c.n)}</td><td class="hub-turn">${esc(c.turn || '')}</td>
        <td>${hubTag(c.server)}</td>
        <td><code class="hub-tool">${esc(c.tool)}</code>${c.summary ? `<div class="hint hub-summary">${esc(hubCut(c.summary, 160))}</div>` : ''}${c.error ? `<div class="hub-call-error">${esc(hubCut(c.error, 240))}</div>` : ''}</td>
        <td class="hub-args" title="${esc(hubPretty(c.args))}">${esc(hubCut(hubArgs(c.args), 110))}</td>
        <td class="hub-mark">${mark[st]}</td>
        <td class="hub-num">${esc(hubMs(c.took))}</td>
        <td class="hub-from-cell">${from}</td></tr>`;
    }).join('')}</table>`;
}

function hubResultHTML(v, tr) {
  if (!tr) return `<div class="hub-result bad" id="hub-result"><b>Флоу не состоялся.</b><div>${esc(v.error || 'причина не названа')}</div></div>`;
  const checks = hubList(tr.verdict && tr.verdict.checks);
  const fails = checks.filter(c => c.level === 'fail').length;
  const warns = checks.filter(c => c.level === 'warn').length;
  const ok = v.state === 'done' && tr.ok;
  const head = ok ? `Флоу прошёл проверки${warns ? ` (предупреждений: ${warns})` : ''}`
    : v.state === 'failed' ? 'Флоу оборвался' : `Флоу не прошёл проверки: провалов ${fails}`;
  let html = `<div class="hub-result ${ok ? 'ok' : 'bad'}" id="hub-result">
    <div class="hub-result-head"><b>${esc(head)}</b>
      <span class="hub-result-meta">
        <span>цена <b id="hub-cost">${esc(hubUSD(tr.costUsd || 0))}</b></span>
        <span>время ${esc(pipeDur(tr.took || v.took) || '—')}</span>
        <span>ходов ${esc(tr.turns || 0)}</span>
        <span>вызовов ${esc(hubList(tr.calls).length || hubList(v.calls).length)}</span>
      </span></div>`;
  if (tr.error) html += `<div class="hub-result-error">${esc(tr.error)}</div>`;
  if (checks.length) {
    html += `<h4 class="hub-h">Проверки</h4><ul class="hub-checks" id="hub-checks">${checks.map(c => {
      const [m, cls, word] = hubLevel[c.level] || ['?', '', c.level];
      return `<li class="hub-check ${cls}" data-level="${esc(c.level)}" title="${esc(word)}"><span class="hub-check-mark">${m}</span><span class="hub-check-name">${esc(c.name)}</span>${c.note ? `<span class="hub-check-note">${esc(c.note)}</span>` : ''}</li>`;
    }).join('')}</ul>`;
  }
  const deltas = hubList(tr.servers);
  if (deltas.length) {
    html += `<h4 class="hub-h">Серверы подтвердили: насчитал сам / в трассе</h4><table class="grid hub-deltas" id="hub-deltas">${deltas.map(d => {
      const served = d.served || {}, traced = d.traced || {};
      const tools = [...new Set([...Object.keys(traced), ...Object.keys(served)])];
      return `<tr class="hub-delta" data-server="${esc(d.server)}"><td>${hubTag(d.server)}${d.pid ? `<div class="hint">pid ${esc(d.pid)}</div>` : ''}</td><td>${tools.map(t => {
        const a = served[t] || 0, b = traced[t] || 0;
        return `<span class="hub-dt ${a === b ? 'ok' : 'bad'}" data-tool="${esc(t)}"><code>${esc(t)}</code> ${esc(a)} / ${esc(b)} ${a === b ? '✓' : '✗'}</span>`;
      }).join('') || '<span class="hint">вызовов не было</span>'}</td></tr>`;
    }).join('')}</table>`;
  }
  if (tr.answer) html += `<h4 class="hub-h">Ответ модели</h4><div class="hub-answer" id="hub-answer">${esc(tr.answer)}</div>`;
  if (tr.file) {
    html += `<h4 class="hub-h">Блокнот</h4><div class="hub-file-row">файл: <code id="hub-file">${esc(tr.file)}</code></div>`;
    if (tr.preview) html += `<pre id="hub-preview" class="hub-preview">${esc(tr.preview)}</pre>`;
  }
  return html + '</div>';
}

function hubRunsHTML() {
  const r = hub.runs;
  if (!r) return '<p class="hint">загружаю…</p>';
  if (!r.ok) return `<p class="hint">${esc(r.code === 404 ? 'сервер приложения не знает /api/hub — обновите приложение' : r.error)}</p>`;
  const rows = hubList(r.data.flows);
  if (!rows.length) return '<p class="hint">Прогонов ещё не было — они живут в памяти приложения до перезапуска.</p>';
  return `<table class="grid hub-runs"><tr><th>№</th><th>когда</th><th>заготовка</th><th>вид</th><th>итог</th><th>вызовов</th><th>цена</th><th>время</th></tr>${rows.map(x => {
    const st = x.state === 'running' ? '<span class="chip warn">идёт</span>'
      : x.ok ? '<span class="chip ok">готово</span>'
        : `<span class="chip bad" title="${esc(x.error || '')}">${esc(x.state === 'failed' ? 'сбой' : 'не прошёл')}</span>`;
    return `<tr class="hub-run-row${x.id === hub.id ? ' current' : ''}" data-action="hubOpenRun" data-arg="${esc(x.id)}">
      <td>${esc(x.id)}</td><td>${esc(when(x.started))}</td><td>${esc(x.title || x.preset || '')}</td><td>${esc(x.species || '')}</td>
      <td>${st}</td><td class="hub-num">${esc(x.calls || 0)}</td><td>${x.costUsd ? esc(hubUSD(x.costUsd)) : '—'}</td><td>${esc(pipeDur(x.took))}</td></tr>`;
  }).join('')}</table>`;
}

/* ---------- запуск и опрос ---------- */

function hubStopPoll() {
  if (hub.timer) clearTimeout(hub.timer);
  hub.timer = null;
}

// hubPoll — GET прогона раз в hub.pollMs, пока он идёт. Опрос идёт и при
// закрытом окне: об итоге скажет тост. Новый вызов отменяет прежний цикл.
async function hubPoll() {
  hubStopPoll();
  const id = hub.id;
  const seq = ++hub.seq;
  if (!id) return;
  const r = await factsAPI('GET', '/api/hub/flows/' + encodeURIComponent(id));
  if (hub.seq !== seq || hub.id !== id) return; // тем временем открыли другой прогон
  if (r.ok) {
    hub.view = r.data;
    hub.error = null;
  } else {
    hub.error = { code: r.code, text: r.error };
    if (r.code === 404) hub.view = null;
  }
  const done = !r.ok || !hub.view || hub.view.state !== 'running';
  if (done) {
    await Promise.all([hubLoadRuns(), hubLoadServers()]); // счётчики вызовов серверов выросли
    if (hub.seq !== seq) return;
  }
  hubPaint('view', done ? 'runs' : '');
  hubFollow(done);
  if (!done) {
    hub.timer = setTimeout(hubPoll, hub.pollMs);
  } else {
    if (r.ok && hub.watching && !hubOpen() && hub.view) {
      const ok = hub.view.state === 'done' && hub.view.trace && hub.view.trace.ok;
      toast('Длинный флоу: ' + (ok ? 'проверки пройдены' : 'есть провалы — подробности в окне «MCP-серверы»'), !ok);
    }
    hub.watching = false;
  }
}

// hubFollow — держать в виду идущий вызов, а по итогу — блок проверок.
function hubFollow(done) {
  if (!hubOpen()) return;
  if (done) {
    const res = $('hub-result');
    if (res && hub.lastRunning) { res.scrollIntoView({ block: 'nearest' }); hub.lastRunning = 0; }
    return;
  }
  const row = document.querySelector('#hub-calls tr.hub-call.st-running');
  const n = row ? Number(row.dataset.n) : 0;
  if (row && n !== hub.lastRunning) {
    hub.lastRunning = n;
    row.scrollIntoView({ block: 'nearest' });
  }
}

async function hubRun() {
  if (hubRunning()) return;
  hubReadForm();
  const f = hub.form;
  const p = hubPreset(f.preset);
  const body = { preset: p ? p.id : f.preset, species: f.species };
  hub.starting = true;
  hub.error = null;
  hubPaint('view');
  const r = await factsAPI('POST', '/api/hub/flows', body);
  hub.starting = false;
  if (!r.ok) {
    hub.error = { code: r.code, text: r.error, hint: r.data && r.data.hint, id: r.data && r.data.id };
    if (r.code === 503) await hubLoadServers();
    hubPaint('view');
    return;
  }
  hub.id = r.data.id;
  hub.watching = true;
  hub.lastRunning = 0;
  hub.view = { id: hub.id, state: 'running', preset: body.preset, title: p ? p.title : '', species: body.species || (p ? p.species : ''), calls: [] };
  hubPaint('view');
  await hubLoadRuns();
  hubPaint('runs');
  hubPoll();
}

// hubReadForm — значения формы в состояние: форма перерисовывается вместе
// с вкладкой, и введённое не должно теряться.
function hubReadForm() {
  const p = $('hub-preset'), s = $('hub-species');
  if (p) hub.form.preset = p.value;
  if (s) hub.form.species = s.value.trim();
}

document.addEventListener('input', ev => { if (ev.target.closest && ev.target.closest('#hub-form')) hubReadForm(); });
document.addEventListener('change', ev => {
  if (!ev.target || ev.target.id !== 'hub-preset') return;
  hub.form.preset = ev.target.value;
  // Другая заготовка — её вид по умолчанию.
  const p = hubPreset(hub.form.preset);
  const s = $('hub-species');
  hub.form.species = p ? p.species || '' : '';
  if (s) { s.value = hub.form.species; s.placeholder = p ? p.species : ''; }
});

Object.assign(actions, {
  hubTab(tab) {
    if (tab !== 'servers' && tab !== 'flow') return;
    hubReadForm();
    hub.tab = tab;
    hubPaint('tabs', 'body');
  },
  async hubConnect() {
    if (hub.connecting) return;
    hub.connecting = true;
    hub.connectError = '';
    hubPaint('servers');
    const r = await factsAPI('POST', '/api/hub/connect');
    hub.connecting = false;
    if (r.ok) hub.servers = r;
    else hub.connectError = r.error;
    hubRenderButton();
    hubPaint('tabs', 'servers');
  },
  hubRun() { hubRun(); },
  async hubOpenRun(id) {
    if (!id) return;
    if (hub.tab !== 'flow') { hubReadForm(); hub.tab = 'flow'; hubPaint('tabs', 'body'); }
    if (id === hub.id && hub.view) { hub.error = null; hubPaint('view'); return; }
    hub.id = id;
    hub.view = null;
    hub.error = null;
    hub.lastRunning = 0;
    hubPaint('view', 'runs');
    await hubPoll();
  },
});

hubLoadServers();
