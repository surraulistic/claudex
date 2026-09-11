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

func Resolve(q string, panes []state.Pane) (state.Pane, error) {
	if q == "" {
		return state.Pane{}, fmt.Errorf("цель не задана")
	}
	for _, p := range panes {
		if p.ID == q || p.SessionID == q {
			return p, nil
		}
	}
	low := strings.ToLower(q)
	// Точное совпадение заголовка снимает двусмысленность там, где подстрока
	// её создаёт: «license» и «license-cleanup» живут рядом.
	for _, p := range panes {
		if strings.ToLower(p.Title) == low {
			return p, nil
		}
	}
	var hits []state.Pane
	for _, p := range panes {
		if strings.Contains(strings.ToLower(p.Title), low) ||
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
		names = append(names, fmt.Sprintf("%s (%s)", p.ID, p.Title))
	}
	return state.Pane{}, fmt.Errorf("цель %q двусмысленна: %s", q, strings.Join(names, ", "))
}
