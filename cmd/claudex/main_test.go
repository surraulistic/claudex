package main

import (
	"reflect"
	"testing"
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
