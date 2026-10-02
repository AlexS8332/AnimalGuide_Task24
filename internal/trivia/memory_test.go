package trivia_test

import (
	"testing"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/trivia"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/trivia/triviatest"
)

func TestMemoryConformance(t *testing.T) {
	triviatest.PickStoreConformance(t, func(t *testing.T) trivia.PickStore { return trivia.NewMemory() })
}

func TestMemoryIssueConformance(t *testing.T) {
	triviatest.IssueStoreConformance(t, func(t *testing.T) interface {
		trivia.IssueStore
		trivia.PickStore
	} {
		return trivia.NewMemory()
	})
}
