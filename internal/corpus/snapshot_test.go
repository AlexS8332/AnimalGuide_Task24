package corpus

import (
	"strings"
	"testing"
)

// TestRepositorySnapshot — снимок в репозитории цел и годится в индекс:
// манифест сходится (Load), объём не меньше 30 страниц, у каждой статьи
// Википедии есть ревизия, служебных разделов и пустых листьев нет, пути
// разделов не повторяются, абзацы внутри документа — тоже (дубль абзаца —
// признак того, что текст подраздела попал и в родителя).
func TestRepositorySnapshot(t *testing.T) {
	docs, m, err := Load("../../corpus")
	if err != nil {
		t.Fatal(err)
	}
	if m.Pages < 30 {
		t.Errorf("корпус %.1f страниц, нужно ≥ 30", m.Pages)
	}
	mddSeen := false
	for _, d := range docs {
		switch d.Source {
		case SourceWikipedia:
			if d.RevID == 0 || !strings.HasSuffix(d.OldURL, "oldid="+itoa(d.RevID)) || d.License != LicenseWikipedia {
				t.Errorf("%s: ревизия %d, %s, %s", d.ID, d.RevID, d.OldURL, d.License)
			}
		case SourceMDD:
			mddSeen = true
		default:
			t.Errorf("%s: источник %q", d.ID, d.Source)
		}
		if d.Intro == "" {
			t.Errorf("%s: нет вступления", d.ID)
		}
		// Повторы абзацев ≥ 60 символов внутри документа (короткие строки
		// вроде «Статус МСОП: …» в MDD повторяются законно).
		paras := map[string]string{}
		for _, b := range append([]Section{{Path: []string{IntroTitle}, Text: d.Intro}}, d.Sections...) {
			for _, para := range strings.Split(b.Text, "\n") {
				para = strings.TrimSpace(para)
				if len([]rune(para)) < 60 {
					continue
				}
				where := strings.Join(b.Path, " › ")
				if prev, ok := paras[para]; ok {
					t.Errorf("%s: абзац повторяется в «%s» и «%s»: %.80s…", d.ID, prev, where, para)
				}
				paras[para] = where
			}
		}
		seen := map[string]bool{}
		for i, s := range d.Sections {
			p := strings.Join(s.Path, " › ")
			if seen[p] {
				t.Errorf("%s: путь %q повторяется", d.ID, p)
			}
			seen[p] = true
			for _, t2 := range s.Path {
				if serviceSections[normalizeTitle(t2)] {
					t.Errorf("%s: служебный раздел %q", d.ID, p)
				}
			}
			if s.Text == "" && (i+1 == len(d.Sections) || d.Sections[i+1].Level <= s.Level) {
				t.Errorf("%s: пустой раздел без подразделов %q", d.ID, p)
			}
		}
	}
	if !mddSeen {
		t.Error("в корпусе нет документа MDD")
	}
}

func itoa(n int64) string {
	var b [20]byte
	i := len(b)
	for {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
		if n == 0 {
			return string(b[i:])
		}
	}
}
