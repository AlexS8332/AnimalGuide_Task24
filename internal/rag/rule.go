package rag

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/kb"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/words"
)

// Правило кодом — первый голос оценки. Оно нарочно простое и предсказуемое:
// ищет в ответе формы из Expect вопроса по словам и числам. Ошибается оно
// там, где ответ сказан другими словами; поэтому рядом судья-модель, а
// расхождения голосов идут списком для ручного просмотра, а не в итог.
//
// Сравнение — по ТОКЕНАМ, а не подстрокой: «45» не должно найтись в
// «1945», «г» — в «года», «2» — в chunk_id «…/structure/002». Слова
// сравниваются по основе (как internal/words: у слов длиннее четырёх букв
// отрезан хвост в две буквы) префиксом, очень короткие («г», «м», «LC»,
// «EN») — только целиком.

// Rule оценивает ответ правилом: correct — все группы must и все числа
// найдены, must_not нет; partial — найдена хотя бы половина; wrong — иначе
// или есть must_not; abstain — в ответе явный отказ («не знаю», «нет
// данных», «не могу ответить») и не найдено ни одной группы. Слова
// сравниваются по основам (internal/words), числа — с допуском, с учётом
// «1,5»/«1.5», «2–3» и записи словами для 1–10.
//
// Для неотвечаемого вопроса (answerable=false) правильный исход — abstain:
// отказ без must_not, без чисел и без утверждения после «но», «однако»,
// «обычно», «из общих сведений» — abstain; любой ответ по существу, в том
// числе рядом с оговоркой «данных нет», — wrong. Числа
// словами понимаются и дальше десяти (до 20, десятки до 30, «полтора»),
// множители — «тыс.», «млн»; диапазон «2–3 тыс.» — это 2000 и 3000.
func Rule(q kb.Question, answer string) RuleResult {
	a := parse(answer)
	var e kb.Expect
	if q.Expect != nil {
		e = *q.Expect
	}
	var r RuleResult
	for _, bad := range e.MustNot {
		if a.has(bad) {
			r.Bad = append(r.Bad, bad)
		}
	}
	refusal := refusalOf(a)

	if !q.Answerable {
		// Отказ засчитывается, только если рядом с ним нет ответа по
		// существу: «данных нет; в неволе доживают до 20 лет» и «в базе
		// нет, но обычно около 10 кг» — это ответ из памяти с оговоркой, а
		// не «не знаю».
		claim := ""
		switch {
		case a.hasNumbers():
			claim = "в ответе есть числа"
		case hedgeClaim(a.norm) != "":
			claim = "после «" + hedgeClaim(a.norm) + "» — утверждение по существу"
		}
		switch {
		case len(r.Bad) > 0:
			r.Verdict, r.Note = Wrong, "в ответе недопустимое: "+strings.Join(r.Bad, ", ")
		case refusal != "" && claim == "":
			r.Verdict, r.Note = Abstain, "отказ: «"+refusal+"» — верно для вопроса без ответа в базе"
		case refusal != "":
			r.Verdict, r.Note = Wrong, "отказ «"+refusal+"», но "+claim+": ответ по существу на вопрос, ответа на который в базе нет"
		default:
			r.Verdict, r.Note = Wrong, "ответ по существу на вопрос, ответа на который в базе нет"
		}
		return r
	}

	found, total := 0, len(e.Must)+len(e.Numbers)
	for _, group := range e.Must {
		if len(group) == 0 {
			total--
			continue
		}
		hit := false
		for _, form := range group {
			if a.has(form) {
				hit = true
				break
			}
		}
		if hit {
			r.Hit = append(r.Hit, group[0])
			found++
		} else {
			r.Miss = append(r.Miss, group[0])
		}
	}
	nums := 0
	for _, n := range e.Numbers {
		mark := "✗"
		if a.hasNumber(n) {
			mark = "✓"
			nums++
		}
		r.Numbers = append(r.Numbers, numberText(n)+" "+mark)
	}
	found += nums
	counts := fmt.Sprintf("групп %d из %d, чисел %d из %d", len(r.Hit), len(r.Hit)+len(r.Miss), nums, len(e.Numbers))
	switch {
	case len(r.Bad) > 0:
		r.Verdict, r.Note = Wrong, "в ответе недопустимое: "+strings.Join(r.Bad, ", ")+"; "+counts
	case refusal != "" && len(r.Hit) == 0:
		r.Verdict, r.Note = Abstain, "отказ: «"+refusal+"»; "+counts
	case total <= 0:
		// Ожидания нет — правилу нечем судить; решает судья.
		r.Verdict, r.Note = Partial, "у вопроса нет ожидания — правило не судит"
	case found == total:
		r.Verdict, r.Note = Correct, counts
	case found*2 >= total:
		r.Verdict, r.Note = Partial, counts
	default:
		r.Verdict, r.Note = Wrong, counts
	}
	return r
}

// token — слово или число ответа. Дефис между числами («45–54») — отдельный
// токен-разделитель диапазона: он нужен множителю («2–3 тыс.»), а при
// сравнении форм пропускается.
type token struct {
	text  string
	num   bool
	value float64
	dash  bool
}

// parsed — ответ, разобранный для сравнения.
type parsed struct {
	norm   string
	toks   []token // без дефисов
	values []float64
}

// chunkRef — ссылка на фрагмент «manul/structure/004»: её цифры — не
// числа ответа (иначе «…/013» засчитал бы «13 часов»).
var chunkRef = regexp.MustCompile(`[\p{L}0-9][\p{L}0-9_-]*/(structure|fixed)/\d+`)

// thousands — число с разрядами через пробел: «5 000», «58 000».
var thousands = regexp.MustCompile(`\d{1,3}(?:[ \x{a0}\x{202f}\x{2009}]\d{3})+`)

// normalize — нижний регистр, ё→е, без ссылок на фрагменты, разряды
// склеены.
func normalize(s string) string {
	s = strings.ReplaceAll(strings.ToLower(s), "ё", "е")
	s = chunkRef.ReplaceAllString(s, " ")
	return collapseThousands(s)
}

// collapseThousands склеивает «5 000» в «5000», но только если группа
// стоит отдельно: «в 2024 500» или «1,500 000» не трогаются.
func collapseThousands(s string) string {
	locs := thousands.FindAllStringIndex(s, -1)
	if len(locs) == 0 {
		return s
	}
	var b strings.Builder
	last := 0
	for _, l := range locs {
		before := l[0] > 0 && (isDigitByte(s[l[0]-1]) || ((s[l[0]-1] == ',' || s[l[0]-1] == '.') && l[0] > 1 && isDigitByte(s[l[0]-2])))
		after := l[1] < len(s) && isDigitByte(s[l[1]])
		if before || after {
			continue
		}
		b.WriteString(s[last:l[0]])
		for _, r := range s[l[0]:l[1]] {
			if unicode.IsDigit(r) {
				b.WriteRune(r)
			}
		}
		last = l[1]
	}
	b.WriteString(s[last:])
	return b.String()
}

func isDigitByte(c byte) bool { return c >= '0' && c <= '9' }

// tokenize режет нормализованный текст на слова, числа и дефисы диапазонов.
// Число — цифры с одной десятичной запятой или точкой между цифрами:
// «2,5» и «2.5» — одно и то же число, а «45, 54» — два.
func tokenize(s string) []token {
	rs := []rune(s)
	var out []token
	for i := 0; i < len(rs); {
		r := rs[i]
		switch {
		case unicode.IsDigit(r):
			j := i
			for j < len(rs) && unicode.IsDigit(rs[j]) {
				j++
			}
			if j+1 < len(rs) && (rs[j] == ',' || rs[j] == '.') && unicode.IsDigit(rs[j+1]) {
				j++
				for j < len(rs) && unicode.IsDigit(rs[j]) {
					j++
				}
			}
			text := strings.ReplaceAll(string(rs[i:j]), ",", ".")
			v, _ := strconv.ParseFloat(text, 64)
			out = append(out, token{text: text, num: true, value: v})
			i = j
		case unicode.IsLetter(r):
			j := i
			for j < len(rs) && unicode.IsLetter(rs[j]) {
				j++
			}
			w := string(rs[i:j])
			// Число словами — слово со значением: в формах оно сравнивается
			// и как число («2 вида» = «два вида»), и как слово.
			out = append(out, token{text: w, value: wordNumbers[w]})
			i = j
		case r == '-' || r == '–' || r == '—' || r == '‒':
			out = append(out, token{text: "-", dash: true})
			i++
		default:
			i++
		}
	}
	return out
}

// parse разбирает ответ: токены для форм и числа с множителями.
func parse(s string) parsed {
	norm := normalize(s)
	all := tokenize(norm)
	p := parsed{norm: norm}
	// Числа: цифрами и словами; множитель после числа (или диапазона)
	// умножает и его, и начало диапазона.
	type numAt struct {
		i int
		v float64
	}
	var nums []numAt
	for i, t := range all {
		switch {
		case t.num:
			nums = append(nums, numAt{i, t.value})
		case !t.dash && t.value != 0:
			nums = append(nums, numAt{i, t.value})
		}
	}
	vals := make([]float64, len(nums))
	for k, n := range nums {
		vals[k] = n.v
	}
	for k, n := range nums {
		if n.i+1 >= len(all) {
			continue
		}
		m := multiplier(all[n.i+1].text)
		if m == 1 || all[n.i+1].num {
			continue
		}
		vals[k] = n.v * m
		// «2–3 тыс.»: начало диапазона — тоже тысячи.
		if k > 0 && nums[k-1].i == n.i-2 && all[n.i-1].dash {
			vals[k-1] = nums[k-1].v * m
		}
	}
	p.values = vals
	for _, t := range all {
		if !t.dash {
			p.toks = append(p.toks, t)
		}
	}
	return p
}

// multiplier — «тыс.», «тысяч», «млн», «миллиона»; 1 — не множитель.
func multiplier(w string) float64 {
	switch {
	case strings.HasPrefix(w, "тыс"):
		return 1e3
	case w == "млн" || strings.HasPrefix(w, "миллион"):
		return 1e6
	case w == "млрд" || strings.HasPrefix(w, "миллиард"):
		return 1e9
	}
	return 1
}

// has — есть ли форма в ответе: токены формы подряд среди токенов ответа.
func (p parsed) has(form string) bool {
	ft := tokenize(normalize(form))
	var want []token
	for _, t := range ft {
		if !t.dash {
			want = append(want, t)
		}
	}
	if len(want) == 0 {
		return false
	}
	for i := 0; i+len(want) <= len(p.toks); i++ {
		ok := true
		for j, w := range want {
			if !tokenMatch(w, p.toks[i+j]) {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// tokenMatch — токен формы против токена ответа. Число — только число
// (2,5 = 2.5; «2» = «два» = «двух»); слово из одной-двух букв — целиком («г» не находится в
// «года»); длиннее — основа формы префиксом слова ответа («серебрян» —
// в «серебряные», «13 час» — в «13 часов»).
func tokenMatch(f, a token) bool {
	fNum, aNum := f.num || f.value != 0, a.num || a.value != 0
	switch {
	case f.num || a.num:
		return fNum && aNum && f.value == a.value
	case fNum && aNum:
		return f.value == a.value
	}
	if len([]rune(f.text)) <= 2 {
		return f.text == a.text
	}
	stems := words.Stems(f.text)
	if len(stems) == 0 {
		return false
	}
	return strings.HasPrefix(a.text, stems[0])
}

// hasNumber — число с допуском среди чисел ответа.
func (p parsed) hasNumber(n kb.Number) bool {
	for _, v := range p.values {
		if math.Abs(v-n.V) <= n.Tol+1e-9 {
			return true
		}
	}
	return false
}

func numberText(n kb.Number) string {
	s := strconv.FormatFloat(n.V, 'f', -1, 64)
	if n.Tol > 0 {
		s += "±" + strconv.FormatFloat(n.Tol, 'f', -1, 64)
	}
	return s
}

// wordNumbers — числа словами во всех падежах, что встречаются в ответах:
// «три-четыре мыши», «два вида», «тринадцать часов», «в двух видах».
var wordNumbers = func() map[string]float64 {
	m := map[string]float64{"полтора": 1.5, "полторы": 1.5, "полутора": 1.5}
	add := func(v float64, forms ...string) {
		for _, f := range forms {
			m[f] = v
		}
	}
	add(1, "один", "одна", "одно", "одного", "одной", "одному", "одним", "одну")
	add(2, "два", "две", "двух", "двум", "двумя")
	add(3, "три", "трех", "трем", "тремя")
	add(4, "четыре", "четырех", "четырем", "четырьмя")
	add(5, "пять", "пяти", "пятью")
	add(6, "шесть", "шести", "шестью")
	add(7, "семь", "семи")
	add(8, "восемь", "восьми", "восемью")
	add(9, "девять", "девяти", "девятью")
	add(10, "десять", "десяти", "десятью")
	for i, stem := range []string{"одиннадцат", "двенадцат", "тринадцат", "четырнадцат", "пятнадцат",
		"шестнадцат", "семнадцат", "восемнадцат", "девятнадцат", "двадцат", "тридцат"} {
		v := float64(11 + i)
		switch stem {
		case "двадцат":
			v = 20
		case "тридцат":
			v = 30
		}
		add(v, stem+"ь", stem+"и", stem+"ью")
	}
	return m
}()

// refusalRe — явный отказ отвечать. Только устойчивые обороты: «не
// указано, сколько весит самец» — отказ, а «длина не указана в метрах» в
// ответе с числами — нет; второе отсекает условие «ни одной группы».
// Ссылки на фрагменты к этому времени уже вырезаны (normalize).
var refusalRe = regexp.MustCompile(`не знаю|не уверен|затрудняюсь|` +
	`не могу (точно |уверенно )?(ответить|сказать|назвать|подтвердить|дать|привести)|` +
	`нет (точных |достоверных |надежных |подтвержденных |конкретных )?(данных|сведений|информации)|` +
	`(данных|сведений|информации)[^.!?\n]{0,40}? (нет|отсутству)|не располагаю|ничего не сказано|` +
	`во? (базе|фрагментах|приведенных фрагментах|данных фрагментах|предоставленных фрагментах|источниках)` +
	`[^.!?\n]{0,60}?(нет|не содерж|не нашл|не указ|не привод|не сказано|не упомина|отсутству)|` +
	`не нашлось|не найден|не содержится|` +
	// «у меня этого нет» — так говорит о незнании модель без базы, и в это же
	// Blind превращает «в базе знаний»/«во фрагментах» перед судьёй.
	`у меня[^.!?\n]{0,40}?(нет|не указ|не сказано|не упомина)`)

// weakRefusalRe — узкие обороты: «точный вес самок не указан» рядом с
// «фосса весит 7–12 кг» — оговорка к ответу, а не отказ. Засчитываются
// отказом, только если в ответе нет чисел.
var weakRefusalRe = regexp.MustCompile(`не указан|не приводится|неизвестно`)

// refusalOf — найденный оборот отказа или пусто.
func refusalOf(a parsed) string {
	if s := refusalRe.FindString(a.norm); s != "" {
		return s
	}
	if !a.hasNumbers() {
		return weakRefusalRe.FindString(a.norm)
	}
	return ""
}

// hasNumbers — есть ли в ответе числа (цифрами или словами). «Один», «одна»
// не в счёт: «ни одного упоминания» — не число ответа.
func (p parsed) hasNumbers() bool {
	for _, v := range p.values {
		if v != 1 {
			return true
		}
	}
	for _, t := range p.toks {
		if t.num {
			return true
		}
	}
	return false
}

// hedgeRe — оборот, после которого идёт ответ по памяти: «но обычно…»,
// «из общих сведений: …». Границы слов — вручную: \b в Go только для ASCII.
var hedgeRe = regexp.MustCompile(`(?:^|[^\p{L}])(но|однако|из общих (?:сведений|данных|знаний)|по общим (?:данным|сведениям)|обычно|как правило|в среднем)(?:[^\p{L}]|$)`)

// offerRe — после «но» не утверждение, а предложение помочь: «не знаю, но
// могу поискать в Википедии».
var offerRe = regexp.MustCompile(`^[^\p{L}]*(я )?(могу|можно|рекоменду|совету|стоит|попробуйте|обратитесь|уточните)`)

// hedgeClaim — оборот, после которого в ответе есть утверждение по
// существу (два слова и больше, не отказ и не предложение помочь); пусто —
// такого нет.
func hedgeClaim(norm string) string {
	for _, m := range hedgeRe.FindAllStringSubmatchIndex(norm, -1) {
		tail := norm[m[3]:]
		if refusalRe.MatchString(tail) || offerRe.MatchString(tail) {
			continue
		}
		n := 0
		for _, t := range tokenize(tail) {
			if !t.dash && !t.num {
				n++
			}
		}
		if n >= 2 {
			return norm[m[2]:m[3]]
		}
	}
	return ""
}
