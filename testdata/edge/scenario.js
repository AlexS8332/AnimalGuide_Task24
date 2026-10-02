'use strict';

/* Сценарий проверок интерфейса для headless Edge. Подключается после
   app.js, ходит по интерфейсу теми же кнопками, что и человек (клик по
   элементу с data-action уходит в общий обработчик app.js), и пишет итог
   в <pre id="test-log">: строка на проверку — «OK имя» или «FAIL имя —
   причина», в конце «DONE». Какой сценарий — из ?scenario=… в адресе. */

(function () {
  const log = document.createElement('pre');
  log.id = 'test-log';
  log.style.display = 'none';
  document.body.appendChild(log);
  const errors = [];
  window.addEventListener('error', e => errors.push('error: ' + e.message));
  window.addEventListener('unhandledrejection', e => errors.push('rejection: ' + (e.reason && e.reason.message || e.reason)));
  // Вопросы пользователю отвечаем сами: имя точки, ветки и т. п.
  window.prompt = () => 'из сценария';
  window.confirm = () => true;

  const write = line => { log.textContent += line + '\n'; };
  const sleep = ms => new Promise(r => setTimeout(r, ms));
  const q = sel => document.querySelector(sel);
  const qa = sel => [...document.querySelectorAll(sel)];
  const text = sel => { const el = q(sel); return el ? el.textContent.replace(/\s+/g, ' ').trim() : ''; };

  // until — ждёт, пока условие станет истинным (и возвращает его значение).
  async function until(what, fn, ms) {
    const end = Date.now() + (ms || 5000);
    for (;;) {
      let v;
      try { v = fn(); } catch (e) { v = null; }
      if (v) return v;
      if (Date.now() > end) throw new Error('не дождались: ' + what);
      await sleep(50);
    }
  }
  function assert(cond, msg) { if (!cond) throw new Error(msg); }
  function click(sel) {
    const el = typeof sel === 'string' ? q(sel) : sel;
    assert(el, 'нет элемента ' + sel);
    el.click();
    return el;
  }
  async function check(name, fn) {
    try {
      await fn();
      write('OK ' + name);
    } catch (e) {
      write('FAIL ' + name + ' — ' + String(e && e.message || e).replace(/\n/g, ' '));
    }
  }
  const windowOpen = () => $('window').open;
  async function openWin(name) {
    click('#windows-button');
    await until('список окон', () => windowOpen() && q('#window-body .wlist'));
    click(q(`#window-body [data-action="openWindow"][data-arg="${name}"]`));
    await until('окно ' + name, () => !text('#window-body').includes('загружаю') && $('window-title').textContent === app.windows[name].title && text('#window-body'));
  }
  const booted = () => until('загрузка диалога', () => app.conv && q('#feed').children.length, 8000);
  const exportURL = el => `/api/conversations/${app.conv.id}/export?kind=${encodeURIComponent(el.dataset.kind)}&id=${encodeURIComponent(el.dataset.arg)}`;

  const scenarios = {};

  /* Основной диалог: карточка рыси, раздел, узел дерева, реплика ведущему
     с правками профиля и памяти, точка, ветка «сравнение» со сравнением. */
  scenarios.main = async () => {
    await booted();

    await check('пульт: липкая шапка с показаниями', () => {
      const p = q('header.pult');
      assert(getComputedStyle(p).position === 'sticky', 'пульт не sticky');
      const r = text('#readings');
      assert(r.includes('Модель') && r.includes('Ходов') && r.includes('Собеседник'), 'показания: ' + r);
      assert(text('#readings').includes('me'), 'собеседник не показан');
    });

    await check('пульт: список диалогов и выбранный', () => {
      const opts = qa('#dialog-select option');
      assert(opts.length === 3, 'диалогов в списке ' + opts.length);
      assert($('dialog-select').value === app.conv.id, 'выбран не текущий диалог');
    });

    await check('ветки: дерево, активная ветка и точка сохранения', () => {
      const br = qa('#branches .branch');
      assert(br.length === 2, 'веток ' + br.length);
      assert(text('#branches .branch.active').includes('↳ сравнение: рысь и манул'), 'активна не ветка сравнения: ' + text('#branches .branch.active'));
      assert(qa('#branches [data-action="fork"]').some(b => b.textContent.includes('до сравнения')), 'нет кнопки точки «до сравнения»');
    });

    await check('ветки: «+ точка» ставит точку сохранения', async () => {
      const before = app.conv.checkpoints.length;
      click('#branches [data-action="mark"]');
      await until('новая точка', () => app.conv.checkpoints.length === before + 1);
      assert(qa('#branches [data-action="fork"]').some(b => b.textContent.includes('из сценария')), 'кнопки новой точки нет');
    });

    await check('механизмы: все из реестра, счётчик', () => {
      const m = qa('#mechanisms .mech');
      assert(m.length === app.meta.mechanisms.length && m.length >= 10, 'кнопок ' + m.length);
      const on = qa('#mechanisms .mech.on').length;
      assert(text('#mech-count') === `включено ${on} из ${m.length}`, 'счётчик: ' + text('#mech-count'));
      assert(m.every(b => b.title.includes('Выключен:')), 'в подсказке нет запасного пути');
    });

    await check('выключатель механизма: выкл и обратно', async () => {
      const sel = '#mechanisms .mech[data-arg="facts"]';
      assert(q(sel).classList.contains('on'), 'карточка фактов не включена');
      click(sel);
      await until('выключение', () => q(sel).classList.contains('off'));
      assert(app.conv.features.facts === false || !app.conv.mechanisms.find(m => m.name === 'facts').on, 'сервер не выключил');
      click(sel);
      await until('включение', () => q(sel).classList.contains('on'));
    });

    await check('контекст: шкала и бюджет', () => {
      assert(qa('#context .scale span').length >= 2, 'в шкале меньше двух частей');
      assert(text('#context .budget').includes('постоянная часть'), 'нет строки бюджета');
    });

    await check('лента: ходы и карточка с латынью', () => {
      assert(qa('#feed .turn').length === app.conv.turnList.length, 'ходов в ленте ' + qa('#feed .turn').length);
      const card = q('#feed article.acard');
      assert(card, 'нет карточки');
      assert(text('#feed .acard h3') === 'Обыкновенная рысь', 'название: ' + text('#feed .acard h3'));
      assert(text('#feed .acard .latin') === 'Lynx lynx', 'латынь: ' + text('#feed .acard .latin'));
      assert(qa('#feed .bubble.user.click').length >= 2, 'клики не помечены как клики');
    });

    await check('разделы: прочитанный раскрывается', async () => {
      const read = qa('#feed .acard details.section').find(d => d.textContent.includes('Питание'));
      assert(read, 'нет прочитанного раздела «Питание»');
      assert(read.querySelector('.s-read'), 'статус не «прочитан»');
      read.querySelector('summary').click();
      await until('раскрытие', () => read.open);
      assert(read.querySelector('.s-body').textContent.includes('Пересказ'), 'в разделе нет пересказа');
    });

    await check('разделы: непрочитанные с кнопкой «прочитать»', () => {
      const lynx = q('#feed .acard');
      assert(lynx.querySelector('h3').textContent === 'Обыкновенная рысь', 'первая карточка не рысь');
      const btns = [...lynx.querySelectorAll('[data-action="section"]')];
      assert(btns.length >= 3, 'кнопок «прочитать» ' + btns.length);
      assert(btns.every(b => b.dataset.card && b.dataset.topic && !b.disabled), 'кнопка без карточки/темы или выключена');
      assert(!btns.some(b => b.dataset.topic === 'diet'), 'прочитанный раздел снова предлагают прочитать');
    });

    await check('дерево: узлы кликабельны, сам вид не нажимается', () => {
      const nodes = qa('#feed .acard .tree [data-action="node"]');
      assert(nodes.length >= 2, 'узлов ' + nodes.length);
      assert(nodes.every(n => n.dataset.key && n.dataset.card), 'узел без ключа');
      assert(q('#feed .acard .tree button.self[disabled]'), 'сам вид не выделен');
    });

    await check('соседи узла: список видов «Кошачьи»', () => {
      const nb = q('#feed .acard .neighbors');
      assert(nb && nb.textContent.includes('Кошачьи'), 'нет блока соседей');
      assert(nb.querySelectorAll('[data-action="open"]').length >= 2, 'в соседях меньше двух видов');
    });

    await check('сравнение: таблица и выгрузка', async () => {
      const cmp = q('#feed .compare');
      assert(cmp, 'нет сравнения');
      assert(cmp.querySelectorAll('table.cmp tr').length >= 3, 'строк мало');
      assert(cmp.querySelector('td.nodata'), 'нет пометки «сведений нет»');
      const res = await fetch(exportURL(cmp.querySelector('[data-action="export"]')));
      assert(res.ok, 'выгрузка сравнения: HTTP ' + res.status);
    });

    await check('чипы: память и профиль под ответом', () => {
      const chips = qa('#feed .chips .chip').map(c => c.textContent);
      assert(chips.some(c => c.includes('🧠') && c.includes('интерес')), 'нет чипа памяти: ' + chips.join(' | '));
      assert(chips.some(c => c.includes('👤')), 'нет чипа профиля');
    });

    await check('панели человека: собеседник и память', () => {
      assert(text('#panel-person').includes('«me»'), 'нет панели собеседника');
      assert(text('#panel-person .chips').includes('Кратко') || qa('#panel-person .chip.ok').length > 0, 'анкета пуста');
      assert(text('#panel-memory').includes('хищники тайги'), 'в панели памяти нет записи');
    });

    await check('журнал: события последнего хода', () => {
      assert($('tab-events').classList.contains('active'), 'не открыт журнал');
      const evs = qa('#journal-body .ev');
      assert(evs.length > 0, 'событий нет');
      assert(text('#journal-turn').includes('событ'), 'нет счётчика событий');
    });

    await check('журнал: событие раскрывается', async () => {
      const ev = qa('#journal-body .ev').find(e => e.querySelector('.ev-detail'));
      assert(ev, 'нет события с подробностями');
      click(ev.querySelector('.ev-head'));
      await until('раскрытие', () => ev.classList.contains('open'));
      assert(getComputedStyle(ev.querySelector('.ev-detail')).display === 'block', 'подробности не видны');
    });

    await check('вкладка «Промпты»: системный промпт и блоки', async () => {
      click('#tab-prompts');
      await until('промпты', () => $('tab-prompts').classList.contains('active') && q('#journal-body .prompt-block'));
      assert(text('#journal-body').includes('системный промпт'), 'нет системного промпта');
      assert(text('#journal-body').includes('инструменты'), 'нет списка инструментов');
      click('#tab-events');
      await until('журнал', () => $('tab-events').classList.contains('active'));
    });

    await check('«журнал хода» переключает журнал', async () => {
      const first = app.conv.turnList[0].id;
      click(`#feed [data-action="selectTurn"][data-arg="${first}"]`);
      await until('выбор хода', () => app.selected === first && q(`#turn-${first}.selected`));
    });

    await check('«почему так» ведёт к событию журнала', async () => {
      const btn = qa('#feed .acard .why').find(b => b.dataset.call);
      assert(btn, 'нет кнопки «?» с вызовом');
      const call = btn.dataset.call;
      click(btn);
      await until('событие вызова', () => qa('#journal-body .ev.open').some(e => e.dataset.call === call));
      assert(app.selected === btn.dataset.turn, 'журнал не того хода');
    });

    await check('выгрузка карточки в markdown', async () => {
      const btn = q('#feed .acard [data-action="export"][data-kind="card"]');
      assert(btn, 'нет кнопки выгрузки');
      const res = await fetch(exportURL(btn));
      const body = await res.text();
      assert(res.ok, 'HTTP ' + res.status);
      assert(body.includes('Обыкновенная рысь') && body.includes('## Питание'), 'в markdown нет карточки: ' + body.slice(0, 120));
      assert((res.headers.get('Content-Disposition') || '').includes('attachment'), 'не вложение');
    });

    await check('окно «Окна»: список окон', async () => {
      click('#windows-button');
      await until('окно', () => windowOpen() && q('#window-body .wlist'));
      const names = qa('#window-body .wlist [data-action="openWindow"]').map(b => b.dataset.arg);
      ['file', 'people', 'memory', 'collections'].forEach(n => assert(names.includes(n), 'нет окна ' + n));
    });

    await check('окно «Файл диалога»', async () => {
      await openWin('file');
      assert(text('#window-body pre').includes('"schema"'), 'в файле нет schema');
      assert(text('#window-body .hint').length > 0, 'нет пути к файлу');
    });

    await check('окно «Картотека профилей»: правка анкеты', async () => {
      await openWin('people');
      const sel = q('#window-body select[data-field="level"]') || q('#window-body select[data-change="setProfileField"]');
      assert(sel, 'нет полей анкеты');
      const opt = [...sel.options].find(o => o.value && o.value !== sel.value);
      sel.value = opt.value;
      sel.dispatchEvent(new Event('change', { bubbles: true }));
      await until('тост', () => !$('toast').hidden && $('toast').textContent.includes('Анкета записана'));
      await until('перерисовка', () => q(`#window-body select[data-field="${sel.dataset.field}"]`).value === opt.value);
    });

    await check('окно «Память целиком»: запись и «забыть»', async () => {
      await openWin('memory');
      assert(text('#window-body table.grid').includes('интерес'), 'нет записи «интерес»');
      assert(q('#window-body [data-action="forgetMemory"]'), 'нет кнопки «забыть»');
    });

    await check('окно «Подборки»: список и выгрузка', async () => {
      await openWin('collections');
      assert(q('#window-body table.grid [data-action="continueCollection"]'), 'нет подборки');
      const a = q('#window-body a[href*="/export"]');
      const res = await fetch(a.getAttribute('href'));
      assert(res.ok && (await res.text()).includes('#'), 'выгрузка подборки');
      click('#window [data-action="closeWindow"]');
      await until('закрытие', () => !windowOpen());
    });

    await check('переход в другую ветку', async () => {
      const before = app.conv.turnList.length;
      const main = qa('#branches .branch').find(b => !b.classList.contains('active'));
      click(main.querySelector('[data-action="switchBranch"]'));
      await until('смена ветки', () => app.conv.turnList.length === before - 1 && !q('#feed .compare'));
      assert(q('#branches .branch.active') && !text('#branches .branch.active').includes('сравнение'), 'активна прежняя ветка');
    });

    await check('прокрутка: пульт у верха окна, журнал под ним', async () => {
      window.scrollTo(0, document.body.scrollHeight);
      await until('прокрутка', () => window.scrollY > 100, 2000);
      await sleep(100);
      const p = q('header.pult').getBoundingClientRect();
      assert(Math.abs(p.top) < 1, 'пульт съехал: top=' + p.top);
      const j = $('journal').getBoundingClientRect();
      assert(j.top >= p.bottom - 1, `журнал заходит под пульт: журнал ${Math.round(j.top)}, низ пульта ${Math.round(p.bottom)}`);
      assert(j.bottom <= window.innerHeight + 1, `журнал ниже окна: ${Math.round(j.bottom)} > ${window.innerHeight}`);
      assert(document.documentElement.scrollWidth <= window.innerWidth + 1, 'горизонтальная прокрутка');
    });
  };

  /* Диалог подборки: план утверждён, первый вид собран. */
  scenarios.collection = async () => {
    await booted();
    await check('подборка: панель с этапами', () => {
      const p = q('#panel-collection');
      assert(p, 'нет панели подборки');
      const steps = qa('#panel-collection .stage-step').map(s => s.textContent);
      assert(steps.join(',') === 'план,сбор,сверка,принята', 'этапы: ' + steps);
      assert(text('#panel-collection .stage-step.now') === 'сбор', 'текущий этап: ' + text('#panel-collection .stage-step.now'));
      assert(q('#panel-collection .stage-step.past'), 'нет пройденного этапа');
    });
    await check('подборка: виды и права этапа', () => {
      const items = qa('#panel-collection .items .item').map(i => i.textContent);
      assert(items.length === 2, 'видов ' + items.length);
      assert(items[0].includes('●'), 'первый вид не отмечен собранным: ' + items[0]);
      assert(qa('#panel-collection .chip.ok').length > 0, 'нет разрешённых инструментов');
      assert(qa('#panel-collection .chip.warn').some(c => c.textContent.includes('🔒')), 'нет запертых инструментов');
    });
    await check('подборка: чипы переходов в ленте', () => {
      const chips = qa('#feed .chip').map(c => c.textContent);
      assert(chips.some(c => c.includes('📋') && c.includes('→')), 'нет чипа перехода этапа: ' + chips.join(' | '));
    });
    await check('подборка: карточка собранного вида', () => {
      assert(q('#feed article.acard'), 'карточки вида нет в ленте');
      assert(q('#panel-collection [data-action="open"]'), 'у вида нет ссылки «карточка»');
    });
  };

  /* Пустой диалог: заглушка, затем «Новый диалог» и живой ход. */
  scenarios.empty = async () => {
    await until('загрузка', () => app.conv && q('#feed .empty-feed'), 8000);
    await check('пустое состояние: подсказка и примеры', () => {
      assert(text('#feed .empty-feed h2') === 'Спросите про животное', 'заголовок');
      assert(qa('#feed .examples [data-action="example"]').length === 5, 'примеров не пять');
      assert(text('#context').includes('появится'), 'шкала контекста без хода');
      assert(text('#journal-body').includes('Журнал хода появится'), 'журнал без хода');
    });
    await check('«Новый диалог» заводит пустой диалог', async () => {
      const before = app.convs.length, old = app.conv.id;
      click('#new-dialog');
      await until('новый диалог', () => app.conv.id !== old && app.convs.length === before + 1);
      assert(location.hash === '#c=' + app.conv.id, 'адрес не обновлён: ' + location.hash);
      assert(q('#feed .empty-feed'), 'новый диалог не пустой');
    });
    await check('живой ход из композера: карточка по мере сборки', async () => {
      $('composer-text').value = 'рысь';
      q('#composer button[type="submit"]').click();
      await until('ход пошёл', () => app.live || app.conv.turnList.length, 5000);
      await until('ход записан', () => !app.live && app.conv.turnList.length === 1, 20000);
      assert(text('#feed .acard h3') === 'Обыкновенная рысь', 'карточки нет: ' + text('#feed'));
      assert(!$('send-button').disabled, 'кнопка «Отправить» осталась выключенной');
      assert(qa('#dialog-select option').some(o => o.selected && o.textContent.includes('(1)')), 'список диалогов не обновился');
    });
  };

  /* «Интересные факты»: подставной демон (edgeFacts в edge_test.go) —
     подключён, лента из трёх выпусков (один с разметкой в текстах), две
     сводки; поиск «ошибка» отвечает 422. */
  const factsCalls = [];
  function spyFetch() {
    const orig = window.fetch;
    window.fetch = (url, opts) => {
      factsCalls.push({ url: String(url), method: (opts && opts.method) || 'GET', body: opts && opts.body });
      return orig(url, opts);
    };
  }
  const factsBody = () => text('#facts-body');
  async function openFacts() {
    await until('кнопка раздела на пульте', () => q('#facts-button'), 8000);
    click('#facts-button');
    await until('окно фактов', () => windowOpen() && q('#facts-root') && $('window-title').textContent === 'Интересные факты', 8000);
  }

  scenarios.facts = async () => {
    spyFetch();
    await booted();
    let asked = '';
    window.confirm = msg => { asked = msg; return true; };

    await check('факты: кнопка на пульте с состоянием демона', async () => {
      const b = await until('кнопка', () => q('#facts-button'));
      assert(b.closest('#pult'), 'кнопка не на пульте');
      await until('точка «подключён»', () => q('#facts-button .facts-dot.ok'));
      assert(b.title.includes('подключён'), 'подсказка: ' + b.title);
    });

    await check('факты: окно и строка состояния', async () => {
      await openFacts();
      const s = text('#facts-status');
      assert(s.includes('демон подключён'), 'нет «подключён»: ' + s);
      assert(s.includes('через 35 мин'), 'нет следующего выпуска: ' + s);
      assert(s.includes('осталось $0.4877'), 'нет остатка бюджета: ' + s);
      assert(app.facts.timer, 'опрос статуса не запущен');
    });

    await check('факты: лента карточками', () => {
      const cards = qa('#facts-body .facts-card');
      assert(cards.length === 3, 'карточек ' + cards.length);
      const c = cards[0];
      assert(text('#facts-body .facts-card h3') === 'Манул: кошка с круглыми зрачками', 'заголовок: ' + text('#facts-body .facts-card h3'));
      assert(c.querySelector('.facts-species').textContent.includes('Манул') && c.querySelector('.latin').textContent === 'Otocolobus manul', 'вид');
      assert(c.textContent.includes('МСОП: LC'), 'нет статуса МСОП');
      assert(c.querySelector('.facts-lead').textContent.includes('холодных степях'), 'нет вступления');
      assert(c.querySelectorAll('.facts-facts li').length === 3, 'фактов не три');
      assert(c.querySelector('.facts-meta').textContent.includes('$0.0021'), 'нет цены');
      assert(cards[1].classList.contains('facts-thin') && cards[1].textContent.includes('мало фактов'), 'тонкий выпуск не помечен');
    });

    await check('факты: номера источников это ссылки в новом окне', () => {
      const links = qa('#facts-body .facts-card:first-child a.facts-src');
      assert(links.length === 3, 'ссылок ' + links.length);
      assert(links.every(a => a.target === '_blank' && a.rel.includes('noopener') && /^https:/.test(a.href)), 'ссылка без _blank/noopener/https');
      assert(links[0].textContent === 'S1' && links[0].href.includes('mammaldiversity'), 'первая ссылка: ' + links[0].outerHTML);
      assert(!q('#facts-root a[href^="javascript"]'), 'javascript: стал ссылкой');
      assert(qa('#facts-body .facts-src.none').some(s => s.textContent === 'S3'), 'источник без годной ссылки не показан номером');
    });

    await check('факты: вне ареала MDD с пояснением', () => {
      const r = text('#facts-body .facts-card:first-child .facts-range');
      assert(r.includes('вне ареала MDD: Germany, Japan'), 'отметка: ' + r);
      assert(r.includes('зоопарки') && r.includes('интродукция'), 'нет пояснения: ' + r);
    });

    await check('факты: разметка из ответов показана буквами', () => {
      assert(!q('#facts-root img') && !q('#facts-root script') && !q('#facts-root b') && !q('#facts-root i'), 'в окне появился элемент из текста выпуска');
      assert(window.__xss === undefined, 'выполнился код из выпуска: ' + window.__xss);
      const hedgehog = qa('#facts-body .facts-card')[1];
      assert(hedgehog.textContent.includes('<img src=x onerror='), 'текст факта не виден буквами');
      assert(hedgehog.querySelector('h3').textContent.startsWith('<script>'), 'заголовок: ' + hedgehog.querySelector('h3').textContent);
    });

    await check('факты: клик по карточке открывает подробности', async () => {
      click('#facts-body .facts-card[data-issue="3"] .facts-lead');
      await until('подробности', () => q('#facts-body .facts-card.full') && q('#facts-body .facts-spend'));
      const b = factsBody();
      assert(b.includes('Манул — предок домашней кошки') && b.includes('причина: источник этого не говорит'), 'нет отброшенного факта с причиной');
      assert(b.includes('1 532') && b.includes('88'), 'нет наблюдений: ' + b.slice(0, 200));
      assert(q('#facts-body tr.facts-out'), 'страна вне ареала не выделена');
      assert(qa('#facts-body .facts-spend tr').length === 4 && b.includes('редактор') && b.includes('проверяющий'), 'нет расхода по шагам');
      assert(factsCalls.some(c => c.url.endsWith('/api/facts/issue?id=3')), 'не спросили /issue?id=3');
      click('#facts-back');
      await until('назад к ленте', () => qa('#facts-body .facts-card').length === 3);
    });

    await check('факты: клик по источнику не открывает подробности', async () => {
      const a = q('#facts-body .facts-card:first-child a.facts-src');
      a.addEventListener('click', e => e.preventDefault(), { once: true }); // не открывать вкладку в тесте
      const before = factsCalls.length;
      a.click();
      await sleep(200);
      assert(!q('#facts-body .facts-card.full'), 'открылись подробности');
      assert(!factsCalls.slice(before).some(c => c.url.includes('/issue')), 'ушёл запрос выпуска');
    });

    await check('факты: поиск над лентой', async () => {
      $('facts-query').value = 'манул';
      click('#facts-search button[type="submit"]');
      await until('выдача', () => q('#facts-body table.facts-found'));
      const rows = qa('#facts-body tr.facts-row');
      assert(rows.length === 1 && rows[0].textContent.includes('Otocolobus manul'), 'строк ' + rows.length);
      assert(factsCalls.some(c => c.url.includes('/api/facts/search?text=' + encodeURIComponent('манул'))), 'не ушёл /search');
      click(rows[0]);
      await until('выпуск из поиска', () => q('#facts-body .facts-card.full'));
      assert(text('#facts-back').includes('к поиску'), 'кнопка назад: ' + text('#facts-back'));
      click('#facts-back');
      await until('снова выдача', () => q('#facts-body table.facts-found'));
    });

    await check('факты: 422 на поиске показан текстом ошибки', async () => {
      $('facts-query').value = 'ошибка';
      click('#facts-search button[type="submit"]');
      await until('ошибка', () => q('#facts-body .facts-error'));
      assert(text('#facts-body .facts-error').includes('since: ожидалась дата'), 'текст: ' + text('#facts-body .facts-error'));
      assert(!q('#facts-body .facts-down'), '422 показан как «демон не подключён»');
      click('#facts-body [data-action="factsSearchReset"]');
      await until('лента', () => qa('#facts-body .facts-card').length === 3);
    });

    await check('факты: сводки: последняя, цифры, прошлые', async () => {
      click('#facts-tab-summaries');
      await until('сводка', () => q('#facts-summary'));
      const s = text('#facts-summary');
      assert(s.includes('Сводка №2') && s.includes('три выпуска'), 'нет текста сводки: ' + s.slice(0, 120));
      const figs = qa('#facts-summary .facts-fig').map(f => f.textContent.replace(/\s+/g, ' '));
      assert(figs.some(f => f.includes('3') && f.includes('выпусков')), 'нет числа выпусков: ' + figs);
      assert(figs.some(f => f.includes('отброшено 2 (29%)')), 'нет отбраковки: ' + figs);
      assert(figs.some(f => f.includes('$0.0081')), 'нет расхода: ' + figs);
      assert(s.includes('Carnivora') && s.includes('GBIF не ответил'), 'нет отрядов или сбоев');
      const rows = qa('#facts-body .facts-sum-row');
      assert(rows.length === 2, 'прошлых сводок ' + rows.length);
      click(rows[1]);
      await until('сводка №1', () => text('#facts-summary h3').includes('Сводка №1'));
      assert(q('#facts-body .facts-sum-row.current[data-arg="1"]'), 'открытая не отмечена');
    });

    await check('факты: «Собрать выпуск»: подтверждение, ожидание, POST, лента', async () => {
      click('#facts-tab-feed');
      await until('лента', () => qa('#facts-body .facts-card').length === 3);
      click('#facts-run-issue');
      assert(asked.includes('$0.002'), 'подтверждение без цены: ' + asked);
      await until('ожидание', () => q('#facts-wait .thinking'));
      assert($('facts-run-issue').disabled && $('facts-run-summary').disabled, 'кнопки не заблокированы');
      $('facts-run-issue').click(); // повторное нажатие — мимо
      await until('итог', () => q('#facts-result'), 8000);
      const posts = factsCalls.filter(c => c.method === 'POST' && c.url.endsWith('/api/facts/run'));
      assert(posts.length === 1, 'POST /run ушло ' + posts.length);
      assert(JSON.parse(posts[0].body).job === 'issue', 'тело: ' + posts[0].body);
      assert(text('#facts-result').includes('готово') && text('#facts-result').includes('Горный тапир'), 'итог: ' + text('#facts-result'));
      await until('лента обновилась', () => qa('#facts-body .facts-card').length === 4);
      assert(text('#facts-body .facts-card h3').includes('Горный тапир'), 'новый выпуск не первым');
      assert(!$('facts-run-issue').disabled, 'кнопка осталась выключенной');
    });

    await check('факты: «Собрать сводку»: POST с hours=24 и новая сводка', async () => {
      asked = '';
      click('#facts-run-summary');
      assert(asked.includes('$0.002'), 'нет подтверждения');
      await until('итог', () => q('#facts-result'), 8000);
      const post = factsCalls.find(c => c.method === 'POST' && c.url.endsWith('/api/facts/summary/build'));
      assert(post && JSON.parse(post.body).hours === 24, 'тело: ' + (post && post.body));
      assert(text('#facts-result').includes('Сводка №3 собрана'), 'итог: ' + text('#facts-result'));
      await until('сводка №3', () => text('#facts-summary h3').includes('Сводка №3'));
    });

    await check('факты: отказ в подтверждении, запроса нет', async () => {
      window.confirm = () => false;
      const before = factsCalls.filter(c => c.method === 'POST').length;
      click('#facts-run-issue');
      await sleep(100);
      assert(factsCalls.filter(c => c.method === 'POST').length === before, 'POST ушёл без согласия');
      window.confirm = msg => { asked = msg; return true; };
    });

    await check('факты: закрытие окна останавливает опрос', async () => {
      click('#window [data-action="closeWindow"]');
      await until('закрытие', () => !windowOpen());
      await until('опрос остановлен', () => !app.facts.timer, 2000);
    });
  };

  /* Демон не отвечает: status — conn=down с подсказкой, остальное 503. */
  scenarios['facts-down'] = async () => {
    await booted();
    await check('факты-503: кнопка на пульте, демон не отвечает', async () => {
      await until('красная точка', () => q('#facts-button .facts-dot.bad'), 8000);
      assert(q('#facts-button').title.includes('не отвечает'), 'подсказка: ' + q('#facts-button').title);
    });
    await check('факты-503: строка состояния с подсказкой', async () => {
      await openFacts();
      const s = text('#facts-status');
      assert(s.includes('демон не отвечает') && s.includes('animals-mcp -http 127.0.0.1:8766'), 'состояние: ' + s);
    });
    await check('факты-503: заглушка вместо ленты, кнопки выключены', () => {
      const d = q('#facts-body .facts-down');
      assert(d && d.textContent.includes('не подключён') && d.textContent.includes('animals-mcp -http'), 'нет заглушки: ' + factsBody());
      assert(!q('#facts-search'), 'строка поиска при недоступном демоне');
      assert(!q('#facts-body .facts-card') && !q('#facts-body .facts-error'), 'лишнее в ленте');
      assert($('facts-run-issue').disabled && $('facts-run-summary').disabled, 'кнопки запуска активны');
    });
    await check('факты-503: сводки, та же заглушка', async () => {
      click('#facts-tab-summaries');
      await until('заглушка сводок', () => q('#facts-body .facts-down') && $('facts-tab-summaries').classList.contains('active'));
      assert(qa('#facts-body .facts-down').length === 1, 'заглушек ' + qa('#facts-body .facts-down').length);
    });
  };

  /* Конвейер: вкладка окна «Интересные факты», подставной REST
     (edgePipe в edge_test.go) — каждый опрос продвигает прогон на полшага;
     вид «ошибка» обрывает цепочку на summarize. В итогах шагов, тексте
     ошибки и превью файла — разметка, она не должна стать элементами. */
  const pipeState = tool => { const el = q(`.pipe-step[data-tool="${tool}"]`); return el ? el.dataset.status : ''; };
  const pipePosts = () => factsCalls.filter(c => c.method === 'POST' && c.url.endsWith('/api/pipeline/runs'));
  async function openPipe() {
    await openFacts();
    click('#facts-tab-pipeline');
    await until('вкладка конвейера', () => q('#pipe-root') && $('facts-tab-pipeline').classList.contains('active') && q('#pipe-runs') && !text('#pipe-runs').includes('загружаю'));
  }

  scenarios.pipeline = async () => {
    spyFetch();
    await booted();
    window.__xss = undefined;

    await check('конвейер: вкладка и форма', async () => {
      await openPipe();
      const qi = $('pipe-query');
      assert(qi && qi.placeholder === 'манул, Otocolobus manul или 1006010', 'поле вида: ' + (qi && qi.placeholder));
      assert($('pipe-random') && $('pipe-random').type === 'checkbox', 'нет флажка «случайный вид»');
      assert($('pipe-format-md').checked && $('pipe-pass-inline').checked && $('pipe-mode-code').checked, 'умолчания формы');
      assert(q('#pipe-mode-agent') && q('#pipe-format-json') && q('#pipe-pass-ref'), 'нет вариантов формата, передачи или исполнителя');
      assert(text('#pipe-run') === 'Запустить цепочку' && !$('pipe-run').disabled, 'кнопка запуска');
    });

    await check('конвейер: схема из трёх шагов со стрелками', () => {
      const steps = qa('#pipe-chain .pipe-step');
      assert(steps.map(s => s.dataset.tool).join(',') === 'search,summarize,save_to_file', 'шаги: ' + steps.map(s => s.dataset.tool));
      assert(steps.every(s => s.dataset.status === 'pending' && s.textContent.includes('ожидает')), 'не все «ожидает»');
      assert(qa('#pipe-chain .pipe-arrow').length === 2, 'стрелок не две');
      assert(text('#pipe-runs').includes('Прогонов ещё не было'), 'список: ' + text('#pipe-runs'));
    });

    await check('конвейер: пустой вид: подсказка, запроса нет', async () => {
      $('pipe-query').value = '';
      click('#pipe-run');
      await until('подсказка', () => q('#pipe-error'));
      assert(text('#pipe-error').includes('случайный вид'), 'текст: ' + text('#pipe-error'));
      assert(!pipePosts().length, 'POST ушёл с пустым видом');
    });

    await check('конвейер: запуск: POST с параметрами формы', async () => {
      $('pipe-query').value = 'манул';
      $('pipe-query').dispatchEvent(new Event('input', { bubbles: true }));
      click('#pipe-pass-ref');
      click('#pipe-run');
      await until('POST', () => pipePosts().length === 1);
      const body = JSON.parse(pipePosts()[0].body);
      assert(body.query === 'манул' && body.random === false && body.format === 'md' && body.pass === 'ref' && body.mode === 'code',
        'тело: ' + pipePosts()[0].body);
      assert(!q('#pipe-error'), 'осталась подсказка о пустом виде');
    });

    await check('конвейер: шаги по мере выполнения, анимация у идущего', async () => {
      await until('search идёт', () => pipeState('search') === 'running');
      assert(pipeState('summarize') === 'pending' && pipeState('save_to_file') === 'pending', 'остальные не ждут');
      assert($('pipe-run').disabled, 'кнопка не заблокирована');
      const st = q('.pipe-step[data-tool="search"]');
      assert(getComputedStyle(st, '::after').animationName === 'pipe-run', 'нет анимации: ' + getComputedStyle(st, '::after').animationName);
      assert(q('#pipe-status .thinking'), 'нет «идёт» в строке прогона');
      await until('summarize идёт', () => pipeState('summarize') === 'running');
      assert(pipeState('search') === 'ok', 'search не готов');
      $('pipe-run').click(); // повторное нажатие — мимо
    });

    await check('конвейер: итог: три шага готовы, проверки и отпечатки', async () => {
      await until('конец', () => q('#pipe-result'), 8000);
      assert(['search', 'summarize', 'save_to_file'].every(t => pipeState(t) === 'ok'), 'не все готовы');
      assert(pipePosts().length === 1, 'POST ушёл повторно: ' + pipePosts().length);
      const s1 = q('.pipe-step[data-tool="search"]');
      assert(s1.querySelector('.pipe-summary').textContent.includes('Манул (Otocolobus manul): 9 материалов'), 'итог шага');
      assert(s1.querySelector('.pipe-digest code').textContent === '1111aaaa2222', 'отпечаток: ' + s1.querySelector('.pipe-digest code').textContent);
      const checks = qa('.pipe-step[data-tool="summarize"] .pipe-checks li');
      assert(checks.length === 3 && checks.every(li => li.classList.contains('ok') && li.textContent.includes('✓')), 'проверки: ' + checks.map(c => c.textContent));
      const links = qa('#pipe-chain .pipe-link');
      assert(links.length === 2 && links[0].textContent.includes('вход = выход шага 1 ✓') && links[1].textContent.includes('вход = выход шага 2 ✓'),
        'стрелки: ' + links.map(l => l.textContent));
      assert(text('.pipe-step[data-tool="summarize"] .pipe-meta').includes('$0.0021'), 'цена шага');
      assert(text('.pipe-step[data-tool="search"] .pipe-meta').includes('1,2 с'), 'время шага: ' + text('.pipe-step[data-tool="search"] .pipe-meta'));
      assert(text('#pipe-status').includes('готово'), 'строка прогона: ' + text('#pipe-status'));
    });

    await check('конвейер: файл: путь, размер, sha256, превью, цена и время', () => {
      assert(text('#pipe-file') === 'exports/otocolobus-manul.md', 'путь: ' + text('#pipe-file'));
      assert(text('#pipe-size') === '2,1 КБ', 'размер: ' + text('#pipe-size'));
      assert(text('#pipe-sha') === '5eed5eed0123', 'sha: ' + text('#pipe-sha'));
      const pre = $('pipe-preview');
      assert(pre && pre.tagName === 'PRE' && pre.textContent.includes('# Манул') && pre.textContent.includes('Зрачки манула'), 'превью');
      const r = text('#pipe-result');
      assert(r.includes('$0.0021') && r.includes('всего'), 'итог: ' + r);
      assert(!$('pipe-run').disabled, 'кнопка осталась выключенной');
    });

    await check('конвейер: разметка из данных не исполняется', () => {
      assert(window.__xss === undefined, 'исполнился код: __xss=' + window.__xss);
      assert(!q('#pipe-root img') && !q('#pipe-root script') && !qa('#pipe-root b').some(b => b.textContent === 'проверены'), 'элемент из данных в окне');
      assert(text('#pipe-preview').includes('<script>window.__xss=22</script>'), 'разметка превью не показана буквами');
      assert(text('.pipe-step[data-tool="search"] .pipe-summary').includes('<img src=x'), 'разметка итога не показана буквами');
    });

    await check('конвейер: список последних прогонов', async () => {
      await until('строка', () => q('#pipe-runs tr.pipe-run-row'));
      const rows = qa('#pipe-runs tr.pipe-run-row');
      assert(rows.length === 1 && rows[0].textContent.includes('манул') && rows[0].textContent.includes('готово') &&
        rows[0].textContent.includes('exports/otocolobus-manul.md'), 'строка: ' + (rows[0] && rows[0].textContent));
      assert(rows[0].classList.contains('current'), 'открытый прогон не отмечен');
    });

    await check('конвейер: ошибка шага красным, с текстом', async () => {
      $('pipe-query').value = 'ошибка';
      $('pipe-query').dispatchEvent(new Event('input', { bubbles: true }));
      click('#pipe-mode-agent');
      click('#pipe-run');
      await until('конец', () => q('#pipe-result.bad'), 8000);
      const body = JSON.parse(pipePosts()[1].body);
      assert(body.mode === 'agent' && body.pass === 'ref', 'тело: ' + pipePosts()[1].body);
      assert(pipeState('search') === 'ok' && pipeState('summarize') === 'failed' && pipeState('save_to_file') === 'pending', 'состояния шагов');
      const err = q('.pipe-step[data-tool="summarize"] .pipe-step-error');
      assert(err && err.textContent.includes('редактор не ответил вовремя'), 'текст шага');
      assert(getComputedStyle(err).color === getComputedStyle(q('#pipe-result.bad')).color, 'ошибка не тем цветом');
      assert(text('#pipe-result').includes('на шаге 2 (summarize)') && text('#pipe-result').includes('редактор не ответил'), 'итог: ' + text('#pipe-result'));
      assert(window.__xss === undefined && !q('#pipe-root script'), 'исполнился код из текста ошибки');
      await until('два прогона в списке', () => qa('#pipe-runs tr.pipe-run-row').length === 2);
      assert(q('#pipe-runs tr.pipe-run-row .chip.bad'), 'ошибка не отмечена в списке');
    });

    await check('конвейер: случайный вид: поле выключено, в запросе random', async () => {
      click('#pipe-random');
      await until('поле выключено', () => $('pipe-query').disabled);
      click('#pipe-run');
      await until('POST', () => pipePosts().length === 3);
      const body = JSON.parse(pipePosts()[2].body);
      assert(body.random === true && body.query === '', 'тело: ' + pipePosts()[2].body);
      await until('конец', () => q('#pipe-result.ok'), 8000);
      assert(text('#pipe-status').includes('случайный вид'), 'строка: ' + text('#pipe-status'));
    });

    await check('конвейер: старый прогон из списка', async () => {
      const row = qa('#pipe-runs tr.pipe-run-row').find(r => r.dataset.arg === 'p1');
      click(row);
      await until('прогон p1', () => q('#pipe-status[data-run="p1"]') && q('#pipe-result.ok'));
      assert(q('#pipe-runs tr.pipe-run-row.current[data-arg="p1"]'), 'не отмечен');
    });

    await check('конвейер: форма переживает перерисовку окна', async () => {
      $('pipe-query').value = 'рысь';
      $('pipe-query').dispatchEvent(new Event('input', { bubbles: true }));
      click('#facts-tab-feed');
      await until('лента', () => q('#facts-body .facts-card'));
      click('#facts-tab-pipeline');
      await until('вкладка', () => q('#pipe-root'));
      assert($('pipe-query').value === 'рысь' && $('pipe-mode-agent').checked && $('pipe-random').checked, 'форма сброшена');
    });
  };

  /* Снимки экрана: только довести страницу до нужного вида. */
  scenarios['shot-top'] = async () => { await until('загрузка', () => app.conv, 8000); await sleep(300); };
  scenarios['shot-bottom'] = async () => {
    await booted();
    await sleep(300);
    document.body.scrollTop = document.body.scrollHeight;
    window.scrollTo(0, document.body.scrollHeight);
  };
  scenarios['shot-people'] = async () => { await booted(); await openWin('people'); };
  scenarios['shot-facts'] = async () => { await booted(); await openFacts(); };
  scenarios['shot-facts-detail'] = async () => {
    await booted();
    await openFacts();
    click('#facts-body .facts-card[data-issue="3"] .facts-lead');
    await until('подробности', () => q('#facts-body .facts-spend'));
  };
  scenarios['shot-facts-summary'] = async () => {
    await booted();
    await openFacts();
    click('#facts-tab-summaries');
    await until('сводка', () => q('#facts-summary'));
  };
  scenarios['shot-facts-down'] = async () => { await booted(); await openFacts(); };
  scenarios['shot-pipeline'] = async () => {
    await booted();
    await openPipe();
    $('pipe-query').value = 'манул';
    $('pipe-query').dispatchEvent(new Event('input', { bubbles: true }));
    click('#pipe-run');
    await until('итог', () => q('#pipe-result'), 8000);
  };

  // ===== Окно «MCP-серверы» (v20) =====

  /* Подставной REST (edgeHub в edge_test.go): настоящий hubapi с подставным
     реестром из трёх серверов (до «Подключить все» — idle) и прогоном,
     который двигается по опросам окна: каждый GET прогона завершает идущий
     вызов и начинает следующий. Флоу — 13 вызовов, facts_get отвечает
     ошибкой, в итоге одна проверка ⚠. В аргументах, ответе, превью и
     причине скрытого маршрута — разметка: она не должна стать элементами. */
  const hubPosts = path => factsCalls.filter(c => c.method === 'POST' && c.url.endsWith('/api/hub/' + path));
  const rgb = el => getComputedStyle(el).color;
  async function openHub() {
    await until('кнопка на пульте', () => q('#hub-button'), 8000);
    click('#hub-button');
    await until('окно серверов', () => windowOpen() && q('#hub-root') && $('window-title').textContent === 'MCP-серверы' && q('#hub-servers-root .hub-bar'), 8000);
  }
  async function hubConnect() {
    click('#hub-connect');
    await until('подключены', () => qa('.hub-server[data-status="ok"]').length === 3 && q('#hub-routes'), 8000);
  }
  async function hubStartFlow() {
    click('[data-hub-tab="flow"]');
    await until('форма флоу', () => q('#hub-form') && q('#hub-preset option') && q('#hub-runs table, #hub-runs p'));
    click('#hub-run');
  }

  scenarios.hub = async () => {
    spyFetch();
    await booted();
    window.__xss = undefined;

    await check('серверы: окно в списке окон, кнопка на пульте', async () => {
      click('#windows-button');
      await until('список окон', () => windowOpen() && q('#window-body .wlist'));
      assert(q('#window-body .wlist [data-arg="hub"]') && text('#window-body .wlist [data-arg="hub"]') === 'MCP-серверы', 'нет окна hub в списке');
      click('#window [data-action="closeWindow"]');
      await until('закрытие', () => !windowOpen());
      const b = await until('кнопка', () => q('#hub-button'));
      assert(b.closest('#pult'), 'кнопка не на пульте');
      await until('три точки серверов', () => qa('#hub-button .hub-pdot').length === 3);
      assert(b.title.includes('sources: не подключался'), 'подсказка: ' + b.title);
    });

    await check('серверы: три сервера до подключения', async () => {
      await openHub();
      assert(q('[data-hub-tab="servers"]').classList.contains('active'), 'открыта не вкладка «Серверы»');
      const rows = qa('.hub-server');
      assert(rows.map(r => r.dataset.name).join(',') === 'sources,daemon,notes', 'серверы: ' + rows.map(r => r.dataset.name));
      assert(rows.every(r => r.dataset.status === 'idle' && r.textContent.includes('не подключался')), 'не все idle');
      assert(text('.hub-server[data-name="daemon"] .hub-transport') === 'HTTP' && text('.hub-server[data-name="notes"] .hub-transport') === 'stdio', 'транспорт');
      assert(text('.hub-server[data-name="daemon"] .hub-addr') === 'http://127.0.0.1:8766/mcp', 'адрес: ' + text('.hub-server[data-name="daemon"] .hub-addr'));
      assert(q('#hub-routes-empty') && !q('.hub-route'), 'маршруты до подключения');
      assert(!hubPosts('connect').length, 'окно подключилось само');
    });

    await check('серверы: «Подключить все»: статусы, initialize, PID, цвета', async () => {
      click('#hub-connect');
      assert($('hub-connect').disabled && text('#hub-connect').includes('подключаю'), 'нет ожидания: ' + text('#hub-connect'));
      await until('подключены', () => qa('.hub-server[data-status="ok"]').length === 3, 8000);
      assert(hubPosts('connect').length === 1, 'POST connect ушло ' + hubPosts('connect').length);
      const d = text('.hub-server[data-name="daemon"]');
      assert(d.includes('animals-daemon') && d.includes('20.0.0') && d.includes('5120') && d.includes('3 выдано') && d.includes('5 скрыто'), 'строка демона: ' + d);
      assert(text('.hub-server[data-name="sources"] .hub-status') === 'подключён', 'статус');
      assert(getComputedStyle(q('.hub-server[data-name="sources"] .hub-swatch')).backgroundColor === 'rgb(47, 107, 79)', 'цвет sources');
      assert(getComputedStyle(q('.hub-server[data-name="notes"] .hub-swatch')).backgroundColor === 'rgb(165, 112, 26)', 'цвет notes');
      assert(qa('#hub-button .hub-pdot.ok').length === 3, 'точки на пульте не обновились');
    });

    await check('маршруты: инструмент → сервер, скрытые приглушены с причиной', () => {
      const rows = qa('.hub-route');
      assert(rows.length === 18 && qa('.hub-route.hidden').length === 6, `маршрутов ${rows.length}, скрытых ${qa('.hub-route.hidden').length}`);
      const mdd = q('.hub-route[data-tool="mdd_get"]');
      assert(mdd.dataset.server === 'daemon' && !mdd.classList.contains('hidden') && mdd.textContent.includes('выдан'), 'mdd_get');
      assert(rgb(mdd.querySelector('.hub-tag')) === 'rgb(44, 90, 133)', 'цвет метки: ' + rgb(mdd.querySelector('.hub-tag')));
      const dup = q('.hub-route.hidden[data-tool="search_wikipedia"]');
      assert(dup && dup.dataset.server === 'daemon' && dup.textContent.includes('дубль → sources'), 'дубль');
      assert(Number(getComputedStyle(dup.querySelector('td')).opacity) < 1, 'скрытая строка не приглушена');
      assert(q('.hub-route.hidden[data-tool="server_info"][data-server="notes"]').textContent.includes('служебный'), 'служебный');
      assert(q('.hub-route[data-tool="run_now"]').textContent.includes('не разрешён <img src=x'), 'причина не буквами');
      assert(text('.hub-sum').includes('модели выдано 12 инструментов, скрыто 6'), 'сводка: ' + text('.hub-sum'));
    });

    await check('флоу: вкладка, заготовка и вид по умолчанию', async () => {
      click('[data-hub-tab="flow"]');
      await until('форма', () => q('#hub-form') && q('[data-hub-tab="flow"]').classList.contains('active'));
      const opts = qa('#hub-preset option');
      assert(opts.length === 2 && $('hub-preset').value === 'passport' && opts[0].textContent === 'Паспорт вида в блокнот', 'заготовки');
      assert($('hub-species').value === 'манул', 'вид: ' + $('hub-species').value);
      assert(text('#hub-run') === 'Запустить флоу' && !$('hub-run').disabled, 'кнопка');
      assert(qa('#hub-legend .hub-tag').length === 3, 'легенда серверов');
      await until('список прогонов', () => text('#hub-runs').includes('Прогонов ещё не было'));
    });

    await check('флоу: другая заготовка, её вид', () => {
      const sel = $('hub-preset');
      sel.value = 'brief';
      sel.dispatchEvent(new Event('change', { bubbles: true }));
      assert($('hub-species').value === 'рысь', 'вид: ' + $('hub-species').value);
      sel.value = 'passport';
      sel.dispatchEvent(new Event('change', { bubbles: true }));
      assert($('hub-species').value === 'манул', 'вид не вернулся');
    });

    await check('флоу: запуск: POST с заготовкой и видом', async () => {
      click('#hub-run');
      await until('POST', () => hubPosts('flows').length === 1);
      const body = JSON.parse(hubPosts('flows')[0].body);
      assert(body.preset === 'passport' && body.species === 'манул', 'тело: ' + hubPosts('flows')[0].body);
    });

    await check('флоу: вызовы по мере хода, идущий подсвечен', async () => {
      await until('№1 идёт', () => q('.hub-call[data-n="1"][data-state="running"]'));
      assert($('hub-run').disabled, 'кнопка не заблокирована');
      assert(q('#hub-status .thinking'), 'нет «идёт» в строке прогона');
      await until('№3 идёт', () => q('.hub-call[data-n="3"][data-state="running"]'));
      assert(q('.hub-call[data-n="1"][data-state="ok"]') && q('.hub-call[data-n="2"][data-state="ok"]'), '№1–2 не готовы');
      assert(qa('.hub-call').length === 3, 'вызовов ' + qa('.hub-call').length);
      const row = q('.hub-call[data-n="3"]');
      assert(row.dataset.server === 'daemon' && row.dataset.tool === 'mdd_search', '№3: ' + row.dataset.server + ' ' + row.dataset.tool);
      assert(rgb(row.querySelector('.hub-tag')) === 'rgb(44, 90, 133)', 'цвет сервера у вызова');
      assert(getComputedStyle(row.children[0]).borderLeftColor === 'rgb(44, 90, 133)', 'полоса сервера: ' + getComputedStyle(row.children[0]).borderLeftColor);
      assert(getComputedStyle(row.children[1], '::after').animationName === 'pipe-run', 'нет анимации у идущего');
      assert(text('.hub-call[data-n="1"] .hub-args') === 'query: манул', 'аргументы: ' + text('.hub-call[data-n="1"] .hub-args'));
      $('hub-run').click(); // повторное нажатие — мимо
    });

    await check('флоу: итог: 13 вызовов трёх серверов, ошибка facts_get, «← из №k»', async () => {
      await until('итог', () => q('#hub-result'), 20000);
      const rows = qa('.hub-call');
      assert(rows.length === 13, 'вызовов ' + rows.length);
      assert([...new Set(rows.map(r => r.dataset.server))].sort().join(',') === 'daemon,notes,sources', 'серверы');
      const fg = q('.hub-call[data-tool="facts_get"]');
      assert(fg.dataset.state === 'fail' && fg.querySelector('.hub-mark').textContent === '✗' && fg.textContent.includes('выпуска о виде 1006010 нет'), 'facts_get');
      assert(qa('.hub-call[data-state="ok"]').length === 12, 'готовых не 12');
      assert(text('.hub-call[data-n="4"] .hub-from-cell') === '← из №3', '№4: ' + text('.hub-call[data-n="4"] .hub-from-cell'));
      assert(qa('.hub-call[data-n="10"] .hub-from').length === 2, 'у №10 не две стрелки');
      assert(hubPosts('flows').length === 1, 'POST ушёл повторно');
      assert(text('#hub-status').includes('готово') && text('#hub-status').includes('13 вызовов'), 'строка: ' + text('#hub-status'));
      assert(!$('hub-run').disabled, 'кнопка осталась выключенной');
    });

    await check('флоу: проверки ✓ и одна ⚠ с пояснением', () => {
      const cs = qa('.hub-check');
      assert(cs.length === 10, 'проверок ' + cs.length);
      const warn = qa('.hub-check[data-level="warn"]');
      assert(warn.length === 1 && warn[0].textContent.includes('⚠') && warn[0].textContent.includes('facts_get') && warn[0].textContent.includes('допустима'), 'предупреждение');
      assert(qa('.hub-check[data-level="ok"]').every(c => c.querySelector('.hub-check-mark').textContent === '✓'), 'нет ✓');
      assert(q('#hub-result.ok') && text('#hub-result').includes('Флоу прошёл проверки (предупреждений: 1)'), 'итог: ' + text('#hub-result .hub-result-head'));
    });

    await check('флоу: серверы подтвердили, ответ, файл, превью, цена', () => {
      assert(qa('.hub-delta').length === 3, 'дельт ' + qa('.hub-delta').length);
      assert(text('.hub-delta[data-server="notes"] .hub-dt[data-tool="nb_add"]') === 'nb_add 3 / 3 ✓', 'nb_add: ' + text('.hub-delta[data-server="notes"] .hub-dt[data-tool="nb_add"]'));
      assert(text('#hub-file') === 'notes/otocolobus-manul.md', 'файл: ' + text('#hub-file'));
      const pre = $('hub-preview');
      assert(pre && pre.tagName === 'PRE' && pre.textContent.includes('# Паспорт: манул') && pre.textContent.includes('Pallas'), 'превью');
      assert(text('#hub-cost') === '$0.0123' && text('#hub-result').includes('ходов 10'), 'цена и ходы');
      assert(text('#hub-answer').startsWith('Паспорт манула записан в блокнот'), 'ответ');
    });

    await check('флоу: разметка из данных не исполняется', () => {
      assert(window.__xss === undefined, 'исполнился код: __xss=' + window.__xss);
      assert(!q('#hub-root img') && !q('#hub-root script'), 'элемент из данных в окне');
      assert(text('#hub-answer').includes('<script>window.__xss=32</script>'), 'ответ не буквами');
      assert(text('#hub-preview').includes('<script>window.__xss=33</script>'), 'превью не буквами');
      assert(text('.hub-call[data-n="10"] .hub-args').includes('<img src=x'), 'аргументы не буквами');
    });

    await check('флоу: список прогонов', async () => {
      await until('строка', () => q('#hub-runs tr.hub-run-row'));
      const rows = qa('#hub-runs tr.hub-run-row');
      assert(rows.length === 1 && rows[0].textContent.includes('манул') && rows[0].textContent.includes('готово') && rows[0].textContent.includes('13'),
        'строка: ' + (rows[0] && rows[0].textContent));
      assert(rows[0].classList.contains('current'), 'открытый прогон не отмечен');
    });

    await check('флоу: вид переживает смену вкладок; счётчики серверов выросли', async () => {
      $('hub-species').value = 'рысь';
      $('hub-species').dispatchEvent(new Event('input', { bubbles: true }));
      click('[data-hub-tab="servers"]');
      await until('серверы', () => q('.hub-server'));
      const calls = name => q(`.hub-server[data-name="${name}"]`).lastElementChild.textContent.trim();
      assert(calls('sources') === '5' && calls('daemon') === '3' && calls('notes') === '5', `вызовов: ${calls('sources')}/${calls('daemon')}/${calls('notes')}`);
      click('[data-hub-tab="flow"]');
      await until('флоу', () => q('#hub-form'));
      assert($('hub-species').value === 'рысь', 'вид сброшен: ' + $('hub-species').value);
      assert(q('#hub-result'), 'итог прогона пропал');
    });
  };

  scenarios['shot-hub-servers'] = async () => {
    await booted();
    await openHub();
    await hubConnect();
  };
  scenarios['shot-hub'] = async () => {
    await booted();
    await openHub();
    await hubConnect();
    await hubStartFlow();
    await until('итог', () => q('#hub-result'), 20000);
    await sleep(300);
  };

  // ===== Окно «База знаний» (v21) =====

  /* REST (edgeKB в edge_test): настоящий kbapi над временной kb.db из
     настоящего корпуса — обе стратегии, векторы embed.Hash (hash-256),
     сохранённый отчёт сравнения — и документ xss-test с разметкой в
     заголовке, тексте и разделе. Сценарии kb-none — база не собрана (503),
     эмбеддер не отвечает. */
  const kbBlocks = () => qa('#kb-text .kb-chunk');
  async function openKB(what) {
    await until('кнопка на пульте', () => q('#kb-button .kb-dot.ok, #kb-button .kb-dot.bad'), 8000);
    click('#kb-button');
    await until('окно базы знаний', () => windowOpen() && q('#kb-root') && $('window-title').textContent === 'База знаний' && q(what || '#kb-docs'), 8000);
  }
  async function kbPickStrategy(s) {
    const sel = $('kb-strategy');
    sel.value = s;
    sel.dispatchEvent(new Event('change', { bubbles: true }));
    await until('стратегия ' + s, () => q(`#kb-text[data-index="${s}"]`) && kbBlocks().length);
  }
  async function kbAsk(query, mode, k) {
    click('[data-kb-tab="search"]');
    await until('вкладка поиска', () => q('#kb-search-form'));
    $('kb-q').value = query;
    $('kb-q').dispatchEvent(new Event('input', { bubbles: true }));
    if (mode) { $('kb-mode').value = mode; $('kb-mode').dispatchEvent(new Event('change', { bubbles: true })); }
    if (k) { $('kb-k').value = String(k); $('kb-k').dispatchEvent(new Event('change', { bubbles: true })); }
    click('#kb-go');
    await until('выдача', () => q('#kb-results .kb-result') && q('#kb-results').dataset.query === query && !$('kb-go').disabled, 8000);
  }

  scenarios.kb = async () => {
    spyFetch();
    await booted();
    window.__xss = undefined;

    await check('база знаний: окно в списке окон, кнопка на пульте рядом с MCP-серверами', async () => {
      click('#windows-button');
      await until('список окон', () => windowOpen() && q('#window-body .wlist'));
      assert(text('#window-body .wlist [data-arg="kb"]') === 'База знаний', 'нет окна kb в списке');
      actions.closeWindow();
      const b = await until('кнопка', () => q('#kb-button .kb-dot.ok') && $('kb-button'), 8000);
      assert(text('#kb-button') === 'База знаний', 'надпись: ' + text('#kb-button'));
      await until('кнопка MCP', () => q('#hub-button'), 8000);
      await until('рядом с MCP-серверами', () => $('hub-button').nextElementSibling === b);
      assert(b.title.includes('hash-256'), 'подсказка: ' + b.title);
    });

    await check('база знаний: семь вкладок, корпус', async () => {
      await openKB();
      const tabs = qa('[data-kb-tab]').map(x => x.dataset.kbTab);
      assert(tabs.join(',') === 'docs,chunks,search,ask,qa,modes,report', 'вкладки: ' + tabs);
      assert(text('[data-kb-tab="ask"]') === 'Спросить' && text('[data-kb-tab="qa"]') === 'Контрольные вопросы', 'надписи вкладок v22');
      assert(text('[data-kb-tab="modes"]') === 'Режимы', 'надпись вкладки v23');
      assert(q('[data-kb-tab="docs"]').classList.contains('active'), 'открыта не «Корпус»');
      assert(text('[data-kb-tab="chunks"]') === 'Чанки' && text('[data-kb-tab="search"]') === 'Поиск' && text('[data-kb-tab="report"]') === 'Сравнение', 'надписи вкладок');
      const n = Number(text('#kb-n-docs'));
      assert(n >= 31 && qa('.kb-doc').length === n, `документов ${n}, строк ${qa('.kb-doc').length}`);
      assert(Number(text('#kb-n-pages').replace(',', '.')) >= 30, 'страниц: ' + text('#kb-n-pages'));
      assert(/^[0-9a-f]{12}$/.test(text('#kb-sha')) && /^[0-9a-f]{64}$/.test($('kb-sha').title), 'corpus_sha: ' + text('#kb-sha'));
      assert(text('#kb-licenses').includes('CC BY-SA'), 'лицензии: ' + text('#kb-licenses'));
      assert(Number(text('#kb-n-chars').replace(/\D/g, '')) > 100000, 'символов: ' + text('#kb-n-chars'));
      assert(q('#kb-embedder.ok') && text('#kb-embedder').includes('hash-256') && text('#kb-embedder').includes('test'), 'эмбеддер: ' + text('#kb-embedder'));
      assert(qa('#kb-indexes .kb-index').map(x => x.dataset.index).join(',') === 'structure,fixed', 'индексы: ' + text('#kb-indexes'));
    });

    await check('база знаний: строка документа: заголовок, источник, символы, страницы, ревизия', () => {
      const row = q('.kb-doc[data-doc="manul"]');
      assert(row && row.textContent.includes('Манул') && row.textContent.includes('wikipedia-ru'), 'строка: ' + (row && row.textContent));
      const a = row.querySelector('a.kb-ext');
      assert(a && /^https:\/\/ru\.wikipedia\.org\/w\/index\.php\?oldid=\d+$/.test(a.href) && a.target === '_blank', 'ссылка: ' + (a && a.href));
      assert(a.textContent.includes(a.href.split('=')[1]), 'revid в ссылке');
      assert(!q('.kb-doc[data-doc="xss-test"] a'), 'ссылка javascript: стала ссылкой');
    });

    let nStructure = 0;
    await check('база знаний: клик по документу открывает его чанки', async () => {
      click(q('.kb-doc[data-doc="manul"] td:nth-child(3)'));
      await until('чанки манула', () => q('#kb-text[data-doc="manul"][data-index="structure"]') && kbBlocks().length, 8000);
      assert(q('[data-kb-tab="chunks"]').classList.contains('active'), 'вкладка не «Чанки»');
      assert($('kb-doc').value === 'manul' && $('kb-strategy').value === 'structure', 'выбор: ' + $('kb-doc').value + ' / ' + $('kb-strategy').value);
      const bl = kbBlocks();
      nStructure = bl.length;
      assert(nStructure > 5, 'блоков ' + nStructure);
      assert(bl.every((b, i) => b.dataset.id === 'manul/structure/' + String(i).padStart(3, '0') || b.dataset.id.startsWith('manul/structure/')), 'id блоков');
      assert(bl[0].classList.contains('even') && bl[1].classList.contains('odd') && bl[2].classList.contains('even'), 'фон не чередуется');
      assert(getComputedStyle(bl[0]).backgroundColor !== getComputedStyle(bl[1]).backgroundColor, 'фон соседних блоков одинаков');
      const head = bl[1].querySelector('.kb-chunk-head').textContent;
      assert(head.includes(bl[1].dataset.id) && /\d+ ток\./.test(head), 'подпись: ' + head);
      assert(text('#kb-summary .kb-sum-s[data-index="structure"] .kb-sum-n').startsWith(String(nStructure)), 'сводка: ' + text('#kb-summary'));
      assert(text('#kb-text').includes('палласов кот') || text('#kb-text').includes('Манул'), 'нет текста статьи');
      assert(q('#kb-text .kb-hd'), 'заголовки разделов не видны');
    });

    await check('база знаний: переключатель стратегии меняет число блоков', async () => {
      await kbPickStrategy('fixed');
      const n = kbBlocks().length;
      assert(n > 0 && n !== nStructure, `fixed ${n}, structure ${nStructure}`);
      assert(kbBlocks().every(b => b.dataset.id.startsWith('manul/fixed/')), 'id блоков fixed');
      assert(q('#kb-text .kb-overlap'), 'перекрытие fixed не видно');
      assert(q('#kb-text .kb-chunk.mixed .kb-mixed'), 'нет пометки «на стыке разделов»');
      assert(text('#kb-summary .kb-sum-s[data-index="fixed"] .kb-sum-n').startsWith(String(n)), 'сводка fixed');
      assert(text('#kb-summary .kb-sum-s[data-index="structure"] .kb-sum-n').startsWith(String(nStructure)), 'сводка structure пропала');
      click('[data-kb-strategy="structure"]');
      await until('обратно structure', () => q('#kb-text[data-index="structure"]') && kbBlocks().length === nStructure);
      assert($('kb-strategy').value === 'structure', 'выбор не синхронен');
    });

    let hit = null;
    await check('база знаний: поиск по двум индексам рядом', async () => {
      await kbAsk('чем питается харза', 'dense', 5);
      const cols = qa('.kb-result');
      assert(cols.map(c => c.dataset.index).join(',') === 'structure,fixed', 'колонки: ' + cols.map(c => c.dataset.index));
      assert(Math.abs(cols[0].getBoundingClientRect().top - cols[1].getBoundingClientRect().top) < 2 &&
        cols[1].getBoundingClientRect().left > cols[0].getBoundingClientRect().right - 1, 'колонки не рядом');
      for (const c of cols) {
        const hits = [...c.querySelectorAll('.kb-hit')];
        assert(hits.length === 5, c.dataset.index + ': попаданий ' + hits.length);
        assert(c.querySelector('.kb-mode.dense') && c.querySelector('.kb-mode').textContent.includes('hash-256'), 'режим: ' + c.querySelector('.kb-result-head').textContent);
        hits.forEach((h, i) => {
          assert(h.querySelector('.kb-rank').textContent === String(i + 1), 'ранг');
          assert(/^-?\d\.\d{3}$/.test(h.querySelector('.kb-score').textContent), 'балл: ' + h.querySelector('.kb-score').textContent);
          assert(h.querySelector('.kb-hit-where').textContent.includes('›'), 'документ › раздел');
          assert(h.querySelector('.kb-hit-text').textContent.length <= 300, 'текст длиннее 300');
        });
      }
      const call = factsCalls.filter(x => x.url.includes('/api/kb/search')).pop();
      assert(call && call.url.includes('k=5') && call.url.includes('mode=dense') && !call.url.includes('index='), 'запрос: ' + (call && call.url));
    });

    await check('база знаний: BM25 находит харзу', async () => {
      await kbAsk('чем питается харза', 'bm25', 3);
      const cols = qa('.kb-result');
      assert(cols.length === 2 && cols.every(c => c.querySelector('.kb-mode.bm25') && c.querySelectorAll('.kb-hit').length === 3), 'выдача bm25');
      assert(cols.every(c => c.querySelector('.kb-hit[data-doc="yellow-throated-marten"]')), 'харзы нет в выдаче');
      hit = q('.kb-result[data-index="fixed"] .kb-hit[data-doc="yellow-throated-marten"]');
    });

    await check('база знаний: клик по попаданию ведёт к чанку', async () => {
      const id = hit.dataset.chunk;
      click(hit.querySelector('.kb-hit-text'));
      await until('чанк', () => q(`#kb-text[data-doc="yellow-throated-marten"][data-index="fixed"] .kb-chunk.focus[data-id="${id}"]`), 8000);
      assert(q('[data-kb-tab="chunks"]').classList.contains('active') && $('kb-doc').value === 'yellow-throated-marten' && $('kb-strategy').value === 'fixed', 'вкладка и выбор');
      const r = q('.kb-chunk.focus').getBoundingClientRect(), w = $('window-body').getBoundingClientRect();
      assert(r.bottom > w.top && r.top < w.bottom, 'чанк не в виду');
    });

    await check('база знаний: форма поиска переживает смену вкладок', async () => {
      click('[data-kb-tab="search"]');
      await until('поиск', () => q('#kb-search-form'));
      assert($('kb-q').value === 'чем питается харза' && $('kb-mode').value === 'bm25' && $('kb-k').value === '3', 'форма сброшена');
      assert(qa('.kb-result').length === 2, 'выдача пропала');
    });

    await check('база знаний: сравнение стратегий', async () => {
      click('[data-kb-tab="report"]');
      await until('отчёт', () => q('#kb-report-meta'), 8000);
      assert(text('#kb-report-date').match(/\d\d\.\d\d\.\d{4}/), 'дата: ' + text('#kb-report-date'));
      assert(text('#kb-report-meta').includes('hash-256'), 'эмбеддер: ' + text('#kb-report-meta'));
      assert(qa('#kb-stats-table .kb-stat-row').map(r => r.dataset.index).join(',') === 'structure,fixed', 'структурные метрики');
      const rows = qa('#kb-retrieval-table .kb-ret-row');
      assert(rows.length === 8, 'строк поиска ' + rows.length);
      assert(rows.some(r => r.dataset.mode === 'bm25') && rows.some(r => r.dataset.split === 'test'), 'режимы и наборы');
      assert(text('#kb-retrieval-table').includes('recall@1') && text('#kb-retrieval-table').includes('MRR') && text('#kb-retrieval-table').includes('recall при'), 'колонки');
      assert(/\d\.\d\d/.test(rows[0].textContent), 'нет чисел');
      assert(qa('#kb-conclusion li').length >= 3 && text('#kb-conclusion').includes('structure'), 'вывод: ' + text('#kb-conclusion'));
    });

    await check('база знаний: разметка из корпуса выводится буквами', async () => {
      click('[data-kb-tab="docs"]');
      await until('корпус', () => q('.kb-doc[data-doc="xss-test"]'));
      const t = q('.kb-doc[data-doc="xss-test"] .kb-doc-title');
      assert(t.textContent.includes('<b>Ксенофоб</b>') && t.textContent.includes('<img src=x'), 'заголовок: ' + t.textContent);
      click(q('.kb-doc[data-doc="xss-test"] td:nth-child(3)'));
      await until('чанки xss', () => q('#kb-text[data-doc="xss-test"]') && kbBlocks().length);
      assert(text('#kb-text').includes('<script>window.__xss=42</script>') && text('#kb-text').includes('<i>Питание</i>'), 'текст: ' + text('#kb-text'));
      assert(text('#kb-text').includes('&amp;'), 'амперсанд раскрылся');
      await kbAsk('ксенофоб разметкой', 'bm25', 3);
      const h = await until('попадание', () => q('.kb-hit[data-doc="xss-test"]'));
      assert(h.textContent.includes('<script>') || h.textContent.includes('<img'), 'выдача: ' + h.textContent);
      assert(!q('#kb-root img') && !q('#kb-root script') && !q('#kb-root .kb-doc-title b') && !q('#kb-root .kb-hit-where i'), 'элемент из данных в окне');
      await sleep(100);
      assert(window.__xss === undefined, 'исполнился код: __xss=' + window.__xss);
    });
  };

  scenarios['kb-none'] = async () => {
    spyFetch();
    await booted();

    await check('база знаний: без базы красная точка на пульте', async () => {
      const b = await until('кнопка', () => q('#kb-button .kb-dot.bad') && $('kb-button'), 8000);
      assert(b.title.includes('базы знаний нет'), 'подсказка: ' + b.title);
    });

    await check('база знаний: без базы понятная подсказка с командами', async () => {
      await openKB('#kb-off');
      assert(text('#kb-off').includes('Базы знаний нет'), 'заголовок: ' + text('#kb-off'));
      assert(text('#kb-off .kb-why').includes('базы знаний нет:'), 'причина: ' + text('#kb-off .kb-why'));
      assert(text('#kb-off').includes('go run ./cmd/kb index -strategy all') && text('#kb-off').includes('go run ./cmd/kb eval') && text('#kb-off').includes('uv run embedder/server.py'), 'команды: ' + text('#kb-off'));
      assert(text('#kb-off .kb-off-emb').includes('не отвечает'), 'эмбеддер: ' + text('#kb-off .kb-off-emb'));
      assert(!q('.kb-doc') && !q('#kb-docs'), 'таблица без базы');
      assert(factsCalls.filter(x => x.url.includes('/api/kb/docs')).length === 0, 'окно спросило docs без базы');
    });

    await check('база знаний: без базы вкладки ведут к той же подсказке', async () => {
      for (const tab of ['search', 'chunks', 'ask', 'qa', 'report']) {
        click(`[data-kb-tab="${tab}"]`);
        await until('вкладка ' + tab, () => q(`[data-kb-tab="${tab}"]`).classList.contains('active') && q('#kb-off'));
      }
      assert(!q('#kb-search-form') && !q('#kb-report-meta') && !q('#kb-ask-form') && !q('#kb-qa-form'), 'вкладки без базы');
      assert(factsCalls.filter(x => x.url.includes('/api/kb/ask') || x.url.includes('/api/kb/evals')).length === 0, 'окно спросило модель без базы');
    });

    await check('база знаний: без базы ask отвечает 503 с причиной и подсказкой', async () => {
      const r = await factsAPI('POST', '/api/kb/ask', { q: 'манул' });
      assert(r.code === 503 && r.data.why.includes('базы знаний нет') && r.data.hint.includes('go run ./cmd/kb index'), 'ответ: ' + r.code + ' ' + JSON.stringify(r.data));
    });
  };

  // ===== «Спросить» и «Контрольные вопросы» (v22) =====

  /* REST — тот же kbapi, но отвечающий агент подставной (edgeRAG в
     edge_rag_test): заготовки по вопросам набора test, фрагменты — настоящий
     поиск BM25, ссылки [chunk_id] — настоящие чанки. Прогон отдаёт вопрос
     за опросом. */
  async function kbTabOpen(tab, what) {
    click(`[data-kb-tab="${tab}"]`);
    await until('вкладка ' + tab, () => q(`[data-kb-tab="${tab}"]`).classList.contains('active') && q(what), 8000);
  }
  function kbPick(id) {
    const sel = $('kb-ask-pick');
    sel.value = id;
    sel.dispatchEvent(new Event('change', { bubbles: true }));
  }
  // kbSetModes — флажки режимов (v23) кликами: сначала снять лишние (пока
  // остаётся хоть один), потом поставить нужные, потом снять остаток.
  // Переключатель перерисовывается после каждого флажка — элемент ищется
  // заново.
  function kbSetModes(box, modes) {
    const box$ = () => $(box);
    const boxes = () => [...box$().querySelectorAll('[data-kb-mode]')];
    const flip = m => click(box$().querySelector(`[data-kb-mode="${m}"]`));
    for (const x of boxes()) if (x.checked && !modes.includes(x.dataset.kbMode) && boxes().filter(y => y.checked).length > 1) flip(x.dataset.kbMode);
    for (const m of modes) if (!box$().querySelector(`[data-kb-mode="${m}"]`).checked) flip(m);
    for (const x of boxes()) if (x.checked && !modes.includes(x.dataset.kbMode)) flip(x.dataset.kbMode);
    const got = boxes().filter(x => x.checked).map(x => x.dataset.kbMode);
    assert(got.join(',') === modes.join(','), 'режимы ' + box + ': ' + got);
  }
  const kbAnswer = m => q(`#kb-answers .kb-answer[data-mode="${m}"]`);
  const kbAskCalls = () => factsCalls.filter(x => x.url.includes('/api/kb/ask'));
  async function kbAskGo(qtext) {
    const n = kbAskCalls().length;
    click('#kb-ask-go');
    assert(q('#kb-ask-out .kb-thinking') && $('kb-ask-go').disabled, 'нет индикатора «думает»');
    await until('ответы', () => q('#kb-answers') && q('#kb-ask-out').dataset.q === qtext && !$('kb-ask-go').disabled, 8000);
    assert(kbAskCalls().length === n + 1, 'запросов ask: ' + (kbAskCalls().length - n));
    return JSON.parse(kbAskCalls().pop().body);
  }

  scenarios['kb-ask'] = async () => {
    spyFetch();
    await booted();
    window.__xss = undefined;
    let t03 = '';

    await check('спросить: вкладка и набор вопросов в выборе', async () => {
      await openKB();
      await kbTabOpen('ask', '#kb-ask-pick optgroup');
      const groups = qa('#kb-ask-pick optgroup').map(g => g.label);
      assert(groups.length === 3 && groups[0].startsWith('test') && groups[2].startsWith('out'), 'группы: ' + groups);
      assert(qa('#kb-ask-pick optgroup')[0].querySelectorAll('option').length === 10, 'test не 10');
      assert(q('#kb-ask-pick option[value="T03"]').textContent.startsWith('T03 · '), 'подпись T03');
      assert(text('#kb-ask-out').includes('T03'), 'подсказка: ' + text('#kb-ask-out'));
      assert(!q('.kb-answer'), 'ответы до вопроса');
    });

    await check('спросить: T03 из набора, ответы двумя колонками рядом, вердикты правила', async () => {
      kbPick('T03');
      t03 = $('kb-ask-q').value;
      assert(t03.includes('MDD v2.5'), 'текст не подставился: ' + t03);
      const body = await kbAskGo(t03);
      assert(body.q === t03 && body.question_id === 'T03', 'тело: ' + JSON.stringify(body));
      const cols = qa('#kb-answers .kb-answer').map(x => x.dataset.mode);
      assert(cols.join(',') === 'norag,rag', 'колонки: ' + cols);
      const a = kbAnswer('norag').getBoundingClientRect(), b = kbAnswer('rag').getBoundingClientRect();
      assert(Math.abs(a.top - b.top) < 2 && b.left > a.right - 1, 'колонки не рядом');
      assert(kbAnswer('norag').querySelector('.kb-verdict[data-mode="norag"]').dataset.verdict === 'wrong', 'вердикт norag: ' + text('.kb-answer[data-mode="norag"] .kb-verdict'));
      assert(kbAnswer('rag').querySelector('.kb-verdict[data-mode="rag"]').dataset.verdict === 'correct', 'вердикт rag');
      assert(text('.kb-answer[data-mode="rag"] .kb-verdict').includes('✓') && text('.kb-answer[data-mode="norag"] .kb-verdict').includes('✗'), 'значки');
      assert(text('.kb-answer[data-mode="norag"] .kb-answer-text').includes('единственный вид'), 'ответ norag');
      assert(text('.kb-answer[data-mode="rag"] .kb-answer-text').includes('Ailurus styani'), 'ответ rag');
      assert(text('#kb-ask-expect').includes('T03') && text('#kb-ask-expect').includes('Ailurus'), 'ожидание: ' + text('#kb-ask-expect'));
      assert(text('.kb-answer[data-mode="rag"] .kb-rule').includes('нашлось'), 'правило: ' + text('.kb-answer[data-mode="rag"] .kb-rule'));
      for (const m of ['norag', 'rag']) {
        const meta = kbAnswer(m).querySelector('.kb-answer-meta').textContent;
        assert(/\$0\.\d{4}/.test(meta) && meta.includes('ток.') && /\d с|мс/.test(meta), 'цена/токены/время ' + m + ': ' + meta);
      }
      assert(!kbAnswer('norag').querySelector('.kb-src'), 'фрагменты у norag');
    });

    await check('спросить: у rag найденные фрагменты с рангом, баллом, разделом и режимом поиска', async () => {
      const src = qa('.kb-answer[data-mode="rag"] .kb-src');
      assert(src.length === 5, 'фрагментов ' + src.length);
      src.forEach((x, i) => {
        assert(x.querySelector('.kb-rank').textContent === String(i + 1), 'ранг');
        assert(/^\d\.\d{3}$/.test(x.querySelector('.kb-score').textContent), 'балл: ' + x.querySelector('.kb-score').textContent);
        assert(x.querySelector('.kb-hit-where').textContent.includes('›'), 'статья › раздел');
        assert(/^[a-z0-9-]+\/structure\/\d{3}$/.test(x.dataset.chunk), 'chunk_id: ' + x.dataset.chunk);
      });
      assert(q('.kb-answer[data-mode="rag"] .kb-srcs-head .kb-mode.bm25'), 'режим поиска: ' + text('.kb-answer[data-mode="rag"] .kb-srcs-head'));
      assert(q('.kb-answer[data-mode="rag"] .kb-src.cited .kb-cited'), 'фрагмент из ответа не помечен');
      assert(q('.kb-answer[data-mode="rag"] .kb-recall'), 'нет пометки recall');
    });

    await check('спросить: «что ушло модели», режимы отличаются только контекстом', async () => {
      const d = $('kb-ask-prompt');
      assert(d && !d.open, 'блок раскрыт сразу');
      d.open = true;
      assert(text('#kb-ask-prompt summary').includes('одинаковый'), 'заголовок: ' + text('#kb-ask-prompt summary'));
      const col = m => q(`#kb-ask-prompt .kb-prompt-col[data-mode="${m}"]`);
      const pre = m => [...col(m).querySelectorAll('.kb-pre')].map(x => x.textContent);
      assert(pre('norag')[0] === pre('rag')[0] && pre('norag')[0].includes('справочник'), 'System разный');
      assert(pre('norag')[1] === t03 && pre('rag')[1].startsWith(t03) && pre('rag')[1].includes('Фрагменты базы знаний'), 'User');
      d.open = false;
    });

    await check('спросить: клик по [chunk_id] в ответе ведёт к чанку', async () => {
      const cite = q('.kb-answer[data-mode="rag"] .kb-answer-text .kb-cite');
      assert(cite && !cite.classList.contains('unknown'), 'нет ссылки на фрагмент');
      const id = cite.dataset.chunk, doc = id.split('/')[0];
      assert(cite.textContent === '[' + id + ']', 'надпись ссылки: ' + cite.textContent);
      click(cite);
      await until('чанк', () => q(`#kb-text[data-doc="${doc}"][data-index="structure"] .kb-chunk.focus[data-id="${id}"]`), 8000);
      assert(q('[data-kb-tab="chunks"]').classList.contains('active') && $('kb-doc').value === doc, 'вкладка и документ');
      await kbTabOpen('ask', '#kb-answers');
      assert($('kb-ask-q').value === t03 && $('kb-ask-pick').value === 'T03', 'форма сброшена');
    });

    await check('спросить: вопрос-продолжение показывает контекст, правка текста снимает выбор', async () => {
      kbPick('T08');
      assert(text('#kb-ask-context').includes('Где в России водится харза?'), 'контекст: ' + text('#kb-ask-context'));
      $('kb-ask-q').value = 'А сколько весит манул?';
      $('kb-ask-q').dispatchEvent(new Event('input', { bubbles: true }));
      assert($('kb-ask-pick').value === '' && $('kb-ask-context').hidden, 'выбор не снят');
    });

    await check('спросить: разметка в ответе модели выводится буквами, переносы строк сохранены', async () => {
      $('kb-ask-q').value = 'ответь разметкой про ксенофоба';
      $('kb-ask-q').dispatchEvent(new Event('input', { bubbles: true }));
      const body = await kbAskGo('ответь разметкой про ксенофоба');
      assert(!body.question_id, 'свой вопрос ушёл с question_id');
      const t = kbAnswer('norag').querySelector('.kb-answer-text');
      assert(t.textContent.includes('<b>жирной</b>') && t.textContent.includes('<script>window.__xss=44</script>') && t.textContent.includes('<img src=x'), 'текст: ' + t.textContent);
      assert(t.textContent.includes('\n') && getComputedStyle(t).whiteSpace === 'pre-wrap', 'перенос строки');
      assert(text('.kb-answer[data-mode="rag"] .kb-answer-text').includes('<i>курсив</i>'), 'rag: ' + text('.kb-answer[data-mode="rag"] .kb-answer-text'));
      assert(q('.kb-src[data-doc="xss-test"]'), 'нет фрагмента xss-test');
      assert(!q('.kb-answer-text b, .kb-answer-text i, .kb-answer-text script, .kb-answer-text img, .kb-src img, .kb-src script, .kb-pre *'), 'элемент из ответа модели');
      assert(!q('#kb-answers .kb-verdict'), 'вердикт у своего вопроса');
      await sleep(100);
      assert(window.__xss === undefined, 'исполнился код: __xss=' + window.__xss);
    });

    await check('спросить: пустой вопрос не уходит', async () => {
      const n = kbAskCalls().length;
      $('kb-ask-q').value = '   ';
      $('kb-ask-q').dispatchEvent(new Event('input', { bubbles: true }));
      click('#kb-ask-go');
      await sleep(100);
      assert(kbAskCalls().length === n, 'ушёл пустой вопрос');
    });

    await check('спросить v23: режимы: флажки, по умолчанию norag и rag, рядом не больше трёх', async () => {
      const boxes = qa('#kb-ask-modes [data-kb-mode]');
      assert(boxes.map(x => x.dataset.kbMode).join(',') === 'norag,rag,rag+filter,rag+rewrite,rag+both', 'режимы: ' + boxes.map(x => x.dataset.kbMode));
      assert(boxes.filter(x => x.checked).map(x => x.dataset.kbMode).join(',') === 'norag,rag', 'по умолчанию');
      kbSetModes('kb-ask-modes', ['norag', 'rag', 'rag+both']);
      const off = qa('#kb-ask-modes [data-kb-mode]').filter(x => x.disabled).map(x => x.dataset.kbMode);
      assert(off.join(',') === 'rag+filter,rag+rewrite', 'четвёртый режим не выключен: ' + off);
      // Последний флажок не снимается.
      kbSetModes('kb-ask-modes', ['rag']);
      click(q('#kb-ask-modes [data-kb-mode="rag"]'));
      assert(q('#kb-ask-modes [data-kb-mode="rag"]').checked, 'сняли последний режим');
    });

    await check('спросить v23: T07 в трёх режимах: три колонки рядом, у rag+both переписанный запрос', async () => {
      kbSetModes('kb-ask-modes', ['norag', 'rag', 'rag+both']);
      kbPick('T07');
      const t07 = $('kb-ask-q').value;
      assert(t07.includes('кошачий медведь'), 'текст: ' + t07);
      const body = await kbAskGo(t07);
      assert(body.modes.join(',') === 'norag,rag,rag+both' && body.question_id === 'T07', 'тело: ' + JSON.stringify(body));
      const cols = qa('#kb-answers .kb-answer');
      assert(cols.map(x => x.dataset.mode).join(',') === 'norag,rag,rag+both', 'колонки: ' + cols.map(x => x.dataset.mode));
      const r = cols.map(x => x.getBoundingClientRect());
      assert(Math.abs(r[0].top - r[2].top) < 2 && r[1].left > r[0].right - 1 && r[2].left > r[1].right - 1 && r[2].right <= $('window-body').getBoundingClientRect().right + 1, 'колонки не рядом');
      const both = kbAnswer('rag+both');
      assert(text('.kb-answer[data-mode="rag+both"] .kb-answer-head').includes('RAG + rewrite + фильтр'), 'заголовок: ' + text('.kb-answer[data-mode="rag+both"] .kb-answer-head'));
      const tb = both.querySelector('.kb-tb');
      assert(tb && tb.dataset.changed === '1' && tb.dataset.empty === '0', 'сводка пути поиска: ' + (tb && tb.textContent));
      assert(tb.querySelector('.kb-tb-q').textContent.includes('малая панда') && tb.querySelector('.kb-tr-add'), 'переписанный: ' + tb.textContent);
      assert([...tb.querySelectorAll('.kb-expanded')].some(x => x.textContent === 'кошачий медведь → малая панда'), 'синонимы: ' + tb.textContent);
      assert(/осталось \d из 20 кандидатов/.test(tb.textContent) && tb.textContent.includes('порог 0.80'), 'осталось: ' + tb.textContent);
      assert(!kbAnswer('rag').querySelector('.kb-tb') && !kbAnswer('norag').querySelector('.kb-srcs'), 'сводка не у того режима');
      assert(both.querySelector('.kb-verdict[data-mode="rag+both"]').dataset.verdict === 'correct', 'вердикт rag+both');
      assert(both.querySelectorAll('.kb-src').length >= 1 && both.querySelector('.kb-src[data-doc="red-panda"]'), 'фрагменты малой панды');
      assert(q('#kb-ask-prompt .kb-prompt-col[data-mode="rag+both"]'), 'промпт rag+both');
    });

    await check('спросить v23: вопрос вне базы: у rag+filter пусто после фильтра', async () => {
      kbSetModes('kb-ask-modes', ['rag', 'rag+filter']);
      kbPick('T10');
      await kbAskGo($('kb-ask-q').value);
      const tb = kbAnswer('rag+filter').querySelector('.kb-tb');
      assert(tb && tb.dataset.empty === '1' && tb.querySelector('.kb-tb-empty').textContent.includes('в базе ответа, вероятно, нет'), 'пусто: ' + (tb && tb.textContent));
      assert(!kbAnswer('rag+filter').querySelector('.kb-src') && text('.kb-answer[data-mode="rag+filter"] .kb-srcs').includes('Фильтр отсёк всех кандидатов'), 'фрагменты: ' + text('.kb-answer[data-mode="rag+filter"] .kb-srcs'));
      assert(kbAnswer('rag+filter').querySelector('.kb-verdict').dataset.verdict === 'abstain', 'вердикт: ' + kbAnswer('rag+filter').querySelector('.kb-verdict').dataset.verdict);
      const r = await factsAPI('POST', '/api/kb/ask', { q: 'манул', modes: ['norag', 'rag', 'rag+filter', 'rag+both'] });
      assert(r.code === 400 && r.error.includes('трёх'), 'четыре режима: ' + r.code + ' ' + r.error);
      await sleep(100);
      assert(window.__xss === undefined, 'исполнился код');
    });
  };

  const kbRow = id => q(`#kb-qa-table .kb-qa-row[data-id="${id}"]`);
  const kbRowV = (id, m) => kbRow(id).querySelector(`.kb-verdict[data-mode="${m}"]`);
  async function kbQaStart(repeats, judge, modes) {
    app.kb.pollMs = 150;
    kbSetModes('kb-qa-modes', modes || ['norag', 'rag']);
    $('kb-qa-repeats').value = String(repeats);
    $('kb-qa-repeats').dispatchEvent(new Event('change', { bubbles: true }));
    $('kb-qa-judge').checked = judge;
    $('kb-qa-judge').dispatchEvent(new Event('change', { bubbles: true }));
    click('#kb-qa-run');
    await until('прогон начат', () => q('#kb-qa-status[data-state="running"]'), 8000);
  }

  scenarios['kb-qa'] = async () => {
    spyFetch();
    await booted();
    window.__xss = undefined;

    await check('контрольные вопросы: таблица набора test', async () => {
      await openKB();
      await kbTabOpen('qa', '#kb-qa-table .kb-qa-row');
      const ids = qa('#kb-qa-table .kb-qa-row').map(r => r.dataset.id);
      assert(ids.join(',') === 'T01,T02,T03,T04,T11,T06,T07,T08,T09,T10', 'строки: ' + ids);
      assert(kbRow('T03').textContent.includes('MDD v2.5') && kbRow('T03').textContent.includes('conflict') && kbRow('T03').textContent.includes('mdd-carnivora'), 'строка T03: ' + kbRow('T03').textContent);
      assert(kbRow('T08').textContent.includes('после: «Где в России водится харза?»'), 'контекст T08');
      assert(kbRow('T10').querySelector('.kb-qa-exp').textContent.includes('в базе нет'), 'ожидание T10');
      assert(!q('#kb-qa-table .kb-verdict'), 'вердикты до прогона');
      assert($('kb-qa-repeats').value === '1' && $('kb-qa-judge').checked && !$('kb-qa-run').disabled, 'настройки');
      // v23: по умолчанию — rag и rag+both, столбцы таблицы — по ним.
      const on = qa('#kb-qa-modes [data-kb-mode]').filter(x => x.checked).map(x => x.dataset.kbMode);
      assert(on.join(',') === 'rag,rag+both', 'режимы по умолчанию: ' + on);
      assert(qa('#kb-qa-table th.kb-qa-v[data-mode]').map(x => x.dataset.mode).join(',') === 'rag,rag+both', 'столбцы: ' + text('#kb-qa-table tr'));
      assert(text('#kb-qa-cost').includes('2 режима'), 'цена: ' + text('#kb-qa-cost'));
    });

    await check('контрольные вопросы: прогон заполняет строки по ходу', async () => {
      await kbQaStart(1, true);
      assert($('kb-qa-run').disabled, 'кнопка не выключена');
      const body = JSON.parse(factsCalls.filter(x => x.url.endsWith('/api/kb/evals') && x.method === 'POST').pop().body);
      assert(body.repeats === 1 && body.judge === true, 'тело: ' + JSON.stringify(body));
      await until('часть строк готова', () => {
        const done = qa('#kb-qa-table .kb-qa-row .kb-verdict[data-mode="rag"]:not(.pending)').length;
        return done >= 1 && done <= 8 && qa('#kb-qa-table .kb-qa-row .kb-verdict.pending').length >= 2;
      }, 15000);
      const partial = text('#kb-qa-count');
      assert(/^\d+ из 20 ответов$/.test(partial) && !partial.startsWith('20 '), 'счётчик: ' + partial);
      assert(kbRowV('T01', 'rag').dataset.verdict === 'correct' && kbRowV('T10', 'rag').classList.contains('pending'), 'первая строка раньше последней');
    });

    await check('контрольные вопросы: второй прогон, пока идёт первый, получает 409 с id идущего', async () => {
      const id = $('kb-qa-status').dataset.run;
      const r = await factsAPI('POST', '/api/kb/evals', {});
      assert(r.code === 409 && r.data.id === id && r.error.includes('уже идёт'), 'ответ: ' + r.code + ' ' + JSON.stringify(r.data));
      actions.kbQaRun();
      await until('ошибка 409 в окне', () => q('#kb-qa-error[data-code="409"]'), 8000);
      assert(text('#kb-qa-error').includes('открыть ' + id), 'кнопка открыть: ' + text('#kb-qa-error'));
      assert($('kb-qa-status').dataset.run === id, 'открыт другой прогон');
    });

    await check('контрольные вопросы: итог, сводка режимов рядом и вывод', async () => {
      await until('прогон готов', () => q('#kb-qa-status[data-state="done"]') && q('#kb-qa-summary'), 20000);
      assert(text('#kb-qa-count') === '20 из 20 ответов', 'счётчик: ' + text('#kb-qa-count'));
      assert(!q('#kb-qa-table .kb-verdict.pending') && !$('kb-qa-run').disabled, 'остались ожидающие');
      assert(kbRowV('T03', 'norag').dataset.verdict === 'wrong' && kbRowV('T03', 'rag').dataset.verdict === 'correct', 'T03');
      assert(kbRowV('T10', 'rag').dataset.verdict === 'abstain' && kbRowV('T10', 'rag').textContent === 'не знаю', 'T10 rag: ' + kbRowV('T10', 'rag').textContent);
      assert(kbRowV('T01', 'rag').textContent === '✓' && kbRowV('T01', 'norag').textContent === '✗', 'значки');
      assert(kbRow('T01').querySelector('.kb-found[data-found]') && !kbRow('T10').querySelector('.kb-found'), 'источник найден');
      const card = m => q(`.kb-qa-card[data-mode="${m}"] b`).textContent;
      assert(card('rag').startsWith('8') && card('norag').startsWith('1'), 'карточки: ' + card('norag') + ' / ' + card('rag'));
      const cell = (k, m) => q(`#kb-qa-stats tr[data-metric="${k}"] td[data-mode="${m}"]`);
      assert(cell('correct', 'rag').textContent === '8' && cell('correct', 'rag').classList.contains('best'), 'верно rag');
      assert(cell('confident_wrong', 'rag').classList.contains('best'), 'уверенные ошибки');
      for (const k of ['partial', 'wrong', 'abstain', 'recall', 'agreement', 'cost']) assert(cell(k, 'rag') && cell(k, 'norag'), 'нет строки ' + k);
      assert(cell('recall', 'norag').textContent === '—', 'recall у norag');
      assert(cell('agreement', 'norag').textContent === '90.0 %', 'согласие: ' + cell('agreement', 'norag').textContent);
      assert(!q('#kb-qa-stats tr[data-metric="flip_rate"]'), 'флипы при одном повторе');
      assert(qa('#kb-qa-conclusion li').length === 5 && text('#kb-qa-conclusion').includes('С базой верно 8 из 10'), 'вывод: ' + text('#kb-qa-conclusion'));
      assert(text('#kb-qa-summary').includes('Правило и судья разошлись: 1'), 'расхождения');
    });

    await check('контрольные вопросы: клик по строке раскрывает ответы и причины судьи', async () => {
      click(kbRow('T03').querySelector('.kb-qa-q'));
      await until('раскрыто', () => q('.kb-qa-detail[data-id="T03"]'));
      const d = q('.kb-qa-detail[data-id="T03"]');
      assert(d.previousElementSibling === kbRow('T03'), 'не под строкой');
      assert([...d.querySelectorAll('.kb-answer')].map(x => x.dataset.mode).join(',') === 'norag,rag', 'режимы');
      assert(d.querySelector('.kb-answer[data-mode="norag"] .kb-judge').textContent.includes('противоречит'), 'причина судьи: ' + d.textContent);
      assert(d.querySelector('.kb-answer[data-mode="rag"] .kb-cite'), 'ссылка на фрагмент');
      assert(d.querySelector('.kb-expect').textContent.includes('Ailurus'), 'ожидание');
      click(kbRow('T03').querySelector('.kb-qa-q'));
      await until('свёрнуто', () => !q('.kb-qa-detail[data-id="T03"]'));
    });

    await check('контрольные вопросы: список прогонов, открытый выделен', async () => {
      const id = $('kb-qa-status').dataset.run;
      const runs = qa('#kb-qa-runs .kb-qa-run');
      assert(runs.length >= 1 && runs[0].dataset.run === id && runs[0].classList.contains('on'), 'прогоны: ' + text('#kb-qa-runs'));
      assert(runs[0].textContent.includes('С базой (RAG) 8/10'), 'счёт: ' + runs[0].textContent);
      const list = await factsAPI('GET', '/api/kb/evals');
      assert(list.ok && list.data.every(x => !x.rows), 'список со строками');
    });

    await check('контрольные вопросы: окно открывается на последнем прогоне', async () => {
      actions.closeWindow();
      app.kb.eval = null;
      await openKB('#kb-qa-summary');
      assert(q('[data-kb-tab="qa"]').classList.contains('active') && qa('#kb-qa-table .kb-qa-row .kb-verdict:not(.pending)').length === 20, 'прогон не открылся');
      await sleep(100);
      assert(window.__xss === undefined, 'исполнился код');
    });

    await check('контрольные вопросы: «Остановить» отменяет идущий прогон', async () => {
      assert($('kb-qa-stop').hidden, 'кнопка видна без прогона');
      await kbQaStart(1, false);
      assert(!$('kb-qa-stop').hidden, 'нет кнопки «Остановить»');
      const id = $('kb-qa-status').dataset.run;
      click('#kb-qa-stop');
      await until('остановлен', () => q(`#kb-qa-status[data-run="${id}"][data-state="cancelled"]`), 8000);
      const del = factsCalls.filter(x => x.method === 'DELETE' && x.url.endsWith('/api/kb/evals/' + id));
      assert(del.length === 1, 'DELETE: ' + del.length);
      assert($('kb-qa-stop').hidden && !$('kb-qa-run').disabled, 'кнопки после остановки');
      assert(text('#kb-qa-status').includes('остановлен') && text('#kb-qa-status').includes('остановлен по запросу'), 'статус: ' + text('#kb-qa-status'));
      assert(qa('#kb-qa-table .kb-qa-row .kb-verdict[data-mode="rag"]:not(.pending)').length < 10 && !q('#kb-qa-summary'), 'прогон дошёл до конца');
      await until('в списке', () => q(`#kb-qa-runs .kb-qa-run[data-run="${id}"][data-state="cancelled"]`), 8000);
    });

    await check('контрольные вопросы v23: прогон rag и rag+both: столбцы по режимам, сводка и путь поиска', async () => {
      await kbQaStart(1, false, ['rag', 'rag+both']);
      const body = JSON.parse(factsCalls.filter(x => x.url.endsWith('/api/kb/evals') && x.method === 'POST').pop().body);
      assert(body.modes.join(',') === 'rag,rag+both', 'тело: ' + JSON.stringify(body));
      assert(qa('#kb-qa-table th.kb-qa-v[data-mode]').map(x => x.dataset.mode).join(',') === 'rag,rag+both', 'столбцы');
      await until('прогон готов', () => q('#kb-qa-status[data-state="done"]') && q('#kb-qa-summary'), 20000);
      assert(text('#kb-qa-count') === '20 из 20 ответов', 'счётчик: ' + text('#kb-qa-count'));
      assert(!kbRow('T03').querySelector('.kb-verdict[data-mode="norag"]'), 'столбец norag');
      assert(kbRowV('T07', 'rag+both').dataset.verdict === 'correct' && kbRowV('T10', 'rag+both').dataset.verdict === 'abstain', 'вердикты rag+both');
      assert(qa('.kb-qa-card').map(x => x.dataset.mode).join(',') === 'rag,rag+both', 'карточки');
      assert(q('#kb-qa-stats th[data-mode="rag+both"]') && text('#kb-qa-stats th[data-mode="rag+both"]') === 'RAG + rewrite + фильтр', 'сводка: ' + text('#kb-qa-stats tr'));
      assert(text('#kb-qa-conclusion').includes('rag+both верно'), 'вывод: ' + text('#kb-qa-conclusion'));
      assert(kbRow('T07').querySelector('.kb-found').title.includes('rag+both'), 'источник по режимам: ' + kbRow('T07').querySelector('.kb-found').title);
      click(kbRow('T07').querySelector('.kb-qa-q'));
      await until('раскрыто', () => q('.kb-qa-detail[data-id="T07"]'));
      const d = q('.kb-qa-detail[data-id="T07"]');
      assert([...d.querySelectorAll('.kb-answer')].map(x => x.dataset.mode).join(',') === 'rag,rag+both', 'режимы раскрытой строки');
      const tb = d.querySelector('.kb-answer[data-mode="rag+both"] .kb-tb');
      assert(tb && tb.textContent.includes('малая панда') && !d.querySelector('.kb-answer[data-mode="rag"] .kb-tb'), 'путь поиска: ' + (tb && tb.textContent));
      click(kbRow('T10').querySelector('.kb-qa-q'));
      await until('раскрыто T10', () => q('.kb-qa-detail[data-id="T10"]'));
      assert(q('.kb-qa-detail[data-id="T10"] .kb-answer[data-mode="rag+both"] .kb-tb[data-empty="1"]'), 'пусто у T10');
      assert(q('#kb-qa-runs .kb-qa-run.on').textContent.includes('rag, rag+both'), 'режимы в списке прогонов: ' + text('#kb-qa-runs .kb-qa-run.on'));
    });
  };

  scenarios['shot-kb-docs'] = async () => { await booted(); await openKB(); await sleep(200); };
  scenarios['shot-kb-structure'] = async () => {
    await booted();
    await openKB();
    click(q('.kb-doc[data-doc="yellow-throated-marten"] td:nth-child(3)'));
    await until('чанки', () => q('#kb-text[data-index="structure"]') && kbBlocks().length, 8000);
    $('window-body').scrollTop = 0;
    await sleep(200);
  };
  scenarios['shot-kb-fixed'] = async () => {
    await scenarios['shot-kb-structure']();
    await kbPickStrategy('fixed');
    $('window-body').scrollTop = 0;
    await sleep(200);
  };
  scenarios['shot-kb-search'] = async () => {
    await booted();
    await openKB();
    await kbAsk('чем питается харза', 'dense', 5);
    await sleep(200);
  };
  scenarios['shot-kb-report'] = async () => {
    await booted();
    await openKB();
    click('[data-kb-tab="report"]');
    await until('отчёт', () => q('#kb-report-meta'), 8000);
    await sleep(200);
  };
  scenarios['shot-kb-none'] = async () => { await booted(); await openKB('#kb-off'); await sleep(200); };
  scenarios['shot-kb-ask'] = async () => {
    await booted();
    await openKB();
    await kbTabOpen('ask', '#kb-ask-pick optgroup');
    kbPick('T03');
    click('#kb-ask-go');
    await until('ответы', () => q('#kb-answers') && !$('kb-ask-go').disabled, 8000);
    $('window-body').scrollTop = 0;
    await sleep(200);
  };
  scenarios['shot-kb-qa'] = async () => {
    await booted();
    await openKB();
    await kbTabOpen('qa', '#kb-qa-table .kb-qa-row');
    await kbQaStart(1, true);
    await until('итог', () => q('#kb-qa-summary'), 20000);
    click(kbRow('T03').querySelector('.kb-qa-q'));
    await until('раскрыто', () => q('.kb-qa-detail[data-id="T03"]'));
    $('window-body').scrollTop = 0;
    await sleep(200);
  };
  scenarios['shot-kb-qa-summary'] = async () => {
    await booted();
    await openKB();
    await kbTabOpen('qa', '#kb-qa-table .kb-qa-row');
    if (!q('#kb-qa-summary')) {
      await kbQaStart(1, true);
      await until('итог', () => q('#kb-qa-summary'), 20000);
    }
    $('kb-qa-summary').scrollIntoView({ block: 'start' });
    await sleep(200);
  };

  // ===== v23: второй этап поиска и вкладка «Режимы» =====

  /* REST — тот же kbapi, конвейер подставной (edgeRetrieve в
     edge_retrieve_test): кандидаты — настоящий BM25 по временной kb.db,
     косинусы — правдоподобные, «кошачий медведь» переписывается в «малая
     панда», динозавры и фосса — пустой итог. Матрица и калибровка — файлы,
     как их пишут kb matrix и kb calibrate; kb-nofiles — файлов нет. */
  const kbT07 = 'Сколько часов в день кошачий медведь тратит на еду?';
  function kbSel(id, v) {
    const el = $(id);
    el.value = String(v);
    el.dispatchEvent(new Event('change', { bubbles: true }));
  }
  function kbFlag(id, on) {
    const el = $(id);
    if (el.checked !== on) click(el);
  }
  // kbPipe — второй этап: rewrite, реранкинг, фильтр, K0, k.
  function kbPipe(rewrite, rerank, filter, k0, k) {
    kbSel('kb-rewrite', rewrite);
    kbSel('kb-rerank', rerank);
    kbFlag('kb-filter', filter);
    if (k0) kbSel('kb-k0', k0);
    if (k) kbSel('kb-k', k);
  }
  async function kbFind(query) {
    $('kb-q').value = query;
    $('kb-q').dispatchEvent(new Event('input', { bubbles: true }));
    click('#kb-go');
    await until('выдача', () => q('#kb-results') && (q('#kb-results').dataset.query === query || q('#kb-search-error')) && !$('kb-go').disabled, 8000);
  }
  const kbCands = () => qa('#kb-cands .kb-cand');
  const kbSearchCalls = () => factsCalls.filter(x => x.url.includes('/api/kb/search'));

  scenarios['kb-trace'] = async () => {
    spyFetch();
    await booted();
    window.__xss = undefined;

    await check('поиск v23: переключатели второго этапа, по умолчанию выключены', async () => {
      await openKB();
      await kbTabOpen('search', '#kb-search-form');
      for (const id of ['kb-index', 'kb-rewrite', 'kb-rerank', 'kb-filter', 'kb-k0', 'kb-ctx']) assert($(id), 'нет #' + id);
      assert($('kb-rewrite').value === '' && $('kb-rerank').value === '' && !$('kb-filter').checked && $('kb-k0').value === '20' && $('kb-index').value === 'all', 'умолчания');
      assert([...$('kb-rewrite').options].map(o => o.value).join(',') === ',code,llm' && [...$('kb-rerank').options].map(o => o.value).join(',') === ',hybrid,llm', 'варианты');
      assert(!$('kb-mode').disabled && !q('#kb-stage2.on'), 'второй этап включён');
      await kbFind('чем питается харза');
      assert(qa('.kb-result').length === 2 && !q('#kb-trace'), 'без конвейера — два индекса рядом');
      const u = new URL(kbSearchCalls().pop().url, location.href);
      assert(!u.searchParams.has('filter') && !u.searchParams.has('k0') && u.searchParams.get('mode') === 'dense', 'параметры: ' + u.search);
    });

    await check('поиск v23: «кошачий медведь»: rewrite, гибрид, фильтр: исходный и переписанный запрос, 20 кандидатов', async () => {
      kbPipe('code', 'hybrid', true, 20, 5);
      assert($('kb-mode').disabled && q('#kb-stage2.on') && text('#kb-search-hint').includes('Второй этап'), 'второй этап не включился');
      await kbFind(kbT07);
      const u = new URL(kbSearchCalls().pop().url, location.href);
      const p = k => u.searchParams.get(k);
      assert(p('rewrite') === 'code' && p('rerank') === 'hybrid' && p('filter') === '1' && p('k0') === '20' && p('k') === '5' && p('index') === 'structure' && !u.searchParams.has('mode'), 'параметры: ' + u.search);
      assert(q('#kb-trace') && qa('.kb-result').length === 0, 'нет блока «до и после»');
      assert(text('#kb-original') === kbT07, 'исходный: ' + text('#kb-original'));
      assert(text('#kb-rewritten').startsWith(kbT07) && text('#kb-rewritten').includes('малая панда') && !text('#kb-rewritten').includes('Ailurus'), 'переписанный: ' + text('#kb-rewritten'));
      assert(text('#kb-bm25q').includes('малая панда Ailurus fulgens'), 'запрос BM25: ' + text('#kb-bm25q'));
      assert(!q('#kb-anchored') && text('#kb-top-gap').includes('отрыв'), 'якорь или отрыв: ' + text('#kb-tr-line'));
      assert(q('#kb-rewritten .kb-tr-add') && q('#kb-rewritten .kb-tr-add').textContent.includes('малая панда'), 'добавленное не выделено');
      assert(qa('.kb-expanded').map(x => x.textContent).includes('кошачий медведь → малая панда'), 'синонимы: ' + qa('.kb-expanded').map(x => x.textContent));
      const line = text('#kb-tr-line');
      assert(/кандидатов 20 → осталось [1-5]/.test(line) && line.includes('порог 0.80') && line.includes('Δ 0.05') && line.includes('dense') && line.includes('гибрид'), 'сводка: ' + line);
      assert(!q('#kb-empty'), 'плашка «пусто» при найденном');
    });

    await check('поиск v23: таблица кандидатов: ранги, косинус с чертой порога, отсечённые зачёркнуты с причиной', async () => {
      const rows = kbCands();
      assert(rows.length === 20, 'кандидатов ' + rows.length);
      assert(rows.map(r => r.querySelector('.kb-cand-rank').textContent).join(',') === Array.from({ length: 20 }, (_, i) => i + 1).join(','), 'ранги не по порядку');
      const kept = rows.filter(r => r.dataset.kept === '1'), cut = rows.filter(r => r.dataset.kept === '0');
      assert(kept.length >= 1 && kept.length <= 5 && String(kept.length) === text('#kb-k1-n'), 'осталось: ' + kept.length + ' / ' + text('#kb-k1-n'));
      assert(kept.some(r => r.dataset.doc === 'red-panda'), 'малой панды нет в итоге');
      rows.forEach(r => assert(/^[a-z0-9-]+\/structure\/\d{3}$/.test(r.dataset.chunk), 'chunk_id: ' + r.dataset.chunk));
      cut.forEach(r => {
        assert(r.querySelector('.kb-reason') && r.querySelector('.kb-reason').textContent.trim(), 'нет причины у ' + r.dataset.chunk);
        assert(getComputedStyle(r.querySelector('.kb-cand-where')).textDecorationLine.includes('line-through'), 'не зачёркнут ' + r.dataset.chunk);
      });
      kept.forEach(r => assert(r.querySelector('.kb-kept') && !r.querySelector('.kb-reason') && !getComputedStyle(r.querySelector('.kb-cand-where')).textDecorationLine.includes('line-through'), 'оставленный ' + r.dataset.chunk));
      const reasons = new Set(cut.map(r => r.querySelector('.kb-reason').textContent));
      assert(reasons.has('порог 0.80') && reasons.size >= 2, 'причины: ' + [...reasons]);
      // Косинус 0,76–0,89, черта порога — одна вертикаль во всех строках.
      const cos = rows.map(r => Number(r.querySelector('.kb-cos-v').textContent));
      assert(cos.every(x => x >= 0.75 && x <= 0.89), 'косинусы: ' + cos);
      const lefts = rows.map(r => r.querySelector('.kb-cos-thr').getBoundingClientRect().left);
      assert(Math.max(...lefts) - Math.min(...lefts) < 1.5 && lefts[0] > rows[0].querySelector('.kb-cos').getBoundingClientRect().left, 'черта порога не вертикаль: ' + lefts);
      // Ниже порога — полоса не доходит до черты, выше — заходит за неё.
      const low = rows.find(r => Number(r.querySelector('.kb-cos-v').textContent) < 0.8);
      assert(low && low.querySelector('.kb-cos-fill').getBoundingClientRect().right <= low.querySelector('.kb-cos-thr').getBoundingClientRect().left + 1, 'полоса ниже порога');
      assert(kept[0].querySelector('.kb-cos-fill').getBoundingClientRect().right > kept[0].querySelector('.kb-cos-thr').getBoundingClientRect().right, 'полоса выше порога');
      const rd = rows.map(r => r.children[2].textContent), rb = rows.map(r => r.children[3].textContent), rrf = rows.map(r => Number(r.children[4].textContent));
      assert(rd.every(x => /^\d+$/.test(x)) && rb.every(x => /^\d+$/.test(x)), 'ранги dense/BM25');
      assert(rrf.every((x, i) => i === 0 || x <= rrf[i - 1] + 1e-9) && rrf[0] > 0.03, 'RRF не по убыванию: ' + rrf);
      assert(rows[0].querySelector('.kb-cand-where').textContent.includes('›'), 'статья › раздел');
      const tr = q('#kb-cands').getBoundingClientRect(), wb = $('window-body').getBoundingClientRect();
      assert(tr.right <= wb.right + 1, 'таблица шире окна');
    });

    await check('поиск v23: клик по кандидату: к чанку, форма второго этапа сохраняется', async () => {
      const r = kbCands().find(x => x.dataset.kept === '1');
      const id = r.dataset.chunk;
      click(r.querySelector('.kb-cand-where'));
      await until('чанк', () => q(`#kb-text[data-doc="${r.dataset.doc}"][data-index="structure"] .kb-chunk.focus[data-id="${id}"]`), 8000);
      await kbTabOpen('search', '#kb-trace');
      assert($('kb-rewrite').value === 'code' && $('kb-rerank').value === 'hybrid' && $('kb-filter').checked && $('kb-q').value === kbT07, 'форма сброшена');
      assert(kbCands().length === 20, 'выдача пропала');
    });

    await check('поиск v23: вопрос вне базы: пусто после фильтра, плашка «в базе ответа, вероятно, нет»', async () => {
      await kbFind('Сколько весил самый крупный динозавр?');
      assert(q('#kb-trace[data-empty="1"]') && q('#kb-empty'), 'нет плашки');
      assert(text('#kb-empty').includes('Пусто после фильтра') && text('#kb-empty').includes('в базе ответа, вероятно, нет') && text('#kb-empty').includes('ниже порога 0.80'), 'плашка: ' + text('#kb-empty'));
      assert(kbCands().length === 20 && kbCands().every(r => r.dataset.kept === '0' && r.querySelector('.kb-reason').textContent === 'порог 0.80'), 'не все отсечены порогом');
      assert(text('#kb-k1-n') === '0', 'осталось: ' + text('#kb-k1-n'));
      assert(text('#kb-rewritten') === text('#kb-original') && q('.kb-tr-q[data-changed="0"]'), 'переписан без синонимов');
      const e = $('kb-empty').getBoundingClientRect();
      assert(e.height > 20 && getComputedStyle($('kb-empty')).borderLeftWidth === '5px', 'плашка незаметна');
    });

    await check('поиск v23: вид назван в запросе — плашка «абсолютный порог не применяется», отрыв лучшего', async () => {
      await kbFind('Сколько весит манул?');
      assert(q('#kb-anchored') && text('#kb-anchored').includes('Вид назван в запросе') && text('#kb-anchored').includes('манул') &&
        text('#kb-anchored').includes('абсолютный порог не применяется'), 'плашка якоря: ' + text('#kb-trace'));
      assert(kbCands().every(r => !(r.querySelector('.kb-reason') || { textContent: '' }).textContent.startsWith('порог')), 'пол применён к якорному запросу');
      assert(!q('#kb-empty') && /отрыв \d\.\d{3}/.test(text('#kb-top-gap')) && text('#kb-tr-line').includes('(умолчание)'), 'сводка: ' + text('#kb-tr-line'));
    });

    await check('поиск v23: без фильтра: итог первые k, остальные «за пределами K1»; K0 = 10', async () => {
      kbPipe('', '', false, 10, 3);
      assert(!q('#kb-stage2.on') && !$('kb-mode').disabled, 'всё выключено — второй этап выключен');
      kbSel('kb-rewrite', 'code');
      await kbFind(kbT07);
      const rows = kbCands();
      assert(rows.length === 10 && rows.filter(r => r.dataset.kept === '1').length === 3, 'кандидатов ' + rows.length);
      assert(rows.slice(0, 3).every(r => r.dataset.kept === '1') && rows.slice(3).every(r => r.querySelector('.kb-reason').textContent === 'за пределами K1'), 'причины без фильтра');
      assert(text('#kb-tr-line').includes('фильтр выключен') && text('#kb-tr-line').includes('реранкинг нет'), 'сводка: ' + text('#kb-tr-line'));
      assert(rows.every(r => r.children[4].textContent === '—'), 'RRF без реранкинга');
    });

    await check('поиск v23: продолжение: предыдущий вопрос уходит в context', async () => {
      kbSel('kb-k', 5);
      $('kb-ctx').value = 'Расскажи про харзу';
      $('kb-ctx').dispatchEvent(new Event('input', { bubbles: true }));
      await kbFind('А сколько она весит?');
      const u = new URL(kbSearchCalls().pop().url, location.href);
      assert(u.searchParams.getAll('context').join('|') === 'Расскажи про харзу', 'context: ' + u.search);
      assert(text('#kb-rewritten').includes('Расскажи про харзу') && qa('.kb-expanded').some(x => x.textContent.includes('вид из контекста')), 'переписанный: ' + text('#kb-rewritten'));
      $('kb-ctx').value = '';
      $('kb-ctx').dispatchEvent(new Event('input', { bubbles: true }));
    });

    await check('поиск v23: rewrite моделью без модели: 503 с подсказкой', async () => {
      kbSel('kb-rewrite', 'llm');
      await kbFind('манул');
      assert(q('#kb-search-error[data-code="503"]') && text('#kb-search-error').includes('модели нет') && text('#kb-search-error').includes('DEEPSEEK_API_KEY'), 'ошибка: ' + text('#kb-results'));
    });

    await check('поиск v23: разметка в запросе, синонимах, заметке и статье: буквами', async () => {
      kbPipe('code', 'hybrid', true, 20, 5);
      const query = 'ксенофоб ест теги <img src=x onerror="window.__xss=48">';
      await kbFind(query);
      assert(text('#kb-original') === query.replace(/\s+/g, ' '), 'исходный: ' + text('#kb-original'));
      assert(text('#kb-rewritten').includes('<b>проверочный зверёк</b>'), 'переписанный: ' + text('#kb-rewritten'));
      assert(qa('.kb-expanded').some(x => x.textContent.includes('<i>ксенофоб</i>')), 'синоним');
      assert(text('.kb-tr-note').includes('<img src=x'), 'заметка: ' + text('.kb-tr-note'));
      assert(kbCands().some(r => r.dataset.doc === 'xss-test'), 'нет кандидата xss-test');
      assert(!q('#kb-trace img, #kb-trace script, #kb-rewritten b, .kb-expanded *, .kb-tr-note *, .kb-cand-where i, #kb-original *'), 'элемент из данных в блоке');
      await sleep(100);
      assert(window.__xss === undefined, 'исполнился код: __xss=' + window.__xss);
    });

    await check('поиск v23: всё выключено: снова два индекса рядом', async () => {
      kbPipe('', '', false);
      await kbFind('чем питается харза');
      assert(qa('.kb-result').length === 2 && !q('#kb-trace'), 'не вернулся прямой поиск');
    });
  };

  scenarios['kb-modes'] = async () => {
    spyFetch();
    await booted();
    window.__xss = undefined;

    await check('режимы: матрица режимов: test по умолчанию, лучшее выделено, вывод', async () => {
      await openKB();
      await kbTabOpen('modes', '#kb-matrix');
      const rows = qa('#kb-matrix .kb-mx-row');
      assert(rows.length === 12 && rows.every(r => r.dataset.split === 'test'), 'строк ' + rows.length);
      assert(rows.slice(0, 4).map(r => r.dataset.name).join(',') === 'base,filter,rewrite,both' && rows[0].dataset.k1 === '3', 'порядок');
      assert(qa('#kb-matrix .kb-mx-row.grp').length === 3, 'группы K1');
      const head = text('#kb-matrix tr');
      for (const h of ['recall до', 'dense∪BM25', 'recall после', 'precision', 'отсечено', 'ошибочно отсечено', 'доказательство снято', 'токенов', 'мс', 'цена']) assert(head.includes(h), 'нет колонки ' + h);
      const filter3 = q('#kb-matrix .kb-mx-row[data-name="filter"][data-k1="3"]');
      assert(filter3.querySelector('[data-col="wrong_cut"]').textContent === '2.0 %' && filter3.querySelector('[data-col="lost_q"]').textContent === '5.0 %', 'ошибочно отсечено и снято: ' + filter3.textContent);
      assert(!head.includes('out: пусто'), 'колонка out на test');
      const both5 = q('#kb-matrix .kb-mx-row[data-name="both"][data-k1="5"]');
      assert(both5.querySelector('[data-col="recall_after"]').classList.contains('best') && both5.querySelector('[data-col="precision"]').classList.contains('best'), 'both не лучший');
      assert(both5.querySelector('[data-col="recall_after"]').textContent === '0.90' && q('#kb-matrix .kb-mx-row[data-name="base"][data-k1="5"] [data-col="recall_after"]').textContent === '0.80', 'числа');
      assert(getComputedStyle(both5.querySelector('.best')).fontWeight >= 600, 'лучшее не жирное');
      assert(qa('#kb-modes-conclusion li').length === 5 && text('#kb-modes-conclusion').includes('5 из 6'), 'вывод: ' + text('#kb-modes-conclusion'));
      assert(text('#kb-matrix-meta').includes('multilingual-e5-base') && text('#kb-matrix-meta').includes('0.815'), 'шапка: ' + text('#kb-matrix-meta'));
      const t = q('#kb-matrix').getBoundingClientRect(), wb = $('window-body').getBoundingClientRect();
      assert(t.right <= wb.right + 1, 'матрица шире окна');
    });

    await check('режимы: наборы out и «все»', async () => {
      click('[data-kb-split="out"]');
      await until('out', () => qa('#kb-matrix .kb-mx-row').every(r => r.dataset.split === 'out') && qa('#kb-matrix .kb-mx-row').length === 12);
      const both = q('#kb-matrix .kb-mx-row[data-name="both"][data-k1="5"] [data-col="out_empty"]');
      assert(both.textContent.includes('5 из 6') && both.classList.contains('best'), 'out: ' + both.textContent);
      assert(q('#kb-matrix .kb-mx-row[data-name="both"][data-k1="5"] [data-col="recall_after"]').textContent === '—', 'recall на out');
      assert(q('#kb-matrix .kb-mx-row[data-name="base"][data-k1="5"] [data-col="out_empty"]').textContent.includes('0 из 6'), 'base out');
      click('[data-kb-split="all"]');
      await until('все', () => qa('#kb-matrix .kb-mx-row').length === 36);
      click('[data-kb-split="test"]');
      await until('test', () => qa('#kb-matrix .kb-mx-row').length === 12);
    });

    await check('режимы: калибровка: таблица порогов, выбранный отмечен', async () => {
      const rows = qa('#kb-calib-table .kb-cal-row');
      assert(rows.length === 15 && rows[0].dataset.min === '0.780' && rows[14].dataset.min === '0.850', 'пороги: ' + rows.map(r => r.dataset.min));
      const chosen = rows.filter(r => r.classList.contains('chosen'));
      assert(chosen.length === 1 && chosen[0].dataset.min === '0.815' && chosen[0].querySelector('.kb-chosen'), 'выбранный: ' + chosen.map(r => r.dataset.min));
      assert(chosen[0].children[1].textContent === '0.95' && chosen[0].children[2].textContent === '83.3 %' && chosen[0].textContent.includes('D01'), 'строка 0.815: ' + chosen[0].textContent);
      assert(text('#kb-calib-chosen') === '0.815' && text('#kb-calib-meta').includes('допустимое падение 0.05'), 'шапка: ' + text('#kb-calib-meta'));
      assert(text('#kb-calib-rule').includes('max-drop') && text('#kb-calib-gap').includes('зазор -0.011') && text('#kb-calib-gap').includes('T06') &&
        text('#kb-calib-gap').includes('зазора нет'), 'зазор: ' + text('#kb-calib-gap'));
    });

    await check('режимы: гистограмма косинусов dev и out с чертой порога (SVG)', async () => {
      const svg = q('svg#kb-hist');
      assert(svg && svg.dataset.dev === '20' && svg.dataset.out === '6', 'нет гистограммы');
      const sum = sel => qa(sel).reduce((s, r) => s + Number(r.dataset.n), 0);
      assert(sum('#kb-hist .kb-hist-dev') === 20 && sum('#kb-hist .kb-hist-out') === 6, 'столбики: ' + sum('#kb-hist .kb-hist-dev') + ' / ' + sum('#kb-hist .kb-hist-out'));
      const thr = q('#kb-hist .kb-hist-thr');
      assert(thr && text('#kb-hist .kb-hist-thr-l') === 'порог 0.815', 'черта порога');
      // Out — левее черты (кроме одного), dev — правее (кроме одного).
      const x = Number(thr.getAttribute('x1'));
      const left = qa('#kb-hist .kb-hist-out').filter(r => Number(r.getAttribute('x')) < x).reduce((s, r) => s + Number(r.dataset.n), 0);
      assert(left === 5, 'out левее порога: ' + left);
      const r = svg.getBoundingClientRect();
      assert(r.width >= 300 && r.height >= 120, 'размер: ' + r.width + '×' + r.height);
      assert(!q('#kb-modes img, #kb-modes script'), 'элемент из данных');
    });

    await check('режимы: REST matrix и calibration', async () => {
      const m = await factsAPI('GET', '/api/kb/matrix'), c = await factsAPI('GET', '/api/kb/calibration');
      assert(m.ok && m.data.rows.length === 36 && c.ok && c.data.chosen === 0.815, 'ответы: ' + m.code + ' ' + c.code);
    });
  };

  scenarios['kb-nofiles'] = async () => {
    await booted();
    await check('режимы: файлов нет: подсказки с командами', async () => {
      await openKB();
      await kbTabOpen('modes', '#kb-matrix-none');
      assert(text('#kb-matrix-none').includes('Матрицы режимов ещё нет') && text('#kb-matrix-none').includes('go run ./cmd/kb matrix'), 'матрица: ' + text('#kb-matrix-none'));
      assert(text('#kb-calib-none').includes('Калибровки порога ещё нет') && text('#kb-calib-none').includes('go run ./cmd/kb calibrate'), 'калибровка: ' + text('#kb-calib-none'));
      assert(!q('#kb-matrix') && !q('#kb-hist'), 'таблицы без файлов');
      const r = await factsAPI('GET', '/api/kb/matrix');
      assert(r.code === 404 && r.data.hint === 'go run ./cmd/kb matrix', 'REST: ' + r.code + ' ' + JSON.stringify(r.data));
    });
  };

  scenarios['shot-kb-trace'] = async () => {
    await booted();
    await openKB();
    await kbTabOpen('search', '#kb-search-form');
    kbPipe('code', 'hybrid', true, 20, 5);
    await kbFind(kbT07);
    $('window-body').scrollTop = 0;
    await sleep(200);
  };
  scenarios['shot-kb-trace-empty'] = async () => {
    await booted();
    await openKB();
    await kbTabOpen('search', '#kb-search-form');
    kbPipe('code', 'hybrid', true, 20, 5);
    await kbFind('Сколько весил самый крупный динозавр?');
    $('window-body').scrollTop = 0;
    await sleep(200);
  };
  scenarios['shot-kb-modes'] = async () => {
    await booted();
    await openKB();
    await kbTabOpen('modes', '#kb-matrix');
    $('window-body').scrollTop = 0;
    await sleep(200);
  };
  scenarios['shot-kb-calib'] = async () => {
    await scenarios['shot-kb-modes']();
    $('kb-calib').scrollIntoView({ block: 'start' });
    await sleep(200);
  };
  scenarios['shot-kb-ask-modes'] = async () => {
    await booted();
    await openKB();
    await kbTabOpen('ask', '#kb-ask-pick optgroup');
    kbSetModes('kb-ask-modes', ['norag', 'rag', 'rag+both']);
    kbPick('T07');
    click('#kb-ask-go');
    await until('ответы', () => q('#kb-answers') && !$('kb-ask-go').disabled, 8000);
    $('window-body').scrollTop = 0;
    await sleep(200);
  };

  async function run() {
    const name = new URLSearchParams(location.search).get('scenario') || 'main';
    const fn = scenarios[name];
    if (!fn) { write('FAIL сценарий — нет сценария ' + name); write('DONE'); return; }
    try {
      await fn();
    } catch (e) {
      write('FAIL ' + name + ' — ' + (e && e.message || e));
    }
    if (!name.startsWith('shot')) {
      await check(name + ': без ошибок JavaScript', () => assert(!errors.length, errors.join('; ')));
    }
    write('DONE');
  }
  run();
})();
