package textual

import (
	"strings"
	"testing"
)

const bar = "  ~/GolandProjects/license-platform  panel-real-data  Opus 5  ██░░░49%  5h░░░░░7% 12:00  7d███░░57% 10.09 02:00"

func TestGaugesBarIsContextNamedAreLimits(t *testing.T) {
	g := Gauges(bar)
	if g.ContextPct == nil || *g.ContextPct != 49 {
		t.Fatalf("первый столбик без префикса — контекст, получено %v", g.ContextPct)
	}
	if g.Limits["5h"] != 7 || g.Limits["7d"] != 57 {
		t.Fatalf("именованные столбики — лимиты, получено %v", g.Limits)
	}
}

func TestGaugesSpelledFormWinsOverBar(t *testing.T) {
	// При высоком заполнении Claude Code пишет процент словами вместо столбика.
	g := Gauges(bar + "\n  97% context used")
	if g.ContextPct == nil || *g.ContextPct != 97 {
		t.Fatalf("текстовая форма важнее столбика, получено %v", g.ContextPct)
	}
}

func TestGaugesAbsentIsNilNotZero(t *testing.T) {
	// Узкая панель режет статусную строку; у не-Claude агента её нет вовсе.
	// Ноль читался бы как «контекст свободен».
	if g := Gauges("⏺ просто вывод"); g.ContextPct != nil {
		t.Fatalf("значения нет на экране — должен быть nil, получено %v", *g.ContextPct)
	}
}

func TestSignalsSqueezedFooterIsNotRead(t *testing.T) {
	// Ужатый футер режет id в середине списка: SD-6613 приезжает как SD-66,
	// которого не существует. Узнаётся по отсутствию подсказки про shift+tab.
	s := Signals("  ⏵⏵ auto mode on · casino-sdk-p ·SD-66 ·SD-82", nil)
	if len(s.Tickets) != 0 || len(s.MRs) != 0 {
		t.Fatalf("ужатый футер читать нельзя, получено %v %v", s.Tickets, s.MRs)
	}
}

func TestSignalsIntactFooterYieldsTrailingTicket(t *testing.T) {
	s := Signals("  ⏵⏵ auto mode on (shift+tab to cycle) · MR !480 · SD-7615", nil)
	if len(s.MRs) != 1 || s.MRs[0] != "!480" {
		t.Fatalf("MR из целого футера, получено %v", s.MRs)
	}
	if len(s.Tickets) != 1 || s.Tickets[0] != "SD-7615" {
		t.Fatalf("последний в строке тикет целого футера берётся, получено %v", s.Tickets)
	}
}

func TestSignalsStatusBarDropsEdgeAndEllipsis(t *testing.T) {
	if s := Signals("  …/GolandProjects/casino-auth-service  SNEW-4…  Opus 5  ░░░░8%", nil); len(s.Tickets) != 0 {
		t.Fatalf("обрезанное многоточием не тикет, получено %v", s.Tickets)
	}
	if s := Signals("  casino-auth  ветка  Opus 5  ░░░░8%  SD-82", nil); len(s.Tickets) != 0 {
		t.Fatalf("упёршееся в край статус-бара не тикет, получено %v", s.Tickets)
	}
}

func TestSignalsPlainTextUntouched(t *testing.T) {
	s := Signals("Влил bonuses!874, закрыл SD-8208", nil)
	if len(s.MRs) != 1 || s.MRs[0] != "bonuses!874" {
		t.Fatalf("обычный текст не режется, получено %v", s.MRs)
	}
	if len(s.Tickets) != 1 || s.Tickets[0] != "SD-8208" {
		t.Fatalf("обычный текст не режется, получено %v", s.Tickets)
	}
}

func TestSignalsHistoryIsNotSubjectToScreenRules(t *testing.T) {
	s := Signals("", entriesOf("закрыт SD-8208 и влит bonuses!874"))
	if len(s.Tickets) != 1 || len(s.MRs) != 1 {
		t.Fatalf("в истории текст целиком, правила экрана не применяются: %v %v", s.Tickets, s.MRs)
	}
}

func TestSignalsSingleBangIsNotMR(t *testing.T) {
	if s := Signals("", entriesOf("ура!1 и !42")); len(s.MRs) != 1 || s.MRs[0] != "!42" {
		t.Fatalf("!1 не номер MR, получено %v", s.MRs)
	}
}

func TestSignalsRepoFromStatusBar(t *testing.T) {
	s := Signals("⏺ Running 3 shell commands…\n  …/GolandProjects/casino-payment-service  SNEW-711-cpf…  Opus 5  ░░░░8%", nil)
	if s.Repo != "GolandProjects/casino-payment-service" {
		t.Fatalf("repo без ведущего многоточия, получено %q", s.Repo)
	}
	if s.CurrentToolCall != "⏺ Running 3 shell commands…" {
		t.Fatalf("последняя строка Running, получено %q", s.CurrentToolCall)
	}
}

func TestCleanTailDropsChrome(t *testing.T) {
	raw := "──────────────────────\n⏺ Всё зелёное.   \n\n❯\n  …/x/casino-payment-service  SNEW-711  ░░░░8%\n  ⏵⏵ auto mode on (shift+tab to cycle) · MR !480"
	got := CleanTail(raw, 12)
	if len(got) != 1 || got[0] != "⏺ Всё зелёное." {
		t.Fatalf("рамки, футер и пустой промпт выбрасываются, получено %#v", got)
	}
}

func TestCleanTailKeepsLastLines(t *testing.T) {
	got := CleanTail("один\nдва\nтри\nчетыре", 2)
	if len(got) != 2 || got[0] != "три" {
		t.Fatalf("берутся последние maxLines, получено %#v", got)
	}
}

func TestCutCollapsesAndMarksTruncation(t *testing.T) {
	if s, tr := Cut("a\n\nb   c", 40); s != "a b c" || tr {
		t.Fatalf("переносы схлопываются, получено %q %v", s, tr)
	}
	if s, tr := Cut("абвгде", 3); s != "абв…" || !tr {
		t.Fatalf("режется по границе с пометкой, получено %q %v", s, tr)
	}
}

func TestStripAnsiKeepsBracketsInText(t *testing.T) {
	esc := "\x1b[31mкрасный\x1b[0m"
	if got := StripANSI(esc); got != "красный" {
		t.Fatalf("escape снимается, получено %q", got)
	}
	if got := StripANSI("см. [0] и [12]"); got != "см. [0] и [12]" {
		t.Fatalf("скобки в тексте не трогаются, получено %q", got)
	}
}

func TestClipKeepsLineBreaks(t *testing.T) {
	// Отличие от Cut: тот сводит запись к одной строке для списка, а окно
	// вокруг записи читают глазами.
	in := "первая\n\nвторая строка"
	if got, cut := Clip(in, 100); got != in || cut {
		t.Fatalf("короткое не трогается, получено %q %v", got, cut)
	}
	got, cut := Clip(in, 8)
	if !cut || !strings.Contains(got, "\n") {
		t.Fatalf("длинное обрезается, разбивка цела, получено %q", got)
	}
}

func entriesOf(texts ...string) []HistoryEntry {
	out := make([]HistoryEntry, 0, len(texts))
	for _, t := range texts {
		out = append(out, HistoryEntry{Kind: "assistant", Text: t})
	}
	return out
}

func TestRepoKeepsLeadingSlash(t *testing.T) {
	// «…» срезается вместе со своей косой; у абсолютного пути корень остаётся.
	for _, c := range []struct{ bar, want string }{
		{"  /private/tmp/проба  панель  Opus 5  ██░░░49%", "/private/tmp/проба"},
		{"  …/GolandProjects/casino-bonuses  панель  Opus 5  ██░░░49%", "GolandProjects/casino-bonuses"},
		{"  ~/projects/site  панель  Opus 5  ██░░░49%", "~/projects/site"},
	} {
		if got := Signals(c.bar, nil).Repo; got != c.want {
			t.Fatalf("из %q получено %q, ожидалось %q", c.bar, got, c.want)
		}
	}
}

func TestLastUserPromptComesFromHistoryByRole(t *testing.T) {
	// По одному тексту реплику пользователя от ответа не отличить — нужна роль.
	s := Signals("", []HistoryEntry{
		{Kind: "assistant", Text: "сделал"},
		{Kind: "user", Text: "почини сборку"},
		{Kind: "user", Text: "а это уже не последняя"},
	})
	if s.LastUserPrompt != "почини сборку" {
		t.Fatalf("получено %q", s.LastUserPrompt)
	}
	if Signals("", []HistoryEntry{{Kind: "assistant", Text: "сделал"}}).LastUserPrompt != "" {
		t.Fatal("без реплики пользователя поле пустое")
	}
}
