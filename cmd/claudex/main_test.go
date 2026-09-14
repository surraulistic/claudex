package main

import (
	"encoding/json"
	"github.com/surraulistic/claudex/internal/journal"
	"github.com/surraulistic/claudex/internal/task"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestSplitArgsTakesFlagsAfterPositional(t *testing.T) {
	// Разбор из стандартной библиотеки останавливается на первом позиционном
	// доводе, и «find "миграция" --limit 3» уводил «--limit 3» в сам запрос.
	for _, c := range []struct {
		in    []string
		flags []string
		rest  []string
	}{
		{[]string{"find", "миграция", "--limit", "3"},
			[]string{"--limit", "3"}, []string{"find", "миграция"}},
		{[]string{"--limit", "3", "find", "миграция"},
			[]string{"--limit", "3"}, []string{"find", "миграция"}},
		{[]string{"delegate", "river", "почини", "--no-wait", "--timeout", "5m"},
			[]string{"--no-wait", "--timeout", "5m"}, []string{"delegate", "river", "почини"}},
		{[]string{"find", "x", "--limit=3"},
			[]string{"--limit=3"}, []string{"find", "x"}},
	} {
		flags, rest := splitArgs(c.in)
		if !reflect.DeepEqual(flags, c.flags) || !reflect.DeepEqual(rest, c.rest) {
			t.Fatalf("%v → флаги %v, остальное %v; ожидалось %v и %v",
				c.in, flags, rest, c.flags, c.rest)
		}
	}
}

func TestDoubleDashEndsFlags(t *testing.T) {
	// После «--» довод, начинающийся с дефиса, — часть запроса, а не флаг.
	flags, rest := splitArgs([]string{"find", "--", "--limit"})
	if len(flags) != 0 || !reflect.DeepEqual(rest, []string{"find", "--limit"}) {
		t.Fatalf("получено флаги %v, остальное %v", flags, rest)
	}
}

func TestNegativeLookingQueryIsNotEatenAsFlagValue(t *testing.T) {
	// Булев флаг значения не берёт: иначе «--no-wait» съел бы следующее слово.
	flags, rest := splitArgs([]string{"delegate", "river", "--no-wait", "задача"})
	if !reflect.DeepEqual(flags, []string{"--no-wait"}) ||
		!reflect.DeepEqual(rest, []string{"delegate", "river", "задача"}) {
		t.Fatalf("получено флаги %v, остальное %v", flags, rest)
	}
}

func TestPanesAreOrderedActiveFirstThenRecent(t *testing.T) {
	// Смотрящий читает список сверху и первым делом должен видеть то, что
	// происходит сейчас; панели без истории уходят в конец своей группы.
	at := func(s string) *string { return &s }
	v := []paneView{
		{PaneID: "p-idle-старая", Status: "idle", LastActivity: at("2026-09-01T00:00:00Z")},
		{PaneID: "p-done-без", Status: "done"},
		{PaneID: "p-idle-свежая", Status: "idle", LastActivity: at("2026-09-10T00:00:00Z")},
		{PaneID: "p-работает", Status: "working", LastActivity: at("2026-01-01T00:00:00Z")},
		{PaneID: "p-done-свежая", Status: "done", LastActivity: at("2026-09-05T00:00:00Z")},
		{PaneID: "p-чужой-статус", Status: "затмение"},
	}
	sort.SliceStable(v, func(i, j int) bool { return less(v[i], v[j]) })
	got := make([]string, len(v))
	for i, p := range v {
		got[i] = p.PaneID
	}
	want := []string{"p-работает", "p-done-свежая", "p-done-без",
		"p-idle-свежая", "p-idle-старая", "p-чужой-статус"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("порядок %v, ожидался %v", got, want)
	}
}

func TestSecondsAndDurationsBothParse(t *testing.T) {
	// Прежняя версия принимала секунды числом; ломать это нельзя, но и
	// «30m» читать удобно.
	for _, c := range []struct {
		in   string
		want time.Duration
	}{
		{"1800", 30 * time.Minute},
		{"30m", 30 * time.Minute},
		{"90s", 90 * time.Second},
		{"0", 0},
	} {
		got, err := parseTimeout(c.in)
		if err != nil || got != c.want {
			t.Fatalf("parseTimeout(%q) = %v, %v; ожидалось %v", c.in, got, err, c.want)
		}
	}
	if _, err := parseTimeout("скоро"); err == nil {
		t.Fatal("невнятный срок — ошибка")
	}
}

func TestDigestCarriesTheFourDocumentedParts(t *testing.T) {
	// Дайджест одной панели — единственная команда, не попавшая в первую
	// сверку, и единственная, которая разошлась с договором: печатала текст
	// вместо JSON. Проверка держит форму, а не содержимое.
	raw, err := json.Marshal(digestView{})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	json.Unmarshal(raw, &got)
	for _, k := range []string{"target", "alias", "live", "history", "tail", "signals"} {
		if _, ok := got[k]; !ok {
			t.Fatalf("часть %q пропала из дайджеста: %s", k, raw)
		}
	}
	live, _ := json.Marshal(liveView{})
	json.Unmarshal(live, &got)
	// По этим полям чужой агент решает, можно ли давать панели новую задачу.
	for _, k := range []string{"pane_id", "status", "context_pct", "limits", "session_id"} {
		if _, ok := got[k]; !ok {
			t.Fatalf("поле %q пропало из live: %s", k, live)
		}
	}
}

func TestTasksNamesWhatWasNeverDelivered(t *testing.T) {
	// Сбой 742309b7 остался незамеченным именно потому, что провал пробуждения
	// жил только в логе отсоединённого наблюдателя.
	recs := []journal.Record{
		{Task: "готовая", Event: journal.Notified, Outcome: "разбужен"},
		{Task: "провал", Event: journal.Notified, Outcome: "не разбужен, показано человеку"},
		{Task: "провал", Event: journal.Finished},
		{Task: "тихая", Event: journal.Notified, Outcome: "не доставлено"},
	}
	got := undeliveredWakes(recs)
	if len(got) != 2 {
		t.Fatalf("названы обе недоставленные, получено %v", got)
	}
	for _, want := range []string{"провал", "тихая"} {
		if !strings.Contains(strings.Join(got, " "), want) {
			t.Fatalf("%q не названа: %v", want, got)
		}
	}
	if strings.Contains(strings.Join(got, " "), "готовая") {
		t.Fatal("разбуженная в список не попадает")
	}
}

func TestTasksSeparatesStagesOfTheSameTask(t *testing.T) {
	// Исход по сроку и пришедший позже отчёт — разные события. Если считать их
	// одним, доставленный таймаут скрывает недоставленный отчёт.
	recs := []journal.Record{
		{Task: "поздняя", Event: journal.Notified, Stage: task.StageFinished, Outcome: "разбужен"},
		{Task: "поздняя", Event: journal.Notified, Stage: task.StageReported,
			Outcome: "не разбужен, показано человеку"},
		{Task: "чистая", Event: journal.Notified, Stage: task.StageFinished, Outcome: "разбужен"},
	}
	got := undeliveredWakes(recs)
	if len(got) != 1 || !strings.Contains(got[0], "поздняя/"+task.StageReported) {
		t.Fatalf("названа именно недоставленная стадия, получено %v", got)
	}
}

func TestPaneListingCarriesNoForeignReports(t *testing.T) {
	// Через это поле чужой отчёт и попал в лицензионный разговор: выдачу
	// панелей читает любой агент, а отчёт принадлежит одному разговору.
	// Недоставленное остаётся в `claudex tasks`, который смотрит человек.
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(src), `"undelivered"`) {
		t.Fatal("выдача панелей не должна нести чужие отчёты")
	}
}

func TestWakeIsRefusedBeforeAnythingIsSent(t *testing.T) {
	// Поручение, которому некуда вернуться, не заводится вовсе: вызывающий
	// узнаёт об этом в своём ходу, а не из лога, который никто не читает.
	dir := t.TempDir()
	j := journal.Open(filepath.Join(dir, "tasks.jsonl"))
	j.Append(journal.Record{Task: "a", Event: journal.Started, Target: "wF:codex", TargetSession: "01a07921"})
	j.Append(journal.Record{Task: "b", Event: journal.Started, Target: "wF:codex", TargetSession: "01a09c3c"})

	err := wakeIsPossible(j, opts{notify: "wF:codex"})
	if err == nil {
		t.Fatal("панель с несколькими разговорами адресом не считается")
	}
	if !strings.Contains(err.Error(), "exec") || !strings.Contains(err.Error(), "wait") {
		t.Fatalf("в отказе сказано, чем заменить, получено %q", err)
	}

	// Без пробуждения поручение берётся: результат вернёт сам вызов.
	if err := wakeIsPossible(j, opts{}); err != nil {
		t.Fatalf("обычный запуск не требует адреса, получено %v", err)
	}
}
