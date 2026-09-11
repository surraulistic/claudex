package main

import (
	"reflect"
	"sort"
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
