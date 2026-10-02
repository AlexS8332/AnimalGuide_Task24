package bench

import (
	"context"
	"fmt"
	"strings"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/agents"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/features"
)

// Роли реплик сценария И-2: по роли решается, что проверять после ответа.
const (
	LineSay      = "реплика"
	LineFollowUp = "уточнение"
	LineRecall   = "после перезапуска"
	LineControl  = "контрольный"
)

// Line — реплика сценария.
type Line struct {
	Text string
	Role string
	// Expect — слова, которые должны быть в ответе (любое из них по
	// каждой группе через «|»): «алекс», «рыс|lynx».
	Expect []string
	// Restart — перед репликой сервер перезапускается.
	Restart bool
}

// Memory — И-2, память и продолжение: разговор из 14 ходов, перезапуск
// сервера посередине, в конце — контрольные вопросы о начале разговора.
//
// Контрольная дорожка — без карточки фактов: то, что ушло из окна, модель
// видит только через неё.
type Memory struct {
	Lines []Line
}

// NewMemory — сценарий ТЗ.
func NewMemory() *Memory {
	return &Memory{Lines: []Line{
		{Text: "Привет! Меня зовут Алекс, я учитель биологии в школе.", Role: LineSay},
		{Text: "Расскажи про рысь: где она живёт?", Role: LineSay},
		{Text: "А чем она питается?", Role: LineFollowUp},
		{Text: "Теперь про манула: какого он размера?", Role: LineSay},
		{Text: "А где он обитает?", Role: LineFollowUp},
		{Text: "И ещё про барсука: чем он питается?", Role: LineSay},
		{Text: "А зимой он спит?", Role: LineFollowUp},
		{Text: "Как меня зовут и о каком животном мы говорили первым?", Role: LineRecall, Restart: true,
			Expect: []string{"алекс", "рыс|lynx"}},
		{Text: "Спасибо, это пригодится для урока.", Role: LineSay},
		{Text: "Напомни, как меня зовут?", Role: LineControl, Expect: []string{"алекс"}},
		{Text: "Кем я работаю?", Role: LineControl, Expect: []string{"учител|педагог"}},
		{Text: "Про какое животное я спросил первым?", Role: LineControl, Expect: []string{"рыс|lynx"}},
		{Text: "Про какое животное мы говорили вторым?", Role: LineControl, Expect: []string{"манул|otocolobus"}},
		{Text: "О каком третьем животном шла речь?", Role: LineControl, Expect: []string{"барсук|meles"}},
	}}
}

func (*Memory) ID() string    { return "И-2" }
func (*Memory) Title() string { return "Память и продолжение" }

// expected — все ли группы слов есть в ответе.
func expected(reply string, groups []string) bool {
	for _, g := range groups {
		if !Mentions(reply, strings.Split(g, "|")...) {
			return false
		}
	}
	return true
}

func (m *Memory) Run(ctx context.Context, s *Stand, r *Result) error {
	r.Goal = "справочник помнит начало разговора, переживает перезапуск и не отрицает сказанное"
	lanes := []Lane{
		{Name: "основная", Note: "окно сообщений и карточка фактов ветки", Features: s.env.Base},
		{Name: "без карточки фактов", Note: "то, что ушло из окна, модели не видно", Features: s.env.Base.With(features.Facts, false)},
	}
	r.describeLanes(s.env.Registry, lanes)
	g, err := s.Group("И-2: память", lanes)
	if err != nil {
		return err
	}
	r.Mechanism = g.Mechanism
	type tally struct {
		control, controlTotal int
		searched              []string
		followUps             int
		recall, recallSeen    bool
		denials               []string
	}
	t := map[string]*tally{}
	for _, l := range lanes {
		t[l.Name] = &tally{}
	}
	for _, line := range m.Lines {
		if line.Restart {
			s.env.logf("  — перезапуск сервера —")
			if err := s.Restart(); err != nil {
				return err
			}
		}
		steps, err := g.Send(ctx, agents.Request{Text: line.Text})
		if err != nil {
			return err
		}
		for _, st := range steps {
			x := t[st.Lane]
			reply := replyOf(st)
			switch line.Role {
			case LineFollowUp:
				x.followUps++
				if n := toolCalls(st.Turn, "search_wikipedia"); n > 0 {
					x.searched = append(x.searched, fmt.Sprintf("«%s»: поисков %d", line.Text, n))
				}
			case LineRecall:
				x.recallSeen = true
				x.recall = st.OK() && expected(reply, line.Expect)
				r.sample("после перезапуска", st, "")
			case LineControl:
				x.controlTotal++
				ok := st.OK() && expected(reply, line.Expect)
				if ok {
					x.control++
				} else {
					r.sample("контрольный вопрос без ответа", st, "ждали: "+strings.Join(line.Expect, ", "))
				}
			}
			if (line.Role == LineRecall || line.Role == LineControl) && Denies(reply) && !expected(reply, line.Expect) {
				x.denials = append(x.denials, fmt.Sprintf("«%s» → «%s»", line.Text, clip(reply, 80)))
			}
		}
	}
	main := lanes[0].Name
	for _, l := range lanes {
		x := t[l.Name]
		if l.Name == main {
			r.atLeast("контрольные вопросы", main, x.control, x.controlTotal, x.controlTotal)
			if x.followUps > 0 {
				r.yes("уточняющий вопрос без повторного поиска", main, len(x.searched) == 0, strings.Join(x.searched, "; "))
			} else {
				r.pending("уточняющий вопрос без повторного поиска", "да", main, "в сценарии нет уточнений")
			}
			if x.recallSeen {
				r.yes("после перезапуска помнит имя и животное", main, x.recall, "")
			} else {
				r.pending("после перезапуска помнит имя и животное", "да", main, "в сценарии нет перезапуска")
			}
			r.zero("ложное «этого не было» про сказанное", main, len(x.denials), x.denials)
		}
		r.metric("контрольные вопросы", l.Name, "%d из %d", x.control, x.controlTotal)
		r.metric("уточнений с повторным поиском", l.Name, "%d из %d", len(x.searched), x.followUps)
		r.metric("ложных «этого не было»", l.Name, "%d", len(x.denials))
	}
	return nil
}
