package card

import "testing"

// Ключами о животном ведает карточка (трекер вызовов), а не реплики:
// память и карточка фактов их не пишут (ИП-4, ИП-14). Сравнение — без
// регистра, краевых пробелов и «ё», с уточнением после пробела.
func TestReservedKeys(t *testing.T) {
	for _, k := range []string{"латынь", " Ареал ", "ПИТАНИЕ", "статус охраны", "вес взрослой особи", "размеры", "среда обитания"} {
		if !Reserved(k) {
			t.Errorf("%q не зарезервирован", k)
		}
	}
	for _, k := range []string{"имя", "город", "интересы", "ареалы", "весна", "видео", ""} {
		if Reserved(k) {
			t.Errorf("%q зарезервирован по ошибке", k)
		}
	}
}

// Раздел ставится на своё место; незнакомый ключ дописывается в конец.
func TestSetSectionReplacesOrAppends(t *testing.T) {
	c := Card{Sections: EmptySections()}
	n := len(c.Sections)
	c.setSection(Section{Key: "diet", Status: SectionRead, Text: "зайцы"})
	if len(c.Sections) != n || c.Section("diet").Text != "зайцы" {
		t.Fatalf("замена раздела: %+v", c.Sections)
	}
	c.setSection(Section{Key: "extra", Status: SectionRead})
	if len(c.Sections) != n+1 || c.Section("extra") == nil {
		t.Fatal("новый раздел не дописан")
	}
	if IDOf(2435240) != "2435240" {
		t.Fatal("IDOf")
	}
}
