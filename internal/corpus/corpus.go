// Package corpus — замороженный корпус базы знаний: документы, их манифест
// и загрузка снимка.
//
// Корпус — источник истины для индекса. Он лежит в репозитории (каталог
// corpus/), а не скачивается на каждом прогоне: TextExtracts Википедии
// всегда отдаёт текущую ревизию, и без снимка повторный прогон шёл бы по
// другим текстам. Обновляет снимок только явная команда `kb fetch`.
//
// Формат на диске:
//
//	corpus/<doc_id>.json   один документ (Doc), schema = Schema
//	corpus/MANIFEST.json   список документов, sha256 каждого файла, corpus_sha
//	corpus/LICENSE.md      лицензия текстов (CC BY-SA 4.0 для Википедии,
//	                       CC BY 4.0 для MDD) и атрибуция со ссылками на oldid
//
// Load сверяет sha256 каждого файла с манифестом: изменённый руками или
// недокачанный документ — ошибка, а не тихо другой корпус.
package corpus

import (
	"context"
	"errors"
	"net/http"
	"strings"
)

// Schema — версия формата документа и манифеста.
const Schema = 1

// PageChars — сколько символов считается «страницей» в отчётах (машинописная
// страница ≈ 1800 знаков).
const PageChars = 1800

// Источники документов.
const (
	SourceWikipedia = "wikipedia-ru"
	SourceMDD       = "mdd"
)

// ErrNotImplemented — заглушка контракта; реализация приходит в своей ветке.
var ErrNotImplemented = errors.New("не реализовано")

// Section — раздел документа. Text — СОБСТВЕННОЕ тело раздела, без текстов
// подразделов: у tools.Wikipedia текст раздела второго уровня включает
// подразделы (splitSections), здесь так нельзя — чанкер получил бы один и
// тот же текст дважды. Раздел, у которого собственного текста нет (только
// подразделы), остаётся с пустым Text: он нужен для пути.
type Section struct {
	// Path — путь заголовков от верхнего уровня: ["Образ жизни", "Питание"].
	Path  []string `json:"path"`
	Title string   `json:"title"`
	// Level — уровень заголовка в разметке «== … ==»: 2, 3, 4.
	Level int    `json:"level"`
	Text  string `json:"text"`
}

// Species — о каком виде документ (у обзорных статей и MDD — nil).
type Species struct {
	Latin   string   `json:"latin,omitempty"`
	Ru      string   `json:"ru,omitempty"`
	Aliases []string `json:"aliases,omitempty"`
}

// Doc — документ корпуса.
type Doc struct {
	Schema int `json:"schema"`
	// ID — slug, он же имя файла: "manul", "snow-leopard", "mdd-carnivora".
	ID     string `json:"doc_id"`
	Source string `json:"source"`
	Title  string `json:"title"`
	URL    string `json:"url"`
	// RevID — ревизия статьи Википедии на момент снимка; OldURL — постоянная
	// ссылка на неё (…/w/index.php?oldid=RevID).
	RevID   int64    `json:"revid,omitempty"`
	OldURL  string   `json:"oldid_url,omitempty"`
	Fetched string   `json:"fetched_at"`
	License string   `json:"license"`
	Species *Species `json:"species,omitempty"`
	// Intro — вступление до первого заголовка.
	Intro    string    `json:"intro"`
	Sections []Section `json:"sections"`
}

// IntroTitle — имя «раздела» вступления в метаданных чанков.
const IntroTitle = "Вступление"

// Text — канонический плоский текст документа: вступление, затем каждый
// раздел строкой заголовка «## Путь › Раздел» и собственным телом, блоки
// через пустую строку. Смещения чанков (Chunk.Start/End) и поиск
// фрагментов-доказательств контрольных вопросов считаются по этому тексту.
// Меняется формат — меняются все смещения, поэтому формат не трогать.
func (d Doc) Text() string {
	var b strings.Builder
	b.WriteString(d.Intro)
	for _, s := range d.Sections {
		b.WriteString("\n\n## ")
		b.WriteString(strings.Join(s.Path, " › "))
		if s.Text != "" {
			b.WriteString("\n")
			b.WriteString(s.Text)
		}
	}
	return b.String()
}

// Chars — длина канонического текста в символах (рунах).
func (d Doc) Chars() int { return len([]rune(d.Text())) }

// Entry — строка манифеста.
type Entry struct {
	ID     string `json:"doc_id"`
	File   string `json:"file"`
	Title  string `json:"title"`
	Source string `json:"source"`
	RevID  int64  `json:"revid,omitempty"`
	Chars  int    `json:"chars"`
	SHA256 string `json:"sha256"`
}

// Manifest — оглавление корпуса. CorpusSHA — sha256 канонического JSON
// списка Entries (по doc_id): одно число на весь корпус, его печатает
// каждый отчёт.
type Manifest struct {
	Schema    int     `json:"schema"`
	Entries   []Entry `json:"entries"`
	Chars     int     `json:"chars"`
	Pages     float64 `json:"pages"`
	CorpusSHA string  `json:"corpus_sha"`
}

// Load читает снимок из каталога и сверяет его с манифестом: каждый файл
// манифеста на месте и с тем же sha256, лишних документов нет, corpus_sha
// пересчитывается. Документы — в порядке манифеста.
func Load(dir string) ([]Doc, Manifest, error) { return load(dir) }

// Save записывает документы и манифест (атомарно, UTF-8, отступ в два
// пробела) и возвращает новый манифест. LICENSE.md пишет WriteLicense.
func Save(dir string, docs []Doc) (Manifest, error) { return save(dir, docs) }

// WriteLicense пишет corpus/LICENSE.md: лицензии и атрибуцию каждого
// документа со ссылкой на его ревизию.
func WriteLicense(dir string, docs []Doc) error { return writeLicense(dir, docs) }

// Find ищет фрагмент в каноническом тексте после нормализации (регистр,
// ё→е, кавычки «»“”„ → ", тире —– → -, пробельные последовательности → один
// пробел). Возвращает смещение и длину в рунах ИСХОДНОГО текста; -1, если
// фрагмента нет. Этой же нормализацией пользуются проверка разметки
// вопросов и (в v24) сверка цитат.
func Find(text, fragment string) (start, length int) { return find(text, fragment) }

// Normalize — нормализация, которой пользуется Find.
func Normalize(s string) string { return normalize(s) }

// Fetcher — снятие снимка статей ru-Википедии: TextExtracts (explaintext,
// exsectionformat=wiki) и ревизия (prop=revisions|info) одним запросом,
// перенаправления раскрываются. Служебные разделы (Примечания, Литература,
// Ссылки, См. также, Источники) отбрасываются.
type Fetcher struct {
	// Base — адрес Википедии; пусто — tools.DefaultWikipediaBase.
	Base string
	HTTP *http.Client
	// UserAgent — по правилам Wikimedia у бота должен быть свой.
	UserAgent string
}

// Wikipedia снимает одну статью. id — slug документа, species — сведения о
// виде из списка (может быть nil).
func (f Fetcher) Wikipedia(ctx context.Context, id, title string, species *Species) (Doc, error) {
	return f.wikipedia(ctx, id, title, species)
}
