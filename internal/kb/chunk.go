package kb

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"sort"
	"strings"
	"unicode"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/corpus"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/tokens"
)

// Чанкеры. Обе стратегии работают по одной раскладке документа (layout):
// канонический текст corpus.Doc.Text() в рунах и для каждого блока
// (вступление, разделы) — где стоит его строка заголовка и где лежит
// собственное тело. Смещения считаются по той же схеме, что и Doc.Text,
// поэтому Text[Start:End] чанка — ровно Chunk.Text, а фрагменты-
// доказательства вопросов (corpus.Find по Doc.Text) сравниваются с чанками
// по смещениям, без поиска подстрок.

// sep — разделитель элементов пути: тот же, что в строке заголовка Doc.Text.
const sep = " › "

// EmbedText — «Заголовок › Путь…», перевод строки и текст. У вступления
// путь пустой — только заголовок статьи.
func (c Chunk) EmbedText() string {
	head := c.Title
	if len(c.Path) > 0 {
		head += sep + strings.Join(c.Path, sep)
	}
	return head + "\n" + c.Text
}

// block — вступление или раздел документа в рунах Doc.Text.
type block struct {
	// idx — номер раздела в Doc.Sections; -1 — вступление.
	idx  int
	path []string
	// head — где начинается строка «## …» (у вступления 0). Блоку
	// принадлежит «территория» [head, head следующего блока).
	head int
	// start, end — собственное тело; start == end — тела нет.
	start, end int
}

// parent — путь родителя: соседи «того же родителя» склеиваются.
func (b block) parent() string {
	if len(b.path) == 0 {
		return ""
	}
	return strings.Join(b.path[:len(b.path)-1], sep)
}

func (b block) section() string {
	if len(b.path) == 0 {
		return corpus.IntroTitle
	}
	return b.path[len(b.path)-1]
}

// layout — текст документа и его блоки. Повторяет Doc.Text построчно;
// расхождение схем ловит тест (TestLayoutMatchesText).
type layout struct {
	text   []rune
	blocks []block
}

func newLayout(d corpus.Doc) layout {
	l := layout{text: []rune(d.Text())}
	pos := runeLen(d.Intro)
	l.blocks = append(l.blocks, block{idx: -1, head: 0, start: 0, end: pos})
	for i, s := range d.Sections {
		pos += 2 // "\n\n"
		b := block{idx: i, path: s.Path, head: pos}
		pos += 3 + runeLen(strings.Join(s.Path, sep)) // "## " + путь
		if s.Text != "" {
			pos++ // "\n"
			b.start = pos
			pos += runeLen(s.Text)
			b.end = pos
		} else {
			b.start, b.end = pos, pos
		}
		l.blocks = append(l.blocks, b)
	}
	return l
}

// at — номер блока, на территории которого лежит смещение.
func (l layout) at(pos int) int {
	i := 0
	for j, b := range l.blocks {
		if b.head <= pos {
			i = j
		} else {
			break
		}
	}
	return i
}

// majority — блок, которому принадлежит большая часть отрезка [s, e)
// (по территориям блоков: строка заголовка считается текстом своего
// раздела); при равенстве — более ранний.
func (l layout) majority(s, e int) int {
	best, bestLen := l.at(s), -1
	for i, b := range l.blocks {
		hi := len(l.text)
		if i+1 < len(l.blocks) {
			hi = l.blocks[i+1].head
		}
		lo := max(b.head, s)
		if n := min(hi, e) - lo; n > bestLen {
			best, bestLen = i, n
		}
	}
	return best
}

// mixed — пересекает ли отрезок строку заголовка другого раздела. Заголовок
// в самом начале отрезка не считается: это начало раздела, а не граница
// внутри чанка.
func (l layout) mixed(s, e int) bool {
	for _, b := range l.blocks[1:] {
		if b.head > s && b.head < e {
			return true
		}
	}
	return false
}

// trim сужает отрезок до непробельных рун по краям.
func (l layout) trim(s, e int) (int, int) {
	for s < e && unicode.IsSpace(l.text[s]) {
		s++
	}
	for e > s && unicode.IsSpace(l.text[e-1]) {
		e--
	}
	return s, e
}

func runeLen(s string) int { return len([]rune(s)) }

// span — отрезок текста [s, e) и блок, к которому он относится (у склейки
// соседей — тот, чьего текста больше); first, last — блоки начала и конца.
type span struct {
	s, e        int
	block       int
	first, last int
}

func (p span) len() int { return p.e - p.s }

// chunk собирает Chunk по отрезку: метаданные документа, раздел — блок b.
func (l layout) chunk(d corpus.Doc, st Strategy, ord int, s, e, b int) Chunk {
	text := string(l.text[s:e])
	blk := l.blocks[b]
	sum := sha256.Sum256([]byte(text))
	c := Chunk{
		ID:       fmt.Sprintf("%s/%s/%03d", d.ID, st, ord),
		DocID:    d.ID,
		Source:   d.Source,
		Title:    d.Title,
		Section:  blk.section(),
		Path:     append([]string(nil), blk.path...),
		Strategy: st,
		Ord:      ord,
		Start:    s,
		End:      e,
		Text:     text,
		Mixed:    l.mixed(s, e),
		Tokens:   int(math.Ceil(tokens.Default.Text(text))),
		SHA:      hex.EncodeToString(sum[:]),
		URL:      d.URL,
		RevID:    d.RevID,
	}
	if c.Path == nil {
		c.Path = []string{}
	}
	return c
}

// ---------------------------------------------------------------- structure

type structure struct{ max, min int }

// NewStructure — граница по заголовкам; max, min в рунах (0 → DefaultMax,
// DefaultMin).
func NewStructure(max, min int) Chunker {
	if max <= 0 {
		max = DefaultMax
	}
	if min <= 0 {
		min = DefaultMin
	}
	if min > max/2 {
		// Иначе склейка двух коротких дала бы чанк длиннее предела.
		min = max / 2
	}
	return structure{max: max, min: min}
}

func (c structure) Strategy() Strategy { return Structure }
func (c structure) Params() Params     { return Params{Max: c.max, Min: c.min} }

// Split: собственное тело каждого блока режется на куски не длиннее max
// (по абзацам, длинный абзац — по предложениям, совсем длинное
// предложение — по пробелам), затем короткие куски (< min) склеиваются с
// соседом. Строка «## …» в чанк не входит: путь раздела попадает в
// EmbedText, а в тексте чанка он был бы шумом и вторым экземпляром
// заголовка.
func (c structure) Split(d corpus.Doc) []Chunk {
	l := newLayout(d)
	var spans []span
	for bi, b := range l.blocks {
		s, e := l.trim(b.start, b.end)
		if s >= e {
			continue
		}
		for _, p := range c.pack(l, s, e) {
			spans = append(spans, span{s: p.s, e: p.e, block: bi, first: bi, last: bi})
		}
	}
	spans = c.merge(l, spans)
	out := make([]Chunk, 0, len(spans))
	for i, p := range spans {
		out = append(out, l.chunk(d, Structure, i, p.s, p.e, p.block))
	}
	return out
}

// pack режет тело [s, e) на куски не длиннее max. Число кусков — минимально
// возможное (ceil(длина/max)), а целевая длина — поровну: жадная упаковка
// «до max» оставляла бы в хвосте обрубок в пару строк.
func (c structure) pack(l layout, s, e int) []span {
	if e-s <= c.max {
		return []span{{s: s, e: e}}
	}
	var units []span
	for _, para := range paragraphs(l, s, e) {
		if para.len() <= c.max {
			units = append(units, para)
			continue
		}
		for _, sent := range sentences(l, para.s, para.e) {
			if sent.len() <= c.max {
				units = append(units, sent)
				continue
			}
			units = append(units, words(l, sent.s, sent.e, c.max)...)
		}
	}
	n := (e - s + c.max - 1) / c.max
	target := (e - s) / n
	var out []span
	cur := span{s: -1}
	for _, u := range units {
		if cur.s < 0 {
			cur = span{s: u.s, e: u.e}
			continue
		}
		grown := u.e - cur.s
		switch {
		case grown <= target,
			// Чуть за целью — берём, если так ближе к ней, чем без.
			grown <= c.max && grown-target < target-cur.len():
			cur.e = u.e
		default:
			out = append(out, cur)
			cur = span{s: u.s, e: u.e}
		}
	}
	if cur.s >= 0 {
		out = append(out, cur)
	}
	return out
}

// merge склеивает короткие куски (< min) с соседом: внутри одного блока —
// всегда, между блоками — только если это соседние разделы одного
// родителя (вступление ни с чем не склеивается). Из двух соседей берётся
// тот, с кем вместе короче; предел склейки — max + min, чтобы короткий
// хвост не оставался одиноким из-за пары символов.
func (c structure) merge(l layout, spans []span) []span {
	can := func(a, b span) bool { // a перед b
		if a.last == b.first {
			return true
		}
		ba, bb := l.blocks[a.last], l.blocks[b.first]
		if ba.idx < 0 || bb.idx != ba.idx+1 || len(ba.path) != len(bb.path) || ba.parent() != bb.parent() {
			return false
		}
		// Раздел за заголовком должен войти в склейку целиком: иначе
		// заголовок окажется посреди чанка, а продолжение раздела — в
		// другом чанке.
		_, end := l.trim(bb.start, bb.end)
		return b.e >= end
	}
	for {
		best, with := -1, -1
		bestLen := math.MaxInt
		for i, p := range spans {
			if p.len() >= c.min {
				continue
			}
			for _, j := range []int{i - 1, i + 1} {
				if j < 0 || j >= len(spans) {
					continue
				}
				a, b := spans[min(i, j)], spans[max(i, j)]
				if !can(a, b) {
					continue
				}
				if n := b.e - a.s; n <= c.max+c.min && n < bestLen {
					best, with, bestLen = i, j, n
				}
			}
			if best >= 0 {
				break // по одному за проход: соседи меняются
			}
		}
		if best < 0 {
			return spans
		}
		i, j := min(best, with), max(best, with)
		a, b := spans[i], spans[j]
		m := span{s: a.s, e: b.e, block: a.block, first: a.first, last: b.last}
		if b.len() > a.len() {
			// Раздел склейки — тот, чьего текста в чанке больше.
			m.block = b.block
		}
		spans = append(spans[:i], append([]span{m}, spans[j+1:]...)...)
	}
}

// paragraphs — абзацы отрезка: границы — переводы строк.
func paragraphs(l layout, s, e int) []span {
	var out []span
	start := s
	for i := s; i <= e; i++ {
		if i == e || l.text[i] == '\n' {
			if ps, pe := l.trim(start, i); ps < pe {
				out = append(out, span{s: ps, e: pe})
			}
			start = i + 1
		}
	}
	return out
}

// sentences — предложения: конец — «.», «!», «?», «…» (и закрывающие
// кавычки/скобки за ними), затем пробел и заглавная буква, цифра или
// открывающая кавычка. Сокращения вроде «лат. Felis» дают ложную границу —
// это безвредно: границы нужны только как места, где резать можно.
func sentences(l layout, s, e int) []span {
	var out []span
	start := s
	for i := s; i < e; i++ {
		if !isTerminal(l.text[i]) {
			continue
		}
		j := i + 1
		for j < e && isCloser(l.text[j]) {
			j++
		}
		if j >= e || !unicode.IsSpace(l.text[j]) {
			continue
		}
		k := j
		for k < e && unicode.IsSpace(l.text[k]) {
			k++
		}
		if k < e && (unicode.IsUpper(l.text[k]) || unicode.IsDigit(l.text[k]) || l.text[k] == '«' || l.text[k] == '"') {
			out = append(out, span{s: start, e: j})
			start = k
			i = k - 1
		}
	}
	if ps, pe := l.trim(start, e); ps < pe {
		out = append(out, span{s: ps, e: pe})
	}
	return out
}

// words — последнее средство: куски не длиннее max по пробелам, поровну:
// число кусков — ceil(длина/max), и каждый следующий режется у цели
// «остаток / сколько кусков осталось». Пробел ищется левее цели не дальше
// её половины; нет пробела (абзац без пробелов и точек) — режем ровно по
// цели. Жадная резка «по max» оставляла бы в хвосте обрубок.
func words(l layout, s, e, max int) []span {
	var out []span
	for s < e {
		rest := e - s
		if rest <= max {
			if ps, pe := l.trim(s, e); ps < pe {
				out = append(out, span{s: ps, e: pe})
			}
			break
		}
		n := (rest + max - 1) / max
		end := s + (rest+n-1)/n // ceil(rest/n) ≤ max
		cut := end
		for cut > s+(end-s)/2 && !unicode.IsSpace(l.text[cut]) {
			cut--
		}
		if !unicode.IsSpace(l.text[cut]) {
			cut = end // пробела рядом нет — режем как есть
		}
		ps, pe := l.trim(s, cut)
		if ps < pe {
			out = append(out, span{s: ps, e: pe})
		}
		s = cut
		for s < e && unicode.IsSpace(l.text[s]) {
			s++
		}
	}
	return out
}

func isTerminal(r rune) bool { return r == '.' || r == '!' || r == '?' || r == '…' }
func isCloser(r rune) bool   { return r == '»' || r == '"' || r == ')' || r == '”' }

// -------------------------------------------------------------------- fixed

type fixed struct{ size, overlap int }

// NewFixed — окно size рун с перекрытием overlap. size ≤ 0 → DefaultSize;
// overlap < 0 → DefaultOverlapPct % размера, 0 — без перекрытия (окна
// встык).
func NewFixed(size, overlap int) Chunker {
	if size <= 0 {
		size = DefaultSize
	}
	if overlap < 0 {
		overlap = size * DefaultOverlapPct / 100
	}
	if overlap >= size/2 {
		overlap = size / 2
	}
	return fixed{size: size, overlap: overlap}
}

func (c fixed) Strategy() Strategy { return Fixed }
func (c fixed) Params() Params     { return Params{Size: c.size, Overlap: c.overlap} }

// Split ведёт окно по сплошному тексту документа, не глядя на разделы.
// Границы окна сдвигаются к ближайшему пробелу (не дальше десятой части
// размера), чтобы не рвать слово; следующее окно начинается за overlap до
// конца предыдущего (без перекрытия — ровно с конца). Раздел чанка — тот,
// которому принадлежит большая часть его текста: раздел «где начался» у
// окна, задевшего хвост предыдущего раздела, был бы чужим, а путь раздела
// уходит в EmbedText и FTS. Mixed — окно пересекло строку «## …».
func (c fixed) Split(d corpus.Doc) []Chunk {
	l := newLayout(d)
	n := len(l.text)
	reach := max(c.size/10, 1)
	var out []Chunk
	start := skipSpace(l.text, 0)
	for start < n {
		end := start + c.size
		if end >= n {
			end = n
		} else {
			end = nearestSpace(l.text, end, reach, start+1)
		}
		s, e := l.trim(start, end)
		if s < e {
			out = append(out, l.chunk(d, Fixed, len(out), s, e, l.majority(s, e)))
		}
		if end >= n {
			break
		}
		next := end
		if c.overlap > 0 {
			next = nearestSpace(l.text, end-c.overlap, reach, start+1)
		}
		if next <= start {
			next = end
		}
		start = skipSpace(l.text, next)
	}
	return out
}

// nearestSpace — ближайшая к pos позиция пробела в пределах reach (но не
// левее floor); пробела нет — pos как есть.
func nearestSpace(t []rune, pos, reach, floor int) int {
	for d := 0; d <= reach; d++ {
		for _, p := range []int{pos - d, pos + d} {
			if p >= floor && p < len(t) && unicode.IsSpace(t[p]) {
				return p
			}
		}
	}
	return pos
}

func skipSpace(t []rune, p int) int {
	for p < len(t) && unicode.IsSpace(t[p]) {
		p++
	}
	return p
}

// MedianChars — медиана длины чанков в рунах: из неё kb index -strategy all
// и И-9 берут размер окна fixed («сравниваются границы, а не размер»).
func MedianChars(chunks []Chunk) int {
	lens := make([]int, 0, len(chunks))
	for _, c := range chunks {
		lens = append(lens, c.End-c.Start)
	}
	return median(lens)
}

func median(xs []int) int {
	if len(xs) == 0 {
		return 0
	}
	s := append([]int(nil), xs...)
	sort.Ints(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

// ForeignHeadings — чанки документа, захватившие строку заголовка «## …»
// чужого раздела: чанк начинается с заголовка или раздел за заголовком
// продолжается за концом чанка (значит, его текст попал и в другой чанк).
// Склейка короткого раздела с соседом сюда не попадает: тело соседа
// целиком внутри чанка и больше нигде не лежит. У structure список обязан
// быть пустым; у fixed не пуст почти всегда — окно режет по чему придётся.
func ForeignHeadings(d corpus.Doc, cs []Chunk) []string {
	l := newLayout(d)
	var bad []string
	for _, c := range cs {
		if c.DocID != d.ID {
			continue
		}
		for _, b := range l.blocks[1:] {
			if b.head >= c.Start && b.head < c.End && (b.head == c.Start || b.end > c.End) {
				bad = append(bad, c.ID)
				break
			}
		}
	}
	return bad
}
