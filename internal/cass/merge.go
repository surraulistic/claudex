package cass

import "sort"

// Local — попадание собственного лексического индекса, приведённое к общему
// виду. Полный тип хранилища сюда не тащим: пакету нужен только ключ сведения,
// а зависимость от store развернула бы направление импорта.
type Local struct {
	ID        int64
	Session   string
	TS        int64 // секунды
	Text      string
	Agent     string
	Workspace string
}

// Откуда попадание.
const (
	ViaCass = "cass" // только семантика/гибрид
	ViaFTS  = "fts5" // только собственный индекс
	ViaBoth = "both" // нашли оба — подтверждено дважды
)

type Merged struct {
	Session   string  `json:"session,omitempty"`
	TS        int64   `json:"ts,omitempty"`
	Text      string  `json:"text"`
	Score     float64 `json:"score,omitempty"`
	MatchType string  `json:"match_type,omitempty"`
	Via       string  `json:"via"`
	Line      int     `json:"line,omitempty"`
	ID        int64   `json:"id,omitempty"`
	Agent     string  `json:"agent,omitempty"`
	Workspace string  `json:"workspace,omitempty"`
}

// key — чем одно и то же попадание опознаётся в двух выдачах.
//
// Общего идентификатора у них нет: cass отдаёт путь транскрипта и номер
// строки, локальный индекс — свой rowid. Сводим по разговору и секунде записи.
// Две записи одного разговора в одну секунду ключ склеит — цена принята
// сознательно, иначе сводить нечем: текст у cass приходит фрагментом с
// подсветкой, а не исходной записью.
type key struct {
	session string
	ts      int64
}

// Merge ставит семантические попадания первыми, а найденные обоими помечает
// отдельно: совпадение двух независимых движков — довод сильнее любого из них.
// Лексические, которых семантика не нашла, идут следом и не выбрасываются:
// точное совпадение по редкому слову семантика пропускает регулярно.
func Merge(sem []Hit, lex []Local, limit int) []Merged {
	local := map[key]Local{}
	for _, l := range lex {
		local[key{l.Session, l.TS}] = l
	}

	out := make([]Merged, 0, len(sem)+len(lex))
	taken := map[key]bool{}

	for _, h := range sem {
		k := key{h.Session, h.TS / 1000}
		if taken[k] {
			continue
		}
		taken[k] = true
		m := Merged{
			Session: h.Session, TS: k.ts, Text: h.Snippet, Score: h.Score,
			MatchType: h.MatchType, Via: ViaCass, Line: h.Line,
			Agent: h.Agent, Workspace: h.Workspace,
		}
		if l, ok := local[k]; ok {
			m.Via, m.ID = ViaBoth, l.ID
			// Текст берём свой: у cass это фрагмент с разметкой подсветки.
			if l.Text != "" {
				m.Text = l.Text
			}
		}
		out = append(out, m)
	}

	rest := make([]Merged, 0, len(lex))
	for _, l := range lex {
		k := key{l.Session, l.TS}
		if taken[k] {
			continue
		}
		taken[k] = true
		rest = append(rest, Merged{
			Session: l.Session, TS: l.TS, Text: l.Text, Via: ViaFTS,
			ID: l.ID, Agent: l.Agent, Workspace: l.Workspace,
		})
	}
	// Свежие первыми: при поиске по всей истории недавний разговор полезнее
	// того, что движок счёл самым похожим.
	sort.SliceStable(rest, func(i, j int) bool { return rest[i].TS > rest[j].TS })
	out = append(out, rest...)

	if limit <= 0 || len(out) <= limit {
		return out
	}
	// Простое обрезание превращает объединение в фикцию: cass почти всегда
	// отдаёт ровно столько, сколько попросили, и лексическая половина не
	// доходит до выдачи никогда. Замерено — при пределе 40 из сорока
	// результатов лексических оказалось ноль.
	//
	// Поэтому часть мест закреплена за теми, кого нашёл только собственный
	// индекс: точное совпадение по редкому слову семантика пропускает
	// регулярно, и именно его ищут, когда помнят формулировку.
	reserve := limit / 4
	if reserve < 1 {
		reserve = 1
	}
	if reserve > len(rest) {
		reserve = len(rest)
	}
	head := out[:len(out)-len(rest)]
	if n := limit - reserve; len(head) > n {
		head = head[:n]
	}
	kept := append(append([]Merged{}, head...), rest[:limit-len(head)]...)
	return kept
}
