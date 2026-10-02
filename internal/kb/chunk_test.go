package kb

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
	"unicode"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/corpus"
)

// miniDocs — мини-корпус: у манула вступление из двух абзацев, длинный
// раздел (режется по абзацам), абзац из длинных предложений (режется по
// предложениям), раздел без текста с двумя подразделами (короткий
// склеивается с соседом), короткий раздел без соседа того же родителя.
func miniDocs() []corpus.Doc {
	sent := func(word string, n int) string {
		var b strings.Builder
		for i := 0; i < n; i++ {
			if i > 0 {
				b.WriteString(" ")
			}
			fmt.Fprintf(&b, "Манул %s добычу номер %d в степи.", word, i+1)
		}
		return b.String()
	}
	return []corpus.Doc{
		{ID: "manul", Source: corpus.SourceWikipedia, Title: "Манул", URL: "https://example.org/manul", RevID: 7,
			License: "CC BY-SA 4.0",
			Intro:   "Манул, или палласов кот, — хищное млекопитающее семейства кошачьих.\nОбитает в Азии: от Ирана до Забайкалья.",
			Sections: []corpus.Section{
				{Path: []string{"Описание"}, Title: "Описание", Level: 2,
					Text: sent("ищет", 4) + "\n" + sent("ловит", 4) + "\n" + sent("ест", 4)},
				{Path: []string{"Длинный абзац"}, Title: "Длинный абзац", Level: 2, Text: sent("прячет", 25)},
				{Path: []string{"Образ жизни"}, Title: "Образ жизни", Level: 2},
				{Path: []string{"Образ жизни", "Питание"}, Title: "Питание", Level: 3,
					Text: "Кормится манул почти исключительно мелкими грызунами и пищухами. Иногда ловит птиц, сусликов и зайцев-толаев."},
				{Path: []string{"Образ жизни", "Размножение"}, Title: "Размножение", Level: 3,
					Text: "Котята рождаются в апреле–мае."},
				{Path: []string{"Охрана"}, Title: "Охрана", Level: 2, Text: "Занесён в Красную книгу России."},
			}},
		{ID: "corsac", Source: corpus.SourceWikipedia, Title: "Корсак", URL: "https://example.org/corsac",
			License: "CC BY-SA 4.0", Intro: "Корсак — степная лисица. Весит от 2,5 до 4 кг."},
	}
}

func miniManifest(docs []corpus.Doc, sha string) corpus.Manifest {
	m := corpus.Manifest{Schema: corpus.Schema, CorpusSHA: sha}
	for _, d := range docs {
		m.Entries = append(m.Entries, corpus.Entry{ID: d.ID, File: d.ID + ".json", Title: d.Title, Source: d.Source,
			Chars: d.Chars(), SHA256: "sha-" + d.ID})
		m.Chars += d.Chars()
	}
	m.Pages = float64(m.Chars) / corpus.PageChars
	return m
}

func realDocs(t *testing.T) []corpus.Doc {
	t.Helper()
	docs, _, err := corpus.Load("../../corpus")
	if err != nil {
		t.Fatal(err)
	}
	return docs
}

// TestLayoutMatchesText — раскладка повторяет Doc.Text: на ней держатся
// все смещения чанков.
func TestLayoutMatchesText(t *testing.T) {
	for _, d := range append(miniDocs(), realDocs(t)...) {
		l := newLayout(d)
		if string(l.text) != d.Text() {
			t.Fatalf("%s: текст раскладки не совпал с Doc.Text", d.ID)
		}
		if got := string(l.text[l.blocks[0].start:l.blocks[0].end]); got != d.Intro {
			t.Fatalf("%s: вступление %q", d.ID, got)
		}
		for i, s := range d.Sections {
			b := l.blocks[i+1]
			if string(l.text[b.head:b.head+3]) != "## " {
				t.Fatalf("%s: заголовок раздела %d не на месте", d.ID, i)
			}
			if got := string(l.text[b.start:b.end]); got != s.Text {
				t.Fatalf("%s: тело раздела %v не на месте: %q", d.ID, s.Path, got)
			}
		}
		if len(l.blocks) != len(d.Sections)+1 {
			t.Fatalf("%s: блоков %d", d.ID, len(l.blocks))
		}
	}
}

var idRe = regexp.MustCompile(`^[a-z0-9-]+/(fixed|structure)/\d{3}$`)

// checkChunks — общие инварианты чанков любой стратегии.
func checkChunks(t *testing.T, d corpus.Doc, cs []Chunk, st Strategy) {
	t.Helper()
	rs := []rune(d.Text())
	for i, c := range cs {
		if c.Start < 0 || c.End > len(rs) || c.Start >= c.End {
			t.Fatalf("%s: границы [%d, %d) вне текста %d", c.ID, c.Start, c.End, len(rs))
		}
		if string(rs[c.Start:c.End]) != c.Text {
			t.Fatalf("%s: Text[Start:End] != Text", c.ID)
		}
		if c.Text != strings.TrimSpace(c.Text) {
			t.Fatalf("%s: пробелы по краям", c.ID)
		}
		if !idRe.MatchString(c.ID) || c.ID != fmt.Sprintf("%s/%s/%03d", d.ID, st, i) || c.Ord != i {
			t.Fatalf("id %q, ord %d (ждали %d)", c.ID, c.Ord, i)
		}
		if c.DocID != d.ID || c.Source != d.Source || c.Title != d.Title || c.URL != d.URL || c.RevID != d.RevID ||
			c.Strategy != st || c.Section == "" || c.SHA == "" || c.Tokens <= 0 {
			t.Fatalf("%s: метаданные %+v", c.ID, c)
		}
		if len(c.Path) == 0 && c.Section != corpus.IntroTitle || len(c.Path) > 0 && c.Section != c.Path[len(c.Path)-1] {
			t.Fatalf("%s: раздел %q при пути %v", c.ID, c.Section, c.Path)
		}
		if i > 0 && st == Fixed && c.Start <= cs[i-1].Start {
			t.Fatalf("%s: окно не продвинулось", c.ID)
		}
		if strings.HasPrefix(c.EmbedText(), d.Title) == false || !strings.HasSuffix(c.EmbedText(), "\n"+c.Text) {
			t.Fatalf("%s: EmbedText %q", c.ID, c.EmbedText())
		}
	}
}

func TestStructureMini(t *testing.T) {
	d := miniDocs()[0]
	ch := NewStructure(300, 80)
	if ch.Strategy() != Structure || ch.Params() != (Params{Max: 300, Min: 80}) {
		t.Fatalf("параметры %v", ch.Params())
	}
	cs := ch.Split(d)
	checkChunks(t, d, cs, Structure)
	l := newLayout(d)
	bySection := map[string][]Chunk{}
	for _, c := range cs {
		key := strings.Join(c.Path, sep)
		bySection[key] = append(bySection[key], c)
		if c.End-c.Start > 300+80 {
			t.Errorf("%s: %d рун больше max+min", c.ID, c.End-c.Start)
		}
		if strings.HasPrefix(c.Text, "## ") {
			t.Errorf("%s: чанк начинается со строки заголовка", c.ID)
		}
	}
	// Вступление — свой чанк с разделом «Вступление», оба абзаца вместе.
	if in := bySection[""]; len(in) != 1 || in[0].Section != corpus.IntroTitle || in[0].Text != d.Intro || in[0].Mixed {
		t.Fatalf("вступление: %+v", in)
	}
	// Длинный раздел режется по абзацам: границы кусков — на переводах строк.
	desc := bySection["Описание"]
	if len(desc) < 2 {
		t.Fatalf("Описание не разрезано: %d", len(desc))
	}
	for _, c := range desc {
		if strings.Contains(c.Text, "\n") && len(desc) == 3 {
			t.Errorf("%s: абзацы не разделены", c.ID)
		}
		if !strings.HasSuffix(c.Text, ".") || c.Mixed {
			t.Errorf("%s: кусок не по абзацу: %q", c.ID, c.Text)
		}
	}
	// Абзац длиннее max — по предложениям: каждый кусок кончается точкой и
	// начинается с заглавной.
	long := bySection["Длинный абзац"]
	if len(long) < 3 {
		t.Fatalf("длинный абзац: %d кусков", len(long))
	}
	for _, c := range long {
		if !strings.HasSuffix(c.Text, ".") || !unicode.IsUpper([]rune(c.Text)[0]) {
			t.Errorf("%s: разрез не по предложению: %q", c.ID, c.Text)
		}
		if midSentence(l, c.Start, c.End) {
			t.Errorf("%s: оборван посреди предложения", c.ID)
		}
	}
	// Короткое «Размножение» склеено с соседом того же родителя
	// («Питание»): один чанк, внутри заголовок соседа, Mixed честно true.
	food := bySection[strings.Join([]string{"Образ жизни", "Питание"}, sep)]
	if len(food) != 1 || !strings.Contains(food[0].Text, "Котята") || !strings.Contains(food[0].Text, "## Образ жизни › Размножение") || !food[0].Mixed {
		t.Fatalf("склейка соседей: %+v", food)
	}
	if _, ok := bySection[strings.Join([]string{"Образ жизни", "Размножение"}, sep)]; ok {
		t.Fatal("короткий раздел остался отдельным чанком")
	}
	// «Охрана» короткая, но соседа того же родителя рядом нет (перед ней —
	// подраздел другого уровня): остаётся одна.
	if g := bySection["Охрана"]; len(g) != 1 || g[0].Text != "Занесён в Красную книгу России." || g[0].Mixed {
		t.Fatalf("Охрана: %+v", g)
	}
	// Ни один символ не попал в два чанка structure.
	for i := 1; i < len(cs); i++ {
		if cs[i].Start < cs[i-1].End {
			t.Fatalf("%s и %s перекрываются", cs[i-1].ID, cs[i].ID)
		}
	}
}

func TestFixedMini(t *testing.T) {
	d := miniDocs()[0]
	ch := NewFixed(200, 30)
	if ch.Strategy() != Fixed || ch.Params() != (Params{Size: 200, Overlap: 30}) {
		t.Fatalf("параметры %v", ch.Params())
	}
	cs := ch.Split(d)
	checkChunks(t, d, cs, Fixed)
	rs := []rune(d.Text())
	l := newLayout(d)
	mixed := 0
	for i, c := range cs {
		// Слово не рвётся: по краям — граница слова.
		if c.Start > 0 && !unicode.IsSpace(rs[c.Start-1]) {
			t.Errorf("%s: начало посреди слова: …%q", c.ID, string(rs[c.Start-1:c.Start+5]))
		}
		if c.End < len(rs) && !unicode.IsSpace(rs[c.End]) {
			t.Errorf("%s: конец посреди слова", c.ID)
		}
		if c.End-c.Start > 200+20 {
			t.Errorf("%s: %d рун", c.ID, c.End-c.Start)
		}
		// Перекрытие с предыдущим окном есть.
		if i > 0 && c.Start >= cs[i-1].End {
			t.Errorf("%s: нет перекрытия", c.ID)
		}
		// Раздел — тот, где лежит большая часть текста чанка.
		if want := l.blocks[l.majority(c.Start, c.End)].section(); c.Section != want {
			t.Errorf("%s: раздел %q, ждали %q", c.ID, c.Section, want)
		}
		if c.Mixed != strings.Contains(c.Text, "\n##") {
			t.Errorf("%s: Mixed=%v, а текст %q", c.ID, c.Mixed, c.Text)
		}
		if c.Mixed {
			mixed++
		}
	}
	if mixed == 0 {
		t.Fatal("окно ни разу не пересекло раздел")
	}
	if cs[len(cs)-1].End != len(rs) {
		t.Fatal("хвост документа не попал в чанки")
	}
	// Короткий документ — один чанк.
	if c := NewFixed(0, 0).Split(miniDocs()[1]); len(c) != 1 || c[0].Text != miniDocs()[1].Intro {
		t.Fatalf("короткий документ: %+v", c)
	}
	if p := NewFixed(0, -1).Params(); p.Size != DefaultSize || p.Overlap != DefaultSize*DefaultOverlapPct/100 {
		t.Fatalf("умолчания fixed: %+v", p)
	}
	// overlap 0 — без перекрытия: окна идут встык.
	if p := NewFixed(0, 0).Params(); p.Overlap != 0 {
		t.Fatalf("overlap 0: %+v", p)
	}
	nov := NewFixed(120, 0).Split(d)
	for i := 1; i < len(nov); i++ {
		if nov[i].Start < nov[i-1].End {
			t.Fatalf("%s: перекрытие при overlap 0 (%d < %d)", nov[i].ID, nov[i].Start, nov[i-1].End)
		}
	}
	if p := NewStructure(0, 0).Params(); p.Max != DefaultMax || p.Min != DefaultMin {
		t.Fatalf("умолчания structure: %+v", p)
	}
}

// TestRealCorpusChunks — обе стратегии на настоящем корпусе: инварианты,
// детерминизм, у structure — нет строк «## » чужих разделов и всё
// собственное тело каждого раздела покрыто.
func TestRealCorpusChunks(t *testing.T) {
	docs := realDocs(t)
	for _, ch := range []Chunker{NewStructure(0, 0), NewFixed(0, 0)} {
		ids := map[string]bool{}
		for _, d := range docs {
			cs := ch.Split(d)
			checkChunks(t, d, cs, ch.Strategy())
			again := ch.Split(d)
			if len(again) != len(cs) {
				t.Fatalf("%s/%s: недетерминированно", d.ID, ch.Strategy())
			}
			for i := range cs {
				if cs[i].ID != again[i].ID || cs[i].SHA != again[i].SHA {
					t.Fatalf("%s: вторая сборка дала другое", cs[i].ID)
				}
				if ids[cs[i].ID] {
					t.Fatalf("%s повторяется", cs[i].ID)
				}
				ids[cs[i].ID] = true
			}
			if ch.Strategy() == Structure {
				l := newLayout(d)
				if bad := ForeignHeadings(d, cs); len(bad) > 0 {
					t.Fatalf("%s: чужие заголовки в чанках: %v", d.ID, bad)
				}
				for _, b := range l.blocks {
					bs, be := l.trim(b.start, b.end)
					for p := bs; p < be; p++ {
						if unicode.IsSpace(l.text[p]) {
							continue
						}
						in := false
						for _, c := range cs {
							if c.Start <= p && p < c.End {
								in = true
								break
							}
						}
						if !in {
							t.Fatalf("%s: символ %d раздела %v не попал ни в один чанк", d.ID, p, b.path)
						}
					}
				}
			}
		}
	}
}

func TestCovers(t *testing.T) {
	cases := []struct {
		cs, ce, qs, qe int
		want           float64
	}{
		{0, 10, 2, 8, 1},
		{0, 5, 0, 10, 0.5},
		{5, 20, 0, 10, 0.5},
		{10, 20, 0, 10, 0},
		{0, 10, 5, 5, 0},
		{3, 7, 0, 10, 0.4},
	}
	for _, c := range cases {
		if got := Covers(c.cs, c.ce, c.qs, c.qe); got != c.want {
			t.Errorf("Covers(%d,%d,%d,%d) = %v, ждали %v", c.cs, c.ce, c.qs, c.qe, got, c.want)
		}
	}
}

func TestMidSentenceAndUnion(t *testing.T) {
	d := corpus.Doc{ID: "x", Intro: "Первое предложение. Второе предложение здесь.\nНовый абзац без точки"}
	l := newLayout(d)
	text := d.Intro
	at := func(s string) int { return len([]rune(text[:strings.Index(text, s)])) }
	end := func(s string) int { return at(s) + len([]rune(s)) }
	if midSentence(l, 0, end("Первое предложение.")) {
		t.Error("целое предложение посчитано оборванным")
	}
	if !midSentence(l, 0, end("Первое предложение. Второе")) {
		t.Error("обрыв посреди предложения не замечен")
	}
	if !midSentence(l, at("предложение здесь"), end("здесь.")) {
		t.Error("начало посреди предложения не замечено")
	}
	if midSentence(l, at("Новый"), len([]rune(text))) {
		t.Error("абзац без точки в конце текста — не обрыв")
	}
	if n := unionLen([][2]int{{0, 10}, {5, 15}, {20, 25}}); n != 20 {
		t.Errorf("unionLen = %d", n)
	}
	if percentile([]int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}, 50) != 5 || percentile([]int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}, 95) != 10 {
		t.Error("percentile")
	}
	if MedianChars([]Chunk{{Start: 0, End: 10}, {Start: 0, End: 30}, {Start: 0, End: 20}}) != 20 {
		t.Error("MedianChars")
	}
}

// TestLongParagraphNoSpaces — абзац без пробелов и точек (длиннее Max)
// режется на ceil(длина/Max) частей поровну, каждая ≤ Max, без потерь.
func TestLongParagraphNoSpaces(t *testing.T) {
	long := strings.Repeat("абвгдежзик", 300) // 3000 рун
	d := corpus.Doc{ID: "x", Source: corpus.SourceWikipedia, Title: "X", Intro: "Вступление.",
		Sections: []corpus.Section{{Path: []string{"Сплошной"}, Title: "Сплошной", Level: 2, Text: long}}}
	cs := NewStructure(1200, 200).Split(d)
	var parts []Chunk
	for _, c := range cs {
		if c.Section == "Сплошной" {
			parts = append(parts, c)
		}
	}
	if len(parts) != 3 {
		t.Fatalf("частей %d, ждали 3", len(parts))
	}
	total := 0
	for _, c := range parts {
		n := c.End - c.Start
		total += n
		if n > 1200 || n < 990 || n > 1010 {
			t.Errorf("%s: %d рун — не поровну", c.ID, n)
		}
	}
	if total != 3000 || strings.Join([]string{parts[0].Text, parts[1].Text, parts[2].Text}, "") != long {
		t.Fatalf("текст потерян: %d рун", total)
	}
	// С редкими пробелами — тоже поровну и ≤ Max.
	spaced := strings.Repeat(strings.Repeat("ж", 140)+" ", 20) // 2820 рун
	d.Sections[0].Text = strings.TrimSpace(spaced)
	for _, c := range NewStructure(1200, 200).Split(d) {
		if c.Section == "Сплошной" && (c.End-c.Start > 1200 || c.End-c.Start < 800) {
			t.Errorf("%s: %d рун", c.ID, c.End-c.Start)
		}
	}
}

// TestFixedSectionMajority — раздел чанка fixed — тот, где лежит большая
// часть его текста, а не тот, где окно началось: на настоящем корпусе у
// заметной доли окон это разные разделы, и путь в EmbedText должен быть
// путём большинства.
func TestFixedSectionMajority(t *testing.T) {
	differ, total := 0, 0
	for _, d := range realDocs(t) {
		l := newLayout(d)
		for _, c := range NewFixed(0, -1).Split(d) {
			total++
			b := l.blocks[l.majority(c.Start, c.End)]
			// Доля текста чанка на территории его раздела — не меньше,
			// чем у любого другого блока.
			own, most := 0, 0
			for i, x := range l.blocks {
				hi := len(l.text)
				if i+1 < len(l.blocks) {
					hi = l.blocks[i+1].head
				}
				n := max(0, min(hi, c.End)-max(x.head, c.Start))
				if x.head == b.head {
					own = n
				}
				most = max(most, n)
			}
			if own != most {
				t.Fatalf("%s: у раздела %q %d рун чанка, а у другого — %d", c.ID, b.section(), own, most)
			}
			if c.Section != b.section() || strings.Join(c.Path, sep) != strings.Join(b.path, sep) {
				t.Fatalf("%s: раздел %q, ждали %q", c.ID, c.Section, b.section())
			}
			if l.at(c.Start) != l.majority(c.Start, c.End) {
				differ++
			}
		}
	}
	if differ == 0 {
		t.Fatalf("ни у одного из %d окон раздел начала не отличается от раздела большинства — тест ничего не проверяет", total)
	}
	t.Logf("окон, где раздел начала ≠ раздел большинства: %d из %d", differ, total)

	// Мини-случай: окно задело хвост вступления и ушло в «Описание».
	d := miniDocs()[0]
	l := newLayout(d)
	s := l.blocks[1].head - 5
	if got := l.blocks[l.majority(s, s+100)].section(); got != "Описание" {
		t.Fatalf("раздел большинства %q", got)
	}
	if got := l.blocks[l.at(s)].section(); got != corpus.IntroTitle {
		t.Fatalf("раздел начала %q", got)
	}
}
