// Package textual разбирает то, что видно на экране панели: статусную строку,
// хвост вывода и упоминания задач. Правила здесь неочевидны и каждое оплачено
// ошибкой — см. комментарии.
package textual

import (
	"regexp"
	"strconv"
	"strings"
)

var (
	ansi    = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)
	barRe   = regexp.MustCompile(`(5h|7d)?\s*[█░]{2,}\s*(\d+)%`)
	spelled = regexp.MustCompile(`(\d+)%\s*context used`)
	mrRe    = regexp.MustCompile(`(?:[A-Za-z0-9._-][A-Za-z0-9._/-]*)?![0-9]{2,}`)
	ticket  = regexp.MustCompile(`\b(?:SNEW|SD|BF)-\d+\b`)
	repoRe  = regexp.MustCompile(`^\s*(\S+)\s{2,}`)
	running = regexp.MustCompile(`⏺\s+Running\b.*`)
	ruleRe  = regexp.MustCompile(`^[\s─-╿]*$`)
	promptR = regexp.MustCompile(`^\s*❯\s*$`)
)

func StripANSI(s string) string { return ansi.ReplaceAllString(s, "") }

// Cut схлопывает переносы и режет по границе, сообщая, была ли обрезка.
// Clip обрезает по длине, но строчную разбивку оставляет: окно вокруг записи
// читают глазами, и переводы строк там часть смысла. Cut, наоборот, сводит
// запись к одной строке для списков.
func Clip(s string, max int) (string, bool) {
	r := []rune(s)
	if len(r) <= max {
		return s, false
	}
	return strings.TrimRight(string(r[:max]), " ") + "…", true
}

func Cut(s string, max int) (string, bool) {
	flat := strings.TrimSpace(regexp.MustCompile(`[ \t]{2,}`).ReplaceAllString(
		regexp.MustCompile(`\s*\n+\s*`).ReplaceAllString(StripANSI(s), " "), " "))
	r := []rune(flat)
	if len(r) <= max {
		return flat, false
	}
	return strings.TrimRight(string(r[:max]), " ") + "…", true
}

func lines(raw string) []string {
	out := []string{}
	for _, l := range strings.Split(StripANSI(raw), "\n") {
		l = strings.TrimRight(l, " \t")
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}

// CleanTail выбрасывает рамки, футер и пустой промпт — то, что не разговор.
func CleanTail(raw string, maxLines int) []string {
	out := []string{}
	for _, l := range lines(raw) {
		if ruleRe.MatchString(l) || promptR.MatchString(l) ||
			strings.Contains(l, "⏵⏵") || strings.ContainsAny(l, "█░") || strings.Contains(l, "auto mode on") {
			continue
		}
		out = append(out, strings.TrimPrefix(strings.TrimPrefix(l, " "), " "))
	}
	if len(out) > maxLines {
		out = out[len(out)-maxLines:]
	}
	return out
}

type Gauge struct {
	ContextPct *int
	Limits     map[string]int
}

// Gauges читает статусную строку: первый столбик без префикса — контекст,
// именованные 5h и 7d — лимиты. При высоком заполнении Claude Code печатает
// процент словами, и эта форма важнее столбика.
func Gauges(raw string) Gauge {
	g := Gauge{}
	for _, l := range lines(raw) {
		if m := spelled.FindStringSubmatch(l); m != nil {
			v, _ := strconv.Atoi(m[1])
			g.ContextPct = &v
		}
		for _, m := range barRe.FindAllStringSubmatch(l, -1) {
			v, _ := strconv.Atoi(m[2])
			if m[1] != "" {
				if g.Limits == nil {
					g.Limits = map[string]int{}
				}
				g.Limits[m[1]] = v
			} else if g.ContextPct == nil {
				n := v
				g.ContextPct = &n
			}
		}
	}
	return g
}

type Signal struct {
	MRs             []string
	Tickets         []string
	Repo            string
	CurrentToolCall string
	LastUserPrompt  string
}

type lineKind int

const (
	kindText lineKind = iota
	kindBar
	kindFooter
	kindSqueezed
	kindHistory
)

// Узкая панель ужимает служебные строки, срезая id и в конце, и в середине
// списка: SD-6613 приезжает как SD-66, а такого тикета не существует. Целый
// футер узнаётся по подсказке про shift+tab; без неё строка ужата и верить ей
// нечему. У статус-бара отбрасывается совпадение, упёршееся в край или в
// многоточие. Обычный текст переносится по словам и таких обрубков не даёт.
func classify(l string) lineKind {
	if strings.Contains(l, "⏵⏵") {
		if strings.Contains(l, "(shift+tab to cycle)") {
			return kindFooter
		}
		return kindSqueezed
	}
	if strings.ContainsAny(l, "█░") {
		return kindBar
	}
	return kindText
}

func collect(re *regexp.Regexp, line string, kind lineKind, into *[]string) {
	if kind == kindSqueezed {
		return
	}
	for _, loc := range re.FindAllStringIndex(line, -1) {
		after := line[loc[1]:]
		if kind != kindHistory {
			if strings.HasPrefix(after, "…") {
				continue
			}
			if kind == kindBar && after == "" {
				continue
			}
		}
		*into = append(*into, line[loc[0]:loc[1]])
	}
}

func dedupe(in []string, cap int) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, v := range in {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
			if len(out) == cap {
				break
			}
		}
	}
	return out
}

// Signals извлекает проверяемое, не делая выводов. entries — тексты записей
// истории: там правила экрана не действуют, потому что текст целый.
func Signals(rawTail string, entries []string) Signal {
	mrs, tickets := []string{}, []string{}
	var bar, run string
	for _, l := range lines(rawTail) {
		k := classify(l)
		collect(mrRe, l, k, &mrs)
		collect(ticket, l, k, &tickets)
		if k == kindBar {
			bar = l
		}
		if running.MatchString(l) {
			run = running.FindString(l)
		}
	}
	for _, e := range entries {
		collect(mrRe, e, kindHistory, &mrs)
		collect(ticket, e, kindHistory, &tickets)
	}
	s := Signal{MRs: dedupe(mrs, 10), Tickets: dedupe(tickets, 10), CurrentToolCall: run}
	if m := repoRe.FindStringSubmatch(bar); m != nil {
		s.Repo = strings.TrimPrefix(strings.TrimPrefix(m[1], "…"), "/")
	}
	return s
}
