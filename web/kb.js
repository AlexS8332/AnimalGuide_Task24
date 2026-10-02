'use strict';

/* Окно «База знаний» (v21–v22): корпус, чанки документа в двух
   стратегиях, поиск по двум индексам рядом, ответ модели с базой и без
   неё, прогон контрольных вопросов и последний отчёт сравнения стратегий.
   Подключается после app.js и пользуется его общими помощниками (app, esc,
   actions, factsAPI, factsURL, num, plural, toast).

   Вкладка «Корпус» — манифест (документы, страницы, символы, corpus_sha,
   лицензии), индексы базы, статус эмбеддера и таблица документов; клик по
   документу открывает его чанки. «Чанки» — канонический текст документа,
   где каждый чанк — блок своего фона с подписью (chunk_id, путь раздела,
   токены, «на стыке разделов»); перекрытие fixed подсвечено штриховкой.
   «Поиск» — один запрос сразу в оба индекса, выдачи рядом; клик по
   попаданию ведёт к чанку. «Сравнение» — таблицы последнего отчёта
   `kb eval` и вывод числами.

   v22: «Спросить» — один вопрос в режимах norag и rag, ответы рядом;
   у rag — найденные фрагменты, ссылки [chunk_id] в ответе ведут к чанку;
   вопрос из набора оценивается правилом. «Контрольные вопросы» — прогон
   набора test в обоих режимах: строки заполняются по мере готовности
   (опрос /api/kb/evals/{id}), итог — сводка режимов рядом и вывод.

   v23: «Поиск» — второй этап (rewrite, реранкинг, фильтр, K0): запрос
   идёт через конвейер в один индекс, блок «до и после» показывает исходный
   и переписанный запрос, K0 кандидатов с косинусом (черта порога), рангами
   dense и BM25, RRF и причиной отсечения; отсечённые зачёркнуты, «пусто
   после фильтра» — плашкой. «Спросить» и «Контрольные вопросы» — выбор
   режимов (norag, rag, rag+filter, rag+rewrite, rag+both), у режимов с
   конвейером — краткая сводка пути поиска. «Режимы» — матрица режимов
   (kb matrix) и калибровка порога (kb calibrate) с гистограммой косинусов.
   Стабильные id и классы v23: #kb-index, #kb-rewrite, #kb-rerank,
   #kb-filter, #kb-k0, #kb-ctx, #kb-trace, #kb-original, #kb-rewritten,
   .kb-expanded, #kb-tr-line, .kb-cand[data-chunk][data-kept], .kb-cos-thr,
   .kb-reason, #kb-empty, [data-kb-mode], #kb-ask-modes, #kb-qa-modes,
   .kb-tb, #kb-modes, #kb-matrix, .kb-mx-row, #kb-modes-conclusion,
   #kb-calib-table, .kb-cal-row, #kb-hist, #kb-matrix-none, #kb-calib-none.

   Всё, что пришло из корпуса (заголовки, тексты, разделы, причины), —
   данные, а не разметка: только через esc(). Стабильные id и классы
   (#kb-button, [data-kb-tab], #kb-docs, .kb-doc[data-doc], #kb-chunks,
   #kb-doc, #kb-strategy, .kb-chunk[data-id], #kb-search, #kb-q, #kb-k,
   #kb-mode, #kb-go, .kb-result[data-index], .kb-hit[data-chunk],
   #kb-report, #kb-off; v22: #kb-ask, #kb-ask-q, #kb-ask-pick, #kb-ask-go,
   .kb-thinking, .kb-answer[data-mode], .kb-src[data-chunk], .kb-cite,
   .kb-verdict[data-mode][data-verdict], #kb-ask-prompt, #kb-qa,
   #kb-qa-run, .kb-qa-row[data-id], #kb-qa-summary, #kb-qa-runs) — для
   сценариев проверок и записи. Ответы модели — тоже данные: esc().

   v24: режим rag+cite — ответ kb_answer с источниками и цитатами. В
   «Спросить» у него вместо текста блок .kb-cited: бейджи проверки кодом
   (.kb-check-badge[data-check]), ответ, источники .kb-cited-src[data-chunk]
   (номер, статья › раздел, chunk_id), цитаты .kb-quote[data-chunk] и
   утверждения судьи смысла .kb-claim[data-supported]; у «не знаю» — плашка
   .kb-unknown с уточняющим вопросом .kb-clarify. Клик по цитате — вкладка
   «Чанки» с подсветкой цитаты внутри чанка (mark.kb-qmark). «Контрольные
   вопросы» — столбцы проверки rag+cite (.kb-qa-c[data-check]) и строки
   сводки с новыми полями ModeStats. app.kbOpenCite(chunk_id, цитата) —
   открыть чанк с подсветкой из чата (GET /api/kb/chunk/{id}). */

const kb = {
  tab: 'docs',          // docs | chunks | search | ask | qa | modes | report
  info: null,           // {ok, code, data, error} — GET /api/kb/info
  docs: null,           // {ok, code, data, error} — GET /api/kb/docs
  doc: '',              // документ вкладки «Чанки»
  strategy: 'structure',
  views: {},            // `${doc}|${index}` → {ok, code, data, error} — GET /api/kb/docs/{id}?index=
  focus: '',            // chunk_id, к которому прокрутить вкладку «Чанки»
  quote: '',            // v24: цитата, подсвеченная внутри чанка focus
  // v23: index — all (оба рядом) или индекс; rewrite, rerank, filter —
  // второй этап: хоть один включён — поиск идёт через конвейер.
  form: { q: '', k: 5, mode: 'dense', index: 'all', rewrite: '', rerank: '', filter: false, k0: 20, ctx: '' },
  result: null,         // {ok, code, data, error} — GET /api/kb/search
  searching: false,
  report: null,         // {ok, code, data, error} — GET /api/kb/report
  // v22
  questions: null,      // {ok, code, data, error} — GET /api/kb/questions
  askForm: { q: '', qid: '' },
  ask: null,            // {ok, code, data, error} — POST /api/kb/ask
  asking: false,
  qaForm: { repeats: 1, judge: true },
  evals: null,          // {ok, code, data, error} — GET /api/kb/evals
  eval: null,           // EvalView открытого прогона
  evalError: null,      // {code, text, id, why, hint} — ошибка запуска или опроса
  starting: false,
  stopping: false,      // идёт DELETE прогона
  qaOpen: {},           // id вопроса → раскрыта строка
  // v23
  askModes: ['norag', 'rag'],
  qaModes: ['rag', 'rag+both'],
  matrix: null,         // {ok, code, data, error} — GET /api/kb/matrix
  calib: null,          // {ok, code, data, error} — GET /api/kb/calibration
  mxSplit: '',          // набор матрицы на экране; пусто — test, если есть
  pollMs: 700,
  timer: null,
  seq: 0,
};
app.kb = kb;

const kbTabs = [
  ['docs', 'Корпус', 'документы корпуса, индексы и эмбеддер'],
  ['chunks', 'Чанки', 'текст документа и границы чанков в двух стратегиях'],
  ['search', 'Поиск', 'один запрос в оба индекса — выдачи рядом; второй этап — кандидаты до и после фильтра'],
  ['ask', 'Спросить', 'ответ модели без базы и с базой — рядом, до трёх режимов'],
  ['qa', 'Контрольные вопросы', 'прогон набора test в выбранных режимах и сравнение'],
  ['modes', 'Режимы', 'матрица режимов поиска и калибровка порога'],
  ['report', 'Сравнение', 'последний отчёт сравнения стратегий (kb eval)'],
];
const kbStrategyText = { structure: 'structure — по разделам', fixed: 'fixed — окно с перекрытием' };
const kbBuild = 'go run ./cmd/kb index -strategy all';
const kbEval = 'go run ./cmd/kb eval';

/* ---------- мелочи ---------- */

const kbList = v => (Array.isArray(v) ? v : []);
function kbCut(s, n) {
  s = String(s == null ? '' : s).replace(/\s+/g, ' ').trim();
  return s.length > n ? s.slice(0, n - 1) + '…' : s;
}
function kbPct(f) { return typeof f === 'number' && isFinite(f) ? (100 * f).toFixed(1) + ' %' : '—'; }
function kbNum2(f) { return typeof f === 'number' && isFinite(f) ? f.toFixed(2) : '—'; }
function kbShort(sha) { return String(sha || '').slice(0, 12); }
function kbPages(p) { return typeof p === 'number' && isFinite(p) ? p.toLocaleString('ru-RU', { maximumFractionDigits: 1 }) : '—'; }
// kbOldURL — постоянная ссылка на ревизию статьи (…/w/index.php?oldid=),
// иначе адрес документа; только http/https.
function kbOldURL(d) {
  const u = factsURL(d.url);
  if (!u || !d.revid) return u;
  try { return new URL('/w/index.php?oldid=' + encodeURIComponent(d.revid), u).href; } catch (e) { return u; }
}
function kbParams(p) {
  p = p || {};
  if (p.size) return `size ${p.size}, overlap ${p.overlap || 0}`;
  if (p.max) return `max ${p.max}, min ${p.min || 0}`;
  return '—';
}
function kbPath(c) { return kbList(c.section_path).join(' › ') || c.section || ''; }
function kbBase() { return !!(kb.info && kb.info.ok); }
function kbIndexes() { return kbBase() ? kbList(kb.info.data.indexes) : []; }
function kbDocList() { return kb.docs && kb.docs.ok ? kbList(kb.docs.data) : []; }
function kbDocInfo(id) { return kbDocList().find(d => d.doc_id === id) || null; }
function kbStrategies() {
  const ids = kbIndexes().map(x => x.index_id);
  return ids.length ? ids : ['structure', 'fixed'];
}

/* ---------- загрузка ---------- */

async function kbLoadInfo() {
  kb.info = await factsAPI('GET', '/api/kb/info');
  kbRenderButton();
}
async function kbLoadDocs() {
  if (!kbBase()) return;
  kb.docs = await factsAPI('GET', '/api/kb/docs');
  if (!kb.doc && kbDocList().length) kb.doc = kbDocList()[0].doc_id;
}
async function kbLoadReport() { kb.report = kbBase() ? await factsAPI('GET', '/api/kb/report') : null; }
// kbLoadDoc — документ в обеих стратегиях: переключатель меняет вид
// мгновенно, а сводка сравнивает числа чанков.
async function kbLoadDoc(id) {
  if (!id || !kbBase()) return;
  await Promise.all(kbStrategies().map(async s => {
    const key = id + '|' + s;
    if (kb.views[key] && kb.views[key].ok) return;
    kb.views[key] = await factsAPI('GET', '/api/kb/docs/' + encodeURIComponent(id) + '?index=' + encodeURIComponent(s));
  }));
}

/* ---------- кнопка на пульте ---------- */

// Рядом с «MCP-серверы»: точка — база и эмбеддер (зелёная — dense,
// янтарная — эмбеддер не отвечает, поиск по BM25, кирпичная — базы нет).
function kbRenderButton() {
  let btn = $('kb-button');
  if (!btn) {
    const anchor = $('windows-button');
    if (!anchor) return;
    btn = document.createElement('button');
    btn.type = 'button';
    btn.id = 'kb-button';
    btn.className = 'ghost kb-button';
    btn.dataset.action = 'openWindow';
    btn.dataset.arg = 'kb';
    // В конец ряда: кнопки «Факты» и «MCP-серверы» встают сразу за «Окна ▾»
    // по мере ответов своих разделов, «База знаний» остаётся последней.
    anchor.parentElement.appendChild(btn);
  }
  const r = kb.info;
  let cls = '', lines = ['База знаний: корпус, чанки, поиск по двум индексам, сравнение стратегий'];
  if (r && r.ok) {
    const emb = r.data.embedder || {};
    cls = emb.ok ? 'ok' : 'warn';
    lines.push(`документов ${r.data.docs}, индексов ${kbList(r.data.indexes).length}`);
    lines.push(emb.ok ? 'эмбеддер: ' + (emb.model || '') : 'эмбеддер не отвечает — поиск по BM25');
  } else if (r) {
    cls = 'bad';
    lines.push(r.code === 404 ? 'сервер приложения не знает /api/kb' : (r.data && r.data.why) || r.error);
  }
  btn.innerHTML = `<span class="kb-dot ${cls}"></span>База знаний`;
  btn.title = lines.join('\n');
}

/* ---------- окно ---------- */

app.windows.kb = {
  title: 'База знаний',
  async render() {
    await kbLoadInfo();
    await kbLoadDocs();
    await kbLoadTab();
    if (kb.focus) setTimeout(kbScrollFocus, 0); // после отрисовки окна
    return `<div id="kb-root" class="kb">
      <div class="tabs kb-tabs" id="kb-tabs">${kbTabsHTML()}</div>
      <div id="kb-body" class="kb-body">${kbBodyHTML()}</div>
    </div>`;
  },
};

// kbLoadTab — что нужно открытой вкладке.
async function kbLoadTab() {
  if (!kbBase()) return;
  if (kb.tab === 'chunks') await kbLoadDoc(kb.doc);
  if (kb.tab === 'report' && (!kb.report || !kb.report.ok)) await kbLoadReport();
  if (kb.tab === 'ask' || kb.tab === 'qa') await kbLoadQuestions();
  if (kb.tab === 'qa') await kbLoadEvals(true);
  if (kb.tab === 'modes') await kbLoadModes();
}

function kbTabsHTML() {
  return kbTabs.map(([id, label, title]) => {
    let n = '';
    if (id === 'docs' && kbBase()) n = ' · ' + kb.info.data.docs;
    return `<button type="button" class="tab${kb.tab === id ? ' active' : ''}" data-action="kbTab" data-arg="${id}" data-kb-tab="${id}" id="kb-tab-${id}" title="${esc(title)}">${esc(label + n)}</button>`;
  }).join('');
}

function kbBodyHTML() {
  if (!kb.info) return '<p class="hint">загружаю…</p>';
  if (!kb.info.ok) return kbOffHTML();
  switch (kb.tab) {
    case 'chunks': return kbChunksHTML();
    case 'search': return kbSearchHTML();
    case 'report': return kbReportHTML();
    case 'ask': return kbAskHTML();
    case 'qa': return kbQaHTML();
    case 'modes': return kbModesTabHTML();
    default: return kbDocsHTML();
  }
}

function kbPaint(...parts) {
  if (!$('kb-root')) return;
  if (parts.includes('tabs')) $('kb-tabs').innerHTML = kbTabsHTML();
  if (parts.includes('body')) $('kb-body').innerHTML = kbBodyHTML();
  if (parts.includes('results') && $('kb-results')) $('kb-results').outerHTML = kbResultsHTML();
  if (parts.includes('ask') && $('kb-ask-out')) $('kb-ask-out').outerHTML = kbAskOutHTML();
  if (parts.includes('qa') && $('kb-qa-live')) $('kb-qa-live').outerHTML = kbQaLiveHTML();
  if (parts.includes('runs') && $('kb-qa-runs')) $('kb-qa-runs').outerHTML = kbQaRunsHTML();
  const go = $('kb-go');
  if (go) { go.disabled = kb.searching; go.innerHTML = kb.searching ? '<span class="thinking">ищу</span>' : 'Найти'; }
  const ask = $('kb-ask-go');
  if (ask) { ask.disabled = kb.asking; ask.innerHTML = kb.asking ? '<span class="thinking">думает</span>' : 'Спросить'; }
  const run = $('kb-qa-run');
  if (run) run.disabled = kbQaRunning() || kb.starting;
  const stop = $('kb-qa-stop');
  if (stop) stop.hidden = !kbQaRunning() || kb.stopping;
}

// kbOffHTML — базы нет (503) или сервер не знает /api/kb.
function kbOffHTML() {
  const r = kb.info;
  const d = r.data || {};
  if (r.code === 404) {
    return `<div class="facts-down kb-off" id="kb-off"><b>База знаний недоступна.</b><div>сервер приложения не знает /api/kb — обновите приложение</div></div>`;
  }
  return `<div class="facts-down kb-off" id="kb-off">
    <b>Базы знаний нет.</b>
    <div class="kb-why">${esc(d.why || r.error)}</div>
    ${d.hint ? `<div class="facts-hint">${esc(d.hint)}</div>` : ''}
    <div class="kb-steps">Как собрать:
      <ol>
        <li>эмбеддер (по желанию): <code>uv run embedder/server.py</code> — без него индекс соберётся только для BM25;</li>
        <li>индекс обеих стратегий: <code>${esc(kbBuild)}</code>;</li>
        <li>отчёт сравнения: <code>${esc(kbEval)}</code>;</li>
        <li>перезапустите приложение и откройте окно снова.</li>
      </ol></div>
    ${d.embedder ? `<div class="hint kb-off-emb">эмбеддер: ${esc(d.embedder.ok ? (d.embedder.model || '') + ' отвечает' : 'не отвечает — ' + (d.embedder.why || ''))}</div>` : ''}
    ${d.path ? `<div class="hint">база ищется здесь: <code>${esc(d.path)}</code> (флаг -kb или KB_DB)</div>` : ''}
  </div>`;
}

/* ---------- вкладка «Корпус» ---------- */

function kbEmbedderHTML(e) {
  e = e || {};
  if (e.ok) {
    return `<div class="kb-emb ok" id="kb-embedder"><span class="kb-dot ok"></span>
      <span>Эмбеддер <b>${esc(e.model || '')}</b>${e.device ? ' · ' + esc(e.device) : ''}${e.dims ? ' · ' + esc(e.dims) + ' изм.' : ''}</span>
      <span class="hint">${esc(e.url || '')}</span></div>`;
  }
  return `<div class="kb-emb bad" id="kb-embedder"><span class="kb-dot warn"></span>
    <span><b>Эмбеддер не отвечает — поиск по BM25.</b> ${esc(e.why || '')}</span>
    ${e.hint ? `<div class="facts-hint">${esc(e.hint)}</div>` : ''}</div>`;
}

function kbDocsHTML() {
  const v = kb.info.data;
  const m = v.manifest || {};
  const docs = kbDocList();
  const licenses = [...new Set(docs.map(d => d.license).filter(Boolean))];
  const chars = m.chars || docs.reduce((s, d) => s + (d.chars || 0), 0);
  let html = `<div id="kb-docs" class="kb-docs">
    <div class="kb-stats" id="kb-stats">
      <div class="kb-stat"><span>документов</span><b id="kb-n-docs">${esc(v.docs)}</b></div>
      <div class="kb-stat"><span>страниц</span><b id="kb-n-pages">${esc(kbPages(v.pages))}</b></div>
      <div class="kb-stat"><span>символов</span><b id="kb-n-chars">${esc(num(chars))}</b></div>
      <div class="kb-stat"><span>corpus_sha</span><b><code id="kb-sha" title="${esc(m.corpus_sha || '')}">${esc(kbShort(m.corpus_sha) || '—')}</code></b></div>
      <div class="kb-stat wide"><span>лицензии</span><b id="kb-licenses">${licenses.map(l => `<span class="chip">${esc(l)}</span>`).join(' ') || '—'}</b></div>
    </div>
    ${kbEmbedderHTML(v.embedder)}
    <div class="kb-indexes" id="kb-indexes">${kbIndexes().map(x => `<span class="kb-index" data-index="${esc(x.index_id)}">
      <b>${esc(x.index_id)}</b> ${esc(num(x.chunks))} чанков · ${esc(kbParams(x.params))} · ${esc(x.embedder || 'без векторов')}${x.dims ? ' · ' + esc(x.dims) + ' изм.' : ''}</span>`).join('') ||
      `<span class="hint">индексов нет — соберите: <code>${esc(kbBuild)}</code></span>`}</div>`;
  if (kb.docs && !kb.docs.ok) return html + `<div class="facts-error">${esc(kb.docs.error)}</div></div>`;
  html += `<table class="grid kb-doc-table" id="kb-doc-table"><tr><th>№</th><th>документ</th><th>источник</th><th class="kb-r">символов</th><th class="kb-r">страниц</th><th>revid</th><th>лицензия</th></tr>
    ${docs.map((d, i) => {
      const u = kbOldURL(d);
      return `<tr class="kb-doc" data-doc="${esc(d.doc_id)}" data-action="kbOpenDoc" data-arg="${esc(d.doc_id)}" title="чанки документа">
        <td class="kb-r hint">${i + 1}</td>
        <td><b class="kb-doc-title">${esc(d.title)}</b> <span class="hint">${esc(d.doc_id)}</span></td>
        <td>${esc(d.source)}</td>
        <td class="kb-r">${esc(num(d.chars))}</td><td class="kb-r">${esc(kbPages(d.pages))}</td>
        <td>${u ? `<a class="kb-ext" href="${esc(u)}" target="_blank" rel="noopener noreferrer" title="${esc(u)}">${esc(d.revid || 'ссылка')} ↗</a>` : esc(d.revid || '—')}</td>
        <td class="hint">${esc(d.license)}</td></tr>`;
    }).join('')}</table>`;
  return html + '</div>';
}

// Ссылка на ревизию внутри строки документа — переход, а не «открыть чанки»:
// общий обработчик app.js отменяет переход у элементов внутри data-action.
document.addEventListener('click', ev => {
  if (ev.target.closest && ev.target.closest('a.kb-ext')) ev.stopPropagation();
}, true);

/* ---------- вкладка «Чанки» ---------- */

function kbChunksHTML() {
  const docs = kbDocList();
  const strategies = kbStrategies();
  let html = `<div id="kb-chunks" class="kb-chunks">
    <div class="kb-bar">
      <label class="lbl" for="kb-doc">документ</label>
      <select id="kb-doc" data-change="kbPickDoc">${docs.map(d => `<option value="${esc(d.doc_id)}"${d.doc_id === kb.doc ? ' selected' : ''}>${esc(d.title)}</option>`).join('')}</select>
      <label class="lbl" for="kb-strategy">стратегия</label>
      <select id="kb-strategy" data-change="kbPickStrategy">${strategies.map(s => `<option value="${esc(s)}"${s === kb.strategy ? ' selected' : ''}>${esc(kbStrategyText[s] || s)}</option>`).join('')}</select>
      <span class="kb-seg">${strategies.map(s => `<button type="button" class="small${s === kb.strategy ? ' on' : ''}" data-action="kbPickStrategy" data-arg="${esc(s)}" data-kb-strategy="${esc(s)}">${esc(s)}</button>`).join('')}</span>
    </div>`;
  if (!kb.doc) return html + '<p class="hint">Документов в базе нет.</p></div>';
  const r = kb.views[kb.doc + '|' + kb.strategy];
  if (!r) return html + '<p class="hint">загружаю…</p></div>';
  if (!r.ok) return html + `<div class="facts-error" id="kb-doc-error">${esc(r.error)}</div></div>`;
  html += kbChunkSummaryHTML(r.data);
  html += `<div class="kb-text" id="kb-text" data-doc="${esc(kb.doc)}" data-index="${esc(r.data.index)}">${kbTextHTML(r.data)}</div>`;
  return html + '</div>';
}

// kbChunkSummaryHTML — сводка по документу: чанков в каждой стратегии,
// сколько на стыке разделов, медиана токенов.
function kbChunkSummaryHTML(v) {
  const d = v.doc || {};
  const u = kbOldURL(d);
  const cells = kbStrategies().map(s => {
    const r = kb.views[kb.doc + '|' + s];
    if (!r || !r.ok) return `<span class="kb-sum-s" data-index="${esc(s)}"><b>${esc(s)}</b> —</span>`;
    const cs = kbList(r.data.chunks);
    const mixed = cs.filter(c => c.mixed).length;
    const toks = cs.map(c => c.tokens || 0).sort((a, b) => a - b);
    const p50 = toks.length ? toks[Math.floor((toks.length - 1) / 2)] : 0;
    return `<span class="kb-sum-s${s === kb.strategy ? ' on' : ''}" data-index="${esc(s)}"><b>${esc(s)}</b>
      <span class="kb-sum-n">${esc(plural(cs.length, 'чанк', 'чанка', 'чанков'))}</span>, на стыке разделов ${esc(mixed)}, p50 ${esc(p50)} ток.</span>`;
  }).join('');
  return `<div class="kb-summary" id="kb-summary">
    <div class="kb-sum-doc"><b>${esc(d.title || kb.doc)}</b> <span class="hint">${esc(num(d.chars))} символов · ${esc(kbPages(d.pages))} стр.</span>
      ${u ? `<a class="kb-ext" href="${esc(u)}" target="_blank" rel="noopener noreferrer">ревизия ${esc(d.revid || '')} ↗</a>` : ''}</div>
    <div class="kb-sum-row">${cells}</div>
  </div>`;
}

// kbPlain — кусок текста документа: строки «## Путь › Раздел» —
// заголовками; всё — через esc(). v24: [a, b) — подсвеченная цитата
// (смещения в кодовых точках куска), её части по строкам — mark.kb-qmark.
function kbPlain(s, a, b) {
  let pos = 0;
  return s.split('\n').map(line => {
    const cps = Array.from(line);
    const from = pos;
    pos += cps.length + 1;
    const head = line.startsWith('## ');
    const html = kbMarkHTML(head ? cps.slice(3) : cps, from + (head ? 3 : 0), a, b);
    return head ? `<span class="kb-hd">${html}</span>` : html;
  }).join('\n');
}
// kbMarkHTML — кодовые точки строки (начало — off) с подсветкой [a, b).
function kbMarkHTML(cps, off, a, b) {
  if (typeof a !== 'number' || b <= off || a >= off + cps.length) return esc(cps.join(''));
  const x = Math.max(0, a - off), y = Math.min(cps.length, b - off);
  return esc(cps.slice(0, x).join('')) + `<mark class="kb-qmark">${esc(cps.slice(x, y).join(''))}</mark>` + esc(cps.slice(y).join(''));
}

// kbTextHTML — текст документа блоками чанков. Смещения — в рунах, поэтому
// текст режется по кодовым точкам. Каждая руна текста выводится один раз:
// перекрытие fixed (начало чанка внутри предыдущего) остаётся в хвосте
// предыдущего блока и подсвечено; промежутки между чанками (заголовки,
// пустые строки) — обычный текст.
function kbTextHTML(v) {
  const t = Array.from(v.text || '');
  const cs = kbList(v.chunks).slice().sort((a, b) => a.start - b.start || a.end - b.end);
  const piece = (a, b) => t.slice(a, b).join('');
  // Промежуток между блоками — без крайних переводов строк: блок и так с новой строки.
  const gap = (a, b) => {
    const s = piece(a, b).replace(/^\n+|\n+$/g, '');
    return s.trim() ? `<div class="kb-gap">${kbPlain(s)}</div>` : '';
  };
  let pos = 0;
  let html = '';
  cs.forEach((c, i) => {
    const start = Math.max(c.start, pos);
    const end = Math.min(Math.max(c.end, start), t.length);
    if (start > pos) html += gap(pos, start);
    const next = cs[i + 1];
    const ov = next && next.start < end ? Math.max(next.start, start) : end;
    const tags = [];
    // v24: цитата внутри чанка, к которому ведёт ссылка, — подсветкой.
    // Смещения — в кодовых точках текста документа.
    let qa = null, qb = null;
    if (kb.quote && c.chunk_id === kb.focus) {
      const f = kbFindQuote(t.slice(start, end), kb.quote);
      if (f) { qa = start + f[0]; qb = start + f[1]; } else tags.push('<span class="chip warn kb-qmiss" title="цитата не нашлась в тексте чанка дословно">цитата не найдена</span>');
    }
    if (c.mixed) tags.push('<span class="chip warn kb-mixed">на стыке разделов</span>');
    if (ov < end) tags.push(`<span class="chip kb-ovl">перекрытие ${esc(end - ov)} симв.</span>`);
    html += `<div class="kb-chunk ${i % 2 ? 'odd' : 'even'}${c.mixed ? ' mixed' : ''}${c.chunk_id === kb.focus ? ' focus' : ''}" data-id="${esc(c.chunk_id)}" data-ord="${esc(c.ord)}">
      <div class="kb-chunk-head"><code class="kb-cid">${esc(c.chunk_id)}</code><span class="kb-cpath">${esc(kbPath(c))}</span><span class="kb-ctok">${esc(c.tokens)} ток. · ${esc(c.start)}–${esc(c.end)}</span>${tags.join('')}</div>
      <div class="kb-chunk-text">${kbPiece(piece(start, ov), start, qa, qb)}${ov < end ? `<span class="kb-overlap" title="этот текст входит и в следующий чанк">${qa === null ? kbPlain(piece(ov, end)) : kbPlain(piece(ov, end), qa - ov, qb - ov)}</span>` : ''}</div></div>`;
    pos = Math.max(pos, end);
  });
  if (pos < t.length) html += gap(pos, t.length);
  if (!cs.length) html += '<p class="hint">В этом индексе у документа нет чанков.</p>';
  return html;
}

// kbPiece — начало блока чанка без ведущих пробелов; подсветка [qa, qb)
// (смещения в тексте документа) сдвигается вместе с ним.
function kbPiece(s, start, qa, qb) {
  const lead = Array.from(s.match(/^[ \t]*/)[0]).length;
  const rest = Array.from(s).slice(lead).join('');
  if (qa === null) return kbPlain(rest);
  return kbPlain(rest, qa - start - lead, qb - start - lead);
}

// kbNorm — нормализация для поиска цитаты, как у проверки кодом (Go,
// rag.quoteNorm): регистр, ё → е, кавычки, тире — «-», пробелы подряд —
// один, пробелы вокруг тире выкинуты («2,5 – 5,8» = «2,5—5,8»); ударение
// выкинуто. map[i] — индекс кодовой точки исходника для i-го символа
// результата.
function kbNorm(cps) {
  let out = '';
  const map = [];
  for (let i = 0; i < cps.length; i++) {
    let ch = cps[i].toLowerCase();
    if (ch === '́') continue;
    if (ch === 'ё') ch = 'е';
    else if (/[«»„“”‟"]/.test(ch)) ch = '"';
    else if (/[‘’‛`]/.test(ch)) ch = "'";
    else if (/[‐‑‒–—―−]/.test(ch)) ch = '-';
    else if (/\s/.test(ch)) {
      if (!out || out.endsWith(' ') || out.endsWith('-')) continue;
      ch = ' ';
    }
    if (ch === '-' && out.endsWith(' ')) { out = out.slice(0, -1); map.pop(); }
    out += ch;
    map.push(i);
  }
  return { s: out, map };
}

// kbGap — пропуск внутри цитаты, как у проверки кодом: «…», три точки и
// больше, в том числе в скобках («[…]», «(...)»).
const kbGap = /[\[(]?(?:…|\.{3,})[\])]?/;

// kbFindQuote — где в тексте (массив кодовых точек) цитата: [a, b) или
// null. Обрамляющие кавычки не ищутся. Сначала — цитата целиком; затем,
// как проверка кодом, части между пропусками по порядку, каждая без
// концевых « .,;:» — подсвечивается от начала первой до конца последней;
// если по порядку не нашлись — самый длинный кусок.
function kbFindQuote(cps, quote) {
  const clean = String(quote || '').trim().replace(/^[«"„“\s]+|[»"”\s]+$/g, '');
  if (!clean) return null;
  const t = kbNorm(cps);
  const norm = p => kbNorm(Array.from(p)).s.trim();
  const span = (a, b) => [t.map[a], t.map[b - 1] + 1];
  const whole = norm(clean);
  if (whole) {
    const i = t.s.indexOf(whole);
    if (i >= 0) return span(i, i + whole.length);
  }
  const parts = clean.split(kbGap).map(p => norm(p).replace(/^[\s.,;:]+|[\s.,;:]+$/g, '')).filter(Boolean);
  if (!parts.length) return null;
  let from = 0, first = -1, ok = true;
  for (const p of parts) {
    const i = t.s.indexOf(p, from);
    if (i < 0) { ok = false; break; }
    if (first < 0) first = i;
    from = i + p.length;
  }
  if (ok) return span(first, from);
  for (const p of parts.slice().sort((a, b) => b.length - a.length)) {
    if (p.length <= 3) continue;
    const i = t.s.indexOf(p);
    if (i >= 0) return span(i, i + p.length);
  }
  return null;
}

function kbScrollFocus() {
  if (!kb.focus || !$('kb-text')) return;
  const el = [...document.querySelectorAll('#kb-text .kb-chunk')].find(x => x.dataset.id === kb.focus);
  if (!el) return;
  const m = el.querySelector('mark.kb-qmark');
  (m || el).scrollIntoView({ block: 'center' });
  el.classList.add('flash');
  setTimeout(() => el.classList.remove('flash'), 1600);
}

async function kbShowChunks(paintFirst) {
  if (kb.tab !== 'chunks') return;
  if (paintFirst) kbPaint('body');
  await kbLoadDoc(kb.doc);
  if (kb.tab !== 'chunks') return;
  kbPaint('body');
  kbScrollFocus();
}

/* ---------- вкладка «Поиск» ---------- */

// kbPiped — включён ли второй этап поиска (конвейер retrieve).
function kbPiped(f) { f = f || kb.form; return !!(f.rewrite || f.rerank || f.filter); }
// kbPipeIndex — индекс конвейера: он ищет в одном; «оба рядом» → structure.
function kbPipeIndex(f) { f = f || kb.form; return f.index && f.index !== 'all' ? f.index : 'structure'; }

const kbRewriteText = { '': 'нет', code: 'код — синонимы и контекст', llm: 'модель — платно' };
const kbRerankText = { '': 'нет', hybrid: 'гибрид dense + BM25 (RRF)', llm: 'модель — платно' };

function kbSearchHintText() {
  if (!kbPiped()) return 'Запрос уходит сразу в оба индекса — выдачи рядом (или в один выбранный). dense без эмбеддера откатывается на BM25 и говорит об этом. Клик по попаданию — к чанку в тексте документа.';
  return `Второй этап: K0 кандидатов из индекса ${kbPipeIndex()} → реранкинг → фильтр релевантности → итог k. Переписанный запрос уходит только в поиск — модель отвечает на исходный. Клик по кандидату — к чанку.`;
}

function kbSearchHTML() {
  const f = kb.form;
  const ks = [1, 3, 5, 8, 10, 20];
  const k0s = [10, 20, 30, 50];
  const piped = kbPiped();
  const opt = (map, v) => Object.keys(map).map(k => `<option value="${esc(k)}"${k === v ? ' selected' : ''}>${esc(map[k])}</option>`).join('');
  return `<div id="kb-search" class="kb-search">
    <form id="kb-search-form" class="kb-form kb-search-form" data-submit="kbSearch" autocomplete="off">
      <div class="kb-form-row">
        <input type="text" id="kb-q" value="${esc(f.q)}" placeholder="чем питается харза" maxlength="500">
        <label class="lbl" for="kb-index">индекс</label>
        <select id="kb-index"><option value="all"${f.index === 'all' ? ' selected' : ''}>оба рядом</option>${kbStrategies().map(s => `<option value="${esc(s)}"${s === f.index ? ' selected' : ''}>${esc(s)}</option>`).join('')}</select>
        <label class="lbl" for="kb-k">k</label>
        <select id="kb-k">${ks.map(k => `<option value="${k}"${k === f.k ? ' selected' : ''}>${k}</option>`).join('')}</select>
        <label class="lbl" for="kb-mode">режим</label>
        <select id="kb-mode"${piped ? ' disabled title="у второго этапа — dense с откатом на BM25 и ранги обоих"' : ''}>
          <option value="dense"${f.mode === 'dense' ? ' selected' : ''}>dense — векторы</option>
          <option value="bm25"${f.mode === 'bm25' ? ' selected' : ''}>BM25 — слова</option>
        </select>
        <button type="submit" class="solid" id="kb-go"${kb.searching ? ' disabled' : ''}>${kb.searching ? '<span class="thinking">ищу</span>' : 'Найти'}</button>
      </div>
      <div class="kb-form-row kb-stage2${piped ? ' on' : ''}" id="kb-stage2">
        <span class="kb-stage2-l" title="конвейер retrieve: rewrite → кандидаты K0 → реранкинг → фильтр → k">второй этап</span>
        <label class="lbl" for="kb-rewrite">rewrite</label>
        <select id="kb-rewrite">${opt(kbRewriteText, f.rewrite)}</select>
        <label class="lbl" for="kb-rerank">реранкинг</label>
        <select id="kb-rerank">${opt(kbRerankText, f.rerank)}</select>
        <label class="kb-check" title="абсолютный порог косинуса, «не хуже лучшего на Δ» и отсев повторов текста"><input type="checkbox" id="kb-filter"${f.filter ? ' checked' : ''}> фильтр релевантности</label>
        <label class="lbl" for="kb-k0">K0</label>
        <select id="kb-k0">${k0s.map(k => `<option value="${k}"${k === f.k0 ? ' selected' : ''}>${k}</option>`).join('')}</select>
        <input type="text" id="kb-ctx" class="kb-ctx" value="${esc(f.ctx)}" placeholder="предыдущий вопрос — для продолжения «а сколько она весит?»" maxlength="500">
      </div>
    </form>
    <div class="hint" id="kb-search-hint">${esc(kbSearchHintText())}</div>
    ${kbResultsHTML()}
  </div>`;
}

function kbModeLine(info) {
  info = info || {};
  if (info.mode === 'dense') return `<span class="kb-mode dense">dense · ${esc(info.embedder || '')}</span>`;
  if (info.fallback) return `<span class="kb-mode fallback" title="${esc(info.fallback)}">BM25 — откат: ${esc(info.fallback)}</span>`;
  return `<span class="kb-mode bm25">BM25</span>`;
}

function kbResultsHTML() {
  const r = kb.result;
  if (!r) return '<div id="kb-results" class="kb-results-empty hint">Введите вопрос — например, «чем питается харза».</div>';
  if (!r.ok) {
    const d = r.data || {};
    return `<div id="kb-results"><div class="facts-error" id="kb-search-error" data-code="${esc(r.code)}">${esc(r.error)}${d.hint ? ' — ' + esc(d.hint) : ''}</div></div>`;
  }
  const res = kbList(r.data.results);
  if (res.length === 1 && (res[0].trace || r.piped)) {
    return `<div id="kb-results" class="kb-results kb-piped" data-query="${esc(r.data.query)}" style="--kb-cols:1">${kbTraceHTML(res[0])}</div>`;
  }
  return `<div id="kb-results" class="kb-results" data-query="${esc(r.data.query)}" style="--kb-cols:${Math.max(1, Math.min(res.length, 3))}">${res.map(x => {
    const info = x.info || {};
    const hits = kbList(x.hits);
    return `<section class="kb-result" data-index="${esc(info.index)}" data-mode="${esc(info.mode || '')}">
      <div class="kb-result-head"><b>${esc(info.index)}</b>${kbModeLine(info)}<span class="hint">${esc(typeof info.ms === 'number' ? info.ms.toFixed(1) + ' мс' : '')}</span></div>
      ${x.error ? `<div class="facts-error">${esc(x.error)}</div>` : ''}
      ${hits.length ? hits.map(h => `<div class="kb-hit" data-chunk="${esc(h.chunk_id)}" data-doc="${esc(h.doc_id)}" data-index="${esc(info.index)}" data-action="kbOpenChunk" data-arg="${esc(h.chunk_id)}" title="открыть чанк в тексте документа">
        <div class="kb-hit-head"><span class="kb-rank">${esc(h.rank)}</span><span class="kb-score">${esc(typeof h.score === 'number' ? h.score.toFixed(3) : '')}</span>
          <span class="kb-hit-where"><b>${esc(h.title)}</b> › ${esc(kbPath(h))}</span></div>
        <div class="kb-hit-text">${esc(kbCut(h.text, 300))}</div>
        <div class="kb-hit-foot"><code>${esc(h.chunk_id)}</code> · ${esc(h.tokens)} ток.${h.mixed ? ' · <span class="kb-mixed-t">на стыке разделов</span>' : ''}</div>
      </div>`).join('') : (x.error ? '' : '<p class="hint">Ничего не нашлось.</p>')}
    </section>`;
  }).join('')}</div>`;
}

/* ---------- v23: блок «до и после» второго этапа ---------- */

function kbNum3(f) { return typeof f === 'number' && isFinite(f) ? f.toFixed(3) : '—'; }

// kbRewrittenHTML — переписанный запрос: если он продолжает исходный,
// добавленное выделено. Обе части — через esc().
function kbRewrittenHTML(orig, rw) {
  if (rw.startsWith(orig) && rw.length > orig.length) {
    return `${esc(orig)}<mark class="kb-tr-add">${esc(rw.slice(orig.length))}</mark>`;
  }
  return esc(rw);
}

// kbTraceQueryHTML — исходный запрос → что ушло в поиск, раскрытые синонимы.
function kbTraceQueryHTML(t) {
  const orig = t.original || '';
  const rw = t.rewritten || orig;
  const changed = rw !== orig;
  const qs = kbList(t.queries).filter(x => x && x !== rw);
  const by = t.rewrite_by || ((t.config || {}).rewrite || '');
  return `<div class="kb-tr-q" data-changed="${changed ? 1 : 0}">
    <div class="kb-tr-box"><span class="kb-tr-l">исходный запрос — на него отвечает модель</span><div class="kb-tr-text" id="kb-original">${esc(orig)}</div></div>
    <span class="kb-tr-arrow" aria-hidden="true">→</span>
    <div class="kb-tr-box${changed ? ' new' : ''}"><span class="kb-tr-l">в поиск${by ? ' · rewrite ' + esc(by) : ''}${changed ? '' : ' — без изменений'}</span>
      <div class="kb-tr-text" id="kb-rewritten">${kbRewrittenHTML(orig, rw)}</div>
      ${kbList(t.expanded).length ? `<div class="kb-tr-exp">${t.expanded.map(e => `<span class="chip kb-expanded">${esc(e)}</span>`).join('')}</div>` : ''}
      ${qs.length ? `<div class="kb-tr-subq hint">подзапросы: ${qs.map(x => `<q>${esc(x)}</q>`).join(' · ')}</div>` : ''}
      ${kbList(t.queries_bm25).length ? `<div class="kb-tr-subq hint" id="kb-bm25q" title="латынь вида идёт только в BM25: dense от неё сбивается">в BM25: ${t.queries_bm25.map(x => `<q>${esc(x)}</q>`).join(' · ')}</div>` : ''}
    </div>
  </div>`;
}

// kbCosScale — шкала полос косинуса: от наименьшего до наибольшего среди
// кандидатов и порогов, с запасом.
function kbCosScale(vals) {
  const xs = vals.filter(v => typeof v === 'number' && v > 0);
  if (!xs.length) return null;
  const lo = Math.floor((Math.min(...xs) - 0.01) * 100) / 100;
  const hi = Math.ceil((Math.max(...xs) + 0.005) * 100) / 100;
  return v => (Math.max(0, Math.min(1, (v - lo) / ((hi - lo) || 1))) * 100).toFixed(1) + '%';
}

// kbTraceHTML — путь поиска через конвейер: запрос до и после, сводка,
// плашка «пусто», таблица кандидатов с судьбой каждого.
function kbTraceHTML(x) {
  const t = x.trace;
  const info = x.info || {};
  if (!t) {
    return `<section class="kb-trace" id="kb-trace" data-index="${esc(info.index)}">${x.error ? `<div class="facts-error" id="kb-trace-error">${esc(x.error)}</div>` : '<p class="hint">Конвейер не вернул пути поиска.</p>'}</section>`;
  }
  const c = t.config || {};
  const cands = kbList(t.candidates).slice().sort((a, b) => (a.final || 999) - (b.final || 999) || (a.rank_dense || 999) - (b.rank_dense || 999));
  const kept = cands.filter(y => y.kept).length;
  const dense = (t.info || info).mode === 'dense';
  const thr = typeof t.min_score === 'number' && t.min_score > 0 ? t.min_score : null;
  const delta = typeof c.delta === 'number' && c.delta > 0 ? c.delta : 0.05;
  const rel = dense && c.filter && t.top_dense > 0 ? t.top_dense - delta : null;
  const pos = kbCosScale(cands.map(y => y.dense).concat(thr && dense ? [thr] : [], rel ? [rel] : []));
  const rerank = c.rerank || '';
  const cost = t.cost && t.cost.usd > 0 ? ' · ' + kbCost(t.cost) : '';
  const line = `<div class="kb-tr-line" id="kb-tr-line">
    <span>кандидатов <b id="kb-k0-n">${esc(cands.length)}</b> → осталось <b id="kb-k1-n">${esc(kept)}</b></span>
    ${c.filter ? `<span>порог <b>${esc(thr ? thr.toFixed(3) : '—')}</b>${t.min_score_from ? ` <span class="hint" id="kb-thr-from">(${esc(t.min_score_from)})</span>` : ''} · Δ <b>${esc(delta.toFixed(2))}</b>${rel ? ` <span class="hint">(не хуже ${esc(rel.toFixed(3))})</span>` : ''}</span>` : '<span class="hint">фильтр выключен — отсечение только за пределами k</span>'}
    ${dense && t.top_dense > 0 ? `<span id="kb-top-gap" title="лучший косинус кандидатов и его отрыв от второго (top1 − top2)">лучший <b>${esc(kbNum3(t.top_dense))}</b> · отрыв <b>${esc(kbNum3(t.gap))}</b></span>` : ''}
    <span>${kbModeLine(t.info || info)}</span>
    <span>реранкинг <b>${esc(rerank ? kbRerankText[rerank] || rerank : 'нет')}</b></span>
    <span class="hint">индекс ${esc(info.index || c.Index || '')} · ${esc(kbMs(t.ms))}${esc(cost)}</span>
  </div>`;
  const anchored = kbList(t.anchored);
  const empty = t.empty ? `<div class="kb-empty" id="kb-empty"><b>Пусто после фильтра</b> — лучший косинус ${esc(kbNum3(t.top_dense))}${thr && !anchored.length ? ' ниже порога ' + esc(thr.toFixed(3)) : ''}: в базе ответа, вероятно, нет. Модель получит «в базе знаний ничего не найдено».</div>` : '';
  // Якорь: вид назван в самой реплике — абсолютный пол к запросу не
  // применяется (статья о виде в базе есть; нужен ли аспект — дело ответа).
  const anchor = anchored.length && c.filter && dense ? `<div class="kb-tr-note kb-anchor" id="kb-anchored" title="пол косинуса отделяет «в базе есть» от «в базе нет»; статья о названном виде в базе есть точно — относительный порог и отсев повторов действуют"><b>Вид назван в запросе</b> (${esc(anchored.join(', '))}) — абсолютный порог не применяется.</div>` : '';
  const head = `<tr><th class="kb-r" title="ранг после реранкинга">№</th>
    <th class="kb-cand-cos">косинус dense${thr && dense ? ` <span class="kb-thr-key" title="черта — абсолютный порог; пунктир — «не хуже лучшего на Δ»">│ порог ${esc(thr.toFixed(3))}</span>` : ''}</th>
    <th class="kb-r" title="ранг в выдаче dense">dense</th><th class="kb-r" title="ранг в выдаче BM25">BM25</th>
    <th class="kb-r" title="${rerank === 'llm' ? 'оценка модели 0–3' : 'RRF рангов dense и BM25, k = 60'}">${rerank === 'llm' ? 'модель' : 'RRF'}</th>
    <th>статья › раздел</th><th>судьба</th></tr>`;
  const rows = cands.map(y => {
    const cos = y.dense > 0 && pos
      ? `<div class="kb-cos" title="косинус dense ${esc(kbNum3(y.dense))}"><span class="kb-cos-fill${thr && y.dense < thr ? ' low' : ''}" style="width:${pos(y.dense)}"></span>${thr && dense ? `<span class="kb-cos-thr" style="left:${pos(thr)}"></span>` : ''}${rel ? `<span class="kb-cos-rel" style="left:${pos(rel)}"></span>` : ''}</div><span class="kb-cos-v">${esc(kbNum3(y.dense))}</span>`
      : '<span class="hint" title="кандидата не было в выдаче dense">—</span>';
    const fate = y.kept ? '<span class="chip ok kb-kept">в итоге</span>' : `<span class="kb-reason">${esc(y.reason || 'отсечён')}</span>`;
    return `<tr class="kb-cand ${y.kept ? 'kept' : 'cut'}" data-chunk="${esc(y.chunk_id)}" data-doc="${esc(y.doc_id)}" data-index="${esc(y.strategy || info.index || '')}" data-kept="${y.kept ? 1 : 0}" data-action="kbOpenChunk" data-arg="${esc(y.chunk_id)}" title="${esc(kbCut(y.text, 400))}">
      <td class="kb-r kb-cand-rank">${esc(y.final || '—')}</td>
      <td class="kb-cand-cos"><div class="kb-cos-cell">${cos}</div></td>
      <td class="kb-r">${esc(y.rank_dense || '—')}</td><td class="kb-r">${esc(y.rank_bm25 || '—')}</td>
      <td class="kb-r">${y.rerank ? esc(rerank === 'llm' ? String(y.rerank) : y.rerank.toFixed(4)) : '—'}</td>
      <td class="kb-cand-where"><b>${esc(y.title)}</b> › ${esc(kbPath(y))}<div class="kb-cand-text">${esc(kbCut(y.text, 140))}</div></td>
      <td class="kb-cand-fate">${fate}</td></tr>`;
  }).join('');
  return `<section class="kb-trace" id="kb-trace" data-index="${esc(info.index)}" data-empty="${t.empty ? 1 : 0}">
    ${kbTraceQueryHTML(t)}
    ${line}
    ${t.note ? `<div class="kb-tr-note">${esc(t.note)}</div>` : ''}
    ${anchor}
    ${x.error ? `<div class="facts-error">${esc(x.error)}</div>` : ''}
    ${empty}
    ${cands.length ? `<table class="grid kb-table kb-cands" id="kb-cands">${head}${rows}</table>` : '<p class="hint">Кандидатов нет — поиск ничего не нашёл.</p>'}
  </section>`;
}

function kbReadForm() {
  const q = $('kb-q'), k = $('kb-k'), m = $('kb-mode'), ix = $('kb-index');
  const rw = $('kb-rewrite'), rr = $('kb-rerank'), fl = $('kb-filter'), k0 = $('kb-k0'), ctx = $('kb-ctx');
  if (q) kb.form.q = q.value;
  if (k) kb.form.k = Number(k.value) || 5;
  if (m) kb.form.mode = m.value;
  if (ix) kb.form.index = ix.value || 'all';
  if (rw) kb.form.rewrite = rw.value;
  if (rr) kb.form.rerank = rr.value;
  if (fl) kb.form.filter = fl.checked;
  if (k0) kb.form.k0 = Number(k0.value) || 20;
  if (ctx) kb.form.ctx = ctx.value;
  // Второй этап включили или выключили — подсказка и режим поиска следом.
  const piped = kbPiped();
  if (m) m.disabled = piped;
  if ($('kb-stage2')) $('kb-stage2').classList.toggle('on', piped);
  if ($('kb-search-hint')) $('kb-search-hint').textContent = kbSearchHintText();
}

async function kbSearch() {
  if (kb.searching) return;
  kbReadForm();
  const f = kb.form;
  const q = f.q.trim();
  if (!q) { $('kb-q') && $('kb-q').focus(); return; }
  kb.searching = true;
  kbPaint();
  const piped = kbPiped();
  const qs = new URLSearchParams({ q, k: String(f.k) });
  if (piped) {
    // Конвейер — один индекс; K0 не меньше итога.
    qs.set('index', kbPipeIndex());
    if (f.rewrite) qs.set('rewrite', f.rewrite);
    if (f.rerank) qs.set('rerank', f.rerank);
    qs.set('filter', f.filter ? '1' : '0');
    qs.set('k0', String(Math.max(f.k0, f.k)));
    if (f.ctx.trim()) qs.append('context', f.ctx.trim());
  } else {
    qs.set('mode', f.mode);
    if (f.index && f.index !== 'all') qs.set('index', f.index);
  }
  const r = await factsAPI('GET', '/api/kb/search?' + qs.toString());
  r.piped = piped;
  kb.result = r;
  kb.searching = false;
  kbPaint('results');
}

document.addEventListener('input', ev => { if (ev.target.closest && ev.target.closest('#kb-search-form')) kbReadForm(); });
document.addEventListener('change', ev => { if (ev.target.closest && ev.target.closest('#kb-search-form')) kbReadForm(); });

/* ---------- вкладка «Сравнение» ---------- */

function kbReportHTML() {
  const r = kb.report;
  if (!r) return '<div id="kb-report"><p class="hint">загружаю…</p></div>';
  if (!r.ok) {
    if (r.code === 404) {
      return `<div id="kb-report" class="kb-report"><div class="facts-down" id="kb-report-none"><b>Отчёта сравнения ещё нет.</b>
        <div>Соберите его: <code>${esc(kbEval)}</code> — он прогонит контрольные вопросы по обоим индексам и сохранит таблицы в базу (и в examples/kb/chunking.md).</div></div></div>`;
    }
    return `<div id="kb-report"><div class="facts-error">${esc(r.error)}</div></div>`;
  }
  const d = r.data;
  const created = d.created ? new Date(d.created).toLocaleString('ru-RU', { day: '2-digit', month: '2-digit', year: 'numeric', hour: '2-digit', minute: '2-digit' }) : '—';
  let html = `<div id="kb-report" class="kb-report">
    <div class="kb-report-meta" id="kb-report-meta">
      <span>отчёт от <b id="kb-report-date">${esc(created)}</b></span>
      <span>эмбеддер <b>${esc(d.embedder || '—')}</b></span>
      <span>corpus_sha <code title="${esc(d.corpus_sha)}">${esc(kbShort(d.corpus_sha))}</code></span>
      <span>документов ${esc(d.docs)}, страниц ${esc(kbPages(d.pages))}</span>
      <span>бюджет топа ${esc(d.budget)} ток.</span>
    </div>`;
  const concl = kbList(d.conclusion);
  if (concl.length) {
    html += `<h4 class="kb-h">Вывод</h4><ul class="kb-conclusion" id="kb-conclusion">${concl.map(x => `<li>${esc(x)}</li>`).join('')}</ul>`;
  }
  const stats = kbList(d.stats);
  html += `<h4 class="kb-h">Структура индексов</h4>
    <table class="grid kb-table" id="kb-stats-table"><tr><th>индекс</th><th>параметры</th><th class="kb-r">чанков</th><th class="kb-r">токенов</th><th class="kb-r">p50</th><th class="kb-r">p95</th>
      <th class="kb-r">на стыке разделов</th><th class="kb-r">разрезано разделов</th><th class="kb-r">перекрытие</th><th class="kb-r">оборвано посреди предложения</th><th class="kb-r">сборка, с</th><th class="kb-r">размер, КБ</th></tr>
    ${stats.map(s => `<tr class="kb-stat-row" data-index="${esc(s.index)}"><td><b>${esc(s.index)}</b></td><td>${esc(kbParams(s.params))}</td>
      <td class="kb-r">${esc(num(s.chunks))}</td><td class="kb-r">${esc(num(s.tokens))}</td><td class="kb-r">${esc(s.p50_tokens)}</td><td class="kb-r">${esc(s.p95_tokens)}</td>
      <td class="kb-r">${esc(kbPct(s.mixed_share))}</td><td class="kb-r">${esc(kbPct(s.split_sections))}</td><td class="kb-r">${esc(kbPct(s.overlap_share))}</td><td class="kb-r">${esc(kbPct(s.mid_sentence))}</td>
      <td class="kb-r">${esc(typeof s.build_seconds === 'number' ? s.build_seconds.toFixed(1) : '—')}</td><td class="kb-r">${esc(num(Math.round((s.bytes || 0) / 1024)))}</td></tr>`).join('')}</table>`;
  const ret = kbList(d.retrieval);
  // Лучшее значение колонки (среди строк того же набора и режима) — жирным.
  const best = (x, f) => {
    const peers = ret.filter(y => y.split === x.split && y.mode === x.mode).map(f);
    return peers.length > 1 && f(x) === Math.max(...peers) ? ' best' : '';
  };
  const rc = k => x => (x.recall || {})[k] || 0;
  html += `<h4 class="kb-h">Поиск по наборам вопросов</h4>
    <table class="grid kb-table" id="kb-retrieval-table"><tr><th>индекс</th><th>режим</th><th>набор</th><th class="kb-r">вопросов</th><th class="kb-r">разорвано доказательств</th>
      <th class="kb-r">recall@1</th><th class="kb-r">recall@3</th><th class="kb-r">recall@5</th><th class="kb-r">MRR</th><th class="kb-r">recall при ${esc(d.budget)} ток.</th></tr>
    ${ret.map(x => `<tr class="kb-ret-row" data-index="${esc(x.index)}" data-mode="${esc(x.mode)}" data-split="${esc(x.split)}">
      <td><b>${esc(x.index)}</b></td><td>${esc(x.mode)}${x.fallback ? ` <span class="chip warn" title="${esc(x.fallback)}">откат</span>` : ''}</td><td>${esc(x.split)}</td>
      <td class="kb-r">${esc(x.n)}</td><td class="kb-r">${esc(kbPct(x.broken_evidence))}</td>
      <td class="kb-r${best(x, rc(1))}">${esc(kbNum2(rc(1)(x)))}</td><td class="kb-r${best(x, rc(3))}">${esc(kbNum2(rc(3)(x)))}</td><td class="kb-r${best(x, rc(5))}">${esc(kbNum2(rc(5)(x)))}</td>
      <td class="kb-r${best(x, y => y.mrr || 0)}">${esc(kbNum2(x.mrr))}</td><td class="kb-r${best(x, y => y.recall_budget || 0)}">${esc(kbNum2(x.recall_budget))}</td></tr>`).join('')}</table>`;
  html += kbQuestionsHTML(ret);
  return html + '</div>';
}

// kbQuestionsHTML — вопросы test: ранг первого релевантного чанка в
// каждом индексе и режиме (— — нет в топ-20).
function kbQuestionsHTML(ret) {
  const cols = ret.filter(x => x.split === 'test');
  if (!cols.length) return '';
  const qs = [];
  for (const c of cols) for (const row of kbList(c.rows)) if (!qs.some(q => q.id === row.id)) qs.push(row);
  if (!qs.length) return '';
  return `<details class="kb-questions"><summary>Вопросы test: ранг первого релевантного чанка</summary>
    <table class="grid kb-table"><tr><th>id</th><th>вопрос</th>${cols.map(c => `<th class="kb-r">${esc(c.index)} ${esc(c.mode)}</th>`).join('')}</tr>
    ${qs.map(q => `<tr><td>${esc(q.id)}</td><td>${esc(kbCut(q.q, 110))}</td>${cols.map(c => {
      const row = kbList(c.rows).find(x => x.id === q.id);
      const rank = row ? row.rank : 0;
      return `<td class="kb-r${rank === 1 ? ' best' : ''}">${rank ? esc(rank) : '—'}</td>`;
    }).join('')}</tr>`).join('')}</table></details>`;
}

/* ---------- v22: общее для «Спросить» и «Контрольных вопросов» ---------- */

// v23: три режима с конвейером retrieve — модель и промпт те же, что у rag,
// отличается только, какие фрагменты дошли.
// v24: rag+cite — поиск как у rag+both, ответ — kb_answer с источниками и
// цитатами, проверенный кодом.
const kbModes = ['norag', 'rag', 'rag+filter', 'rag+rewrite', 'rag+both', 'rag+cite'];
const kbModeTitle = {
  norag: 'Без базы', rag: 'С базой (RAG)',
  'rag+filter': 'RAG + фильтр', 'rag+rewrite': 'RAG + rewrite', 'rag+both': 'RAG + rewrite + фильтр',
  'rag+cite': 'RAG + источники и цитаты',
};
const kbModeHint = {
  norag: 'модель отвечает по памяти: системный промпт и вопрос',
  rag: 'тот же системный промпт и вопрос плюс найденные фрагменты базы',
  'rag+filter': 'кандидаты K0 → фильтр релевантности и отсев повторов текста → итог',
  'rag+rewrite': 'запрос переписан кодом (синонимы, контекст) — переписанный идёт только в поиск',
  'rag+both': 'rewrite кодом + фильтр релевантности',
  'rag+cite': 'как rag+both, ответ — источники и дословные цитаты, проверенные кодом; «не знаю» при слабом контексте решает код',
};
const kbMaxAskModes = 3;
function kbPipedMode(m) { return m === 'rag+filter' || m === 'rag+rewrite' || m === 'rag+both' || m === 'rag+cite'; }
// kbSortModes — режимы в порядке kbModes, без повторов и неизвестных.
function kbSortModes(ms) { return kbModes.filter(m => ms.includes(m)); }

// kbModePickHTML — флажки режимов; max — предел выбранных (остальные
// флажки выключаются).
function kbModePickHTML(id, sel, max) {
  return `<span class="kb-modes-pick" id="${esc(id)}" role="group" aria-label="режимы ответа"><span class="lbl">режимы</span>${kbModes.map(m => {
    const on = sel.includes(m);
    const off = !on && max && sel.length >= max;
    return `<label class="kb-mode-pick${on ? ' on' : ''}${off ? ' off' : ''}" data-mode="${esc(m)}" title="${esc(kbModeHint[m] + (off ? ' — рядом не больше ' + max + ' режимов' : ''))}"><input type="checkbox" data-kb-mode="${esc(m)}" value="${esc(m)}"${on ? ' checked' : ''}${off ? ' disabled' : ''}>${esc(m)}</label>`;
  }).join('')}</span>`;
}

// Флажок режима: хотя бы один режим остаётся выбранным.
document.addEventListener('change', ev => {
  const el = ev.target;
  if (!el || !el.dataset || !el.dataset.kbMode) return;
  const box = el.closest('#kb-ask-modes, #kb-qa-modes');
  if (!box) return;
  const key = box.id === 'kb-ask-modes' ? 'askModes' : 'qaModes';
  const sel = kbSortModes([...box.querySelectorAll('[data-kb-mode]')].filter(x => x.checked).map(x => x.dataset.kbMode));
  if (sel.length) kb[key] = sel;
  box.outerHTML = kbModePickHTML(box.id, kb[key], key === 'askModes' ? kbMaxAskModes : 0);
  if (key === 'qaModes' && $('kb-qa-cost')) $('kb-qa-cost').textContent = kbQaCostText();
  if (key === 'askModes' && $('kb-ask-out') && !kb.ask && !kb.asking) $('kb-ask-out').outerHTML = kbAskOutHTML();
});
// Значок и слово вердикта; у «не знаю» значок — сами слова.
const kbVerdicts = {
  correct: ['✓', 'верно'],
  partial: ['½', 'частично'],
  wrong: ['✗', 'неверно'],
  abstain: ['не знаю', 'не знаю'],
};
const kbSplitTitle = { test: 'test — контрольные', dev: 'dev — подбор', out: 'out — вне базы' };
// chunk_id — «doc/strategy/ord»: «manul/structure/004».
const kbChunkRe = /^[a-z0-9][a-z0-9_.-]*\/[a-z0-9_-]+\/\d{3}$/i;

function kbQuestionList() { return kb.questions && kb.questions.ok ? kbList(kb.questions.data) : []; }
function kbQuestion(id) { return id ? kbQuestionList().find(q => q.id === id) || null : null; }
function kbCost(c) {
  if (!c || typeof c.usd !== 'number') return '—';
  return (c.known ? '' : '≈') + factsUSD(c.usd);
}
function kbMs(ms) { return typeof ms === 'number' && ms > 0 ? (ms >= 1000 ? (ms / 1000).toFixed(1) + ' с' : ms + ' мс') : '—'; }
function kbSources(q) {
  return kbList(q && q.sources).map(s => s.doc_id + (s.section ? ' › ' + s.section : '')).join('; ');
}

async function kbLoadQuestions() {
  if (kb.questions && kb.questions.ok) return;
  kb.questions = await factsAPI('GET', '/api/kb/questions');
}

// kbVerdictHTML — вердикт режима; long — со словом. Пусто — ответа ещё нет.
function kbVerdictHTML(mode, v, title, long) {
  if (!v) return `<span class="kb-verdict pending" data-mode="${esc(mode)}" data-verdict="" title="ответа ещё нет">…</span>`;
  const [icon, label] = kbVerdicts[v] || ['?', v];
  const word = long && v !== 'abstain' ? ` <span class="kb-verdict-l">${esc(label)}</span>` : '';
  return `<span class="kb-verdict ${esc(v)}" data-mode="${esc(mode)}" data-verdict="${esc(v)}" title="${esc(title || label)}">${esc(icon)}${word}</span>`;
}

// kbAnswerTextHTML — ответ модели как данные: esc(), переносы строк — CSS
// (pre-wrap); ссылки [chunk_id] (и [a, b]) — кнопки к чанку. Ссылка на
// фрагмент, которого не было в выдаче, помечена: модель могла её выдумать.
function kbAnswerTextHTML(text, hits) {
  const known = new Set(kbList(hits).map(h => h.chunk_id));
  return esc(text || '').replace(/\[([^[\]\n]{3,300})\]/g, (all, inner) => {
    const ids = inner.split(/\s*[,;]\s*/);
    if (!ids.every(x => kbChunkRe.test(x))) return all;
    return ids.map(id => kbCiteHTML(id, known.has(id))).join(' ');
  });
}
function kbCiteHTML(id, found) {
  const [doc, index] = id.split('/');
  const title = found ? 'открыть фрагмент в тексте документа' : 'этого фрагмента не было в выдаче — модель могла его выдумать';
  return `<button type="button" class="kb-cite${found ? '' : ' unknown'}" data-chunk="${esc(id)}" data-doc="${esc(doc)}" data-index="${esc(index)}" data-action="kbOpenChunk" data-arg="${esc(id)}" title="${esc(title)}">[${esc(id)}]</button>`;
}

// kbRuleTitle — правило словами: что нашлось и чего нет.
function kbRuleTitle(r) {
  if (!r) return '';
  const parts = ['правило: ' + ((kbVerdicts[r.verdict] || [])[1] || r.verdict || '—')];
  if (kbList(r.hit).length) parts.push('нашлось: ' + r.hit.join(', '));
  if (kbList(r.miss).length) parts.push('нет: ' + r.miss.join(', '));
  if (kbList(r.bad).length) parts.push('лишнее: ' + r.bad.join(', '));
  if (kbList(r.numbers).length) parts.push('числа: ' + r.numbers.join(', '));
  if (r.note) parts.push(r.note);
  return parts.join('; ');
}
function kbRuleHTML(r) {
  if (!r || !r.verdict) return '';
  const bits = [];
  if (kbList(r.hit).length) bits.push(`нашлось <b>${esc(r.hit.join(', '))}</b>`);
  if (kbList(r.miss).length) bits.push(`нет <b>${esc(r.miss.join(', '))}</b>`);
  if (kbList(r.bad).length) bits.push(`лишнее <b>${esc(r.bad.join(', '))}</b>`);
  if (kbList(r.numbers).length) bits.push(`числа ${esc(r.numbers.join(', '))}`);
  if (r.note) bits.push(esc(r.note));
  return `<div class="kb-rule">правило: ${bits.join(' · ') || esc((kbVerdicts[r.verdict] || [])[1] || r.verdict)}</div>`;
}

// kbExpectHTML — ожидание вопроса из набора.
function kbExpectHTML(q, id) {
  if (!q) return '';
  const e = q.expect || {};
  const note = e.note || q.note || (q.answerable ? '' : 'в базе ответа нет — правильный исход «не знаю»');
  const must = kbList(e.must).map(g => kbList(g).join(' / ')).filter(Boolean);
  return `<div class="kb-expect"${id ? ` id="${esc(id)}"` : ''}>
    <div><b>${esc(q.id)}</b> <span class="chip">${esc(q.type)}</span>${q.answerable ? '' : ' <span class="chip warn">неотвечаемый</span>'} <span class="kb-expect-l">Ожидание:</span> ${esc(note || '—')}</div>
    ${must.length ? `<div class="hint">правило ищет: ${must.map(x => `<code>${esc(x)}</code>`).join(' · ')}</div>` : ''}
    ${kbSources(q) ? `<div class="hint">источники: ${esc(kbSources(q))}</div>` : ''}
  </div>`;
}

/* ---------- вкладка «Спросить» ---------- */

function kbAskHTML() {
  const f = kb.askForm;
  const qs = kbQuestionList();
  const groups = ['test', 'dev', 'out'].map(s => [s, qs.filter(q => q.split === s)]).filter(([, l]) => l.length);
  return `<div id="kb-ask" class="kb-ask">
    <form id="kb-ask-form" class="kb-form kb-ask-form" data-submit="kbAsk" autocomplete="off">
      <select id="kb-ask-pick" data-change="kbAskPick" title="вопрос из набора: подставит текст, ответы оценит правило">
        <option value="">свой вопрос</option>
        ${groups.map(([s, l]) => `<optgroup label="${esc(kbSplitTitle[s] || s)}">${l.map(q => `<option value="${esc(q.id)}"${q.id === f.qid ? ' selected' : ''}>${esc(q.id + ' · ' + kbCut(q.q, 64))}</option>`).join('')}</optgroup>`).join('')}
      </select>
      <input type="text" id="kb-ask-q" value="${esc(f.q)}" placeholder="сколько видов малых панд признаёт MDD v2.5" maxlength="500">
      <button type="submit" class="solid" id="kb-ask-go"${kb.asking ? ' disabled' : ''}>${kb.asking ? '<span class="thinking">думает</span>' : 'Спросить'}</button>
      ${kbModePickHTML('kb-ask-modes', kb.askModes, kbMaxAskModes)}
    </form>
    ${kbAskContextHTML()}
    ${kb.questions && !kb.questions.ok ? `<div class="hint" id="kb-ask-noset">набора вопросов нет: ${esc(kb.questions.error)}${kb.questions.data && kb.questions.data.hint ? ' — ' + esc(kb.questions.data.hint) : ''}</div>` : ''}
    <div class="hint">Все режимы — один агент без инструментов и один системный промпт; режимы с базой получают ещё найденные фрагменты, rag+… — после второго этапа поиска. Рядом — до трёх режимов. Ссылка [chunk_id] в ответе — к чанку в тексте документа.</div>
    ${kbAskOutHTML()}
  </div>`;
}

// kbAskContextHTML — у вопроса-продолжения: что человек спросил перед ним.
function kbAskContextHTML() {
  const q = kbQuestion(kb.askForm.qid);
  const ctx = kbList(q && q.context);
  if (!ctx.length) return '<div id="kb-ask-context" hidden></div>';
  return `<div id="kb-ask-context" class="kb-ask-ctx">Вопрос-продолжение. Перед ним человек спросил: ${ctx.map(c => `<q>${esc(c)}</q>`).join(' → ')} — эти реплики уходят модели историей, а в поиск — вместе с вопросом.</div>`;
}

function kbAskOutHTML() {
  const n = kb.askModes.length;
  if (kb.asking) {
    return `<div id="kb-ask-out" class="kb-ask-out"><div class="kb-thinking"><span class="thinking">модель отвечает в ${esc(plural(n, 'режиме', 'режимах', 'режимах'))} — ${esc(kb.askModes.join(', '))}</span></div></div>`;
  }
  const r = kb.ask;
  if (!r) {
    return `<div id="kb-ask-out" class="kb-ask-out kb-results-empty hint">Выберите вопрос из набора — например, T03 о малых пандах в MDD v2.5 — или задайте свой. Платно: ${esc(plural(n, 'запрос', 'запроса', 'запросов'))} к модели.</div>`;
  }
  const d = r.data || {};
  if (!r.ok && r.code === 503) {
    return `<div id="kb-ask-out" class="kb-ask-out"><div class="facts-down kb-off" id="kb-ask-off"><b>Спросить нельзя.</b>
      <div class="kb-why">${esc(d.why || r.error)}</div>${d.hint ? `<div class="facts-hint">${esc(d.hint)}</div>` : ''}</div></div>`;
  }
  const answers = kbList(d.answers);
  if (!r.ok && !answers.length) {
    return `<div id="kb-ask-out" class="kb-ask-out"><div class="facts-error" id="kb-ask-error">${esc(r.error)}</div></div>`;
  }
  const runs = kbList(d.runs);
  const errs = String(d.error || '').split('; ').filter(Boolean);
  const errFor = m => (errs.find(e => e.startsWith(m + ': ')) || '').slice(m.length + 2);
  const q = kbQuestion(r.qid);
  return `<div id="kb-ask-out" class="kb-ask-out" data-q="${esc(d.q)}" data-qid="${esc(r.qid || '')}">
    ${kbExpectHTML(q, 'kb-ask-expect')}
    ${d.note ? `<div class="hint kb-ask-note" id="kb-ask-note">${esc(d.note)}</div>` : ''}
    <div class="kb-answers" id="kb-answers" style="--kb-cols:${Math.max(1, Math.min(answers.length, 3))}">${answers.map(a =>
      kbAnswerHTML(a, runs.find(x => x.answer && x.answer.mode === a.mode), q, errFor(a.mode))).join('')}</div>
    ${kbPromptHTML(answers)}
  </div>`;
}

// kbAnswerHTML — колонка режима: ответ, вердикт правила, цена; у rag —
// найденные фрагменты.
function kbAnswerHTML(a, run, q, err) {
  const u = a.usage || {};
  const meta = err ? '' : `<span class="kb-answer-meta" title="цена · токены вопроса → ответа · время">${esc(kbCost(a.cost))} · ${esc(num(u.prompt))} → ${esc(num(u.completion))} ток. · ${esc(kbMs(a.ms))}</span>`;
  return `<section class="kb-answer" data-mode="${esc(a.mode)}">
    <div class="kb-answer-head"><b>${esc(kbModeTitle[a.mode] || a.mode)}</b><code>${esc(a.mode)}</code>
      ${run ? kbVerdictHTML(a.mode, run.final, kbRuleTitle(run.rule), true) : ''}${meta}</div>
    <div class="kb-answer-sub hint">${esc(kbModeHint[a.mode] || '')}</div>
    ${err ? `<div class="facts-error kb-answer-error">${esc(err)}</div>` : a.cited ? kbCitedHTML(a) : `<div class="kb-answer-text">${kbAnswerTextHTML(a.text, a.hits)}</div>`}
    ${run ? kbRuleHTML(run.rule) : ''}
    ${a.mode !== 'norag' && !err ? kbSourcesHTML(a, run, q) : ''}
  </section>`;
}

// kbSourcesHTML — выдача поиска под ответом rag: ранг, балл, статья ›
// раздел, режим поиска (dense или BM25 с причиной отката); фрагменты, на
// которые ответ сослался, и фрагменты из источников вопроса помечены.
function kbSourcesHTML(a, run, q) {
  const hits = kbList(a.hits);
  const s = a.search || {};
  const want = new Set(kbList(q && q.sources).map(x => x.doc_id));
  let recall = '';
  if (run && q && kbList(q.evidence).length) {
    recall = run.recall ? '<span class="chip ok kb-recall" data-recall="1">доказательство в выдаче ✓</span>'
      : '<span class="chip bad kb-recall" data-recall="0">доказательства в выдаче нет ✗</span>';
  }
  return `<div class="kb-srcs">
    <div class="kb-srcs-head"><b>Найденные фрагменты</b>${s.index ? ` <code>${esc(s.index)}</code>` : ''} ${kbModeLine(s)}${recall}</div>
    ${a.trace ? kbTraceBriefHTML(a.trace) : ''}
    ${hits.length ? hits.map(h => {
      const cited = (a.text || '').includes(h.chunk_id) || kbCitedIDs(a).has(h.chunk_id);
      return `<div class="kb-src${cited ? ' cited' : ''}${want.has(h.doc_id) ? ' want' : ''}" data-chunk="${esc(h.chunk_id)}" data-doc="${esc(h.doc_id)}" data-index="${esc(h.strategy || s.index || '')}" data-action="kbOpenChunk" data-arg="${esc(h.chunk_id)}" title="открыть чанк в тексте документа">
        <div class="kb-hit-head"><span class="kb-rank">${esc(h.rank)}</span><span class="kb-score">${esc(typeof h.score === 'number' ? h.score.toFixed(3) : '')}</span>
          <span class="kb-hit-where"><b>${esc(h.title)}</b> › ${esc(kbPath(h))}</span>${cited ? '<span class="chip ok kb-cited">в ответе</span>' : ''}</div>
        <div class="kb-hit-text">${esc(kbCut(h.text, 200))}</div>
        <div class="kb-hit-foot"><code>${esc(h.chunk_id)}</code> · ${esc(h.tokens)} ток.</div>
      </div>`;
    }).join('') : `<p class="hint">${a.trace && a.trace.empty ? 'Фильтр отсёк всех кандидатов' : 'Поиск ничего не нашёл'} — модель получила строку «в базе знаний ничего не найдено».</p>`}
  </div>`;
}

// kbTraceBriefHTML — путь поиска режима с конвейером коротко: что ушло в
// поиск, сколько осталось из K0, пусто ли после фильтра.
function kbTraceBriefHTML(t) {
  const orig = t.original || '';
  const rw = t.rewritten || orig;
  const cands = kbList(t.candidates);
  const kept = cands.filter(c => c.kept).length || kbList(t.hits).length;
  return `<div class="kb-tb" data-empty="${t.empty ? 1 : 0}" data-changed="${rw !== orig ? 1 : 0}">
    ${rw !== orig ? `<div class="kb-tb-rw">в поиск: <q class="kb-tb-q">${kbRewrittenHTML(orig, rw)}</q>${kbList(t.expanded).map(e => ` <span class="chip kb-expanded">${esc(e)}</span>`).join('')}</div>` : '<div class="kb-tb-rw hint">запрос не переписан</div>'}
    <div class="kb-tb-n">осталось <b>${esc(kept)}</b> из ${esc(cands.length)} кандидатов${typeof t.min_score === 'number' && t.min_score > 0 && (t.config || {}).filter ? ` · порог ${esc(t.min_score.toFixed(3))}${kbList(t.anchored).length ? ' (не применяется: вид назван в запросе)' : ''}` : ''}${typeof t.top_dense === 'number' && t.top_dense > 0 ? ` · лучший косинус ${esc(t.top_dense.toFixed(3))}${typeof t.gap === 'number' ? ', отрыв ' + esc(t.gap.toFixed(3)) : ''}` : ''}</div>
    ${t.empty ? '<div class="kb-tb-empty">пусто после фильтра — в базе ответа, вероятно, нет</div>' : ''}
  </div>`;
}

// kbPromptHTML — что ушло модели: видно, что режимы отличаются только
// контекстом.
function kbPromptHTML(answers) {
  const xs = answers.filter(a => a.system || a.user);
  if (!xs.length) return '';
  const same = xs.length > 1 && xs.every(a => a.system === xs[0].system);
  const head = same ? 'системный промпт одинаковый — режимы отличаются только контекстом в сообщении пользователя' : 'System и User по режимам';
  return `<details class="kb-prompt" id="kb-ask-prompt"><summary>Что ушло модели — ${esc(head)}</summary>
    <div class="kb-prompt-cols" style="--kb-cols:${Math.max(1, Math.min(xs.length, 3))}">${xs.map(a => `<div class="kb-prompt-col" data-mode="${esc(a.mode)}">
      <b>${esc(kbModeTitle[a.mode] || a.mode)}</b>
      <div class="kb-prompt-l">System · ${esc(num(Array.from(a.system || '').length))} симв.${same ? ' · <span class="chip ok">одинаковый</span>' : ''}</div>
      <pre class="kb-pre">${esc(a.system)}</pre>
      <div class="kb-prompt-l">User · ${esc(num(Array.from(a.user || '').length))} симв.</div>
      <pre class="kb-pre">${esc(a.user)}</pre></div>`).join('')}</div>
  </details>`;
}

function kbAskReadForm() {
  const q = $('kb-ask-q');
  if (!q) return;
  kb.askForm.q = q.value;
  // Текст правили — это уже не вопрос набора: правило его не оценит.
  const x = kbQuestion(kb.askForm.qid);
  if (x && x.q !== q.value.trim()) {
    kb.askForm.qid = '';
    if ($('kb-ask-pick')) $('kb-ask-pick').value = '';
    if ($('kb-ask-context')) $('kb-ask-context').outerHTML = kbAskContextHTML();
  }
}

async function kbAsk() {
  if (kb.asking) return;
  kbAskReadForm();
  const text = kb.askForm.q.trim();
  if (!text) { $('kb-ask-q') && $('kb-ask-q').focus(); return; }
  const q = kbQuestion(kb.askForm.qid);
  const body = { q: text, modes: kbSortModes(kb.askModes).slice(0, kbMaxAskModes) };
  if (q) body.question_id = q.id;
  kb.asking = true;
  kbPaint('ask');
  const r = await factsAPI('POST', '/api/kb/ask', body);
  r.qid = q ? q.id : '';
  kb.ask = r;
  kb.asking = false;
  kbPaint('ask');
  if (!$('kb-ask-out')) toast(r.ok ? 'Ответы готовы — окно «База знаний», вкладка «Спросить»' : 'Спросить не вышло: ' + r.error, !r.ok);
}

document.addEventListener('input', ev => { if (ev.target.closest && ev.target.closest('#kb-ask-form')) kbAskReadForm(); });

/* ---------- вкладка «Контрольные вопросы» ---------- */

function kbQaRunning() { return !!(kb.eval && kb.eval.state === 'running'); }

// kbLoadEvals — список прогонов; open — открыть последний, если ни один не
// открыт (идёт — опрашивать).
async function kbLoadEvals(open) {
  kb.evals = await factsAPI('GET', '/api/kb/evals');
  if (!open || kb.eval || !kb.evals.ok) return;
  const last = kbList(kb.evals.data)[0];
  if (!last) return;
  const r = await factsAPI('GET', '/api/kb/evals/' + encodeURIComponent(last.id));
  if (!r.ok || kb.eval) return;
  kb.eval = r.data;
  if (kbQaRunning()) kb.timer = setTimeout(kbQaPoll, kb.pollMs);
}

function kbQaHTML() {
  const f = kb.qaForm;
  return `<div id="kb-qa" class="kb-qa">
    <form id="kb-qa-form" class="kb-form kb-qa-form" data-submit="kbQaRun" autocomplete="off">
      <label class="lbl" for="kb-qa-repeats">повторы</label>
      <select id="kb-qa-repeats">${[1, 2, 3].map(n => `<option value="${n}"${n === f.repeats ? ' selected' : ''}>${n}</option>`).join('')}</select>
      <label class="kb-check" title="второй голос: судья-модель видит вопрос, ожидание и один ответ без пометки режима"><input type="checkbox" id="kb-qa-judge"${f.judge ? ' checked' : ''}> судья-модель</label>
      <button type="submit" class="solid" id="kb-qa-run"${kbQaRunning() || kb.starting ? ' disabled' : ''}>Прогнать набор test</button>
      <button type="button" id="kb-qa-stop" data-action="kbQaStop"${kbQaRunning() && !kb.stopping ? '' : ' hidden'}>Остановить</button>
      <span class="hint" id="kb-qa-cost">${esc(kbQaCostText())}</span>
      ${kbModePickHTML('kb-qa-modes', kb.qaModes, 0)}
    </form>
    ${kbQaLiveHTML()}
    ${kbQaRunsHTML()}
  </div>`;
}

// kbQaCostText — во что обойдётся прогон выбранных режимов.
function kbQaCostText() {
  const n = kbQuestionList().filter(q => q.split === 'test').length || 10;
  return `Платно: ${n} вопросов × ${plural(kb.qaModes.length, 'режим', 'режима', 'режимов')} × повторы; судья — ещё запрос на каждый ответ.`;
}

// kbQaModes — столбцы таблицы: режимы открытого прогона, иначе выбранные.
function kbQaModes() {
  const v = kb.eval;
  return v && kbList(v.request && v.request.modes).length ? v.request.modes : kb.qaModes;
}

// kbQaQuestions — вопросы таблицы: наборы прогона (по умолчанию test);
// набора нет — вопросы из строк прогона.
function kbQaQuestions() {
  const v = kb.eval;
  const splits = v && kbList(v.request && v.request.splits).length ? v.request.splits : ['test'];
  const qs = kbQuestionList().filter(q => splits.includes(q.split));
  if (qs.length) return qs;
  return kbList(v && v.rows).map(r => r.question).filter(Boolean);
}

function kbQaErrorHTML() {
  const e = kb.evalError;
  if (!e) return '';
  if (e.code === 503) {
    return `<div class="facts-down kb-off" id="kb-qa-off"><b>Прогон запустить нельзя.</b><div class="kb-why">${esc(e.why || e.text)}</div>${e.hint ? `<div class="facts-hint">${esc(e.hint)}</div>` : ''}</div>`;
  }
  if (e.code === 409 && e.id) {
    return `<div class="facts-error" id="kb-qa-error" data-code="409">${esc(e.text)} <button type="button" class="small" data-action="kbQaOpen" data-arg="${esc(e.id)}">открыть ${esc(e.id)}</button></div>`;
  }
  return `<div class="facts-error" id="kb-qa-error" data-code="${esc(e.code)}">${esc(e.text)}${e.hint ? ' — ' + esc(e.hint) : ''}</div>`;
}

function kbQaLiveHTML() {
  const v = kb.eval;
  let html = `<div id="kb-qa-live" class="kb-qa-live">${kbQaErrorHTML()}`;
  if (kb.questions && !kb.questions.ok) {
    html += `<div class="facts-error" id="kb-qa-noset">набора вопросов нет: ${esc(kb.questions.error)}${kb.questions.data && kb.questions.data.hint ? ' — ' + esc(kb.questions.data.hint) : ''}</div>`;
  }
  if (v) {
    const pct = v.total ? Math.round(100 * (v.done || 0) / v.total) : 0;
    const req = v.request || {};
    const state = v.state === 'running' ? '<span class="thinking">идёт</span>'
      : v.state === 'done' ? '<span class="chip ok">готово</span>'
      : v.state === 'cancelled' ? '<span class="chip">остановлен</span>' : '<span class="chip bad">сбой</span>';
    html += `<div class="kb-qa-status" id="kb-qa-status" data-run="${esc(v.id)}" data-state="${esc(v.state)}">
      <b>Прогон ${esc(v.id)}</b> ${state}
      <span id="kb-qa-count">${esc(v.done || 0)} из ${esc(v.total || 0)} ответов</span>
      <span class="hint">повторов ${esc(req.repeats || 1)} · ${req.judge ? 'правило и судья' : 'только правило'} · начат ${esc(kbTime(v.started))}</span>
      <div class="kb-progress"><span style="width:${pct}%"></span></div>
      ${v.error ? `<div class="facts-error">${esc(v.error)}</div>` : ''}
    </div>`;
  }
  const qs = kbQaQuestions();
  const rows = kbList(v && v.rows);
  const modes = kbQaModes();
  if (qs.length) {
    html += `<table class="grid kb-table kb-qa-table" id="kb-qa-table" data-modes="${esc(modes.join(','))}"><tr><th>id</th><th>тип</th><th>вопрос</th><th>ожидание</th><th>источники</th>
      ${modes.map(m => `<th class="kb-qa-v" data-mode="${esc(m)}" title="${esc(kbModeHint[m] || '')}">${esc(kbModeTitle[m] || m)}</th>`).join('')}<th class="kb-qa-v" title="нашёлся ли в выдаче режимов с базой фрагмент с доказательством">источник найден</th>${modes.includes('rag+cite') ? kbQaCiteHeadHTML() : ''}</tr>
      ${qs.map(q => kbQaRowHTML(q, rows.find(r => r.question && r.question.id === q.id), modes)).join('')}</table>`;
  }
  if (v && v.report) html += kbQaSummaryHTML(v.report, v);
  return html + '</div>';
}

function kbQaRowHTML(q, row, modes) {
  const open = !!kb.qaOpen[q.id];
  const running = kbQaRunning();
  const e = q.expect || {};
  const exp = e.note || (q.answerable ? '' : 'в базе нет — ждём «не знаю»');
  const cell = m => {
    if (!modes.includes(m)) return '<span class="hint">—</span>';
    const runs = kbList(row && row.runs && row.runs[m]);
    if (!runs.length) return running || row ? kbVerdictHTML(m, '') : '';
    const v = (row.majority || {})[m] || runs[runs.length - 1].final;
    const flips = (row.flips || {})[m] || 0;
    const title = runs.map(r => 'повтор ' + r.repeat + ': ' + ((kbVerdicts[r.final] || [])[1] || r.final || 'ошибка')).join('; ') + (flips ? '; флипов ' + flips : '');
    const icon = x => (x === 'abstain' ? '?' : (kbVerdicts[x] || ['!'])[0]);
    const reps = runs.length > 1 ? `<span class="kb-reps" title="по повторам">${esc(runs.map(r => icon(r.final)).join(''))}</span>` : '';
    return kbVerdictHTML(m, v, title) + reps;
  };
  let found = '';
  // По режимам с базой: ✓ — доказательство в выдаче хотя бы одного прогона.
  const base = modes.filter(m => m !== 'norag' && kbList(row && row.runs && row.runs[m]).length);
  const hit = m => kbList(row.runs[m]).some(r => r.recall);
  if (!kbList(q.evidence).length) found = '<span class="hint" title="у вопроса нет доказательства в базе">—</span>';
  else if (base.length) {
    const title = base.map(m => m + ' ' + (hit(m) ? '✓' : '✗')).join(' · ');
    const marks = base.length > 1 ? base.map(m => hit(m) ? '✓' : '✗').join('') : (hit(base[0]) ? '✓' : '✗');
    found = `<span class="kb-found ${base.some(hit) ? 'yes' : 'no'}" data-found="${base.some(hit) ? 1 : 0}" title="${esc(title)}">${esc(marks)}</span>`;
  } else if (running) found = '<span class="kb-verdict pending">…</span>';
  let html = `<tr class="kb-qa-row${open ? ' open' : ''}${row ? ' has' : ''}" data-id="${esc(q.id)}" data-action="kbQaToggle" data-arg="${esc(q.id)}" title="ответы обоих режимов и причины оценок">
    <td><b>${esc(q.id)}</b></td><td><span class="chip">${esc(q.type)}</span></td>
    <td class="kb-qa-q">${esc(q.q)}${kbList(q.context).length ? `<div class="hint">после: ${q.context.map(c => `«${esc(c)}»`).join(' → ')}</div>` : ''}</td>
    <td class="kb-qa-exp">${esc(kbCut(exp, 110))}</td><td class="kb-qa-src hint">${esc(kbSources(q))}</td>
    ${modes.map(m => `<td class="kb-qa-v" data-mode="${esc(m)}">${cell(m)}</td>`).join('')}<td class="kb-qa-v">${found}</td>${modes.includes('rag+cite') ? kbQaCiteCellsHTML(q, row, running) : ''}</tr>`;
  const extra = modes.includes('rag+cite') ? kbQaCiteCols.length : 0;
  if (open) html += `<tr class="kb-qa-detail" data-id="${esc(q.id)}"><td colspan="${5 + modes.length + 1 + extra}">${kbQaDetailHTML(q, row, modes)}</td></tr>`;
  return html;
}

// kbQaDetailHTML — раскрытая строка: ответы режимов по повторам, оценки
// правила и судьи с причиной.
function kbQaDetailHTML(q, row, modes) {
  if (!row) return `${kbExpectHTML(q)}<p class="hint">${kbQaRunning() ? 'Ответов на этот вопрос ещё нет — прогон идёт.' : 'Этот вопрос ещё не прогонялся.'}</p>`;
  return `${kbExpectHTML(q)}<div class="kb-answers" style="--kb-cols:${Math.max(1, Math.min(modes.length, 3))}">${modes.map(m => {
    const runs = kbList(row.runs && row.runs[m]);
    return `<section class="kb-answer" data-mode="${esc(m)}">
      <div class="kb-answer-head"><b>${esc(kbModeTitle[m] || m)}</b><code>${esc(m)}</code></div>
      ${runs.length ? runs.map(r => {
        const a = r.answer || {};
        const j = r.judge;
        return `<div class="kb-run" data-repeat="${esc(r.repeat)}">
          <div class="kb-run-head">${runs.length > 1 ? `<span class="hint">повтор ${esc(r.repeat)}</span>` : ''}
            итог ${kbVerdictHTML(m, r.final, '', true)}
            <span class="hint">правило</span> ${kbVerdictHTML(m, r.rule && r.rule.verdict, kbRuleTitle(r.rule))}
            ${j ? `<span class="hint">судья</span> ${kbVerdictHTML(m, j.verdict, j.reason)}` : ''}
            <span class="kb-answer-meta">${esc(kbCost(a.cost))} · ${esc(kbMs(a.ms))}</span></div>
          ${r.error ? `<div class="facts-error">${esc(r.error)}</div>` : a.cited ? kbCitedHTML(a) : `<div class="kb-answer-text">${kbAnswerTextHTML(a.text, a.hits)}</div>`}
          ${a.trace ? kbTraceBriefHTML(a.trace) : ''}
          ${j && j.reason ? `<div class="kb-judge"><b>судья:</b> ${esc(j.reason)}</div>` : ''}
          ${kbRuleHTML(r.rule)}
        </div>`;
      }).join('') : '<p class="hint">ответа нет</p>'}
    </section>`;
  }).join('')}</div>`;
}

const kbQaMetrics = [
  ['questions', 'вопросов', s => num(s.questions)],
  ['correct', 'верно', s => num(s.correct), 'max'],
  ['partial', 'частично', s => num(s.partial)],
  ['wrong', 'неверно', s => num(s.wrong), 'min'],
  ['abstain', '«не знаю»', s => num(s.abstain)],
  ['confident_wrong', 'уверенные ошибки на отвечаемых', s => num(s.confident_wrong), 'min'],
  ['right_abstain', '«не знаю» там, где ответа нет', s => num(s.right_abstain), 'max'],
  ['answered_unanswerable', 'ответ по существу там, где ответа нет', s => num(s.answered_unanswerable), 'min'],
  ['discriminative_correct', 'верно на дискриминативных', s => `${num(s.discriminative_correct)} из ${num(s.discriminative)}`, 'max'],
  ['recall', 'recall доказательств', s => (s.mode === 'norag' ? '—' : kbNum2(s.recall))],
  ['agreement', 'согласие правила и судьи', s => kbPct(s.agreement)],
  ['flip_rate', 'флипы между повторами', s => kbPct(s.flip_rate)],
  ['tokens', 'токенов', s => num((s.usage || {}).total)],
  ['cost', 'цена', s => kbCost(s.cost)],
  ['avg_ms', 'среднее время ответа', s => kbMs(s.avg_ms)],
];

function kbQaSummaryHTML(rep, v) {
  const stats = kbList(rep.stats);
  const req = (v && v.request) || {};
  const cards = stats.map(s => `<div class="kb-qa-card" data-mode="${esc(s.mode)}">
    <span>${esc(kbModeTitle[s.mode] || s.mode)}</span>
    <b>${esc(s.correct)}<small> из ${esc(s.questions)}</small></b>
    <span class="hint">частично ${esc(s.partial)} · «не знаю» ${esc(s.abstain)} · уверенных ошибок ${esc(s.confident_wrong)}</span></div>`).join('');
  const val = (s, k) => { const x = k === 'tokens' ? (s.usage || {}).total : k === 'cost' ? (s.cost || {}).usd : s[k]; return typeof x === 'number' ? x : null; };
  const best = (k, dir, s) => {
    if (!dir || stats.length < 2) return '';
    const xs = stats.map(y => val(y, k)).filter(x => x !== null);
    const x = val(s, k);
    if (x === null || xs.every(y => y === x)) return '';
    return x === (dir === 'max' ? Math.max(...xs) : Math.min(...xs)) ? ' best' : '';
  };
  const cite = stats.some(s => s.mode === 'rag+cite');
  const metrics = kbQaMetrics.concat(cite ? kbQaCiteMetrics : [])
    .filter(([k]) => (k !== 'agreement' || req.judge) && (k !== 'flip_rate' || (rep.repeats || req.repeats) > 1));
  const created = rep.created ? kbTime(rep.created) : '';
  return `<div id="kb-qa-summary" class="kb-qa-summary">
    <h4 class="kb-h">Итог</h4>
    <div class="kb-report-meta">${created ? `<span>от <b>${esc(created)}</b></span>` : ''}${rep.model ? `<span>модель <b>${esc(rep.model)}</b></span>` : ''}
      ${rep.index ? `<span>индекс <b>${esc(rep.index)}</b>, k ${esc(rep.k)}</span>` : ''}${rep.embedder ? `<span>эмбеддер <b>${esc(rep.embedder)}</b></span>` : ''}
      <span>повторов ${esc(rep.repeats || req.repeats || 1)}</span>${req.judge ? `<span>судья ${esc(kbCost(rep.judge_cost))}</span>` : ''}</div>
    <div class="kb-qa-cards">${cards}</div>
    <table class="grid kb-table kb-qa-stats" id="kb-qa-stats"><tr><th></th>${stats.map(s => `<th class="kb-r" data-mode="${esc(s.mode)}">${esc(kbModeTitle[s.mode] || s.mode)}</th>`).join('')}</tr>
      ${metrics.map(([k, label, f, dir]) => `<tr data-metric="${esc(k)}"><td>${esc(label)}</td>${stats.map(s => `<td class="kb-r${best(k, dir, s)}" data-mode="${esc(s.mode)}">${esc(f(s))}</td>`).join('')}</tr>`).join('')}</table>
    ${kbList(rep.conclusion).length ? `<h4 class="kb-h">Вывод</h4><ul class="kb-conclusion" id="kb-qa-conclusion">${rep.conclusion.map(x => `<li>${esc(x)}</li>`).join('')}</ul>` : ''}
    ${kbList(rep.disagreements).length ? `<details class="kb-questions"><summary>Правило и судья разошлись: ${esc(rep.disagreements.length)}</summary><ul>${rep.disagreements.map(x => `<li>${esc(x)}</li>`).join('')}</ul></details>` : ''}
  </div>`;
}

function kbTime(s) {
  if (!s) return '—';
  const d = new Date(s);
  return isNaN(d) ? String(s) : d.toLocaleString('ru-RU', { day: '2-digit', month: '2-digit', hour: '2-digit', minute: '2-digit' });
}

// kbQaRunsHTML — прошлые прогоны (новые первыми); клик открывает прогон.
function kbQaRunsHTML() {
  const r = kb.evals;
  const list = r && r.ok ? kbList(r.data) : [];
  if (!list.length) return '<div id="kb-qa-runs" hidden></div>';
  const cur = kb.eval && kb.eval.id;
  return `<div id="kb-qa-runs" class="kb-qa-runs"><h4 class="kb-h">Прогоны</h4>${list.map(x => {
    const st = kbList(x.report && x.report.stats);
    const by = m => st.find(s => s.mode === m);
    const score = st.length ? kbModes.filter(by).map(m => `${kbModeTitle[m]} ${by(m).correct}/${by(m).questions}`).join(' · ') : '';
    const ms = kbList((x.request || {}).modes);
    const cost = st.length ? kbCost(st.reduce((c, s) => ({ usd: c.usd + ((s.cost || {}).usd || 0), known: c.known && (s.cost || {}).known }), { usd: 0, known: true })) : '';
    const state = x.state === 'running' ? 'идёт' : x.state === 'done' ? 'готово' : x.state === 'cancelled' ? 'остановлен' : 'сбой';
    return `<button type="button" class="kb-qa-run${x.id === cur ? ' on' : ''}" data-run="${esc(x.id)}" data-state="${esc(x.state)}" data-action="kbQaOpen" data-arg="${esc(x.id)}">
      <b>${esc(x.id)}</b> <span>${esc(kbTime(x.started))}</span> <span class="chip${x.state === 'done' ? ' ok' : x.state === 'failed' ? ' bad' : ''}">${esc(state)}</span>
      <span class="hint">×${esc((x.request || {}).repeats || 1)}${(x.request || {}).judge ? ', судья' : ''}${ms.length ? ' · ' + esc(ms.join(', ')) : ''}</span>${score ? ` <span>${esc(score)}</span>` : ''}${cost ? ` <span class="hint">${esc(cost)}</span>` : ''}</button>`;
  }).join('')}</div>`;
}

function kbQaReadForm() {
  const r = $('kb-qa-repeats'), j = $('kb-qa-judge');
  if (r) kb.qaForm.repeats = Number(r.value) || 1;
  if (j) kb.qaForm.judge = j.checked;
}
document.addEventListener('change', ev => { if (ev.target.closest && ev.target.closest('#kb-qa-form')) kbQaReadForm(); });

async function kbQaRun() {
  if (kb.starting) return;
  kbQaReadForm();
  kb.starting = true;
  kbPaint();
  const r = await factsAPI('POST', '/api/kb/evals', { repeats: kb.qaForm.repeats, judge: kb.qaForm.judge, modes: kbSortModes(kb.qaModes) });
  kb.starting = false;
  if (r.ok) {
    kb.eval = r.data;
    kb.evalError = null;
    kb.qaOpen = {};
    kb.watching = true;
    kbPaint('qa');
    kbQaPoll();
    await kbLoadEvals(false);
    kbPaint('runs');
  } else {
    const d = r.data || {};
    kb.evalError = { code: r.code, text: r.error, id: d.id, why: d.why, hint: d.hint };
    kbPaint('qa');
  }
}

function kbQaStopPoll() {
  if (kb.timer) clearTimeout(kb.timer);
  kb.timer = null;
}

// kbQaPoll — GET прогона раз в kb.pollMs, пока он идёт. Опрос идёт и при
// закрытом окне: об итоге скажет тост. Новый вызов отменяет прежний цикл.
async function kbQaPoll() {
  kbQaStopPoll();
  const id = kb.eval && kb.eval.id;
  const seq = ++kb.seq;
  if (!id) return;
  const r = await factsAPI('GET', '/api/kb/evals/' + encodeURIComponent(id));
  if (kb.seq !== seq || !kb.eval || kb.eval.id !== id) return; // тем временем открыли другой прогон
  if (r.ok) kb.eval = r.data;
  else kb.evalError = { code: r.code, text: r.error };
  const done = !r.ok || !kbQaRunning();
  if (done) {
    await kbLoadEvals(false);
    if (kb.seq !== seq) return;
  }
  kbPaint('qa', done ? 'runs' : '');
  if (!done) {
    kb.timer = setTimeout(kbQaPoll, kb.pollMs);
    return;
  }
  if (kb.watching && !$('kb-qa') && r.ok) {
    const stats = kbList(kb.eval.report && kb.eval.report.stats);
    const st = stats.find(s => s.mode === 'rag') || stats.find(s => s.mode !== 'norag');
    const what = kb.eval.state === 'done' ? 'прогон готов' + (st ? `, ${st.mode} верно ${st.correct} из ${st.questions}` : '')
      : kb.eval.state === 'cancelled' ? 'прогон остановлен' : 'сбой — подробности в окне «База знаний»';
    toast('Контрольные вопросы: ' + what, kb.eval.state === 'failed');
  }
  kb.watching = false;
}

Object.assign(actions, {
  kbAsk() { kbAsk(); },
  kbAskPick(id) {
    kb.askForm.qid = id || '';
    const q = kbQuestion(id);
    if (q) {
      kb.askForm.q = q.q;
      if ($('kb-ask-q')) $('kb-ask-q').value = q.q;
    }
    if ($('kb-ask-context')) $('kb-ask-context').outerHTML = kbAskContextHTML();
  },
  kbQaRun() { kbQaRun(); },
  // kbQaStop — DELETE прогона: сервер отменяет его контекст и сразу
  // отвечает состоянием cancelled со строками, что успели.
  async kbQaStop() {
    const id = kb.eval && kb.eval.id;
    if (!id || !kbQaRunning() || kb.stopping) return;
    kb.stopping = true;
    kbPaint();
    const r = await factsAPI('DELETE', '/api/kb/evals/' + encodeURIComponent(id));
    kb.stopping = false;
    if (!r.ok && r.code !== 409) { toast('Остановить не вышло: ' + r.error, true); kbPaint(); return; }
    if (r.ok && kb.eval && kb.eval.id === id) kb.eval = r.data;
    kbQaPoll();
  },
  kbQaToggle(id) {
    if (!id) return;
    kb.qaOpen[id] = !kb.qaOpen[id];
    kbPaint('qa');
  },
  async kbQaOpen(id) {
    if (!id) return;
    const r = await factsAPI('GET', '/api/kb/evals/' + encodeURIComponent(id));
    if (!r.ok) { toast('Прогон не открылся: ' + r.error, true); return; }
    kbQaStopPoll();
    kb.seq++;
    kb.eval = r.data;
    kb.evalError = null;
    kb.qaOpen = {};
    kbPaint('qa', 'runs');
    if (kbQaRunning()) kb.timer = setTimeout(kbQaPoll, kb.pollMs);
  },
});

/* ---------- v23: вкладка «Режимы» ---------- */

const kbMatrixCmd = 'go run ./cmd/kb matrix';
const kbCalibCmd = 'go run ./cmd/kb calibrate';
// Конфигурации матрицы словами.
const kbPresetText = {
  base: 'без фильтра и rewrite',
  filter: 'фильтр релевантности',
  rewrite: 'rewrite кодом',
  both: 'rewrite + фильтр',
  hybrid: 'rewrite + гибрид (RRF) + фильтр',
  'rrf-only': 'только гибрид (RRF)',
  'llm-rewrite': 'rewrite моделью (платно)',
  'llm-rerank': 'реранкинг моделью (платно)',
};
// Колонки матрицы: ключ, заголовок, подсказка, формат, направление лучшего.
const kbMxCols = [
  ['recall_before', 'recall до', 'доказательство в dense-выдаче K0 (как у base)', kbNum2, 'max'],
  ['recall_union', 'dense∪BM25', 'доказательство среди всех кандидатов: dense и BM25 всех запросов', kbNum2, 'max'],
  ['recall_after', 'recall после', 'доказательство в итоге — то, что увидит модель', kbNum2, 'max'],
  ['mrr', 'MRR', 'средний обратный ранг доказательства в итоге', kbNum2, 'max'],
  ['precision', 'precision', 'среднее по вопросам доли итоговых фрагментов с доказательством; пустой итог на отвечаемом вопросе — 0', kbNum2, 'max'],
  ['cut_share', 'отсечено', 'доля кандидатов K0, отсечённых фильтром', kbPct, ''],
  ['wrong_cut', 'ошибочно отсечено', 'доля релевантных (с доказательством) среди отсечённых фильтром кандидатов', kbPct, 'min'],
  ['lost_q', 'доказательство снято', 'доля вопросов, где фильтр снял доказательство, которое было среди кандидатов', kbPct, 'min'],
  ['out_empty', 'out: пусто', 'неотвечаемые вопросы, где после фильтра не осталось ничего', null, 'max'],
  ['tokens', 'токенов', 'среднее токенов итоговых фрагментов', x => num(Math.round(x || 0)), 'min'],
  ['ms', 'мс', 'среднее время поиска', x => (typeof x === 'number' ? x.toFixed(1) : '—'), 'min'],
  ['cost_usd', 'цена', 'цена прогона конфигурации', x => (x > 0 ? factsUSD(x) : '$0'), 'min'],
];

async function kbLoadModes() {
  const need = r => !r || (!r.ok && r.code !== 404);
  const [m, c] = await Promise.all([
    need(kb.matrix) ? factsAPI('GET', '/api/kb/matrix') : kb.matrix,
    need(kb.calib) ? factsAPI('GET', '/api/kb/calibration') : kb.calib,
  ]);
  kb.matrix = m;
  kb.calib = c;
}

// kbNoneHTML — файла ещё нет: что запустить.
function kbNoneHTML(id, what, r, cmd, about) {
  const d = (r && r.data) || {};
  return `<div class="facts-down kb-none" id="${esc(id)}"><b>${esc(what)} ещё нет.</b>
    <div>Соберите: <code>${esc(d.hint || cmd)}</code> — ${esc(about)}</div>
    ${d.path ? `<div class="hint">файл ищется здесь: <code>${esc(d.path)}</code></div>` : ''}</div>`;
}

function kbModesTabHTML() {
  if (!kb.matrix && !kb.calib) return '<div id="kb-modes"><p class="hint">загружаю…</p></div>';
  return `<div id="kb-modes" class="kb-modes">${kbMatrixHTML()}${kbCalibHTML()}</div>`;
}

// kbMxSplits — наборы матрицы в порядке test, dev, out.
function kbMxSplits(rows) {
  const all = [...new Set(rows.map(r => r.split))];
  const known = ['test', 'dev', 'out'];
  return known.filter(s => all.includes(s)).concat(all.filter(s => !known.includes(s)));
}

function kbMatrixHTML() {
  const r = kb.matrix;
  if (!r) return '';
  if (!r.ok) {
    if (r.code === 404) return `<section class="kb-mx">${kbNoneHTML('kb-matrix-none', 'Матрицы режимов', r, kbMatrixCmd, 'прогонит конфигурации поиска (без фильтра, фильтр, rewrite, оба, гибрид) × K1 на наборах test, dev и out и сохранит examples/rag/filter.json.')}</section>`;
    return `<section class="kb-mx"><div class="facts-error" id="kb-matrix-error">${esc(r.error)}</div></section>`;
  }
  const d = r.data || {};
  const rows = kbList(d.rows);
  const splits = kbMxSplits(rows);
  const split = kb.mxSplit === 'all' || splits.includes(kb.mxSplit) ? kb.mxSplit : (splits[0] || 'all');
  const shown = split === 'all' ? rows : rows.filter(x => x.split === split);
  // Только неотвечаемые (out) — recall и precision не определены; колонка
  // «out: пусто» — только если на экране есть такие вопросы.
  const outOnly = x => x.out_n > 0 && x.out_n >= x.n;
  const noRecall = ['recall_before', 'recall_union', 'recall_after', 'mrr', 'precision', 'wrong_cut', 'lost_q'];
  // Старые отчёты (до lost_q): wrong_cut там — доля вопросов, где фильтр
  // снял доказательство, то есть нынешний lost_q.
  const legacy = rows.length > 0 && rows.every(x => !('lost_q' in x));
  const field = (x, k) => (legacy && k === 'lost_q' ? x.wrong_cut : legacy && k === 'wrong_cut' ? undefined : x[k]);
  const cols = kbMxCols.filter(c => c[0] !== 'out_empty' || shown.some(x => x.out_n));
  const val = (x, k) => (typeof field(x, k) === 'number' && !(outOnly(x) && noRecall.includes(k)) ? field(x, k) : null);
  // Лучшее — среди строк того же набора и K1.
  const best = (x, k, dir) => {
    if (!dir) return '';
    if (k === 'out_empty' && !x.out_n) return '';
    const peers = rows.filter(y => y.split === x.split && y.k1 === x.k1 && (k !== 'out_empty' || y.out_n)).map(y => val(y, k)).filter(v => v !== null);
    const v = val(x, k);
    if (v === null || peers.length < 2 || peers.every(y => y === v)) return '';
    return v === (dir === 'max' ? Math.max(...peers) : Math.min(...peers)) ? ' best' : '';
  };
  const fmt = (x, c) => {
    if (outOnly(x) && noRecall.includes(c[0])) return '<span class="hint">—</span>';
    if (c[0] === 'out_empty') return x.out_n ? `${esc(kbNum2(x.out_empty))} <span class="hint">(${esc(Math.round(x.out_empty * x.out_n))} из ${esc(x.out_n)})</span>` : '<span class="hint">—</span>';
    return esc(c[3](field(x, c[0])));
  };
  let prev = '';
  const body = shown.map(x => {
    const g = x.split + '|' + x.k1;
    const first = g !== prev;
    prev = g;
    return `<tr class="kb-mx-row${first ? ' grp' : ''}" data-name="${esc(x.name)}" data-split="${esc(x.split)}" data-k1="${esc(x.k1)}">
      <td><b class="kb-mx-name">${esc(x.name)}</b> <span class="hint">${esc(kbPresetText[x.name] || '')}</span></td>
      <td class="kb-r">${esc(x.k1)}</td><td>${esc(x.split)}</td><td class="kb-r">${esc(x.n)}</td>
      ${cols.map(c => `<td class="kb-r${best(x, c[0], c[4])}" data-col="${esc(c[0])}">${fmt(x, c)}</td>`).join('')}</tr>`;
  }).join('');
  const created = d.created ? kbTime(d.created) : '—';
  return `<section class="kb-mx" id="kb-mx">
    <div class="kb-mx-head"><h4 class="kb-h">Матрица режимов поиска</h4>
      <span class="kb-seg kb-mx-seg">${splits.concat(['all']).map(s => `<button type="button" class="small${s === split ? ' on' : ''}" data-action="kbMxSplit" data-arg="${esc(s)}" data-kb-split="${esc(s)}">${esc(s === 'all' ? 'все' : s)}</button>`).join('')}</span></div>
    <div class="kb-report-meta" id="kb-matrix-meta"><span>от <b>${esc(created)}</b></span><span>эмбеддер <b>${esc(d.embedder || '—')}</b></span>
      <span>индекс <b>${esc(d.index || '—')}</b></span><span>порог <b>${esc(kbNum3(d.min_score))}</b> · Δ <b>${esc(kbNum2(d.delta))}</b></span>
      ${d.corpus_sha ? `<span>corpus_sha <code title="${esc(d.corpus_sha)}">${esc(kbShort(d.corpus_sha))}</code></span>` : ''}</div>
    ${kbList(d.conclusion).length ? `<ul class="kb-conclusion" id="kb-modes-conclusion">${d.conclusion.map(x => `<li>${esc(x)}</li>`).join('')}</ul>` : ''}
    <div class="kb-mx-wrap"><table class="grid kb-table kb-mx-table" id="kb-matrix"><tr><th>конфигурация</th><th class="kb-r">K1</th><th>набор</th><th class="kb-r">вопросов</th>
      ${cols.map(c => `<th class="kb-r" title="${esc(c[2])}">${esc(c[1])}</th>`).join('')}</tr>${body}</table></div>
    <div class="hint">Жирным — лучшее значение среди конфигураций того же набора и K1. Порог подобран на dev и out, test — только проверка.</div>
  </section>`;
}

function kbCalibHTML() {
  const r = kb.calib;
  if (!r) return '';
  if (!r.ok) {
    if (r.code === 404) return `<section class="kb-cal">${kbNoneHTML('kb-calib-none', 'Калибровки порога', r, kbCalibCmd, 'подберёт порог косинуса на dev и out — середину зазора между вопросами вне базы и доказательствами dev (без зазора — наибольший без заметной потери recall); с -write запишет его в индекс (отчёт — examples/rag/calibrate.json).')}</section>`;
    return `<section class="kb-cal"><div class="facts-error" id="kb-calib-error">${esc(r.error)}</div></section>`;
  }
  const d = r.data || {};
  const near = (a, b) => typeof a === 'number' && typeof b === 'number' && Math.abs(a - b) < 1e-9;
  // Середина зазора может не попасть на шаг перебора — её строка (at)
  // встаёт в таблицу отдельно.
  const grid = kbList(d.table);
  const at = d.at && d.at.min_score > 0 && !grid.some(x => near(x.min_score, d.chosen)) ? [d.at] : [];
  const table = grid.concat(at).sort((a, b) => a.min_score - b.min_score);
  const rows = table.map(x => {
    const on = near(x.min_score, d.chosen);
    const lost = kbList(x.lost_dev);
    return `<tr class="kb-cal-row${on ? ' chosen' : ''}" data-min="${esc(kbNum3(x.min_score))}"${on ? ' data-chosen="1"' : ''}>
      <td class="kb-r"><b>${esc(kbNum3(x.min_score))}</b>${on ? ' <span class="chip ok kb-chosen">выбран</span>' : ''}</td>
      <td class="kb-r">${esc(kbNum2(x.dev_recall))}</td><td class="kb-r">${esc(kbPct(x.out_empty))}</td>
      <td class="kb-cal-lost">${lost.length ? lost.map(id => `<code>${esc(id)}</code>`).join(' ') : '<span class="hint">—</span>'}</td></tr>`;
  }).join('');
  return `<section class="kb-cal" id="kb-calib">
    <h4 class="kb-h">Калибровка порога</h4>
    <div class="kb-report-meta" id="kb-calib-meta"><span>индекс <b>${esc(d.index || '—')}</b></span><span>эмбеддер <b>${esc(d.embedder || '—')}</b></span>
      <span>выбран порог <b id="kb-calib-chosen">${esc(kbNum3(d.chosen))}</b></span><span>Δ <b>${esc(kbNum2(d.delta))}</b></span>
      <span>recall dev без фильтра <b>${esc(kbNum2(d.base_recall))}</b>, допустимое падение <b>${esc(kbNum2(d.max_drop))}</b></span></div>
    ${kbCalibGapHTML(d)}
    <div class="kb-cal-grid">
      <div class="kb-cal-tbl"><table class="grid kb-table" id="kb-calib-table"><tr><th class="kb-r">порог</th><th class="kb-r" title="доказательство в итоге на отвечаемых вопросах dev">recall dev</th>
        <th class="kb-r" title="доля вопросов out, где после фильтра пусто">out пусто</th><th title="dev-вопросы, у которых фильтр отсёк доказательство">потеряно dev</th></tr>${rows}</table></div>
      <figure class="kb-hist-fig">${kbHistSVG(kbList(d.dev_top), kbList(d.out_top), d.chosen)}
        <figcaption class="hint">Лучший косинус кандидатов: <span class="kb-hist-key dev"></span> отвечаемые dev, <span class="kb-hist-key out"></span> вне базы (out); черта — выбранный порог.</figcaption></figure>
    </div>
  </section>`;
}

// kbCalibGapHTML — правило выбора порога: зазор между лучшим косинусом
// вопросов вне базы и доказательством неякорных dev, запасы и вопросы,
// чувствительные к полу. У старых отчётов (без rule) — ничего.
function kbCalibGapHTML(d) {
  if (!d.rule) return '';
  const ids = xs => (kbList(xs).length ? kbList(xs).map(id => `<code>${esc(id)}</code>`).join(' ') : '<span class="hint">—</span>');
  const has = v => typeof v === 'number' && v >= 0;
  const rule = d.rule === 'gap' ? 'середина зазора' : `max-drop ${kbNum2(d.max_drop)}`;
  return `<div class="kb-cal-gap" id="kb-calib-gap">
    <div><span class="chip${d.rule === 'gap' ? ' ok' : ''}" id="kb-calib-rule">${esc(rule)}</span>
      вне базы (без якоря) до <b>${esc(has(d.out_max) ? kbNum3(d.out_max) : '—')}</b>${d.out_max_id ? ` <span class="hint">${esc(d.out_max_id)}</span>` : ''}
      · доказательство неякорных dev от <b>${esc(has(d.evidence_min) ? kbNum3(d.evidence_min) : '—')}</b>${d.evidence_min_id ? ` <span class="hint">${esc(d.evidence_min_id)}</span>` : ''}
      ${has(d.out_max) && has(d.evidence_min) ? ` · зазор <b>${esc((d.gap >= 0 ? '+' : '') + kbNum3(d.gap))}</b> · запас <b>${esc(kbNum3(d.margin_out))}</b> / <b>${esc(kbNum3(d.margin_dev))}</b>` : ''}</div>
    <div class="hint">к полу чувствительны (вид в реплике не назван): dev ${ids(d.floor_dev)} · out ${ids(d.floor_out)} · test ${ids(d.floor_test)}${kbList(d.floor_missed).length ? ` · не дошли и без пола (в зазор не входят): ${ids(d.floor_missed)}` : ''}</div>
    ${d.note ? `<div class="kb-tr-note">${esc(d.note)}</div>` : ''}
  </div>`;
}

// kbHistSVG — гистограмма косинусов лучшего кандидата dev и out по
// корзинам 0,01 и черта порога. Без библиотек: прямоугольники SVG; всё
// внутри — числа.
function kbHistSVG(dev, out, chosen) {
  dev = dev.filter(v => typeof v === 'number' && isFinite(v));
  out = out.filter(v => typeof v === 'number' && isFinite(v));
  const xs = dev.concat(out);
  if (!xs.length) return '<div class="hint" id="kb-hist">косинусов в калибровке нет</div>';
  const step = 0.01;
  const thr = typeof chosen === 'number' && chosen > 0 ? chosen : null;
  const all = thr ? xs.concat([thr]) : xs;
  const lo = Math.floor(Math.min(...all) / step + 1e-9) * step;
  const n = Math.max(1, Math.floor((Math.max(...all) - lo) / step + 1e-9) + 1);
  const bin = v => Math.min(n - 1, Math.max(0, Math.floor((v - lo) / step + 1e-9)));
  const cd = new Array(n).fill(0), co = new Array(n).fill(0);
  dev.forEach(v => { cd[bin(v)]++; });
  out.forEach(v => { co[bin(v)]++; });
  const top = Math.max(1, ...cd, ...co);
  const W = 520, H = 210, L = 28, R = 10, T = 22, B = 34;
  const bw = (W - L - R) / n;
  const y = c => T + (H - T - B) * (1 - c / top);
  const x = v => L + ((v - lo) / (n * step)) * (W - L - R);
  const every = n > 16 ? 2 : 1;
  let g = '';
  for (let i = 0; i < n; i++) {
    const x0 = L + i * bw;
    const w = Math.max(1, bw / 2 - 1.5);
    const from = (lo + i * step).toFixed(2), to = (lo + (i + 1) * step).toFixed(2);
    if (cd[i]) g += `<rect class="kb-hist-dev" x="${(x0 + 1).toFixed(1)}" y="${y(cd[i]).toFixed(1)}" width="${w.toFixed(1)}" height="${(H - B - y(cd[i])).toFixed(1)}" data-n="${cd[i]}"><title>dev ${from}–${to}: ${cd[i]}</title></rect>`;
    if (co[i]) g += `<rect class="kb-hist-out" x="${(x0 + bw / 2 + 0.5).toFixed(1)}" y="${y(co[i]).toFixed(1)}" width="${w.toFixed(1)}" height="${(H - B - y(co[i])).toFixed(1)}" data-n="${co[i]}"><title>out ${from}–${to}: ${co[i]}</title></rect>`;
    if (i % every === 0) g += `<text class="kb-hist-tick" x="${x0.toFixed(1)}" y="${H - B + 14}">${from}</text>`;
  }
  g += `<line class="kb-hist-axis" x1="${L}" y1="${H - B}" x2="${W - R}" y2="${H - B}"/>`;
  g += `<text class="kb-hist-tick" x="${L - 6}" y="${H - B}" text-anchor="end">0</text><text class="kb-hist-tick" x="${L - 6}" y="${T + 4}" text-anchor="end">${top}</text>`;
  if (thr) {
    const cx = x(thr).toFixed(1);
    g += `<line class="kb-hist-thr" x1="${cx}" y1="${T - 6}" x2="${cx}" y2="${H - B}"/><text class="kb-hist-thr-l" x="${cx}" y="${T - 9}" text-anchor="middle">порог ${thr.toFixed(3)}</text>`;
  }
  g += `<text class="kb-hist-cap" x="${W - R}" y="${H - 4}" text-anchor="end">косинус лучшего кандидата</text>`;
  return `<svg id="kb-hist" class="kb-hist" viewBox="0 0 ${W} ${H}" width="${W}" height="${H}" role="img" aria-label="гистограмма косинусов dev и out" data-dev="${dev.length}" data-out="${out.length}">${g}</svg>`;
}

/* ---------- v24: ответ с источниками и цитатами (rag+cite) ---------- */

// kbCitedIDs — chunk_id источников и цитат ответа rag+cite.
function kbCitedIDs(a) {
  const c = (a && a.cited && a.cited.cited) || {};
  return new Set(kbList(c.sources).map(s => s.chunk_id).concat(kbList(c.quotes).map(x => x.chunk_id)));
}

// kbChunkWhere — «статья › раздел» фрагмента из выдачи ответа; фрагмента в
// выдаче нет — null.
function kbChunkWhere(hits, id) {
  const h = kbList(hits).find(x => x.chunk_id === id);
  if (!h) return null;
  return { title: h.title || h.doc_id || '', section: h.section || kbList(h.section_path).slice(-1)[0] || '' };
}

// kbChunkAttrs — data-атрибуты перехода к чанку: документ и стратегия — из
// chunk_id («doc/strategy/ord»).
function kbChunkAttrs(id) {
  const [doc, index] = String(id || '').split('/');
  return `data-chunk="${esc(id)}" data-doc="${esc(doc || '')}" data-index="${esc(index || '')}"`;
}

// kbCheckBadgesHTML — итог проверки кодом бейджами: источники, цитаты,
// дословность, числа в цитатах, «не проверено» и число отказов.
function kbCheckBadgesHTML(c, ch) {
  const badge = (key, cls, label, title) =>
    `<span class="kb-check-badge ${cls}" data-check="${esc(key)}" data-ok="${cls === 'ok' ? 1 : 0}" title="${esc(title)}">${esc(label)}</span>`;
  const out = [];
  if (c.status !== 'unknown') {
    const unknownIDs = kbList(ch.unknown_ids);
    const src = ch.has_sources && !unknownIDs.length;
    out.push(badge('sources', src ? 'ok' : 'bad', 'источники ' + (src ? '✓' : '✗'),
      unknownIDs.length ? 'не из выдачи хода: ' + unknownIDs.join(', ') : ch.has_sources ? 'все источники — фрагменты, выданные в этом ходе' : 'у ответа нет источника'));
    out.push(badge('quotes', ch.has_quotes ? 'ok' : 'bad', 'цитаты ' + (ch.has_quotes ? '✓' : '✗'), ch.has_quotes ? 'у ответа есть цитата' : 'у ответа нет цитаты'));
    const m = kbList(c.quotes).length;
    const n = Math.max(0, m - kbList(ch.not_verbatim).length);
    out.push(badge('verbatim', m && n === m ? 'ok' : 'bad', `дословно ${n} из ${m}`, 'цитата — подстрока своего фрагмента после нормализации (регистр, ё, кавычки, тире, пробелы)'));
    const miss = kbList(ch.numbers_missing);
    out.push(badge('numbers', miss.length ? 'warn' : 'ok', 'числа в цитатах ' + (miss.length ? '⚠ ' + miss.join(', ') : '✓'),
      miss.length ? 'числа ответа, которых нет ни в одной цитате' : 'все числа ответа есть в цитатах'));
  }
  if (ch.unverified) out.push(badge('unverified', 'bad', 'не проверено', `ответ принят после ${ch.rejects || 0} отказов проверки — проверку он так и не прошёл`));
  if (ch.rejects > 0) out.push(badge('rejects', ch.unverified ? 'bad' : 'warn', 'отказов: ' + ch.rejects, 'сколько раз код вернул ответ модели на исправление'));
  return out.length ? `<div class="kb-check" data-ok="${ch.ok ? 1 : 0}">${out.join('')}</div>` : '';
}

// kbSupportHTML — судья смысла: утверждения ответа и чем подтверждены.
function kbSupportHTML(sp) {
  if (!sp) return '';
  const claims = kbList(sp.claims);
  const ok = claims.filter(c => c.supported).length;
  return `<div class="kb-support" data-ok="${sp.ok ? 1 : 0}">
    <div class="kb-cited-h">Смысл — судья: подтверждено <b>${esc(ok)}</b> из ${esc(claims.length)}${sp.cost && sp.cost.usd > 0 ? ` <span class="hint">${esc(kbCost(sp.cost))}</span>` : ''}</div>
    <ul class="kb-claims">${claims.map(c => {
      const k = typeof c.quote === 'number' && c.quote >= 0 ? c.quote + 1 : 0;
      const verdict = c.supported ? (k ? 'подтверждено цитатой №' + k : 'подтверждено') : 'не подтверждено';
      return `<li class="kb-claim ${c.supported ? 'ok' : 'bad'}" data-supported="${c.supported ? 1 : 0}"${k ? ` data-quote="${k}"` : ''}>
        <span class="kb-claim-mark">${c.supported ? '✓' : '✗'}</span> <span class="kb-claim-t">${esc(c.claim)}</span> <span class="kb-claim-v">${esc(verdict)}</span>${c.reason ? `<div class="hint">${esc(c.reason)}</div>` : ''}</li>`;
    }).join('')}</ul></div>`;
}

// kbCitedHTML — ответ rag+cite: бейджи проверки, ответ, источники,
// цитаты, судья смысла; у «не знаю» — плашка с уточняющим вопросом. Всё,
// что сказала модель, — через esc().
function kbCitedHTML(a) {
  const r = a.cited || {};
  const c = r.cited || {};
  const ch = r.check || {};
  const srcs = kbList(c.sources);
  const num1 = id => srcs.findIndex(s => s.chunk_id === id) + 1;
  const badges = kbCheckBadgesHTML(c, ch);
  const problems = kbList(ch.problems).length
    ? `<details class="kb-problems"><summary>Замечания проверки: ${esc(ch.problems.length)}</summary><ul>${ch.problems.map(x => `<li>${esc(x)}</li>`).join('')}</ul></details>` : '';
  const notV = new Set(kbList(ch.not_verbatim));
  const unknownIDs = new Set(kbList(ch.unknown_ids));
  const srcHTML = srcs.map((s, i) => {
    const w = kbChunkWhere(a.hits, s.chunk_id);
    const bad = !w || unknownIDs.has(s.chunk_id);
    return `<button type="button" class="kb-cited-src${bad ? ' unknown' : ''}" ${kbChunkAttrs(s.chunk_id)} data-n="${i + 1}" data-action="kbOpenChunk" data-arg="${esc(s.chunk_id)}" title="${esc(bad ? 'этого фрагмента не было в выдаче хода' : 'открыть фрагмент в тексте документа')}">
      <span class="kb-cited-n">[${i + 1}]</span> ${w ? `<b>${esc(w.title)}</b> › ${esc(w.section)}` : '<b>не из выдачи</b>'} <code>${esc(s.chunk_id)}</code></button>`;
  }).join('');
  if (c.status === 'unknown') {
    return `<div class="kb-cited" data-status="unknown" data-forced="${ch.forced ? 1 : 0}">
      ${badges}
      <div class="kb-unknown"><b>Не знаю</b>${c.answer ? ' — ' + esc(c.answer) : ''}
        ${c.clarify ? `<div class="kb-clarify"><span class="kb-clarify-l">Уточните:</span> ${esc(c.clarify)}</div>` : '<div class="kb-clarify none hint">уточняющего вопроса нет</div>'}
        ${ch.forced ? `<div class="kb-forced" data-forced="1">решил код: ${esc(ch.gate_reason || 'релевантность ниже порога — ответить по существу было нечем')}</div>`
          : '<div class="kb-forced hint" data-forced="0">решила модель: в найденных фрагментах ответа нет</div>'}</div>
      ${srcs.length ? `<div class="kb-cited-srcs near"><div class="kb-cited-h">Ближайшее в базе <span class="hint">— что нашлось рядом с вопросом</span></div>${srcHTML}</div>` : ''}
      ${problems}
    </div>`;
  }
  const answer = esc(c.answer || '').replace(/\[(\d{1,2})\]/g, '<sup class="kb-cited-ref">[$1]</sup>');
  const quotes = kbList(c.quotes).map((x, i) => {
    const n = num1(x.chunk_id);
    const bad = notV.has(i);
    return `<button type="button" class="kb-quote${bad ? ' bad' : ''}" ${kbChunkAttrs(x.chunk_id)} data-quote="${esc(x.text)}" data-n="${i + 1}" data-src="${n}" data-verbatim="${bad ? 0 : 1}" data-action="kbOpenQuote" data-arg="${esc(x.chunk_id)}" title="открыть фрагмент с подсветкой цитаты">
      <span class="kb-quote-n">№${i + 1}</span> <i class="kb-quote-t">«${esc(x.text)}»</i> <span class="kb-cited-n">[${n || '?'}]</span>${bad ? ' <span class="chip warn">не дословно</span>' : ''}</button>`;
  }).join('');
  return `<div class="kb-cited" data-status="answered" data-ok="${ch.ok ? 1 : 0}"${ch.unverified ? ' data-unverified="1"' : ''}>
    ${badges}
    <div class="kb-cited-answer kb-answer-text">${answer}</div>
    ${srcs.length ? `<div class="kb-cited-srcs"><div class="kb-cited-h">Источники</div>${srcHTML}</div>` : '<div class="kb-cited-h bad">Источников нет</div>'}
    ${quotes ? `<div class="kb-quotes"><div class="kb-cited-h">Цитаты <span class="hint">— клик откроет фрагмент с подсветкой</span></div>${quotes}</div>` : '<div class="kb-cited-h bad">Цитат нет</div>'}
    ${kbSupportHTML(a.support)}
    ${problems}
  </div>`;
}

// Столбцы проверки rag+cite в таблице контрольных вопросов.
const kbQaCiteCols = [
  ['sources', 'источники', 'у ответа есть источник, и все chunk_id — из выдачи хода'],
  ['quotes', 'цитаты', 'у ответа есть хотя бы одна цитата'],
  ['verbatim', 'дословно', 'все цитаты — дословные подстроки своих фрагментов'],
  ['support', 'смысл', 'судья: каждое утверждение ответа подтверждено цитатами'],
  ['unknown', 'не знаю', '✓ — «не знаю» там, где ответа в базе нет; ✗ — ложное «не знаю» или ответ по существу там, где ответа нет'],
];

function kbQaCiteHeadHTML() {
  return kbQaCiteCols.map(([k, label, title]) => `<th class="kb-qa-c" data-check="${esc(k)}" title="${esc('rag+cite: ' + title)}">${esc(label)}</th>`).join('');
}

// kbCiteVerdict — проверка одного прогона rag+cite: ok, bad или na (не
// применимо: у «не знаю» нет источников, у ответа без судьи — смысла).
function kbCiteVerdict(q, run, key) {
  const a = run.answer || {};
  const c = (a.cited && a.cited.cited) || {};
  const ch = (a.cited && a.cited.check) || {};
  const unknown = c.status === 'unknown';
  switch (key) {
    case 'sources': return unknown ? 'na' : ch.has_sources && !kbList(ch.unknown_ids).length ? 'ok' : 'bad';
    case 'quotes': return unknown ? 'na' : ch.has_quotes ? 'ok' : 'bad';
    case 'verbatim': return unknown ? 'na' : kbList(c.quotes).length && !kbList(ch.not_verbatim).length ? 'ok' : 'bad';
    case 'support': return unknown || !a.support ? 'na' : a.support.ok ? 'ok' : 'bad';
    case 'unknown':
      if (unknown) return q.answerable ? 'bad' : 'ok';
      return q.answerable ? 'na' : 'bad';
  }
  return 'na';
}

// kbQaCiteCellsHTML — ячейки проверки rag+cite строки: по всем повторам
// (хоть один ✗ — ✗).
function kbQaCiteCellsHTML(q, row, running) {
  const runs = kbList(row && row.runs && row.runs['rag+cite']).filter(r => r.answer && r.answer.cited);
  return kbQaCiteCols.map(([k, label]) => {
    if (!runs.length) return `<td class="kb-qa-c" data-check="${esc(k)}" data-v="">${running ? '<span class="kb-verdict pending">…</span>' : ''}</td>`;
    const vs = runs.map(r => kbCiteVerdict(q, r, k));
    const v = vs.includes('bad') ? 'bad' : vs.includes('ok') ? 'ok' : 'na';
    const icon = { ok: '✓', bad: '✗', na: '—' }[v];
    const parts = [label + ': ' + { ok: 'да', bad: 'нет', na: 'не применимо' }[v]];
    const last = runs[runs.length - 1].answer.cited;
    const ch = last.check || {};
    if (k === 'verbatim' && last.cited && kbList(last.cited.quotes).length) {
      const m = last.cited.quotes.length;
      parts.push(`дословно ${m - kbList(ch.not_verbatim).length} из ${m}`);
    }
    if (k === 'unknown' && last.cited && last.cited.status === 'unknown') parts.push(ch.forced ? 'решил код' : 'решила модель');
    if (k === 'support' && runs[runs.length - 1].answer.support) {
      const sp = runs[runs.length - 1].answer.support;
      parts.push(`подтверждено ${sp.supported || 0} из ${(sp.supported || 0) + (sp.unsupported || 0)}`);
    }
    if (runs.some(r => (r.answer.cited.check || {}).unverified)) parts.push('есть «не проверено» после отказов');
    return `<td class="kb-qa-c ${v}" data-check="${esc(k)}" data-v="${v}" title="${esc(parts.join('; '))}">${icon}</td>`;
  }).join('');
}

// kbCiteOf — поле ModeStats v24: у режима без цитат и если поля нет в
// JSON (старый отчёт) — «—».
function kbCiteOf(s, k, f) { return s.mode === 'rag+cite' && typeof s[k] === 'number' ? f(s[k], s) : '—'; }
const kbOutOf = (x, s) => `${num(x)} из ${num(s.questions)}`;
const kbQaCiteMetrics = [
  ['with_sources', 'с источниками', s => kbCiteOf(s, 'with_sources', kbOutOf), 'max'],
  ['with_quotes', 'с цитатами', s => kbCiteOf(s, 'with_quotes', kbOutOf), 'max'],
  ['verbatim', 'дословность цитат', s => kbCiteOf(s, 'verbatim', x => (x <= 1 ? kbPct(x) : num(x))), 'max'],
  ['supported', 'смысл подтверждён цитатами', s => kbCiteOf(s, 'supported', kbOutOf), 'max'],
  ['unverified', '«не проверено» после отказов', s => kbCiteOf(s, 'unverified', x => num(x)), 'min'],
  ['forced_unknown', '«не знаю» решил код', s => kbCiteOf(s, 'forced_unknown', x => num(x))],
  ['false_unknown', 'ложных «не знаю» на отвечаемых', s => kbCiteOf(s, 'false_unknown', x => num(x)), 'min'],
];

// kbGoChunk — вкладка «Чанки» на чанке id; quote — подсветить цитату.
async function kbGoChunk(id, doc, index, quote) {
  kbReadForm();
  kbAskReadForm();
  const [d, s] = String(id).split('/');
  kb.doc = doc || d || kb.doc;
  kb.strategy = index || s || kb.strategy;
  kb.focus = id;
  kb.quote = quote || '';
  kb.tab = 'chunks';
  kbPaint('tabs');
  await kbShowChunks(true);
}

// app.kbOpenCite — для чата: окно «База знаний» на чанке chunk_id с
// подсветкой цитаты. Чанк сначала спрашивается у /api/kb/chunk/{id}: так
// известны документ и стратегия, а нет чанка — тост, а не пустая вкладка.
app.kbOpenCite = async function (id, quote) {
  if (!id) return false;
  const r = await factsAPI('GET', '/api/kb/chunk/' + String(id).split('/').map(encodeURIComponent).join('/'));
  if (!r.ok) {
    toast('Фрагмент ' + id + ' не открылся: ' + r.error, true);
    return false;
  }
  const c = r.data.chunk || {};
  if ($('window').open && $('kb-root')) {
    await kbGoChunk(c.chunk_id || id, c.doc_id, c.strategy, quote);
    return true;
  }
  const [d, s] = String(id).split('/');
  kb.doc = c.doc_id || d;
  kb.strategy = c.strategy || s || kb.strategy;
  kb.focus = c.chunk_id || id;
  kb.quote = quote || '';
  kb.tab = 'chunks';
  openWindow('kb');
  return true;
};

/* ---------- действия ---------- */

Object.assign(actions, {
  async kbTab(tab) {
    if (!kbTabs.some(([id]) => id === tab)) return;
    if (kb.tab === 'search') kbReadForm();
    if (kb.tab === 'ask') kbAskReadForm();
    kb.tab = tab;
    if (tab !== 'chunks') { kb.focus = ''; kb.quote = ''; }
    kbPaint('tabs', 'body');
    if (tab === 'chunks') await kbShowChunks(false);
    if (tab === 'report' && (!kb.report || !kb.report.ok)) {
      await kbLoadReport();
      if (kb.tab === 'report') kbPaint('body');
    }
    if (tab === 'modes' && kbBase()) {
      await kbLoadModes();
      if (kb.tab === 'modes') kbPaint('body');
    }
    if ((tab === 'ask' || tab === 'qa') && kbBase()) {
      await kbLoadTab();
      if (kb.tab === tab) kbPaint('body');
      if (tab === 'qa' && kbQaRunning()) kbQaPoll();
    }
  },
  async kbOpenDoc(id) {
    if (!id) return;
    kb.doc = id;
    kb.focus = '';
    kb.quote = '';
    kb.tab = 'chunks';
    kbPaint('tabs');
    await kbShowChunks(true);
    const t = $('kb-text');
    if (t) t.scrollIntoView({ block: 'start' });
  },
  async kbOpenChunk(id, el) {
    if (!id || !el) return;
    await kbGoChunk(id, el.dataset.doc, el.dataset.index, '');
  },
  // v24: цитата ответа rag+cite — к чанку с подсветкой процитированного.
  async kbOpenQuote(id, el) {
    if (!id || !el) return;
    await kbGoChunk(id, el.dataset.doc, el.dataset.index, el.dataset.quote || '');
  },
  async kbPickDoc(id) {
    if (!id) return;
    kb.doc = id;
    kb.focus = '';
    kb.quote = '';
    await kbShowChunks(true);
  },
  async kbPickStrategy(s) {
    if (!s || s === kb.strategy) return;
    kb.strategy = s;
    // Чанк из другой стратегии — не тот, к которому прокручивать.
    if (kb.focus && !kb.focus.includes('/' + s + '/')) { kb.focus = ''; kb.quote = ''; }
    const top = $('window-body') ? $('window-body').scrollTop : 0;
    await kbShowChunks(true);
    if (!kb.focus && $('window-body')) $('window-body').scrollTop = top;
  },
  kbSearch() { kbSearch(); },
  kbMxSplit(s) {
    if (!s) return;
    kb.mxSplit = s;
    kbPaint('body');
  },
});

// kbPlaceButton — «База знаний» сразу за «MCP-серверы»: кнопки разделов
// встают за «Окна ▾» по мере ответов своих REST, порядок заранее не известен.
function kbPlaceButton() {
  const b = $('kb-button'), h = $('hub-button');
  if (b && h && h.nextElementSibling !== b) h.insertAdjacentElement('afterend', b);
}

kbRenderButton();
if ($('kb-button')) new MutationObserver(kbPlaceButton).observe($('kb-button').parentElement, { childList: true });
kbLoadInfo();
