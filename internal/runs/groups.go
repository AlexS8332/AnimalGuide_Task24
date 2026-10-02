package runs

import (
	"fmt"
	"strings"

	"github.com/AlexS8332/AnimalGuide_Task24/internal/agents"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/features"
	"github.com/AlexS8332/AnimalGuide_Task24/internal/history"
)

// Lane — дорожка стенда (ФТ-47): свой диалог, свой набор механизмов и свой
// собеседник. Собеседник у каждой дорожки отдельный: профиль и
// долговременная память привязаны к человеку, и дорожки с одним человеком
// делили бы их, а разницу пришлось бы придумывать.
type Lane struct {
	Name     string       `json:"name"`
	Features features.Set `json:"features"`
	// Owner — собеседник дорожки; пусто — «<стенд>-<дорожка>».
	Owner string `json:"owner,omitempty"`
	Title string `json:"title,omitempty"`
}

// ErrLanesDiffer — дорожки отличаются больше чем одним механизмом: стенд
// не запускается (ИП-13), иначе разница в ответах имела бы две причины.
var ErrLanesDiffer = fmt.Errorf("дорожки стенда должны отличаться не больше чем одним механизмом")

// StartGroup заводит стенд: по пустому диалогу на дорожку. Первая дорожка —
// основная, с ней сверяются остальные.
func (m *Manager) StartGroup(title string, lanes []Lane) (string, []Detail, error) {
	if len(lanes) == 0 {
		return "", nil, fmt.Errorf("у стенда нет дорожек")
	}
	base := m.cfg.Registry.Complete(lanes[0].Features)
	for _, l := range lanes[1:] {
		if diff := m.cfg.Registry.Diff(base, m.cfg.Registry.Complete(l.Features)); len(diff) > 1 {
			return "", nil, fmt.Errorf("%w: «%s» отличается от «%s» механизмами %v", ErrLanesDiffer, l.Name, lanes[0].Name, diff)
		}
	}
	group := history.NewID()
	var out []Detail
	var ids []string
	for i, l := range lanes {
		owner := strings.TrimSpace(l.Owner)
		if owner == "" {
			// Номер дорожки в имени: русские названия дорожек в латинское
			// имя файла не переводятся, и без номера все дорожки получили бы
			// одного собеседника.
			owner = fmt.Sprintf("%s-%d-%s", group, i+1, laneSlug(l.Name))
		}
		d, err := m.Create(StartOptions{Features: l.Features, Owners: []string{owner},
			Titles: map[string]string{owner: l.Title}, Title: title + " — " + l.Name, Group: group, Lane: l.Name})
		if err != nil {
			return "", nil, err
		}
		ids = append(ids, d.ID)
		out = append(out, d)
	}
	m.mu.Lock()
	m.groups[group] = ids
	m.mu.Unlock()
	return group, out, nil
}

func laneSlug(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == ' ' || r == '-' || r == '_':
			b.WriteRune('-')
		}
	}
	s := strings.Trim(b.String(), "-")
	if s == "" {
		s = "lane"
	}
	if len(s) > 24 {
		s = s[:24]
	}
	return s
}

// SendGroup задаёт следующий вопрос всем дорожкам сразу. Если хоть одна
// ещё отвечает, ход не начинается ни в одной: дорожки идут в ногу.
func (m *Manager) SendGroup(group string, req agents.Request) ([]*Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ids := m.groups[group]
	if len(ids) == 0 {
		return nil, ErrNotFound
	}
	for _, id := range ids {
		if _, running := m.active[id]; running {
			return nil, ErrBusy
		}
	}
	var out []*Session
	for _, id := range ids {
		s, err := m.sendLocked(m.convs[id], req)
		if err != nil {
			return out, err
		}
		out = append(out, s)
	}
	return out, nil
}

// Group — дорожки стенда по порядку.
func (m *Manager) Group(group string) ([]Detail, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ids := m.groups[group]
	if len(ids) == 0 {
		return nil, false
	}
	out := make([]Detail, 0, len(ids))
	for _, id := range ids {
		if c, ok := m.convs[id]; ok {
			out = append(out, m.detailLocked(c))
		}
	}
	return out, true
}

// Groups — стенды: идентификатор → диалоги дорожек.
func (m *Manager) Groups() map[string][]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string][]string, len(m.groups))
	for g, ids := range m.groups {
		out[g] = append([]string(nil), ids...)
	}
	return out
}
