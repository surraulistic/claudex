// Package target разрешает человеческое имя панели в саму панель.
//
// Цель приходит в разном виде: pane_id из вывода sessions, session_id из
// вывода find, кусок заголовка или рабочий каталог. Раньше идентификатор
// сессии не принимался, и выданную самим же инструментом строку нельзя было
// вернуть ему обратно.
package target

import (
	"fmt"
	"strings"

	"github.com/surraulistic/claudex/internal/state"
)

// Resolve принимает метки вкладок отдельной картой: метка — свойство вкладки,
// а не панели, и тащить её в модель состояния незачем.
func Resolve(q string, panes []state.Pane, labels map[string]string) (state.Pane, error) {
	if q == "" {
		return state.Pane{}, fmt.Errorf("цель не задана")
	}
	for _, p := range panes {
		if p.ID == q || p.SessionID == q {
			return p, nil
		}
	}
	low := strings.ToLower(q)
	// Имя агента и метка вкладки — то, чем панель называют люди и что сам
	// инструмент печатает в поле target. Точное совпадение сильнее любой
	// подстроки: «media» не должно цепляться за чужой заголовок.
	for _, p := range panes {
		if strings.ToLower(p.Name) == low || strings.ToLower(labels[p.ID]) == low {
			return p, nil
		}
	}
	for _, p := range panes {
		if strings.ToLower(p.Title) == low {
			return p, nil
		}
	}
	var hits []state.Pane
	for _, p := range panes {
		if strings.Contains(strings.ToLower(p.Title), low) ||
			strings.Contains(strings.ToLower(p.Name), low) ||
			strings.Contains(strings.ToLower(labels[p.ID]), low) ||
			strings.HasSuffix(p.CWD, q) {
			hits = append(hits, p)
		}
	}
	switch len(hits) {
	case 1:
		return hits[0], nil
	case 0:
		return state.Pane{}, fmt.Errorf("панель %q не найдена", q)
	}
	var names []string
	for _, p := range hits {
		name := p.Title
		if labels[p.ID] != "" {
			name = labels[p.ID]
		}
		names = append(names, fmt.Sprintf("%s (%s)", p.ID, name))
	}
	return state.Pane{}, fmt.Errorf("цель %q двусмысленна: %s", q, strings.Join(names, ", "))
}
