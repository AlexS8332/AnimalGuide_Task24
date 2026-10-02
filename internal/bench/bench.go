// Package bench — стенд дорожек и опыт -report как постоянная часть
// продукта (ФТ-47, раздел 13 ТЗ). Каждое следующее упражнение приносит сюда
// своё испытание, а не пишет свой report.go заново.
//
// Правила стенда, ради которых пакет заведён:
//
//   - дорожки сравнения отличаются ровно одним механизмом реестра
//     (ИП-13): иначе у разницы в ответах было бы две причины, и стенд
//     отказывается запускаться — и при двух механизмах, и при нуле;
//   - дорожки идут в ногу через runs.Manager: один вопрос всем дорожкам
//     сразу, следующий — только когда ответили все;
//   - у каждого испытания свой каталог данных: свод, профили и подборки
//     одного испытания не текут в другое;
//   - ход, прошедший не с теми механизмами, что просили (Effective ≠
//     Requested), — брак, и отчёт его показывает.
package bench

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/agents"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/collection"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/features"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/history"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/llm"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/mcp"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/profile"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/runs"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/store"
)

// ErrLanes — дорожки стенда отличаются не ровно одним механизмом.
var ErrLanes = errors.New("дорожки стенда должны отличаться ровно одним механизмом")

// Lane — дорожка стенда: имя, набор механизмов и подготовка собеседника.
type Lane struct {
	Name string
	// Note — чем дорожка отличается, словами для отчёта.
	Note     string
	Features features.Set
	// Same — дорожка с тем же набором механизмов, что у основной: другой
	// собеседник (детский и взрослый профиль в И-3) или повтор для замера
	// шума модели (B′ в И-7). Разницы механизмов такая дорожка не
	// добавляет, но и скрыть её не может: у стенда всё равно должна быть
	// дорожка, отличающаяся ровно одним механизмом.
	Same bool
	// Setup — подготовка собеседника дорожки до первого хода (анкета).
	Setup func(s *Stand, owner string) error
}

// CheckLanes проверяет правило ИП-13 и возвращает механизм, который стенд
// сравнивает. Первая дорожка — основная, с ней сверяются остальные.
func CheckLanes(reg *features.Registry, lanes []Lane) (features.Name, error) {
	if len(lanes) < 2 {
		return "", fmt.Errorf("%w: у стенда %d дорожек, сравнивать не с чем", ErrLanes, len(lanes))
	}
	base := reg.Complete(lanes[0].Features)
	names := map[string]bool{}
	var compared []features.Name
	seen := map[features.Name]bool{}
	for i, l := range lanes {
		if strings.TrimSpace(l.Name) == "" {
			return "", fmt.Errorf("у дорожки %d нет имени", i+1)
		}
		if names[l.Name] {
			return "", fmt.Errorf("дорожка «%s» объявлена дважды", l.Name)
		}
		names[l.Name] = true
		fs := reg.Complete(l.Features)
		if err := reg.Validate(fs); err != nil {
			return "", fmt.Errorf("дорожка «%s»: %w", l.Name, err)
		}
		if i == 0 {
			continue
		}
		diff := reg.Diff(base, fs)
		if l.Same {
			if len(diff) != 0 {
				return "", fmt.Errorf("%w: «%s» помечена как та же, что «%s», но отличается механизмами %v", ErrLanes, l.Name, lanes[0].Name, diff)
			}
			continue
		}
		switch len(diff) {
		case 0:
			return "", fmt.Errorf("%w: «%s» не отличается от «%s» ни одним механизмом; повтор дорожки помечается Same", ErrLanes, l.Name, lanes[0].Name)
		case 1:
		default:
			return "", fmt.Errorf("%w: «%s» отличается от «%s» механизмами %s", ErrLanes, l.Name, lanes[0].Name, diffNames(diff))
		}
		if !seen[diff[0]] {
			seen[diff[0]] = true
			compared = append(compared, diff[0])
		}
	}
	switch len(compared) {
	case 0:
		return "", fmt.Errorf("%w: ни одна дорожка не отличается от «%s»", ErrLanes, lanes[0].Name)
	case 1:
		return compared[0], nil
	}
	return "", fmt.Errorf("%w: дорожки сравнивают разные механизмы %v", ErrLanes, diffNames(compared))
}

func diffNames(ns []features.Name) string {
	if len(ns) == 0 {
		return "(никакими)"
	}
	parts := make([]string, len(ns))
	for i, n := range ns {
		parts[i] = string(n)
	}
	return strings.Join(parts, ", ")
}

// Options — чем каталог испытания отличается от обычного запуска.
type Options struct {
	// WikiBase — адрес Википедии вместо настоящей: подставные статьи с
	// попыткой управлять агентом (И-5). Пусто — как у приложения.
	WikiBase string
	// KB — база знаний (kb.db) сборки: механизм rag в каталоге стенда
	// иначе искал бы <каталог стенда>/kb.db и откатывался (И-10). Пусто —
	// как у приложения.
	KB string
}

// MCPClient — клиент MCP-сервера сборки, как его видит стенд (И-7): убить
// процесс посреди хода, снять счётчики клиента и самого сервера.
type MCPClient interface {
	Kill() error
	State() mcp.State
	ServerInfo(ctx context.Context) (*mcp.Info, error)
}

// Build — собранное приложение над каталогом стенда: менеджер ходов и то,
// что испытаниям нужно видеть помимо него.
type Build struct {
	Manager *runs.Manager
	// MCP — клиент MCP-сервера этой сборки; nil — сборка без MCP.
	MCP MCPClient
	// HTTP — сколько HTTP-запросов к источникам ушло в сеть из процесса
	// приложения; nil — не считается.
	HTTP func() int64
}

// Env — всё, что стенду нужно от приложения. Сборку менеджера (агенты,
// хуки, источники) делает приложение: стенд не знает, какие механизмы
// висят вокруг хода, и потому гоняет ровно то, что увидит человек.
type Env struct {
	Registry *features.Registry
	// Base — набор основной дорожки: умолчания приложения.
	Base features.Set
	// Root — каталог прогона; у каждого испытания в нём свой подкаталог.
	Root string
	// Open собирает приложение над каталогом данных. Повторный вызов на том
	// же каталоге — перезапуск сервера.
	Open func(dir string, o Options) (Build, error)
	// Legacy — каталог памятников прошлых форматов (testdata/legacy).
	Legacy string
	Model  string
	// Timeout — сколько ждать один ход.
	Timeout time.Duration
	// Judge — кто решает, нарушен ли свод в ответе (И-5); nil — признаки
	// кодом (Markers).
	Judge Judge
	// Progress — куда писать ход прогона; nil — никуда.
	Progress io.Writer
	// LLM — клиент модели для испытаний, которые собирают не приложение, а
	// демон (И-8); nil — такие испытания не идут.
	LLM llm.Chatter
	// KB — база знаний приложения (kb.db): по ней И-10 сравнивает ответы
	// с базой и без; пусто — kb.db в корне репозитория.
	KB string
}

func (e *Env) timeout() time.Duration {
	if e.Timeout <= 0 {
		return 10 * time.Minute
	}
	return e.Timeout
}

func (e *Env) judge() Judge {
	if e.Judge == nil {
		return Markers{}
	}
	return e.Judge
}

func (e *Env) logf(format string, args ...any) {
	if e.Progress != nil {
		fmt.Fprintf(e.Progress, format+"\n", args...)
	}
}

// Stand — каталог испытания и менеджер над ним. Все ходы, прошедшие через
// стенд, записываются по дорожкам: из них отчёт считает цену и разбивку.
type Stand struct {
	env  *Env
	dir  string
	opts Options
	m    *runs.Manager
	b    Build
	rec  *recorder

	Profiles    *profile.Store
	Collections *collection.Store
}

// NewStand заводит каталог испытания и менеджер над ним.
func (e *Env) NewStand(name string, o Options) (*Stand, error) {
	return e.stand(filepath.Join(e.Root, slug(name)), o, &recorder{reg: e.Registry})
}

func (e *Env) stand(dir string, o Options, rec *recorder) (*Stand, error) {
	if e.Open == nil {
		return nil, errors.New("стенду не передана сборка менеджера (Env.Open)")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	b, err := e.Open(dir, o)
	if err != nil {
		return nil, err
	}
	if b.Manager == nil {
		return nil, errors.New("сборка не вернула менеджер ходов")
	}
	d := store.NewDir(dir)
	return &Stand{env: e, dir: dir, opts: o, m: b.Manager, b: b, rec: rec,
		Profiles: profile.NewStore(d), Collections: collection.NewStore(d)}, nil
}

// Sub — второй каталог того же испытания со своими настройками (подставная
// Википедия в И-5). Ходы пишутся в общий учёт дорожек.
func (s *Stand) Sub(name string, o Options) (*Stand, error) {
	return s.env.stand(filepath.Join(s.dir, slug(name)), o, s.rec)
}

// Manager — менеджер стенда.
func (s *Stand) Manager() *runs.Manager { return s.m }

// MCP — клиент MCP-сервера сборки стенда; nil — сборка без MCP.
func (s *Stand) MCP() MCPClient { return s.b.MCP }

// HTTP — запросов к источникам из процесса приложения; -1 — не считается.
func (s *Stand) HTTP() int64 {
	if s.b.HTTP == nil {
		return -1
	}
	return s.b.HTTP()
}

// Dir — каталог данных стенда.
func (s *Stand) Dir() string { return s.dir }

// Restart — перезапуск сервера посреди испытания: новый менеджер на том же
// каталоге поднимает диалоги и стенды с диска.
func (s *Stand) Restart() error {
	b, err := s.env.Open(s.dir, s.opts)
	if err != nil {
		return err
	}
	if b.Manager == nil {
		return errors.New("сборка не вернула менеджер ходов")
	}
	m := b.Manager
	if _, problems := m.Load(); len(problems) > 0 {
		return fmt.Errorf("после перезапуска не поднялись диалоги: %v", problems)
	}
	s.m, s.b = m, b
	return nil
}

// Dialog — диалог одной дорожки. Дорожка может перейти в новый диалог
// (продолжение подборки, профиль в новом разговоре) — тогда она идёт уже не
// через стенд менеджера, а своим диалогом.
type Dialog struct {
	Lane  Lane
	ID    string
	Owner string
	s     *Stand
	moved bool
}

// Group — стенд из нескольких дорожек, идущих в ногу.
type Group struct {
	ID string
	// Mechanism — механизм, который стенд сравнивает.
	Mechanism features.Name
	Dialogs   []*Dialog
	s         *Stand
}

// Group заводит стенд. Дорожки проверяются до создания диалогов: стенд,
// сравнивающий не ровно один механизм, не запускается вовсе.
func (s *Stand) Group(title string, lanes []Lane) (*Group, error) {
	mech, err := CheckLanes(s.env.Registry, lanes)
	if err != nil {
		return nil, err
	}
	rl := make([]runs.Lane, len(lanes))
	for i, l := range lanes {
		rl[i] = runs.Lane{Name: l.Name, Features: l.Features, Title: l.Name}
	}
	id, details, err := s.m.StartGroup(title, rl)
	if err != nil {
		return nil, err
	}
	g := &Group{ID: id, Mechanism: mech, s: s}
	for i, d := range details {
		dl := &Dialog{Lane: lanes[i], ID: d.ID, Owner: firstOwner(d.Owners), s: s}
		if lanes[i].Setup != nil {
			if err := lanes[i].Setup(s, dl.Owner); err != nil {
				return nil, fmt.Errorf("дорожка «%s»: %w", lanes[i].Name, err)
			}
		}
		g.Dialogs = append(g.Dialogs, dl)
	}
	return g, nil
}

// Solo — один диалог без сравнения: длинный разговор для замера цены,
// поправка свода. Правило одного механизма к нему не относится — сравнивать
// не с чем.
func (s *Stand) Solo(title string, l Lane) (*Dialog, error) {
	fs := s.env.Registry.Complete(l.Features)
	if err := s.env.Registry.Validate(fs); err != nil {
		return nil, err
	}
	owner := fmt.Sprintf("%s-%s", history.NewID(), laneSlug(l.Name))
	d, err := s.m.Create(runs.StartOptions{Features: fs, Owners: []string{owner}, Titles: map[string]string{owner: l.Name},
		Title: title + " — " + l.Name, Lane: l.Name})
	if err != nil {
		return nil, err
	}
	dl := &Dialog{Lane: l, ID: d.ID, Owner: owner, s: s, moved: true}
	if l.Setup != nil {
		if err := l.Setup(s, owner); err != nil {
			return nil, err
		}
	}
	return dl, nil
}

func firstOwner(owners []string) string {
	if len(owners) == 0 {
		return ""
	}
	return owners[0]
}

// Step — ход одной дорожки: запись хода и диалог после него.
type Step struct {
	Lane   string
	Turn   history.Turn
	Detail runs.Detail
}

// OK — ход завершён.
func (st Step) OK() bool { return st.Turn.Status == history.TurnDone }

// Send задаёт вопрос всем дорожкам. Пока дорожки в своём стенде, вопрос
// уходит через SendGroup менеджера; перешедшие в новые диалоги получают его
// каждая своим диалогом, но всё равно в ногу: ответа ждём от всех, прежде
// чем вернуть.
func (g *Group) Send(ctx context.Context, req agents.Request) ([]Step, error) {
	return g.SendWatch(ctx, req, nil)
}

// SendWatch — то же, что Send, но каждый начатый ход сначала отдаётся
// watch: испытание может следить за журналом хода, пока он идёт (И-7
// убивает MCP-сервер на первом вызове источника). watch не должен
// блокироваться: за журналом он следит своей горутиной.
func (g *Group) SendWatch(ctx context.Context, req agents.Request, watch func(lane string, sess *runs.Session)) ([]Step, error) {
	m := g.s.m
	moved := false
	for _, d := range g.Dialogs {
		moved = moved || d.moved
	}
	var sessions []*runs.Session
	if !moved {
		ss, err := m.SendGroup(g.ID, req)
		if err != nil {
			return nil, err
		}
		sessions = ss
	} else {
		for _, d := range g.Dialogs {
			sess, err := m.Send(d.ID, req)
			if err != nil {
				return nil, fmt.Errorf("дорожка «%s»: %w", d.Lane.Name, err)
			}
			sessions = append(sessions, sess)
		}
	}
	if watch != nil {
		for i, sess := range sessions {
			watch(g.Dialogs[i].Lane.Name, sess)
		}
	}
	out := make([]Step, len(sessions))
	errs := make([]error, len(sessions))
	var wg sync.WaitGroup
	for i, sess := range sessions {
		wg.Add(1)
		go func(i int, sess *runs.Session) {
			defer wg.Done()
			out[i], errs[i] = g.Dialogs[i].collect(ctx, sess)
		}(i, sess)
	}
	wg.Wait()
	return out, errors.Join(errs...)
}

// Send — ход только этой дорожки.
func (d *Dialog) Send(ctx context.Context, req agents.Request) (Step, error) {
	sess, err := d.s.m.Send(d.ID, req)
	if err != nil {
		return Step{}, fmt.Errorf("дорожка «%s»: %w", d.Lane.Name, err)
	}
	return d.collect(ctx, sess)
}

// Ask — реплика человека.
func (d *Dialog) Ask(ctx context.Context, text string) (Step, error) {
	return d.Send(ctx, agents.Request{Text: text})
}

// collect ждёт ход вместе с записью в историю и достаёт его из диалога.
func (d *Dialog) collect(ctx context.Context, sess *runs.Session) (Step, error) {
	wait := d.s.env.timeout()
	if dl, ok := ctx.Deadline(); ok && time.Until(dl) < wait {
		wait = time.Until(dl)
	}
	v := sess.Wait(wait)
	if v.Status == runs.StatusRunning {
		return Step{Lane: d.Lane.Name}, fmt.Errorf("дорожка «%s»: ход не закончился за %s", d.Lane.Name, wait.Round(time.Second))
	}
	det, ok := d.s.m.Get(v.ConversationID)
	if !ok {
		return Step{Lane: d.Lane.Name}, fmt.Errorf("дорожка «%s»: диалог %s пропал", d.Lane.Name, v.ConversationID)
	}
	st := Step{Lane: d.Lane.Name, Detail: det}
	for _, t := range det.TurnList {
		if t.ID == v.ID {
			st.Turn = t
		}
	}
	if st.Turn.ID == "" {
		return st, fmt.Errorf("дорожка «%s»: ход %s не записан в историю", d.Lane.Name, v.ID)
	}
	d.s.rec.add(d.Lane.Name, st.Turn)
	d.s.env.logf("  %s · %s: %s", d.Lane.Name, clip(st.Turn.User, 60), stepLine(st))
	return st, nil
}

func stepLine(st Step) string {
	if !st.OK() {
		return "ошибка — " + clip(st.Turn.Error, 120)
	}
	return fmt.Sprintf("%s, запросов %d, %.1f с", orDash(st.Turn.Route), st.Turn.Totals.LLMCalls, st.Turn.Totals.Seconds)
}

// Continue переводит дорожку в новый диалог того же собеседника с тем же
// набором механизмов; collection — подборка, которую новый диалог
// продолжает (пусто — без подборки).
func (d *Dialog) Continue(title, collectionID string) error {
	m := d.s.m
	old, ok := m.Get(d.ID)
	if !ok {
		return fmt.Errorf("дорожка «%s»: диалог %s не найден", d.Lane.Name, d.ID)
	}
	nd, err := m.Create(runs.StartOptions{Features: old.Features, Owners: []string{d.Owner},
		Titles: map[string]string{d.Owner: d.Lane.Name}, Title: title + " — " + d.Lane.Name, Lane: d.Lane.Name})
	if err != nil {
		return err
	}
	if collectionID != "" {
		if _, err := m.SetCollection(nd.ID, collectionID, ""); err != nil {
			return err
		}
	}
	d.ID, d.moved = nd.ID, true
	return nil
}

// Detail — диалог дорожки сейчас.
func (d *Dialog) Detail() (runs.Detail, bool) { return d.s.m.Get(d.ID) }

// Dialog — диалог дорожки по имени.
func (g *Group) Dialog(lane string) *Dialog {
	for _, d := range g.Dialogs {
		if d.Lane.Name == lane {
			return d
		}
	}
	return nil
}

// recorder копит ходы испытания по дорожкам.
type recorder struct {
	reg   *features.Registry
	mu    sync.Mutex
	order []string
	turns map[string][]history.Turn
}

func (r *recorder) add(lane string, t history.Turn) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.turns == nil {
		r.turns = map[string][]history.Turn{}
	}
	if _, ok := r.turns[lane]; !ok {
		r.order = append(r.order, lane)
	}
	r.turns[lane] = append(r.turns[lane], t)
}

func (r *recorder) snapshot() ([]string, map[string][]history.Turn) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string][]history.Turn, len(r.turns))
	for k, v := range r.turns {
		out[k] = append([]history.Turn(nil), v...)
	}
	return append([]string(nil), r.order...), out
}

func slug(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			b.WriteRune(r)
		case r >= 'а' && r <= 'я', r == 'ё':
			b.WriteString(translit[r])
		case r == ' ' || r == '_' || r == '.':
			b.WriteRune('-')
		}
	}
	s := strings.Trim(b.String(), "-")
	if s == "" {
		return "trial"
	}
	return s
}

// laneSlug — имя дорожки латиницей для идентификатора собеседника.
func laneSlug(name string) string {
	s := slug(name)
	if len(s) > 24 {
		s = strings.Trim(s[:24], "-")
	}
	return s
}

var translit = map[rune]string{'а': "a", 'б': "b", 'в': "v", 'г': "g", 'д': "d", 'е': "e", 'ё': "e", 'ж': "zh",
	'з': "z", 'и': "i", 'й': "y", 'к': "k", 'л': "l", 'м': "m", 'н': "n", 'о': "o", 'п': "p", 'р': "r", 'с': "s",
	'т': "t", 'у': "u", 'ф': "f", 'х': "h", 'ц': "c", 'ч': "ch", 'ш': "sh", 'щ': "sch", 'ъ': "", 'ы': "y", 'ь': "",
	'э': "e", 'ю': "yu", 'я': "ya"}

func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}
