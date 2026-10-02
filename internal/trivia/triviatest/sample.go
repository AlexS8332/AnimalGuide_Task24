// Package triviatest — общие помощники тестов trivia: проверки контракта
// PickStore, которые обязана проходить каждая реализация, и правдоподобные
// выборы и проверки для чужих тестов.
package triviatest

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/trivia"
)

// sampleNames — латинские названия для образцов: по id вида берётся одно из
// них, чтобы выборы в тестах не были безликими «Species 1».
var sampleNames = []struct{ sci, iucn string }{
	{"Otocolobus manul", "LC"},
	{"Ailurus fulgens", "EN"},
	{"Panthera uncia", "VU"},
	{"Diceros bicornis", "CR"},
	{"Pteropus rodricensis", "EN"},
	{"Castor fiber", "LC"},
	{"Ursus maritimus", "VU"},
}

func sampleName(speciesID int) (sci, iucn string) {
	i := speciesID % len(sampleNames)
	if i < 0 {
		i = -i
	}
	return sampleNames[i].sci, sampleNames[i].iucn
}

// SampleCheck — проверка вида на момент at. ok=true — есть статья и много
// наблюдений; ok=false — статья есть, но наблюдений мало (ReasonFewRecords).
// Заполнены все поля Eligibility, чтобы потеря любого была заметна.
func SampleCheck(speciesID int, at time.Time, ok bool) trivia.Eligibility {
	sci, _ := sampleName(speciesID)
	title := strings.ReplaceAll(sci, " ", "_")
	e := trivia.Eligibility{
		SpeciesID: speciesID,
		SciName:   sci,
		CheckedAt: at,
		OK:        ok,
		WikiLang:  "ru",
		WikiTitle: sci,
		WikiURL:   "https://ru.wikipedia.org/wiki/" + url.PathEscape(title),
		EnTitle:   sci,
		GBIFKey:   2400000 + speciesID,
	}
	if ok {
		e.Occurrences = 1200 + speciesID
	} else {
		e.Reason = trivia.ReasonFewRecords
		e.Occurrences = 12
	}
	return e
}

// SamplePick — выбор вида speciesID в момент at: два отвергнутых
// кандидата, пригодная проверка минутой раньше и вероятности статусов с
// суммой 1. ID нулевой — его присваивает SavePick.
func SamplePick(speciesID int, at time.Time) trivia.Pick {
	sci, iucn := sampleName(speciesID)
	rej1, _ := sampleName(speciesID + 1)
	rej2, _ := sampleName(speciesID + 2)
	return trivia.Pick{
		SpeciesID: speciesID,
		SciName:   sci,
		IUCN:      iucn,
		PickedAt:  at,
		Attempts:  3,
		Rejected: []trivia.Rejection{
			{SpeciesID: speciesID + 100000, SciName: rej1, Reason: trivia.ReasonFewRecords,
				Detail: "12 наблюдений < 50"},
			{SpeciesID: speciesID + 200000, SciName: rej2, Reason: trivia.ReasonNoArticle},
		},
		Eligibility: SampleCheck(speciesID, at.Add(-time.Minute), true),
		Weights: trivia.WeightsReport{
			"LC": 0.5, "NT": 0.125, "VU": 0.125, "EN": 0.125, "CR": 0.0625, "DD": 0.0625,
		},
	}
}

// describePick — выбор одной строкой для сообщений об ошибке.
func describePick(p trivia.Pick) string {
	return fmt.Sprintf("{ID:%d Species:%d At:%s}", p.ID, p.SpeciesID, p.PickedAt.Format(time.RFC3339Nano))
}
