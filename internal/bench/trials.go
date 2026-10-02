package bench

import (
	"fmt"
	"strings"
)

// Trials — все испытания регрессионного набора по порядку ТЗ. Набор
// гоняется целиком перед каждым следующим упражнением (шаг 8 регламента).
func Trials() []Trial {
	return []Trial{NewFacts(), NewMemory(), NewProfile(), NewCollection(), NewInvariants(), NewCost(), NewMCP(), NewTrivia(), NewKB(), NewRAG(), NewRerank(), NewCite()}
}

// Select выбирает испытания по строке флага: «all», «1,6», «И-1,И-6»,
// «i2». Неизвестный номер — ошибка: опечатка иначе молча дала бы не тот
// прогон.
func Select(all []Trial, spec string) ([]Trial, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" || strings.EqualFold(spec, "all") {
		return all, nil
	}
	byNum := map[string]Trial{}
	var known []string
	for _, t := range all {
		byNum[number(t.ID())] = t
		known = append(known, t.ID())
	}
	var out []Trial
	seen := map[string]bool{}
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		n := number(part)
		t, ok := byNum[n]
		if !ok {
			return nil, fmt.Errorf("неизвестное испытание %q; известны: %s", part, strings.Join(known, ", "))
		}
		if !seen[n] {
			seen[n] = true
			out = append(out, t)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("не выбрано ни одного испытания: %q", spec)
	}
	return out, nil
}

// number — номер испытания из «И-1», «и1», «i-1», «1».
func number(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}
