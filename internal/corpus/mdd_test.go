package corpus

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/mdd"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/mdd/mddtest"
)

func sampleStore(t *testing.T, edit func(*mdd.Dataset)) mdd.Store {
	t.Helper()
	d := mddtest.Sample()
	if edit != nil {
		edit(d)
	}
	st := mdd.NewMemory()
	if err := st.Replace(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	return st
}

func TestMDDDoc(t *testing.T) {
	ctx := context.Background()
	d, err := MDDDoc(ctx, sampleStore(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	if d.ID != MDDDocID || d.Source != SourceMDD || d.License != LicenseMDD || d.URL != "https://www.mammaldiversity.org" || d.Schema != Schema {
		t.Fatalf("поля: %+v", d)
	}
	if d.RevID != 0 || d.Fetched != "2026-09-01T12:00:00Z" {
		t.Errorf("revid %d, fetched %s", d.RevID, d.Fetched)
	}
	// Мини-набор: 11 видов, из них хищных 7 (панда, лиса, кошка, рысь,
	// манул, лев, ирбис) в 6 родах и 3 семействах.
	for _, want := range []string{
		"релизу MDD v2.5 от 2026-07-28 (предыдущий релиз — v2.4)",
		"В релизе MDD v2.5 11 видов млекопитающих",
		"относятся 7 видов из 6 родов в 3 семействах",
		"лицензия CC BY 4.0", "https://www.mammaldiversity.org",
	} {
		if !strings.Contains(d.Intro, want) {
			t.Errorf("во вступлении нет %q:\n%s", want, d.Intro)
		}
	}

	var paths []string
	byTitle := map[string]string{}
	for _, s := range d.Sections {
		paths = append(paths, strings.Join(s.Path, " › "))
		byTitle[s.Title] = s.Text
		if s.Level != 2 || len(s.Path) != 1 || s.Path[0] != s.Title || s.Text == "" {
			t.Errorf("раздел %+v", s)
		}
	}
	// Порядок семейств — как в списке Евразии; семейства без видов в
	// наборе пропущены.
	want := []string{"Семейства хищных", "Кошачьи (Felidae)", "Псовые (Canidae)", "Медвежьи (Ursidae)", "Изменения релиза в отряде"}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("разделы: %q", paths)
	}

	sum := byTitle["Семейства хищных"]
	for _, w := range []string{"Медвежьи (Ursidae) — 1 вид, 1 род.", "Псовые (Canidae) — 1 вид, 1 род.", "Кошачьи (Felidae) — 5 видов, 4 рода."} {
		if !strings.Contains(sum, w) {
			t.Errorf("в сводке нет %q:\n%s", w, sum)
		}
	}
	cats := byTitle["Кошачьи (Felidae)"]
	for _, w := range []string{
		"Семейство Кошачьи (Felidae) по MDD v2.5: 5 видов в 4 родах.",
		"Род Panthera — 2 вида:",
		"Otocolobus manul (Pallas, 1776) — англ. Pallas's Cat; МСОП: LC (вызывающий наименьшие опасения).",
		"Panthera uncia (Boddaert, 1772) — англ. Snow Leopard; МСОП: VU (уязвимый).",
		"Felis catus Linnaeus, 1758 — англ. Domestic Cat; МСОП: NE (не оценивался); домашний вид.",
	} {
		if !strings.Contains(cats, w) {
			t.Errorf("в разделе кошачьих нет %q:\n%s", w, cats)
		}
	}
	if strings.Contains(d.Text(), "Ornithorhynchus") || strings.Contains(d.Text(), "Diceros") {
		t.Error("в документ попали не хищные")
	}
	ch := byTitle["Изменения релиза в отряде"]
	if !strings.Contains(ch, "В релизе v2.5 изменений в отряде нет") {
		t.Errorf("изменения: %s", ch)
	}

	// Документ детерминирован: тот же справочник — тот же текст.
	d2, err := MDDDoc(ctx, sampleStore(t, nil))
	if err != nil || !reflect.DeepEqual(d, d2) {
		t.Fatal("MDDDoc не детерминирован")
	}
}

func TestMDDDocChangesAndExtinct(t *testing.T) {
	st := sampleStore(t, func(d *mdd.Dataset) {
		d.Changes = append(d.Changes,
			mdd.Change{OldName: "Felis silvestris", NewName: "Felis lybica", Comment: "split", Category: "split", Reference: "Автор 2026"},
			mdd.Change{NewName: "Panthera nova", Category: "de novo"},
		)
		for i := range d.Species {
			if d.Species[i].SciName == "Panthera leo" {
				d.Species[i].Extinct = true
			}
		}
	})
	d, err := MDDDoc(context.Background(), st)
	if err != nil {
		t.Fatal(err)
	}
	ch := d.Sections[len(d.Sections)-1]
	if ch.Title != "Изменения релиза в отряде" {
		t.Fatalf("последний раздел %q", ch.Title)
	}
	for _, w := range []string{
		"В релизе v2.5 в отряде хищных 2 изменения относительно v2.4:",
		"Felis silvestris → Felis lybica (категория: split; split). Источник: Автор 2026.",
		"Новый вид Panthera nova (категория: de novo).",
	} {
		if !strings.Contains(ch.Text, w) {
			t.Errorf("в изменениях нет %q:\n%s", w, ch.Text)
		}
	}
	if strings.Contains(ch.Text, "Afronycteris") {
		t.Error("изменение рукокрылых попало к хищным")
	}
	if !strings.Contains(d.Text(), "Panthera leo (Linnaeus, 1758) — англ. Lion; МСОП: VU (уязвимый); вымерший вид.") {
		t.Error("нет пометки «вымерший вид»")
	}
}

func TestMDDDocEmptyStore(t *testing.T) {
	if _, err := MDDDoc(context.Background(), mdd.NewMemory()); err == nil {
		t.Fatal("пустой справочник принят")
	}
}

func TestPlural(t *testing.T) {
	cases := map[int]string{1: "1 вид", 2: "2 вида", 5: "5 видов", 11: "11 видов", 12: "12 видов", 21: "21 вид", 22: "22 вида", 111: "111 видов", 0: "0 видов"}
	for n, want := range cases {
		if got := plural(n, "вид", "вида", "видов"); got != want {
			t.Errorf("plural(%d) = %q, ждали %q", n, got, want)
		}
	}
}

func TestMDDDocPagesThroughSearch(t *testing.T) {
	// Search отдаёт не больше 100 видов за раз: 250 видов одного рода
	// должны попасть в документ все.
	st := sampleStore(t, func(d *mdd.Dataset) {
		for i := 0; i < 250; i++ {
			d.Species = append(d.Species, mdd.Species{
				ID: 2000000 + i, Phylosort: 5000 + i, SciName: fmt.Sprintf("Mustela sp%03d", i),
				Order: "Carnivora", Family: "Mustelidae", Genus: "Mustela", Epithet: fmt.Sprintf("sp%03d", i),
			})
		}
	})
	d, err := MDDDoc(context.Background(), st)
	if err != nil {
		t.Fatal(err)
	}
	text := d.Text()
	if !strings.Contains(text, "Куньи (Mustelidae) — 250 видов, 1 род.") || !strings.Contains(text, "Mustela sp249 — статус МСОП не указан.") {
		t.Fatalf("постраничный обход потерял виды:\n%.600s", d.Sections[0].Text)
	}
}
