package retrieve

import (
	"context"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/corpus"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/kb"
)

// Словарь названий и его правила.
//
// Совпадение — по основам слов, а не по строке: «кошачьего медведя» должен
// найтись как «кошачий медведь». Основа своя, а не words.Stems: тот отрезает
// две последние буквы у любого слова длиннее четырёх, и «барсук» у него
// становится «барс» — синоним ирбиса нашёлся бы в вопросе про барсука.
// Здесь отрезается окончание из короткого списка (падежные окончания
// существительных и прилагательных, притяжательное «-ов/-ова»), основа не
// короче трёх букв; «барс» и «барсук» остаются разными словами.
//
// Многословное название ищется как подряд идущие слова. Правила разбора:
//
//  1. Длинное совпадение раньше короткого: слова, вошедшие в найденное
//     название, второй раз не используются. «Кошачий медведь» — малая
//     панда, и «медведь» внутри него не раскрывается в бурого.
//  2. Однословный синоним, который входит словом в название ДРУГОГО вида,
//     не раскрывается никогда: он общий. «Медведь» (синоним бурого) есть в
//     «белом», «гималайском» и «кошачьем медведе», «барс» — в «амурском
//     барсе», «рысь» — в «степной рыси» (каракал). Сказать «медведь» — не
//     назвать вид.
//  3. Название после слова-определения («пятнистая гиена», «морская
//     выдра», «эфиопский волк», «американский красный волк») не
//     раскрывается и вид не считается названным: определение, не
//     входящее в само название, называет, скорее всего, другой вид, которого
//     в корпусе нет. Правило касается и канонических названий, и
//     многословных. Признак определения — окончание прилагательного у
//     слова прямо перед названием (без знаков между ними); не определения —
//     местоимения («какой», «этот», «всего») и прилагательные, которые вид
//     не меняют («взрослая перегузна», «крупный манул»): neutralAdj.
//  4. Каноническое название вида в запросе не раскрывается (раскрывать
//     нечего), но вид считается названным: для вопроса-продолжения и для
//     латыни.
//  5. Однословное название с основой короче четырёх букв ищется только
//     начальной формой и формами множественного числа: у «долов» (синоним
//     красного волка) основа «дол», и по основе он нашёлся бы в «долю
//     рациона»; «долы», «дол», «долов» — находятся.
//  6. Однословное название не ищется:
//     - внутри слова через дефис: «барсук-медоед» — не барсук, «волк-
//       одиночка» — не о виде;
//     - в прилагательном на -ов/-ев/-ин: «харзовым» — не харза (основа
//       совпала, потому что окончание «-овым» отрезается ради «палласова»);
//     - в омониме-действии: «наложить перевязку» — не вид (homonyms,
//       короткий стоп-лист данными: слово перед названием).
//
// Синонимы — не только Species.Aliases: из первого предложения вступления
// берутся названия через «или» («Камышовый кот, или хаус, или камышовая
// кошка, или болотная рысь»; «Перевязка или перегузна»), а у кошачьих —
// пара «кот ↔ кошка» («барханный кот» → «барханная кошка»).
//
// Пропущенное раскрытие безопасно — запрос уходит как есть; ошибочное —
// добавляет в запрос чужой вид и уводит поиск. Поэтому правила 2, 3 и 6
// осторожны: лучше не раскрыть «гиену», чем раскрыть «пятнистую гиену» в
// полосатую.

// endings — окончания, которые отрезаются от слова (по убыванию длины).
var endings = func() []string {
	list := []string{
		"овой", "евой", "овым", "евым",
		"ого", "его", "ому", "ему", "ыми", "ими", "ами", "ями", "ова", "ева", "ову", "еву", "овы", "евы", "ове", "еве",
		"ой", "ей", "ий", "ый", "ая", "яя", "ое", "ее", "ые", "ие", "ых", "их", "ую", "юю", "ом", "ем", "ам", "ям",
		"ах", "ях", "ов", "ев", "ым", "им",
		"а", "я", "ы", "и", "у", "ю", "е", "о", "ь", "й",
	}
	sort.SliceStable(list, func(i, j int) bool { return utf8.RuneCountInString(list[i]) > utf8.RuneCountInString(list[j]) })
	return list
}()

// adjEndings — окончания прилагательного (правило 3). Без «-ее» и «-ей»:
// это чаще сравнительная степень («тяжелее харза») и родительный падеж
// существительного («детёнышей манула»), чем определение.
var adjEndings = []string{"ый", "ий", "ой", "ая", "яя", "ое", "ые", "ие", "ого", "его", "ому", "ему", "ым", "им", "ых", "их", "ую", "юю", "ыми", "ими"}

// determiners — местоимения и наречия с окончанием прилагательного: перед
// названием вида они его не меняют («какая гиена», «этот медведь», «всего
// манулов»).
var determiners = map[string]bool{
	"какой": true, "какая": true, "какое": true, "какие": true, "каких": true, "каким": true, "какую": true, "какого": true, "каком": true,
	"такой": true, "такая": true, "такое": true, "такие": true, "таких": true, "такого": true,
	"этот": true, "эта": true, "это": true, "эти": true, "этого": true, "этой": true, "этих": true, "этом": true, "этому": true,
	"тот": true, "та": true, "те": true, "того": true, "той": true, "тех": true,
	"каждый": true, "каждая": true, "каждого": true, "любой": true, "любая": true, "любого": true,
	"мой": true, "моя": true, "наш": true, "наша": true, "нашего": true, "весь": true, "вся": true, "все": true, "всех": true, "всего": true,
	"сам": true, "самый": true, "самая": true, "самого": true, "самой": true, "самых": true,
	"другой": true, "другая": true, "другого": true, "другие": true, "других": true,
	"ваш": true, "ваша": true, "твой": true, "твоя": true, "свой": true, "своего": true, "своей": true, "свои": true, "своих": true,
	"его": true, "ее": true, "их": true, "него": true, "много": true, "немного": true, "сколько": true,
	"который": true, "которая": true, "которого": true, "которой": true, "которые": true, "которых": true,
	"один": true, "одна": true, "одного": true, "одной": true, "обоих": true,
}

// neutralAdj — основы прилагательных, которые не меняют вид (правило 3):
// «взрослая перегузна» — перевязка, «крупный манул» — манул. Сравнение — по
// началу слова.
var neutralAdj = []string{
	"взросл", "молод", "юн", "стар", "пожил", "крупн", "мелк", "маленьк", "больш", "огромн",
	"обычн", "типичн", "средн", "здоров", "больн", "ранен", "голодн", "сыт", "беремен", "кормящ",
	"настоящ", "жив", "дик", "одинок", "сам",
}

// homonyms — омонимы-действия (правило 6): основа названия → начала слов,
// после которых оно не вид («наложить перевязку», «сменить перевязку»).
var homonyms = map[string][]string{
	"перевязк": {"налож", "наклад", "сдела", "дела", "смен", "меня", "сня", "снима", "туг"},
}

// shortForms — окончания, с которыми ищется однословное название с
// короткой основой (правило 5): начальная форма и множественное число.
var shortForms = []string{"", "ы", "ов", "ам", "ами", "ах"}

// stem — основа слова: нижний регистр, «ё» как «е», без окончания из
// endings (основа не короче трёх букв) и без мягкого знака на конце основы
// («кошачьего» → «кошачь» → «кошач», как «кошачий»).
func stem(w string) string {
	w = strings.ReplaceAll(strings.ToLower(w), "ё", "е")
	n := utf8.RuneCountInString(w)
	for _, e := range endings {
		if strings.HasSuffix(w, e) && n-utf8.RuneCountInString(e) >= 3 {
			w = strings.TrimSuffix(w, e)
			break
		}
	}
	if strings.HasSuffix(w, "ь") && utf8.RuneCountInString(w) > 3 {
		w = strings.TrimSuffix(w, "ь")
	}
	return w
}

// sameStem — основы совпадают; длинные основы — и с разницей в одну
// букву на конце («солонг» / «солонго» — беглая гласная и «-ой/-оя»).
// Короче пяти букв — только точно: «барс» не «барсук».
func sameStem(a, b string) bool {
	if a == b {
		return true
	}
	la, lb := utf8.RuneCountInString(a), utf8.RuneCountInString(b)
	if la > lb {
		a, b, la, lb = b, a, lb, la
	}
	return la >= 5 && lb-la == 1 && strings.HasPrefix(b, a)
}

// token — слово текста.
type token struct {
	word string // нижний регистр, ё → е
	stem string
	from int // байтовые смещения в тексте
	to   int
	// hyph — слово соединено с соседним дефисом («барсук-медоед»).
	hyph bool
}

func tokenize(s string) []token {
	var out []token
	start := -1
	flush := func(end int) {
		if start >= 0 {
			w := strings.ReplaceAll(strings.ToLower(s[start:end]), "ё", "е")
			out = append(out, token{word: w, stem: stem(w), from: start, to: end})
			start = -1
		}
	}
	for i, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			if start < 0 {
				start = i
			}
			continue
		}
		flush(i)
	}
	flush(len(s))
	for i := 1; i < len(out); i++ {
		if between := s[out[i-1].to:out[i].from]; between == "-" || between == "‑" || between == "‐" {
			out[i-1].hyph, out[i].hyph = true, true
		}
	}
	return out
}

// entry — название в индексе сопоставления.
type entry struct {
	key       string // нормализованное название
	stems     []string
	words     []string
	canon     string
	canonical bool // само каноническое название
	ambiguous bool // однословное и входит в название другого вида (правило 2)
}

// matchIndex — названия по убыванию числа слов (правило 1).
type matchIndex struct {
	entries []entry
}

// index — индекс сопоставления, строится из Canon при первом вызове: словарь
// можно собрать и литералом (тесты, другой источник названий).
func (a *Aliases) index() *matchIndex {
	a.once.Do(func() {
		ix := &matchIndex{}
		for key, canon := range a.Canon {
			var stems, words []string
			for _, t := range tokenize(key) {
				stems = append(stems, t.stem)
				words = append(words, t.word)
			}
			if len(stems) == 0 {
				continue
			}
			ix.entries = append(ix.entries, entry{key: key, stems: stems, words: words, canon: canon,
				canonical: key == corpus.Normalize(canon)})
		}
		for i := range ix.entries {
			e := &ix.entries[i]
			if len(e.stems) != 1 || e.canonical {
				continue
			}
		other:
			for _, o := range ix.entries {
				if o.canon == e.canon {
					continue
				}
				for _, s := range o.stems {
					if sameStem(s, e.stems[0]) {
						e.ambiguous = true
						break other
					}
				}
			}
		}
		sort.Slice(ix.entries, func(i, j int) bool {
			x, y := ix.entries[i], ix.entries[j]
			if len(x.stems) != len(y.stems) {
				return len(x.stems) > len(y.stems)
			}
			return x.key < y.key
		})
		a.ix = ix
	})
	return a.ix
}

// found — название, найденное в тексте.
type found struct {
	from, to  int // слова [from, to)
	canon     string
	canonical bool
	surface   string // как написано в тексте
}

// matches — названия видов в тексте по правилам 1–6, по порядку в тексте.
func (a *Aliases) matches(text string) []found {
	if a == nil || len(a.Canon) == 0 {
		return nil
	}
	ix := a.index()
	toks := tokenize(text)
	used := make([]bool, len(toks))
	var out []found
	for _, e := range ix.entries {
		n := len(e.stems)
	scan:
		for i := 0; i+n <= len(toks); i++ {
			for j := 0; j < n; j++ {
				if used[i+j] || !sameStem(toks[i+j].stem, e.stems[j]) {
					continue scan
				}
			}
			if n == 1 && !oneWordOK(e, toks[i]) {
				continue
			}
			if n == 1 && i > 0 && homonymBefore(e.stems[0], toks[i-1]) {
				continue
			}
			if modifierBefore(text, toks, used, i) {
				continue
			}
			for j := i; j < i+n; j++ {
				used[j] = true
			}
			out = append(out, found{from: i, to: i + n, canon: e.canon, canonical: e.canonical,
				surface: text[toks[i].from:toks[i+n-1].to]})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].from < out[j].from })
	return out
}

// oneWordOK — однословное название e можно засчитать в слове t (правила 2,
// 5, 6).
func oneWordOK(e entry, t token) bool {
	if !e.canonical && e.ambiguous {
		return false
	}
	if t.hyph {
		return false
	}
	st := e.stems[0]
	if utf8.RuneCountInString(st) < 4 {
		// Правило 5: начальная форма и множественное число, а не любая
		// форма с той же основой: «долы», а не «долю».
		for _, f := range shortForms {
			if t.word == st+f || t.word == e.words[0] {
				return true
			}
		}
		return false
	}
	// Правило 6: прилагательное на -ов/-ев/-ин от названия («харзовым»):
	// после основы — суффикс и ещё окончание. Родительный множественного
	// («хаусов», «волков») — ровно «-ов/-ев» — остаётся.
	if strings.HasPrefix(t.word, st) {
		rest := strings.TrimPrefix(t.word, st)
		if utf8.RuneCountInString(rest) > 2 && (strings.HasPrefix(rest, "ов") || strings.HasPrefix(rest, "ев") || strings.HasPrefix(rest, "ин")) {
			return false
		}
	}
	return true
}

// homonymBefore — слово перед названием делает его действием, а не видом
// (правило 6, homonyms).
func homonymBefore(st string, prev token) bool {
	for _, p := range homonyms[st] {
		if strings.HasPrefix(prev.word, p) {
			return true
		}
	}
	return false
}

// modifierBefore — перед словом i стоит определение (правило 3): слово с
// окончанием прилагательного прямо перед названием, без знаков между ними,
// не вошедшее в другое найденное название, не местоимение и не
// прилагательное, которое вид не меняет.
func modifierBefore(text string, toks []token, used []bool, i int) bool {
	if i == 0 || used[i-1] {
		return false
	}
	if strings.TrimSpace(text[toks[i-1].to:toks[i].from]) != "" {
		return false
	}
	w := toks[i-1].word
	if determiners[w] || utf8.RuneCountInString(w) < 4 {
		return false
	}
	if strings.HasSuffix(w, "ние") || strings.HasSuffix(w, "тие") {
		// «питание манула», «развитие» — существительные, не определения.
		return false
	}
	for _, p := range neutralAdj {
		if strings.HasPrefix(w, p) {
			return false
		}
	}
	for _, e := range adjEndings {
		if strings.HasSuffix(w, e) {
			return true
		}
	}
	return false
}

// LoadAliases — словарь из документов базы (kb.Store.Doc → Species).
// Канон — Species.Ru (пусто — заголовок документа строчными); синонимы —
// сам канон, Species.Aliases, латынь, заголовок документа, названия через
// «или» из первого предложения вступления и пары «кот ↔ кошка» (AliasesOf).
// Синоним, который у двух видов разный, выбрасывается: раскрывать его не во
// что.
func LoadAliases(ctx context.Context, st *kb.Store) (*Aliases, error) {
	infos, err := st.Docs(ctx)
	if err != nil {
		return nil, err
	}
	var docs []corpus.Doc
	for _, d := range infos {
		doc, err := st.Doc(ctx, d.ID)
		if err != nil {
			return nil, err
		}
		docs = append(docs, doc)
	}
	return AliasesOf(docs), nil
}

// AliasesOf — словарь из документов корпуса (для тестов и corpus.Load;
// добавление к контракту).
func AliasesOf(docs []corpus.Doc) *Aliases {
	a := &Aliases{Canon: map[string]string{}, Latin: map[string]string{}}
	bad := map[string]bool{}
	put := func(name, canon string) {
		k := corpus.Normalize(name)
		if k == "" || bad[k] {
			return
		}
		if prev, ok := a.Canon[k]; ok && prev != canon {
			delete(a.Canon, k)
			bad[k] = true
			return
		}
		a.Canon[k] = canon
	}
	for _, d := range docs {
		sp := d.Species
		if sp == nil {
			continue
		}
		canon := strings.TrimSpace(sp.Ru)
		if canon == "" {
			canon = strings.ToLower(strings.TrimSpace(d.Title))
		}
		if canon == "" {
			continue
		}
		names := []string{canon}
		names = append(names, sp.Aliases...)
		names = append(names, d.Title)
		names = append(names, introNames(d.Intro)...)
		for _, x := range names {
			put(x, canon)
			if v := catVariant(x); v != "" {
				put(v, canon)
			}
		}
		if sp.Latin != "" {
			put(sp.Latin, canon)
			a.Latin[canon] = sp.Latin
		}
	}
	return a
}

// introNames — названия через «или» из первого предложения вступления:
// «Камышовый кот, или хаус, или камышовая кошка, или болотная рысь (лат.
// …)» → хаус, камышовая кошка, болотная рысь; «Перевязка или перегузна» →
// перегузна. Скобки (латынь, «устаревшие названия: …») выбрасываются;
// после «или» берётся текст до запятой; название — одно–три русских слова.
func introNames(intro string) []string {
	s := strings.TrimSpace(intro)
	// Скобки — парами, затем — до первой незакрытой скобки или конца фразы.
	for {
		i := strings.Index(s, "(")
		j := strings.Index(s, ")")
		if i < 0 || j < i {
			break
		}
		s = s[:i] + " " + s[j+1:]
	}
	for _, stop := range []string{"(", ".", " — ", ";", ":"} {
		if i := strings.Index(s, stop); i >= 0 {
			s = s[:i]
		}
	}
	parts := strings.Split(s, " или ")
	if len(parts) < 2 {
		return nil
	}
	var out []string
	for _, p := range parts[1:] {
		if i := strings.Index(p, ","); i >= 0 {
			p = p[:i]
		}
		p = strings.TrimSpace(strings.Trim(strings.TrimSpace(p), ","))
		ws := strings.Fields(p)
		if len(ws) == 0 || len(ws) > 3 || !cyrillicWords(ws) || utf8.RuneCountInString(p) < 4 || introSkip[strings.ToLower(ws[0])] {
			continue
		}
		out = append(out, p)
	}
	return out
}

// introSkip — сокращения и пометки после «или», а не названия: «или
// устар. …» у дальневосточного леопарда.
var introSkip = map[string]bool{"устар": true, "лат": true, "англ": true, "др": true, "также": true, "реже": true, "устаревшее": true}

// cyrillicWords — все слова — русские буквы (без «лат», цифр и сокращений).
func cyrillicWords(ws []string) bool {
	for _, w := range ws {
		for _, r := range w {
			if !unicode.Is(unicode.Cyrillic, r) && r != '-' {
				return false
			}
		}
	}
	return true
}

// catVariant — у кошачьих пара «кот ↔ кошка»: «барханный кот» →
// «барханная кошка», «камышовая кошка» → «камышовый кот». Определения
// согласуются по роду (сопоставление — по основам, так что важна только
// основа); названия без «кот/кошка» последним словом — пусто.
func catVariant(name string) string {
	ws := strings.Fields(strings.ToLower(strings.TrimSpace(name)))
	if len(ws) < 2 {
		return ""
	}
	last := strings.ReplaceAll(ws[len(ws)-1], "ё", "е")
	var toFem bool
	switch last {
	case "кот":
		toFem = true
	case "кошка":
	default:
		return ""
	}
	out := make([]string, 0, len(ws))
	for _, w := range ws[:len(ws)-1] {
		out = append(out, regender(w, toFem))
	}
	if toFem {
		return strings.Join(append(out, "кошка"), " ")
	}
	return strings.Join(append(out, "кот"), " ")
}

// regender — определение в женском (fem) или мужском роде.
func regender(w string, fem bool) string {
	if fem {
		switch {
		case strings.HasSuffix(w, "ый") || strings.HasSuffix(w, "ой"):
			return strings.TrimSuffix(strings.TrimSuffix(w, "ый"), "ой") + "ая"
		case strings.HasSuffix(w, "ий"):
			base := strings.TrimSuffix(w, "ий")
			if strings.HasSuffix(base, "к") || strings.HasSuffix(base, "г") || strings.HasSuffix(base, "х") ||
				strings.HasSuffix(base, "ж") || strings.HasSuffix(base, "ш") || strings.HasSuffix(base, "ч") || strings.HasSuffix(base, "щ") {
				return base + "ая"
			}
			return base + "яя"
		case strings.HasSuffix(w, "ов") || strings.HasSuffix(w, "ев") || strings.HasSuffix(w, "ин"):
			return w + "а"
		}
		return w
	}
	switch {
	case strings.HasSuffix(w, "ая"):
		return strings.TrimSuffix(w, "ая") + "ый"
	case strings.HasSuffix(w, "яя"):
		return strings.TrimSuffix(w, "яя") + "ий"
	case strings.HasSuffix(w, "ова") || strings.HasSuffix(w, "ева") || strings.HasSuffix(w, "ина"):
		return strings.TrimSuffix(w, "а")
	}
	return w
}

// Expand — запрос с раскрытыми синонимами и список раскрытого. Канон уже в
// запросе — ничего не добавляется. К запросу дописываются канон и латынь
// раскрытого вида: «Сколько весит кошачий медведь? малая панда Ailurus
// fulgens»; в списке — «кошачий медведь → малая панда». Запросы конвейера
// для dense и BM25 раздельно — Queries.
func (a *Aliases) Expand(q string) (string, []string) {
	dense, _, list := a.Queries(q)
	if len(list) == 0 {
		return q, nil
	}
	var lat []string
	for _, x := range list {
		canon := x[strings.LastIndex(x, " → ")+len(" → "):]
		if l := a.Latin[canon]; l != "" && !containsFold(q, l) {
			lat = append(lat, l)
		}
	}
	if len(lat) == 0 {
		return dense, list
	}
	return dense + " " + strings.Join(lat, " "), list
}

// Queries — запросы для dense и BM25 и список раскрытого.
//
//   - dense: реплика как есть плюс канон вида, названного синонимом;
//     если вид назван каноном — реплика без изменений. Латынь в dense не
//     идёт: e5 читает её как шум, и на dev латынь роняла доказательство с
//     первого места на четвёртое;
//   - bm25: dense плюс латынь каждого названного вида (MDD в корпусе
//     записан латынью, и вопрос о статусе ищет MDD по ней).
func (a *Aliases) Queries(q string) (dense, bm25 string, list []string) {
	q = strings.TrimSpace(q)
	ms := a.matches(q)
	named := map[string]bool{}
	for _, m := range ms {
		if m.canonical {
			named[m.canon] = true
		}
	}
	var add, lat []string
	seen := map[string]bool{}
	for _, m := range ms {
		if !seen[m.canon] {
			seen[m.canon] = true
			if l := a.Latin[m.canon]; l != "" && !containsFold(q, l) {
				lat = append(lat, l)
			}
		}
		if m.canonical || named[m.canon] {
			continue
		}
		named[m.canon] = true
		list = append(list, m.surface+" → "+m.canon)
		add = append(add, m.canon)
	}
	dense = q
	if len(add) > 0 {
		dense = q + " " + strings.Join(add, " ")
	}
	bm25 = dense
	if len(lat) > 0 {
		bm25 = dense + " " + strings.Join(lat, " ")
	}
	return dense, bm25, list
}

// Species — канонические названия видов, упомянутых в тексте, по порядку
// первого упоминания (для вопросов-продолжений: вид из контекста).
func (a *Aliases) Species(text string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range a.matches(text) {
		if !seen[m.canon] {
			seen[m.canon] = true
			out = append(out, m.canon)
		}
	}
	return out
}

func containsFold(s, sub string) bool {
	return strings.Contains(corpus.Normalize(s), corpus.Normalize(sub))
}

// animalNouns — основы существительных-животных вне корпуса (и общих слов
// «зверь», «животное»): реплика с таким словом — не продолжение прошлой
// темы, а новый вопрос («Сколько весит взрослый жираф?» после «Где водится
// харза?»). Названия корпуса (последние слова названий) добавляются из
// словаря — Aliases.animal.
var animalNouns = []string{
	"жираф", "слон", "кит", "акул", "пингвин", "ягуар", "лев", "льв", "тигр", "гепард", "леопард", "пум", "кошк", "кот",
	"собак", "пес", "псов", "лис", "лисиц", "волк", "медвед", "птиц", "рыб", "зме", "змей", "кенгуру", "обезьян", "зебр",
	"бегемот", "носорог", "крокодил", "черепах", "орл", "орел", "сов", "мыш", "крыс", "заяц", "зайц", "кролик", "белк",
	"еж", "ежик", "олен", "лос", "кабан", "лошад", "коров", "овц", "коз", "свин", "верблюд", "панд", "енот", "скунс",
	"фосс", "мангуст", "гиен", "шакал", "койот", "барсук", "куниц", "хор", "хорек", "хорьк", "ласк", "норк", "выдр",
	"собол", "росомах", "горностай", "тюлен", "морж", "дельфин", "хомяк", "звер", "животн", "хищник", "млекопитающ",
	"мейн", "кун", "гризли", "барс", "рыс",
}

// animal — основа — существительное-животное: из animalNouns или последнее
// слово названия вида корпуса («кот», «панд», «манул»).
func (a *Aliases) animal(st string) bool {
	for _, x := range animalNouns {
		if sameStem(st, x) {
			return true
		}
	}
	if a == nil || len(a.Canon) == 0 {
		return false
	}
	for _, e := range a.index().entries {
		if sameStem(st, e.stems[len(e.stems)-1]) {
			return true
		}
	}
	return false
}
